# ADR 006 — Workload naming and Ingress path routing

- **Status:** Accepted, amended 2026-04-24 (Ingress → Gateway API HTTPRoute)
- **Date:** 2026-04-24
- **Deciders:** @jurg
- **Related:** the project plan's "Architecture essentials — three workload
  types, not two". Resolves the Service-naming and Ingress-routing questions
  left open by Session 1's CRD surface (`spec.workloads` added to
  `api/v1alpha1/magestore_types.go`). Session 2 consumes this ADR in the
  `ensureWorkloads` rewrite and the new `ensureRouting` step (originally
  `ensureIngress` — see 2026-04-24 amendment below).

## Context

Session 1 landed `spec.workloads` as three named sub-fields —
`frontend` (required), `admin` (optional), `api` (optional). The
split is subtractive optimization: when `admin` or `api` is unset,
their Ingress paths route to the frontend Service. The CRD shape is
decided; what isn't decided is the naming and routing machinery that
surfaces it.

Four open questions:

1. **Service names.** What does the reconciler name the Deployment /
   Service for each workload?
2. **Ingress path map.** Which URL paths route to which Service, and
   how does the router handle absent optional workloads?
3. **Custom admin URL prefix.** Magento's `setup:install
   --backend-frontname=admin_xyz` randomizes the admin panel URL.
   Where does the prefix live — CRD field, image-contract env var, or
   bootstrap-artifact lookup?
4. **Ingress resource shape.** One Ingress with multiple rules, or one
   Ingress per workload?

Pre-ADR draft (dropped): `spec.workloads.admin.urlPrefix` as a CRD
field. Two real concerns pushed that back:

- **WorkloadSpec struct is shared across all three workload types
  today.** Adding `urlPrefix` forces an early split into per-type
  structs before the other two workload types have grown any
  type-specific fields. Premature abstraction.
- **The admin prefix is a bootstrap artifact.** It's set at
  `setup:install` time (`--backend-frontname`) and lives in
  `env.php` / `core_config_data` thereafter. Carrying it on the
  workload struct duplicates the source of truth.

## Decision

### Service and Deployment naming

**`<store>-frontend`, `<store>-admin`, `<store>-api`.** The
reconciler always materialises `<store>-frontend` (frontend is
required by CRD); `<store>-admin` exists when
`spec.workloads.admin` is set; `<store>-api` exists when
`spec.workloads.api` is set.

Same naming convention for the Deployment, the Service, and the
HPA (when autoscaling is configured). Labels follow the existing
`app.kubernetes.io/*` pattern:

```
app.kubernetes.io/name:       magento
app.kubernetes.io/instance:   <store>
app.kubernetes.io/component:  frontend | admin | api
app.kubernetes.io/managed-by: mage-operator
```

Matches the Slot-20 `<store>-frontend` precedent (reused verbatim)
and mirrors how mariadb-operator / valkey-operator / rabbitmq-
cluster-operator name their resources off the parent CR.

### Ingress path routing with fallthrough

**One Ingress resource per MageStore, multiple rules, path-based
backend selection. Fallthrough is implemented by the reconciler
choosing which backend Service name each path points at based on
which workload fields are set — not by nginx-level try-next logic.**

The Ingress rule list for a single-host MageStore:

| Path prefix          | Backend (admin set, api set)  | Backend (admin set, api unset) | Backend (admin unset, api unset) |
|----------------------|-------------------------------|-------------------------------|------------------------------|
| `/admin` *           | `<store>-admin`               | `<store>-admin`              | `<store>-frontend`           |
| `/rest`              | `<store>-api`                 | `<store>-frontend`           | `<store>-frontend`           |
| `/graphql`           | `<store>-api`                 | `<store>-frontend`           | `<store>-frontend`           |
| `/` (catch-all)      | `<store>-frontend`            | `<store>-frontend`           | `<store>-frontend`           |

`*` — `/admin` is the default prefix when Magento's
`--backend-frontname` is unset. Custom admin prefix handling is
resolved separately (see below); for the default case the prefix
is `admin`.

All paths use `pathType: Prefix`. Order in the rule list matters
only for loose readability — the Ingress controller evaluates
most-specific-first regardless of order, and the four prefixes
above don't overlap in ambiguous ways.

Multi-store (`spec.stores` with multiple hosts, Slot 21.4's ADR 007
shape): one Ingress rule group per host, each with the same four
path prefixes from the table above. The backend Service set is
identical across hosts — all hosts hit the same frontend Deployment
because they share a codebase. The MAGE_RUN_CODE/TYPE dispatch
happens at the nginx sidecar layer, not the Ingress layer, so
Ingress stays simple.

**Why per-path backend selection over rewrite-based routing.** An
alternative was to always route everything to `<store>-frontend`
and use nginx `rewrite` rules to proxy specific paths onward to
admin / api pods. Rejected: rewrite-based routing couples the
Ingress layer to the frontend pod's nginx sidecar config (which
becomes stateful about admin / api Service existence), and it
pessimistically routes all admin / api traffic through the frontend
pod's nginx before hitting the dedicated Deployment — defeating
the performance split that `spec.workloads.admin` / `.api` exists
for.

### Custom admin URL prefix — deferred, bootstrap-derived when it lands

**Phase 1 ships with `/admin` as the only supported admin path
prefix.** The `--backend-frontname` flag at `setup:install` time is
invoked with the literal `admin` value; the Ingress rule is
hard-coded to `/admin`. Stores that randomize the prefix for
security-through-obscurity reasons won't work on v0.1.0.

When demand surfaces, the shape is **bootstrap-derived**, not a
new `spec.workloads.admin.urlPrefix` CRD field:

- `spec.bootstrap.admin.urlPrefix` (or a sibling path — exact
  placement decided when the field lands) carries the prefix.
- `ensureBootstrap` passes it to `bin/magento setup:install
  --backend-frontname=<prefix>`.
- `ensureIngress` reads the same field to generate the matching
  Ingress rule (`/<prefix>` → admin or frontend Service).

The field lives on `spec.bootstrap` rather than `spec.workloads`
because it's a setup:install input, not a per-workload runtime
setting. Carrying it on the workload struct would force per-type
WorkloadSpec structs (frontend has no such concept) and would
surface as "configurable after install" when it's really not —
changing it post-install breaks admin access and requires a
Magento-side core_config_data update to match.

Open issue for the deferred work: the Ingress rule is generated at
reconcile time but the prefix isn't known until bootstrap has
succeeded and the Job's container args reflect the final value. The
reconciler needs to read `spec.bootstrap.admin.urlPrefix` (or its
default `admin`) and fence on it consistently. If both
bootstrap-pending and Ingress-generation happen in the same
reconcile pass, the input is the spec field; the Job's container
args and the Ingress rule share that input verbatim.

### One Ingress resource per MageStore

**The operator materialises exactly one Ingress resource named
`<store>`.** It carries a rule group per entry in `spec.stores` (or
one rule group for the default hostname when `spec.stores` is
unset), and the four path prefixes listed in the table above for
each rule group.

Rejected: one Ingress per workload (three Ingress resources when
all three workload types exist). The alternative was thinner per-
resource but multiplied the operator's reconcile surface by 3×,
created naming conflicts when two workload Ingresses claimed the
same host, and broke the TLS termination model — cert-manager
annotations on the Ingress resource must cover all paths for a
given host, and multiple Ingresses per host with per-path TLS is
supported by some controllers and not others.

One Ingress also matches the Slot-20 precedent: a single Ingress
rule pointing at `<store>-frontend` exists as the current shape,
and growing it to multiple backends under the same resource is the
minimum-delta change.

## Consequences

**Positive**

- Subtractive split model is user-facing simple. A MageStore with
  only `spec.workloads.frontend` set works identically to the
  Slot-20 demo; adding `spec.workloads.admin` "just routes `/admin`
  at a dedicated Deployment" with no other user-visible changes
  required.
- Each Ingress rule points at a real Service — no
  `backend: <doesntexist>` states, no nginx-level try-next logic.
  The Ingress reconciler can validate its output against the
  Service reconciler's output without runtime coupling.
- Naming is predictable. Users looking at `kubectl get svc` see
  `<store>-frontend`, and conditionally `<store>-admin` /
  `<store>-api` — the presence of the latter two directly mirrors
  the MageStore spec.

**Negative**

- Changing `spec.workloads.admin` from set to unset (or vice versa)
  churns the Ingress rule's backend pointer — a brief window
  where in-flight `/admin` requests hit the old backend. Since
  admin traffic is low-volume and toggling the workload split is
  a rare operation, not a meaningful impact. Documented in the
  CRD field descriptions.
- No shared Ingress resource across multiple MageStores. A site
  with ten MageStores (unusual) gets ten Ingress resources. Fine
  for v0.1.0; if consolidation pressure arises, the operator could
  grow a `spec.ingress.className` + `spec.ingress.sharedHosts`
  surface without breaking this ADR's structural decisions.
- Custom admin URL prefix is a deferred feature. Stores that rely
  on `--backend-frontname=admin_xyz` as a security-through-
  obscurity layer can't use v0.1.0 as-is. Workaround: a
  `pvcRef`-style pre-created Ingress with custom rules — not
  supported yet either; documented as "graduate to a separate
  Ingress managed outside the operator" until the bootstrap-
  derived shape lands.

**Neutral**

- The Ingress controller choice (nginx-ingress, Traefik,
  HAProxy, …) is orthogonal. `pathType: Prefix` is universally
  supported and the four path prefixes don't use any
  controller-specific annotations. cert-manager TLS annotations
  when `spec.ingress.tls` grows a field are equally agnostic.
- RBAC gains `create/update/patch/delete` on
  `networking.k8s.io/ingresses`. Typical for an operator that
  owns its traffic layer.

## Open questions

- **Cert-manager annotations.** Does `spec.ingress` grow a TLS
  block for v0.1.0, or do users annotate the Ingress out-of-band?
  Defer until Session 2 needs to emit the Ingress; the ADR 006
  shape is cert-manager-agnostic either way.
- **Session affinity.** Admin panel sessions are PHP-session-
  based. Does the `<store>-admin` Service need `sessionAffinity:
  ClientIP` to keep admins pinned to one pod? Arguably yes for
  admin (multi-pod admin with sticky sessions reduces login
  flapping); not needed for frontend (sessions live in Redis per
  image contract). Sub-decision for the `ensureWorkloads` rewrite.
- **HPA + initial replicas race.** When `autoscaling` is set with
  `minReplicas` unset, the operator seeds the HPA's floor from
  the workload's `replicas` field. If a user later sets
  `autoscaling.minReplicas` explicitly, the seed is stale. Not a
  data-loss concern; Session 2 decides whether to always re-read
  the seed or preserve the first-applied value.

## Amendment 2026-04-24 — Ingress → Gateway API HTTPRoute

**Switched from `networking.k8s.io/v1.Ingress` to
`gateway.networking.k8s.io/v1.HTTPRoute` + `Gateway`.** Surfaced
mid-Slot-22.5 when the original ingress-nginx install for e2e
validation prompted the question: ingress-nginx is in maintenance
mode; pinning the operator's only routing primitive to a
controller in maintenance is forward-debt. Gateway API is the
strategic Kubernetes routing direction (graduated to GA at
v1.0.0 in 2023, broad implementer support by 2026), and the
fallthrough table this ADR pins translates 1:1 to HTTPRoute's
`PathMatchPathPrefix` matcher + `BackendRefs[]`.

### What changed

- **Per-MageStore Gateway.** The operator emits a `<store>-gateway`
  resource alongside the existing per-store HTTPRoute, attached to
  the cluster's `eg` GatewayClass (Envoy Gateway's helm-chart
  convention). Single HTTP listener on port 80 named `http`;
  `AllowedRoutes.Namespaces.From: Same` (HTTPRoutes from other
  namespaces cannot attach). TLS landing deferred — graduates
  alongside cert-manager wiring or a user-supplied Secret ref.
- **Per-MageStore HTTPRoute.** Replaces the Ingress. Same
  `<store>` name, same path prefixes (`/admin`, `/rest`, `/graphql`,
  `/`), same fallthrough table. `parentRefs` attaches it to the
  per-store Gateway. Multi-host: `spec.hostnames` carries one
  entry per `spec.stores` entry (rather than the Ingress's "one
  rule group per host" shape).
- **`ensureIngress` → `ensureRouting`** in the codebase. One file
  (`ensure_routing.go`) contains both `ensureGateway` and
  `ensureHTTPRoute` plus the orchestrator — the routing layer is
  one concern, two resources.
- **RBAC swap.** `networking.k8s.io/ingresses` removed,
  `gateway.networking.k8s.io/{gateways,httproutes}` added.
- **Owns chain swap.** `Owns(&networkingv1.Ingress{})` removed,
  `Owns(&gatewayv1.Gateway{})` and `Owns(&gatewayv1.HTTPRoute{})`
  added.
- **scheme registration.** `gatewayv1.Install` added in `cmd/main.go`
  and `internal/controller/suite_test.go`.

### What stayed

- The fallthrough table is unchanged — backends still resolve as
  per the table in the original Decision section, just expressed
  as `BackendRefs[]` instead of `Backend.Service`.
- One routing resource per workload kind set is unchanged
  (single Ingress → single HTTPRoute + single Gateway, all bound
  to the MageStore lifecycle).
- The "Ingress controller choice is orthogonal" property
  generalises to "Gateway API implementation choice is
  orthogonal" — every conformant implementation handles
  HTTPRoute identically.

### Why per-MageStore Gateway, not shared

The shared-Gateway shape (one cluster Gateway, MageStores attach
HTTPRoutes via `parentRefs` to it) is architecturally cleaner —
fewer Envoy data planes, easier TLS aggregation. It requires a
new CRD field (`spec.routing.gatewayRef` or similar) so the
operator knows which Gateway to attach to. Slot 22.0's "no CRD
changes mid-Session-2" guardrail blocks that mid-flight.

Per-MageStore Gateway is the v0.1.0 simplification. Phase 2:
- Add `spec.routing.gatewayRef`. When set, skip
  `ensureGateway` and use the named Gateway as the
  `parentRefs[0]` target.
- Document the trade-off (per-store data plane isolation vs
  resource cost). Recommend shared-Gateway for production
  multi-tenant clusters, per-store for dev / single-MageStore
  shops.

### Why hard-code `eg` as the GatewayClass

Same reason as the per-MageStore Gateway: a `spec.routing.gatewayClassName`
field is a CRD change. Convention-name `eg` covers Envoy
Gateway's default; users with a different controller create a
GatewayClass alias named `eg` pointing at their preferred
controller, or wait for the Phase 2 field. Cluster-admin-level
indirection, not per-MageStore.

### Migration notes for any user on the pre-amendment Ingress shape

Pre-amendment is the period between Slot 22.2 (original
ensureIngress landing) and this amendment in Slot 22.5 — a
gap of about an hour, no users exposed. The transition is a
straight swap: delete the old Ingress, the new HTTPRoute +
Gateway materialise on next reconcile. No data migration
required (routing is stateless).

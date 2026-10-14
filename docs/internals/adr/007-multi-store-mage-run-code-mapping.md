# ADR 007 — Multi-store MAGE_RUN_CODE mapping

- **Status:** Accepted
- **Date:** 2026-04-24
- **Deciders:** @jurg
- **Related:** the project plan's "Architecture essentials — multi-store
  = one CR, many hosts". Builds on ADR 006 (workload naming + Ingress
  routing). Session 1's `spec.stores` CRD addition is the input; Session 2's
  new `ensureNginxConfig` step is the consumer.

## Context

Magento's multi-store model lets one codebase serve multiple
websites (different domains, different product catalogues,
different currencies) out of one deployment. Scope dispatch is
driven by two request-scoped variables — `MAGE_RUN_CODE` and
`MAGE_RUN_TYPE` — that Magento's `app/bootstrap.php` reads at the
start of every request to pick which store to serve.

Session 1 landed `spec.stores` as an array of
`{host, code, type}` entries. What's unresolved:

1. **Where does the host → code/type mapping happen in the request
   path?** nginx sidecar in the frontend pod, Ingress controller
   annotations, or somewhere else?
2. **What shape does the mapping ConfigMap take, and which
   reconciler step owns it?**
3. **How do MAGE_RUN_CODE / MAGE_RUN_TYPE reach php-fpm?** Env
   vars, fastcgi_param, or $_SERVER injection?
4. **How does Varnish (when enabled) interact with the mapping?**
   Per-host backend selection, per-host cache namespacing, or
   both?
5. **What happens for unknown hosts** (a request for a Host
   header not in `spec.stores`)?

Pre-ADR draft (dropped): expose `MAGE_RUN_CODE` / `MAGE_RUN_TYPE`
as per-workload container env vars populated by the operator. Two
real problems:

- Env vars are per-pod-instance, not per-request. A single
  Deployment serving three stores can't switch MAGE_RUN_CODE
  between requests via env vars.
- Pushes the host→scope dispatch into Kubernetes config rather
  than the request path, where it belongs.

## Decision

### Host → scope dispatch lives in the nginx sidecar

**Every workload Deployment (frontend, admin, api) that contains
the Magento image runs an nginx sidecar that performs the host →
MAGE_RUN_CODE / MAGE_RUN_TYPE dispatch before forwarding to
php-fpm via fastcgi.** The sidecar reads a shared ConfigMap
(`<store>-nginx-conf`) containing two generated `map` directives:

```nginx
map $http_host $MAGE_RUN_CODE {
    de.example.com   store_de;
    fr.example.com   store_fr;
    default          "";
}

map $http_host $MAGE_RUN_TYPE {
    de.example.com   website;
    fr.example.com   website;
    default          "";
}
```

The dispatch happens here (not at Ingress, not at Varnish)
because:

- **Request variables are what Magento consumes.** php-fpm reads
  `$_SERVER['MAGE_RUN_CODE']` / `$_SERVER['MAGE_RUN_TYPE']` in
  `bootstrap.php`. fastcgi_param is the canonical way to populate
  `$_SERVER` from nginx; the map directive + fastcgi_param pair
  is the idiomatic shape.
- **Ingress annotations are controller-specific.** nginx-ingress
  supports `nginx.ingress.kubernetes.io/server-snippet`; Traefik
  uses middlewares; HAProxy has its own shape. Doing the map in
  the sidecar keeps ADR 006's "Ingress controller choice is
  orthogonal" property intact.
- **Varnish is in front of the sidecar, not replacing it.** Even
  when Varnish caches the response, the sidecar still runs for
  cache misses, and Varnish's hash keys (see below) depend on
  the same host → scope dispatch the sidecar performs.

### ConfigMap shape and ownership

**One ConfigMap per MageStore, named `<store>-nginx-conf`**, with
two keys:

- `stores.conf` — the two `map` directives above, generated
  from `spec.stores`. Included into the sidecar's main
  `nginx.conf` at the top (map directives must live at the `http`
  block level, not inside `server`).
- `default.conf` — the fastcgi server block, parameter passing,
  and any per-workload directives. Includes the two
  `fastcgi_param MAGE_RUN_CODE $MAGE_RUN_CODE;` and
  `fastcgi_param MAGE_RUN_TYPE $MAGE_RUN_TYPE;` lines that
  surface the map values into php-fpm's `$_SERVER`.

Owned by a new `ensureNginxConfig` reconciler step (Session 2).
The ConfigMap has an `ownerReference` back to the MageStore;
deleting the MageStore deletes it.

**When `spec.stores` is unset**, `stores.conf` is generated with
neither `map` directive — the two `fastcgi_param` lines still
render but pass empty strings, and Magento's `bootstrap.php`
falls back to the default store. Matches the Slot-20 demo
behaviour (no `spec.stores`, no per-request scope dispatch,
single default store).

**When `spec.stores` has entries**, each host becomes one line in
each of the two maps. The `default` clause maps to empty string
(not the literal `"default"`) so unknown hosts fall back to
Magento's default store rather than being routed to a non-existent
`default` store code.

**ConfigMap hash annotation for rollout-on-change.** The
reconciler computes SHA-256 of the ConfigMap's `Data` field and
annotates each workload Deployment's pod template:

```yaml
metadata:
  annotations:
    mage.aknostic.com/nginx-config-hash: <sha256>
```

A change to `spec.stores` rewrites `stores.conf`, recomputes the
hash, and triggers a rolling restart of each workload Deployment.
Same pattern as the image-contract env var rollout trigger
(Slot-20 precedent).

### fastcgi_param is the plumbing, not env vars

**The Magento image's env.php must NOT read MAGE_RUN_CODE /
MAGE_RUN_TYPE from environment variables (`getenv()`).** It reads
them from `$_SERVER`, which the nginx sidecar populates via
`fastcgi_param`. This differs from every other image-contract
env var (database host, Redis password, ...) which are
env-var-based for per-pod static configuration.

The reason: scope dispatch is per-request. Two requests arriving
at the same php-fpm worker from different hosts must see
different `MAGE_RUN_CODE` values in `$_SERVER`; env vars are
worker-global and can't express that.

The image contract (`docs/image-contract.md`) grows a section
documenting this divergence: MAGE_RUN_CODE / MAGE_RUN_TYPE are
documented as `$_SERVER`-sourced (fastcgi_param), all other
image-contract variables remain `getenv()`-sourced (env vars).

### Per-workload Service interaction (admin, api)

**Admin and api sidecars receive the same `stores.conf`
ConfigMap.** Admin panel actions carry the same MAGE_RUN_CODE
context as the storefront that triggered them (e.g., saving a
product in the German store's admin must target the German
catalogue), so the map dispatches identically for admin
requests. API requests likewise — a `POST /rest/V1/products` on
`de.example.com` targets the German store scope.

The ConfigMap is mounted into all three workload Deployments
(frontend, admin, api) when the corresponding sidecar is
declared. One ConfigMap, three mounts. The hash annotation is
applied to all three Deployment pod templates so any change
rolls all three.

### Varnish interaction — cache namespacing, not backend selection

**When Varnish is enabled alongside `spec.stores`, the default
VCL uses `req.http.host` as a cache-key prefix so each store's
cached responses live in their own namespace.** The backend
selection does NOT vary per host — all hosts hit the same
`<store>-frontend` Service because they share a Deployment (per
ADR 006).

The default VCL generated by the operator:

```vcl
sub vcl_hash {
    hash_data(req.http.host);          # store namespacing
    hash_data(req.url);
    return (lookup);
}

backend frontend {
    .host = "<store>-frontend.<namespace>.svc";
    .port = "8080";
}
```

When `spec.stores` is unset, the default VCL omits the
`hash_data(req.http.host)` line — single-store installs have
nothing to namespace by.

When `spec.varnish.vclConfigMapRef` is set (user-supplied VCL),
the operator does NOT modify the user's VCL. Custom VCL authors
wanting per-store cache namespacing for multi-store setups must
include it themselves. The image contract (`docs/image-
contract.md`) grows a "Varnish multi-store VCL" snippet authors
can copy.

### Unknown-host handling

**Requests with a Host header not in `spec.stores` fall back to
Magento's default store.** The `default` clause of each nginx
map directive resolves to empty string, which Magento interprets
as "no scope override — use the default." Alternatives
considered:

- **Reject at Ingress with 404.** Too blunt: users adding a new
  hostname via DNS before editing the MageStore CR would see a
  hard failure. With the default-store fallback, the site is
  reachable (showing the default catalogue) until `spec.stores`
  is updated.
- **Return 421 Misdirected Request.** Semantically correct per
  RFC 7540 but no Magento-aware retry logic exists in clients;
  behaves as a 5xx to users.

Defaulting to the default store is the least-surprising shape
for hostnames the operator doesn't know about — and aligns with
Magento's own behaviour when MAGE_RUN_CODE is unset.

## Consequences

**Positive**

- Host → scope dispatch stays in the request path, not in
  Kubernetes config. Adding a new store = one CRD edit + a
  rolling restart; no env.php changes, no Magento rebuild.
- Ingress controller stays unopinionated. The map directive
  lives in the sidecar where the nginx version is under the
  operator's control.
- Varnish's per-store cache namespacing is mechanical — derived
  from the same `spec.stores` input that drives the nginx map.
  Users don't specify it twice.
- ConfigMap hash rollout pattern is the same as for image-
  contract env vars (Slot-20 precedent), so reconciler code can
  share a helper.

**Negative**

- Users with a hand-written nginx config can't simply point
  `vclConfigMapRef` (varnish) or an equivalent nginx override at
  the operator — `spec.workloads.<type>.nginxConfigMapRef` would
  need to exist, which it doesn't in v0.1.0. Documented as an
  escape hatch if a user hits a case the default sidecar config
  doesn't cover.
- fastcgi_param-based dispatch means the image CANNOT be run
  standalone (without the sidecar) if `spec.stores` is set —
  MAGE_RUN_CODE would never be populated. Acceptable: the
  operator owns the sidecar pairing; standalone image usage is
  Magento-default-store only.
- ConfigMap key change (e.g., renaming `stores.conf`) is a
  breaking change requiring coordinated image-contract version
  bump. Not a concern for v0.1.0 but documented in the image
  contract so downstream image maintainers know.

**Neutral**

- RBAC gains `create/update/patch/delete` on `configmaps`.
  Already typical for operators that own their workload config.
- The operator becomes aware of the nginx sidecar's
  configuration shape. This is a leak of concern across the
  image-operator boundary but an unavoidable one — the
  operator must know how to generate nginx config that works
  with the specific image it's driving.

## Open questions

- **nginx sidecar image.** The image contract today is silent on
  the sidecar image (`nginx:alpine` vs a customer-baked variant).
  Phase 2 may grow `spec.workloads.<type>.sidecar.image` for
  customers whose security review prohibits upstream nginx
  images. Out of v0.1.0 scope.
- **Varnish VCL generation for complex store topologies.**
  Multi-website + multi-store-view setups (one website with
  German and Spanish store views on de.example.com and
  es.example.com) may need VCL cache-key logic beyond
  `hash_data(req.http.host)`. The default VCL covers the
  common case; edge cases graduate to `vclConfigMapRef`.
- **SSL termination location.** Ingress-terminated TLS is the
  default assumption. Pass-through TLS to the Varnish or
  sidecar layer isn't supported yet; if it grows, the fastcgi
  map directive stays identical (host header is the input
  regardless of where TLS terminates).

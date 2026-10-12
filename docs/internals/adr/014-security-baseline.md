# ADR 014 — Security baseline

- **Status:** Accepted
- **Date:** 2026-05-08
- **Deciders:** @jurg
- **Related:**
  builds on `AdminSpec` (ADR 005 — bootstrap as Job) and the workload
  PodTemplateSpec emission across nine `ensure_*` controllers;
  shares the `<store>-config` Secret artefact with ADR 002 / ADR 012;
  reuses the hash-driven re-run pattern from ADR 013.

## Context

The 2026-05-07 v0.6.3 Kapsule re-validation cleared the fresh-install
polestar — Luma storefront, 2FA, transactional email, all in 4 m 28 s
with zero workarounds. That closed the v0.6.x real-infra arc and
exposed the largest *unaddressed* gap relative to the project's
stated motivations: the **security posture of the workloads the
operator generates.**

Three holes, all visible in `kubectl describe pod` on any v0.6.3
store:

1. **Flat namespace network.** No NetworkPolicies. A compromised
   frontend pod reaches the database, RabbitMQ, the search cluster,
   every Service IP in the namespace, and the cluster API server
   (subject only to RBAC). After any Magento RCE — and Magento has
   the track record — the attacker's first move is horizontal.
2. **Workload pods run as root.** Every PodTemplateSpec emitted by
   the nine workload-emitting controllers omits `securityContext`.
   Default UID is root, root filesystem is writable, every Linux
   capability is held, seccomp is the kubelet default. The
   operator's own manager Deployment has tight chart-side defaults
   — ironic, given the manager is not the thing that takes Magento
   RCEs.
3. **`/admin/` is the front door.** Every Magento store this
   operator deploys lives at the literal path. Adobe's own Commerce
   hardening guide lists "randomize admin URL" as a baseline
   mitigation alongside 2FA (which the operator wires correctly)
   and IP allowlisting.

For a project that promises "freedom to operate +
security," the operator hardens the manager that
won't be compromised and leaves the workloads that will. v0.7.0
closes the three gaps as one coordinated security-baseline release.

This ADR captures the architectural choices that frame the
implementation arc. Implementation mechanics — NetworkPolicy
resource shapes, securityContext field values, randomization
algorithm — live in the v0.7.0 brief and per-session
implementation commits.

## Decision

Three architectural choices, binding across sessions 1-3 of the
v0.7.0 arc.

### One umbrella `spec.security` group

Security-relevant CRD fields live under a single new `SecuritySpec`
on `MageStoreSpec`:

```go
type MageStoreSpec struct {
    // ... existing fields ...

    // +optional
    Security *SecuritySpec `json:"security,omitempty"`
}

type SecuritySpec struct {
    // +optional
    NetworkPolicies *NetworkPoliciesSpec `json:"networkPolicies,omitempty"`

    // +optional
    WorkloadSecurityContext *WorkloadSecurityContextSpec `json:"workloadSecurityContext,omitempty"`
}
```

Sprinkling three uncoordinated security knobs at the top level —
`spec.networkPolicies`, `spec.workloadSecurityContext`, `spec.security.adminUrl` — ages badly. The umbrella shape:

- gives v0.8.0+ a clear extension point for future security fields
  (admin IP allowlist, 2FA enforcement, mTLS toggle, AppArmor /
  SELinux profiles, FQDN egress policies). Each arrives as a new
  sub-block under `spec.security` rather than contesting top-level
  CRD real estate.
- groups the shared defaulting-safety rule under one conceptual
  umbrella, so users find it once.
- makes per-store opt-in coherent — `kubectl edit magestore <name>
  -- /spec/security` is the single audit point.
- mirrors the chart's own structure: the operator manager's
  `securityContext` lives under
  `controllerManager.manager.securityContext` in chart values;
  doing the same for workload-pod security keeps the conceptual
  map intact.

The `Enabled *bool` shape inside each sub-block (rather than a
top-level `spec.security.enabled`) lets users adopt NetworkPolicies
and workload-securityContext independently. Critical for staged
rollouts where the reference-image rebuild (gap #2) may lag behind
the policy ensure-function (gap #1).

### Shared upgrade-safety defaulting rule

A v0.6.x → v0.7.0 operator upgrade MUST NOT auto-apply default-deny
+ non-root + random-URL to running stores. That breaks running
workloads (Magento processes restart as root → permissions errors
on `/app/var`), breaks admin bookmarks (random URL replaces
`/admin/`), and overrides any customer-side NetworkPolicy
customizations the operator missed.

**Detection rule**, applied identically across all three items at
first reconcile of the new operator version:

- **Fresh store** = no operator-owned Secret exists yet for the
  store. Operator is bootstrapping. Defaults apply:
  `NetworkPolicies.enabled: true`, workload `securityContext`
  defaults applied, `BackendFrontname` randomized.
- **Existing store** = the operator-owned Secret already exists.
  Operator is upgrading. Defaults stay off:
  `NetworkPolicies.enabled: false`, workload `securityContext`
  empty (existing posture preserved), `BackendFrontname` defaults
  to literal `admin`.

Why a Secret-existence check? Operator-owned per-store Secrets are
created exactly once at first bootstrap, owned by the MageStore.
Their existence is the durable signal: present iff the operator
has seen the store before, absent iff this is the operator's first
reconcile.

Two operator-owned Secrets satisfy the criterion equally: each is
created exactly once per store at first bootstrap and lives until
the MageStore is deleted. The per-item implementation picks
whichever fits cleanly:

| Item | Signal Secret | Rationale |
|---|---|---|
| Admin URL randomization (gap #3) | `<store>-admin` | Co-located with the field being mutated (`backendFrontname` key on the same Secret). One Get serves both the upgrade-safety check and the persistence layer; avoids a reconcile-order race where `<store>-config` is rewritten by `ensureSecrets` earlier in the same pass. |
| NetworkPolicies (gap #1) | `<store>-config` | Natural ambient signal at the time `ensureNetworkPolicies` runs. |
| WorkloadSecurityContext (gap #2) | `<store>-config` | Same. The PodTemplate emitters check it before mutating the workload pod spec. |

The choice is implementation-driven, not architectural — both
Secrets are equivalent for the purpose. Other detection candidates
were considered and rejected:

- The MageStore's `BootstrapReady=True` condition. Rejected: also
  true for adopted stores (`spec.bootstrap` unset) where the
  operator never actually bootstrapped, and we do not want to
  harden adopted stores autonomously.
- A new `metadata.annotation` written by the operator at
  fresh-bootstrap time (e.g. `mage.aknostic.com/v0.7.0-default-applied:
  true`). Rejected: relies on the operator having seen the store
  at fresh-bootstrap time, which fails for stores already running
  on v0.6.x — exactly the case the rule is designed to handle.
- The MageStore's `creationTimestamp` versus a hardcoded v0.7.0
  release date. Rejected: brittle (clock skew, re-create after
  delete) and would force every existing store to take the
  defaults.

Existing stores opt in via explicit `kubectl edit` of the CR —
three independent toggles, each independently reversible.
UPGRADING.md walks the opt-in for each at v0.7.0 release time.

### Admin URL randomization lives on `AdminSpec`, not `SecuritySpec`

Counter to the umbrella decision, the `BackendFrontname` field
lands on `AdminSpec`:

```go
type AdminSpec struct {
    // ... existing fields (Email, FirstName, LastName, Username,
    // PasswordSecretRef) ...

    // +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_-]{4,32}$`
    // +optional
    BackendFrontname string `json:"backendFrontname,omitempty"`
}
```

Reason: it is a **bootstrap-time install argument**, not a
**reconcile-time security policy**. The other two items
(`NetworkPolicies`, `WorkloadSecurityContext`) are policies the
operator applies on every reconcile — they belong with the
security knobs. `BackendFrontname` is plumbed through to
`bin/magento setup:install --backend-frontname=...` exactly once at
bootstrap and is sticky thereafter (a random value generated on
first reconcile is persisted in the `<store>-admin` Secret). No
reconcile-time enforcement; no drift detection; no cross-cutting
policy concern.

`AdminSpec` already carries admin-account install arguments
(`Email`, `FirstName`, `LastName`, `Username`, `PasswordSecretRef`).
`BackendFrontname` is a sibling of `PasswordSecretRef` in shape —
both are operator-defaulted strings persisted in the `<store>-admin`
Secret. Magento devs reading `api/v1alpha1/magestore_types.go`
looking for "where do I set the admin URL?" find it under
`AdminSpec` as expected.

The shared upgrade-safety defaulting rule still applies — the
implementation just lives in a different ensure-function
(`ensure_bootstrap.go`, where `ensureAdminBackendFrontname` runs
before `ensureAdminPassword` and reads the `<store>-admin` Secret as
its signal — see the per-item table above) than the other two
(`ensure_network_policies.go` and the
nine `ensure_*` PodTemplateSpec emitters).

## Consequences

### Positive

- One coherent security-baseline release frame, easier to document
  and easier for users to reason about.
- Shared upgrade-safety pattern designed once, applied across all
  three items consistently. Future v0.x security additions inherit
  the pattern.
- Per-item opt-in lets customers stage hardening: NetworkPolicies
  first (zero downtime if the defaulting rule fires correctly),
  then workload-securityContext (may require reference-image
  catch-up), then admin URL randomization (requires bookmark
  migration).
- CRD extension point under `spec.security` cleanly absorbs
  v0.8.0+ security work without further top-level CRD surface.

### Negative

- One CRD shape change spans all three items even though session 1
  only ships admin URL randomization end-to-end. Sessions 2-3 must
  respect the placeholder shape from session 1 — small risk of
  "we'd have done it differently if we'd known" once
  implementations land. Mitigation: session 1 places only minimal
  field shape (the `Enabled *bool` discriminator and the obvious
  config fields per sub-block); implementation sessions are free to
  extend within the umbrella.
- The upgrade-safety rule must be tested across all three items in
  real-infra Phase E — adds validation complexity. Mitigated by
  running each toggle's opt-in separately on the existing v0.6.x
  demo namespace.
- Admin URL on `AdminSpec` instead of `SecuritySpec` forces
  documentation discipline. Security-baseline release notes must
  point readers at `AdminSpec.BackendFrontname` alongside the two
  `SecuritySpec` fields.

### Neutral

- Operator-owned Secret existence is the upgrade-safety signal.
  Per the per-item table in §"Shared upgrade-safety defaulting
  rule", admin URL randomization checks `<store>-admin`; the other
  two items check `<store>-config`. If a future ADR renames or
  restructures either Secret, the rule needs updating — a small
  coupling cost, called out here so it is not a surprise.

## Alternatives considered

- **Three top-level fields, no umbrella** (`spec.networkPolicies`,
  `spec.workloadSecurityContext`, `spec.adminUrl`). Rejected: ages
  badly, contests top-level CRD surface, distributes the
  upgrade-safety rule across three unrelated reconcile paths.
- **Admin URL randomization under `spec.security`** (full
  umbrella). Rejected: confuses Magento devs looking for the field
  on `AdminSpec`; adds an asymmetric bootstrap-time field to an
  otherwise reconcile-time policy group.
- **Hard cutover on upgrade — apply defaults to existing stores.**
  Rejected: silently restarts pods, breaks bookmarks, may break
  customer NetworkPolicy customizations. The operator does not get
  to decide that for the user.
- **Annotation-driven opt-in** (e.g.
  `mage.aknostic.com/security-baseline: enabled`). Rejected:
  per-item opt-in is finer-grained — a customer may want
  NetworkPolicies but not workload non-root because their own image
  is not ready. Three booleans on the CRD are the right shape; an
  annotation is cruder.
- **Split into v0.7.0 (NetworkPolicies) + v0.8.0
  (securityContext) + v0.9.0 (admin URL).** Rejected per the brief:
  bundling lets the upgrade-safety rule be designed once, and
  "security baseline" is a more legible release frame than three
  uncoordinated minor features.

## Open questions / future work

- **Per-tier securityContext overrides.** Frontend may need to
  differ from admin (different UID conventions, different
  capability needs). Deferred until a real ask.
- **FQDN-based egress policies** (Cilium CiliumNetworkPolicy).
  Standard `networking.k8s.io/v1` is the lowest-common-denominator;
  FQDN is mesh-specific and platform-conditional.
- **Admin URL rotation API.** Currently the value is generated
  once at bootstrap and sticky thereafter. Rotation is documented
  as redeploy + manual `bin/magento config:set`. Future: a
  `rotateBackendFrontname: true` annotation that triggers
  Job-driven rotation.
- **AppArmor / SELinux profiles.** Platform-conditional, not
  portable. v1.0.0+ if at all.
- **2FA enforcement defaulting.** Separate discussion from the
  v0.7.0 baseline; lives under `spec.security.adminAuth` if it
  lands at all.

## References

- ADR 005 — bootstrap as Job (defines the `bin/magento setup:install`
  invocation that `BackendFrontname` plumbs through).
- ADR 010 — public-edge TLS (defines `spec.routing.tls`, used by
  the admin URL log message and consumed by the upgrade-safety
  defaulting check).
- ADR 012 — SMTP configuration (writes to `<store>-config` Secret;
  the upgrade-safety check shares the artefact).
- ADR 013 — base-URL derivation (shares the hash-driven re-run
  pattern that future security ensure-functions can reuse for
  policy churn).
- Adobe Commerce hardening guide — randomize admin URL is listed
  alongside 2FA and IP allowlisting as a baseline.
- In-cluster TLS-to-clients deferral (see CHANGELOG) — this ADR's scope is
  segmentation now, encryption later.

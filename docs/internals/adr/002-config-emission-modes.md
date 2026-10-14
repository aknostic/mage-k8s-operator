# ADR 002 — Config emission: envvars only (ecetools deferred)

- **Status:** Amended 2026-04-24 — envvars-only; ecetools path deferred pending demand (see Amendment below). Originally proposed 2026-04-22 as a dual-mode ADR (envvars default + ecetools opt-in).
- **Date:** 2026-04-22 (original) / 2026-04-24 (amendment)
- **Deciders:** @jurg
- **Related:** the original project plan (unpublished); ADR 001 (kubebuilder-over-operator-sdk) not yet written

## Amendment (2026-04-24)

**Decision:** drop the `ecetools` emission mode from Phase 1
entirely. The operator ships `envvars` mode unconditionally; no
`spec.config.mode` field on the CRD, no mode-switch branch in
`ensureSecrets`, no `MAGENTO_CLOUD_RELATIONSHIPS` / `_ROUTES` /
`_VARIABLES` JSON emission.

**Why:** ECE-Tools is an Adobe Commerce Cloud construct, and the
operator's positioning is specifically "traditional Magento /
Mage-OS / Adobe Commerce *not* on Adobe Cloud" — the target
audience runs on Hetzner, Scaleway, bare metal, self-hosted
Kubernetes. Shops migrating *from* Adobe Cloud are a narrow
slice, and asking them to strip out ECE-Tools in exchange for
moving to this operator is a one-time migration cost the
operator shouldn't bend v0.1.0 scope for. Keeping the dual-mode
shape would cost a week of v0.1.0 work (CRD field + CEL
validator + mode-switch in ensureSecrets + ecetools JSON
emission + per-mode image-contract validation Job + tests for
both paths) with no current user.

**When to revisit:** clear user demand
for Adobe-Cloud migration support. Re-adding the ecetools path
is ~1 week of work and preserves the `envvars` default exactly
as it is today. The door stays open; we just don't pay to keep
it staffed.

**What this deprecates from the sections below:**

- The `spec.config.mode` field is NOT added to the CRD. No CEL
  enum validator; the field simply doesn't exist.
- `docs/image-contract.md` documents only the envvars namespace;
  the ecetools image-contract section is removed.
- `internal/magento/payload.go` may still carry the
  Relationships/Routes/Variables intermediate representation —
  it's useful plumbing for emitting envvars-mode env vars too,
  and re-adding ecetools later is easier if the intermediate
  stays in place. No action to remove it.
- The Rejected alternatives section below stays as written —
  "ecetools-only" and "operator emits env.php directly" are
  still rejected, they haven't been reversed.

Below this Amendment, the original dual-mode ADR is preserved
unchanged for the reasoning trail.

---

## Context

The plan states, as Lesson 1: *"Use ECE-Tools, not raw env.php generation."* The pitch is that ECE-Tools eliminates the version-specific env.php generator problem by accepting three base64-encoded JSON env vars (`MAGENTO_CLOUD_RELATIONSHIPS`, `MAGENTO_CLOUD_ROUTES`, `MAGENTO_CLOUD_VARIABLES`) and materialising `env.php` itself. PHOENIX-MEDIA's production Helm chart follows this pattern, and it's standard for any shop that runs or used to run on Adobe Commerce Cloud.

On re-reading with help from a focused investigation, the "ECE-Tools is a pure win" framing glosses over a real cost for a non-trivial slice of the operator's likely user base:

1. **ECE-Tools is a Composer dependency that the customer image MUST include.** Shops coming from Adobe Cloud already ship it. Shops on traditional hosting (Hetzner, Scaleway, bare metal) may not — and adding it is a code change to their deploy pipeline, not just a container swap. The operator pitch is "zero-migration Magento on Kubernetes"; mandating ECE-Tools breaks that pitch for traditional-hosting shops.
2. **ECE-Tools was designed for Adobe Cloud specifically.** Some of its behaviour (wizards, build hooks, directory-layout assumptions) is Cloud-native. Running it elsewhere works but uses ~20% of its surface.
3. **A well-proven alternative exists.** Click & Mortar's `magento-kubernetes` project emits individual environment variables (`MAGENTO_DATABASE_HOST`, `MAGENTO_REDIS_HOST`, …) and relies on the customer's `env.php` using `getenv()` to wire them in. No ECE-Tools dependency. The operator contract becomes a stable env var namespace instead of a JSON schema.

The plan's original concern — "don't make the operator own a version-specific env.php generator" — is satisfied by **either** path. In ECE-Tools mode, Adobe owns the mapping. In env-vars mode, the customer's `env.php` owns the mapping (one-time edit, stable across Magento minor versions for the fields we care about).

## Decision

**The operator supports two config emission modes, selected per MageStore via `spec.config.mode`.**

```yaml
spec:
  config:
    mode: envvars  # envvars | ecetools — default: envvars
```

- **`envvars` (default).** Operator emits a Secret containing individual env vars: `MAGENTO_DATABASE_HOST`, `MAGENTO_DATABASE_PORT`, `MAGENTO_DATABASE_NAME`, `MAGENTO_DATABASE_USER`, `MAGENTO_DATABASE_PASSWORD`, `MAGENTO_REDIS_CACHE_HOST`, `MAGENTO_REDIS_SESSION_HOST`, `MAGENTO_OPENSEARCH_HOST`, `MAGENTO_RABBITMQ_HOST`, `MAGENTO_BASE_URL`, `MAGENTO_ADMIN_URL`, plus cryptographic material. Customer image consumes via `getenv()` in `env.php`.
- **`ecetools`.** Operator emits a Secret containing `MAGENTO_CLOUD_RELATIONSHIPS`, `MAGENTO_CLOUD_ROUTES`, `MAGENTO_CLOUD_VARIABLES` — three base64-encoded JSON blobs matching the Magento Cloud convention. Customer image runs ECE-Tools at startup.

Both modes consume the same internal `magento.Relationships` / `magento.Routes` / `magento.Variables` structures (see `internal/magento/payload.go`). Only the emission differs.

## Consequences

**Positive**
- Operator works for both Adobe-Cloud-alumni shops and traditional-hosting shops without forcing a migration either way.
- Default is the lighter, less-assumption path (`envvars`). ECE-Tools becomes opt-in for shops that want it.
- The customer image contract for `envvars` mode is short, stable, and does not depend on upstream Adobe packaging decisions.
- Image-contract validation (separate Phase-1 item) can tailor its check per mode: ECE-Tools mode validates `composer show magento/ece-tools`; env-vars mode validates that `env.php` references the expected variables.

**Negative**
- Two emission paths = two integration-test surfaces. Each Magento version matrix must be validated in both modes (double the testcontainers work).
- Cryptographic material (`CRYPT_KEY`, admin password hashes, etc.) has slightly different handling per mode; the operator must canonicalise its generation to keep the two paths in sync.
- Multi-store routing (`spec.stores`) generates different outputs per mode — ECE-Tools uses `MAGENTO_CLOUD_ROUTES`, env-vars uses an nginx ConfigMap reading `MAGE_RUN_CODE` env vars. Both are already in the plan, but the coupling to `config.mode` needs to be explicit.

**Neutral**
- The `internal/magento` package stays single-source-of-truth for the intermediate representation; only the ensure-step that emits the final Secret branches on `mode`.

## Image contract implications

- **`envvars` mode image must include**: an `env.php` that uses `getenv('MAGENTO_DATABASE_HOST')` etc. to wire in operator-provided env vars. Operator will publish a reference `env.php` snippet in `docs/image-contract.md`.
- **`ecetools` mode image must include**: the `magento/ece-tools` Composer package and a startup hook that lets ECE-Tools materialise `env.php` before php-fpm accepts traffic.

## Rejected alternatives

1. **ECE-Tools-only (original plan).** Rejected because it silently excludes traditional-hosting shops from the operator's usable user base.
2. **Direct-env-vars-only (strict Click & Mortar).** Rejected because Adobe-Cloud-alumni shops would have to strip out their existing ECE-Tools integration, which is a migration the operator should not force.
3. **Operator emits `env.php` directly.** Rejected by Lesson 1 of the plan. Version-specific serialisation is not a job for the operator; the image is the right level for that responsibility.

## Open questions

- **Credential origin.** ~~Both modes need~~ (see Amendment above — only envvars remains) The envvars path needs database credentials. mariadb-operator generates a Secret with user/password; the operator watches it and re-emits on rotation. The *exact* Secret key names and Magento credential-reuse semantics are settled by ensureDatabase (Slot 17) and ensureSecrets (Slot 18).
- ~~**Default mode.**~~ Moot per the Amendment — envvars is the only mode.

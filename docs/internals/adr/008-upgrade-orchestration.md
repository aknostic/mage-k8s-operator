# ADR 008 — Upgrade orchestration via `ensureUpgrade` Job + Service-selector maintenance flip

- **Status:** Accepted
- **Date:** 2026-04-26
- **Deciders:** @jurg
- **Related:** extends ADR 005 (one-shot Job pattern, status-condition gate);
  fills the Phase-2 / v0.2.0 frame of the original project plan;
  consumes the image-contract surface defined in `docs/image-contract.md`.

## Context

Magento ships schema and data migrations as part of every minor and patch
release. The canonical sequence is `bin/magento maintenance:enable →
setup:upgrade → cache:flush → maintenance:disable`. `setup:upgrade` is
not transactional: it iterates over every module's `Setup/Patch/Schema`
and `Setup/Patch/Data` patches and commits each as it runs. If the
seventh patch of twenty-three fails, patches one through six are already
in the database. There is no rollback.

The operator's job is to orchestrate this declaratively: when
`spec.image.tag` changes, drive the cluster from "old image, old schema"
to "new image, new schema" without serving traffic against an
intermediate state and without leaving consumers or cron pods writing
into a half-migrated database.

v0.1.x already solved the closest analogue. `ensureBootstrap` (ADR 005)
runs `setup:install` as an operator-owned, one-shot Job and gates
downstream reconcile on `StoreInstalled=True`. Upgrade orchestration is
the second instance of the same pattern: a long-running, destructive,
non-idempotent command that must run exactly once per arc, must surface
status, and must not block frontend pod startup. The differences from
bootstrap are that the cluster is *already running* when an upgrade
starts (so traffic-fencing matters), and that an upgrade has more moving
parts to quiesce — consumers, cron, frontend/admin/api workloads.

Three pieces of state in the existing codebase line up with this work
and the brief flagged each of them. `MageStorePhase` defines `Upgrading`
at `api/v1alpha1/magestore_types.go:1060` but no reconciler reads or
writes it. `status` has no field tracking which image tag was last
successfully upgraded to. `spec` has no `upgrade` stanza. Those are the
three surfaces this ADR adds.

The operator's audience — traditional-hosting Magento shops on
self-managed Kubernetes — pushes the design toward "safe by default,
zero ambiguity in the failure path." Auto-retry on a destructive
operation is the wrong default; partial-rollout-during-migration is the
wrong default; "site comes back broken because admin reverted the tag"
is a foot-gun the operator can prevent. Each decision below leans into
that posture.

## Decision

`ensureUpgrade` is a new reconciler step. Trigger condition is
`status.observedImageTag != ""` AND
`status.observedImageTag != spec.image.tag` (or the same comparison on
`spec.image.repository` — both flow through the same path because the
operator does not interpret registry identity, only image content).
`status.observedImageTag == ""` falls through to the install path
(`ensureBootstrap`) unchanged, so install-vs-upgrade is distinguished
without a mode flag.

The upgrade arc, end to end:

1. Detect drift; set `status.phase = Upgrading` and condition
   `UpgradeReady=False, Reason=UpgradeStarting`.
2. Materialise `<store>-maintenance` Deployment (one-replica nginx
   serving a 503 page) and Service. Wait for the Deployment to reach
   AvailableReplicas before any traffic flip.
3. Patch the `frontend`, `admin`, and `api` Service selectors to match
   the maintenance pod's labels. Kubernetes reconciles Endpoints
   automatically; ingress traffic now lands on the maintenance pod.
4. Suspend every owned CronJob (`spec.suspend = true`) and wait up to
   five minutes for any actively-running cron pods to terminate. If
   the timeout expires, fail with
   `UpgradeFailed=True, Reason=CronQuiesceTimeout` and stop — admin
   diagnoses the long-running cron, kills it, and re-enters the path
   by deleting the failed upgrade Job.
5. Scale every consumer Deployment to `replicas: 0` and wait for pods
   to terminate (Magento consumers do not handle SIGTERM cleanly, so
   the existing 60s `terminationGracePeriodSeconds` applies).
6. Materialise `<store>-upgrade-<from-tag>-<to-tag>` Job. The Job pulls
   the new image, mounts the same config Secret as the workload pods,
   and runs `bin/magento setup:upgrade && bin/magento cache:flush` in
   one shell. `BackoffLimit: 1`, matching `ensureBootstrap`. The
   `<from>-<to>` suffix in the name is human-readable (so
   `kubectl get jobs` tells the admin which arc the Job belongs to);
   uniqueness comes from `<store>-upgrade-` being one-per-store at any
   point in time.
7. On Job success: roll the workload Deployments forward (the existing
   `ensureWorkloads` step does this naturally on next reconcile because
   the image tag in the Pod spec annotation differs); wait for the new
   pods to reach `AvailableReplicas`. The window between "new pods
   Ready" and "Service selectors restored" is intentionally kept
   non-zero so an admin can verify the upgrade via
   `kubectl port-forward deployment/<store>-frontend 8080:8080` —
   port-forward bypasses the Service selector flip. Once the workload
   roll is complete, restore consumer replicas, unsuspend CronJobs,
   restore Service selectors, exec a Varnish ban
   (`varnishadm ban "obj.http.X-Magento-Tags ~ .*"`) when
   `spec.varnish.enabled` is true, delete the maintenance Deployment,
   advance `status.observedImageTag` to `spec.image.tag`, and set
   `phase = Running` with `UpgradeReady=True`.
8. On Job failure: set `phase = Failed`,
   `UpgradeFailed=True, Reason=SetupUpgradeFailed`. Stay quiesced.
   Maintenance Service flip stays in place; consumer replicas stay at
   zero; CronJobs stay suspended. The admin diagnoses the Job's logs,
   restores the database from out-of-band backup if the failure left
   the schema in an unusable state, then deletes the failed Job. The
   operator notices the deletion on next reconcile and re-evaluates
   the upgrade trigger; if `spec.image.tag` still differs from
   `observedImageTag`, a fresh Job is created against current spec.

The maintenance backend uses Service-selector flip rather than
Magento's native `var/.maintenance.flag` mechanism because the flag is
a local-filesystem signal: the upgrade Job pod writes it on its own
filesystem, and frontend pods (running on different nodes, with their
own filesystems) never see it. Making the native flag work would
require either an RWX volume shared between the Job and every workload
pod (the project explicitly avoids RWX for the same reasons that
`spec.media.s3` is the recommended media path) or a sidecar
ConfigMap-watcher per workload pod. The Service-selector flip is the
Kubernetes-native equivalent: it produces the same user-visible effect
(a 503 page) without the filesystem coupling, reuses the existing
Service surface, and works regardless of how many `spec.stores[]`
hostnames are involved (every host already routes through the same
Service, so flipping the selector flips them all simultaneously).

The CRD adds `spec.upgrade.maintenance` with two fields:

```yaml
spec:
  upgrade:
    maintenance:
      enabled: true                      # default
      htmlConfigMapRef:                  # optional override of the 503 page
        name: my-custom-503
        key: index.html                  # default
```

`enabled: true` is the default (opt-out). The opt-out exists for shops
that genuinely run blue-green or canary upgrades and explicitly accept
the integrity risk of mid-migration writes; for everyone else the
default is correct because mid-migration writes against a
non-transactional `setup:upgrade` produce silent data corruption that
no amount of post-hoc cache flushing will heal. `htmlConfigMapRef` is
optional — the operator ships a sensible default 503 page baked into
the maintenance Deployment's static config, and shops that want their
own branded "we'll be right back" page point at a ConfigMap with a
single HTML key.

Status surfacing adds two new fields and one new condition. New:
`status.observedImageTag` (string) — the image tag the schema is
currently consistent with; advances on Job success, never on
Job failure. New: condition `UpgradeReady` — True when
`observedImageTag == spec.image.tag` AND workload pods reach
`AvailableReplicas`, False during any in-flight upgrade or after a
failed upgrade, with a `Reason` field that names the specific stage
(`UpgradeStarting`, `CronQuiesceTimeout`, `SetupUpgradeFailed`,
`WorkloadRolloutPending`, etc.). The existing `MageStorePhase`
constant `Upgrading` is now load-bearing — the reconciler sets it on
upgrade-arc entry and clears it on successful exit.

The image contract gains one explicit requirement: `setup:di:compile`
and `setup:static-content:deploy` MUST be baked into the image at
build time. The operator does not run them at runtime. This is not a
limitation; it is the correct design. Both commands write to the
codebase (`generated/code`, `pub/static`) and require write access to
parts of the filesystem that production-mode images keep read-only as
a hardening measure. Running them at runtime would defeat the
read-only filesystem posture, add minutes to every upgrade window, and
force every workload pod to either share writable storage or recompile
independently. Build-time is where this work belongs;
`docs/image-contract.md` is updated to spell this out, and an early
phase of `ensureUpgrade` will eventually grow a sanity check that
fails the upgrade with `Reason=ImageContractViolation` if the new
image's `generated/code` directory is empty (deferred to v0.2.0
implementation, not blocking on this ADR).

Failure handling is forward-only. The operator does not auto-rollback
because there is nothing to roll back to: pre-upgrade backup is the
domain of the v0.4.0 `MageBackup` CRD, and without that snapshot any
"rollback" would just be re-running an older `setup:upgrade` against a
newer schema, which fails or succeeds in a half-broken way. Admins who
need to recover from a failed upgrade restore the database from
out-of-band backup AND revert `spec.image.tag` in lockstep. The
operator enforces the lockstep half by refusing to un-quiesce while
`UpgradeFailed=True`, regardless of whether `spec.image.tag` is
subsequently changed; this prevents the foot-gun where an admin
reverts the tag, the operator sees `tag == observedImageTag`, decides
no upgrade is needed, restores Service selectors, and the site comes
back broken because the schema is half-migrated to a version the
restored image does not understand. The unstick mechanism is identical
to ADR-005's bootstrap-failure unstick: delete the failed Job, and the
operator re-evaluates from current spec.

Concurrency has two flavours. Reconcile-loop concurrency (multiple
reconciler invocations against the same MageStore in flight at once)
is handled by deterministic Job naming and the get-or-create pattern
ADR-005 already established: a second reconcile finds the existing
Job by name and inspects its status rather than creating a duplicate.
User concurrency (admin edits `spec.image.tag` mid-upgrade) is handled
by inspect-only-while-Upgrading: while `phase == Upgrading` the
operator looks at the existing Job's status but does not consider new
spec drift. When the in-flight Job terminates and `observedImageTag`
advances on success (or stays put on failure), the next reconcile
naturally sees any residual drift and either kicks off a second
upgrade arc against the new tag or stays quiesced if `UpgradeFailed`
is set. There is no cancellation of an in-flight Job; setup:upgrade
is destructive and the "stop halfway" state is worse than "complete
against the original tag and then start a second arc."

Multi-store is invisible to `ensureUpgrade`. `spec.stores[]` is a list
of `(host, MAGE_RUN_CODE, MAGE_RUN_TYPE)` triples that share a single
codebase, database, and frontend Deployment (per ADR 007). One
`setup:upgrade` per MageStore CR — there is exactly one schema and one
codebase to migrate, regardless of how many hostnames the storefront
exposes. The Service-selector flip applies once and covers every host
behind the frontend Service simultaneously.

The orchestrator is **version-agnostic**. The same arc handles a
patch-level bump (2.4.8 → 2.4.8-p1), a minor bump (2.4.7 → 2.4.8), and
a major-version bump (2.3.x → 2.4.x). What differs across major bumps
is image contract — PHP version compatibility, Composer dep updates,
search-engine compatibility (2.4 dropped MySQL/Solr support and
requires Elasticsearch/OpenSearch), RabbitMQ schema rewrites — and
those are the user's responsibility per the existing image contract.
If the user's new-version image is internally consistent and
`setup:upgrade` succeeds against the populated DB, the orchestrator
works the same way regardless of how big the version delta is. If
`setup:upgrade` fails because a third-party module's upgrade script
is incompatible with the new Magento version, the operator's
failure-handling kicks in cleanly; that failure mode is image quality,
not orchestrator quality. Per-major-version migration hooks (e.g.
"run X before setup:upgrade when upgrading from 2.3 to 2.4") are out
of scope for v0.2.0; if a future Magento release ships migrations
that genuinely need orchestrator-mediated pre-steps, a v0.3.0+
`spec.upgrade.hooks[]` field can land later without redesigning this
ADR.

## Validation

The test pyramid for v0.2.0 is layered, with each layer validating a
distinct property:

**Layer 1 — envtest (in-CI, every PR).** Reconciler logic against a
fake apiserver: state-machine transitions, Job-name idempotency,
status-condition surface, get-or-create concurrency, the failure
path's enforced-block behaviour. Most of `ensureUpgrade`'s bugs will
live here. This is where `Reason=CronQuiesceTimeout` and
`Reason=SetupUpgradeFailed` paths are covered without paying for a
real cluster.

**Layer 2 — kind e2e with `setup:perf:generate-fixtures --profile=small`
(in-CI nightly + on tag).** Brings up a kind cluster with
mariadb-operator and opensearch-operator, applies a MageStore CR,
populates the DB with the perf-toolkit small profile (800 simple +
16 configurable products, 200 customers, 80 orders, 30 categories) via
a one-off Pod, then triggers an upgrade by patching `spec.image.tag`
to a different reference-image version. Verifies: the maintenance
flip serves a 503 on the public Ingress; port-forward to the frontend
pod reaches whatever pods are currently running (verifying the
verification-window contract holds); `phase=Upgrading` then `Running`;
`observedImageTag` advances; `UpgradeReady=True`; the storefront
serves successfully against the migrated DB; admin login works
against migrated `customer_*`/`admin_user` tables. Includes a
deliberate-failure path test (an upgrade Job pointed at a known-broken
image) to confirm `UpgradeFailed=True` halts the orchestrator with
quiesce in place. The perf-toolkit small profile is the right size
for CI: 800 SKUs and 80 orders is enough to exercise the migration
paths schema migrations actually care about (`catalog_*`, `customer_*`,
`sales_*`, `eav_*`) without the runtime cost of larger profiles.

**Layer 3 — manual pre-tag sweep with
`setup:perf:generate-fixtures --profile=medium` (manual checkpoint
before tagging v0.2.0).** Same flow as Layer 2 but with the medium
profile (24,000 simple products, 50,000 orders) — large enough for
DDL-on-large-row-count behaviours to surface (`sales_order_grid`
re-indexing, FK changes against populated child tables). Run once
locally before tagging; document the recipe in
`docs/internals/releasing.md`. Not in CI because the medium profile
takes 30-60 minutes to populate.

**Layer 3b — local realistic-shop bridge (Mage-OS sample data).** When
a real-infra environment is unavailable, the Mage-OS sample-data fork
(`mage-os/mageos-magento2-sample-data`, ~250 products including ~200
configurables, real images, Luma theme, real CMS pages) provides a
local proxy on kind. Validates storefront-realistic upgrade behaviour:
theme assets after upgrade, URL rewrites with real catalog data,
frontend rendering with real product images, customer login flow
against populated tables. Requires the `mage-os/sample-data` Composer
dep in the reference image, or installing it via Composer in a one-off
Pod that mounts the codebase volume. Same kind harness as Layer 2.

**Layer 4 — real-infra real-shop validation (parallel
track, mirrors v0.1.0 model).** A real Kapsule or EKS
cluster with real S3-backed media, running an upgrade against a realistic,
long-lived shop (third-party
modules, custom UpgradeData scripts, historical EAV cruft, multi-year
order tables). Validates "does this work against a real shop with a
real third-party-module mix" — the question Layers 1-3b cannot answer
because they all run against stock Magento. Not gated on for v0.2.0
tag, mirroring v0.1.0; this track surfaces issues
asynchronously and feeds v0.2.x patches.

This raises the v0.2.0 acceptance bar above v0.1.0. v0.1.0 was
"demo-runnable on kind, runtime-validation tracked in parallel." v0.2.0
is "scripted populated-data e2e in CI plus a manual large-volume sweep
before tag, real-infra realism in parallel." The bar is higher because
the upgrade path is the most corruption-prone surface in the operator;
shipping it without proving that schema migrations succeed against
populated data would be irresponsible.

## Consequences

**Positive**

- The arc reuses ADR-005's pattern wholesale: one-shot Job, owner
  reference, condition gate, get-or-create idempotency, manual unstick
  on failure. New ground covered in this ADR is mostly choreography
  (which step runs when, what gets quiesced, what gets restored), not
  novel reconciler primitives.
- Status surfacing distinguishes install from upgrade cleanly via
  `observedImageTag`. No mode flag needed; the empty-string sentinel
  for "not yet installed" falls through to `ensureBootstrap`
  naturally.
- Service-selector flip implements the maintenance window without
  RWX storage, without admission webhooks, and without per-pod
  sidecars. It also works uniformly across multi-store deployments
  because the selector flip happens at the Service level, above the
  per-host routing.
- The verification window between "new pods Available" and "selectors
  restored" is intentional and gives admins a cheap way to validate
  the upgrade (`kubectl port-forward`) before public traffic resumes.
  No additional CRD field needed; port-forward is K8s-native and
  already works.
- The orchestrator is version-agnostic: same arc for patch, minor,
  and major version bumps. Major-version compatibility is the user's
  image-contract problem, which is the right place for it.

**Negative**

- The maintenance window is binary in v0.2.0: either every host is in
  503 or none are. Per-store maintenance (e.g. "upgrade staging hosts
  while keeping production hosts live") is not supported. Shops
  running a true blue-green deployment opt out of maintenance entirely
  (`enabled: false`) and accept the integrity risk; staged rollouts
  per host land in v0.5.0 production-hardening or later.
- Failed upgrades require admin intervention to unstick. Auto-retry on
  a partially-succeeded `setup:upgrade` would re-run patches that
  already committed and likely fail differently the second time;
  intentional admin-in-the-loop is the right default but does mean
  failed upgrades stretch downtime until someone notices.
- The operator does not back up before upgrading. Users back up out of
  band today (their database operator's snapshot mechanism, or
  `mariadb-operator`'s `Backup` CR, or `mysqldump` from a sidecar).
  v0.4.0's `MageBackup` integration will lift this; until then,
  this remains a documented gap.
- Cross-major version bumps work mechanically but the orchestrator
  cannot validate that the user's image's PHP/search/Composer
  compatibility matches the new Magento version. A user pointing the
  CR at an image with PHP 7.4 alongside Magento 2.4.7 will see
  `setup:upgrade` fail at runtime with a PHP-version error; the
  orchestrator's failure-handling catches the failure cleanly but
  cannot prevent it. Image-contract validation at upgrade entry
  (deferred) would tighten this loop.
- `setup:upgrade` is non-transactional. When a partial-migration state
  is left after failure, no operator-side mechanism recovers it. The
  documented recovery path is "restore DB from out-of-band backup,
  delete the failed Job, retry."

**Neutral**

- The `<store>-maintenance` Deployment + Service are operator-owned
  and exist only during the upgrade arc. ~10MiB-RAM nginx pod for the
  duration of the arc; deleted on successful exit. They do not show
  up in `kubectl get all -n <ns>` in steady state.
- One additional CRD field group ships with v0.2.0 (`spec.upgrade`).
  v1alpha1 surface remains additive; no existing field changes shape.
- The maintenance Deployment's default 503 HTML is operator-baked;
  custom branding via `htmlConfigMapRef` is optional and adds ~5
  lines of mount logic to the Deployment template.

## Open questions

- **Image-contract validation at upgrade entry.** A pre-upgrade Job
  step that exec's `php --version` and `ls generated/code/Magento`
  against the new image and fails fast with
  `Reason=ImageContractViolation` would catch the most common
  cross-major foot-gun (PHP version mismatch, missing build-time DI
  compilation) before the destructive `setup:upgrade` runs. Out of
  scope for the ADR's first cut; revisit when v0.2.0 implementation
  surfaces real-world failures the check would have prevented.
- **Per-major-version migration hooks.** Some Magento major bumps
  ship migration steps that fall outside `setup:upgrade` (RabbitMQ
  exchange rewrites, marketplace key rotations, search-engine
  config changes that need to land before `setup:upgrade` rather
  than after). Today these are documented in Magento's release notes
  and applied manually. A future `spec.upgrade.hooks[]` with
  `before` / `after` Job templates per from-version pair would let
  the operator orchestrate them declaratively. Defer until a
  specific upgrade path actually requires it; designing the hook
  surface speculatively bloats the v0.2.0 contract.
- **Header-bypass / IP-whitelist for external verification.** During
  maintenance, `kubectl port-forward` is the v0.2.0 verification
  path. Shops with multi-person ops teams whose verifiers do not
  have kubectl access need a header-token bypass on the maintenance
  pod proxying selectively to the upgraded frontend. v0.5.0
  production-hardening territory; out of scope here.
- **Admission webhook for `spec.image.tag` immutability during
  upgrade.** Today the operator handles mid-upgrade tag edits by
  ignoring them until the in-flight Job terminates. A webhook
  rejecting the edit at admission time produces a clearer admin
  experience but adds a webhook to v0.2.0; the brief defers
  webhooks to v0.5.0 and that decision holds.
- **Auto-rollback on first failure.** Depends on `MageBackup`
  (v0.4.0) for the snapshot to roll back to. Once `MageBackup`
  ships, an opt-in `spec.upgrade.autoRollbackOnFailure: true` could
  drive the recovery flow declaratively. Out of scope for v0.2.0.

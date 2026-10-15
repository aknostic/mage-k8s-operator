# Upgrading a MageStore

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

A `MageStore`'s upgrade arc is driven by changes to
`spec.image.tag`. The operator orchestrates the migration
end-to-end: it serves a 503 page during the schema migration,
runs `bin/magento setup:upgrade` as a one-shot Job, restores
traffic when the new pods are healthy, and ships a Varnish
ban to flush stale cache.

For the design rationale, see
[ADR 008](../internals/adr/008-upgrade-orchestration.md).
For the CRD field surface, see the
[`spec.upgrade` section of magestore-reference.md](../magestore-reference.md#specupgrade).

> **Status:** upgrade orchestration is a v0.2.0 deliverable. The
> page below is the skeleton; sections marked TODO are filled in
> as the v0.2.0 implementation lands. Track v0.2.0 progress in
> [CHANGELOG.md](../../CHANGELOG.md).

## Triggering an upgrade

You upgrade a Magento installation by editing the MageStore's
`spec.image.tag`:

```bash
kubectl patch magestore store --type merge -p '{"spec":{"image":{"tag":"2.4.8-p1"}}}'
```

The operator detects the drift between `spec.image.tag` and the
schema-baseline tag stored in `status.observedImageTag`, and
enters the upgrade arc. For a fresh install (no
`observedImageTag` baseline yet), `bin/magento setup:install`
runs once via [ensureBootstrap](../internals/adr/005-bootstrap-as-job.md)
and that completion stamps the initial baseline; the upgrade arc
only fires from the second tag change onward.

## What happens during an upgrade

The arc walks through these stages in order. With
`spec.upgrade.maintenance.enabled: true` (the default), the
storefront serves a 503 page from "selectors flipped" through
"selectors restored." Approximate timings assume a
small-catalogue store; large catalogues with long
`setup:upgrade` runtimes scale stage 5 accordingly.

1. **Detect drift.** Reconciler observes
   `spec.image.tag != status.observedImageTag`. Sets
   `phase = Upgrading`, surfaces `UpgradeReady=False,
   Reason=UpgradeStarting`. _< 1 second._
2. **Stand up maintenance.** Materialises
   `<store>-maintenance` Deployment (one nginx pod serving the
   503 page), Service, and supporting ConfigMap(s). Waits for
   the Deployment to reach Available. _10-30 seconds._
3. **Flip Service selectors.** Patches the `<store>-frontend`,
   `<store>-admin`, `<store>-api` Service selectors to point
   at the maintenance pod. From this moment, public traffic
   sees the 503 page. The
   `mage.aknostic.com/maintenance-flipped` annotation on each
   Service marks it as fenced and prevents the next reconcile
   of `ensureWorkloadService` from reverting the flip. _< 1
   second._
4. **Quiesce cron + consumers.** Suspends every owned CronJob
   (`spec.suspend = true`); waits up to 5 minutes for
   actively-running cron pods to terminate (timeout surfaces
   `Reason=CronQuiesceTimeout`); scales consumer Deployments
   to `replicas: 0` after snapshotting their original replica
   counts to a `mage.aknostic.com/upgrade-saved-replicas`
   annotation per Deployment. _Up to 5 minutes; usually
   seconds._
5. **Run the upgrade Job.** Creates
   `<store>-upgrade-<from>-<to>` running `bin/magento
   setup:upgrade && bin/magento cache:flush` against the new
   image. Surfaces `Reason=WorkloadRolloutPending` while it
   runs. _Minutes to tens of minutes depending on catalogue
   size and the number of patches between versions._
6. **On Job success — stage 1.** Advances
   `status.observedImageTag` to the new tag, restores cron
   (un-suspend) and consumers (replicas snapshot annotation),
   surfaces `Reason=WorkloadRolloutPending`. The maintenance
   fence stays in place. The pipeline now continues past
   `ensureUpgrade` so `ensureWorkloads` can roll the workload
   Deployments forward to the new image. _< 1 second of
   reconciler work; minutes for the workload rollout._
7. **On Job success — stage 2.** Once the new workload pods
   reach Available, the operator restores Service selectors
   (clears the `maintenance-flipped` annotation), deletes the
   maintenance Deployment + Service + ConfigMap (cascade via
   OwnerReference), and surfaces `UpgradeReady=True,
   Reason=UpgradeComplete`. Public traffic resumes against the
   new image and migrated schema. _< 1 second._
8. **On Job failure.** Sets `phase = Failed` and
   `UpgradeReady=False, Reason=RetryingFailedJob`, then deletes
   and recreates the Job after a growing wait (30s, 2m, 5m, 10m,
   30m, ±25 %). After five failed attempts it reports
   `Reason=PermanentlyFailed` and stops retrying. Throughout,
   cron stays suspended, consumers stay at zero, the maintenance
   fence stays in place, and `Ready` reports the same Reason.
   The operator refuses to un-quiesce until the failed Job is
   deleted — see [Recovering from a failed
   upgrade](#recovering-from-a-failed-upgrade). Whether the
   automatic retries fit that contract is tracked in [#22](https://github.com/aknostic/mage-k8s-operator/issues/22).

The window between stage 7's "new pods Available" and "Service
selectors restored" is intentionally non-zero — admins can use
`kubectl port-forward` against the workload Deployments to
verify the upgrade before public traffic resumes (see the
verification section below; full content lands with v0.2.0).

> **Varnish cache:** stale cache entries served by Varnish are
> evicted on TTL after the arc completes — the operator does
> not yet exec a `varnishadm ban` to flush proactively. Tracked
> for a v0.2.x patch; not a correctness gap because the
> migrated schema's responses simply replace older cached ones
> as TTL elapses.

## Reason values during an upgrade

The `UpgradeReady` condition's `Reason` field tells you what
phase the arc is in.

| Reason | Status | Meaning | Expected duration |
|---|---|---|---|
| `UpToDate` | True | Steady state — `observedImageTag == spec.image.tag` and workloads Available. | indefinite |
| `AwaitingUpgrade` | Unknown | Fresh-install path; no baseline yet. ensureBootstrap is the gate. | until first install completes |
| `UpgradeStarting` | False | Arc just entered; maintenance fence standing up or cron quiesce in progress. | seconds, or up to 5 minutes if active cron pods are draining |
| `WorkloadRolloutPending` | False | The upgrade Job is running, OR the Job completed and workload pods are rolling forward to the new image. | minutes; longer for large catalogues or many patches |
| `UpgradeComplete` | True | Arc finished successfully; workloads Available, fence torn down. Replaced by `UpToDate` on the next reconcile. | transient |
| `RetryingFailedJob` | False | The `setup:upgrade` Job failed; the operator recreates it after a backoff (the message says when). The store stays fenced. | up to ~47 minutes over five attempts |
| `PermanentlyFailed` | False | The Job failed five times; the operator stops retrying and keeps the failed Job for `kubectl logs`. The schema may be mid-migration. **Admin intervention required** — see [Recovering from a failed upgrade](#recovering-from-a-failed-upgrade). | indefinite until unstuck |
| `CronQuiesceTimeout` | False | Active cron pods refused to terminate within 5 minutes. **Admin intervention required** — kill the long-running cron pod, then delete any partial upgrade Job, then reconcile re-evaluates. | indefinite until unstuck |

`Reason=UpgradeComplete` exists for a single reconcile pass —
once the operator observes that the maintenance fence is fully
torn down and `observedImageTag == spec.image.tag`, the next
reconcile transitions to `UpToDate`. If you see your status
parked at `UpgradeComplete` for more than a minute, something
upstream is preventing the next reconcile (operator pod
crashed, RBAC error). Check the operator logs.

## Verifying an upgrade with port-forward

The operator deliberately keeps a verification window during the
upgrade arc — between "new pods ready" and "public traffic
resumed" — so you can validate the upgrade before the storefront
goes live again.

### Why port-forward bypasses the maintenance fence

The maintenance fence flips Service selectors (frontend / admin /
api) at the maintenance pod. Public traffic via the Gateway / Ingress
sees a 503 page during the arc. But `kubectl port-forward` operates
at the **pod** level — kubelet streams traffic directly to the named
pod, bypassing Service selectors entirely. This means you can
validate the upgrade against the workload pods while public traffic
stays fenced.

### Recipe

```bash
# Find your store's frontend Deployment.
kubectl get deployment -n <ns> -l app.kubernetes.io/component=frontend

# Port-forward to the Deployment's pods (NOT the Service — the
# Service is selector-flipped to the maintenance pod during the
# arc). Pick any free local port; the example uses 8080.
kubectl port-forward -n <ns> deployment/<store>-frontend 8080:8080 &

# Curl the new image's responses. Success looks like a Magento
# storefront response (HTTP 200, HTML body containing the Magento
# default theme markup) — not the maintenance pod's 503 page.
curl -v http://localhost:8080/

# Compare with public traffic (still fenced) — should return 503
# with the maintenance HTML.
curl -v http://<your-public-host>/

# Cleanup when done.
kill %1
```

`kubectl port-forward service/<store>-frontend` does NOT bypass the
fence: kubectl resolves the Service's selectors and connects to a
backing pod, which during the arc is the maintenance pod. Always
target `deployment/<store>-frontend` for verification.

### When the verification window opens and closes

The operator opens the verification window once the upgrade Job
succeeds and the workload pods reach `AvailableReplicas` for the
new image. It closes when the operator restores Service selectors
(stage 2 of the success path). For a small store this is ~10-30
seconds; for large catalogues with multi-replica frontend rollouts
it can be minutes.

### Extending the window for slow validation

If you need more time to validate, scale `spec.workloads.frontend`
to `replicas: 0` mid-arc. The operator restores selectors only
when AvailableReplicas reaches the spec'd count; zero replicas
means it never reaches Available, so stage 2 never fires and the
fence stays up indefinitely. Scale `replicas` back when validation
completes — the operator advances the arc on the next reconcile.

This trick is useful for deep manual checks (admin login, reindex
behaviour, third-party module compatibility) that take longer than
the natural rollout window. Not a feature, but a documented escape
hatch.

### How this is verified

The kind-e2e in `test/e2e/upgrade_test.go` (ADR-008 Validation
Layer 2) asserts both halves of this contract on every run:
during the maintenance flip, port-forward to `service/<store>-frontend`
returns 503 while port-forward to `deployment/<store>-frontend`
returns a non-503 status. Failure breaks the test, so the
verification window contract is regression-tested in CI.

## Recovering from a failed upgrade

`setup:upgrade` is non-transactional. When a Job fails partway
through, the database is left in a half-migrated state — some
patches applied, others not.

When the upgrade Job fails, the operator first retries it up to
five times (`Reason=RetryingFailedJob`; [#22](https://github.com/aknostic/mage-k8s-operator/issues/22) tracks whether
upgrades should be retried at all). Once the attempts are spent,
`UpgradeReady` surfaces `Reason=PermanentlyFailed`,
`phase = Failed`, and the operator stops touching anything:
cron stays suspended,
consumers stay at zero, the maintenance fence stays in place.
**Do not just re-run the same Job** — re-running
`setup:upgrade` against a mid-migration schema applies patches
that already committed and produces a worse failure mode the
second time.

### Recovery sequence

1. **Investigate.** Pull the Job's logs to identify what
   patch/module failed:

   ```bash
   kubectl logs job/store-upgrade-<from-tag>-<to-tag>
   ```

   The Job name encodes the from/to tag pair (with dots
   sanitised to dashes); `kubectl get jobs -l
   app.kubernetes.io/component=upgrade` lists the operator-
   owned upgrade Job for the store.

2. **Restore the database from out-of-band backup.** The
   operator does not yet own backup; this step is the user's
   responsibility for v0.2.x. Recommended sources, in
   decreasing order of preference:

   - `mariadb-operator`'s `Backup` CR if the database is
     mariadb-operator-managed.
   - A `mysqldump` from a sidecar or a snapshot taken before
     the upgrade.
   - A point-in-time recovery from your managed cloud database
     provider (RDS, Cloud SQL, Scaleway).

   Restore to the schema state from before the failed upgrade
   (`status.observedImageTag` in the MageStore tells you which
   image's schema you're targeting). v0.4.0's `MageBackup` CRD
   will land first-class operator-driven backup; until then,
   document your recovery point in your runbook.

3. **Delete the failed Job.** This is the unstick:

   ```bash
   kubectl delete job store-upgrade-<from-tag>-<to-tag>
   ```

   The operator's lockstep refusal keeps cron + consumers
   quiesced and the maintenance fence in place as long as the
   failed Job exists. Reverting `spec.image.tag` alone does
   NOT unstick — the operator considers a present-but-failed
   Job authoritative over spec drift.

4. **Reconcile re-evaluates.** On the next reconcile after
   the Job is deleted, the operator sees no in-flight Job and
   evaluates current spec:

   - If `spec.image.tag == status.observedImageTag` (you
     restored the DB and reverted the tag in lockstep): the
     operator restores cron + consumers + Service selectors,
     deletes the maintenance Deployment, and surfaces
     `UpToDate`. The store is back online on the old image.
   - If `spec.image.tag` still names the new tag: the operator
     enters a fresh upgrade arc against the restored database.
     Set the tag back to the old value first if you don't
     want to retry the upgrade immediately.

The `kubectl port-forward` verification path (next section)
is useful for confirming the database state after restore but
before un-quiescing.

## Disabling the maintenance window (advanced)

For shops running blue-green or canary upgrades that explicitly
accept the integrity risk of mid-migration writes, the
maintenance fence can be opted out:

```yaml
spec:
  upgrade:
    maintenance:
      enabled: false
```

When set, the operator skips the maintenance Deployment and
Service-selector flip entirely; the upgrade Job runs while the
storefront continues serving from the existing pods. **This is
unsafe by default** — write operations against a non-transactional
`setup:upgrade` produce silent data corruption that no amount of
post-hoc cache flushing will heal. Use only when you have
external traffic-shaping (canary cohort, blue-green slot swap)
that prevents writes from reaching the in-flight schema.

## Customising the maintenance page

By default the operator serves a baked-in 503 HTML page during
the maintenance window. To brand it with your shop's "we'll be
right back" page:

```yaml
spec:
  upgrade:
    maintenance:
      htmlConfigMapRef:
        name: my-custom-503     # ConfigMap in the same namespace
        key: index.html         # default
```

The ConfigMap's `key` value is mounted as the maintenance pod's
HTML body. Static HTML only — no PHP, no per-request content.

## Out of scope today (roadmap)

- **Auto-rollback on failure.** The v0.4.0 `MageBackup` CRD will
  enable this via an opt-in `spec.upgrade.autoRollbackOnFailure`
  field. Until then, recovery is admin-driven per the
  [Recovering from a failed upgrade](#recovering-from-a-failed-upgrade)
  recipe.
- **Per-major-version migration hooks.** Some Magento major
  bumps need work outside `setup:upgrade` (RabbitMQ exchange
  rewrites, marketplace key rotations, ad-hoc data migrations).
  A future `spec.upgrade.hooks[]` with `before` / `after` Job
  templates per version pair is on the v0.3.0+ roadmap.
- **Image-contract validation at upgrade entry.** A pre-upgrade
  Job that exec's `php --version` and checks `generated/code`
  against the new image would catch the most common cross-major
  foot-gun before the destructive `setup:upgrade` runs. Planned
  for a v0.2.x patch; tracked in CHANGELOG.

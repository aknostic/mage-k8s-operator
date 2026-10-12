# ADR 005 — `ensureBootstrap` uses an operator-owned Job, not an initContainer

- **Status:** Accepted
- **Date:** 2026-04-23
- **Deciders:** @jurg
- **Related:** supersedes the hand-applied
  `config/samples/bootstrap-install-job.yaml` template (first draft of
  ensureBootstrap from Slot 13). Informs future one-shot reconciler
  work (schema migrations, reindex after upgrade, cache warming).

## Context

Magento's `bin/magento setup:install` is one-shot and destructive: it
creates the database schema, seeds core config, and writes an initial
admin user. Running it twice against the same database either fails
(foreign key conflict on `admin_user.username`) or silently corrupts
state, depending on Magento minor version. Every `ensure*` step that
precedes it in the reconcile chain is convergent — `ensureDatabase`
drives the MariaDB CR to Ready, `ensureSecrets` renders the config
Secret into a declared shape — so `ensureBootstrap` is the first
reconciler step in this codebase whose behavior has to *stop*.

Two natural shapes for that work:

1. **initContainer on the frontend Deployment.** setup:install runs
   inline before the Magento container starts. The pod's Ready gate
   naturally blocks traffic until install finishes. No separate
   resource to manage.
2. **Operator-owned Job.** The operator creates a dedicated `<store>-
   bootstrap` Job with an owner reference back to the MageStore.
   StoreInstalled condition flips True on Job success; further
   reconciles short-circuit before touching the Job.

Both shapes were realistic candidates mid-session. The hand-applied
sample manifest replaced in this slot is a Job — so "operator-owned
Job" is closer to a refactor-plus-ownership than a greenfield rewrite —
but that alone is not a design reason.

## Decision

**`ensureBootstrap` creates and owns a `<store>-bootstrap` Job.** The
StoreInstalled condition on the MageStore status is the source of
truth for "is this store installed"; the Job is the implementation
mechanism. Once the condition is True, the reconciler never looks at
the Job again — user-driven Job deletion is a no-op on subsequent
reconciles.

The initContainer shape was rejected on three grounds:

- **Scale-up races.** `spec.replicas: 3` means three pods starting in
  parallel, each running an idempotence check against the database
  and potentially all invoking setup:install. Coordinating this
  requires a distributed lock in the DB or an external-world
  condition, which is most of the Job-owner's logic anyway — without
  the clean separation a Job provides.
- **Couples one-shot work to pod lifecycle.** Every pod restart (image
  update, node reboot, crashloop) re-runs the initContainer. The
  idempotence check must be bulletproof forever; any regression in
  Magento's `--skip-*` flags or in the check's SQL surfaces as a
  destructive rerun. A Job runs once; there's no rerun surface.
- **No natural condition-gate for downstream reconcile.** The plan
  (and this ADR) want `ensureWorkloads` to gate on
  `StoreInstalled=True`. With an initContainer, StoreInstalled would
  be derived from "any pod passed its readiness probe," which is a
  proxy, not a direct signal — and it assumes the probe itself is
  tied to install completion, which the reference image's
  supervisord+nginx+php-fpm setup doesn't guarantee.

The Job shape doesn't come for free: the operator now owns Job
lifecycle (create, watch for completion, set condition, tolerate
manual deletion) and surfaces a second per-store resource in
`kubectl get all -n demo`. Those costs are acceptable because the
pattern generalises — future one-shot reconciler steps (schema
migrations during upgrade, reindex after a major version bump,
first-run cache warming) follow the same "create Job, watch status,
flip condition" template instead of each inventing its own.

## Consequences

**Positive**
- Clean separation of one-shot from convergent work. Subsequent
  reconciles short-circuit on `StoreInstalled=True` without touching
  the Job; the Job's existence (or deletion) after the fact is
  irrelevant.
- Gate on downstream `ensureWorkloads` is direct:
  `StoreInstalled=True` is the signal, not a pod-readiness proxy.
- Pattern generalises. The first reusable "run N commands once,
  persist success as a condition" template lands in this session and
  future one-shot work (schema migration, post-upgrade reindex, cache
  warming) follows the same shape.

**Negative**
- Job status watching adds ~30 lines of controller-runtime plumbing
  (`.Owns(&batchv1.Job{})` plus a condition-check switch over
  `job.Status.Conditions`). Not load-bearing, just boilerplate.
- Re-bootstrap against an existing DB is a sharp edge. If the user
  deletes the MageStore and recreates it against the same MariaDB CR
  (common in `ref` mode), the new MageStore starts with
  `StoreInstalled` unset → operator creates a Job → the Job
  setup:install fails because the `admin_user` / `core_config_data`
  tables already exist. Mitigation: document the sharp edge; a future
  "detect existing install" check against the DB can lift this, but
  it's not load-bearing for v0.1.0.
- Re-running after a failed Job requires user intervention (`kubectl
  delete job <store>-bootstrap` forces a fresh attempt). Intentional
  — automatic retry on a destructive operation is the wrong default.

**Neutral**
- The admin password generated by the operator lands in
  `<store>-admin` (key `password`). Users who want to bring their own
  set `spec.bootstrap.admin.passwordSecretRef` and point at a
  pre-existing Secret; operator-generated and user-supplied Secrets
  are indistinguishable downstream.

## Open questions

- **Detecting pre-bootstrapped databases.** A `spec.database` adoption
  path needs a way to say "the DB already has Magento tables, mark
  StoreInstalled=True without running the Job." Options: a probe
  container running `mysql -e "SHOW TABLES LIKE 'core_config_data'"`
  before creating the Job, or an explicit `spec.bootstrap.skip: true`
  field. Decision deferred to the session where `spec.database`
  retrofit lands (which introduces the `external` sibling that makes
  this concern concrete).
- **Password rotation.** `spec.bootstrap.admin.passwordSecretRef`
  today is read-at-install. Post-install rotation requires
  `bin/magento admin:user:unlock --password=<new>` or a direct DB
  update — not a CRD edit. A future `ensureAdmin` step (separate from
  bootstrap) can watch the Secret and drive rotations. Out of scope
  for v0.1.0.
- **Sample-data seeding.** Production use-cases (demo environments,
  CI) often want Magento sample data loaded after setup:install.
  Adding `spec.bootstrap.sampleData: true` is a natural follow-on,
  running `bin/magento sampledata:deploy && setup:upgrade` as part
  of the same Job. Deferred until a user asks.

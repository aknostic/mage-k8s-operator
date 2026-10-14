# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

Implemented and released through the v0.6.x line; see `CHANGELOG.md` for what each release shipped. Architecture decisions live in `docs/internals/adr/` — read the relevant ADR before changing a subsystem it covers.

## What this project is

A Kubernetes operator for Magento / Adobe Commerce / Mage-OS, built with Kubebuilder v4 and controller-runtime. The operator owns the Magento lifecycle (workloads, consumers, cron groups, upgrades) and composes existing operators (mariadb-operator, Redis/Valkey, OpenSearch, RabbitMQ) for stateful dependencies — it does NOT embed database/cache/search logic.

Not affiliated with, endorsed by, or sponsored by Adobe Inc. "Magento" and "Adobe Commerce" are trademarks of Adobe Inc. Keep the disclaimer in all user-facing docs.

## Tooling

Go 1.26+, Kubebuilder v4, controller-runtime v0.19+, envtest + Ginkgo/Gomega + kind for tests, golangci-lint (10m timeout), distroless base image, `slog` for logging, `prometheus/client_golang` for metrics. The usual Kubebuilder targets apply: `make generate`, `make manifests`, `make test`, `make lint`, `make docker-build`, `make deploy`, `make undeploy`.

## Architecture essentials (the things you can't derive from code)

### envvars emission mode only (ECE-Tools deferred)
The operator does NOT generate `env.php`. It generates a Secret with individual env vars: `MAGENTO_DATABASE_HOST`, `MAGENTO_REDIS_HOST`, `MAGENTO_RABBITMQ_HOST`, etc. The customer's `env.php` uses `getenv()` to wire them in.

There is no `spec.config.mode` field. The `ecetools` emission path (three base64-encoded Magento-Cloud JSON blobs) was considered (see `docs/internals/adr/002-config-emission-modes.md` Amendment 2026-04-24) but dropped from Phase 1: ECE-Tools is an Adobe Commerce Cloud construct, and the operator's target audience is traditional-hosting shops (Hetzner / Scaleway / bare-metal / self-hosted k8s) that don't ship ECE-Tools in their image. Re-adding ecetools support is ~1 week of work whenever there is clear demand for it; until then, don't treat `spec.config.mode` or `MAGENTO_CLOUD_*` as a gap.

`internal/magento/payload.go` carries a `Relationships` / `Routes` / `Variables` / `Endpoint` intermediate representation that's useful for emitting envvars-mode env vars and leaves the door open to re-add ecetools later without redesigning the ensure step.

Image contract: image ships an `env.php` that reads the documented env var namespace (see `docs/image-contract.md`). Validation at reconcile time is a planned Phase-1 image-contract Job; no admission webhook.

### Three workload types, not two
`frontend`, `admin`, `api` — each is a separate Deployment (php-fpm + nginx sidecar) with its own HPA and resource profile. Ingress routes paths to the correct Service. Admin panel is resource-heavy and must not compete with storefront traffic.

### Consumers are Deployments, not CronJobs (when RabbitMQ is in play)
Magento has ~20 queue consumer processes. Each needs its own Deployment with `replicas: 1` running `bin/magento queue:consumers:start <name>`. Magento consumers **do not handle SIGTERM properly** — they get killed mid-message. Default `terminationGracePeriodSeconds: 60` and document idempotency requirements. Known consumer list per Magento version lives in `internal/magento/consumers.go`.

### Per-group CronJobs, not monolithic `cron:run`
Each cron group (`default`, `index`, `consumers`, …) becomes a separate CronJob running `bin/magento cron:run --group=<name>`. The `index` group gets more resources (reindex is CPU/memory-heavy and long-running). Use `concurrencyPolicy: Allow` so a long reindex doesn't block the next `default` cron.

### S3 for media, not RWX PVC
`spec.media.s3` is the default/recommended path. ReadWriteMany NFS causes performance issues and data corruption at scale. PVC is supported but documented as dev/test only.

### Multi-store = one CR, many hosts
One MageStore CR = one codebase = one deployment. Multi-store is modeled as `spec.stores[]` which generates an nginx `map $http_host $MAGE_RUN_CODE` block and per-host Ingress rules pointing at the same frontend Service. Separate codebases get separate MageStore CRs.

### Database: MariaDB or Percona, never Postgres
Magento only supports MySQL/MariaDB. The CRD supports `spec.database.mariaDbRef`, `spec.database.perconaRef`, or `spec.database.external`. Implementation: a `DatabaseRefResolver` interface in `internal/util/database.go` with `MariaDBResolver` / `PerconaResolver` implementations. Default to mariadb-operator.

### Redis field name stays `redisRef` (Valkey is wire-compatible)
Do not rename to `cacheRef` — it would confuse users and doesn't match the mariadb-operator naming pattern. Valkey works without any operator changes; just document it.

### Varnish VCL
The operator generates a small default VCL (`GenerateDefaultVCL` in `internal/magento/vcl.go`): one backend pointing at the frontend Service, cache bypass for `/admin`, `/rest`, `/graphql` and non-GET/HEAD requests, and a 1-hour fallback TTL. When `spec.stores` is present it adds an explicit `vcl_hash` on Host + URL (ADR 007). `spec.varnish.vclConfigMapRef` replaces the generated VCL wholesale — the operator never splices fragments into a user VCL. Hash the VCL ConfigMap and annotate the Varnish Deployment to trigger rollouts on change.

### External Secrets Operator from v0.1.0
Not a Phase 2 feature. When `spec.secrets.provider: external-secrets`, generate `ExternalSecret` CRs instead of raw `Secret` resources.

## Reconciler conventions

- One `ensure*` function per resource type in `internal/controller/`, each owning a single concern.
- All owned resources get `OwnerReference` via `ctrl.SetControllerReference`.
- Status conditions via `meta.SetStatusCondition` from apimachinery.
- Structured logging via `slog`; always include a `store` key with namespace/name.
- No panics in the reconciler — return `ctrl.Result{}` with error.
- Requeue after 30s on transient failures, 5m when waiting on external dependencies.
- Config hashes in pod template annotations to trigger rollouts when config changes.
- Unit tests via envtest (no real cluster); e2e tests via kind with mariadb-operator pre-installed.

## Naming and trademark

- Project name: `mage-operator`
- CRD API group: `mage.aknostic.com`, version `v1alpha1`
- CRD kinds: `MageStore`, `MageIndex`, `MageBackup`, `MageRestore`
- License: Apache 2.0 from the first commit
- Every user-facing surface (README, Helm chart description, docs) carries the non-affiliation disclaimer

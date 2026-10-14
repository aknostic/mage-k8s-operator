# ADR 004 — Seeded-but-extractable dependencies (inline / ref / external)

- **Status:** Accepted
- **Date:** 2026-04-23
- **Deciders:** @jurg
- **Related:** the project plan's "Architecture essentials". Supersedes
  the implicit ref-only shape in `spec.databaseRef` (current Phase-1 code)
  and `spec.search.opensearchRef` (drafted mid-Slot-15 before this ADR
  landed).

## Context

Magento has several stateful dependencies: a MySQL-protocol database,
a search engine (OpenSearch / Elasticsearch), a cache (Redis / Valkey),
a message broker (RabbitMQ), and object storage for media. The operator
composes external operators for the in-cluster variants and passes
external endpoints through for managed services — but the *shape* of
that composition was not explicitly decided.

Pre-ADR draft (dropped): `spec.search.opensearchRef` pointed at an
admin-supplied `OpenSearchCluster` CR, mirroring the `spec.databaseRef`
pattern already in the code. Same pattern as mariadb-operator
integration. Two real costs:

1. **Quickstart friction.** A five-minute quickstart then needs three
   `kubectl apply` commands (`mariadb.yaml`, `opensearch.yaml`,
   `magestore.yaml`). The pitch is "zero-migration Magento on
   Kubernetes"; three manifests is still closer to "compose three
   Helm charts yourself" than to the operator owning the experience.
2. **No path to managed backends.** RDS, Cloud SQL, Scaleway Managed DB,
   and similar services don't produce Kubernetes CRs. A ref-only shape
   has no place to put them short of inventing a fake CR — which is an
   anti-pattern.

## Decision

**Each stateful dependency on `MageStoreSpec` is a discriminated union
over three siblings: `<backend>` (inline), `<backend>Ref` (reference),
and `external` (unmanaged endpoint).**

Exemplar for search:

```yaml
spec:
  search:
    # exactly one of:
    openSearch:                     # inline — operator creates & owns
      version: "2.19"
      replicas: 1
      diskSize: "2Gi"
      jvmHeap: "512m"
    opensearchRef:                  # reference — admin pre-creates the CR
      name: prod-os
    external:                       # unmanaged — e.g. managed OpenSearch
      host: os.internal.example.com
      port: 9200
```

Semantic per sibling:

- **Inline (`<backend>`):** the operator materialises the upstream CR
  (`OpenSearchCluster`, `MariaDB`, …) with an `ownerReference` back to
  the `MageStore`. Deleting the `MageStore` deletes the upstream CR.
  Defaults are opinionated — plaintext HTTP in-cluster, single-node
  cluster_manager+data pool for search, Magento-compatible version
  pins, mage-operator-managed naming (`<store>-<backend>`). The inline
  spec exposes only the fields Phase-1 cares about; escape hatch is to
  move to the ref variant.
- **Reference (`<backend>Ref`):** the operator only `Get`s and Watches
  an externally-managed CR. No `ownerReference`, no delete cascade.
  Best for multi-store deployments that share a backend or for
  admins who own a dedicated stateful-platform team.
- **External:** no CR at all. The operator reads literal host/port/
  credentials-Secret-ref and emits them straight into the config
  Secret. Best for managed cloud services (RDS, ElastiCache, managed
  OpenSearch, etc.) and for any backend the operator has no reason
  to touch.

All three modes emit the same env var namespace to the image
(`MAGENTO_DATABASE_*`, `MAGENTO_OPENSEARCH_*`, …). The image cannot
tell which backing mode produced them — and should not try.

Exactly one sibling may be set. CRD-level validation enforces this
(`+kubebuilder:validation:XValidation` or equivalent CEL rule).

### "Extraction" is a documented migration, not a runtime flip

Users commonly start inline (one-command quickstart) and later need
to extract the stateful backend — split it out into admin-managed
space, keep it across `MageStore` re-creations, or point a second
store at it. The transition:

1. `kubectl edit` the upstream CR, remove its `ownerReference`.
2. `kubectl edit` the `MageStore`, replace `spec.search.openSearch`
   with `spec.search.opensearchRef: {name: <same>}`.
3. Operator observes the spec change, stops owning the CR, and now
   just watches it. No data movement, no downtime, no cluster restart.

Reverse (ref → inline) is not supported — the operator will not adopt
a CR it didn't create, because adoption would mean either silently
overwriting the admin's tuning or diffing-then-refusing, both of which
surprise the admin more than a deliberate migration.

### Retrofit order

- **Slot 15:** implemented the union for `spec.search` (inline + ref).
  Skipped `external` for search — Magento nearly always talks to
  OpenSearch in-cluster and managed OpenSearch services are rare; the
  `external` path lands for search when a user asks.
- **Slot 17:** retrofit `spec.database` with all three modes (`mariaDb`
  inline, `mariaDbRef`, `external`). The old `databaseRef` field was
  removed outright (no backward-compat shim — pre-v0.1.0). External
  mode surfaces a credentials-Secret schema (`username` / `password`
  keys on a Secret named via `credentialsSecretRef`); that schema is
  the reusable template for other dependencies that grow an `external`
  sibling.
- **Slot 19:** retrofit `spec.cache` with all three modes (`redis`
  inline, `redisRef`, `external`). Field names stay `redis` / `redisRef`
  even though the inline operator is Valkey-first, because wire-compat
  means the user-visible concept is Redis (per `CLAUDE.md`'s naming
  guardrail). External mode reuses the Slot-17 credentials-Secret
  template (`password` key only — Redis has no per-user auth shape).
- **Slot 20:** retrofit `spec.queue` with all three modes
  (`rabbitMq` inline, `rabbitMqRef`, `external`). External mode
  extends the Slot-17 credentials template to four fields
  (`username` / `password` / `vhost` / `port`) and adds a fifth
  shape variant: the credentials Secret schema comes straight from
  upstream's default-user Secret keys, so `credentialsSecretRef`
  pointed at a `RabbitmqCluster`-generated Secret works without
  edits. Inline mode relies on rabbitmq-cluster-operator's
  `Status.DefaultUser.SecretReference` to self-describe the Secret
  name + key names — the cleanest credentials hand-off of the four
  dependencies.
- **Slot 21 (Session 1):** retrofit `spec.media` — **first two-
  sibling application of the ADR-004 pattern**. `s3` (external-
  like: bucket + endpoint + region + credentialsSecretRef with
  `accessKeyId`/`secretAccessKey` keys per AWS SDK v2) and `pvc`
  (inline-like: operator-provisioned PersistentVolumeClaim owned
  by the MageStore). CEL integer-sum over exactly one of the
  two. The three-sibling shape collapses to two because object
  storage has no managed-CR concept — there's no operator
  provisioning an S3 bucket the way mariadb-operator provisions a
  database, so `ref` has no referent — and the `s3` sibling
  already carries the "unmanaged endpoint" semantics that `external`
  would duplicate. Unset `spec.media` falls back to an emptyDir
  volume (dev/demo only). See ADR 007 for how media access
  interacts with the workload split.
- **Future:** a `perconaRef` sibling under `spec.database` is
  anticipated by the plan but not implemented — the discriminated-union
  shape is designed so it can land without breaking existing CRs.

### Two-sibling applications — when `ref` and `external` collapse

The canonical three-sibling shape (`<backend>` inline, `<backend>Ref`
reference, `external` unmanaged) assumes the backend has both:
**(a)** a Kubernetes-native managed representation that an admin
might pre-create (the `ref` case), **and (b)** a plausible
off-cluster deployment that an operator CR would misrepresent (the
`external` case). When either assumption fails, the pattern
collapses gracefully:

- **Missing `ref` sibling:** skip when no managed-CR concept exists
  for the backend. `spec.media`'s S3 case — no `S3Bucket` CRD
  ecosystem exists across cloud providers (Crossplane offers one
  but it's infrastructure-provisioning, not pointed-at-bucket
  semantics). The inline-like sibling (`pvc`) covers admin-pre-
  created storage via `storageClassName`; the external-like
  sibling (`s3`) covers everything else.
- **Missing `external` sibling:** skip when the unmanaged-endpoint
  case is redundant with the managed-CR case. `spec.search`'s
  OpenSearch case through Slot 20 — Magento nearly always talks
  to OpenSearch in-cluster, and the `external` path was deferred
  until a user asked.

Two-sibling applications keep the CEL integer-sum shape unchanged
(just sum over the two siblings that exist). Future three-sibling
landings — media gains an `ossRef` if Crossplane-style bucket CRDs
become standard, search gains `external` when a managed OpenSearch
user surfaces — slot in without breaking existing CRs.

### Redis / Valkey inline mode — operator choice

Unlike MySQL (mariadb-operator is the obvious default) or OpenSearch
(opensearch-project/opensearch-k8s-operator is the only credible
option), there's no dominant Kubernetes operator for Redis. Slot 19
surveyed six candidates against nine criteria (license, activity,
typed Go API, typed readiness helper, credentials auto-gen,
standalone-mode support, HA graduation path, absence of conversion
webhook, Valkey-compat).

**Decision: hyperspike/valkey-operator, pinned at `v0.0.61`.** Module
path `hyperspike.io/valkey-operator`; typed API at
`hyperspike.io/valkey-operator/api/v1`; CRD group `hyperspike.io`,
kind `Valkey`. Chosen for the combination of:

- Apache 2.0 (same licence profile as mariadb-operator and opensearch-
  operator — no SSPL/RSAL licensing hazard for downstream distribution).
- Typed `ValkeyStatus.Ready bool` + typed `Conditions` — we get the
  same `md.IsReady()`-shaped shortcut that made Slot 17 clean, not the
  hand-rolled Phase+Health check OpenSearch forced on us in Slot 15.
- Operator auto-generates a password Secret (`<valkey-cr>` in the same
  namespace, key `password`, 16 random chars) when `anonymousAuth:
  false` and no `ServicePassword` is set. Same pattern as mariadb-
  operator's `RootPasswordSecretKeyRef.Generate = true`.
- No conversion webhook on the CRD — envtest loads the YAML directly
  from the module cache, no webhook-server plumbing needed.
- Valkey-native: sidesteps the Redis Inc. SSPL/RSAL situation
  permanently. Operator ships with a Valkey image by default; `Image`
  field on the spec lets operators override to a Redis image if a
  customer has a hard Redis-the-trademark requirement.

Runners-up, with one-line rejections:

- **OT-Container-Kit/redis-operator (v1beta2).** Rejected because
  `RedisStatus` is literally an empty struct in the v1beta2 Go API
  — no typed readiness, no Conditions. Would force the same hand-
  rolled phase-polling as OpenSearch. Secondary issue: credentials
  are `ExistingPasswordSecret`-only (no auto-gen).
- **dragonflydb/dragonfly-operator.** Rejected because Dragonfly is
  its own Redis-compatible binary, not Valkey. Can't offer the same
  operator for inline and a graduation path customers will describe
  as "give me Redis." Also untyped `Phase string` status, no auto-
  generated password.

If a Magento-to-Valkey incompatibility surfaces during validation
(hyperspike's `nodes: 1` still runs cluster-protocol internally —
Magento's Predis client handles this but it hasn't been burnt in on
our stack), the fallback is to ship v0.1.0 with `redisRef` + `external`
only and defer inline; both spotahome (dead since 2023) and OT-CK
(empty Status) fail our bar, so there's no second-choice inline
operator to swap in.

### RabbitMQ inline mode — operator choice

Unlike Redis (six contested candidates), RabbitMQ has an obvious
default: the RabbitMQ team's own
[`rabbitmq/cluster-operator`](https://github.com/rabbitmq/cluster-operator),
pinned at **v2.20.1**. No scored survey necessary — the structural
criteria hit on the first read:

- **License: MPL 2.0.** Different from the other three operators
  (all Apache 2.0) but neutral for our use case: MPL is file-level
  copyleft on upstream source, and we depend on the module as a
  library without modifying it. No SSPL/RSAL-style service-
  prohibition hazard. Documented here so downstream users (Helm-
  chart consumers, distribution-builders) know the licence mix.
- **Typed Go API at `github.com/rabbitmq/cluster-operator/v2/api/v1beta1`.**
  Single served version, no conversion webhook — envtest loads the
  CRD YAML directly from the module cache.
- **Typed `Status.Conditions` with four named condition types:**
  `ClusterAvailable`, `AllReplicasReady`, `NoWarnings`,
  `ReconcileSuccess`. No `IsReady() bool` helper like mariadb-
  operator; readiness is "walk the conditions, check
  `ClusterAvailable=True && AllReplicasReady=True`." More
  ceremony than Valkey's single typed bool, less pain than
  OpenSearch's Phase+Health enum map.
- **Credentials auto-generation is best-in-class.** Upstream sets
  `Status.DefaultUser.SecretReference.{Name, Keys}` — a self-
  describing reference that tells the consumer *which* Secret to
  read and *which keys* inside it hold `username`, `password`,
  `host`, `port`. Cleaner than mariadb-operator (caller names the
  Secret via `RootPasswordSecretKeyRef`) and cleaner than Valkey
  (Secret name == CR name by convention). The mage-operator reads
  the reference once ready and emits `MAGENTO_RABBITMQ_*` env
  vars from it.
- **Runners-up with one-line rejections.** `messaging-topology-
  operator` from the same team covers queue/exchange/user CRDs
  *on top of* a running cluster — it composes with cluster-operator,
  it's not an alternative. `bitnami/rabbitmq-cluster-operator` is a
  third-party fork with a different type namespace and release
  cadence; no reason to prefer it to the upstream team's own.
  No other credible candidates surfaced.

RabbitmqCluster defaults (single-replica cluster, 10Gi persistence,
automatic default-user Secret) match the inline-mode discipline
directly — `InlineRabbitMQ` exposes only `resources` and optional
`storage` as a curated subset, same shape as `InlineRedis`.

**Slot 19 calibration notes:**

- Last upstream commit observed 2026-01-28 (three months before
  Slot 19); the pace slowed from the weekly-release cadence earlier in
  2025 but the project still accepts Dependabot PRs and the API is
  stable at v0.0.61. Treat as "maintenance-active" not "dead." If the
  repo genuinely stalls, the graduation story remains intact: users in
  `redisRef` mode pre-create a `Valkey` CR the admin manages; the
  operator only `Get`s it and doesn't depend on upstream activity.
- Storage: `ValkeySpec.Storage *corev1.PersistentVolumeClaim` — full
  PVC template. Default behaviour when unset needs verification on
  kind; Slot 15's "emptyDir deadlocks on bootstrap" lesson applies if
  the operator defaults to emptyDir. Validation at Slot 19.5 is the
  forcing function. If emptyDir is the default, the inline defaults
  in `InlineRedis` pin a small PVC.
- The hyperspike CR's default `nodes: 3` (cluster topology) is not a
  fit for inline dev mode; `InlineRedis.nodes` defaults to 1 in our
  curated subset and graduates to `redisRef` for multi-shard deploys.

## Consequences

**Positive**
- One-command quickstart (`kubectl apply -f magestore.yaml`) for the
  80% case. The operator provisions everything needed to boot a fresh
  Magento — database, search, cache — with defaults tuned for the
  Magento version the image ships.
- Managed-service path exists from day one. RDS, Cloud SQL, Scaleway
  Managed DB, ElastiCache, and friends are first-class; the operator
  has no hidden assumption that backends must be in-cluster.
- Migrations between modes are explicit and low-risk. Admins know
  when a CR changes ownership; there's no autodetect magic that
  surprises them during upgrades.

**Negative**
- Triples the CRD surface for every dependency. Field validation,
  defaulting, and docs get bigger. Mitigation: the inline spec
  exposes a small curated subset of upstream fields; escape is the
  ref variant, not an inline field explosion.
- Test matrix grows. Each `ensure*` step must exercise all three
  modes (or two, for dependencies that skip `external`). Envtest +
  kind e2e covers the real combinations.
- Inline mode couples the stateful backend's lifecycle to the
  `MageStore`. A `kubectl delete magestore` takes the DB with it,
  which is usually correct but can surprise users who ran `helm
  uninstall`-equivalent operations. Mitigation: document clearly, and
  always recommend the ref variant for production.

**Neutral**
- The operator now has `create/update/patch/delete` RBAC on upstream
  operators' CRDs, not just `get/list/watch`. That is already true for
  any resource the operator `Owns`; it becomes true for stateful
  backends as well.

## Open questions

- **Per-mode defaults for each backend.** Inline OpenSearch defaults to
  version 2.19, single-node, plaintext HTTP. Inline MariaDB will need
  its own defaults (11.4, 1Gi disk, operator-generated root password).
  Each lands with its slot's ensure\* step; this ADR defines the
  pattern, not every default.
- **Validation of `external`-mode credentials.** For RDS-style DBs the
  user supplies a Secret containing username/password. The operator
  should surface a `DatabaseReady=False (CredentialSecretMissing)`
  condition when the Secret doesn't exist, rather than failing silently
  at pod-boot time. Same pattern for any other `external` backend with
  credentials.
- **Schema evolution.** If inline OpenSearch spec exposes only 5 fields
  today and users ask for a 6th, adding it is a CRD change (minor
  bump). Exposing the full upstream `OpenSearchClusterSpec` verbatim
  would avoid future adds but explodes the surface. Stays curated;
  re-evaluate when we see actual pressure from field requests.

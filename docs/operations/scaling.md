# Scaling and HA

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

How to size a `MageStore` for production traffic and survive
single-node loss. This page is opinionated: the recommendations
below match the configuration the operator's authors run on real
clusters, not every theoretically-possible shape.

For the field-by-field CRD reference, see
[`magestore-reference.md`](../magestore-reference.md). For
prerequisites and version pins, see
[`prerequisites.md`](../prerequisites.md).

## Stateless tiers

The frontend / admin / api Deployments, Varnish, and the operator
itself are all stateless. Scale them horizontally; survive node
loss by running multiple replicas across nodes.

### Workload split (frontend / admin / api)

The three workload kinds are independent Deployments fronting
different request profiles. Run more frontend replicas than
admin/api replicas in nearly every shop:

```yaml
spec:
  workloads:
    frontend:
      replicas: 3
      resources:
        requests: { cpu: 500m, memory: 1Gi }
        limits:   { memory: 2Gi }
      autoscaling:
        minReplicas: 3
        maxReplicas: 12
        targetCPUUtilizationPercentage: 70
    admin:
      replicas: 1
      resources:
        requests: { cpu: 200m, memory: 512Mi }
        limits:   { memory: 2Gi }
    api:
      replicas: 2
      resources:
        requests: { cpu: 300m, memory: 1Gi }
        limits:   { memory: 2Gi }
```

When `admin` or `api` is unset, the matching ingress paths
(`/admin`, `/rest`, `/graphql`) fall through to the frontend
Service. Splitting them out matters when admin sessions or
heavy API consumers must not contend with storefront traffic.

### Varnish

Default `replicas: 2`. Bump for very high cache-hit-rate shops
where storefront traffic dwarfs origin work — Varnish is cheap
to scale and the operator-generated VCL doesn't keep state
across replicas.

```yaml
spec:
  varnish:
    enabled: true
    replicas: 4
```

Set a custom VCL via `spec.varnish.vclConfigMapRef` when the
default doesn't fit. Be aware: with `spec.stores[]` set, the
operator-generated VCL includes the per-host hash key
automatically; custom VCL must include it manually (see ADR 007).

### The operator itself

The operator runs as a single Deployment in the
`mage-operator-system` namespace and does not implement leader
election in v0.1.x or v0.2.x. **Run a single replica.** Two
replicas would race on `MageStore.status` writes. Tracked for
v0.3.0 — see [Known limitations](#known-limitations-v030-roadmap)
below.

The operator's pod is not a hot path; brief unavailability during
node loss only delays reconciles, it does not affect serving
traffic from already-running workloads.

## Stateful tiers

The four data-tier dependencies — MariaDB, Valkey/Redis,
OpenSearch, RabbitMQ — are the load-bearing HA surfaces. **Use
ref-mode for production.** Inline mode (`spec.database.mariaDb:
{}`, `spec.cache.redis: {}`, etc.) provisions single-replica
dev-sized clusters regardless of your traffic; the operator does
not auto-tune those defaults from your spec.

The pattern in every tier: the cluster admin pre-creates the
upstream operator's CR with HA settings, and the MageStore points
at it via `<tier>Ref.name`.

### MariaDB (Galera)

Pre-create a `MariaDB` CR with Galera enabled and `replicas: 3`
(odd number is required for Galera quorum):

```yaml
# 1. Root password Secret (mariadb-operator generates this when
#    `generate: true` is set, but pre-creating it lets you
#    snapshot the value out of band).
apiVersion: v1
kind: Secret
metadata:
  name: store-mariadb-root
  namespace: default
type: Opaque
stringData:
  password: '<long-random-string>'
---
# 2. MariaDB CR with Galera replication.
apiVersion: k8s.mariadb.com/v1alpha1
kind: MariaDB
metadata:
  name: store-mariadb
  namespace: default
spec:
  rootPasswordSecretKeyRef:
    name: store-mariadb-root
    key: password
  replicas: 3
  galera:
    enabled: true
  storage:
    size: 100Gi
    storageClassName: <your-storage-class>
  resources:
    requests: { cpu: 1, memory: 4Gi }
    limits:   { memory: 8Gi }
```

Then point the MageStore at it:

```yaml
spec:
  database:
    mariaDbRef:
      name: store-mariadb
```

mariadb-operator handles primary failover within the Galera
cluster; the MageStore reads the Galera Service endpoint from
the upstream CR's status and emits the `MAGENTO_DATABASE_HOST`
env var pointing at it. For per-field reference of the upstream
CR see the
[mariadb-operator docs](https://github.com/mariadb-operator/mariadb-operator).

### Valkey / Redis

Magento can't use Redis Cluster (`Cm_Cache_Backend_Redis` does
multi-key DELs during cache-tag cleanup that fail with CROSSSLOT —
Magento issue #39224, ADR 009). The supported HA shape is
**Sentinel**: one master, N-1 replicas, and a Sentinel ensemble that
arbitrates failover.

Inline mode in v0.3.0+ provisions exactly that topology when
`spec.cache.redis.replicas` is 3 / 5 / 7 (odd values only — Sentinel
needs an odd voter count). Quorum is hard-coded at `(N/2)+1`. The
operator manages the Valkey StatefulSet, the per-pod PVCs, and the
Sentinel Deployment directly — no upstream Redis or Valkey operator
is required.

```yaml
spec:
  cache:
    redis:
      replicas: 3
      resources:
        requests: { cpu: 200m, memory: 512Mi }
        limits:   { memory: 1Gi }
```

Magento connects via the Sentinel ClusterIP Service (port 26379).
The reference image's `env.php` switches `Cm_Cache_Backend_Redis`
into Sentinel-discovery mode automatically when
`MAGENTO_REDIS_SENTINEL_MASTER` is set in the environment (the
operator emits this env var only in HA mode).

For shops needing topology outside this envelope (TLS-to-clients,
custom storage class, dedicated read replicas), graduate to
`redisRef` and point at any third-party Redis/Valkey operator's
Service:

```yaml
spec:
  cache:
    redisRef:
      name: my-shared-cache  # the front-door Service name
```

The operator reads the password from a conventional
`<service-name>` Secret (key `password`).

> **Online resize is not supported in v0.3.0.** Changing
> `spec.cache.redis.replicas` between 1 and 3+ on an existing
> store is best-effort — the operator updates the StatefulSet but
> does not orchestrate Sentinel bootstrap mid-flight. Recreate the
> cache or the MageStore to change topology cleanly. Tracked for
> v0.4.0.

### OpenSearch

The inline default (`spec.search.openSearch: {}`) already
defaults to `replicas: 3` — single-replica OpenSearch deadlocks
on `cluster_manager_not_discovered` per opensearch-k8s-operator's
behavior. **Inline mode is HA-by-default for OpenSearch** and is
suitable for production unless you need custom node-pool topology
(dedicated master, data, ingest tiers).

Production sizing on inline:

```yaml
spec:
  search:
    openSearch:
      replicas: 3
      jvmHeap: 4g
      resources:
        requests: { cpu: 1, memory: 8Gi }
        limits:   { memory: 10Gi }
```

For custom topology, pre-create an `OpenSearchCluster` CR and
use `spec.search.opensearchRef.name`. The opensearch-k8s-operator
docs cover the multi-node-pool shape.

### RabbitMQ

Pre-create a `RabbitmqCluster` CR with `replicas: 3` (odd number
for quorum):

```yaml
apiVersion: rabbitmq.com/v1beta1
kind: RabbitmqCluster
metadata:
  name: store-rabbitmq
  namespace: default
spec:
  replicas: 3
  persistence:
    storage: 20Gi
    storageClassName: <your-storage-class>
  resources:
    requests: { cpu: 500m, memory: 2Gi }
    limits:   { memory: 2Gi }
```

Then in the MageStore:

```yaml
spec:
  queue:
    rabbitMqRef:
      name: store-rabbitmq
```

rabbitmq-cluster-operator publishes the default-user credentials
on `Status.DefaultUser.SecretReference`; the MageStore reads
these automatically and threads them into Magento's env var
namespace.

### Media (S3, not RWX)

`spec.media.s3` is the only HA-by-design choice for Magento
media. RWX-mounted PVC produces silent corruption at scale —
Magento's media write paths assume single-writer semantics, and
concurrent writes from multiple frontend / consumer pods on the
same RWX volume race on temp-file rename and produce missing or
half-written assets.

```yaml
spec:
  media:
    s3:
      bucket: myshop-media
      endpoint: https://s3.fr-par.scw.cloud  # omit for AWS S3
      region: fr-par
      credentialsSecretRef:
        name: store-s3-creds
```

Any S3-compatible endpoint works: AWS S3, Scaleway Object
Storage, Cloudflare R2, Wasabi, MinIO, Backblaze B2.

## Putting it together — a production MageStore

```yaml
apiVersion: mage.aknostic.com/v1alpha1
kind: MageStore
metadata:
  name: store
  namespace: default
spec:
  image:
    repository: registry.example.com/myshop/magento
    tag: "2.4.8"
  workloads:
    frontend:
      replicas: 3
      resources: { requests: { cpu: 500m, memory: 1Gi }, limits: { memory: 2Gi } }
      autoscaling: { minReplicas: 3, maxReplicas: 12, targetCPUUtilizationPercentage: 70 }
    admin:
      replicas: 1
      resources: { requests: { cpu: 200m, memory: 512Mi }, limits: { memory: 2Gi } }
    api:
      replicas: 2
      resources: { requests: { cpu: 300m, memory: 1Gi }, limits: { memory: 2Gi } }
  database:
    mariaDbRef:
      name: store-mariadb
  cache:
    redis:
      replicas: 3
      resources: { requests: { cpu: 200m, memory: 512Mi }, limits: { memory: 1Gi } }
  search:
    opensearchRef:
      name: store-opensearch
  queue:
    rabbitMqRef:
      name: store-rabbitmq
  media:
    s3:
      bucket: myshop-media
      region: fr-par
      endpoint: https://s3.fr-par.scw.cloud
      credentialsSecretRef:
        name: store-s3-creds
  varnish:
    enabled: true
    replicas: 2
  consumers:
    enabled: true
  cron:
    enabled: true
  bootstrap:
    admin:
      email: admin@example.com
```

Pre-create the upstream CRs (`store-mariadb`, `store-opensearch`,
`store-rabbitmq`) before applying the MageStore. The cache tier no
longer needs an external CR — `spec.cache.redis.replicas: 3`
provisions Sentinel HA inline (ADR 009).

## Known limitations (v0.3.0 roadmap)

The operator has gaps in its HA surface that are tracked for
v0.3.0. Document them prominently because every one is something
a careful operator will check for in production:

### 1. Consumer replicas are hardcoded at 1

Every consumer Deployment runs `replicas: 1`. Magento queue
consumers don't coordinate across replicas; running two of the
same consumer double-dispatches every message (every order
processed twice, every async stock update applied twice). The
right scale-out shape is per-consumer-name, not per-replica
within a name — that's how `spec.consumers.overrides` is
structured today, but it surfaces only `resources` (no
`replicas` field).

The implication for HA: **if the node a consumer pod runs on
fails, the consumer is down until the pod reschedules** — there
is no warm replica to fail over to. Schedule consumers across
nodes (anti-affinity, tracked in #3 below) and accept the small
unavailability window during reschedule.

A future `spec.consumers.overrides[<name>].replicas` field guarded
by a `dangerouslyEnableMultipleReplicas` switch is on the v0.3.0
roadmap.

### 2. No `PodDisruptionBudget` surface

The operator does not emit PodDisruptionBudgets for any workload.
A node drain (kubelet upgrade, planned maintenance, autoscaler
eviction) can take all replicas down simultaneously.

**Mitigation today:** hand-write a `PodDisruptionBudget` resource
in the same namespace. The operator does not own it — you do.
Example for the frontend:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: store-frontend-pdb
  namespace: default
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app.kubernetes.io/instance: store
      app.kubernetes.io/component: frontend
```

v0.3.0 will add `spec.workloads.<kind>.podDisruptionBudget` so
the PDB lifecycle follows the MageStore's.

### 3. No affinity / nodeSelector / topologySpreadConstraints

The operator does not let you constrain pod placement via the
CRD. To spread frontend pods across zones today, you patch the
Deployment directly — which conflicts with the operator's
reconciliation, because the next reconcile will overwrite your
patch.

**Mitigation today:** none clean. Workarounds include
cluster-wide default constraints via the
`PodTopologySpread` admission plugin (placed via webhook), or
running the workload on a dedicated nodepool whose nodes are
geographically spread.

v0.3.0 will add `spec.workloads.<kind>.affinity`,
`.nodeSelector`, and `.topologySpreadConstraints` so the operator
emits these directly on the Deployment.

### 4. Single-replica operator (no leader election)

The operator's own Deployment runs at `replicas: 1`. There is no
leader election today. Running two operator pods would produce
write conflicts on shared `MageStore.status` and is not safe.

**Mitigation today:** none — accept the single-replica posture.
The operator pod is not on the data path; if the node it's on
fails, reconciles pause until the pod reschedules. Already-running
workload pods serve traffic unaffected.

v0.3.0 will land controller-runtime's standard leader-election
wiring; the chart will gain a `--replicas` flag and elect-or-
exit semantics.

### Roadmap reference

For each of the four limitations, the v0.3.0 ADR lives (or will
live) under [`docs/internals/adr/`](../internals/adr/). Track
in [`CHANGELOG.md`](../../CHANGELOG.md) under the v0.3.0
unreleased section once it exists.

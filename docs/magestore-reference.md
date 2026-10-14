# MageStore reference

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

Field-by-field reference for the `MageStore` resource (API
version `mage.aknostic.com/v1alpha1`). Every top-level stanza is
documented with its type, default, and when to set it.

For a working example, see [`config/samples/mage_v1alpha1_magestore.yaml`](../config/samples/mage_v1alpha1_magestore.yaml).
For installation, see [`quickstart.md`](quickstart.md).

## Top-level shape

```yaml
apiVersion: mage.aknostic.com/v1alpha1
kind: MageStore
metadata:
  name: store
  namespace: demo
spec:
  image:        # required — your Magento container image
  workloads:    # required — at least frontend
  stores:       # optional — multi-store hostnames
  routing:      # optional — Gateway-layer config (TLS, etc.)
  database:     # optional — MariaDB / Percona / external
  search:       # optional — OpenSearch / Elasticsearch
  cache:        # optional — Redis / Valkey / external
  queue:        # optional — RabbitMQ / external
  consumers:    # optional — requires spec.queue
  cron:         # optional — Magento cron groups as CronJobs
  media:        # optional — S3 / PVC / unset (emptyDir)
  varnish:      # optional — caching layer in front of /
  bootstrap:    # optional but needed for fresh installs
```

The discriminated-union stanzas (`database`, `search`, `cache`,
`queue`, `media`) all follow ADR 004's pattern: one *inline*
sibling (operator provisions a CR), one *ref* sibling (admin
pre-creates the CR), and an *external* sibling (unmanaged
endpoint). Exactly one sibling per stanza, validated server-side.

## `spec.image` — required

The Magento container image the workloads run.

| Field | Type | Default | Notes |
|---|---|---|---|
| `repository` | string | — | Image name without tag. Required. |
| `tag` | string | — | Image tag. Required. |

```yaml
spec:
  image:
    repository: registry.example.com/myshop/magento
    tag: "2.4.8"
```

The image must honor the operator's image contract — see
[`image-contract.md`](image-contract.md). Use the reference image
at `magento-test-image/` as a template.

## `spec.workloads` — required

The Magento workload split. Frontend is required; admin and api
are optional and represent subtractive optimization. When unset,
their HTTPRoute paths fall through to the frontend Service.

| Field | Type | Default | Notes |
|---|---|---|---|
| `frontend` | `WorkloadSpec` | — | The storefront-serving workload. Required. |
| `admin` | `WorkloadSpec` | unset | Split out for stores whose admin traffic must not contend with storefront traffic. |
| `api` | `WorkloadSpec` | unset | Split out for stores with materially different REST/GraphQL traffic profiles. |

Each `WorkloadSpec`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `replicas` | int (>= 1) | 1 | Initial Deployment replica count. When `autoscaling` is set, the HPA owns the steady-state value. |
| `resources` | `ResourceRequirements` | operator default | Container resource requests/limits. |
| `autoscaling` | `WorkloadAutoscaling` | unset | Optional HPA configuration. |

`WorkloadAutoscaling`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `minReplicas` | int (>= 1) | spec.replicas | HPA lower bound. |
| `maxReplicas` | int (>= 1) | — | HPA upper bound. Required. |
| `targetCPUUtilizationPercentage` | int (1-100) | 80 | Average CPU utilization driving scale decisions. |

```yaml
spec:
  workloads:
    frontend:
      replicas: 3
      autoscaling:
        maxReplicas: 10
        targetCPUUtilizationPercentage: 70
    admin:
      replicas: 1
      resources:
        requests: { cpu: 200m, memory: 512Mi }
        limits: { memory: 2Gi }
```

## `spec.stores` — optional

Multi-store host → MAGE_RUN_CODE mapping. Each entry declares a
public hostname, a Magento store code, and the scope type. The
operator generates an nginx `map $http_host` block and per-host
HTTPRoute hostnames from the list.

When `spec.stores` is unset, Magento serves its default store and
the HTTPRoute matches any host. See ADR 007 in
[`internals/adr/007-multi-store-mage-run-code-mapping.md`](internals/adr/007-multi-store-mage-run-code-mapping.md).

| Field | Type | Default | Notes |
|---|---|---|---|
| `host` | string | — | Public hostname this entry responds to. Exact match, no wildcards. Required. |
| `code` | string | — | Magento store code (must match an existing scope). Required. |
| `type` | enum (`website`, `store`, `store_view`) | — | Magento scope type. Required. |

```yaml
spec:
  stores:
    - host: de.example.com
      code: store_de
      type: website
    - host: fr.example.com
      code: store_fr
      type: website
```

## `spec.routing` — optional

Gateway-layer configuration. When unset, the operator emits the
default — a single HTTP listener on port 80, no TLS. Future fields
(`gatewayRef`, `gatewayClassName`) will land here as Phase 2 work.

### `spec.routing.tls`

Enables HTTPS on your storefront. The operator adds one TLS-terminating
HTTPS listener on port 443 per `spec.stores[].host`, alongside the
existing HTTP/80 listener.

`spec.routing.tls` is a discriminated union — exactly one of
`issuerRef` or `certificateSecretRef` MUST be set. Setting neither
or both fails CRD validation.

`spec.stores` MUST be non-empty when `spec.routing.tls` is set —
HTTPS listeners need hostnames for SNI (every store domain you serve
needs at least one entry in `spec.stores` so the Gateway can present
the right certificate per host). Violation surfaces as
`Ready=False, Reason=TLSRequiresStoreHosts`.

See [`internals/adr/010-public-edge-tls.md`](internals/adr/010-public-edge-tls.md) for the design.

| Field | Type | Default | Notes |
|---|---|---|---|
| `issuerRef` | object | unset | cert-manager Issuer or ClusterIssuer reference. Mutually exclusive with `certificateSecretRef`. |
| `issuerRef.kind` | enum (`Issuer`, `ClusterIssuer`) | required | `ClusterIssuer` for cluster-wide issuers; `Issuer` for namespace-scoped ones. |
| `issuerRef.name` | string | required | Name of the Issuer or ClusterIssuer resource. |
| `certificateSecretRef` | object | unset | BYO `kubernetes.io/tls` Secret reference. Mutually exclusive with `issuerRef`. |
| `certificateSecretRef.name` | string | required | Name of the Secret in the MageStore's namespace. |

#### `issuerRef` — cert-manager-managed certs (recommended)

The operator annotates the Gateway with
`cert-manager.io/cluster-issuer` (or `cert-manager.io/issuer` for
namespace-scoped issuers). cert-manager 1.14+'s Gateway API shim
provisions one Certificate per hostname declared in `spec.stores`,
storing it in a Secret named `<store>-gateway-tls-<host-slug>` (where
host-slug replaces `.` with `-`). Renewal is cert-manager's
responsibility.

Prerequisite: cert-manager installed cluster-wide and a
`ClusterIssuer` (or `Issuer`) ready. See [`prerequisites.md`](prerequisites.md)
and the step-by-step walkthrough in [`quickstart.md`](quickstart.md#enabling-tls).

**Note on Secret lifecycle.** The `<store>-gateway-tls-<host-slug>`
Secrets are owned and managed by cert-manager, not by the operator.
Removing a host from `spec.stores`, switching `issuerRef` to
`certificateSecretRef`, or unsetting `spec.routing.tls` will delete the
cert-manager Gateway annotation on the next reconcile, but the
previously-provisioned Secrets are left in place — cert-manager owns
their lifecycle. This is benign (the Secret is simply unused), but if
you want to clean up, delete by hand:
`kubectl delete secret <store>-gateway-tls-<host-slug> -n <namespace>`.

```yaml
spec:
  stores:
    - host: shop.example.com
      code: base           # Magento's built-in default-website code.
      type: website        # (Not 'default', which is the store-view code.)
  routing:
    tls:
      issuerRef:
        kind: ClusterIssuer
        name: letsencrypt-prod
```

#### `certificateSecretRef` — bring-your-own cert

Use when you have a certificate from a corporate PKI, a wildcard cert,
or any cert provisioned outside cert-manager. Pre-create a
`kubernetes.io/tls` Secret in the same namespace as the MageStore
with `tls.crt` and `tls.key` keys, then reference it here.

For multi-store deployments the cert must cover every hostname in
`spec.stores` via SANs or a wildcard — the operator does not inspect
the cert contents. A mismatch surfaces as Envoy listener errors
(`kubectl describe gateway <store>-gateway`).

```yaml
spec:
  stores:
    - host: shop.example.com
      code: base           # Magento's built-in default-website code.
      type: website        # (Not 'default', which is the store-view code.)
  routing:
    tls:
      certificateSecretRef:
        name: shop-tls-cert
```

#### Trust model

Visitors connect to your storefront over HTTPS. Inside the cluster,
traffic between the load balancer and your storefront pods runs on the
internal pod network without TLS — the same trust boundary as
v0.1.x–v0.4.x. (Technical: HTTPS terminates at the Gateway listener;
Envoy proxies decrypted HTTP to the backend Service. No
re-encryption.) No mTLS between the Gateway and workloads. For the
full trust-model analysis see
[`internals/adr/010-public-edge-tls.md`](internals/adr/010-public-edge-tls.md).

## `spec.database` — optional

MySQL-protocol backend. Discriminated union over three modes per
ADR 004:

| Sibling | Use when |
|---|---|
| `mariaDb` | You want the operator to provision and own a MariaDB CR (via mariadb-operator). Deleting the MageStore deletes the database. |
| `mariaDbRef` | An admin pre-created the MariaDB CR; the operator only watches it. |
| `external` | The database is unmanaged — typically a managed cloud service (RDS, Cloud SQL, Scaleway Managed DB) **or any MySQL-protocol endpoint, including one fronted by a different MySQL operator** (percona-operator, vitess, planetscale, hand-rolled MySQL on a VM). The operator emits the same `MAGENTO_DATABASE_*` env var namespace regardless of who runs the database; the image's `env.php` doesn't know the difference. First-class inline/ref siblings for non-MariaDB operators (e.g. `perconaRef`) are planned for v0.2.0+ — track as a Phase-2 item. |

### `spec.database.mariaDb` (inline)

| Field | Type | Default | Notes |
|---|---|---|---|
| `version` | string | `11.4` | MariaDB image tag. Stick to 10.2-10.6 / 11.4 for Magento 2.4.8 setup:install version checks. |
| `storageSize` | string (Quantity) | `1Gi` | PVC size. Dev-sized; bump or graduate to `mariaDbRef` for real catalogs. |
| `resources` | `ResourceRequirements` | operator default | Container resources. |

### `spec.database.mariaDbRef` (reference)

| Field | Type | Default | Notes |
|---|---|---|---|
| `name` | string | — | MariaDB CR name in the same namespace. |

### `spec.database.external` (unmanaged)

| Field | Type | Default | Notes |
|---|---|---|---|
| `host` | string | — | MySQL hostname. No protocol prefix. Required. |
| `port` | int (1-65535) | 3306 | MySQL port. |
| `databaseName` | string | `magento` | Schema name. |
| `credentialsSecretRef.name` | string | — | Secret with `username` + `password` keys. Required. |

```yaml
spec:
  database:
    external:
      host: my-magento.cluster.eu-west-1.rds.amazonaws.com
      port: 3306
      databaseName: magento
      credentialsSecretRef:
        name: store-db-creds
```

## `spec.search` — optional

OpenSearch / Elasticsearch backend. Magento 2.4.x hard-requires a
reachable search endpoint at `setup:install`. Two modes:

| Sibling | Use when |
|---|---|
| `openSearch` | Operator provisions an OpenSearchCluster CR (via opensearch-operator). |
| `opensearchRef` | Admin pre-created the OpenSearchCluster CR. |

(External-mode OpenSearch is not yet supported.)

### `spec.search.openSearch` (inline)

| Field | Type | Default | Notes |
|---|---|---|---|
| `version` | string | `2.19.2` | OpenSearch image tag. Magento 2.4.x is not certified against OpenSearch 3.x. |
| `replicas` | int (>= 1) | 3 | Node pool size. Defaulting to 3 because single-replica startup races opensearch-operator's bootstrap-pod flow. |
| `jvmHeap` | string | `512m` | JVM `-Xms`/`-Xmx`. Roughly half the container memory limit, capped at 31g. |
| `resources` | `ResourceRequirements` | operator default | Container resources. |

```yaml
spec:
  search:
    openSearch:
      version: "2.19.2"
      replicas: 3
      jvmHeap: 1g
      resources:
        requests: { cpu: 500m, memory: 2Gi }
        limits: { memory: 3Gi }
```

## `spec.cache` — optional

Redis-protocol backend. Three modes per ADR 004:

| Sibling | Use when |
|---|---|
| `redis` | Operator runs Valkey itself as a StatefulSet — single instance by default, Sentinel HA with 3/5/7 replicas (ADR 009). No cache operator needed. Wire-compatible with Redis. |
| `redisRef` | You run the cache yourself (any operator or a hand-rolled deployment). `redisRef.name` names its Service; the password is read from a Secret of the same name (key `password`). |
| `external` | Managed cloud service (ElastiCache, Memorystore, Upstash, Redis Cloud). |

Field name stays `redis` even though the inline cache is
Valkey — the user-visible concept is Redis.

### `spec.cache.redis` (inline)

| Field | Type | Default | Notes |
|---|---|---|---|
| `replicas` | int (1, 3, 5, 7) | 1 | 1 = single instance; 3/5/7 = Sentinel HA. Switching between the two recreates the cache and needs the `mage.aknostic.com/cache-topology-change: "approved"` annotation. |
| `resources` | `ResourceRequirements` | operator default | Container resources. |

### `spec.cache.external` (unmanaged)

| Field | Type | Default | Notes |
|---|---|---|---|
| `host` | string | — | Redis hostname. No protocol prefix. Required. |
| `port` | int (1-65535) | 6379 | Redis port. |
| `passwordSecretRef.name` | string | unset | Optional Secret carrying the AUTH password (default key: `password`). Leave unset for password-less endpoints. |

Magento's three logical stores (default cache, page cache,
session storage) all use the same Redis instance with different
logical DB numbers (0/1/2), wired by the image's `env.php`.

## `spec.queue` — optional

AMQP message broker for Magento's deferred work. Three modes per
ADR 004:

| Sibling | Use when |
|---|---|
| `rabbitMq` | Operator provisions a RabbitmqCluster CR inline (via rabbitmq-cluster-operator). |
| `rabbitMqRef` | Admin pre-created the RabbitmqCluster CR. |
| `external` | Managed cloud service (Amazon MQ, CloudAMQP, self-hosted RabbitMQ). |

Without `spec.queue`, Magento runs in synchronous-processing
fallback — fine for dev, fine for storefronts that don't use
bulk operations.

### `spec.queue.rabbitMq` (inline)

| Field | Type | Default | Notes |
|---|---|---|---|
| `resources` | `ResourceRequirements` | operator default | Container resources. |

### `spec.queue.external` (unmanaged)

| Field | Type | Default | Notes |
|---|---|---|---|
| `host` | string | — | AMQP hostname. No protocol prefix. Required. |
| `port` | int (1-65535) | 5672 | AMQP port. |
| `vhost` | string | `/` | AMQP virtual host. |
| `credentialsSecretRef.name` | string | — | Secret with `username` + `password` keys. Required. |

The Secret schema (`username`/`password`) matches
rabbitmq-cluster-operator's default-user Secret, so a
`credentialsSecretRef` pointed at one of those Secrets works
without translation.

## `spec.consumers` — optional

Per-consumer Deployments for Magento's queue consumers (~20
processes). Requires `spec.queue` to be set; a runtime
`ConsumersReady=False, Reason=QueueNotConfigured` condition
surfaces if not.

| Field | Type | Default | Notes |
|---|---|---|---|
| `enabled` | bool | — | Gate for the entire subsystem. Required. |
| `overrides` | `map[string]ConsumerOverride` | empty | Per-consumer tuning. Key = consumer name as published by `bin/magento queue:consumers:list`. |

`ConsumerOverride`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `resources` | `ResourceRequirements` | operator default | Per-consumer resource override. |

Frequently-tuned consumers:
`product_action_attribute.update` (memory-hungry on million-SKU
catalogues), `async.operations.all` (CPU-hungry on bulk-API-heavy
stores).

```yaml
spec:
  consumers:
    enabled: true
    overrides:
      product_action_attribute.update:
        resources:
          requests: { cpu: 200m, memory: 512Mi }
          limits: { memory: 2Gi }
```

## `spec.cron` — optional

Per-group CronJobs for Magento's cron groups. Each declared group
runs `bin/magento cron:run --group=<name>` on its schedule.

| Field | Type | Default | Notes |
|---|---|---|---|
| `enabled` | bool | — | Gate for the entire subsystem. Required. |
| `overrides` | `map[string]CronOverride` | empty | Per-group tuning. Key = cron group name. |

Default groups: `default`, `index`, `consumers`, `staging`,
`catalog_event`. The `index` group ships with a more generous
memory profile (reindex is long-running and memory-heavy).

`CronOverride`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `schedule` | string | operator default | Standard 5-field cron syntax. |
| `resources` | `ResourceRequirements` | operator default | Per-group resource override. |
| `disabled` | bool | false | Skip this group entirely (useful when you run an external indexing service). |

```yaml
spec:
  cron:
    enabled: true
    overrides:
      index:
        schedule: "*/5 * * * *"
        resources:
          requests: { memory: 2Gi }
          limits: { memory: 4Gi }
      catalog_event:
        disabled: true
```

## `spec.media` — optional

Object storage for Magento's `pub/media` directory (product
images, generated thumbnails, wysiwyg uploads). Two-sibling union
per ADR 004:

| Sibling | Use when |
|---|---|
| `s3` | **Production default.** S3-compatible object storage (AWS S3, Scaleway Object Storage, Wasabi, Minio, Cloudflare R2, ...). |
| `pvc` | Dev/test only. RWO PVC mounted at pub/media. RWX is documented as known-corruption at scale. |

When `spec.media` is unset, the operator mounts an `emptyDir` —
ephemeral, demos only.

### `spec.media.s3` (object storage)

| Field | Type | Default | Notes |
|---|---|---|---|
| `bucket` | string | — | Bucket name. Required. The operator does not create the bucket. |
| `endpoint` | string | unset | S3-compatible endpoint URL (e.g. `https://s3.fr-par.scw.cloud`). Leave unset for AWS S3. |
| `region` | string | — | AWS region (or provider-specific region tag). Required by the AWS SDK v2 even for non-AWS endpoints. |
| `credentialsSecretRef.name` | string | — | Secret with `accessKeyId` + `secretAccessKey` keys. Required. |

```yaml
spec:
  media:
    s3:
      bucket: myshop-media
      endpoint: https://s3.fr-par.scw.cloud
      region: fr-par
      credentialsSecretRef:
        name: store-s3-creds
```

### `spec.media.pvc` (PersistentVolumeClaim)

| Field | Type | Default | Notes |
|---|---|---|---|
| `storageClassName` | string | unset (cluster default) | StorageClass for the PVC. |
| `size` | string (Quantity) | `10Gi` | PVC capacity. |

## `spec.varnish` — optional

Caching layer in front of the storefront. When unset, the
HTTPRoute points directly at the frontend Service. When `enabled:
true`, the operator creates a Varnish Deployment + Service and
routes `/` through it. `/admin`, `/rest`, `/graphql` always
bypass Varnish.

| Field | Type | Default | Notes |
|---|---|---|---|
| `enabled` | bool | — | Gate for the subsystem. Required. |
| `replicas` | int (>= 1) | 2 | Varnish Deployment replicas. |
| `version` | string | `7.5` | Varnish image tag (`docker.io/library/varnish:<tag>`). |
| `vclConfigMapRef` | `LocalObjectReference` | unset | Optional ConfigMap holding a custom VCL (key: `default.vcl`). When unset, the operator generates a default VCL tuned for the Magento version + multi-store hash key when `spec.stores` is set. |
| `resources` | `ResourceRequirements` | operator default | Container resources. |

```yaml
# Default VCL — operator-generated.
spec:
  varnish:
    enabled: true

# Custom VCL — operator uses your VCL verbatim.
spec:
  varnish:
    enabled: true
    vclConfigMapRef:
      name: my-custom-vcl
```

When supplying a custom VCL for a multi-store deployment, you
must include the per-host backend mapping yourself — the
operator does not splice into custom VCL. See ADR 007.

## `spec.bootstrap` — optional but usually required

The one-shot `setup:install` configuration. When unset, the
operator does not invoke `setup:install` and the workload rollout
proceeds as soon as the dependency chain is ready. Use that path
only when pointing at a pre-bootstrapped database; **every fresh
store needs `spec.bootstrap`.**

| Field | Type | Default | Notes |
|---|---|---|---|
| `admin` | `AdminSpec` | — | Magento administrator account. Required when `spec.bootstrap` is set. |

`AdminSpec`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `email` | string (email format) | — | Admin email. Required. |
| `firstName` | string | `Admin` | Admin first name. |
| `lastName` | string | `User` | Admin last name. |
| `username` | string | `admin` | Admin username. |
| `passwordSecretRef` | `SecretKeySelector` | unset | Secret with the initial password (default key: `password`). Leave unset to have the operator generate a 20-char random password and store it in `<store>-admin`. |

```yaml
spec:
  bootstrap:
    admin:
      email: admin@example.com
      firstName: Jane
      lastName: Doe
      # No passwordSecretRef → operator generates one once.
```

Password is write-once — rotating it requires a `bin/magento
admin:user:unlock` flow, not a CRD edit.

## `spec.upgrade` — optional

Configures how `spec.image.tag` drift becomes a Magento
`setup:upgrade` arc. When unset, the operator still performs
`setup:upgrade` on tag drift but with default-fenced maintenance
(see ADR 008 for the orchestration contract). Future fields
(`hooks`, `autoRollbackOnFailure`) land additively under
`spec.upgrade`.

| Field | Type | Default | Notes |
|---|---|---|---|
| `maintenance` | `MageStoreUpgradeMaintenanceSpec` | unset | Maintenance window configuration. When the parent struct is set without an `enabled` field, the maintenance window is on. |

`MageStoreUpgradeMaintenanceSpec`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `enabled` | `*bool` | `true` | Toggles the 503 maintenance window during the upgrade. Default `true` — disabling is opt-in for shops running blue-green / canary upgrades that explicitly accept the integrity risk of mid-migration writes. Pointer type so the default applies on omission only; explicit `false` is preserved. |
| `htmlConfigMapRef` | `MaintenanceHtmlConfigMapRef` | unset | Optional override of the 503 page. The operator mounts the named key into the maintenance pod. When unset, a baked-in default 503 page is served. |

`MaintenanceHtmlConfigMapRef`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `name` | string (min length 1) | — | ConfigMap name in the same namespace as the MageStore. Required. |
| `key` | string | `index.html` | ConfigMap data key holding the HTML payload. |

```yaml
spec:
  upgrade:
    maintenance:
      enabled: true                      # default; opt out only for genuine blue-green
      htmlConfigMapRef:
        name: my-custom-503
        key: index.html                  # default
```

> See [`operations/upgrades.md`](operations/upgrades.md) for the
> upgrade flow, the per-Reason vocabulary, and the unstick recipe
> for failed upgrades.

## Status

### `status.observedImageTag`

The image tag the schema is currently consistent with. Empty
before the first install completes; advances on successful
`setup:upgrade` Job completion. Never advances on Job failure —
the failure path keeps the prior value so the next reconcile
re-evaluates the same upgrade arc. See ADR 008.

### Conditions

The operator surfaces detailed readiness state via
`status.conditions`. Each condition has a `Type`, `Status`
(`True` / `False` / `Unknown`), `Reason`, and `Message`.

| Condition Type | What it means |
|---|---|
| `DatabaseReady` | The database backend is reachable and the credentials Secret exists. |
| `SearchReady` | The search backend is reachable. |
| `CacheReady` | The cache backend is reachable. |
| `QueueReady` | The AMQP broker is reachable. `Unknown/NotEnabled` when `spec.queue` is unset. |
| `SecretsReady` | The per-store config Secret has been materialised. |
| `WorkloadsReady` | All workload Deployments have their desired replicas Available. |
| `VarnishReady` | Varnish is reachable. `Unknown/NotEnabled` when `spec.varnish` is unset. |
| `ConsumersReady` | Consumer Deployments are running. `Unknown/NotEnabled` when `spec.consumers.enabled: false`. |
| `CronReady` | Cron CronJobs exist. `Unknown/NotEnabled` when `spec.cron.enabled: false`. |
| `StoreInstalled` | `setup:install` has completed successfully. |
| `UpgradeReady` | `True` when `status.observedImageTag == spec.image.tag` and the post-upgrade workload rollout has reached Available. `False` during any in-flight upgrade or after a failed upgrade. The `Reason` field names the stage; see [`operations/upgrades.md`](operations/upgrades.md#reason-values-during-an-upgrade) for the full vocabulary and what each value means. |
| `Ready` | Rollup of all the above. `True` when every other applicable condition is `True`. |

For the condition-by-condition `Reason` reference (every
`Reason: X` value the operator emits, with "what it means" and
"what to check"), see [`troubleshooting.md`](troubleshooting.md).

## Examples

The bundled sample at
[`config/samples/mage_v1alpha1_magestore.yaml`](../config/samples/mage_v1alpha1_magestore.yaml)
exercises every stanza with reasonable demo defaults. Adapt it to
your image and dependency choices.

## What's not (yet) in v0.1.0

These are deferred to later releases:

- `spec.routing.gatewayRef` / `spec.routing.gatewayClassName` — for
  now, the operator hard-codes the `eg` GatewayClass that Envoy
  Gateway provisions. Phase-2 work.
- `spec.consumers.extras[]` — adding consumers beyond the known
  Magento 2.4.8 list. Until then, unknown override keys surface
  via `ConsumersReady=False, Reason=UnknownConsumerOverride`.
- `spec.email.smtpSecretRef` — first-class SMTP wiring. Today,
  workloads pick up an optional `<store>-smtp` Secret if it
  exists; see [`internals/local-smtp.md`](internals/local-smtp.md).
- `spec.secrets.provider: external-secrets` — External Secrets
  Operator integration. Phase-2 work.
- `MageIndex`, `MageBackup`, `MageRestore` — separate CRDs for
  reindex orchestration, backup, and restore. Phase 2-3 per the
  roadmap.

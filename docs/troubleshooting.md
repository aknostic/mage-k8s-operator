# Troubleshooting

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

A `MageStore` whose `Ready` condition is `False` carries one or more
sub-condition `Reason` values describing what's wrong. This page is
the lookup table — find your `Reason` and follow the "what to check"
guidance.

```
kubectl get magestore <name> -o yaml
```

The `status.conditions` list has one entry per top-level concern:
`DatabaseReady`, `SearchReady`, `CacheReady`, `QueueReady`,
`SecretsReady`, `StoreInstalled`, `WorkloadsReady`, `VarnishReady`,
`ConsumersReady`, `CronReady`, plus the `Ready` rollup. Each entry's
`reason` and `message` are the data points below.

## DependencyOperatorMissing — the cluster lacks the operator you opted into

Surfaces on `DatabaseReady`, `SearchReady`, or `QueueReady` when the corresponding `spec.<tier>.<inline>` /
`<inlineRef>` is set but the dependency operator's CRDs are not
installed in the cluster.

**Why this exists.** The mage-operator probes for each dependency
operator's CRDs at startup and degrades gracefully when they're
missing — the controller boots and serves all unaffected MageStores.
Only the affected sub-condition is set to `False`; nothing crashes.

**What to check.**

1. Look at the `message` — it names the operator you need to install
   (`mariadb-operator`, `opensearch-k8s-operator` or
   `rabbitmq-cluster-operator`; the cache needs no operator).
2. Confirm the operator is installed: `kubectl get crd | grep <crd>`,
   where `<crd>` is the resource the operator owns. The expected CRDs
   are listed in [`prerequisites.md`](prerequisites.md).
3. Install the operator using the install command in
   [`prerequisites.md`](prerequisites.md), then **restart the
   mage-operator pod** — capability detection runs once at startup,
   not per-reconcile.
4. If you don't want to add the operator, switch to
   `spec.<tier>.external` instead: it points the operator at a
   reachable endpoint without needing the inline/ref CRDs.

## DatabaseReady reasons

| Reason | What it means | What to check |
|---|---|---|
| `NotConfigured` | `spec.database` is empty. | If the store is meant to use a database, set `spec.database.mariaDb`, `mariaDbRef`, or `external`. |
| `DependencyOperatorMissing` | See above. | Install mariadb-operator or switch to `spec.database.external`. |
| `MaterialiseFailed` | Inline mode failed to create or update the MariaDB CR. | Read the message for the underlying apiserver error. Common cause: invalid `spec.database.mariaDb.storageSize`. |
| `NotFound` | The named MariaDB CR does not exist in the namespace. | Inline mode: transient — wait one reconcile. Ref mode: confirm the admin pre-created the CR with the name in `spec.database.mariaDbRef.name`. |
| `LookupError` | Apiserver returned a non-NotFound error. | Read the message; usually transient. Persistent: check apiserver logs and operator RBAC. |
| `MariaDB<UpstreamReason>` | The upstream MariaDB CR's own Ready condition is False. | `kubectl describe mariadb <name>` and follow mariadb-operator's diagnostics. |
| `NotReady` | The MariaDB CR has no Ready condition yet. | Wait — bootstrap takes ~1 minute. Check `kubectl get pods` for the StatefulSet's pod state. |
| `MissingCredentialRef` | `spec.rootPasswordSecretKeyRef` is unset on the MariaDB CR. | Ref mode only: the admin must have configured the upstream CR's password ref. |
| `CredentialSecretMissing` / `CredentialKeyEmpty` | The credentials Secret named in the spec doesn't exist or lacks the expected keys. | External mode: confirm `spec.database.external.credentialsSecretRef.name` exists with non-empty `username` + `password` keys. Inline/ref: the upstream operator should have created this — check its logs. |
| `MariaDBReady` / `ExternalReady` | Healthy. | — |

## SearchReady reasons

| Reason | What it means | What to check |
|---|---|---|
| `NotConfigured` | `spec.search` is empty. | Magento 2.4.x hard-requires search; set inline / ref / external. |
| `DependencyOperatorMissing` | See above. | Install opensearch-k8s-operator or switch to `spec.search.external`. |
| `MaterialiseFailed` | Inline mode failed to create the OpenSearchCluster CR. | Read the message; check `spec.search.openSearch.version` is a valid 2.x semver tag. |
| `NotFound` | The OpenSearchCluster CR does not exist. | Inline: transient. Ref: confirm the named CR exists. |
| `LookupError` | Apiserver returned an error. | Read the message; usually transient. |
| `NotReady` | OpenSearchCluster has not reached Phase=RUNNING + Initialized + Health in {green, yellow}. | `kubectl describe opensearchcluster <name>` and check pod logs. Common cause: single-replica clusters deadlock on `cluster_manager_not_discovered` — see CLAUDE.md or graduate to `replicas: 3` (the default). |
| `CredentialSecretMissing` / `CredentialKeyEmpty` | External mode's `credentialsSecretRef` is unresolvable. | Confirm the Secret exists with `username` + `password` keys. |
| `OpenSearchReady` / `ExternalReady` | Healthy. | — |

## CacheReady reasons

| Reason | What it means | What to check |
|---|---|---|
| `NotConfigured` | `spec.cache` is empty. | Magento heavily uses Redis; set inline / ref / external. |
| `MaterialiseFailed` | Inline mode failed to create or update a cache resource (Valkey StatefulSet, Services, ConfigMaps, auth Secret, or the Sentinel Deployment). | Read the message. |
| `TopologyChangeRequiresApproval` | `spec.cache.redis.replicas` changed between single-instance (1) and Sentinel HA (3/5/7). | The switch recreates the cache and drops sessions; approve it with the `mage.aknostic.com/cache-topology-change: "approved"` annotation on the MageStore. |
| `SweepFailed` | Removing leftover Sentinel resources after going back to a single instance failed. | Read the message. |
| `NotFound` | Inline: the `<store>-valkey` StatefulSet does not exist yet. Ref: the Service named in `redisRef` does not exist. | Inline: transient. Ref: confirm the Service exists in the MageStore's namespace. |
| `NotReady` | The `<store>-valkey` StatefulSet has no ready pods. | `kubectl describe statefulset <store>-valkey` and check the pod logs. |
| `SentinelPending` / `SentinelBelowQuorum` | Sentinel HA: the `<store>-valkey-sentinel` Deployment is missing or has fewer available pods than the quorum. | Usually transient after enabling HA; otherwise check the Sentinel pods. |
| `LookupError` | Apiserver returned an error. | Read the message; usually transient. |
| `CredentialSecretMissing` / `CredentialKeyEmpty` | The cache password Secret can't be read. | Ref: the Secret named after the `redisRef` Service (key `password`). External (with `passwordSecretRef`): confirm the Secret + key. |
| `ValkeyReady` / `SentinelReady` / `ServiceReady` / `ExternalReady` | Healthy. | — |

## QueueReady reasons

| Reason | What it means | What to check |
|---|---|---|
| `NotConfigured` | `spec.queue` is empty — Magento runs in synchronous mode. | Optional. Set inline / ref / external if you need async consumers / bulk API. |
| `DependencyOperatorMissing` | See above. | Install rabbitmq-cluster-operator or switch to `spec.queue.external`. |
| `MaterialiseFailed` | Inline mode failed to create the RabbitmqCluster CR. | Read the message. |
| `NotFound` | The RabbitmqCluster CR does not exist. | Inline: transient. Ref: confirm the named CR exists. |
| `LookupError` | Apiserver returned an error. | Read the message; usually transient. |
| `NotReady` | RabbitmqCluster has not reached `ClusterAvailable=True` + `AllReplicasReady=True`. | `kubectl describe rabbitmqcluster <name>` and check pod logs. |
| `CredentialSecretMissing` / `CredentialKeyEmpty` | Credentials Secret missing or empty. | Inline/ref: check `Status.DefaultUser.SecretReference` on the CR. External: confirm `credentialsSecretRef` resolves. |
| `RabbitmqClusterReady` / `ExternalReady` | Healthy. | — |

## SecretsReady reasons

The operator-rendered config Secret is what each Magento pod loads as
`envFrom` at startup.

| Reason | What it means | What to check |
|---|---|---|
| `AwaitingDatabase` / `AwaitingSearch` / `AwaitingCache` / `AwaitingQueue` | Render is held until the named tier reaches Ready. | Resolve the upstream condition — see above. |
| `NoDatabaseEndpoint` | Database is "Ready" but no endpoint resolved (typically `spec.database` is unset on a store that needs one). | Set `spec.database` if Magento needs a backing database. |
| `ApplyFailed` | The Secret create/update returned an apiserver error. | Read the message; check operator RBAC. |
| `ConfigRendered` | Healthy. | — |

## StoreInstalled (bootstrap) reasons

`spec.bootstrap` runs `bin/magento setup:install` once on a fresh
store.

| Reason | What it means | What to check |
|---|---|---|
| `NotConfigured` | `spec.bootstrap` is unset; the operator is pointing at a pre-bootstrapped database. | Intentional for stores using an existing DB; otherwise set `spec.bootstrap.admin`. |
| `AwaitingDependencies` | Bootstrap blocks until SecretsReady is True. | Resolve the upstream condition. |
| `AdminPasswordError` | The admin password Secret reference is unresolvable or empty. | Confirm `spec.bootstrap.admin.passwordSecretRef` (or leave unset for an operator-generated password in `<store>-admin`). |
| `JobCreateFailed` | The setup:install Job failed to apply. | Read the message. |
| `BootstrapInProgress` | The Job is running. | `kubectl logs job/<store>-bootstrap` for live progress. |
| `BootstrapComplete` | Healthy. | — |
| `BootstrapFailed` | The Job's pod failed. | `kubectl logs job/<store>-bootstrap` — typical causes: unreachable database, schema drift, invalid admin email. |

## WorkloadsReady reasons

The frontend / admin / api Deployments.

| Reason | What it means | What to check |
|---|---|---|
| `AwaitingDependencies` | SecretsReady is False, or StoreInstalled is False. | Resolve the upstream conditions. |
| `OrphanCleanupFailed` | Removing a Deployment / Service / HPA from a previous spec failed. | Read the message; check operator RBAC. |
| `ApplyFailed` | Deployment / Service / HPA apply failed. | Read the message. |
| `RolloutInProgress` | One or more Deployments has not reached desired replica count. | `kubectl rollout status deployment <name>` — typical pause point: pod failing readiness probe (DB unreachable, missing env vars). |
| `AllReplicasAvailable` | Healthy. | — |

## VarnishReady reasons

Optional caching layer (`spec.varnish.enabled: true`).

| Reason | What it means | What to check |
|---|---|---|
| `NotEnabled` | `spec.varnish.enabled` is false — informational. | — |
| `OrphanCleanupFailed` / `ApplyFailed` | Resource lifecycle failure. | Read the message. |
| `VCLConfigMapMissing` / `VCLKeyMissing` | `spec.varnish.vclConfigMapRef` points at a ConfigMap that doesn't exist or has no `default.vcl` key. | Confirm the ConfigMap, or unset `vclConfigMapRef` to use the default operator-generated VCL. |
| `LookupError` | Transient apiserver error. | Read the message. |
| `RolloutInProgress` | The Varnish Deployment has not reached desired replicas. | `kubectl rollout status deployment <store>-varnish`. |
| `VarnishReady` | Healthy. | — |

## ConsumersReady reasons

Per-consumer Deployments materialised when `spec.consumers.enabled: true`.

| Reason | What it means | What to check |
|---|---|---|
| `NotEnabled` / `OrphanCleanupFailed` / `ApplyFailed` / `RolloutInProgress` | As above. | — |
| `QueueNotConfigured` | Consumers are enabled but `spec.queue` is unset. | Either set `spec.queue` or disable consumers. |
| `AwaitingDependencies` | SecretsReady / StoreInstalled is False. | Resolve the upstream conditions. |
| `UnknownConsumerOverride` | `spec.consumers.overrides` references a consumer name not on the operator's known-good list. | Run `bin/magento queue:consumers:list` against the image and copy the exact name. |
| `AllReady` | Healthy. | — |

## CronReady reasons

Per-group CronJobs materialised when `spec.cron.enabled: true`.

| Reason | What it means | What to check |
|---|---|---|
| `NotEnabled` / `OrphanCleanupFailed` / `ApplyFailed` | As above. | — |
| `AwaitingDependencies` | SecretsReady / StoreInstalled is False. | Resolve the upstream conditions. |
| `UnknownCronGroupOverride` | `spec.cron.overrides` references a group name not on the operator's known-good list. | Run `bin/magento cron:run --help` against the image to list valid group names. |
| `AllReady` | Healthy. | — |

## Ready (rollup) reasons

`Ready` is a strict AND over the sub-conditions. The first failing
sub-condition drives the `Reason`.

| Reason | What it means |
|---|---|
| `DependenciesMet` | Healthy — all declared dependencies are ready. |
| `TLSRequiresStoreHosts` | See below. |
| `AwaitingDatabase` / `AwaitingSearch` / `AwaitingCache` / `AwaitingQueue` / `AwaitingSecrets` / `AwaitingBootstrap` / `AwaitingWorkloads` / `AwaitingVarnish` / `AwaitingConsumers` / `AwaitingCron` | Look up the named sub-condition above. |

### `TLSRequiresStoreHosts` — TLS is configured but `spec.stores` is empty

Surfaces on the top-level `Ready` condition when `spec.routing.tls`
is set but `spec.stores` is empty or unset.

**Why this exists.** HTTPS listeners terminate TLS via SNI — the
Gateway needs at least one hostname to know which certificate to
present and which listener to route a connection to. The HTTP-only
fallback that ADR 006 supports (HTTPRoute matches any host when
`spec.stores` is unset) does not translate to TLS. Setting
`spec.routing.tls` without any store hostnames would produce a
misconfigured Gateway that Envoy Gateway cannot service.

**What to check.**

1. Confirm `spec.stores` has at least one entry:
   `kubectl get magestore <name> -o jsonpath='{.spec.stores}'`
2. Add a `spec.stores[]` entry for each hostname your storefront
   should respond to:
   ```yaml
   spec:
     stores:
       - host: shop.example.com
         code: base           # Magento's built-in default-website code.
         type: website        # (Not 'default', which is the store-view code.)
   ```
3. Save the manifest. The next reconcile clears the condition
   automatically — no pod restart needed.

Single-tenant TLS with one hostname is supported: a single
`spec.stores` entry is enough.

# Minimal MageStore examples

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

The smallest viable `MageStore` resources for the two common
starting cases. Adapt these to your needs; for every other field,
see [`magestore-reference.md`](magestore-reference.md).

## Fresh install

Use this when you don't have an existing Magento database. The
operator runs `bin/magento setup:install` once and stores the
generated admin password in `<store>-admin`.

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
      replicas: 1
  database:
    mariaDb: {}
  search:
    openSearch:
      jvmHeap: 256m  # fits a kind cluster; leave out on a real cluster (default 512m)
  bootstrap:
    admin:
      email: admin@example.com
```

A fresh install needs `spec.database` and `spec.search`: the
store doesn't become Ready without a database, and `setup:install`
needs a search backend. `mariaDb: {}` and `openSearch: {}` run
both inline (MariaDB via `mariadb-operator`, OpenSearch via
`opensearch-k8s-operator`). Both are dev-sized — one MariaDB
replica, three OpenSearch nodes (a single node never forms a
cluster, see [#21](https://github.com/aknostic/mage-k8s-operator/issues/21)),
small PVCs — and adequate for a local kind cluster or a
non-production test deployment. `jvmHeap: 256m` keeps OpenSearch
within a kind cluster's memory; leave it out on a real cluster.

The admin password is operator-generated; retrieve it after
`Ready=True`:

```bash
kubectl get secret store-admin -o jsonpath='{.data.password}' | base64 -d
```

## Pre-bootstrapped database

Use this when you already have a Magento database running
elsewhere — your existing deployment, a managed cloud DB, or a
restored backup. The operator skips `setup:install` and rolls
out the workloads as soon as the dependency chain is ready.

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
      replicas: 1
  database:
    external:
      host: my-magento.cluster.example.com
      port: 3306
      databaseName: magento
      credentialsSecretRef:
        name: store-db-creds
```

Before applying, create the credentials Secret with `username`
and `password` keys:

```bash
kubectl create secret generic store-db-creds \
  --from-literal=username=magento \
  --from-literal=password='<your-db-password>'
```

Omitting `spec.bootstrap` is what tells the operator the
database is already populated. With `spec.bootstrap` set on a
pre-bootstrapped DB, `setup:install` would refuse and the
StoreInstalled condition would surface
`BootstrapFailed`.

## What's not in these examples

The minimal examples above are intentionally bare. Realistic
deployments add at least:

- **External search.** The fresh-install example runs OpenSearch
  inline. For an external OpenSearch endpoint, see
  [`magestore-reference.md` § spec.search](magestore-reference.md#specsearch).
- **Cache** (Redis-protocol). Heavily used by Magento. Add
  `cache: {redis: {}}` for a single-instance, dev-sized Valkey that
  the operator runs itself.
- **Queue** (RabbitMQ). Optional — Magento runs in synchronous-
  processing mode without it, fine for dev. Required if you
  enable consumers.
- **Consumers + cron**. Disabled by default. Enable with
  `spec.consumers.enabled: true` and `spec.cron.enabled: true`;
  consumers require `spec.queue` to be set.
- **Media storage**. Defaults to an `emptyDir` (ephemeral, demos
  only). Production deployments set `spec.media.s3` (recommended)
  or `spec.media.pvc` (dev/test only).
- **Varnish**. Optional caching layer in front of `/`. Enabled
  with `spec.varnish.enabled: true`.
- **Multi-store hostnames**. Set `spec.stores[]` to map public
  hostnames to Magento store codes.
- **HA**. These examples are single-replica everywhere except OpenSearch and use
  inline (dev-sized) data tiers. For production HA — frontend
  autoscaling, ref-mode data tiers, sizing recommendations — see
  [`operations/scaling.md`](operations/scaling.md).

The `config/samples/mage_v1alpha1_magestore.yaml` in the repo
exercises every stanza with the reference image; it's a useful
starting template once you've outgrown the minimal examples.

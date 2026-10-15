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
  bootstrap:
    admin:
      email: admin@example.com
```

Without any `spec.database`, `spec.search`, or `spec.cache`
stanza, the operator provisions the inline default for each
tier (MariaDB via `mariadb-operator`, OpenSearch via
`opensearch-k8s-operator`, Valkey run by the operator itself). All
are dev-sized — single replicas (three for OpenSearch, which can't
run as one node, see [#21](https://github.com/aknostic/mage-k8s-operator/issues/21)),
small PVCs — and adequate for a
local kind cluster or a non-production test deployment.

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

- **Search**, externally or inline. Magento 2.4.x hard-requires
  a search backend; the fresh-install example relies on the
  inline default. For external OpenSearch, see
  [`magestore-reference.md` § spec.search](magestore-reference.md#specsearch).
- **Cache** (Redis-protocol). Heavily used by Magento; inline
  defaults to a single-instance dev-sized Valkey.
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

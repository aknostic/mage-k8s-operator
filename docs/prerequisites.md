# Prerequisites

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

What needs to exist in your cluster **before** you `helm install`
the mage-operator. Audience: cluster admins setting up the
operator for one or more shops.

## TL;DR

| Component | Hard requirement? | Why |
|---|---|---|
| Kubernetes 1.28+ | yes | Gateway API v1 GA + structural CRD defaulting are load-bearing. |
| **Gateway API CRDs** (v1.2.x) | **yes, always** | The operator emits `Gateway` and `HTTPRoute` resources for every MageStore. No `external` opt-out. |
| **A Gateway API controller** (Envoy Gateway recommended) | **yes, always** | The Gateway API CRDs are inert without a controller to provision data planes. |
| `mariadb-operator` v26.x | only if you use `spec.database.mariaDb` or `mariaDbRef` | Inline/ref database modes create + watch its `MariaDB` CRs. |
| `opensearch-k8s-operator` v3.x | only if you use `spec.search.openSearch` or `opensearchRef` | Inline/ref search modes create + watch its `OpenSearchCluster` CRs. |
| `valkey-operator` (`hyperspike.io`) v0.0.x | only if you use `spec.cache.redis` or `redisRef` | Inline/ref cache modes create + watch its `Valkey` CRs. |
| `rabbitmq-cluster-operator` v2.x | only if you use `spec.queue.rabbitMq` or `rabbitMqRef` | Inline/ref queue modes create + watch its `RabbitmqCluster` CRs. |
| `cert-manager` v1.19+ | only if you use `spec.routing.tls.issuerRef` | Gateway API shim provisions and renews TLS certificates per host. Not needed for HTTP-only stores or `certificateSecretRef`. |

The four data-tier operators (mariadb / opensearch / valkey /
rabbitmq) are **all opt-out-able** via the corresponding `spec.X.external`
sibling — point at any reachable endpoint and the operator just
threads credentials into the image. This includes search:
`spec.search.external` accepts a host + optional credentials
Secret, and the operator emits `MAGENTO_OPENSEARCH_*` env vars
the same way it does for the inline / ref paths.

## Substituting another database operator

The discriminated-union pattern (ADR 004) is designed for this.
**Today**, use `spec.database.external` to point at a Service
fronting any MySQL-protocol backend — including ones provisioned
by other operators (percona-operator, vitess, etc.). The image
contract treats them all identically. **v0.2.0+** will add
first-class `perconaRef` (and other) siblings for users who
want the operator to also watch the upstream CR's status.

Same pattern applies to substituting Redis-compatible caches
(`spec.cache.external` already covers ElastiCache, Memorystore,
Upstash, Redis Cloud), AMQP brokers (`spec.queue.external` for
Amazon MQ, CloudAMQP), and S3-compatible object storage
(`spec.media.s3.endpoint` for Minio, Wasabi, Cloudflare R2,
Scaleway Object Storage).

## Pinned versions (v0.1.0 known-good)

These are the versions the operator's go.mod / envtest / dev
script pin against. Other minor versions likely work; bumping
across majors needs operator-side validation.

| Operator | Version | Install command |
|---|---|---|
| Gateway API CRDs | `v1.2.1` | `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml` |
| Envoy Gateway | `v1.3.0` | `helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version v1.3.0 -n envoy-gateway-system --create-namespace` |
| mariadb-operator | `26.3.0` | `helm upgrade --install mariadb-operator-crds mariadb-operator/mariadb-operator-crds --version 26.3.0`<br>`helm upgrade --install mariadb-operator mariadb-operator/mariadb-operator --version 26.3.0 -n mariadb-operator --create-namespace` |
| opensearch-k8s-operator | `3.0.2` | `helm upgrade --install opensearch-operator opensearch-operator/opensearch-operator --version 3.0.2 -n opensearch-operator-system --create-namespace` |
| valkey-operator (hyperspike) | `v0.0.61` | `kubectl apply -f https://github.com/hyperspike/valkey-operator/releases/download/v0.0.61/install.yaml` |
| rabbitmq-cluster-operator | `v2.20.1` | `kubectl apply -f https://github.com/rabbitmq/cluster-operator/releases/download/v2.20.1/cluster-operator.yml` |
| cert-manager (optional) | `v1.19.2` | `helm install cert-manager jetstack/cert-manager --namespace cert-manager --create-namespace --version v1.19.2 --set installCRDs=true --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 --set config.kind=ControllerConfiguration --set config.enableGatewayAPI=true` |

For local development on kind, `scripts/dev-k8s-setup.sh` runs
all of the above as one command.

## cert-manager (optional)

Required only if you use `spec.routing.tls.issuerRef` to have the
operator manage TLS certificates via cert-manager. Skip this for
HTTP-only deployments or when using `spec.routing.tls.certificateSecretRef`
with a pre-issued cert.

cert-manager 1.14+ ships the Gateway API shim that `spec.routing.tls.issuerRef`
depends on. It watches the per-MageStore Gateway for the
`cert-manager.io/cluster-issuer` (or `cert-manager.io/issuer`)
annotation, then provisions and renews one Certificate per
`spec.stores[].host` automatically.

The `config.enableGatewayAPI=true` flag is required: without it cert-manager's
Gateway controller is not registered and issuer annotations on Gateway resources
are silently ignored.

Install:

```bash
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.19.2 \
  --set installCRDs=true \
  --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 \
  --set config.kind=ControllerConfiguration \
  --set config.enableGatewayAPI=true
```

You also need a `ClusterIssuer` (or namespace-scoped `Issuer`) before
the MageStore can request a certificate. See
[`quickstart.md`](quickstart.md#enabling-tls) for a Let's Encrypt
HTTP-01 example.

## Other Gateway API controllers

Envoy Gateway is the recommended controller because it's what the
operator's `eg` GatewayClass hard-codes today. To use Cilium,
Traefik, HAProxy, or another Gateway API implementation, you
need either:

- **A `GatewayClass` named `eg` aliased at your preferred controller.**
  Most controllers let you create multiple GatewayClasses pointing
  at the same control plane; create one named `eg` so the
  operator's HTTPRoutes attach correctly. Workable hack, not
  documented as supported.
- **Wait for `spec.routing.gatewayClassName`** to land in v0.2.0+
  (planned Phase-2 CRD field).
  This is the proper escape hatch.

## Startup behavior with missing dependency CRDs

The operator probes for each dependency operator's CRDs at startup
and degrades gracefully when one or more are absent. You only need
to install the operators backing the modes you actually use.

- **Gateway API CRDs (`Gateway`, `HTTPRoute`)** are a hard
  requirement. The operator emits HTTPRoutes unconditionally, so
  if these CRDs are missing the operator pod fails fast at
  startup with a clear error pointing at the install command in
  the table above.
- **The four data-tier operators** are soft. When their CRDs are
  missing, the operator logs a one-line warning at startup naming
  the missing CRD + operator + which `spec.<tier>` modes are
  affected. The corresponding informer is not registered, so the
  manager boots cleanly. MageStores using the affected inline /
  ref modes surface a `*Ready=False,
  Reason=DependencyOperatorMissing` condition; the
  [troubleshooting guide](troubleshooting.md) names the operator
  to install. MageStores using only `spec.<tier>.external` (or
  not configuring the tier at all) work fine.

Capability detection runs once at startup. If you install a
missing dependency operator after the fact, **restart the
mage-operator pod** to pick up the change.

## What `helm install` does NOT install

The mage-operator chart ships only the operator (Deployment, RBAC,
metrics Service, MageStore CRD). It does not pull in the
dependency operators as subcharts — the chart stays minimal so:

- Cluster admins who already run mariadb-operator (or another
  flavor) at a specific version are not fought.
- Version-locking the dependency operators to a chart-managed
  version doesn't surprise users on minor mage-operator bumps.
- Operations teams keep control over which operators run in their
  cluster and at what version.

If you want a one-command install for demos or small shops, run
`scripts/dev-k8s-setup.sh` (kind-targeted) before
`helm install mage-operator`. A "kitchen sink" sibling chart
that bundles everything is on the v0.2.0+ punch list as one
possible option.

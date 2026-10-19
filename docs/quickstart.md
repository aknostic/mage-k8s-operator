# Quickstart

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

End-to-end installation on a Kubernetes cluster. Two paths:

- **You have your own Magento image.** ~5 minutes from a fresh
  cluster to a working store. This is the canonical path; jump to
  [Step 1](#step-1--install-the-dependency-operators).
- **You don't have an image yet** and want to try the operator
  end-to-end. The repo ships a reference image you can build and
  use locally; ~30 minutes for the first build (one-time). Skip to
  [Using the reference image on kind](#using-the-reference-image-on-kind).

## What you'll have at the end

```
$ curl -H "Host: store.local" http://localhost:8080/
HTTP/1.1 200 OK
…the Magento storefront…

$ kubectl -n default get magestore
NAME    READY   AGE
store   True    8m
```

A `MageStore` resource named `store` reconciles to:

- a managed MariaDB cluster (via `mariadb-operator`),
- an OpenSearch cluster (via `opensearch-operator`),
- a Valkey instance for cache + session (run by the operator itself),
- a RabbitMQ broker for queue consumers (via `rabbitmq-cluster-operator`),
- a frontend Deployment + Service running your Magento image,
- consumer Deployments + cron CronJobs against the broker,
- a Varnish Deployment fronting the frontend with an operator-generated VCL,
- an HTTPRoute attached to an Envoy Gateway exposing the storefront.

All owned by the `MageStore` — `kubectl delete magestore store`
cleans it all up.

## Prerequisites

You need these installed locally:

- [`kubectl`](https://kubernetes.io/docs/tasks/tools/) v1.28+
- [`helm`](https://helm.sh/docs/intro/install/) v3.14+
- A Kubernetes cluster you can `kubectl apply` against. For a
  local cluster: [`kind`](https://kind.sigs.k8s.io/) v0.23+ and
  Docker (or compatible runtime).
- Docker and `make`, to build the operator image — needed until
  v0.7.0 is published on `ghcr.io`.

## Step 1 — install the dependency operators

The mage-operator composes upstream operators for the four data
tiers — MariaDB, Valkey/Redis, OpenSearch, RabbitMQ — plus Envoy
Gateway for routing. Install all of them before installing the
mage-operator itself.

### On a fresh kind cluster

The repo ships a script that creates a kind cluster and installs
every dependency in one pass. Pull and run it:

```bash
curl -sSfL https://raw.githubusercontent.com/aknostic/mage-k8s-operator/main/scripts/dev-k8s-setup.sh -o dev-k8s-setup.sh
# Inspect before running — it does a lot:
less dev-k8s-setup.sh
bash dev-k8s-setup.sh
```

Re-running is idempotent; reuse the cluster across sessions.

### On any other cluster (EKS, Kapsule, on-prem, etc.)

Install each operator individually. The version pins and exact
`helm install` / `kubectl apply` commands live in
[`prerequisites.md`](prerequisites.md). The three data-tier
operators are opt-out — if you're using a managed cloud service
(RDS, ElastiCache, OpenSearch Service, Amazon MQ) you only need
the operators for the tiers you provision in-cluster.

Gateway API CRDs and Envoy Gateway are **always required** —
the operator emits HTTPRoute and Gateway resources unconditionally.

## Step 2 — install the mage-operator

Until v0.7.0 is published on `ghcr.io`, build the operator from
source. On kind:

```bash
git clone https://github.com/aknostic/mage-k8s-operator.git
cd mage-k8s-operator
make docker-build IMG=mage-operator:dev
kind load docker-image mage-operator:dev --name mage-operator-dev
helm install mage-operator charts/mage-operator \
  --set image.repository=mage-operator --set image.tag=dev \
  --namespace mage-operator-system --create-namespace
```

On any other cluster, push the image to a registry the cluster can
pull from (`make docker-build docker-push IMG=<registry>/mage-operator:dev`)
and set `image.repository` / `image.tag` to match.

Verify:

```bash
kubectl -n mage-operator-system rollout status deployment/mage-operator --timeout=60s
```

## Step 3 — apply a MageStore

The smallest viable MageStore for a fresh install:

```yaml
# store.yaml
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

```bash
kubectl apply -f store.yaml
```

For more example shapes (pre-bootstrapped database, multi-store,
S3 media, Varnish enabled, consumers + cron, custom resources):
see [`minimal-magestore.md`](minimal-magestore.md) and the
field-by-field [`magestore-reference.md`](magestore-reference.md).

If your image is in a private registry, set `imagePullSecrets` on
the namespace's default ServiceAccount before applying — the
operator does not currently emit pull secrets via the CRD.

## Step 4 — wait for Ready

```bash
kubectl wait --for=condition=Ready magestore/store --timeout=10m
```

Status conditions tell you what's happening at any moment:

```bash
kubectl get magestore store -o jsonpath='{.status.conditions}' | jq
```

Each `Type` (`DatabaseReady`, `SearchReady`, `CacheReady`,
`QueueReady`, `WorkloadsReady`, `VarnishReady`, `ConsumersReady`,
`CronReady`, `StoreInstalled`, `UpgradeReady`, `Ready`) carries a
`Reason` and `Message` explaining its current state. The
condition-by-condition reference lives in
[`troubleshooting.md`](troubleshooting.md).

## Step 5 — open the storefront

For an in-cluster test (any cluster):

```bash
kubectl port-forward service/store-frontend 8080:8080 &
curl -i http://localhost:8080/
```

For a Gateway-routed test on kind (Envoy Gateway from the dev
script):

```bash
ENVOY_POD=$(kubectl -n envoy-gateway-system get pods -l \
  gateway.envoyproxy.io/owning-gateway-name=store-gateway \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n envoy-gateway-system port-forward "$ENVOY_POD" 8080:10080 &
curl -i http://localhost:8080/
```

Expect HTTP 200 with the Magento default theme HTML. The first
request is slow (Magento boots its DI container on cold start);
subsequent requests serve from Varnish.

For the admin panel:

```bash
# The operator-generated admin password and admin URL live in the
# per-store admin Secret. The admin path is random on fresh stores.
kubectl get secret store-admin -o jsonpath='{.data.password}' | base64 -d; echo
kubectl get secret store-admin -o jsonpath='{.data.adminUrl}' | base64 -d; echo

# Open the admin URL in a browser.
# Username: admin
# Password: the value above
```

First admin login enrolls you in Magento's mandatory 2FA. Skip
this in dev by disabling the modules:

```bash
kubectl exec deployment/store-frontend -- \
  bin/magento module:disable Magento_TwoFactorAuth Magento_AdminAdobeImsTwoFactorAuth
```

## Using the reference image on kind

If you don't have your own Magento image yet, the repo includes a
reference image at `magento-test-image/` that satisfies the
operator's image contract. Useful for trying the operator
end-to-end before investing in your own image build.

The first build takes ~25-30 minutes (composer pulls ~600
packages; `setup:di:compile` and `setup:static-content:deploy`
are CPU-bound). Allow ~4 GB of disk space.

```bash
# Clone the repo (skip if you already did in Step 2).
git clone https://github.com/aknostic/mage-k8s-operator.git
cd mage-k8s-operator

# Build and load the image.
docker build -t mage-operator/magento-reference:2.4.8 magento-test-image/
kind load docker-image mage-operator/magento-reference:2.4.8 \
  --name mage-operator-dev

# Apply the bundled demo MageStore (uses the reference image,
# enables every feature: workloads + consumers + cron + varnish).
kubectl apply -k config/samples/
```

Then wait for Ready as in [Step 4](#step-4--wait-for-ready) — note
the demo CR is in the `demo` namespace, not `default`:

```bash
kubectl -n demo wait --for=condition=Ready magestore/store --timeout=10m
```

The image-contract specification at
[`image-contract.md`](image-contract.md) is the canonical
reference for building your own image. Fork
`magento-test-image/` as a starting template, or roll your own
from scratch.

## Use your own image (replacing the reference image)

After the reference-image walkthrough, point the MageStore at
your own image:

```yaml
spec:
  image:
    repository: registry.example.com/myshop/magento
    tag: "2.4.8-custom"
```

For a private registry: set `imagePullSecrets` on the
namespace's default ServiceAccount, or via your customer-owned
ServiceAccount referenced by the workload Pods.

## Demo SMTP sink (mailpit)

Magento sends transactional email — order confirmations, password
resets, customer registrations — through the host configured in
`spec.smtp.inline.host` (or the SMTP-relay credentials in
`spec.smtp.secretRef`). For a demo or dev cluster you don't need a
real provider; [mailpit](https://mailpit.axllent.org/) is a tiny
in-cluster SMTP sink that captures every send and exposes them on
a web UI you can `kubectl port-forward` to. Drop these manifests
into your demo namespace alongside the MageStore:

```yaml
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mailpit
spec:
  replicas: 1
  selector: {matchLabels: {app: mailpit}}
  template:
    metadata: {labels: {app: mailpit}}
    spec:
      containers:
      - name: mailpit
        image: axllent/mailpit:v1.20
        ports:
        - {containerPort: 1025, name: smtp}
        - {containerPort: 8025, name: http}
        env:
        - {name: MP_SMTP_AUTH_ALLOW_INSECURE, value: "true"}
---
apiVersion: v1
kind: Service
metadata:
  name: mailpit
spec:
  selector: {app: mailpit}
  ports:
  - {name: smtp, port: 1025, targetPort: 1025}
  - {name: http, port: 8025, targetPort: 8025}
```

Reference it from the MageStore (same namespace as mailpit):

```yaml
spec:
  smtp:
    inline:
      host: mailpit.<your-namespace>.svc.cluster.local
      port: 1025
      from: noreply@example.com
```

To inspect captured email:

```bash
kubectl port-forward svc/mailpit 8025:8025 &
open http://localhost:8025/
```

> **Production stores: do NOT use mailpit.** Substitute the SMTP
> relay's hostname (Postmark, SES, SendGrid, your corporate relay,
> etc.) in `spec.smtp.inline.host`, or supply provider credentials
> via `spec.smtp.secretRef`. Mailpit drops every message on the
> floor; sending production traffic through it loses the email.

## DNS pinning across demo-namespace recreations

If you tear down and recreate the demo namespace, the cluster's
LoadBalancer service gets re-provisioned and the storefront's
external IP changes — your DNS A-record points at the old IP and
the storefront stops answering. Two ways to handle it:

- **Update DNS each time.** Acceptable in dev: re-run `kubectl get
  gateway store-gateway -o jsonpath='{.status.addresses[0].value}'`
  after the new namespace's Gateway shows up, then update the A
  record. Cert-manager re-issues the TLS cert against the new IP
  on its next reconcile.
- **Pin the LoadBalancer IP** with a cloud-provider-specific
  annotation. The operator chart deliberately doesn't bundle this
  (every cloud has its own annotation key and ID format), but
  applying it directly to the Gateway-owned Service before
  teardown keeps the IP stable. On Scaleway Kapsule:

  ```bash
  # Look up the current LB ID once, then annotate the Service so
  # the next provisioning re-uses it.
  kubectl annotate svc <gateway-svc-name> \
    service.beta.kubernetes.io/scw-loadbalancer-id=<lb-uuid>
  ```

  Equivalents: AWS uses `service.beta.kubernetes.io/aws-load-
  balancer-static-ip`; GCP uses `cloud.google.com/load-balancer-
  type` plus a pre-reserved `loadBalancerIP`. Check your
  provider's documentation.

## Cleanup

```bash
# Tear down the MageStore + everything it owns.
kubectl delete magestore store

# Tear down the kind cluster entirely.
kind delete cluster --name mage-operator-dev
```

## Enabling TLS

> **Production / publicly-reachable clusters only.** The steps below
> use Let's Encrypt HTTP-01 validation, which requires a real DNS
> name resolving to your cluster's load balancer. A local kind cluster
> without public DNS won't complete HTTP-01 — use
> `certificateSecretRef` with a self-signed cert instead, or skip TLS
> on kind entirely.

By default the operator's Gateway listens on HTTP port 80. To serve
your storefront over HTTPS, you need a `spec.stores` list (HTTPS
listeners are per-hostname) and a TLS source — either cert-manager or
a pre-issued Secret.

### Step 1 — install cert-manager (one-time, cluster-wide)

```bash
helm repo add jetstack https://charts.jetstack.io
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.14.5 --set installCRDs=true
```

Verify:

```bash
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=60s
```

### Step 2 — create a ClusterIssuer

For Let's Encrypt HTTP-01 (replace `you@example.com` and the gateway
name/namespace to match your store):

```yaml
# cluster-issuer.yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    email: you@example.com
    server: https://acme-v02.api.letsencrypt.org/directory
    privateKeySecretRef:
      name: letsencrypt-prod-account-key
    solvers:
      - http01:
          gatewayHTTPRoute:
            parentRefs:
              - name: store-gateway   # <store>-gateway
                namespace: default
                kind: Gateway
```

```bash
kubectl apply -f cluster-issuer.yaml
```

### Step 3 — opt the MageStore into HTTPS

Add `spec.stores` and `spec.routing.tls` to your MageStore:

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

### Step 4 — point DNS at the Gateway

Retrieve the Gateway's external IP or hostname:

Replace `store` in the command below with your MageStore name (e.g. `myshop-gateway` for a store named `myshop`):

```bash
kubectl get gateway store-gateway \
  -o jsonpath='{.status.addresses[0].value}'
```

Create an A record (or CNAME for cloud load balancers) pointing
`shop.example.com` at that address.

> **Note: CoreDNS negative-cache gotcha**
>
> If cert-manager's first ACME order takes 5+ minutes to validate, you may be hitting CoreDNS's negative-cache TTL. CoreDNS caches NXDOMAIN responses for `_acme-challenge.<your-host>` for the duration of the SOA's `minimum` TTL (typically 5 min). cert-manager creates the TXT record but CoreDNS still serves the cached NXDOMAIN.
>
> Workaround:
> ```bash
> kubectl -n kube-system rollout restart deploy coredns
> ```
> Re-run the `kubectl get certificate <store>-gateway-tls-<host-slug> -w` command; the order should resolve within ~30s after CoreDNS restarts.
>
> Permanent fix: lower the SOA's `minimum` TTL in your zone configuration, or trigger a CoreDNS restart in your provisioning automation alongside any new HTTPS host. cert-manager 1.18+ has improved retry behavior here but doesn't eliminate the race entirely.

### Step 5 — wait for the certificate

cert-manager provisions one Certificate per hostname. Track progress:

```bash
kubectl get certificate
```

Once `READY` is `True`, browse to `https://shop.example.com/`. The
HTTP listener on port 80 stays active alongside HTTPS. To enforce HTTPS (redirect HTTP visitors to HTTPS), add a redirect rule on the HTTPRoute the operator generates — see the Gateway API docs for `HTTPRoute` redirect filters.

For BYO certs (wildcard, corporate PKI, non-cert-manager providers),
use `spec.routing.tls.certificateSecretRef` instead — see
[`magestore-reference.md`](magestore-reference.md#specroutingtls).

## What's next

- [`minimal-magestore.md`](minimal-magestore.md) — copy-pasteable
  example CRs for common shapes.
- [`magestore-reference.md`](magestore-reference.md) — every
  field on the `MageStore` resource, with defaults and when to
  set them.
- [`operations/scaling.md`](operations/scaling.md) — production
  / HA configuration.
- [`image-contract.md`](image-contract.md) — the operator ↔ image
  interface specification.
- [`troubleshooting.md`](troubleshooting.md) — condition reasons
  reference.

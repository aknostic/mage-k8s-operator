# mage-operator

Run your Magento, Adobe Commerce, or Mage-OS store on Kubernetes
declaratively. You bring a container image; the operator owns the
Kubernetes shape — database, search, cache, queue, workloads,
routing, consumers, cron, media storage, optional Varnish.

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

## Why use this

Running Magento on Kubernetes for one shop is a solved problem:
write a Helm chart with your specific assumptions baked in.
Running it for many shops, or across dev / staging / prod with the
same code path, is not — everyone ends up with a slightly
different Helm chart, duplicating decisions about MariaDB
provisioning, Redis layout, consumer Deployments, cron groups,
Varnish VCL, multi-store dispatch, upgrade orchestration, and
public-edge TLS.

This operator lifts those decisions into a single CRD. You
describe *what* you want — a `MageStore` with N frontend
replicas, M consumer workers, S3 media, two storefront hosts,
your image — and the operator owns the *how*. It composes
existing operators (`mariadb-operator`, `opensearch-k8s-operator`,
`rabbitmq-cluster-operator`, Envoy Gateway) rather than embedding
their logic; the Valkey cache is run by the operator itself.

## Status

**Alpha (v0.x).** Release vX.Y.Z is the first on `ghcr.io`: the
operator image and the Helm chart install from the registry, with no
source build. Earlier releases (up to v0.6.4) were published before
the project moved to GitHub and are no longer publicly available. The
operator runs end-to-end on kind and has been validated on a real
Scaleway Kapsule cluster. See [`CHANGELOG.md`](CHANGELOG.md) for
what each release contains and what is still in progress.

The CRD shape is not yet frozen — breaking field changes are gated
to minor version bumps during the v0.x line and documented in the
CHANGELOG.

**Don't use this in production yet.**

## Quick install

If you have a Magento container image that honors the
[image contract](docs/image-contract.md), you can have a working
store on Kubernetes in ~5 minutes:

```bash
# 1. Install the dependency operators (MariaDB, OpenSearch,
#    RabbitMQ, Envoy Gateway). On a kind cluster:
curl -sSfL https://raw.githubusercontent.com/aknostic/mage-k8s-operator/main/scripts/dev-k8s-setup.sh | bash

# On any other cluster, install each operator manually — see
# docs/prerequisites.md for the per-operator install commands and
# version pins.

# 2. Install the operator from ghcr.io (no login needed).
helm install mage-operator \
  oci://ghcr.io/aknostic/charts/mage-operator \
  --version X.Y.Z \
  --namespace mage-operator-system --create-namespace

# 3. Apply a minimal MageStore pointing at your image.
kubectl apply -f - <<'EOF'
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
EOF

# 4. Wait for Ready.
kubectl wait --for=condition=Ready magestore/store --timeout=10m
```

The admin password is operator-generated; retrieve it with
`kubectl get secret store-admin -o jsonpath='{.data.password}' | base64 -d`.

For more `MageStore` examples (pre-bootstrapped database,
multi-store, S3 media, Varnish enabled): see
[`docs/minimal-magestore.md`](docs/minimal-magestore.md) and the
field-by-field [`docs/magestore-reference.md`](docs/magestore-reference.md).

For production / HA configuration:
[`docs/operations/scaling.md`](docs/operations/scaling.md).

### Don't have a Magento image yet?

A prebuilt reference image that satisfies the contract is published
at `ghcr.io/aknostic/magento-reference:2.4.8`. Use it as `spec.image`
in step 3 to try the operator end-to-end without building anything:

```yaml
  image:
    repository: ghcr.io/aknostic/magento-reference
    tag: "2.4.8"
```

It is a demo image, not a production one. The walkthrough is in the
[quickstart](docs/quickstart.md#using-the-reference-image). Its
Dockerfile is in `magento-test-image/` if you want to fork it.

### Build from source (contributors)

To run an unreleased change, build the operator yourself. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) and the
[quickstart](docs/quickstart.md#build-the-operator-from-source-contributors).

## Documentation

- **[`docs/quickstart.md`](docs/quickstart.md)** — end-to-end
  install on kind, including admin-panel access. Also covers the
  reference-image build path.
- **[`docs/minimal-magestore.md`](docs/minimal-magestore.md)** —
  smallest viable `MageStore` examples (pre-bootstrapped DB and
  fresh install).
- **[`docs/prerequisites.md`](docs/prerequisites.md)** — what
  needs to exist in your cluster before installing the operator,
  per-feature opt-out paths via `external` mode, and how to
  substitute alternative database / cache / search / queue
  operators.
- **[`docs/magestore-reference.md`](docs/magestore-reference.md)** —
  field-by-field reference for the `MageStore` CRD. Includes
  [`spec.routing.tls`](docs/magestore-reference.md#specroutingtls)
  — **public-edge TLS** via cert-manager Gateway API shim or BYO
  TLS Secret.
- **[`docs/operations/scaling.md`](docs/operations/scaling.md)** —
  HA configuration: stateless tier scaling, ref-mode recipes for
  the four data tiers, known v0.3.0-roadmap limitations.
- **[`docs/operations/upgrades.md`](docs/operations/upgrades.md)** —
  upgrading a `MageStore` via `spec.image.tag` changes.
- **[`docs/troubleshooting.md`](docs/troubleshooting.md)** —
  condition-by-condition reference: every `Reason` value the
  operator emits with "what it means" and "what to check."
- **[`docs/image-contract.md`](docs/image-contract.md)** — the
  operator ↔ image interface specification. Read this before
  building or adapting your own Magento image.
- **[`CHANGELOG.md`](CHANGELOG.md)** — release notes.

For contributors: [`docs/internals/`](docs/internals/) holds the
ADRs, release flow, SOPS workflow, and local SMTP setup.

## Cosign verification

Release container images and Helm charts on `ghcr.io` are signed
with cosign. Verify before pulling into production. The public key
(`cosign.pub`) is in this repo and attached to each GitHub Release.

```bash
# Operator image (the image tag keeps the "v" prefix).
cosign verify --key cosign.pub \
  ghcr.io/aknostic/mage-operator:vX.Y.Z

# Helm chart (the chart version has no "v" prefix).
cosign verify --key cosign.pub \
  ghcr.io/aknostic/charts/mage-operator:X.Y.Z
```

Release flow detail at
[`docs/internals/releasing.md`](docs/internals/releasing.md).

## Contributing

Issues and pull requests welcome. Before opening a substantive
PR, please open an issue first and read the relevant
[ADR](docs/internals/adr/) — especially ADR 004 (the
seeded-but-extractable dependencies pattern) and ADR 006 (workload
naming + Gateway API routing). Match the commit-message style of
the existing log.

Local development setup, tests and the PR checklist are in
[`CONTRIBUTING.md`](CONTRIBUTING.md); the release flow and ADRs
live under [`docs/internals/`](docs/internals/). To report a
security issue, see [`SECURITY.md`](SECURITY.md).

## License

Apache License 2.0. See [`LICENSE`](LICENSE) for the full text.

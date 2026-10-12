# Releasing mage-operator

The release flow is deliberately simple: bump the chart, push a
semver tag, let GitLab CI sign + publish.

## Cadence and versioning

- **Semver from `v0.1.0`.** No `v0.0.x` releases — the operator
  starts at `v0.1.0` so semver math (MAJOR.MINOR.PATCH) is
  meaningful from the first ship.
- **`v0.x.y` is alpha-stable**: API may change with documented
  deprecation in CHANGELOG between minor versions. `v1.0.0` lands
  when the CRD shape is frozen.
- **Patch releases** (`v0.1.1`, `v0.1.2`, …) ship bug fixes only.
- **Minor releases** (`v0.2.0`) ship new CRD fields, new
  reconciler steps, image-contract additions.

Per-release notes live in `CHANGELOG.md`; the changelog-check CI
stage requires every MR to touch it.

## Pre-release checklist

Before pushing a tag:

1. **`CHANGELOG.md`** — promote the `[Unreleased]` block to
   `[vX.Y.Z] - YYYY-MM-DD` and start a fresh `[Unreleased]`.
2. **`charts/mage-operator/Chart.yaml`** — bump `version` and
   `appVersion` to match the tag (chart `version` without the
   `v` prefix per Helm semver convention; `appVersion` keeps the
   `v` prefix to match the container image tag).
3. **`make test`** locally — the CI runs the same target on push.
4. **Confirm the latest `main`-branch CI pipeline is green** — the
   `tag-image` job polls for the SHA-tagged container image to
   exist, and a failed branch pipeline means there's no image to
   retag.
5. **Upgrade-arc validation — manual `--profile=medium` sweep**
   (gates v0.2.0+ minor releases that change `ensureUpgrade`,
   `ensureMaintenance`, or any code path the upgrade orchestrator
   touches; skip for patch releases that don't touch the upgrade
   surface). ADR-008 Validation Layer 2 is automated in
   `make test-e2e` (the kind-e2e suite hits the orchestrator with
   `setup:perf:generate-fixtures --profile=small`), but Layer 3 —
   the medium-profile sweep — runs manually because the fixture
   load takes 30-60 minutes. Recipe:

   ```bash
   # Bring up the same kind harness the e2e suite uses.
   make setup-test-e2e
   make build-reference-image

   # Apply a fresh MageStore at the reference image's current tag.
   kubectl apply -n mage-upgrade-medium -f - <<'EOF'
   apiVersion: mage.aknostic.com/v1alpha1
   kind: MageStore
   metadata: { name: store, namespace: mage-upgrade-medium }
   spec:
     image: { repository: mage-operator/magento-reference, tag: "2.4.8" }
     workloads: { frontend: { replicas: 1 } }
     database: { mariaDb: {} }
     search:   { openSearch: {} }
     bootstrap: { admin: { email: ops@example.com } }
   EOF

   # Wait for Ready=True (15-20 min cold).
   kubectl wait --for=condition=Ready --timeout=20m \
     -n mage-upgrade-medium magestore/store

   # Populate with the medium profile (24,000 simple products,
   # 50,000 orders — 30-60 min wall time, CPU-bound).
   POD=$(kubectl get pod -n mage-upgrade-medium \
     -l app.kubernetes.io/component=frontend -o name | head -1)
   kubectl exec -n mage-upgrade-medium "$POD" -- bin/magento \
     setup:perf:generate-fixtures \
     /app/setup/performance-toolkit/profiles/ce/medium.xml

   # Retag and trigger the upgrade arc.
   docker tag mage-operator/magento-reference:2.4.8 \
              mage-operator/magento-reference:2.4.8-rc-medium
   kind load docker-image mage-operator/magento-reference:2.4.8-rc-medium \
     --name mage-operator-test-e2e
   kubectl patch -n mage-upgrade-medium magestore/store --type=merge \
     -p '{"spec":{"image":{"tag":"2.4.8-rc-medium"}}}'

   # Watch the arc walk through. DDL on the populated tables is the
   # surface the medium profile exercises that small does not —
   # specifically catalog_product_entity, sales_order_grid, and
   # url_rewrite re-indexes against populated child rows.
   kubectl get magestore store -n mage-upgrade-medium -w
   ```

   Pass criteria: `UpgradeReady=True, Reason=UpToDate` and the
   storefront serves a 200 on `kubectl port-forward
   deployment/store-frontend` after the arc completes. Capture the
   wall-clock for each stage and add it to the release notes if it
   diverges materially from prior tags. Failure here blocks the tag
   regardless of how green the small-profile e2e was.

## Cutting the release

```bash
git tag -s vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

The signed (`-s`) tag is preferred when the operator's signing key
is configured locally; unsigned tags work the same as far as CI is
concerned.

## What CI does

A semver tag (`vX.Y.Z` matching `^v\d+\.\d+\.\d+`) triggers the
**release** stage of `.gitlab-ci.yml`. Three jobs run:

- **`tag-image`** uses
  [`crane`](https://github.com/google/go-containerregistry) to
  retag the existing `${IMAGE_NAME}:${CI_COMMIT_SHA}` image
  (built and signed by the previous `main`-branch pipeline) as
  `${IMAGE_NAME}:vX.Y.Z`. The crane retag is a manifest copy — no
  rebuild — so the image bytes (and the cosign signature attached
  to the digest) are preserved verbatim. The job polls up to 15
  minutes waiting for the SHA-tagged image to appear, allowing a
  tag pushed quickly after a merge to land cleanly.

- **`helm-push`** packages `charts/mage-operator/` and pushes the
  resulting `.tgz` to the GitLab container registry as an OCI
  artifact at `oci://${CI_REGISTRY_IMAGE}/charts/mage-operator`.
  Guarded against drift: the job fails if `Chart.yaml` `version`
  does not match the git tag (without the `v` prefix). Users
  install with:

  ```bash
  helm install mage-operator \
    oci://registry.gitlab.aknostic.com/aknostic/mage-operator/charts/mage-operator \
    --version X.Y.Z
  ```

- **`create-release`** uses GitLab's `release-cli` to create the
  GitLab release object with the tag name and a link to the
  container image.

## Image and chart signing (cosign)

Every container image built on `main` is signed by the
`sign-image` CI job using the cosign private key stored as
`COSIGN_PRIVATE_KEY_B64` in GitLab CI variables. The public key
is committed at the repo root as `cosign.pub`.

The image signature is attached to the image **digest**, not the
tag. When `tag-image` retags the SHA-tagged image as `vX.Y.Z`,
the signature still applies because both tags point at the same
digest.

Helm OCI charts are signed on tag pipelines by the `sign-chart`
job (release stage, runs after `helm-push`). Charts pushed via
`helm push` are OCI artifacts at the registry; cosign signs at
the manifest digest level and is mediaType-agnostic, so the same
key works for both image and chart. Chart signing is gated on
`/^v\d+\.\d+\.\d+/` tags only — snapshot chart pushes are not
signed (matching the image-signing posture, where only
`main`-branch builds get signed). **Chart signing landed in
v0.2.0**; v0.1.x charts are unsigned and stay that way (re-
signing post-hoc would change the chart's signature attestation
timing without changing the chart bytes — not worth the
inconsistency).

Verifiers run:

```bash
# Operator image (every release).
cosign verify \
  --key cosign.pub \
  registry.gitlab.aknostic.com/aknostic/mage-operator:vX.Y.Z

# Helm chart (v0.2.0 and later — note the chart version has no `v` prefix).
cosign verify \
  --key cosign.pub \
  registry.gitlab.aknostic.com/aknostic/mage-operator/charts/mage-operator:X.Y.Z
```

## SBOM

Every `main`-branch build also runs `syft` to produce SPDX and
CycloneDX SBOMs (`sbom.spdx.json`, `sbom.cdx.json`), retained as
GitLab CI artifacts for 90 days.

## Rollback

A bad release is rolled back by retagging:

```bash
git tag -d vX.Y.Z              # local
git push origin :vX.Y.Z        # delete remote tag
```

The container image tag and Helm OCI chart **remain in the
registry** — GitLab does not delete OCI artifacts on tag deletion.
Republish the previous good version's chart by re-tagging the
prior commit (`git tag vX.Y.Z-1 vX.Y.Z-1`) which is a no-op for
`tag-image` (the image already exists at that semver) but
re-publishes the chart and recreates the GitLab release object.

For users who already pulled the bad chart: bump the patch number
forward (`vX.Y.Z+1`) with the rollback fix rather than pushing a
mutated `vX.Y.Z` — semver immutability is a contract.

## Non-goals for v0.1.0

- **ArtifactHub publishing.** Deferred to v1.0.0. ArtifactHub
  listing requires (a) the GitLab OCI registry to be publicly
  readable, (b) an `artifacthub-repo.yml` file shipped alongside
  the chart, and (c) a manual repo registration on artifacthub.io
  — all of which are easier once the chart's surface has settled
  through the v0.x iteration. Users discover the chart via this
  README and the GitLab project README until v1.0.0.
- **Cosign keyless / Sigstore TUF.** v0.1.0 uses a long-lived
  cosign key. Keyless OIDC signing graduates when the GitLab
  runner's identity provider is configured.
- **Multi-arch images.** v0.1.0 ships `linux/amd64` only. A
  `linux/arm64` build lands in a Phase-2 minor release once the
  M-series Mac developer base is large enough to warrant the build
  time.

# Releasing mage-operator

The release flow is deliberately simple: bump the chart, push a
semver tag, let GitHub Actions build, sign and publish.

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

Per-release notes live in `CHANGELOG.md`; the `changelog` CI job
requires every pull request to touch it (label docs- or CI-only PRs
`skip-changelog`).

## Pre-release checklist

Before pushing a tag:

1. **`CHANGELOG.md`** — promote the `[Unreleased]` block to
   `[vX.Y.Z] - YYYY-MM-DD` and start a fresh `[Unreleased]`.
2. **`charts/mage-operator/Chart.yaml`** — bump `version` and
   `appVersion` to match the tag (chart `version` without the
   `v` prefix per Helm semver convention; `appVersion` keeps the
   `v` prefix to match the container image tag).
3. **`make test`** locally — the CI runs the same target on every
   pull request.
4. **Confirm CI on `main` is green** — the release workflow builds
   the tagged commit but does not re-run the test suite.
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

A semver tag (`vX.Y.Z`) triggers
[`.github/workflows/release.yml`](../../.github/workflows/release.yml),
a single job that:

1. **Guards the version.** Fails if `Chart.yaml` `version` does not
   match the tag (without the `v` prefix), so a tag pushed without a
   chart bump cannot re-publish an old chart version.
2. **Builds and pushes the image** for `linux/amd64` and
   `linux/arm64` from the tagged commit, as
   `ghcr.io/aknostic/mage-operator:vX.Y.Z` and `:latest`.
3. **Scans it with Trivy.** HIGH/CRITICAL findings fail the release
   before anything is signed or the chart is published.
4. **Generates an SPDX SBOM** for the image digest.
5. **Signs the image** with cosign.
6. **Packages, pushes and signs the chart** at
   `oci://ghcr.io/aknostic/charts/mage-operator`.
7. **Creates the GitHub Release** with generated notes, attaching the
   chart `.tgz`, `sbom.spdx.json` and `cosign.pub`. `v0.*` tags and
   tags with a `-suffix` are marked as pre-releases.

Users install with:

```bash
helm install mage-operator \
  oci://ghcr.io/aknostic/charts/mage-operator \
  --version X.Y.Z
```

The workflow reads the repository secrets `COSIGN_PRIVATE_KEY_B64`
(base64 of the cosign key matching `cosign.pub`) and
`COSIGN_PASSWORD`. Packages pushed to GHCR can start out private;
after the first release, check that the `mage-operator` image and
the `charts/mage-operator` chart are public in the organization's
package settings.

### Releases before v0.7.0

v0.1.0 – v0.6.4 were published by the project's former GitLab
pipeline, and their artefacts stay there:

- image: `registry.gitlab.aknostic.com/aknostic/mage-operator:vX.Y.Z`
- chart: `oci://registry.gitlab.aknostic.com/aknostic/mage-operator/charts/mage-operator`

They verify against the same `cosign.pub`.

## Image and chart signing (cosign)

Release images and charts are signed with a long-lived cosign key
held in the `COSIGN_PRIVATE_KEY_B64` / `COSIGN_PASSWORD` repository
secrets. The public key is committed at the repo root as
`cosign.pub`.

Signatures attach to the manifest **digest**, not the tag. Helm OCI
charts are OCI artifacts too, and cosign is mediaType-agnostic, so
the same key and verify command work for both. **Chart signing
landed in v0.2.0**; v0.1.x charts are unsigned and stay that way
(re-signing post-hoc would change the chart's signature attestation
timing without changing the chart bytes — not worth the
inconsistency).

Verifiers run:

```bash
# Operator image.
cosign verify \
  --key cosign.pub \
  ghcr.io/aknostic/mage-operator:vX.Y.Z

# Helm chart (note the chart version has no `v` prefix).
cosign verify \
  --key cosign.pub \
  ghcr.io/aknostic/charts/mage-operator:X.Y.Z
```

For v0.6.4 and earlier, use the GitLab registry paths listed under
[Releases before v0.7.0](#releases-before-v070).

## SBOM

Every release generates an SPDX SBOM (`sbom.spdx.json`) for the
image digest and attaches it to the GitHub Release.

## Rollback

Releases are never mutated. To stop people picking up a bad
release, delete its GitHub Release and tag:

```bash
gh release delete vX.Y.Z
git tag -d vX.Y.Z              # local
git push origin :vX.Y.Z        # delete remote tag
```

The image and Helm chart **remain in GHCR** — deleting a tag or a
release does not delete packages, and `:latest` keeps pointing at
the bad image until the next release. Delete the package versions
in the organization's package settings if they must go.

Then bump the patch number forward (`vX.Y.Z+1`) with the fix rather
than re-pushing `vX.Y.Z` — semver immutability is a contract.

## Non-goals for v0.1.0

- **ArtifactHub publishing.** Deferred to v1.0.0. ArtifactHub
  listing requires (a) the chart to be publicly pullable, (b) an
  `artifacthub-repo.yml` file shipped alongside the chart, and (c) a
  manual repo registration on artifacthub.io — all of which are
  easier once the chart's surface has settled through the v0.x
  iteration. Users discover the chart via this repo's README until
  v1.0.0.
- **Cosign keyless / Sigstore TUF.** v0.x uses a long-lived cosign
  key. GitHub Actions can provide the OIDC identity keyless signing
  needs, but switching changes the verify command for every user,
  so it waits for a deliberate release.

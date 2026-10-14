# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

First session of the v0.7.0 security baseline arc.
ADR 014 sets the architectural shape; the umbrella `spec.security`
CRD group lands as a scaffold (`networkPolicies` and
`workloadSecurityContext` sub-blocks are placeholders that gain
controller wiring in v0.7.0 sessions 2/3); admin URL randomization
(gap #3 of the security baseline) ships end-to-end including the
routing-layer fix. Existing v0.6.x stores keep `/admin/` to
preserve admin bookmarks per ADR 014's upgrade-safety rule. No
release artefact yet — chart bump + tag wait until session 4
closes the arc.

### Added

- **`spec.security` umbrella CRD group.** New top-level field on
  `MageStoreSpec` carrying two placeholder sub-blocks:
  `networkPolicies` (`Enabled *bool`,
  `AdditionalIngressNamespaces []LabelSelector`,
  `AdditionalEgressCIDRs []string`) and `workloadSecurityContext`
  (`Pod *corev1.PodSecurityContext`, `Container
  *corev1.SecurityContext`). CRD shape only at this point;
  ensure-* controller wiring lands in v0.7.0 sessions 2/3. See
  ADR 014 for the why-one-group decision and the per-item
  upgrade-safety Secret-existence signal.
- **`spec.bootstrap.admin.backendFrontname` field** (regex
  `^[a-zA-Z0-9_-]{4,32}$`, optional, no default). Configurable
  Magento admin URL path segment. When unset, the operator
  generates a 16-character random URL-safe value once at first
  bootstrap and persists it in the `<store>-admin` Secret under
  key `backendFrontname`, alongside a convenience `adminUrl` key
  with the full `https://<host>/<frontname>/`. Subsequent
  reconciles read the sticky value from the Secret. Existing
  v0.6.x stores upgraded to v0.7.0 — detected by an existing
  `<store>-admin` Secret with no `backendFrontname` key —
  default to literal `admin` to preserve admin bookmarks per
  ADR 014's upgrade-safety rule.
- **ADR 014 — security baseline.** Captures the three
  architectural decisions binding v0.7.0 sessions 1-3: one
  umbrella `spec.security` group; shared upgrade-safety
  defaulting rule signalled by operator-owned Secret existence
  (per-item table maps each gap to its Secret); admin URL
  randomization on `AdminSpec` not `SecuritySpec`
  (bootstrap-time install argument, not reconcile-time policy).

### Changed

- **Project moved to GitHub** at
  [`aknostic/mage-k8s-operator`](https://github.com/aknostic/mage-k8s-operator).
  The Go module path is now `github.com/aknostic/mage-k8s-operator`
  (was `gitlab.aknostic.com/aknostic/mage-operator`). From v0.7.0 the
  operator image is published to `ghcr.io/aknostic/mage-operator` and
  the Helm chart to `oci://ghcr.io/aknostic/charts/mage-operator`; the chart's
  default `image.repository` follows. v0.6.4 and earlier stay on
  `registry.gitlab.aknostic.com`. Upgrading from v0.6.x: point
  `helm upgrade` at the new OCI URL.
- **CI moved to GitHub Actions** (`.github/workflows/`), replacing
  `.gitlab-ci.yml`. Release images are built from the tagged commit
  for `linux/amd64` and `linux/arm64`, and each release gets a GitHub
  Release with the chart, an SPDX SBOM and `cosign.pub` attached.
  Dependabot watches Go modules, Actions and the Dockerfile base
  image. `make docker-buildx` builds the Dockerfile directly, since
  its builder stage now pins `--platform=$BUILDPLATFORM` itself.

### Fixed

- **HTTPRoute admin rule follows the resolved `backendFrontname`.**
  Pre-fix, `internal/controller/ensure_routing.go` baked literal
  `/admin` PathPrefix into the admin-tier rule regardless of
  `--backend-frontname`, defeating the randomization at the
  gateway layer (fresh-install Magento served at `/<random>/`
  was still reachable via `/admin`). Post-fix,
  `ensureHTTPRoute` and `ensureHTTPSRoute` call the new
  `resolveAdminFrontname` helper (one Get on `<store>-admin`,
  with literal `admin` fallback for adoption cases or
  pre-bootstrap reconciles) and pass the resolved value to
  `buildRouteRules` / `buildRouteRulesWithXFP`. Existing routing
  tests pass without modification — none pre-create the admin
  Secret, so the fallback path keeps the v0.6.x `/admin/`
  assertion verbatim.
- **CI `container-scanning` job scoped to default branch.**
  Previously declared `needs: [build-and-push]` unconditionally
  but ran via `when: on_success`, so MR pipelines tripped on
  YAML validation (`build-and-push` only runs on the default
  branch). Aligned with sibling jobs `generate-sbom` and
  `sign-image`. Pre-existing latent bug — surfaced now because
  this is the project's first MR pipeline.
- **CI `changelog-check` job missing git.** The `alpine:3` image
  does not ship `git`; the diff command failed silently with a
  misleading `ERROR: CHANGELOG.md was not modified` message.
  Added an `apk add --no-cache git` before-script and pinned
  `GIT_DEPTH: "0"` so the diff against
  `$CI_MERGE_REQUEST_TARGET_BRANCH_SHA` always resolves.

### Security

- **Admin URL randomization closes the
  Magento-admin-front-door fingerprint surface.** `/admin/` was
  the first path every Magento attacker scans; Adobe Commerce
  hardening guide lists "randomize admin URL" as a baseline
  mitigation alongside 2FA (which the operator already wires
  correctly). v0.7.0 makes a 16-character random URL-safe path
  the default for fresh stores. Existing v0.6.x stores opt in
  via `kubectl edit magestore <name>` to set
  `spec.bootstrap.admin.backendFrontname`; UPGRADING.md will
  walk this when v0.7.0 ships.
- **`golang.org/x/net` bumped `v0.53.0` → `v0.55.0`** to clear
  **GO-2026-5026** (failure to reject ASCII-only Punycode-encoded
  labels in `golang.org/x/net/idna`). Reachable via the
  reconciler's k8s client `Update` call eventually invoking
  `idna.ToASCII`. Practical attack surface is negligible —
  the operator talks only to the in-cluster Kubernetes API
  endpoint, which the attacker does not control — but
  `govulncheck` flagged it on `main` after the v0.7.0 session-1
  merge, so the bump rides here rather than waiting for a
  v0.6.5 backport. The fix carries forward via the v0.7.0
  release.

## [v0.6.4] - 2026-05-08

Security patch. Bumps the Go toolchain from `1.26.2` to `1.26.3` to
clear six standard-library vulnerabilities flagged by `govulncheck`
on `main` after the upstream `1.26.3` release. Two are reachable
from the operator at runtime (`net/mail` quadratic parse via
`manager.New` → `mail.ParseAddress`; `net/http` HTTP/2 transport
infinite loop via the reconciler client), the others are reachable
only from test utilities or Windows-only paths — but the toolchain
is one artefact, so the bump ships in one go. No behavioural change,
no CRD change, no chart-shape change beyond the version field;
`helm upgrade` from 0.6.3 to 0.6.4 is a clean rollover that picks
up the rebuilt operator image.

### Security

- **Go toolchain `1.26.2` → `1.26.3`** in the Dockerfile builder,
  the `govulncheck` CI image, and the `test` CI image. Closes:
  - **GO-2026-4986** — quadratic string concatenation in
    `consumeComment` in `net/mail`. Reachable: `cmd.main` calls
    `manager.New`, which eventually calls `mail.ParseAddress`.
  - **GO-2026-4977** — quadratic string concatenation in
    `consumePhrase` in `net/mail`. Same reach as #4986.
  - **GO-2026-4918** — infinite loop in HTTP/2 transport when given
    a bad `SETTINGS_MAX_FRAME_SIZE` in
    `net/http/internal/http2`. Reachable: reconciler client
    `http.Client.Do`.
  - **GO-2026-4982** — bypass of meta content URL escaping causes
    XSS in `html/template`. Reachable from `test/utils` only.
  - **GO-2026-4980** — escaper bypass leads to XSS in
    `html/template`. Reachable from `test/utils` only.
  - **GO-2026-4971** — panic in `net.Dial`/`LookupPort` when
    handling a NUL byte on Windows. Operator does not target
    Windows; bumped for hygiene.

## [v0.6.3] - 2026-05-07

Bug-fix release. Three gaps surfaced during the v0.6.2 Kapsule
re-validation, all rooted in real-infra friction the operator's
local envtest didn't model: a stale-cache problem on shared node
pools, a cold-start race the recovery loop was burning ~3-5 min
unsticking on every install, and a documentation gap in the demo
onboarding. Closing them removes the last `kubectl cordon` /
`kubectl apply -f - <<EOF mailpit-yaml` reflexes from the
acceptance playbook. No new ADRs, one new optional spec field with
a sensible default. Brief at
`docs/internals/plans/2026-05-07-v0.6.3-real-infra-gaps.md`.

### Fixed

- **Workload Deployments default to `imagePullPolicy: Always`.**
  Frontend / admin / api workloads, the ~20 queue-consumer
  Deployments, and Varnish all now write `Always` onto their
  primary container's pull policy. Defends against the v0.6.2
  validation symptom: a node pool with the `:2.4.8-sampledata` tag
  cached at a v0.6.1-era digest sat next to fresh pools that pulled
  the v0.6.2 digest, so 8 of 21 consumer pods landed on the stale
  pool and exit-looped because their `consumers_wait_for_messages`
  was still `0`. `kubectl rollout restart` did NOT fix it
  (`IfNotPresent` + same tag → kubelet skipped the pull); the only
  workaround was `kubectl cordon <stale-node> + kubectl delete pod`.
  With `Always` the kubelet HEAD-resolves the registry's current
  digest on every pod start (KB on the wire, not GB) and cache
  drift can't take effect. One-shot Jobs (bootstrap, smtp probe,
  store-urls, media seed, upgrade) already pinned `Always` from
  v0.6.1 #2; this closes the long-running-pod side of the same
  pattern.
- **`ensureMediaSeed` gates on `SecretsReady=True` for S3 mode.**
  v0.6.2's validation showed the seed Job created at T+30s with
  `SecretsReady=False` (still waiting on the third OpenSearch node
  to initialise), looping on `CreateContainerConfigError: secret
  "<store>-config" not found` until the v0.6.2 recovery helper
  deleted-and-recreated it ~3 min later. Recovery worked — but
  burned cold-start budget unsticking a race the reconcile graph
  could have prevented in zero time. The structural gate (new row
  1.5 in the `ensureMediaSeed` state machine) returns
  `MediaSeeded=Unknown,Reason=AwaitingSecrets` until the dependent
  Secret materialises. Volume modes (emptyDir / PVC) are unaffected
  — their seeding runs as a workload-pod initContainer that's
  already gated transitively via `dependenciesReady`. The recovery
  code stays in place for OTHER cold-start races we haven't found
  yet; v0.6.3 just removes the *known* one from its workload.

### Added

- **`spec.workloads.imagePullPolicy` field** (optional,
  enum=`Always`/`IfNotPresent`/`Never`, defaults to `Always` via
  CRD schema). Single knob covers every long-running operator-owned
  pod (frontend / admin / api workloads, queue consumers, Varnish).
  Set to `IfNotPresent` for air-gapped registries (kubelet can't
  reach the registry on every pod start) or rate-limited registries
  (anonymous Docker Hub pulls). The operator does not validate
  digest-stability when this override is in effect.
- **`docs/quickstart.md` "Demo SMTP sink (mailpit)" section.**
  Copy-paste Deployment + Service YAML for mailpit, with a note that
  it is dev/demo only and production stores must substitute a real
  SMTP relay in `spec.smtp.inline.host` (or use `spec.smtp.secretRef`
  for credentials). Also adds a "DNS pinning across demo-namespace
  recreations" subsection covering the LoadBalancer-IP churn that
  happens when a demo namespace is torn down and recreated, with
  cloud-provider annotation hints (Scaleway / AWS / GCP). The
  operator chart deliberately does NOT bundle these — they are
  cloud-provider-specific machinery, not part of the operator's
  surface.

## [v0.6.2] - 2026-05-07

Bug-fix release. Three gaps surfaced during the v0.6.1 Kapsule
re-validation, all rooted in "delete the failed Job / remember the
magic flag" reflexes that the v0.6.1 session needed but no doc
captured. Closing them removes those reflexes from the v0.6.1
acceptance run. No new ADRs, no new spec fields beyond a status
sub-object. Brief at
`docs/internals/plans/2026-05-07-v0.6.2-real-infra-gaps.md`.

### Fixed

- **Operator recovers terminal-Failed owned Jobs.** `media-seed`,
  `smtp-probe`, `bootstrap`, `store-urls`, and `upgrade` Jobs that
  hit `JobFailed=True` (BackoffLimitExceeded / DeadlineExceeded) are
  now deleted and recreated by the operator across an exponential
  backoff window (30s → 2m → 5m → 10m → 30m, ±25% jitter) before
  surfacing `PermanentlyFailed` after the cap. The v0.6.1
  re-validation hit two distinct cold-start races that previously
  required `kubectl delete job` to unstick: a seed-Job ordering race
  against `<store>-config` and a 60s probe-Job deadline blown by
  cold-node image pull. Both now self-heal.
- **`UPGRADING.md` documents sticky `image.tag` + workload
  rollout-restart.** Releases that ever pinned `image.tag` via
  `--set` keep the prior operator image after `helm upgrade` until
  the user passes `--reset-values` (standard Helm semantics, but
  surprising the first time). The chart's UPGRADING.md now spells
  out the verify-then-reset procedure and the per-MageStore
  `kubectl rollout restart deploy` needed to pick up the rebuilt
  reference image.
- **Reference image: `consumers_wait_for_messages = 1`.**
  `magento-test-image/env.php` previously hard-coded `0`, the
  fast-exit-on-drain value Adobe documents for cron-managed
  consumers. The operator deploys each Magento queue consumer as a
  long-lived Deployment; with `0` every consumer pod exited the
  moment its queue drained, the kubelet treated that as a clean
  exit, and ~20 consumer pods churned in CrashLoopBackOff with
  `ConsumersReady=False` blocking the `Ready=True` rollup. Flipping
  to `1` makes consumers block on the queue waiting for new
  messages, which is what long-lived Deployments need. Comment
  block above the assignment is rewritten so it scopes to its
  actual purpose. `docs/image-contract.md` canonical example
  aligned.

### Added

- **`status.jobAttempts` map.** New per-Job-name retry state
  (`count`, `lastFailedAt`, `nextAttemptAt`) backing the recovery
  helper above. Visible via `kubectl get magestore -o yaml` —
  operators reading status see "we've burned 3 attempts already" on
  long-failing one-shot Jobs without having to count `kubectl logs`
  invocations.

## [v0.6.1] - 2026-05-06

Bug-fix release. Six real-infra gaps surfaced during the v0.6.0
Kapsule validation; this release closes all of them so the v0.6.0
acceptance playbook runs against a real cluster with zero manifest or
operator workarounds. No new ADRs, no new CRD fields. Brief at
`docs/internals/plans/2026-05-06-v0.6.1-real-infra-gaps.md`.

### Fixed

- **Helm chart now manages CRD upgrades.** The `MageStore` CRD moves
  from the chart's install-only `crds/` directory into `templates/`
  with `helm.sh/resource-policy: keep`. `helm upgrade` now applies
  schema changes; `helm uninstall` still preserves the CRD so
  existing CRs survive. One-time adoption step required when
  upgrading from `v0.6.0` — see
  `charts/mage-operator/UPGRADING.md`.
- **One-shot Jobs use `imagePullPolicy: Always`.** Customer image
  rebuilds under the same tag (e.g.
  `magento-reference:2.4.8-sampledata`) are picked up immediately by
  the seed (`<store>-media-seed`), probe (`<store>-smtp-probe`),
  bootstrap (`<store>-bootstrap`), upgrade
  (`<store>-upgrade-...`), and store-URLs (`<store>-store-urls`)
  Jobs. Workload Deployments keep the default `IfNotPresent` —
  customers should pin those by digest.
- **Admin password generator is class-checked.** The 20-character
  base64 password is now retried until it contains at least one
  digit AND one letter, eliminating the ~4% per-call probability of
  `setup:install` aborting with `"Your password must include both
  numeric and alphabetic characters."`
- **Media-seed Job uses `bash`, not `sh`.** The seed script declares
  `set -euo pipefail`; the reference image's `/bin/sh` is dash and
  doesn't support `pipefail`, which combined with `set -e` killed
  the script at line 1. The Job's container `Command` is now
  `["bash", "-c"]`.
- **Consumer args dropped invalid `--max-time` flag.** Magento
  2.4.x's `bin/magento queue:consumers:start` does not accept
  `--max-time`. Wall-clock bound is now enforced by wrapping the
  command in coreutils `timeout 3600`; `--max-messages=20000` stays.
- **Anonymous SMTP supported by the reference image.**
  `magento-test-image/entrypoint.sh` now tolerates `SMTP_USERNAME` /
  `SMTP_PASSWORD` being unset under `set -eu` and conditionally
  emits the `auth` / `user` / `password` lines in the generated
  `/etc/msmtprc`. Mailpit and IP-allowlisted relays (Postmark, AWS
  SES with sender authorization) work without the `MageStore` spec
  needing a stub auth pair. Image-contract doc updated.

## [v0.6.0] - 2026-05-06

### Added

- **`spec.smtp` field** — discriminated union (`inline | secretRef`)
  for SMTP configuration per ADR 012. Inline mode exposes Magento's
  5 native trans_email identities for per-identity From: email AND
  display name overrides. Probe Job (`<store>-smtp-probe`) using
  swaks runs EHLO+STARTTLS+AUTH against the configured relay and
  gates workloads on success. New `SMTPReady` condition.
- **`spec.media.seedFromImage` flag** — image-baked media seeding
  per ADR 011. `MediaSpec` restructured to a three-way discriminator
  (`emptyDir | pvc | s3`). Volume modes seed via `cp -rn`
  initContainer; S3 mode runs `bin/magento remote-storage:sync`
  Job. Image-contract addition: `/app/pub/media-baked/`. New
  `MediaSeeded` condition.
- **`ensureStoreURLs` reconcile step** — per ADR 013 phase 2.
  Hash-driven Job applies per-website/store-view URL overrides
  with `cache:flush` so storefront immediately reflects the new
  URLs. Auto-heals v0.5.x deployments stuck on `localhost:8080`.
  New `URLsApplied` condition.
- **`spec.bootstrap.admin.disableTFA` flag** — opt-out of TFA
  during bootstrap (gap #4). Appends
  `config:set twofactorauth/general/enable 0` +
  `module:disable Magento_TwoFactorAuth Magento_AdminAdobeImsTwoFactorAuth` +
  `setup:upgrade` to the bootstrap Job. Demo manifests use it;
  production manifests with SMTP configured leave it false.
- **`X-Forwarded-Proto: https` HTTPRoute filter** — belt-and-braces
  for non-Envoy Gateway controllers (gap #5). When `spec.routing.tls`
  is set, the operator emits a second HTTPRoute scoped via
  `sectionName` to each HTTPS listener with a `RequestHeaderModifier`
  filter forcing `X-Forwarded-Proto: https`.
- **Consumer runtime bounds** — `--max-messages=20000` and
  `--max-time=3600` on every queue consumer Deployment (gap #10).
  Magento consumers exit cleanly on their own schedule; Kubernetes
  restarts for the next batch.
- **`swaks` in reference image** — for the SMTP probe Job
  (gap #9 / ADR 012). ~150 KB; image-contract-required for
  customer images.

### Fixed

- **`setup:install` URLs derived from spec** (gap #1 phase 1; ADR
  013 phase 1). Replaces the hardcoded
  `--base-url=http://localhost:8080/` at `ensure_bootstrap.go:71`.
  Storefront now generates correct links from the start.
- **Default VCL doesn't cache 5xx responses** (gap #6). Single
  backend hiccup no longer served from cache for the configured TTL.
- **cert-manager Gateway API integration** in
  `scripts/install-dep-operators.sh` (gap #2). Switched the
  cert-manager install from a `kubectl apply` of the bundle manifest
  to `helm upgrade --install` so `config.enableGatewayAPI=true` can
  be passed; without it the cert-manager.io/issuer annotation on
  Gateway resources was ignored. Pinned cert-manager to v1.19.2 in
  both `scripts/install-dep-operators.sh` and `docs/prerequisites.md`
  to close the version drift.
- **Sample manifest store-code** changed from `default` to `base`
  for `type: website` entries (gap #3). Magento's actual default
  website code is `base`; `default` is the default store-view code.
- **Quickstart CoreDNS-negative-cache callout** between Step 4 and
  Step 5 of "Enabling TLS" (gap #7).

### Breaking changes

- **`spec.smtp` is now required to activate SMTP.** The previous
  implicit pattern — where any `<store>-smtp` Secret in the
  namespace was automatically mounted as `optional: true` envFrom
  on every pod — has been removed. Customers using the implicit
  pattern must add the following block to their MageStore CR to
  keep SMTP working:
  ```yaml
  spec:
    smtp:
      secretRef:
        name: <store>-smtp
  ```
  The referenced Secret must contain `SMTP_HOST` and `SMTP_FROM` keys
  at minimum (see ADR 012 §"secretRef mode → Secret key contract" for
  the full key list). Customers whose existing `<store>-smtp` Secret
  uses different key names will hit `SMTPReady=False,
  Reason=SecretMissingKeys` after migration; rename keys to match the
  contract or move to inline mode.

  Per gap #11 admin-email-defaulting policy: `spec.smtp` unset →
  no SMTP env vars emitted → outbound mail fails at transport with
  a clear log line. Loud-failure-beats-silent-leak. See ADR 012
  for full design.
- **`MediaSpec` discriminator restructured.** `spec.media: {}`
  (empty block) is now invalid; `spec.media: nil` (block omitted)
  still works for back-compat (per-pod emptyDir scratch). Explicit
  emptyDir is now `spec.media: { emptyDir: {} }` (required if you
  also want to set `seedFromImage: true`). Existing CRs with
  `pvc: {...}` or `s3: {...}` are unaffected.

## [v0.5.1] - 2026-05-04

### Fixed

- **Chart-bundled `MageStore` CRD re-synced from `config/crd/bases/`.**
  The chart's CRD copy at `charts/mage-operator/crds/mage.aknostic.com_magestores.yaml`
  had been frozen at the v0.3.0 schema since commit `4ac2753` (April 2026)
  — `make manifests` regenerates `config/crd/bases/` but did not propagate
  to the chart. v0.4.0 added the cache topology-change description (still
  cosmetic at the chart layer because annotations don't appear in the
  schema), and **v0.5.0 added the load-bearing `spec.routing.tls`
  block** which was absent from the chart CRD. Helm-installed v0.5.0
  operators rejected `spec.routing` with `unknown field` strict-decoding
  errors, making the headline v0.5.0 surface unreachable for chart users.
  Surfaced on the first real-infra `MageStore` apply during the Kapsule
  demo. No operator code changed — pure release artifact fix.
- **`scripts/install-dep-operators.sh` now creates the `eg` GatewayClass.**
  The Envoy Gateway helm chart provisions the controller and CRDs but
  does **not** ship a `GatewayClass` for it. Without this, Gateways the
  operator emits sit at `Programmed=False, Reason=NoMatchingGatewayClass`
  and never get a data plane. The script was claiming the chart installs
  the GatewayClass — corrected the comment and added an explicit
  `kubectl apply` for a `gateway.networking.k8s.io/v1` `GatewayClass`
  named `eg` pointing at `gateway.envoyproxy.io/gatewayclass-controller`,
  with a 60s `kubectl wait --for=condition=Accepted` so callers see a
  fast failure if the controllerName drifts in a future Envoy Gateway
  release. Surfaced during the v0.5.0 Kapsule fix-forward exercise:
  e2e on kind asserts operator outputs (Gateway emitted with
  `gatewayClassName: eg`), not data-plane readiness, so the gap slipped
  through. Idempotent re-runs are fine — `kubectl apply` no-ops.

### Changed

- **`make manifests` now runs `sync-chart-crds`** (a new target that
  mirrors `config/crd/bases/*.yaml` into `charts/mage-operator/crds/`).
  This prevents the chart-CRD drift that produced the v0.5.0 release
  artifact bug above. Standalone target available for chart-only
  re-syncs.

## [v0.5.0] - 2026-05-04

### Added

- **Public-edge TLS** via Gateway API. New `spec.routing.tls` field
  accepts either `issuerRef` (cert-manager-managed Issuer/ClusterIssuer)
  or `certificateSecretRef` (BYO `kubernetes.io/tls` Secret). When set,
  the operator adds per-host HTTPS listeners on port 443 to the
  per-MageStore Gateway alongside the existing HTTP/80 listener.
  cert-manager 1.14+'s Gateway shim provisions Certificates into
  `<store>-gateway-tls-<host-slug>` Secrets automatically when
  `issuerRef` is used. Default behavior is unchanged when
  `spec.routing.tls` is unset (HTTP only). Per ADR 010.
- **Operator-managed cert Secret naming convention.** When using
  `spec.routing.tls.issuerRef`, cert-manager provisions one Secret per
  `spec.stores[].host` following the pattern
  `<store>-gateway-tls-<host-slug>` where `host-slug` replaces `.` with
  `-`. Use `kubectl get secret -n <namespace>` to inspect them.
- **CEL apply-time validation for `spec.routing.tls`.** Setting both
  `issuerRef` and `certificateSecretRef`, or setting neither while
  `spec.routing.tls` is present, is rejected at `kubectl apply` time
  with the message `"exactly one of issuerRef or certificateSecretRef
  must be set"`. No retry needed — fix the manifest and re-apply.

### Validation

- New top-of-Reconcile check: `spec.routing.tls` set without
  `spec.stores` surfaces `Ready=False, Reason=TLSRequiresStoreHosts`.
  TLS listeners need SNI hostnames; this catches the misconfiguration
  before any ensure step runs.

### Tests

- New `internal/controller/ensure_routing_test.go` adds 5 envtest
  specs: HTTP-only baseline (v0.4.x regression), IssuerRef single-store,
  CertificateSecretRef, multi-store listener fan-out, and
  TLS-without-stores validation.

## [v0.4.0] - 2026-05-03

### Fixed
- **Sentinel-aware cache bootstrap.** `internal/controller/ensure_cache.go:valkeyBootstrapScript` now queries Sentinel via the Sentinel ClusterIP Service (`<store>-valkey-sentinel:26379`) before defaulting to ordinal-master role. When Sentinel reports a master other than this pod, the script appends `--replicaof <sentinel-promoted-master> 6379` regardless of pod ordinal, eliminating the v0.3.x split-brain window where a restarted pod 0 reverted to ordinal-master role after a Sentinel-driven failover. Cold-start (Sentinel not yet up, or quorum-loss / network failure) and single-instance mode (no Sentinel deployed) fall back to the v0.3.x ordinal logic; the single-instance path opts out via an empty `SENTINEL_HOST` short-circuit so no time is wasted on a Sentinel that will never exist. Closes the v0.3.0 known-gap "Restarted pod 0 reverts to ordinal-master role." Existing in-flight pods keep running the v0.3.x script until they themselves restart — no forced rollout, the fix takes effect naturally on the next pod restart (which is the scenario it targets). Regression-tested by two new envtest cases (HA bootstrap script contains the Sentinel-query stanza pointing at the Sentinel ClusterIP; single-instance script omits it) plus a new kind-e2e Context (`test/e2e/cache_test.go`) that provisions an HA cache, kills the Sentinel-tracked master, asserts Sentinel promotes a replica, kills pod 0 again to simulate node-loss restart, and asserts pod 0's launch ARGS include `--replicaof <new-master-fqdn> 6379`.
- **Cross-topology cache resize is annotation-opt-in.** `internal/controller/ensure_cache.go:ensureCacheInline` now refuses cross-topology cache changes (`replicas: 1` ↔ `replicas: 3+`) without `mage.aknostic.com/cache-topology-change: "approved"` on the MageStore — cross-topology transitions discard cache contents (Sentinel HA's data isn't migratable to a single-instance shape and vice versa), so admin acknowledgement is required. Refusal reports `CacheReady=False, Reason=TopologyChangeRequiresApproval` with a message naming the annotation. With the annotation set to `"approved"`, the operator clears the annotation, deletes the existing StatefulSet, sweeps Sentinel artifacts, and reports `Reason=TopologyTransitioning` until the next reconcile materialises the new topology fresh; the annotation Patch fires the controller's `For()` watch so the next reconcile runs immediately. Within-topology resize (e.g. `replicas: 3 → 5` in HA, `replicas: 1` unchanged) stays online — `topologyOf(replicas)` classifies into `single` (≤1) vs `ha` (≥3) and only topology jumps trigger the gate. Closes the v0.3.0 known-gap "Online resize between `replicas: 1` and `replicas: 3+` is best-effort, not orchestrated." Regression-tested by three new envtest cases (same-topology 3→5 resize allowed without annotation; cross-topology 1→3 refused without annotation; cross-topology 3→1 executed with annotation, including annotation-cleared and Sentinel-swept assertions). E2e is envtest-only — the destructive recreate adds little signal beyond envtest, and the kind-e2e budget is allocated to the Sentinel-aware bootstrap arc.

### Known gaps (v0.4.x / v1.0.0+)
- **TLS-to-clients deferred to v1.0.0+ pending user/developer demand.** The operator's deployment model is "everything in-cluster" (Magento, Valkey, MariaDB, OpenSearch, RabbitMQ all on the same pod network), so the threat model TLS-to-clients addresses (eavesdropping on cluster-internal traffic) is structurally moot for the target audience. Customers needing TLS-to-clients today have `redisRef` or `external` against a TLS-terminated front door. Will revisit if a paying client or clear public-issue volume forces a re-read. Tracking explicitly so the v0.4.0 acceptance bar isn't read as a regression — TLS-to-clients was a v0.3.0 carry-over deferred to v0.4.0; the v0.4.0 design call (2026-05-03) re-defers it to v1.0.0+.

## [v0.3.1] - 2026-05-03

### Added
- **Failure-path upgrade-arc e2e covered.** `test/e2e/upgrade_test.go` gains a second `Context` block exercising the broken-`setup:upgrade` arc end-to-end on a real kind cluster: patch `spec.image.tag` to a broken-image fixture (new `magento-test-image/Dockerfile.broken` ships a `bin/magento` PHP shim that exits 1 on `setup:upgrade` arguments), wait for `UpgradeReady=False, Reason=SetupUpgradeFailed`, revert the tag, **`Consistently`** assert the operator stays quiesced for 30 s (lockstep refusal per ADR 008), delete the failed Job, and assert the operator recovers to `UpgradeReady=True, Reason=UpToDate`. New `Makefile` target `build-broken-reference-image` builds + kind-loads the fixture; `make test-e2e` wires it as a prerequisite. Closes the v0.3.0 known-gap "Upgrade-arc failure-path e2e still not covered" carried over from v0.2.1.

### Fixed
- **`handleExistingUpgradeJob` now short-circuits stale Complete Jobs.** When `spec.image.tag` drifts past the Complete Job's `upgradeToAnnotation`, the operator runs the idempotent stage-1 advance (observedImageTag = upgradeTo), deletes the stale Job, and requeues — the next reconcile detects observed-vs-spec drift and enters a fresh arc via `runUpgradeArc`. Pre-fix the operator parked at `UpgradeReady=True, Reason=UpgradeComplete` for up to 1h until the stale Job's `TTLSecondsAfterFinished=3600` expired, blocking back-to-back upgrades. The maintenance fence (already-flipped Services + maintenance Deployment) is intentionally retained across arcs — `runUpgradeArc` re-enters idempotently and the visitor sees a continuous 503 across the chained arcs. Closes the v0.3.0 known-gap "Operator does not enter a fresh upgrade arc while a previous Complete Job is still within its TTL=3600s." Regression-tested by new envtest Case C7 (`internal/controller/ensure_upgrade_test.go`).
- **`failed-Job-deleted` recovery path now drops the maintenance fence symmetrically.** `ensureUpgrade`'s no-Job-no-drift branch now calls `restoreServicesFromMaintenance` + `deleteMaintenance` alongside `restoreFromQuiesce`, with the same first-run `banVarnish` gate as the JobComplete success path. Pre-fix the operator reported `UpgradeReady=True, Reason=UpToDate` while the workload Services stayed selector-flipped to the maintenance pod, leaving the storefront serving 503 indefinitely. Closes the v0.3.0 known-gap "Operator does not restore Service-selector flips on the `failed-Job-deleted` recovery path." Regression-tested by new envtest Case C8 (`internal/controller/ensure_upgrade_test.go`) plus the failure-path e2e's recovery `It` extended with Service-selector + maintenance-Deployment assertions. The failure-path e2e Context's workaround `It` (`kubectl delete jobs -l app.kubernetes.io/component=upgrade`) is also dropped — the JobComplete short-circuit makes it unnecessary.

## [v0.3.0] - 2026-04-30

### Breaking changes
- **`spec.cache.redisRef` semantics shift from Valkey CR to Service.** Pre-v0.3.0 manifests pointed `redisRef.name` at a `valkeyv1.Valkey` CR managed by hyperspike/valkey-operator. v0.3.0 makes the field operator-agnostic: it now points at any Service in the same namespace (front-door for whichever Redis/Valkey operator the admin runs, or a hand-rolled deployment), and the operator reads the password from a conventional `<service-name>` Secret (key `password`). Existing `redisRef` manifests against a hyperspike Valkey CR must migrate to either inline mode (`spec.cache.redis: { replicas: 3 }` for HA) or `external` against the cluster's front-door Service DNS.
- **Inline Valkey CR data is lost on upgrade.** v0.2.x stored cache (and any Magento sessions kept in Redis) on hyperspike/valkey-operator's PVCs. v0.3.0 drops the operator dep and provisions its own StatefulSet with new PVCs — the old data is not migrated. Cache regenerates automatically; sessions are lost. Shops with Redis-backed sessions should either drain users out-of-band before upgrading or accept a logout. Switch to `external` against a managed Redis before upgrading if session continuity matters.
- **`hyperspike/valkey-operator` is no longer a dependency.** Remove the install from any deployment scripts (`scripts/install-dep-operators.sh` no longer runs it). The chart's ClusterRole drops the `hyperspike.io/valkeys` rule. Clusters with `valkey-operator` installed for unrelated reasons can keep it; mage-operator no longer touches the CRDs.

### Added
- **Operator-managed cache StatefulSet (ADR 009).** `spec.cache.redis` now drives the operator directly: it provisions a Valkey StatefulSet (image `valkey/valkey:8-alpine`, AOF persistence on per-pod 1Gi PVC), a headless Service for pod DNS, an auto-generated password Secret, and a ConfigMap with `valkey.conf` + an ordinal-aware bootstrap script. No upstream Redis or Valkey operator is required. Magento connects via standard host:port — no CROSSSLOT errors during `setup:upgrade`'s cache-tag cleanup (Magento issue #39224 mitigated at the topology layer).
- **`spec.cache.redis.replicas` selects single-instance vs. Sentinel HA.** New CRD field with enum `[1, 3, 5, 7]` (odd values only — Sentinel needs an odd voter count). `replicas: 1` materialises a bare single-instance StatefulSet (default for dev). `replicas: 3 / 5 / 7` layers a Sentinel Deployment + ClusterIP Service on top, with ordinal-aware `replicaof` bootstrap and quorum hard-coded at `(N/2)+1`. Magento's `Cm_Cache_Backend_Redis` switches into Sentinel-discovery mode automatically when the operator emits `MAGENTO_REDIS_SENTINEL_MASTER`. Survives master-pod deletion: Sentinel promotes a replica within the configured `down-after-milliseconds` window and Magento's Sentinel-discovery client follows the new master without a config change.
- **`MAGENTO_REDIS_SENTINEL_MASTER` env var in the image contract.** Emitted only in HA mode; `env.php` reads it and switches `Cm_Cache_Backend_Redis`'s `backend_options` to Sentinel-discovery shape (single Sentinel ClusterIP service in `server`, `sentinel_master` set to the monitor name). Single-instance and external modes don't emit it; their `env.php` paths are unchanged.

### Fixed
- **Manager RBAC: missing `apps/statefulsets` create/list/watch/etc. permission.** The operator's new inline-cache code path manages a StatefulSet directly. The chart's ClusterRole was updated in the same change, but the kubebuilder marker on `magestore_controller.go` was not — `config/rbac/role.yaml` is generated from the markers, not from the chart, so kustomize-deployed operators (`make deploy`) lacked the permission. Surfaced during kind smoke-test as `statefulsets.apps is forbidden`. Mirrors the v0.2.0 Subsession-D RBAC bugs: kubebuilder markers + chart RBAC must update together.
- **Operator-owned config Secret now shrinks when fields are removed from spec.** `ensureSecrets` switched from `StringData` (which merges into `Data` and never removes keys) to `Data` (which replaces). Removing `spec.cache` or `spec.queue` after install now removes the corresponding `MAGENTO_REDIS_*` / `MAGENTO_RABBITMQ_*` keys from the Secret on the next reconcile, and workload pods restart with the now-correct env. Closes the v0.2.1 known-gap "Operator-owned Secret doesn't shrink when fields are removed from spec." Existing v0.2.x stores get one-time shrinkage on the next reconcile after upgrade — desired.
- **Operator now sweeps orphaned dependency CRs and resources when their parent spec is removed.** Each `ensureX` reconciler step checks `spec.X == nil` and lists owned CRs / cache resources by `app.kubernetes.io/instance` + `managed-by` labels, deleting them. Cache (StatefulSet, Sentinel Deployment, Services, ConfigMaps, Secret), database (MariaDB CR), search (OpenSearchCluster CR), queue (RabbitmqCluster CR) all participate. Ref-mode CRs are passed over (they don't carry the operator's labels). Closes the v0.2.1 known-gap "Operator does not sweep orphaned dependency CRs when their parent spec is removed."

### Changed
- **`fullCapabilities()` test helper drops `HasValkey`.** Moved alongside the `Capabilities` struct simplification in `internal/controller/capabilities.go`. envtest suite no longer registers `valkeyv1` or its CRDs.
- **Reference image `env.php` Sentinel-aware.** Detects `MAGENTO_REDIS_SENTINEL_MASTER` and reshapes the `default` / `page_cache` cache frontends into Sentinel-discovery mode. Standalone-mode behaviour is unchanged.
- **`docs/operations/scaling.md` cache section rewritten.** The Sentinel HA recipe now points at inline `spec.cache.redis.replicas: 3` instead of a hand-rolled Valkey CR. `redisRef` is reframed as the operator-agnostic Service pointer.

### Known gaps (v0.3.x)
- **TLS-to-clients is not supported in inline mode.** Use `redisRef` or `external` for clusters requiring TLS-to-clients. (Closed in v0.4.0 by deferral — see v0.4.0's Known gaps.)
- **Upgrade-arc failure-path e2e still not covered.** Carried over from v0.2.1 — separate session. (Closed in v0.3.1.)

## [v0.2.1] - 2026-04-30

### Fixed
- **Reference image: PHP `memory_limit` bumped to 2G.** Upstream `php:8.3-fpm-bookworm` defaults to 128M, which is below Magento's documented minimum (768M) and trips fatal errors on `bin/magento setup:upgrade`, consumer pods backed by Redis cache, and most CLI invocations. New `/usr/local/etc/php/conf.d/zz-magento-memory.ini` applies to both php-fpm (workload pods) and php-cli (consumer / cron / Job pods). Surfaced when wiring Redis cache exposed the existing memory ceiling.
- **Bootstrap script declares RabbitMQ queue topology.** `setup:install` previously ran without `--amqp-*` flags, so Magento never declared the queue topology on the broker. Consumer pods then crashed with `NOT_FOUND - no queue 'async.operations.all'` because Magento's consumer code calls `basic_consume` without first declaring. The bootstrap script now conditionally appends `--amqp-host / --amqp-port / --amqp-user / --amqp-password / --amqp-virtualhost` when `MAGENTO_RABBITMQ_HOST` is set; absent = no AMQP args, matching the no-queue path.
- **Consumer list aligned with Magento 2.4.8-p4.** `internal/magento/consumers.go` listed `inventoryQtyCounter` (not present in 2.4.8-p4 → consumer pod crashed with "Consumer 'inventoryQtyCounter' is not declared") and was missing `catalog_website_attribute_value_sync` and `media.gallery.renditions.update`. Verified against `bin/magento queue:consumers:list` against a freshly-installed reference image.
- **Manager RBAC: missing `pods/exec` create permission.** `ensureVarnish.banVarnish` uses `client-go/remotecommand` to exec `varnishadm ban` in Varnish pods at stage-2 finalise, but the operator ServiceAccount only had `pods` get/list/watch. Every ban attempt failed with `User "..." cannot create resource "pods/exec"`. Add via kubebuilder marker (regenerates `config/rbac/role.yaml`) and mirror in `charts/mage-operator/templates/clusterrole.yaml`. Surfaced during DX validation of the v0.2.0 ban path.
- **Varnish ban now fires once per arc, not on every reconcile.** The completed upgrade Job stays around for the lockstep-failure unstick path, so each reconcile that finds it re-entered stage-2 finalise and re-issued the ban. `restoreServicesFromMaintenance` now returns the count of Services it actually flipped back; `banVarnish` only runs when count > 0 — i.e. on the single reconcile that did the real selector restoration. Subsequent reconciles see the Services already restored (annotation absent) and skip the ban.

### Known gaps (v0.3.0)
- **valkey-operator runs Valkey in cluster mode regardless of `nodes: 1`.** Magento's `Cm_Cache_Backend_Redis` (and `Magento\Framework\Cache\Backend\Redis` which extends it) does multi-key `DEL` operations during cache-tag cleanup (`matchingTag` mode); in Redis cluster mode these fail with `CROSSSLOT Keys in request don't hash to the same slot`. Effect: `spec.cache.redis: {}` (the inline-Valkey path) breaks `setup:upgrade`'s cache cleanup and any cache-tag invalidation. Workarounds today: `spec.cache.external` to a non-clustered Redis, `spec.cache.redisRef` to a CR from a different operator (spotahome/redis-operator, OT-CONTAINER-KIT/redis-operator, or a bare Bitnami Helm chart in standalone mode), or omit `spec.cache` entirely (Magento falls back to file/db cache). Long-term fix: support a non-cluster Valkey path (upstream issue + operator config), or default to a standalone-capable Redis operator.
- **Operator-owned Secret doesn't shrink when fields are removed from spec.** Kubernetes' `Secret.StringData` merges into `Secret.Data` on update rather than replacing it; the operator's `ensureSecrets` only writes the keys it currently emits, so removing `spec.cache` or `spec.queue` after install leaves the now-stale `MAGENTO_REDIS_*` / `MAGENTO_RABBITMQ_*` keys in the Secret indefinitely. Workload pods continue picking them up via envFrom and behave as if those subsystems are still configured. Fix needs an explicit `Data = newMap` write or a Patch with the merge-strategy directive that supports key removal.
- **Operator does not sweep orphaned dependency CRs when their parent spec is removed.** Removing `spec.cache` from a MageStore today leaves the `<store>-valkey` Valkey CR untouched (still consuming a PVC + pod). Same risk for `spec.search` / `spec.queue` / `spec.database` when toggled inline → external/ref or unset. The reference goalert-provisioning operator's "orphan resource cleanup on spec changes" pattern is the model to follow.
- **Upgrade-arc failure-path e2e not yet covered.** The `broken-image upgrade → SetupUpgradeFailed` path is exercised manually only; no `test/e2e/upgrade_test.go` `It` block exists for it. To be added alongside CI integration (nightly + on-tag).

## [v0.2.0] - 2026-04-28

### Added
- **`spec.upgrade.maintenance` CRD surface (ADR 008, v0.2.0 Subsession A).** New optional fields land the upgrade-orchestration shape ahead of the reconciler that consumes it: `spec.upgrade.maintenance.enabled` (default `true`, `*bool` so explicit `false` survives roundtrip), `spec.upgrade.maintenance.htmlConfigMapRef.{name, key}` (custom 503 page override). New status field `status.observedImageTag` and new condition type `UpgradeReady` complete the surface. Surface-only release: `ensureUpgrade` lands in Subsession B; until then `spec.upgrade` is accepted by the API server but does not drive any reconciler behaviour.
- **`ensureUpgrade` reconciler step (ADR 008, v0.2.0 Subsession B).** Tag drift between `spec.image.tag` and `status.observedImageTag` now drives a `setup:upgrade` Job arc: suspend owned CronJobs, scale consumer Deployments to zero (with a per-Deployment annotation snapshotting the original replica count for restore), wait for cron pods to terminate (5min timeout → `Reason=CronQuiesceTimeout`), create a `<store>-upgrade-<from>-<to>` Job, and on `JobComplete` advance `observedImageTag` and restore quiesce state. Failed Jobs are unstuck by `kubectl delete job <name>`; the operator refuses to un-quiesce while a failed Job exists, even if `spec.image.tag` is reverted.
- **Maintenance fence + Service-selector flip (ADR 008, v0.2.0 Subsession C).** Closes the production-safety gap from Subsession B. On upgrade-arc entry, the operator now materialises a `<store>-maintenance` Deployment (one nginx replica serving a 503 page) and Service, then patches the `<store>-frontend` / `<store>-admin` / `<store>-api` Service selectors to the maintenance pod — public traffic sees a 503 for the duration of the arc instead of hitting workload pods talking to a migrating schema. The flip uses a `mage.aknostic.com/maintenance-flipped: "true"` annotation as the coordination channel between `ensureUpgrade` (writer) and `ensureWorkloadService` (skip-gate reader); without that annotation, every reconcile would revert the flip. The success path is now two-stage: stage 1 advances `observedImageTag` and restores cron + consumers immediately on Job complete; stage 2 (gated on workload pods reaching Available on the new image) restores Service selectors and deletes the maintenance resources. The window between "new pods Available" and "selectors restored" is the verification window — admins can `kubectl port-forward` the workload Deployments to validate the upgrade before public traffic resumes. Opt-out via `spec.upgrade.maintenance.enabled: false` skips the fence entirely (blue-green / canary path); custom 503 page via `spec.upgrade.maintenance.htmlConfigMapRef`. **Deferred to a v0.2.x patch:** the post-arc Varnish ban (`varnishadm ban "obj.http.X-Magento-Tags ~ .*"`) is not yet wired — the `*rest.Config` injection + client-go remotecommand plumbing is non-trivial. Stale Varnish entries serve old HTML for at most a TTL window after upgrade; not a correctness gap.
- **Upgrade-arc kind-e2e (ADR 008 Validation Layer 2, v0.2.0 Subsession D).** New `test/e2e/upgrade_test.go` exercises the orchestrator surface end-to-end on a real kind cluster: applies a `MageStore` at the reference image's `2.4.8` tag, populates the database via `setup:perf:generate-fixtures --profile=small`, retags the image as `2.4.8-rc2` (same bytes — the operator compares tags not digests, so this triggers the orchestrator without requiring a second Magento-version reference image), patches `spec.image.tag`, and asserts the arc walks through `UpgradeStarting → WorkloadRolloutPending → UpgradeComplete → UpToDate`. Both halves of the verification-window contract are regression-tested: port-forward to `service/<store>-frontend` returns 503 during the arc (Service-selector flip in effect) while port-forward to `deployment/<store>-frontend` returns a non-503 status (kubelet-direct path bypasses the fence). New helpers in `test/utils/utils.go`: `ApplyManifest`, `PatchMageStore`, `GetMageStoreCondition`, `GetMageStoreFieldJSONPath`, `ExecInPod`, `RetagImage`, `CurlURL`, `PortForward`, `DeleteResource`. `make setup-test-e2e` now installs the full dep-operator stack (cert-manager, mariadb-operator, opensearch-operator, valkey-operator, rabbitmq-cluster-operator, Gateway API CRDs, Envoy Gateway) via the new `scripts/install-dep-operators.sh`, which `scripts/dev-k8s-setup.sh` also delegates to so the dev and e2e flows share version pins. New `local/kind-config-e2e.yaml` ships a 2-node kind topology — single-node kind starves the apiserver mid-OpenSearch-bootstrap once Magento + workload pods schedule alongside. **Out of scope for this release:** the failure-path test (broken-image upgrade → `SetupUpgradeFailed`) lands in a v0.2.x patch alongside CI integration (nightly + on-tag); local-only e2e via `make test-e2e` is sufficient for v0.2.0 tag. Multi-version Magento schema migration is the ADR-008 Layer 3 manual `--profile=medium` sweep documented in `docs/internals/releasing.md`.
- **Cosign signing for the Helm chart.** The release pipeline now signs the OCI Helm chart on tag pipelines using the same cosign key the operator image is signed with. Verifiers run `cosign verify --key cosign.pub registry.gitlab.aknostic.com/aknostic/mage-operator/charts/mage-operator:X.Y.Z`. v0.2.0 is the first release with a signed chart; v0.1.x charts are unsigned. See `docs/internals/releasing.md`.

### Documentation
- **README + quickstart rewritten for the consumer-image-first path.** README's Quick install no longer steers readers through a 25-minute reference-image build before showing how to install. The canonical path is now `helm install` from the public OCI chart + `kubectl apply` of a minimal `MageStore` pointing at the user's own Magento image; the reference-image build moves to a "Don't have an image yet?" tail section in both README and quickstart.
- **New `docs/minimal-magestore.md`** — smallest viable `MageStore` examples for the two common starting cases (fresh install with operator-provisioned dependencies, pre-bootstrapped database with `spec.database.external`).
- **New `docs/operations/scaling.md`** — opinionated production / HA guide. Three sections: stateless-tier scaling (frontend / admin / api / Varnish replicas + autoscaling), stateful-tier HA recipes (ref-mode end-to-end for MariaDB Galera, Valkey replicas, OpenSearch already-HA-by-default, RabbitMQ multi-node) plus S3-not-RWX media guidance, and known v0.3.0-roadmap limitations (consumer replicas hardcoded at 1, no PodDisruptionBudget surface, no affinity / topologySpreadConstraints surface, single-replica operator without leader election).
- **New `docs/operations/upgrades.md`** — skeleton for the upgrade-orchestration user guide; populated content lands incrementally as v0.2.0 implementation completes.
- **`docs/magestore-reference.md` polished.** Internal "Subsession" jargon removed from the `UpgradeReady` row; stale "Phase B docs ship" forward-reference replaced with a direct link to `troubleshooting.md`.

### Fixed
- **Reference image: Redis cache and RabbitMQ AMQP now wired in env.php.** `magento-test-image/env.php` now conditionally emits a `cache.frontend.default` + `cache.frontend.page_cache` Redis backend block when `MAGENTO_REDIS_HOST` is set, and a `queue.amqp` block when `MAGENTO_RABBITMQ_HOST` is set. Both use the existing `getenv()` pattern; absent env vars fall back to Magento's defaults (db cache / synchronous queue). Found during ADR-008 Layer 3 medium sweep.
- **Operator: `config-secret-hash` annotation added to workload Deployment pod templates.** `ensureSecrets` now returns a SHA-256 of the rendered Secret's `Data`; the hash flows through `ensureWorkloads` into a `mage.aknostic.com/config-secret-hash` pod-template annotation. Adding `spec.queue.rabbitMq`, changing `spec.database`, or rotating credentials after initial install now triggers an automatic rolling restart without a manual pod delete. Mirrors the existing `nginx-config-hash` pattern.
- **Post-arc Varnish ban wired.** `banVarnish` execs `varnishadm ban "obj.http.X-Magento-Tags ~ .*"` in every running Varnish pod at stage-2 finalise (after Service selectors are restored). Best-effort: a failed ban is logged but does not block upgrade completion; stale entries expire within the Varnish TTL regardless. `RestConfig *rest.Config` and `KubeClient kubernetes.Interface` added to `MageStoreReconciler`; both nil-safe for envtest/unit-test contexts.

### Fixed
- **Chart CRD re-synced from `config/crd/bases/`.** v0.1.1 introduced `spec.search.external` in `config/crd/bases/mage.aknostic.com_magestores.yaml` but the chart copy at `charts/mage-operator/crds/mage.aknostic.com_magestores.yaml` was not refreshed; v0.1.1 and v0.1.2 shipped with chart-installed clusters silently dropping `spec.search.external`. v0.2.0 chart installs accept the field as documented.
- **Manager RBAC: missing `apiextensions.k8s.io/customresourcedefinitions` get/list permission.** The capability-detection code added in v0.1.1 (`internal/controller/capabilities.go`'s `ProbeCapabilities`) reads CRDs at cluster scope to detect which dependency operators are installed, but the kubebuilder RBAC marker for that read was never added. v0.1.x operator pods deployed in a cluster with strict RBAC enforcement crashed at startup with `customresourcedefinitions ... is forbidden: User "system:serviceaccount:..." cannot get resource "customresourcedefinitions"`; the bug was masked in unit tests (envtest's RBAC enforcement doesn't gate the apiextensions client) and in dev clusters where the operator's SA was sometimes bound to a wider role. Surfaced during v0.2.0 Subsession D's first cold `make test-e2e` run. Fix: add `// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list` to `magestore_controller.go` (regenerates `config/rbac/role.yaml`) and the matching block to `charts/mage-operator/templates/clusterrole.yaml`.

## [v0.1.2] - 2026-04-26

### Added
- Consumer Deployments now mount the same media volume the workload Deployments use, at `/app/pub/media`. The image-resize consumer needs this for its source-image staging in s3 mode (the remote-storage driver uses local pub/media as a scratch staging area before uploading).
- ADR 008 — Upgrade orchestration design (`docs/internals/adr/008-upgrade-orchestration.md`). Captures the v0.2.0 implementation contract for image-tag-driven Magento upgrades; no runtime change in v0.1.2 (implementation lands in v0.2.0).

### Changed
- **Reference image (`magento-test-image/env.php`) now wires `MAGENTO_MEDIA_STORAGE_*` into Magento's `remote_storage` block via `getenv()` directly, not via `bin/magento setup:config:set --remote-storage-*` on entrypoint.** The previous image-contract guidance was incorrect for an image whose `env.php` uses `getenv()` for every other config: `setup:config:set` parses `env.php`, mutates the array, and writes it back, which would freeze every `getenv()` call into a static value at write-time. The env.php-direct pattern keeps the dynamic-config approach uniform across all env vars. `docs/image-contract.md` updated to reflect this; the canonical `env.php` example in the docs gains the `remote_storage` block.

## [v0.1.1] - 2026-04-25

### Added
- **Conditional watching of dependency CRDs.** The operator probes `apiextensions/v1` at startup for each dependency CRD (mariadb / opensearch / valkey / rabbitmq / gateway / httproute) and degrades gracefully when a soft dep is absent: scheme registration and `Watches()` are skipped, and MageStores using the affected inline / ref modes surface `*Ready=False, Reason=DependencyOperatorMissing`. External-mode siblings work unaffected. Gateway API CRDs (Gateway, HTTPRoute) remain hard requirements — missing them fails fast at startup.
- `spec.search.external` — the third sibling closing the search tier's ADR-004 union (host + optional credentialsSecretRef). Auth-less external endpoints supported. Emits `MAGENTO_OPENSEARCH_USERNAME` / `MAGENTO_OPENSEARCH_PASSWORD` env vars when credentials are set.
- `docs/troubleshooting.md` — condition-by-condition Reason reference for every `*Ready=False` value the operator can surface.
- Top-level `LICENSE` file (Apache 2.0) — was referenced in source headers and the README but never lived as a standalone file.
- `docs/quickstart.md` — consumer-first 10-minute install walkthrough.
- `docs/magestore-reference.md` — field-by-field reference for the `MageStore` CRD.
- `docs/prerequisites.md` — cluster-admin onboarding doc covering Gateway API + Envoy Gateway as the only hard requirement, the four data-tier operators as opt-out-able via `external` siblings, pinned versions, and the path for substituting alternative operators (percona, etc.). Linked from the README.
- `scripts/dev-k8s-setup.sh` now installs Gateway API CRDs (`v1.2.1`) and Envoy Gateway (`v1.3.0`). The Slot 22.5 pivot to Gateway API HTTPRoute had been done manually; fresh checkouts can now run `kubectl apply -k config/samples/` and reach `Ready=True` without out-of-band installs.

### Changed
- `README.md` rewritten consumer-first: opens with what the operator does for a Magento dev, drops the build-from-source quickstart in favor of `helm install` against the published OCI chart, points at `docs/quickstart.md` for the full walkthrough.
- Documentation tree split between consumer-facing material (`docs/`) and contributor material (`docs/internals/`):
  - `docs/secrets.md` → `docs/internals/secrets.md`
  - `docs/local-smtp.md` → `docs/internals/local-smtp.md`
  - `docs/releasing.md` → `docs/internals/releasing.md`
  - `docs/adr/` → `docs/internals/adr/`
  - `mage-operator-plan.md` → `docs/internals/plan.md`
- All in-repo cross-references (CLAUDE.md, ADRs, sample comments, source comments, `.sops.yaml`) updated to the new paths.
- `docs/magestore-reference.md` `spec.database.external` callout: explicit that `external` mode works for any MySQL-protocol backend, including ones fronted by other MySQL operators (percona, vitess, planetscale).
- `TIMING.md` v0.1.x / v0.2.0 punch list adds: conditional watching of dependency CRDs (graceful degradation when a dep operator isn't installed), `spec.search.external` to fill the Phase-2 gap, `spec.database.perconaRef` + `Percona` siblings as the representative "another MySQL operator" first-class integration.

## [v0.1.0] - 2026-04-25

The first cuttable release: a single `kubectl apply -k config/samples/`
brings up a complete Magento store on a kind cluster — database,
search, cache, queue, three-Deployment workload split, consumers,
cron groups, Gateway API routing, optional Varnish caching, S3 or
PVC media. Demo-runnable; runtime-validation on real infra (EKS,
Kapsule) tracked in parallel.

### Added

#### CRD surface (`mage.aknostic.com/v1alpha1`)
- `MageStore` with `spec.{image, workloads, database, cache, search, queue, consumers, cron, media, varnish, secrets, stores, bootstrap}`.
- Three-Deployment workload split (`spec.workloads.{frontend, admin, api}`) — each workload is a separate Deployment with its own resource profile (ADR 006).
- Inline / external / Ref discriminated unions for stateful deps: `spec.database.{mariaDb, mariaDbRef, perconaRef, external}`, `spec.cache.{redis, redisRef, external}`, `spec.search.{openSearch, openSearchRef, external}`, `spec.queue.{rabbitMq, rabbitMqRef, external}` (ADR 004).
- `spec.stores[]` for multi-store host → MAGE_RUN_CODE mapping (ADR 007).
- `spec.varnish` with optional user-supplied VCL via `vclConfigMapRef`; default VCL generated by the operator when unset (ADR 007 multi-store hash key embedded automatically).
- `spec.consumers.enabled` (and per-consumer overrides) for Magento queue consumer fan-out — 20-name canonical list for Magento 2.4.8.
- `spec.cron.enabled` (and per-group overrides) for the five canonical Magento cron groups.
- `spec.media.{s3, pvc}` — S3 (default) or PVC-backed media storage.

#### Reconcilers (`internal/controller/`)
- `ensureDatabase`, `ensureCache`, `ensureSearch`, `ensureQueue` — composed external operator CRs (mariadb-operator, valkey-operator, opensearch-k8s-operator, rabbitmq-cluster-operator).
- `ensureSecrets` — envvars-mode config Secret per ADR 002 (ECE-Tools mode dropped from Phase 1; revisit on client demand).
- `ensureBootstrap` — one-shot `bin/magento setup:install` Job per ADR 005, with admin password Secret generation.
- `ensureWorkloads` — three-Deployment fan-out + per-workload Service.
- `ensureNginxConfig` — sidecar nginx ConfigMap with `map $http_host $MAGE_RUN_CODE` blocks for multi-store dispatch.
- `ensureRouting` — Gateway API Gateway + HTTPRoute (ADR 006 amendment) with path-based fallthrough (`/admin → admin`, `/rest /graphql → api`, `/ → frontend or varnish`).
- `ensureVarnish` — optional Varnish layer in front of `/`, with operator-generated default VCL when no `vclConfigMapRef` supplied; multi-store hash key embedded per ADR 007.
- `ensureConsumers` — per-consumer Deployment with `replicas: 1`, `terminationGracePeriodSeconds: 60` (Magento consumers do not handle SIGTERM cleanly).
- `ensureCron` — per-group CronJob with cautious memory profiles; `index` group sized for long reindex runs.
- `ensureMedia` — endpoint-returning step exposing volume + env vars consumed by `ensureSecrets` and `ensureWorkloads`.
- Reactive `Watches` on referenced external CRs so dependency state propagates within seconds.

#### Distribution
- Helm chart at `charts/mage-operator/` — operator Deployment, ServiceAccount (toggleable), ClusterRole + ClusterRoleBinding for reconcile permissions, conditional leader-election Role + RoleBinding, conditional metrics Service, MageStore CRD under `crds/`.
- GitLab CI pipeline (`.gitlab-ci.yml`) — trufflehog + govulncheck + semgrep SAST, golangci-lint (10m timeout), helm-lint, kube-linter, hadolint, envtest, kaniko image build, trivy container scan, syft SBOM, cosign signing, crane-based image retag for semver tags, Helm OCI chart push, GitLab release-cli release object.
- `docs/releasing.md` — the release flow: chart bump, semver tag, what CI does, image signing, rollback strategy.

#### Documentation
- ADRs 002-007 — config emission (002), crypt-key format (003), seeded-but-extractable dependencies (004), bootstrap as Job (005), workload naming + Gateway API routing (006), multi-store MAGE_RUN_CODE mapping (007).
- `docs/image-contract.md` — env var namespace the Magento image consumes, sidecar mount expectations.
- `docs/secrets.md`, `docs/local-smtp.md` — SOPS encryption for demo secrets, local SMTP for the bootstrap admin email.

### Notes
- The CRD ships only `magestores`; `MageIndex`, `MageBackup`, `MageRestore` (per the plan) graduate post-v0.1.0.
- Ingress controller choice is orthogonal to the operator (ADR 006 amendment); v0.1.0 hard-codes the `eg` GatewayClass with a Phase-2 `spec.routing.gatewayClassName` field documented as the escape hatch.
- The reference image lives in-tree at `magento-test-image/`; production users replace it with their own image honoring `docs/image-contract.md`.

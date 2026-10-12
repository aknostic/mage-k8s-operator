# ADR 011 — Image-baked media seeding

- **Status:** Accepted
- **Date:** 2026-05-04
- **Deciders:** @jurg
- **Related:** closes gap #8 of the v0.6.0 real-infra gap list; extends
  the `MediaSpec` shape introduced alongside `ensure_media.go`;
  orthogonal to ADR 010 (public-edge TLS) and the in-cluster
  TLS-deferral stance from CLAUDE.md.

## Context

Sample-data deployments ship product images **inside the workload OCI
image** at `/app/pub/media/...`. The reference image's
`magento-test-image/Dockerfile:50-54` runs
`bin/magento sampledata:deploy` when `WITH_SAMPLEDATA=true`, which
writes ~2 GB of Luma catalog imagery into the builder stage's
`pub/media/`. The runtime stage carries those files forward via
`COPY --from=builder /app /app`.

At runtime, the workload pod mounts a volume — `emptyDir`
(unset / S3 modes), or the operator-provisioned RWO PVC — at
`/app/pub/media`. The volume mount **overlays and hides** the image's
baked content. Storefront pods come up with empty `pub/media/` and
serve broken-image icons across the entire catalog.

This is the regression the v0.5.0 Kapsule demo surfaced (memory
`v0_6_0_real_infra_gaps`, 2026-05-04). Three deployment modes need a
coherent answer:

- **emptyDir mode** (the demo path): per-pod scratch volume. Without
  intervention, every pod starts with an empty `pub/media/`.
- **PVC mode** (dev/test single-pod, RWO per CLAUDE.md "S3 for media,
  not RWX PVC"): persistent volume that's empty on first bind.
- **S3 mode** (the production multi-pod path, first-class in Magento
  via the built-in `Magento_RemoteStorage` module since 2.4.0): bucket
  is the canonical store; `pub/media` is a local scratch dir for
  flysystem's image-processing pipeline. A fresh bucket is empty.

The v0.6.0 acceptance bar is "fresh `helm install + apply MageStore`
against an empty cluster reaches a working Luma storefront with
images" — closing this gap is necessary for that bar.

We considered three options when scoping the gap (per the v0.6.0 plan
doc lines 162-170):

- **A — initContainer-to-seed unconditionally** when an emptyDir
  mounts at `/app/pub/media`. No CRD change. Magic; no opt-out for
  customer images that don't carry the baked media.
- **B — `spec.media.seedFromImage: true` opt-in**, same mechanism as
  A but explicit. Default off; demo manifest sets it on. Less magic,
  more discoverable.
- **C — document S3 + populate** as the only supported sample-data
  demo path. Customer stands up Minio/Scaleway, runs offline `mc cp`,
  then deploys. Multi-step prerequisite for a "fresh demo" feels
  off-piste; punts the broken default.

We chose **B** with an explicit decision to support all three media
modes (emptyDir + PVC + S3), not just the emptyDir demo path.
Multi-pod Magento ⇒ S3 (per CLAUDE.md and operator-wide stance), and
seed-from-image is exactly the bootstrap a freshly-provisioned bucket
needs to look right out of the gate. Restricting `seedFromImage` to
volume modes only would force production-leaning users to populate
their bucket out-of-band — exactly the friction that motivates an
operator in the first place.

## Decision

Add `spec.media.seedFromImage: bool` as an opt-in flag, paired with a
restructured `MediaSpec` that makes the existing pvc/s3/unset
trichotomy a proper three-way discriminated union. The seeding
mechanism is mode-aware: `cp -rn` initContainer for volume modes
(emptyDir + PVC), one-shot Job running Magento's native
`bin/magento remote-storage:sync` for S3.

### CRD shape

```go
type MediaSpec struct {
    // Exactly one of EmptyDir, Pvc, S3 must be set when MediaSpec is present.
    EmptyDir *MediaEmptyDir `json:"emptyDir,omitempty"`
    Pvc      *MediaPvc      `json:"pvc,omitempty"`
    S3       *MediaS3       `json:"s3,omitempty"`

    // SeedFromImage populates the configured backend with the workload
    // image's /app/pub/media-baked/* on initial install. Image must
    // satisfy the docs/image-contract.md "media-baked path" contract.
    // +optional
    SeedFromImage bool `json:"seedFromImage,omitempty"`
}

// MediaEmptyDir is a marker; per-pod scratch volume. Reserved for
// future tuning fields (sizeLimit, medium).
type MediaEmptyDir struct{}
```

CEL on `MediaSpec` (only fires when the block is present; nil-block
stays valid via the parent `+optional`):

```
(self.has('emptyDir') ? 1 : 0) +
(self.has('pvc')      ? 1 : 0) +
(self.has('s3')       ? 1 : 0) == 1
```

`ensureMedia` keeps a defensive fall-through to emptyDir if all three
mode branches miss (mirroring the existing
`ensure_media.go:142-150` "CRD-level XValidation blocks this; keep
the branch so we degrade rather than nil-pointer-dereference if CEL
is ever bypassed" pattern). The fall-through is unreachable under a
correctly-validated CRD; it exists purely as defence against schema
drift.

Back-compat: `spec.media: nil` keeps falling back to emptyDir scratch
per existing `ensure_media.go:92-101`. Existing CRs that omit the
block are unaffected.

What changes from existing valid shapes:

- `spec.media: {}` (empty block) → rejected (discriminator-count == 0).
- `spec.media: { seedFromImage: true }` (flag without mode) → rejected.
- `spec.media: { emptyDir: {}, pvc: {...} }` (two modes) → rejected.

New canonical shapes:

- `spec.media: { emptyDir: {}, seedFromImage: true }` — the demo path.
- `spec.media: { pvc: {...}, seedFromImage: true }` — dev/test seed.
- `spec.media: { s3: {...}, seedFromImage: true }` — bucket seed.

### Image-contract delta — `/app/pub/media-baked/`

The workload image must expose baked sample-data at
`/app/pub/media-baked/` — a *parallel* path to the runtime mount
point, deliberately outside `/app/pub/media` so the volume mount
cannot hide it.

Reference image (`magento-test-image/Dockerfile`): the runtime stage
moves rather than duplicates. Insert immediately after
`COPY --from=builder /app /app` (currently line 125), and **before**
the multi-target `mkdir -p ... /app/pub/media ...` at line 152 — that
mkdir bundles `/app/pub/media` with `var/cache`, `var/page_cache`,
`var/session`, `var/log`, `var/report`, `generated/code`,
`generated/metadata`. Leave the bundled mkdir intact; only the `mv`
step is new:

```dockerfile
# After: COPY --from=builder /app /app   (line 125)
RUN mv /app/pub/media /app/pub/media-baked
# Before: RUN mkdir -p /app/var/cache ... /app/pub/media   (line 152)
```

The existing line-152 `mkdir -p` re-creates `/app/pub/media` as the
empty mount point. No disk-space duplication; the baked tree lives
only at `media-baked`.
Customer images forking the reference must carry the same directory
when they intend to set `seedFromImage: true`. Customers running
emptyDir/PVC without sample-data, or populating S3 buckets
out-of-band, leave `seedFromImage` unset and need no image-contract
change.

Missing the directory while `seedFromImage: true` is a hard-fail (see
"Error handling" below). Silent fallback was rejected: a feature
named "populate the storefront with images" should not no-op when
the source isn't there.

### Mechanism — volume modes (emptyDir + PVC)

`ensureWorkloads` adds an initContainer to the pod template when
`spec.media.seedFromImage: true && spec.media.s3 == nil`:

```yaml
initContainers:
  - name: media-seed
    image: <same image as workload container>
    command: ["sh", "-c"]
    args: ["cp -rn /app/pub/media-baked/. /app/pub/media/"]
    resources:
      requests: { cpu: 50m, memory: 64Mi }
    volumeMounts:
      - name: media
        mountPath: /app/pub/media
```

The initContainer runs on **all three** workload deployments
(frontend / admin / api). Same image, same volume mount, same command.
Per-workload optimization (e.g., skip api) was rejected: the
consistency tax is small relative to the brittleness of mode-aware
exclusions, and api can still receive REST-driven media writes that
need a populated baseline.

`cp -n` (no-clobber) is the idempotency gate. emptyDir: every pod
start sees a fresh empty volume → copies everything (~30-60s for 2
GB sample data). PVC: first pod mounts an empty volume → copies
everything; subsequent pod replacements on the same RWO PVC find the
populated volume → `cp -n` skips every file in ~1s.

### Mechanism — S3 mode

Operator emits a one-shot Job named `<store>-media-seed`:

```yaml
apiVersion: batch/v1
kind: Job
spec:
  backoffLimit: 2
  activeDeadlineSeconds: 1800
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: seed
          image: <same image as workload>
          envFrom:
            - secretRef: { name: <store>-config }   # full backend stack
          volumeMounts:
            - name: media
              mountPath: /app/pub/media
          command: ["sh", "-c"]
          args:
            - |
              set -euo pipefail
              cp -rn /app/pub/media-baked/. /app/pub/media/
              bin/magento remote-storage:sync
      volumes:
        - name: media
          emptyDir: {}
```

The `cp` step populates the Job's local `pub/media` (the Job's
emptyDir is its own; not shared with workload pods). The
`remote-storage:sync` step then reads from local `pub/media` and
pushes via the env-configured `aws-s3` flysystem driver to the
bucket. `app/etc/env.php:13-32` already wires the
`MAGENTO_MEDIA_STORAGE_*` env vars into Magento's canonical
`remote_storage` driver block — no env.php change needed.

Why `bin/magento remote-storage:sync` over `s5cmd` or `aws-cli`:

- Magento ships `remote-storage:sync` natively since 2.4.0 in the
  built-in `Magento_RemoteStorage` module.
- No image-contract addition for tooling. `s5cmd` would have required
  a new RUN line in every customer image (negligible — ~30 MB static
  binary — but a precedent we don't need to set).
- Content-aware idempotency falls out of Magento's flysystem layer.
  Re-runs after partial failure resume cheaply (compare-then-write per
  object).
- Consistent with the rest of the operator's stance (CLAUDE.md
  "image-contract over operator-side tooling"): everything
  operator-emitted runs the customer's workload image; we have zero
  third-party operator-pinned images today, and adding one for
  bulk-S3-upload is high precedent cost for one feature.

Trade-off: `remote-storage:sync` is single-threaded PHP and slower
than `s5cmd`'s parallel Go uploads (~5-10×). For a one-shot 2 GB
upload at install time, this isn't on any latency budget worth
optimising.

### Reconcile graph and gating

New step `ensureMediaSeed` between `ensureBootstrap` and
`ensureWorkloads`:

```
ensureDatabase    → DatabaseReady
ensureSearch      → SearchReady
ensureCache       → CacheReady
ensureQueue       → QueueReady
ensureMedia       → MediaEndpoint (Volume + EnvVars + SeedFromImage)
ensureSecrets     → config Secret
ensureBootstrap   → BootstrapReady
ensureMediaSeed   → MediaSeeded (new)
ensureWorkloads   → WorkloadsReady (gates on MediaSeeded for S3)
ensureVarnish     → VarnishReady
ensureConsumers   → ConsumersReady
ensureCron        → CronReady
```

`MediaSeeded` is a new top-level condition on `MageStore.status`,
single-producer (`ensureMediaSeed`):

| Mode + flag | Condition behaviour |
|---|---|
| `seedFromImage: false` (any mode) | condition omitted from `status.conditions` |
| emptyDir/PVC + `seedFromImage: true`, `WorkloadsReady=False` | `MediaSeeded=Unknown` |
| emptyDir/PVC + `seedFromImage: true`, `WorkloadsReady=True` | `MediaSeeded=True` (init-container success implied by pod-Ready) |
| S3 + `seedFromImage: true`, no Job yet | `MediaSeeded=Unknown, Reason=Seeding`; create Job |
| S3 + `seedFromImage: true`, Job Active | `MediaSeeded=Unknown, Reason=Seeding` |
| S3 + `seedFromImage: true`, Job Succeeded | `MediaSeeded=True, Reason=SeedComplete` |
| S3 + `seedFromImage: true`, Job Failed (backoff exhausted) | `MediaSeeded=False, Reason=SeedFailed` |

`MediaSeeded=True` is **sticky** — once the operator sets it True, it
stays True for the in-CR lifecycle. Subsequent reconciles do not
re-evaluate Job state; `ensureMediaSeed` early-returns. If the user
manually deletes the seed Job after success (e.g., during a re-seed
attempt), the operator does **not** detect the deletion and does not
re-create the Job. Re-seed requires the manual procedure documented
under "Future work" below.

Workload gating (`ensureWorkloads`):

- S3 + `seedFromImage: true` + `MediaSeeded=True` → emit Deployments.
- S3 + `seedFromImage: true` + `MediaSeeded` not True → skip
  Deployments; surface `WorkloadsReady=False, Reason=AwaitingMediaSeed`.
- emptyDir/PVC + `seedFromImage: true` → emit Deployments
  unconditionally (initContainer is the gate, not the condition).
- `seedFromImage: false` → emit Deployments unconditionally.

Synchronous gating in S3 mode means "MageStore Ready=True implies
storefront has Luma images" — consistent with volume modes where
pod-Ready already implies init-container success. Cost: ~1-3 min
serial dependency on first install. Acceptable for an install-time
event; surfaced clearly through the condition message.

### Steady-state writes (post-seeding) are workload-pod direct

The seed Job populates the bucket once at install. After
`MediaSeeded=True`, every subsequent media write — admin product
upload, REST API `POST /V1/products/.../media`, image-resize cache,
customer wishlist photo — is workload-pod-direct via Magento's
flysystem. No operator code path involved. The condition gate has
zero effect on steady-state I/O.

This is the architectural payoff of building on Magento's first-class
`remote_storage` support: ongoing operations don't need
operator-side machinery.

## Consequences

### Positive

- **Fresh-install demo path works** without `kubectl exec` or
  out-of-band media population (the v0.6.0 acceptance bar).
- **Three modes covered consistently.** emptyDir for demo, PVC for
  dev/test, S3 for production — `seedFromImage` works the same way
  to the user across all three.
- **No new operator dependencies.** Seed Job uses the workload image
  the operator already pulls; no `s5cmd` / `aws-cli` / sidecar image
  to mirror, pin, or CVE-track.
- **First-class Magento.** Built on `Magento_RemoteStorage` and
  `bin/magento remote-storage:sync` — toolchain Magento ships, tests,
  and documents.
- **Idempotent everywhere.** `cp -n` for volume modes;
  content-aware `remote-storage:sync` for S3.
- **Clean error surface.** Image-contract violations surface loudly
  through `WorkloadsReady` (volume modes) or `MediaSeeded` (S3) with
  a clear reason.

### Negative

- **Image-contract change cascades.** Customer images forking the
  reference must add the `media-baked` move-step. Documented in
  `docs/image-contract.md`; no auto-migration.
- **No auto re-seed on image rebuild.** A workload-image bump that
  ships new sample-data won't auto-propagate (`MediaSeeded=True` and
  `cp -n` both block). Manual procedure documented; an
  image-digest-keyed re-run hook is future work (see "Future work"
  below).
- **First-install wall time grows by ~1-3 min in S3 mode.** Seed
  Job sits on the critical path before workloads come up. Acceptable
  for a once-per-store event.
- **Per-pod re-seed cost in emptyDir mode.** Every pod start /
  scale-up runs `cp -rn ~2 GB` for ~30-60s. Pod startup latency
  budget on rollouts grows correspondingly. Steady-state is
  unaffected.

### Neutral

- **`MediaSeeded` is a new top-level status condition.** The
  `MageStore` ready-condition list grows by one. CHANGELOG entry on
  release.
- **Seed Jobs accumulate.** No TTL; same convention as bootstrap.
  `kubectl get jobs -n <ns>` shows both a `<store>-bootstrap` and
  (for S3 + seedFromImage) a `<store>-media-seed`. Cleanup is
  garbage-collected with the MageStore via ownerReference.

## Alternatives considered

- **InitContainer for all modes (Option A pre-design).** Would have
  pushed `aws s3 sync` into a workload-pod initContainer for S3
  mode. Rejected: ~2 GB scan + checksum on every pod start, every
  scale-up, every rollout — wasteful on a once-per-store concern.
  Also pulled `s5cmd`/`aws-cli` into the workload image (rejected
  separately).
- **Document S3 + populate (Option C pre-design).** Customer
  populates bucket out-of-band before applying MageStore. Punts the
  broken-default UX; "fresh demo" stops being one-step.
- **`spec.media.seedFromImage` per-mode (`spec.media.s3.seedFromImage`,
  …).** Disperses the flag across three sub-structs. Forces users
  to know which mode they're in to find the flag. Rejected for
  weaker discoverability.
- **`spec.seedFromImage` as a sibling of `spec.media`.** Decouples
  seeding-policy from storage-policy structurally. Rejected:
  conceptually weird because seeding is about populating media —
  naturally a media concern.
- **`spec.media: { seedFromImage: true }` valid as "emptyDir + seed"
  shorthand.** Considered for demo ergonomics. Rejected (Q4 → B):
  the discriminator should be explicit; `spec.media: { emptyDir: {},
  seedFromImage: true }` is two more characters and removes
  ambiguity from CRD validation.
- **Single mechanism via bootstrap Job for all modes.** Add the
  seeding step inline to `ensureBootstrap`. Works for PVC + S3 but
  fails for emptyDir (bootstrap Job's emptyDir is its own; can't
  populate workload pods' emptyDirs that don't exist yet). Two
  mechanisms is unavoidable; `ensureMediaSeed` keeps the concerns
  separate.
- **`s5cmd` baked into workload image (B from Q3).** Faster (~5-10×)
  than `remote-storage:sync` for bulk upload. Rejected: image-contract
  addition for a one-shot install-time concern; bypasses Magento's
  filesystem abstraction (forward compatibility risk if Magento
  changes remote-storage semantics).
- **Operator-pinned `peakcom/s5cmd` sidecar (A from Q3).** Two-
  container Job with operator-controlled tooling image. Rejected:
  introduces a new operator dependency on a third-party image
  (digest pinning, registry mirroring, CVE tracking) that the
  operator doesn't have today.
- **Inline in bootstrap Job (C from Q3).** Add `if [ "$SEED" = true
  ]` step to bootstrap. Rejected: collapses bootstrap-failure modes
  and seed-failure modes into one status; large upload sits on the
  bootstrap critical path.
- **Marker object in bucket (C from Q5).** Operator HEADs
  `s3://bucket/.mage-seeded` to gate the Job. Pulls AWS SDK into
  the controller binary; survives MageStore delete-and-recreate
  against the same bucket. Rejected: heavier, and the
  status-condition path already gives idempotency for the in-CR
  lifecycle. Re-creating against an existing bucket is a manual-
  reseed scenario covered separately.
- **Image-digest-keyed annotation (D from Q5).** Auto re-seed when
  the workload image digest changes. Correct behaviour for "we
  rebuilt sample data, push the new images" but introduces digest
  tracking the operator doesn't currently do, and re-uploading 2 GB
  on every image bump is heavy by default. Deferred.
- **`spec.media: { seedFromImage: true }` skip workloads + skip api
  optimisation (B/C from Q7).** Skip the initContainer on workloads
  that don't strictly need media (api). Rejected for consistency.
- **Async seeding for S3 (B/C from Q8).** Workloads come up against
  empty bucket; storefront serves broken images during seed window.
  Rejected: re-introduces the v0.5.0 broken-image UX, even briefly,
  on a feature whose entire purpose is fixing that.

## Open questions / future work

- **Auto re-seed on image rebuild.** Tracked for a v0.7+ ADR or
  enhancement: image-digest-keyed annotation triggers re-run of the
  seed Job (and per-pod init re-copy via a pod-template hash). Out
  of scope for v0.6.0.
- **Manual re-seed UX.** Today the procedure is: wipe bucket /
  delete PVC contents, `kubectl delete job <store>-media-seed`,
  `kubectl patch magestore <name>` to remove the `MediaSeeded`
  condition. Future enhancement: `mage.aknostic.com/reseed-media:
  "true"` annotation triggers the same flow; operator clears
  annotation after re-seed completes.
- **`MediaEmptyDir` extensibility.** The marker struct is reserved
  for future `sizeLimit` and `medium` fields if/when a user asks.
  Empty for now.
- **S3 seed observability.** Seed-Job logs are accessible via
  `kubectl logs job/<store>-media-seed`. Streaming progress
  (e.g., a `MediaSeeded` message that updates with byte-counts) is
  not emitted; condition transitions only on terminal Job state. If
  installation feedback becomes a UX concern, future enhancement is
  a sidecar that tails the sync output and patches the condition
  message.
- **kind e2e for S3 mode.** Deferred — Minio-in-kind setup exceeds
  the e2e budget for this ADR. Real-infra exposure happens in the
  v0.6.0 Kapsule validation; real-infra findings are fixed forward.

## References

- Existing media-handling: `internal/controller/ensure_media.go`,
  `internal/controller/ensure_workloads.go:295-345`.
- Image-contract: `docs/image-contract.md` (gets the
  `/app/pub/media-baked/` requirement section in this ADR's
  implementation).
- Magento `remote-storage:sync`: built into the
  `Magento_RemoteStorage` module since Magento 2.4.0.
- Reference image: `magento-test-image/Dockerfile`,
  `magento-test-image/env.php:13-32`.

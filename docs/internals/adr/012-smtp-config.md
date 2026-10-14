# ADR 012 — SMTP configuration

- **Status:** Accepted
- **Date:** 2026-05-04
- **Deciders:** @jurg
- **Related:** closes gap #9 of the v0.6.0 real-infra gap list; encodes
  the gap #11 admin-email-defaulting principle (never fall back to
  the admin's email as sender); mirrors the discriminated-union pattern
  from ADR 004 (database/cache/queue inline-vs-ref shape) without
  inheriting its three-way `Inline + Ref + External` shape (no
  sibling SMTP-as-a-service operator exists to compose).

## Context

The operator has no spec field for SMTP. Today's pattern is implicit:
customers create a `<store>-smtp` Secret out-of-band (typically via
SOPS — see `docs/internals/secrets.md`) with the 7 `SMTP_*` keys;
`ensure_workloads.go:298`, `ensure_consumers.go:224`,
`ensure_cron.go:202`, and `ensure_bootstrap.go:313` mount that Secret
as `optional: true` envFrom; the image's
`magento-test-image/entrypoint.sh` reads the env vars and writes
`/etc/msmtprc` on pod start.

Three things this fails to do:

1. **Doesn't model SMTP in the CRD at all.** A new user reading the
   MageStore reference has no way to discover that SMTP is
   configurable, much less how. Multi-tenant operators pointing at
   different SMTP relays per store have no declarative surface.
2. **Bootstrap script silently leaks the admin email.**
   `ensure_bootstrap.go:92` does `FROM_ADDR="${SMTP_FROM:-${ADMIN_EMAIL}}"`
   and writes that into Magento's `trans_email/ident_*/email` config.
   If a chart user sets `bootstrap.admin.email` but no SMTP, every
   transactional email Magento ever attempts is sent (and
   gateway-rejected) as the admin's address. This is the locked
   failure mode gap #11 exists to forbid.
3. **The optional-mount pattern silently re-activates SMTP.** Whenever
   a `<store>-smtp` Secret happens to exist (left over from a
   previous install, accidentally created, etc.), every pod picks up
   SMTP env vars without any spec declaration. Same silent-leak
   class.

The image-side surface (`entrypoint.sh`) already supports the full
SMTP shape: 7 env vars (`SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`,
`SMTP_PASSWORD`, `SMTP_FROM`, `SMTP_AUTH`, `SMTP_TLS`) with sensible
defaults (PORT=587, AUTH=login, TLS=starttls). The image also
supports msmtp's three TLS modes (`starttls | tls | off`) — losing
that fidelity to a bool would corner users on implicit-TLS-on-465
deployments. The CRD shape needs to expose this surface declaratively
without leaking the image's plumbing details.

Magento itself models 5 sender identities natively in
`trans_email/ident_*/email` (general, sales, support, custom1,
custom2). Production stores want different `From:` addresses per
mail type (orders@, support@, noreply@) within one verified domain
for sender-reputation reasons. The current bootstrap script collapses
all 5 into a single `SMTP_FROM` rewrite. A perfect-operator design
exposes per-identity overrides directly.

A perfect-operator design also catches "wrong credentials" before
Magento attempts the first transactional email. Today's failure mode
is "deploy succeeds, mail attempts fail 5–15 minutes later when an
admin tries password reset" — a probe Job moves that to "deploy
fails in 10 seconds with the relay's exact 5xx text".

## Decision

Add `spec.smtp` to the MageStore CRD as a two-way discriminated union
(`inline | secretRef`), expose Magento's 5 native sender identities,
gate workloads on a probe Job that exercises real SMTP authentication,
and remove the implicit-mount pattern entirely.

This is a **breaking change** in v0.6.0 (still v1alpha1 — within the
API contract for this version line). Existing customers using the
implicit `<store>-smtp` pattern migrate with a one-line CR edit:
`spec.smtp: { secretRef: { name: <store>-smtp } }`.

### Required CHANGELOG entry (v0.6.0 release deliverable)

The v0.6.0 CHANGELOG must include this `[v0.6.0]` → "Breaking
changes" subsection (verbatim wording is part of the ADR's
loud-failure-beats-silent-leak framing — implementer must not
soften it):

```markdown
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
  Per gap #11 admin-email-defaulting policy: `spec.smtp` unset →
  no SMTP env vars emitted → outbound mail fails at transport with
  a clear log line. Loud-failure-beats-silent-leak. See ADR 012
  for full design.
```

Without this CHANGELOG entry, customers upgrading from v0.5.x lose
SMTP silently — the exact failure mode the breaking change exists
to prevent.

### CRD shape

```go
// SMTPSpec is a discriminated union — exactly one of Inline or
// SecretRef must be set when the block is present.
type SMTPSpec struct {
    Inline    *SMTPInline           `json:"inline,omitempty"`
    SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

type SMTPInline struct {
    // Required.
    Host string `json:"host"`

    // Required. Default From: email address and SMTP envelope MAIL FROM.
    // Per gap #11 (admin-email defaulting policy, locked 2026-05-04):
    // operator does NOT fall back to bootstrap.admin.email.
    From string `json:"from"`

    // Optional. Default From: header display name. Magento renders
    // outgoing mail's From: header as "${FromName} <${From}>". When
    // empty, mail goes out as bare "<${From}>". Per-identity overrides
    // in FromIdentities take precedence.
    FromName string `json:"fromName,omitempty"`

    // Optional. Default 587.
    Port int32 `json:"port,omitempty"`

    // Optional. Anonymous / IP-allowlist relays leave User and
    // PasswordSecretRef unset (mailpit, on-prem Postfix relay rules).
    // CEL: has(user) == has(passwordSecretRef).
    User              string             `json:"user,omitempty"`
    PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`

    // Optional. Enum "login" | "plain" | "cram-md5". Default "login".
    // Maps 1:1 to the image entrypoint's SMTP_AUTH env var. Most
    // modern providers (SES, Postmark, SendGrid, Scaleway TEM) use
    // login; plain is functionally equivalent; cram-md5 is legacy
    // but still supported by some on-prem relays.
    Auth string `json:"auth,omitempty"`

    // Optional. Enum "starttls" | "tls" | "off". Default "starttls".
    // Maps 1:1 to the image entrypoint's SMTP_TLS env var.
    TLS string `json:"tls,omitempty"`

    // Optional. Per-identity From: header overrides for Magento's 5
    // built-in trans_email identities. Each identity carries its own
    // email + display name. Fields left empty fall back to the
    // umbrella From / FromName at the SMTPInline level.
    FromIdentities *SMTPFromIdentities `json:"fromIdentities,omitempty"`
}

// Closed set; mirrors Magento's built-in trans_email/ident_* keys.
// Modeled as a struct (rather than map[string]string) so typos like
// "salees" are caught at admit time without CEL key-whitelist gymnastics.
type SMTPFromIdentities struct {
    General *SMTPIdentity `json:"general,omitempty"`
    Sales   *SMTPIdentity `json:"sales,omitempty"`
    Support *SMTPIdentity `json:"support,omitempty"`
    Custom1 *SMTPIdentity `json:"custom1,omitempty"`
    Custom2 *SMTPIdentity `json:"custom2,omitempty"`
}

// SMTPIdentity carries one Magento trans_email identity's display
// surface. Magento has TWO config keys per identity —
// trans_email/ident_*/email AND trans_email/ident_*/name — so
// production stores can render "Acme Store Orders <orders@acme.example.com>"
// instead of bare "<orders@acme.example.com>". Either field can be empty;
// empty falls back to the SMTPInline-level umbrella value.
type SMTPIdentity struct {
    Email string `json:"email,omitempty"`
    Name  string `json:"name,omitempty"`
}
```

CEL on `SMTPSpec`:
```
(self.has('inline') ? 1 : 0) + (self.has('secretRef') ? 1 : 0) == 1
```

CEL on `SMTPInline`:
```
self.has('user') == self.has('passwordSecretRef')
```

`SMTPInline.TLS` enum constraint via kubebuilder marker:
`+kubebuilder:validation:Enum=starttls;tls;off`.

`SMTPInline.Auth` enum constraint via kubebuilder marker:
`+kubebuilder:validation:Enum=login;plain;cram-md5`.

### Inline mode → operator-emitted Secret keys

Operator constructs `<store>-smtp` Secret on each reconcile via
CreateOrUpdate, ownerReferenced to the MageStore:

| Key | Source |
|---|---|
| `SMTP_HOST` | `spec.smtp.inline.host` |
| `SMTP_FROM` | `spec.smtp.inline.from` |
| `SMTP_FROM_NAME` | `spec.smtp.inline.fromName` (key omitted if empty) |
| `SMTP_PORT` | `spec.smtp.inline.port` (else `587`) |
| `SMTP_USERNAME` | `spec.smtp.inline.user` (key omitted if empty) |
| `SMTP_PASSWORD` | resolved from `spec.smtp.inline.passwordSecretRef` (key omitted if unset) |
| `SMTP_AUTH` | `spec.smtp.inline.auth` (else `login`) |
| `SMTP_TLS` | `spec.smtp.inline.tls` (else `starttls`) |
| `SMTP_FROM_GENERAL` | `spec.smtp.inline.fromIdentities.general.email` (key omitted if empty) |
| `SMTP_FROM_NAME_GENERAL` | `spec.smtp.inline.fromIdentities.general.name` (key omitted if empty) |
| `SMTP_FROM_SALES` | `spec.smtp.inline.fromIdentities.sales.email` (key omitted if empty) |
| `SMTP_FROM_NAME_SALES` | `spec.smtp.inline.fromIdentities.sales.name` (key omitted if empty) |
| `SMTP_FROM_SUPPORT` | `spec.smtp.inline.fromIdentities.support.email` (key omitted if empty) |
| `SMTP_FROM_NAME_SUPPORT` | `spec.smtp.inline.fromIdentities.support.name` (key omitted if empty) |
| `SMTP_FROM_CUSTOM1` | `spec.smtp.inline.fromIdentities.custom1.email` (key omitted if empty) |
| `SMTP_FROM_NAME_CUSTOM1` | `spec.smtp.inline.fromIdentities.custom1.name` (key omitted if empty) |
| `SMTP_FROM_CUSTOM2` | `spec.smtp.inline.fromIdentities.custom2.email` (key omitted if empty) |
| `SMTP_FROM_NAME_CUSTOM2` | `spec.smtp.inline.fromIdentities.custom2.name` (key omitted if empty) |

The Secret carries `app.kubernetes.io/managed-by: mage-operator` as
a label, used by `ensureSMTP` to detect name-collision with a
pre-existing customer-created Secret of the same name (see
"Name-collision edge case" under Error handling).

### secretRef mode → Secret key contract

Customer-managed Secret carries the same surface as inline mode,
expressed as keys directly:

| Key | Required | Default | Notes |
|---|---|---|---|
| `SMTP_HOST` | yes | — | reconcile-time validation |
| `SMTP_FROM` | yes | — | reconcile-time validation; gap #11 enforced |
| `SMTP_FROM_NAME` | no | — | umbrella display name; empty = bare `<from>` |
| `SMTP_PORT` | no | `587` | image entrypoint default |
| `SMTP_USERNAME` | no | — | unset = anon relay; CEL parity with `SMTP_PASSWORD` |
| `SMTP_PASSWORD` | no | — | unset = anon relay; CEL parity with `SMTP_USERNAME` |
| `SMTP_AUTH` | no | `login` | image entrypoint enum: `login\|plain\|cram-md5` |
| `SMTP_TLS` | no | `starttls` | image entrypoint enum: `starttls\|tls\|off` |
| `SMTP_FROM_GENERAL` | no | `SMTP_FROM` | per-identity From: email |
| `SMTP_FROM_NAME_GENERAL` | no | `SMTP_FROM_NAME` | per-identity display name |
| `SMTP_FROM_SALES` | no | `SMTP_FROM` | per-identity From: email |
| `SMTP_FROM_NAME_SALES` | no | `SMTP_FROM_NAME` | per-identity display name |
| `SMTP_FROM_SUPPORT` | no | `SMTP_FROM` | per-identity From: email |
| `SMTP_FROM_NAME_SUPPORT` | no | `SMTP_FROM_NAME` | per-identity display name |
| `SMTP_FROM_CUSTOM1` | no | `SMTP_FROM` | per-identity From: email |
| `SMTP_FROM_NAME_CUSTOM1` | no | `SMTP_FROM_NAME` | per-identity display name |
| `SMTP_FROM_CUSTOM2` | no | `SMTP_FROM` | per-identity From: email |
| `SMTP_FROM_NAME_CUSTOM2` | no | `SMTP_FROM_NAME` | per-identity display name |

Operator validates `SMTP_HOST` + `SMTP_FROM` presence at reconcile
time → `SMTPReady=False, Reason=SecretMissingKeys` if absent. Operator
also validates `SMTP_USERNAME` ↔ `SMTP_PASSWORD` parity (both present
or both absent) — inline mode rejects this at admit time via CEL,
but a secretRef-mode Secret could ship malformed; without parity
validation here the probe Job's `set -eu` would exit on `unbound
variable` rather than surface a useful condition message.
`SMTPReady=False, Reason=SecretMissingKeys, Message="SMTP_USERNAME
requires SMTP_PASSWORD"` (or reverse). Other keys are best-effort
— missing keys fall back to entrypoint defaults or to `SMTP_FROM`
(per-identity overrides).

### Reconcile graph

New step `ensureSMTP` in `internal/controller/ensure_smtp.go`:

```
ensureDatabase    → DatabaseReady
ensureSearch      → SearchReady
ensureCache       → CacheReady
ensureQueue       → QueueReady
ensureMedia       → MediaEndpoint
ensureSecrets     → config Secret
ensureSMTP        → SMTPReady (new)            ← here
ensureBootstrap   → BootstrapReady
ensureMediaSeed   → MediaSeeded
ensureWorkloads   → WorkloadsReady
ensureVarnish     → VarnishReady
ensureConsumers   → ConsumersReady
ensureCron        → CronReady
```

`ensureSMTP` is the single producer of `SMTPReady`. Other reconcile
steps (bootstrap, workloads, consumers, cron) read but do not write.

### `SMTPReady` state machine

Two-stage. Stage 1 validates the Secret exists and carries required
keys; stage 2 runs a probe Job against the configured relay.

| Spec | `ensureSMTP` action | `SMTPReady` |
|---|---|---|
| `spec.smtp` unset | Best-effort delete owned `<store>-smtp` Secret if present; delete owned `<store>-smtp-probe` Job if present | omitted |
| `spec.smtp.inline` set, `<store>-smtp` exists without `app.kubernetes.io/managed-by: mage-operator` label | Don't write SMTP Secret | `False, Reason=SecretNameCollision, Message="Secret <ns>/<store>-smtp exists without operator's managed-by label; rename your Secret or migrate to spec.smtp.secretRef"` |
| `spec.smtp.inline` set, `passwordSecretRef` not found | Don't write SMTP Secret | `False, Reason=PasswordRefNotFound, Message="passwordSecretRef <namespace>/<name>:<key> not found"` |
| `spec.smtp.secretRef`, Secret missing | — | `False, Reason=SecretNotFound, Message="Secret <namespace>/<name> not found"` |
| `spec.smtp.secretRef`, missing `SMTP_HOST` or `SMTP_FROM` | — | `False, Reason=SecretMissingKeys, Message="missing keys: <list>"` |
| Keys validated, no probe Job exists yet | CreateOrUpdate `<store>-smtp` Secret (inline mode); create `<store>-smtp-probe` Job | `Unknown, Reason=Probing` |
| Probe Job Active | — | `Unknown, Reason=Probing` |
| Probe Job Succeeded | — | `True, Reason=Ready` |
| Probe Job Failed (`backoffLimit` exhausted) | — | `False, Reason=ProbeFailed, Message="<last line of swaks output>"` |

Probe Job re-runs whenever the SMTP-secret-hash changes (new
credentials → re-probe). `ensureSMTP` deletes the existing probe Job
when the hash diverges, forcing recreation.

### Probe Job spec

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: <store>-smtp-probe
  ownerReferences: [<MageStore>]
spec:
  backoffLimit: 1                     # 2 total attempts
  activeDeadlineSeconds: 60           # DNS-stall ceiling
  ttlSecondsAfterFinished: 600        # auto-clean 10 min after success
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: probe
          image: <workload image>
          envFrom:
            - secretRef: { name: <store>-smtp | spec.smtp.secretRef.name }
          command: ["sh", "-c"]
          args:
            - |
              set -eu
              AUTH_ARGS=""
              if [ -n "${SMTP_USERNAME:-}" ]; then
                  # swaks --auth takes uppercase enum names (LOGIN, PLAIN,
                  # CRAM-MD5); image entrypoint accepts lowercase. Upper-case
                  # here so SMTP_AUTH stays consistent across surfaces.
                  SWAKS_AUTH=$(echo "${SMTP_AUTH:-login}" | tr 'a-z' 'A-Z')
                  AUTH_ARGS="--auth $SWAKS_AUTH --auth-user $SMTP_USERNAME --auth-password $SMTP_PASSWORD"
              fi
              TLS_ARGS=""
              case "${SMTP_TLS:-starttls}" in
                  starttls) TLS_ARGS="--tls" ;;
                  tls)      TLS_ARGS="--tls-on-connect" ;;
                  off)      TLS_ARGS="" ;;
              esac
              swaks --to "validation@example.com" --from "$SMTP_FROM" \
                --server "$SMTP_HOST" --port "${SMTP_PORT:-587}" \
                $AUTH_ARGS $TLS_ARGS --quit-after AUTH --silent
```

Failed Jobs persist (no TTL on failure) so users can inspect
`kubectl logs job/<store>-smtp-probe` to see the relay's exact
error text.

`--quit-after AUTH` deliberately stops before any envelope state.
Some relays (SES sandbox, strict provider configs) `5xx`-reject
`RCPT TO: validation@example.com` even when AUTH succeeded — they
enforce verified-recipient rules at RCPT time. Stopping at AUTH
verifies "credentials valid + relay reachable + TLS works" without
crossing into provider-specific recipient policy.

### Image-contract delta — `swaks` in the workload image

Reference image's runtime stage adds one apt line:

```dockerfile
RUN apt-get update \
    && apt-get install -y --no-install-recommends swaks ca-certificates \
    && rm -rf /var/lib/apt/lists/*
```

`swaks` is in Debian/Ubuntu apt; pulls `Net::SMTP::SSL` and
`Authen::SASL` as deps. ~150 KB total against a 2-4 GB image.

Customer images forking the reference need the same line for the
probe Job to run. Without `swaks`, the probe Job exits 127
("command not found") → `SMTPReady=False, Reason=ProbeFailed,
Message="image-contract violation: swaks not in image"`. Hard-fail
shape mirrors ADR 011's missing `media-baked` directory.

### Pod-template integration — conditional envFrom

Today: all four pod templates (workloads, consumers, cron, bootstrap)
unconditionally mount `smtpSecretName(store)` with `optional: true`.

ADR 012 makes the mount **conditional based on `spec.smtp`**:

```go
envFrom := []corev1.EnvFromSource{
    {SecretRef: &corev1.SecretEnvSource{
        LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
    }},
}

if smtpName := smtpEnvFromSecretName(store); smtpName != "" {
    envFrom = append(envFrom, corev1.EnvFromSource{
        SecretRef: &corev1.SecretEnvSource{
            LocalObjectReference: corev1.LocalObjectReference{Name: smtpName},
            // No Optional: true. Reaching this branch means
            // SMTPReady=True (gating), so the Secret MUST exist.
        },
    })
}
```

Where `smtpEnvFromSecretName(store)` returns:
- `""` if `spec.smtp` is unset (no mount)
- `<store>-smtp` if `spec.smtp.inline` is set
- `spec.smtp.secretRef.name` if `spec.smtp.secretRef` is set

The `optional: true` flag goes away — gating on `SMTPReady=True`
guarantees the referenced Secret exists by the time pods are spawned.
Keeping `optional: true` would silently swallow the "Secret deleted
out from under us" failure mode.

The current `smtpSecretName` helper (returns `<store>-smtp` always)
is misleading once we have secretRef mode. Renamed to
`inlineSMTPSecretName` (only used by inline mode + `ensureSMTP`
cleanup paths). New `smtpEnvFromSecretName(store)` is the abstraction
the four ensure-pod-template steps consume.

### Hash-driven rollout

envFrom-mounted Secret content changes do **not** auto-rollover on
running pods (Kubernetes does not propagate Secret-data changes into
running envvars). Solution: include the SMTP Secret content hash in
the workload pod-template annotation — same mechanism used for
`nginx-config-hash` and `config-secret-hash` today.

Annotation key: `mage.aknostic.com/smtp-secret-hash`. Hash input:
sorted-key concatenation of the SMTP envFrom Secret's data. Hash
flips → pod-template differs → Deployment / Job / CronJob rolls.

### Gating

Workloads, consumers, cron, and bootstrap **gate on `SMTPReady=True`**
when `spec.smtp` is set. `Unknown` (probe in flight) blocks too —
"Ready" means both gating-validation AND probe-success.

`Reason=AwaitingSMTP, Message="probe Job <name> in progress"` until
probe terminal state.

When `spec.smtp` is unset → no gating. All four steps proceed as
today, with no SMTP envFrom mount on their pod templates.

### Bootstrap script change

`ensure_bootstrap.go:91-95` — current logic falls back to the admin
email when `SMTP_FROM` is unset. Per gap #11 locked principle, drop
the fallback and skip the rewrite entirely when SMTP is unset:

```sh
echo "== rewriting trans_email identities to a real sender =="
if [ -n "${SMTP_FROM:-}" ]; then
    for ident in general sales support custom1 custom2; do
        case "$ident" in
            general)
                addr="${SMTP_FROM_GENERAL:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_GENERAL:-${SMTP_FROM_NAME:-}}"
                ;;
            sales)
                addr="${SMTP_FROM_SALES:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_SALES:-${SMTP_FROM_NAME:-}}"
                ;;
            support)
                addr="${SMTP_FROM_SUPPORT:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_SUPPORT:-${SMTP_FROM_NAME:-}}"
                ;;
            custom1)
                addr="${SMTP_FROM_CUSTOM1:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_CUSTOM1:-${SMTP_FROM_NAME:-}}"
                ;;
            custom2)
                addr="${SMTP_FROM_CUSTOM2:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_CUSTOM2:-${SMTP_FROM_NAME:-}}"
                ;;
        esac
        php -d memory_limit=2G /app/bin/magento config:set \
          "trans_email/ident_${ident}/email" "$addr"
        if [ -n "$name" ]; then
            php -d memory_limit=2G /app/bin/magento config:set \
              "trans_email/ident_${ident}/name" "$name"
            echo "    ident_${ident} → \"${name}\" <${addr}>"
        else
            echo "    ident_${ident} → <${addr}>"
        fi
    done
    php -d memory_limit=2G /app/bin/magento cache:clean config
else
    echo "  SMTP_FROM unset → skipping trans_email rewrite"
    echo "  (mail delivery will fail at transport with 551 errors, by design;"
    echo "   set spec.smtp on the MageStore to configure outbound email)"
fi
```

The case statement replaces an earlier `eval` indirection; bash
parameter expansion is well-defined here, no eval surface, no
shell-injection risk on weird-but-valid email addresses.

The per-identity `echo "    ident_${ident} → ${addr}"` line surfaces
the effective trans_email rewrite in `kubectl logs job/<store>-bootstrap`
— direct debug visibility without needing to exec into a workload pod
and run `bin/magento config:show`.

The comment block at `ensure_bootstrap.go:44-50` gets rewritten to
reflect the new rationale:

```go
//   - trans_email identity rewrite — setup:install seeds every
//     trans_email/ident_*/email with user@example.com. Every real
//     SMTP gateway 551-rejects that envelope sender. When SMTP_FROM
//     is set (spec.smtp configured), we rewrite each of the 5 identities
//     using the per-identity SMTP_FROM_* env var if present, else the
//     umbrella SMTP_FROM. When SMTP_FROM is unset, we skip the rewrite
//     entirely — outbound mail fails at transport with a clear log
//     line, which is preferable to silently sending with the admin's
//     email as From: (per gap #11 admin-email-defaulting policy,
//     locked 2026-05-04).
```

## Error handling

### `spec.smtp.secretRef` points at non-existent Secret

`ensureSMTP` `Get` returns NotFound → `SMTPReady=False,
Reason=SecretNotFound, Message="Secret <namespace>/<name> not found"`.
Reconcile requeues normally; user creates the Secret; next reconcile
picks it up. Bootstrap/workloads/consumers/cron stay on `AwaitingSMTP`.

### `spec.smtp.secretRef` Secret missing required keys

`SMTPReady=False, Reason=SecretMissingKeys, Message="missing keys:
SMTP_HOST, SMTP_FROM"`. Per-identity override keys are best-effort
— their absence does NOT fail validation.

### `spec.smtp.inline.passwordSecretRef` missing

`SMTPReady=False, Reason=PasswordRefNotFound`. The owned `<store>-smtp`
Secret is **not written** (no partial-state Secret with a missing
password value).

### Probe Job fails — wrong credentials, unreachable relay, TLS reject

`SMTPReady=False, Reason=ProbeFailed, Message="<swaks last line>"`.
`swaks`'s last line is the relay's exact 5xx response. Examples:
- "535 5.7.8 Authentication credentials invalid"
- "550 5.1.1 unverified sender"
- "Connection refused at /usr/bin/swaks line N" (DNS / TCP failure)

Failed probe Jobs persist for log inspection (no TTL on failure).
User fixes credentials → SMTP-secret-hash flips → ensureSMTP deletes
the failed Job and creates a fresh one.

### Inline-mode Secret lifecycle

- Created on first reconcile when `spec.smtp.inline` is set and
  password resolves.
- Updated on every subsequent reconcile (CreateOrUpdate). Idempotent;
  no diff means no-op.
- Deleted by `ensureSMTP` when `spec.smtp` transitions to unset OR to
  `secretRef` mode. Operator owns it; explicit delete is cleaner
  than relying on ownerReference garbage collection.
- ownerReference GC catches the explicit-delete miss as defence in
  depth.

### `spec.smtp` removed while pods are running

`ensureSMTP` removes the condition from `status.conditions` and
(inline mode) deletes the owned Secret. Pod-template
`smtp-secret-hash` annotation flips (now hashes empty/no-secret state)
→ workloads/consumers/cron rolled on next reconcile. New pods spawn
without SMTP envFrom; msmtp falls back to its "no config" path;
outbound mail fails at transport with the clear log line.

trans_email/ident_*/email rows in Magento DB still hold the previous
`SMTP_FROM`/per-identity values (bootstrap doesn't re-run). Magento
attempts to send with those From: headers but the underlying msmtp
transport refuses → mail logs surface the failure. User redeploys
with `spec.smtp` set or accepts the broken-mail state.

### Name-collision: pre-existing `<store>-smtp` Secret without operator label

The migration story (`spec.smtp.secretRef: { name: <store>-smtp }`)
keeps customer-managed Secrets named `<store>-smtp` — exactly the
name inline mode also wants to use. If a customer transitions from
the implicit pattern straight to **inline** mode (skipping secretRef
as an intermediate), there's a pre-existing Secret at `<store>-smtp`
that the operator's CreateOrUpdate would absorb on the first
reconcile. The customer's data gets overwritten silently.

`ensureSMTP` defends against this: before CreateOrUpdate on
`<store>-smtp`, the operator `Get`s the Secret and checks the
`app.kubernetes.io/managed-by` label. Three cases:

- **Secret doesn't exist** → CreateOrUpdate proceeds; new Secret has
  the managed-by label.
- **Secret exists with `managed-by: mage-operator` label** → operator
  owns it; CreateOrUpdate proceeds.
- **Secret exists without the managed-by label** → operator refuses
  to write. `SMTPReady=False, Reason=SecretNameCollision,
  Message="Secret <ns>/<store>-smtp exists without operator's
  managed-by label; rename your Secret or migrate to
  spec.smtp.secretRef"`.

Resolution paths for the customer:
1. Migrate to `spec.smtp.secretRef: { name: <store>-smtp }` (use the
   existing Secret as-is via secretRef mode).
2. Rename the existing Secret (`kubectl get secret <store>-smtp -o
   yaml | sed 's/<store>-smtp/<new-name>/' | kubectl apply -f -;
   kubectl delete secret <store>-smtp`) and use
   `spec.smtp.secretRef: { name: <new-name> }`.
3. Delete the existing Secret (data loss; only safe if the customer
   has the source-of-truth elsewhere) — operator's inline mode then
   creates a fresh `<store>-smtp` it owns.

This also means: if a v0.6.0 user EVER runs `spec.smtp.inline` and
later transitions back to the implicit-style "I want to manage
`<store>-smtp` myself" workflow, they must use `spec.smtp.secretRef`
explicitly. The inline mode never relinquishes ownership of a Secret
once it has written to it.

### Inline → secretRef pointing at the same name

Edge case: user transitions from `spec.smtp.inline` to
`spec.smtp.secretRef: { name: <store>-smtp }`. Operator deletes the
previously-owned `<store>-smtp` Secret because it's no longer
inline-owned, then secretRef can't find it. Race window.

Resolution: `ensureSMTP` detects this case (secretRef.name matches
inline-mode naming convention `<store>-smtp`) and **does not delete**
the Secret on the inline → secretRef transition. The Secret stays;
ownership-tracking annotation flips from operator-owned to
user-managed. Documented limitation: subsequent edits to the Secret
content don't trigger probe re-run automatically (operator no longer
writes to the Secret); user manually deletes the probe Job to
re-trigger.

### Provider-side rejection (SES sandbox, unverified domain, etc.)

Out of scope for the operator. Probe Job catches AUTH-level rejections
(invalid credentials, blocked IP). Verified-domain / DKIM / SPF
issues surface only at MAIL FROM / DATA time, which the
`--quit-after AUTH` probe deliberately skips. Customer's responsibility
to configure provider-side verification before configuring `spec.smtp`.

### Multi-tenant operators with different SMTP relays per store

Each MageStore CR has its own `spec.smtp`; condition + Secret + probe
Job are per-store. No cross-store interaction. Tested via envtest
fixture with two MageStores in different namespaces, each with
distinct inline SMTP configs.

### Diagnostic snippet

Quick-reference commands for `docs/internals/troubleshooting.md` (or
operator README):

```bash
# Inspect SMTPReady reason
kubectl get magestore <name> \
  -o jsonpath='{.status.conditions[?(@.type=="SMTPReady")]}{"\n"}'

# View effective SMTP config (inline mode)
kubectl get secret <store>-smtp -o yaml | yq '.data | map_values(@base64d)'

# View probe Job output (failure mode includes relay's exact 5xx text)
kubectl logs job/<store>-smtp-probe

# Force probe Job re-run (e.g. after fixing a secretRef-mode Secret
# in place, or recovering from the inline→secretRef-same-name transition
# where operator no longer auto-detects content changes)
kubectl delete job/<store>-smtp-probe

# View bootstrap's per-identity rewrites (each ident shows
# both the From: email AND the display name when configured)
kubectl logs job/<store>-bootstrap | grep "ident_"
```

**Probe passes but mail still fails?** The probe's `--quit-after AUTH`
deliberately stops before envelope state, so it does NOT exercise
provider-side sender verification (DKIM/SPF/DMARC, SES sandbox
recipient rules, Postmark verified-sender lists). If `SMTPReady=True`
but Magento's transactional emails still 5xx-reject, the gap is
provider-side: verify the `SMTP_FROM` domain in the relay's admin
console, ensure DKIM/SPF DNS records exist, lift any sandbox /
unverified-sender restrictions. The probe Job validates "credentials
work + relay reachable + TLS works", not "mail will actually be
delivered to recipients in the wild".

## Testing

### Envtest unit cases

Location: `internal/controller/ensure_smtp_test.go`, plus amendments
to `ensure_workloads_test.go`, `ensure_consumers_test.go`,
`ensure_cron_test.go`, `ensure_bootstrap_test.go`.

CRD validation:
- `spec.smtp` unset → accepted (back-compat).
- `spec.smtp: {}` → rejected (CEL discriminator: 0 modes).
- `spec.smtp: { inline: ..., secretRef: ... }` → rejected (2 modes).
- `spec.smtp.inline` without `host` → rejected (required).
- `spec.smtp.inline` without `from` → rejected (required).
- `spec.smtp.inline.user` set without `passwordSecretRef` → rejected.
- `spec.smtp.inline.passwordSecretRef` set without `user` → rejected.
- `spec.smtp.inline.tls` outside enum → rejected.
- `spec.smtp.inline.auth` outside enum (`login`/`plain`/`cram-md5`) → rejected.
- `spec.smtp.inline.fromIdentities.{general,sales,support,custom1,custom2}.{email,name}`
  accepted; arbitrary keys rejected by struct shape.
- `spec.smtp.inline.fromName` accepted (umbrella display name).
- `spec.smtp.secretRef.name` empty → rejected.

`ensureSMTP` Secret + condition logic:
- `spec.smtp` unset → no `<store>-smtp` Secret created; no
  `SMTPReady` condition; pre-existing owned Secret deleted;
  pre-existing probe Job deleted.
- `spec.smtp.inline` set, `passwordSecretRef` resolves → `<store>-smtp`
  Secret created with all derived `SMTP_*` keys; ownerRef to
  MageStore; `SMTPReady=Unknown, Reason=Probing` (probe Job created).
- Same setup, `passwordSecretRef` Secret missing → no `<store>-smtp`
  written; `SMTPReady=False, Reason=PasswordRefNotFound`.
- Pre-existing `<store>-smtp` Secret without `app.kubernetes.io/managed-by:
  mage-operator` label → no CreateOrUpdate; `SMTPReady=False,
  Reason=SecretNameCollision`. After the user adds the label OR
  deletes the Secret OR migrates to secretRef, the next reconcile
  unblocks.
- secretRef-mode Secret with `SMTP_USERNAME` set but no `SMTP_PASSWORD`
  (or vice versa) → `SMTPReady=False, Reason=SecretMissingKeys,
  Message="SMTP_USERNAME requires SMTP_PASSWORD"`. Symmetric for the
  reverse case.
- `spec.smtp.inline` with anonymous-relay shape (no `user`, no
  `passwordSecretRef`) → `<store>-smtp` written without
  `SMTP_USERNAME`/`SMTP_PASSWORD` keys; probe runs with empty
  AUTH_ARGS.
- `spec.smtp.secretRef` pointing at existing Secret with valid keys
  → no operator-owned Secret; probe Job created.
- `spec.smtp.secretRef` pointing at non-existent Secret →
  `SMTPReady=False, Reason=SecretNotFound`.
- `spec.smtp.secretRef` Secret missing `SMTP_HOST` →
  `SMTPReady=False, Reason=SecretMissingKeys, Message` includes the
  list.
- Probe Job present + Active → `SMTPReady=Unknown`.
- Probe Job present + Succeeded → `SMTPReady=True, Reason=Ready`.
- Probe Job present + Failed (`backoffLimit` exhausted) →
  `SMTPReady=False, Reason=ProbeFailed, Message` includes last log
  line.
- `SMTPReady=True` already + secret-hash unchanged → next reconcile
  no-ops on probe Job.
- Secret-hash flipped → existing probe Job deleted, new one created,
  condition flipped to Unknown.

Per-identity inline → Secret keys:
- `spec.smtp.inline.fromIdentities.general.email` set → Secret has
  `SMTP_FROM_GENERAL` key with that value.
- `spec.smtp.inline.fromIdentities.general.name` set → Secret has
  `SMTP_FROM_NAME_GENERAL` key with that value.
- `spec.smtp.inline.fromIdentities.general` set with only `email`
  (no `name`) → Secret has `SMTP_FROM_GENERAL`, NO `SMTP_FROM_NAME_GENERAL`.
- `spec.smtp.inline.fromName` set → Secret has umbrella `SMTP_FROM_NAME`.
- Empty per-identity field → key omitted from Secret entirely.

Pod-template integration:
- `spec.smtp` unset → workloads/consumers/cron/bootstrap pod
  templates have NO SMTP envFrom mount.
- `spec.smtp.inline` with `SMTPReady=True` → all four pod templates
  carry an envFrom referring to `<store>-smtp` (no `optional: true`).
- `spec.smtp.secretRef: { name: foo }` with `SMTPReady=True` → all
  four pod templates carry an envFrom referring to `foo`.

Hash-flip regression assertion (single most important regression-class
test): reconcile twice with different `spec.smtp.inline.host` values
→ assert the pod-template `mage.aknostic.com/smtp-secret-hash`
annotation **differs between reconciles** on every workload Deployment,
the bootstrap Job, the consumer Deployment, and the cron CronJob.
Catches "operator updates owned Secret but pods don't see new env
vars" — silent stale-config — which is a real-world horror story.

Gating:
- `spec.smtp` set, `SMTPReady=Unknown` (probe in flight) → bootstrap
  Job not created; workloads/consumers/cron Deployments not emitted;
  reasons all `AwaitingSMTP`.
- `spec.smtp` set, `SMTPReady=False` → same.
- `spec.smtp` set, `SMTPReady=True` → all four proceed.
- `spec.smtp` unset → all four proceed; `SMTPReady` condition absent.

Transition cases:
- inline → unset → inline cycle: owned Secret + probe Job created →
  deleted on unset → recreated on re-set; condition transitions
  match.
- inline → secretRef cycle: owned Secret deleted (unless secretRef
  points at the same name; see edge case); new probe Job spawned;
  condition flips through Unknown→True.
- secretRef → inline cycle: owned Secret created; probe Job re-runs.
- All transitions trigger pod-template hash flips (verified via
  annotation diff).

### E2e (kind, `test/e2e/smtp_test.go`)

Two scenarios using mailpit as in-cluster SMTP capture (mailpit's
container image is ~30 MB; the kind cluster pulls it once):

- **Inline mode happy path:** deploy mailpit Service in test
  namespace → apply MageStore with `spec.smtp.inline` pointing at
  `mailpit.<ns>.svc:1025`, `tls: off`, anonymous (no user/password),
  `fromName: "Acme Store"`,
  `fromIdentities.general: { email: noreply@example.com, name: "Acme Store" }`,
  `fromIdentities.sales: { email: orders@example.com, name: "Acme Store Orders" }`.
  Wait for `SMTPReady=True`. Trigger a mail send via Magento admin
  REST → curl mailpit's HTTP API → assert one mail captured with
  `From: "Acme Store" <noreply@example.com>` header. Then exec
  `bin/magento config:show trans_email/ident_sales/email` → assert
  `orders@example.com`; `config:show trans_email/ident_sales/name`
  → assert `Acme Store Orders`.

- **Probe failure path:** apply MageStore with
  `spec.smtp.inline.host: nonexistent.example.invalid`. Wait 60s
  (probe `activeDeadlineSeconds`). Assert `SMTPReady=False,
  Reason=ProbeFailed, Message` contains "swaks" output (DNS resolution
  failure or connection refused). Bootstrap Job not created. Patch
  CR to a working mailpit; assert recovery (`SMTPReady=True`,
  bootstrap proceeds).

### Gap #11 smoking-gun e2e regression test

The single most important e2e assertion. Apply MageStore with
`spec.bootstrap.admin.email: changeme@example.com` and `spec.smtp`
unset → wait for bootstrap Job complete → exec into a workload pod
→ run `bin/magento config:show trans_email/ident_general/email` →
**assert output is `user@example.com`** (the `setup:install` seed,
NOT the admin email). Repeat for the other 4 identities. Also assert
`config:show trans_email/ident_general/name` returns the
`setup:install`-seeded default (typically "Owner") — NOT the admin's
firstname+lastname or any operator-derived value. Both email AND
name must remain operator-untouched when `spec.smtp` is unset.

Without this test, a future contributor could reintroduce
`FROM_ADDR="${SMTP_FROM:-${ADMIN_EMAIL}}"` and every other test
would still pass — envtest doesn't run `setup:install`; the e2e
happy path has `spec.smtp` set. This test is the loud-failure for
gap #11 regression.

### Image-contract regression test

The reference-image build (the `build-magento-reference` job of the
GitLab pipeline at the time; now
`.github/workflows/reference-image.yml`, which pushes
`ghcr.io/aknostic/magento-reference`) gets a post-build smoke step:

```
docker run --rm $IMAGE which swaks
```

Same pattern as ADR 011's `media-baked` directory check, lands in
the same job.

### Sample-manifest CI-guardrail compatibility

ADR 012's implementation will add `spec.smtp` examples to
`config/samples/mage_v1alpha1_magestore.yaml`, `docs/quickstart.md`,
`docs/magestore-reference.md`. Per gap #11's CI guardrail, all such
manifests must use `changeme@example.com` (Tier 1 — headline samples)
or `admin@example.com` (Tier 2 — throwaway prose) for every email
field. Automatically enforced by the existing `no-personal-emails`
job; the implementer doesn't need to do anything beyond using the
correct placeholders.

## Consequences

### Positive

- **CRD-level discoverability.** Users reading the MageStore reference
  see `spec.smtp` as a first-class field with documented sub-shape.
- **Closes gap #11.** `SMTP_FROM` is no longer derived from
  `bootstrap.admin.email`. Loud-failure-beats-silent-leak encoded in
  CRD validation, reconciler logic, and bootstrap script.
- **Per-identity senders, full surface.** Both `From:` email AND
  display name exposed per identity, mirroring Magento's native
  `trans_email/ident_*/email` + `trans_email/ident_*/name` config
  pair. Production stores configure proper sender-reputation
  patterns ("Acme Store Orders <orders@>", "Acme Store Support
  <support@>") without custom Magento module work.
- **Fast-feedback debug loop.** Probe Job catches wrong credentials
  / unreachable relay / TLS misconfig in ~10s rather than 5–15min
  after Magento attempts the first transactional email. Probe-Job
  failure log carries the relay's exact 5xx text.
- **Multi-tenant clean separation.** Each MageStore has its own
  `spec.smtp`; no implicit Secret-name collision between stores in
  the same namespace.
- **No silent activation.** Removing `spec.smtp` from the CR removes
  SMTP from all pods. No surprise re-activation if a stray Secret
  named `<store>-smtp` happens to exist.

### Negative

- **Breaking change.** Existing customers using the implicit
  `<store>-smtp` Secret pattern must add `spec.smtp.secretRef:
  { name: <store>-smtp }` to their MageStore CR. One-line migration,
  documented in CHANGELOG `[v0.6.0]` "Breaking changes".
- **Image-contract addition.** Customer images forking the reference
  must add `swaks` (one apt line, ~150 KB). Without it, probe Job
  fails with a clear "image-contract violation" error.
- **First-install latency grows by ~10s.** Probe Job sits on the
  critical path before bootstrap unblocks. Acceptable tradeoff for
  catching wrong-credentials early.
- **Pod-template hash on four resource kinds.** Workload Deployments,
  consumer Deployments, cron CronJobs, and the bootstrap Job all
  carry the `smtp-secret-hash` annotation. SMTP config edit triggers
  rolling restarts across all four. SMTP edits are rare; acceptable.

### Neutral

- **`SMTPReady` is a new top-level status condition.** The `MageStore`
  ready-condition list grows by one. CHANGELOG entry on release.
- **Probe Jobs accumulate on failure.** Failed probe Jobs persist
  (no TTL on failure) for log inspection. Ownership-referenced;
  garbage-collected with the MageStore.

## Alternatives considered

- **Three-way `Inline + Ref + External` (mirroring DatabaseSpec).**
  No sibling SMTP-as-a-service operator exists to compose, so the
  middle "Ref → CR managed by another operator" mode collapses
  into the `secretRef` mode. Two-way is the correct mapping.
- **Bool `startTLS` instead of enum `tls`.** Loses
  implicit-TLS-on-465 support (uncommon but real). Enum keeps the
  three msmtp modes 1:1 with the image surface.
- **`map[string]string` for `fromIdentities`.** Allows arbitrary
  keys including typos (`salees`). Closed-set struct catches typos
  at admit time without CEL key-whitelist gymnastics.
- **Per-identity email-only (no display name).** Initial draft
  exposed only `trans_email/ident_*/email`, leaving the
  `trans_email/ident_*/name` config key on Magento defaults.
  Rejected on a careful re-read: "Acme Store Orders <orders@>"
  vs. bare "<orders@>" is a real production-experience difference,
  Magento natively models both, and adding the field after v0.6.0
  ships requires a CRD change ripple (non-breaking, but a
  code/docs/CHANGELOG ripple). Cost-of-late > cost-of-now; folded
  into the design.
- **Hide `SMTP_AUTH` in inline mode (defer to entrypoint default).**
  Considered as YAGNI given ~all modern providers use `login`. Rejected
  because exposing TLS-as-enum but hiding AUTH-as-enum is asymmetric:
  the cost of an additional `+kubebuilder:validation:Enum=login;plain;cram-md5`
  is one line; the cost-of-defer is "user trips on it, files an
  issue, we add it in a later release". One-line consistency wins.
- **`--quit-after RCPT` for the probe.** Some relays 5xx-reject
  RCPT for unverified recipients even when AUTH succeeded
  (SES sandbox is the canonical example). False-negative on
  `SMTPReady`. `--quit-after AUTH` strictly better.
- **`eval` for per-identity selection in bootstrap script.** Works,
  but invites shell-injection or weird-character regressions.
  Explicit case is 2 LOC longer with no eval surface.
- **Keep `optional: true` on the SMTP envFrom mount.** Today's
  pattern. Silently swallows "Secret deleted" failures; pods come up
  without SMTP env vars and the user wonders why mail is broken.
  Removing `optional: true` (gated by `SMTPReady=True`) makes the
  failure mode loud.
- **Soft fallback to `bootstrap.admin.email` for `SMTP_FROM`.**
  Locked out by gap #11 (admin-email-defaulting-policy spec).
  Re-litigated and locked here for future readers.
- **Operator-managed mailpit / maildev sidecar Deployment for dev
  capture.** Scope creep — operator would run a mail-receiving
  service. Better: customers deploy mailpit themselves and point
  `spec.smtp.inline` at it. Two-line dev manifest beats embedded
  service.
- **External Secrets Operator (ESO) wiring for inline-mode
  password Secret.** CLAUDE.md commits to ESO support from v0.1.0,
  but the existing inline modes (DatabaseSpec, CacheSpec, QueueSpec)
  also don't yet have ESO wiring. Doing it for SMTP alone would be
  inconsistent. Track as a separate cross-cutting v0.7+ ADR ("ESO
  inline-mode integration across all resources") — apply to all
  four resource types in one consistent change.
- **On-demand "send a test mail" annotation
  (`mage.aknostic.com/smtp-test=true`).** Useful for ad-hoc
  verification but adds annotation-driven workflow surface. Defer
  unless users ask. The probe Job already validates AUTH-level
  reachability; full delivery testing is a customer-side concern
  (provider DKIM/SPF setup, etc.).

## Open questions / future work

- **ESO wiring across inline modes.** Tracked separately as a
  cross-cutting v0.7+ ADR. Apply consistently to DatabaseSpec,
  CacheSpec, QueueSpec, SMTPSpec at once.
- **Probe Job extensions.** `--quit-after AUTH` is the right scope
  today. Future extension: optional opt-in `spec.smtp.probeMode:
  full` that exercises through `MAIL FROM` to surface
  verified-domain issues earlier. Costs a Provider-specific
  classification of which 5xx codes mean "fail loudly" vs. "warn
  but proceed". Defer unless users ask.
- **`bin/magento dev:email:test`-style on-demand harness.**
  Annotation-triggered test send; useful but scope creep. Note as
  potential v0.7+ enhancement.

## References

- Existing implicit-mount pattern: `internal/controller/ensure_secrets.go:266-274`,
  `internal/controller/ensure_workloads.go:298`,
  `internal/controller/ensure_consumers.go:224`,
  `internal/controller/ensure_cron.go:202`,
  `internal/controller/ensure_bootstrap.go:313`.
- Bootstrap script trans_email rewrite (current): `ensure_bootstrap.go:91-95`.
- Image SMTP plumbing: `magento-test-image/entrypoint.sh`.
- Existing discriminated unions: `api/v1alpha1/magestore_types.go`
  `DatabaseSpec` (lines 256-279), `CacheSpec` (lines 499-531),
  `QueueSpec` (lines 621-648).
- ADR 004 — seeded-but-extractable dependencies (the inline-vs-ref
  pattern this ADR mirrors).
- ADR 011 — image-baked media seeding (parallel pattern: image-contract
  delta + probe/seed Job + new status condition).

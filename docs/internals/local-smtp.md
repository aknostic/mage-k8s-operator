# Local SMTP via Scaleway TEM + msmtp

Magento 2.4.x mandates 2FA for every admin login; the setup flow
requires receiving an authenticator link by email, and password resets
also go through email. Getting the admin panel usable in a dev kind
cluster means giving the pod a working outbound SMTP path.

This document describes the flow the reference image (`magento-test-
image/`) and the operator ship for local development. It's dev/acc
shape, not production scale — production sites use a real transactional
email provider (AWS SES, SendGrid, Postmark, and friends) and
configure it through the same env var namespace.

## Responsibility split — generic operator vs. local-test-specific config

Keeping the operator reusable across customers means strict separation
between what it emits and what customers (or local dev flows) configure
on top. The table below maps each piece.

| Layer                          | Generic (operator / reference image) | Local-test-specific (this repo's samples) |
|--------------------------------|---------------------------------------|-------------------------------------------|
| Mounting the SMTP Secret       | `ensureWorkloads` wires `envFrom` for `<store>-smtp` as optional. No knowledge of Scaleway, TEM, or any specific provider. | N/A — same machinery applies in prod.     |
| `/etc/msmtprc` generation      | `magento-test-image/entrypoint.sh` translates `SMTP_*` env vars to msmtp config. Provider-agnostic — works for any STARTTLS/TLS SMTP gateway. | N/A — image is the reference.             |
| SMTP credentials themselves    | Never in image. Never in operator code. | `config/secrets/smtp.yaml` (SOPS-encrypted). |
| Sender identity (`trans_email/ident_*/email`) | Magento ships defaults at `*@example.com`. Operator doesn't touch them. | `config/samples/bootstrap-install-job.yaml` sets them to `SMTP_FROM` after `setup:install`. |
| Admin user email               | Operator doesn't know what constitutes a valid admin inbox. | `bootstrap-install-job.yaml` reads `ADMIN_EMAIL`; default `admin@example.com` for in-house dev, override per user. |

The rule of thumb: **if it names a domain, a mailbox, or a provider,
it's customer-specific and belongs in a manifest/Secret, not in the
operator or image.** The operator's `ensureWorkloads` mounts a Secret
by name; what's in the Secret is none of its business. The reference
image's entrypoint reads a fixed env var namespace; the values are
none of its business. Only `bootstrap-install-job.yaml` — explicitly
a local test/install template — knows about Scaleway TEM, sender
domains, and admin mailboxes.

Production customers replace `bootstrap-install-job.yaml` with their
own first-run bootstrapping (initContainer, their CI pipeline, their
GitOps setup). They bring their own sender identity conventions and
admin user policy. The operator still works the same.

## Two addresses, two different concerns

A common confusion point — they look alike but serve different roles:

| Value           | Set via                                                                    | Constraint                                  |
|-----------------|----------------------------------------------------------------------------|---------------------------------------------|
| `SMTP_FROM`     | `demo-store-smtp` Secret → env var → `/etc/msmtprc` `from` line → envelope-from | **Must be on a domain your SMTP gateway has verified.** Scaleway TEM 551-rejects envelope senders on unverified domains. |
| `trans_email/ident_*/email` | `bootstrap-install-job.yaml` → `bin/magento config:set` after install | Same constraint: Magento uses this as the From: header and sendmail uses it as envelope-from. Set to `SMTP_FROM` for consistency. |
| Admin user email | `bootstrap-install-job.yaml` `--admin-email="${ADMIN_EMAIL}"`, later `UPDATE admin_user SET email=...` | **Must be a real inbox you can check.** Magento sends 2FA setup links, password resets, store alerts here. Unrelated to sender domain verification. |

If the envelope sender isn't verified, you get a synchronous `551
5.5.3 Domain name 'example.com' must be added, and validated, before
using it` in `/app/var/log/exception.log` and the user-facing
"Failed to send the message" error. If the admin email is invalid,
mail delivers but bounces async — you won't see a synchronous error.

## Architecture

```
 ┌────────────────────────────────────────────────────────────────┐
 │  demo-store-frontend pod (reference image)                     │
 │                                                                │
 │   php-fpm  ──►  /usr/sbin/sendmail  ──►  msmtp  ──────┐        │
 │                                                       │        │
 │   env vars from envFrom:                              ▼        │
 │     SMTP_HOST / SMTP_PORT / SMTP_USERNAME /   smtp.tem.scw.cloud│
 │     SMTP_PASSWORD / SMTP_FROM / SMTP_AUTH /    (Scaleway TEM)  │
 │     SMTP_TLS                                                   │
 │                                                                │
 │   entrypoint.sh writes /etc/msmtprc from those                 │
 │   env vars on every pod start.                                 │
 └────────────────────────────────────────────────────────────────┘
        ▲
        │ envFrom: secretRef: demo-store-smtp (optional)
        │
    ┌───────────────────────────┐
    │ Secret demo-store-smtp    │
    │ (created via SOPS — see   │
    │  internals/secrets.md)    │
    └───────────────────────────┘
```

The image's `msmtp-mta` package symlinks `/usr/sbin/sendmail` to
`msmtp`. Magento's default `Magento\Framework\Mail\Transport` uses
`sendmail`, so no Magento-side configuration change is needed once
`msmtp` is configured. `entrypoint.sh` generates `/etc/msmtprc` at
container start from the `SMTP_*` env vars; there is no hard-coded
SMTP config in the image.

The Secret is marked **optional** in `ensureWorkloads` (`optional:
true` on the envFrom). Stores that haven't set up email still start
cleanly; admin email operations will fail at dispatch time but the
storefront is unaffected.

## One-time setup

### 1. Create (or reuse) the Scaleway TEM project

1. In the Scaleway console, go to **Transactional Email** → create
   the project (one project can be shared across environments).
2. Verify the sender domain (DKIM + SPF + DMARC records on DNS).
   The domain owner publishes these once; TEM shows the exact records
   to add.
3. Create an IAM application scoped to the TEM project, generate an
   API key. The **Access Key ID** is your SMTP username; the
   **Secret Key** is your SMTP password.

Shape you end up with:

| Key           | Value shape                              |
|---------------|------------------------------------------|
| `SMTP_HOST`   | `smtp.tem.scw.cloud`                     |
| `SMTP_PORT`   | `2587` (also valid: `25` / `587` / `2525`) |
| `SMTP_USERNAME` | Scaleway project ID (UUID)              |
| `SMTP_PASSWORD` | TEM API secret key (UUID)               |
| `SMTP_FROM`   | `noreply@<verified-domain>`              |
| `SMTP_AUTH`   | `login`                                  |
| `SMTP_TLS`    | `starttls`                               |

### 2. Create the encrypted Secret

SOPS with an age key guards the credentials in git. Full workflow in
[`docs/secrets.md`](secrets.md); the short form:

```bash
# one-time
age-keygen -o age.key         # private key; gitignored
chmod 600 age.key

# per-Secret
cp config/secrets/smtp.example.yaml config/secrets/smtp.yaml
$EDITOR config/secrets/smtp.yaml    # paste real values
SOPS_AGE_KEY_FILE="$PWD/age.key" sops -e -i config/secrets/smtp.yaml
git add config/secrets/smtp.yaml
```

Commit the encrypted `smtp.yaml`; never commit the plaintext.

### 3. Apply the Secret to the cluster

```bash
SOPS_AGE_KEY_FILE="$PWD/age.key" sops -d config/secrets/smtp.yaml \
    | kubectl apply -f -
kubectl get secret demo-store-smtp
```

### 4. Roll the pod to pick up the envFrom

On a fresh install the operator already wires the envFrom; on an
existing install, trigger a pod rotation:

```bash
kubectl delete pod -l app.kubernetes.io/component=frontend
```

## First-install pitfall: default `example.com` identities

Out of the box, `bin/magento setup:install` seeds every email identity
with an `@example.com` address:

| core_config_data path                 | Default value                  |
|--------------------------------------|--------------------------------|
| `trans_email/ident_general/email`    | `owner@example.com`            |
| `trans_email/ident_sales/email`      | `sales@example.com`            |
| `trans_email/ident_support/email`    | `support@example.com`          |
| `trans_email/ident_custom1/email`    | `custom1@example.com`          |
| `trans_email/ident_custom2/email`    | `custom2@example.com`          |

Any Magento-initiated email (2FA setup, order confirmation, newsletter)
picks one of these as the envelope-from. Scaleway TEM (and every other
hygienic SMTP gateway) rejects envelope senders on unverified
domains with a synchronous 551. The user-facing symptom is **"Failed
to send the message. Please contact the administrator."** The real
error lands in `/app/var/log/exception.log`:

```
Symfony\Component\Mailer\Exception\TransportException:
  Process failed with exit code 65:
  sendmail: envelope from address owner@example.com not accepted by the server
  sendmail: server message: 551 5.5.3 Domain name 'example.com' must be added,
  and validated, before using it
```

`bootstrap-install-job.yaml` rewrites every `trans_email/ident_*/email`
to `SMTP_FROM` immediately after `setup:install` so this never surfaces
on fresh installs via that template. If you install by some other
means, run the same overrides manually:

```bash
FROM="noreply@your-verified-domain.example.com"
for ident in general sales support custom1 custom2; do
    kubectl exec <frontend-pod> -- \
        php -d memory_limit=2G /app/bin/magento config:set \
        "trans_email/ident_${ident}/email" "$FROM"
done
kubectl exec <frontend-pod> -- \
    php -d memory_limit=2G /app/bin/magento cache:clean config
```

## Verifying delivery

### Inside the pod, send a raw test message

```bash
FP=$(kubectl get pod -l app.kubernetes.io/component=frontend -o name | head -1)
kubectl exec $FP -- sh -c 'echo "Subject: smoke test

hi" | msmtp -a default you@example.com'
```

Expected output (success):

```
Apr 23 02:41:22 host=smtp.tem.scw.cloud tls=on auth=on user=<...>
from=noreply@example.com recipients=you@example.com mailsize=N
smtpstatus=250 smtpmsg='250 2.0.0 OK: queued as <uuid>' exitcode=EX_OK
```

`smtpstatus=250 ... exitcode=EX_OK` means Scaleway accepted the
message for delivery. Check the Scaleway TEM dashboard (Monitor →
Emails) for the queued record.

### Trigger a Magento admin email

Log into the admin panel. Magento 2.4's first-login flow sends a
2FA setup link via email. If `/etc/msmtprc` is configured correctly,
the admin receives the link and can proceed. If email dispatch
fails, Magento shows "Failed to send the message" and the 2FA
wall doesn't open.

### Tail msmtp logs

`msmtp` writes to `/dev/stderr` inside the container; Kubernetes
collects those:

```bash
kubectl logs -l app.kubernetes.io/component=frontend --tail=50 | grep -iE 'smtp|msmtp|mail'
```

## Rotation

When the TEM API key is compromised or a team member leaves:

1. In Scaleway, regenerate the secret key on the existing IAM
   application (or create a new application + delete the old).
2. Edit the encrypted Secret:
   `SOPS_AGE_KEY_FILE="$PWD/age.key" sops config/secrets/smtp.yaml`.
3. Save; SOPS re-encrypts on exit.
4. Reapply:
   `sops -d config/secrets/smtp.yaml | kubectl apply -f -`.
5. `kubectl delete pod -l app.kubernetes.io/component=frontend` to
   pick up the new values.

The sender domain (`noreply@example.com`) and the IAM application
identity don't change on a key rotation; only the password field.

## Production differences

For real customer deployments:

- Use a per-customer Scaleway project (or their own SES/SendGrid/
  Postmark/etc. account). Don't share one TEM project across
  unrelated customers — bounce and spam reputation is per-sender.
- `msmtp` is fine for low-volume transactional email (password
  resets, 2FA, order confirmations at a few per minute). For higher
  volume, route through a dedicated MTA (Postfix with a smarthost
  configuration), or have Magento use a proper SMTP extension that
  talks directly to the provider's API.
- TLS trust — the reference image ships with Debian's
  `ca-certificates` bundle. Customers with private CAs need to COPY
  their root into `/etc/ssl/certs/` in their fork.
- Bounce / complaint handling — TEM publishes webhooks; route these
  to whatever monitoring system handles deliverability. None of that
  is operator-level concern.

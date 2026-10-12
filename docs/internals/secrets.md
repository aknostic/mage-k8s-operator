# Secrets management with SOPS + age

This repository uses [SOPS](https://github.com/getsops/sops) with
[age](https://github.com/FiloSottile/age) keys to keep secrets encrypted
in git. The pattern is minimal, single-cluster-friendly, and lives
entirely outside the operator — SOPS encrypts the `data:` and
`stringData:` fields of Kubernetes `Secret` manifests before commit;
the cluster receives plain `Secret` resources at apply time via a
GitOps tool (Flux / ArgoCD) or via a `sops -d ... | kubectl apply` for
direct pushes.

The operator itself never sees encrypted material. It consumes
`Secret` refs on CRs (see `spec.database.external.credentialsSecretRef`,
the per-store `<store>-smtp` Secret, planned
`spec.email.smtpSecretRef`), and those Secrets are populated by
whatever deploy pipeline owns them.

## Scope

What SOPS is used for here:

- Secret manifests committed under `config/secrets/` that ship example
  deployments (SMTP credentials, third-party API keys, external-auth
  tokens).
- Helm chart default-values overrides that contain credentials.

What SOPS is **not** used for:

- `cosign` private signing keys — those stay in GitLab CI/CD variables
  per `.gitlab-ci.yml`, not in git at all.
- Credentials generated at runtime by Kubernetes operators (e.g.
  mariadb-operator's root password). Those live in their own K8s
  Secrets, were never in git, and are outside this policy.
- Per-deployment environment-specific secrets (dev/acc/prod). Those
  belong in the deployment overlay, not the base repo — set up a
  `config/overlays/<env>/secrets/` directory when multi-environment
  support lands.

## One-time setup

```bash
# Install tooling (macOS)
brew install sops age

# Generate an age key; keep it at the repo root (gitignored)
age-keygen -o age.key
chmod 600 age.key
```

The public key (the `age1...` line in `age.key`) goes in the repo's
`.sops.yaml` as a recipient. The private key stays on your machine;
never commit it, never paste it into chat.

For CI, set `SOPS_AGE_KEY` as a protected variable (paste the full
`AGE-SECRET-KEY-1...` line as the value). Don't reuse the developer
key — generate a CI-specific one and add its public key to the
`.sops.yaml` recipients list.

## Daily flow

### Create a new encrypted Secret

```bash
# Start from the template
cp config/secrets/smtp.example.yaml config/secrets/smtp.yaml

# Edit with real values
$EDITOR config/secrets/smtp.yaml

# Encrypt in place
SOPS_AGE_KEY_FILE=$PWD/age.key sops -e -i config/secrets/smtp.yaml

# Commit the encrypted file
git add config/secrets/smtp.yaml && git commit -m "add smtp secret (encrypted)"
```

After `sops -e -i`, the file is still valid YAML (same keys, same
structure) but every value under `data:` / `stringData:` is replaced
with `ENC[AES256_GCM,data:...,tag:...]` blobs. The file is safe to
commit.

### Edit an existing encrypted Secret

```bash
SOPS_AGE_KEY_FILE=$PWD/age.key sops config/secrets/smtp.yaml
```

SOPS decrypts into your `$EDITOR`, waits for you to save, and
re-encrypts on exit. Don't try to hand-edit the encrypted fields.

### Decrypt and apply to the cluster

```bash
SOPS_AGE_KEY_FILE=$PWD/age.key sops -d config/secrets/smtp.yaml | kubectl apply -f -
```

For automated flows, Flux's `kustomize-controller` has native SOPS
support — you supply the age key as a cluster-side Secret and Flux
decrypts on sync. That's the production shape; the `sops -d | apply`
above is fine for dev/manual deploys.

### Rotate a value

```bash
SOPS_AGE_KEY_FILE=$PWD/age.key sops config/secrets/smtp.yaml  # edit, save
# re-apply to cluster as above
```

### Rotate the encryption key (team-member leaves, key compromise)

1. Generate a new keypair: `age-keygen -o age.key.new`.
2. Replace the `age:` line in `.sops.yaml` with the new public key.
3. Re-encrypt every existing file:
   `SOPS_AGE_KEY_FILE=$PWD/age.key sops updatekeys config/secrets/*.yaml`.
4. Swap `age.key.new` → `age.key` for local use.
5. Distribute the new key to remaining team members via an
   out-of-band channel (1Password, signed message, in person).

Note that rotating the encryption key does not rotate the underlying
Secret values — anyone who had the old key has seen the plaintext. If
a key is compromised, rotate both: regenerate SMTP passwords, API
tokens, etc., update the Secrets, re-encrypt.

## Why age, not PGP or KMS?

- **age** requires no infrastructure and one binary. Matches the
  single-kind-cluster dev shape we have today.
- **PGP** works but requires a keyring and is painful for new
  contributors. Drop in if the team already runs GnuPG.
- **AWS KMS / GCP KMS / Azure Key Vault** is the production shape
  when the cluster is in one of those clouds — the operator pod gets
  IAM-bound decryption and you don't distribute any private key.
  Revisit this when the first paid customer's environment is known.

## What gets committed vs. what stays local

| File                                | In git?                          | Why                                                                |
|-------------------------------------|----------------------------------|--------------------------------------------------------------------|
| `.sops.yaml`                        | Yes                              | Public recipient list; safe.                                       |
| `age.key`                           | **No** (gitignored)              | Private key.                                                       |
| `config/secrets/*.yaml` (encrypted) | Yes                              | Encrypted values are safe in git.                                  |
| `config/secrets/*.example.yaml`     | Yes                              | Templates with placeholder values. Not encrypted; not real creds.  |
| CI's copy of the age key            | **No** (CI variable)             | Stored in the CI platform's secret store, loaded at pipeline time. |

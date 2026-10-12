# ADR 003 — `MAGENTO_CRYPT_KEY` emission format: `base64<base64>`

- **Status:** Accepted
- **Date:** 2026-04-22
- **Deciders:** @jurg
- **Related:** ADR 002 (config emission modes). Supersedes the pre-spike
  assumption in `internal/controller/ensure_secrets.go` that emitting a
  raw 44-char base64 string was sufficient.

## Context

An early spike stood up a Mage-OS 2.4.8-p4 container
against the operator's envvars-mode Secret. `setup:install` completed
successfully against the operator's `MAGENTO_DATABASE_*` keys, but the
first `/admin` request returned HTTP 500 with:

```
sodium_crypto_aead_chacha20poly1305_ietf_encrypt():
Argument #4 ($key) must be SODIUM_CRYPTO_AEAD_CHACHA20POLY1305_IETF_KEYBYTES bytes long
```

Magento 2.4.x's encryption pipeline (`Magento\Framework\Encryption\Encryptor`)
uses libsodium `chacha20-poly1305-ietf`, which requires a 32-byte raw key.
The operator's pre-ADR emission was 32 random bytes **base64-encoded** (44
ASCII chars). When `env.php` passed that value verbatim to `crypt.key`,
Magento read 44 bytes where it expected 32 and the encryption function
bailed.

Reading `Encryptor::decodeKey` in `vendor/magento/framework/Encryption/Encryptor.php`:

```php
private function decodeKey(string $key) : string|bool
{
    return (str_starts_with($key, ConfigOptionsListConstants::STORE_KEY_ENCODED_RANDOM_STRING_PREFIX)) ?
        base64_decode(substr($key, strlen(ConfigOptionsListConstants::STORE_KEY_ENCODED_RANDOM_STRING_PREFIX))) :
        $key;
}
```

with

```php
public const STORE_KEY_ENCODED_RANDOM_STRING_PREFIX = 'base64';
```

Magento recognises two key shapes:
1. `base64<base64_string>` → literal prefix `"base64"` (no colon, no separator) followed by a base64-encoded random key; Magento strips the prefix and base64-decodes the remainder.
2. Anything else → used as raw bytes.

## Decision

**The operator emits `MAGENTO_CRYPT_KEY` as `base64<base64-of-32-random-bytes>`.**

```
MAGENTO_CRYPT_KEY=base64<output of: openssl rand -base64 32>
```

The customer's `env.php` uses this value verbatim:

```php
'crypt' => ['key' => getenv('MAGENTO_CRYPT_KEY')],
```

No decoding, no prefixing, no format juggling in the image. Magento's
`Encryptor::decodeKey` recognises the `"base64"` prefix, decodes the
remainder, and has the 32 raw bytes libsodium needs.

### Rotation and preservation

`ensureSecrets` preserves the existing value across reconciles —
regenerating the key silently would invalidate any data already
encrypted with the old one. Pre-ADR Secrets with an unprefixed 44-char
base64 value are auto-migrated on next reconcile by prepending
`"base64"`; the random bytes are unchanged, so nothing encrypted with
the legacy value becomes unreadable.

An intermediate `"7:"` prefix appeared briefly in an early draft of
this ADR (misreading Magento's encrypted-value cipher-version marker as
the key-format marker). Secrets carrying that prefix are auto-migrated
by stripping the `"7:"` and prepending `"base64"` — still the same
underlying random bytes.

## Consequences

**Positive**
- `env.php` stays simple and stable: `getenv('MAGENTO_CRYPT_KEY')` is
  the entire integration point.
- Matches what `setup:install` and `bin/magento encryption:key:change`
  generate when Magento produces its own keys. Operator-emitted and
  Magento-rotated values are indistinguishable in `env.php`.
- Leverages a Magento-internal convention that's been stable since
  Magento 2.0 (the `STORE_KEY_ENCODED_RANDOM_STRING_PREFIX` constant).

**Negative**
- The operator is coupled to this specific Magento constant. If Adobe
  ever renames `STORE_KEY_ENCODED_RANDOM_STRING_PREFIX` away from
  `"base64"`, the operator must follow. One-line change in
  `newCryptKey`, but it's a change.
- Customers migrating from a pre-ADR build see their existing Secret
  auto-migrate on next reconcile. If a customer has hand-hacked
  `env.php` to `base64_decode` the value, their `env.php` breaks when
  the prefix lands — the decoded bytes will include the literal
  `"base64"` as noise. Mitigation: the image-contract docs ship the
  canonical snippet and explicitly call out "do not decode."

**Neutral**
- Secret size grows by 6 bytes per store. Irrelevant.

## Alternatives considered

1. **Emit raw 32 bytes (base64-encoded in transit), let `env.php`
   prefix `"base64"` itself.** Cleaner separation — operator provisions
   a byte blob, image decides the format. Rejected because every
   customer's `env.php` then knows about the prefix and must change if
   Magento ever renames it. The operator is Magento-specific by
   definition; owning the format is not an abstraction break.

2. **Emit hex-32 (64 ASCII chars).** Legacy Magento-1-style keys. Still
   accepted by Magento 2.4.x's `Encryptor` because `decodeKey` falls
   through to the "used as raw" branch for unknown shapes. Rejected
   because this path relies on the raw-bytes fallback continuing to
   exist — not a documented contract, and the key would be interpreted
   as 64 raw bytes (wrong length for sodium). Fragile.

3. **Emit an unprefixed 44-char base64 string and document the
   `base64_decode` requirement in `env.php`.** Minimal operator change.
   Rejected because decoded-32-bytes-then-feed-to-sodium only works if
   every customer's `env.php` knows to decode, and it breaks the
   "operator owns the format" framing — the image becomes responsible
   for interpreting operator-emitted secrets.

## Image-contract implications

The canonical `env.php` snippet in `docs/image-contract.md` sets:

```php
'crypt' => ['key' => getenv('MAGENTO_CRYPT_KEY')],
```

No `base64_decode`. No `bin2hex`. No prefix handling. The operator owns
the format.

## Open questions

- **Key rotation via operator.** `bin/magento encryption:key:change`
  writes a new `base64<…>` key to env.php — but our env.php reads from
  env vars, not from a static string in the PHP file. An operator-driven
  rotation would have to update the Secret (new `"base64"` value), then
  trigger a re-encrypt of any data that used the old key. This is a
  separate ADR when the feature lands.
- **External Secrets Operator mode** (planned per CLAUDE.md). An
  `ExternalSecret` backed by Vault stores the same `base64<…>` value
  verbatim; no special handling. Documented alongside `ensureSecrets`
  when ESO support lands.

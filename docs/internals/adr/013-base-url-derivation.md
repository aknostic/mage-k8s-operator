# ADR 013 — Base-URL derivation

- **Status:** Accepted
- **Date:** 2026-05-04
- **Deciders:** @jurg
- **Related:** closes gap #1 of the v0.6.0 real-infra gap list;
  builds on `spec.stores` (ADR 007 — multi-store mage-run-code
  mapping) and `spec.routing.tls` (ADR 010 — public-edge TLS);
  extends the bootstrap script defined in ADR 005.

## Amendment (2026-10-15)

Without TLS the bootstrap script no longer passes
`--base-url-secure`. Magento's `setup:install` rejects any value for
it that isn't `https://` ("you should use only following schemes:
'https'"), even with `--use-secure=0`, so the "No TLS → both URLs
`http://`" row below made every install without TLS fail. Magento
falls back to `--base-url` for the secure URL. The derivation itself
is unchanged: the store-URLs Job still sets `web/secure/base_url` to
the `http://` URL.

## Context

`internal/controller/ensure_bootstrap.go:71` hardcodes
`--base-url=http://localhost:8080/` on `bin/magento setup:install`.
The operator emits no `--base-url-secure`, no `--use-secure`, no
`--use-secure-admin`, and never reads `spec.stores` or
`spec.routing.tls`.

Consequence: every freshly-bootstrapped Magento DB has
`web/{secure,unsecure}/base_url = http://localhost:8080/` baked in.
After bootstrap completes, the storefront serves at the user's real
DNS hostname (via Gateway routing — ADR 006/010) but Magento
generates URLs pointing at `localhost:8080` for every internal link,
redirect, sitemap entry, and email. Users hit
`Location: http://localhost:8080/customer/account/login` redirects
on login; password-reset emails carry localhost links; sitemaps are
unusable.

This is gap #1 from the 2026-05-04 Kapsule demo. The fix has three
independent dimensions:

1. **Default-scope URL derivation.** `setup:install` needs
   `--base-url` and `--base-url-secure` derived from
   `spec.stores[0].host` (or localhost fallback) plus
   `spec.routing.tls` presence.
2. **Per-website / per-store-view URL overrides.** Multi-store
   setups (`spec.stores[1:]`) need per-scope `config:set` of the
   four URL keys (`web/{secure,unsecure}/base_{url,link_url}`) at
   `--scope=websites` or `--scope=stores` (Magento's vocabulary —
   "stores" means store views).
3. **Auto-heal on upgrade.** Existing v0.5.x deployments with
   `localhost:8080` baked into Magento's DB need to be fixed when
   the operator is upgraded to v0.6.0, without forcing manual
   `kubectl exec` from the user.

A perfect-operator design also keeps the URL config aligned with
spec edits made AFTER the initial bootstrap. Today's pattern (URLs
written once at setup:install time) is brittle — every subsequent
spec edit affecting URLs would force the user into manual
intervention.

## Decision

Two-phase. Phase 1: `ensureBootstrap`'s script gets the URL matrix
derivation, replacing the hardcoded `localhost:8080`. Phase 2: a new
`ensureStoreURLs` reconcile step gates on `BootstrapReady=True` and
idempotently runs per-store `config:set` via a one-shot Job,
re-triggered when a hash of `spec.stores + spec.routing.tls`
changes. New `URLsApplied` status condition surfaces the result.

The two-phase shape is deliberate. Inline-only doesn't auto-heal
v0.5.x (bootstrap is sticky-once-True, doesn't re-run);
reconcile-only would force `setup:install` to keep its
`localhost:8080` placeholder and rely on the reconcile step to fix
it post-install — which leaves a window between install completion
and URL-fix where Magento serves with wrong URLs. Two-phase fixes
URLs at install time AND on every subsequent stores/tls edit, and
the reconcile step also auto-heals v0.5.x deployments on first
reconcile post-upgrade.

**No CRD shape change.** ADR 013 uses existing fields —
`spec.stores` (introduced earlier per ADR 007) and
`spec.routing.tls` (added in v0.5.0 per ADR 010). The ADR is purely
about reconciler behaviour. Tighter scope; smaller test surface;
no migration concerns for existing CRs.

### URL derivation matrix at default scope

`setup:install` arguments derived from the two spec inputs:

| `spec.stores` | `spec.routing.tls` | `--base-url` | `--base-url-secure` | `--use-secure` | `--use-secure-admin` |
|---|---|---|---|---|---|
| unset / empty | unset | `http://localhost:8080/` | `http://localhost:8080/` | 0 | 0 |
| unset / empty | set | `http://localhost:8080/` | `https://localhost:8080/` | 1 | 1 |
| set | unset | `http://<stores[0].host>/` | `http://<stores[0].host>/` | 0 | 0 |
| set | set | `https://<stores[0].host>/` | `https://<stores[0].host>/` | 1 | 1 |

Reasoning per row:

- **Row 1 (back-compat).** Unchanged from current state. Existing
  v0.5.x deployments with no `spec.stores` match this; their
  behaviour is preserved.
- **Row 2 (pathological — TLS without public hostname).** Operator
  emits the URL but the cert won't validate against `localhost`.
  User's responsibility to either add stores or unset TLS. Operator
  emits a Warning event; doesn't error.
- **Row 3 (real hostname, no TLS).** HTTP-only public site.
  Magento's docs explicitly recommend matching `base_url` and
  `base_url_secure` when no SSL — keeps Magento's link-generation
  logic from emitting a mix of schemes.
- **Row 4 (real hostname, TLS).** HTTPS-only public site. Both URLs
  `https://`; `--use-secure` flags flip so Magento renders `https://`
  consistently in storefront and admin.

Two design choices baked into the matrix:

- **`--use-secure-admin=1` whenever TLS is configured** (rows 2+4).
  Without it, the admin login page redirects to HTTP if accessed
  via HTTP — broken when only HTTPS is reachable through the
  Gateway. The kubectl-port-forward-to-internal-HTTP use case is
  rare and the user can flip `web/secure/use_in_adminhtml` to 0
  via `config:set` post-install if needed.
- **No port suffix for real hostnames** (rows 3+4). Gateway
  terminates 80/443 at public edge; pods listen on 8080 internally
  but URLs reflect the public-edge surface. Localhost fallback
  keeps `:8080` (no Gateway in front for that case).

`spec.stores: []` (empty array, not nil) is treated identically to
unset for the matrix — both signal "no public hostname".

### Per-store override mechanics

For each `spec.stores[i]` whose `Type` maps to a Magento
URL-override scope, `ensureStoreURLs` sets four config keys:

| Config key | Value (TLS set) | Value (TLS unset) |
|---|---|---|
| `web/secure/base_url` | `https://<host>/` | `http://<host>/` |
| `web/unsecure/base_url` | `https://<host>/` | `http://<host>/` |
| `web/secure/base_link_url` | `https://<host>/` | `http://<host>/` |
| `web/unsecure/base_link_url` | `https://<host>/` | `http://<host>/` |

`base_static_url` and `base_media_url` are deliberately not set
per-store. Magento's `{{base_url}}` placeholder resolves
per-active-scope at request time, so a per-website `base_url`
override cascades to static/media URLs naturally. This also keeps
the door open for users running CDN setups (their `base_static_url`
points at a separate CDN domain at default or website scope, and the
operator stays out of CDN territory).

`StoreSpec.Type` → Magento `--scope=` mapping:

| `StoreSpec.Type` | `config:set --scope=` | Behaviour |
|---|---|---|
| `website` | `websites` | Emit four `config:set` commands per store. |
| `store_view` | `stores` | Same; `stores` is Magento's confusingly-named scope for store views. |
| `store` (store group) | — | **Skip URL overrides.** Magento has no URL config level for store groups. Entry still drives `MAGE_RUN_CODE` mapping per ADR 007. |

Per-website `web/secure/use_in_{frontend,adminhtml}` flags are NOT
set by `ensureStoreURLs`. They're set only at default scope by
`setup:install` per the URL matrix. Per-website override of these
flags is technically supported by Magento for the rare mixed-HTTP-
HTTPS multi-site setup but is not exposed in v0.6.0. Future work if
a user requests it.

### Reconcile graph

```
ensureDatabase    → DatabaseReady
ensureSearch      → SearchReady
ensureCache       → CacheReady
ensureQueue       → QueueReady
ensureMedia       → MediaEndpoint
ensureSecrets     → config Secret
ensureSMTP        → SMTPReady
ensureBootstrap   → BootstrapReady
ensureMediaSeed   → MediaSeeded
ensureStoreURLs   → URLsApplied (new)            ← here
ensureWorkloads   → WorkloadsReady
ensureVarnish     → VarnishReady
ensureConsumers   → ConsumersReady
ensureCron        → CronReady
```

`ensureStoreURLs` runs after `ensureBootstrap` (needs a populated
DB) and **does not gate `ensureWorkloads`**. Workloads can come up
against slightly-stale URL config — Magento serves traffic correctly
via nginx routing regardless of what's in
`web/{secure,unsecure}/base_url`; the URL config affects only what
Magento *generates* (links, redirects, emails), not what nginx
routes. Stale URLs are a UX issue, not an availability issue, so
blocking pod startup on URL application would be over-cautious.

`ensureMediaSeed` (ADR 011) and `ensureStoreURLs` are siblings —
neither blocks the other. Both gate on `BootstrapReady=True`.

### Job spec — `<store>-store-urls`

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: <store>-store-urls
  ownerReferences: [<MageStore>]
  annotations:
    mage.aknostic.com/urls-hash: <sha256 of stores+tls inputs>
spec:
  backoffLimit: 1                    # 2 total attempts
  activeDeadlineSeconds: 300         # 5 min — DI bootstrap + N config:sets
  ttlSecondsAfterFinished: 600       # auto-clean 10 min after success
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: store-urls
          image: <workload image>
          envFrom:
            - secretRef: { name: <store>-config }    # DB + cache + search + queue
          command: ["sh", "-c"]
          args:
            - |
              set -eu
              echo "== applying per-store URL overrides =="
              # Default scope assertion (auto-heal for v0.5.x deployments)
              php -d memory_limit=2G bin/magento config:set \
                web/unsecure/base_url "<derived-default-url>"
              php -d memory_limit=2G bin/magento config:set \
                web/secure/base_url "<derived-default-secure-url>"
              echo "  default scope → <derived-default-url>"
              # Per-store overrides — operator-rendered, see below.
              # ... emit one block per stores[i] of type website / store_view ...
              # ... emit echo line per skipped store-group entry ...
              php -d memory_limit=2G bin/magento cache:flush
```

The script body is **operator-rendered** at Job-build time — the
operator iterates `spec.stores[]`, emits one `config:set` block per
applicable entry, and embeds the values into the bash. No `eval`,
no env-var indirection at runtime. Each per-store block looks like:

```sh
# website shop-eu
php -d memory_limit=2G bin/magento config:set \
  --scope=websites --scope-code=shop-eu \
  web/secure/base_url "https://shop.example.de/"
php -d memory_limit=2G bin/magento config:set \
  --scope=websites --scope-code=shop-eu \
  web/unsecure/base_url "https://shop.example.de/"
php -d memory_limit=2G bin/magento config:set \
  --scope=websites --scope-code=shop-eu \
  web/secure/base_link_url "https://shop.example.de/"
php -d memory_limit=2G bin/magento config:set \
  --scope=websites --scope-code=shop-eu \
  web/unsecure/base_link_url "https://shop.example.de/"
echo "  website shop-eu → https://shop.example.de/"
```

Skipped store-group entries get an echo line:
```sh
echo "  skipped store-group shop-emea-group (Magento has no URL override level for groups)"
```

### `cache:flush`, not `cache:clean config` (load-bearing)

The script's final command is `bin/magento cache:flush` — a **full
flush** of all Magento cache types. Not `cache:clean config`
(config-cache only).

Reason: Magento stores URL-substituted HTML in `full_page_cache`
(Varnish-tier or db-backed) and `block_html` (per-block cache).
After a URL change, both still contain pre-fix rendered HTML with
the old URLs. Storefront keeps rendering `localhost:8080` links
from cache for the entire FPC TTL (default 24h) even though the DB
is correct. Without `cache:flush`, `URLsApplied=True` is a
misleading signal — DB correct but user-visible storefront still
wrong for hours.

`cache:flush` causes a brief regeneration cost on the next request
set (everything regenerates). Acceptable — URL changes are rare,
and the operator already accepts the same impact for SMTP / media
seeding.

Two cache layers escape the operator and are documented in the
diagnostic snippet:
- **Varnish FPC** when `spec.varnish` is configured — outside
  Magento's `cache:flush`, requires `varnishadm ban .` separately.
- **Browser / CDN cache** — out of operator scope; user's
  responsibility to clear.

### Hash-driven re-run

`urls-hash` annotation on the Job is the operator's idempotency
gate:

- **Hash inputs:** sha256 of `(stores[].host, stores[].code,
  stores[].type, tls != nil)`. Fields that affect URL derivation;
  nothing else as of v0.6.0. TLS is reduced to a boolean
  (`set | unset`) — issuer choice doesn't affect URL scheme today.
  Future ADRs that make issuer choice URL-relevant (e.g., a staging
  cert that maps to a different URL prefix) would need to extend
  this hash; the implementation should note "v0.6.0 hash shape" in
  a code comment near the hash function so the extension point is
  obvious.
- **On reconcile:**
  1. Compute `current_hash` from spec.
  2. `Get` Job `<store>-store-urls`.
  3. If Job exists and
     `Job.annotations["mage.aknostic.com/urls-hash"] == current_hash`:
     read Job status, set `URLsApplied` accordingly. Done.
  4. If Job exists and hash mismatches: delete Job, fall through to
     step 5.
  5. If Job doesn't exist: create Job with `current_hash`
     annotation, set `URLsApplied=Unknown, Reason=Applying`.

Same idempotency pattern as ADR 012's SMTP probe Job (which uses
`smtp-secret-hash`). Consistent operator-wide.

### `URLsApplied` state machine

| Spec state | Condition |
|---|---|
| `BootstrapReady=Unknown` (`spec.bootstrap` unset, adopted DB) | omitted — operator does not modify URLs in the adoption case |
| `BootstrapReady=False` (bootstrap in progress / failed) | omitted — gates on bootstrap success |
| `BootstrapReady=True`, hash unchanged, prior Job Succeeded | `True, Reason=Applied` (sticky for the same hash) |
| `BootstrapReady=True`, hash changed OR no prior Job | `Unknown, Reason=Applying` while Job runs |
| Job Succeeded | `True, Reason=Applied, Message="<derived enumeration>"` |
| Job Failed (backoff exhausted) | `False, Reason=ApplyFailed, Message="<last error>"` |

`URLsApplied=True` is sticky **for the same hash**. A spec change
that flips the hash automatically un-stickies it (operator deletes
the old Job and creates a new one). No manual intervention to
re-trigger.

The condition's `Message` field enumerates what was applied AND
what was skipped, with reasons:

```
URLsApplied=True, Reason=Applied,
Message="default scope: https://primary.example.com/;
         websites: shop-eu, shop-us;
         store-views: shop-fr-fr;
         skipped (store-group entries — Magento has no URL override level for groups): shop-emea-group"
```

A single `kubectl get magestore <name> -o jsonpath='{.status.conditions[?(@.type=="URLsApplied")].message}'`
shows the full state.

## Auto-heal on upgrade

This is the killer property of the two-phase shape. Concrete
sequence:

```
Day 0 (v0.5.x): user installs operator + applies MageStore with
                spec.stores=[{host: shop.example.com, ...}]. Bootstrap
                runs with --base-url=http://localhost:8080/. Magento
                DB has web/{secure,unsecure}/base_url=http://localhost:8080/.
                Storefront serves at shop.example.com but generates
                broken localhost links throughout.

Day N (upgrade): user runs `helm upgrade` to operator v0.6.0.
                 Operator's first reconcile pass against the existing
                 MageStore observes BootstrapReady=True (already set;
                 sticky). ensureStoreURLs computes urls-hash from
                 spec.stores + spec.routing.tls; no prior
                 <store>-store-urls Job exists; creates the Job with
                 the computed hash. Job runs, applies derived URLs at
                 default + per-website scope, runs cache:flush.
                 URLsApplied=True.

Day N+1: storefront generates correct https://shop.example.com/
         links throughout. No user intervention required.
```

The auto-heal works because:
- `ensureBootstrap` is idempotent on the condition side —
  `BootstrapReady=True` is sticky, the operator doesn't re-run
  `setup:install` (which would be destructive).
- `ensureStoreURLs` doesn't depend on bootstrap being a recent event
  — it only depends on the DB being populated (which
  `BootstrapReady=True` implies).
- Magento's `config:set` is idempotent at the DB level — applying
  URLs that match what's already there is a no-op write.
- `cache:flush` ensures the user-visible storefront catches up to
  the DB on next request.

## Error handling

### Job fails with DI bootstrap error

DB unreachable, Redis down: probably means a dependency regression
that should also have flipped `DatabaseReady` / `CacheReady` —
operator surfaces both. `URLsApplied=False, Reason=ApplyFailed`
adds a redundant signal but doesn't lie about state. User fixes
underlying dep; URL Job retries on next hash-flip or manual re-run.

### Job fails with `config:set` error for an invalid scope-code

User typo'd `stores[i].code` to a value that doesn't match any
Magento website scope. `bin/magento config:set --scope=websites
--scope-code=foo` errors with "Wrong scope-code 'foo'". Job exits
non-zero; `URLsApplied=False, Reason=ApplyFailed, Message` carries
the bin/magento error. User fixes the typo (Magento UI or
out-of-band CR edit creates the missing scope) → hash flip → Job
re-runs.

### `spec.stores[]` reordered without content change

Hash-input is order-sensitive. Re-run happens. Idempotent at DB
level — no harm.

### `spec.stores[]` host edited

Hash flips → Job re-runs → URL config moves to new host. **One
missing piece:** Magento doesn't auto-clear cookies / sessions on
URL change. Users with active sessions may see redirects or 4xx for
~one session lifetime. Documented as a known wrinkle; out of
operator scope.

### `spec.routing.tls` flipped from set → unset (downgrade to HTTP)

Hash flips → Job re-runs with HTTP URLs. `--use-secure=0` is NOT
touched by `ensureStoreURLs` — it's only set by `setup:install` per
the URL matrix. Caveat: Magento's existing `web/secure/use_in_frontend=1`
from the previous TLS-enabled bootstrap remains, so storefront
still tries to redirect to HTTPS.

**Documented limitation:** flipping TLS off post-install requires
the user to manually run:
```
bin/magento config:set web/secure/use_in_frontend 0
bin/magento config:set web/secure/use_in_adminhtml 0
bin/magento cache:flush
```

`ensureStoreURLs` is not the right step to manage these flags
(they're install-time-set per the URL matrix). Future work: a
separate `ensureSecureFlags` reconcile step or expansion of
ensureStoreURLs's surface — defer until a user actually downgrades
TLS post-install.

### Adoption case (`spec.bootstrap` unset)

`BootstrapReady=Unknown`. `ensureStoreURLs` does NOT run.
`URLsApplied` stays absent. User who adopted a pre-bootstrapped DB
manages URLs themselves out-of-band. The operator never
auto-modifies a pre-existing Magento config in the adoption case —
same conservative stance as `ensureBootstrap`.

### Spec-vs-DB drift (manual admin-UI edits)

A user manually edits URLs in the Magento admin UI; spec stays the
same; hash unchanged; `ensureStoreURLs` doesn't re-run; operator's
`URLsApplied=True` doesn't reflect the manual edit's reality. The
drift causes "why is the operator lying?" debugging.

Resolution: `kubectl delete job/<store>-store-urls` forces the
operator to re-create the Job on next reconcile (since the Job no
longer exists, the hash-existence check trips), re-asserting URLs
from spec. Documented in the diagnostic snippet.

The operator does not detect drift autonomously. Reading Magento DB
state on every reconcile (running `bin/magento config:show` per
URL key) would be heavyweight; the operator stays declarative —
spec is the source of truth, the operator writes the spec, manual
DB edits are user error to be resolved by re-applying spec.

### `spec.stores` set with duplicate hosts across two entries

Operator emits both `config:set` commands; the second wins.
Magento's UI would also accept this (overlapping scopes are
user-error territory). No defensive validation in the operator —
user fixes their CR.

### Empty `spec.stores: []` vs. unset

Array exists but has zero entries. Operator falls back to localhost
in the URL matrix (matches `unset` row 1/2). `ensureStoreURLs` then
has no per-store overrides to apply — Job runs only the
default-scope assertion (auto-heal localhost). `URLsApplied=True,
Message="default scope: http://localhost:8080/; no stores defined"`.

## Diagnostic snippet

For `docs/internals/troubleshooting.md` (or operator README):

```bash
# Inspect URLsApplied condition
kubectl get magestore <name> \
  -o jsonpath='{.status.conditions[?(@.type=="URLsApplied")]}{"\n"}'

# View the applied URL set (default + per-store + skipped)
kubectl get magestore <name> \
  -o jsonpath='{.status.conditions[?(@.type=="URLsApplied")].message}'

# View the Job's per-store echo
kubectl logs job/<store>-store-urls | grep "→\|skipped"

# Force re-application (e.g., after manually editing Magento URLs
# in admin and wanting to re-assert from spec)
kubectl delete job/<store>-store-urls
# Operator recreates on next reconcile.

# Compare current spec-derived URL with actual Magento DB state
spec_host=$(kubectl get magestore <name> \
  -o jsonpath='{.spec.stores[0].host}')
db_url=$(kubectl exec deploy/<store>-frontend -- \
  php /app/bin/magento config:show web/unsecure/base_url 2>/dev/null)
echo "spec primary host: ${spec_host:-<unset, expects localhost:8080>}"
echo "db base_url:       $db_url"
# If mismatched: kubectl delete job/<store>-store-urls

# What hash does the operator's last successful Job have?
kubectl get job/<store>-store-urls \
  -o jsonpath='{.metadata.annotations.mage\.aknostic\.com/urls-hash}{"\n"}'
```

**If `URLsApplied=True` but storefront still renders old URLs:**

```bash
# 1. Force-flush all Magento caches (the Job does this on URL change,
#    but useful for ad-hoc debugging or after manual config:set):
kubectl exec deploy/<store>-frontend -- php /app/bin/magento cache:flush

# 2. Ban the Varnish cache if spec.varnish is configured (Varnish FPC
#    sits outside Magento's cache:flush):
kubectl exec deploy/<store>-varnish -- varnishadm ban "obj.status >= 0"

# 3. Check browser/CDN cache (out of operator scope) — hard-refresh
#    or curl with -H "Cache-Control: no-cache" to bypass.
```

## Testing

### Envtest unit cases

Location: `internal/controller/ensure_store_urls_test.go` plus
amendments to `ensure_bootstrap_test.go` for the URL-matrix
derivation in `setup:install` args.

URL matrix derivation (in `ensureBootstrap`):
- `spec.stores` unset, `spec.routing.tls` unset → bootstrap Job's
  `setup:install` args contain `--base-url=http://localhost:8080/`,
  `--base-url-secure=http://localhost:8080/`, `--use-secure=0`,
  `--use-secure-admin=0`.
- `spec.stores` unset, `spec.routing.tls` set →
  `--base-url=http://localhost:8080/`,
  `--base-url-secure=https://localhost:8080/`, `--use-secure=1`,
  `--use-secure-admin=1`.
- `spec.stores=[{host: shop.example.com}]`, `spec.routing.tls`
  unset → `--base-url=http://shop.example.com/`,
  `--base-url-secure=http://shop.example.com/`, `--use-secure=0`,
  `--use-secure-admin=0`.
- `spec.stores=[{host: shop.example.com}]`, `spec.routing.tls`
  set → `--base-url=https://shop.example.com/`,
  `--base-url-secure=https://shop.example.com/`, `--use-secure=1`,
  `--use-secure-admin=1`.
- `spec.stores=[]` (empty array, not nil) → same outputs as unset
  (back-compat, both treated as "no public hostname").

`ensureStoreURLs` Job + condition logic:
- `BootstrapReady=Unknown` (adoption case) → no Job created;
  `URLsApplied` condition absent.
- `BootstrapReady=False` → no Job created; `URLsApplied` condition
  absent.
- `BootstrapReady=True`, no prior Job, single-store inline → Job
  created with computed `urls-hash` annotation; rendered script
  contains exactly two default-scope `config:set` lines (unsecure
  + secure) + zero per-website blocks; `URLsApplied=Unknown,
  Reason=Applying`.
- Multi-store fixture (`stores=[{shop-eu, website},
  {shop-us, website}, {shop-fr-fr, store_view},
  {shop-emea, store}]`) → script contains: 2 default-scope lines +
  4 per-store-block × 3 applicable stores (12 lines) + 1 echo for
  the `store` group entry. The `store` group entry contributes no
  `config:set` commands.
- Job `status.succeeded=1` → `URLsApplied=True, Reason=Applied`.
  Message string is asserted to contain: `default scope: ...`,
  `websites: shop-eu, shop-us`, `store-views: shop-fr-fr`,
  `skipped (store-group ...): shop-emea`.
- Job `status.failed=backoffLimit` → `URLsApplied=False,
  Reason=ApplyFailed`.
- Hash-flip regression: reconcile twice with `spec.stores[0].host`
  differing between reconciles → assert the Job's `urls-hash`
  annotation differs across the two reconciles AND assert the
  operator deleted the first Job before creating the second.
- Hash-stable: reconcile twice with identical spec → no Job
  deletion or recreation; condition stays True with the same
  message.
- `cache:flush` (not `cache:clean config`) is the script's final
  command — asserted by inspecting the Job's pod-template `args`
  for the literal `cache:flush` string. Catches the operator-side
  fix's regression.

`StoreSpec.Type` → scope mapping:
- `type: website, code: shop-eu` → script contains
  `--scope=websites --scope-code=shop-eu` for each of the 4 keys.
- `type: store_view, code: shop-fr-fr` → script contains
  `--scope=stores --scope-code=shop-fr-fr` for each of the 4 keys.
- `type: store, code: shop-emea` → script contains NO
  `config:set` for this entry; only the echo line.

### E2e (kind, `test/e2e/store_urls_test.go`)

Three scenarios:

1. **Auto-heal regression test (the v0.5.x upgrade case).** Apply
   MageStore, wait for `BootstrapReady=True` and `URLsApplied=True`.
   `kubectl exec` into a workload pod →
   `bin/magento config:show web/unsecure/base_url` → assert output
   is `http://shop.example.com/` (matches `spec.stores[0].host`),
   NOT `http://localhost:8080/`. Then patch the MageStore status
   to artificially clear `URLsApplied` AND patch
   `web/unsecure/base_url` in DB to `http://localhost:8080/`
   (simulating a v0.5.x deployment). Re-trigger reconcile (e.g.,
   via dummy spec annotation tweak). Wait for `URLsApplied=True`
   again. `config:show` → assert output is
   `http://shop.example.com/` once more. **This is the auto-heal
   regression's smoking-gun.**

2. **Multi-store happy path.** Apply MageStore with three stores:
   one website (shop-eu), one store_view (shop-fr-fr), one store
   group (shop-emea-group). Wait for `URLsApplied=True`. Verify:
   - `config:show --scope=websites --scope-code=shop-eu
     web/unsecure/base_url` matches `stores[0].host`.
   - `config:show --scope=stores --scope-code=shop-fr-fr
     web/unsecure/base_url` matches `stores[1].host`.
   - `URLsApplied` condition message contains
     `skipped (store-group entries — Magento has no URL override
     level for groups): shop-emea-group`.
   - Job logs (via `kubectl logs job/<store>-store-urls`) contain
     `→` echo lines for the two applicable stores.

3. **Spec-edit propagation.** Apply MageStore with single-store
   spec; wait for `URLsApplied=True`. Patch `spec.stores[0].host`
   to a new value. Wait for hash-flip detection + new Job +
   `URLsApplied=True` again. `config:show` → assert new host.
   Demonstrates post-install flexibility working without manual
   intervention.

### No CRD validation tests for ADR 013

No CRD shape change. Existing `spec.stores` and `spec.routing.tls`
validation (per ADR 007 and ADR 010 respectively) is unaffected.

### No image-contract regression test

No image-contract change. The Job uses `bin/magento` directly;
`bin/magento config:set` and `bin/magento cache:flush` are core
Magento commands, not optional tooling.

## Consequences

### Positive

- **Storefront URLs are correct out of the gate.** Fresh `helm
  install + apply MageStore` produces a Magento DB with proper
  `base_url` config. The v0.5.0 Kapsule demo's headline regression
  closes.
- **Auto-heal on upgrade.** Existing v0.5.x deployments with
  `localhost:8080` baked in are auto-fixed on first reconcile after
  upgrading to v0.6.0. No manual `kubectl exec` required.
- **Post-install flexibility.** `spec.stores[]` edits and
  `spec.routing.tls` flips propagate to Magento DB automatically
  via hash-driven Job re-run. User changes spec; URLs follow.
- **Multi-store first-class.** Per-website / per-store-view
  overrides applied automatically; previously a manual
  `bin/magento config:set` exercise per scope.
- **No CRD shape change.** Tighter ADR scope; smaller test
  surface; no migration concerns for existing CRs; no breaking
  change.
- **Diagnostic surface for the most common debug flows.**
  Spec-vs-DB drift, hash inspection, cache-flush guidance — all
  surfaced where users look first.

### Negative

- **One additional reconcile step + status condition.** Adds
  `ensureStoreURLs` to the reconcile graph, `URLsApplied` to the
  condition list. Cost of the new feature; not free.
- **TLS-downgrade post-install caveat.** Flipping
  `spec.routing.tls` from set → unset doesn't auto-update
  `web/secure/use_in_{frontend,adminhtml}`. User must manually
  flip these. Documented limitation; future work.
- **Drift detection is opt-in (manual `kubectl delete`).** If a
  user edits URLs in Magento admin manually, the operator doesn't
  notice. By design — autonomous drift-correction would require
  reading Magento DB on every reconcile, which is heavyweight.

### Neutral

- **Job accumulates on hash-stable reconciles.** Successful Jobs
  live for `ttlSecondsAfterFinished: 600` then auto-clean. Failed
  Jobs persist for log inspection (no TTL on failure), garbage-
  collected with the MageStore via ownerReference.
- **`cache:flush` impact on next request set.** Brief
  regeneration cost on first request after URL change. Acceptable
  — URL changes are rare.

## Alternatives considered

- **Inline-only (URLs derived once at `setup:install`, no
  reconcile step).** Doesn't auto-heal v0.5.x; doesn't propagate
  spec edits. Forces manual intervention for both. Rejected.
- **Reconcile-only (keep `setup:install`'s `localhost:8080`
  hardcode, fix post-install).** Window between bootstrap
  completion and URL-fix where Magento DB has wrong URLs and the
  bootstrap pod has already exited. The two-phase shape closes
  this window without losing the auto-heal property.
- **Hybrid with both inline + separate Job (belt-and-braces).**
  Two producers want to touch the same config rows; race
  conditions at first install (which writes first?). Two-phase
  cleanly separates responsibilities — bootstrap writes default
  scope at install time; ensureStoreURLs re-asserts default + adds
  per-scope on every hash flip. Same end state; cleaner
  ownership.
- **`cache:clean config` (only config cache).** What the script
  did in early drafts. Rejected: leaves `full_page_cache` and
  `block_html` holding pre-fix HTML for up to 24h, making
  `URLsApplied=True` a misleading signal. `cache:flush`
  (full flush) is the correct call.
- **CRD field for "primary store"** (e.g., `spec.stores[i].primary:
  bool`). Considered for the "which host drives default scope"
  question. Rejected: adds CRD surface for what `stores[0]`
  already conveys cleanly. Order-sensitive arrays are an idiom
  users understand.
- **Reject `type: store` (store group) entries at admit time.**
  Magento has no URL config level for store groups, but they're
  meaningful for `MAGE_RUN_TYPE` routing per ADR 007. Rejecting
  them in this ADR would break ADR 007. Skip-with-message is the
  right call.
- **Set `web/secure/use_in_{frontend,adminhtml}` at per-website
  scope automatically.** Magento supports it; operator could emit.
  Rejected (YAGNI): the mixed-HTTP-HTTPS multi-site setup is
  genuinely rare; setting these to the same value as default scope
  would be redundant; setting them to different values requires
  spec input the CRD doesn't currently expose. Future work if a
  user asks.
- **Operator-side periodic drift detection.** Reading Magento DB
  state on every reconcile (`bin/magento config:show` per URL key)
  to detect manual admin-UI edits. Rejected as heavyweight; spec
  is the declarative source of truth, drift is user error to be
  resolved by `kubectl delete job/<store>-store-urls`.
- **Strip `:8080` from localhost fallback.** Considered
  cosmetic-only. Rejected: the image listens on 8080; localhost
  URLs without the port don't reach the pod when port-forwarded.

## Open questions / future work

- **`ensureSecureFlags` reconcile step.** Manage `web/secure/
  use_in_{frontend,adminhtml}` reactively to `spec.routing.tls`
  flips. Defer until a user actually performs a TLS downgrade
  post-install. Could fold into `ensureStoreURLs` or be a separate
  step. When the deferred step lands, surface the TLS-downgrade
  case via a Warning Event AND a `URLsApplied` condition message
  hint — currently a user who flips TLS off post-install only finds
  out their `use_in_frontend` is stale by reading this ADR's
  "Negative consequences" section.
- **Per-website `use_secure` overrides.** For mixed-HTTP-HTTPS
  multi-site setups. Add CRD surface (`spec.stores[i].useSecure:
  bool`) when a user asks. Trivial extension once requested.
- **Static / media URL overrides.** Currently inherited via
  Magento's `{{base_url}}` placeholder resolution. CDN setups
  override out-of-band. Could be exposed as
  `spec.stores[i].staticUrl` / `mediaUrl` if multi-domain CDN
  routing becomes a recurring user need.
- **Observability: emit a Kubernetes Event on URL change.**
  Currently the operator's own logs and the Job logs carry the
  signal; surfacing as a `kubectl get events` Event would make
  audit trails easier. Defer — small ergonomic win, not blocking.

## References

- Existing bootstrap script:
  `internal/controller/ensure_bootstrap.go:51-98` (the multi-line
  bash const) and `:71` (the hardcoded `--base-url`).
- ADR 005 — bootstrap as Job.
- ADR 007 — multi-store mage-run-code mapping (defines
  `spec.stores`, `StoreSpec.Type`).
- ADR 010 — public-edge TLS (defines `spec.routing.tls`).
- ADR 011 — image-baked media seeding (sibling reconcile step
  with hash-driven Job pattern).
- ADR 012 — SMTP configuration (probe Job pattern with hash-driven
  re-run, mirror of this ADR's `urls-hash`).
- Magento `config:set` scope vocabulary documentation (Adobe
  Commerce DevDocs):
  `default | websites | stores`. Note: Magento's "stores" scope
  means store views, not store groups.

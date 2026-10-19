# The `mage-operator` image contract

This document defines what a customer-provided Magento image must include to
work with `mage-operator`. The operator emits configuration as individual
env vars per [ADR 002](internals/adr/002-config-emission-modes.md) (amended 2026-04-24
— the alternative ECE-Tools emission path was dropped from Phase 1 and will
be revisited only on user demand). There is no `spec.config.mode` field
on the CRD.

The operator is explicitly **not** a Magento container builder. Customers
bring their own image — forked from the reference in `magento-test-image/`
or built from scratch — and the operator wires configuration into it.
This file is the interface specification between those two sides.

> Not affiliated with, endorsed by, or sponsored by Adobe Inc. "Magento"
> and "Adobe Commerce" are trademarks of Adobe Inc.

## The short version

Your image must:

1. Read configuration from environment variables at startup (the list is
   below).
2. Ship an `app/etc/env.php` that translates those env vars into Magento's
   expected config shape (canonical snippet below).
3. Include the PHP extensions Magento 2.4 requires.
4. Run an HTTP server on port `8080` that serves Magento's front
   controller (`pub/index.php`).

The operator handles everything else: Secret generation, credential
rotation, probes, the Deployment, the Service, and — when `ensureBootstrap`
lands — the one-shot `setup:install` for fresh stores.

## Environment variable namespace

The operator emits all configuration via a single Secret per MageStore,
mounted into the workload pods through `envFrom`. The names are stable
across operator releases; if we need to change one, it's an ADR.

### Currently emitted (Phase 1)

| Variable                    | Source                                                | Notes                                                                                    |
|-----------------------------|-------------------------------------------------------|------------------------------------------------------------------------------------------|
| `MAGENTO_DATABASE_HOST`     | `spec.database` (all three modes) → Service name or `external.host` | Hostname only. Port is separate.                                                         |
| `MAGENTO_DATABASE_PORT`     | same                                                  | Default `3306`; `external.port` overrides.                                               |
| `MAGENTO_DATABASE_NAME`     | Hard-coded `magento` for inline/ref; `external.databaseName` (default `magento`) | Will become configurable for inline/ref when a real use-case surfaces.                   |
| `MAGENTO_DATABASE_USERNAME` | MariaDB CR root (inline/ref, Phase 1 shortcut) or `external.credentialsSecretRef[username]` | Inline/ref will become a dedicated Magento user via mariadb-operator's `User`/`Grant` CRDs. |
| `MAGENTO_DATABASE_PASSWORD` | MariaDB-operator-generated Secret (inline/ref) or `external.credentialsSecretRef[password]` | Rotated at source → operator re-emits → `envFrom` pushes new value to pods.            |
| `MAGENTO_CRYPT_KEY`         | Generated once per store, preserved across reconciles | Format is `base64<base64-of-32-bytes>` per [ADR 003](adr/003-crypt-key-format.md). Use verbatim — do not decode. |
| `MAGE_MODE`                 | Hard-coded `production` today                         | Will be driven by `spec.deploymentMode` once that field lands.                           |
| `MAGENTO_OPENSEARCH_HOST`   | `spec.search.openSearch` (inline) or `opensearchRef`  | Resolved via `ensureSearch`; only emitted once `SearchReady=True`. Hostname only.        |
| `MAGENTO_OPENSEARCH_PORT`   | same                                                  | Default `9200`.                                                                          |
| `MAGENTO_REDIS_HOST`        | `spec.cache` (all three modes) → Service name or `external.host` | Resolved via `ensureCache`; only emitted once `CacheReady=True`. Hostname only.          |
| `MAGENTO_REDIS_PORT`        | same                                                  | Default `6379`; `external.port` overrides.                                               |
| `MAGENTO_REDIS_PASSWORD`    | Inline: operator-generated Secret (key `password`, same name as Valkey CR). Ref: same. External: `external.passwordSecretRef`. | **Not emitted** when the endpoint is password-less (ExternalCache without `passwordSecretRef`). Image must honour absence as "connect without AUTH". |
| `MAGENTO_RABBITMQ_HOST`     | `spec.queue` (all three modes) → Service name or `external.host` | Resolved via `ensureQueue`; only emitted once `QueueReady=True`. Hostname only.          |
| `MAGENTO_RABBITMQ_PORT`     | same                                                  | Default `5672`; `external.port` overrides.                                               |
| `MAGENTO_RABBITMQ_VHOST`    | Hard-coded `/` for inline/ref; `external.vhost` (default `/`) | AMQP virtual host. Magento Phase-1 always uses the default vhost.                        |
| `MAGENTO_RABBITMQ_USERNAME` | Inline/ref: `Status.DefaultUser.SecretReference.Keys["username"]` on RabbitmqCluster (upstream self-describes). External: `external.credentialsSecretRef[username]`. | Emitted only when queue is configured; Magento falls back to synchronous processing otherwise. |
| `MAGENTO_RABBITMQ_PASSWORD` | Inline/ref: same Secret reference, key `password`. External: `external.credentialsSecretRef[password]`. | RabbitMQ always requires AUTH — no password-less branch like Redis.                      |

### Per-request scope dispatch — `$_SERVER`-sourced, not env-var-sourced

Two values are intentionally NOT in the env-var table above:
`MAGE_RUN_CODE` and `MAGE_RUN_TYPE`. They are per-request rather
than per-pod — a single php-fpm worker serving multiple stores
(via `spec.stores`) sees a different `MAGE_RUN_CODE` for each
incoming HTTP request. Env vars are worker-global and can't
express that, so the operator surfaces them via nginx's
`fastcgi_param` directive instead, populating `$_SERVER` rather
than the process environment. See [ADR 007](adr/007-multi-store-mage-run-code-mapping.md)
for the full dispatch model.

The image's `bootstrap.php` / `env.php` MUST read these two
values from `$_SERVER`, not via `getenv()`:

```php
$runCode = $_SERVER['MAGE_RUN_CODE'] ?? '';
$runType = $_SERVER['MAGE_RUN_TYPE'] ?? '';
```

Empty strings mean "unknown host" or "single-store install"
(Magento falls back to its default store). Every other
operator-emitted variable in the table above remains
`getenv()`-sourced — only these two diverge.

The operator generates a per-MageStore ConfigMap
`<store>-nginx-conf` (Session 2) carrying the two `map
$http_host` directives (`stores.conf`) and a fastcgi server
block (`default.conf`). A SHA-256 hash of the ConfigMap data
lands on every workload Deployment's pod template as
`mage.aknostic.com/nginx-config-hash`, so changes to
`spec.stores` trigger a rolling restart.

**Mount target — landed Session 3 (co-hosted shape).** The
reference image's `nginx.conf` now includes
`include /etc/nginx/conf.d/*.conf;` at the http level, and the
operator subPath-mounts the ConfigMap's `stores.conf` key at
`/etc/nginx/conf.d/stores.conf` on every workload Deployment.
The subPath discipline exists because the ConfigMap also
carries a `default.conf` fastcgi server block intended for the
Session-4 sidecar shape, which would conflict with the co-
hosted image's own server block on port 8080 if mounted
together. Customer images forking the reference MUST either
include the same `include` directive at http level or graduate
to the Session-4 sidecar shape.

The fastcgi location in the reference image's nginx.conf now
also surfaces the two `fastcgi_param MAGE_RUN_CODE
$MAGE_RUN_CODE;` / `fastcgi_param MAGE_RUN_TYPE $MAGE_RUN_TYPE;`
lines so the map values reach PHP's `$_SERVER`. The generated
`stores.conf` always declares both `map $http_host` blocks
(default clause is an empty string when `spec.stores` is unset)
— nginx errors on undeclared variables, so the operator emits
a valid map even for single-store installs.

Magento convention for logical-DB layout on the single emitted Redis
endpoint:

| Logical store  | DB # | `env.php` entry         |
|----------------|------|-------------------------|
| Default cache  | 0    | `cache.frontend.default`|
| Page cache     | 1    | `cache.frontend.page_cache` |
| Session store  | 2    | `session.redis.database` |

The operator emits one `MAGENTO_REDIS_HOST` / `PORT` / `PASSWORD`; the
image's `env.php` picks the DB number per backend. The single-endpoint-
plus-logical-DB convention is the same pattern Adobe Commerce Cloud uses
internally, so env.php written against this contract is structurally
close to what Adobe-Cloud migrants already run — the values get wired
via `getenv()` instead of the Cloud-specific `MAGENTO_CLOUD_RELATIONSHIPS`
JSON blob, but the logical layout is identical.

### Media (emitted for `spec.media.s3` mode only)

| Variable                                  | Source                                                              | Notes                                                                                                                                             |
|-------------------------------------------|---------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| `MAGENTO_MEDIA_STORAGE_BUCKET`            | `spec.media.s3.bucket`                                              | S3 (or S3-compatible) bucket name. Emitted only in s3 mode; PVC / emptyDir modes provide media via mounted volume and need no env vars.           |
| `MAGENTO_MEDIA_STORAGE_ENDPOINT`          | `spec.media.s3.endpoint`                                            | Optional. Unset for AWS S3 (SDK picks regional endpoint); set for Scaleway / Minio / R2 / etc.                                                    |
| `MAGENTO_MEDIA_STORAGE_REGION`            | `spec.media.s3.region`                                              | Required by AWS SDK v2 even for non-AWS endpoints.                                                                                                |
| `MAGENTO_MEDIA_STORAGE_ACCESS_KEY_ID`     | `spec.media.s3.credentialsSecretRef[accessKeyId]`                   | User-managed Secret, key `accessKeyId`.                                                                                                           |
| `MAGENTO_MEDIA_STORAGE_SECRET_ACCESS_KEY` | `spec.media.s3.credentialsSecretRef[secretAccessKey]`               | User-managed Secret, key `secretAccessKey`. Schema matches the AWS SDK v2 default credential shape.                                               |

Images wire these into Magento's remote-storage driver via the
`remote_storage` block in `app/etc/env.php`, reading from
`getenv()` exactly the same way every other env var is consumed
(see the canonical `env.php` below). `bin/magento
setup:config:set --remote-storage-*` is *not* the right tool here:
it parses `env.php`, mutates the array, and writes it back —
which freezes every other `getenv()` call into a static value at
write-time, defeating the dynamic-config pattern. The reference
image in this repo's `magento-test-image/env.php` shows the
expected shape: a top-level guard on
`MAGENTO_MEDIA_STORAGE_BUCKET` selects the `aws-s3` driver when
emitted, otherwise falls back to the local-disk `file` driver
(matching what the mounted volume — emptyDir or PVC — provides
at `/app/pub/media`).

### Planned (as ensure* steps land)

| Variable                       | Status         | Notes                                                           |
|--------------------------------|----------------|-----------------------------------------------------------------|
| `MAGENTO_BASE_URL`             | Planned Day 8  | Derived from `spec.stores[].host` + ingress TLS.                |

Images are free to read env vars that don't exist yet — `getenv()` returns
`false`, and a thoughtful `env.php` uses `getenv(...) ?: $default`. That's
what lets us add variables without breaking existing customer images.

## Canonical `env.php`

Ship this file in your image at `app/etc/env.php`. It consumes the
operator's env vars directly; every value is driven by the mounted Secret.

```php
<?php
// remote_storage selects the aws-s3 driver when the operator
// emits MAGENTO_MEDIA_STORAGE_BUCKET (= spec.media.s3 mode).
// Unset / PVC modes leave it at the local-disk file driver,
// matching the mounted volume at /app/pub/media.
$remoteStorage = ['driver' => 'file'];
if ($mediaBucket = getenv('MAGENTO_MEDIA_STORAGE_BUCKET')) {
    $remoteStorage = [
        'driver' => 'aws-s3',
        'config' => [
            'bucket'      => $mediaBucket,
            'region'      => getenv('MAGENTO_MEDIA_STORAGE_REGION') ?: '',
            'credentials' => [
                'key'    => getenv('MAGENTO_MEDIA_STORAGE_ACCESS_KEY_ID') ?: '',
                'secret' => getenv('MAGENTO_MEDIA_STORAGE_SECRET_ACCESS_KEY') ?: '',
            ],
        ],
    ];
    if ($mediaEndpoint = getenv('MAGENTO_MEDIA_STORAGE_ENDPOINT')) {
        $remoteStorage['config']['endpoint'] = $mediaEndpoint;
    }
}

return [
    'backend' => ['frontName' => 'admin'],
    'crypt' => ['key' => getenv('MAGENTO_CRYPT_KEY')],
    'db' => [
        'connection' => [
            'default' => [
                'host'           => getenv('MAGENTO_DATABASE_HOST') . ':' . getenv('MAGENTO_DATABASE_PORT'),
                'dbname'         => getenv('MAGENTO_DATABASE_NAME'),
                'username'       => getenv('MAGENTO_DATABASE_USERNAME'),
                'password'       => getenv('MAGENTO_DATABASE_PASSWORD'),
                'model'          => 'mysql4',
                'engine'         => 'innodb',
                'initStatements' => 'SET NAMES utf8;',
                'active'         => '1',
            ],
        ],
        'table_prefix' => '',
    ],
    'resource'    => ['default_setup' => ['connection' => 'default']],
    'MAGE_MODE'   => getenv('MAGE_MODE') ?: 'production',
    'install'     => ['date' => getenv('MAGENTO_INSTALL_DATE') ?: 'Sun, 01 Jan 2023 00:00:00 +0000'],
    'directories' => ['document_root_is_pub' => true],
    'session'     => ['save' => 'files'],
    'lock'        => ['provider' => 'db'],
    'cache_types' => [
        'config'                  => 1,
        'layout'                  => 1,
        'block_html'              => 1,
        'collections'             => 1,
        'reflection'              => 1,
        'db_ddl'                  => 1,
        'compiled_config'         => 1,
        'eav'                     => 1,
        'customer_notification'   => 1,
        'config_integration'      => 1,
        'config_integration_api'  => 1,
        'full_page'               => 1,
        'config_webservice'       => 1,
        'translate'               => 1,
    ],
    'queue'          => ['consumers_wait_for_messages' => 1],
    'remote_storage' => $remoteStorage,
];
```

**Do not `base64_decode` or `bin2hex` the crypt key.** The operator emits
it in a format Magento reads verbatim. ADR 003 explains why.

**Do not hardcode host/port/credentials.** They are not stable across
environments, not stable across upgrades, and — for password rotation —
not stable across reconciles. Always read them via `getenv()`.

## Required PHP extensions

Mage-OS / Magento 2.4.8 `composer install` fails without these. The
reference image builds them all via `docker-php-ext-install`:

```
bcmath  ftp  gd  intl  opcache  pdo_mysql  soap  sockets  xsl  zip  sodium
```

`sodium` is usually bundled in upstream PHP builds; the rest need to be
installed explicitly on `php:8.3-fpm-bookworm`.

## HTTP server

The operator's `ensureWorkloads` exposes **port 8080** named `http` and
points the Service at it. The image must listen on `8080` inside the
container. The production shape is an nginx sidecar in front of php-fpm;
the reference image co-hosts nginx + php-fpm via supervisord for
simplicity (documented as reference, not production).

The nginx config must serve Magento's front controller — the upstream
`nginx.conf.sample` from `pub/index.php`'s directory is a good starting
point. The reference image's `magento-test-image/nginx.conf` is a pruned
minimal version.

## Bootstrap (setup:install) responsibility

`setup:install` creates the Magento schema, indexers, and admin user on
a fresh store. The operator owns this as a one-shot
`<store>-bootstrap` Job driven by `spec.bootstrap.admin` on the
MageStore (see [ADR 005](adr/005-bootstrap-as-job.md) for the Job-vs-
initContainer tradeoff and the `StoreInstalled` condition gate).

The Job uses the same image the frontend Deployment does, mounts the
operator-rendered config Secret (plus the optional per-store SMTP
Secret) via `envFrom`, and runs
`bin/magento setup:install` with database, admin, and search args
wired from the env vars. Before setup:install the Job removes
`/app/app/etc/env.php` from its ephemeral filesystem — Magento reads
an existing env.php as "already installed" and bails on the default-
website lookup against an empty DB. Frontend pods are unaffected
because the file is only removed in the Job container, not rewritten
in the image.

Images don't need to change. The bootstrap script is entirely driven
by env vars already in the contract.

## What the image does NOT need

- A custom entrypoint. Default `php-fpm -F` (or the reference image's
  supervisord wrapper) is fine.
- Sample data. Magento sample data is optional for all shops; the
  operator doesn't provision it.
- ECE-Tools. The operator does not load, invoke, or require it.
  Phase-1 scope is envvars-only (see ADR 002 Amendment 2026-04-24).
  Adobe-Cloud-migrant shops can still bring an ECE-Tools-equipped
  image — the env vars the operator emits coexist with ECE-Tools'
  own env.php materialisation — but the operator doesn't
  orchestrate ECE-Tools startup.
- A crypt key in the image. The operator generates and preserves it;
  writing a hard-coded key into the image is a security bug.

## Validation

For local validation of a customer image against this contract, the
current sample manifests cover the end-to-end path — the only
reconciler step still hand-applied is `ensureServices` (the frontend
Service):

1. `docker build -t <your image> <your Dockerfile context>`
2. `kind load docker-image <your image>`
3. Edit `config/samples/mage_v1alpha1_magestore.yaml` — point
   `spec.image.repository` / `spec.image.tag` at your image, and set
   `spec.bootstrap.admin.email` to your address.
4. `kubectl apply -k config/samples/` — creates the `demo` namespace,
   the MariaDB CR, and the MageStore (which inline-provisions its
   OpenSearchCluster per ADR 004 and its `setup:install` Job per
   ADR 005).
5. `kubectl apply -f config/samples/frontend-service.yaml`
6. `kubectl -n demo wait --for=condition=Ready magestore/store --timeout=45m`
7. `kubectl -n demo port-forward svc/store-frontend 8080:8080` and
   `curl http://localhost:8080/admin`

HTTP 200 with Magento's admin login HTML means the contract is
honored. Static-asset URLs (e.g. `/static/version*/adminhtml/…`)
returning HTTP 200 means the image's `setup:static-content:deploy`
output is present and nginx's version-prefix rewrite is in place.

## Image-baked media seeding — `pub/media-baked`

Per ADR 011, the operator can seed a fresh `pub/media` volume (or S3
bucket) from the workload image's baked sample data when
`spec.media.seedFromImage: true`. The seeding mechanism reads from a
parallel path inside the image — `/app/pub/media-baked` — chosen so
the runtime mount at `/app/pub/media` cannot hide it.

**Required at:** `/app/pub/media-baked/` exists in the runtime image
when `seedFromImage: true` is set on the MageStore. May be empty (the
operator's seed mechanism degrades to no-op).

**Reference image:**
```dockerfile
COPY --from=builder /app /app
RUN mv /app/pub/media /app/pub/media-baked
# Subsequent multi-target mkdir re-creates /app/pub/media as the
# empty mount point.
```

When `seedFromImage: true` AND `/app/pub/media-baked/` is missing, the
seeding mechanism (initContainer for emptyDir/PVC; Job for S3) fails
loudly: `cp` exits 1 → Pod `Init:Error` (volume modes) or Job
`Failed` → `MediaSeeded=False, Reason=ImageContractViolation`.

## SMTP probe tooling — `swaks`

Per ADR 012, the operator runs a one-shot probe Job that exercises
`EHLO + STARTTLS + AUTH` against the configured SMTP relay before
gating workloads on `SMTPReady=True`. The probe uses [swaks][swaks]
— a single Perl script with `Net::SMTP` dependencies.

**Required at:** `/usr/bin/swaks` (Debian/Ubuntu apt path).

**Dockerfile addition (Debian/Ubuntu base):**
```dockerfile
RUN apt-get update \
    && apt-get install -y --no-install-recommends swaks ca-certificates \
    && rm -rf /var/lib/apt/lists/*
```

Without `swaks`, the probe Job exits 127 ("command not found") →
`SMTPReady=False, Reason=ProbeFailed, Message="image-contract
violation: swaks not in image"`.

[swaks]: https://www.jetmore.org/john/code/swaks/

### Anonymous SMTP

`spec.smtp.inline.user` / `passwordSecretRef` are optional (CEL-paired:
both or neither). When both are absent, the operator omits
`SMTP_USERNAME` and `SMTP_PASSWORD` from the per-store SMTP Secret —
they do not appear in the workload pod's environment. Mailpit and
IP-allowlisted relays (Postmark, AWS SES with sender authorization)
accept SMTP without `AUTH`; this is the path for those setups.

**Image responsibility.** The image's entrypoint must tolerate
`SMTP_USERNAME` / `SMTP_PASSWORD` being unset under `set -eu` — i.e.,
`${SMTP_USERNAME:-}` (default expansion), and conditional emission
of any `auth` / `user` / `password` lines in the generated msmtp /
sendmail config. The reference image's `entrypoint.sh` shows the
pattern: it builds the auth section only when `[ -n "${SMTP_USERNAME:-}" ]`,
otherwise it lets msmtp connect anonymously.

The operator's probe Job follows the same pattern: it constructs
swaks `--auth` flags only when `SMTP_USERNAME` is non-empty, so
anonymous-relay deployments pass the probe without reaching for AUTH.

## Reference image

`magento-test-image/` in this repository is a working Mage-OS 2.4.8-p4
image that satisfies this contract. Treat it as a starting point to
fork, not an image to deploy to production — it co-hosts nginx +
php-fpm in a single container and has no custom modules, themes, or
extensions. See `magento-test-image/README.md` for the full list of
simplifications relative to a production image.

# Reference image for mage-operator envvars mode.
#
# A working, forkable starting point for customer images that satisfy
# the contract in docs/image-contract.md. Not a production image — it
# co-hosts nginx + php-fpm in a single container via supervisord and
# has no custom modules/themes/extensions. Production shape is a
# sidecar nginx container and an image forked from this one with the
# customer's codebase grafted in.
#
# Target: Magento 2.4.8-p4 via the Mage-OS mirror (no Adobe auth).

# ---------------------------------------------------------------------------
# Stage 1 — composer install + di:compile + static-content:deploy.
# Produces /app with a ready-to-serve Magento codebase.
# ---------------------------------------------------------------------------
FROM php:8.3-fpm-bookworm AS builder

ENV COMPOSER_ALLOW_SUPERUSER=1 \
    COMPOSER_NO_INTERACTION=1 \
    COMPOSER_MEMORY_LIMIT=-1

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates curl git unzip \
       libfreetype6-dev libicu-dev libjpeg62-turbo-dev libpng-dev libxslt1-dev libzip-dev libwebp-dev \
    && docker-php-ext-configure gd --with-freetype --with-jpeg --with-webp \
    && docker-php-ext-install -j"$(nproc)" bcmath ftp gd intl opcache pdo_mysql soap sockets xsl zip \
    && curl -sSL https://getcomposer.org/download/latest-2.x/composer.phar -o /usr/local/bin/composer \
    && chmod +x /usr/local/bin/composer \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install Magento 2.4.8-p4 via the Mage-OS mirror. The mirror is a drop-in
# unauthenticated replacement for repo.magento.com: it serves the upstream
# `magento/*` package names, so the create-project target stays `magento/...`
# (not `mage-os/...` — that's a separate metapackage scheme).
RUN composer create-project \
    --repository-url=https://mirror.mage-os.org/ \
    magento/project-community-edition=2.4.8-p4 \
    /app \
    --no-dev --no-progress

# Optionally add Magento sample data (Luma catalog, ~2,000 SKUs, real product
# images). The Mage-OS mirror serves the same `magento/module-*-sample-data`
# packages as Adobe Marketplace without auth. `sampledata:deploy` rewrites
# /app/composer.json with the sample-data requires and runs composer update.
# Cost: ~2 GB on disk (fixtures + images) and ~30-45 min on top of the base
# build (composer fetch + di:compile inflation + heavier static deploy).
ARG WITH_SAMPLEDATA=false
RUN if [ "${WITH_SAMPLEDATA}" = "true" ]; then \
        composer config repositories.mage-os composer https://mirror.mage-os.org/ \
        && php -d memory_limit=2G bin/magento sampledata:deploy ; \
    fi

# A freshly-created Magento skeleton has no app/etc/config.php — modules are
# not yet enabled, so setup:di:compile bails with "modules are not enabled".
RUN php -d memory_limit=2G bin/magento module:enable --all

# Generate DI classes at build time.
RUN php -d memory_limit=2G bin/magento setup:di:compile

# Seed app/etc/config.php with default scopes (admin + base websites and
# their stores). setup:static-content:deploy in Magento 2.4.x reads scopes
# from this file at build time; without the seed it bails with "The
# default website isn't defined." The seed is stripped again below so
# setup:install at runtime sees a clean config.
COPY seed-scopes.php /tmp/seed-scopes.php
RUN php /tmp/seed-scopes.php /app && rm /tmp/seed-scopes.php

# Build-time tuning knobs. Customers forking this image override these:
#   docker build --build-arg STATIC_LOCALES="en_US de_DE fr_FR" ...
# CONTENT_VERSION pins pub/static/deployed_version.txt so replicas serve
# the same cache-busting URL segment; browsers don't see version thrash
# on a scaling event.
ARG STATIC_LOCALES="en_US"
# Magento's --content-version validator only accepts [a-zA-Z0-9.], no
# hyphens. Default is the Magento minor version. Customers should override
# per build (image tag, git sha, build number) so cache busting works
# across releases.
ARG CONTENT_VERSION="2.4.8"

# Deploy static assets for both adminhtml (admin panel) and frontend themes
# in the requested locales. Runs in production mode by default — required
# because MAGE_MODE=production pods 404 on missing static files rather than
# lazy-generating them. Adds ~5-10 min to the build and ~200-500 MB to the
# image; that's the production-quality cost.
RUN php -d memory_limit=4G bin/magento setup:static-content:deploy \
    --content-version="${CONTENT_VERSION}" \
    -f ${STATIC_LOCALES}

# Strip the build-time scopes seed back out of app/etc/config.php. The seed
# was only needed so static-content:deploy could iterate websites/stores;
# leaving it in place breaks `setup:install` at runtime because the seeded
# scopes reference rows that don't exist in the empty DB
# (WebsiteRepository::getDefault throws "The default website isn't defined").
# setup:install rewrites the scopes section itself once it has populated
# the DB.
RUN php -r '$p="/app/app/etc/config.php"; $c=require $p; unset($c["scopes"]); file_put_contents($p, "<?php\nreturn ".var_export($c,true).";\n");'

# Clean var/ — that's caches/logs/sessions/reports, regenerated at
# runtime. Keep generated/ intact: it's the output of
# setup:di:compile above, shipping it in the image is the whole point
# of running di:compile at build time. Wiping it forces Magento to
# lazy-generate interceptors at runtime, which triggers permission
# errors on nested mkdirs and slow first-request latency.
RUN rm -rf var/*

# ---------------------------------------------------------------------------
# Stage 2 — runtime. nginx + php-fpm + msmtp in a single container
# orchestrated by supervisord.
# ---------------------------------------------------------------------------
FROM php:8.3-fpm-bookworm AS runtime

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       nginx supervisor \
       msmtp msmtp-mta ca-certificates \
       swaks \
       libfreetype6 libicu72 libjpeg62-turbo libpng16-16 libxslt1.1 libzip4 libwebp7 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/etc/php/conf.d/docker-php-ext-*.ini /usr/local/etc/php/conf.d/
COPY --from=builder /usr/local/lib/php/extensions /usr/local/lib/php/extensions

COPY --from=builder /app /app

# Move builder-stage's pub/media (which holds Luma sample-data images
# when WITH_SAMPLEDATA=true) to pub/media-baked. The runtime mount at
# /app/pub/media (emptyDir or PVC) would otherwise hide the baked
# content; ensureMediaSeed (volume modes via initContainer, S3 via Job)
# reads from /app/pub/media-baked to seed the runtime mount on first
# install. Per ADR 011. The line-153 mkdir below re-creates
# /app/pub/media as the empty mount point.
RUN mv /app/pub/media /app/pub/media-baked

# Canonical envvars-mode env.php per docs/image-contract.md. Overwrites the
# stub env.php that setup:di:compile leaves behind — ours reads every
# config value from env vars the operator mounts via envFrom.
COPY env.php /app/app/etc/env.php

COPY nginx.conf /etc/nginx/nginx.conf
COPY supervisord.conf /etc/supervisor/conf.d/mage-runtime.conf
COPY php-fpm.conf /usr/local/etc/php-fpm.d/zz-magento.conf

# Bump PHP memory_limit from the upstream 128M default. Magento CLI
# invocations (consumers, cron, setup:upgrade) routinely exceed 128M
# once Redis cache backends are wired in; production guidance is 2G
# minimum. Applies to both php-fpm (workload pods) and php-cli
# (consumer/cron pods + Job-driven setup:install / setup:upgrade).
RUN echo "memory_limit = 2G" > /usr/local/etc/php/conf.d/zz-magento-memory.ini

# Entrypoint writes /etc/msmtprc from SMTP_* env vars (if present) then
# execs supervisord. msmtp-mta already symlinks /usr/sbin/sendmail to
# msmtp, so Magento's Mail\Transport picks up delivery automatically.
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Writable runtime dirs. Magento writes caches, sessions, reports, and
# generated code under var/ and generated/ at runtime; pub/static is also
# populated lazily when static assets are requested.
RUN mkdir -p /app/var/cache /app/var/page_cache /app/var/session /app/var/log /app/var/report /app/generated/code /app/generated/metadata /app/pub/media \
    && chown -R www-data:www-data /app \
    && chmod -R g+rwX /app/var /app/generated /app/pub/static /app/pub/media /app/app/etc

WORKDIR /app

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/conf.d/mage-runtime.conf"]

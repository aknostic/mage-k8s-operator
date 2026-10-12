<?php
// Failure-path e2e fixture. Replaces /app/bin/magento in the broken
// reference image so the operator's setup:upgrade Job exits non-zero.
//
// The upgrade Job script (internal/controller/ensure_upgrade.go) runs:
//
//   php -d memory_limit=2G bin/magento setup:upgrade
//
// This shim detects the setup:upgrade subcommand and exits 1 with a
// recognisable stderr line. Other subcommands fall through to a quiet
// success — workload pods don't invoke bin/magento at runtime (nginx +
// php-fpm via supervisord), and during the failure-path arc the cron
// CronJobs are suspended and consumer Deployments are at zero, so in
// practice only the operator-owned upgrade Job ever runs this file.
// Keeping non-setup:upgrade calls passing is defensive so the fixture
// stays usable for any future arc that pokes at bin/magento between
// arc-entry and Job-create.
if (in_array('setup:upgrade', $argv, true)) {
    fwrite(STDERR, "BROKEN-IMAGE: bin/magento setup:upgrade fixture — exiting 1 to simulate upgrade failure\n");
    exit(1);
}
fwrite(STDERR, "BROKEN-IMAGE: bin/magento " . implode(' ', array_slice($argv, 1)) . " — no-op pass\n");
exit(0);

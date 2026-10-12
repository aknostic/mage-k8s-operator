package magento

import "slices"

// Consumers is the canonical list of Magento 2.4.8 OSS queue consumer
// names, extracted from the upstream `queue_consumer.xml` files across
// core modules (app/code/Magento/*/etc/queue_consumer.xml) plus the
// Mage-OS mirror's equivalents. The operator materialises one
// Deployment per entry when `spec.consumers.enabled: true`; users
// override per-consumer resources via
// `spec.consumers.overrides[<name>]` keyed on the exact strings below.
//
// **Stable order** — the slice is append-only; new consumers added in
// a future Magento minor version land at the end. Preserves reconcile-
// log determinism and sample manifest diffs across releases.
//
// **Customer-added consumers** (modules outside Magento OSS core that
// publish their own `queue_consumer.xml`) are out of scope for v0.1.0:
// the operator fan-out iterates this slice, not the user-declared
// override map. A future `spec.consumers.extras[]` field lets users
// declare additional consumer names alongside the known list; tracked
// as Phase-2 work.
//
// **Commerce-only consumers excluded.** `matchCustomerSegmentProcessor`
// (Customer Segment) and a handful of other Adobe-Commerce-only
// consumers are not in this list — users on Adobe Commerce with a
// forked operator image extend the list, or wait for the extras field.
var Consumers = []string{
	"async.operations.all",
	"catalog_website_attribute_value_sync",
	"codegeneratorProcessor",
	"exportProcessor",
	"inventory.indexer.sourceItem",
	"inventory.indexer.stock",
	"inventory.mass.update",
	"inventory.reservations.cleanup",
	"inventory.reservations.update",
	"inventory.reservations.updateSalabilityStatus",
	"inventory.source.items.cleanup",
	"media.content.synchronization",
	"media.gallery.renditions.update",
	"media.gallery.synchronization",
	"media.storage.catalog.image.resize",
	"product_action_attribute.update",
	"product_action_attribute.website.update",
	"product_alert",
	"sales.rule.quote.trigger.recollect",
	"sales.rule.update.coupon.usage",
	"saveConfigProcessor",
}

// IsKnownConsumer returns true when the given name is in the canonical
// list. Used by the reconciler to flag unknown override keys with
// `ConsumersReady=False, Reason=UnknownConsumerOverride`.
func IsKnownConsumer(name string) bool {
	return slices.Contains(Consumers, name)
}

// CronGroups is the canonical list of Magento 2.4.8 cron group names,
// extracted from the upstream `crontab.xml` files across core modules.
// The operator materialises one CronJob per entry when
// `spec.cron.enabled: true`; users override schedule, resources, or
// the disabled flag via `spec.cron.overrides[<group>]`.
//
// **Stable order** — the slice is append-only; new groups added in a
// future Magento minor version land at the end. Preserves reconcile-
// log determinism and sample manifest diffs across releases.
//
// Defaults per group live in `internal/controller/ensure_cron.go` —
// `index` gets the generous memory profile (reindex is long-running
// and memory-hungry), the rest get a cautious default. All five
// default to `* * * * *` per Magento's upstream crontab convention.
var CronGroups = []string{
	"default",
	"index",
	"consumers",
	"staging",
	"catalog_event",
}

// IsKnownCronGroup returns true when the given name is in the
// canonical list. Used by the reconciler to flag unknown override
// keys with `CronReady=False, Reason=UnknownCronGroupOverride`.
func IsKnownCronGroup(name string) bool {
	return slices.Contains(CronGroups, name)
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MageStoreSpec is the v0.1.0 surface. Session 1 landed the full field
// set (workloads, consumers, cron, stores, media, varnish) in advance of
// the reconcilers that consume them; Session 2+ implement the
// behaviours. The image-contract env var namespace that downstream
// workloads read from lives in `docs/image-contract.md`.
type MageStoreSpec struct {
	// image selects the Magento container image. The image MUST include
	// ECE-Tools (magento/ece-tools); this is validated at reconcile time.
	// +required
	Image MageStoreImage `json:"image"`

	// workloads configures the Magento workload split. Frontend is
	// required; admin and api are optional and represent subtractive
	// optimization — when unset, their Ingress paths fall through to
	// the frontend Service at the nginx/Ingress layer (see ADR 006).
	// Same codebase ships across all three, so a single frontend
	// Deployment is functionally complete for small stores; the split
	// exists for stores whose admin or API traffic must not compete
	// with storefront traffic for CPU/memory.
	// +required
	Workloads WorkloadsSpec `json:"workloads"`

	// stores lists the Magento store scopes this MageStore serves.
	// Each entry declares a public hostname, a Magento store code,
	// and the scope type (website / store / store_view) — the
	// operator generates an nginx `map $http_host` block and per-host
	// Ingress rules from them, setting `MAGE_RUN_CODE` and
	// `MAGE_RUN_TYPE` on the request before it reaches the frontend
	// Service. When `spec.stores` is unset, the operator emits no
	// MAGE_RUN_CODE / MAGE_RUN_TYPE env vars and Magento serves its
	// default store (matches Slot-20 demo behaviour). See ADR 007.
	// +optional
	Stores []StoreSpec `json:"stores,omitempty"`

	// routing configures Gateway-layer concerns: public-edge TLS
	// (cert-manager-managed or BYO Secret), and Phase 2 follow-ons
	// (gatewayRef, gatewayClassName). When unset, the operator emits
	// the v0.4.x default — single HTTP listener on port 80, no TLS.
	// See ADR 010 for the design and ADR 006's open-questions section
	// for the deferred-fields trajectory.
	// +optional
	Routing *RoutingSpec `json:"routing,omitempty"`

	// database configures the MySQL-protocol backend Magento talks to.
	// Discriminated union per ADR 004: inline (`mariaDb`, operator
	// provisions a MariaDB CR), reference (`mariaDbRef`, admin pre-creates
	// the CR), or `external` (unmanaged endpoint — e.g. RDS / Cloud SQL).
	// +optional
	Database *DatabaseSpec `json:"database,omitempty"`

	// search configures the search backend. Magento 2.4.x hard-requires a
	// reachable OpenSearch/Elasticsearch endpoint at `setup:install` time.
	// +optional
	Search *SearchSpec `json:"search,omitempty"`

	// cache configures the Redis-protocol backend Magento uses for its
	// three logical stores (default cache, page cache, session storage).
	// Discriminated union per ADR 004: inline (`redis`, operator manages
	// a Valkey StatefulSet directly per ADR 009 — single-instance for
	// dev, Sentinel HA for production), reference (`redisRef`, points
	// at an admin-provisioned Service), or `external` (unmanaged
	// endpoint — e.g. ElastiCache / Memorystore). Field name stays
	// `redis`/`redisRef` even though the inline image is Valkey,
	// because wire-compat means the user-visible concept is Redis.
	// +optional
	Cache *CacheSpec `json:"cache,omitempty"`

	// queue configures the AMQP message broker Magento uses for
	// asynchronous work (queue consumers, bulk API, deferred index
	// rebuilds). Discriminated union per ADR 004: inline (`rabbitMq`,
	// operator provisions a RabbitmqCluster CR via rabbitmq-cluster-
	// operator), reference (`rabbitMqRef`, admin pre-creates the CR),
	// or `external` (unmanaged endpoint — e.g. Amazon MQ / CloudAMQP).
	// Optional at the CRD level: a MageStore without a queue runs in
	// Magento's synchronous-processing fallback, which is fine for dev
	// and for storefronts that don't use bulk operations.
	// +optional
	Queue *QueueSpec `json:"queue,omitempty"`

	// consumers configures the Magento queue consumer Deployments
	// (per-consumer `replicas: 1`, `terminationGracePeriodSeconds: 60`
	// because Magento consumers don't handle SIGTERM cleanly — see
	// CLAUDE.md architecture notes). Requires `spec.queue` to be set
	// for the consumers to have anything to consume; surfaced as a
	// runtime `ConsumersReady=False, Reason=QueueNotConfigured`
	// condition rather than a CRD-level CEL rule, because `spec.queue`
	// is optional at the CRD level and the runtime gate is more
	// informative.
	// +optional
	Consumers *ConsumersSpec `json:"consumers,omitempty"`

	// cron configures the Magento per-group cron CronJobs. Each
	// declared group runs as a separate CronJob invoking `bin/magento
	// cron:run --group=<name>` with baked-in per-group schedule +
	// resource defaults. See `internal/controller/ensure_cron.go`
	// (Session 2+) for the known-group list.
	// +optional
	Cron *CronSpec `json:"cron,omitempty"`

	// media configures the object-storage backend for Magento's
	// pub/media directory (product images, generated thumbnails,
	// wysiwyg uploads). Two-sibling ADR-004 union: `s3` for S3-
	// compatible object storage (the recommended prod path) or
	// `pvc` for an operator-provisioned PersistentVolumeClaim
	// (dev/test only — ReadWriteMany against Magento's pub/media
	// is known to corrupt at scale per CLAUDE.md). When unset, the
	// operator mounts an emptyDir at pub/media — ephemeral, fine
	// for demos, never for production.
	// +optional
	Media *MediaSpec `json:"media,omitempty"`

	// varnish configures an optional Varnish caching layer that
	// sits in front of the frontend Service. When unset, no
	// Varnish is materialised and Ingress points directly at the
	// frontend. When set with `enabled: true`, the operator
	// creates a Varnish Deployment + Service, generates a default
	// VCL tuned for the Magento version (updated to match
	// `spec.stores` when present — see ADR 007), and reroutes
	// Ingress through it. Full VCL override via
	// `vclConfigMapRef`.
	// +optional
	Varnish *VarnishSpec `json:"varnish,omitempty"`

	// smtp configures outbound email per ADR 012. When unset, no SMTP
	// env vars are emitted; outbound mail fails at transport (this is
	// intentional per gap #11 admin-email-defaulting policy — loud
	// failure beats silent leak of admin@example.com).
	// +optional
	SMTP *SMTPSpec `json:"smtp,omitempty"`

	// bootstrap configures the one-shot `setup:install` step the operator
	// runs against a fresh store. When unset, the operator does not invoke
	// setup:install and the workload rollout proceeds as soon as the
	// dependency chain (database, search, secrets) is ready — appropriate
	// only for stores pointing at a pre-bootstrapped database. Every
	// fresh store needs this field set. See
	// `internal/controller/ensure_bootstrap.go` for the one-shot semantics.
	// +optional
	Bootstrap *BootstrapSpec `json:"bootstrap,omitempty"`

	// upgrade configures how `spec.image.tag` drift becomes a Magento
	// `setup:upgrade` arc. v0.2.0 introduces a maintenance window
	// (Service-selector flip to a 503 page) fenced around the upgrade
	// Job; future fields (hooks, autoRollbackOnFailure) land additively
	// here. When unset, the operator still performs `setup:upgrade` on
	// tag drift but with default-fenced maintenance — see ADR 008.
	// +optional
	Upgrade *MageStoreUpgradeSpec `json:"upgrade,omitempty"`

	// security carries operator-wide security defaults — NetworkPolicy
	// generation, workload pod/container securityContext, future
	// security knobs. Absent → operator falls back to v0.6.x posture
	// (no NetworkPolicies, no workload securityContext). Present →
	// the sub-block defaults apply, modulated by the upgrade-safety
	// rule (fresh stores get defaults; existing stores opt in
	// explicitly). See ADR 014.
	//
	// Note: admin URL randomization (`AdminSpec.BackendFrontname`)
	// lives on `AdminSpec` not here — it is a bootstrap-time install
	// argument, not a reconcile-time policy. ADR 014 explains why.
	// +optional
	Security *SecuritySpec `json:"security,omitempty"`
}

// BootstrapSpec carries the operator-driven `setup:install` configuration.
// Today the only field is `admin`; sample-data seeding, base-URL overrides,
// and locale/currency knobs land alongside their use-cases. The struct
// exists as a parent so those fields have an obvious home when they do.
type BootstrapSpec struct {
	// admin is the Magento administrator account setup:install will create.
	// Required whenever `spec.bootstrap` is set — without an admin identity
	// the install command has nothing to write.
	// +required
	Admin AdminSpec `json:"admin"`
}

// AdminSpec describes the admin user setup:install creates on first boot.
// The email is required because Magento uses it verbatim as the initial
// sender identity for transactional email (the operator rewrites these
// identities post-install to the SMTP_FROM of the configured gateway,
// since Magento's `user@example.com` default is 551-rejected by any real
// SMTP gateway). First name / last name / username default to generic
// values. The initial password is either drawn from `passwordSecretRef`
// (user-supplied) or generated once by the operator and stashed in the
// per-store admin Secret (<store>-admin, key `password`).
type AdminSpec struct {
	// email is the initial admin user's email. Required. Must be a valid
	// address; the operator does not otherwise constrain the domain
	// because trans_email identity rewrite handles sender-verification
	// concerns. Used both as the admin contact address and as the
	// setup:install `--admin-email` argument.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:Pattern=`^[^@\s]+@[^@\s]+\.[^@\s]+$`
	// +required
	Email string `json:"email"`

	// firstName maps to setup:install's `--admin-firstname`.
	// +kubebuilder:default="Admin"
	// +optional
	FirstName string `json:"firstName,omitempty"`

	// lastName maps to setup:install's `--admin-lastname`.
	// +kubebuilder:default="User"
	// +optional
	LastName string `json:"lastName,omitempty"`

	// username maps to setup:install's `--admin-user`.
	// +kubebuilder:default="admin"
	// +optional
	Username string `json:"username,omitempty"`

	// backendFrontname is the URL path segment under which Magento's
	// admin panel is served. Maps to setup:install's
	// `--backend-frontname` flag. When unset, the operator generates
	// a 16-character random URL-safe string once at first bootstrap
	// and persists it in the `<store>-admin` Secret under key
	// `backendFrontname` (also writes a convenience `adminUrl` key
	// holding the full `https://<host>/<frontname>/` URL). For
	// existing stores upgraded from v0.6.x — detected by an existing
	// `<store>-admin` Secret with no `backendFrontname` key — the
	// operator defaults to the literal string `admin` to preserve
	// existing bookmarks per the ADR 014 upgrade-safety rule.
	// Subsequent reconciles read the value from the Secret; the
	// operator does NOT rotate it on spec change once persisted —
	// rotation is documented as redeploy + manual config:set.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_-]{4,32}$`
	// +optional
	BackendFrontname string `json:"backendFrontname,omitempty"`

	// passwordSecretRef points at a user-managed Secret whose `key`
	// (default `password`) contains the initial admin password. Leave
	// unset to have the operator generate a 20-character random password
	// once and store it in `<store>-admin` (key `password`). Password is
	// write-once — rotating it requires a bin/magento admin:user:unlock
	// flow, not a CRD edit. The operator does not watch this Secret for
	// changes post-install.
	// +optional
	PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// disableTFA opts out of two-factor authentication after
	// setup:install. When true, bootstrap appends:
	//   bin/magento config:set twofactorauth/general/enable 0
	//   bin/magento module:disable Magento_TwoFactorAuth Magento_AdminAdobeImsTwoFactorAuth
	//   bin/magento setup:upgrade
	// Production stores with SMTP configured (spec.smtp set) leave
	// this false and use real TFA enrolment via email. Demo manifests
	// without an SMTP relay set this true to keep admin login working.
	// +optional
	DisableTFA bool `json:"disableTFA,omitempty"`
}

// SecretKeySelector is a namespaced-local reference to a single key in
// a Secret — mirrors corev1.SecretKeySelector but keeps the MageStore
// CRD free of the upstream import, which would bring in the whole
// LocalObjectReference optionality dance we already avoid.
type SecretKeySelector struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// key defaults to "password" when unset.
	// +kubebuilder:default=password
	// +optional
	Key string `json:"key,omitempty"`
}

// DatabaseSpec is a discriminated union over the database-backend
// provisioning modes. ADR 004 establishes the pattern: exactly one
// sibling is set per MageStore. Phase-1 ships three siblings —
// `mariaDb` (inline — operator creates and owns a MariaDB CR),
// `mariaDbRef` (reference — admin pre-creates it), and `external`
// (unmanaged endpoint, typically a managed cloud database like RDS /
// Cloud SQL / Scaleway Managed DB). A future `perconaRef` sibling is
// anticipated but not implemented; the shape is designed so it can
// land without breaking existing CRs.
//
// All three siblings emit the same MAGENTO_DATABASE_* env var namespace
// through `ensureSecrets`. The image cannot tell which mode produced
// the credentials, and should not try.
// +kubebuilder:validation:XValidation:rule="(has(self.mariaDb) ? 1 : 0) + (has(self.mariaDbRef) ? 1 : 0) + (has(self.external) ? 1 : 0) == 1",message="exactly one of mariaDb, mariaDbRef, or external must be set"
type DatabaseSpec struct {
	// mariaDb asks the operator to provision a MariaDB CR inline, with
	// an ownerReference back to this MageStore. Deleting the MageStore
	// deletes the database. Tuned for Magento 2.4.x in-cluster deployments
	// (version 11.4, 1Gi PVC, operator-generated root password).
	// +optional
	MariaDb *InlineMariaDB `json:"mariaDb,omitempty"`

	// mariaDbRef names an externally-managed MariaDB CR (group
	// k8s.mariadb.com, version v1alpha1). The operator watches the CR
	// but does not own it; deleting the MageStore leaves the database
	// intact. Use this mode for shared clusters or when a dedicated
	// stateful-platform team owns the database lifecycle.
	// +optional
	MariaDbRef *LocalObjectReference `json:"mariaDbRef,omitempty"`

	// external is an unmanaged MySQL-protocol endpoint — typically a
	// managed cloud service (RDS, Cloud SQL, Scaleway Managed DB). The
	// operator emits host/port/credentials straight into the config
	// Secret; no CR is created or watched. The user is responsible for
	// the database's provisioning, backups, and upgrade lifecycle.
	// +optional
	External *ExternalDatabase `json:"external,omitempty"`
}

// InlineMariaDB is the curated subset of MariaDB CR fields exposed when
// the operator provisions the database on the MageStore's behalf. Small
// on purpose — the escape hatch for any field not listed here is the
// `mariaDbRef` sibling, which takes a full MariaDB CR. ADR 004 has the
// rationale; defaults tuned for Magento 2.4.x.
type InlineMariaDB struct {
	// version pins the MariaDB image tag. Magento 2.4.8's RDBMS support
	// matrix lists MariaDB 10.2-10.6 and 11.4; the mariadb-operator
	// default (currently 11.8) falls outside that set. We default to
	// 11.4 so `setup:install`'s version check passes. Must be a tag
	// present on the upstream image (`docker-registry1.mariadb.com/
	// library/mariadb:<tag>`).
	// +kubebuilder:default="11.4"
	// +optional
	Version string `json:"version,omitempty"`

	// storageSize is the PVC size for the MariaDB StatefulSet. Default
	// is 1Gi — enough for a Magento 2.4.8 database with a few thousand
	// products, not enough for a catalogue with millions of SKUs plus
	// order history. Graduate to `mariaDbRef` when sizing gets real.
	// +kubebuilder:default="1Gi"
	// +optional
	StorageSize string `json:"storageSize,omitempty"`

	// resources overrides the MariaDB container's resource
	// requests/limits. Default (requests 100m / 256Mi, limits 512Mi
	// memory) is dev-test-sized; production values belong on a
	// separately-managed CR via `mariaDbRef`.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ExternalDatabase describes an unmanaged MySQL-protocol endpoint. The
// operator performs no provisioning, no watching, and no readiness
// checks against the endpoint — it only validates that the credentials
// Secret exists and has the expected keys, and then threads the values
// into the config Secret. If the endpoint is unreachable at pod-boot
// time the image fails `setup:install` / page requests as normal; the
// operator does not attempt a TCP probe in this Phase-1 shape.
type ExternalDatabase struct {
	// host is the MySQL-protocol hostname (or address) the image
	// connects to. No protocol prefix, no trailing port — use `port`.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// port overrides the default MySQL-protocol port.
	// +kubebuilder:default=3306
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// databaseName is the MySQL schema Magento targets. Default is
	// `magento`, which matches the inline and ref paths. Set this only
	// when the managed database enforces a different naming convention.
	// +kubebuilder:default="magento"
	// +kubebuilder:validation:MinLength=1
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// credentialsSecretRef points at a user-managed Secret containing
	// the database credentials. The Secret MUST have keys `username`
	// and `password`; the operator reads them verbatim and surfaces a
	// DatabaseReady=False (CredentialSecretMissing / CredentialKeyEmpty)
	// condition if either is absent. The Secret is not watched — rotate
	// credentials out-of-band and trigger a reconcile.
	// +required
	CredentialsSecretRef LocalObjectReference `json:"credentialsSecretRef"`
}

// SearchSpec is a discriminated union over the search-backend provisioning
// modes. ADR 004 establishes the pattern: exactly one sibling is set per
// MageStore. Phase-1 ships three siblings — inline `openSearch` (operator
// creates & owns the OpenSearchCluster CR), `opensearchRef` (admin pre-
// creates it), and `external` (unmanaged endpoint, typically a managed
// cloud service like AWS OpenSearch Service or Bonsai).
//
// All three siblings emit the same MAGENTO_OPENSEARCH_* env var
// namespace through `ensureSecrets`. The image cannot tell which mode
// produced the credentials, and should not try.
// +kubebuilder:validation:XValidation:rule="(has(self.openSearch) ? 1 : 0) + (has(self.opensearchRef) ? 1 : 0) + (has(self.external) ? 1 : 0) == 1",message="exactly one of openSearch, opensearchRef, or external must be set"
type SearchSpec struct {
	// openSearch asks the operator to provision an OpenSearchCluster CR
	// inline, with an ownerReference back to this MageStore. Deleting the
	// MageStore deletes the cluster. Tuned for Magento 2.4.x in-cluster
	// deployments (plain HTTP, single-node, version 2.19). Users needing
	// fields outside this curated subset should graduate to opensearchRef.
	// +optional
	OpenSearch *InlineOpenSearch `json:"openSearch,omitempty"`

	// opensearchRef names an externally-managed OpenSearchCluster CR
	// (group opensearch.org, version v1). The operator watches the CR
	// but does not own it; deleting the MageStore leaves the cluster
	// intact. Admins typically use this mode for shared clusters or for
	// production deployments where the stateful-platform team owns the
	// search cluster lifecycle.
	// +optional
	OpenSearchRef *LocalObjectReference `json:"opensearchRef,omitempty"`

	// external is an unmanaged OpenSearch/Elasticsearch endpoint —
	// typically a managed cloud service (AWS OpenSearch Service, Bonsai,
	// Elastic Cloud). The operator emits host/port/credentials straight
	// into the config Secret; no CR is created or watched. The user is
	// responsible for the cluster's provisioning, sizing, and upgrade
	// lifecycle. Magento 2.4.x supports HTTP and HTTPS endpoints
	// transparently — set `port` to match the managed endpoint's
	// listener.
	// +optional
	External *ExternalSearch `json:"external,omitempty"`
}

// ExternalSearch describes an unmanaged OpenSearch/Elasticsearch
// endpoint. The operator performs no provisioning, no watching, and no
// readiness checks against the endpoint — it only validates the
// optional credentials Secret and threads the values into the config
// Secret. If the endpoint is unreachable at pod-boot time the image
// fails `setup:install` / page requests as normal; the operator does
// not attempt a TCP probe in this Phase-1 shape.
type ExternalSearch struct {
	// host is the OpenSearch hostname (or address) the image connects
	// to. No protocol prefix, no trailing port — use `port`.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// port overrides the default OpenSearch HTTP port. AWS OpenSearch
	// Service typically listens on 443; managed Bonsai endpoints commonly
	// expose 9200; in-cluster OpenSearch uses 9200.
	// +kubebuilder:default=9200
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// credentialsSecretRef is an optional reference to a user-managed
	// Secret carrying basic-auth credentials. Leave unset for clusters
	// without authentication (in-cluster Magento-only OpenSearch with
	// the security plugin disabled — the inline-mode default — is the
	// canonical no-auth case). When set, the Secret MUST have keys
	// `username` and `password`; the operator emits
	// MAGENTO_OPENSEARCH_USERNAME / MAGENTO_OPENSEARCH_PASSWORD env
	// vars and the image consumes them via env.php's getenv(). Surfaces
	// SearchReady=False (CredentialSecretMissing / CredentialKeyEmpty)
	// when set but unresolvable.
	// +optional
	CredentialsSecretRef *LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// InlineOpenSearch is the curated subset of OpenSearchCluster fields
// exposed when the operator provisions a cluster on the MageStore's
// behalf. The spec stays small on purpose — the escape hatch for any
// field not listed here is the `opensearchRef` sibling, which takes a
// full OpenSearchCluster CR. ADR 004 captures the rationale.
type InlineOpenSearch struct {
	// version pins the OpenSearch server image tag. Magento 2.4.x is not
	// certified against OpenSearch 3.x — the default stays on the 2.x
	// compatibility track. Must be a full semver tag that exists on the
	// upstream image (`docker.io/opensearchproject/opensearch:<tag>`);
	// bare minor series like "2.19" are not valid image tags.
	// +kubebuilder:default="2.19.2"
	// +optional
	Version string `json:"version,omitempty"`

	// replicas sizes the single node pool. The default is 3 because
	// opensearch-k8s-operator's bootstrap-pod flow races badly against
	// single-replica startup: the main pod's TCP readiness probe passes
	// before cluster formation completes, the upstream operator marks
	// `Status.Initialized=true`, deletes the bootstrap pod, and the
	// single remaining node deadlocks on `cluster_manager_not_discovered`
	// because the bootstrap pod it was configured to join is gone. With
	// >= 3 main replicas, a quorum survives the bootstrap departure and
	// the cluster reaches green. Setting replicas=1 is supported for
	// aggressive dev/test resource savings but known to be fragile;
	// prefer opensearchRef with a single-node OpenSearchCluster CR where
	// you can control discovery.type.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// jvmHeap is the -Xms and -Xmx value applied to the OpenSearch JVM.
	// Rule of thumb: roughly half the container memory limit, capped at
	// 31g per Elastic/OpenSearch guidance.
	// +kubebuilder:default="512m"
	// +optional
	JvmHeap string `json:"jvmHeap,omitempty"`

	// resources overrides container resource requests/limits. Default:
	// requests 250m / 768Mi, limits 1500Mi memory. CPU limits left unset
	// (CFS throttling is rarely what OpenSearch wants).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// CacheSpec is a discriminated union over cache-backend provisioning
// modes. ADR 004 establishes the pattern; ADR 009 reshapes the inline
// path to a directly-owned Valkey StatefulSet (no upstream Redis or
// Valkey operator dependency) — exactly one sibling is set per
// MageStore. Phase-1 ships three siblings — `redis` (inline — operator
// manages StatefulSet + optional Sentinel Deployment directly),
// `redisRef` (reference — admin-provisioned cache fronted by a Service
// in the namespace, operator-agnostic), and `external` (unmanaged
// Redis-protocol endpoint, typically a managed cloud service like
// ElastiCache / Memorystore / Upstash).
//
// Field names stay Redis-shaped (`redis`, `redisRef`) even though the
// inline image is Valkey. Wire-compat means the user-visible concept
// is Redis (per CLAUDE.md's naming guardrail — don't confuse users by
// renaming a well-understood term when the protocol is identical).
//
// All three siblings emit the same MAGENTO_REDIS_* env var namespace
// through `ensureSecrets`. Magento's three logical stores (default
// cache, page cache, session storage) all share a single Redis
// instance with different logical DB numbers (0/1/2) — the DB layout
// is documented in the image contract and baked into customer env.php,
// not plumbed through the operator as env vars. HA inline mode adds
// `MAGENTO_REDIS_SENTINEL_MASTER` to the namespace so env.php can
// switch the cache backend into Sentinel-discovery mode.
// +kubebuilder:validation:XValidation:rule="(has(self.redis) ? 1 : 0) + (has(self.redisRef) ? 1 : 0) + (has(self.external) ? 1 : 0) == 1",message="exactly one of redis, redisRef, or external must be set"
type CacheSpec struct {
	// redis asks the operator to manage the cache directly via a
	// Valkey StatefulSet (image valkey/valkey:8-alpine) with
	// ownerReferences back to this MageStore — no upstream Redis or
	// Valkey operator dependency. Topology selected by
	// `redis.replicas`: 1 for single-instance dev, 3 / 5 / 7 for
	// Sentinel HA with replicaof bootstrap and a separate Sentinel
	// Deployment. See ADR 009 for the topology and Sentinel bootstrap
	// rationale.
	// +optional
	Redis *InlineRedis `json:"redis,omitempty"`

	// redisRef points at an admin-provisioned cache in the same
	// namespace via its front-door Service. The operator emits
	// `MAGENTO_REDIS_HOST` from the Service name and reads the
	// password from a conventional `<service-name>` Secret (key
	// `password`). Topology and operator choice are the admin's
	// concern — the operator stays out of the way. Use this mode for
	// any third-party Redis or Valkey operator (SAP, OT-CONTAINER-KIT,
	// upstream valkey-io once it ships v1) that exposes a
	// Redis-protocol Service. Pre-v0.3.0 manifests pointing at a
	// hyperspike Valkey CR migrate to either inline mode or `external`.
	// +optional
	RedisRef *LocalObjectReference `json:"redisRef,omitempty"`

	// external is an unmanaged Redis-protocol endpoint — typically a
	// managed cloud service (ElastiCache, Memorystore, Upstash, Redis
	// Cloud). The operator emits host/port/password straight into the
	// config Secret; no CR is created or watched. The user is
	// responsible for the cache's provisioning, sizing, and failover.
	// +optional
	External *ExternalCache `json:"external,omitempty"`
}

// InlineRedis is the curated knobs exposed when the operator manages
// the cache StatefulSet directly (ADR 009). Two topologies: single-
// instance (`replicas: 1`, default for dev) and Sentinel HA
// (`replicas: 3 / 5 / 7`, recommended for production). Quorum is
// hard-coded at (N/2)+1 — odd-only replica counts avoid split-brain;
// 2 / 4 are rejected by the enum. Users wanting anything outside this
// envelope graduate to `redisRef` (point at a pre-existing Service
// from any third-party Redis/Valkey operator) or `external` (managed
// cloud endpoint).
type InlineRedis struct {
	// replicas selects the cache topology. 1 = single-instance bare
	// StatefulSet (dev sizing); 3 / 5 / 7 = Sentinel HA with ordinal-
	// aware `replicaof` bootstrap and a separate Sentinel Deployment.
	// Quorum is (N/2)+1, hard-coded — Magento doesn't need a knob here.
	//
	// Within-topology resize (e.g. 3 → 5 in Sentinel HA) is online:
	// same StatefulSet, Sentinel auto-discovers new replicas, quorum
	// recalculates. No special handling required.
	//
	// Cross-topology changes (1 ↔ 3+) are destructive and require
	// explicit opt-in: edit replicas AND set the annotation
	// `mage.aknostic.com/cache-topology-change: "approved"` on the
	// MageStore. The operator deletes the existing cache resources and
	// materialises the new topology fresh — cache is regenerable, but
	// any sessions stored in Redis are lost. Without the annotation,
	// the operator refuses the transition and surfaces
	// `CacheReady=False, Reason=TopologyChangeRequiresApproval` with
	// the supported workflow in the message. See ADR 009 §"What this
	// doesn't change."
	// +kubebuilder:default=1
	// +kubebuilder:validation:Enum=1;3;5;7
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// resources overrides the Valkey container's resource
	// requests/limits. Default (requests 50m / 64Mi, limits 256Mi
	// memory) is dev-test-sized; production values belong on a
	// separately-managed cache via `redisRef` or `external`.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ExternalCache describes an unmanaged Redis-protocol endpoint. The
// operator performs no provisioning, no watching, and no readiness
// checks — only credential-Secret validation (when set) and env var
// emission. If the endpoint is unreachable at pod-boot time the image
// fails page requests at first cache read; the operator does not
// attempt a TCP probe in this Phase-1 shape.
type ExternalCache struct {
	// host is the Redis-protocol hostname (or address) the image
	// connects to. No protocol prefix, no trailing port — use `port`.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// port overrides the default Redis-protocol port.
	// +kubebuilder:default=6379
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// passwordSecretRef is an optional reference to a user-managed
	// Secret carrying the Redis AUTH password. Leave unset for
	// password-less endpoints (many dev-mode Redis/ElastiCache
	// deployments skip auth; the operator surfaces that cleanly by
	// emitting no MAGENTO_REDIS_PASSWORD env var). When set, the
	// Secret's key (default `password`) is read verbatim and
	// surfaces a CacheReady=False (CredentialSecretMissing /
	// CredentialKeyEmpty) condition if absent or empty.
	// +optional
	PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`
}

// QueueSpec is a discriminated union over AMQP-broker provisioning
// modes. ADR 004 establishes the pattern: exactly one sibling is set
// per MageStore. Phase-1 ships three siblings — `rabbitMq` (inline —
// operator creates and owns a RabbitmqCluster CR via rabbitmq-
// cluster-operator), `rabbitMqRef` (reference — admin pre-creates
// the CR), and `external` (unmanaged AMQP endpoint, typically a
// managed cloud service like Amazon MQ / CloudAMQP).
//
// All three siblings emit the same MAGENTO_RABBITMQ_* env var
// namespace through `ensureSecrets`. Magento's Phase-1 queue usage
// is simple (default vhost, one connection) — the image-contract
// env vars stay minimal (host/port/vhost/username/password) and the
// queue topology (exchanges, bindings) is managed by Magento itself
// at setup:install time, not by the operator.
// +kubebuilder:validation:XValidation:rule="(has(self.rabbitMq) ? 1 : 0) + (has(self.rabbitMqRef) ? 1 : 0) + (has(self.external) ? 1 : 0) == 1",message="exactly one of rabbitMq, rabbitMqRef, or external must be set"
type QueueSpec struct {
	// rabbitMq asks the operator to provision a RabbitmqCluster CR
	// inline (group rabbitmq.com, kind RabbitmqCluster) with an
	// ownerReference back to this MageStore. Deleting the MageStore
	// deletes the broker. rabbitmq-cluster-operator auto-generates
	// the default-user Secret and publishes its coordinates on
	// Status.DefaultUser.SecretReference — the cleanest credentials
	// hand-off of the four composed operators.
	// +optional
	RabbitMq *InlineRabbitMQ `json:"rabbitMq,omitempty"`

	// rabbitMqRef names an externally-managed RabbitmqCluster CR
	// (group rabbitmq.com, kind RabbitmqCluster). The operator
	// watches the CR but does not own it; deleting the MageStore
	// leaves the broker intact. Use this mode for shared brokers or
	// HA-tuned clusters that live outside a single MageStore's
	// lifecycle.
	// +optional
	RabbitMqRef *LocalObjectReference `json:"rabbitMqRef,omitempty"`

	// external is an unmanaged AMQP endpoint — typically a managed
	// cloud service (Amazon MQ, CloudAMQP, self-hosted RabbitMQ not
	// operated by rabbitmq-cluster-operator). The operator emits
	// host/port/vhost/username/password straight into the config
	// Secret; no CR is created or watched.
	// +optional
	External *ExternalQueue `json:"external,omitempty"`
}

// InlineRabbitMQ is the curated subset of RabbitmqCluster CR fields
// exposed when the operator provisions the broker on the MageStore's
// behalf. Small on purpose — the escape hatch for any field not
// listed here is the `rabbitMqRef` sibling, which takes a full
// RabbitmqCluster CR. ADR 004 captures the rationale; defaults aim
// for dev-sized single-node broker.
type InlineRabbitMQ struct {
	// resources overrides the RabbitMQ container's resource
	// requests/limits. Default (requests 100m / 256Mi, limits 512Mi
	// memory) is dev-test-sized; production values belong on a
	// separately-managed RabbitmqCluster via `rabbitMqRef`.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ExternalQueue describes an unmanaged AMQP endpoint. The operator
// performs no provisioning, no watching, and no readiness checks
// against the endpoint — it only validates that the credentials
// Secret exists and has the expected keys, and then threads the
// values into the config Secret. If the endpoint is unreachable at
// pod-boot time the image fails at first queue publish; the operator
// does not attempt a TCP probe in this Phase-1 shape.
type ExternalQueue struct {
	// host is the AMQP hostname (or address) the image connects to.
	// No protocol prefix, no trailing port — use `port`.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// port overrides the default AMQP port.
	// +kubebuilder:default=5672
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// vhost is the AMQP virtual host Magento publishes to. Default is
	// "/" — the broker's implicit default vhost. Set this only when
	// the managed broker enforces a named vhost convention.
	// +kubebuilder:default="/"
	// +kubebuilder:validation:MinLength=1
	// +optional
	Vhost string `json:"vhost,omitempty"`

	// credentialsSecretRef points at a user-managed Secret containing
	// the AMQP credentials. The Secret MUST have keys `username` and
	// `password`; the operator reads them verbatim and surfaces a
	// QueueReady=False (CredentialSecretMissing / CredentialKeyEmpty)
	// condition if either is absent. The Secret is not watched —
	// rotate credentials out-of-band and trigger a reconcile. The key
	// names match rabbitmq-cluster-operator's default-user Secret
	// schema so a `credentialsSecretRef` pointed at a
	// RabbitmqCluster-generated Secret works without translation.
	// +required
	CredentialsSecretRef LocalObjectReference `json:"credentialsSecretRef"`
}

// WorkloadsSpec configures the Magento workload split. The Magento
// codebase ships a single container image that can serve all traffic
// classes (storefront, admin panel, REST / GraphQL API) off one pod.
// Splitting into per-class Deployments is a subtractive optimization
// — when `admin` or `api` is unset, their Ingress paths route to the
// frontend Service (see ADR 006). Frontend is required because a
// MageStore with no frontend is incoherent.
type WorkloadsSpec struct {
	// frontend is the storefront-serving workload.
	// +required
	Frontend WorkloadSpec `json:"frontend"`

	// admin is the admin-panel workload. When unset, admin-path
	// requests route to the frontend Service. Split out for stores
	// whose admin traffic has a different resource profile (heavy
	// reports, long-running mass actions) that must not contend with
	// storefront traffic.
	// +optional
	Admin *WorkloadSpec `json:"admin,omitempty"`

	// api is the REST / GraphQL API workload. When unset, /rest and
	// /graphql paths route to the frontend Service. Split out for
	// stores whose API traffic profile (headless storefronts, PWA
	// clients, integration partners) differs materially from
	// storefront web traffic.
	// +optional
	Api *WorkloadSpec `json:"api,omitempty"`

	// imagePullPolicy controls whether kubelet re-checks the registry
	// for the workload image's digest on every pod start. Default
	// (`Always`) protects against stale node-cached layers when the
	// reference image's tag is mutable (e.g. `:2.4.8-sampledata` is
	// rebuilt across releases) — `IfNotPresent` would let kubelet
	// skip the pull when any layer for that name:tag exists locally,
	// even if the registry's current digest has moved on. The
	// `Always` default adds one HEAD-only digest resolution per pod
	// start (KB on the wire, not the GB of a layer pull) which is
	// negligible for storefronts where pod restarts are rare in
	// steady state. Set to `IfNotPresent` for air-gapped registries
	// (kubelet cannot reach the registry on every pod start) or
	// rate-limited registries (anonymous Docker Hub pulls); the
	// operator does not validate digest-stability when this override
	// is in effect. Applies to every pod the operator runs from the
	// store's image: frontend / admin / api workloads, the ~20
	// queue-consumer Deployments, Varnish, and the one-shot Jobs
	// (bootstrap, smtp probe, store-urls, media seed, upgrade). Use
	// `IfNotPresent` or `Never` for images that exist only on the
	// nodes, such as images loaded with `kind load docker-image`.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=Always
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// WorkloadSpec is the shared shape for all three workload types
// (frontend, admin, api). A common struct lets Session 2's reconciler
// reuse a single Deployment-building helper. Type-specific extensions
// (e.g., admin's URL prefix once ADR 006 settles it) grow by splitting
// this struct rather than by polymorphic dispatch inside the helper.
type WorkloadSpec struct {
	// replicas is the desired replica count for this workload's
	// Deployment. When `autoscaling` is set, `replicas` seeds the
	// initial count but the HPA owns the steady-state value.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// resources overrides the workload container's resource
	// requests/limits. Operator defaults apply when unset. Production
	// sizing differs meaningfully across storefront, admin, and API
	// workloads and is expected to be explicit rather than inferred.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// autoscaling configures an HPA targeting this workload's
	// Deployment. When unset, the workload runs at the declared
	// `replicas` count without horizontal scaling.
	// +optional
	Autoscaling *WorkloadAutoscaling `json:"autoscaling,omitempty"`
}

// WorkloadAutoscaling configures the HPA the operator materialises
// for a workload when autoscaling is opted into. CPU-target-based
// for v0.1.0; memory-target and custom-metric support land when a
// user asks.
type WorkloadAutoscaling struct {
	// minReplicas is the HPA lower bound. When unset, the HPA uses
	// the workload's `replicas` value as its floor.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// maxReplicas is the HPA upper bound. Required — without a cap
	// autoscaling has no meaningful configuration.
	// +kubebuilder:validation:Minimum=1
	// +required
	MaxReplicas int32 `json:"maxReplicas"`

	// targetCPUUtilizationPercentage is the average CPU utilization
	// target driving scale decisions. Default 80% matches the
	// Kubernetes HPA default.
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

// ConsumersSpec configures the Magento queue consumer Deployments.
// Magento ships ~20 consumer processes
// (`product_action_attribute.update`, `async.operations.all`,
// `inventory.mass_update`, ...), each reading from a queue and
// performing a specific class of deferred work. Per-consumer
// Deployments are necessary because Magento consumers do not handle
// SIGTERM cleanly — they must run alone at `replicas: 1` with a
// generous `terminationGracePeriodSeconds` (see CLAUDE.md).
//
// Default observability (per-consumer metrics, logs, dashboards,
// alerts) is a v0.1.0 prerequisite once this field is populated:
// without it, operators cannot diagnose which of 20 Deployments
// caused a symptom. Scoped to Session 3 or a parallel observability
// track.
type ConsumersSpec struct {
	// enabled gates the entire consumer subsystem. When true, the
	// operator materialises one Deployment per known consumer with
	// baked-in per-consumer defaults. When false, no consumer
	// Deployments exist regardless of `overrides` contents —
	// overrides are preserved across toggles so tuning survives a
	// temporary disable.
	// +required
	Enabled bool `json:"enabled"`

	// overrides tunes per-consumer settings. Key = the consumer name
	// as published by `bin/magento queue:consumers:list`. Unknown
	// keys (typos, renamed consumers, customer-specific additions
	// not in the operator's known-good list) surface at reconcile
	// time via `ConsumersReady=False,
	// Reason=UnknownConsumerOverride` — not CRD-validation time,
	// because CEL cannot validate map keys against an open set.
	// +optional
	Overrides map[string]ConsumerOverride `json:"overrides,omitempty"`
}

// ConsumerOverride is the tunable subset exposed per consumer. Kept
// small on purpose — the operator's baked-in defaults cover the
// common case, and growing this struct prematurely invites users to
// micro-manage things the operator should manage.
type ConsumerOverride struct {
	// resources overrides the consumer container's resource
	// requests/limits. The operator's per-consumer defaults (tuned
	// for average-sized catalogues) apply when unset. Frequently-
	// tuned consumers: `product_action_attribute.update` (memory-
	// hungry on million-SKU catalogues), `async.operations.all`
	// (CPU-hungry on bulk-API-heavy stores).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// CronSpec configures the Magento per-group cron CronJobs. Magento
// partitions cron work into groups (`default`, `index`, `consumers`,
// `staging`, `catalog_event`), and each group has a materially
// different resource profile — `index` is CPU/memory-heavy (reindex
// is long-running), `default` drives lightweight tasks like sitemap
// generation and email dispatch. Per-group CronJobs let the
// operator ship sensible defaults and let users tune heavy groups
// independently without affecting cheap ones.
type CronSpec struct {
	// enabled gates the entire cron subsystem. When true, the
	// operator materialises one CronJob per known group (minus any
	// that a per-group override disables). When false, no CronJobs
	// exist regardless of `overrides`.
	// +required
	Enabled bool `json:"enabled"`

	// overrides tunes per-group settings (schedule, resources,
	// disabled). Key = the cron group name as published by
	// `bin/magento cron:run --help`. Unknown keys surface at
	// reconcile time via `CronReady=False,
	// Reason=UnknownCronGroupOverride`.
	// +optional
	Overrides map[string]CronOverride `json:"overrides,omitempty"`
}

// CronOverride exposes the per-group tunable surface. Ships with a
// `disabled` field from day one to cover stores that disable the
// default `index` group because they run an external indexing
// service, or that disable `consumers` because they drive consumers
// directly via `spec.consumers`.
type CronOverride struct {
	// schedule overrides the operator's baked-in cron expression
	// for this group. Standard 5-field cron syntax (minute, hour,
	// day-of-month, month, day-of-week). Example: `*/2 * * * *` for
	// every two minutes.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// resources overrides the CronJob container's resource
	// requests/limits. The operator's per-group defaults (cautious
	// for `default`; 2Gi memory for `index`; moderate CPU for
	// `consumers`) apply when unset.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// disabled skips this group entirely when the parent
	// `spec.cron.enabled` is true. Useful for stores using an
	// external indexing service (disable `index`), for temporarily
	// quieting a noisy group during maintenance, or for shops that
	// drive consumers via `spec.consumers` rather than the
	// `consumers` cron group.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
}

// StoreSpec identifies one Magento store scope by public hostname,
// Magento store code, and scope type. Used to generate the nginx
// `map $http_host $MAGE_RUN_CODE` block and per-host Ingress rules —
// see ADR 007. Multi-store Magento deployments (multiple websites
// in one codebase) set `spec.stores` with one entry per public
// host; separate codebases with independent DBs get separate
// MageStore CRs.
type StoreSpec struct {
	// host is the public hostname this store entry responds to.
	// Exact Host-header match; no wildcard support. The operator
	// generates one Ingress rule per host and one nginx map entry
	// per host.
	// +kubebuilder:validation:MinLength=1
	// +required
	Host string `json:"host"`

	// code is the Magento store code (as configured in the Magento
	// admin or `setup:install`) that `MAGE_RUN_CODE` will be set to
	// for requests with this host. Must match an existing Magento
	// scope of the declared `type`.
	// +kubebuilder:validation:MinLength=1
	// +required
	Code string `json:"code"`

	// type is the Magento scope type for `code`. `website` selects a
	// website scope (most common for multi-domain EU shops), `store`
	// selects a store group, `store_view` selects an individual
	// store view (common for language-variant storefronts on a
	// single domain). Maps verbatim to `MAGE_RUN_TYPE`.
	// +kubebuilder:validation:Enum=website;store;store_view
	// +required
	Type string `json:"type"`
}

// MediaSpec — three-way discriminator. Exactly one of EmptyDir, Pvc,
// S3 must be set when MediaSpec is present. Per ADR 011.
//
// SeedFromImage is orthogonal — it populates the configured backend
// with the workload image's /app/pub/media-baked/* on initial install
// (initContainer for emptyDir/PVC; one-shot Job for S3, see C2.2).
//
// When spec.media is nil (block omitted), the operator falls back to
// per-pod emptyDir scratch — this back-compat shape pre-dates the
// explicit emptyDir form. spec.media: {} (empty block) is now invalid
// (CEL XValidation rejects it).
// +kubebuilder:validation:XValidation:rule="(has(self.emptyDir) ? 1 : 0) + (has(self.pvc) ? 1 : 0) + (has(self.s3) ? 1 : 0) == 1",message="exactly one of emptyDir, pvc, or s3 must be set"
type MediaSpec struct {
	// emptyDir is per-pod scratch. Marker struct, reserved for future
	// tuning fields (sizeLimit, medium).
	// +optional
	EmptyDir *MediaEmptyDir `json:"emptyDir,omitempty"`

	// pvc configures a ReadWriteOnce PVC the operator creates and
	// mounts at pub/media on each workload Deployment that needs
	// media access. Known issue: the workload split (frontend /
	// admin / api) would require ReadWriteMany for consistent
	// media across Deployments, and RWX Magento pub/media is known
	// to corrupt at scale. Dev/test only — production stores
	// should use `s3`.
	// +optional
	Pvc *MediaPVC `json:"pvc,omitempty"`

	// s3 configures an S3-compatible object-storage backend (AWS
	// S3, Scaleway Object Storage, Wasabi, Minio, Cloudflare R2,
	// ...). The operator threads bucket coordinates + credentials
	// into the per-workload config Secret; the image's env.php
	// wires Magento's remote storage driver from them. The bucket
	// itself is not provisioned — users create it out-of-band.
	// +optional
	S3 *MediaS3 `json:"s3,omitempty"`

	// seedFromImage populates the configured backend with the workload
	// image's /app/pub/media-baked/* on initial install. Volume modes
	// (emptyDir, pvc) seed via initContainer; S3 mode runs a one-shot
	// Job (see ensureMediaSeed). Per ADR 011.
	// +optional
	SeedFromImage bool `json:"seedFromImage,omitempty"`
}

// MediaEmptyDir is a marker struct for the explicit emptyDir mode.
// Empty for now; reserved for future fields (sizeLimit, medium).
type MediaEmptyDir struct{}

// MediaS3 describes an unmanaged S3-compatible bucket. External-
// like shape — operator performs no provisioning, no watching,
// and no readiness checks against the endpoint. Credential-Secret
// schema is fixed (`accessKeyId` + `secretAccessKey`); IAM-role /
// anonymous access is not supported in Phase 1.
type MediaS3 struct {
	// bucket is the S3 bucket name. The operator does not create
	// or delete the bucket; users provision it out-of-band.
	// +kubebuilder:validation:MinLength=1
	// +required
	Bucket string `json:"bucket"`

	// endpoint is the S3-compatible endpoint URL (including
	// protocol). Leave unset for AWS S3 — the SDK picks the
	// regional endpoint from `region`. Set for non-AWS providers,
	// e.g. `https://s3.fr-par.scw.cloud` (Scaleway) or
	// `https://minio.example.com` (self-hosted Minio).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// region is the AWS region (or provider-specific region tag)
	// holding the bucket. Required by the AWS SDK v2 even for
	// non-AWS endpoints — pass the provider's regional identifier.
	// +kubebuilder:validation:MinLength=1
	// +required
	Region string `json:"region"`

	// credentialsSecretRef points at a user-managed Secret with
	// keys `accessKeyId` and `secretAccessKey`. Schema matches the
	// AWS SDK v2 default credential shape; buckets requiring IAM-
	// role or anonymous access are not supported in Phase 1. If
	// the Secret is absent or either key is empty, `ensureMedia`
	// returns an error and the reconcile fails; the failure
	// surfaces via `Ready=False` (no media-specific condition
	// exists — media is a per-pod volume decision, not a
	// rollout-gate condition; see `ensureMedia`'s doc-comment).
	// +required
	CredentialsSecretRef LocalObjectReference `json:"credentialsSecretRef"`
}

// MediaPVC describes an operator-provisioned PVC mounted at
// pub/media. Inline-like shape — the operator creates the PVC with
// an `ownerReference` back to the MageStore, so deleting the
// MageStore deletes the media (intentional: the PVC path is
// dev/test only and losing the media with the demo is desired).
// Production stores use `s3`.
type MediaPVC struct {
	// storageClassName selects the StorageClass for the PVC.
	// Leave unset to use the cluster's default StorageClass. Set
	// explicitly for clusters with multiple StorageClasses where
	// the default isn't the right choice (e.g., a cheap-slow class
	// for media vs. a fast-SSD class for the database).
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// size is the PVC's requested capacity. Default 10Gi covers a
	// modest-catalogue demo; stores with image-heavy catalogues
	// should set this explicitly or graduate to `s3`. Must be a
	// valid Kubernetes resource.Quantity string (e.g. `10Gi`,
	// `100Gi`, `1Ti`).
	// +kubebuilder:default="10Gi"
	// +optional
	Size string `json:"size,omitempty"`
}

// SMTPSpec is a discriminated union — exactly one of Inline or
// SecretRef MUST be set when the block is present. Per ADR 012.
// +kubebuilder:validation:XValidation:rule="(has(self.inline) ? 1 : 0) + (has(self.secretRef) ? 1 : 0) == 1",message="exactly one of inline or secretRef must be set"
type SMTPSpec struct {
	// Inline mode: operator constructs a Secret with the SMTP_*
	// keys derived from the fields below.
	// +optional
	Inline *SMTPInline `json:"inline,omitempty"`

	// SecretRef mode: customer-managed Secret with the SMTP_* keys
	// directly. Operator validates SMTP_HOST + SMTP_FROM key
	// presence at reconcile time.
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// SMTPInline carries the spec fields the operator uses to construct
// the per-store SMTP Secret. Per gap #11 admin-email-defaulting
// policy (locked 2026-05-04): NO operator-side fallback to
// bootstrap.admin.email for SMTP_FROM.
// +kubebuilder:validation:XValidation:rule="has(self.user) == has(self.passwordSecretRef)",message="user and passwordSecretRef must both be set or both unset"
type SMTPInline struct {
	// Required.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Required. Default From: email and SMTP envelope MAIL FROM.
	// +kubebuilder:validation:MinLength=1
	From string `json:"from"`

	// Optional. Default From: header display name.
	// +optional
	FromName string `json:"fromName,omitempty"`

	// Optional. Default 587.
	// +kubebuilder:default=587
	// +optional
	Port int32 `json:"port,omitempty"`

	// Optional. Anonymous / IP-allowlist relays leave User and
	// PasswordSecretRef unset. CEL enforces parity.
	// +optional
	User string `json:"user,omitempty"`
	// +optional
	PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// Optional. Default "login".
	// +kubebuilder:validation:Enum=login;plain;cram-md5
	// +kubebuilder:default=login
	// +optional
	Auth string `json:"auth,omitempty"`

	// Optional. Default "starttls".
	// +kubebuilder:validation:Enum=starttls;tls;off
	// +kubebuilder:default=starttls
	// +optional
	TLS string `json:"tls,omitempty"`

	// Optional. Per-identity From: header overrides.
	// +optional
	FromIdentities *SMTPFromIdentities `json:"fromIdentities,omitempty"`
}

// SMTPFromIdentities maps Magento's 5 built-in trans_email identities
// to per-identity From: email + display name. Closed set; mirrors
// trans_email/ident_*/{email,name} keys.
type SMTPFromIdentities struct {
	// +optional
	General *SMTPIdentity `json:"general,omitempty"`
	// +optional
	Sales *SMTPIdentity `json:"sales,omitempty"`
	// +optional
	Support *SMTPIdentity `json:"support,omitempty"`
	// +optional
	Custom1 *SMTPIdentity `json:"custom1,omitempty"`
	// +optional
	Custom2 *SMTPIdentity `json:"custom2,omitempty"`
}

// SMTPIdentity carries one Magento identity's From: email + display name.
// MinLength=1 on each field means "if you set the key, you mean it" —
// blank values would silently produce empty SMTP_FROM_<IDENT> /
// SMTP_FROM_NAME_<IDENT> env vars.
type SMTPIdentity struct {
	// +kubebuilder:validation:MinLength=1
	// +optional
	Email string `json:"email,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +optional
	Name string `json:"name,omitempty"`
}

// VarnishSpec configures the optional Varnish caching sidecar.
// When `spec.varnish` is unset, no Varnish is materialised and
// Ingress points directly at the frontend Service — the Slot-20
// demo shape. When set with `enabled: true`, the operator
// creates a `<store>-varnish` Deployment + Service, generates a
// default VCL (or uses `vclConfigMapRef` verbatim), and reroutes
// Ingress through it. Varnish is cache (not data) — a restart
// costs a cold cache, not data loss — so the default `replicas: 2`
// is for HA availability rather than data durability.
//
// Default VCL is operator-generated per Magento version,
// extracted from `bin/magento varnish:vcl:generate` plus
// production hardening. When `spec.stores` is set, the default
// VCL embeds per-host backend selection — see ADR 007 for the
// generated pattern. Custom VCL supplied via `vclConfigMapRef` is
// used verbatim; multi-store authors must include their own
// per-host backend mapping.
type VarnishSpec struct {
	// enabled gates the Varnish subsystem. When false, no Varnish
	// resources are materialised and Ingress points directly at
	// the frontend Service. When true, the operator creates the
	// Varnish Deployment, Service, and VCL ConfigMap (when
	// generated). Required so the meaning of `spec.varnish: {}` is
	// explicit rather than defaulting silently.
	// +required
	Enabled bool `json:"enabled"`

	// replicas is the Varnish Deployment replica count. Default 2
	// because `enabled: true` is an explicit opt-in that presumes
	// prod-ish intent; demo-only deployments wanting a single
	// Varnish instance should set `replicas: 1` explicitly.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// version pins the Varnish image tag. Must be a valid tag on
	// the upstream image (`docker.io/library/varnish:<tag>`). VCL
	// syntax varies across Varnish major versions, so pinning
	// matters when a custom VCL is supplied via
	// `vclConfigMapRef`.
	// +kubebuilder:default="7.5"
	// +optional
	Version string `json:"version,omitempty"`

	// vclConfigMapRef points at a user-managed ConfigMap holding
	// the full VCL file (expected key: `default.vcl`). When unset,
	// the operator generates VCL tuned for the Magento version and
	// — when `spec.stores` is set — with per-host backend
	// selection baked in (ADR 007). When set, the operator uses
	// the supplied VCL verbatim and does NOT modify it; custom VCL
	// authors wanting multi-store support must include the per-
	// host mapping themselves.
	// +optional
	VclConfigMapRef *LocalObjectReference `json:"vclConfigMapRef,omitempty"`

	// resources overrides the Varnish container's resource
	// requests/limits. Operator defaults (conservative — requests
	// 100m / 256Mi, limits 512Mi memory) are demo-sized; production
	// Varnish sizing depends on catalogue size and hit-rate targets
	// and is expected to be explicit.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MageStoreImage identifies the Magento container image.
type MageStoreImage struct {
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// +kubebuilder:validation:MinLength=1
	Tag string `json:"tag"`
}

// LocalObjectReference points to a Kubernetes resource in the same namespace.
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// MageStoreUpgradeSpec governs how `spec.image.tag` drift is turned into a
// Magento `setup:upgrade` arc. See ADR 008 for the orchestration design;
// Subsession A lands the surface, Subsession B's `ensureUpgrade` is the
// first reader.
type MageStoreUpgradeSpec struct {
	// maintenance controls the maintenance window that fences traffic
	// during an upgrade. Default behaviour fences traffic via a 503
	// page (Service-selector flip to a one-replica nginx Deployment);
	// opt-out exists for blue-green / canary admins who explicitly
	// accept the integrity risk of mid-migration writes.
	// +optional
	Maintenance *MageStoreUpgradeMaintenanceSpec `json:"maintenance,omitempty"`
}

// MageStoreUpgradeMaintenanceSpec configures the maintenance window the
// operator wraps around `setup:upgrade`. The `Enabled *bool` shape is the
// first instance of pointer-bool in this codebase; required so the
// kubebuilder default applies only when the field is omitted (a plain
// `bool` collapses "user set false" with "user did not specify").
type MageStoreUpgradeMaintenanceSpec struct {
	// enabled toggles the maintenance window during an upgrade. Default
	// true — disabling is opt-in for shops running genuine blue-green
	// or canary upgrades that explicitly accept the integrity risk of
	// mid-migration writes against a non-transactional `setup:upgrade`.
	// Pointer type so the +kubebuilder:default applies on omission only;
	// explicit `false` is preserved through API server roundtrip.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// htmlConfigMapRef optionally points at a ConfigMap with a custom
	// 503 HTML page. The operator mounts the named key into the
	// maintenance pod. When unset, the operator's baked-in default 503
	// page is served.
	// +optional
	HtmlConfigMapRef *MaintenanceHtmlConfigMapRef `json:"htmlConfigMapRef,omitempty"`
}

// MaintenanceHtmlConfigMapRef references a ConfigMap key carrying the
// HTML body served by the maintenance Deployment during an upgrade.
type MaintenanceHtmlConfigMapRef struct {
	// name is the ConfigMap name (same namespace as the MageStore).
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// key is the ConfigMap data key holding the HTML payload.
	// +kubebuilder:default=index.html
	// +optional
	Key string `json:"key,omitempty"`
}

// MageStorePhase is a coarse lifecycle summary. Programmatic consumers should
// read Conditions instead.
// +kubebuilder:validation:Enum=Pending;Provisioning;Upgrading;Running;Failed
type MageStorePhase string

const (
	PhasePending      MageStorePhase = "Pending"
	PhaseProvisioning MageStorePhase = "Provisioning"
	PhaseUpgrading    MageStorePhase = "Upgrading"
	PhaseRunning      MageStorePhase = "Running"
	PhaseFailed       MageStorePhase = "Failed"
)

// MageStoreStatus is the observed state of a MageStore.
type MageStoreStatus struct {
	// +optional
	Phase MageStorePhase `json:"phase,omitempty"`

	// observedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// observedImageTag is the image tag the schema is currently
	// consistent with. Empty before first install completes; advances
	// on successful `setup:upgrade` Job completion. Never advances on
	// Job failure — the failure path keeps the prior value so the next
	// reconcile re-evaluates the same upgrade arc. Subsession B's
	// `ensureUpgrade` is the first writer; until then this field
	// remains unset. See ADR 008.
	// +optional
	ObservedImageTag string `json:"observedImageTag,omitempty"`

	// conditions carry detailed readiness state. Expected types
	// (in pipeline order, with Ready as the rollup):
	// DatabaseReady, SearchReady, CacheReady, QueueReady,
	// SecretsReady, StoreInstalled, WorkloadsReady, VarnishReady,
	// ConsumersReady, CronReady, UpgradeReady, Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// jobAttempts tracks operator-initiated retry state for owned
	// one-shot Jobs (media-seed, smtp-probe, bootstrap, store-urls,
	// upgrade). Keyed by Job name. The operator deletes and
	// recreates a terminal-Failed Job after an exponential backoff
	// window elapses, ramping across recreations until the cap is
	// reached. See v0.6.2 punch list (gap #1) for the schedule.
	// +optional
	JobAttempts map[string]JobAttemptStatus `json:"jobAttempts,omitempty"`
}

// JobAttemptStatus tracks operator-initiated retry state for a single
// owned one-shot Job. Keyed by Job name in MageStoreStatus.JobAttempts.
type JobAttemptStatus struct {
	// count is the number of terminal-Failed observations the operator
	// has recorded for this Job name across recreations. Resets to 0
	// on successful Job completion. Drives the exponential backoff
	// schedule used by the job-recovery helper.
	// +kubebuilder:default=0
	Count int `json:"count"`

	// lastFailedAt is the wall-clock time at which the operator first
	// observed the most recent terminal-Failed Job. Cleared (set
	// implicitly to nil) after the operator deletes the Job; the next
	// terminal-Failed observation re-stamps it.
	// +optional
	LastFailedAt *metav1.Time `json:"lastFailedAt,omitempty"`

	// nextAttemptAt is the wall-clock time at or after which the
	// operator may delete the terminal-Failed Job. The value already
	// includes ±25 % jitter so multiple MageStores in lockstep don't
	// retry simultaneously. Cleared after deletion.
	// +optional
	NextAttemptAt *metav1.Time `json:"nextAttemptAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ms
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image.repository`
// +kubebuilder:printcolumn:name="Tag",type=string,JSONPath=`.spec.image.tag`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MageStore is the Schema for the magestores API
type MageStore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of MageStore
	// +required
	Spec MageStoreSpec `json:"spec"`

	// status defines the observed state of MageStore
	// +optional
	Status MageStoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MageStoreList contains a list of MageStore
type MageStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MageStore `json:"items"`
}

// RoutingSpec carries Gateway-layer configuration. Currently only
// `tls` is supported; Phase 2 fields per ADR 010's open questions
// (gatewayRef, gatewayClassName) graduate alongside their reconcile
// changes.
type RoutingSpec struct {
	// tls configures public-edge TLS termination on the per-MageStore
	// Gateway. When unset, the Gateway listens on HTTP/80 only
	// (v0.4.x default). When set, the operator adds per-host HTTPS
	// listeners on port 443 with `mode: Terminate` and references the
	// cert Secret per the discriminated-union below. See ADR 010.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`
}

// TLSSpec is a discriminated union — exactly one of `issuerRef` or
// `certificateSecretRef` MUST be set. Mirrors ADR 004's
// `inline | ref | external` shape used elsewhere in the CRD.
//
// `issuerRef` opts into cert-manager's Gateway API shim
// (cert-manager 1.14+): the operator annotates the Gateway with
// `cert-manager.io/cluster-issuer` (or `cert-manager.io/issuer`),
// cert-manager provisions a Certificate per host into a Secret named
// `<store>-gateway-tls-<host-slug>`, and the listener references it.
// Renewal is cert-manager's responsibility.
//
// `certificateSecretRef` references a user-provided Secret of type
// `kubernetes.io/tls`. The operator does not validate cert contents
// (X.509 parsing in the controller is over-engineering); misconfig
// surfaces as Envoy listener errors.
// +kubebuilder:validation:XValidation:rule="(has(self.issuerRef) ? 1 : 0) + (has(self.certificateSecretRef) ? 1 : 0) == 1",message="exactly one of issuerRef or certificateSecretRef must be set"
type TLSSpec struct {
	// issuerRef points at a cert-manager Issuer or ClusterIssuer.
	// Mutually exclusive with `certificateSecretRef`.
	// +optional
	IssuerRef *IssuerRefSpec `json:"issuerRef,omitempty"`

	// certificateSecretRef points at a user-provided `kubernetes.io/tls`
	// Secret in the same namespace as the MageStore. The Secret MUST
	// contain `tls.crt` and `tls.key` keys; multi-store deployments
	// require a SAN or wildcard cert covering every `spec.stores[].host`.
	// Mutually exclusive with `issuerRef`.
	// +optional
	CertificateSecretRef *LocalObjectReference `json:"certificateSecretRef,omitempty"`
}

// IssuerRefSpec identifies a cert-manager Issuer or ClusterIssuer.
// The operator translates this into the corresponding annotation
// on the Gateway resource and lets cert-manager handle the rest.
type IssuerRefSpec struct {
	// kind is the cert-manager resource kind. Must be either
	// `Issuer` (namespaced) or `ClusterIssuer` (cluster-wide).
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	// +required
	Kind string `json:"kind"`

	// name is the Issuer or ClusterIssuer resource name.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// SecuritySpec carries operator-wide security defaults. See ADR 014
// for the why-one-group decision and the shared upgrade-safety
// defaulting rule that applies across all sub-blocks. The
// `<store>-config` Secret existence check distinguishes fresh stores
// (defaults apply) from existing stores (opt-in via explicit field
// values). Fields below are CRD shape only as of v0.7.0 session 1;
// controller wiring lands in subsequent sessions of the v0.7.0 arc.
type SecuritySpec struct {
	// networkPolicies controls per-MageStore NetworkPolicy generation
	// (gap #1 of the v0.7.0 arc). When unset, no policies are
	// generated. When set with `enabled: true`, the operator emits
	// 6-8 NetworkPolicies covering the documented communication graph
	// (Internet → Gateway → Varnish; Varnish → workloads; workloads →
	// resolved data tiers; consumers/cron same data-tier reach minus
	// inbound HTTP; every pod → kube-dns; image-pull egress).
	// Implementation lands in session 2/3 of the v0.7.0 arc — this
	// field is CRD shape only as of v0.7.0 session 1.
	// +optional
	NetworkPolicies *NetworkPoliciesSpec `json:"networkPolicies,omitempty"`

	// workloadSecurityContext overrides the operator-emitted pod and
	// container security context defaults applied to every workload
	// PodTemplateSpec (frontend, admin, api, consumers, cron,
	// bootstrap, smtp, mediaSeed, storeUrls, upgrade, varnish). When
	// unset, no security context is applied — workload pods inherit
	// cluster defaults (root by default). When set, operator defaults
	// apply (`runAsNonRoot: true`, UID 1000, read-only root, ALL caps
	// dropped, RuntimeDefault seccomp) and user-supplied fields merge
	// on top. Implementation lands in session 2 of the v0.7.0 arc —
	// this field is CRD shape only as of v0.7.0 session 1.
	// +optional
	WorkloadSecurityContext *WorkloadSecurityContextSpec `json:"workloadSecurityContext,omitempty"`
}

// NetworkPoliciesSpec configures the per-MageStore NetworkPolicy
// generation introduced as gap #1 of the v0.7.0 arc.
//
// CRD shape lands in v0.7.0 session 1 alongside SecuritySpec; the
// `ensureNetworkPolicies` controller that consumes these fields is
// part of session 2/3.
type NetworkPoliciesSpec struct {
	// enabled toggles NetworkPolicy generation. Operator-side
	// upgrade-safety defaulting flips this to true for fresh stores
	// (no `<store>-config` Secret yet) and leaves it at the
	// user-set or zero value (effectively false) for existing
	// stores. Existing stores opt in by setting `enabled: true`.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// additionalIngressNamespaces lists namespaces whose pods may
	// reach workload pods directly (bypassing Varnish). Use for
	// monitoring scrapers, service meshes, CI sidecars. Each entry
	// is a `LabelSelector` matched against namespace labels.
	// +optional
	AdditionalIngressNamespaces []metav1.LabelSelector `json:"additionalIngressNamespaces,omitempty"`

	// additionalEgressCIDRs lists CIDRs the workload pods may egress
	// to beyond the operator-resolved data tiers. Use for image
	// registries, package mirrors, payment APIs, external SMTP
	// relays. The operator does not inspect overlap with private
	// ranges or the cluster pod CIDR — the user owns the safety of
	// what is listed here.
	// +optional
	AdditionalEgressCIDRs []string `json:"additionalEgressCIDRs,omitempty"`
}

// WorkloadSecurityContextSpec configures the pod and container
// securityContext applied to every workload PodTemplateSpec emitted
// by the operator (gap #2 of the v0.7.0 arc).
//
// Merge semantics: operator defaults apply (`runAsNonRoot: true`,
// UID 1000, read-only root, ALL caps dropped, RuntimeDefault
// seccomp). User-set fields in `Pod` / `Container` win at the field
// level — unspecified fields keep operator defaults. This lets
// users punch holes (e.g. a customer image that requires
// `runAsUser: 33`) without re-declaring every field.
//
// CRD shape lands in v0.7.0 session 1; the ensure-* helpers that
// consume these defaults land in session 2 of the v0.7.0 arc.
type WorkloadSecurityContextSpec struct {
	// pod is merged on top of the operator default
	// PodSecurityContext for every workload pod. User-set fields win
	// at the field level; unspecified fields keep operator defaults.
	// +optional
	Pod *corev1.PodSecurityContext `json:"pod,omitempty"`

	// container is merged on top of the operator default
	// SecurityContext applied to every workload container. Same
	// field-level merge as `pod`.
	// +optional
	Container *corev1.SecurityContext `json:"container,omitempty"`
}

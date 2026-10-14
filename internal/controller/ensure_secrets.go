package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// cryptKeyPrefix is the marker Magento's Encryptor looks for to detect a
// base64-encoded random key (see ADR 003). `Encryptor::decodeKey` strips
// this literal prefix and base64-decodes the remainder into 32 raw bytes
// for sodium. The operator emits "base64<base64-of-32-raw-bytes>" so the
// customer's env.php can use getenv("MAGENTO_CRYPT_KEY") verbatim.
const cryptKeyPrefix = "base64"

// Per-store Secret name and expected keys. The namespace follows the
// Click & Mortar reference env.php convention (see ADR 002).
const (
	configSecretSuffix        = "-config"
	envKeyDBHost              = "MAGENTO_DATABASE_HOST"
	envKeyDBPort              = "MAGENTO_DATABASE_PORT"
	envKeyDBName              = "MAGENTO_DATABASE_NAME"
	envKeyDBUsername          = "MAGENTO_DATABASE_USERNAME"
	envKeyDBPassword          = "MAGENTO_DATABASE_PASSWORD"
	envKeyMageMode            = "MAGE_MODE"
	envKeyCryptKey            = "MAGENTO_CRYPT_KEY"
	envKeySearchHost          = "MAGENTO_OPENSEARCH_HOST"
	envKeySearchPort          = "MAGENTO_OPENSEARCH_PORT"
	envKeySearchUsername      = "MAGENTO_OPENSEARCH_USERNAME"
	envKeySearchPassword      = "MAGENTO_OPENSEARCH_PASSWORD"
	envKeyCacheHost           = "MAGENTO_REDIS_HOST"
	envKeyCachePort           = "MAGENTO_REDIS_PORT"
	envKeyCachePassword       = "MAGENTO_REDIS_PASSWORD"
	envKeyCacheSentinelMaster = "MAGENTO_REDIS_SENTINEL_MASTER"
	envKeyQueueHost           = "MAGENTO_RABBITMQ_HOST"
	envKeyQueuePort           = "MAGENTO_RABBITMQ_PORT"
	envKeyQueueVhost          = "MAGENTO_RABBITMQ_VHOST"
	envKeyQueueUsername       = "MAGENTO_RABBITMQ_USERNAME"
	envKeyQueuePassword       = "MAGENTO_RABBITMQ_PASSWORD"
)

// ensureSecrets emits the operator-owned Secret that carries the env
// vars Magento pods load at startup. Phase-1 covers envvars mode (the
// default per ADR 002) and the database + search + cache + queue
// slices of the namespace — remote storage follows its own ensure*
// step.
//
// The step is endpoint-driven: database, search, cache, and queue
// credentials arrive as resolved *Endpoint structs from their
// respective ensure* steps. This step does not know or care which
// backend mode (inline / ref / external) produced the values — that's
// the point of the ADR-004 union and what keeps Phase-1 image contract
// stable across configurations.
//
// MAGENTO_CRYPT_KEY is generated once on Secret creation and preserved
// across reconciles. Rotating it silently would re-encrypt-with-old and
// leave the app unable to decrypt its own config.
func (r *MageStoreReconciler) ensureSecrets(
	ctx context.Context,
	store *magev1alpha1.MageStore,
	dbReady, searchReady, cacheReady, queueReady bool,
	database DatabaseEndpoint,
	search SearchEndpoint,
	cache CacheEndpoint,
	queue QueueEndpoint,
	mediaEnvVars map[string]string,
) (metav1.Condition, string, error) {
	if !dbReady {
		return metav1.Condition{
			Type:    ConditionSecretsReady,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingDatabase",
			Message: "holding config Secret render until database is ready",
		}, "", nil
	}
	if !searchReady {
		return metav1.Condition{
			Type:    ConditionSecretsReady,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingSearch",
			Message: "holding config Secret render until search backend is ready",
		}, "", nil
	}
	if !cacheReady {
		return metav1.Condition{
			Type:    ConditionSecretsReady,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingCache",
			Message: "holding config Secret render until cache backend is ready",
		}, "", nil
	}
	if !queueReady {
		return metav1.Condition{
			Type:    ConditionSecretsReady,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingQueue",
			Message: "holding config Secret render until queue backend is ready",
		}, "", nil
	}

	if database.Host == "" {
		// dbReady=true without a populated endpoint means ensureDatabase
		// declared the backend Unknown (spec.database nil). We refuse to
		// render a config Secret without database credentials because the
		// image wouldn't boot anyway.
		return metav1.Condition{
			Type:    ConditionSecretsReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NoDatabaseEndpoint",
			Message: "cannot render config Secret without a resolved database endpoint",
		}, "", nil
	}

	name := store.Name + configSecretSuffix
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		// Preserve MAGENTO_CRYPT_KEY across reconciles. It must be stable
		// for the lifetime of the store; regenerating it would invalidate
		// encrypted data at rest. Pre-ADR-003 values (raw 44-char base64)
		// are migrated in place by prepending the "base64" marker — the
		// random bytes themselves are unchanged. Values with any other
		// leading non-base64 garbage (e.g. the short-lived "7:" prefix
		// from an early draft of this code) are normalized the same way.
		cryptKey := string(sec.Data[envKeyCryptKey])
		switch {
		case cryptKey == "":
			cryptKey = newCryptKey()
		case strings.HasPrefix(cryptKey, "7:"):
			cryptKey = cryptKeyPrefix + strings.TrimPrefix(cryptKey, "7:")
		case !strings.HasPrefix(cryptKey, cryptKeyPrefix):
			cryptKey = cryptKeyPrefix + cryptKey
		}

		sec.Type = corev1.SecretTypeOpaque
		// Use Data (not StringData) so removed keys shrink the Secret
		// on the next reconcile — StringData merges into Data and never
		// removes existing keys, which left stale MAGENTO_REDIS_* /
		// MAGENTO_RABBITMQ_* entries behind in v0.2.x when users took
		// a backend out of spec. ADR-009 gap #2a.
		data := map[string][]byte{
			envKeyDBHost:     []byte(database.Host),
			envKeyDBPort:     []byte(strconv.Itoa(int(database.Port))),
			envKeyDBName:     []byte(database.Name),
			envKeyDBUsername: []byte(database.Username),
			envKeyDBPassword: []byte(database.Password),
			envKeyMageMode:   []byte("production"),
			envKeyCryptKey:   []byte(cryptKey),
		}
		// Search env vars are only emitted when a backend is configured
		// AND ensureSearch handed us a resolved endpoint. Stores without
		// search configured still get a usable Secret — bin/magento
		// setup:install will fail at runtime, but that's the documented
		// contract. Username/password are only set in external mode
		// against an authenticated managed cluster; in-cluster
		// inline/ref OpenSearch runs plaintext no-auth and emits no
		// MAGENTO_OPENSEARCH_USERNAME — the image contract interprets
		// the absence of either key as "connect anonymously."
		if search.Host != "" {
			data[envKeySearchHost] = []byte(search.Host)
			data[envKeySearchPort] = []byte(strconv.Itoa(int(search.Port)))
			if search.Username != "" {
				data[envKeySearchUsername] = []byte(search.Username)
				data[envKeySearchPassword] = []byte(search.Password)
			}
		}
		// Cache env vars: same conditional as search. Password is
		// additionally optional — password-less Redis deployments (dev
		// ElastiCache, in-cluster Valkey with anonymousAuth: true) emit
		// no MAGENTO_REDIS_PASSWORD, and the image contract interprets
		// its absence as "connect without AUTH."
		//
		// SentinelMaster is non-empty only in inline-HA mode (ADR 009).
		// env.php detects MAGENTO_REDIS_SENTINEL_MASTER and switches
		// Cm_Cache_Backend_Redis into Sentinel-discovery mode.
		if cache.Host != "" {
			data[envKeyCacheHost] = []byte(cache.Host)
			data[envKeyCachePort] = []byte(strconv.Itoa(int(cache.Port)))
			if cache.Password != "" {
				data[envKeyCachePassword] = []byte(cache.Password)
			}
			if cache.SentinelMaster != "" {
				data[envKeyCacheSentinelMaster] = []byte(cache.SentinelMaster)
			}
		}
		// Queue env vars: same conditional as search / cache. Queue is
		// genuinely optional — a MageStore without a queue runs in
		// Magento's synchronous-processing fallback. When configured,
		// RabbitMQ always requires AUTH, so username + password are
		// always emitted alongside host/port/vhost.
		if queue.Host != "" {
			data[envKeyQueueHost] = []byte(queue.Host)
			data[envKeyQueuePort] = []byte(strconv.Itoa(int(queue.Port)))
			data[envKeyQueueVhost] = []byte(queue.Vhost)
			data[envKeyQueueUsername] = []byte(queue.Username)
			data[envKeyQueuePassword] = []byte(queue.Password)
		}
		// Media env vars: only populated when ensureMedia resolved s3
		// credentials. PVC / emptyDir modes provide media via the
		// mounted Volume and emit nothing here.
		for k, v := range mediaEnvVars {
			data[k] = []byte(v)
		}
		sec.Data = data
		// Clear StringData explicitly — controller-runtime merges
		// previously-set StringData into Data on update, which would
		// reintroduce the stale-key drift Data-mode fixes.
		sec.StringData = nil
		return ctrl.SetControllerReference(store, sec, r.Scheme)
	})
	if err != nil {
		return metav1.Condition{
			Type:    ConditionSecretsReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ApplyFailed",
			Message: err.Error(),
		}, "", fmt.Errorf("apply config secret: %w", err)
	}

	return metav1.Condition{
		Type:    ConditionSecretsReady,
		Status:  metav1.ConditionTrue,
		Reason:  "ConfigRendered",
		Message: fmt.Sprintf("Secret %q populated with envvars-mode config", name),
	}, hashSecretData(sec.Data), nil
}

// hashSecretData returns a stable SHA-256 hex string over a Secret's Data map.
// Sorted keys + null-byte delimiters prevent hash collisions across key
// boundaries. Used as a pod-template annotation to trigger workload rollouts
// when Secret contents change (same pattern as nginx-config-hash / vcl-config-hash).
func hashSecretData(data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// configSecretName is the convention the workload step uses to mount the
// operator-rendered Secret as envFrom.
func configSecretName(store *magev1alpha1.MageStore) string {
	return store.Name + configSecretSuffix
}

// inlineSMTPSecretName is the operator-owned Secret in inline mode.
// Mounted by ensureSMTP; consumed by smtpEnvFromSecretName.
func inlineSMTPSecretName(store *magev1alpha1.MageStore) string {
	return store.Name + "-smtp"
}

// smtpEnvFromSecretName returns the Secret name to mount via envFrom
// on workloads/consumers/cron/bootstrap, or "" when spec.smtp is unset.
// Inline mode → operator-owned <store>-smtp Secret.
// SecretRef mode → user-supplied Secret name from spec.smtp.secretRef.
func smtpEnvFromSecretName(store *magev1alpha1.MageStore) string {
	if store.Spec.SMTP == nil {
		return ""
	}
	if store.Spec.SMTP.Inline != nil {
		return inlineSMTPSecretName(store)
	}
	if store.Spec.SMTP.SecretRef != nil {
		return store.Spec.SMTP.SecretRef.Name
	}
	return ""
}

func newCryptKey() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf) // crypto/rand never errors on supported platforms
	return cryptKeyPrefix + base64.StdEncoding.EncodeToString(buf)
}

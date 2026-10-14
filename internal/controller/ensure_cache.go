package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// Per ADR 009, the operator manages the inline cache StatefulSet
// directly — no upstream Redis/Valkey operator dependency. Two
// topologies share the same StatefulSet shape: single-instance
// (replicas: 1, default for dev) and Sentinel HA (replicas: 3 / 5 /
// 7, recommended for production). Magento's `Cm_Cache_Backend_Redis`
// can't use Cluster mode (CROSSSLOT during cache-tag DELs — Magento
// issue #39224, ADR 009 §Context), so Sentinel is the only viable HA
// shape. The CRD field names (`spec.cache.redis` / `redisRef`) and
// emitted env vars (`MAGENTO_REDIS_*`) stay Redis-shaped because
// wire-compat means that's still the right vocabulary at the user /
// env.php boundary; only the in-cluster resource names reveal the
// Valkey implementation.
const (
	valkeyImage                   = "valkey/valkey:8-alpine"
	valkeySuffix                  = "-valkey"
	valkeyAuthSecretSuffix        = "-valkey-auth"
	valkeyConfigMapSuffix         = "-valkey-config"
	valkeySentinelSuffix          = "-valkey-sentinel"
	valkeySentinelConfigMapSuffix = "-valkey-sentinel-config"
	valkeyPasswordSecretKey       = "password"
	valkeyPort                    = int32(6379)
	sentinelPort                  = int32(26379)
	sentinelMasterName            = "mymaster"
	cacheComponentLabel           = "cache"

	// cacheTopologyChangeAnnotation gates cross-topology cache
	// resize (`replicas: 1` ↔ `replicas: 3+`) — the operator refuses
	// such transitions without explicit admin acknowledgement that
	// cache contents will be discarded. Set the annotation to
	// cacheTopologyChangeApproved on the MageStore to authorise the
	// next reconcile to delete the existing cache resources and
	// materialise the new topology fresh; the operator clears the
	// annotation as part of the transition so a subsequent edit
	// requires fresh approval.
	cacheTopologyChangeAnnotation = "mage.aknostic.com/cache-topology-change"
	cacheTopologyChangeApproved   = "approved"

	// defaultExternalCachePasswordKey is the Secret key consulted on
	// an ExternalCache.PasswordSecretRef when the user doesn't
	// override via SecretKeySelector.Key. Mirrors AdminSpec.Password-
	// SecretRef's default for consistency — users who curate their
	// own Secrets don't need to memorise a per-field convention.
	defaultExternalCachePasswordKey = "password"

	// defaultRedisPort is the Redis-protocol port. External endpoints
	// default to it when spec.cache.external.port is unset; redisRef
	// resolves to the same when the targeted Service exposes 6379.
	defaultRedisPort = int32(6379)
)

// CacheEndpoint carries the resolved Redis-protocol connection details
// for ensureSecrets. Mirrors DatabaseEndpoint / SearchEndpoint: a zero
// value means "nothing to emit," but configured-but-not-ready is
// indicated by the CacheReady condition, not Endpoint presence. The
// secrets step does not and must not branch on which mode produced the
// endpoint.
//
// SentinelMaster is non-empty only in inline-HA mode — it's the
// Sentinel monitor name (`mymaster`). When set, ensureSecrets emits
// `MAGENTO_REDIS_SENTINEL_MASTER` so env.php can switch the cache
// backend into Sentinel-discovery mode (server = sentinel ClusterIP,
// port = 26379).
//
// Password is optional — many dev-mode Redis endpoints skip AUTH, and
// the envvars-mode image contract honours an empty MAGENTO_REDIS_PASSWORD
// by falling back to unauthenticated connects. The logical-DB split
// Magento uses on top of this connection (db=0 default cache / db=1
// page cache / db=2 sessions) is an image-contract convention, not
// part of CacheEndpoint — the endpoint describes the *connection*,
// not the Magento-specific logical-DB layout.
type CacheEndpoint struct {
	Host           string
	Port           int32
	Password       string
	SentinelMaster string
}

// ensureCache resolves the configured cache backend and returns a
// CacheReady condition plus the connection details ensureSecrets
// needs. Three modes per ADR 004 (with ADR 009's reshape of inline /
// ref):
//
//   - Inline (`spec.cache.redis`): the operator manages the cache
//     StatefulSet directly. `replicas: 1` materialises a bare single-
//     instance StatefulSet + headless Service + auto-generated
//     password Secret + ConfigMap. `replicas: 3` / 5 / 7 layers a
//     Sentinel Deployment + ClusterIP Service on top, with ordinal-
//     aware `replicaof` bootstrap on the StatefulSet pods. No
//     upstream Redis/Valkey operator is involved.
//   - Reference (`spec.cache.redisRef`): operator-agnostic. The user
//     provisions a cache via any third-party operator (SAP,
//     OT-CONTAINER-KIT, upstream valkey-io) and points `redisRef.name`
//     at the front-door Service. Operator emits MAGENTO_REDIS_HOST
//     from the Service and reads the password from a conventional
//     `<service-name>` Secret (key `password`). Pre-v0.3.0 manifests
//     pointing at a hyperspike Valkey CR migrate to inline or external.
//   - External (`spec.cache.external`): no provisioning. Operator
//     synthesises the endpoint from the user-supplied host/port and
//     reads the password from an optional Secret. Reachability is
//     verified implicitly at first cache read.
//
// When `spec.cache` transitions to nil, sweepCacheOrphans removes the
// resources from the previous reconcile so removing the field stops
// the cache cleanly instead of leaving orphans.
func (r *MageStoreReconciler) ensureCache(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, CacheEndpoint, error) {
	if store.Spec.Cache == nil {
		if err := r.sweepCacheOrphans(ctx, store); err != nil {
			return metav1.Condition{}, CacheEndpoint{}, err
		}
		return metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.cache is empty",
		}, CacheEndpoint{}, nil
	}

	switch {
	case store.Spec.Cache.Redis != nil:
		return r.ensureCacheInline(ctx, store)
	case store.Spec.Cache.RedisRef != nil:
		return r.ensureCacheRef(ctx, store, store.Spec.Cache.RedisRef.Name)
	case store.Spec.Cache.External != nil:
		return r.ensureCacheExternal(ctx, store)
	default:
		// CRD-level XValidation blocks this in practice; keep the branch
		// so the operator degrades to Unknown if CEL is ever bypassed.
		return metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.cache has no backend sibling set",
		}, CacheEndpoint{}, nil
	}
}

// ensureCacheInline materialises (and on each reconcile, refreshes) the
// operator-owned cache topology. Replicas selects the shape: 1 = bare
// single-instance StatefulSet; 3 / 5 / 7 = Sentinel HA with ordinal-
// aware `replicaof` bootstrap and a separate Sentinel Deployment.
func (r *MageStoreReconciler) ensureCacheInline(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, CacheEndpoint, error) {
	redis := store.Spec.Cache.Redis
	replicas := redis.Replicas
	if replicas == 0 {
		// CRD default applies on creation, but envtest-style tests
		// that POST a minimal CacheSpec{Redis: {}} hit the API server
		// without the default — keep the runtime fallback so the
		// reconciler is robust to either path.
		replicas = 1
	}
	haMode := replicas >= 3

	if cond, transitioning, err := r.handleCacheTopologyChange(ctx, store, replicas); err != nil || transitioning {
		return cond, CacheEndpoint{}, err
	}

	password, err := r.ensureValkeyAuthSecret(ctx, store)
	if err != nil {
		return metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionFalse,
			Reason:  "MaterialiseFailed",
			Message: err.Error(),
		}, CacheEndpoint{}, fmt.Errorf("ensure valkey auth secret: %w", err)
	}

	sentinelHost := ""
	if haMode {
		sentinelHost = store.Name + valkeySentinelSuffix
	}
	if err := r.ensureValkeyConfigMap(ctx, store, sentinelHost); err != nil {
		return materialiseFailed("valkey configmap", err)
	}
	if err := r.ensureValkeyHeadlessService(ctx, store); err != nil {
		return materialiseFailed("valkey headless service", err)
	}
	if err := r.ensureValkeyStatefulSet(ctx, store, replicas, redis.Resources); err != nil {
		return materialiseFailed("valkey statefulset", err)
	}

	if haMode {
		quorum := (replicas / 2) + 1
		if err := r.ensureSentinelConfigMap(ctx, store, quorum); err != nil {
			return materialiseFailed("sentinel configmap", err)
		}
		if err := r.ensureSentinelService(ctx, store); err != nil {
			return materialiseFailed("sentinel service", err)
		}
		if err := r.ensureSentinelDeployment(ctx, store, replicas); err != nil {
			return materialiseFailed("sentinel deployment", err)
		}
	} else {
		// Single-instance: any Sentinel artifacts left from a previous
		// HA reconcile are orphans — sweep them before reporting ready.
		if err := r.sweepSentinelArtifacts(ctx, store); err != nil {
			return metav1.Condition{
				Type:    ConditionCacheReady,
				Status:  metav1.ConditionFalse,
				Reason:  "SweepFailed",
				Message: err.Error(),
			}, CacheEndpoint{}, fmt.Errorf("sweep sentinel artifacts: %w", err)
		}
	}

	return r.evaluateInlineReadiness(ctx, store, replicas, haMode, password)
}

// evaluateInlineReadiness reports the StatefulSet (and, in HA mode,
// Sentinel Deployment) status. Single-instance is ready when at least
// one Valkey pod is Ready. HA is ready when at least one Valkey pod is
// Ready *and* Sentinel has reached quorum on AvailableReplicas — until
// then, Magento's Sentinel-discovery client has nothing to ask for the
// master address.
func (r *MageStoreReconciler) evaluateInlineReadiness(ctx context.Context, store *magev1alpha1.MageStore, replicas int32, haMode bool, password string) (metav1.Condition, CacheEndpoint, error) {
	ssName := store.Name + valkeySuffix
	var ss appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: ssName}, &ss); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.Condition{
				Type:    ConditionCacheReady,
				Status:  metav1.ConditionFalse,
				Reason:  "NotFound",
				Message: fmt.Sprintf("StatefulSet %q not found yet", ssName),
			}, CacheEndpoint{}, nil
		}
		return metav1.Condition{}, CacheEndpoint{}, fmt.Errorf("get cache statefulset: %w", err)
	}

	if ss.Status.ReadyReplicas < 1 {
		return metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotReady",
			Message: fmt.Sprintf("StatefulSet %q has 0/%d ready pods", ssName, replicas),
		}, CacheEndpoint{}, nil
	}

	if haMode {
		quorum := (replicas / 2) + 1
		sentinelName := store.Name + valkeySentinelSuffix
		var sentinel appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: sentinelName}, &sentinel); err != nil {
			if apierrors.IsNotFound(err) {
				return metav1.Condition{
					Type:    ConditionCacheReady,
					Status:  metav1.ConditionFalse,
					Reason:  "SentinelPending",
					Message: fmt.Sprintf("Sentinel Deployment %q not found yet", sentinelName),
				}, CacheEndpoint{}, nil
			}
			return metav1.Condition{}, CacheEndpoint{}, fmt.Errorf("get sentinel deployment: %w", err)
		}
		if sentinel.Status.AvailableReplicas < quorum {
			return metav1.Condition{
				Type:    ConditionCacheReady,
				Status:  metav1.ConditionFalse,
				Reason:  "SentinelBelowQuorum",
				Message: fmt.Sprintf("Sentinel %q has %d/%d available pods (quorum=%d)", sentinelName, sentinel.Status.AvailableReplicas, replicas, quorum),
			}, CacheEndpoint{}, nil
		}
		return metav1.Condition{
				Type:    ConditionCacheReady,
				Status:  metav1.ConditionTrue,
				Reason:  "SentinelReady",
				Message: fmt.Sprintf("Sentinel HA cache ready (%d/%d valkey, %d/%d sentinel)", ss.Status.ReadyReplicas, replicas, sentinel.Status.AvailableReplicas, replicas),
			}, CacheEndpoint{
				Host:           store.Name + valkeySentinelSuffix,
				Port:           sentinelPort,
				Password:       password,
				SentinelMaster: sentinelMasterName,
			}, nil
	}

	return metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionTrue,
			Reason:  "ValkeyReady",
			Message: fmt.Sprintf("Valkey StatefulSet %q ready (%d/1)", ssName, ss.Status.ReadyReplicas),
		}, CacheEndpoint{
			Host:     store.Name + valkeySuffix,
			Port:     valkeyPort,
			Password: password,
		}, nil
}

// materialiseFailed builds the standard MaterialiseFailed condition for
// any of the inline ensure* helpers. Wraps the underlying error so the
// reconciler can requeue on transient API failures.
func materialiseFailed(stage string, err error) (metav1.Condition, CacheEndpoint, error) {
	return metav1.Condition{
		Type:    ConditionCacheReady,
		Status:  metav1.ConditionFalse,
		Reason:  "MaterialiseFailed",
		Message: fmt.Sprintf("%s: %s", stage, err.Error()),
	}, CacheEndpoint{}, fmt.Errorf("%s: %w", stage, err)
}

// topologyOf classifies a desired or actual replica count into the two
// cache topologies the operator materialises: a single bare Valkey
// StatefulSet (replicas == 1) or Sentinel-fronted HA (replicas >= 3).
// The topology classification is the comparison key for cross-
// topology resize gating — within-topology resizes (e.g. 3 → 5 in HA)
// proceed online without admin approval; topology jumps (1 ↔ 3+)
// discard cache contents and require explicit acknowledgement via
// the cacheTopologyChangeAnnotation.
func topologyOf(replicas int32) string {
	if replicas <= 1 {
		return "single"
	}
	return "ha"
}

// handleCacheTopologyChange gates cross-topology cache transitions on
// an explicit admin annotation. Returns transitioning=true when the
// caller must short-circuit the rest of ensureCacheInline (either
// because the change was refused, or because we just deleted the
// existing resources and the next reconcile will materialise the new
// topology fresh).
//
// Returns transitioning=false on the happy paths: no existing SS
// (fresh install), or SS exists but at the same topology
// (within-topology resize like 3 → 5 stays online).
//
// On the approved-transition path the annotation is cleared first so
// (a) the Patch's For() watch fires the next reconcile and
// (b) a transient API error during the destructive steps doesn't
// leak the annotation across reconciles. If the destructive steps
// fail after the annotation is cleared, the next reconcile sees the
// old SS still present + annotation absent → refusal, admin re-applies
// the annotation. That's the right UX for a rare API-level failure
// during an admin-driven destructive op.
func (r *MageStoreReconciler) handleCacheTopologyChange(ctx context.Context, store *magev1alpha1.MageStore, desiredReplicas int32) (metav1.Condition, bool, error) {
	var existing appsv1.StatefulSet
	key := types.NamespacedName{Namespace: store.Namespace, Name: store.Name + valkeySuffix}
	if err := r.Get(ctx, key, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.Condition{}, false, nil
		}
		cond, _, e := materialiseFailed("statefulset lookup", err)
		return cond, true, e
	}

	var existingReplicas int32
	if existing.Spec.Replicas != nil {
		existingReplicas = *existing.Spec.Replicas
	}
	from := topologyOf(existingReplicas)
	to := topologyOf(desiredReplicas)
	if from == to {
		return metav1.Condition{}, false, nil
	}

	if store.Annotations[cacheTopologyChangeAnnotation] != cacheTopologyChangeApproved {
		return metav1.Condition{
			Type:   ConditionCacheReady,
			Status: metav1.ConditionFalse,
			Reason: ReasonTopologyChangeRequiresApproval,
			Message: fmt.Sprintf(
				"cache topology change %s → %s discards cache contents; set annotation %q=%q on this MageStore to approve",
				from, to, cacheTopologyChangeAnnotation, cacheTopologyChangeApproved,
			),
		}, true, nil
	}

	if err := r.clearTopologyChangeAnnotation(ctx, store); err != nil {
		cond, _, e := materialiseFailed("clear cache-topology-change annotation", err)
		return cond, true, e
	}
	if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
		cond, _, e := materialiseFailed("delete cache statefulset for topology change", err)
		return cond, true, e
	}
	if err := r.sweepSentinelArtifacts(ctx, store); err != nil {
		cond, _, e := materialiseFailed("sweep sentinel artifacts during topology change", err)
		return cond, true, e
	}
	return metav1.Condition{
		Type:   ConditionCacheReady,
		Status: metav1.ConditionFalse,
		Reason: ReasonTopologyTransitioning,
		Message: fmt.Sprintf(
			"transitioning cache topology from %s to %s; cache resources deleted, next reconcile materialises the new topology fresh",
			from, to,
		),
	}, true, nil
}

// clearTopologyChangeAnnotation removes the cache-topology-change
// annotation from the MageStore. No-op when the annotation is absent.
// Mirrors clearQuiesceStarted's metadata-Patch pattern: a metadata
// Patch is a separate sub-resource path on the API server, so it
// doesn't conflict with an in-flight Status update.
func (r *MageStoreReconciler) clearTopologyChangeAnnotation(ctx context.Context, store *magev1alpha1.MageStore) error {
	if _, ok := store.Annotations[cacheTopologyChangeAnnotation]; !ok {
		return nil
	}
	patch := client.MergeFrom(store.DeepCopy())
	delete(store.Annotations, cacheTopologyChangeAnnotation)
	return r.Patch(ctx, store, patch)
}

// ensureValkeyAuthSecret creates (once) and preserves the auto-
// generated cache password. The Secret is owned by the MageStore so
// it goes away with the parent; its content is stable across
// reconciles so existing Magento sessions / cache entries survive
// operator restarts. 24 random bytes encoded URL-safe-base64 → ~32
// printable characters, sufficient entropy without shell-quoting hazards.
func (r *MageStoreReconciler) ensureValkeyAuthSecret(ctx context.Context, store *magev1alpha1.MageStore) (string, error) {
	name := store.Name + valkeyAuthSecretSuffix
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	var password string
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		existing := string(sec.Data[valkeyPasswordSecretKey])
		if existing != "" {
			password = existing
		} else {
			buf := make([]byte, 24)
			_, _ = rand.Read(buf) // crypto/rand never errors on supported platforms
			password = base64.RawURLEncoding.EncodeToString(buf)
		}
		applyCacheLabels(sec, store)
		sec.Type = corev1.SecretTypeOpaque
		sec.Data = map[string][]byte{valkeyPasswordSecretKey: []byte(password)}
		return ctrl.SetControllerReference(store, sec, r.Scheme)
	})
	if err != nil {
		return "", err
	}
	return password, nil
}

// ensureValkeyConfigMap renders the StatefulSet's bootstrap script and
// valkey.conf. The script sets `requirepass` and `masterauth` from the
// password env var (sensitive) and queries Sentinel (when present) to
// decide master vs. replica role; valkey.conf carries only static
// settings (AOF persistence, eviction policy) — the password never
// leaks into the ConfigMap.
//
// sentinelHost is the Sentinel ClusterIP Service hostname when HA mode
// is active, empty for single-instance mode. The script short-circuits
// the Sentinel query when sentinelHost is empty, preserving the v0.3.x
// ordinal-only bootstrap path for single-instance.
func (r *MageStoreReconciler) ensureValkeyConfigMap(ctx context.Context, store *magev1alpha1.MageStore, sentinelHost string) error {
	name := store.Name + valkeyConfigMapSuffix
	headless := store.Name + valkeySuffix
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		applyCacheLabels(cm, store)
		cm.Data = map[string]string{
			"valkey.conf":  valkeyConf(),
			"bootstrap.sh": valkeyBootstrapScript(headless, sentinelHost),
		}
		return ctrl.SetControllerReference(store, cm, r.Scheme)
	})
	return err
}

// ensureValkeyHeadlessService is the Service backing the StatefulSet's
// pod DNS (`<store>-valkey-N.<store>-valkey.<ns>.svc...`). Single-
// instance Magento connects via this Service name (single-A-record
// resolution); HA mode uses pod-FQDN inside Sentinel monitor and
// connects via the Sentinel ClusterIP Service.
func (r *MageStoreReconciler) ensureValkeyHeadlessService(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + valkeySuffix
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		applyCacheLabels(svc, store)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Selector = cacheSelectorLabels(store, "valkey")
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "valkey",
			Port:       valkeyPort,
			TargetPort: intOrStr(valkeyPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		return ctrl.SetControllerReference(store, svc, r.Scheme)
	})
	return err
}

// ensureValkeyStatefulSet creates the cache pods. Pod 0 always boots
// as master (no `replicaof`); pods 1..N-1 boot as replicas of pod 0
// via the bootstrap script's ordinal check. AOF persistence on a
// per-pod 1Gi PVC; volume claim retention follows StatefulSet's
// default (PVCs survive pod recreation, survive StatefulSet deletion
// — accepted for v0.3.0, documented).
func (r *MageStoreReconciler) ensureValkeyStatefulSet(ctx context.Context, store *magev1alpha1.MageStore, replicas int32, resourcesOverride *corev1.ResourceRequirements) error {
	name := store.Name + valkeySuffix
	cmName := store.Name + valkeyConfigMapSuffix
	secretName := store.Name + valkeyAuthSecretSuffix

	ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ss, func() error {
		applyCacheLabels(ss, store)
		ss.Spec.Replicas = new(replicas)
		ss.Spec.ServiceName = name
		ss.Spec.Selector = &metav1.LabelSelector{MatchLabels: cacheSelectorLabels(store, "valkey")}
		ss.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
		ss.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: cachePodLabels(store, "valkey")},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:    "valkey",
					Image:   valkeyImage,
					Command: []string{"/bin/sh", "/etc/valkey/bootstrap.sh"},
					Env: []corev1.EnvVar{{
						Name: "VALKEY_PASSWORD",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
								Key:                  valkeyPasswordSecretKey,
							},
						},
					}},
					Ports: []corev1.ContainerPort{{
						Name:          "valkey",
						ContainerPort: valkeyPort,
						Protocol:      corev1.ProtocolTCP,
					}},
					LivenessProbe:  redisCliPingProbe(10, 10),
					ReadinessProbe: redisCliPingProbe(5, 5),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/data"},
						{Name: "config", MountPath: "/etc/valkey"},
					},
					Resources: dereferenceResources(defaultValkeyResources(resourcesOverride)),
				}},
				Volumes: []corev1.Volume{{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
							DefaultMode:          new(int32(0o755)),
						},
					},
				}},
			},
		}
		ss.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{Name: "data"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}}
		return ctrl.SetControllerReference(store, ss, r.Scheme)
	})
	return err
}

// ensureSentinelConfigMap renders the Sentinel config and bootstrap
// script. Sentinel rewrites its config at runtime (it persists known-
// master state on failover), so the bootstrap script copies the
// initial template to an emptyDir before starting valkey-sentinel.
// The password placeholder is substituted at pod start from
// $VALKEY_PASSWORD — the ConfigMap never carries the password literal.
func (r *MageStoreReconciler) ensureSentinelConfigMap(ctx context.Context, store *magev1alpha1.MageStore, quorum int32) error {
	name := store.Name + valkeySentinelConfigMapSuffix
	headless := store.Name + valkeySuffix
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		applyCacheLabels(cm, store)
		cm.Data = map[string]string{
			"sentinel.conf": sentinelConf(headless, quorum),
			"bootstrap.sh":  sentinelBootstrapScript(),
		}
		return ctrl.SetControllerReference(store, cm, r.Scheme)
	})
	return err
}

// ensureSentinelService is the ClusterIP Magento clients connect to
// for Sentinel discovery. kube-dns load-balances queries across the
// Sentinel pods; clients then talk to whichever Sentinel returns the
// current master address.
func (r *MageStoreReconciler) ensureSentinelService(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + valkeySentinelSuffix
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		applyCacheLabels(svc, store)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = cacheSelectorLabels(store, "valkey-sentinel")
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "sentinel",
			Port:       sentinelPort,
			TargetPort: intOrStr(sentinelPort),
			Protocol:   corev1.ProtocolTCP,
		}}
		return ctrl.SetControllerReference(store, svc, r.Scheme)
	})
	return err
}

// ensureSentinelDeployment provisions the Sentinel pods. Replica count
// matches the StatefulSet (3 / 5 / 7 — same as Valkey replicas) so
// quorum survives single-pod loss. Anti-affinity is preferred-only so
// kind clusters with one node still schedule the full set; production
// users targeting genuine HA should pin nodeAffinity / topology spread
// constraints out-of-band via `redisRef` if they need stronger
// guarantees.
func (r *MageStoreReconciler) ensureSentinelDeployment(ctx context.Context, store *magev1alpha1.MageStore, replicas int32) error {
	name := store.Name + valkeySentinelSuffix
	cmName := store.Name + valkeySentinelConfigMapSuffix
	secretName := store.Name + valkeyAuthSecretSuffix

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		applyCacheLabels(dep, store)
		dep.Spec.Replicas = new(replicas)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: cacheSelectorLabels(store, "valkey-sentinel")}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: cachePodLabels(store, "valkey-sentinel")},
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
							Weight: 100,
							PodAffinityTerm: corev1.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{MatchLabels: cacheSelectorLabels(store, "valkey-sentinel")},
								TopologyKey:   "kubernetes.io/hostname",
							},
						}},
					},
				},
				Containers: []corev1.Container{{
					Name:    "sentinel",
					Image:   valkeyImage,
					Command: []string{"/bin/sh", "/etc/sentinel-init/bootstrap.sh"},
					Env: []corev1.EnvVar{{
						Name: "VALKEY_PASSWORD",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
								Key:                  valkeyPasswordSecretKey,
							},
						},
					}},
					Ports: []corev1.ContainerPort{{
						Name:          "sentinel",
						ContainerPort: sentinelPort,
						Protocol:      corev1.ProtocolTCP,
					}},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{Port: intOrStr(sentinelPort)},
						},
						InitialDelaySeconds: 15,
						PeriodSeconds:       10,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"sh", "-c", "valkey-cli -p 26379 ping"},
							},
						},
						InitialDelaySeconds: 10,
						PeriodSeconds:       5,
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "config", MountPath: "/etc/sentinel-init"},
						{Name: "state", MountPath: "/etc/sentinel"},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				}},
				Volumes: []corev1.Volume{
					{
						Name: "config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
								DefaultMode:          new(int32(0o755)),
							},
						},
					},
					{
						Name:         "state",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					},
				},
			},
		}
		return ctrl.SetControllerReference(store, dep, r.Scheme)
	})
	return err
}

// ensureCacheRef resolves a user-managed cache via its front-door
// Service name. ADR 009 reshape: redisRef is operator-agnostic — the
// reference points at any Service in the same namespace (created by
// SAP/valkey-operator, OT-CONTAINER-KIT/redis-operator, upstream
// valkey-io once it ships v1, or a hand-rolled deployment), not at a
// hyperspike Valkey CR. Password resolves from a conventional
// `<service-name>` Secret (key `password`).
func (r *MageStoreReconciler) ensureCacheRef(ctx context.Context, store *magev1alpha1.MageStore, serviceName string) (metav1.Condition, CacheEndpoint, error) {
	var svc corev1.Service
	key := types.NamespacedName{Namespace: store.Namespace, Name: serviceName}
	if err := r.Get(ctx, key, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.Condition{
				Type:    ConditionCacheReady,
				Status:  metav1.ConditionFalse,
				Reason:  "NotFound",
				Message: fmt.Sprintf("Service %q not found in namespace %q", serviceName, store.Namespace),
			}, CacheEndpoint{}, nil
		}
		return metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "LookupError",
			Message: err.Error(),
		}, CacheEndpoint{}, nil
	}

	port := defaultRedisPort
	for _, p := range svc.Spec.Ports {
		if p.Port > 0 {
			port = p.Port
			break
		}
	}

	password, probCond, err := r.readCacheSecretKey(ctx, store.Namespace, serviceName, valkeyPasswordSecretKey)
	if err != nil {
		return metav1.Condition{}, CacheEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, CacheEndpoint{}, nil
	}

	return metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionTrue,
			Reason:  "ServiceReady",
			Message: fmt.Sprintf("redisRef Service %q resolved", serviceName),
		}, CacheEndpoint{
			Host:     serviceName,
			Port:     port,
			Password: password,
		}, nil
}

// ensureCacheExternal wires `spec.cache.external` through to an
// endpoint. The operator performs no provisioning, no readiness probe,
// and no ongoing watch of the remote cache — PasswordSecretRef
// resolution (when set) is the only health signal in Phase-1. Pod boot
// is where a misconfigured external endpoint actually surfaces.
func (r *MageStoreReconciler) ensureCacheExternal(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, CacheEndpoint, error) {
	ext := store.Spec.Cache.External

	port := ext.Port
	if port == 0 {
		port = defaultRedisPort
	}

	endpoint := CacheEndpoint{
		Host: ext.Host,
		Port: port,
	}

	// Password-less external endpoints are supported — many dev-mode
	// and ElastiCache-without-auth deployments skip AUTH. The image
	// contract honours an absent MAGENTO_REDIS_PASSWORD by connecting
	// unauthenticated. When PasswordSecretRef IS set, the Secret must
	// exist and the key must be non-empty; that's the only external
	// failure we surface pre-pod-boot.
	if ext.PasswordSecretRef != nil {
		secretKey := ext.PasswordSecretRef.Key
		if secretKey == "" {
			secretKey = defaultExternalCachePasswordKey
		}
		password, probCond, err := r.readCacheSecretKey(ctx, store.Namespace, ext.PasswordSecretRef.Name, secretKey)
		if err != nil {
			return metav1.Condition{}, CacheEndpoint{}, err
		}
		if probCond.Status != "" {
			return probCond, CacheEndpoint{}, nil
		}
		endpoint.Password = password
	}

	authMode := "no-auth"
	if endpoint.Password != "" {
		authMode = "AUTH from " + ext.PasswordSecretRef.Name
	}
	return metav1.Condition{
		Type:    ConditionCacheReady,
		Status:  metav1.ConditionTrue,
		Reason:  "ExternalReady",
		Message: fmt.Sprintf("external endpoint %s:%d (%s)", ext.Host, port, authMode),
	}, endpoint, nil
}

// readCacheSecretKey fetches a single key from a namespaced Secret.
// On a user-visible problem (Secret missing, key missing/empty) it
// returns a CacheReady=False condition with a specific Reason so the
// caller can surface it unchanged. Transient errors propagate as Go
// errors and the reconciler requeues. The zero-condition sentinel
// ("no problem, use the value") keeps the happy path in the caller
// linear.
func (r *MageStoreReconciler) readCacheSecretKey(ctx context.Context, namespace, name, key string) (string, metav1.Condition, error) {
	var sec corev1.Secret
	nn := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, nn, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", metav1.Condition{
				Type:    ConditionCacheReady,
				Status:  metav1.ConditionFalse,
				Reason:  "CredentialSecretMissing",
				Message: fmt.Sprintf("Secret %q not found in namespace %q", name, namespace),
			}, nil
		}
		return "", metav1.Condition{}, fmt.Errorf("get credential secret %s: %w", nn, err)
	}
	val, ok := sec.Data[key]
	if !ok || len(val) == 0 {
		return "", metav1.Condition{
			Type:    ConditionCacheReady,
			Status:  metav1.ConditionFalse,
			Reason:  "CredentialKeyEmpty",
			Message: fmt.Sprintf("Secret %q has no key %q", name, key),
		}, nil
	}
	return string(val), metav1.Condition{}, nil
}

// sweepCacheOrphans removes operator-owned cache resources when
// `spec.cache` transitions to nil. Closes ADR-009 gap #2b: prior
// behaviour left the StatefulSet / Sentinel Deployment / Services /
// ConfigMaps / Secret running against an unrelated MageStore spec.
// Idempotent — successive sweeps are no-ops once the labelled
// resources are gone.
func (r *MageStoreReconciler) sweepCacheOrphans(ctx context.Context, store *magev1alpha1.MageStore) error {
	listOpts := []client.ListOption{
		client.InNamespace(store.Namespace),
		client.MatchingLabels(cacheOwnerLabels(store)),
	}

	var ssList appsv1.StatefulSetList
	if err := r.List(ctx, &ssList, listOpts...); err != nil {
		return fmt.Errorf("list cache statefulsets: %w", err)
	}
	for i := range ssList.Items {
		if err := r.Delete(ctx, &ssList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete cache statefulset %s: %w", ssList.Items[i].Name, err)
		}
	}

	var depList appsv1.DeploymentList
	if err := r.List(ctx, &depList, listOpts...); err != nil {
		return fmt.Errorf("list cache deployments: %w", err)
	}
	for i := range depList.Items {
		if err := r.Delete(ctx, &depList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete cache deployment %s: %w", depList.Items[i].Name, err)
		}
	}

	var svcList corev1.ServiceList
	if err := r.List(ctx, &svcList, listOpts...); err != nil {
		return fmt.Errorf("list cache services: %w", err)
	}
	for i := range svcList.Items {
		if err := r.Delete(ctx, &svcList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete cache service %s: %w", svcList.Items[i].Name, err)
		}
	}

	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, listOpts...); err != nil {
		return fmt.Errorf("list cache configmaps: %w", err)
	}
	for i := range cmList.Items {
		if err := r.Delete(ctx, &cmList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete cache configmap %s: %w", cmList.Items[i].Name, err)
		}
	}

	var secList corev1.SecretList
	if err := r.List(ctx, &secList, listOpts...); err != nil {
		return fmt.Errorf("list cache secrets: %w", err)
	}
	for i := range secList.Items {
		if err := r.Delete(ctx, &secList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete cache secret %s: %w", secList.Items[i].Name, err)
		}
	}

	return nil
}

// sweepSentinelArtifacts removes Sentinel resources when an existing
// HA cache transitions to single-instance (replicas: 3 → 1). The main
// StatefulSet survives — only the Sentinel Deployment, Service, and
// ConfigMap go away. Delete-by-name (not by label) avoids racing with
// the ensureSentinel* helpers in HA mode; the names follow fixed
// suffixes.
func (r *MageStoreReconciler) sweepSentinelArtifacts(ctx context.Context, store *magev1alpha1.MageStore) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: store.Name + valkeySentinelSuffix, Namespace: store.Namespace}}
	if err := r.Delete(ctx, dep); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete sentinel deployment: %w", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: store.Name + valkeySentinelSuffix, Namespace: store.Namespace}}
	if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete sentinel service: %w", err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: store.Name + valkeySentinelConfigMapSuffix, Namespace: store.Namespace}}
	if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete sentinel configmap: %w", err)
	}
	return nil
}

// applyCacheLabels is the canonical owner-label set for every cache
// resource. The instance label is the orphan-cleanup selector; the
// component label distinguishes cache from database / search / queue
// when listing across the namespace.
func applyCacheLabels(obj client.Object, store *magev1alpha1.MageStore) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app.kubernetes.io/name"] = "valkey"
	labels["app.kubernetes.io/instance"] = store.Name
	labels["app.kubernetes.io/managed-by"] = managedByLabel
	labels["app.kubernetes.io/component"] = cacheComponentLabel
	obj.SetLabels(labels)
}

// cacheOwnerLabels is the selector for sweepCacheOrphans. Matches every
// resource applyCacheLabels stamps; unrelated resources in the namespace
// (e.g. media PVC, varnish ConfigMap) lack the cache component label
// and are passed over.
func cacheOwnerLabels(store *magev1alpha1.MageStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  cacheComponentLabel,
	}
}

// cacheSelectorLabels is the StatefulSet/Deployment selector — narrow
// enough to distinguish Valkey pods from Sentinel pods within the same
// store.
func cacheSelectorLabels(store *magev1alpha1.MageStore, role string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     role,
		"app.kubernetes.io/instance": store.Name,
	}
}

// cachePodLabels is the pod-template label set: superset of the selector
// (selectors are immutable, full label sets aren't) plus managed-by /
// component for tooling that scopes by namespace.
func cachePodLabels(store *magev1alpha1.MageStore, role string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       role,
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  cacheComponentLabel,
	}
}

// redisCliPingProbe wraps a `valkey-cli -a $PASSWORD ping` exec. Shell
// wrapper required because the env-var expansion of $VALKEY_PASSWORD
// happens in the shell, not in Kubernetes' command translation.
func redisCliPingProbe(initialDelay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"sh", "-c", `valkey-cli -a "$VALKEY_PASSWORD" ping | grep -q PONG`},
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      3,
		FailureThreshold:    5,
	}
}

// dereferenceResources unwraps an optional ResourceRequirements. The
// helpers return *ResourceRequirements for back-compat with the
// previous Valkey-CR shape; the StatefulSet builder needs the concrete
// value type, and a nil pointer translates to the zero ResourceRequirements
// (no requests, no limits).
func dereferenceResources(req *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if req == nil {
		return corev1.ResourceRequirements{}
	}
	return *req
}

// defaultValkeyResources applies dev-test-sensible defaults when the
// user doesn't override. Production deployments that want tighter
// limits or non-trivial sizing should move off inline mode.
func defaultValkeyResources(override *corev1.ResourceRequirements) *corev1.ResourceRequirements {
	if override != nil {
		return override
	}
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// valkeyConf is the in-cluster Valkey daemon config. Static across
// reconciles — the password is set on the command line via the
// bootstrap script's `--requirepass`, never written into the
// ConfigMap. AOF persistence + LRU eviction are the right defaults
// for Magento's cache + sessions workload.
func valkeyConf() string {
	return `appendonly yes
appendfsync everysec
dir /data
maxmemory-policy allkeys-lru
`
}

// valkeyBootstrapScript is the entrypoint shell that decides master vs.
// replica role for each Valkey pod. When sentinelHost is non-empty
// (HA mode), the script queries Sentinel via the Sentinel ClusterIP
// Service: if Sentinel reports a master other than this pod, boot as
// replica of that master regardless of pod ordinal. This makes
// restarts after a Sentinel-driven failover (e.g. node loss +
// re-schedule) Sentinel-aware — a restarted pod 0 joins the
// Sentinel-promoted master as a replica instead of reverting to
// ordinal-master role.
//
// Cold-start (Sentinel not up yet, or quorum-loss / network failure)
// falls back to the v0.3.x ordinal logic: pod 0 boots as master, pods
// 1..N-1 boot as replica of pod 0. Single-instance mode opts out by
// passing an empty sentinelHost — the script short-circuits the query
// so no time is wasted on a Sentinel that will never exist.
//
// The Sentinel query has no `-a` flag because sentinel.conf carries no
// `requirepass`; Sentinel accepts unauthed clients (the `auth-pass`
// directive is Sentinel-to-master auth, not client-to-Sentinel auth).
func valkeyBootstrapScript(headless, sentinelHost string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
HEADLESS='%s'
SENTINEL_HOST='%s'
SELF_FQDN="${HOSTNAME}.${HEADLESS}"
ORDINAL="${HOSTNAME##*-}"
ARGS="--requirepass $VALKEY_PASSWORD --masterauth $VALKEY_PASSWORD"

discover_master() {
  [ -n "$SENTINEL_HOST" ] || return 1
  out=$(timeout 3 valkey-cli -h "$SENTINEL_HOST" -p 26379 SENTINEL get-master-addr-by-name mymaster 2>/dev/null) || return 1
  master_host=$(printf '%%s\n' "$out" | sed -n '1p')
  master_port=$(printf '%%s\n' "$out" | sed -n '2p')
  [ -n "$master_host" ] && [ -n "$master_port" ] || return 1
  printf '%%s %%s\n' "$master_host" "$master_port"
}

if mh_mp=$(discover_master); then
  master_host=${mh_mp%% *}
  master_port=${mh_mp##* }
  if [ "$master_host" != "$SELF_FQDN" ]; then
    ARGS="$ARGS --replicaof $master_host $master_port"
  fi
elif [ "$ORDINAL" != "0" ]; then
  ARGS="$ARGS --replicaof %s-0.%s 6379"
fi

exec valkey-server /etc/valkey/valkey.conf $ARGS
`, headless, sentinelHost, headless, headless)
}

// sentinelConf is the initial Sentinel monitor template. Sentinel
// rewrites this file at runtime as it learns about replicas and
// failovers — the bootstrap script copies this template into an
// emptyDir before starting Sentinel so the rewrite doesn't try to
// modify a read-only ConfigMap mount.
//
// `resolve-hostnames yes` is required because monitor configures the
// master by FQDN (pod-N.headless), not by IP — Sentinel needs to
// resolve those before talking to them.
func sentinelConf(headless string, quorum int32) string {
	return fmt.Sprintf(`port %d
sentinel resolve-hostnames yes
sentinel announce-hostnames yes
sentinel monitor %s %s-0.%s 6379 %d
sentinel auth-pass %s __PASSWORD__
sentinel down-after-milliseconds %s 5000
sentinel parallel-syncs %s 1
sentinel failover-timeout %s 30000
`, sentinelPort, sentinelMasterName, headless, headless, quorum,
		sentinelMasterName, sentinelMasterName, sentinelMasterName, sentinelMasterName)
}

// sentinelBootstrapScript copies the sentinel.conf template into a
// writeable emptyDir and substitutes the password placeholder, then
// execs valkey-sentinel against the writeable copy. Sentinel persists
// runtime state (known-replicas, failover-history) to that file —
// which is fine to lose on pod restart since Sentinel re-discovers
// peers via the static monitor entry.
func sentinelBootstrapScript() string {
	return `#!/bin/sh
set -eu
sed "s|__PASSWORD__|$VALKEY_PASSWORD|g" /etc/sentinel-init/sentinel.conf > /etc/sentinel/sentinel.conf
exec valkey-sentinel /etc/sentinel/sentinel.conf
`
}

// intOrStr is a tiny helper for ContainerPort → ServicePort.TargetPort
// conversions. Avoids the verbose `intstr.FromInt32(int32(x))` line at
// every call site.
func intOrStr(port int32) intstr.IntOrString {
	return intstr.FromInt32(port)
}

package controller

import (
	"context"
	"fmt"

	rabbitmqv1beta1 "github.com/rabbitmq/cluster-operator/v2/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// Naming and key conventions for the queue backend.
const (
	rabbitMqSuffix = "-rabbitmq"

	// defaultRabbitMQPort is the AMQP port rabbitmq-cluster-operator
	// publishes on the front-door Service. External endpoints default
	// to the same port when spec.queue.external.port is unset.
	defaultRabbitMQPort = int32(5672)

	// defaultQueueVhost is the AMQP virtual host Magento publishes to
	// when spec.queue.external.vhost is unset. Matches rabbitmq-cluster-
	// operator's broker default — the implicit "/" vhost.
	defaultQueueVhost = "/"

	// defaultExternalQueueUsernameKey / PasswordKey are the keys
	// consulted on ExternalQueue.CredentialsSecretRef. They match
	// rabbitmq-cluster-operator's default-user Secret schema so a
	// credentialsSecretRef pointed at a RabbitmqCluster-generated
	// Secret works without translation.
	defaultExternalQueueUsernameKey = "username"
	defaultExternalQueuePasswordKey = "password"

	// rabbitMQ condition type literals. Upstream defines these under
	// github.com/rabbitmq/cluster-operator/v2/internal/status, which
	// Go's internal-package rule forbids us from importing. The
	// underlying type is `type RabbitmqClusterConditionType string`, so
	// we compare cond.Type via string() conversion to these literals.
	rabbitMQConditionClusterAvailable = "ClusterAvailable"
	rabbitMQConditionAllReplicasReady = "AllReplicasReady"
)

// QueueEndpoint carries the resolved AMQP connection details for
// ensureSecrets. Mirrors DatabaseEndpoint / SearchEndpoint /
// CacheEndpoint: a zero value means "nothing to emit," but
// configured-but-not-ready is indicated by the QueueReady condition,
// not Endpoint presence. The secrets step does not and must not
// branch on which mode produced the endpoint.
type QueueEndpoint struct {
	Host     string
	Port     int32
	Vhost    string
	Username string
	Password string
}

// ensureQueue resolves the configured queue backend and returns a
// QueueReady condition plus the connection details ensureSecrets
// needs. Three modes per ADR 004:
//
//   - Inline (`spec.queue.rabbitMq`): the operator creates and owns a
//     RabbitmqCluster CR (group rabbitmq.com, kind RabbitmqCluster).
//     rabbitmq-cluster-operator auto-generates the default-user Secret
//     and publishes its coordinates on Status.DefaultUser.Secret-
//     Reference — self-describing, so the mage-operator doesn't need
//     to know the upstream Secret-naming convention. The endpoint
//     resolves to the Service DNS name + the referenced credentials.
//   - Reference (`spec.queue.rabbitMqRef`): the RabbitmqCluster CR is
//     pre-created by an admin. Operator Get()s it for readiness; no
//     ownerReference. Credentials come from the same Status.Default-
//     User.SecretReference path.
//   - External (`spec.queue.external`): no CR is involved. The operator
//     reads credentials from the required user-provided Secret and
//     synthesises the endpoint. RabbitMQ always requires AUTH (no
//     password-less shape like external Redis), so the credentials
//     Secret is required at the CRD level.
//
// Inline and ref share readiness via readyForRabbitmqCluster which
// walks Status.Conditions for ClusterAvailable=True AND
// AllReplicasReady=True. Unlike MariaDB (IsReady() method) or Valkey
// (single typed Status.Ready bool), RabbitMQ exposes typed conditions
// and we AND two of them — still cleaner than OpenSearch's hand-rolled
// Phase+Health, because we walk typed Conditions rather than matching
// enum strings.
func (r *MageStoreReconciler) ensureQueue(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, QueueEndpoint, error) {
	if store.Spec.Queue == nil {
		if r.Capabilities.HasRabbitmq {
			if err := r.sweepOrphanRabbitmq(ctx, store); err != nil {
				return metav1.Condition{}, QueueEndpoint{}, err
			}
		}
		return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.queue is empty",
		}, QueueEndpoint{}, nil
	}

	switch {
	case store.Spec.Queue.RabbitMq != nil:
		if !r.Capabilities.HasRabbitmq {
			cond := dependencyMissingCondition(ConditionQueueReady, depRabbitmq)
			return cond, QueueEndpoint{}, nil
		}
		return r.ensureQueueInline(ctx, store)
	case store.Spec.Queue.RabbitMqRef != nil:
		if !r.Capabilities.HasRabbitmq {
			cond := dependencyMissingCondition(ConditionQueueReady, depRabbitmq)
			return cond, QueueEndpoint{}, nil
		}
		return r.readyForRabbitmqCluster(ctx, store, store.Spec.Queue.RabbitMqRef.Name)
	case store.Spec.Queue.External != nil:
		return r.ensureQueueExternal(ctx, store)
	default:
		// CRD-level XValidation blocks this in practice; keep the branch
		// so the operator degrades to Unknown if CEL is ever bypassed.
		return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.queue has no backend sibling set",
		}, QueueEndpoint{}, nil
	}
}

// ensureQueueInline materialises the RabbitmqCluster CR, then delegates
// to the shared readiness path used by rabbitMqRef. The inline mode
// owns the CR; ref mode does not — that distinction lives entirely in
// materialiseRabbitmqCluster.
func (r *MageStoreReconciler) ensureQueueInline(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, QueueEndpoint, error) {
	name := store.Name + rabbitMqSuffix
	if err := r.materialiseRabbitmqCluster(ctx, store, name); err != nil {
		return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionFalse,
			Reason:  "MaterialiseFailed",
			Message: err.Error(),
		}, QueueEndpoint{}, fmt.Errorf("materialise RabbitmqCluster: %w", err)
	}
	return r.readyForRabbitmqCluster(ctx, store, name)
}

// readyForRabbitmqCluster looks up the RabbitmqCluster CR, evaluates
// readiness via typed Conditions (ClusterAvailable=True AND
// AllReplicasReady=True), and resolves credentials into a
// QueueEndpoint. Shared by inline and ref modes. The credentials come
// from Status.DefaultUser.SecretReference — a self-describing pointer
// the upstream operator publishes once the cluster settles, naming
// both the Secret and the keys inside it for username / password /
// host / port.
func (r *MageStoreReconciler) readyForRabbitmqCluster(ctx context.Context, store *magev1alpha1.MageStore, name string) (metav1.Condition, QueueEndpoint, error) {
	var rc rabbitmqv1beta1.RabbitmqCluster
	key := types.NamespacedName{Namespace: store.Namespace, Name: name}
	if err := r.Get(ctx, key, &rc); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.Condition{
				Type:    ConditionQueueReady,
				Status:  metav1.ConditionFalse,
				Reason:  "NotFound",
				Message: fmt.Sprintf("RabbitmqCluster %q not found in namespace %q", key.Name, key.Namespace),
			}, QueueEndpoint{}, nil
		}
		if meta.IsNoMatchError(err) {
			return dependencyMissingCondition(ConditionQueueReady, depRabbitmq), QueueEndpoint{}, nil
		}
		return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "LookupError",
			Message: err.Error(),
		}, QueueEndpoint{}, nil
	}

	if !rabbitmqClusterReady(&rc) {
		return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotReady",
			Message: fmt.Sprintf("RabbitmqCluster %q has not reached ClusterAvailable+AllReplicasReady", key.Name),
		}, QueueEndpoint{}, nil
	}

	if rc.Status.DefaultUser == nil || rc.Status.DefaultUser.SecretReference == nil {
		return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionFalse,
			Reason:  "CredentialSecretMissing",
			Message: fmt.Sprintf("RabbitmqCluster %q has no status.defaultUser.secretReference yet", key.Name),
		}, QueueEndpoint{}, nil
	}

	ref := rc.Status.DefaultUser.SecretReference
	usernameKey := mapOrDefault(ref.Keys, "username", defaultExternalQueueUsernameKey)
	passwordKey := mapOrDefault(ref.Keys, "password", defaultExternalQueuePasswordKey)

	username, probCond, err := r.readQueueSecretKey(ctx, ref.Namespace, ref.Name, usernameKey)
	if err != nil {
		return metav1.Condition{}, QueueEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, QueueEndpoint{}, nil
	}
	password, probCond, err := r.readQueueSecretKey(ctx, ref.Namespace, ref.Name, passwordKey)
	if err != nil {
		return metav1.Condition{}, QueueEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, QueueEndpoint{}, nil
	}

	return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionTrue,
			Reason:  "RabbitmqClusterReady",
			Message: fmt.Sprintf("RabbitmqCluster %q is ready", key.Name),
		}, QueueEndpoint{
			Host:     rc.Name,
			Port:     defaultRabbitMQPort,
			Vhost:    defaultQueueVhost,
			Username: username,
			Password: password,
		}, nil
}

// rabbitmqClusterReady evaluates readiness by ANDing ClusterAvailable
// and AllReplicasReady. Upstream's internal/status package defines
// these Types but Go blocks the import from outside the module tree;
// we compare via string() conversion instead. ReconcileSuccess is
// not gated — it flips False during normal operator work and would
// otherwise make readiness flicker.
func rabbitmqClusterReady(rc *rabbitmqv1beta1.RabbitmqCluster) bool {
	var clusterAvailable, allReplicasReady bool
	for _, cond := range rc.Status.Conditions {
		switch string(cond.Type) {
		case rabbitMQConditionClusterAvailable:
			clusterAvailable = cond.Status == corev1.ConditionTrue
		case rabbitMQConditionAllReplicasReady:
			allReplicasReady = cond.Status == corev1.ConditionTrue
		}
	}
	return clusterAvailable && allReplicasReady
}

// mapOrDefault looks up a key in a string-map with a fallback default —
// used to read the semantic-name → secret-key translations upstream
// publishes on Status.DefaultUser.SecretReference.Keys. The fallback
// matches the conventional Secret key upstream uses, so it's safe when
// Keys is nil on older CR states.
func mapOrDefault(m map[string]string, key, fallback string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return fallback
}

// ensureQueueExternal wires `spec.queue.external` through to an
// endpoint. The operator performs no provisioning, no readiness probe,
// and no ongoing watch of the remote broker — CredentialsSecretRef
// resolution is the only health signal in Phase-1. Pod boot is where
// a misconfigured external endpoint surfaces.
func (r *MageStoreReconciler) ensureQueueExternal(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, QueueEndpoint, error) {
	ext := store.Spec.Queue.External

	port := ext.Port
	if port == 0 {
		port = defaultRabbitMQPort
	}
	vhost := ext.Vhost
	if vhost == "" {
		vhost = defaultQueueVhost
	}

	username, probCond, err := r.readQueueSecretKey(ctx, store.Namespace, ext.CredentialsSecretRef.Name, defaultExternalQueueUsernameKey)
	if err != nil {
		return metav1.Condition{}, QueueEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, QueueEndpoint{}, nil
	}
	password, probCond, err := r.readQueueSecretKey(ctx, store.Namespace, ext.CredentialsSecretRef.Name, defaultExternalQueuePasswordKey)
	if err != nil {
		return metav1.Condition{}, QueueEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, QueueEndpoint{}, nil
	}

	return metav1.Condition{
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionTrue,
			Reason:  "ExternalReady",
			Message: fmt.Sprintf("external endpoint %s:%d vhost=%q (AUTH from %s)", ext.Host, port, vhost, ext.CredentialsSecretRef.Name),
		}, QueueEndpoint{
			Host:     ext.Host,
			Port:     port,
			Vhost:    vhost,
			Username: username,
			Password: password,
		}, nil
}

// readQueueSecretKey fetches a single key from a namespaced Secret.
// Mirrors readCacheSecretKey / readDatabaseSecretKey — on a user-
// visible problem (Secret missing, key missing/empty) it returns a
// QueueReady=False condition with a specific Reason. Transient errors
// propagate as Go errors and the reconciler requeues.
func (r *MageStoreReconciler) readQueueSecretKey(ctx context.Context, namespace, name, key string) (string, metav1.Condition, error) {
	var sec corev1.Secret
	nn := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, nn, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", metav1.Condition{
				Type:    ConditionQueueReady,
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
			Type:    ConditionQueueReady,
			Status:  metav1.ConditionFalse,
			Reason:  "CredentialKeyEmpty",
			Message: fmt.Sprintf("Secret %q has no key %q", name, key),
		}, nil
	}
	return string(val), metav1.Condition{}, nil
}

// materialiseRabbitmqCluster creates or updates the RabbitmqCluster CR
// when the MageStore is in inline mode. Curated defaults keep the
// inline spec small: single-replica (default), upstream's default
// 10Gi persistence (rabbitmq-cluster-operator takes care of the PVC
// template itself, unlike hyperspike/valkey-operator which had a
// nil-storage diff bug we worked around explicitly — upstream
// rabbitmq's PVC behaviour was reviewed and verified in the kind
// e2e). Users needing HA replicas, TLS, or
// custom queue topology should graduate to `rabbitMqRef`.
func (r *MageStoreReconciler) materialiseRabbitmqCluster(ctx context.Context, store *magev1alpha1.MageStore, name string) error {
	inline := store.Spec.Queue.RabbitMq

	rc := &rabbitmqv1beta1.RabbitmqCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rc, func() error {
		if rc.Labels == nil {
			rc.Labels = map[string]string{}
		}
		rc.Labels["app.kubernetes.io/name"] = "rabbitmq"
		rc.Labels["app.kubernetes.io/instance"] = store.Name
		rc.Labels["app.kubernetes.io/managed-by"] = managedByLabel

		one := int32(1)
		rc.Spec.Replicas = &one
		rc.Spec.Resources = defaultRabbitMQResources(inline.Resources)

		return ctrl.SetControllerReference(store, rc, r.Scheme)
	})
	return err
}

// defaultRabbitMQResources applies dev-test-sensible defaults when
// the user doesn't override. Production deployments with tighter
// limits or non-trivial sizing should move off inline mode.
func defaultRabbitMQResources(override *corev1.ResourceRequirements) *corev1.ResourceRequirements {
	if override != nil {
		return override
	}
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

// sweepOrphanRabbitmq deletes the inline-mode RabbitmqCluster CR when
// `spec.queue` transitions to nil. Ref-mode CRs lack the operator's
// instance + managed-by labels and are passed over. Closes ADR-009
// gap #2b for the queue tier.
func (r *MageStoreReconciler) sweepOrphanRabbitmq(ctx context.Context, store *magev1alpha1.MageStore) error {
	var list rabbitmqv1beta1.RabbitmqClusterList
	listOpts := []client.ListOption{
		client.InNamespace(store.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/instance":   store.Name,
			"app.kubernetes.io/managed-by": managedByLabel,
		},
	}
	if err := r.List(ctx, &list, listOpts...); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list owned rabbitmqclusters: %w", err)
	}
	for i := range list.Items {
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete rabbitmqcluster %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

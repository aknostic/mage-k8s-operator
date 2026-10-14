package controller

import (
	"context"
	"fmt"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// openSearchServiceSuffix is the conventional name for the inline-created
// OpenSearchCluster CR: "<store>-opensearch". ensureSearch uses this name
// for both the CR and its front-door Service; keeping them identical lets
// the config Secret's MAGENTO_OPENSEARCH_HOST resolve to the right Service
// via normal cluster DNS without a second lookup step.
const openSearchSuffix = "-opensearch"

// External-mode constants for the search backend. Mirror the database /
// queue conventions: keys default to `username` / `password` so a user
// pointing at a managed-OpenSearch credentials Secret doesn't need a
// per-store key remapping. Default HTTP port matches the in-cluster
// inline path; managed services typically expose 443 (HTTPS) or 9200
// and override via spec.search.external.port.
const (
	defaultOpenSearchPort            = int32(9200)
	defaultExternalSearchUsernameKey = "username"
	defaultExternalSearchPasswordKey = "password"
)

// SearchEndpoint carries the resolved connection details for the search
// backend. A zero Host signals "nothing to emit" (either unconfigured or
// not yet resolvable); callers MUST NOT distinguish those two cases from
// SearchEndpoint alone — SearchReady condition on the MageStore is the
// source of truth for configured-but-not-ready. Username / Password are
// optional: in-cluster OpenSearch (inline / ref) defaults to plaintext
// no-auth and leaves them empty; external mode populates them from the
// optional credentialsSecretRef.
type SearchEndpoint struct {
	Host     string
	Port     int32
	Username string
	Password string
}

// ensureSearch resolves the configured search backend and returns a
// SearchReady condition plus the connection details the config Secret
// needs. Three modes (per ADR 004):
//
//   - Inline (`spec.search.openSearch`): the operator creates and owns
//     the OpenSearchCluster CR (ownerReference back to the MageStore).
//     Spec changes on the MageStore reconcile through to the cluster CR.
//   - Reference (`spec.search.opensearchRef`): the OpenSearchCluster CR
//     is pre-created by an admin. Operator Get()s it for readiness and
//     nothing else.
//   - External (`spec.search.external`): no CR is involved. The operator
//     reads optional credentials from a user-provided Secret and
//     synthesises the endpoint. Auth-less external endpoints are
//     supported (in-cluster OpenSearch with the security plugin disabled
//     is the canonical case; the inline-mode default is exactly this).
//
// Inline and ref share the OpenSearchCluster readiness check.
// opensearch-k8s-operator's OpenSearchCluster exposes neither an
// IsReady() helper nor a typed Conditions slice — just Phase
// ("PENDING"/"RUNNING"/"UPGRADING"), Initialized, AvailableNodes,
// Health ("red"/"yellow"/"green"/"unknown"). We treat Phase=RUNNING
// AND Initialized AND Health in {green,yellow} as Ready; yellow is
// accepted because a single-node cluster is structurally yellow (no
// replica shards to assign). External uses a shorter Secret-only path.
func (r *MageStoreReconciler) ensureSearch(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, SearchEndpoint, error) {
	if store.Spec.Search == nil {
		if r.Capabilities.HasOpenSearch {
			if err := r.sweepOrphanOpenSearch(ctx, store); err != nil {
				return metav1.Condition{}, SearchEndpoint{}, err
			}
		}
		return metav1.Condition{
			Type:    ConditionSearchReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.search is empty",
		}, SearchEndpoint{}, nil
	}

	var clusterName string
	switch {
	case store.Spec.Search.OpenSearch != nil:
		if !r.Capabilities.HasOpenSearch {
			cond := dependencyMissingCondition(ConditionSearchReady, depOpenSearch)
			return cond, SearchEndpoint{}, nil
		}
		clusterName = store.Name + openSearchSuffix
		if err := r.materialiseOpenSearchCluster(ctx, store, clusterName); err != nil {
			return metav1.Condition{
				Type:    ConditionSearchReady,
				Status:  metav1.ConditionFalse,
				Reason:  "MaterialiseFailed",
				Message: err.Error(),
			}, SearchEndpoint{}, fmt.Errorf("materialise OpenSearchCluster: %w", err)
		}
	case store.Spec.Search.OpenSearchRef != nil:
		if !r.Capabilities.HasOpenSearch {
			cond := dependencyMissingCondition(ConditionSearchReady, depOpenSearch)
			return cond, SearchEndpoint{}, nil
		}
		clusterName = store.Spec.Search.OpenSearchRef.Name
	case store.Spec.Search.External != nil:
		return r.ensureSearchExternal(ctx, store)
	default:
		return metav1.Condition{
			Type:    ConditionSearchReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.search has no backend sibling set",
		}, SearchEndpoint{}, nil
	}

	var os opensearchv1.OpenSearchCluster
	key := types.NamespacedName{Namespace: store.Namespace, Name: clusterName}
	if err := r.Get(ctx, key, &os); err != nil {
		if apierrors.IsNotFound(err) {
			// Inline mode just created the CR; transient NotFound can
			// surface if the cache hasn't caught up yet. Ref mode means
			// the admin hasn't created the CR. Same False condition in
			// both cases — the difference is user-visible in the Reason.
			return metav1.Condition{
				Type:    ConditionSearchReady,
				Status:  metav1.ConditionFalse,
				Reason:  "NotFound",
				Message: fmt.Sprintf("OpenSearchCluster %q not found in namespace %q", key.Name, key.Namespace),
			}, SearchEndpoint{}, nil
		}
		if meta.IsNoMatchError(err) {
			return dependencyMissingCondition(ConditionSearchReady, depOpenSearch), SearchEndpoint{}, nil
		}
		return metav1.Condition{
			Type:    ConditionSearchReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "LookupError",
			Message: err.Error(),
		}, SearchEndpoint{}, nil
	}

	host := os.Spec.General.ServiceName
	if host == "" {
		host = os.Name
	}
	port := os.Spec.General.HttpPort
	if port == 0 {
		port = 9200
	}
	endpoint := SearchEndpoint{Host: host, Port: port}

	healthy := os.Status.Health == opensearchv1.OpenSearchGreenHealth ||
		os.Status.Health == opensearchv1.OpenSearchYellowHealth
	if os.Status.Phase == opensearchv1.PhaseRunning && os.Status.Initialized && healthy {
		return metav1.Condition{
			Type:   ConditionSearchReady,
			Status: metav1.ConditionTrue,
			Reason: "OpenSearchReady",
			Message: fmt.Sprintf("OpenSearchCluster %q phase=%s health=%s nodes=%d",
				key.Name, os.Status.Phase, os.Status.Health, os.Status.AvailableNodes),
		}, endpoint, nil
	}

	return metav1.Condition{
		Type:   ConditionSearchReady,
		Status: metav1.ConditionFalse,
		Reason: "NotReady",
		Message: fmt.Sprintf("OpenSearchCluster %q phase=%q health=%q initialized=%t",
			key.Name, os.Status.Phase, os.Status.Health, os.Status.Initialized),
	}, endpoint, nil
}

// ensureSearchExternal wires `spec.search.external` through to an
// endpoint. The operator performs no provisioning, no readiness probe,
// and no ongoing watch of the remote cluster — credentials Secret
// resolution (when set) is the only health signal in Phase-1. Pod boot
// is where a misconfigured external endpoint surfaces.
//
// Auth-less external endpoints are supported because in-cluster
// Magento-only OpenSearch with the security plugin disabled is the
// canonical no-auth case (and is also what the inline-mode default
// produces). When CredentialsSecretRef IS set, the Secret must exist
// and both `username` + `password` keys must be non-empty.
func (r *MageStoreReconciler) ensureSearchExternal(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, SearchEndpoint, error) {
	ext := store.Spec.Search.External

	port := ext.Port
	if port == 0 {
		port = defaultOpenSearchPort
	}

	endpoint := SearchEndpoint{
		Host: ext.Host,
		Port: port,
	}

	if ext.CredentialsSecretRef != nil {
		secretName := ext.CredentialsSecretRef.Name
		username, probCond, err := r.readSearchSecretKey(ctx, store.Namespace, secretName, defaultExternalSearchUsernameKey)
		if err != nil {
			return metav1.Condition{}, SearchEndpoint{}, err
		}
		if probCond.Status != "" {
			return probCond, SearchEndpoint{}, nil
		}
		password, probCond, err := r.readSearchSecretKey(ctx, store.Namespace, secretName, defaultExternalSearchPasswordKey)
		if err != nil {
			return metav1.Condition{}, SearchEndpoint{}, err
		}
		if probCond.Status != "" {
			return probCond, SearchEndpoint{}, nil
		}
		endpoint.Username = username
		endpoint.Password = password
	}

	authMode := "no-auth"
	if endpoint.Username != "" {
		authMode = "AUTH from " + ext.CredentialsSecretRef.Name
	}
	return metav1.Condition{
		Type:    ConditionSearchReady,
		Status:  metav1.ConditionTrue,
		Reason:  "ExternalReady",
		Message: fmt.Sprintf("external endpoint %s:%d (%s)", ext.Host, port, authMode),
	}, endpoint, nil
}

// readSearchSecretKey fetches a single key from a namespaced Secret.
// Mirrors readDatabaseSecretKey / readCacheSecretKey / readQueueSecretKey
// — on a user-visible problem (Secret missing, key missing/empty) it
// returns a SearchReady=False condition with a specific Reason.
// Transient errors propagate as Go errors and the reconciler requeues.
func (r *MageStoreReconciler) readSearchSecretKey(ctx context.Context, namespace, name, key string) (string, metav1.Condition, error) {
	var sec corev1.Secret
	nn := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, nn, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", metav1.Condition{
				Type:    ConditionSearchReady,
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
			Type:    ConditionSearchReady,
			Status:  metav1.ConditionFalse,
			Reason:  "CredentialKeyEmpty",
			Message: fmt.Sprintf("Secret %q has no key %q", name, key),
		}, nil
	}
	return string(val), metav1.Condition{}, nil
}

// materialiseOpenSearchCluster creates or updates the OpenSearchCluster
// CR the operator owns when the MageStore is in inline mode. The curated
// InlineOpenSearch spec is expanded into a full ClusterSpec with
// opinionated defaults: plaintext HTTP (Magento talks to search inside
// the cluster), single-pool cluster_manager+data, emptyDir persistence.
// Users who need anything outside that envelope graduate to opensearchRef.
func (r *MageStoreReconciler) materialiseOpenSearchCluster(ctx context.Context, store *magev1alpha1.MageStore, name string) error {
	inline := store.Spec.Search.OpenSearch

	cluster := &opensearchv1.OpenSearchCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		if cluster.Labels == nil {
			cluster.Labels = map[string]string{}
		}
		cluster.Labels["app.kubernetes.io/name"] = "opensearch"
		cluster.Labels["app.kubernetes.io/instance"] = store.Name
		cluster.Labels["app.kubernetes.io/managed-by"] = managedByLabel

		// Only set the General fields we care about; leave upstream-
		// operator-defaulted fields (SetVMMaxMapCount, Monitoring, …)
		// untouched so CreateOrUpdate doesn't churn against them.
		cluster.Spec.General.Version = inline.Version
		cluster.Spec.General.ServiceName = name
		cluster.Spec.General.HttpPort = 9200
		cluster.Spec.General.DrainDataNodes = true
		// Disable the opensearch-security plugin at the yml level. Magento
		// talks to OpenSearch in-cluster over plain HTTP; security-plugin
		// bootstrap needs either a cert-manager-backed admin cert or a
		// user-supplied securityconfig Secret, neither of which is worth
		// plumbing for v0.1.0. Users who need security-on graduate to
		// opensearchRef and manage the cluster CR directly.
		if cluster.Spec.General.AdditionalConfig == nil {
			cluster.Spec.General.AdditionalConfig = map[string]string{}
		}
		cluster.Spec.General.AdditionalConfig["plugins.security.disabled"] = "true"
		// We can't opt out of the upstream operator's 2-pod bootstrap
		// flow: it hard-codes `cluster.initial_master_nodes` on the main
		// node pointing at the bootstrap pod's DNS name, which conflicts
		// with `discovery.type: single-node` at OpenSearch startup. Let
		// the bootstrap pod form the initial cluster; it garbage-
		// collects after the main nodes join. Cluster state then lives
		// in the main node's emptyDir. Pod *crashes* (no pod delete)
		// preserve state; pod *deletes* wipe it, which the operator
		// handles by re-running the bootstrap flow on next reconcile.
		// Production deployments that want survivability across pod
		// deletes graduate to `opensearchRef` with a PVC-backed CR.

		// Leave Spec.Security nil — with the plugin disabled in opensearch.yml
		// the upstream operator skips TLS/cert bootstrap entirely.
		cluster.Spec.Security = nil

		cluster.Spec.Dashboards.Enable = false

		cluster.Spec.NodePools = []opensearchv1.NodePool{{
			Component: "nodes",
			Replicas:  inline.Replicas,
			Roles:     []string{"data", "cluster_manager"},
			Resources: defaultOpenSearchResources(inline.Resources),
			Jvm:       fmt.Sprintf("-Xms%s -Xmx%s", inline.JvmHeap, inline.JvmHeap),
			// 2Gi PVC backed by the cluster's default StorageClass. PVC
			// persistence is mandatory for single-replica inline mode:
			// the upstream operator's bootstrap-pod flow hard-codes
			// cluster.initial_master_nodes at the (ephemeral) bootstrap
			// pod's DNS name, so once the bootstrap pod leaves, the main
			// node's persisted cluster state is the only place the
			// master election can resume from. emptyDir loses that state
			// on any pod restart; the cluster then deadlocks on
			// "cluster_manager_not_discovered". A PVC survives restarts
			// and keeps single-node clusters healthy. Multi-replica
			// clusters survive single-pod loss on emptyDir via quorum,
			// but PVC is still the right default.
			DiskSize: resource.MustParse("2Gi"),
			Persistence: &opensearchv1.PersistenceConfig{
				PersistenceSource: opensearchv1.PersistenceSource{
					PVC: &opensearchv1.PVCSource{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					},
				},
			},
		}}
		return ctrl.SetControllerReference(store, cluster, r.Scheme)
	})
	return err
}

// defaultOpenSearchResources applies dev-test-sensible defaults when the
// user doesn't override. Production deployments typically want tighter
// memory limits matched to the JVM heap — graduate to opensearchRef.
func defaultOpenSearchResources(override *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if override != nil {
		return *override
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("768Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1536Mi"),
		},
	}
}

// sweepOrphanOpenSearch deletes the inline-mode OpenSearchCluster CR
// when `spec.search` transitions to nil. Ref-mode CRs lack the
// operator's instance + managed-by labels and are passed over.
// Closes ADR-009 gap #2b for the search tier.
func (r *MageStoreReconciler) sweepOrphanOpenSearch(ctx context.Context, store *magev1alpha1.MageStore) error {
	var list opensearchv1.OpenSearchClusterList
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
		return fmt.Errorf("list owned opensearchclusters: %w", err)
	}
	for i := range list.Items {
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete opensearchcluster %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

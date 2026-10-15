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

package controller

import (
	"context"
	"fmt"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"
	rabbitmqv1beta1 "github.com/rabbitmq/cluster-operator/v2/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// Condition types. The plan defines DatabaseReady / CacheReady / SearchReady /
// MagentoReady as the primary readiness signals. Phase-1 wires DatabaseReady
// against the mariadb-operator; the other sub-readiness conditions land with
// their own ensure* steps. Ready rolls up the rest.
const (
	ConditionReady          = "Ready"
	ConditionDatabaseReady  = "DatabaseReady"
	ConditionSearchReady    = "SearchReady"
	ConditionCacheReady     = "CacheReady"
	ConditionQueueReady     = "QueueReady"
	ConditionSecretsReady   = "SecretsReady"
	ConditionStoreInstalled = "StoreInstalled"
	ConditionWorkloadsReady = "WorkloadsReady"
	ConditionVarnishReady   = "VarnishReady"
	ConditionConsumersReady = "ConsumersReady"
	ConditionCronReady      = "CronReady"
	ConditionUpgradeReady   = "UpgradeReady"

	ReasonReconciling               = "Reconciling"
	ReasonDependenciesMet           = "DependenciesMet"
	ReasonAwaitingDatabase          = "AwaitingDatabase"
	ReasonAwaitingSearch            = "AwaitingSearch"
	ReasonAwaitingCache             = "AwaitingCache"
	ReasonAwaitingQueue             = "AwaitingQueue"
	ReasonAwaitingSecrets           = "AwaitingSecrets"
	ReasonAwaitingBootstrap         = "AwaitingBootstrap"
	ReasonAwaitingWorkloads         = "AwaitingWorkloads"
	ReasonAwaitingVarnish           = "AwaitingVarnish"
	ReasonAwaitingConsumers         = "AwaitingConsumers"
	ReasonAwaitingCron              = "AwaitingCron"
	ReasonAwaitingMediaSeed         = "AwaitingMediaSeed"
	ReasonDependencyOperatorMissing = "DependencyOperatorMissing"

	// Upgrade-arc Reason vocabulary (ADR 008). UpgradeReady carries
	// these values; the empty-baseline fresh-install case uses
	// AwaitingUpgrade. UpgradeStarting and WorkloadRolloutPending are
	// the in-flight values; SetupUpgradeFailed and CronQuiesceTimeout
	// are the failure values. The literal "UpToDate" and
	// "UpgradeComplete" Reasons live inline in ensure_upgrade.go
	// because they're not part of the failure surface.
	ReasonUpgradeStarting        = "UpgradeStarting"
	ReasonSetupUpgradeFailed     = "SetupUpgradeFailed"
	ReasonCronQuiesceTimeout     = "CronQuiesceTimeout"
	ReasonWorkloadRolloutPending = "WorkloadRolloutPending"
	ReasonAwaitingUpgrade        = "AwaitingUpgrade"

	// TLS structural-validity Reasons (ADR 010). spec.routing.tls
	// without spec.stores is invalid at apply time; the operator fails
	// fast with Ready=False so users see a clear signal rather than a
	// Gateway with no SNI hostnames.
	ReasonTLSRequiresStoreHosts = "TLSRequiresStoreHosts"

	// Cache cross-topology resize Reasons (v0.4.0). The operator
	// refuses cross-topology changes (`replicas: 1` ↔ `replicas: 3+`)
	// without the cacheTopologyChangeAnnotation set to "approved" on
	// the MageStore — cache contents would be discarded by the
	// transition, so admin acknowledgement is required. With the
	// annotation, the operator deletes the existing cache resources
	// and reports TopologyTransitioning until the next reconcile
	// materialises the new topology fresh.
	ReasonTopologyChangeRequiresApproval = "TopologyChangeRequiresApproval"
	ReasonTopologyTransitioning          = "TopologyTransitioning"

	// managedByLabel is the value every operator-owned resource sets on
	// `app.kubernetes.io/managed-by`. Kept as a single package constant
	// so a future project rename (or a Helm-chart-driven override) only
	// edits one place.
	managedByLabel = "mage-operator"
)

// MageStoreReconciler reconciles a MageStore object
type MageStoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Capabilities is the snapshot of dependency CRDs present at startup
	// (see capabilities.go). Reconcile-time `ensure*` steps consult it to
	// surface DependencyOperatorMissing instead of crashing on a missing
	// kind, and SetupWithManager conditions its Watches() chain on it so
	// the manager doesn't fail to start when a soft dep's informer can't
	// list its CRD. Populated by main.go before SetupWithManager runs;
	// envtest constructs it explicitly per-test.
	Capabilities Capabilities

	// RestConfig and KubeClient are required for pod exec operations
	// (Varnish ban after upgrade arc). Both are nil-safe: ensure_varnish.go
	// skips the ban gracefully when KubeClient is nil (envtest / unit tests).
	RestConfig *rest.Config
	KubeClient kubernetes.Interface

	// Now returns the current wall-clock time. nil = time.Now. Test
	// seam used by the terminal-Failed Job recovery helper to drive
	// the backoff window deterministically; production callers leave
	// it nil.
	Now func() time.Time
	// RandFloat returns a uniform random float in [0, 1). nil =
	// math/rand.Float64. Test seam for the ±25 % jitter applied to
	// the Job-recovery backoff window.
	RandFloat func() float64
	// JobBackoffScale multiplies every entry of the Job-recovery
	// backoff schedule (30s … 30m). 0 or 1 = the production schedule.
	// Set from the manager's --job-retry-backoff-scale flag; the e2e
	// suite shrinks it so the failure-path spec reaches the
	// retry-budget-exhausted state in minutes instead of ~47.
	JobBackoffScale float64
}

// ProbeCapabilities (cmd/main.go) reads CustomResourceDefinitions
// at cluster scope to detect which dependency operators are present
// at startup; without get/list on this resource the operator pod
// fails fast with a probe error before the manager starts.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list
// +kubebuilder:rbac:groups=mage.aknostic.com,resources=magestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mage.aknostic.com,resources=magestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mage.aknostic.com,resources=magestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=k8s.mariadb.com,resources=mariadbs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=opensearch.org,resources=opensearchclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// `cronPodsStillRunning` in ensure_upgrade.go lists pods by label
// to wait for cron pods to drain before the upgrade arc proceeds;
// without list+watch the operator pod's controller-runtime cache
// cannot back the List() call and the upgrade arc parks
// indefinitely at Reason=UpgradeStarting.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the MageStore object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/reconcile
//
// nolint:gocyclo
// Reconcile's complexity grows ~3-4 branches per ensure* dependency
// (condition set, err-guard, Ready-rollup case, configured-guard).
// Slot 20 added queue and tipped gocyclo over 30; splitting the
// pipeline into sub-Reconciles loses more clarity than it saves.
func (r *MageStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("store", req.NamespacedName)

	var store magev1alpha1.MageStore
	if err := r.Get(ctx, req.NamespacedName, &store); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get MageStore: %w", err)
	}

	// Structural spec validation — pure, no API calls. Fail fast before
	// any ensure* step so users see a clear Ready=False signal rather
	// than a partially-materialised stack. The user must fix the spec;
	// do not requeue.
	if reason, valErr := validateMageStoreSpec(&store); valErr != nil {
		meta.SetStatusCondition(&store.Status.Conditions, metav1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: valErr.Error(),
		})
		store.Status.Phase = magev1alpha1.PhaseFailed
		if updateErr := r.Status().Update(ctx, &store); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	log.Info("reconciling",
		"image", fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag),
		"frontendReplicas", store.Spec.Workloads.Frontend.Replicas,
		"generation", store.Generation,
		"observed", store.Status.ObservedGeneration,
	)

	// Reconcile dependency-by-dependency. Each ensure* returns a condition;
	// Ready is an AND over the sub-conditions.
	dbCond, dbEndpoint, err := r.ensureDatabase(ctx, &store)
	meta.SetStatusCondition(&store.Status.Conditions, dbCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	searchCond, searchEndpoint, err := r.ensureSearch(ctx, &store)
	meta.SetStatusCondition(&store.Status.Conditions, searchCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	cacheCond, cacheEndpoint, err := r.ensureCache(ctx, &store)
	meta.SetStatusCondition(&store.Status.Conditions, cacheCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	queueCond, queueEndpoint, err := r.ensureQueue(ctx, &store)
	meta.SetStatusCondition(&store.Status.Conditions, queueCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	// Media: per-pod volume source + optional MAGENTO_MEDIA_STORAGE_*
	// env vars for s3 mode. No condition — media is not a rollout
	// gate; volume source issues surface via pod scheduling, s3
	// credential errors surface via ensureSecrets' transient-error
	// requeue. See ensure_media.go for mode semantics.
	mediaEndpoint, err := r.ensureMedia(ctx, &store)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	// <dep>Configured distinguishes "user didn't opt in" (<Dep>Ready=
	// Unknown, Ready passes) from "user opted in but backend isn't there
	// yet" (<Dep>Ready=False, Ready blocks). Magento 2.4 hard-requires
	// search and uses Redis heavily; in practice every real store
	// declares both. The opt-out paths exist so un-updated pre-Day-N CRs
	// don't instantly fail Ready until they pick up the new fields.
	// Queue is genuinely optional — Magento runs in synchronous-
	// processing mode when no broker is configured, which is fine for
	// dev and for storefronts that don't use bulk operations.
	searchConfigured := store.Spec.Search != nil &&
		(store.Spec.Search.OpenSearch != nil || store.Spec.Search.OpenSearchRef != nil || store.Spec.Search.External != nil)
	searchReady := !searchConfigured || searchCond.Status == metav1.ConditionTrue

	cacheConfigured := store.Spec.Cache != nil &&
		(store.Spec.Cache.Redis != nil || store.Spec.Cache.RedisRef != nil || store.Spec.Cache.External != nil)
	cacheReady := !cacheConfigured || cacheCond.Status == metav1.ConditionTrue

	queueConfigured := store.Spec.Queue != nil &&
		(store.Spec.Queue.RabbitMq != nil || store.Spec.Queue.RabbitMqRef != nil || store.Spec.Queue.External != nil)
	queueReady := !queueConfigured || queueCond.Status == metav1.ConditionTrue

	secCond, configSecretHash, err := r.ensureSecrets(ctx, &store, dbCond.Status == metav1.ConditionTrue, searchReady, cacheReady, queueReady, dbEndpoint, searchEndpoint, cacheEndpoint, queueEndpoint, mediaEndpoint.EnvVars)
	meta.SetStatusCondition(&store.Status.Conditions, secCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	smtpCond, smtpSecretHash, err := r.ensureSMTP(ctx, &store)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}
	if smtpCond.Type != "" {
		meta.SetStatusCondition(&store.Status.Conditions, smtpCond)
	}

	// smtpReady gates downstream steps (bootstrap, workloads/consumers/cron).
	// When spec.smtp is unset, treat as ready (no SMTP requirement). Mirrors the
	// searchReady/cacheReady/queueReady pattern earlier in this function.
	smtpReady := store.Spec.SMTP == nil || smtpCond.Status == metav1.ConditionTrue

	bootCond, err := r.ensureBootstrap(ctx, &store, secCond.Status == metav1.ConditionTrue && smtpReady, smtpSecretHash)
	meta.SetStatusCondition(&store.Status.Conditions, bootCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	advanceObservedImageTagOnInstall(&store, bootCond)

	// Media seed step. Runs BETWEEN ensureBootstrap and ensureWorkloads.
	// workloadsReady is read from the PRIOR reconcile pass — on the first
	// pass after BootstrapReady=True this is false, which is correct:
	// volume modes transition Unknown→True over two passes via eventual
	// consistency. S3 mode emits a one-shot Job here; C2.4 will gate
	// ensureWorkloads on MediaSeeded=True for S3 mode. Do NOT move this
	// call after ensureWorkloads — that ordering breaks C2.4's gate.
	workloadsReady := meta.IsStatusConditionTrue(store.Status.Conditions, ConditionWorkloadsReady)
	// secretsReady is the v0.6.3 #2 gate: S3 seed Job's envFrom needs
	// `<store>-config`, which ensureSecrets only writes once its
	// upstream conditions are all True. Creating the Job before then
	// burns ~3-5 min of cold-start time on the recovery loop.
	mediaSeedCond, err := r.ensureMediaSeed(ctx, &store, mediaEndpoint, workloadsReady, secCond.Status == metav1.ConditionTrue)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}
	if mediaSeedCond.Type != "" {
		meta.SetStatusCondition(&store.Status.Conditions, mediaSeedCond)
	}

	// Store URL step. Per ADR 013 phase 2. Runs AFTER ensureMediaSeed and
	// BEFORE ensureUpgrade / ensureWorkloads. Does not gate ensureWorkloads
	// — workloads can come up against slightly-stale URL config; Magento
	// serves traffic correctly via nginx routing regardless of what's in
	// web/{secure,unsecure}/base_url. The URL config affects only what
	// Magento *generates* (links, redirects, emails), not what nginx routes.
	urlsCond, err := r.ensureStoreURLs(ctx, &store, bootCond.Status == metav1.ConditionTrue)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}
	if urlsCond.Type != "" {
		meta.SetStatusCondition(&store.Status.Conditions, urlsCond)
	}

	// Upgrade arc. ensureUpgrade returns Requeue/RequeueAfter when an
	// in-flight Job is running OR cron pods are still terminating —
	// those states halt the pipeline so workload Deployments don't roll
	// forward to the new image tag against an un-migrated schema. On
	// steady state (no drift), arc-success, or terminal-failure paths
	// it returns ctrl.Result{} and the pipeline continues.
	//
	// Subsession-B caveat: the cron-quiesce-timeout and Job-failure
	// terminal paths return ctrl.Result{}, so workloads CAN roll
	// forward in those failure modes. Subsession C adds the
	// maintenance Service-selector flip that fences traffic during
	// the arc; until it lands, v0.2.0 pre-release builds are
	// production-unsafe in the failure path.
	upgCond, upgResult, err := r.ensureUpgrade(ctx, &store)
	meta.SetStatusCondition(&store.Status.Conditions, upgCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}
	if upgResult.RequeueAfter > 0 {
		store.Status.ObservedGeneration = store.Generation
		if err := r.Status().Update(ctx, &store); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status during upgrade arc: %w", err)
		}
		return upgResult, nil
	}

	// Workload rollout is gated on StoreInstalled. True (Job succeeded) and
	// Unknown (user opted out by leaving spec.bootstrap nil, adopting a
	// pre-bootstrapped DB) both let workloads proceed; False blocks until
	// bootstrap completes, to avoid serving 500s from pods talking to an
	// un-migrated schema.
	// nginx ConfigMap is generated independently of dependency readiness
	// — its content is derived purely from `spec.stores` and doesn't
	// need credentials or a live database. Emitting it before
	// ensureWorkloads lets us pass the content hash through as a
	// pod-template annotation that triggers rollouts on
	// `spec.stores` edits (ADR 007). The ConfigMap mount itself is
	// deferred to Session 3 — see ensure_nginx_config.go header.
	nginxConfigHash, err := r.ensureNginxConfig(ctx, &store)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	storeInstalled := bootCond.Status != metav1.ConditionFalse

	// S3 + seedFromImage gates ensureWorkloads on MediaSeeded=True so
	// pods don't come up against an empty bucket. Volume modes don't
	// need this gate (the initContainer in ensureWorkloadDeployment is
	// the gate). Per ADR 011 §"Reconcile graph and gating".
	needMediaSeedGate := store.Spec.Media != nil &&
		store.Spec.Media.S3 != nil &&
		store.Spec.Media.SeedFromImage
	mediaReady := !needMediaSeedGate ||
		meta.IsStatusConditionTrue(store.Status.Conditions, ConditionMediaSeeded)

	wlCond, err := r.ensureWorkloads(ctx, &store, secCond.Status == metav1.ConditionTrue && storeInstalled && smtpReady, mediaReady, nginxConfigHash, configSecretHash, smtpSecretHash, mediaEndpoint)
	meta.SetStatusCondition(&store.Status.Conditions, wlCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	// Consumers fan-out. Gates on the same dependency chain as
	// ensureWorkloads — consumers connect to the same DB / Redis /
	// RabbitMQ backends and need the config Secret, plus the
	// schema needs to exist (StoreInstalled) before a consumer
	// starts issuing queries. Per-consumer Deployment shape lives
	// in ensure_consumers.go.
	consumersCond, err := r.ensureConsumers(ctx, &store, secCond.Status == metav1.ConditionTrue && storeInstalled && smtpReady, smtpSecretHash, mediaEndpoint.Volume)
	meta.SetStatusCondition(&store.Status.Conditions, consumersCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	// Cron fan-out. Same dependency gate as consumers — each
	// `bin/magento cron:run` invocation issues queries against the
	// DB + cache, so the schema must exist. Per-group CronJob
	// shape lives in ensure_cron.go.
	cronCond, err := r.ensureCron(ctx, &store, secCond.Status == metav1.ConditionTrue && storeInstalled && smtpReady, smtpSecretHash)
	meta.SetStatusCondition(&store.Status.Conditions, cronCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	// Varnish materialisation. Runs before ensureRouting so the
	// HTTPRoute's `/` backend name is stable across reconciles —
	// ensureRouting reads spec.varnish.enabled to decide whether `/`
	// points at <store>-frontend or <store>-varnish, and the Varnish
	// Service must exist (or be absent) consistently with the
	// routing decision. VarnishReady=False blocks Ready but NOT
	// routing: even when Varnish is still rolling, HTTPRoute points
	// at the Service, which gains endpoints when the pods come up.
	varnishCond, err := r.ensureVarnish(ctx, &store)
	meta.SetStatusCondition(&store.Status.Conditions, varnishCond)
	if err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	// Routing layer (Gateway + HTTPRoute) emitted unconditionally
	// once we know the workload set. Backend Service refs in the
	// HTTPRoute may point at Services without endpoints yet — that's
	// fine; the Gateway data plane starts routing as soon as
	// endpoints appear. No RoutingReady condition is surfaced; users
	// diagnose routing problems through the Gateway controller's
	// status, not through MageStore. ADR 006 (2026-04-24 amendment)
	// for the Ingress→HTTPRoute switch.
	if err := r.ensureRouting(ctx, &store); err != nil {
		_ = r.Status().Update(ctx, &store)
		return ctrl.Result{}, err
	}

	// Ready rollup — strict AND over sub-conditions. The first failing
	// sub-condition drives the Reason we surface on Ready.
	readyCond := metav1.Condition{
		Type:    ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonDependenciesMet,
		Message: "All declared dependencies ready",
	}
	store.Status.Phase = magev1alpha1.PhaseRunning
	switch {
	case dbCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingDatabase
		readyCond.Message = dbCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case searchConfigured && searchCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingSearch
		readyCond.Message = searchCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case cacheConfigured && cacheCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingCache
		readyCond.Message = cacheCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case queueConfigured && queueCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingQueue
		readyCond.Message = queueCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case secCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingSecrets
		readyCond.Message = secCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case bootCond.Status == metav1.ConditionFalse:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingBootstrap
		readyCond.Message = bootCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case upgCond.Status == metav1.ConditionFalse &&
		(upgCond.Reason == ReasonSetupUpgradeFailed || upgCond.Reason == ReasonCronQuiesceTimeout):
		// Terminal-failure paths from ensureUpgrade. The in-flight
		// paths (UpgradeStarting, WorkloadRolloutPending) already
		// short-circuited the pipeline above with Requeue, so the
		// rollup only sees terminal Reasons here.
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = upgCond.Reason
		readyCond.Message = upgCond.Message
		store.Status.Phase = magev1alpha1.PhaseFailed
	case wlCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingWorkloads
		readyCond.Message = wlCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case store.Spec.Varnish != nil && store.Spec.Varnish.Enabled && varnishCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingVarnish
		readyCond.Message = varnishCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case store.Spec.Consumers != nil && store.Spec.Consumers.Enabled && consumersCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingConsumers
		readyCond.Message = consumersCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	case store.Spec.Cron != nil && store.Spec.Cron.Enabled && cronCond.Status != metav1.ConditionTrue:
		readyCond.Status = metav1.ConditionFalse
		readyCond.Reason = ReasonAwaitingCron
		readyCond.Message = cronCond.Message
		store.Status.Phase = magev1alpha1.PhaseProvisioning
	}
	meta.SetStatusCondition(&store.Status.Conditions, readyCond)

	store.Status.ObservedGeneration = store.Generation
	if err := r.Status().Update(ctx, &store); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	// We Watch referenced MariaDBs and OpenSearchClusters (see
	// SetupWithManager), so state changes trigger an immediate reconcile
	// and we don't need a polling fallback for Database or Search
	// readiness. Other ensure* steps that gain external refs should follow
	// the same pattern.
	//
	// Job-recovery requeue: ensure-* steps that observed a terminal-
	// Failed owned Job stamp NextAttemptAt on store.Status.JobAttempts.
	// A terminal-Failed Job emits no further events, so without an
	// explicit RequeueAfter the controller would sleep until something
	// else woke it. Pick the soonest pending attempt across all owned
	// Jobs and requeue at that instant.
	return ctrl.Result{RequeueAfter: r.nextJobAttemptRequeue(&store)}, nil
}

// mapMariaDBToMageStores translates a MariaDB change into reconcile
// requests for every MageStore in the same namespace whose database
// spec points at the changed CR. Handles both inline mode (CR name
// derived as `<store>-mariadb`) and ref mode (CR name from
// spec.database.mariaDbRef.name). External mode has no CR to watch;
// spec changes there requeue via the MageStore watch directly.
func (r *MageStoreReconciler) mapMariaDBToMageStores(ctx context.Context, obj client.Object) []reconcile.Request {
	var list magev1alpha1.MageStoreList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, store := range list.Items {
		if store.Spec.Database == nil {
			continue
		}
		var expected string
		switch {
		case store.Spec.Database.MariaDb != nil:
			expected = store.Name + mariaDbSuffix
		case store.Spec.Database.MariaDbRef != nil:
			expected = store.Spec.Database.MariaDbRef.Name
		default:
			continue
		}
		if expected != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: store.Namespace,
			Name:      store.Name,
		}})
	}
	return out
}

// mapOpenSearchToMageStores is the OpenSearchCluster equivalent of
// mapMariaDBToMageStores — reconcile every MageStore in the namespace
// whose search spec points at the changed cluster. Handles both inline
// mode (cluster name derived as `<store>-opensearch`) and ref mode
// (cluster name from spec.search.opensearchRef.name). For inline mode
// the ownerReference would also let us use `.Owns()` directly, but
// mirroring the ref-mode path here keeps both modes on the same plumbing.
func (r *MageStoreReconciler) mapOpenSearchToMageStores(ctx context.Context, obj client.Object) []reconcile.Request {
	var list magev1alpha1.MageStoreList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, store := range list.Items {
		if store.Spec.Search == nil {
			continue
		}
		var expected string
		switch {
		case store.Spec.Search.OpenSearch != nil:
			expected = store.Name + openSearchSuffix
		case store.Spec.Search.OpenSearchRef != nil:
			expected = store.Spec.Search.OpenSearchRef.Name
		default:
			continue
		}
		if expected != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: store.Namespace,
			Name:      store.Name,
		}})
	}
	return out
}

// mapRabbitmqClusterToMageStores is the RabbitmqCluster equivalent of
// mapMariaDBToMageStores — reconcile every MageStore in the namespace
// whose queue spec points at the changed CR. Handles both inline mode
// (CR name derived as `<store>-rabbitmq`) and ref mode (CR name from
// spec.queue.rabbitMqRef.name). External mode has no CR to watch;
// spec changes there requeue via the MageStore watch directly.
func (r *MageStoreReconciler) mapRabbitmqClusterToMageStores(ctx context.Context, obj client.Object) []reconcile.Request {
	var list magev1alpha1.MageStoreList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, store := range list.Items {
		if store.Spec.Queue == nil {
			continue
		}
		var expected string
		switch {
		case store.Spec.Queue.RabbitMq != nil:
			expected = store.Name + rabbitMqSuffix
		case store.Spec.Queue.RabbitMqRef != nil:
			expected = store.Spec.Queue.RabbitMqRef.Name
		default:
			continue
		}
		if expected != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: store.Namespace,
			Name:      store.Name,
		}})
	}
	return out
}

// SetupWithManager sets up the controller with the Manager. The
// Owns/Watches chain registers per-soft-dep watches conditionally on
// Capabilities. The four data-tier informers (MariaDB / OpenSearch /
// Valkey / RabbitmqCluster) are skipped when their CRD wasn't found at
// startup so the manager can boot on a partial-deps cluster. Gateway /
// HTTPRoute Owns() entries are not gated here because Gateway API is a
// hard requirement enforced fail-fast in main.go.
func (r *MageStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&magev1alpha1.MageStore{}).
		Owns(&appsv1.Deployment{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&gatewayv1.Gateway{}).
		Owns(&gatewayv1.HTTPRoute{})

	if r.Capabilities.HasMariaDB {
		b = b.Watches(
			&mariadbv1alpha1.MariaDB{},
			handler.EnqueueRequestsFromMapFunc(r.mapMariaDBToMageStores),
		)
	}
	if r.Capabilities.HasOpenSearch {
		b = b.Watches(
			&opensearchv1.OpenSearchCluster{},
			handler.EnqueueRequestsFromMapFunc(r.mapOpenSearchToMageStores),
		)
	}
	if r.Capabilities.HasRabbitmq {
		b = b.Watches(
			&rabbitmqv1beta1.RabbitmqCluster{},
			handler.EnqueueRequestsFromMapFunc(r.mapRabbitmqClusterToMageStores),
		)
	}

	return b.Named("magestore").Complete(r)
}

// validateMageStoreSpec checks invariants that must hold before any
// ensure* step runs. Returns (reason, error) — empty reason + nil
// error means the spec is valid; otherwise the caller surfaces
// Ready=False with the given Reason and Message=err.Error().
//
// Currently checks:
//   - spec.routing.tls set requires spec.stores non-empty (per ADR 010,
//     TLS listeners need SNI hostnames).
//
// Add invariants here as they emerge — keep it cheap (no API calls).
func validateMageStoreSpec(store *magev1alpha1.MageStore) (string, error) {
	if store.Spec.Routing != nil && store.Spec.Routing.TLS != nil && len(store.Spec.Stores) == 0 {
		return ReasonTLSRequiresStoreHosts, fmt.Errorf(
			"spec.routing.tls is set but spec.stores is empty; TLS listeners require at least one hostname for SNI (see ADR 010)",
		)
	}
	return "", nil
}

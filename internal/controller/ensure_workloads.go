package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
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

// workloadKind names one of the three Magento workload types. ADR 006
// pins the values (`frontend`, `admin`, `api`) — they surface in
// Deployment / Service / HPA names (`<store>-<kind>`) and in the
// `app.kubernetes.io/component` label.
type workloadKind string

const (
	workloadFrontend workloadKind = "frontend"
	workloadAdmin    workloadKind = "admin"
	workloadAPI      workloadKind = "api"
)

// allWorkloadKinds drives both the materialise-declared-kinds and
// delete-orphan-kinds passes. Stable order keeps reconcile logs
// deterministic when multiple workloads roll out in the same pass.
var allWorkloadKinds = []workloadKind{workloadFrontend, workloadAdmin, workloadAPI}

// workloadImagePullPolicy returns the pull policy for every container
// the operator runs from the MageStore's image: the workload
// Deployments (frontend / admin / api, queue consumers, Varnish) and
// the one-shot Jobs (bootstrap, SMTP probe, store URLs, media seed,
// upgrade). The Jobs used to pin `PullAlways`, which made a store
// impossible to install from an image that only exists on the node
// (kind-loaded or air-gapped).
//
// Default `PullAlways` defends against stale node-cached
// layers when the reference image's mutable tag (e.g.
// `:2.4.8-sampledata`) is rebuilt across releases — kubelet
// re-resolves the digest on every pod start instead of trusting the
// local cache. The CRD applies the same default at admission time, so
// in normal flow `Spec.Workloads.ImagePullPolicy` is already populated;
// the empty fallback here covers test fixtures and direct Go
// construction that bypass CRD defaulting.
func workloadImagePullPolicy(store *magev1alpha1.MageStore) corev1.PullPolicy {
	if p := store.Spec.Workloads.ImagePullPolicy; p != "" {
		return p
	}
	return corev1.PullAlways
}

// ensureWorkloads owns per-workload Deployments, Services, and HPAs for
// the three workload kinds (frontend always, admin/api when declared).
// Orphan resources from previously-declared-then-unset kinds are
// deleted. Services are materialised unconditionally for declared kinds
// (they can exist before pods are ready, and routing activates as soon
// as endpoints appear); Deployment + HPA creation gates on
// dependenciesReady. WorkloadsReady is True iff every declared
// workload's Deployment has AvailableReplicas >= desired.
//
// Session 2 ships single-container Deployments — no nginx sidecar. ADR
// 007's multi-store dispatch requires `<store>-nginx-conf` mounted into
// an nginx that carries the map + fastcgi_param directives, and that
// mount lands in Session 3 alongside the reference-image contract
// update (the current reference image co-hosts nginx+php-fpm inline,
// with no `include` hook). The ConfigMap + Deployment pod-template
// hash annotation are already emitted here via ensureNginxConfig so
// Session 3's one-line mount addition triggers rollouts cleanly.
//
// Service and Deployment naming follow ADR 006: `<store>-frontend`,
// `<store>-admin`, `<store>-api`. Labels carry
// `app.kubernetes.io/component: <kind>` so the label selector uniquely
// picks each workload.
func (r *MageStoreReconciler) ensureWorkloads(ctx context.Context, store *magev1alpha1.MageStore, dependenciesReady, mediaReady bool, nginxConfigHash, configSecretHash, smtpSecretHash string, mediaEndpoint MediaEndpoint) (metav1.Condition, error) {
	log := ctrl.LoggerFrom(ctx)

	// Orphan sweep: admin / api that were declared in a previous
	// generation but are now unset must have their Deployment +
	// Service + HPA deleted. Frontend is CRD-required so it's never
	// an orphan — the loop still visits it but declaredWorkload
	// returns (_, true), short-circuiting the delete. Scoped to
	// workload kinds only; not a general orphan sweep.
	for _, kind := range allWorkloadKinds {
		if _, declared := declaredWorkload(store, kind); declared {
			continue
		}
		if err := r.deleteWorkloadResources(ctx, store, kind); err != nil {
			return metav1.Condition{
				Type:    ConditionWorkloadsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "OrphanCleanupFailed",
				Message: err.Error(),
			}, fmt.Errorf("orphan cleanup for %s: %w", kind, err)
		}
	}

	// Services first, unconditionally for declared kinds. A Service
	// with no endpoints is harmless — routing activates as soon as
	// the Deployment's pods come ready — and emitting it before the
	// Deployment gate keeps ingress path-rule backends valid even
	// while dependencies are still provisioning.
	for _, kind := range allWorkloadKinds {
		if _, declared := declaredWorkload(store, kind); !declared {
			continue
		}
		if err := r.ensureWorkloadService(ctx, store, kind); err != nil {
			return metav1.Condition{
				Type:    ConditionWorkloadsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ApplyFailed",
				Message: err.Error(),
			}, fmt.Errorf("apply %s service: %w", kind, err)
		}
	}

	// S3+seedFromImage waits for the seed Job to populate the bucket
	// before pods start; otherwise pods would come up against an empty
	// bucket and serve broken-image icons. Volume modes (emptyDir, PVC)
	// don't gate here — the per-workload initContainer (C2.3) is the
	// gate at pod-start time. Per ADR 011 §"Reconcile graph and gating".
	if !mediaReady {
		return metav1.Condition{
			Type:    ConditionWorkloadsReady,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonAwaitingMediaSeed,
			Message: "skipping workload rollout until S3 media seeding completes",
		}, nil
	}

	if !dependenciesReady {
		return metav1.Condition{
			Type:    ConditionWorkloadsReady,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingDependencies",
			Message: "skipping workload rollout until dependencies are ready",
		}, nil
	}

	type workloadStatus struct {
		kind      workloadKind
		available int32
		desired   int32
	}
	statuses := make([]workloadStatus, 0, len(allWorkloadKinds))
	for _, kind := range allWorkloadKinds {
		spec, declared := declaredWorkload(store, kind)
		if !declared {
			continue
		}
		avail, desired, err := r.ensureWorkloadDeployment(ctx, store, kind, spec, nginxConfigHash, configSecretHash, smtpSecretHash, mediaEndpoint)
		if err != nil {
			return metav1.Condition{
				Type:    ConditionWorkloadsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ApplyFailed",
				Message: err.Error(),
			}, fmt.Errorf("apply %s deployment: %w", kind, err)
		}
		if err := r.ensureWorkloadHPA(ctx, store, kind, spec); err != nil {
			return metav1.Condition{
				Type:    ConditionWorkloadsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ApplyFailed",
				Message: err.Error(),
			}, fmt.Errorf("apply %s hpa: %w", kind, err)
		}
		statuses = append(statuses, workloadStatus{kind: kind, available: avail, desired: desired})
	}

	// Rollup: ANDed readiness across declared workloads. The first
	// still-rolling workload drives the Reason so users see the exact
	// kind blocking Ready.
	for _, s := range statuses {
		if s.available < s.desired {
			return metav1.Condition{
				Type:    ConditionWorkloadsReady,
				Status:  metav1.ConditionFalse,
				Reason:  "RolloutInProgress",
				Message: fmt.Sprintf("%s: %d/%d replicas available", s.kind, s.available, s.desired),
			}, nil
		}
	}

	parts := make([]string, 0, len(statuses))
	for _, s := range statuses {
		parts = append(parts, fmt.Sprintf("%s=%d/%d", s.kind, s.available, s.desired))
	}
	msg := "workloads ready: " + strings.Join(parts, ", ")
	log.V(1).Info("all workloads ready", "summary", msg)
	return metav1.Condition{
		Type:    ConditionWorkloadsReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReplicasAvailable",
		Message: msg,
	}, nil
}

// workloadName returns the shared name for a given workload kind's
// Deployment, Service, and HPA. ADR 006 pins the shape
// `<store>-<kind>`.
func workloadName(store *magev1alpha1.MageStore, kind workloadKind) string {
	return fmt.Sprintf("%s-%s", store.Name, kind)
}

// workloadLabels is the full app.kubernetes.io/* label set applied to
// every workload resource. The selector (workloadSelector) is a subset
// without `managed-by`, matching the Slot-18 rationale (selectors
// should be the minimum identity that uniquely picks the workload;
// `managed-by` is observability metadata, not identity).
func workloadLabels(store *magev1alpha1.MageStore, kind workloadKind) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  string(kind),
		"app.kubernetes.io/managed-by": managedByLabel,
	}
}

// workloadSelector is the identity subset of workloadLabels used for
// Deployment pod selectors and Service target selectors.
func workloadSelector(store *magev1alpha1.MageStore, kind workloadKind) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "magento",
		"app.kubernetes.io/instance":  store.Name,
		"app.kubernetes.io/component": string(kind),
	}
}

// declaredWorkload returns (spec, true) when the MageStore has declared
// this workload kind — frontend always, admin / api when their pointer
// fields are non-nil. Zero-value spec with false when the kind is
// undeclared; callers must check the bool before using spec.
func declaredWorkload(store *magev1alpha1.MageStore, kind workloadKind) (magev1alpha1.WorkloadSpec, bool) {
	switch kind {
	case workloadFrontend:
		return store.Spec.Workloads.Frontend, true
	case workloadAdmin:
		if store.Spec.Workloads.Admin == nil {
			return magev1alpha1.WorkloadSpec{}, false
		}
		return *store.Spec.Workloads.Admin, true
	case workloadAPI:
		if store.Spec.Workloads.Api == nil {
			return magev1alpha1.WorkloadSpec{}, false
		}
		return *store.Spec.Workloads.Api, true
	}
	return magev1alpha1.WorkloadSpec{}, false
}

// ensureWorkloadDeployment materialises the Deployment for a single
// workload kind. Returns (availableReplicas, desiredReplicas, err) so
// the caller can compute WorkloadsReady without a second Get. Desired
// replicas = spec.Replicas when autoscaling is unset; when set, the
// HPA owns the replica count — we seed it from spec.Replicas on first
// create and preserve the server-side value (HPA's latest scale
// decision) on subsequent reconciles.
func (r *MageStoreReconciler) ensureWorkloadDeployment(ctx context.Context, store *magev1alpha1.MageStore, kind workloadKind, spec magev1alpha1.WorkloadSpec, nginxConfigHash, configSecretHash, smtpSecretHash string, mediaEndpoint MediaEndpoint) (int32, int32, error) {
	name := workloadName(store, kind)
	labels := workloadLabels(store, kind)
	selector := workloadSelector(store, kind)
	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag)

	// Default replicas=1 mirrors the CRD default (workloads.<kind>.replicas),
	// which already fires at admission time for server-created objects.
	// Envtest and programmatic struct construction can bypass admission
	// defaulting and leave replicas=0; guard against that so the
	// Deployment is never created with zero replicas.
	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template.Labels = labels
		// nginx-config-hash annotation triggers rolling restart when
		// the `<store>-nginx-conf` ConfigMap content changes (ADR
		// 007). The hash is emitted now even though Session 2 doesn't
		// yet mount the ConfigMap — the wiring lets Session 3's mount
		// landing pick up rollouts on `spec.stores` edits without an
		// extra reconcile pass.
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		if nginxConfigHash != "" {
			dep.Spec.Template.Annotations[nginxConfigHashAnnotation] = nginxConfigHash
		}
		if configSecretHash != "" {
			dep.Spec.Template.Annotations["mage.aknostic.com/config-secret-hash"] = configSecretHash
		}
		if smtpSecretHash != "" {
			dep.Spec.Template.Annotations[smtpSecretHashAnnotation] = smtpSecretHash
		} else {
			// Prune the annotation when SMTP is removed from a previously-
			// configured store. The envFrom slice change already triggers a
			// rollout; the prune keeps the "annotation present ↔ SMTP mounted"
			// contract honest for kubectl describe / debugging.
			delete(dep.Spec.Template.Annotations, smtpSecretHashAnnotation)
		}

		// Replica ownership: when autoscaling is opted-in, the HPA owns
		// the replica count. CreateOrUpdate does a Get first, so
		// dep.Spec.Replicas already holds the server-side value (the
		// HPA's last scale decision). Seed from spec on first create
		// (replicas == nil), preserve otherwise.
		if spec.Autoscaling == nil {
			r := replicas
			dep.Spec.Replicas = &r
		} else if dep.Spec.Replicas == nil {
			r := replicas
			dep.Spec.Replicas = &r
		}

		// EnvFrom: operator-owned config Secret + conditional SMTP Secret.
		// The SMTP Secret is mounted only when spec.smtp is set and
		// SMTPReady=True (gated in the reconcile graph). No Optional:true —
		// reaching this branch means the Secret MUST exist.
		envFrom := []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
			}},
		}
		if smtpName := smtpEnvFromSecretName(store); smtpName != "" {
			envFrom = append(envFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: smtpName},
				},
			})
		}
		container := corev1.Container{
			Name:            "magento",
			Image:           image,
			ImagePullPolicy: workloadImagePullPolicy(store),
			Ports:           []corev1.ContainerPort{{ContainerPort: 8080, Name: "http"}},
			EnvFrom:         envFrom,
		}
		if spec.Resources != nil {
			container.Resources = *spec.Resources
		}
		// Media + nginx-conf mounts. Media: emptyDir for unset / s3
		// modes (scratch for the image-processing pipeline before
		// upload), PVC-backed for pvc mode. Nginx-conf: subPath
		// mount of `stores.conf` at
		// /etc/nginx/conf.d/stores.conf so the image's
		// `include /etc/nginx/conf.d/*.conf;` directive picks up
		// the two `map $http_host` blocks per ADR 007. subPath is
		// deliberate — mounting the whole ConfigMap at
		// /etc/nginx/conf.d/ would also pull in default.conf (a
		// fastcgi server block intended for the Session-4 sidecar
		// shape), which conflicts with the co-hosted image's own
		// server block on port 8080. subPath mounts don't auto-
		// update on ConfigMap edits, but the nginx-config-hash
		// pod-template annotation triggers a rollout whenever the
		// ConfigMap content changes, so the pod restart picks up
		// the new file.
		container.VolumeMounts = []corev1.VolumeMount{
			{
				Name:      mediaVolumeName,
				MountPath: mediaMountPath,
			},
			{
				Name:      "nginx-conf",
				MountPath: "/etc/nginx/conf.d/stores.conf",
				SubPath:   "stores.conf",
				ReadOnly:  true,
			},
		}
		dep.Spec.Template.Spec.Containers = []corev1.Container{container}
		dep.Spec.Template.Spec.Volumes = []corev1.Volume{
			mediaEndpoint.Volume,
			{
				Name: "nginx-conf",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: store.Name + "-nginx-conf",
						},
					},
				},
			},
		}

		// InitContainers: rebuilt from scratch each reconcile to avoid
		// append-on-requeue churn. For now, media-seed is the only init
		// container; future tasks append to this explicit-rebuild block.
		// S3 mode (EnvVars != nil) is excluded — ensureMediaSeed's Job
		// handles bucket population; cp -rn into an emptyDir scratch dir
		// would no-op against the bucket path.
		dep.Spec.Template.Spec.InitContainers = nil
		if mediaEndpoint.SeedFromImage && mediaEndpoint.EnvVars == nil {
			initContainer := corev1.Container{
				Name:            "media-seed",
				Image:           container.Image,
				ImagePullPolicy: container.ImagePullPolicy,
				Command:         []string{"sh", "-c"},
				Args:            []string{"cp -rn /app/pub/media-baked/. /app/pub/media/"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: mediaVolumeName, MountPath: mediaMountPath},
				},
			}
			dep.Spec.Template.Spec.InitContainers = []corev1.Container{initContainer}
		}

		return ctrl.SetControllerReference(store, dep, r.Scheme)
	})
	if err != nil {
		return 0, replicas, err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("workload deployment reconciled", "kind", string(kind), "op", string(op))
	}

	// Read current status off the object we just applied —
	// CreateOrUpdate populates it from the server response. The
	// available-replica count drives the WorkloadsReady rollup.
	var latest appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: name}, &latest); err != nil {
		return 0, replicas, err
	}

	desired := replicas
	if latest.Spec.Replicas != nil {
		desired = *latest.Spec.Replicas
	}
	return latest.Status.AvailableReplicas, desired, nil
}

// ensureWorkloadService materialises the ClusterIP Service fronting a
// workload's Deployment. Port 8080 / named `http` matches the image
// contract (see docs/image-contract.md "HTTP server").
func (r *MageStoreReconciler) ensureWorkloadService(ctx context.Context, store *magev1alpha1.MageStore, kind workloadKind) error {
	name := workloadName(store, kind)
	labels := workloadLabels(store, kind)
	selector := workloadSelector(store, kind)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		// Maintenance fence skip-gate. While the upgrade arc has
		// flipped this Service's selector at the maintenance pod
		// (annotated by ensureUpgrade.flipServicesToMaintenance), do
		// NOT overwrite the selector — that would revert the fence
		// on every reconcile and make the maintenance window
		// decorative. Ports/Type still reconcile normally; only the
		// selector is gated.
		if svc.Annotations[maintenanceFlippedAnnotation] != maintenanceFlippedValue {
			svc.Spec.Selector = selector
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       8080,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		}}
		return ctrl.SetControllerReference(store, svc, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("workload service reconciled", "kind", string(kind), "op", string(op))
	}
	return nil
}

// ensureWorkloadHPA creates or updates the HorizontalPodAutoscaler for
// a workload when spec.Autoscaling is set; deletes any existing HPA
// when autoscaling transitions from set to unset. Target metric is
// CPU utilisation (percentage of requests) — memory / custom metrics
// land when a user asks. ScaleTargetRef points at the workload's
// Deployment by name; `apps/v1` / `Deployment` are hard-coded because
// that's what ensureWorkloadDeployment emits.
func (r *MageStoreReconciler) ensureWorkloadHPA(ctx context.Context, store *magev1alpha1.MageStore, kind workloadKind, spec magev1alpha1.WorkloadSpec) error {
	name := workloadName(store, kind)

	if spec.Autoscaling == nil {
		// Delete any HPA that existed from a previous generation when
		// autoscaling was still set. IsNotFound is the success case.
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
		}
		if err := r.Delete(ctx, hpa); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	labels := workloadLabels(store, kind)

	// minReplicas: explicit override when spec.autoscaling.minReplicas
	// is set, otherwise seed from spec.replicas (default 1). HPA's
	// floor exists independently of spec.replicas so users can scale
	// down manually without re-reconciling.
	var minReplicas int32
	if spec.Autoscaling.MinReplicas != nil {
		minReplicas = *spec.Autoscaling.MinReplicas
	} else {
		minReplicas = spec.Replicas
		if minReplicas == 0 {
			minReplicas = 1
		}
	}

	// targetCPU defaults to 80 — matches the Kubernetes HPA default
	// and the CRD default. Explicit here for envtest paths that
	// bypass admission defaulting.
	var targetCPU int32 = 80
	if spec.Autoscaling.TargetCPUUtilizationPercentage != nil {
		targetCPU = *spec.Autoscaling.TargetCPUUtilizationPercentage
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		hpa.Labels = labels
		hpa.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       name,
		}
		min := minReplicas
		hpa.Spec.MinReplicas = &min
		hpa.Spec.MaxReplicas = spec.Autoscaling.MaxReplicas
		cpu := targetCPU
		hpa.Spec.Metrics = []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &cpu,
				},
			},
		}}
		return ctrl.SetControllerReference(store, hpa, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("workload hpa reconciled", "kind", string(kind), "op", string(op))
	}
	return nil
}

// deleteWorkloadResources removes the Deployment, Service, and HPA for
// a workload kind that's no longer declared. Called by the orphan
// sweep in ensureWorkloads. IsNotFound on any delete is the success
// case — the resource may never have existed (frontend path) or may
// have been deleted in a prior reconcile. Errors on any other failure
// mode surface through WorkloadsReady=False,
// Reason=OrphanCleanupFailed.
func (r *MageStoreReconciler) deleteWorkloadResources(ctx context.Context, store *magev1alpha1.MageStore, kind workloadKind) error {
	name := workloadName(store, kind)
	key := client.ObjectKey{Namespace: store.Namespace, Name: name}
	log := ctrl.LoggerFrom(ctx)

	targets := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}},
	}
	for _, obj := range targets {
		if err := r.Delete(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete %T %s: %w", obj, key, err)
		}
		log.Info("orphan workload resource deleted", "kind", string(kind), "resource", fmt.Sprintf("%T", obj))
	}
	return nil
}

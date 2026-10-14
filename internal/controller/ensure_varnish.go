package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
	"github.com/aknostic/mage-k8s-operator/internal/magento"
)

// varnishSuffix is the common suffix for Varnish Deployment + Service
// names. Matches the dependency-suffix convention used by mariaDb /
// openSearch / valkey / rabbitMq.
const varnishSuffix = "-varnish"

// varnishVCLKey is the expected key in the VCL ConfigMap (user-
// supplied OR operator-generated). User-supplied VCL via
// `spec.varnish.vclConfigMapRef` MUST put the body under this exact
// key so the operator mounts it at /etc/varnish/default.vcl (which
// varnishd loads with -f).
const varnishVCLKey = magento.DefaultVCLKey

// varnishDefaultVCLSuffix is the suffix for the operator-generated
// default VCL ConfigMap when the user does not supply one via
// `spec.varnish.vclConfigMapRef`. The ConfigMap is owned by the
// MageStore and swept when Varnish is disabled.
const varnishDefaultVCLSuffix = "-varnish-vcl"

// varnishVCLHashAnnotation stamps the SHA-256 of the user's VCL
// ConfigMap contents on the Varnish pod template. VCL edits
// (kubectl edit / kustomize overlay / GitOps push) rewrite the
// ConfigMap, the operator observes the new content hash on next
// reconcile, and the Deployment rolls out a fresh pod — same shape
// as ensure_nginx_config's nginx-config-hash.
const varnishVCLHashAnnotation = "mage.aknostic.com/vcl-config-hash"

// varnishHTTPPort is the Varnish client listener port inside the
// container. Varnish's historical default is 6081; we keep it
// explicit so the command-line flag in the Deployment and the
// targetPort on the Service stay consistent. The Service itself
// exposes port 8080 (matching every other workload / backend Service
// the HTTPRoute points at), which makes `ensureRouting`'s `/` rewire
// a pure backend-name swap with no port bookkeeping.
const varnishHTTPPort int32 = 6081

// ensureVarnish materialises the optional Varnish caching layer in
// front of `<store>-frontend`. Per ADR 004, Varnish is a discriminated
// subsystem: `spec.varnish` is nil or `enabled: false` → no Varnish
// resources, HTTPRoute points directly at the frontend. With
// `enabled: true`, the operator creates a `<store>-varnish`
// Deployment + Service; `ensureRouting` reroutes the HTTPRoute's `/`
// backend accordingly.
//
// **VCL source.** Two paths: when `spec.varnish.vclConfigMapRef` is
// nil, the operator generates a default VCL via
// `magento.GenerateDefaultVCL` and materialises a `<store>-varnish-vcl`
// ConfigMap from it (owned by the MageStore, swept on Varnish disable).
// When the field is set, the user-supplied ConfigMap is used as-is and
// the operator does not splice anything into it. Both paths feed the
// same Deployment + Service + hash-rollout machinery.
//
// `/admin`, `/rest`, `/graphql` continue to route directly to their
// backends (admin → admin-or-frontend, api → api-or-frontend) — only
// the `/` path fronts through Varnish. This matches Magento's
// convention (admin and REST bypass the cache; only storefront
// traffic benefits from HTTP caching). Users whose VCL handles
// admin/API differently own that in the VCL itself; the operator
// defines which URL classes *reach* Varnish, not what Varnish does
// with them once there.
//
// No watch-mapper: the Deployment and Service are Owned by the
// MageStore, and the VCL ConfigMap is user-managed. Users editing
// the ConfigMap out-of-band trigger a reconcile because we stamp
// the ConfigMap content hash on the Varnish pod template — rollouts
// pick up VCL changes on the next reconcile pass without an extra
// watch.
func (r *MageStoreReconciler) ensureVarnish(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, error) {
	// Not configured / disabled: sweep any stale resources from a
	// previous generation (user toggled enabled: true → false), then
	// return Unknown/NotEnabled so Ready doesn't block.
	if store.Spec.Varnish == nil || !store.Spec.Varnish.Enabled {
		if err := r.deleteVarnishResources(ctx, store); err != nil {
			return metav1.Condition{
				Type:    ConditionVarnishReady,
				Status:  metav1.ConditionFalse,
				Reason:  "OrphanCleanupFailed",
				Message: err.Error(),
			}, fmt.Errorf("orphan cleanup varnish: %w", err)
		}
		return metav1.Condition{
			Type:    ConditionVarnishReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotEnabled",
			Message: "spec.varnish.enabled is not true",
		}, nil
	}

	// VCL source: user-supplied ConfigMap (vclConfigMapRef set) or
	// operator-generated default ConfigMap (vclConfigMapRef nil). The
	// operator-generated path materialises `<store>-varnish-vcl` from
	// magento.GenerateDefaultVCL — multi-store hash key (per ADR 007)
	// is embedded when spec.stores has entries.
	var vclName string
	if store.Spec.Varnish.VclConfigMapRef != nil {
		vclName = store.Spec.Varnish.VclConfigMapRef.Name
	} else {
		name, err := r.ensureDefaultVCLConfigMap(ctx, store)
		if err != nil {
			return metav1.Condition{
				Type:    ConditionVarnishReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ApplyFailed",
				Message: err.Error(),
			}, fmt.Errorf("apply default vcl configmap: %w", err)
		}
		vclName = name
	}

	// VCL ConfigMap must exist and carry the expected key. For the
	// user-supplied path these are user errors (missing ConfigMap or
	// missing key); for the operator-generated path the Get-after-
	// create is a sanity check that catches eventual-consistency races
	// between the create-or-update above and the Get below.
	var vcl corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: vclName}, &vcl); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.Condition{
				Type:    ConditionVarnishReady,
				Status:  metav1.ConditionFalse,
				Reason:  "VCLConfigMapMissing",
				Message: fmt.Sprintf("ConfigMap %q not found in namespace %q", vclName, store.Namespace),
			}, nil
		}
		return metav1.Condition{
			Type:    ConditionVarnishReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "LookupError",
			Message: err.Error(),
		}, fmt.Errorf("get VCL ConfigMap: %w", err)
	}
	if _, ok := vcl.Data[varnishVCLKey]; !ok {
		return metav1.Condition{
			Type:    ConditionVarnishReady,
			Status:  metav1.ConditionFalse,
			Reason:  "VCLKeyMissing",
			Message: fmt.Sprintf("ConfigMap %q has no key %q (expected VCL body under this key)", vclName, varnishVCLKey),
		}, nil
	}
	vclHash := hashConfigMapData(vcl.Data)

	if err := r.ensureVarnishService(ctx, store); err != nil {
		return metav1.Condition{
			Type:    ConditionVarnishReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ApplyFailed",
			Message: err.Error(),
		}, fmt.Errorf("apply varnish service: %w", err)
	}

	avail, desired, err := r.ensureVarnishDeployment(ctx, store, vclName, vclHash)
	if err != nil {
		return metav1.Condition{
			Type:    ConditionVarnishReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ApplyFailed",
			Message: err.Error(),
		}, fmt.Errorf("apply varnish deployment: %w", err)
	}

	if avail < desired {
		return metav1.Condition{
			Type:    ConditionVarnishReady,
			Status:  metav1.ConditionFalse,
			Reason:  "RolloutInProgress",
			Message: fmt.Sprintf("varnish: %d/%d replicas available", avail, desired),
		}, nil
	}

	return metav1.Condition{
		Type:    ConditionVarnishReady,
		Status:  metav1.ConditionTrue,
		Reason:  "VarnishReady",
		Message: fmt.Sprintf("varnish: %d/%d replicas available", avail, desired),
	}, nil
}

// ensureVarnishService materialises the `<store>-varnish` ClusterIP
// Service. Port 8080 → targetPort "http" (container port 6081) mirrors
// the workload Service shape so `ensureRouting`'s `/` rewire is a
// pure backend-name swap.
func (r *MageStoreReconciler) ensureVarnishService(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + varnishSuffix
	labels := varnishLabels(store)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = varnishSelector(store)
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
		ctrl.LoggerFrom(ctx).Info("varnish service reconciled", "op", string(op))
	}
	return nil
}

// ensureVarnishDeployment materialises the Varnish Deployment.
// Returns (availableReplicas, desiredReplicas, err) so the caller can
// compute readiness without a second Get. Command is explicit —
// `varnishd -F -f /etc/varnish/default.vcl -a :6081 ...` — rather than
// leaning on the upstream image's entrypoint, so the container shape
// is stable across Varnish image minor bumps.
func (r *MageStoreReconciler) ensureVarnishDeployment(ctx context.Context, store *magev1alpha1.MageStore, vclConfigMapName, vclHash string) (int32, int32, error) {
	name := store.Name + varnishSuffix
	labels := varnishLabels(store)
	selector := varnishSelector(store)

	spec := store.Spec.Varnish
	version := spec.Version
	if version == "" {
		version = "7.5"
	}
	image := "docker.io/library/varnish:" + version

	replicas := spec.Replicas
	if replicas == 0 {
		// CRD default is 2, but envtest and programmatic construction
		// can bypass admission defaulting. Mirror the CRD default so
		// the operator never emits a zero-replica Deployment.
		replicas = 2
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template.Labels = labels

		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations[varnishVCLHashAnnotation] = vclHash

		count := replicas
		dep.Spec.Replicas = &count

		container := corev1.Container{
			Name:            "varnish",
			Image:           image,
			ImagePullPolicy: workloadImagePullPolicy(store),
			Command: []string{
				"varnishd",
				"-F",
				"-f", "/etc/varnish/default.vcl",
				"-a", fmt.Sprintf(":%d", varnishHTTPPort),
				"-s", "malloc,256m",
				"-t", "120",
			},
			Ports: []corev1.ContainerPort{{
				ContainerPort: varnishHTTPPort,
				Name:          "http",
			}},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "vcl",
				MountPath: "/etc/varnish",
				ReadOnly:  true,
			}},
		}
		if spec.Resources != nil {
			container.Resources = *spec.Resources
		}
		dep.Spec.Template.Spec.Containers = []corev1.Container{container}
		dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name: "vcl",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: vclConfigMapName},
				},
			},
		}}

		return ctrl.SetControllerReference(store, dep, r.Scheme)
	})
	if err != nil {
		return 0, replicas, err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("varnish deployment reconciled", "op", string(op))
	}

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

// deleteVarnishResources removes Deployment + Service + the operator-
// generated default-VCL ConfigMap for a Varnish that is no longer
// enabled. Called by the orphan sweep in ensureVarnish. IsNotFound on
// any delete is the success case — the resources may never have
// existed (e.g., the user used vclConfigMapRef and the default
// ConfigMap was never created) or may have been cleaned up by a prior
// reconcile. User-supplied VCL ConfigMaps (vclConfigMapRef) are not
// touched by this sweep — the operator never owned them.
func (r *MageStoreReconciler) deleteVarnishResources(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + varnishSuffix
	defaultVCLName := store.Name + varnishDefaultVCLSuffix
	log := ctrl.LoggerFrom(ctx)

	targets := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: defaultVCLName, Namespace: store.Namespace}},
	}
	for _, obj := range targets {
		if err := r.Delete(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete %T %s/%s: %w", obj, store.Namespace, obj.GetName(), err)
		}
		log.Info("orphan varnish resource deleted", "resource", fmt.Sprintf("%T", obj), "name", obj.GetName())
	}
	return nil
}

// ensureDefaultVCLConfigMap materialises `<store>-varnish-vcl` with
// the operator-generated VCL body from magento.GenerateDefaultVCL.
// Multi-store hash key (ADR 007) is included automatically when
// spec.stores has entries. Owned by the MageStore so cluster-wide
// `kubectl delete magestore` cleans it up; explicitly swept by
// deleteVarnishResources on enabled: true → false.
//
// Idempotent via CreateOrUpdate: the body is regenerated from spec on
// every reconcile, and the ConfigMap content hash drives the Varnish
// pod template's vcl-config-hash annotation so spec.stores edits roll
// out the new body automatically.
func (r *MageStoreReconciler) ensureDefaultVCLConfigMap(ctx context.Context, store *magev1alpha1.MageStore) (string, error) {
	name := store.Name + varnishDefaultVCLSuffix
	multiStore := len(store.Spec.Stores) > 0
	body := magento.GenerateDefaultVCL(store.Name, multiStore)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = varnishLabels(store)
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[varnishVCLKey] = body
		return ctrl.SetControllerReference(store, cm, r.Scheme)
	})
	if err != nil {
		return "", err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("default vcl configmap reconciled", "op", string(op), "name", name)
	}
	return name, nil
}

func varnishLabels(store *magev1alpha1.MageStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  "varnish",
		"app.kubernetes.io/managed-by": managedByLabel,
	}
}

func varnishSelector(store *magev1alpha1.MageStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "magento",
		"app.kubernetes.io/instance":  store.Name,
		"app.kubernetes.io/component": "varnish",
	}
}

// banVarnish issues a full-cache ban in every running Varnish pod via
// `varnishadm ban "obj.http.X-Magento-Tags ~ .*"`. Called at the end of
// the upgrade arc's stage-2 finalise so stale HTML / stale JS hash
// references don't serve from Varnish after a deployment.
//
// Best-effort: a failed ban is logged but does not block upgrade
// completion. Stale cache entries expire within the Varnish TTL window
// regardless; the ban simply eliminates the window. Skipped gracefully
// when KubeClient is nil (envtest / unit test contexts).
func (r *MageStoreReconciler) banVarnish(ctx context.Context, store *magev1alpha1.MageStore) {
	if store.Spec.Varnish == nil || !store.Spec.Varnish.Enabled {
		return
	}
	if r.KubeClient == nil {
		return
	}
	log := ctrl.LoggerFrom(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(store.Namespace),
		client.MatchingLabels(varnishSelector(store)),
	); err != nil {
		log.Error(err, "varnish ban: list pods failed")
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if err := r.execVarnishBan(ctx, pod.Namespace, pod.Name); err != nil {
			log.Error(err, "varnish ban: exec failed", "pod", pod.Name)
		} else {
			log.Info("varnish ban issued", "pod", pod.Name)
		}
	}
}

func (r *MageStoreReconciler) execVarnishBan(ctx context.Context, namespace, podName string) error {
	req := r.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "varnish",
			Command:   []string{"varnishadm", "ban", "obj.http.X-Magento-Tags ~ .*"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build executor: %w", err)
	}
	var stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &bytes.Buffer{},
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("stream: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}

// hashConfigMapData is a general SHA-256 over a ConfigMap's data map.
// Sorted keys + null-byte delimiters keep the hash stable across map-
// iteration order and key-boundary ambiguity. Shared with
// ensure_nginx_config's computeNginxConfigHash in spirit; keeping a
// separate copy avoids cross-file coupling for a handful of lines.
func hashConfigMapData(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(data[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

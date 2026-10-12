package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// Naming for operator-owned maintenance resources.
const (
	maintenanceSuffix           = "-maintenance"
	maintenanceConfigSuffix     = "-maintenance-config"
	maintenanceDefault503Suffix = "-maintenance-default-503"
	maintenanceComponent        = "maintenance"
	maintenanceContainer        = "nginx"
	maintenanceImage            = "docker.io/library/nginx:1.27-alpine"
	maintenanceContainerPort    = 8080
	maintenanceHTMLKey          = "index.html"
	maintenanceConfKey          = "default.conf"
)

// maintenanceNginxConf is the nginx configuration baked into the
// `<store>-maintenance-config` ConfigMap. nginx listens on 8080 to
// match the workload Services' port (so the Service-selector flip is
// a pure target-pod swap with no port translation), and returns 503
// for every path while serving /index.html as the body via
// error_page redirection. The internal-only /index.html guards
// against a curl probing /index.html directly and getting a 200 — the
// rewrite path stays an internal nginx detail.
const maintenanceNginxConf = `server {
  listen 8080 default_server;
  charset utf-8;
  root /usr/share/nginx/html;

  error_page 503 /index.html;
  location / {
    return 503;
  }
  location = /index.html {
    internal;
  }
}
`

// maintenanceDefaultHTML is the operator-baked 503 page. Plain HTML —
// no per-request content, no PHP, no JavaScript. Shops that want
// branded maintenance pages override via
// `spec.upgrade.maintenance.htmlConfigMapRef`.
const maintenanceDefaultHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>We'll be right back</title>
</head>
<body>
  <h1>We'll be right back</h1>
  <p>This store is undergoing scheduled maintenance. Please check back in a few minutes.</p>
</body>
</html>
`

// ensureMaintenance materialises the maintenance Deployment, Service,
// and supporting ConfigMap(s) used to fence storefront traffic during
// an upgrade arc. Returns (ready, err) — ready is true when at least
// one maintenance pod has reached Available, signalling the caller it
// is safe to flip workload Service selectors at the maintenance pod.
//
// Layout:
//
//   - Always: `<store>-maintenance-config` ConfigMap with `default.conf`
//     (nginx config) — this lives apart from the HTML so the override
//     path doesn't have to duplicate operator-internal config.
//   - Default mode: `<store>-maintenance-default-503` ConfigMap with
//     `index.html` (operator-baked HTML).
//   - Override mode: skip the default-503 ConfigMap; the user's
//     ConfigMap (named in `spec.upgrade.maintenance.htmlConfigMapRef`)
//     supplies the HTML. The user-provided key is mounted at
//     /usr/share/nginx/html/index.html via subPath, so any key name in
//     the user's ConfigMap works.
//   - `<store>-maintenance` Deployment: one replica, nginx:1.27-alpine,
//     two volume mounts (nginx config + html).
//   - `<store>-maintenance` Service: ClusterIP, port 8080.
//
// All resources carry an OwnerReference to the MageStore so cascade-
// deletion cleans them up. The deletion at arc-finalise (handled in
// the caller, not here) lets ownerReference handle the GC, but the
// caller can also call deleteMaintenance for prompt removal.
func (r *MageStoreReconciler) ensureMaintenance(ctx context.Context, store *magev1alpha1.MageStore) (bool, error) {
	override := maintenanceHTMLOverride(store)

	if err := r.ensureMaintenanceConfigConfigMap(ctx, store); err != nil {
		return false, fmt.Errorf("ensure maintenance config configmap: %w", err)
	}
	if override == nil {
		if err := r.ensureMaintenanceDefault503ConfigMap(ctx, store); err != nil {
			return false, fmt.Errorf("ensure maintenance default-503 configmap: %w", err)
		}
	} else {
		// Clean up the default ConfigMap if the user transitioned from
		// default to override mid-arc — uncommon but possible.
		if err := r.deleteMaintenanceDefault503ConfigMap(ctx, store); err != nil {
			return false, fmt.Errorf("clean up maintenance default-503 configmap: %w", err)
		}
	}
	available, err := r.ensureMaintenanceDeployment(ctx, store, override)
	if err != nil {
		return false, fmt.Errorf("ensure maintenance deployment: %w", err)
	}
	if err := r.ensureMaintenanceService(ctx, store); err != nil {
		return false, fmt.Errorf("ensure maintenance service: %w", err)
	}
	return available > 0, nil
}

// deleteMaintenance removes the maintenance Deployment, Service, and
// owned ConfigMaps. Called at arc-finalise (workloads ready, selectors
// restored, ban issued — no further need for the 503 pod). Does not
// error on already-deleted resources.
func (r *MageStoreReconciler) deleteMaintenance(ctx context.Context, store *magev1alpha1.MageStore) error {
	objects := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: store.Name + maintenanceSuffix, Namespace: store.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: store.Name + maintenanceSuffix, Namespace: store.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: store.Name + maintenanceConfigSuffix, Namespace: store.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: store.Name + maintenanceDefault503Suffix, Namespace: store.Namespace}},
	}
	for _, obj := range objects {
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("delete %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// maintenanceHTMLOverride returns the user's htmlConfigMapRef (with
// the `Key` field defaulted to "index.html" if unset). Returns nil
// when no override is configured.
func maintenanceHTMLOverride(store *magev1alpha1.MageStore) *magev1alpha1.MaintenanceHtmlConfigMapRef {
	if store.Spec.Upgrade == nil || store.Spec.Upgrade.Maintenance == nil {
		return nil
	}
	ref := store.Spec.Upgrade.Maintenance.HtmlConfigMapRef
	if ref == nil {
		return nil
	}
	out := *ref
	if out.Key == "" {
		out.Key = maintenanceHTMLKey
	}
	return &out
}

// maintenanceEnabled checks whether the maintenance fence should be
// applied. Default true — opt-out is opt-in via
// `spec.upgrade.maintenance.enabled: false`. Pointer-typed Enabled
// (so kubebuilder default applies on omission) means we have three
// states to map: nil → true (default), &true → true, &false → false.
func maintenanceEnabled(store *magev1alpha1.MageStore) bool {
	if store.Spec.Upgrade == nil || store.Spec.Upgrade.Maintenance == nil {
		return true
	}
	if store.Spec.Upgrade.Maintenance.Enabled == nil {
		return true
	}
	return *store.Spec.Upgrade.Maintenance.Enabled
}

// maintenanceLabels matches the workload-label shape so log/metrics
// tooling that filters on `app.kubernetes.io/component` distinguishes
// the maintenance pod from frontend/admin/api/varnish.
func maintenanceLabels(store *magev1alpha1.MageStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  maintenanceComponent,
		"app.kubernetes.io/managed-by": managedByLabel,
	}
}

// maintenanceSelector is the identity subset used for Deployment and
// Service selectors. Stripping `managed-by` from the selector matches
// the workload-Service convention.
func maintenanceSelector(store *magev1alpha1.MageStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "magento",
		"app.kubernetes.io/instance":  store.Name,
		"app.kubernetes.io/component": maintenanceComponent,
	}
}

func (r *MageStoreReconciler) ensureMaintenanceConfigConfigMap(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + maintenanceConfigSuffix
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = maintenanceLabels(store)
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[maintenanceConfKey] = maintenanceNginxConf
		return ctrl.SetControllerReference(store, cm, r.Scheme)
	})
	return err
}

func (r *MageStoreReconciler) ensureMaintenanceDefault503ConfigMap(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + maintenanceDefault503Suffix
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = maintenanceLabels(store)
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[maintenanceHTMLKey] = maintenanceDefaultHTML
		return ctrl.SetControllerReference(store, cm, r.Scheme)
	})
	return err
}

func (r *MageStoreReconciler) deleteMaintenanceDefault503ConfigMap(ctx context.Context, store *magev1alpha1.MageStore) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      store.Name + maintenanceDefault503Suffix,
		Namespace: store.Namespace,
	}}
	return client.IgnoreNotFound(r.Delete(ctx, cm))
}

func (r *MageStoreReconciler) ensureMaintenanceDeployment(ctx context.Context, store *magev1alpha1.MageStore, override *magev1alpha1.MaintenanceHtmlConfigMapRef) (int32, error) {
	name := store.Name + maintenanceSuffix
	labels := maintenanceLabels(store)
	selector := maintenanceSelector(store)

	htmlConfigMapName := store.Name + maintenanceDefault503Suffix
	htmlKey := maintenanceHTMLKey
	if override != nil {
		htmlConfigMapName = override.Name
		htmlKey = override.Key
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template.Labels = labels

		one := int32(1)
		dep.Spec.Replicas = &one

		dep.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  maintenanceContainer,
			Image: maintenanceImage,
			Ports: []corev1.ContainerPort{{
				Name:          "http",
				ContainerPort: maintenanceContainerPort,
				Protocol:      corev1.ProtocolTCP,
			}},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "nginx-config",
					MountPath: "/etc/nginx/conf.d/" + maintenanceConfKey,
					SubPath:   maintenanceConfKey,
					ReadOnly:  true,
				},
				{
					Name:      "html",
					MountPath: "/usr/share/nginx/html/" + maintenanceHTMLKey,
					SubPath:   maintenanceHTMLKey,
					ReadOnly:  true,
				},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(maintenanceContainerPort)},
				},
				PeriodSeconds: 5,
			},
		}}
		dep.Spec.Template.Spec.Volumes = []corev1.Volume{
			{
				Name: "nginx-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: store.Name + maintenanceConfigSuffix,
						},
					},
				},
			},
			{
				Name: "html",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: htmlConfigMapName},
						Items: []corev1.KeyToPath{
							{Key: htmlKey, Path: maintenanceHTMLKey},
						},
					},
				},
			},
		}
		return ctrl.SetControllerReference(store, dep, r.Scheme)
	})
	if err != nil {
		return 0, err
	}

	var latest appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: name}, &latest); err != nil {
		return 0, err
	}
	return latest.Status.AvailableReplicas, nil
}

func (r *MageStoreReconciler) ensureMaintenanceService(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + maintenanceSuffix
	labels := maintenanceLabels(store)
	selector := maintenanceSelector(store)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = selector
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       maintenanceContainerPort,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		}}
		return ctrl.SetControllerReference(store, svc, r.Scheme)
	})
	return err
}

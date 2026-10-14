package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
	"github.com/aknostic/mage-k8s-operator/internal/magento"
)

// consumerResourcePrefix slots between the store name and the
// sanitised consumer name: `<store>-consumer-<sanitised>`. The
// dedicated "consumer-" infix distinguishes queue-consumer
// Deployments from workload Deployments (`<store>-frontend` etc.)
// at a glance and lets a future `spec.consumers.extras[]` field
// coexist with the known list without naming collisions.
const consumerResourcePrefix = "-consumer-"

// consumerTerminationGracePeriodSeconds matches CLAUDE.md's
// architecture note: Magento consumers do not handle SIGTERM
// cleanly — they're killed mid-message on graceful termination.
// 60s gives the consumer time to drain its current batch before
// the kernel forces a kill; customers whose consumers take longer
// override via a future `spec.consumers.overrides.terminationGrace`
// field (not in v0.1.0 scope).
const consumerTerminationGracePeriodSeconds int64 = 60

// consumerDesiredReplicas is fixed at 1 — Magento consumers do not
// coordinate across replicas, running two of the same consumer
// double-dispatches every message. Customers scale a consumer-heavy
// workload by scaling ACROSS consumer names (enable more consumer
// types), not replicas within one name.
const consumerDesiredReplicas int32 = 1

// ensureConsumers materialises one Deployment per entry in
// `internal/magento.Consumers` when `spec.consumers.enabled: true`.
// Orchestrates four passes: override validation (surface unknown
// keys), orphan sweep (stale consumer Deployments from a prior
// known-list revision), gate evaluation (queue must be configured,
// dependencies must be ready), and per-consumer fan-out.
//
// Gate semantics, sharpest first:
//
//   - `spec.consumers == nil` or `enabled: false` → Unknown/NotEnabled,
//     orphan-sweep any prior consumer Deployments. No error — consumers
//     are genuinely optional (Magento runs in synchronous-processing
//     mode without them).
//   - `spec.queue == nil` → False/QueueNotConfigured. Per the CRD
//     comment on ConsumersSpec, the gate is a runtime condition (not
//     a CEL rule) so the operator can serve a useful error message
//     to users who toggle consumers on without a broker.
//   - `!dependenciesReady` → False/AwaitingDependencies. Consumers
//     connect to DB + Redis + RabbitMQ at startup; without a
//     config Secret they crashloop instantly.
//   - unknown override keys → False/UnknownConsumerOverride, but
//     known-consumer Deployments still materialise (the unknown key
//     is a user error that shouldn't break the ones we DO know about).
//   - otherwise → True/AllReady or False/RolloutInProgress.
//
// Per-consumer Deployment shape: `replicas: 1` (Magento consumers
// do not coordinate across replicas — running two of the same
// consumer double-dispatches messages), `terminationGracePeriodSeconds:
// 60`, single container running `bin/magento queue:consumers:start
// <consumer-name>`, envFrom the operator's config Secret + optional
// SMTP Secret (same shape as workload Deployments). No Service — the
// consumer never accepts inbound connections. No HPA — consumer
// scale is per-consumer-name, not per-replica.
func (r *MageStoreReconciler) ensureConsumers(ctx context.Context, store *magev1alpha1.MageStore, dependenciesReady bool, smtpSecretHash string, mediaVolume corev1.Volume) (metav1.Condition, error) {
	if store.Spec.Consumers == nil || !store.Spec.Consumers.Enabled {
		if err := r.deleteAllConsumerResources(ctx, store); err != nil {
			return metav1.Condition{
				Type:    ConditionConsumersReady,
				Status:  metav1.ConditionFalse,
				Reason:  "OrphanCleanupFailed",
				Message: err.Error(),
			}, fmt.Errorf("orphan cleanup consumers: %w", err)
		}
		return metav1.Condition{
			Type:    ConditionConsumersReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotEnabled",
			Message: "spec.consumers.enabled is not true",
		}, nil
	}

	// spec.queue must be set — consumers without a broker have
	// nothing to consume and would crashloop trying to connect to a
	// nonexistent RabbitMQ Service. CRD-level XValidation can't
	// express "if A then B" cleanly, so we surface it as a runtime
	// condition per the CRD comment.
	if store.Spec.Queue == nil {
		return metav1.Condition{
			Type:    ConditionConsumersReady,
			Status:  metav1.ConditionFalse,
			Reason:  "QueueNotConfigured",
			Message: "spec.consumers.enabled: true requires spec.queue to be set (no broker configured)",
		}, nil
	}

	unknownOverrides := unknownConsumerOverrides(store.Spec.Consumers.Overrides)

	if !dependenciesReady {
		return metav1.Condition{
			Type:    ConditionConsumersReady,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingDependencies",
			Message: "holding consumer rollout until dependencies are ready",
		}, nil
	}

	type consumerStatus struct {
		name      string
		available int32
		desired   int32
	}
	statuses := make([]consumerStatus, 0, len(magento.Consumers))
	for _, name := range magento.Consumers {
		override := store.Spec.Consumers.Overrides[name]
		avail, err := r.ensureConsumerDeployment(ctx, store, name, override, smtpSecretHash, mediaVolume)
		if err != nil {
			return metav1.Condition{
				Type:    ConditionConsumersReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ApplyFailed",
				Message: err.Error(),
			}, fmt.Errorf("apply consumer %s: %w", name, err)
		}
		statuses = append(statuses, consumerStatus{name: name, available: avail, desired: consumerDesiredReplicas})
	}

	// Unknown override keys surface AFTER the known consumers are
	// materialised, so the user error doesn't cascade and block
	// deployments that are otherwise fine. The condition still
	// surfaces False so `kubectl describe` flags the typo.
	if len(unknownOverrides) > 0 {
		return metav1.Condition{
			Type:    ConditionConsumersReady,
			Status:  metav1.ConditionFalse,
			Reason:  "UnknownConsumerOverride",
			Message: fmt.Sprintf("unknown consumer names in spec.consumers.overrides: %s (see internal/magento/consumers.go for the canonical list)", strings.Join(unknownOverrides, ", ")),
		}, nil
	}

	for _, s := range statuses {
		if s.available < s.desired {
			return metav1.Condition{
				Type:    ConditionConsumersReady,
				Status:  metav1.ConditionFalse,
				Reason:  "RolloutInProgress",
				Message: fmt.Sprintf("%s: %d/%d replicas available", s.name, s.available, s.desired),
			}, nil
		}
	}

	return metav1.Condition{
		Type:    ConditionConsumersReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: fmt.Sprintf("%d consumer Deployments ready", len(statuses)),
	}, nil
}

// ensureConsumerDeployment materialises one consumer's Deployment.
// Naming: `<store>-consumer-<sanitised>`. The sanitiser handles the
// Magento consumer-name character set (dots, underscores, mixed
// case) → DNS-1123 subdomain. Returns availableReplicas; desired
// is always consumerDesiredReplicas (1) so the caller needn't track
// it per-call.
//
// Every consumer Deployment mounts the same media volume the
// workload Deployments use, at /app/pub/media. The image-resize
// consumer needs this for its source-image staging in s3 mode (the
// remote-storage driver pulls source images into local pub/media,
// generates variants, uploads back); other consumers don't actively
// touch media but mounting uniformly keeps the consumer pod template
// shape parallel with the workload pod template — no per-consumer
// branching on whether to mount. PVC mode mounts the same RWO PVC
// the workload Deployments mount, which surfaces an inherent
// PVC-RWO-conflict for any pod beyond the first; PVC mode is
// dev/test only per CLAUDE.md and that conflict is documented
// rather than worked around.
func (r *MageStoreReconciler) ensureConsumerDeployment(ctx context.Context, store *magev1alpha1.MageStore, consumerName string, override magev1alpha1.ConsumerOverride, smtpSecretHash string, mediaVolume corev1.Volume) (int32, error) {
	name := consumerResourceName(store, consumerName)
	labels := consumerLabels(store, consumerName)
	selector := consumerSelector(store, consumerName)
	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template.Labels = labels

		replicas := consumerDesiredReplicas
		dep.Spec.Replicas = &replicas

		grace := consumerTerminationGracePeriodSeconds
		dep.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace

		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		if smtpSecretHash != "" {
			dep.Spec.Template.Annotations[smtpSecretHashAnnotation] = smtpSecretHash
		} else {
			// Prune on SMTP removal — see ensure_workloads.go for rationale.
			delete(dep.Spec.Template.Annotations, smtpSecretHashAnnotation)
		}

		consumerEnvFrom := []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
			}},
		}
		if smtpName := smtpEnvFromSecretName(store); smtpName != "" {
			consumerEnvFrom = append(consumerEnvFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: smtpName},
				},
			})
		}
		container := corev1.Container{
			Name:            "consumer",
			Image:           image,
			ImagePullPolicy: workloadImagePullPolicy(store),
			// Bound each consumer process by message count AND wall-clock
			// time. Magento consumers leak memory over time and SIGTERM is
			// unreliable (per terminationGracePeriodSeconds=60 above), so
			// the process needs to exit cleanly on its own schedule and let
			// the kubelet restart the pod for the next batch. We wrap with
			// coreutils `timeout 3600` (exit 124 on hit) instead of
			// Magento's own --max-time, which does not exist in 2.4.x.
			Command: []string{"timeout", "3600", "bin/magento"},
			Args: []string{
				"queue:consumers:start",
				consumerName,
				"--max-messages=20000",
			},
			EnvFrom: consumerEnvFrom,
			VolumeMounts: []corev1.VolumeMount{
				{Name: mediaVolumeName, MountPath: mediaMountPath},
			},
		}
		if override.Resources != nil {
			container.Resources = *override.Resources
		}
		dep.Spec.Template.Spec.Containers = []corev1.Container{container}
		dep.Spec.Template.Spec.Volumes = []corev1.Volume{mediaVolume}

		return ctrl.SetControllerReference(store, dep, r.Scheme)
	})
	if err != nil {
		return 0, err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("consumer deployment reconciled", "consumer", consumerName, "op", string(op))
	}

	var latest appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: name}, &latest); err != nil {
		return 0, err
	}
	return latest.Status.AvailableReplicas, nil
}

// deleteAllConsumerResources removes every consumer Deployment for
// this MageStore. Called when `spec.consumers.enabled` transitions
// true→false, or when `spec.consumers` is cleared entirely. Iterates
// the known list — stale consumers from a prior operator release
// whose names no longer appear in `magento.Consumers` wash out via
// the orphan-sweep in a future migration.
func (r *MageStoreReconciler) deleteAllConsumerResources(ctx context.Context, store *magev1alpha1.MageStore) error {
	log := ctrl.LoggerFrom(ctx)
	for _, consumerName := range magento.Consumers {
		name := consumerResourceName(store, consumerName)
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
		if err := r.Delete(ctx, dep); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete consumer Deployment %s: %w", name, err)
		}
		log.Info("orphan consumer resource deleted", "consumer", consumerName)
	}
	return nil
}

// unknownConsumerOverrides returns the sorted list of override keys
// that don't match a known consumer name. Sort-stable so condition
// messages don't churn across reconciles.
func unknownConsumerOverrides(overrides map[string]magev1alpha1.ConsumerOverride) []string {
	if len(overrides) == 0 {
		return nil
	}
	var out []string
	for k := range overrides {
		if !magento.IsKnownConsumer(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// consumerResourceName returns the Deployment name for one consumer.
// Sanitises the Magento consumer name (which may contain dots,
// underscores, mixed case) into a DNS-1123-compatible suffix.
func consumerResourceName(store *magev1alpha1.MageStore, consumerName string) string {
	return store.Name + consumerResourcePrefix + sanitiseConsumerName(consumerName)
}

// sanitiseConsumerName maps a Magento consumer name to a DNS-1123
// subdomain suffix: lowercase, with `_` and `.` → `-`. The mapping
// is not reversible, but the original name surfaces verbatim in the
// Deployment's `app.kubernetes.io/component` label and in the
// container args so tooling can still filter / log on it.
func sanitiseConsumerName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

func consumerLabels(store *magev1alpha1.MageStore, consumerName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  "consumer",
		"app.kubernetes.io/managed-by": managedByLabel,
		"mage.aknostic.com/consumer":   sanitiseConsumerName(consumerName),
	}
}

func consumerSelector(store *magev1alpha1.MageStore, consumerName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "magento",
		"app.kubernetes.io/instance":  store.Name,
		"app.kubernetes.io/component": "consumer",
		"mage.aknostic.com/consumer":  sanitiseConsumerName(consumerName),
	}
}

// Compile-time check that client.Object is satisfied by the
// Deployment pointer — exercised when building orphan-sweep calls.
var _ client.Object = (*appsv1.Deployment)(nil)

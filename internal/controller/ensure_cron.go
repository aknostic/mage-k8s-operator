package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
	"github.com/aknostic/mage-k8s-operator/internal/magento"
)

// cronResourcePrefix slots between the store name and the cron group:
// `<store>-cron-<group>`. Group names in `magento.CronGroups` are
// already DNS-1123-compatible (no dots, underscores replaced) so no
// sanitiser is needed — a future group with a non-compliant name
// would gain one at that point.
const cronResourcePrefix = "-cron-"

// cronGroupDefault is the baked-in per-group shape. Schedules match
// Magento's upstream crontab.xml defaults (every minute for all
// groups; the Magento scheduler internally backs off long-running
// tasks via the `schedule_generate_every` config). Memory profiles
// reflect the CLAUDE.md architecture notes: `index` gets a generous
// 2Gi limit because reindex is long-running and memory-hungry;
// every other group stays on a cautious 512Mi limit, large enough
// for email dispatch + sitemap generation without attracting OOM
// kills on a moderately-sized catalogue.
type cronGroupDefault struct {
	schedule      string
	memoryRequest string
	memoryLimit   string
}

var cronGroupDefaults = map[string]cronGroupDefault{
	"default":       {schedule: "* * * * *", memoryRequest: "256Mi", memoryLimit: "512Mi"},
	"index":         {schedule: "* * * * *", memoryRequest: "1Gi", memoryLimit: "2Gi"},
	"consumers":     {schedule: "* * * * *", memoryRequest: "256Mi", memoryLimit: "512Mi"},
	"staging":       {schedule: "* * * * *", memoryRequest: "256Mi", memoryLimit: "512Mi"},
	"catalog_event": {schedule: "* * * * *", memoryRequest: "256Mi", memoryLimit: "512Mi"},
}

// ensureCron materialises one CronJob per entry in
// `internal/magento.CronGroups` when `spec.cron.enabled: true`.
// Mirrors `ensureConsumers` in shape (fan-out + override validation
// + orphan sweep) but emits CronJobs rather than Deployments.
//
// Per-group CronJob shape: `concurrencyPolicy: Allow` (per CLAUDE.md
// — a long `index` reindex must not block the next `default` cron),
// `successfulJobsHistoryLimit: 3` / `failedJobsHistoryLimit: 1` (log
// retention for troubleshooting without a Job-object blizzard), job
// template runs `bin/magento cron:run --group=<group>`, envFrom the
// config Secret.
//
// Per-group disable: `spec.cron.overrides[<group>].disabled: true`
// causes that CronJob to be deleted (or never created). Useful for
// stores running an external indexer (disable `index`), for shops
// driving consumers via `spec.consumers` rather than the `consumers`
// cron group, or for temporarily quieting a noisy group.
//
// Gate semantics match ensureConsumers: `NotEnabled` / `AwaitingDependencies`
// / `UnknownCronGroupOverride` / `AllReady`. No `RolloutInProgress`
// state — CronJobs don't have a rollout lifecycle, they're either
// scheduled or not.
func (r *MageStoreReconciler) ensureCron(ctx context.Context, store *magev1alpha1.MageStore, dependenciesReady bool, smtpSecretHash string) (metav1.Condition, error) {
	if store.Spec.Cron == nil || !store.Spec.Cron.Enabled {
		if err := r.deleteAllCronResources(ctx, store); err != nil {
			return metav1.Condition{
				Type:    ConditionCronReady,
				Status:  metav1.ConditionFalse,
				Reason:  "OrphanCleanupFailed",
				Message: err.Error(),
			}, fmt.Errorf("orphan cleanup cron: %w", err)
		}
		return metav1.Condition{
			Type:    ConditionCronReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotEnabled",
			Message: "spec.cron.enabled is not true",
		}, nil
	}

	if !dependenciesReady {
		return metav1.Condition{
			Type:    ConditionCronReady,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingDependencies",
			Message: "holding cron rollout until dependencies are ready",
		}, nil
	}

	unknownOverrides := unknownCronGroupOverrides(store.Spec.Cron.Overrides)

	materialised := 0
	disabledCount := 0
	for _, group := range magento.CronGroups {
		override := store.Spec.Cron.Overrides[group]
		if override.Disabled {
			if err := r.deleteCronJob(ctx, store, group); err != nil {
				return metav1.Condition{
					Type:    ConditionCronReady,
					Status:  metav1.ConditionFalse,
					Reason:  "ApplyFailed",
					Message: err.Error(),
				}, fmt.Errorf("delete cron %s (disabled): %w", group, err)
			}
			disabledCount++
			continue
		}
		if err := r.ensureCronJob(ctx, store, group, override, smtpSecretHash); err != nil {
			return metav1.Condition{
				Type:    ConditionCronReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ApplyFailed",
				Message: err.Error(),
			}, fmt.Errorf("apply cron %s: %w", group, err)
		}
		materialised++
	}

	if len(unknownOverrides) > 0 {
		return metav1.Condition{
			Type:    ConditionCronReady,
			Status:  metav1.ConditionFalse,
			Reason:  "UnknownCronGroupOverride",
			Message: fmt.Sprintf("unknown group names in spec.cron.overrides: %s (see internal/magento/consumers.go for the canonical list)", strings.Join(unknownOverrides, ", ")),
		}, nil
	}

	msg := fmt.Sprintf("%d CronJob(s) scheduled", materialised)
	if disabledCount > 0 {
		msg = fmt.Sprintf("%s, %d disabled by override", msg, disabledCount)
	}
	return metav1.Condition{
		Type:    ConditionCronReady,
		Status:  metav1.ConditionTrue,
		Reason:  "AllReady",
		Message: msg,
	}, nil
}

// ensureCronJob materialises one group's CronJob. Merges the baked-in
// group defaults with any user override.
func (r *MageStoreReconciler) ensureCronJob(ctx context.Context, store *magev1alpha1.MageStore, group string, override magev1alpha1.CronOverride, smtpSecretHash string) error {
	name := cronResourceName(store, group)
	labels := cronLabels(store, group)
	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag)

	defaults := cronGroupDefaults[group]
	schedule := defaults.schedule
	if override.Schedule != "" {
		schedule = override.Schedule
	}

	var resources corev1.ResourceRequirements
	if override.Resources != nil {
		resources = *override.Resources
	} else {
		resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse(defaults.memoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse(defaults.memoryLimit),
			},
		}
	}

	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		cj.Labels = labels
		cj.Spec.Schedule = schedule
		cj.Spec.ConcurrencyPolicy = batchv1.AllowConcurrent
		successful := int32(3)
		failed := int32(1)
		cj.Spec.SuccessfulJobsHistoryLimit = &successful
		cj.Spec.FailedJobsHistoryLimit = &failed

		cj.Spec.JobTemplate.Spec.Template.Labels = labels

		if cj.Spec.JobTemplate.Spec.Template.Annotations == nil {
			cj.Spec.JobTemplate.Spec.Template.Annotations = map[string]string{}
		}
		if smtpSecretHash != "" {
			cj.Spec.JobTemplate.Spec.Template.Annotations[smtpSecretHashAnnotation] = smtpSecretHash
		} else {
			// Prune on SMTP removal — see ensure_workloads.go for rationale.
			delete(cj.Spec.JobTemplate.Spec.Template.Annotations, smtpSecretHashAnnotation)
		}

		cronEnvFrom := []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
			}},
		}
		if smtpName := smtpEnvFromSecretName(store); smtpName != "" {
			cronEnvFrom = append(cronEnvFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: smtpName},
				},
			})
		}
		container := corev1.Container{
			Name:      "cron",
			Image:     image,
			Command:   []string{"bin/magento"},
			Args:      []string{"cron:run", "--group=" + group},
			EnvFrom:   cronEnvFrom,
			Env:       adminFrontnameEnv(store),
			Resources: resources,
		}
		cj.Spec.JobTemplate.Spec.Template.Spec.Containers = []corev1.Container{container}
		cj.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure

		return ctrl.SetControllerReference(store, cj, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("cron reconciled", "group", group, "op", string(op))
	}
	return nil
}

// deleteCronJob removes a single group's CronJob. Called on
// `disabled: true` override or on the whole-subsystem sweep when
// `spec.cron.enabled` goes true→false. IsNotFound tolerated.
func (r *MageStoreReconciler) deleteCronJob(ctx context.Context, store *magev1alpha1.MageStore, group string) error {
	name := cronResourceName(store, group)
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	if err := r.Delete(ctx, cj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete CronJob %s: %w", name, err)
	}
	ctrl.LoggerFrom(ctx).Info("cron resource deleted", "group", group)
	return nil
}

// deleteAllCronResources removes every known-group CronJob for this
// MageStore. Called when `spec.cron` is cleared or `enabled: false`.
func (r *MageStoreReconciler) deleteAllCronResources(ctx context.Context, store *magev1alpha1.MageStore) error {
	for _, group := range magento.CronGroups {
		if err := r.deleteCronJob(ctx, store, group); err != nil {
			return err
		}
	}
	return nil
}

// unknownCronGroupOverrides returns the sorted list of override keys
// that don't match a known cron group. Sort-stable so condition
// messages don't churn across reconciles.
func unknownCronGroupOverrides(overrides map[string]magev1alpha1.CronOverride) []string {
	if len(overrides) == 0 {
		return nil
	}
	var out []string
	for k := range overrides {
		if !magento.IsKnownCronGroup(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// cronResourceName returns the CronJob name for a group: the group
// names in `magento.CronGroups` are DNS-1123-compatible out of the
// box, but the `catalog_event` underscore needs replacing for use in
// a Kubernetes resource name.
func cronResourceName(store *magev1alpha1.MageStore, group string) string {
	return store.Name + cronResourcePrefix + strings.ReplaceAll(group, "_", "-")
}

func cronLabels(store *magev1alpha1.MageStore, group string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  "cron",
		"app.kubernetes.io/managed-by": managedByLabel,
		"mage.aknostic.com/cron-group": strings.ReplaceAll(group, "_", "-"),
	}
}

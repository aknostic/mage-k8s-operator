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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// ConditionMediaSeeded is the sub-readiness condition ensureMediaSeed
// owns. True = seed complete (S3 Job succeeded or workloads-ready
// implies initContainer success for volume modes); False = seed Job
// failed (S3 mode only); Unknown = seeding in progress.
//
// The condition is sticky-once-True: once set to True it is never
// re-evaluated or re-run, even if spec changes. This avoids
// re-seeding S3 buckets that already have production data. To force
// a re-seed, delete the <store>-media-seed Job manually and patch
// the condition off the status.
//
// Condition constants live in their respective ensure_*.go files
// rather than all in magestore_controller.go so unrelated subsystems
// don't need to import each other's vocabulary.
const ConditionMediaSeeded = "MediaSeeded"

// mediaSeedJobSuffix is the suffix for the one-shot S3 media-seed Job.
const mediaSeedJobSuffix = "-media-seed"

// mediaSeedScript is the shell command the seed Job container runs.
// Verbatim from ADR 011 §"Job spec":
//
//   - cp -rn copies the image-baked sample data from /app/pub/media-baked
//     to the Job-local emptyDir at /app/pub/media. The trailing dot on the
//     source path copies directory contents, not the directory itself.
//   - bin/magento remote-storage:sync uploads from the local emptyDir up
//     to S3 via Magento's built-in remote-storage driver.
//
// set -euo pipefail (with pipefail) is required by ADR 011; do not
// simplify to set -eu.
const mediaSeedScript = `set -euo pipefail
cp -rn /app/pub/media-baked/. /app/pub/media/
bin/magento remote-storage:sync`

// ensureMediaSeed is the single producer of the MediaSeeded condition.
//
// State machine (per ADR 011 §"Reconcile graph and gating", with the
// v0.6.3 #2 row 1.5 added between row 1 and row 6 to gate S3 seeding
// on the dependent `<store>-config` Secret existing):
//
//	Row 1:   seedFromImage=false (any mode) — no-op; return zero
//	         metav1.Condition (Type=="", caller omits it).
//	Row 1.5: S3 + seedFromImage=true, secretsReady=false — no-op; return
//	         Unknown/AwaitingSecrets. The seed Job's container envFrom
//	         resolves `<store>-config`, which ensureSecrets only writes
//	         once DB+Search+Cache+Queue+Bootstrap conditions are True.
//	         Without this gate, a Job created here loops on
//	         CreateContainerConfigError until the recovery helper deletes
//	         and recreates it (~3-5 min wall time on every cold-start).
//	         Volume modes (emptyDir / PVC) skip this gate — their
//	         seeding runs as a workload-pod initContainer that's gated
//	         separately on workloadsReady (and that gate composes
//	         transitively with the SecretsReady gate ensureWorkloads
//	         already enforces via dependenciesReady).
//	Row 2:   emptyDir + seedFromImage=true, WorkloadsReady=false — no-op
//	         (initContainer in C2.3 handles it); return Unknown/Seeding.
//	Row 3:   emptyDir + seedFromImage=true, WorkloadsReady=true — no-op;
//	         return True/SeedComplete (initContainer success implied by
//	         pod-Ready).
//	Row 4:   PVC + seedFromImage=true, WorkloadsReady=false — same as row 2.
//	Row 5:   PVC + seedFromImage=true, WorkloadsReady=true — same as row 3.
//	Row 6:   S3 + seedFromImage=true, no Job yet — create <store>-media-seed
//	         Job; return Unknown/Seeding.
//	Row 7:   S3 + seedFromImage=true, Job Active — return Unknown/Seeding.
//	Row 8:   S3 + seedFromImage=true, Job Succeeded — return True/SeedComplete.
//	Row 9:   S3 + seedFromImage=true, Job Failed (backoff exhausted) — return
//	         False/SeedFailed with message from Job's Failed condition.
//
// # Sticky-once-True
//
// At the top of this function we check whether the existing condition on
// store.Status.Conditions is already True. If so we return it unchanged
// and skip all further evaluation. This prevents re-seeding an S3 bucket
// that already holds production data. Distinct from SMTPReady, which can
// flip on hash changes — media seeding is a one-time bootstrap operation.
//
// # TTL choice
//
// The seed Job carries ttlSecondsAfterFinished=600 (option a from the
// plan open question), matching the SMTP probe Job pattern. Failed jobs
// auto-clean after 10 min; operators needing a longer inspection window
// can disable the TTL by patching the Job directly before the TTL fires.
//
// # Placement in reconcile graph
//
// ensureMediaSeed runs BETWEEN ensureBootstrap and ensureWorkloads.
// For S3 mode, C2.4 will gate ensureWorkloads on MediaSeeded=True;
// that gate breaks if ensureMediaSeed were moved after ensureWorkloads.
// Do not reorder.
func (r *MageStoreReconciler) ensureMediaSeed(
	ctx context.Context,
	store *magev1alpha1.MageStore,
	mediaEndpoint MediaEndpoint,
	workloadsReady bool,
	secretsReady bool,
) (metav1.Condition, error) {
	// Sticky-once-True: if MediaSeeded is already True, return it unchanged.
	// This is the primary guard against re-seeding a bucket that already
	// holds production data.
	existing := meta.FindStatusCondition(store.Status.Conditions, ConditionMediaSeeded)
	if existing != nil && existing.Status == metav1.ConditionTrue {
		return *existing, nil
	}

	// Row 1: seedFromImage=false for any mode — no-op; return zero condition.
	if !mediaEndpoint.SeedFromImage {
		return metav1.Condition{}, nil
	}

	// S3 mode: rows 6–9, gated by row 1.5 (v0.6.3 #2). EnvVars is non-nil
	// only for S3 mode.
	if mediaEndpoint.EnvVars != nil {
		// Row 1.5: hold off on creating the seed Job until SecretsReady.
		// The Job's pod envFrom-mounts `<store>-config`; if we create the
		// Job before that Secret exists, kubelet loops on
		// CreateContainerConfigError until backoffLimit kicks in and the
		// terminal-Failed-Job recovery helper deletes-and-recreates it.
		// Recovery is correct but expensive (~3-5 min); the structural
		// gate is free.
		if !secretsReady {
			return metav1.Condition{
				Type:    ConditionMediaSeeded,
				Status:  metav1.ConditionUnknown,
				Reason:  "AwaitingSecrets",
				Message: "Waiting for SecretsReady=True before creating S3 media-seed Job",
			}, nil
		}
		return r.reconcileMediaSeedJob(ctx, store)
	}

	// Volume modes (emptyDir or PVC): rows 2–5.
	// The initContainer (C2.3) handles the actual seeding; we only reflect
	// readiness based on whether WorkloadsReady is true (implying initContainer
	// ran successfully in a Ready pod).
	if workloadsReady {
		// Row 3 / 5: workloads are up, initContainer success implied by pod-Ready.
		return metav1.Condition{
			Type:    ConditionMediaSeeded,
			Status:  metav1.ConditionTrue,
			Reason:  "SeedComplete",
			Message: "Media seeded via workload initContainer (WorkloadsReady=True)",
		}, nil
	}
	// Row 2 / 4: workloads not yet ready; initContainer hasn't run or finished.
	return metav1.Condition{
		Type:    ConditionMediaSeeded,
		Status:  metav1.ConditionUnknown,
		Reason:  "Seeding",
		Message: "Waiting for workloads to become ready; media initContainer will seed on pod start",
	}, nil
}

// reconcileMediaSeedJob manages the S3 media-seed Job lifecycle.
//
// Rows 6 (no Job): create Job → Unknown/Seeding.
// Row 7 (Job Active): → Unknown/Seeding.
// Row 8 (Job Succeeded): → True/SeedComplete.
// Row 9 (Job Failed): → False/SeedFailed (after retry budget exhausted)
//
//	or False/RetryingFailedJob (inside backoff window)
//	or False/RecreatingFailedJob (delete-and-recreate).
func (r *MageStoreReconciler) reconcileMediaSeedJob(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, error) {
	jobName := store.Name + mediaSeedJobSuffix

	var existingJob batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: jobName}, &existingJob)
	switch {
	case apierrors.IsNotFound(err):
		// Row 6: no Job yet — create one. Reset attempts on create so
		// a manual `kubectl delete job` outside the recovery loop also
		// gets a clean slate.
		recordJobAttempts(store, jobName, magev1alpha1.JobAttemptStatus{})
		return r.createMediaSeedJob(ctx, store, jobName)
	case err != nil:
		return metav1.Condition{}, fmt.Errorf("get media seed job: %w", err)
	}

	// Run terminal-Failed recovery before deriving the condition.
	// Healthy / in-progress / succeeded Jobs flow through unchanged
	// (Terminal=false), but a Failed Job either gets deleted (caller's
	// NotFound branch recreates next pass), held inside the backoff
	// window, or surfaces PermanentlyFailed when the budget is spent.
	out, err := r.recoverTerminalFailedJob(ctx, &existingJob, store.Status.JobAttempts[jobName])
	if err != nil {
		return metav1.Condition{}, err
	}
	recordJobAttempts(store, jobName, out.Attempts)
	if out.Deleted {
		return metav1.Condition{
			Type:    ConditionMediaSeeded,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRecreatingReason,
			Message: fmt.Sprintf("Media seed Job %q failed; deleted to recreate (attempt %d/%d)", jobName, out.Attempts.Count, len(jobBackoffSchedule)),
		}, nil
	}
	if out.Permanent {
		return metav1.Condition{
			Type:    ConditionMediaSeeded,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryReason,
			Message: fmt.Sprintf("Media seed Job %q failed %d times; retry budget exhausted — inspect with `kubectl logs job/%s` and delete the Job to retry", jobName, out.Attempts.Count, jobName),
		}, nil
	}
	if out.Terminal {
		return metav1.Condition{
			Type:    ConditionMediaSeeded,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRetryReason,
			Message: fmt.Sprintf("Media seed Job %q failed (attempt %d); will recreate at %s", jobName, out.Attempts.Count, out.Attempts.NextAttemptAt.UTC().Format(time.RFC3339)),
		}, nil
	}

	// Non-terminal — derive condition from Job status.
	return mediaSeedJobCondition(&existingJob), nil
}

// createMediaSeedJob builds and creates the seed Job, then returns Unknown/Seeding.
func (r *MageStoreReconciler) createMediaSeedJob(ctx context.Context, store *magev1alpha1.MageStore, jobName string) (metav1.Condition, error) {
	job := buildMediaSeedJob(store, jobName)
	if err := ctrl.SetControllerReference(store, job, r.Scheme); err != nil {
		return metav1.Condition{}, fmt.Errorf("set media seed job owner ref: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return metav1.Condition{}, fmt.Errorf("create media seed job: %w", err)
	}
	return metav1.Condition{
		Type:    ConditionMediaSeeded,
		Status:  metav1.ConditionUnknown,
		Reason:  "Seeding",
		Message: fmt.Sprintf("Job %q created, waiting for completion", jobName),
	}, nil
}

// mediaSeedJobCondition derives the MediaSeeded condition from the current
// status of an existing seed Job.
func mediaSeedJobCondition(job *batchv1.Job) metav1.Condition {
	jobName := job.Name
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return metav1.Condition{
				Type:    ConditionMediaSeeded,
				Status:  metav1.ConditionTrue,
				Reason:  "SeedComplete",
				Message: fmt.Sprintf("Media seed Job %q succeeded", jobName),
			}
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			msg := c.Message
			if msg == "" {
				msg = fmt.Sprintf("media seed Job failed; inspect logs via `kubectl logs job/%s`", jobName)
			}
			return metav1.Condition{
				Type:    ConditionMediaSeeded,
				Status:  metav1.ConditionFalse,
				Reason:  "SeedFailed",
				Message: msg,
			}
		}
	}
	// Job exists but has no terminal condition: still active.
	return metav1.Condition{
		Type:    ConditionMediaSeeded,
		Status:  metav1.ConditionUnknown,
		Reason:  "Seeding",
		Message: fmt.Sprintf("Job %q running: active=%d succeeded=%d failed=%d", jobName, job.Status.Active, job.Status.Succeeded, job.Status.Failed),
	}
}

// buildMediaSeedJob constructs the seed Job object per ADR 011 §"Job spec".
// Caller is responsible for SetControllerReference and Create.
//
// The Job's media volume is a NEW emptyDir scoped to the Job pod — NOT
// shared with workload pods. The Job populates this local emptyDir from
// /app/pub/media-baked/ (image-baked sample data) and then syncs UP to
// S3 via bin/magento remote-storage:sync. Workload pods read DOWN from
// S3 via the same module.
//
// ttlSecondsAfterFinished=600 matches the SMTP probe Job pattern
// (option a). Failed jobs auto-clean after 10 min.
func buildMediaSeedJob(store *magev1alpha1.MageStore, name string) *batchv1.Job {
	backoffLimit := int32(2)
	activeDeadline := int64(1800) // 30 min — sample-data sync can take a while
	ttl := int32(600)

	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag)
	labels := map[string]string{
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  "media-seed",
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: store.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &activeDeadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:            "seed",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
							},
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "media",
							MountPath: mediaMountPath,
						}},
						Command: []string{"bash", "-c"},
						Args:    []string{mediaSeedScript},
					}},
					Volumes: []corev1.Volume{{
						Name: "media",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}},
				},
			},
		},
	}
}

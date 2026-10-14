package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// Naming, labels, and annotations specific to the upgrade arc.
const (
	upgradeJobInfix  = "-upgrade-"
	upgradeContainer = "setup-upgrade"

	// Component label value identifying the operator-owned upgrade Job.
	upgradeComponent = "upgrade"

	// from/to image tags baked onto the Job at create time as
	// annotations (NOT labels) — labels enforce DNS-1123 charset which
	// would mangle Magento patch tags like `2.4.8-p1`. The arc's
	// success path reads upgradeToAnnotation back off the Job to advance
	// observedImageTag, so the tag value must roundtrip verbatim.
	upgradeFromAnnotation = "mage.aknostic.com/upgrade-from"
	upgradeToAnnotation   = "mage.aknostic.com/upgrade-to"

	// upgradeSavedReplicasAnnotation snapshots a consumer Deployment's
	// replica count before quiesce so the restore path puts the
	// original value back. Restoring to "1" by default would shrink
	// any user-overridden consumer; the snapshot is the only correct
	// way to preserve user intent across the quiesce window.
	upgradeSavedReplicasAnnotation = "mage.aknostic.com/upgrade-saved-replicas"

	// upgradeQuiesceStartedAnnotation lives on the MageStore itself
	// and records (RFC3339) when the cron-quiesce wait first noticed
	// a cron pod still running. Persisted so the 5-minute timeout
	// survives operator restarts and reconcile-loop bounces; cleared
	// on successful Job creation or arc restore.
	upgradeQuiesceStartedAnnotation = "mage.aknostic.com/upgrade-quiesce-started"

	// maintenanceFlippedAnnotation lives on each workload Service
	// (frontend / admin / api) for the duration of the upgrade arc.
	// It's the coordination channel between ensureUpgrade (the
	// writer that flips selectors at arc entry and clears at
	// finalise) and ensureWorkloadService (the reader that skips
	// its selector reconciliation while the annotation is set, so
	// the flip doesn't get reverted on the next reconcile pass).
	// Without this gate the maintenance fence is decorative — every
	// reconcile would overwrite the flip back to the workload
	// selector before the API server registered the change.
	maintenanceFlippedAnnotation = "mage.aknostic.com/maintenance-flipped"

	// maintenanceFlippedValue is the canonical "true" marker we set
	// on the annotation. Bare-string equality keeps the read-side
	// (ensureWorkloadService skip-gate) trivial.
	maintenanceFlippedValue = "true"

	cronQuiesceTimeout = 5 * time.Minute

	// upgradeRequeueShort is the requeue-after for "in-flight, check
	// back soon" states (Job running, cron pods still terminating).
	// 30s matches the reconciler's transient-failure requeue.
	upgradeRequeueShort = 30 * time.Second
)

// upgradeScript is the verbatim shell the upgrade Job container runs.
// `setup:upgrade` walks every module's Setup/Patch/Schema and Setup/Patch/Data
// and is non-transactional — partial failures leave the DB in a half-migrated
// state. `cache:flush` after upgrade clears stale config/layout caches that
// would otherwise serve old metadata against the new schema.
const upgradeScript = `set -eu
echo "== setup:upgrade =="
php -d memory_limit=2G bin/magento setup:upgrade
echo "== cache:flush =="
php -d memory_limit=2G bin/magento cache:flush
echo "== upgrade complete =="
`

// advanceObservedImageTagOnInstall is the controller-level glue between
// ensureBootstrap and ensureUpgrade. The first time StoreInstalled flips
// to True, we stamp the spec's image tag onto observedImageTag so the
// upgrade arc has a baseline to compare against. Without this, a
// freshly-installed store would silently skip the very first
// `spec.image.tag` change because ensureUpgrade's drift check
// (observedImageTag != spec) returns Unknown when the field is empty.
//
// Idempotent: subsequent calls with observedImageTag already populated
// are no-ops, so the snippet is safe to run on every reconcile.
func advanceObservedImageTagOnInstall(store *magev1alpha1.MageStore, bootCond metav1.Condition) {
	if bootCond.Status == metav1.ConditionTrue && store.Status.ObservedImageTag == "" {
		store.Status.ObservedImageTag = store.Spec.Image.Tag
	}
}

// ensureUpgrade drives the `setup:upgrade` arc when `spec.image.tag` drifts
// from `status.observedImageTag`. Semantics in ADR 008; the short version:
//
//   - No baseline (observedImageTag empty) → fresh-install path; no-op
//     here, ensureBootstrap is the gate. The first observedImageTag
//     advance happens in the controller's main loop on bootstrap success.
//   - Existing operator-owned upgrade Job present → its status drives
//     the condition surface (in-flight, complete, failed). The Job's
//     mere presence is the lockstep mechanism: while one exists the
//     operator does NOT enter a new arc and does NOT un-quiesce, even
//     if the admin reverts spec.image.tag.
//   - No Job, no drift → steady state. Restore any lingering quiesce
//     state (covers the "admin deleted a failed Job" unstick path).
//   - No Job, drift detected → enter the arc: suspend cron, quiesce
//     consumers, wait for cron pods, create the upgrade Job.
//
// Returns a (Condition, ctrl.Result, error) triple. Result semantics:
// `ctrl.Result{}` continues the pipeline (steady state or success);
// `Requeue: true` / `RequeueAfter > 0` halts the pipeline (in-flight
// arc — workloads MUST NOT roll forward against an un-migrated schema);
// any non-nil error halts with the error.
//
// Subsession-B caveat: the cron-quiesce-timeout and Job-failure paths
// return ctrl.Result{} per the brief, which means workloads CAN roll
// forward to the new image tag against an un-migrated schema. This is
// production-unsafe and is consistent with v0.2.0's plan: Subsession C
// adds the maintenance Service-selector flip that fences traffic during
// the arc. v0.2.0 is not tag-able until C lands.
func (r *MageStoreReconciler) ensureUpgrade(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, ctrl.Result, error) {
	// 1. No baseline yet (fresh-install path; ensureBootstrap is the
	//    gate). Surface Unknown so Ready rollups don't trip on a
	//    field nobody has written yet.
	if store.Status.ObservedImageTag == "" {
		return metav1.Condition{
			Type:    ConditionUpgradeReady,
			Status:  metav1.ConditionUnknown,
			Reason:  ReasonAwaitingUpgrade,
			Message: "no baseline yet; install path",
		}, ctrl.Result{}, nil
	}

	// 2. Look for an operator-owned upgrade Job for this store. We
	//    list by label rather than Get-by-name because lockstep refusal
	//    requires finding a leftover failed Job whose name encodes a
	//    different (from, to) pair than the current spec.
	job, err := r.findUpgradeJob(ctx, store)
	if err != nil {
		return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("list upgrade jobs: %w", err)
	}

	// 3a. Job present — its status drives everything. We never enter a
	//     fresh arc while a Job exists; admin unsticks failed Jobs by
	//     deleting them.
	if job != nil {
		return r.handleExistingUpgradeJob(ctx, store, job)
	}

	// 3b. No Job, no drift — steady state. Restore any lingering
	//     quiesce state AND maintenance-fence state (covers the
	//     "admin deleted a failed Job" unstick path: failed-Job-state
	//     gone, operator un-quiesces, drops the fence, resumes normal
	//     operation). Symmetric with the JobComplete success path's
	//     stage-1 + stage-2 finalise — without the maintenance
	//     teardown the operator reports UpgradeReady=True while the
	//     storefront still serves 503 from the maintenance pod via
	//     the still-flipped Service selectors.
	if store.Status.ObservedImageTag == store.Spec.Image.Tag {
		if err := r.restoreFromQuiesce(ctx, store); err != nil {
			return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("restore from quiesce: %w", err)
		}
		if err := r.clearQuiesceStarted(ctx, store); err != nil {
			return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("clear quiesce-started annotation: %w", err)
		}
		restored, err := r.restoreServicesFromMaintenance(ctx, store)
		if err != nil {
			return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("restore services from maintenance: %w", err)
		}
		if err := r.deleteMaintenance(ctx, store); err != nil {
			return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("delete maintenance resources: %w", err)
		}
		// Ban Varnish only on the reconcile that actually restored
		// Service selectors — same first-run gate as the JobComplete
		// success path, so subsequent steady-state reconciles don't
		// re-issue the ban.
		if restored > 0 {
			r.banVarnish(ctx, store)
		}
		return metav1.Condition{
			Type:    ConditionUpgradeReady,
			Status:  metav1.ConditionTrue,
			Reason:  "UpToDate",
			Message: fmt.Sprintf("schema consistent with %q", store.Spec.Image.Tag),
		}, ctrl.Result{}, nil
	}

	// 3c. Drift detected, no Job — enter the arc.
	return r.runUpgradeArc(ctx, store)
}

// findUpgradeJob returns the operator-owned upgrade Job for this store,
// if any. Returns (nil, nil) when none exists. List-by-label is required
// for lockstep refusal — a failed Job's name encodes the (from, to) pair
// at the time of creation, which may no longer match current spec.
func (r *MageStoreReconciler) findUpgradeJob(ctx context.Context, store *magev1alpha1.MageStore) (*batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(store.Namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  upgradeComponent,
	}); err != nil {
		return nil, err
	}
	// Ignore Jobs with DeletionTimestamp set — those are pending GC
	// and represent the admin-driven unstick path. Once the API
	// server's GC clears the finalizer the Job is gone; surfacing it
	// as "still in flight" would falsely keep the operator quiesced.
	live := jobs.Items[:0]
	for i := range jobs.Items {
		if jobs.Items[i].DeletionTimestamp != nil {
			continue
		}
		live = append(live, jobs.Items[i])
	}
	switch len(live) {
	case 0:
		return nil, nil
	case 1:
		return &live[0], nil
	default:
		// Multiple Jobs would be a controller bug (we never create a
		// second one while the first exists). Return the most-recently-
		// created so status is at least consistent with the latest arc.
		newest := &live[0]
		for i := 1; i < len(live); i++ {
			if live[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
				newest = &live[i]
			}
		}
		return newest, nil
	}
}

// handleExistingUpgradeJob translates the Job's batchv1 status to the
// MageStore's UpgradeReady condition and decides whether to halt or
// continue the pipeline. On JobComplete the Job's upgradeToLabel value
// becomes the new observedImageTag — so the Job's labels are the
// load-bearing record of which (from, to) pair this arc was for.
func (r *MageStoreReconciler) handleExistingUpgradeJob(ctx context.Context, store *magev1alpha1.MageStore, job *batchv1.Job) (metav1.Condition, ctrl.Result, error) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			// Stage 1 — work that's safe to do as soon as the schema
			// is migrated, regardless of where the workload pods are
			// in their rollout: advance observedImageTag, restore
			// cron + consumer quiesce, clear the quiesce-start
			// annotation. Idempotent across reconciles; running
			// multiple times has no effect once each step has
			// completed.
			target := job.Annotations[upgradeToAnnotation]
			if target != "" {
				store.Status.ObservedImageTag = target
			}
			store.Status.Phase = magev1alpha1.PhaseRunning
			if err := r.restoreFromQuiesce(ctx, store); err != nil {
				return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("restore from quiesce: %w", err)
			}
			if err := r.clearQuiesceStarted(ctx, store); err != nil {
				return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("clear quiesce-started annotation: %w", err)
			}

			// Stale-Complete short-circuit: spec.image.tag has drifted
			// past this Job's recorded target while the Job's TTL
			// window (3600s) is still open. Skip stage 2 — deliberately
			// leave the maintenance fence in place across arcs. Delete
			// the stale Job and requeue; the next reconcile sees no
			// Job + drift and enters a fresh arc via runUpgradeArc,
			// which idempotently re-flips the already-flipped Services
			// and re-creates the already-existing maintenance
			// Deployment. The visitor sees a continuous 503 across
			// back-to-back arcs, which is correct: setup:upgrade is
			// running again.
			if target != "" && target != store.Spec.Image.Tag {
				if err := client.IgnoreNotFound(r.Delete(ctx, job)); err != nil {
					return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("delete stale upgrade job %s: %w", job.Name, err)
				}
				return metav1.Condition{
					Type:    ConditionUpgradeReady,
					Status:  metav1.ConditionFalse,
					Reason:  ReasonUpgradeStarting,
					Message: fmt.Sprintf("Job %q complete (→%s); spec drifted to %s, deleted stale Job to enter fresh arc", job.Name, target, store.Spec.Image.Tag),
				}, ctrl.Result{RequeueAfter: upgradeRequeueShort}, nil
			}

			// Stage 2 — restore Service selectors, delete the
			// maintenance Deployment, surface UpgradeComplete. The
			// stage-2 work is entirely maintenance-fence cleanup, so
			// it runs only when the fence was actually applied (i.e.
			// maintenance was enabled at arc entry). When opted out,
			// there's nothing to gate and UpgradeComplete fires
			// immediately on JobComplete + stage-1 restore.
			//
			// When the fence IS in place: gate the teardown on the
			// new workload pods reaching Available. Until then, the
			// fence stays in place via ensureWorkloadService's
			// skip-gate (which inspects maintenanceFlippedAnnotation
			// on each Service). This is the verification window —
			// admins can `kubectl port-forward` the workload
			// Deployments to validate the upgrade before public
			// traffic resumes.
			//
			// Pipeline must continue (ctrl.Result{}) during the
			// rollout so ensureWorkloads can update the Deployment
			// image and roll workloads forward to the new tag.
			// ensureWorkloadService runs but its skip-gate keeps the
			// Service pointing at the maintenance pod; no premature
			// flip-back.
			if maintenanceEnabled(store) {
				ready, err := r.workloadsReady(ctx, store)
				if err != nil {
					return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("check workloads ready: %w", err)
				}
				if !ready {
					return metav1.Condition{
						Type:    ConditionUpgradeReady,
						Status:  metav1.ConditionFalse,
						Reason:  ReasonWorkloadRolloutPending,
						Message: fmt.Sprintf("Job %q complete; waiting for workload pods to reach Available on the new image", job.Name),
					}, ctrl.Result{}, nil
				}
				restored, err := r.restoreServicesFromMaintenance(ctx, store)
				if err != nil {
					return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("restore services from maintenance: %w", err)
				}
				if err := r.deleteMaintenance(ctx, store); err != nil {
					return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("delete maintenance resources: %w", err)
				}
				// Ban Varnish only on the reconcile that actually restored
				// the Service selectors. Subsequent reconciles see the
				// Job as Complete and re-enter this branch, but with no
				// flipped Services to restore — without this gate every
				// reconcile would re-issue the ban.
				if restored > 0 {
					r.banVarnish(ctx, store)
				}
			}
			return metav1.Condition{
				Type:    ConditionUpgradeReady,
				Status:  metav1.ConditionTrue,
				Reason:  "UpgradeComplete",
				Message: fmt.Sprintf("Job %q succeeded; schema now at %q", job.Name, target),
			}, ctrl.Result{}, nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			// Lockstep stays in place across the recovery loop: cron
			// remains suspended, consumer replicas at zero. The
			// operator never touches quiesce state while a Failed
			// upgrade Job exists, regardless of spec.image.tag revert.
			//
			// Recovery delete-and-recreate sequence (gap #1 from the
			// v0.6.2 brief): observation 1 stamps the backoff window
			// and surfaces RetryingFailedJob; once the window elapses
			// the helper deletes the Job, the next reconcile re-enters
			// runUpgradeArc which materialises a fresh Job. setup:upgrade
			// is idempotent against an already-migrated schema, so a
			// retry against a partial migration is safe.
			out, err := r.recoverTerminalFailedJob(ctx, job, store.Status.JobAttempts[job.Name])
			if err != nil {
				return metav1.Condition{}, ctrl.Result{}, err
			}
			recordJobAttempts(store, job.Name, out.Attempts)
			if out.Deleted {
				store.Status.Phase = magev1alpha1.PhaseUpgrading
				return metav1.Condition{
					Type:    ConditionUpgradeReady,
					Status:  metav1.ConditionFalse,
					Reason:  JobRecoveryRecreatingReason,
					Message: fmt.Sprintf("Upgrade Job %q failed; deleted to recreate (attempt %d/%d)", job.Name, out.Attempts.Count, len(jobBackoffSchedule)),
				}, ctrl.Result{RequeueAfter: upgradeRequeueShort}, nil
			}
			store.Status.Phase = magev1alpha1.PhaseFailed
			if out.Permanent {
				return metav1.Condition{
					Type:    ConditionUpgradeReady,
					Status:  metav1.ConditionFalse,
					Reason:  JobRecoveryReason,
					Message: fmt.Sprintf("Upgrade Job %q failed %d times; retry budget exhausted — inspect with `kubectl logs job/%s` and delete the Job to retry", job.Name, out.Attempts.Count, job.Name),
				}, ctrl.Result{}, nil
			}
			return metav1.Condition{
				Type:    ConditionUpgradeReady,
				Status:  metav1.ConditionFalse,
				Reason:  JobRecoveryRetryReason,
				Message: fmt.Sprintf("Upgrade Job %q failed (attempt %d): %s — will recreate at %s", job.Name, out.Attempts.Count, c.Message, out.Attempts.NextAttemptAt.UTC().Format(time.RFC3339)),
			}, ctrl.Result{}, nil
		}
	}
	// In progress — halt pipeline so workloads don't advance to the new
	// image tag while the schema is still being migrated.
	store.Status.Phase = magev1alpha1.PhaseUpgrading
	return metav1.Condition{
		Type:    ConditionUpgradeReady,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonWorkloadRolloutPending,
		Message: fmt.Sprintf("Job %q running: active=%d succeeded=%d failed=%d", job.Name, job.Status.Active, job.Status.Succeeded, job.Status.Failed),
	}, ctrl.Result{RequeueAfter: upgradeRequeueShort}, nil
}

// runUpgradeArc performs the entry-side choreography: phase=Upgrading,
// stand up the maintenance fence (when enabled), suspend CronJobs,
// scale consumers to zero, wait for any in-flight cron pods to
// terminate, then create the upgrade Job.
//
// Maintenance ordering matters: ensureMaintenance + flipServicesToMaintenance
// happen BEFORE cron/consumer quiesce so storefront traffic is fenced
// off the workload pods before we start tearing down their dependencies.
// If the maintenance pod isn't yet Available, runUpgradeArc returns
// RequeueAfter without entering quiesce — the workload pods keep
// serving until the maintenance pod is ready to take over.
//
// `spec.upgrade.maintenance.enabled: false` opts out: we skip the
// maintenance materialise + flip and proceed straight to quiesce
// (matching B's behaviour). The user has explicitly accepted the
// integrity risk of mid-arc traffic.
func (r *MageStoreReconciler) runUpgradeArc(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, ctrl.Result, error) {
	store.Status.Phase = magev1alpha1.PhaseUpgrading

	if maintenanceEnabled(store) {
		ready, err := r.ensureMaintenance(ctx, store)
		if err != nil {
			return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("ensure maintenance: %w", err)
		}
		if !ready {
			return metav1.Condition{
				Type:    ConditionUpgradeReady,
				Status:  metav1.ConditionFalse,
				Reason:  ReasonUpgradeStarting,
				Message: "waiting for maintenance pod to reach Available before flipping traffic",
			}, ctrl.Result{RequeueAfter: upgradeRequeueShort}, nil
		}
		if err := r.flipServicesToMaintenance(ctx, store); err != nil {
			return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("flip services to maintenance: %w", err)
		}
	}

	if err := r.suspendOwnedCronJobs(ctx, store); err != nil {
		return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("suspend owned cronjobs: %w", err)
	}
	if err := r.quiesceConsumers(ctx, store); err != nil {
		return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("quiesce consumers: %w", err)
	}

	// Wait for any actively-running cron pods to terminate before we
	// run setup:upgrade — concurrent cron writes against a migrating
	// schema produce silent corruption. The wait is bounded; on
	// timeout we surface CronQuiesceTimeout and stop. The brief
	// specifies ctrl.Result{} on timeout so the timeout is
	// administratively-blocked rather than transiently retried.
	cronPodsRunning, err := r.cronPodsStillRunning(ctx, store)
	if err != nil {
		return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("inspect cron pods: %w", err)
	}
	if cronPodsRunning {
		started, err := r.markQuiesceStarted(ctx, store)
		if err != nil {
			return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("mark quiesce-start: %w", err)
		}
		if time.Since(started) > cronQuiesceTimeout {
			return metav1.Condition{
				Type:    ConditionUpgradeReady,
				Status:  metav1.ConditionFalse,
				Reason:  ReasonCronQuiesceTimeout,
				Message: fmt.Sprintf("cron pods still active after %s — admin must kill them and delete any partial upgrade Job", cronQuiesceTimeout),
			}, ctrl.Result{}, nil
		}
		return metav1.Condition{
			Type:    ConditionUpgradeReady,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonUpgradeStarting,
			Message: "waiting for in-flight cron pods to terminate before creating upgrade Job",
		}, ctrl.Result{RequeueAfter: upgradeRequeueShort}, nil
	}

	// Quiesce complete — create the Job.
	if err := r.clearQuiesceStarted(ctx, store); err != nil {
		return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("clear quiesce-started annotation: %w", err)
	}
	jobName := upgradeJobName(store, store.Status.ObservedImageTag, store.Spec.Image.Tag)
	job := r.buildUpgradeJob(store, jobName, store.Status.ObservedImageTag, store.Spec.Image.Tag)
	if err := ctrl.SetControllerReference(store, job, r.Scheme); err != nil {
		return metav1.Condition{}, ctrl.Result{}, fmt.Errorf("set upgrade job owner ref: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		// Conflict with an existing Job (race against a sibling reconcile)
		// is harmless — the next reconcile finds the Job by label list
		// and inspects its status. Bubble other errors.
		if apierrors.IsAlreadyExists(err) {
			return metav1.Condition{
				Type:    ConditionUpgradeReady,
				Status:  metav1.ConditionFalse,
				Reason:  ReasonUpgradeStarting,
				Message: fmt.Sprintf("Job %q already exists; will inspect on next reconcile", jobName),
			}, ctrl.Result{RequeueAfter: upgradeRequeueShort}, nil
		}
		return metav1.Condition{
			Type:    ConditionUpgradeReady,
			Status:  metav1.ConditionFalse,
			Reason:  "JobCreateFailed",
			Message: err.Error(),
		}, ctrl.Result{}, fmt.Errorf("create upgrade job: %w", err)
	}

	return metav1.Condition{
		Type:    ConditionUpgradeReady,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonUpgradeStarting,
		Message: fmt.Sprintf("Job %q created (%s → %s)", jobName, store.Status.ObservedImageTag, store.Spec.Image.Tag),
	}, ctrl.Result{RequeueAfter: upgradeRequeueShort}, nil
}

// suspendOwnedCronJobs flips spec.suspend = true on every CronJob
// ensureCron materialised for this store. Idempotent: already-suspended
// CronJobs are skipped.
func (r *MageStoreReconciler) suspendOwnedCronJobs(ctx context.Context, store *magev1alpha1.MageStore) error {
	var cronJobs batchv1.CronJobList
	if err := r.List(ctx, &cronJobs, client.InNamespace(store.Namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  "cron",
	}); err != nil {
		return err
	}
	for i := range cronJobs.Items {
		cj := &cronJobs.Items[i]
		if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
			continue
		}
		t := true
		cj.Spec.Suspend = &t
		if err := r.Update(ctx, cj); err != nil {
			return fmt.Errorf("suspend cronjob %s: %w", cj.Name, err)
		}
	}
	return nil
}

// quiesceConsumers scales every consumer Deployment to replicas: 0 and
// snapshots the original count into upgradeSavedReplicasAnnotation so
// restore can put the original value back. Idempotent — already-zero
// Deployments with the annotation set are skipped.
func (r *MageStoreReconciler) quiesceConsumers(ctx context.Context, store *magev1alpha1.MageStore) error {
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, client.InNamespace(store.Namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  "consumer",
	}); err != nil {
		return err
	}
	for i := range deps.Items {
		dep := &deps.Items[i]
		alreadyZero := dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0
		_, alreadyAnnotated := dep.Annotations[upgradeSavedReplicasAnnotation]
		if alreadyZero && alreadyAnnotated {
			// Idempotent path — nothing to persist.
			continue
		}
		if !alreadyAnnotated {
			current := int32(0)
			if dep.Spec.Replicas != nil {
				current = *dep.Spec.Replicas
			}
			if dep.Annotations == nil {
				dep.Annotations = map[string]string{}
			}
			dep.Annotations[upgradeSavedReplicasAnnotation] = strconv.Itoa(int(current))
		}
		zero := int32(0)
		dep.Spec.Replicas = &zero
		if err := r.Update(ctx, dep); err != nil {
			return fmt.Errorf("scale consumer %s to zero: %w", dep.Name, err)
		}
	}
	return nil
}

// restoreFromQuiesce un-suspends cron, restores consumer replicas from
// the snapshot annotation, and clears the annotation. Used on Job
// success and on the no-Job-no-drift unstick path.
func (r *MageStoreReconciler) restoreFromQuiesce(ctx context.Context, store *magev1alpha1.MageStore) error {
	var cronJobs batchv1.CronJobList
	if err := r.List(ctx, &cronJobs, client.InNamespace(store.Namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  "cron",
	}); err != nil {
		return err
	}
	for i := range cronJobs.Items {
		cj := &cronJobs.Items[i]
		if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
			continue
		}
		f := false
		cj.Spec.Suspend = &f
		if err := r.Update(ctx, cj); err != nil {
			return fmt.Errorf("unsuspend cronjob %s: %w", cj.Name, err)
		}
	}

	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, client.InNamespace(store.Namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  "consumer",
	}); err != nil {
		return err
	}
	for i := range deps.Items {
		dep := &deps.Items[i]
		savedStr, ok := dep.Annotations[upgradeSavedReplicasAnnotation]
		if !ok {
			continue
		}
		saved, err := strconv.Atoi(savedStr)
		if err != nil {
			return fmt.Errorf("parse %s on %s: %w", upgradeSavedReplicasAnnotation, dep.Name, err)
		}
		reps := int32(saved)
		dep.Spec.Replicas = &reps
		delete(dep.Annotations, upgradeSavedReplicasAnnotation)
		if err := r.Update(ctx, dep); err != nil {
			return fmt.Errorf("restore consumer %s replicas: %w", dep.Name, err)
		}
	}
	return nil
}

// cronPodsStillRunning reports whether any cron-owned pod is still
// Pending or Running. List by label and filter on phase; in envtest no
// pods are scheduled so this returns false immediately and the arc
// proceeds to Job creation.
func (r *MageStoreReconciler) cronPodsStillRunning(ctx context.Context, store *magev1alpha1.MageStore) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(store.Namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/component":  "cron",
	}); err != nil {
		return false, err
	}
	for i := range pods.Items {
		switch pods.Items[i].Status.Phase {
		case corev1.PodPending, corev1.PodRunning:
			return true, nil
		}
	}
	return false, nil
}

// markQuiesceStarted records the first-observed quiesce-wait time on
// the MageStore as an RFC3339 annotation so the 5-minute timeout
// survives reconcile restarts. Returns the persisted timestamp; if the
// annotation already exists and parses, returns the prior value.
//
// We Patch metadata directly (not Update) so the in-flight status write
// in the caller's Reconcile remains the authoritative status mutator —
// metadata is a different sub-resource path on the API server, and
// patching it doesn't conflict with the subsequent Status().Update.
func (r *MageStoreReconciler) markQuiesceStarted(ctx context.Context, store *magev1alpha1.MageStore) (time.Time, error) {
	if existing, ok := store.Annotations[upgradeQuiesceStartedAnnotation]; ok {
		if t, err := time.Parse(time.RFC3339, existing); err == nil {
			return t, nil
		}
		// Corrupt value — fall through and overwrite.
	}
	patch := client.MergeFrom(store.DeepCopy())
	if store.Annotations == nil {
		store.Annotations = map[string]string{}
	}
	now := time.Now().UTC()
	store.Annotations[upgradeQuiesceStartedAnnotation] = now.Format(time.RFC3339)
	if err := r.Patch(ctx, store, patch); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

// clearQuiesceStarted removes the quiesce-started annotation from the
// MageStore. No-op when the annotation is absent.
func (r *MageStoreReconciler) clearQuiesceStarted(ctx context.Context, store *magev1alpha1.MageStore) error {
	if _, ok := store.Annotations[upgradeQuiesceStartedAnnotation]; !ok {
		return nil
	}
	patch := client.MergeFrom(store.DeepCopy())
	delete(store.Annotations, upgradeQuiesceStartedAnnotation)
	return r.Patch(ctx, store, patch)
}

// upgradeJobName composes the upgrade Job's resource name. The
// human-readable from→to suffix lets `kubectl get jobs` tell admins
// which arc the Job belongs to; uniqueness is from the operator's
// invariant of one in-flight upgrade per store.
//
// Tag values are sanitised to DNS-1123-subdomain charset because Job
// names must conform; in practice Magento tags (2.4.7, 2.4.7-p1, etc.)
// already conform and the sanitiser is a defensive no-op.
func upgradeJobName(store *magev1alpha1.MageStore, fromTag, toTag string) string {
	return store.Name + upgradeJobInfix + sanitiseTag(fromTag) + "-" + sanitiseTag(toTag)
}

// sanitiseTag maps an image tag to a DNS-1123-label-safe substring for
// use in Kubernetes resource names: lowercase, with any character
// outside [a-z0-9-] replaced by `-`. Dots are explicitly mapped to `-`
// because Job names propagate to Pod names, which must be DNS labels
// (no dots). The original tag value is preserved verbatim on the Job's
// annotations; this sanitiser only shapes the Job name itself.
func sanitiseTag(tag string) string {
	s := strings.ToLower(tag)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// buildUpgradeJob constructs the Job object. Caller is responsible for
// SetControllerReference and Create. Mirrors ensure_bootstrap.go's Job
// shape: same envFrom Secret refs, same backoff/TTL semantics, same
// shell-driver command. The image is `spec.image.repository:spec.image.tag`
// (the *target* tag — the Job runs the new image's `bin/magento
// setup:upgrade` against the live DB).
func (r *MageStoreReconciler) buildUpgradeJob(store *magev1alpha1.MageStore, name, fromTag, toTag string) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  upgradeComponent,
		"app.kubernetes.io/managed-by": managedByLabel,
	}
	annotations := map[string]string{
		upgradeFromAnnotation: fromTag,
		upgradeToAnnotation:   toTag,
	}
	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, toTag)

	backoffLimit := int32(1)
	ttl := int32(3600)

	upgradeEnvFrom := []corev1.EnvFromSource{
		{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
		}},
	}
	if smtpName := smtpEnvFromSecretName(store); smtpName != "" {
		upgradeEnvFrom = append(upgradeEnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: smtpName},
			},
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   store.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:            upgradeContainer,
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						WorkingDir:      "/app",
						Command:         []string{"/bin/sh", "-c"},
						Args:            []string{upgradeScript},
						EnvFrom:         upgradeEnvFrom,
					}},
				},
			},
		},
	}
}

// workloadsReady reports whether every declared workload Deployment
// (frontend / admin / api) has reached its desired Available replica
// count. Used by the success path to gate the second-stage finalise
// (selector restore + maintenance teardown) on the new image actually
// being live, not just the Job being done.
//
// A Deployment that doesn't yet exist (NotFound) reports false rather
// than erroring — fresh-install + immediate-upgrade arcs can race here
// and the polite answer is "not ready, come back."
func (r *MageStoreReconciler) workloadsReady(ctx context.Context, store *magev1alpha1.MageStore) (bool, error) {
	for _, kind := range allWorkloadKinds {
		spec, declared := declaredWorkload(store, kind)
		if !declared {
			continue
		}
		var dep appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: store.Namespace,
			Name:      workloadName(store, kind),
		}, &dep); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("get workload deployment %s: %w", kind, err)
		}
		// spec.Replicas comes from the user's `spec.workloads.<kind>.replicas`.
		// HPA-managed deployments may be running more replicas than the
		// declared minimum; we only need the declared count to be Available
		// to call the rollout done.
		desired := max(spec.Replicas, 1)
		if dep.Status.AvailableReplicas < desired {
			return false, nil
		}
	}
	return true, nil
}

// flipServicesToMaintenance patches the frontend / admin / api Service
// selectors to point at the maintenance pod. Annotates each Service
// with `maintenanceFlippedAnnotation: "true"` so ensureWorkloadService
// recognises the in-flight state and skips its selector reconciliation
// — without that coordination, every reconcile during the arc would
// overwrite the flip back to the workload selector.
//
// Idempotent: already-flipped Services are skipped. Services that
// don't exist yet (the operator hasn't materialised them, e.g. fresh
// install before workloads roll) are also skipped silently.
func (r *MageStoreReconciler) flipServicesToMaintenance(ctx context.Context, store *magev1alpha1.MageStore) error {
	target := maintenanceSelector(store)
	for _, kind := range allWorkloadKinds {
		if _, declared := declaredWorkload(store, kind); !declared {
			continue
		}
		var svc corev1.Service
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: store.Namespace,
			Name:      workloadName(store, kind),
		}, &svc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get workload service %s: %w", kind, err)
		}
		if svc.Annotations[maintenanceFlippedAnnotation] == maintenanceFlippedValue {
			continue
		}
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations[maintenanceFlippedAnnotation] = maintenanceFlippedValue
		svc.Spec.Selector = target
		if err := r.Update(ctx, &svc); err != nil {
			return fmt.Errorf("flip workload service %s to maintenance: %w", kind, err)
		}
	}
	return nil
}

// restoreServicesFromMaintenance is the inverse of flip. Clears the
// annotation and re-emits the canonical workloadSelector — the latter
// is reconstructable from `workloadSelector(store, kind)`, so we don't
// need to snapshot the selector before flipping. After the annotation
// is cleared, ensureWorkloadService resumes its normal selector
// reconciliation on the next pass.
//
// Idempotent: Services without the annotation are skipped (they may
// have been restored on a prior reconcile, or never flipped because
// they were created post-arc-entry).
//
// Returns the number of Services actually restored on this call.
// Stage-2 finalise uses the count as a "first run" gate so the post-
// arc Varnish ban fires once per arc, not on every reconcile that
// finds the (long-lived) completed upgrade Job.
func (r *MageStoreReconciler) restoreServicesFromMaintenance(ctx context.Context, store *magev1alpha1.MageStore) (int, error) {
	restored := 0
	for _, kind := range allWorkloadKinds {
		if _, declared := declaredWorkload(store, kind); !declared {
			continue
		}
		var svc corev1.Service
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: store.Namespace,
			Name:      workloadName(store, kind),
		}, &svc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return restored, fmt.Errorf("get workload service %s: %w", kind, err)
		}
		if svc.Annotations[maintenanceFlippedAnnotation] != maintenanceFlippedValue {
			continue
		}
		delete(svc.Annotations, maintenanceFlippedAnnotation)
		svc.Spec.Selector = workloadSelector(store, kind)
		if err := r.Update(ctx, &svc); err != nil {
			return restored, fmt.Errorf("restore workload service %s from maintenance: %w", kind, err)
		}
		restored++
	}
	return restored, nil
}

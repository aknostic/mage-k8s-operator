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
	"math/rand"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// jobBackoffSchedule is the exponential wait between successive retry
// attempts for an operator-owned one-shot Job. Index N (zero-based)
// gives the wait between observation N+1 and the next delete-and-
// recreate. After the schedule's last entry the operator surfaces
// PermanentlyFailed and stops touching the Job — the corpse stays
// for `kubectl logs` inspection.
//
// Values from the v0.6.2 real-infra gap list (gap #1).
var jobBackoffSchedule = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
}

// jobBackoffJitter is the fractional ± window applied to each backoff
// entry. ±25 % means a nominal 30 s wait can resolve to anywhere in
// [22.5 s, 37.5 s]. Jitter is stamped onto NextAttemptAt at observation
// time so the value is stable across reconciles.
const jobBackoffJitter = 0.25

// JobRecoveryReason is the Reason callers compose into their step-
// specific Failed condition (ConditionStoreInstalled,
// ConditionMediaSeeded, ...) when the helper has exhausted the
// retry budget.
const JobRecoveryReason = "PermanentlyFailed"

// JobRecoveryRetryReason is the Reason callers use when a Job is
// terminal-Failed but still inside its backoff window. The condition
// is False (the step has not made progress this reconcile) but the
// reason distinguishes the transient retry-soon state from permanent
// failure.
const JobRecoveryRetryReason = "RetryingFailedJob"

// JobRecoveryRecreatingReason is the Reason callers use when the
// helper just deleted a terminal-Failed Job. The condition flips to
// Unknown / False with this reason; the next reconcile recreates the
// Job from the existing NotFound branch in each ensure-* function.
const JobRecoveryRecreatingReason = "RecreatingFailedJob"

// JobRecoveryOutcome carries the helper's decision back to the caller.
type JobRecoveryOutcome struct {
	// Terminal is true when the Job has a JobFailed=True condition. On
	// false the Job is healthy / in-progress / succeeded; the helper
	// is a no-op except for resetting Attempts on success.
	Terminal bool

	// Deleted is true when the helper deleted the Job. Caller should
	// surface a transient "recreating" Failed/Unknown condition. The
	// existing NotFound branch in the ensure-* function recreates the
	// Job on the next reconcile pass.
	Deleted bool

	// Permanent is true when the retry budget is exhausted. Caller
	// should surface a stable Failed condition with Reason
	// PermanentlyFailed; helper does NOT delete the Job.
	Permanent bool

	// Attempts is the (possibly mutated) JobAttemptStatus the caller
	// must write back to store.Status.JobAttempts[jobName] before
	// Status().Update(). Always valid even on no-op paths so callers
	// can persist unconditionally.
	Attempts magev1alpha1.JobAttemptStatus
}

// recoverTerminalFailedJob inspects job's status and decides what to
// do about a terminal-Failed Job:
//
//   - Healthy or in-progress: no-op; returns Terminal=false.
//   - Succeeded: resets Attempts to its zero value (caller persists
//     so the next failure starts a fresh budget).
//   - Terminal-Failed, first observation: increments attempts.Count,
//     stamps LastFailedAt and NextAttemptAt with jitter.
//   - Terminal-Failed, inside backoff window: leaves the Job alone;
//     caller propagates the wakeup via nextJobAttemptRequeue.
//   - Terminal-Failed, past backoff window: deletes the Job and
//     clears NextAttemptAt so the next observation re-stamps.
//   - Terminal-Failed, retry budget exhausted: surfaces Permanent
//     and leaves the Job in place.
//
// recoverTerminalFailedJob is a method so it can use r.now() and
// r.randFloat() — the test seams that let unit tests drive the
// backoff window deterministically.
func (r *MageStoreReconciler) recoverTerminalFailedJob(
	ctx context.Context,
	job *batchv1.Job,
	attempts magev1alpha1.JobAttemptStatus,
) (JobRecoveryOutcome, error) {
	out := JobRecoveryOutcome{Attempts: attempts}

	switch {
	case isJobSucceeded(job):
		out.Attempts = magev1alpha1.JobAttemptStatus{}
		return out, nil
	case !isJobTerminalFailed(job):
		// In-progress or never-run; leave Attempts untouched.
		return out, nil
	}

	out.Terminal = true

	// First observation of this terminal-Failed Job: stamp the
	// timeline. LastFailedAt being nil is the operator's "I have
	// not yet counted this failure" marker; subsequent reconciles
	// see the stamped value and skip the increment.
	if out.Attempts.LastFailedAt == nil {
		out.Attempts.Count++
		now := r.now()
		out.Attempts.LastFailedAt = &metav1.Time{Time: now}
		out.Attempts.NextAttemptAt = &metav1.Time{Time: now.Add(r.backoffWithJitter(out.Attempts.Count))}
	}

	// Past the cap: surface PermanentlyFailed. The corpse stays so
	// operators can `kubectl logs` after the fact.
	if out.Attempts.Count > len(jobBackoffSchedule) {
		out.Permanent = true
		return out, nil
	}

	// Inside the backoff window: caller's central requeue picks the
	// soonest NextAttemptAt across all owned Jobs.
	if out.Attempts.NextAttemptAt != nil && r.now().Before(out.Attempts.NextAttemptAt.Time) {
		return out, nil
	}

	// Window elapsed: delete the Job. Background propagation matches
	// the existing ensure-* delete sites (smtp probe, store-urls).
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return out, fmt.Errorf("delete terminal-failed job %q: %w", job.Name, err)
	}
	out.Deleted = true
	// Clear timestamps so the next reconcile (which sees a freshly-
	// recreated Job) treats the next terminal-Failed observation as a
	// new failure and increments Count again. The prior failure's
	// visibility now lives only in the (still-incremented) Count
	// field — operators reading status see "we've burned 3 attempts
	// already" rather than a misleading-old LastFailedAt.
	out.Attempts.LastFailedAt = nil
	out.Attempts.NextAttemptAt = nil
	return out, nil
}

// backoffWithJitter looks up the nominal duration for the given 1-based
// attempt number, scales it by r.JobBackoffScale (when set) and applies
// ±jobBackoffJitter via r.randFloat().
// Attempts past the schedule's length cap to the last entry — the
// caller's Permanent check (against len(schedule)) gates whether the
// helper is even invoked at that point.
func (r *MageStoreReconciler) backoffWithJitter(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(jobBackoffSchedule) {
		attempt = len(jobBackoffSchedule)
	}
	nominal := jobBackoffSchedule[attempt-1]
	if r.JobBackoffScale > 0 {
		nominal = time.Duration(float64(nominal) * r.JobBackoffScale)
	}
	// r.randFloat is in [0, 1); map to [-jitter, +jitter].
	delta := (r.randFloat()*2 - 1) * jobBackoffJitter
	return nominal + time.Duration(float64(nominal)*delta)
}

// nextJobAttemptRequeue scans store.Status.JobAttempts for the soonest
// pending NextAttemptAt and returns the duration until that instant.
// Returns 0 (no requeue) when no Job is in-window. Called by Reconcile
// after every ensure-* step has run; produces the controller's wakeup
// for terminal-Failed Jobs that emit no further events of their own.
func (r *MageStoreReconciler) nextJobAttemptRequeue(store *magev1alpha1.MageStore) time.Duration {
	var soonest time.Time
	for _, attempts := range store.Status.JobAttempts {
		if attempts.NextAttemptAt == nil {
			continue
		}
		if soonest.IsZero() || attempts.NextAttemptAt.Time.Before(soonest) {
			soonest = attempts.NextAttemptAt.Time
		}
	}
	if soonest.IsZero() {
		return 0
	}
	d := soonest.Sub(r.now())
	if d <= 0 {
		// Already due; ask for a quick requeue so we run the helper's
		// delete branch promptly rather than the controller's default
		// resync interval.
		return time.Second
	}
	return d
}

// recordJobAttempts writes the (possibly updated) JobAttemptStatus
// back into store.Status.JobAttempts[jobName], creating the map if
// nil. Helper is a one-liner that callers invoke after every
// recoverTerminalFailedJob call so the persistence is uniform across
// ensure-* sites.
func recordJobAttempts(store *magev1alpha1.MageStore, jobName string, attempts magev1alpha1.JobAttemptStatus) {
	if store.Status.JobAttempts == nil {
		store.Status.JobAttempts = map[string]magev1alpha1.JobAttemptStatus{}
	}
	store.Status.JobAttempts[jobName] = attempts
}

// isJobTerminalFailed reports whether the Job carries a
// JobFailed=True condition (BackoffLimitExceeded, DeadlineExceeded,
// or any other Failed reason set by kube-controller-manager).
func isJobTerminalFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isJobSucceeded reports whether the Job carries a JobComplete=True
// condition. Mirrors the inline checks already in each ensure-*
// function.
func isJobSucceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// now returns the current wall-clock time. Production: time.Now();
// tests inject a stable value via r.Now.
func (r *MageStoreReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// randFloat returns a uniform random number in [0, 1). Production:
// math/rand.Float64; tests inject deterministic values via
// r.RandFloat (typically returning 0.5 for the "no jitter" path or
// 0.0 / 1.0 for the bounds).
func (r *MageStoreReconciler) randFloat() float64 {
	if r.RandFloat != nil {
		return r.RandFloat()
	}
	return rand.Float64()
}

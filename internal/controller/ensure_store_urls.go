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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ConditionURLsApplied is the sub-readiness condition ensureStoreURLs owns.
// True = per-store config:set Job succeeded; False = Job failed; Unknown =
// Job running. The condition is omitted (zero metav1.Condition) when
// BootstrapReady is not True — there is no URL config to apply to an
// uninstalled store.
//
// Condition constants live in their respective ensure_*.go files rather than
// all in magestore_controller.go so unrelated subsystems don't need to import
// each other's vocabulary.
const ConditionURLsApplied = "URLsApplied"

// storeURLsJobSuffix is the name suffix for the one-shot store-URLs Job.
const storeURLsJobSuffix = "-store-urls"

// storeURLsHashAnnotation is the annotation key on the store-URLs Job that
// carries the 16-char hex SHA-256 snippet of the hash inputs (spec.stores +
// TLS flag). A hash mismatch triggers Job recreation; a hash match is sticky.
const storeURLsHashAnnotation = "mage.aknostic.com/urls-hash"

// Magento scope-type literals used in spec.stores[].type. Extracted as
// constants to satisfy goconst (each appears in multiple switch branches).
const (
	storeTypeWebsite   = "website"
	storeTypeStoreView = "store_view"
	storeTypeStore     = "store"
)

// ensureStoreURLs is the single producer of URLsApplied. Per ADR 013 phase 2.
//
// Gates on bootstrapReady. Builds a one-shot Job that runs
// bin/magento config:set per-scope and finishes with cache:flush.
// Idempotent at the DB level. Hash-driven re-run on spec changes.
//
// Sticky-on-same-hash: when the existing Job's annotation matches the
// freshly computed hash AND the Job already succeeded, return the
// existing True condition unchanged. A spec change that flips the hash
// triggers Job recreation.
//
// When bootstrapReady=false the condition is omitted (zero metav1.Condition,
// Type==""). This is distinct from False — it means "not applicable yet".
func (r *MageStoreReconciler) ensureStoreURLs(
	ctx context.Context,
	store *magev1alpha1.MageStore,
	bootstrapReady bool,
) (metav1.Condition, error) {
	// Gate: bootstrap not complete → omit condition entirely.
	if !bootstrapReady {
		return metav1.Condition{}, nil
	}

	hasTLS := store.Spec.Routing != nil && store.Spec.Routing.TLS != nil
	currentHash := hashStoreURLInputs(store.Spec.Stores, hasTLS)

	jobName := store.Name + storeURLsJobSuffix

	// Fetch existing Job (if any).
	var existing batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: jobName}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		// No Job yet — create one. Reset attempts so a manual delete
		// outside the recovery loop also gets a clean slate.
		recordJobAttempts(store, jobName, magev1alpha1.JobAttemptStatus{})
		return r.createStoreURLsJob(ctx, store, jobName, currentHash)

	case err != nil:
		return metav1.Condition{}, fmt.Errorf("get store-urls job: %w", err)
	}

	// Job exists. Compare hashes.
	existingHash := existing.Annotations[storeURLsHashAnnotation]
	if existingHash != currentHash {
		// Hash mismatch: spec changed. Delete old Job and re-create;
		// reset attempts since the new Job represents a different spec.
		if delErr := r.Delete(ctx, &existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return metav1.Condition{}, fmt.Errorf("delete stale store-urls job: %w", delErr)
		}
		recordJobAttempts(store, jobName, magev1alpha1.JobAttemptStatus{})
		return r.createStoreURLsJob(ctx, store, jobName, currentHash)
	}

	// Same hash — first run terminal-Failed recovery.
	out, err := r.recoverTerminalFailedJob(ctx, &existing, store.Status.JobAttempts[jobName])
	if err != nil {
		return metav1.Condition{}, err
	}
	recordJobAttempts(store, jobName, out.Attempts)
	if out.Deleted {
		return metav1.Condition{
			Type:    ConditionURLsApplied,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRecreatingReason,
			Message: fmt.Sprintf("Store-URLs Job %q failed; deleted to recreate (attempt %d/%d)", jobName, out.Attempts.Count, len(jobBackoffSchedule)),
		}, nil
	}
	if out.Permanent {
		return metav1.Condition{
			Type:    ConditionURLsApplied,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryReason,
			Message: fmt.Sprintf("Store-URLs Job %q failed %d times; retry budget exhausted — inspect with `kubectl logs job/%s`", jobName, out.Attempts.Count, jobName),
		}, nil
	}
	if out.Terminal {
		return metav1.Condition{
			Type:    ConditionURLsApplied,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRetryReason,
			Message: fmt.Sprintf("Store-URLs Job %q failed (attempt %d); will recreate at %s", jobName, out.Attempts.Count, out.Attempts.NextAttemptAt.UTC().Format(time.RFC3339)),
		}, nil
	}

	// Non-terminal — sticky semantics: return condition derived from
	// the Job's status. For in-flight jobs, this is Unknown/Applying;
	// for completed jobs, True/Applied with the per-scope summary.
	return storeURLsJobCondition(&existing, store), nil
}

// createStoreURLsJob builds and creates the store-URLs Job, then returns
// Unknown/Applying.
func (r *MageStoreReconciler) createStoreURLsJob(ctx context.Context, store *magev1alpha1.MageStore, jobName, hash string) (metav1.Condition, error) {
	job := r.buildStoreURLsJob(store, jobName, hash)
	if err := ctrl.SetControllerReference(store, job, r.Scheme); err != nil {
		return metav1.Condition{}, fmt.Errorf("set store-urls job owner ref: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return metav1.Condition{}, fmt.Errorf("create store-urls job: %w", err)
	}
	return metav1.Condition{
		Type:    ConditionURLsApplied,
		Status:  metav1.ConditionUnknown,
		Reason:  "Applying",
		Message: fmt.Sprintf("Job %q created, waiting for completion", jobName),
	}, nil
}

// storeURLsJobCondition derives the URLsApplied condition from an existing Job.
//
// For the True case (Job succeeded and same hash), the message enumerates
// which scope categories were configured (default, websites, store-views,
// skipped store-groups) to give operators a clear audit trail.
func storeURLsJobCondition(job *batchv1.Job, store *magev1alpha1.MageStore) metav1.Condition {
	jobName := job.Name

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			// Enumerate what was applied for the message.
			return metav1.Condition{
				Type:    ConditionURLsApplied,
				Status:  metav1.ConditionTrue,
				Reason:  "Applied",
				Message: buildAppliedMessage(jobName, store),
			}
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			msg := c.Message
			if msg == "" {
				msg = fmt.Sprintf("store-urls Job failed; inspect logs via `kubectl logs job/%s`", jobName)
			}
			return metav1.Condition{
				Type:    ConditionURLsApplied,
				Status:  metav1.ConditionFalse,
				Reason:  "ApplyFailed",
				Message: msg,
			}
		}
	}

	// Job exists but has no terminal condition: still in flight.
	return metav1.Condition{
		Type:    ConditionURLsApplied,
		Status:  metav1.ConditionUnknown,
		Reason:  "Applying",
		Message: fmt.Sprintf("Job %q running: active=%d succeeded=%d failed=%d", jobName, job.Status.Active, job.Status.Succeeded, job.Status.Failed),
	}
}

// buildAppliedMessage constructs the True/Applied condition message that
// enumerates default scope auto-heal, per-website/store-view overrides,
// and skipped store-groups.
func buildAppliedMessage(jobName string, store *magev1alpha1.MageStore) string {
	var websites, storeViews, skipped []string
	for _, s := range store.Spec.Stores {
		switch s.Type {
		case storeTypeWebsite:
			websites = append(websites, s.Code)
		case storeTypeStoreView:
			storeViews = append(storeViews, s.Code)
		case storeTypeStore:
			skipped = append(skipped, s.Code)
		}
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("Job %q succeeded", jobName))
	parts = append(parts, "default scope auto-healed")
	if len(websites) > 0 {
		parts = append(parts, fmt.Sprintf("websites=%v", websites))
	}
	if len(storeViews) > 0 {
		parts = append(parts, fmt.Sprintf("store-views=%v", storeViews))
	}
	if len(skipped) > 0 {
		parts = append(parts, fmt.Sprintf("skipped store-groups=%v", skipped))
	}
	return strings.Join(parts, "; ")
}

// buildStoreURLsJob constructs the store-URLs Job object per ADR 013 §"Job spec".
// Caller is responsible for SetControllerReference and Create.
//
// The Job renders the config:set script at construction time — the operator
// embeds host literals and scope strings directly into the bash args via
// strings.Builder / fmt.Fprintf. There is no eval and no env-var indirection
// at runtime; the script is deterministic for a given hash.
//
// activeDeadlineSeconds=300: DI bootstrap + N config:set calls fit in 5 min
// under normal cluster conditions. backoffLimit=1 gives one retry on a
// transient pod eviction before the operator declares failure.
// ttlSecondsAfterFinished=600 mirrors the SMTP probe Job pattern (option a).
func (r *MageStoreReconciler) buildStoreURLsJob(store *magev1alpha1.MageStore, name, hash string) *batchv1.Job {
	backoffLimit := int32(1)
	activeDeadline := int64(300)
	ttl := int32(600)

	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag)
	labels := map[string]string{
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  "store-urls",
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: store.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				storeURLsHashAnnotation: hash,
			},
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
						Name:            "store-urls",
						Image:           image,
						ImagePullPolicy: workloadImagePullPolicy(store),
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
							},
						}},
						Command: []string{"sh", "-c"},
						Args:    []string{r.buildStoreURLsScript(store)},
					}},
				},
			},
		},
	}
}

// buildStoreURLsScript renders the config:set script at Job-construction time
// by iterating spec.stores. No const template, no eval. Operator embeds host
// literals into the bash directly via strings.Builder / fmt.Fprintf.
//
// Default scope is auto-healed using deriveBootstrapURLs (B7 helper) so the
// default-scope URLs stay consistent with what setup:install set, regardless of
// how many per-store overrides are present.
//
// cache:flush (NOT cache:clean config) per ADR 013 §254-272: config:set writes
// to the config table but Magento's FPC and block caches hold stale values
// until flushed. cache:clean config would only clean the compiled-config
// artefacts; a full flush ensures both the compiled and the FPC layers are
// dropped so the next request picks up the new URLs.
func (r *MageStoreReconciler) buildStoreURLsScript(store *magev1alpha1.MageStore) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("echo \"== applying per-store URL overrides ==\"\n")

	// Default scope auto-heal. Reuse deriveBootstrapURLs from B7 (ensure_bootstrap.go).
	baseURL, secureURL, _, _ := deriveBootstrapURLs(store)
	fmt.Fprintf(&b, "php -d memory_limit=2G bin/magento config:set web/unsecure/base_url %q\n", baseURL)
	fmt.Fprintf(&b, "php -d memory_limit=2G bin/magento config:set web/secure/base_url %q\n", secureURL)
	fmt.Fprintf(&b, "echo \"  default scope → %s\"\n", baseURL)

	// Per-store scope overrides.
	for _, s := range store.Spec.Stores {
		var scope string
		switch s.Type {
		case storeTypeWebsite:
			scope = "websites"
		case storeTypeStoreView:
			scope = "stores"
		case storeTypeStore:
			// Magento has no URL override level for store groups. Skip with an echo.
			fmt.Fprintf(&b, "echo \"  skipped store-group %s (Magento has no URL override level for groups)\"\n", s.Code)
			continue
		default:
			// Defensive — CEL validation accepts only the three types above.
			continue
		}

		url := buildPerStoreURL(s.Host, store)
		for _, key := range []string{
			"web/secure/base_url", "web/unsecure/base_url",
			"web/secure/base_link_url", "web/unsecure/base_link_url",
		} {
			fmt.Fprintf(&b, "php -d memory_limit=2G bin/magento config:set --scope=%s --scope-code=%s %s %q\n",
				scope, s.Code, key, url)
		}
		fmt.Fprintf(&b, "echo \"  %s %s → %s\"\n", labelForType(s.Type), s.Code, url)
	}

	b.WriteString("php -d memory_limit=2G bin/magento cache:flush\n")
	return b.String()
}

// buildPerStoreURL builds https://<host>/ when spec.routing.tls is set,
// http://<host>/ otherwise. Uses the same TLS detection as B7's helper
// (deriveBootstrapURLs).
func buildPerStoreURL(host string, store *magev1alpha1.MageStore) string {
	if store.Spec.Routing != nil && store.Spec.Routing.TLS != nil {
		return "https://" + host + "/"
	}
	return "http://" + host + "/"
}

// labelForType returns the human label used in the per-store echo lines
// of the config:set script.
func labelForType(t string) string {
	switch t {
	case storeTypeWebsite:
		return storeTypeWebsite
	case storeTypeStoreView:
		return "store-view"
	case storeTypeStore:
		return "store-group"
	}
	return t
}

// hashStoreURLInputs is the v0.6.0 hash shape — see ADR 013 §"Hash-driven re-run"
// and §"Future Work". Future ADRs that make TLS issuer choice URL-relevant
// would need to extend this.
//
// Byte format: for each StoreSpec, host\x00code\x00type\x00; then
// "tls=1" or "tls=0". The null-byte separators prevent hash collisions
// between adjacent fields ("ab"+"c" vs "a"+"bc").
func hashStoreURLInputs(stores []magev1alpha1.StoreSpec, hasTLS bool) string {
	h := sha256.New()
	for _, s := range stores {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00", s.Host, s.Code, s.Type)
	}
	if hasTLS {
		h.Write([]byte("tls=1"))
	} else {
		h.Write([]byte("tls=0"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

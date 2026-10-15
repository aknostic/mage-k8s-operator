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
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ConditionSMTPReady is the sub-readiness condition the SMTP probe step
// owns. True = probe Job succeeded; False = configuration error or probe
// failed; Unknown = probe is still running (Reason=Probing).
// Condition constants live in their respective ensure_*.go files rather
// than all in magestore_controller.go so unrelated subsystems don't need
// to import each other's vocabulary.
const ConditionSMTPReady = "SMTPReady"

// smtpSecretSuffix is the name suffix for the operator-owned per-store
// SMTP Secret (inline mode). Pre-existing code in ensure_secrets.go /
// ensure_bootstrap.go references the Secret via inlineSMTPSecretName(), which
// appends the same "-smtp" suffix; reuse the naming function rather than
// duplicating the constant.

// smtpProbeJobSuffix is the suffix for the swaks probe Job that
// ensureSMTP creates to validate SMTP connectivity.
const smtpProbeJobSuffix = "-smtp-probe"

// smtpSecretHashAnnotation is the annotation key on the probe Job that
// carries the 16-char hex SHA-256 snippet of the SMTP Secret data.
// Downstream (C1.3) pod templates copy this annotation for rollout
// triggering; keeping the annotation key in one place avoids skew.
const smtpSecretHashAnnotation = "mage.aknostic.com/smtp-secret-hash"

// smtpProbeScript is the swaks command the probe Job container runs.
// Verbatim from ADR 012 §"Probe Job spec". The script resolves TLS mode
// and optional auth credentials from environment variables supplied via
// envFrom on the Secret, then calls swaks with --quit-after AUTH so it
// bails out as soon as the relay accepts the auth exchange without
// actually delivering a message.
//
// The trailing `$AUTH_ARGS $TLS_ARGS` are intentionally unquoted so the
// shell word-splits them into zero-or-more separate swaks flags.
// Quoting them ("$AUTH_ARGS") would pass an empty string as a single
// argument when no auth/TLS is configured, which swaks misparses.
const smtpProbeScript = `set -eu
AUTH_ARGS=""
if [ -n "${SMTP_USERNAME:-}" ]; then
    SWAKS_AUTH=$(echo "${SMTP_AUTH:-login}" | tr 'a-z' 'A-Z')
    AUTH_ARGS="--auth $SWAKS_AUTH --auth-user $SMTP_USERNAME --auth-password $SMTP_PASSWORD"
fi
TLS_ARGS=""
case "${SMTP_TLS:-starttls}" in
    starttls) TLS_ARGS="--tls" ;;
    tls)      TLS_ARGS="--tls-on-connect" ;;
    off)      TLS_ARGS="" ;;
esac
swaks --to "validation@example.com" --from "$SMTP_FROM" \
  --server "$SMTP_HOST" --port "${SMTP_PORT:-587}" \
  $AUTH_ARGS $TLS_ARGS --quit-after AUTH --silent`

// ensureSMTP is the single producer of the SMTPReady condition.
//
// State machine (9 rows, per ADR 012 §"SMTPReady state machine"):
//
//	Row 1: spec.smtp unset — delete owned smtp Secret + probe Job if present;
//	       return zero metav1.Condition (Type == "", caller omits it).
//	Row 2: inline, name collision — existing <store>-smtp lacks managed-by label
//	       → False / SecretNameCollision.
//	Row 3: inline, passwordSecretRef missing → False / PasswordRefNotFound.
//	Row 4: secretRef, Secret missing → False / SecretNotFound.
//	Row 5: secretRef, keys missing / parity error → False / SecretMissingKeys.
//	Row 6: keys valid, no probe Job yet → CreateOrUpdate smtp Secret (inline);
//	       create probe Job → Unknown / Probing.
//	Row 7: probe Job Active → Unknown / Probing.
//	Row 8: probe Job Succeeded → True / Ready.
//	Row 9: probe Job Failed → False / ProbeFailed.
//
// # TTL choice
//
// The probe Job carries ttlSecondsAfterFinished=600 unconditionally (option a
// from the plan open question). Failed jobs therefore auto-clean after 10 min,
// which means users must act promptly. The alternative (patch TTL only on
// success so failed jobs persist indefinitely) is a valid v0.6.x follow-up if
// real-world ops teams report needing longer inspection windows.
//
// Returns (condition, smtpSecretHash, error). smtpSecretHash is the 16-char
// hex SHA-256 snippet of the SMTP Secret data; "" when spec.smtp is unset.
// Downstream pod templates carry the hash as a
// mage.aknostic.com/smtp-secret-hash annotation for rollout triggering
// (ADR 012 §"Hash-driven rollout").
func (r *MageStoreReconciler) ensureSMTP(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, string, error) {
	smtpName := inlineSMTPSecretName(store)
	probeName := store.Name + smtpProbeJobSuffix

	// Row 1: spec.smtp unset — best-effort cleanup; return zero condition.
	if store.Spec.SMTP == nil {
		// Attempt to delete the owned smtp Secret, ignoring not-found.
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: smtpName}, &sec); err == nil {
			if isOperatorOwned(&sec) {
				_ = r.Delete(ctx, &sec)
			}
		}
		// Attempt to delete the owned probe Job, ignoring not-found.
		var job batchv1.Job
		if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: probeName}, &job); err == nil {
			if isOperatorOwned(&job) {
				_ = r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			}
		}
		return metav1.Condition{}, "", nil
	}

	var secretName string // name of the Secret to mount in the probe Job

	switch {
	case store.Spec.SMTP.Inline != nil:
		// Rows 2, 3, 6: inline mode.
		inline := store.Spec.SMTP.Inline

		// Row 2: check for name collision — an existing Secret without our label.
		var existing corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: smtpName}, &existing)
		if err == nil && !isOperatorOwned(&existing) {
			return metav1.Condition{
				Type:    ConditionSMTPReady,
				Status:  metav1.ConditionFalse,
				Reason:  "SecretNameCollision",
				Message: fmt.Sprintf("Secret %q/%q exists but lacks app.kubernetes.io/managed-by=%q label; rename it or delete it to allow the operator to manage SMTP", store.Namespace, smtpName, managedByLabel),
			}, "", nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return metav1.Condition{}, "", fmt.Errorf("get smtp secret: %w", err)
		}

		// Row 3: passwordSecretRef present but Secret doesn't exist.
		var smtpPassword string
		if inline.PasswordSecretRef != nil {
			var pwSec corev1.Secret
			if getErr := r.Get(ctx, types.NamespacedName{
				Namespace: store.Namespace,
				Name:      inline.PasswordSecretRef.Name,
			}, &pwSec); getErr != nil {
				if apierrors.IsNotFound(getErr) {
					return metav1.Condition{
						Type:    ConditionSMTPReady,
						Status:  metav1.ConditionFalse,
						Reason:  "PasswordRefNotFound",
						Message: fmt.Sprintf("Secret %q/%q referenced by spec.smtp.inline.passwordSecretRef not found", store.Namespace, inline.PasswordSecretRef.Name),
					}, "", nil
				}
				return metav1.Condition{}, "", fmt.Errorf("get smtp password secret: %w", getErr)
			}
			key := inline.PasswordSecretRef.Key
			if key == "" {
				key = "password"
			}
			smtpPassword = string(pwSec.Data[key])
		}

		// Build the inline Secret data.
		data := buildInlineSMTPSecretData(inline, smtpPassword)

		// CreateOrUpdate the operator-owned smtp Secret.
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: smtpName, Namespace: store.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
			sec.Type = corev1.SecretTypeOpaque
			sec.Labels = map[string]string{
				"app.kubernetes.io/managed-by": managedByLabel,
			}
			sec.Data = data
			return ctrl.SetControllerReference(store, sec, r.Scheme)
		}); err != nil {
			return metav1.Condition{}, "", fmt.Errorf("apply smtp secret: %w", err)
		}

		secretName = smtpName

	case store.Spec.SMTP.SecretRef != nil:
		// Rows 4, 5: secretRef mode.
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: store.Spec.SMTP.SecretRef.Name}, &sec); err != nil {
			if apierrors.IsNotFound(err) {
				return metav1.Condition{
					Type:    ConditionSMTPReady,
					Status:  metav1.ConditionFalse,
					Reason:  "SecretNotFound",
					Message: fmt.Sprintf("Secret %q/%q referenced by spec.smtp.secretRef not found", store.Namespace, store.Spec.SMTP.SecretRef.Name),
				}, "", nil
			}
			return metav1.Condition{}, "", fmt.Errorf("get smtp secretRef: %w", err)
		}

		// Row 5: validate required keys.
		if err := validateSMTPSecretRef(sec.Data); err != nil {
			return metav1.Condition{
				Type:    ConditionSMTPReady,
				Status:  metav1.ConditionFalse,
				Reason:  "SecretMissingKeys",
				Message: err.Error(),
			}, "", nil
		}

		secretName = store.Spec.SMTP.SecretRef.Name
	}

	// Rows 6–9: probe Job lifecycle.
	return r.reconcileSMTPProbeJob(ctx, store, secretName)
}

// reconcileSMTPProbeJob manages the probe Job lifecycle. It computes the
// smtp-secret-hash from the current Secret data and compares it to the
// annotation on the existing Job to decide whether to re-run.
//
// Rows 6 (no Job yet) / re-hash mismatch: create probe Job → Unknown/Probing.
// Row 7 (Job Active): → Unknown/Probing.
// Row 8 (Job Succeeded): → True/Ready.
// Row 9 (Job Failed): → False/ProbeFailed.
func (r *MageStoreReconciler) reconcileSMTPProbeJob(ctx context.Context, store *magev1alpha1.MageStore, secretName string) (metav1.Condition, string, error) {
	probeName := store.Name + smtpProbeJobSuffix

	// Fetch the current SMTP Secret data to compute the hash.
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: secretName}, &sec); err != nil {
		return metav1.Condition{}, "", fmt.Errorf("get smtp secret for hash: %w", err)
	}
	currentHash := hashSMTPSecret(sec.Data)

	// Fetch existing probe Job (if any).
	var existingJob batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: probeName}, &existingJob)
	switch {
	case apierrors.IsNotFound(err):
		// Row 6: no probe Job yet — create one. Clear any prior
		// attempts so a manual `kubectl delete job` (outside the
		// recovery loop) gets a clean slate.
		recordJobAttempts(store, probeName, magev1alpha1.JobAttemptStatus{})
		return r.createSMTPProbeJob(ctx, store, secretName, currentHash, probeName)

	case err != nil:
		return metav1.Condition{}, "", fmt.Errorf("get smtp probe job: %w", err)
	}

	// Job exists. Check if the hash has changed — if so, delete and re-create.
	existingHash := existingJob.Annotations[smtpSecretHashAnnotation]
	if existingHash != currentHash {
		// Hash drift: delete the stale Job (background so we don't block)
		// and create a fresh one on this reconcile pass. Reset attempts
		// since the new Job represents a different spec; the old
		// failure budget should not bleed across.
		if delErr := r.Delete(ctx, &existingJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return metav1.Condition{}, "", fmt.Errorf("delete stale smtp probe job: %w", delErr)
		}
		recordJobAttempts(store, probeName, magev1alpha1.JobAttemptStatus{})
		return r.createSMTPProbeJob(ctx, store, secretName, currentHash, probeName)
	}

	// Same hash — first run terminal-Failed recovery, then derive
	// condition from the (possibly post-delete) state.
	out, err := r.recoverTerminalFailedJob(ctx, &existingJob, store.Status.JobAttempts[probeName])
	if err != nil {
		return metav1.Condition{}, currentHash, err
	}
	recordJobAttempts(store, probeName, out.Attempts)
	if out.Deleted {
		return metav1.Condition{
			Type:    ConditionSMTPReady,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRecreatingReason,
			Message: fmt.Sprintf("SMTP probe Job %q failed; deleted to recreate (attempt %d/%d)", probeName, out.Attempts.Count, len(jobBackoffSchedule)),
		}, currentHash, nil
	}
	if out.Permanent {
		return metav1.Condition{
			Type:    ConditionSMTPReady,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryReason,
			Message: fmt.Sprintf("SMTP probe Job %q failed %d times; retry budget exhausted — inspect with `kubectl logs job/%s`", probeName, out.Attempts.Count, probeName),
		}, currentHash, nil
	}
	if out.Terminal {
		return metav1.Condition{
			Type:    ConditionSMTPReady,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRetryReason,
			Message: fmt.Sprintf("SMTP probe Job %q failed (attempt %d); will recreate at %s", probeName, out.Attempts.Count, out.Attempts.NextAttemptAt.UTC().Format(time.RFC3339)),
		}, currentHash, nil
	}

	return smtpProbeJobCondition(&existingJob), currentHash, nil
}

// createSMTPProbeJob builds and creates the probe Job, then returns
// Unknown/Probing.
func (r *MageStoreReconciler) createSMTPProbeJob(ctx context.Context, store *magev1alpha1.MageStore, secretName, hash, probeName string) (metav1.Condition, string, error) {
	job := buildSMTPProbeJob(store, secretName, hash, probeName)
	if err := ctrl.SetControllerReference(store, job, r.Scheme); err != nil {
		return metav1.Condition{}, "", fmt.Errorf("set smtp probe job owner ref: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return metav1.Condition{}, "", fmt.Errorf("create smtp probe job: %w", err)
	}
	return metav1.Condition{
		Type:    ConditionSMTPReady,
		Status:  metav1.ConditionUnknown,
		Reason:  "Probing",
		Message: fmt.Sprintf("Job %q created, waiting for completion", probeName),
	}, hash, nil
}

// smtpProbeJobCondition derives the SMTPReady condition from the current
// status of an existing probe Job.
func smtpProbeJobCondition(job *batchv1.Job) metav1.Condition {
	probeName := job.Name
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return metav1.Condition{
				Type:    ConditionSMTPReady,
				Status:  metav1.ConditionTrue,
				Reason:  "Ready",
				Message: fmt.Sprintf("SMTP probe Job %q succeeded", probeName),
			}
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			msg := c.Message
			if msg == "" {
				msg = fmt.Sprintf("probe Job failed; inspect logs via `kubectl logs job/%s`", probeName)
			}
			return metav1.Condition{
				Type:    ConditionSMTPReady,
				Status:  metav1.ConditionFalse,
				Reason:  "ProbeFailed",
				Message: msg,
			}
		}
	}
	// Job exists but has no terminal condition: still active.
	return metav1.Condition{
		Type:    ConditionSMTPReady,
		Status:  metav1.ConditionUnknown,
		Reason:  "Probing",
		Message: fmt.Sprintf("Job %q running: active=%d succeeded=%d failed=%d", probeName, job.Status.Active, job.Status.Succeeded, job.Status.Failed),
	}
}

// buildSMTPProbeJob constructs the probe Job object. Caller is responsible
// for SetControllerReference and Create.
func buildSMTPProbeJob(store *magev1alpha1.MageStore, secretName, hash, name string) *batchv1.Job {
	backoffLimit := int32(1)
	activeDeadline := int64(60)
	ttl := int32(600)

	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag)
	labels := map[string]string{
		"app.kubernetes.io/managed-by": managedByLabel,
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  "smtp-probe",
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: store.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				smtpSecretHashAnnotation: hash,
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
						Name:            "probe",
						Image:           image,
						ImagePullPolicy: workloadImagePullPolicy(store),
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
							},
						}},
						Command: []string{"sh", "-c"},
						Args:    []string{smtpProbeScript},
					}},
				},
			},
		},
	}
}

// buildInlineSMTPSecretData constructs the map[string][]byte that goes into
// the operator-owned <store>-smtp Secret for inline mode. Only non-empty
// optional keys are included; callers pass an empty smtpPassword when
// passwordSecretRef is absent (and the inline spec has no User set).
func buildInlineSMTPSecretData(inline *magev1alpha1.SMTPInline, smtpPassword string) map[string][]byte {
	port := inline.Port
	if port == 0 {
		port = 587
	}
	auth := inline.Auth
	if auth == "" {
		auth = "login"
	}
	tls := inline.TLS
	if tls == "" {
		tls = "starttls"
	}

	data := map[string][]byte{
		"SMTP_HOST": []byte(inline.Host),
		"SMTP_FROM": []byte(inline.From),
		"SMTP_PORT": []byte(strconv.Itoa(int(port))),
		"SMTP_AUTH": []byte(auth),
		"SMTP_TLS":  []byte(tls),
	}

	if inline.FromName != "" {
		data["SMTP_FROM_NAME"] = []byte(inline.FromName)
	}

	if inline.User != "" {
		data["SMTP_USERNAME"] = []byte(inline.User)
		data["SMTP_PASSWORD"] = []byte(smtpPassword)
	}

	// Per-identity overrides. Only emit keys that are explicitly set in the
	// spec — empty strings would silently produce zero-length env vars that
	// Magento would use as sender addresses.
	if ids := inline.FromIdentities; ids != nil {
		type identEntry struct {
			key   string
			ident *magev1alpha1.SMTPIdentity
		}
		entries := []identEntry{
			{"GENERAL", ids.General},
			{"SALES", ids.Sales},
			{"SUPPORT", ids.Support},
			{"CUSTOM1", ids.Custom1},
			{"CUSTOM2", ids.Custom2},
		}
		for _, e := range entries {
			if e.ident == nil {
				continue
			}
			if e.ident.Email != "" {
				data["SMTP_FROM_"+e.key] = []byte(e.ident.Email)
			}
			if e.ident.Name != "" {
				data["SMTP_FROM_NAME_"+e.key] = []byte(e.ident.Name)
			}
		}
	}

	return data
}

// validateSMTPSecretRef checks that a customer-managed secretRef Secret
// contains the minimum required keys (SMTP_HOST, SMTP_FROM) and that
// SMTP_USERNAME / SMTP_PASSWORD appear as a matched pair (both or neither).
func validateSMTPSecretRef(data map[string][]byte) error {
	_, hasHost := data["SMTP_HOST"]
	_, hasFrom := data["SMTP_FROM"]
	if !hasHost || !hasFrom {
		return fmt.Errorf("secret must contain SMTP_HOST and SMTP_FROM; one or both are missing")
	}
	_, hasUser := data["SMTP_USERNAME"]
	_, hasPass := data["SMTP_PASSWORD"]
	switch {
	case hasUser && !hasPass:
		return fmt.Errorf("SMTP_USERNAME requires SMTP_PASSWORD")
	case hasPass && !hasUser:
		return fmt.Errorf("SMTP_PASSWORD requires SMTP_USERNAME")
	}
	return nil
}

// hashSMTPSecret computes a stable hash of the SMTP Secret's data.
// Used for the smtp-secret-hash annotation on the probe Job and on
// downstream pod templates (C1.3).
func hashSMTPSecret(data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s\x00", k)
		h.Write(data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// isOperatorOwned returns true when the object carries the
// app.kubernetes.io/managed-by label with value managedByLabel. Used to
// guard cleanup of resources that might have been created by a different
// party (SecretNameCollision row).
func isOperatorOwned(obj metav1.Object) bool {
	labels := obj.GetLabels()
	if labels == nil {
		return false
	}
	return labels["app.kubernetes.io/managed-by"] == managedByLabel
}

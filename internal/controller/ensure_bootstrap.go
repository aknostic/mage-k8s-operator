package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// Naming for operator-owned bootstrap resources.
const (
	bootstrapJobSuffix             = "-bootstrap"
	adminSecretSuffix              = "-admin"
	adminSecretPasswordKey         = "password"
	adminSecretBackendFrontnameKey = "backendFrontname"
	adminSecretAdminURLKey         = "adminUrl"
	bootstrapContainer             = "setup-install"

	// adminBackendFrontnameLength is the random URL-safe length the
	// operator generates when AdminSpec.BackendFrontname is unset on a
	// fresh store. Adobe Commerce hardening guide treats "randomize
	// admin URL" as a baseline mitigation; 16 characters of [a-zA-Z0-9]
	// space is ~95 bits of entropy — well past brute-force territory
	// while staying short enough that ops tooling and 1Password notes
	// stay readable. The CRD allows 4-32 chars; 16 is the operator's
	// generated default.
	adminBackendFrontnameLength = 16

	// existingStoreBackendFrontname is the literal value the operator
	// uses for stores that pre-date v0.7.0 — preserves admin bookmarks
	// per ADR 014's upgrade-safety rule.
	existingStoreBackendFrontname = "admin"
)

// adminBackendFrontnameAlphabet is the URL-safe character set the
// operator draws from when generating a random backendFrontname. Excludes
// `_` and `-` (still allowed by the CRD regex but easy to confuse with
// path separators in copy-pastes).
const adminBackendFrontnameAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// bootstrapScriptTemplate is the exact sequence the Job container runs.
// Ported verbatim from the hand-applied
// config/samples/bootstrap-install-job.yaml that shipped while
// ensureBootstrap was still a sample manifest. Three non-obvious steps
// worth calling out:
//
//   - `rm -f /app/app/etc/env.php` — the image bakes a getenv()-driven
//     env.php; setup:install refuses to proceed if env.php exists (it
//     reads the file, sees no error, concludes the store is installed,
//     and bails with "The default website isn't defined" once it queries
//     an empty DB). Removing env.php in THIS container's ephemeral fs
//     makes setup:install take the fresh-install code path. Frontend
//     pods are unaffected — they start from the image and keep env.php.
//   - PDO `CREATE DATABASE IF NOT EXISTS` — mariadb-operator's default
//     provisioning does not create the `magento` database; setup:install
//     wants it to exist. One PDO call is cheaper than a second MariaDB
//     CR field.
//   - trans_email identity rewrite — setup:install seeds every
//     `trans_email/ident_*/email` with `user@example.com`. Every real
//     SMTP gateway (Scaleway TEM, SES, SendGrid) 551-rejects that
//     envelope sender. When SMTP_FROM is set (from the per-store SMTP
//     Secret), we rewrite each identity using SMTP_FROM_<IDENT> with
//     fallback to SMTP_FROM; the display name is read from
//     SMTP_FROM_NAME_<IDENT> with fallback to SMTP_FROM_NAME. When
//     SMTP_FROM is unset, we skip the rewrite entirely — the seeded
//     user@example.com remains, outbound mail fails at transport with
//     a clear 551, and no admin email silently leaks (gap #11, ADR 012).
//
// The four fmt directives are (in order): baseURL, the whole
// `--base-url-secure` line (empty without TLS), useSecure,
// useSecureAdmin — rendered by buildBootstrapScript via
// deriveBootstrapURLs. See ADR 013 for the URL derivation matrix.
const bootstrapScriptTemplate = `set -eu
echo "== removing env.php so setup:install sees a clean install =="
rm -f /app/app/etc/env.php

echo "== ensuring database exists =="
php -r "\$p=new PDO('mysql:host='.getenv('MAGENTO_DATABASE_HOST').';port='.getenv('MAGENTO_DATABASE_PORT'),getenv('MAGENTO_DATABASE_USERNAME'),getenv('MAGENTO_DATABASE_PASSWORD'));\$p->exec('CREATE DATABASE IF NOT EXISTS '.getenv('MAGENTO_DATABASE_NAME'));echo \"db ensured\n\";"

echo "== setup:install =="
# AMQP args are conditional: setup:install declares the queue topology
# on the broker (exchanges + queues + bindings) only when --amqp-host is
# set. Without this, consumer pods crash with "NOT_FOUND - no queue ..."
# because Magento's consumer code does basic_consume without first
# declaring. We only pass the args when the operator emitted RabbitMQ
# env vars (spec.queue.rabbitMq / rabbitMqRef / external).
AMQP_ARGS=""
if [ -n "${MAGENTO_RABBITMQ_HOST:-}" ]; then
    AMQP_ARGS="--amqp-host=${MAGENTO_RABBITMQ_HOST} --amqp-port=${MAGENTO_RABBITMQ_PORT} --amqp-user=${MAGENTO_RABBITMQ_USERNAME} --amqp-password=${MAGENTO_RABBITMQ_PASSWORD} --amqp-virtualhost=${MAGENTO_RABBITMQ_VHOST:-/}"
fi

php -d memory_limit=2G bin/magento setup:install \
    --base-url=%s \
%s    --use-secure=%d \
    --use-secure-admin=%d \
    --backend-frontname="${ADMIN_BACKEND_FRONTNAME}" \
    --db-host="${MAGENTO_DATABASE_HOST}" \
    --db-name="${MAGENTO_DATABASE_NAME}" \
    --db-user="${MAGENTO_DATABASE_USERNAME}" \
    --db-password="${MAGENTO_DATABASE_PASSWORD}" \
    --admin-firstname="${ADMIN_FIRSTNAME}" \
    --admin-lastname="${ADMIN_LASTNAME}" \
    --admin-email="${ADMIN_EMAIL}" \
    --admin-user="${ADMIN_USERNAME}" \
    --admin-password="${ADMIN_PASSWORD}" \
    --language=en_US \
    --currency=USD \
    --timezone=UTC \
    --use-rewrites=1 \
    --search-engine=opensearch \
    --opensearch-host="${MAGENTO_OPENSEARCH_HOST}" \
    --opensearch-port="${MAGENTO_OPENSEARCH_PORT}" \
    ${AMQP_ARGS}

echo "== rewriting trans_email identities to a real sender =="
if [ -n "${SMTP_FROM:-}" ]; then
    for ident in general sales support custom1 custom2; do
        case "$ident" in
            general)
                addr="${SMTP_FROM_GENERAL:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_GENERAL:-${SMTP_FROM_NAME:-}}"
                ;;
            sales)
                addr="${SMTP_FROM_SALES:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_SALES:-${SMTP_FROM_NAME:-}}"
                ;;
            support)
                addr="${SMTP_FROM_SUPPORT:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_SUPPORT:-${SMTP_FROM_NAME:-}}"
                ;;
            custom1)
                addr="${SMTP_FROM_CUSTOM1:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_CUSTOM1:-${SMTP_FROM_NAME:-}}"
                ;;
            custom2)
                addr="${SMTP_FROM_CUSTOM2:-$SMTP_FROM}"
                name="${SMTP_FROM_NAME_CUSTOM2:-${SMTP_FROM_NAME:-}}"
                ;;
        esac
        php -d memory_limit=2G /app/bin/magento config:set \
          "trans_email/ident_${ident}/email" "$addr"
        if [ -n "$name" ]; then
            php -d memory_limit=2G /app/bin/magento config:set \
              "trans_email/ident_${ident}/name" "$name"
            echo "    ident_${ident} → \"${name}\" <${addr}>"
        else
            echo "    ident_${ident} → <${addr}>"
        fi
    done
    php -d memory_limit=2G /app/bin/magento cache:clean config
else
    echo "  SMTP_FROM unset → skipping trans_email rewrite"
    echo "  (mail delivery will fail at transport with 551 errors, by design;"
    echo "   set spec.smtp on the MageStore to configure outbound email)"
fi
echo "== setup:install complete =="
`

// deriveBootstrapURLs computes the four setup:install URL flags from the
// MageStore spec per the URL derivation matrix in ADR 013 §"URL derivation
// matrix at default scope". Rules in brief:
//
//   - No stores → host = localhost:8080 (blank/test install).
//   - TLS set → useSecure=1 / useSecureAdmin=1; secureURL = https://…
//   - Real hostname + TLS → baseURL = https:// (both URLs identical).
//   - Localhost + TLS (pathological) → baseURL = http://, secureURL = https://.
//   - No TLS → both URLs http://, useSecure=0 / useSecureAdmin=0.
func deriveBootstrapURLs(store *magev1alpha1.MageStore) (baseURL, secureURL string, useSecure, useSecureAdmin int) {
	hasStores := len(store.Spec.Stores) > 0
	hasTLS := store.Spec.Routing != nil && store.Spec.Routing.TLS != nil

	var host string
	if hasStores {
		host = store.Spec.Stores[0].Host
	} else {
		host = "localhost:8080"
	}

	if hasTLS {
		useSecure = 1
		useSecureAdmin = 1
		secureURL = "https://" + host + "/"
		if hasStores {
			// Real hostname + TLS: both URLs https://
			baseURL = secureURL
		} else {
			// Localhost + TLS (pathological): base=http, secure=https
			baseURL = "http://" + host + "/"
		}
	} else {
		// No TLS: both URLs http://
		baseURL = "http://" + host + "/"
		secureURL = baseURL
	}
	return
}

// tfaDisableBlock is appended to the bootstrap script when
// spec.bootstrap.admin.disableTFA is true. setup:upgrade (full regen,
// NOT --keep-generated which corrupted /app/generated/code in a
// previous session) re-resolves the DI graph after the module:disable.
const tfaDisableBlock = `echo "== disabling TFA per spec.bootstrap.admin.disableTFA =="
php -d memory_limit=2G /app/bin/magento config:set twofactorauth/general/enable 0
php -d memory_limit=2G /app/bin/magento module:disable Magento_TwoFactorAuth Magento_AdminAdobeImsTwoFactorAuth
php -d memory_limit=2G /app/bin/magento setup:upgrade
`

// buildBootstrapScript renders bootstrapScriptTemplate for the given store
// using the URL derivation from deriveBootstrapURLs (ADR 013) and appends
// the TFA-disable block when spec.bootstrap.admin.disableTFA is true (gap #4).
func buildBootstrapScript(store *magev1alpha1.MageStore) string {
	baseURL, secureURL, useSecure, useSecureAdmin := deriveBootstrapURLs(store)
	// setup:install rejects any --base-url-secure that isn't https://
	// ("you should use only following schemes: 'https'"), even with
	// --use-secure=0. Without TLS, leave the flag out; Magento then
	// uses --base-url for the secure URL too.
	secureArg := ""
	if useSecure == 1 {
		secureArg = fmt.Sprintf("    --base-url-secure=%s \\\n", secureURL)
	}
	script := fmt.Sprintf(bootstrapScriptTemplate, baseURL, secureArg, useSecure, useSecureAdmin)
	if store.Spec.Bootstrap != nil && store.Spec.Bootstrap.Admin.DisableTFA {
		script += tfaDisableBlock
	}
	return script
}

// ensureBootstrap drives the one-shot `setup:install` step for a fresh
// MageStore. Semantics and rationale live in ADR 005; the short version:
//
//   - If `spec.bootstrap` is nil, the operator assumes the store is
//     already installed (common for stores adopting a pre-bootstrapped
//     database) and returns StoreInstalled=Unknown. ensureWorkloads
//     treats that as a pass — the user opts out of the gate by opting
//     out of bootstrap.
//   - Otherwise, the operator creates a `<store>-bootstrap` Job owned
//     by the MageStore. Job completion flips StoreInstalled to True
//     and subsequent reconciles short-circuit before touching the
//     Job — re-running setup:install would be destructive.
//   - If the admin password is operator-generated, the operator writes
//     it once to `<store>-admin` (key `password`) and preserves across
//     reconciles. Rotation is out-of-band (see ADR 005 open questions).
func (r *MageStoreReconciler) ensureBootstrap(ctx context.Context, store *magev1alpha1.MageStore, dependenciesReady bool, smtpSecretHash string) (metav1.Condition, error) {
	if store.Spec.Bootstrap == nil {
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.bootstrap is unset; operator assumes an already-installed store",
		}, nil
	}

	// setup:install is destructive on re-run. Once the condition is True,
	// the operator must never create another Job or look at the existing
	// one — manual Job deletion post-success is a no-op.
	//
	// We read the condition off the live object, which the caller has
	// already mutated via meta.SetStatusCondition for this pass. The
	// source of truth lives there rather than in Job.Status to keep the
	// contract simple: MageStore.status is what gates reconcile, nothing
	// else.
	//
	// This check must come before the dependency gate. Otherwise a
	// dependency outage on an installed store returns
	// AwaitingDependencies, the caller overwrites StoreInstalled=True
	// with it, and once the dependency recovers (and the completed Job
	// has been garbage-collected) a fresh bootstrap Job runs
	// setup:install against the live database.
	for _, c := range store.Status.Conditions {
		if c.Type == ConditionStoreInstalled && c.Status == metav1.ConditionTrue {
			return c, nil
		}
	}

	if !dependenciesReady {
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  "AwaitingDependencies",
			Message: "holding setup:install until database, search, and config Secret are ready",
		}, nil
	}

	// Admin backend-frontname must resolve BEFORE ensureAdminPassword:
	// the latter Creates `<store>-admin` if it does not yet exist,
	// which would mask the upgrade-safety signal (existing-Secret-
	// without-backendFrontname-key → use literal `admin`). Resolving
	// the frontname first lets ensureAdminBackendFrontname see the
	// pristine state of the Secret. ensureAdminPassword then uses
	// CreateOrUpdate against the same Secret and adds the password key
	// idempotently — both functions co-own the `<store>-admin`
	// resource. See ADR 014 §"Admin URL randomization lives on
	// AdminSpec, not SecuritySpec".
	frontname, frontnameRef, err := r.ensureAdminBackendFrontname(ctx, store)
	if err != nil {
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  "AdminBackendFrontnameError",
			Message: err.Error(),
		}, fmt.Errorf("ensure admin backend frontname: %w", err)
	}

	// Admin password: either user-supplied via passwordSecretRef or
	// operator-generated into <store>-admin. Generate-once semantics —
	// the Secret exists after the first successful pass, and subsequent
	// reconciles leave it alone.
	adminPasswordRef, err := r.ensureAdminPassword(ctx, store)
	if err != nil {
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  "AdminPasswordError",
			Message: err.Error(),
		}, fmt.Errorf("ensure admin password secret: %w", err)
	}

	// Log the resolved admin URL once on first reconcile of a fresh
	// store (when BootstrapReady is not yet True). Operators read this
	// from kubectl logs or `<store>-admin` Secret's `adminUrl` key —
	// the log is the loud surface; the Secret is the durable one.
	log := ctrl.LoggerFrom(ctx)
	log.Info("admin URL resolved",
		"store", fmt.Sprintf("%s/%s", store.Namespace, store.Name),
		"backendFrontname", frontname,
		"adminUrl", deriveAdminURL(store, frontname),
	)

	jobName := store.Name + bootstrapJobSuffix

	var existing batchv1.Job
	err = r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: jobName}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		// Reset attempts on (re)create — manual `kubectl delete job`
		// outside the recovery loop also wants a clean slate.
		recordJobAttempts(store, jobName, magev1alpha1.JobAttemptStatus{})
		job := r.buildBootstrapJob(store, jobName, adminPasswordRef, frontnameRef, smtpSecretHash)
		if err := ctrl.SetControllerReference(store, job, r.Scheme); err != nil {
			return metav1.Condition{}, fmt.Errorf("set bootstrap job owner ref: %w", err)
		}
		if err := r.Create(ctx, job); err != nil {
			return metav1.Condition{
				Type:    ConditionStoreInstalled,
				Status:  metav1.ConditionFalse,
				Reason:  "JobCreateFailed",
				Message: err.Error(),
			}, fmt.Errorf("create bootstrap job: %w", err)
		}
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  "BootstrapInProgress",
			Message: fmt.Sprintf("Job %q created", jobName),
		}, nil
	case err != nil:
		return metav1.Condition{}, fmt.Errorf("get bootstrap job: %w", err)
	}

	// Run terminal-Failed recovery before deriving the success path.
	// A Failed bootstrap Job under the old behaviour stuck at
	// BootstrapFailed forever; recovery now deletes-and-recreates
	// across an exponential backoff window. setup:install is
	// idempotent against an already-installed schema (it surfaces
	// "Magento application is already installed" and exits 0), so
	// re-running on a partially-installed DB is safer than the
	// previous freeze. See v0.6.2 punch list (gap #1).
	out, err := r.recoverTerminalFailedJob(ctx, &existing, store.Status.JobAttempts[jobName])
	if err != nil {
		return metav1.Condition{}, err
	}
	recordJobAttempts(store, jobName, out.Attempts)
	if out.Deleted {
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRecreatingReason,
			Message: fmt.Sprintf("Bootstrap Job %q failed; deleted to recreate (attempt %d/%d)", jobName, out.Attempts.Count, len(jobBackoffSchedule)),
		}, nil
	}
	if out.Permanent {
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryReason,
			Message: fmt.Sprintf("Bootstrap Job %q failed %d times; retry budget exhausted — inspect with `kubectl logs job/%s`", jobName, out.Attempts.Count, jobName),
		}, nil
	}
	if out.Terminal {
		return metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionFalse,
			Reason:  JobRecoveryRetryReason,
			Message: fmt.Sprintf("Bootstrap Job %q failed (attempt %d); will recreate at %s", jobName, out.Attempts.Count, out.Attempts.NextAttemptAt.UTC().Format(time.RFC3339)),
		}, nil
	}

	// Non-terminal — read Job status conditions for completion signal.
	// batchv1.Job.Status.Conditions uses JobComplete and JobFailed types.
	for _, c := range existing.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return metav1.Condition{
				Type:    ConditionStoreInstalled,
				Status:  metav1.ConditionTrue,
				Reason:  "BootstrapComplete",
				Message: fmt.Sprintf("Job %q succeeded", jobName),
			}, nil
		}
	}
	return metav1.Condition{
		Type:    ConditionStoreInstalled,
		Status:  metav1.ConditionFalse,
		Reason:  "BootstrapInProgress",
		Message: fmt.Sprintf("Job %q running: active=%d succeeded=%d failed=%d", jobName, existing.Status.Active, existing.Status.Succeeded, existing.Status.Failed),
	}, nil
}

// ensureAdminPassword resolves the admin-password Secret reference. If
// the user supplied passwordSecretRef the operator honors it unchanged.
// Otherwise the operator generates a 20-character base64 password once
// into <store>-admin (key `password`), with an owner reference back to
// the MageStore so delete-cascade cleans it up.
func (r *MageStoreReconciler) ensureAdminPassword(ctx context.Context, store *magev1alpha1.MageStore) (corev1.SecretKeySelector, error) {
	admin := store.Spec.Bootstrap.Admin
	if admin.PasswordSecretRef != nil {
		key := admin.PasswordSecretRef.Key
		if key == "" {
			key = adminSecretPasswordKey
		}
		return corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: admin.PasswordSecretRef.Name},
			Key:                  key,
		}, nil
	}

	name := store.Name + adminSecretSuffix
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		// Generate-once: only populate the key when it doesn't already
		// exist. Rotating the password via reconcile would lock out
		// existing logins silently.
		if _, ok := sec.Data[adminSecretPasswordKey]; !ok {
			if sec.StringData == nil {
				sec.StringData = map[string]string{}
			}
			sec.StringData[adminSecretPasswordKey] = newAdminPassword()
		}
		sec.Type = corev1.SecretTypeOpaque
		return ctrl.SetControllerReference(store, sec, r.Scheme)
	})
	if err != nil {
		return corev1.SecretKeySelector{}, err
	}
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  adminSecretPasswordKey,
	}, nil
}

// buildBootstrapJob constructs the unowned Job object. Caller is
// responsible for SetControllerReference and Create. The Job shape
// mirrors config/samples/bootstrap-install-job.yaml — same envFrom
// sources (config Secret + optional SMTP Secret), same working dir,
// same restart/backoff policy. The only live differences from the
// sample are admin-identity env vars (sourced from spec.bootstrap.admin)
// and the ADMIN_PASSWORD reference (resolved by ensureAdminPassword).
func (r *MageStoreReconciler) buildBootstrapJob(store *magev1alpha1.MageStore, name string, adminPasswordRef, adminBackendFrontnameRef corev1.SecretKeySelector, smtpSecretHash string) *batchv1.Job {
	admin := store.Spec.Bootstrap.Admin
	labels := map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/component":  "bootstrap",
		"app.kubernetes.io/managed-by": managedByLabel,
	}
	image := fmt.Sprintf("%s:%s", store.Spec.Image.Repository, store.Spec.Image.Tag)

	// backoffLimit=1 matches the sample manifest's behavior: one retry
	// on transient failure (DB slow to accept connections, search not
	// fully warm) and then give up. Automatic retry-forever on a
	// destructive operation is the wrong default; the user unsticks a
	// failed bootstrap by deleting the Job and letting the operator
	// recreate it.
	backoffLimit := int32(1)
	ttl := int32(3600) // garbage-collect one hour after completion

	podAnnotations := map[string]string{}
	if smtpSecretHash != "" {
		podAnnotations[smtpSecretHashAnnotation] = smtpSecretHash
	}

	bootstrapEnvFrom := []corev1.EnvFromSource{
		{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configSecretName(store)},
		}},
	}
	if smtpName := smtpEnvFromSecretName(store); smtpName != "" {
		bootstrapEnvFrom = append(bootstrapEnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: smtpName},
			},
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: store.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:            bootstrapContainer,
						Image:           image,
						ImagePullPolicy: workloadImagePullPolicy(store),
						WorkingDir:      "/app",
						Command:         []string{"/bin/sh", "-c"},
						Args:            []string{buildBootstrapScript(store)},
						EnvFrom:         bootstrapEnvFrom,
						Env: []corev1.EnvVar{
							{Name: "ADMIN_EMAIL", Value: admin.Email},
							{Name: "ADMIN_FIRSTNAME", Value: defaulted(admin.FirstName, "Admin")},
							{Name: "ADMIN_LASTNAME", Value: defaulted(admin.LastName, "User")},
							{Name: "ADMIN_USERNAME", Value: defaulted(admin.Username, "admin")},
							{Name: "ADMIN_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &adminPasswordRef}},
							{Name: "ADMIN_BACKEND_FRONTNAME", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &adminBackendFrontnameRef}},
						},
					}},
				},
			},
		},
	}
	return job
}

// defaulted returns v if non-empty, else fallback. The CRD's kubebuilder
// defaults take care of this server-side, but reading the type off the
// live object after create/update gives us whatever was persisted —
// which may be the server-defaulted value or a user-supplied empty
// string depending on how the CR was applied. Belt-and-braces.
func defaulted(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// newAdminPassword produces a 20-character base64-encoded random string
// guaranteed to contain at least one digit and one letter.
//
// Magento 2.4's `setup:install` rejects passwords missing either class
// with "Your password must include both numeric and alphabetic
// characters." A 20-char RawURLEncoding string drawn from a 64-char
// alphabet has a ~4% chance of containing zero digits; we retry until
// the class check passes (P(failure)^5 ≈ 1e-7, bounded loop is safe).
func newAdminPassword() string {
	for {
		buf := make([]byte, 15)
		_, _ = rand.Read(buf)
		pw := base64.RawURLEncoding.EncodeToString(buf)
		if strings.ContainsAny(pw, "0123456789") &&
			strings.ContainsAny(pw, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			return pw
		}
	}
}

// resolveAdminFrontname returns the persisted admin URL path segment
// for routing-rule construction. Reads `<store>-admin` Secret's
// `backendFrontname` key. Falls back to literal `admin` (the
// existingStoreBackendFrontname constant) when the Secret is absent
// (adoption case) or when the key is unset (rare reconcile pass that
// runs before ensureBootstrap has materialised the Secret).
//
// Used by ensureRouting to build the admin-tier HTTPRoute rule —
// without this lookup, the HTTPRoute would route literal `/admin` to
// the admin backend regardless of `--backend-frontname`, defeating the
// gap #3 randomization end-to-end. Pure read; no mutation. Safe to
// call on every reconcile of every ensureRouting invocation.
func (r *MageStoreReconciler) resolveAdminFrontname(ctx context.Context, store *magev1alpha1.MageStore) (string, error) {
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: store.Name + adminSecretSuffix}, sec)
	if apierrors.IsNotFound(err) {
		return existingStoreBackendFrontname, nil
	}
	if err != nil {
		return "", fmt.Errorf("get admin secret for routing: %w", err)
	}
	if v, ok := sec.Data[adminSecretBackendFrontnameKey]; ok && len(v) > 0 {
		return string(v), nil
	}
	return existingStoreBackendFrontname, nil
}

// newAdminBackendFrontname produces a random URL-safe path segment
// drawn from adminBackendFrontnameAlphabet, of length
// adminBackendFrontnameLength. ~95 bits of entropy in 16 chars; well
// past brute-force territory for an unlinked admin URL. The output
// always satisfies the AdminSpec.BackendFrontname regex
// `^[a-zA-Z0-9_-]{4,32}$`.
func newAdminBackendFrontname() string {
	buf := make([]byte, adminBackendFrontnameLength)
	_, _ = rand.Read(buf)
	out := make([]byte, adminBackendFrontnameLength)
	for i, b := range buf {
		out[i] = adminBackendFrontnameAlphabet[int(b)%len(adminBackendFrontnameAlphabet)]
	}
	return string(out)
}

// deriveAdminURL formats the convenience admin URL string the operator
// writes to the `<store>-admin` Secret under `adminUrl`. Mirrors the
// ADR 013 URL derivation matrix for default scope: real hostname +
// TLS → https://; real hostname no TLS → http://; no stores → falls
// back to localhost:8080 (dev/test installs). Trailing slash is
// included so the value drops into a curl/browser cleanly.
func deriveAdminURL(store *magev1alpha1.MageStore, frontname string) string {
	hasStores := len(store.Spec.Stores) > 0
	hasTLS := store.Spec.Routing != nil && store.Spec.Routing.TLS != nil
	host := "localhost:8080"
	if hasStores {
		host = store.Spec.Stores[0].Host
	}
	scheme := "http"
	if hasTLS {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/", scheme, host, frontname)
}

// ensureAdminBackendFrontname resolves the admin URL path segment the
// bootstrap Job uses for `--backend-frontname` and persists the
// resolved value (plus a convenience `adminUrl` key) into the
// `<store>-admin` Secret. Resolution rules per ADR 014:
//
//  1. AdminSpec.BackendFrontname is set → use it verbatim. Persist for
//     stickiness so the bootstrap Job's env var sources from the
//     Secret regardless of spec.
//  2. `<store>-admin` Secret already carries `backendFrontname` → use
//     the existing value (sticky-once-generated; the operator does
//     NOT rotate on spec edit).
//  3. `<store>-admin` Secret exists with no `backendFrontname` key →
//     existing v0.6.x store upgraded to v0.7.0. Default to literal
//     `admin` per ADR 014's upgrade-safety rule. Persist for
//     stickiness.
//  4. `<store>-admin` Secret does not exist → fresh store. Generate a
//     16-char random URL-safe string. Persist.
//
// Returns the resolved frontname value and a SecretKeySelector pointing
// at the persisted key (consumed by buildBootstrapJob to source the
// container's ADMIN_BACKEND_FRONTNAME env var). Mirrors the shape of
// ensureAdminPassword — together they own the `<store>-admin` Secret.
//
// Why `<store>-admin` Secret existence is the upgrade-safety signal
// (not `<store>-config` as ADR 014 §"Shared upgrade-safety defaulting
// rule" calls out): both Secrets satisfy the criterion ("operator-
// owned, created exactly once at first bootstrap"), but `<store>-admin`
// is co-located with the field — checking it here in the same Get
// avoids the reconcile-order race introduced by `<store>-config` being
// written by ensureSecrets earlier in the same pass. Functionally
// equivalent to the ADR's signal; cleaner implementation.
func (r *MageStoreReconciler) ensureAdminBackendFrontname(ctx context.Context, store *magev1alpha1.MageStore) (string, corev1.SecretKeySelector, error) {
	name := store.Name + adminSecretSuffix
	sec := &corev1.Secret{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: name}, sec)

	var resolved string
	switch {
	case store.Spec.Bootstrap.Admin.BackendFrontname != "":
		resolved = store.Spec.Bootstrap.Admin.BackendFrontname
	case getErr == nil:
		if existing, ok := sec.Data[adminSecretBackendFrontnameKey]; ok && len(existing) > 0 {
			// Sticky value already persisted; return it without
			// rewriting the Secret. Idempotent on every subsequent
			// reconcile.
			return string(existing), corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Key:                  adminSecretBackendFrontnameKey,
			}, nil
		}
		// Secret exists but no backendFrontname key — existing v0.6.x
		// store upgraded to v0.7.0. Preserve bookmarks per ADR 014.
		resolved = existingStoreBackendFrontname
	case apierrors.IsNotFound(getErr):
		// Fresh store. Generate.
		resolved = newAdminBackendFrontname()
	default:
		return "", corev1.SecretKeySelector{}, fmt.Errorf("get admin secret: %w", getErr)
	}

	// Persist resolved value + convenience adminUrl key.
	persist := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, persist, func() error {
		if persist.StringData == nil {
			persist.StringData = map[string]string{}
		}
		persist.StringData[adminSecretBackendFrontnameKey] = resolved
		persist.StringData[adminSecretAdminURLKey] = deriveAdminURL(store, resolved)
		persist.Type = corev1.SecretTypeOpaque
		return ctrl.SetControllerReference(store, persist, r.Scheme)
	}); err != nil {
		return "", corev1.SecretKeySelector{}, err
	}

	return resolved, corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  adminSecretBackendFrontnameKey,
	}, nil
}

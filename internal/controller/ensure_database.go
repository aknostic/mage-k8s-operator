package controller

import (
	"context"
	"fmt"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// Naming and key conventions for the database backend. `mariaDbSuffix` is
// the inline-mode MariaDB CR name derived from the MageStore; it doubles
// as the Service hostname (mariadb-operator names the front-door Service
// after the CR). `mariaDbRootSecret*` are the Secret name/key the inline
// path asks mariadb-operator to generate into. External-mode credential
// keys are fixed to `username` / `password` so users don't have to
// encode a key-mapping per MageStore.
const (
	mariaDbSuffix             = "-mariadb"
	mariaDbRootSecretSuffix   = "-mariadb-root"
	mariaDbRootSecretKey      = "root-password"
	externalUsernameSecretKey = "username"
	externalPasswordSecretKey = "password"

	// defaultMagentoDatabaseName is the schema name setup:install seeds.
	// mariadb-operator's initial Database field is unset so the operator
	// creates the schema itself during bootstrap (see ensure_bootstrap.go
	// — the PDO CREATE DATABASE line).
	defaultMagentoDatabaseName = "magento"

	// defaultMariaDBImageRegistry is the canonical image base recommended
	// by mariadb-operator's docs. The inline path pins the tag per
	// spec.database.mariaDb.version; the registry is fixed.
	defaultMariaDBImageRegistry = "docker-registry1.mariadb.com/library/mariadb"
)

// DatabaseEndpoint carries the resolved DB connection details for
// ensureSecrets. Mirrors SearchEndpoint: a zero value means "nothing
// to emit," but configured-but-not-ready is indicated by the
// DatabaseReady condition, not Endpoint presence. The secrets step
// does not and must not branch on which mode produced the endpoint.
type DatabaseEndpoint struct {
	Host     string
	Port     int32
	Name     string
	Username string
	Password string
}

// ensureDatabase resolves the configured database backend and returns
// a DatabaseReady condition plus the connection details ensureSecrets
// needs. Three modes per ADR 004:
//
//   - Inline (`spec.database.mariaDb`): the operator creates and owns a
//     MariaDB CR. mariadb-operator generates the root password and
//     provisions the StatefulSet. Endpoint resolves to the operator-
//     generated Service + credentials.
//   - Reference (`spec.database.mariaDbRef`): the MariaDB CR is pre-
//     created by an admin. Operator Get()s it for readiness; no
//     ownerReference. Credentials come from the CR's configured
//     rootPasswordSecretKeyRef.
//   - External (`spec.database.external`): no CR is involved. The
//     operator reads credentials from a user-provided Secret and
//     synthesises the endpoint. Readiness is "Secret exists with
//     username+password keys" — the operator cannot probe an arbitrary
//     remote MySQL endpoint without a side-channel, so actual reach-
//     ability is verified implicitly by setup:install.
//
// Inline and ref share a single readiness path (MariaDB.IsReady());
// external uses a shorter Secret-only path.
func (r *MageStoreReconciler) ensureDatabase(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, DatabaseEndpoint, error) {
	if store.Spec.Database == nil {
		if r.Capabilities.HasMariaDB {
			if err := r.sweepOrphanMariaDB(ctx, store); err != nil {
				return metav1.Condition{}, DatabaseEndpoint{}, err
			}
		}
		return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.database is empty",
		}, DatabaseEndpoint{}, nil
	}

	switch {
	case store.Spec.Database.MariaDb != nil:
		if !r.Capabilities.HasMariaDB {
			cond := dependencyMissingCondition(ConditionDatabaseReady, depMariaDB)
			return cond, DatabaseEndpoint{}, nil
		}
		return r.ensureDatabaseInline(ctx, store)
	case store.Spec.Database.MariaDbRef != nil:
		if !r.Capabilities.HasMariaDB {
			cond := dependencyMissingCondition(ConditionDatabaseReady, depMariaDB)
			return cond, DatabaseEndpoint{}, nil
		}
		return r.readyForMariaDB(ctx, store, store.Spec.Database.MariaDbRef.Name)
	case store.Spec.Database.External != nil:
		return r.ensureDatabaseExternal(ctx, store)
	default:
		// CRD-level XValidation blocks this in practice; keep the branch
		// so the operator degrades to Unknown if CEL is ever bypassed.
		return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "NotConfigured",
			Message: "spec.database has no backend sibling set",
		}, DatabaseEndpoint{}, nil
	}
}

// ensureDatabaseInline materialises the MariaDB CR, then delegates to
// the shared readiness path used by mariaDbRef. The inline mode owns
// the CR; ref mode does not — that distinction lives entirely in
// materialiseMariaDB.
func (r *MageStoreReconciler) ensureDatabaseInline(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, DatabaseEndpoint, error) {
	name := store.Name + mariaDbSuffix
	if err := r.materialiseMariaDB(ctx, store, name); err != nil {
		return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionFalse,
			Reason:  "MaterialiseFailed",
			Message: err.Error(),
		}, DatabaseEndpoint{}, fmt.Errorf("materialise MariaDB: %w", err)
	}
	return r.readyForMariaDB(ctx, store, name)
}

// readyForMariaDB looks up the MariaDB CR, evaluates readiness via
// the typed IsReady helper, and resolves credentials into a
// DatabaseEndpoint. Shared by inline and ref modes.
func (r *MageStoreReconciler) readyForMariaDB(ctx context.Context, store *magev1alpha1.MageStore, name string) (metav1.Condition, DatabaseEndpoint, error) {
	var md mariadbv1alpha1.MariaDB
	key := types.NamespacedName{Namespace: store.Namespace, Name: name}
	if err := r.Get(ctx, key, &md); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.Condition{
				Type:    ConditionDatabaseReady,
				Status:  metav1.ConditionFalse,
				Reason:  "NotFound",
				Message: fmt.Sprintf("MariaDB %q not found in namespace %q", key.Name, key.Namespace),
			}, DatabaseEndpoint{}, nil
		}
		if meta.IsNoMatchError(err) {
			// Probe-stale race: CRD was present at startup but has since
			// been uninstalled. Surface the same condition the up-front
			// capability check would have produced.
			return dependencyMissingCondition(ConditionDatabaseReady, depMariaDB), DatabaseEndpoint{}, nil
		}
		return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "LookupError",
			Message: err.Error(),
		}, DatabaseEndpoint{}, nil
	}

	if !md.IsReady() {
		if upstream := meta.FindStatusCondition(md.Status.Conditions, mariadbv1alpha1.ConditionTypeReady); upstream != nil {
			return metav1.Condition{
				Type:    ConditionDatabaseReady,
				Status:  upstream.Status,
				Reason:  "MariaDB" + upstream.Reason,
				Message: upstream.Message,
			}, DatabaseEndpoint{}, nil
		}
		return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NotReady",
			Message: "MariaDB has no Ready condition yet",
		}, DatabaseEndpoint{}, nil
	}

	pwRef := md.Spec.RootPasswordSecretKeyRef
	if pwRef.Name == "" || pwRef.Key == "" {
		return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionFalse,
			Reason:  "MissingCredentialRef",
			Message: "MariaDB.spec.rootPasswordSecretKeyRef is unset",
		}, DatabaseEndpoint{}, nil
	}
	password, probCond, err := r.readDatabaseSecretKey(ctx, store.Namespace, pwRef.Name, pwRef.Key)
	if err != nil {
		return metav1.Condition{}, DatabaseEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, DatabaseEndpoint{}, nil
	}

	port := int32(3306)
	if md.Spec.Port != 0 {
		port = md.Spec.Port
	}
	return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionTrue,
			Reason:  "MariaDBReady",
			Message: fmt.Sprintf("MariaDB %q is ready", key.Name),
		}, DatabaseEndpoint{
			Host:     md.Name,
			Port:     port,
			Name:     defaultMagentoDatabaseName,
			Username: "root",
			Password: password,
		}, nil
}

// ensureDatabaseExternal wires `spec.database.external` through to an
// endpoint. The operator performs no provisioning, no readiness probe,
// and no ongoing watch of the remote database — it validates that the
// credentials Secret has the expected keys, and that's the full extent
// of Phase-1's health signal. setup:install / pod boot is where a
// misconfigured external endpoint actually surfaces.
func (r *MageStoreReconciler) ensureDatabaseExternal(ctx context.Context, store *magev1alpha1.MageStore) (metav1.Condition, DatabaseEndpoint, error) {
	ext := store.Spec.Database.External
	secretName := ext.CredentialsSecretRef.Name

	username, probCond, err := r.readDatabaseSecretKey(ctx, store.Namespace, secretName, externalUsernameSecretKey)
	if err != nil {
		return metav1.Condition{}, DatabaseEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, DatabaseEndpoint{}, nil
	}
	password, probCond, err := r.readDatabaseSecretKey(ctx, store.Namespace, secretName, externalPasswordSecretKey)
	if err != nil {
		return metav1.Condition{}, DatabaseEndpoint{}, err
	}
	if probCond.Status != "" {
		return probCond, DatabaseEndpoint{}, nil
	}

	port := ext.Port
	if port == 0 {
		port = 3306
	}
	dbName := ext.DatabaseName
	if dbName == "" {
		dbName = defaultMagentoDatabaseName
	}
	return metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionTrue,
			Reason:  "ExternalReady",
			Message: fmt.Sprintf("external endpoint %s:%d credentials resolved from Secret %q", ext.Host, port, secretName),
		}, DatabaseEndpoint{
			Host:     ext.Host,
			Port:     port,
			Name:     dbName,
			Username: username,
			Password: password,
		}, nil
}

// readDatabaseSecretKey fetches a single key from a namespaced Secret.
// On a user-visible problem (Secret missing, key missing/empty) it
// returns a DatabaseReady=False condition with a specific Reason so the
// caller can surface it unchanged. Transient errors propagate as Go
// errors and the reconciler requeues. The zero-condition sentinel
// ("no problem, use the value") is intentional: it keeps the happy
// path in the caller linear and avoids shuttling an ok-boolean through
// three nested branches.
func (r *MageStoreReconciler) readDatabaseSecretKey(ctx context.Context, namespace, name, key string) (string, metav1.Condition, error) {
	var sec corev1.Secret
	nn := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Get(ctx, nn, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", metav1.Condition{
				Type:    ConditionDatabaseReady,
				Status:  metav1.ConditionFalse,
				Reason:  "CredentialSecretMissing",
				Message: fmt.Sprintf("Secret %q not found in namespace %q", name, namespace),
			}, nil
		}
		return "", metav1.Condition{}, fmt.Errorf("get credential secret %s: %w", nn, err)
	}
	val, ok := sec.Data[key]
	if !ok || len(val) == 0 {
		return "", metav1.Condition{
			Type:    ConditionDatabaseReady,
			Status:  metav1.ConditionFalse,
			Reason:  "CredentialKeyEmpty",
			Message: fmt.Sprintf("Secret %q has no key %q", name, key),
		}, nil
	}
	return string(val), metav1.Condition{}, nil
}

// materialiseMariaDB creates or updates the MariaDB CR when the
// MageStore is in inline mode. Curated defaults keep the inline spec
// small: Magento-compatible image version pin (11.4 by default), a
// single-replica StatefulSet, 1Gi PVC on the cluster's default
// StorageClass, and a mariadb-operator-generated root-password Secret
// (`<store>-mariadb-root`, key `root-password`). Users needing anything
// outside this envelope (replication, custom my.cnf, Backup/Restore,
// dedicated Magento User via User/Grant CRDs) should graduate to
// `mariaDbRef` and manage the CR directly.
func (r *MageStoreReconciler) materialiseMariaDB(ctx context.Context, store *magev1alpha1.MageStore, name string) error {
	inline := store.Spec.Database.MariaDb
	storageSize, err := resource.ParseQuantity(inline.StorageSize)
	if err != nil {
		return fmt.Errorf("parse storageSize %q: %w", inline.StorageSize, err)
	}

	md := &mariadbv1alpha1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, md, func() error {
		if md.Labels == nil {
			md.Labels = map[string]string{}
		}
		md.Labels["app.kubernetes.io/name"] = "mariadb"
		md.Labels["app.kubernetes.io/instance"] = store.Name
		md.Labels["app.kubernetes.io/managed-by"] = managedByLabel

		md.Spec.Image = fmt.Sprintf("%s:%s", defaultMariaDBImageRegistry, inline.Version)

		// mariadb-operator generates the root-password Secret on first
		// reconcile when Generate=true. The operator does NOT own that
		// Secret (mariadb-operator does) — we just read it back in the
		// readiness path. Name/Key must remain stable across reconciles;
		// the upstream webhook marker `inmutableinit` rejects changes.
		md.Spec.RootPasswordSecretKeyRef = mariadbv1alpha1.GeneratedSecretKeyRef{
			SecretKeySelector: mariadbv1alpha1.SecretKeySelector{
				LocalObjectReference: mariadbv1alpha1.LocalObjectReference{Name: store.Name + mariaDbRootSecretSuffix},
				Key:                  mariaDbRootSecretKey,
			},
			Generate: true,
		}

		md.Spec.Storage.Size = &storageSize

		md.Spec.Resources = defaultMariaDBResources(inline.Resources)

		return ctrl.SetControllerReference(store, md, r.Scheme)
	})
	return err
}

// sweepOrphanMariaDB deletes the inline-mode MariaDB CR when
// `spec.database` transitions to nil. Ref-mode CRs lack the operator's
// instance + managed-by labels and are passed over by the selector.
// Closes ADR-009 gap #2b for the database tier.
func (r *MageStoreReconciler) sweepOrphanMariaDB(ctx context.Context, store *magev1alpha1.MageStore) error {
	var list mariadbv1alpha1.MariaDBList
	listOpts := []client.ListOption{
		client.InNamespace(store.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/instance":   store.Name,
			"app.kubernetes.io/managed-by": managedByLabel,
		},
	}
	if err := r.List(ctx, &list, listOpts...); err != nil {
		// Probe-stale race: caps said HasMariaDB at startup but the CRD
		// is gone now. Don't fail the reconcile — there's nothing left
		// to sweep.
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list owned mariadbs: %w", err)
	}
	for i := range list.Items {
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete mariadb %s: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

// defaultMariaDBResources applies dev-test-sensible defaults when the
// user doesn't override. Production deployments that want tighter
// limits or any non-trivial sizing should move off inline mode.
func defaultMariaDBResources(override *corev1.ResourceRequirements) *mariadbv1alpha1.ResourceRequirements {
	if override != nil {
		return &mariadbv1alpha1.ResourceRequirements{
			Requests: override.Requests,
			Limits:   override.Limits,
		}
	}
	return &mariadbv1alpha1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

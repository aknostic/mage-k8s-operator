package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// Media env var names. The namespace mirrors `MAGENTO_*` for
// consistency with the existing image-contract env vars; the image's
// env.php reads them via `getenv()` and wires them into Magento's
// remote-storage driver config. Only populated for `spec.media.s3`
// mode — PVC / emptyDir modes provide media via the mounted volume
// and need no env vars.
const (
	envKeyMediaBucket          = "MAGENTO_MEDIA_STORAGE_BUCKET"
	envKeyMediaEndpoint        = "MAGENTO_MEDIA_STORAGE_ENDPOINT"
	envKeyMediaRegion          = "MAGENTO_MEDIA_STORAGE_REGION"
	envKeyMediaAccessKeyID     = "MAGENTO_MEDIA_STORAGE_ACCESS_KEY_ID"
	envKeyMediaSecretAccessKey = "MAGENTO_MEDIA_STORAGE_SECRET_ACCESS_KEY"
)

// mediaVolumeName is the stable Kubernetes volume name for the
// pub/media mount. Referenced by the VolumeMount in
// ensureWorkloadDeployment.
const mediaVolumeName = "media"

// mediaMountPath is the canonical in-container path Magento expects
// for pub/media. Matches the Dockerfile's `mkdir -p /app/pub/media`.
const mediaMountPath = "/app/pub/media"

// mediaPvcSuffix is the PVC name suffix for the operator-provisioned
// PVC in `spec.media.pvc` mode.
const mediaPvcSuffix = "-media"

// Secret keys on the user-supplied S3 credentials Secret. Match AWS
// SDK v2's default credential-file schema so a user pointing
// `credentialsSecretRef` at an AWS-style Secret works without key
// renaming.
const (
	mediaS3AccessKeyIDKey     = "accessKeyId"
	mediaS3SecretAccessKeyKey = "secretAccessKey"
)

// MediaEndpoint carries the per-workload volume source and any env
// vars that downstream steps must emit. `ensureMedia` is the single
// producer; `ensureSecrets` consumes `EnvVars`; `ensureWorkloads`
// consumes `Volume` and `SeedFromImage`.
//
// Every mode populates a Volume (media is a per-pod scratch dir even
// when the backing store is remote — Magento's image processing
// pipeline uses pub/media as a staging area before uploading). Only
// the s3 mode populates EnvVars.
//
// Four modes map to concrete values:
//
//   - unset (`spec.media == nil`): Volume = emptyDir, EnvVars = nil,
//     SeedFromImage = false. Dev/demo back-compat shape — media does
//     not survive pod restarts.
//   - `spec.media.emptyDir`: Volume = emptyDir, EnvVars = nil.
//     Explicit emptyDir mode; SeedFromImage honoured.
//   - `spec.media.pvc`: Volume = PVC reference, EnvVars = nil. The
//     operator provisions `<store>-media` with an ownerReference
//     back to the MageStore. SeedFromImage honoured.
//   - `spec.media.s3`: Volume = emptyDir (scratch), EnvVars =
//     MAGENTO_MEDIA_STORAGE_* resolved from
//     spec.media.s3.credentialsSecretRef + the inline fields.
//     SeedFromImage triggers a one-shot seed Job (ensureMediaSeed).
type MediaEndpoint struct {
	Volume        corev1.Volume
	EnvVars       map[string]string
	SeedFromImage bool // consumed by ensureWorkloads (initContainer in C2.3) and ensureMediaSeed (S3 Job in C2.2)
}

// ensureMedia resolves the configured media backend into a
// MediaEndpoint. No condition is surfaced — media is a per-pod
// volume decision, not a rollout gate. S3-credential errors surface
// through ensureSecrets (the same mechanism we use for other
// credential-Secret problems); PVC provisioning errors surface via
// the workload pod's own status (unschedulable → Pending).
//
// Pipeline placement: right after ensureQueue, before ensureSecrets.
// ensureSecrets needs the S3 env vars inline with DB/Redis/etc.;
// ensureWorkloads needs the Volume on the pod template.
// emptyDirMediaVolume returns the standard emptyDir Volume for pub/media.
// Used by nil-spec back-compat, explicit emptyDir mode, and S3 mode
// (S3 still needs a local scratch dir for Magento's image-processing
// pipeline).
func emptyDirMediaVolume() corev1.Volume {
	return corev1.Volume{
		Name: mediaVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

// pvcMediaVolume returns a PVC-backed Volume referencing the operator-
// provisioned <store>-media PVC.
func pvcMediaVolume(store *magev1alpha1.MageStore) corev1.Volume {
	return corev1.Volume{
		Name: mediaVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: store.Name + mediaPvcSuffix,
			},
		},
	}
}

func (r *MageStoreReconciler) ensureMedia(ctx context.Context, store *magev1alpha1.MageStore) (MediaEndpoint, error) {
	seedFlag := false
	if store.Spec.Media != nil {
		seedFlag = store.Spec.Media.SeedFromImage
	}

	// Unset: emptyDir scratch mount. Good for demos, fine for
	// frontend-only single-replica deployments, inappropriate for
	// anything else. SeedFromImage cannot be requested without an
	// explicit MediaSpec block.
	if store.Spec.Media == nil {
		return MediaEndpoint{
			Volume:        emptyDirMediaVolume(),
			SeedFromImage: false,
		}, nil
	}

	if store.Spec.Media.EmptyDir != nil {
		return MediaEndpoint{
			Volume:        emptyDirMediaVolume(),
			SeedFromImage: seedFlag,
		}, nil
	}

	if store.Spec.Media.Pvc != nil {
		if err := r.ensureMediaPVC(ctx, store); err != nil {
			return MediaEndpoint{}, fmt.Errorf("ensure media pvc: %w", err)
		}
		return MediaEndpoint{
			Volume:        pvcMediaVolume(store),
			SeedFromImage: seedFlag,
		}, nil
	}

	if store.Spec.Media.S3 != nil {
		envVars, err := r.resolveMediaS3EnvVars(ctx, store)
		if err != nil {
			return MediaEndpoint{}, fmt.Errorf("resolve s3 credentials: %w", err)
		}
		return MediaEndpoint{
			// S3 mode still mounts an emptyDir at pub/media —
			// Magento's image-processing pipeline uses it as a
			// staging area; the remote-storage driver uploads
			// from there to S3. Zero bytes on-disk at steady
			// state on a storefront-only pod.
			Volume:        emptyDirMediaVolume(),
			EnvVars:       envVars,
			SeedFromImage: seedFlag,
		}, nil
	}

	// CRD-level XValidation blocks this; keep the branch so we
	// degrade to emptyDir rather than nil-pointer-dereference if
	// CEL is ever bypassed.
	return MediaEndpoint{
		Volume:        emptyDirMediaVolume(),
		SeedFromImage: false,
	}, nil
}

// ensureMediaPVC provisions the `<store>-media` PVC. ReadWriteOnce
// per the CRD comment — RWX against Magento's pub/media is known
// to corrupt at scale. Workload split (frontend / admin / api) with
// a single RWO PVC means only one workload pod can have the PVC
// mounted at a time; this is documented as a dev/test-only shape.
func (r *MageStoreReconciler) ensureMediaPVC(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + mediaPvcSuffix
	inline := store.Spec.Media.Pvc

	size := inline.Size
	if size == "" {
		size = "10Gi"
	}
	sizeQuantity, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("parse pvc size %q: %w", size, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		// AccessModes and Resources are immutable once the PVC is
		// bound; CreateOrUpdate's Get-then-mutate pattern preserves
		// them across reconciles without a diff error. Size CAN be
		// grown on some StorageClasses — not attempting that here;
		// users resize via `kubectl edit pvc` directly.
		if pvc.CreationTimestamp.IsZero() {
			pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			pvc.Spec.Resources = corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: sizeQuantity,
				},
			}
			if inline.StorageClassName != "" {
				sc := inline.StorageClassName
				pvc.Spec.StorageClassName = &sc
			}
		}
		pvc.Labels = map[string]string{
			"app.kubernetes.io/name":       "magento",
			"app.kubernetes.io/instance":   store.Name,
			"app.kubernetes.io/component":  "media",
			"app.kubernetes.io/managed-by": managedByLabel,
		}
		return ctrl.SetControllerReference(store, pvc, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("media pvc reconciled", "op", string(op))
	}
	return nil
}

// resolveMediaS3EnvVars reads the user's S3 credentials Secret and
// returns the MAGENTO_MEDIA_STORAGE_* env vars. Missing Secret /
// missing keys propagate as errors so the reconciler requeues and
// surfaces via ensureSecrets' downstream error path. Endpoint is
// emitted only when set (AWS S3 users leave it unset to let the SDK
// pick the regional default).
func (r *MageStoreReconciler) resolveMediaS3EnvVars(ctx context.Context, store *magev1alpha1.MageStore) (map[string]string, error) {
	s3 := store.Spec.Media.S3
	secretName := s3.CredentialsSecretRef.Name

	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: store.Namespace, Name: secretName}, &sec); err != nil {
		return nil, fmt.Errorf("get media S3 credentials Secret %s: %w", secretName, err)
	}
	accessKeyID, ok := sec.Data[mediaS3AccessKeyIDKey]
	if !ok || len(accessKeyID) == 0 {
		return nil, fmt.Errorf("media S3 Secret %q missing key %q", secretName, mediaS3AccessKeyIDKey)
	}
	secretAccessKey, ok := sec.Data[mediaS3SecretAccessKeyKey]
	if !ok || len(secretAccessKey) == 0 {
		return nil, fmt.Errorf("media S3 Secret %q missing key %q", secretName, mediaS3SecretAccessKeyKey)
	}

	envVars := map[string]string{
		envKeyMediaBucket:          s3.Bucket,
		envKeyMediaRegion:          s3.Region,
		envKeyMediaAccessKeyID:     string(accessKeyID),
		envKeyMediaSecretAccessKey: string(secretAccessKey),
	}
	if s3.Endpoint != "" {
		envVars[envKeyMediaEndpoint] = s3.Endpoint
	}
	return envVars, nil
}

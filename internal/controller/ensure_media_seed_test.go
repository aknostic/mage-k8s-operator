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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// ensureMediaSeed tests cover all 7 state-machine rows from ADR 011
// §"Reconcile graph and gating". No real Pods or Jobs run — envtest
// uses the fake API server; Job statuses are written with
// k8sClient.Status().Update to simulate Job completion (same pattern
// as ensure_upgrade_test.go's markJobComplete / markJobFailed helpers).
var _ = Describe("ensureMediaSeed", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	// emptyDirEndpoint returns a MediaEndpoint for emptyDir mode.
	emptyDirEndpoint := func(seed bool) MediaEndpoint {
		return MediaEndpoint{
			Volume:        emptyDirMediaVolume(),
			EnvVars:       nil,
			SeedFromImage: seed,
		}
	}

	// pvcEndpoint returns a MediaEndpoint for PVC mode.
	pvcEndpoint := func(seed bool) MediaEndpoint {
		return MediaEndpoint{
			Volume: corev1.Volume{
				Name: mediaVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "store-media",
					},
				},
			},
			EnvVars:       nil,
			SeedFromImage: seed,
		}
	}

	// s3Endpoint returns a MediaEndpoint for S3 mode (EnvVars non-nil).
	s3Endpoint := func(seed bool) MediaEndpoint {
		return MediaEndpoint{
			Volume:        emptyDirMediaVolume(),
			EnvVars:       map[string]string{"MAGENTO_MEDIA_STORAGE_BUCKET": "my-bucket"},
			SeedFromImage: seed,
		}
	}

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensuremediaseed-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Capabilities: fullCapabilities(),
		}
		store = &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/magento",
					Tag:        "2.4.8",
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
	})

	// Row 1: seedFromImage=false, any mode.
	Context("seedFromImage: false", func() {
		It("returns empty condition (no-op) for emptyDir mode", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, emptyDirEndpoint(false), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty(), "zero condition means no MediaSeeded row in status")
		})

		It("returns empty condition (no-op) for PVC mode", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, pvcEndpoint(false), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty())
		})

		It("returns empty condition (no-op) for S3 mode", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(false), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty())
		})
	})

	// Rows 2–3: emptyDir + seedFromImage=true.
	Context("emptyDir + seedFromImage: true", func() {
		It("returns Unknown/Seeding when WorkloadsReady=false", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, emptyDirEndpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Seeding"))
		})

		It("returns True/SeedComplete when WorkloadsReady=true", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, emptyDirEndpoint(true), true, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("SeedComplete"))
		})
	})

	// Rows 4–5: PVC + seedFromImage=true.
	Context("PVC + seedFromImage: true", func() {
		It("returns Unknown/Seeding when WorkloadsReady=false", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, pvcEndpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Seeding"))
		})

		It("returns True/SeedComplete when WorkloadsReady=true", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, pvcEndpoint(true), true, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("SeedComplete"))
		})
	})

	// Rows 6–9 + sticky: S3 + seedFromImage=true.
	Context("S3 + seedFromImage: true", func() {
		It("creates <store>-media-seed Job when no Job exists", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Seeding"))

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())

			// Verify Job spec matches ADR 011.
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(2)))
			Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(1800)))
			Expect(*job.Spec.TTLSecondsAfterFinished).To(Equal(int32(600)))
			Expect(job.Labels["app.kubernetes.io/component"]).To(Equal("media-seed"))
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal("seed"))
			Expect(container.Image).To(Equal("registry.example.com/magento:2.4.8"))
			Expect(container.ImagePullPolicy).To(Equal(corev1.PullAlways))
			Expect(container.Command).To(Equal([]string{"bash", "-c"}))
			Expect(container.Args[0]).To(ContainSubstring("set -euo pipefail"))
			Expect(container.Args[0]).To(ContainSubstring("cp -rn /app/pub/media-baked/."))
			Expect(container.Args[0]).To(ContainSubstring("bin/magento remote-storage:sync"))
			Expect(container.EnvFrom[0].SecretRef.Name).To(Equal(store.Name + "-config"))
			Expect(container.VolumeMounts[0].MountPath).To(Equal(mediaMountPath))
			Expect(job.Spec.Template.Spec.Volumes[0].EmptyDir).NotTo(BeNil())
		})

		It("returns Unknown/Seeding while Job is Active", func() {
			// First call creates the Job.
			_, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())

			// Mark Job as active.
			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())
			job.Status.Active = 1
			Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

			// Second call should still return Seeding.
			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Seeding"))
		})

		It("returns True/SeedComplete when Job Succeeded", func() {
			// First call creates the Job.
			_, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())
			Expect(markJobComplete(k8sClient, &job)).To(Succeed())

			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("SeedComplete"))
		})

		It("returns False/RetryingFailedJob on first terminal-Failed observation (gap #1 recovery)", func() {
			// First call creates the Job.
			_, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())
			Expect(markJobFailed(k8sClient, &job, "")).To(Succeed())

			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(JobRecoveryRetryReason))
			// JobAttempts records the failure for the central requeue.
			Expect(store.Status.JobAttempts).To(HaveKey(store.Name + mediaSeedJobSuffix))
			Expect(store.Status.JobAttempts[store.Name+mediaSeedJobSuffix].Count).To(Equal(1))
		})

		It("does not re-create Job when MediaSeeded=True is already set (sticky)", func() {
			// Pre-create the seed Job so there's an existing resource to check UID against.
			preJob := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      store.Name + mediaSeedJobSuffix,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": managedByLabel,
					},
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers:    []corev1.Container{{Name: "seed", Image: "alpine"}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, preJob)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, preJob)).To(Succeed())
			originalUID := preJob.UID

			// Pre-set MediaSeeded=True on the store's status conditions.
			store.Status.Conditions = []metav1.Condition{{
				Type:               ConditionMediaSeeded,
				Status:             metav1.ConditionTrue,
				Reason:             "SeedComplete",
				Message:            "pre-set for sticky test",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			// Call ensureMediaSeed — must short-circuit at the sticky-True check.
			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("SeedComplete"))

			// The existing Job must not have been recreated — UID unchanged.
			var afterJob batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, &afterJob)).To(Succeed())
			Expect(afterJob.UID).To(Equal(originalUID), "sticky-True must not recreate the Job")

			// The returned condition must be the existing True one (not a new object).
			existing := apimeta.FindStatusCondition(store.Status.Conditions, ConditionMediaSeeded)
			Expect(existing).NotTo(BeNil())
			Expect(existing.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// Row 1.5: S3 + seedFromImage=true + secretsReady=false. v0.6.3 #2
	// gate. Without this, the seed Job is created before
	// `<store>-config` exists, kubelet loops on
	// CreateContainerConfigError until backoffLimit, and the recovery
	// helper deletes-and-recreates it (~3-5 min wall time per
	// cold-start). The structural gate prevents the race entirely.
	Context("S3 + seedFromImage=true + secretsReady=false (row 1.5, v0.6.3 #2)", func() {
		It("returns Unknown/AwaitingSecrets and does NOT create a Job", func() {
			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionMediaSeeded))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("AwaitingSecrets"))

			var job batchv1.Job
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, &job)
			Expect(err).To(HaveOccurred(), "no media-seed Job should exist while AwaitingSecrets")
		})

		It("creates the Job once secretsReady flips to true (re-entrant)", func() {
			// Pass 1: secretsReady=false → AwaitingSecrets, no Job.
			cond, err := reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal("AwaitingSecrets"))

			// Pass 2: secretsReady=true → Job created, returns Seeding.
			cond, err = reconciler.ensureMediaSeed(ctx, store, s3Endpoint(true), false, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal("Seeding"))

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + mediaSeedJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())
		})

		It("does NOT gate volume modes on secretsReady (initContainer path)", func() {
			// emptyDir + seedFromImage + secretsReady=false should NOT
			// short-circuit on AwaitingSecrets — the volume-mode seed
			// path runs as a workload-pod initContainer that's gated
			// transitively via dependenciesReady → workloadsReady. Row
			// 2 (Unknown/Seeding) is the correct outcome here.
			cond, err := reconciler.ensureMediaSeed(ctx, store, emptyDirEndpoint(true), false, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal("Seeding"), "volume modes use the initContainer path; the SecretsReady gate scopes to S3 only")
		})
	})
})

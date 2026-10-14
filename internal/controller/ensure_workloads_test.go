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

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// testMediaVolume returns the emptyDir-backed Volume that ensureMedia
// emits when spec.media is nil. Tests that don't exercise the media
// path use testMediaEndpoint() instead; this helper is kept for the
// new media-seed test cases that construct MediaEndpoint inline.
func testMediaVolume() corev1.Volume {
	return corev1.Volume{
		Name: "media",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

// testMediaEndpoint returns a MediaEndpoint with an emptyDir volume,
// SeedFromImage: false, and no EnvVars. Used by existing tests that
// don't exercise the media-seed path — default "no seed" posture.
func testMediaEndpoint() MediaEndpoint {
	return MediaEndpoint{
		Volume:        testMediaVolume(),
		SeedFromImage: false,
		EnvVars:       nil,
	}
}

// ensureWorkloads is the operator's busiest fan-out step — three
// workload kinds, three resource types per kind (Deployment, Service,
// HPA), an orphan sweep, and a routing sibling (Gateway + HTTPRoute)
// that shares its backend selection with the workload set. envtest is
// the cheapest place to exercise the multi-workload + transition
// shapes without spinning up real pods; the fake apiserver lets us
// toggle `spec.workloads.admin` / `.api` and watch the operator's
// materialise + delete behaviour without scheduling.
//
// The suite covers ensureWorkloads + ensureRouting inline (same
// reconciler, same per-kind backend selection — splitting them
// would duplicate the per-kind setup); ensure_nginx_config_test.go
// covers the ConfigMap content path.
var _ = Describe("ensureWorkloads", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	// Fresh namespace per test — same GenerateName pattern as the
	// ensureCache / ensureQueue suites. envtest has no namespace
	// controller; Delete would mark namespaces Terminating forever
	// and cause name reuse to collide.
	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensureworkloads-"},
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
					Repository: "registry.example.com/test/magento",
					Tag:        "2.4.8",
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
	})

	Context("frontend-only (Slot-20 shape)", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("emits one Deployment and one Service named <store>-frontend", func() {
			cond, err := reconciler.ensureWorkloads(ctx, store, true, true, "deadbeef", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionWorkloadsReady))

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "frontend"))
			Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(nginxConfigHashAnnotation, "deadbeef"))

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/component", "frontend"))

			// Admin / api resources must NOT exist.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-api", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("emits no HPA when autoscaling is unset", func() {
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &autoscalingv2.HorizontalPodAutoscaler{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("returns AwaitingDependencies and skips Deployment when !dependenciesReady, but still emits the Service", func() {
			cond, err := reconciler.ensureWorkloads(ctx, store, false, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("AwaitingDependencies"))

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// Service is materialised even before deps ready — Slot-20
			// behaviour preserved.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &corev1.Service{})).To(Succeed())
		})
	})

	Context("all three workloads", func() {
		BeforeEach(func() {
			store.Spec.Workloads.Admin = &magev1alpha1.WorkloadSpec{Replicas: 2}
			store.Spec.Workloads.Api = &magev1alpha1.WorkloadSpec{
				Replicas: 1,
				Autoscaling: &magev1alpha1.WorkloadAutoscaling{
					MaxReplicas:                    10,
					TargetCPUUtilizationPercentage: ptr.To[int32](70),
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("materialises three Deployments + three Services", func() {
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "hash1", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			for _, kind := range []string{"frontend", "admin", "api"} {
				name := "store-" + kind
				var dep appsv1.Deployment
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &dep)).To(Succeed())
				Expect(dep.Spec.Template.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", kind))
				Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(nginxConfigHashAnnotation, "hash1"))

				var svc corev1.Service
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &svc)).To(Succeed())
				Expect(svc.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/component", kind))
			}
		})

		It("emits an HPA only for api (the only autoscaling-declared workload)", func() {
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var hpa autoscalingv2.HorizontalPodAutoscaler
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-api", Namespace: namespace}, &hpa)).To(Succeed())
			Expect(hpa.Spec.ScaleTargetRef.Kind).To(Equal("Deployment"))
			Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal("store-api"))
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(10)))
			Expect(hpa.Spec.Metrics).To(HaveLen(1))
			Expect(hpa.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(ptr.To[int32](70)))

			// No HPA for frontend / admin (no autoscaling field).
			for _, kind := range []string{"frontend", "admin"} {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "store-" + kind, Namespace: namespace}, &autoscalingv2.HorizontalPodAutoscaler{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "kind %s should have no HPA", kind)
			}
		})
	})

	Context("orphan cleanup on transition", func() {
		// Initial reconcile with admin set — admin Deployment + Service
		// + HPA materialise. Then unset admin and reconcile again —
		// the three resources must be gone.
		BeforeEach(func() {
			store.Spec.Workloads.Admin = &magev1alpha1.WorkloadSpec{
				Replicas: 1,
				Autoscaling: &magev1alpha1.WorkloadAutoscaling{
					MaxReplicas: 5,
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("deletes admin Deployment + Service + HPA when spec.workloads.admin transitions to unset", func() {
			// First reconcile — admin resources exist.
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &corev1.Service{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &autoscalingv2.HorizontalPodAutoscaler{})).To(Succeed())

			// Unset admin and reconcile again.
			store.Spec.Workloads.Admin = nil
			_, err = reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &corev1.Service{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &autoscalingv2.HorizontalPodAutoscaler{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("deletes the HPA when spec.autoscaling transitions to unset", func() {
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &autoscalingv2.HorizontalPodAutoscaler{})).To(Succeed())

			store.Spec.Workloads.Admin.Autoscaling = nil
			_, err = reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &autoscalingv2.HorizontalPodAutoscaler{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// Deployment + Service must remain — only the HPA was unset.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-admin", Namespace: namespace}, &corev1.Service{})).To(Succeed())
		})
	})

	Context("SMTP envFrom mount", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("does NOT mount SMTP envFrom when spec.smtp is unset", func() {
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			for _, ef := range dep.Spec.Template.Spec.Containers[0].EnvFrom {
				if ef.SecretRef != nil {
					Expect(ef.SecretRef.Name).NotTo(HaveSuffix("-smtp"), "unexpected SMTP envFrom when spec.smtp is unset")
				}
			}
		})

		It("mounts <store>-smtp envFrom (without optional: true) when spec.smtp.inline is set", func() {
			store.Spec.SMTP = &magev1alpha1.SMTPSpec{
				Inline: &magev1alpha1.SMTPInline{
					Host: "smtp.example.com",
					From: "no-reply@example.com",
				},
			}
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())

			found := false
			for _, ef := range dep.Spec.Template.Spec.Containers[0].EnvFrom {
				if ef.SecretRef != nil && ef.SecretRef.Name == testInlineSMTPSecretName {
					found = true
					Expect(ef.SecretRef.Optional).To(BeNil(), "Optional must be nil (not true) when SMTPReady gates the mount")
				}
			}
			Expect(found).To(BeTrue(), "expected envFrom entry for store-smtp")
		})

		It("mounts the user-supplied Secret name when spec.smtp.secretRef is set", func() {
			store.Spec.SMTP = &magev1alpha1.SMTPSpec{
				SecretRef: &magev1alpha1.LocalObjectReference{Name: testSecretRefSMTPSecretName},
			}
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())

			found := false
			for _, ef := range dep.Spec.Template.Spec.Containers[0].EnvFrom {
				if ef.SecretRef != nil && ef.SecretRef.Name == testSecretRefSMTPSecretName {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected envFrom entry for my-smtp-secret")

			for _, ef := range dep.Spec.Template.Spec.Containers[0].EnvFrom {
				if ef.SecretRef != nil {
					Expect(ef.SecretRef.Name).NotTo(Equal(testInlineSMTPSecretName), "inline smtp name must not appear in secretRef mode")
				}
			}
		})

		It("sets smtp-secret-hash annotation when smtpSecretHash arg is non-empty", func() {
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "abc123hash", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(smtpSecretHashAnnotation, "abc123hash"))
		})

		It("does not set smtp-secret-hash annotation when smtpSecretHash is empty", func() {
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(smtpSecretHashAnnotation))
		})

		It("flips smtp-secret-hash annotation across reconciles, and prunes it on SMTP removal", func() {
			// Reconcile 1: hash A.
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "hashA", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(smtpSecretHashAnnotation, "hashA"))

			// Reconcile 2: hash B → annotation flips to B.
			_, err = reconciler.ensureWorkloads(ctx, store, true, true, "", "", "hashB", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(smtpSecretHashAnnotation, "hashB"))

			// Reconcile 3: hash empty (SMTP removed) → stale annotation pruned.
			_, err = reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(smtpSecretHashAnnotation))
		})
	})

	Context("media-seed initContainer (ADR 011)", func() {
		BeforeEach(func() {
			store.Spec.Workloads.Admin = &magev1alpha1.WorkloadSpec{Replicas: 1}
			store.Spec.Workloads.Api = &magev1alpha1.WorkloadSpec{Replicas: 1}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("attaches initContainer to all three workload Deployments when emptyDir + seedFromImage", func() {
			me := MediaEndpoint{
				Volume:        testMediaVolume(), // emptyDir
				SeedFromImage: true,
				EnvVars:       nil,
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			for _, kind := range []string{"frontend", "admin", "api"} {
				var dep appsv1.Deployment
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-" + kind, Namespace: namespace}, &dep)).To(Succeed())
				Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1), "kind %s: expected exactly one initContainer", kind)
				Expect(dep.Spec.Template.Spec.InitContainers[0].Name).To(Equal("media-seed"), "kind %s", kind)
			}
		})

		It("attaches initContainer in pvc mode + seedFromImage", func() {
			pvcVolume := corev1.Volume{
				Name: mediaVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "store-media",
					},
				},
			}
			me := MediaEndpoint{
				Volume:        pvcVolume,
				SeedFromImage: true,
				EnvVars:       nil,
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.InitContainers[0].Name).To(Equal("media-seed"))
		})

		It("does NOT attach initContainer in s3 mode (Job handles it)", func() {
			// EnvVars != nil signals S3 mode.
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: true,
				EnvVars:       map[string]string{"MAGENTO_MEDIA_STORAGE_ENDPOINT": "https://s3.example.com"},
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(BeEmpty())
		})

		It("does NOT attach initContainer when seedFromImage: false", func() {
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: false,
				EnvVars:       nil,
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			for _, kind := range []string{"frontend", "admin", "api"} {
				var dep appsv1.Deployment
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-" + kind, Namespace: namespace}, &dep)).To(Succeed())
				Expect(dep.Spec.Template.Spec.InitContainers).To(BeEmpty(), "kind %s: expected no initContainers", kind)
			}
		})

		It("initContainer command is `cp -rn /app/pub/media-baked/. /app/pub/media/`", func() {
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: true,
				EnvVars:       nil,
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			init := dep.Spec.Template.Spec.InitContainers[0]
			Expect(init.Command).To(Equal([]string{"sh", "-c"}))
			Expect(init.Args).To(Equal([]string{"cp -rn /app/pub/media-baked/. /app/pub/media/"}))
		})

		It("initContainer mounts the same media volume as the main container", func() {
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: true,
				EnvVars:       nil,
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			init := dep.Spec.Template.Spec.InitContainers[0]
			Expect(init.VolumeMounts).To(HaveLen(1))
			Expect(init.VolumeMounts[0].Name).To(Equal(mediaVolumeName))
			Expect(init.VolumeMounts[0].MountPath).To(Equal(mediaMountPath))
		})

		It("initContainer image matches the main container image", func() {
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: true,
				EnvVars:       nil,
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			mainImage := dep.Spec.Template.Spec.Containers[0].Image
			initImage := dep.Spec.Template.Spec.InitContainers[0].Image
			Expect(initImage).To(Equal(mainImage))
		})

		It("initContainer slice stays length 1 across two reconciles (no append churn)", func() {
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: true,
				EnvVars:       nil,
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			_, err = reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1), "re-reconcile must not grow the initContainers slice")
		})
	})

	Context("MediaSeeded gate (S3 mode, ADR 011 §C2.4)", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("returns WorkloadsReady=False, Reason=AwaitingMediaSeed when mediaReady=false", func() {
			// S3 + seedFromImage with the seed Job not yet succeeded ⇒
			// magestore_controller.go computes mediaReady=false and passes
			// it through. ensureWorkloads must short-circuit BEFORE emitting
			// any Deployment.
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: true,
				EnvVars:       map[string]string{"MAGENTO_MEDIA_STORAGE_ENDPOINT": "https://s3.example.com"},
			}
			cond, err := reconciler.ensureWorkloads(ctx, store, true, false, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionWorkloadsReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonAwaitingMediaSeed))

			// No Deployment was emitted — only the unconditional Service.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("proceeds to emit Deployments once mediaReady=true", func() {
			me := MediaEndpoint{
				Volume:        testMediaVolume(),
				SeedFromImage: true,
				EnvVars:       map[string]string{"MAGENTO_MEDIA_STORAGE_ENDPOINT": "https://s3.example.com"},
			}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", me)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
		})
	})
})

// ensureRouting's behaviour is the ADR 006 fallthrough table laid out
// as data — the test surface mirrors that table directly. Inline in
// the workloads suite per the brief's "tested inline in the workloads
// suite" note: routing shares the per-kind backend selection logic
// and is the cheapest sibling to test alongside.
//
// Two sibling resources land per call: a per-MageStore Gateway named
// `<store>-gateway` and an HTTPRoute named `<store>` that attaches via
// `parentRefs`. The HTTPRoute carries the fallthrough rules; the
// Gateway is fixture-shaped (single HTTP listener on port 80).
var _ = Describe("ensureRouting", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurerouting-"},
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
					Repository: "registry.example.com/test/magento",
					Tag:        "2.4.8",
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
	})

	It("emits a Gateway attached to the eg GatewayClass", func() {
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		var gw gatewayv1.Gateway
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-gateway", Namespace: namespace}, &gw)).To(Succeed())
		Expect(string(gw.Spec.GatewayClassName)).To(Equal("eg"))
		Expect(gw.Spec.Listeners).To(HaveLen(1))
		Expect(gw.Spec.Listeners[0].Port).To(Equal(gatewayv1.PortNumber(80)))
		Expect(gw.Spec.Listeners[0].Protocol).To(Equal(gatewayv1.HTTPProtocolType))
	})

	It("emits an HTTPRoute with no hostnames when spec.stores is unset", func() {
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		var rt gatewayv1.HTTPRoute
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, &rt)).To(Succeed())
		Expect(rt.Spec.Hostnames).To(BeEmpty())
		Expect(rt.Spec.Rules).To(HaveLen(4))
		Expect(rt.Spec.ParentRefs).To(HaveLen(1))
		Expect(string(rt.Spec.ParentRefs[0].Name)).To(Equal("store-gateway"))
	})

	It("emits hostnames matching spec.stores entries", func() {
		store.Spec.Stores = []magev1alpha1.StoreSpec{
			{Host: "de.example.com", Code: "store_de", Type: "website"},
			{Host: "fr.example.com", Code: "store_fr", Type: "website"},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		var rt gatewayv1.HTTPRoute
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, &rt)).To(Succeed())
		Expect(rt.Spec.Hostnames).To(HaveLen(2))
		Expect(string(rt.Spec.Hostnames[0])).To(Equal("de.example.com"))
		Expect(string(rt.Spec.Hostnames[1])).To(Equal("fr.example.com"))
		// Backends don't vary per host (per ADR 007); rules count is the same.
		Expect(rt.Spec.Rules).To(HaveLen(4))
	})

	It("falls back to <store>-frontend on /admin /rest /graphql when admin/api are unset", func() {
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		var rt gatewayv1.HTTPRoute
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, &rt)).To(Succeed())
		for _, rule := range rt.Spec.Rules {
			Expect(rule.BackendRefs).To(HaveLen(1))
			Expect(string(rule.BackendRefs[0].Name)).To(Equal("store-frontend"))
		}
	})

	It("routes /admin to <store>-admin and /rest /graphql to <store>-api when both are declared", func() {
		store.Spec.Workloads.Admin = &magev1alpha1.WorkloadSpec{Replicas: 1}
		store.Spec.Workloads.Api = &magev1alpha1.WorkloadSpec{Replicas: 1}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		var rt gatewayv1.HTTPRoute
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, &rt)).To(Succeed())
		backends := map[string]string{}
		for _, rule := range rt.Spec.Rules {
			Expect(rule.Matches).To(HaveLen(1))
			Expect(rule.Matches[0].Path).NotTo(BeNil())
			Expect(rule.Matches[0].Path.Value).NotTo(BeNil())
			backends[*rule.Matches[0].Path.Value] = string(rule.BackendRefs[0].Name)
		}
		Expect(backends).To(HaveKeyWithValue("/admin", "store-admin"))
		Expect(backends).To(HaveKeyWithValue("/rest", "store-api"))
		Expect(backends).To(HaveKeyWithValue("/graphql", "store-api"))
		Expect(backends).To(HaveKeyWithValue("/", "store-frontend"))
	})

	It("routes /rest /graphql to <store>-frontend when only admin is declared (api fallthrough)", func() {
		store.Spec.Workloads.Admin = &magev1alpha1.WorkloadSpec{Replicas: 1}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		var rt gatewayv1.HTTPRoute
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, &rt)).To(Succeed())
		backends := map[string]string{}
		for _, rule := range rt.Spec.Rules {
			backends[*rule.Matches[0].Path.Value] = string(rule.BackendRefs[0].Name)
		}
		Expect(backends).To(HaveKeyWithValue("/admin", "store-admin"))
		Expect(backends).To(HaveKeyWithValue("/rest", "store-frontend"))
		Expect(backends).To(HaveKeyWithValue("/graphql", "store-frontend"))
		Expect(backends).To(HaveKeyWithValue("/", "store-frontend"))
	})

	It("is idempotent — second reconcile must not bump HTTPRoute ResourceVersion", func() {
		// Regression test for the Gateway API CRD-defaulting churn
		// observed in Slot 22.5: ParentRef.Group/Kind, BackendRef.Group/
		// Kind, and HTTPBackendRef.Weight are server-side-defaulted by
		// the Gateway API CRD's OpenAPI schema. The mutator must set
		// these explicitly so CreateOrUpdate's diff sees no churn on a
		// stable spec.
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())
		var rt1 gatewayv1.HTTPRoute
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, &rt1)).To(Succeed())
		rv1 := rt1.ResourceVersion

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())
		var rt2 gatewayv1.HTTPRoute
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, &rt2)).To(Succeed())
		Expect(rt2.ResourceVersion).To(Equal(rv1), "second reconcile bumped HTTPRoute resourceVersion — Gateway API field defaulting is causing churn")
	})

	// imagePullPolicy on long-running workload Deployments. v0.6.3 #1
	// fix: defends against stale node-cached layers when the reference
	// image's mutable tag is rebuilt across releases. CRD default is
	// `Always`; an explicit override on `spec.workloads.imagePullPolicy`
	// passes through verbatim. The empty fallback in
	// workloadImagePullPolicy() covers test fixtures that bypass CRD
	// defaulting (set the field directly via the typed Go struct).
	Context("imagePullPolicy on workload Deployments (v0.6.3 #1)", func() {
		It("defaults to Always on the magento container when spec.workloads.imagePullPolicy is unset", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullAlways))
		})

		It("propagates an explicit IfNotPresent override to the magento container", func() {
			store.Spec.Workloads.ImagePullPolicy = corev1.PullIfNotPresent
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))
		})

		It("applies the same policy to the media-seed initContainer (emptyDir + seedFromImage)", func() {
			store.Spec.Workloads.ImagePullPolicy = corev1.PullIfNotPresent
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			seedingEndpoint := MediaEndpoint{Volume: testMediaVolume(), SeedFromImage: true}
			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", seedingEndpoint)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-frontend", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.InitContainers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))
		})

		It("applies the policy to admin and api workloads as well as frontend", func() {
			store.Spec.Workloads.Admin = &magev1alpha1.WorkloadSpec{Replicas: 1}
			store.Spec.Workloads.Api = &magev1alpha1.WorkloadSpec{Replicas: 1}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureWorkloads(ctx, store, true, true, "", "", "", testMediaEndpoint())
			Expect(err).NotTo(HaveOccurred())

			for _, name := range []string{"store-frontend", "store-admin", "store-api"} {
				var dep appsv1.Deployment
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &dep)).To(Succeed(), name)
				Expect(dep.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullAlways), name)
			}
		})
	})
})

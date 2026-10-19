/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
	"github.com/aknostic/mage-k8s-operator/internal/magento"
)

// ensureConsumers covers the fan-out, the runtime gates (queue must
// be configured, dependencies must be ready), the override-
// validation path, and orphan sweep on toggle-off. The full known-
// consumer list (`magento.Consumers`) drives every materialise
// pass, so envtest asserts the count and spot-checks one consumer's
// Deployment shape rather than iterating 20 names.
var _ = Describe("ensureConsumers", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensureconsumers-"},
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
				// Queue stub — required for ensureConsumers's
				// QueueNotConfigured gate to pass. External mode needs
				// no CR, minimal in the envtest.
				Queue: &magev1alpha1.QueueSpec{
					External: &magev1alpha1.ExternalQueue{
						Host:                 "mq.example.com",
						Port:                 5672,
						Vhost:                "/",
						CredentialsSecretRef: magev1alpha1.LocalObjectReference{Name: "mq-creds"},
					},
				},
			},
		}
	})

	Context("when spec.consumers is unset", func() {
		It("returns Unknown/NotEnabled", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("NotEnabled"))
		})
	})

	Context("when enabled: true but spec.queue is unset", func() {
		It("returns False/QueueNotConfigured", func() {
			store.Spec.Queue = nil
			store.Spec.Consumers = &magev1alpha1.ConsumersSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("QueueNotConfigured"))
		})
	})

	Context("when enabled with valid overrides", func() {
		BeforeEach(func() {
			store.Spec.Consumers = &magev1alpha1.ConsumersSpec{
				Enabled: true,
				Overrides: map[string]magev1alpha1.ConsumerOverride{
					"product_action_attribute.update": {
						Resources: &corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("materialises one Deployment per known consumer with grace period and override applied", func() {
			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			// One Deployment per entry in the canonical list.
			var deps appsv1.DeploymentList
			Expect(k8sClient.List(ctx, &deps, &client.ListOptions{Namespace: namespace})).To(Succeed())
			consumerDeps := 0
			for _, d := range deps.Items {
				if d.Labels["app.kubernetes.io/component"] == "consumer" {
					consumerDeps++
				}
			}
			Expect(consumerDeps).To(Equal(len(magento.Consumers)))

			// Spot-check: the override consumer's Deployment has the
			// overridden memory limit and the 60s grace period.
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-product-action-attribute-update", Namespace: namespace}, &dep)).To(Succeed())
			Expect(*dep.Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(60)))
			Expect(dep.Spec.Template.Spec.Containers[0].Resources.Limits).To(HaveKey(corev1.ResourceMemory))
			Expect(dep.Spec.Template.Spec.Containers[0].Args).To(ContainElements("queue:consumers:start", "product_action_attribute.update"))
		})

		It("mounts the media volume at /app/pub/media so the image-resize consumer can stage", func() {
			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-product-action-attribute-update", Namespace: namespace}, &dep)).To(Succeed())

			Expect(dep.Spec.Template.Spec.Volumes).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Volumes[0].Name).To(Equal(mediaVolumeName))
			Expect(dep.Spec.Template.Spec.Volumes[0].EmptyDir).NotTo(BeNil())

			Expect(dep.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(corev1.VolumeMount{
				Name:      mediaVolumeName,
				MountPath: mediaMountPath,
			}))
		})

		It("wraps with `timeout 3600` and uses --max-messages (no --max-time, which does not exist in Magento 2.4)", func() {
			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var deps appsv1.DeploymentList
			Expect(k8sClient.List(ctx, &deps,
				client.InNamespace(namespace),
				client.MatchingLabels{"app.kubernetes.io/component": "consumer"},
			)).To(Succeed())
			Expect(deps.Items).NotTo(BeEmpty())

			for _, dep := range deps.Items {
				c := dep.Spec.Template.Spec.Containers[0]
				Expect(c.Command).To(Equal([]string{"timeout", "3600", "bin/magento"}))
				Expect(c.Args).To(ContainElement("--max-messages=20000"))
				Expect(c.Args).NotTo(ContainElement("--max-time=3600"),
					"--max-time is not a valid bin/magento queue:consumers:start flag — wall-clock bound is enforced via coreutils `timeout` in Command")
			}
		})
	})

	Context("when overrides contain an unknown key", func() {
		It("returns False/UnknownConsumerOverride but still materialises the known Deployments", func() {
			store.Spec.Consumers = &magev1alpha1.ConsumersSpec{
				Enabled: true,
				Overrides: map[string]magev1alpha1.ConsumerOverride{
					"not.a.real.consumer": {},
					"async.operations.all": {
						Resources: &corev1.ResourceRequirements{
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("UnknownConsumerOverride"))
			Expect(cond.Message).To(ContainSubstring("not.a.real.consumer"))

			// But the known Deployments still exist — unknown key is a
			// user warning, not a fatal error.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())
		})
	})

	Context("transition from enabled to unset", func() {
		It("deletes all consumer Deployments", func() {
			store.Spec.Consumers = &magev1alpha1.ConsumersSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())

			store.Spec.Consumers = nil
			_, err = reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("SMTP envFrom mount", func() {
		BeforeEach(func() {
			store.Spec.Consumers = &magev1alpha1.ConsumersSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("does NOT mount SMTP envFrom when spec.smtp is unset", func() {
			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &dep)).To(Succeed())
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

			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &dep)).To(Succeed())

			found := false
			for _, ef := range dep.Spec.Template.Spec.Containers[0].EnvFrom {
				if ef.SecretRef != nil && ef.SecretRef.Name == testInlineSMTPSecretName {
					found = true
					Expect(ef.SecretRef.Optional).To(BeNil())
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

			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &dep)).To(Succeed())

			found := false
			for _, ef := range dep.Spec.Template.Spec.Containers[0].EnvFrom {
				if ef.SecretRef != nil && ef.SecretRef.Name == testSecretRefSMTPSecretName {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected envFrom entry for my-smtp-secret")
		})

		It("sets smtp-secret-hash annotation when smtpSecretHash arg is non-empty", func() {
			_, err := reconciler.ensureConsumers(ctx, store, true, "abcdef12", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Annotations).To(HaveKeyWithValue(smtpSecretHashAnnotation, "abcdef12"))
		})

		It("does not set smtp-secret-hash annotation when smtpSecretHash is empty", func() {
			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-consumer-async-operations-all", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(smtpSecretHashAnnotation))
		})
	})

	// imagePullPolicy on consumer Deployments. Consumer pods restart
	// often (timeout 3600 + max-messages bound), so a stale node-cached
	// image bites them harder than the workload pods — par-3's stale
	// :sampledata cache exit-looped 7 of 8 consumer pods on the v0.6.2
	// validation. v0.6.3 #1 makes Always the default.
	Context("imagePullPolicy on consumer Deployments (v0.6.3 #1)", func() {
		BeforeEach(func() {
			store.Spec.Consumers = &magev1alpha1.ConsumersSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("defaults to Always on every consumer container when spec.workloads.imagePullPolicy is unset", func() {
			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var depList appsv1.DeploymentList
			Expect(k8sClient.List(ctx, &depList, client.InNamespace(namespace))).To(Succeed())
			Expect(depList.Items).NotTo(BeEmpty())
			for _, dep := range depList.Items {
				Expect(dep.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullAlways), dep.Name)
				expectAdminFrontnameEnv(dep.Spec.Template.Spec.Containers[0], store.Name)
			}
		})

		It("propagates an explicit IfNotPresent override to every consumer container", func() {
			store.Spec.Workloads.ImagePullPolicy = corev1.PullIfNotPresent
			Expect(k8sClient.Update(ctx, store)).To(Succeed())

			_, err := reconciler.ensureConsumers(ctx, store, true, "", emptyDirMediaVolume())
			Expect(err).NotTo(HaveOccurred())

			var depList appsv1.DeploymentList
			Expect(k8sClient.List(ctx, &depList, client.InNamespace(namespace))).To(Succeed())
			Expect(depList.Items).NotTo(BeEmpty())
			for _, dep := range depList.Items {
				Expect(dep.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent), dep.Name)
			}
		})
	})
})

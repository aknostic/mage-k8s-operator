/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// ensureCron covers the fan-out + disabled-override + unknown-group
// paths. Resource-default merging is the one knob worth a dedicated
// assertion — `index` gets a 2Gi limit while the rest share the
// cautious default.
var _ = Describe("ensureCron", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurecron-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Capabilities: fullCapabilities()}
		store = &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: namespace},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.gitlab.aknostic.com/test/magento",
					Tag:        "2.4.8",
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
	})

	It("emits one CronJob per group with index getting the 2Gi memory limit", func() {
		store.Spec.Cron = &magev1alpha1.CronSpec{Enabled: true}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		cond, err := reconciler.ensureCron(ctx, store, true, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))

		for _, group := range []string{"default", "index", "consumers", "staging", "catalog-event"} {
			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-" + group, Namespace: namespace}, &cj)).To(Succeed())
			Expect(cj.Spec.Schedule).To(Equal("* * * * *"))
			Expect(cj.Spec.ConcurrencyPolicy).To(Equal(batchv1.AllowConcurrent))
		}

		var indexJob batchv1.CronJob
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-index", Namespace: namespace}, &indexJob)).To(Succeed())
		memLimit := indexJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Resources.Limits.Memory()
		Expect(memLimit.String()).To(Equal("2Gi"))
	})

	It("skips groups with disabled: true override", func() {
		store.Spec.Cron = &magev1alpha1.CronSpec{
			Enabled: true,
			Overrides: map[string]magev1alpha1.CronOverride{
				"index": {Disabled: true},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		_, err := reconciler.ensureCron(ctx, store, true, "")
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-index", Namespace: namespace}, &batchv1.CronJob{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-default", Namespace: namespace}, &batchv1.CronJob{})).To(Succeed())
	})

	It("flags unknown override keys with UnknownCronGroupOverride", func() {
		store.Spec.Cron = &magev1alpha1.CronSpec{
			Enabled: true,
			Overrides: map[string]magev1alpha1.CronOverride{
				"not-a-group": {},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

		cond, err := reconciler.ensureCron(ctx, store, true, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("UnknownCronGroupOverride"))
	})

	Context("SMTP envFrom mount", func() {
		BeforeEach(func() {
			store.Spec.Cron = &magev1alpha1.CronSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("does NOT mount SMTP envFrom when spec.smtp is unset", func() {
			_, err := reconciler.ensureCron(ctx, store, true, "")
			Expect(err).NotTo(HaveOccurred())

			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-default", Namespace: namespace}, &cj)).To(Succeed())
			for _, ef := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].EnvFrom {
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

			_, err := reconciler.ensureCron(ctx, store, true, "")
			Expect(err).NotTo(HaveOccurred())

			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-default", Namespace: namespace}, &cj)).To(Succeed())

			found := false
			for _, ef := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].EnvFrom {
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

			_, err := reconciler.ensureCron(ctx, store, true, "")
			Expect(err).NotTo(HaveOccurred())

			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-default", Namespace: namespace}, &cj)).To(Succeed())

			found := false
			for _, ef := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].EnvFrom {
				if ef.SecretRef != nil && ef.SecretRef.Name == testSecretRefSMTPSecretName {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "expected envFrom entry for my-smtp-secret")
		})

		It("sets smtp-secret-hash annotation when smtpSecretHash arg is non-empty", func() {
			_, err := reconciler.ensureCron(ctx, store, true, "cronhash99")
			Expect(err).NotTo(HaveOccurred())

			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-default", Namespace: namespace}, &cj)).To(Succeed())
			Expect(cj.Spec.JobTemplate.Spec.Template.Annotations).To(HaveKeyWithValue(smtpSecretHashAnnotation, "cronhash99"))
		})

		It("does not set smtp-secret-hash annotation when smtpSecretHash is empty", func() {
			_, err := reconciler.ensureCron(ctx, store, true, "")
			Expect(err).NotTo(HaveOccurred())

			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-cron-default", Namespace: namespace}, &cj)).To(Succeed())
			Expect(cj.Spec.JobTemplate.Spec.Template.Annotations).NotTo(HaveKey(smtpSecretHashAnnotation))
		})
	})
})

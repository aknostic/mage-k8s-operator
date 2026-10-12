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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// ensureVarnish covers the two VCL paths (operator-generated default
// when vclConfigMapRef is nil, user-supplied when it is set), the
// missing-ConfigMap / missing-key user-error paths, and the orphan-
// cleanup sweep on disable. No condition is surfaced for absent
// spec.varnish (Unknown/NotEnabled), so Ready never blocks on a store
// that chose not to run Varnish.
var _ = Describe("ensureVarnish", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurevarnish-"},
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
					Repository: "registry.gitlab.aknostic.com/test/magento",
					Tag:        "2.4.8",
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
	})

	Context("when spec.varnish is unset", func() {
		It("returns Unknown/NotEnabled and creates no resources", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("NotEnabled"))

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("when enabled: true without vclConfigMapRef", func() {
		It("generates a default VCL ConfigMap and materialises the Deployment", func() {
			store.Spec.Varnish = &magev1alpha1.VarnishSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish-vcl", Namespace: namespace}, &cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("default.vcl"))
			Expect(cm.Data["default.vcl"]).To(ContainSubstring(`.host = "store-frontend"`))
			// Single-store: no explicit hash_data(req.http.host) per ADR 007.
			Expect(cm.Data["default.vcl"]).NotTo(ContainSubstring("hash_data(req.http.host)"))
			Expect(cm.OwnerReferences).To(HaveLen(1))
			Expect(cm.OwnerReferences[0].Name).To(Equal("store"))

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("store-varnish-vcl"))
		})

		It("embeds the ADR-007 multi-store hash key when spec.stores is set", func() {
			store.Spec.Varnish = &magev1alpha1.VarnishSpec{Enabled: true}
			store.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "de.example.com", Code: "store_de", Type: "website"},
				{Host: "fr.example.com", Code: "store_fr", Type: "website"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish-vcl", Namespace: namespace}, &cm)).To(Succeed())
			Expect(cm.Data["default.vcl"]).To(ContainSubstring("hash_data(req.http.host)"))
			Expect(cm.Data["default.vcl"]).To(ContainSubstring("sub vcl_hash"))
		})

		It("sweeps the default VCL ConfigMap on disable", func() {
			store.Spec.Varnish = &magev1alpha1.VarnishSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish-vcl", Namespace: namespace}, &corev1.ConfigMap{})).To(Succeed())

			store.Spec.Varnish = nil
			_, err = reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish-vcl", Namespace: namespace}, &corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("when vclConfigMapRef points at a missing ConfigMap", func() {
		It("returns False/VCLConfigMapMissing", func() {
			store.Spec.Varnish = &magev1alpha1.VarnishSpec{
				Enabled:         true,
				VclConfigMapRef: &magev1alpha1.LocalObjectReference{Name: "missing-vcl"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("VCLConfigMapMissing"))
		})
	})

	Context("when vclConfigMapRef is set + ConfigMap exists with default.vcl key", func() {
		BeforeEach(func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "store-vcl", Namespace: namespace},
				Data:       map[string]string{"default.vcl": "vcl 4.1;\nbackend default { .host = \"store-frontend\"; .port = \"8080\"; }"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			store.Spec.Varnish = &magev1alpha1.VarnishSpec{
				Enabled:         true,
				Replicas:        2,
				Version:         "7.5",
				VclConfigMapRef: &magev1alpha1.LocalObjectReference{Name: "store-vcl"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("materialises Deployment + Service with VCL mounted + hash annotation", func() {
			_, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
			Expect(dep.Spec.Template.Annotations).To(HaveKey(varnishVCLHashAnnotation))
			Expect(dep.Spec.Template.Annotations[varnishVCLHashAnnotation]).NotTo(BeEmpty())
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("docker.io/library/varnish:7.5"))
			Expect(dep.Spec.Template.Spec.Volumes).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Volumes[0].ConfigMap.Name).To(Equal("store-vcl"))

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
		})

		It("transitions to unset → deletes Deployment + Service", func() {
			_, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())

			store.Spec.Varnish = nil
			_, err = reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &corev1.Service{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	// imagePullPolicy on the Varnish Deployment. Varnish ships from
	// the same registry as Magento (separate image but same registry
	// surface) and inherits the same stale-cache failure mode. v0.6.3
	// #1 makes Always the default; spec.workloads.imagePullPolicy
	// applies even though Varnish is technically a sibling not a
	// workload — keeping a single knob for "any long-running
	// operator-owned pod" avoids a second field that would always be
	// set in lockstep with the first.
	Context("imagePullPolicy on the Varnish Deployment (v0.6.3 #1)", func() {
		It("defaults to Always when spec.workloads.imagePullPolicy is unset", func() {
			store.Spec.Varnish = &magev1alpha1.VarnishSpec{Enabled: true}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullAlways))
		})

		It("propagates an explicit IfNotPresent override to the Varnish container", func() {
			store.Spec.Varnish = &magev1alpha1.VarnishSpec{Enabled: true}
			store.Spec.Workloads.ImagePullPolicy = corev1.PullIfNotPresent
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, err := reconciler.ensureVarnish(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-varnish", Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))
		})
	})
})

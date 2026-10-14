/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ensureSearch external mode mirrors ensureCacheExternal: an unmanaged
// endpoint with optional authentication. In-cluster Magento OpenSearch
// is plaintext no-auth (the inline-mode default), so the canonical
// no-auth case is supported. Auth-required managed services (AWS
// OpenSearch Service, Bonsai) thread credentials through a Secret. The
// inline / ref readiness paths are exercised in
// magestore_controller_test.go end-to-end; this suite focuses on the
// external branch added in v0.1.x for the Phase-2 dependency-
// substitution gap.
var _ = Describe("ensureSearch external mode", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensuresearch-"},
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
			},
		}
	})

	It("returns Ready and emits no auth credentials when CredentialsSecretRef is unset", func() {
		store.Spec.Search = &magev1alpha1.SearchSpec{
			External: &magev1alpha1.ExternalSearch{
				Host: "opensearch.example.com",
				Port: 9200,
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		cond, endpoint, err := reconciler.ensureSearch(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Type).To(Equal(ConditionSearchReady))
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("ExternalReady"))
		Expect(cond.Message).To(ContainSubstring("no-auth"))
		Expect(endpoint.Host).To(Equal("opensearch.example.com"))
		Expect(endpoint.Port).To(Equal(int32(9200)))
		Expect(endpoint.Username).To(BeEmpty())
		Expect(endpoint.Password).To(BeEmpty())
	})

	It("defaults port to 9200 when unset", func() {
		store.Spec.Search = &magev1alpha1.SearchSpec{
			External: &magev1alpha1.ExternalSearch{
				Host: "opensearch.example.com",
				// Port intentionally zero — covers the "user bypassed
				// CRD defaulting via apply patch" path. Production traffic
				// routes through CRD defaulting (port=9200) so they hit
				// the same constant.
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		_, endpoint, err := reconciler.ensureSearch(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Port).To(Equal(int32(9200)))
	})

	It("returns Ready with credentials when CredentialsSecretRef resolves cleanly", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ext-search-creds", Namespace: namespace},
			Data: map[string][]byte{
				defaultExternalSearchUsernameKey: []byte("magento"),
				defaultExternalSearchPasswordKey: []byte("hunter2"),
			},
		})).To(Succeed())
		store.Spec.Search = &magev1alpha1.SearchSpec{
			External: &magev1alpha1.ExternalSearch{
				Host: "search.example.com",
				Port: 443,
				CredentialsSecretRef: &magev1alpha1.LocalObjectReference{
					Name: "ext-search-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		cond, endpoint, err := reconciler.ensureSearch(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("ExternalReady"))
		Expect(cond.Message).To(ContainSubstring("AUTH"))
		Expect(endpoint.Username).To(Equal("magento"))
		Expect(endpoint.Password).To(Equal("hunter2"))
	})

	It("returns CredentialSecretMissing when CredentialsSecretRef points at a non-existent Secret", func() {
		store.Spec.Search = &magev1alpha1.SearchSpec{
			External: &magev1alpha1.ExternalSearch{
				Host: "search.example.com",
				CredentialsSecretRef: &magev1alpha1.LocalObjectReference{
					Name: "missing",
				},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		cond, endpoint, err := reconciler.ensureSearch(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("CredentialSecretMissing"))
		Expect(endpoint).To(Equal(SearchEndpoint{}))
	})

	It("returns CredentialKeyEmpty when the Secret is missing the password key", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "partial-creds", Namespace: namespace},
			Data: map[string][]byte{
				defaultExternalSearchUsernameKey: []byte("magento"),
				// password key intentionally missing
			},
		})).To(Succeed())
		store.Spec.Search = &magev1alpha1.SearchSpec{
			External: &magev1alpha1.ExternalSearch{
				Host: "search.example.com",
				CredentialsSecretRef: &magev1alpha1.LocalObjectReference{
					Name: "partial-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())

		cond, _, err := reconciler.ensureSearch(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Reason).To(Equal("CredentialKeyEmpty"))
	})
})

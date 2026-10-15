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

// Capabilities-driven graceful degradation: when the operator boots
// against a cluster missing one or more dependency operator CRDs, each
// affected ensure* step surfaces DependencyOperatorMissing instead of
// crashing on a missing kind. External-mode siblings are unaffected
// because they don't touch any dep CRD.
var _ = Describe("Capabilities-driven dependency degradation", func() {
	var (
		store     *magev1alpha1.MageStore
		namespace string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "capabilities-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

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

	Context("DependencyOperatorMissing surfaces for each data tier when its CRD is absent", func() {
		It("ensureDatabase reports MariaDB operator missing on inline mode without crashing", func() {
			r := &MageStoreReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Capabilities: Capabilities{},
			}
			store.Spec.Database = &magev1alpha1.DatabaseSpec{
				MariaDb: &magev1alpha1.InlineMariaDB{Version: "11.4", StorageSize: "1Gi"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())

			cond, endpoint, err := r.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionDatabaseReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonDependencyOperatorMissing))
			Expect(cond.Message).To(ContainSubstring("mariadb-operator"))
			Expect(endpoint).To(Equal(DatabaseEndpoint{}))
		})

		It("ensureDatabase reports MariaDB operator missing on ref mode", func() {
			r := &MageStoreReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Capabilities: Capabilities{},
			}
			store.Spec.Database = &magev1alpha1.DatabaseSpec{
				MariaDbRef: &magev1alpha1.LocalObjectReference{Name: "shared-mariadb"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())

			cond, _, err := r.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal(ReasonDependencyOperatorMissing))
		})

		It("ensureSearch reports OpenSearch operator missing on inline mode", func() {
			r := &MageStoreReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Capabilities: Capabilities{},
			}
			store.Spec.Search = &magev1alpha1.SearchSpec{
				OpenSearch: &magev1alpha1.InlineOpenSearch{Version: "2.19.2", Replicas: 3, JvmHeap: "512m"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())

			cond, _, err := r.ensureSearch(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionSearchReady))
			Expect(cond.Reason).To(Equal(ReasonDependencyOperatorMissing))
			Expect(cond.Message).To(ContainSubstring("opensearch-k8s-operator"))
		})

		// Note: ensureCache no longer has a CRD dependency for inline /
		// ref modes — ADR 009 removed the upstream Valkey operator dep
		// in favour of an operator-owned StatefulSet. The cache tier
		// degrades only at runtime via StatefulSet readiness, not via
		// DependencyOperatorMissing.

		It("ensureQueue reports rabbitmq-cluster-operator missing on inline mode", func() {
			r := &MageStoreReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Capabilities: Capabilities{},
			}
			store.Spec.Queue = &magev1alpha1.QueueSpec{
				RabbitMq: &magev1alpha1.InlineRabbitMQ{},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())

			cond, _, err := r.ensureQueue(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionQueueReady))
			Expect(cond.Reason).To(Equal(ReasonDependencyOperatorMissing))
			Expect(cond.Message).To(ContainSubstring("rabbitmq-cluster-operator"))
		})
	})

	Context("external-mode siblings are unaffected by missing CRDs", func() {
		It("ensureDatabase external mode returns Ready when caps are zeroed but the credentials Secret is present", func() {
			r := &MageStoreReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Capabilities: Capabilities{},
			}
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "ext-db-creds", Namespace: namespace},
				Data: map[string][]byte{
					externalUsernameSecretKey: []byte("magento"),
					externalPasswordSecretKey: []byte("hunter2"),
				},
			})).To(Succeed())
			store.Spec.Database = &magev1alpha1.DatabaseSpec{
				External: &magev1alpha1.ExternalDatabase{
					Host: "db.example.com",
					Port: 3306,
					CredentialsSecretRef: magev1alpha1.LocalObjectReference{
						Name: "ext-db-creds",
					},
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())

			cond, endpoint, err := r.ensureDatabase(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ExternalReady"))
			Expect(endpoint.Host).To(Equal("db.example.com"))
			Expect(endpoint.Username).To(Equal("magento"))
		})

		It("ensureCache external mode (password-less) returns Ready when caps are zeroed", func() {
			r := &MageStoreReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				Capabilities: Capabilities{},
			}
			store.Spec.Cache = &magev1alpha1.CacheSpec{
				External: &magev1alpha1.ExternalCache{Host: "redis.example.com", Port: 6379},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())

			cond, endpoint, err := r.ensureCache(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ExternalReady"))
			Expect(endpoint.Host).To(Equal("redis.example.com"))
		})
	})
})

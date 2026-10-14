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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ensureNginxConfig generates the `<store>-nginx-conf` ConfigMap from
// `spec.stores` and returns a hash of the data so workload pod
// templates can rollout-on-change. Three things matter:
//
//  1. The CRD→nginx content generator produces the right `map`
//     directives for each spec.stores shape.
//  2. The ConfigMap exists with both keys (`stores.conf`,
//     `default.conf`) and an ownerReference back to the MageStore.
//  3. The hash changes when content changes (drives rollout-on-
//     change) and is stable across map iteration order (so
//     reconciles produce the same hash for unchanged content).
//
// The mount target is deferred to Session 3 — these tests don't try to
// validate "the workload pod actually mounts the ConfigMap." See
// ensure_nginx_config.go header.
var _ = Describe("ensureNginxConfig", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurenginxconfig-"},
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

	Context("when spec.stores is unset", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("creates the ConfigMap with empty-default map directives so $MAGE_RUN_CODE is declared", func() {
			hash, err := reconciler.ensureNginxConfig(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(hash).NotTo(BeEmpty())

			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-nginx-conf", Namespace: namespace}, &cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("stores.conf"))
			Expect(cm.Data).To(HaveKey("default.conf"))
			// Session 3: always emit the map directives even when
			// spec.stores is unset, because the image's nginx.conf
			// references $MAGE_RUN_CODE via fastcgi_param and nginx
			// errors on undeclared variables. Only the default clause
			// appears — no per-host entries.
			Expect(cm.Data["stores.conf"]).To(ContainSubstring("map $http_host $MAGE_RUN_CODE"))
			Expect(cm.Data["stores.conf"]).To(ContainSubstring("map $http_host $MAGE_RUN_TYPE"))
			Expect(cm.Data["stores.conf"]).To(ContainSubstring("default \"\";"))

			// Owner reference back to the MageStore.
			Expect(cm.OwnerReferences).To(HaveLen(1))
			Expect(cm.OwnerReferences[0].Name).To(Equal("store"))
			Expect(cm.OwnerReferences[0].Kind).To(Equal("MageStore"))
		})
	})

	Context("when spec.stores has multiple hosts", func() {
		BeforeEach(func() {
			store.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "de.example.com", Code: "store_de", Type: "website"},
				{Host: "fr.example.com", Code: "store_fr", Type: "website"},
				{Host: "en.example.com", Code: "store_en", Type: "store_view"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("emits both map directives with one entry per host plus a default", func() {
			_, err := reconciler.ensureNginxConfig(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-nginx-conf", Namespace: namespace}, &cm)).To(Succeed())

			conf := cm.Data["stores.conf"]
			Expect(conf).To(ContainSubstring("map $http_host $MAGE_RUN_CODE {"))
			Expect(conf).To(ContainSubstring("map $http_host $MAGE_RUN_TYPE {"))
			Expect(conf).To(ContainSubstring("    de.example.com store_de;"))
			Expect(conf).To(ContainSubstring("    fr.example.com store_fr;"))
			Expect(conf).To(ContainSubstring("    en.example.com store_en;"))
			Expect(conf).To(ContainSubstring("    en.example.com store_view;"))
			// Unknown hosts → empty string fall-back per ADR 007 decision 5.
			Expect(conf).To(ContainSubstring("    default \"\";"))
		})
	})

	Context("hash stability and change detection", func() {
		It("produces the same hash for unchanged spec.stores across reconciles", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			h1, err := reconciler.ensureNginxConfig(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			h2, err := reconciler.ensureNginxConfig(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(h1).To(Equal(h2))
		})

		It("produces a different hash when spec.stores changes", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			hashEmpty, err := reconciler.ensureNginxConfig(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			store.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "de.example.com", Code: "store_de", Type: "website"},
			}
			hashOne, err := reconciler.ensureNginxConfig(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(hashOne).NotTo(Equal(hashEmpty))

			store.Spec.Stores = append(store.Spec.Stores, magev1alpha1.StoreSpec{
				Host: "fr.example.com", Code: "store_fr", Type: "website",
			})
			hashTwo, err := reconciler.ensureNginxConfig(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(hashTwo).NotTo(Equal(hashOne))
		})
	})
})

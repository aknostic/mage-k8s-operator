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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// Validation that lives in the CRD schema (kubebuilder markers), checked
// against the envtest API server.
var _ = Describe("MageStore CRD validation", func() {
	storeWithOpenSearchReplicas := func(name string, replicas int32) *magev1alpha1.MageStore {
		return &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/test/magento",
					Tag:        "2.4.8",
				},
				Search: &magev1alpha1.SearchSpec{
					OpenSearch: &magev1alpha1.InlineOpenSearch{Version: "2.19.2", Replicas: replicas, JvmHeap: "512m"},
				},
			},
		}
	}

	It("rejects spec.search.openSearch.replicas: 1 (issue #21)", func() {
		err := k8sClient.Create(ctx, storeWithOpenSearchReplicas("opensearch-one-node", 1))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("issue #21"))
	})

	It("accepts spec.search.openSearch.replicas: 3", func() {
		store := storeWithOpenSearchReplicas("opensearch-three-nodes", 3)
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Delete(ctx, store)).To(Succeed())
	})
})

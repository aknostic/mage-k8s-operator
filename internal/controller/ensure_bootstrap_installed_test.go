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

// ensure_bootstrap_installed_test.go covers the "never run setup:install
// twice" guarantee of ensureBootstrap via envtest: once StoreInstalled is
// True, a dependency outage must not flip it back, and no new bootstrap
// Job may be created — even after the completed Job has been
// garbage-collected.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

var _ = Describe("ensureBootstrap on an installed store — envtest", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bootstrap-installed-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Capabilities: fullCapabilities(),
		}

		store = newMinimalMageStore(namespace)
		store.Spec.Bootstrap = &magev1alpha1.BootstrapSpec{
			Admin: magev1alpha1.AdminSpec{Email: "admin@example.com"},
		}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
	})

	bootstrapJobs := func() []batchv1.Job {
		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace))).To(Succeed())
		return jobs.Items
	}

	markInstalled := func() {
		meta.SetStatusCondition(&store.Status.Conditions, metav1.Condition{
			Type:    ConditionStoreInstalled,
			Status:  metav1.ConditionTrue,
			Reason:  "Installed",
			Message: "setup:install completed",
		})
	}

	It("keeps StoreInstalled=True and creates no Job when a dependency goes not-ready", func() {
		markInstalled()

		cond, err := reconciler.ensureBootstrap(ctx, store, false, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Type).To(Equal(ConditionStoreInstalled))
		Expect(cond.Status).To(Equal(metav1.ConditionTrue),
			"a dependency outage must not overwrite StoreInstalled=True")
		Expect(bootstrapJobs()).To(BeEmpty())
	})

	It("creates no Job when dependencies recover after the completed Job was garbage-collected", func() {
		markInstalled()

		// Outage, then recovery. No bootstrap Job exists any more (TTL).
		cond, err := reconciler.ensureBootstrap(ctx, store, false, "")
		Expect(err).NotTo(HaveOccurred())
		meta.SetStatusCondition(&store.Status.Conditions, cond)

		cond, err = reconciler.ensureBootstrap(ctx, store, true, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(bootstrapJobs()).To(BeEmpty(),
			"setup:install must never run again on an installed store")
	})

	It("still holds a fresh install until dependencies are ready", func() {
		cond, err := reconciler.ensureBootstrap(ctx, store, false, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("AwaitingDependencies"))
		Expect(bootstrapJobs()).To(BeEmpty())
	})
})

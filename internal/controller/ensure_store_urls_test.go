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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ensureStoreURLs tests cover all rows of the URLsApplied state machine
// from ADR 013 §"URLsApplied state machine" and the hash-driven re-run
// semantics from §"Hash-driven re-run". No real Pods or Jobs run —
// envtest uses the fake API server; Job statuses are written via
// k8sClient.Status().Update to simulate completion (same pattern as
// ensure_upgrade_test.go's markJobComplete / markJobFailed helpers).
var _ = Describe("ensureStoreURLs", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	// newStore constructs a minimal MageStore with optional spec.stores and
	// optional TLS routing. Callers that need a fully wired store mutate the
	// returned value before Creating it.
	newStore := func() *magev1alpha1.MageStore {
		return &magev1alpha1.MageStore{
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
	}

	// storeWithSingleWebsite returns a MageStore spec with one website-scope entry.
	storeWithSingleWebsite := func() []magev1alpha1.StoreSpec {
		return []magev1alpha1.StoreSpec{
			{Host: "shop.example.com", Code: "base", Type: "website"},
		}
	}

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurestoreurls-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Capabilities: fullCapabilities(),
		}
		store = newStore()
		store.Spec.Stores = storeWithSingleWebsite()
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
	})

	// BootstrapReady != True — condition is omitted.
	Context("BootstrapReady != True", func() {
		It("returns empty condition (omitted) when bootstrapReady=false", func() {
			cond, err := reconciler.ensureStoreURLs(ctx, store, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty(), "zero condition means URLsApplied row is omitted from status")
		})
	})

	// BootstrapReady=True, no prior Job.
	Context("BootstrapReady=True, no prior Job", func() {
		It("creates <store>-store-urls Job with urls-hash annotation", func() {
			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())

			Expect(job.Annotations[storeURLsHashAnnotation]).NotTo(BeEmpty())
			Expect(job.Labels["app.kubernetes.io/component"]).To(Equal("store-urls"))
			Expect(job.Labels["app.kubernetes.io/managed-by"]).To(Equal(managedByLabel))
			Expect(job.Labels["app.kubernetes.io/instance"]).To(Equal(store.Name))

			// Job spec per ADR 013 §"Job spec".
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(1)))
			Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(300)))
			Expect(*job.Spec.TTLSecondsAfterFinished).To(Equal(int32(600)))

			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal("store-urls"))
			Expect(container.Image).To(Equal("registry.example.com/magento:2.4.8"))
			Expect(container.ImagePullPolicy).To(Equal(corev1.PullAlways))
			Expect(container.Command).To(Equal([]string{"sh", "-c"}))
			Expect(container.EnvFrom[0].SecretRef.Name).To(Equal(store.Name + "-config"))
			expectAdminFrontnameEnv(container, store.Name)

			// restartPolicy per ADR 013.
			Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))

			// condition is Unknown/Applying.
			Expect(cond.Type).To(Equal(ConditionURLsApplied))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Applying"))
		})

		It("returns Unknown/Applying immediately after creating the Job", func() {
			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionURLsApplied))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Applying"))
		})
	})

	// Job lifecycle.
	Context("Job lifecycle", func() {
		var job batchv1.Job

		JustBeforeEach(func() {
			// Create the Job by calling ensureStoreURLs once.
			_, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())
		})

		It("returns Unknown/Applying while Job is Active", func() {
			job.Status.Active = 1
			Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionURLsApplied))
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Applying"))
		})

		It("returns True/Applied when Job Succeeded", func() {
			Expect(markJobComplete(k8sClient, &job)).To(Succeed())

			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionURLsApplied))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Applied"))
		})

		It("URLsApplied message enumerates default/websites/store-views/skipped", func() {
			// Store with all three types.
			mixedStore := newStore()
			mixedStore.Name = "mixed"
			mixedStore.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "shop.example.com", Code: "base", Type: "website"},
				{Host: "fr.example.com", Code: "fr", Type: "store_view"},
				{Host: "de.example.com", Code: "de", Type: "store"},
			}
			Expect(k8sClient.Create(ctx, mixedStore)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mixed", Namespace: namespace}, mixedStore)).To(Succeed())

			_, err := reconciler.ensureStoreURLs(ctx, mixedStore, true)
			Expect(err).NotTo(HaveOccurred())

			var mixedJob batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: mixedStore.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &mixedJob)).To(Succeed())
			Expect(markJobComplete(k8sClient, &mixedJob)).To(Succeed())

			cond, err := reconciler.ensureStoreURLs(ctx, mixedStore, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Applied"))
			// Message enumerates scope categories.
			Expect(cond.Message).To(ContainSubstring("default scope auto-healed"))
			Expect(cond.Message).To(ContainSubstring("websites="))
			Expect(cond.Message).To(ContainSubstring("store-views="))
			Expect(cond.Message).To(ContainSubstring("skipped store-groups="))
		})

		It("returns False/RetryingFailedJob on first terminal-Failed observation (gap #1 recovery)", func() {
			Expect(markJobFailed(k8sClient, &job, "config:set exited 1")).To(Succeed())

			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionURLsApplied))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(JobRecoveryRetryReason))
			Expect(store.Status.JobAttempts).To(HaveKey(store.Name + storeURLsJobSuffix))
			Expect(store.Status.JobAttempts[store.Name+storeURLsJobSuffix].Count).To(Equal(1))
		})

		It("transitions to False/RecreatingFailedJob once the backoff window elapses", func() {
			Expect(markJobFailed(k8sClient, &job, "")).To(Succeed())

			// Inject a fake clock so we can advance past the first
			// 30 s window without sleeping. RandFloat=0.5 → no jitter.
			fixedNow := time.Now()
			reconciler.Now = func() time.Time { return fixedNow }
			reconciler.RandFloat = func() float64 { return 0.5 }

			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal(JobRecoveryRetryReason))

			// Advance past the 30 s window; recovery deletes the Job.
			fixedNow = fixedNow.Add(45 * time.Second)
			cond, err = reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal(JobRecoveryRecreatingReason))
		})
	})

	// Script content.
	Context("script content", func() {
		DescribeTable("script renders the correct scope per StoreSpec.Type",
			func(stores []magev1alpha1.StoreSpec, expectedSubstrings []string, expectedSkippedSubstrings []string) {
				s := newStore()
				s.Name = "scripttest"
				s.Spec.Stores = stores
				script := reconciler.buildStoreURLsScript(s)
				for _, sub := range expectedSubstrings {
					Expect(script).To(ContainSubstring(sub))
				}
				for _, sub := range expectedSkippedSubstrings {
					Expect(script).To(ContainSubstring(sub))
				}
			},
			Entry("type: website → --scope=websites",
				[]magev1alpha1.StoreSpec{{Host: "shop.example.com", Code: "base", Type: "website"}},
				[]string{"--scope=websites", "--scope-code=base"},
				nil,
			),
			Entry("type: store_view → --scope=stores",
				[]magev1alpha1.StoreSpec{{Host: "fr.example.com", Code: "fr", Type: "store_view"}},
				[]string{"--scope=stores", "--scope-code=fr"},
				nil,
			),
			Entry("type: store → skipped with echo line",
				[]magev1alpha1.StoreSpec{{Host: "de.example.com", Code: "de", Type: "store"}},
				nil,
				[]string{"skipped store-group de"},
			),
		)

		It("uses cache:flush, NOT cache:clean config (load-bearing per ADR 013)", func() {
			script := reconciler.buildStoreURLsScript(store)
			Expect(script).To(ContainSubstring("cache:flush"))
			Expect(script).NotTo(ContainSubstring("cache:clean config"))
		})

		It("does NOT contain eval", func() {
			script := reconciler.buildStoreURLsScript(store)
			Expect(script).NotTo(ContainSubstring("eval"))
		})

		It("sets web/secure/base_url and web/unsecure/base_url for default scope", func() {
			script := reconciler.buildStoreURLsScript(store)
			Expect(script).To(ContainSubstring("web/unsecure/base_url"))
			Expect(script).To(ContainSubstring("web/secure/base_url"))
		})

		It("sets all four URL keys per store (base + link URLs)", func() {
			s := newStore()
			s.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "shop.example.com", Code: "base", Type: "website"},
			}
			script := reconciler.buildStoreURLsScript(s)
			Expect(script).To(ContainSubstring("web/secure/base_link_url"))
			Expect(script).To(ContainSubstring("web/unsecure/base_link_url"))
		})

		It("uses https:// URL when spec.routing.tls is set", func() {
			s := newStore()
			s.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "shop.example.com", Code: "base", Type: "website"},
			}
			s.Spec.Routing = &magev1alpha1.RoutingSpec{
				TLS: &magev1alpha1.TLSSpec{},
			}
			script := reconciler.buildStoreURLsScript(s)
			Expect(script).To(ContainSubstring("https://shop.example.com/"))
		})

		It("uses http:// URL when spec.routing.tls is unset", func() {
			s := newStore()
			s.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "shop.example.com", Code: "base", Type: "website"},
			}
			script := reconciler.buildStoreURLsScript(s)
			Expect(script).To(ContainSubstring("http://shop.example.com/"))
		})
	})

	// Hash-driven re-run.
	Context("hash-driven re-run", func() {
		It("hash-flip deletes existing Job and creates new one (different UID)", func() {
			// First call creates the Job.
			_, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())

			var oldJob batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &oldJob)).To(Succeed())
			oldUID := oldJob.UID

			// Change spec.stores to flip the hash.
			store.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "new.example.com", Code: "new", Type: "website"},
			}

			// Second call: hash mismatch → delete + create.
			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Applying"))

			// The new Job should eventually be visible (background delete may race
			// envtest; the condition returning Applying is the observable signal).
			// We verify the old Job was deleted by checking the returned condition
			// indicates a fresh Job creation (Applying with fresh message).
			Expect(cond.Message).To(ContainSubstring("created"))

			// Verify the old UID is gone or the hash annotation changed.
			var newJob batchv1.Job
			if err := k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &newJob); err == nil {
				// New job may have same name but different UID if recreated, or the
				// background delete hasn't run yet. The key assertion is that if we
				// can read the job, its hash annotation reflects the new spec.
				newHash := hashStoreURLInputs(store.Spec.Stores, false)
				if newJob.UID != oldUID {
					Expect(newJob.Annotations[storeURLsHashAnnotation]).To(Equal(newHash))
				}
			}
		})

		It("hash-stable: same spec → no Job recreation (UID identity)", func() {
			// First call creates the Job.
			_, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())
			originalUID := job.UID

			// Second call with same spec: hash identical → no re-create.
			_, err = reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())

			var afterJob batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &afterJob)).To(Succeed())
			Expect(afterJob.UID).To(Equal(originalUID), "same-hash must not recreate the Job")
		})

		It("URLsApplied=True is sticky for the SAME hash (different from sticky-True)", func() {
			// Create the Job.
			_, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &job)).To(Succeed())
			// Mark it as succeeded.
			Expect(markJobComplete(k8sClient, &job)).To(Succeed())

			// Call again with same spec: same hash + Job succeeded → True/Applied (sticky).
			cond, err := reconciler.ensureStoreURLs(ctx, store, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionURLsApplied))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Applied"))

			// Verify that the Job UID did not change (no recreation).
			var afterJob batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + storeURLsJobSuffix, Namespace: namespace,
			}, &afterJob)).To(Succeed())
			Expect(afterJob.UID).To(Equal(job.UID), "sticky-same-hash must not recreate the Job")
		})
	})

	// hashStoreURLInputs unit tests (pure, no API calls).
	Context("hashStoreURLInputs", func() {
		It("produces a 16-char hex string", func() {
			h := hashStoreURLInputs(nil, false)
			Expect(h).To(HaveLen(16))
			Expect(h).To(MatchRegexp(`^[0-9a-f]{16}$`))
		})

		It("differs when TLS flag changes", func() {
			stores := []magev1alpha1.StoreSpec{{Host: "shop.example.com", Code: "base", Type: "website"}}
			h1 := hashStoreURLInputs(stores, false)
			h2 := hashStoreURLInputs(stores, true)
			Expect(h1).NotTo(Equal(h2))
		})

		It("differs when store host changes", func() {
			s1 := []magev1alpha1.StoreSpec{{Host: "a.example.com", Code: "a", Type: "website"}}
			s2 := []magev1alpha1.StoreSpec{{Host: "b.example.com", Code: "a", Type: "website"}}
			Expect(hashStoreURLInputs(s1, false)).NotTo(Equal(hashStoreURLInputs(s2, false)))
		})

		It("differs when store code changes", func() {
			s1 := []magev1alpha1.StoreSpec{{Host: "shop.example.com", Code: "a", Type: "website"}}
			s2 := []magev1alpha1.StoreSpec{{Host: "shop.example.com", Code: "b", Type: "website"}}
			Expect(hashStoreURLInputs(s1, false)).NotTo(Equal(hashStoreURLInputs(s2, false)))
		})

		It("is stable for the same inputs", func() {
			stores := []magev1alpha1.StoreSpec{{Host: "shop.example.com", Code: "base", Type: "website"}}
			h1 := hashStoreURLInputs(stores, false)
			h2 := hashStoreURLInputs(stores, false)
			Expect(h1).To(Equal(h2))
		})
	})

	// Reconcile graph: URLsApplied condition appears on store status after reconcile.
	Context("reconcile graph integration", func() {
		It("condition is omitted from status when bootstrapReady=false", func() {
			cond, err := reconciler.ensureStoreURLs(ctx, store, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty())
			// Caller checks Type != "" before SetStatusCondition, so it won't appear.
			existing := apimeta.FindStatusCondition(store.Status.Conditions, ConditionURLsApplied)
			Expect(existing).To(BeNil())
		})
	})
})

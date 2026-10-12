/*
Copyright 2026.
*/

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// Test fixture tags. fromTag mirrors a Magento patch-pre-2.4.8 baseline;
// toTag is the post-upgrade target. These constants exist purely to
// keep `goconst` quiet without sprinkling magic strings across cases.
const (
	upgradeFromTag = "2.4.7"
	upgradeToTag   = "2.4.8"
)

// markJobComplete writes the JobComplete=True status with the
// precursor SuccessCriteriaMet condition and StartTime/CompletionTime
// fields that K8s 1.35 batch/v1 validation requires.
func markJobComplete(c client.Client, job *batchv1.Job) error {
	now := metav1.Now()
	earlier := metav1.NewTime(now.Add(-1 * time.Minute))
	job.Status.StartTime = &earlier
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}
	return c.Status().Update(ctx, job)
}

// disableMaintenance opts the store out of the maintenance fence by
// setting `spec.upgrade.maintenance.enabled = false`. Used in tests
// that exercise the B-shape arc (cron quiesce + Job + restore)
// directly, without C's maintenance-pod-ready / workloads-ready
// gating. Without this, envtest's lack of a Deployment controller
// keeps AvailableReplicas at 0 forever and the arc stalls at
// "waiting for maintenance pod."
func disableMaintenance(store *magev1alpha1.MageStore) {
	falseVal := false
	if store.Spec.Upgrade == nil {
		store.Spec.Upgrade = &magev1alpha1.MageStoreUpgradeSpec{}
	}
	store.Spec.Upgrade.Maintenance = &magev1alpha1.MageStoreUpgradeMaintenanceSpec{
		Enabled: &falseVal,
	}
}

// markJobFailed writes the JobFailed=True status with the precursor
// FailureTarget condition and StartTime field that K8s 1.35 batch/v1
// validation requires.
func markJobFailed(c client.Client, job *batchv1.Job, message string) error {
	earlier := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	job.Status.StartTime = &earlier
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Message: message},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: message},
	}
	return c.Status().Update(ctx, job)
}

// Behaviour-mode tests for ensureUpgrade and the controller-level
// glue that feeds it. These are envtest assertions against the real
// API server; no real Pods or Jobs run, so the cron-pods-running
// branch always sees zero pods (covered separately by an explicit
// Pod-create case).
var _ = Describe("ensureUpgrade", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensureupgrade-"},
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
					Tag:        upgradeToTag,
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
	})

	Context("Case 1: bootstrap completion advances observedImageTag", func() {
		It("advances on the first StoreInstalled=True transition and is idempotent thereafter", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			Expect(store.Status.ObservedImageTag).To(BeEmpty())

			// First True transition stamps the spec tag.
			advanceObservedImageTagOnInstall(store, metav1.Condition{
				Type:   ConditionStoreInstalled,
				Status: metav1.ConditionTrue,
			})
			Expect(store.Status.ObservedImageTag).To(Equal(upgradeToTag))

			// A subsequent True call must NOT overwrite — even if the
			// caller passes a stale spec via tampering with the store.
			store.Spec.Image.Tag = "2.4.9"
			advanceObservedImageTagOnInstall(store, metav1.Condition{
				Type:   ConditionStoreInstalled,
				Status: metav1.ConditionTrue,
			})
			Expect(store.Status.ObservedImageTag).To(Equal(upgradeToTag))
		})

		It("does not advance on Unknown (spec.bootstrap unset) or False (in-flight)", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			advanceObservedImageTagOnInstall(store, metav1.Condition{
				Type:   ConditionStoreInstalled,
				Status: metav1.ConditionUnknown,
			})
			Expect(store.Status.ObservedImageTag).To(BeEmpty())

			advanceObservedImageTagOnInstall(store, metav1.Condition{
				Type:   ConditionStoreInstalled,
				Status: metav1.ConditionFalse,
			})
			Expect(store.Status.ObservedImageTag).To(BeEmpty())
		})

		It("ensureUpgrade returns Unknown/AwaitingUpgrade when observedImageTag is empty", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal(ReasonAwaitingUpgrade))
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	Context("Case 2: tag drift creates an upgrade Job", func() {
		BeforeEach(func() {
			store.Spec.Image.Tag = upgradeToTag
			disableMaintenance(store) // exercise B's quiesce-then-Job flow without the C maintenance fence
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())
		})

		It("creates a Job with the right name, labels, image, and BackoffLimit", func() {
			cond, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonUpgradeStarting))
			// In-flight arc — pipeline must halt.
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "store-upgrade-2-4-7-2-4-8",
				Namespace: namespace,
			}, &job)).To(Succeed())

			Expect(job.Annotations[upgradeFromAnnotation]).To(Equal(upgradeFromTag))
			Expect(job.Annotations[upgradeToAnnotation]).To(Equal(upgradeToTag))
			Expect(job.Labels["app.kubernetes.io/component"]).To(Equal(upgradeComponent))
			Expect(job.Labels["app.kubernetes.io/managed-by"]).To(Equal(managedByLabel))

			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			c := job.Spec.Template.Spec.Containers[0]
			Expect(c.Image).To(Equal("registry.gitlab.aknostic.com/test/magento:2.4.8"))
			Expect(c.ImagePullPolicy).To(Equal(corev1.PullAlways))
			Expect(c.Args).To(HaveLen(1))
			Expect(c.Args[0]).To(ContainSubstring("setup:upgrade"))
			Expect(c.Args[0]).To(ContainSubstring("cache:flush"))

			Expect(*job.Spec.BackoffLimit).To(Equal(int32(1)))
			Expect(job.Spec.TTLSecondsAfterFinished).NotTo(BeNil())
			Expect(*job.Spec.TTLSecondsAfterFinished).To(Equal(int32(3600)))

			// Owner reference back to the MageStore.
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Name).To(Equal(store.Name))

			// Phase reflects in-flight arc.
			Expect(store.Status.Phase).To(Equal(magev1alpha1.PhaseUpgrading))
		})

		It("phase=Upgrading short-circuits the pipeline (RequeueAfter > 0)", func() {
			_, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			// The brief's load-bearing invariant: an in-flight arc must
			// halt the pipeline so ensureWorkloads can't roll the
			// frontend Deployment forward to the new image tag against
			// an un-migrated schema.
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		})
	})

	Context("Case 3: idempotency — second reconcile finds the existing Job", func() {
		BeforeEach(func() {
			store.Spec.Image.Tag = upgradeToTag
			disableMaintenance(store)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())
		})

		It("does not duplicate the Job and surfaces WorkloadRolloutPending while it runs", func() {
			// First call creates the Job.
			_, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var first batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "store-upgrade-2-4-7-2-4-8",
				Namespace: namespace,
			}, &first)).To(Succeed())
			firstUID := first.UID

			// Second call must find the same Job (UID stable) and surface
			// WorkloadRolloutPending — Job exists but has no JobComplete /
			// JobFailed condition yet.
			cond, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonWorkloadRolloutPending))
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			var second batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "store-upgrade-2-4-7-2-4-8",
				Namespace: namespace,
			}, &second)).To(Succeed())
			Expect(second.UID).To(Equal(firstUID))

			// And only one upgrade Job in the namespace.
			var allJobs batchv1.JobList
			Expect(k8sClient.List(ctx, &allJobs, client.InNamespace(namespace), client.MatchingLabels{
				"app.kubernetes.io/component":  upgradeComponent,
				"app.kubernetes.io/managed-by": managedByLabel,
			})).To(Succeed())
			Expect(allJobs.Items).To(HaveLen(1))
		})
	})

	Context("Case 4: Job success advances observedImageTag and restores quiesce state", func() {
		var (
			cronJob      *batchv1.CronJob
			consumerDep  *appsv1.Deployment
			upgradeJob   *batchv1.Job
			originalReps int32 = 1
		)

		BeforeEach(func() {
			store.Spec.Image.Tag = upgradeToTag
			disableMaintenance(store)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			// Pre-quiesced cron — suspended, with the operator labels.
			cronJob = newOwnedCronJob("store-cron-default", namespace, "store", true)
			Expect(k8sClient.Create(ctx, cronJob)).To(Succeed())

			// Pre-quiesced consumer — replicas=0 with the saved-replicas
			// annotation set to "1" so restore knows what to put back.
			consumerDep = newOwnedConsumerDeployment("store-consumer-async-operations-all", namespace, "store", 0)
			consumerDep.Annotations = map[string]string{
				upgradeSavedReplicasAnnotation: "1",
			}
			Expect(k8sClient.Create(ctx, consumerDep)).To(Succeed())

			// Existing upgrade Job. Status flipped manually — envtest
			// has no Job controller running, so we write JobComplete
			// directly.
			upgradeJob = reconciler.buildUpgradeJob(store, "store-upgrade-2-4-7-2-4-8", upgradeFromTag, upgradeToTag)
			Expect(ctrl.SetControllerReference(store, upgradeJob, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, upgradeJob)).To(Succeed())

			Expect(markJobComplete(k8sClient, upgradeJob)).To(Succeed())
		})

		It("advances observedImageTag, sets phase=Running, and restores cron + consumer state", func() {
			cond, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("UpgradeComplete"))
			Expect(result.RequeueAfter).To(BeZero())

			Expect(store.Status.ObservedImageTag).To(Equal(upgradeToTag))
			Expect(store.Status.Phase).To(Equal(magev1alpha1.PhaseRunning))

			// CronJob un-suspended.
			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cronJob.Name, Namespace: namespace}, &cj)).To(Succeed())
			Expect(cj.Spec.Suspend).NotTo(BeNil())
			Expect(*cj.Spec.Suspend).To(BeFalse())

			// Consumer scaled back to its saved replica count and
			// annotation cleared.
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: consumerDep.Name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Replicas).NotTo(BeNil())
			Expect(*dep.Spec.Replicas).To(Equal(originalReps))
			Expect(dep.Annotations).NotTo(HaveKey(upgradeSavedReplicasAnnotation))
		})
	})

	Context("Case 5: Job failure stays quiesced", func() {
		var (
			cronJob     *batchv1.CronJob
			consumerDep *appsv1.Deployment
			upgradeJob  *batchv1.Job
		)

		BeforeEach(func() {
			store.Spec.Image.Tag = upgradeToTag
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			cronJob = newOwnedCronJob("store-cron-default", namespace, "store", true)
			Expect(k8sClient.Create(ctx, cronJob)).To(Succeed())

			consumerDep = newOwnedConsumerDeployment("store-consumer-async-operations-all", namespace, "store", 0)
			consumerDep.Annotations = map[string]string{
				upgradeSavedReplicasAnnotation: "1",
			}
			Expect(k8sClient.Create(ctx, consumerDep)).To(Succeed())

			upgradeJob = reconciler.buildUpgradeJob(store, "store-upgrade-2-4-7-2-4-8", upgradeFromTag, upgradeToTag)
			Expect(ctrl.SetControllerReference(store, upgradeJob, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, upgradeJob)).To(Succeed())

			Expect(markJobFailed(k8sClient, upgradeJob, "setup:upgrade exited 1")).To(Succeed())
		})

		It("sets phase=Failed, UpgradeReady=False/RetryingFailedJob, and leaves quiesce state in place", func() {
			cond, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			// v0.6.2 gap #1: first terminal-Failed observation surfaces
			// RetryingFailedJob (operator will delete-and-recreate after
			// the backoff window). Lockstep preservation unchanged.
			Expect(cond.Reason).To(Equal(JobRecoveryRetryReason))
			// Brief specifies ctrl.Result{} on terminal failure — the
			// pipeline continues (B-only production-unsafe gap).
			Expect(result.RequeueAfter).To(BeZero())

			Expect(store.Status.Phase).To(Equal(magev1alpha1.PhaseFailed))
			// observedImageTag must NOT advance on failure.
			Expect(store.Status.ObservedImageTag).To(Equal(upgradeFromTag))

			// Cron stays suspended.
			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cronJob.Name, Namespace: namespace}, &cj)).To(Succeed())
			Expect(cj.Spec.Suspend).NotTo(BeNil())
			Expect(*cj.Spec.Suspend).To(BeTrue())

			// Consumer stays at zero, annotation preserved.
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: consumerDep.Name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Replicas).NotTo(BeNil())
			Expect(*dep.Spec.Replicas).To(Equal(int32(0)))
			Expect(dep.Annotations).To(HaveKeyWithValue(upgradeSavedReplicasAnnotation, "1"))
		})
	})

	Context("Case 6: lockstep refusal — failed Job + reverted spec.image.tag", func() {
		var (
			cronJob     *batchv1.CronJob
			consumerDep *appsv1.Deployment
			upgradeJob  *batchv1.Job
		)

		BeforeEach(func() {
			// spec.image.tag is the OLD tag (admin reverted from 2.4.8 →
			// 2.4.7 hoping to "undo" the failed arc). observedImageTag
			// is also 2.4.7 because the failed Job didn't advance it.
			// Despite spec == observed (no apparent drift), the failed
			// Job's presence is the lockstep mechanism that keeps the
			// operator quiesced.
			store.Spec.Image.Tag = upgradeFromTag
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			cronJob = newOwnedCronJob("store-cron-default", namespace, "store", true)
			Expect(k8sClient.Create(ctx, cronJob)).To(Succeed())

			consumerDep = newOwnedConsumerDeployment("store-consumer-async-operations-all", namespace, "store", 0)
			consumerDep.Annotations = map[string]string{
				upgradeSavedReplicasAnnotation: "1",
			}
			Expect(k8sClient.Create(ctx, consumerDep)).To(Succeed())

			// Failed Job named with the original (2.4.7 → 2.4.8) pair —
			// admin has since reverted spec.image.tag, so the Job's
			// name no longer matches the current (from, to). List-by-
			// label is what surfaces it.
			upgradeJob = reconciler.buildUpgradeJob(store, "store-upgrade-2-4-7-2-4-8", upgradeFromTag, upgradeToTag)
			Expect(ctrl.SetControllerReference(store, upgradeJob, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, upgradeJob)).To(Succeed())

			Expect(markJobFailed(k8sClient, upgradeJob, "setup:upgrade exited 1")).To(Succeed())
		})

		It("refuses to un-quiesce while the failed Job exists, even though spec == observed", func() {
			cond, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			// v0.6.2 gap #1: recovery surfaces RetryingFailedJob on
			// first observation; lockstep semantics unchanged.
			Expect(cond.Reason).To(Equal(JobRecoveryRetryReason))
			Expect(store.Status.Phase).To(Equal(magev1alpha1.PhaseFailed))

			// Cron + consumer remain quiesced.
			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cronJob.Name, Namespace: namespace}, &cj)).To(Succeed())
			Expect(*cj.Spec.Suspend).To(BeTrue())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: consumerDep.Name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(0)))
			Expect(dep.Annotations).To(HaveKeyWithValue(upgradeSavedReplicasAnnotation, "1"))
		})

		It("un-quiesces after the admin deletes the failed Job", func() {
			Expect(k8sClient.Delete(ctx, upgradeJob)).To(Succeed())

			cond, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("UpToDate"))

			// CronJob un-suspended; consumer replicas restored from annotation.
			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cronJob.Name, Namespace: namespace}, &cj)).To(Succeed())
			Expect(*cj.Spec.Suspend).To(BeFalse())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: consumerDep.Name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
			Expect(dep.Annotations).NotTo(HaveKey(upgradeSavedReplicasAnnotation))
		})
	})

	// ─── C: maintenance fence + Service-selector flip ───────────────

	Context("Case C1: maintenance materialise on arc entry", func() {
		BeforeEach(func() {
			store.Spec.Image.Tag = upgradeToTag
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())
		})

		It("creates maintenance Deployment + Service + default 503 ConfigMap on first reconcile", func() {
			cond, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			// Maintenance pod has no controller in envtest, so AvailableReplicas
			// stays at 0 — the arc parks at "waiting for maintenance pod."
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonUpgradeStarting))
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(ContainSubstring("nginx"))
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(dep.OwnerReferences[0].Name).To(Equal(store.Name))

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Spec.Selector["app.kubernetes.io/component"]).To(Equal(maintenanceComponent))

			var cmConfig, cm503 corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance-config", Namespace: namespace}, &cmConfig)).To(Succeed())
			Expect(cmConfig.Data).To(HaveKey(maintenanceConfKey))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance-default-503", Namespace: namespace}, &cm503)).To(Succeed())
			Expect(cm503.Data).To(HaveKey(maintenanceHTMLKey))
			Expect(cm503.Data[maintenanceHTMLKey]).To(ContainSubstring("right back"))
		})

		It("Service selectors do NOT flip until the maintenance pod reaches Available", func() {
			// Pre-create the workload Service so the flip would have something to flip.
			frontendSvc := newOwnedFrontendService(store)
			Expect(k8sClient.Create(ctx, frontendSvc)).To(Succeed())

			_, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Maintenance pod isn't Available (envtest has no Deployment controller),
			// so the flip didn't fire and the Service still has its workload selector.
			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: frontendSvc.Name, Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Annotations).NotTo(HaveKey(maintenanceFlippedAnnotation))
			Expect(svc.Spec.Selector["app.kubernetes.io/component"]).To(Equal("frontend"))
		})
	})

	Context("Case C2: Service-selector flip when maintenance pod is Available", func() {
		It("flips frontend / admin / api selectors and sets the flipped annotation", func() {
			store.Spec.Image.Tag = upgradeToTag
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			// Pre-create the workload Service so flip has a target.
			frontendSvc := newOwnedFrontendService(store)
			Expect(k8sClient.Create(ctx, frontendSvc)).To(Succeed())

			// Pre-create maintenance Deployment with AvailableReplicas=1 so
			// ensureMaintenance reports ready on first reconcile.
			Expect(reconciler.ensureMaintenance(ctx, store)).Error().NotTo(HaveOccurred())
			var maintDep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &maintDep)).To(Succeed())
			// K8s 1.35 enforces Status.AvailableReplicas <= ReadyReplicas <= Replicas.
			maintDep.Status.Replicas = 1
			maintDep.Status.ReadyReplicas = 1
			maintDep.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, &maintDep)).To(Succeed())

			_, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: frontendSvc.Name, Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Annotations).To(HaveKeyWithValue(maintenanceFlippedAnnotation, "true"))
			Expect(svc.Spec.Selector["app.kubernetes.io/component"]).To(Equal(maintenanceComponent))
		})
	})

	Context("Case C3: ensureWorkloadService skip-gate respects the flipped annotation", func() {
		It("does not overwrite the maintenance selector while the annotation is set", func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			// A flipped Service exists with the maintenance selector + annotation.
			flipped := newOwnedFrontendService(store)
			flipped.Annotations = map[string]string{maintenanceFlippedAnnotation: "true"}
			flipped.Spec.Selector = maintenanceSelector(store)
			Expect(k8sClient.Create(ctx, flipped)).To(Succeed())

			// Calling ensureWorkloadService directly (the path
			// ensureWorkloads exercises every reconcile during an arc).
			Expect(reconciler.ensureWorkloadService(ctx, store, workloadFrontend)).To(Succeed())

			var got corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: flipped.Name, Namespace: namespace}, &got)).To(Succeed())
			// Selector preserved at maintenance; ports/type still reconciled.
			Expect(got.Spec.Selector["app.kubernetes.io/component"]).To(Equal(maintenanceComponent))
			Expect(got.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(got.Spec.Ports).To(HaveLen(1))
			Expect(got.Spec.Ports[0].Port).To(Equal(int32(8080)))
		})
	})

	Context("Case C4: opt-out (spec.upgrade.maintenance.enabled = false) skips fence", func() {
		It("does not create maintenance resources and proceeds straight to quiesce", func() {
			store.Spec.Image.Tag = upgradeToTag
			disableMaintenance(store)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			_, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &dep)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// And the Job WAS created (no maintenance gating, straight to Job).
			var jobs batchv1.JobList
			Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{
				"app.kubernetes.io/component": upgradeComponent,
			})).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1))
		})
	})

	Context("Case C5: two-stage success path", func() {
		var upgradeJob *batchv1.Job

		BeforeEach(func() {
			store.Spec.Image.Tag = upgradeToTag
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			// Pre-create flipped frontend Service so restoreServicesFromMaintenance
			// has something to clear at finalise.
			frontendSvc := newOwnedFrontendService(store)
			frontendSvc.Annotations = map[string]string{maintenanceFlippedAnnotation: "true"}
			frontendSvc.Spec.Selector = maintenanceSelector(store)
			Expect(k8sClient.Create(ctx, frontendSvc)).To(Succeed())

			// Pre-existing complete upgrade Job.
			upgradeJob = reconciler.buildUpgradeJob(store, "store-upgrade-2-4-7-2-4-8", upgradeFromTag, upgradeToTag)
			Expect(ctrl.SetControllerReference(store, upgradeJob, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, upgradeJob)).To(Succeed())
			Expect(markJobComplete(k8sClient, upgradeJob)).To(Succeed())

			// Pre-create maintenance Deployment so deleteMaintenance has work to do.
			Expect(reconciler.ensureMaintenance(ctx, store)).Error().NotTo(HaveOccurred())
		})

		It("stage 1 surfaces WorkloadRolloutPending while workload Deployments are not yet Available", func() {
			cond, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonWorkloadRolloutPending))

			// observedImageTag advanced (stage 1 work done).
			Expect(store.Status.ObservedImageTag).To(Equal(upgradeToTag))

			// Maintenance flip still in place — fence holds.
			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workloadName(store, workloadFrontend), Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Annotations).To(HaveKeyWithValue(maintenanceFlippedAnnotation, "true"))

			// Maintenance Deployment still exists.
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &dep)).To(Succeed())
		})

		It("stage 2 finalises (selector restore + maintenance delete) once workload Deployments are Available", func() {
			// Workload Deployments don't exist in envtest (ensureWorkloads not called),
			// so we synthesise one with AvailableReplicas matching desired.
			feDep := newReadyFrontendDeployment(store, 1)
			Expect(k8sClient.Create(ctx, feDep)).To(Succeed())
			feDep.Status.Replicas = 1
			feDep.Status.ReadyReplicas = 1
			feDep.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, feDep)).To(Succeed())

			cond, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("UpgradeComplete"))

			// Frontend Service selector restored, annotation cleared.
			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workloadName(store, workloadFrontend), Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Annotations).NotTo(HaveKey(maintenanceFlippedAnnotation))
			Expect(svc.Spec.Selector["app.kubernetes.io/component"]).To(Equal("frontend"))

			// Maintenance Deployment deleted (or in deletion).
			var maintDep appsv1.Deployment
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &maintDep)
			if err == nil {
				// envtest may keep finalizers around; assert DeletionTimestamp set.
				Expect(maintDep.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		})
	})

	Context("Case C7: stale Complete Job + spec drifted past it", func() {
		var upgradeJob *batchv1.Job
		const driftTag = "2.4.9"

		BeforeEach(func() {
			// spec.image.tag has drifted past the prior arc's recorded
			// target — admin patched to 2.4.9 within the Complete Job's
			// 1h TTL window. observedImageTag is still the pre-arc
			// baseline (the controller's main loop persists it after
			// each reconcile, so on entry here it reflects pre-arc).
			store.Spec.Image.Tag = driftTag
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			// Pre-flipped frontend Service mirroring C5's fixture — the
			// short-circuit must NOT tear it down (the new arc re-uses
			// the fence; tearing it down + re-flipping would briefly
			// expose the storefront to traffic against a workload pod
			// that's about to be quiesced again).
			frontendSvc := newOwnedFrontendService(store)
			frontendSvc.Annotations = map[string]string{maintenanceFlippedAnnotation: "true"}
			frontendSvc.Spec.Selector = maintenanceSelector(store)
			Expect(k8sClient.Create(ctx, frontendSvc)).To(Succeed())

			// Pre-existing Complete Job for the prior (2.4.7 → 2.4.8) arc.
			upgradeJob = reconciler.buildUpgradeJob(store, "store-upgrade-2-4-7-2-4-8", upgradeFromTag, upgradeToTag)
			Expect(ctrl.SetControllerReference(store, upgradeJob, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, upgradeJob)).To(Succeed())
			Expect(markJobComplete(k8sClient, upgradeJob)).To(Succeed())

			// Pre-create maintenance Deployment so we can assert it's
			// NOT deleted by the short-circuit.
			Expect(reconciler.ensureMaintenance(ctx, store)).Error().NotTo(HaveOccurred())
		})

		It("deletes the stale Job, requeues, and retains the maintenance fence for the new arc", func() {
			cond, result, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Critical regression assertion: NOT UpgradeComplete. Pre-fix
			// the operator would park here at UpgradeReady=True/UpgradeComplete
			// for up to 1h until the stale Job's TTL expired.
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonUpgradeStarting))
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Stage-1 advance ran: observedImageTag now matches the
			// stale Job's recorded target, leaving observed != spec
			// drift for the next reconcile to consume.
			Expect(store.Status.ObservedImageTag).To(Equal(upgradeToTag))

			// Stale Job deleted (or has DeletionTimestamp set in envtest,
			// which lacks the API-server's Job GC controller).
			var got batchv1.Job
			err = k8sClient.Get(ctx, types.NamespacedName{Name: upgradeJob.Name, Namespace: namespace}, &got)
			if err == nil {
				Expect(got.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}

			// Maintenance fence intentionally retained — flip annotation
			// still set; maintenance Deployment still alive (no
			// DeletionTimestamp). The next reconcile's runUpgradeArc
			// will idempotently re-enter and re-use both.
			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workloadName(store, workloadFrontend), Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Annotations).To(HaveKeyWithValue(maintenanceFlippedAnnotation, "true"))
			Expect(svc.Spec.Selector["app.kubernetes.io/component"]).To(Equal(maintenanceComponent))

			var maintDep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &maintDep)).To(Succeed())
			Expect(maintDep.DeletionTimestamp).To(BeNil())

			// No fresh upgrade Job created on this reconcile — the
			// reconcile only does delete+requeue. Any Job still listed
			// must be the stale one in deletion.
			var jobs batchv1.JobList
			Expect(k8sClient.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{
				"app.kubernetes.io/component": upgradeComponent,
			})).To(Succeed())
			for i := range jobs.Items {
				Expect(jobs.Items[i].DeletionTimestamp).NotTo(BeNil())
			}
		})
	})

	Context("Case C8: failed-Job-deleted recovery drops the maintenance fence symmetrically", func() {
		var (
			cronJob     *batchv1.CronJob
			consumerDep *appsv1.Deployment
			upgradeJob  *batchv1.Job
		)

		BeforeEach(func() {
			// spec reverted to fromTag after a failed (2.4.7 → 2.4.8)
			// arc. observed never advanced because the Job failed.
			store.Spec.Image.Tag = upgradeFromTag
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			// Lockstep state from the failed arc: cron suspended,
			// consumer at zero with saved-replicas annotation.
			cronJob = newOwnedCronJob("store-cron-default", namespace, "store", true)
			Expect(k8sClient.Create(ctx, cronJob)).To(Succeed())

			consumerDep = newOwnedConsumerDeployment("store-consumer-async-operations-all", namespace, "store", 0)
			consumerDep.Annotations = map[string]string{upgradeSavedReplicasAnnotation: "1"}
			Expect(k8sClient.Create(ctx, consumerDep)).To(Succeed())

			// Flipped frontend Service still pointing at the maintenance
			// pod — the failed arc never reached stage-2 finalise.
			frontendSvc := newOwnedFrontendService(store)
			frontendSvc.Annotations = map[string]string{maintenanceFlippedAnnotation: "true"}
			frontendSvc.Spec.Selector = maintenanceSelector(store)
			Expect(k8sClient.Create(ctx, frontendSvc)).To(Succeed())

			// Maintenance Deployment + Service still present.
			Expect(reconciler.ensureMaintenance(ctx, store)).Error().NotTo(HaveOccurred())

			// Failed Job that the admin is about to delete.
			upgradeJob = reconciler.buildUpgradeJob(store, "store-upgrade-2-4-7-2-4-8", upgradeFromTag, upgradeToTag)
			Expect(ctrl.SetControllerReference(store, upgradeJob, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, upgradeJob)).To(Succeed())
			Expect(markJobFailed(k8sClient, upgradeJob, "setup:upgrade exited 1")).To(Succeed())

			// Admin deletes the failed Job — the operator-defined
			// unstick path (per ADR 008).
			Expect(k8sClient.Delete(ctx, upgradeJob)).To(Succeed())
		})

		It("clears Service-selector flip and tears down maintenance Deployment alongside cron/consumer restore", func() {
			cond, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("UpToDate"))

			// Existing Case 6 behaviour preserved: cron un-suspended,
			// consumer replicas restored from the saved annotation.
			var cj batchv1.CronJob
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cronJob.Name, Namespace: namespace}, &cj)).To(Succeed())
			Expect(cj.Spec.Suspend).NotTo(BeNil())
			Expect(*cj.Spec.Suspend).To(BeFalse())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: consumerDep.Name, Namespace: namespace}, &dep)).To(Succeed())
			Expect(dep.Spec.Replicas).NotTo(BeNil())
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
			Expect(dep.Annotations).NotTo(HaveKey(upgradeSavedReplicasAnnotation))

			// New behaviour: Service-selector flip cleared, maintenance
			// annotation gone, selector back to the workload pods.
			var svc corev1.Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workloadName(store, workloadFrontend), Namespace: namespace}, &svc)).To(Succeed())
			Expect(svc.Annotations).NotTo(HaveKey(maintenanceFlippedAnnotation))
			Expect(svc.Spec.Selector["app.kubernetes.io/component"]).To(Equal("frontend"))

			// New behaviour: maintenance Deployment deleted (or in
			// deletion — envtest may keep finalizers).
			var maintDep appsv1.Deployment
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &maintDep)
			if err == nil {
				Expect(maintDep.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		})
	})

	Context("Case C6: htmlConfigMapRef override", func() {
		It("mounts the user's ConfigMap and skips the default-503 ConfigMap", func() {
			userCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "my-503", Namespace: namespace},
				Data:       map[string]string{"index.html": "<h1>custom maintenance</h1>"},
			}
			Expect(k8sClient.Create(ctx, userCM)).To(Succeed())

			store.Spec.Image.Tag = upgradeToTag
			if store.Spec.Upgrade == nil {
				store.Spec.Upgrade = &magev1alpha1.MageStoreUpgradeSpec{}
			}
			store.Spec.Upgrade.Maintenance = &magev1alpha1.MageStoreUpgradeMaintenanceSpec{
				HtmlConfigMapRef: &magev1alpha1.MaintenanceHtmlConfigMapRef{
					Name: "my-503",
					Key:  "index.html",
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
			store.Status.ObservedImageTag = upgradeFromTag
			Expect(k8sClient.Status().Update(ctx, store)).To(Succeed())

			_, _, err := reconciler.ensureUpgrade(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Maintenance Deployment mounts the user's ConfigMap.
			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance", Namespace: namespace}, &dep)).To(Succeed())
			var htmlVolFound bool
			for _, vol := range dep.Spec.Template.Spec.Volumes {
				if vol.Name == "html" {
					Expect(vol.ConfigMap).NotTo(BeNil())
					Expect(vol.ConfigMap.Name).To(Equal("my-503"))
					htmlVolFound = true
				}
			}
			Expect(htmlVolFound).To(BeTrue())

			// Default-503 ConfigMap NOT created.
			var cm corev1.ConfigMap
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "store-maintenance-default-503", Namespace: namespace}, &cm)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})
})

// newOwnedCronJob produces a minimal CronJob carrying the labels
// ensureCron emits. The Spec is deliberately near-empty — the upgrade
// reconciler only ever toggles spec.suspend, so the rest of the
// schedule shape is irrelevant to these tests. The `name` and
// `storeName` parameters look unused-by-unparam because every caller
// passes the canonical fixture values, but keeping them keeps the
// helper signature symmetric with the per-cron-group naming
// convention the operator uses in production (one CronJob per group).
//
//nolint:unparam // intentionally parameterised for readability/symmetry
func newOwnedCronJob(name, namespace, storeName string, suspend bool) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "magento",
				"app.kubernetes.io/instance":   storeName,
				"app.kubernetes.io/component":  "cron",
				"app.kubernetes.io/managed-by": managedByLabel,
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "* * * * *",
			Suspend:  &suspend,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{{
								Name:  "cron",
								Image: "placeholder",
							}},
						},
					},
				},
			},
		},
	}
}

// newOwnedConsumerDeployment produces a minimal Deployment carrying
// the labels ensureConsumers emits. Used by the quiesce/restore tests
// to assert the upgrade reconciler scales replicas correctly. Same
// param-symmetry note as newOwnedCronJob: every caller currently uses
// canonical values, but the per-consumer name is a load-bearing
// concept in production (one Deployment per consumer) so the helper
// stays parameterised.
//
//nolint:unparam // intentionally parameterised for readability/symmetry
func newOwnedConsumerDeployment(name, namespace, storeName string, replicas int32) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   storeName,
		"app.kubernetes.io/component":  "consumer",
		"app.kubernetes.io/managed-by": managedByLabel,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "consumer",
						Image: "placeholder",
					}},
				},
			},
		},
	}
}

// newOwnedFrontendService produces a minimal frontend Service
// carrying the labels ensureWorkloadService emits — used by C-flow
// tests that need a flip target without standing up a full
// ensureWorkloads pass. Frontend-only because that's what the C
// tests exercise; an admin/api variant can be added if a future
// test case needs it.
func newOwnedFrontendService(store *magev1alpha1.MageStore) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName(store, workloadFrontend),
			Namespace: store.Namespace,
			Labels:    workloadLabels(store, workloadFrontend),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: workloadSelector(store, workloadFrontend),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8080,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromString("http"),
			}},
		},
	}
}

// newReadyFrontendDeployment is the workload-Deployment counterpart
// to newOwnedConsumerDeployment — used by C5's stage-2 test to
// synthesise a frontend Deployment that workloadsReady can match
// against. Frontend-only for the same reason as above.
func newReadyFrontendDeployment(store *magev1alpha1.MageStore, replicas int32) *appsv1.Deployment {
	labels := workloadLabels(store, workloadFrontend)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName(store, workloadFrontend),
			Namespace: store.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "placeholder",
					}},
				},
			},
		},
	}
}

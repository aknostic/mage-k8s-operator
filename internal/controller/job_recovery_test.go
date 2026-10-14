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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// Tests cover the terminal-Failed Job recovery helper in isolation
// using a fake client (no envtest dependency). Each spec drives the
// helper's three observable paths — no-op, deletion, and permanent —
// plus the bookkeeping (Attempts.Count ramp, jitter bounds, central
// requeue duration).
var _ = Describe("recoverTerminalFailedJob", func() {
	const fixedTime = "2026-05-07T10:00:00Z"

	var (
		r          *MageStoreReconciler
		fakeClient client.Client
		fixedNow   time.Time
		// recordedRand drives the jitter helper. 0.5 → 0 % delta
		// (deterministic "no jitter" path used by most tests).
		recordedRand float64
	)

	parseTime := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339, s)
		Expect(err).NotTo(HaveOccurred())
		return t
	}

	makeJob := func(name string) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
		}
	}

	// markFailed stamps the JobFailed=True condition on a Job
	// (BackoffLimitExceeded is what kube-controller-manager writes
	// when --backoff-limit is exceeded).
	markFailed := func(j *batchv1.Job) {
		j.Status.Conditions = []batchv1.JobCondition{{
			Type:    batchv1.JobFailed,
			Status:  corev1.ConditionTrue,
			Reason:  "BackoffLimitExceeded",
			Message: "Job has reached the specified backoff limit",
		}}
	}

	markComplete := func(j *batchv1.Job) {
		j.Status.Conditions = []batchv1.JobCondition{{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
			Reason: "Complete",
		}}
	}

	BeforeEach(func() {
		fixedNow = parseTime(fixedTime)
		recordedRand = 0.5
		s := runtime.NewScheme()
		Expect(magev1alpha1.AddToScheme(s)).To(Succeed())
		Expect(scheme.AddToScheme(s)).To(Succeed())
		fakeClient = fake.NewClientBuilder().WithScheme(s).Build()
		r = &MageStoreReconciler{
			Client: fakeClient,
			Scheme: s,
			Now:    func() time.Time { return fixedNow },
			RandFloat: func() float64 {
				return recordedRand
			},
		}
	})

	Context("non-terminal Jobs", func() {
		It("returns Terminal=false for an in-progress Job and leaves attempts untouched", func() {
			job := makeJob("inflight")
			job.Status.Active = 1
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			out, err := r.recoverTerminalFailedJob(ctx, job, magev1alpha1.JobAttemptStatus{Count: 2})

			Expect(err).NotTo(HaveOccurred())
			Expect(out.Terminal).To(BeFalse())
			Expect(out.Deleted).To(BeFalse())
			Expect(out.Permanent).To(BeFalse())
			Expect(out.Attempts.Count).To(Equal(2))
			Expect(out.Attempts.LastFailedAt).To(BeNil())
		})

		It("resets Attempts to zero when the Job succeeded", func() {
			job := makeJob("done")
			markComplete(job)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			past := metav1.NewTime(fixedNow.Add(-time.Hour))
			input := magev1alpha1.JobAttemptStatus{Count: 3, LastFailedAt: &past, NextAttemptAt: &past}
			out, err := r.recoverTerminalFailedJob(ctx, job, input)

			Expect(err).NotTo(HaveOccurred())
			Expect(out.Terminal).To(BeFalse())
			Expect(out.Attempts).To(Equal(magev1alpha1.JobAttemptStatus{}))
		})
	})

	Context("first observation of a terminal-Failed Job", func() {
		It("increments Count to 1 and stamps LastFailedAt + NextAttemptAt", func() {
			job := makeJob("seed")
			markFailed(job)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			out, err := r.recoverTerminalFailedJob(ctx, job, magev1alpha1.JobAttemptStatus{})

			Expect(err).NotTo(HaveOccurred())
			Expect(out.Terminal).To(BeTrue())
			Expect(out.Deleted).To(BeFalse())
			Expect(out.Permanent).To(BeFalse())
			Expect(out.Attempts.Count).To(Equal(1))
			Expect(out.Attempts.LastFailedAt).NotTo(BeNil())
			Expect(out.Attempts.LastFailedAt.Time).To(Equal(fixedNow))
			Expect(out.Attempts.NextAttemptAt).NotTo(BeNil())
			// recordedRand=0.5 produces zero jitter delta; nominal[0]=30s.
			Expect(out.Attempts.NextAttemptAt.Time).To(Equal(fixedNow.Add(30 * time.Second)))

			// Still has the Job — first observation does not delete.
			var still batchv1.Job
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "seed", Namespace: "default"}, &still)).To(Succeed())
		})

		It("rolls Count from 1→2 across two observations of distinct Job runs", func() {
			job := makeJob("seed")
			markFailed(job)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			// First observation.
			out, err := r.recoverTerminalFailedJob(ctx, job, magev1alpha1.JobAttemptStatus{})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Attempts.Count).To(Equal(1))

			// Advance time past the first window; helper deletes the Job.
			fixedNow = fixedNow.Add(45 * time.Second)
			out, err = r.recoverTerminalFailedJob(ctx, job, out.Attempts)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Deleted).To(BeTrue())
			Expect(out.Attempts.NextAttemptAt).To(BeNil())
			// Count stays at 1 across the delete.
			Expect(out.Attempts.Count).To(Equal(1))

			// Caller's NotFound branch creates a fresh Job which again fails.
			job2 := makeJob("seed")
			markFailed(job2)
			Expect(fakeClient.Create(ctx, job2)).To(Succeed())

			// Second observation: Count goes 1→2, NextAttemptAt re-stamped.
			out, err = r.recoverTerminalFailedJob(ctx, job2, out.Attempts)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Attempts.Count).To(Equal(2))
			Expect(out.Attempts.NextAttemptAt).NotTo(BeNil())
			Expect(out.Attempts.NextAttemptAt.Time).To(Equal(fixedNow.Add(2 * time.Minute)))
		})
	})

	Context("inside the backoff window", func() {
		It("does not delete; keeps NextAttemptAt for the central requeue", func() {
			job := makeJob("seed")
			markFailed(job)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			lastFailed := metav1.NewTime(fixedNow.Add(-5 * time.Second))
			next := metav1.NewTime(fixedNow.Add(20 * time.Second))
			attempts := magev1alpha1.JobAttemptStatus{
				Count:         1,
				LastFailedAt:  &lastFailed,
				NextAttemptAt: &next,
			}

			out, err := r.recoverTerminalFailedJob(ctx, job, attempts)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Terminal).To(BeTrue())
			Expect(out.Deleted).To(BeFalse())
			Expect(out.Attempts.NextAttemptAt.Time).To(Equal(next.Time))

			var still batchv1.Job
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "seed", Namespace: "default"}, &still)).To(Succeed())
		})
	})

	Context("past the backoff window", func() {
		It("deletes the Job and clears NextAttemptAt", func() {
			job := makeJob("seed")
			markFailed(job)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			lastFailed := metav1.NewTime(fixedNow.Add(-5 * time.Minute))
			next := metav1.NewTime(fixedNow.Add(-1 * time.Minute))
			attempts := magev1alpha1.JobAttemptStatus{
				Count:         1,
				LastFailedAt:  &lastFailed,
				NextAttemptAt: &next,
			}

			out, err := r.recoverTerminalFailedJob(ctx, job, attempts)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Terminal).To(BeTrue())
			Expect(out.Deleted).To(BeTrue())
			// Both timestamps clear on delete so the next observation
			// (against the recreated Job) is treated as a fresh failure
			// and increments Count.
			Expect(out.Attempts.NextAttemptAt).To(BeNil())
			Expect(out.Attempts.LastFailedAt).To(BeNil())
			// Count persists for visibility.
			Expect(out.Attempts.Count).To(Equal(1))

			var deleted batchv1.Job
			err = fakeClient.Get(ctx, types.NamespacedName{Name: "seed", Namespace: "default"}, &deleted)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Job should be gone after recovery delete")
		})
	})

	Context("retry budget exhausted", func() {
		It("returns Permanent=true and does NOT delete the Job", func() {
			job := makeJob("seed")
			markFailed(job)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			// Count > len(jobBackoffSchedule) trips the cap.
			lastFailed := metav1.NewTime(fixedNow.Add(-time.Hour))
			next := metav1.NewTime(fixedNow.Add(-time.Hour))
			attempts := magev1alpha1.JobAttemptStatus{
				Count:         len(jobBackoffSchedule) + 1,
				LastFailedAt:  &lastFailed,
				NextAttemptAt: &next,
			}

			out, err := r.recoverTerminalFailedJob(ctx, job, attempts)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Permanent).To(BeTrue())
			Expect(out.Deleted).To(BeFalse())

			var still batchv1.Job
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "seed", Namespace: "default"}, &still)).To(Succeed())
		})
	})

	Context("backoff schedule", func() {
		DescribeTable("nominal durations match the v0.6.2 brief",
			func(attempt int, want time.Duration) {
				// recordedRand=0.5 → 0 % delta → nominal value.
				Expect(r.backoffWithJitter(attempt)).To(Equal(want))
			},
			Entry("attempt 1 → 30s", 1, 30*time.Second),
			Entry("attempt 2 → 2m", 2, 2*time.Minute),
			Entry("attempt 3 → 5m", 3, 5*time.Minute),
			Entry("attempt 4 → 10m", 4, 10*time.Minute),
			Entry("attempt 5 → 30m", 5, 30*time.Minute),
		)

		It("jitter is bounded to ±25 % of nominal across 100 trials", func() {
			// Stub randFloat with a deterministic sweep so each call
			// returns a different value across [0, 1).
			calls := 0
			r.RandFloat = func() float64 {
				v := float64(calls) / 100.0
				calls++
				return v
			}
			nominal := jobBackoffSchedule[0] // 30s
			min := nominal - time.Duration(float64(nominal)*jobBackoffJitter)
			max := nominal + time.Duration(float64(nominal)*jobBackoffJitter)

			for i := range 100 {
				got := r.backoffWithJitter(1)
				Expect(got).To(BeNumerically(">=", min), "trial %d: %s", i, got)
				Expect(got).To(BeNumerically("<=", max), "trial %d: %s", i, got)
			}
		})
	})

	Context("nextJobAttemptRequeue", func() {
		It("returns 0 when no JobAttempts have NextAttemptAt set", func() {
			store := &magev1alpha1.MageStore{}
			Expect(r.nextJobAttemptRequeue(store)).To(Equal(time.Duration(0)))
		})

		It("returns 0 when the map is empty", func() {
			store := &magev1alpha1.MageStore{
				Status: magev1alpha1.MageStoreStatus{
					JobAttempts: map[string]magev1alpha1.JobAttemptStatus{},
				},
			}
			Expect(r.nextJobAttemptRequeue(store)).To(Equal(time.Duration(0)))
		})

		It("picks the soonest NextAttemptAt across multiple Jobs", func() {
			soon := metav1.NewTime(fixedNow.Add(15 * time.Second))
			later := metav1.NewTime(fixedNow.Add(5 * time.Minute))
			store := &magev1alpha1.MageStore{
				Status: magev1alpha1.MageStoreStatus{
					JobAttempts: map[string]magev1alpha1.JobAttemptStatus{
						"a": {Count: 1, NextAttemptAt: &later},
						"b": {Count: 1, NextAttemptAt: &soon},
						"c": {Count: 0}, // no attempt scheduled
					},
				},
			}
			Expect(r.nextJobAttemptRequeue(store)).To(Equal(15 * time.Second))
		})

		It("returns a small positive duration when an attempt is already due", func() {
			past := metav1.NewTime(fixedNow.Add(-time.Minute))
			store := &magev1alpha1.MageStore{
				Status: magev1alpha1.MageStoreStatus{
					JobAttempts: map[string]magev1alpha1.JobAttemptStatus{
						"a": {Count: 1, NextAttemptAt: &past},
					},
				},
			}
			d := r.nextJobAttemptRequeue(store)
			Expect(d).To(BeNumerically(">", time.Duration(0)))
			Expect(d).To(BeNumerically("<=", 5*time.Second))
		})
	})

	Context("recordJobAttempts", func() {
		It("creates the map lazily on first write", func() {
			store := &magev1alpha1.MageStore{}
			recordJobAttempts(store, "media-seed", magev1alpha1.JobAttemptStatus{Count: 1})
			Expect(store.Status.JobAttempts).To(HaveLen(1))
			Expect(store.Status.JobAttempts["media-seed"].Count).To(Equal(1))
		})

		It("overwrites an existing entry", func() {
			store := &magev1alpha1.MageStore{
				Status: magev1alpha1.MageStoreStatus{
					JobAttempts: map[string]magev1alpha1.JobAttemptStatus{
						"media-seed": {Count: 1},
					},
				},
			}
			recordJobAttempts(store, "media-seed", magev1alpha1.JobAttemptStatus{Count: 2})
			Expect(store.Status.JobAttempts["media-seed"].Count).To(Equal(2))
		})
	})
})

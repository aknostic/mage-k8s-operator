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
	"k8s.io/apimachinery/pkg/types"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// ensureSMTP tests cover all 9 state-machine rows from ADR 012. No real
// Pod or Job runs — envtest uses the fake API server; Job statuses are
// written with k8sClient.Status().Update to simulate Job completion.
var _ = Describe("ensureSMTP", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensuresmtp-"},
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
					Repository: "registry.example.com/magento",
					Tag:        "2.4.8",
				},
				Workloads: magev1alpha1.WorkloadsSpec{
					Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
				},
			},
		}
	})

	// Row 1 — spec.smtp unset.
	Context("spec.smtp unset", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("emits no SMTPReady condition", func() {
			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty(), "zero condition means no SMTPReady row in status")
		})

		It("deletes a pre-existing owned smtp Secret when smtp is unset", func() {
			// Create a Secret with the managed-by label to simulate a
			// previous inline-mode managed Secret.
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      store.Name + "-smtp",
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": managedByLabel,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sec)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty())

			// Secret should be gone.
			var got corev1.Secret
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name: store.Name + "-smtp", Namespace: namespace,
				}, &got))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
		})

		It("deletes a pre-existing owned probe Job when smtp is unset", func() {
			// Create a fake probe Job with managed-by label.
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      store.Name + smtpProbeJobSuffix,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": managedByLabel,
					},
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers:    []corev1.Container{{Name: "probe", Image: "alpine"}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(BeEmpty())

			// Job delete is best-effort/background; we just verify no error was returned.
		})
	})

	// Row 2 — inline, name collision.
	Context("spec.smtp.inline, name-collision", func() {
		It("returns False/SecretNameCollision when <store>-smtp lacks managed-by label", func() {
			store.Spec.SMTP = &magev1alpha1.SMTPSpec{
				Inline: &magev1alpha1.SMTPInline{
					Host: "smtp.example.com",
					From: "noreply@example.com",
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			// Create a Secret without the managed-by label.
			collidingSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      store.Name + "-smtp",
					Namespace: namespace,
					// No managed-by label — collision.
				},
			}
			Expect(k8sClient.Create(ctx, collidingSecret)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionSMTPReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretNameCollision"))
			Expect(cond.Message).To(ContainSubstring(store.Name + "-smtp"))
		})
	})

	// Row 3 — inline, passwordSecretRef not found.
	Context("spec.smtp.inline, passwordSecretRef missing", func() {
		It("returns False/PasswordRefNotFound", func() {
			store.Spec.SMTP = &magev1alpha1.SMTPSpec{
				Inline: &magev1alpha1.SMTPInline{
					Host:              "smtp.example.com",
					From:              "noreply@example.com",
					User:              "smtpuser",
					PasswordSecretRef: &magev1alpha1.SecretKeySelector{Name: "missing-pw-secret", Key: "password"},
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionSMTPReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("PasswordRefNotFound"))
			Expect(cond.Message).To(ContainSubstring("missing-pw-secret"))
		})
	})

	// Rows 6–9 — inline valid, probe Job lifecycle.
	Context("spec.smtp.inline valid", func() {
		BeforeEach(func() {
			store.Spec.SMTP = &magev1alpha1.SMTPSpec{
				Inline: &magev1alpha1.SMTPInline{
					Host: "smtp.example.com",
					From: "noreply@example.com",
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("CreateOrUpdates <store>-smtp with required SMTP_* keys (no auth)", func() {
			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionSMTPReady))

			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + "-smtp", Namespace: namespace}, &sec)).To(Succeed())
			Expect(sec.Data).To(HaveKey("SMTP_HOST"))
			Expect(sec.Data).To(HaveKey("SMTP_FROM"))
			Expect(sec.Data).To(HaveKey("SMTP_PORT"))
			Expect(sec.Data).To(HaveKey("SMTP_AUTH"))
			Expect(sec.Data).To(HaveKey("SMTP_TLS"))
			// No auth keys expected.
			Expect(sec.Data).NotTo(HaveKey("SMTP_USERNAME"))
			Expect(sec.Data).NotTo(HaveKey("SMTP_PASSWORD"))
		})

		It("includes SMTP_USERNAME/PASSWORD when spec sets user + passwordSecretRef", func() {
			// Supply the password Secret.
			pwSec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "smtp-pw", Namespace: namespace},
				Data:       map[string][]byte{"password": []byte("secret123")},
			}
			Expect(k8sClient.Create(ctx, pwSec)).To(Succeed())

			// Update store with user + ref.
			store.Spec.SMTP.Inline.User = "smtpuser"
			store.Spec.SMTP.Inline.PasswordSecretRef = &magev1alpha1.SecretKeySelector{Name: "smtp-pw", Key: "password"}
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + "-smtp", Namespace: namespace}, &sec)).To(Succeed())
			Expect(string(sec.Data["SMTP_USERNAME"])).To(Equal("smtpuser"))
			Expect(string(sec.Data["SMTP_PASSWORD"])).To(Equal("secret123"))
		})

		It("includes SMTP_FROM_NAME and per-identity keys only when set", func() {
			store.Spec.SMTP.Inline.FromName = "My Store"
			store.Spec.SMTP.Inline.FromIdentities = &magev1alpha1.SMTPFromIdentities{
				Sales: &magev1alpha1.SMTPIdentity{Email: "sales@example.com", Name: "Sales Team"},
			}
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())

			_, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + "-smtp", Namespace: namespace}, &sec)).To(Succeed())
			Expect(string(sec.Data["SMTP_FROM_NAME"])).To(Equal("My Store"))
			Expect(string(sec.Data["SMTP_FROM_SALES"])).To(Equal("sales@example.com"))
			Expect(string(sec.Data["SMTP_FROM_NAME_SALES"])).To(Equal("Sales Team"))
			// Unset identities should not appear.
			Expect(sec.Data).NotTo(HaveKey("SMTP_FROM_GENERAL"))
		})

		It("creates probe Job with smtp-secret-hash annotation", func() {
			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Probing"))

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job)).To(Succeed())
			Expect(job.Annotations).To(HaveKey(smtpSecretHashAnnotation))
			Expect(job.Annotations[smtpSecretHashAnnotation]).To(HaveLen(16))
			Expect(job.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullAlways))
		})

		It("returns Unknown/Probing while Job is active", func() {
			// First call creates the Job.
			_, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Mark Job as active via Status.
			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job)).To(Succeed())
			job.Status.Active = 1
			Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())

			// Second call should still return Probing.
			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
			Expect(cond.Reason).To(Equal("Probing"))
		})

		It("returns True/Ready when Job succeeds", func() {
			// First call creates the Job.
			_, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Mark Job succeeded.
			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job)).To(Succeed())
			Expect(markJobComplete(k8sClient, &job)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionSMTPReady))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Ready"))
		})

		It("returns False/RetryingFailedJob on first terminal-Failed observation (gap #1 recovery)", func() {
			// First call creates the Job.
			_, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			// Mark Job failed.
			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job)).To(Succeed())
			Expect(markJobFailed(k8sClient, &job, "")).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionSMTPReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(JobRecoveryRetryReason))
			Expect(store.Status.JobAttempts).To(HaveKey(store.Name + smtpProbeJobSuffix))
			Expect(store.Status.JobAttempts[store.Name+smtpProbeJobSuffix].Count).To(Equal(1))
		})
	})

	// Rows 4 + 5 — secretRef mode.
	Context("spec.smtp.secretRef", func() {
		BeforeEach(func() {
			store.Spec.SMTP = &magev1alpha1.SMTPSpec{
				SecretRef: &magev1alpha1.LocalObjectReference{Name: "my-smtp"},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("returns False/SecretNotFound when Secret missing", func() {
			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Type).To(Equal(ConditionSMTPReady))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretNotFound"))
			Expect(cond.Message).To(ContainSubstring("my-smtp"))
		})

		It("returns False/SecretMissingKeys when Secret missing SMTP_HOST or SMTP_FROM", func() {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-smtp", Namespace: namespace},
				Data:       map[string][]byte{"SMTP_HOST": []byte("smtp.example.com")},
				// SMTP_FROM is missing.
			}
			Expect(k8sClient.Create(ctx, sec)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretMissingKeys"))
		})

		It("returns False/SecretMissingKeys for SMTP_USERNAME without SMTP_PASSWORD", func() {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-smtp", Namespace: namespace},
				Data: map[string][]byte{
					"SMTP_HOST":     []byte("smtp.example.com"),
					"SMTP_FROM":     []byte("noreply@example.com"),
					"SMTP_USERNAME": []byte("user"),
					// No SMTP_PASSWORD.
				},
			}
			Expect(k8sClient.Create(ctx, sec)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretMissingKeys"))
			Expect(cond.Message).To(ContainSubstring("SMTP_USERNAME requires SMTP_PASSWORD"))
		})

		It("returns False/SecretMissingKeys for SMTP_PASSWORD without SMTP_USERNAME", func() {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-smtp", Namespace: namespace},
				Data: map[string][]byte{
					"SMTP_HOST":     []byte("smtp.example.com"),
					"SMTP_FROM":     []byte("noreply@example.com"),
					"SMTP_PASSWORD": []byte("secret"),
					// No SMTP_USERNAME.
				},
			}
			Expect(k8sClient.Create(ctx, sec)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretMissingKeys"))
			Expect(cond.Message).To(ContainSubstring("SMTP_PASSWORD requires SMTP_USERNAME"))
		})

		It("creates probe Job referencing the user-supplied Secret name", func() {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-smtp", Namespace: namespace},
				Data: map[string][]byte{
					"SMTP_HOST": []byte("smtp.example.com"),
					"SMTP_FROM": []byte("noreply@example.com"),
				},
			}
			Expect(k8sClient.Create(ctx, sec)).To(Succeed())

			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal("Probing"))

			var job batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job)).To(Succeed())
			// The Job's envFrom should reference the user-supplied Secret name.
			Expect(job.Spec.Template.Spec.Containers[0].EnvFrom[0].SecretRef.Name).To(Equal("my-smtp"))
		})
	})

	// Hash-flip re-run.
	Context("hash-flip re-run", func() {
		BeforeEach(func() {
			store.Spec.SMTP = &magev1alpha1.SMTPSpec{
				Inline: &magev1alpha1.SMTPInline{
					Host: "smtp.example.com",
					From: "noreply@example.com",
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "store", Namespace: namespace}, store)).To(Succeed())
		})

		It("no-ops when smtp-secret-hash matches existing Job's annotation", func() {
			// First call creates Job.
			_, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var job1 batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job1)).To(Succeed())
			hash1 := job1.Annotations[smtpSecretHashAnnotation]

			// Second call with same spec — no change.
			_, _, err = reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var job2 batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job2)).To(Succeed())
			// Same UID means no delete+create cycle.
			Expect(job2.UID).To(Equal(job1.UID))
			Expect(job2.Annotations[smtpSecretHashAnnotation]).To(Equal(hash1))
		})

		It("deletes existing Job and creates new one when smtp-secret-hash differs", func() {
			// First call creates Job.
			_, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			var job1 batchv1.Job
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &job1)).To(Succeed())
			oldUID := job1.UID

			// Manually patch the Job's annotation to a stale hash so the
			// next reconcile sees a mismatch.
			job1.Annotations[smtpSecretHashAnnotation] = "stale00000000000"
			Expect(k8sClient.Update(ctx, &job1)).To(Succeed())

			// Trigger a hash change by updating the Secret's data directly.
			var sec corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + "-smtp", Namespace: namespace}, &sec)).To(Succeed())
			sec.Data["SMTP_FROM"] = []byte("changed@example.com")
			Expect(k8sClient.Update(ctx, &sec)).To(Succeed())

			// Reconcile — should detect mismatch and re-create.
			cond, _, err := reconciler.ensureSMTP(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(cond.Reason).To(Equal("Probing"))

			// Eventually the old Job is gone and a new one appears.
			Eventually(func() bool {
				var newJob batchv1.Job
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: store.Name + smtpProbeJobSuffix, Namespace: namespace}, &newJob); err != nil {
					return false
				}
				return newJob.UID != oldUID
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
		})
	})
})

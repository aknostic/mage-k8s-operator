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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

var _ = Describe("MageStore CRD validation — spec.smtp", func() {
	const namespace = "default"
	ctx := context.Background()

	It("rejects spec.smtp: {} (no mode set)", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-no-mode"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("exactly one of inline or secretRef must be set")))
	})

	It("rejects spec.smtp.inline with user but no passwordSecretRef", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-user-no-password"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			Inline: &magev1alpha1.SMTPInline{
				Host: "smtp.example.com",
				From: "noreply@example.com",
				User: "smtpuser",
				// no PasswordSecretRef — CEL parity rule should fire
			},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("user and passwordSecretRef must both")))
	})

	It("rejects spec.smtp.inline.tls outside enum", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-bad-tls"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			Inline: &magev1alpha1.SMTPInline{
				Host: "smtp.example.com",
				From: "noreply@example.com",
				TLS:  "ssl",
			},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("spec.smtp.inline.tls")))
		Expect(err).To(MatchError(ContainSubstring(`Unsupported value: "ssl"`)))
	})

	It("rejects spec.smtp.inline.auth outside enum", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-bad-auth"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			Inline: &magev1alpha1.SMTPInline{
				Host: "smtp.example.com",
				From: "noreply@example.com",
				Auth: "kerberos",
			},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("spec.smtp.inline.auth")))
		Expect(err).To(MatchError(ContainSubstring(`Unsupported value: "kerberos"`)))
	})

	It("rejects spec.smtp.inline with passwordSecretRef but no user (parity, other direction)", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-password-no-user"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			Inline: &magev1alpha1.SMTPInline{
				Host:              "smtp.example.com",
				From:              "noreply@example.com",
				PasswordSecretRef: &magev1alpha1.SecretKeySelector{Name: "smtp-creds", Key: "password"},
				// no User — CEL parity rule should fire in this direction too
			},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("user and passwordSecretRef must both")))
	})

	It("accepts spec.smtp.inline with valid fields", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-inline-valid"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			Inline: &magev1alpha1.SMTPInline{
				Host: "smtp.example.com",
				From: "noreply@example.com",
				Port: 587,
				TLS:  "starttls",
			},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		_ = k8sClient.Delete(ctx, store)
	})

	It("accepts spec.smtp.secretRef with valid name", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-secretref-valid"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			SecretRef: &magev1alpha1.LocalObjectReference{Name: "my-smtp"},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).NotTo(HaveOccurred())
		_ = k8sClient.Delete(ctx, store)
	})

	It("rejects setting both inline AND secretRef", func() {
		store := newMinimalMageStore(namespace)
		store.Name = "smtp-both-modes"
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			Inline: &magev1alpha1.SMTPInline{
				Host: "smtp.example.com",
				From: "noreply@example.com",
			},
			SecretRef: &magev1alpha1.LocalObjectReference{Name: "my-smtp"},
		}
		err := k8sClient.Create(ctx, store)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ContainSubstring("exactly one of inline or secretRef must be set")))
	})
})

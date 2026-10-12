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

// ensure_bootstrap_test.go covers buildBootstrapScript / deriveBootstrapURLs.
// These are pure functions — no API calls, no r.Client/r.Scheme access —
// so no envtest setup is required.  A zero-value MageStoreReconciler works
// as receiver for buildBootstrapJob; the tests call buildBootstrapScript
// directly to keep assertions focused.
//
// URL derivation rules follow ADR 013 §"URL derivation matrix at default
// scope".  The five table entries map the full matrix:
//   nil stores / nil TLS     → http://localhost:8080/   useSecure=0
//   nil stores / TLS set     → base http, secure https  useSecure=1
//   stores set / nil TLS     → http://shop.example.com/ useSecure=0
//   stores set / TLS set     → https://shop.example.com/ useSecure=1
//   empty stores / nil TLS   → http://localhost:8080/   useSecure=0

import (
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

var _ = Describe("buildBootstrapScript", func() {
	// minimalBootstrapStore returns the smallest MageStore that
	// buildBootstrapScript touches.  Fields not relevant to URL derivation
	// are left at their zero values.
	minimalBootstrapStore := func() *magev1alpha1.MageStore {
		return &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-store",
				Namespace: "default",
			},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/magento",
					Tag:        "2.4.8",
				},
				Bootstrap: &magev1alpha1.BootstrapSpec{
					Admin: magev1alpha1.AdminSpec{
						Email: "admin@example.com",
					},
				},
			},
		}
	}

	type urlEntry struct {
		mutate                 func(*magev1alpha1.MageStore)
		expectedBaseURL        string
		expectedSecure         string
		expectedUseSecure      int
		expectedUseSecureAdmin int
	}

	DescribeTable("URL derivation matrix (ADR 013)",
		func(e urlEntry) {
			store := minimalBootstrapStore()
			if e.mutate != nil {
				e.mutate(store)
			}

			script := buildBootstrapScript(store)

			Expect(script).To(ContainSubstring("--base-url="+e.expectedBaseURL),
				"base-url should match")
			Expect(script).To(ContainSubstring("--base-url-secure="+e.expectedSecure),
				"base-url-secure should match")
			Expect(script).To(ContainSubstring(
				"--use-secure="+strconv.Itoa(e.expectedUseSecure)),
				"use-secure should match")
			Expect(script).To(ContainSubstring(
				"--use-secure-admin="+strconv.Itoa(e.expectedUseSecureAdmin)),
				"use-secure-admin should match")
		},

		Entry("nil stores, nil TLS → localhost http, useSecure=0",
			urlEntry{
				mutate:                 nil,
				expectedBaseURL:        "http://localhost:8080/",
				expectedSecure:         "http://localhost:8080/",
				expectedUseSecure:      0,
				expectedUseSecureAdmin: 0,
			},
		),

		Entry("nil stores, TLS set → localhost base=http secure=https, useSecure=1",
			urlEntry{
				mutate: func(s *magev1alpha1.MageStore) {
					s.Spec.Routing = &magev1alpha1.RoutingSpec{
						TLS: &magev1alpha1.TLSSpec{},
					}
				},
				expectedBaseURL:        "http://localhost:8080/",
				expectedSecure:         "https://localhost:8080/",
				expectedUseSecure:      1,
				expectedUseSecureAdmin: 1,
			},
		),

		Entry("stores set, nil TLS → real host http, useSecure=0",
			urlEntry{
				mutate: func(s *magev1alpha1.MageStore) {
					s.Spec.Stores = []magev1alpha1.StoreSpec{
						{Host: "shop.example.com", Code: "default"},
					}
				},
				expectedBaseURL:        "http://shop.example.com/",
				expectedSecure:         "http://shop.example.com/",
				expectedUseSecure:      0,
				expectedUseSecureAdmin: 0,
			},
		),

		Entry("stores set, TLS set → real host https both, useSecure=1",
			urlEntry{
				mutate: func(s *magev1alpha1.MageStore) {
					s.Spec.Stores = []magev1alpha1.StoreSpec{
						{Host: "shop.example.com", Code: "default"},
					}
					s.Spec.Routing = &magev1alpha1.RoutingSpec{
						TLS: &magev1alpha1.TLSSpec{},
					}
				},
				expectedBaseURL:        "https://shop.example.com/",
				expectedSecure:         "https://shop.example.com/",
				expectedUseSecure:      1,
				expectedUseSecureAdmin: 1,
			},
		),

		Entry("empty stores slice, nil TLS → treated as unset → localhost http, useSecure=0",
			urlEntry{
				mutate: func(s *magev1alpha1.MageStore) {
					s.Spec.Stores = []magev1alpha1.StoreSpec{}
				},
				expectedBaseURL:        "http://localhost:8080/",
				expectedSecure:         "http://localhost:8080/",
				expectedUseSecure:      0,
				expectedUseSecureAdmin: 0,
			},
		),
	)
})

var _ = Describe("buildBootstrapJob SMTP envFrom", func() {
	// minimalStore constructs the smallest MageStore that buildBootstrapJob
	// can operate on without panicking — admin email and image fields must
	// be present; bootstrap spec must be non-nil.
	minimalStore := func() *magev1alpha1.MageStore {
		return &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{Name: "mystore", Namespace: "default"},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/magento",
					Tag:        "2.4.8",
				},
				Bootstrap: &magev1alpha1.BootstrapSpec{
					Admin: magev1alpha1.AdminSpec{Email: "admin@example.com"},
				},
			},
		}
	}

	r := &MageStoreReconciler{}
	adminRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "mystore-admin"},
		Key:                  "password",
	}
	frontnameRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "mystore-admin"},
		Key:                  "backendFrontname",
	}

	It("does NOT mount SMTP envFrom when spec.smtp is unset", func() {
		store := minimalStore()
		job := r.buildBootstrapJob(store, "mystore-bootstrap", adminRef, frontnameRef, "")

		container := job.Spec.Template.Spec.Containers[0]
		for _, ef := range container.EnvFrom {
			if ef.SecretRef != nil {
				Expect(ef.SecretRef.Name).NotTo(HaveSuffix("-smtp"), "unexpected SMTP envFrom when spec.smtp is unset")
			}
		}
	})

	It("uses imagePullPolicy: Always so customer image rebuilds under the same tag are picked up", func() {
		store := minimalStore()
		job := r.buildBootstrapJob(store, "mystore-bootstrap", adminRef, frontnameRef, "")
		Expect(job.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullAlways))
	})

	It("mounts <store>-smtp envFrom (without optional: true) when spec.smtp.inline is set", func() {
		store := minimalStore()
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			Inline: &magev1alpha1.SMTPInline{
				Host: "smtp.example.com",
				From: "no-reply@example.com",
			},
		}
		job := r.buildBootstrapJob(store, "mystore-bootstrap", adminRef, frontnameRef, "")

		found := false
		for _, ef := range job.Spec.Template.Spec.Containers[0].EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == "mystore-smtp" {
				found = true
				Expect(ef.SecretRef.Optional).To(BeNil())
			}
		}
		Expect(found).To(BeTrue(), "expected envFrom entry for mystore-smtp")
	})

	It("mounts the user-supplied Secret name when spec.smtp.secretRef is set", func() {
		store := minimalStore()
		store.Spec.SMTP = &magev1alpha1.SMTPSpec{
			SecretRef: &magev1alpha1.LocalObjectReference{Name: "custom-smtp"},
		}
		job := r.buildBootstrapJob(store, "mystore-bootstrap", adminRef, frontnameRef, "")

		found := false
		for _, ef := range job.Spec.Template.Spec.Containers[0].EnvFrom {
			if ef.SecretRef != nil && ef.SecretRef.Name == "custom-smtp" {
				found = true
			}
		}
		Expect(found).To(BeTrue(), "expected envFrom entry for custom-smtp")

		for _, ef := range job.Spec.Template.Spec.Containers[0].EnvFrom {
			if ef.SecretRef != nil {
				Expect(ef.SecretRef.Name).NotTo(Equal("mystore-smtp"), "inline smtp name must not appear in secretRef mode")
			}
		}
	})

	It("sets smtp-secret-hash annotation on pod template when smtpSecretHash is non-empty", func() {
		store := minimalStore()
		job := r.buildBootstrapJob(store, "mystore-bootstrap", adminRef, frontnameRef, "hashvalue1")

		Expect(job.Spec.Template.Annotations).To(HaveKeyWithValue(smtpSecretHashAnnotation, "hashvalue1"))
	})

	It("does not set smtp-secret-hash annotation when smtpSecretHash is empty", func() {
		store := minimalStore()
		job := r.buildBootstrapJob(store, "mystore-bootstrap", adminRef, frontnameRef, "")

		Expect(job.Spec.Template.Annotations).NotTo(HaveKey(smtpSecretHashAnnotation))
	})
})

var _ = Describe("trans_email rewrite (gap #11 + per-identity, ADR 012)", func() {
	// minimalBootstrapStore returns the smallest MageStore that
	// buildBootstrapScript touches — matches the helper in the URL matrix
	// describe block above.
	minimalBootstrapStore := func() *magev1alpha1.MageStore {
		return &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-store",
				Namespace: "default",
			},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/magento",
					Tag:        "2.4.8",
				},
				Bootstrap: &magev1alpha1.BootstrapSpec{
					Admin: magev1alpha1.AdminSpec{
						Email: "admin@example.com",
					},
				},
			},
		}
	}

	r := &MageStoreReconciler{}
	adminRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "test-store-admin"},
		Key:                  "password",
	}
	frontnameRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "test-store-admin"},
		Key:                  "backendFrontname",
	}

	It("skips the rewrite block entirely when SMTP_FROM is unset", func() {
		// Build a script with spec.smtp == nil. The template's shell-level
		// guard is what we assert against.
		store := minimalBootstrapStore()
		job := r.buildBootstrapJob(store, "test-bootstrap", adminRef, frontnameRef, "")
		script := job.Spec.Template.Spec.Containers[0].Args[0]

		// The script CONTAINS the gating `if [ -n "${SMTP_FROM:-}" ]`.
		Expect(script).To(ContainSubstring(`if [ -n "${SMTP_FROM:-}" ]`))
		// The script does NOT do the silent admin-email fallback.
		Expect(script).NotTo(ContainSubstring(`FROM_ADDR="${SMTP_FROM:-${ADMIN_EMAIL}}"`))
		Expect(script).NotTo(ContainSubstring(`${SMTP_FROM:-${ADMIN_EMAIL}}`))
		// The else-branch documents the failure mode.
		Expect(script).To(ContainSubstring(`SMTP_FROM unset → skipping trans_email rewrite`))
	})

	It("uses per-identity SMTP_FROM_<IDENT> with fallback to SMTP_FROM", func() {
		store := minimalBootstrapStore()
		script := buildBootstrapScript(store)
		Expect(script).To(ContainSubstring(`addr="${SMTP_FROM_SALES:-$SMTP_FROM}"`))
		Expect(script).To(ContainSubstring(`addr="${SMTP_FROM_GENERAL:-$SMTP_FROM}"`))
		Expect(script).To(ContainSubstring(`addr="${SMTP_FROM_SUPPORT:-$SMTP_FROM}"`))
		Expect(script).To(ContainSubstring(`addr="${SMTP_FROM_CUSTOM1:-$SMTP_FROM}"`))
		Expect(script).To(ContainSubstring(`addr="${SMTP_FROM_CUSTOM2:-$SMTP_FROM}"`))
	})

	It("config:sets both email AND name when SMTP_FROM_NAME_<IDENT> is present", func() {
		store := minimalBootstrapStore()
		script := buildBootstrapScript(store)
		// Branches on `if [ -n "$name" ]`, runs config:set trans_email/ident_*/name only when set.
		Expect(script).To(ContainSubstring(`if [ -n "$name" ]`))
		Expect(script).To(ContainSubstring(`trans_email/ident_${ident}/name`))
	})

	It("uses explicit case statement, not eval", func() {
		store := minimalBootstrapStore()
		script := buildBootstrapScript(store)
		// Defensive: the previous shape was vulnerable to eval injection if anyone
		// refactored to `eval addr=\$SMTP_FROM_${ident^^}`. Make sure no eval landed.
		Expect(script).NotTo(ContainSubstring("eval"))
		Expect(script).To(ContainSubstring(`case "$ident" in`))
	})
})

var _ = Describe("disableTFA opt-in (gap #4)", func() {
	minimalBootstrapStore := func() *magev1alpha1.MageStore {
		return &magev1alpha1.MageStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-store",
				Namespace: "default",
			},
			Spec: magev1alpha1.MageStoreSpec{
				Image: magev1alpha1.MageStoreImage{
					Repository: "registry.example.com/magento",
					Tag:        "2.4.8",
				},
				Bootstrap: &magev1alpha1.BootstrapSpec{
					Admin: magev1alpha1.AdminSpec{
						Email: "admin@example.com",
					},
				},
			},
		}
	}

	It("appends TFA-disable commands when spec.bootstrap.admin.disableTFA: true", func() {
		store := minimalBootstrapStore()
		store.Spec.Bootstrap.Admin.DisableTFA = true
		script := buildBootstrapScript(store)

		Expect(script).To(ContainSubstring("twofactorauth/general/enable 0"))
		Expect(script).To(ContainSubstring("module:disable Magento_TwoFactorAuth Magento_AdminAdobeImsTwoFactorAuth"))
		Expect(script).To(ContainSubstring("setup:upgrade"))
		// Full regen, not --keep-generated which corrupted /app/generated/code
		// in a previous session — verify the bootstrap script doesn't sneak it in.
		Expect(script).NotTo(ContainSubstring("--keep-generated"))
	})

	It("does NOT append TFA-disable when DisableTFA is false (default)", func() {
		store := minimalBootstrapStore()
		// DisableTFA defaults to false (zero value).
		script := buildBootstrapScript(store)

		Expect(script).NotTo(ContainSubstring("twofactorauth/general/enable"))
		Expect(script).NotTo(ContainSubstring("module:disable Magento_TwoFactorAuth"))
	})

	It("does NOT append TFA-disable when spec.bootstrap is nil (adopted DB)", func() {
		store := minimalBootstrapStore()
		store.Spec.Bootstrap = nil
		script := buildBootstrapScript(store)

		Expect(script).NotTo(ContainSubstring("twofactorauth/general/enable"))
	})
})

var _ = Describe("newAdminPassword", func() {
	const digits = "0123456789"
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

	// Magento 2.4 setup:install rejects passwords missing either class
	// with "Your password must include both numeric and alphabetic
	// characters." 1000 iterations dwarfs the ~4% per-call probability
	// that a naive base64 encoding lands on zero digits, so any failure
	// here means the class-check loop in newAdminPassword is broken.
	It("always contains at least one digit and one letter (1000 iterations)", func() {
		for i := range 1000 {
			pw := newAdminPassword()
			Expect(pw).To(HaveLen(20), "iteration %d: %q", i, pw)
			Expect(strings.ContainsAny(pw, digits)).To(BeTrue(), "iteration %d: %q has no digit", i, pw)
			Expect(strings.ContainsAny(pw, letters)).To(BeTrue(), "iteration %d: %q has no letter", i, pw)
		}
	})
})

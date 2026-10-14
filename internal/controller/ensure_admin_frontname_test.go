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

// ensure_admin_frontname_test.go covers the v0.7.0 admin URL randomization
// — gap #3 of the security-baseline arc. Three concerns split into three
// Describes:
//
//   1. Pure-function helpers (newAdminBackendFrontname, deriveAdminURL,
//      buildBootstrapScript substitution, buildBootstrapJob env var).
//   2. Stateful ensureAdminBackendFrontname behavior via envtest —
//      fresh-store random generation, sticky-once, ADR 014 upgrade-safety
//      fallback to literal "admin" for existing v0.6.x stores.
//   3. CRD-level regex validation on AdminSpec.BackendFrontname.

import (
	"context"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	magev1alpha1 "github.com/aknostic/mage-k8s-operator/api/v1alpha1"
)

// adminPathsFromHTTPRoute returns the PathPrefix values whose backend is
// the admin tier — used by the routing tests below to assert the rule
// builder writes /<frontname> rather than literal /admin.
func adminPathsFromHTTPRoute(rt *gatewayv1.HTTPRoute, adminBackend string) []string {
	var out []string
	for _, rule := range rt.Spec.Rules {
		if len(rule.BackendRefs) == 0 || string(rule.BackendRefs[0].Name) != adminBackend {
			continue
		}
		for _, m := range rule.Matches {
			if m.Path != nil && m.Path.Value != nil {
				out = append(out, *m.Path.Value)
			}
		}
	}
	return out
}

var _ = Describe("admin URL randomization — pure helpers", func() {
	Describe("newAdminBackendFrontname", func() {
		It("returns a 16-character alphanumeric string passing the AdminSpec regex", func() {
			pattern := regexp.MustCompile(`^[a-zA-Z0-9_-]{4,32}$`)
			alphanum := regexp.MustCompile(`^[a-zA-Z0-9]+$`)
			for range 50 {
				v := newAdminBackendFrontname()
				Expect(v).To(HaveLen(16))
				Expect(pattern.MatchString(v)).To(BeTrue(), "value %q must satisfy AdminSpec.BackendFrontname regex", v)
				Expect(alphanum.MatchString(v)).To(BeTrue(), "operator-generated default uses [a-zA-Z0-9] only (regex allows _- but generator excludes them)")
			}
		})

		It("draws fresh randomness — two consecutive calls do not collide", func() {
			a, b := newAdminBackendFrontname(), newAdminBackendFrontname()
			Expect(a).NotTo(Equal(b), "collisions in 95 bits of entropy indicate a seeding bug, not improbable luck")
		})
	})

	Describe("deriveAdminURL", func() {
		// Mirrors the four-row matrix from ADR 013 §"URL derivation matrix
		// at default scope" — the admin URL is just default-scope baseURL
		// with the frontname appended.
		minimalStore := func() *magev1alpha1.MageStore {
			return &magev1alpha1.MageStore{
				ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: "default"},
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

		It("falls back to localhost:8080 (http) when no stores configured", func() {
			Expect(deriveAdminURL(minimalStore(), "secretadmin")).
				To(Equal("http://localhost:8080/secretadmin/"))
		})

		It("uses real host on http when stores set, no TLS", func() {
			s := minimalStore()
			s.Spec.Stores = []magev1alpha1.StoreSpec{{Host: "shop.example.com", Code: "default"}}
			Expect(deriveAdminURL(s, "secretadmin")).
				To(Equal("http://shop.example.com/secretadmin/"))
		})

		It("uses real host on https when stores + TLS set", func() {
			s := minimalStore()
			s.Spec.Stores = []magev1alpha1.StoreSpec{{Host: "shop.example.com", Code: "default"}}
			s.Spec.Routing = &magev1alpha1.RoutingSpec{TLS: &magev1alpha1.TLSSpec{}}
			Expect(deriveAdminURL(s, "secretadmin")).
				To(Equal("https://shop.example.com/secretadmin/"))
		})

		It("uses localhost on https when TLS set but no stores (pathological row)", func() {
			s := minimalStore()
			s.Spec.Routing = &magev1alpha1.RoutingSpec{TLS: &magev1alpha1.TLSSpec{}}
			Expect(deriveAdminURL(s, "secretadmin")).
				To(Equal("https://localhost:8080/secretadmin/"))
		})
	})

	Describe("buildBootstrapScript", func() {
		// Regression guard: gap #3 replaces literal --backend-frontname=admin
		// with shell-expanded ${ADMIN_BACKEND_FRONTNAME}. The literal must
		// not reappear in the template.
		minimalStore := func() *magev1alpha1.MageStore {
			return &magev1alpha1.MageStore{
				ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: "default"},
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

		It(`substitutes "${ADMIN_BACKEND_FRONTNAME}" not literal "admin"`, func() {
			script := buildBootstrapScript(minimalStore())
			Expect(script).To(ContainSubstring(`--backend-frontname="${ADMIN_BACKEND_FRONTNAME}"`))
			Expect(script).NotTo(ContainSubstring(`--backend-frontname=admin`),
				"literal `admin` would defeat the gap #3 randomization")
		})
	})

	Describe("buildBootstrapJob", func() {
		// Asserts the new ADMIN_BACKEND_FRONTNAME env var is sourced via
		// SecretKeyRef from the same <store>-admin Secret as ADMIN_PASSWORD.
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
		adminPasswordRef := corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "mystore-admin"},
			Key:                  "password",
		}
		frontnameRef := corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "mystore-admin"},
			Key:                  "backendFrontname",
		}

		It("adds ADMIN_BACKEND_FRONTNAME env var sourced from <store>-admin.backendFrontname", func() {
			job := r.buildBootstrapJob(minimalStore(), "mystore-bootstrap", adminPasswordRef, frontnameRef, "")
			env := job.Spec.Template.Spec.Containers[0].Env

			var found bool
			for _, e := range env {
				if e.Name == "ADMIN_BACKEND_FRONTNAME" {
					found = true
					Expect(e.ValueFrom).NotTo(BeNil())
					Expect(e.ValueFrom.SecretKeyRef).NotTo(BeNil())
					Expect(e.ValueFrom.SecretKeyRef.Name).To(Equal("mystore-admin"))
					Expect(e.ValueFrom.SecretKeyRef.Key).To(Equal("backendFrontname"))
					// The new env var must NOT carry a static Value — that
					// would override the SecretKeyRef on most kubelets and
					// is the obvious regression to guard.
					Expect(e.Value).To(BeEmpty())
				}
			}
			Expect(found).To(BeTrue(), "expected ADMIN_BACKEND_FRONTNAME env var on bootstrap container")
		})
	})
})

var _ = Describe("ensureAdminBackendFrontname — envtest", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "admin-frontname-"}}
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
	})

	Context("fresh store (no <store>-admin Secret yet)", func() {
		It("generates a 16-char random value and persists backendFrontname + adminUrl", func() {
			value, ref, err := reconciler.ensureAdminBackendFrontname(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(value).To(MatchRegexp(`^[a-zA-Z0-9]{16}$`),
				"fresh-store path must generate the operator's 16-char default")

			Expect(ref.Name).To(Equal(store.Name + "-admin"))
			Expect(ref.Key).To(Equal("backendFrontname"))

			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + "-admin", Namespace: namespace,
			}, sec)).To(Succeed())
			Expect(sec.Data).To(HaveKeyWithValue("backendFrontname", []byte(value)))
			Expect(sec.Data).To(HaveKey("adminUrl"))
			Expect(string(sec.Data["adminUrl"])).To(HavePrefix("http://localhost:8080/"))
			Expect(string(sec.Data["adminUrl"])).To(HaveSuffix("/" + value + "/"))
		})

		It("is sticky-once — second call returns the same value, does not rotate", func() {
			first, _, err := reconciler.ensureAdminBackendFrontname(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			second, _, err := reconciler.ensureAdminBackendFrontname(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			Expect(second).To(Equal(first), "sticky-once-generated guarantee")
		})
	})

	Context("existing v0.6.x store (admin Secret exists with password but no backendFrontname)", func() {
		BeforeEach(func() {
			// Simulate a v0.6.x admin Secret — only `password` key. The
			// upgrade-safety rule must detect this and default to literal
			// "admin" to preserve existing bookmarks per ADR 014.
			pre := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      store.Name + "-admin",
					Namespace: namespace,
				},
				StringData: map[string]string{"password": "pre-existing-password"},
			}
			Expect(k8sClient.Create(ctx, pre)).To(Succeed())
		})

		It(`falls back to literal "admin" and persists the value for stickiness`, func() {
			value, _, err := reconciler.ensureAdminBackendFrontname(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(value).To(Equal("admin"),
				"ADR 014 upgrade-safety: existing v0.6.x stores keep /admin/ to preserve bookmarks")

			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + "-admin", Namespace: namespace,
			}, sec)).To(Succeed())
			Expect(sec.Data).To(HaveKeyWithValue("backendFrontname", []byte("admin")))
			Expect(sec.Data).To(HaveKeyWithValue("password", []byte("pre-existing-password")),
				"password from the pre-existing Secret must not be touched")
		})

		It("a follow-up reconcile remains stuck on admin (sticky-once)", func() {
			_, _, err := reconciler.ensureAdminBackendFrontname(ctx, store)
			Expect(err).NotTo(HaveOccurred())

			value, _, err := reconciler.ensureAdminBackendFrontname(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(value).To(Equal("admin"))
		})
	})

	Context("explicit AdminSpec.BackendFrontname", func() {
		It("uses the user-supplied value verbatim and persists it", func() {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
			store.Spec.Bootstrap.Admin.BackendFrontname = "secretadmin"
			Expect(k8sClient.Update(ctx, store)).To(Succeed())

			value, _, err := reconciler.ensureAdminBackendFrontname(ctx, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(value).To(Equal("secretadmin"))

			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: store.Name + "-admin", Namespace: namespace,
			}, sec)).To(Succeed())
			Expect(sec.Data).To(HaveKeyWithValue("backendFrontname", []byte("secretadmin")))
			Expect(sec.Data).To(HaveKeyWithValue("adminUrl", []byte("http://localhost:8080/secretadmin/")))
		})
	})
})

var _ = Describe("HTTPRoute admin path uses resolved frontname", func() {
	// Regression guard: pre-fix the HTTPRoute baked literal `/admin`
	// regardless of `--backend-frontname`, so a fresh-install Magento
	// served at `/<random>/` was still reachable via `/admin`.
	// Post-fix, ensureRouting reads `<store>-admin` Secret's
	// `backendFrontname` key (with literal `admin` fallback) and
	// builds the rule as `/<frontname>` → admin backend.
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "admin-frontname-routing-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Capabilities: fullCapabilities(),
		}

		store = newMinimalMageStore(namespace)
		store.Spec.Workloads.Admin = &magev1alpha1.WorkloadSpec{Replicas: 1}
		Expect(k8sClient.Create(ctx, store)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
	})

	It("falls back to literal /admin when no <store>-admin Secret yet (adoption case)", func() {
		// No Secret pre-created — this is the path existing v0.6.x
		// code took, and what every existing routing test exercises.
		// Verifies the v0.7.0 change didn't break the fallback.
		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		rt := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, rt)).To(Succeed())
		paths := adminPathsFromHTTPRoute(rt, store.Name+"-admin")
		Expect(paths).To(ContainElement("/admin"),
			"adoption case (no admin Secret) must fall back to literal /admin")
	})

	It("uses /<frontname> when <store>-admin Secret carries a randomized backendFrontname", func() {
		// Simulate post-bootstrap state: ensureAdminBackendFrontname has
		// run and persisted a random URL-safe value to the Secret.
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: store.Name + "-admin", Namespace: namespace},
			StringData: map[string]string{
				"password":         "doesntmatter",
				"backendFrontname": "7Kx2pQrN9mZb4Tv8",
			},
		})).To(Succeed())

		Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

		rt := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, rt)).To(Succeed())
		paths := adminPathsFromHTTPRoute(rt, store.Name+"-admin")
		Expect(paths).To(ContainElement("/7Kx2pQrN9mZb4Tv8"),
			"randomized frontname must propagate to the HTTPRoute admin rule")
		Expect(paths).NotTo(ContainElement("/admin"),
			"literal /admin must NOT appear when randomized — defeats gap #3")
	})
})

var _ = Describe("MageStore CRD validation — AdminSpec.BackendFrontname", func() {
	const namespace = "default"
	ctx := context.Background()

	makeStore := func(name, frontname string) *magev1alpha1.MageStore {
		s := newMinimalMageStore(namespace)
		s.Name = name
		s.Spec.Bootstrap = &magev1alpha1.BootstrapSpec{
			Admin: magev1alpha1.AdminSpec{
				Email:            "admin@example.com",
				BackendFrontname: frontname,
			},
		}
		return s
	}

	It("accepts a 16-char alphanumeric value", func() {
		s := makeStore("admin-fn-valid-16", "abcdefghij012345")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		// Cleanup — the validation suite re-uses the default namespace.
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
	})

	It("accepts dashes and underscores within the regex", func() {
		s := makeStore("admin-fn-valid-mixed", "secret_admin-9")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
	})

	It("rejects values shorter than 4 characters", func() {
		s := makeStore("admin-fn-too-short", "abc")
		err := k8sClient.Create(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("backendFrontname"))
	})

	It("rejects values longer than 32 characters", func() {
		s := makeStore("admin-fn-too-long", "0123456789012345678901234567890123") // 34 chars
		err := k8sClient.Create(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("backendFrontname"))
	})

	It("rejects values containing path separators", func() {
		s := makeStore("admin-fn-with-slash", "secret/admin")
		err := k8sClient.Create(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("backendFrontname"))
	})
})

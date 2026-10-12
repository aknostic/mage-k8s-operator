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
	. "github.com/onsi/gomega/gstruct"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// ensure_routing_test.go covers the per-MageStore Gateway/HTTPRoute
// reconcile. The HTTP-only path (no spec.routing.tls) is the v0.4.x
// behavior that ADR 006 amendment landed; the TLS path is per
// ADR 010. The HTTPRoute shape is regression-tested as part of this
// file rather than a separate file because Gateway and HTTPRoute
// always reconcile together (one ensureRouting call).

var _ = Describe("ensureRouting", func() {
	var (
		reconciler *MageStoreReconciler
		store      *magev1alpha1.MageStore
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ensurerouting-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Capabilities: fullCapabilities(),
		}
		store = newMinimalMageStore(namespace)
	})

	Context("when spec.routing is unset (v0.4.x default)", func() {
		// Cleanup #1: lift Create+Get into a BeforeEach so each It only
		// carries its own assertions (mirrors ensure_cache_test.go pattern).
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("emits a Gateway with a single HTTP listener on port 80", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-gateway",
				Namespace: namespace,
			}, gw)).To(Succeed())

			Expect(gw.Spec.Listeners).To(HaveLen(1))
			Expect(gw.Spec.Listeners[0].Name).To(Equal(gatewayv1.SectionName("http")))
			Expect(gw.Spec.Listeners[0].Port).To(Equal(gatewayv1.PortNumber(80)))
			Expect(gw.Spec.Listeners[0].Protocol).To(Equal(gatewayv1.HTTPProtocolType))
			Expect(gw.Annotations).NotTo(HaveKey("cert-manager.io/cluster-issuer"))
			Expect(gw.Annotations).NotTo(HaveKey("cert-manager.io/issuer"))
		})

		// Cleanup #3: description now names only the fallthrough rule that
		// the body actually asserts. Assertions cover rules [0..3] backends
		// so the description no longer over-promises.
		It("emits an HTTPRoute with four path-prefix rules, all pointing at the frontend", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			rt := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name,
				Namespace: namespace,
			}, rt)).To(Succeed())

			Expect(rt.Spec.Rules).To(HaveLen(4))

			// When no admin/api workloads are declared, all four rules point
			// at the frontend Service. Assert each rule's backend so we catch
			// regressions in the path→backend mapping, not just rule count.
			for i, rule := range rt.Spec.Rules {
				Expect(rule.BackendRefs).To(HaveLen(1), "rule %d backend count", i)
				Expect(string(rule.BackendRefs[0].Name)).To(Equal(store.Name+"-frontend"), "rule %d backend name", i)
			}

			// The last rule is always the "/" fallthrough catch-all.
			lastRule := rt.Spec.Rules[3]
			Expect(lastRule.Matches).To(HaveLen(1))
			Expect(*lastRule.Matches[0].Path.Value).To(Equal("/"))
		})

		It("is idempotent across repeated calls", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-gateway",
				Namespace: namespace,
			}, gw)).To(Succeed())
			Expect(gw.Spec.Listeners).To(HaveLen(1))

			// Cleanup #2: idempotency check also asserts HTTPRoute Rules
			// are stable (not appended on each call).
			rt := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name,
				Namespace: namespace,
			}, rt)).To(Succeed())
			Expect(rt.Spec.Rules).To(HaveLen(4))
		})
	})

	// Phase 3 (ADR 010): spec.routing.tls without spec.stores is
	// structurally invalid — TLS listeners need SNI hostnames.
	// Validation runs at the top of Reconcile (pure, no API calls)
	// and surfaces Ready=False, Reason=TLSRequiresStoreHosts.
	Context("when spec.routing.tls is set without spec.stores", func() {
		It("surfaces Ready=False with Reason=TLSRequiresStoreHosts", func() {
			store := newMinimalMageStore(namespace)
			store.Spec.Routing = &magev1alpha1.RoutingSpec{
				TLS: &magev1alpha1.TLSSpec{
					IssuerRef: &magev1alpha1.IssuerRefSpec{
						Kind: "ClusterIssuer",
						Name: "letsencrypt-prod",
					},
				},
			}

			reason, err := validateMageStoreSpec(store)
			Expect(err).To(HaveOccurred())
			Expect(reason).To(Equal(ReasonTLSRequiresStoreHosts))
		})

		// Phase 3 cleanup: valid-path test for validateMageStoreSpec.
		It("returns no error for a spec with TLS unset", func() {
			reason, err := validateMageStoreSpec(newMinimalMageStore(namespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(BeEmpty())
		})
	})

	// Phase 4 (ADR 010): IssuerRef single-store — cert-manager Gateway
	// shim annotation and one TLS-Terminate listener per host.
	Context("when spec.routing.tls.issuerRef is set (ClusterIssuer, single store)", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			store = storeWithHostsAndIssuerRef(namespace,
				[]string{"shop.example.com"},
				[]string{"default"},
			)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("annotates the Gateway with cert-manager.io/cluster-issuer and adds one HTTPS listener", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-gateway",
				Namespace: namespace,
			}, gw)).To(Succeed())

			// cert-manager annotation: cluster-issuer set, namespace issuer absent.
			Expect(gw.Annotations).To(HaveKeyWithValue("cert-manager.io/cluster-issuer", "letsencrypt-prod"))
			Expect(gw.Annotations).NotTo(HaveKey("cert-manager.io/issuer"))

			// Two listeners: HTTP + one HTTPS.
			Expect(gw.Spec.Listeners).To(HaveLen(2))

			idx := indexByListenerName(gw.Spec.Listeners, "https-shop-example-com")
			Expect(idx).NotTo(Equal(-1), "expected listener https-shop-example-com")
			l := gw.Spec.Listeners[idx]

			Expect(l.Port).To(Equal(gatewayv1.PortNumber(443)))
			Expect(l.Protocol).To(Equal(gatewayv1.HTTPSProtocolType))
			Expect(string(*l.Hostname)).To(Equal("shop.example.com"))
			Expect(*l.TLS.Mode).To(Equal(gatewayv1.TLSModeTerminate))
			Expect(string(l.TLS.CertificateRefs[0].Name)).To(Equal("store-gateway-tls-shop-example-com"))
		})
	})

	// Phase 4 (ADR 010): CertificateSecretRef — BYO Secret, no annotation.
	Context("when spec.routing.tls.certificateSecretRef is set (BYO cert)", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			store = newMinimalMageStore(namespace)
			store.Spec.Stores = []magev1alpha1.StoreSpec{
				{Host: "shop.example.com", Code: "default", Type: "website"},
			}
			store.Spec.Routing = &magev1alpha1.RoutingSpec{
				TLS: &magev1alpha1.TLSSpec{
					CertificateSecretRef: &magev1alpha1.LocalObjectReference{Name: "byo-cert"},
				},
			}
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("references the user-supplied Secret and sets no cert-manager annotations", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-gateway",
				Namespace: namespace,
			}, gw)).To(Succeed())

			// No cert-manager annotations on the BYO path.
			Expect(gw.Annotations).NotTo(HaveKey("cert-manager.io/cluster-issuer"))
			Expect(gw.Annotations).NotTo(HaveKey("cert-manager.io/issuer"))

			// HTTPS listener uses the user-supplied Secret name.
			idx := indexByListenerName(gw.Spec.Listeners, "https-shop-example-com")
			Expect(idx).NotTo(Equal(-1), "expected listener https-shop-example-com")
			l := gw.Spec.Listeners[idx]
			Expect(string(l.TLS.CertificateRefs[0].Name)).To(Equal("byo-cert"))
		})
	})

	// Phase 4 (ADR 010): multi-store IssuerRef fan-out — one HTTPS
	// listener per host, unique cert Secret per host.
	Context("when spec.routing.tls.issuerRef is set with two store hosts", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			store = storeWithHostsAndIssuerRef(namespace,
				[]string{"shop.example.com", "admin.example.com"},
				[]string{"shop", "admin"},
			)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("emits three listeners (HTTP + two HTTPS) with per-host cert Secrets", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-gateway",
				Namespace: namespace,
			}, gw)).To(Succeed())

			Expect(gw.Spec.Listeners).To(HaveLen(3))

			shopIdx := indexByListenerName(gw.Spec.Listeners, "https-shop-example-com")
			Expect(shopIdx).NotTo(Equal(-1), "expected listener https-shop-example-com")
			Expect(string(gw.Spec.Listeners[shopIdx].TLS.CertificateRefs[0].Name)).To(Equal("store-gateway-tls-shop-example-com"))

			adminIdx := indexByListenerName(gw.Spec.Listeners, "https-admin-example-com")
			Expect(adminIdx).NotTo(Equal(-1), "expected listener https-admin-example-com")
			Expect(string(gw.Spec.Listeners[adminIdx].TLS.CertificateRefs[0].Name)).To(Equal("store-gateway-tls-admin-example-com"))
		})
	})

	// I-3 (pre-merge polish): exercises the annotation-cleanup branch in
	// ensureGateway (ensure_routing.go:112-117). No existing test mutates
	// a CR across two reconciles; this one does. The branch fires in the
	// `else` arm when TLS is off OR is set to CertificateSecretRef —
	// it must delete any stale cert-manager annotations left by a prior
	// IssuerRef reconcile.
	Context("when toggling from issuerRef to certificateSecretRef", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			// Start with IssuerRef so the first reconcile writes the
			// cert-manager annotation.
			store = storeWithHostsAndIssuerRef(namespace,
				[]string{"shop.example.com"},
				[]string{"default"},
			)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("removes cert-manager annotations and updates CertificateRefs after the toggle", func() {
			// Reconcile 1: IssuerRef mode — annotation must be present.
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			gw := &gatewayv1.Gateway{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-gateway",
				Namespace: namespace,
			}, gw)).To(Succeed())
			Expect(gw.Annotations).To(HaveKey("cert-manager.io/cluster-issuer"),
				"first reconcile must set cluster-issuer annotation")

			// Mutate: switch from IssuerRef to CertificateSecretRef.
			store.Spec.Routing = &magev1alpha1.RoutingSpec{
				TLS: &magev1alpha1.TLSSpec{
					CertificateSecretRef: &magev1alpha1.LocalObjectReference{Name: "byo-cert"},
				},
			}
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())

			// Reconcile 2: BYO Secret mode — annotation-cleanup branch fires.
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-gateway",
				Namespace: namespace,
			}, gw)).To(Succeed())

			// Both cert-manager annotations must be absent after the toggle.
			Expect(gw.Annotations).NotTo(HaveKey("cert-manager.io/cluster-issuer"),
				"cluster-issuer annotation must be removed after toggle to BYO cert")
			Expect(gw.Annotations).NotTo(HaveKey("cert-manager.io/issuer"),
				"issuer annotation must also be absent after toggle to BYO cert")

			// The HTTPS listener must reference the user-supplied Secret, not
			// the operator-managed name.
			idx := indexByListenerName(gw.Spec.Listeners, "https-shop-example-com")
			Expect(idx).NotTo(Equal(-1), "expected listener https-shop-example-com after toggle")
			Expect(string(gw.Spec.Listeners[idx].TLS.CertificateRefs[0].Name)).To(Equal("byo-cert"))
		})
	})
})

// Gap #5 (v0.6.0): X-Forwarded-Proto header injection via HTTPRoute filter.
//
// When spec.routing.tls is set the operator emits TWO HTTPRoutes:
//   - <store>     — parentRef sectionName "http" only, no XFP filter.
//   - <store>-https — parentRef sectionName per https-<slug> listener,
//     every rule carries a RequestHeaderModifier forcing
//     X-Forwarded-Proto: https.
//
// The split is necessary because a single HTTPRoute cannot selectively
// apply a filter to only the HTTPS listener — the filter would also run
// on plain-HTTP requests, injecting the wrong scheme header.
var _ = Describe("ensureRouting XFP filter (gap #5)", func() {
	var (
		reconciler *MageStoreReconciler
		namespace  string
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "xfp-"},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name

		reconciler = &MageStoreReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Capabilities: fullCapabilities(),
		}
	})

	Context("when spec.routing.tls is set (single store host)", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			store = storeWithHostsAndIssuerRef(namespace,
				[]string{"shop.example.com"},
				[]string{"default"},
			)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("emits a <store>-https HTTPRoute with XFP filter on every rule", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			httpsRoute := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-https",
				Namespace: namespace,
			}, httpsRoute)).To(Succeed())

			// One parentRef per https listener, sectionName = "https-shop-example-com".
			Expect(httpsRoute.Spec.ParentRefs).To(HaveLen(1))
			Expect(httpsRoute.Spec.ParentRefs[0].SectionName).NotTo(BeNil())
			Expect(string(*httpsRoute.Spec.ParentRefs[0].SectionName)).To(Equal("https-shop-example-com"))

			// Four rules, each carrying the XFP filter.
			Expect(httpsRoute.Spec.Rules).To(HaveLen(4))
			for i, rule := range httpsRoute.Spec.Rules {
				Expect(rule.Filters).To(ContainElement(
					MatchFields(IgnoreExtras, Fields{
						"Type": Equal(gatewayv1.HTTPRouteFilterRequestHeaderModifier),
						"RequestHeaderModifier": PointTo(MatchFields(IgnoreExtras, Fields{
							"Set": ContainElement(MatchFields(IgnoreExtras, Fields{
								"Name":  Equal(gatewayv1.HTTPHeaderName("X-Forwarded-Proto")),
								"Value": Equal("https"),
							})),
						})),
					}),
				), "rule %d should have XFP filter", i)
			}
		})

		It("scopes the plain <store> HTTPRoute to sectionName http and adds NO XFP filter", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			rt := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name,
				Namespace: namespace,
			}, rt)).To(Succeed())

			// parentRef carries sectionName "http" when TLS is active.
			Expect(rt.Spec.ParentRefs).To(HaveLen(1))
			Expect(rt.Spec.ParentRefs[0].SectionName).NotTo(BeNil())
			Expect(string(*rt.Spec.ParentRefs[0].SectionName)).To(Equal("http"))

			// No rule should carry an XFP filter.
			for i, rule := range rt.Spec.Rules {
				for _, f := range rule.Filters {
					Expect(f.Type).NotTo(Equal(gatewayv1.HTTPRouteFilterRequestHeaderModifier),
						"rule %d must not have XFP filter on HTTP route", i)
				}
			}
		})
	})

	Context("when spec.routing.tls is set with two store hosts", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			store = storeWithHostsAndIssuerRef(namespace,
				[]string{"shop.example.com", "admin.example.com"},
				[]string{"shop", "admin"},
			)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("emits <store>-https with one parentRef per https listener", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			httpsRoute := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-https",
				Namespace: namespace,
			}, httpsRoute)).To(Succeed())

			// Two parentRefs, one per host.
			Expect(httpsRoute.Spec.ParentRefs).To(HaveLen(2))
			sectionNames := make([]string, 0, 2)
			for _, p := range httpsRoute.Spec.ParentRefs {
				Expect(p.SectionName).NotTo(BeNil())
				sectionNames = append(sectionNames, string(*p.SectionName))
			}
			Expect(sectionNames).To(ConsistOf("https-shop-example-com", "https-admin-example-com"))
		})
	})

	Context("when spec.routing.tls is unset", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			store = newMinimalMageStore(namespace)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("does NOT create a <store>-https HTTPRoute", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			httpsRoute := &gatewayv1.HTTPRoute{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-https",
				Namespace: namespace,
			}, httpsRoute)
			Expect(err).To(HaveOccurred(), "<store>-https should not exist when TLS is unset")
		})

		It("emits <store> HTTPRoute with no sectionName and no XFP filter", func() {
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())

			rt := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name,
				Namespace: namespace,
			}, rt)).To(Succeed())

			// No sectionName when TLS is unset (attaches to all listeners).
			Expect(rt.Spec.ParentRefs).To(HaveLen(1))
			Expect(rt.Spec.ParentRefs[0].SectionName).To(BeNil())

			// No XFP filter on any rule.
			for i, rule := range rt.Spec.Rules {
				for _, f := range rule.Filters {
					Expect(f.Type).NotTo(Equal(gatewayv1.HTTPRouteFilterRequestHeaderModifier),
						"rule %d must not have XFP filter when TLS is unset", i)
				}
			}
		})
	})

	Context("cleanup: when TLS is removed after being set", func() {
		var store *magev1alpha1.MageStore

		BeforeEach(func() {
			store = storeWithHostsAndIssuerRef(namespace,
				[]string{"shop.example.com"},
				[]string{"default"},
			)
			Expect(k8sClient.Create(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())
		})

		It("deletes the <store>-https HTTPRoute when TLS is removed", func() {
			// Reconcile 1: TLS active — <store>-https must be created.
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())
			httpsRoute := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-https",
				Namespace: namespace,
			}, httpsRoute)).To(Succeed(), "<store>-https should exist after first reconcile")

			// Remove TLS from the store spec.
			store.Spec.Routing = nil
			Expect(k8sClient.Update(ctx, store)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: store.Name, Namespace: namespace}, store)).To(Succeed())

			// Reconcile 2: TLS gone — <store>-https must be deleted, and
			// <store>'s parentRef must drop the sectionName=http scope so it
			// re-attaches to all Gateway listeners (a future change that
			// breaks the hasTLS guard would otherwise leave HTTP traffic
			// stuck on a now-nonexistent listener).
			Expect(reconciler.ensureRouting(ctx, store)).To(Succeed())
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name + "-https",
				Namespace: namespace,
			}, httpsRoute)
			Expect(err).To(HaveOccurred(), "<store>-https should be deleted after TLS removal")

			plainRoute := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      store.Name,
				Namespace: namespace,
			}, plainRoute)).To(Succeed())
			Expect(plainRoute.Spec.ParentRefs).To(HaveLen(1))
			Expect(plainRoute.Spec.ParentRefs[0].SectionName).To(BeNil(),
				"<store> parentRef sectionName must clear back to nil after TLS removal")
		})
	})
})

// storeWithHostsAndIssuerRef builds a MageStore with spec.stores populated
// from the given hosts/codes slices and spec.routing.tls.issuerRef hardcoded
// to a ClusterIssuer named "letsencrypt-prod" — the only kind/name shape any
// caller exercises today. Re-introduce parameters if a namespace-scoped
// Issuer or alternate name lands in a future test.
func storeWithHostsAndIssuerRef(ns string, hosts, codes []string) *magev1alpha1.MageStore {
	s := newMinimalMageStore(ns)
	for i, h := range hosts {
		s.Spec.Stores = append(s.Spec.Stores, magev1alpha1.StoreSpec{
			Host: h,
			Code: codes[i],
			Type: "website",
		})
	}
	s.Spec.Routing = &magev1alpha1.RoutingSpec{
		TLS: &magev1alpha1.TLSSpec{
			IssuerRef: &magev1alpha1.IssuerRefSpec{Kind: "ClusterIssuer", Name: "letsencrypt-prod"},
		},
	}
	return s
}

// indexByListenerName returns the index of the first listener with the
// given name in ls, or -1 if none match.
func indexByListenerName(ls []gatewayv1.Listener, name string) int {
	for i := range ls {
		if string(ls[i].Name) == name {
			return i
		}
	}
	return -1
}

// newMinimalMageStore returns a MageStore CR that passes capabilities
// validation: required fields populated, dependencies in inline mode,
// no spec.stores, no spec.routing.
func newMinimalMageStore(ns string) *magev1alpha1.MageStore {
	return &magev1alpha1.MageStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "store",
			Namespace: ns,
		},
		Spec: magev1alpha1.MageStoreSpec{
			Image: magev1alpha1.MageStoreImage{
				Repository: "registry.gitlab.aknostic.com/test/magento",
				Tag:        "2.4.8",
			},
			Workloads: magev1alpha1.WorkloadsSpec{
				Frontend: magev1alpha1.WorkloadSpec{Replicas: 1},
			},
		},
	}
}

package controller

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	magev1alpha1 "gitlab.aknostic.com/aknostic/mage-operator/api/v1alpha1"
)

// gatewayClassName is the GatewayClass the operator attaches its
// per-MageStore Gateway to. ADR 006 (2026-04-24 amendment) hard-codes
// `eg` — the convention name used by Envoy Gateway's helm chart and
// the simplest path for the v0.1.0 dev story. Production users with a
// different GatewayClass name (e.g. `traefik-public`,
// `aws-load-balancer`) graduate to a `spec.routing.gatewayClassName`
// CRD field in Phase 2; for v0.1.0 they create a `eg`-named
// GatewayClass alias pointing at their preferred controller, or
// override at the controller binary's flag level (TBD).
const gatewayClassName = "eg"

// ensureRouting materialises the routing layer per ADR 006 (2026-04-24
// amendment): one Gateway and one HTTPRoute per MageStore. Both attach
// to the cluster's Envoy Gateway-managed `eg` GatewayClass; the
// controller provisions an Envoy data plane behind the Gateway and
// the HTTPRoute carries ADR 006's fallthrough table verbatim.
//
// Two resources, one orchestrator: Gateway is the listener
// (port 80, HTTP), HTTPRoute is the rule set (path-based backend
// fallthrough, multi-hostname when `spec.stores` is set). Splitting
// into two files would duplicate the per-store naming + label
// boilerplate; keeping them in one file mirrors the pre-amendment
// `ensureIngress` shape that emitted a single resource for the same
// concern.
//
// No readiness condition. Like the pre-amendment Ingress shape,
// downstream Service readiness drives WorkloadsReady, which is what
// "the path actually serves traffic" reduces to. Gateway/HTTPRoute
// status is exposed by Envoy Gateway and queried via kubectl when
// users diagnose routing problems.
//
// Watch-mapper: not needed. Both Gateway and HTTPRoute are Owned by
// the MageStore via `SetControllerReference`; the existing `Owns()`
// chain in SetupWithManager triggers parent reconciles for both.
func (r *MageStoreReconciler) ensureRouting(ctx context.Context, store *magev1alpha1.MageStore) error {
	if err := r.ensureGateway(ctx, store); err != nil {
		return fmt.Errorf("apply gateway: %w", err)
	}
	if err := r.ensureHTTPRoute(ctx, store); err != nil {
		return fmt.Errorf("apply httproute: %w", err)
	}
	return nil
}

// ensureGateway materialises the per-MageStore Gateway. v0.1.0
// simplification: one Gateway per MageStore (rather than a shared
// cluster Gateway referenced via `spec.routing.gatewayRef`) because a
// shared-Gateway shape requires a CRD field, and Slot 22.0's "no CRD
// changes mid-Session-2" guardrail blocks that. The shared-Gateway
// shape is documented in the ADR amendment as a Phase 2 follow-on.
//
// Single HTTP listener on port 80 named `http` — TLS landing requires
// either cert-manager wiring or a user-supplied Secret reference, both
// of which graduate alongside the gatewayRef field. AllowedRoutes is
// scoped to the same namespace so cross-namespace HTTPRoute attaches
// (which we don't need) are explicitly disallowed.
func (r *MageStoreReconciler) ensureGateway(ctx context.Context, store *magev1alpha1.MageStore) error {
	name := store.Name + "-gateway"
	labels := routingLabels(store)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, gw, func() error {
		gw.Labels = labels
		gw.Spec.GatewayClassName = gatewayv1.ObjectName(gatewayClassName)

		from := gatewayv1.NamespacesFromSame
		listeners := []gatewayv1.Listener{{
			Name:     "http",
			Protocol: gatewayv1.HTTPProtocolType,
			Port:     gatewayv1.PortNumber(80),
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: &from},
			},
		}}
		listeners = append(listeners, buildHTTPSListeners(store)...)
		gw.Spec.Listeners = listeners

		// cert-manager Gateway shim annotation. cert-manager 1.14+ watches
		// Gateways with these annotations and provisions Certificates for
		// each TLS-listener hostname into the listener's
		// certificateRefs[0] Secret.
		if store.Spec.Routing != nil && store.Spec.Routing.TLS != nil && store.Spec.Routing.TLS.IssuerRef != nil {
			if gw.Annotations == nil {
				gw.Annotations = map[string]string{}
			}
			switch store.Spec.Routing.TLS.IssuerRef.Kind {
			case "ClusterIssuer":
				gw.Annotations["cert-manager.io/cluster-issuer"] = store.Spec.Routing.TLS.IssuerRef.Name
				delete(gw.Annotations, "cert-manager.io/issuer")
			case "Issuer":
				gw.Annotations["cert-manager.io/issuer"] = store.Spec.Routing.TLS.IssuerRef.Name
				delete(gw.Annotations, "cert-manager.io/cluster-issuer")
			}
		} else {
			// TLS off or BYO Secret: ensure stale cert-manager annotations
			// don't linger from a previous reconcile.
			delete(gw.Annotations, "cert-manager.io/cluster-issuer")
			delete(gw.Annotations, "cert-manager.io/issuer")
		}
		return ctrl.SetControllerReference(store, gw, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("gateway reconciled", "op", string(op))
	}
	return nil
}

// ensureHTTPRoute materialises the routing rules per ADR 006's
// fallthrough table. One HTTPRoute named `<store>` carries four
// path-prefix rules (`/admin`, `/rest`, `/graphql`, `/`); backend
// Service selection follows the declared workload set (admin path
// to `<store>-admin` if declared else `<store>-frontend`; `/rest`
// and `/graphql` to `<store>-api` if declared else `<store>-frontend`;
// `/` always to `<store>-frontend`).
//
// Multi-host: the `hostnames` field carries one entry per
// `spec.stores` entry. When `spec.stores` is unset, hostnames is
// nil and the HTTPRoute matches any host (parent Gateway listener
// determines admissible hostnames). Backends don't vary per host
// (per ADR 007's "all hosts share the same Deployment"), so a
// single HTTPRoute with multi-host `hostnames` is functionally
// equivalent to one HTTPRoute per host.
//
// Two-route split (gap #5 / ADR 013): when spec.routing.tls is set, a
// second HTTPRoute named `<store>-https` is emitted alongside `<store>`.
// `<store>` is scoped to sectionName "http" so it only attaches to the
// HTTP listener. `<store>-https` attaches to each https-<slug> listener
// via a parentRef per host and forces X-Forwarded-Proto: https on every
// rule via a RequestHeaderModifier filter. This split is necessary
// because a single HTTPRoute cannot selectively apply a filter to only
// HTTPS-listener-attached traffic — the filter would run on plain-HTTP
// requests too, injecting the wrong scheme header.
func (r *MageStoreReconciler) ensureHTTPRoute(ctx context.Context, store *magev1alpha1.MageStore) error {
	hasTLS := store.Spec.Routing != nil && store.Spec.Routing.TLS != nil

	labels := routingLabels(store)

	// Resolve the admin URL path segment up front so the rule-builder
	// stays a pure function. Falls back to literal `admin` when the
	// admin Secret hasn't been written yet (adoption case, or rare
	// reconcile-order edge cases) — that's the same path v0.6.x users
	// know, so the fallback degrades gracefully.
	frontname, err := r.resolveAdminFrontname(ctx, store)
	if err != nil {
		return fmt.Errorf("resolve admin frontname for httproute: %w", err)
	}

	rt := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: store.Name, Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, rt, func() error {
		rt.Labels = labels
		// Group/Kind set explicitly even though they default to
		// gateway.networking.k8s.io / Gateway. Without these, the
		// Gateway API CRD's OpenAPI structural defaulting populates
		// them server-side, our next mutator wipes them, and
		// CreateOrUpdate sees a phantom diff every reconcile —
		// `op=updated` thrash with no real change. Same fix applies
		// to BackendObjectReference + HTTPBackendRef.Weight in
		// buildRouteRules.
		parentRef := gatewayv1.ParentReference{
			Group: ptr.To(gatewayv1.Group(gatewayv1.GroupName)),
			Kind:  ptr.To(gatewayv1.Kind("Gateway")),
			Name:  gatewayv1.ObjectName(store.Name + "-gateway"),
		}
		if hasTLS {
			// Scope to the HTTP listener only so the XFP filter on the
			// HTTPS route doesn't accidentally bleed to HTTP traffic.
			httpSection := gatewayv1.SectionName("http")
			parentRef.SectionName = &httpSection
		}
		rt.Spec.ParentRefs = []gatewayv1.ParentReference{parentRef}
		rt.Spec.Hostnames = buildRouteHostnames(store)
		rt.Spec.Rules = buildRouteRules(store, frontname)
		return ctrl.SetControllerReference(store, rt, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("httproute reconciled", "op", string(op))
	}

	if hasTLS {
		// The <store> apply above and ensureHTTPSRoute below are not
		// transactional. A reconciler crash between them leaves <store>
		// scoped to sectionName=http with no <store>-https route yet, so
		// HTTPS traffic is unrouted until the next reconcile picks up.
		// Acceptable for an operator (eventual consistency); flagged here
		// for archaeology.
		return r.ensureHTTPSRoute(ctx, store)
	}

	// TLS is unset: clean up any stale <store>-https HTTPRoute that a
	// previous TLS-enabled reconcile may have created.
	stale := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: store.Name + "-https", Namespace: store.Namespace},
	}
	if err := r.Delete(ctx, stale); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete stale httproute %s-https: %w", store.Name, err)
	}
	return nil
}

// ensureHTTPSRoute materialises the HTTPS-listener-attached HTTPRoute for
// gap #5. One parentRef per https-<slug> listener, same path rules as the
// HTTP route, but every rule carries a RequestHeaderModifier filter that
// forces X-Forwarded-Proto: https.
//
// Belt-and-braces rationale: Envoy Gateway propagates the proto header
// correctly when traffic arrives on the HTTPS listener, so this filter is
// a no-op on EG. Other Gateway API controllers (Istio, Traefik, NGINX) may
// not propagate it, which causes Magento's `isSecure()` check to return
// false even on HTTPS connections, breaking the admin panel redirect loop
// and checkout HTTPS enforcement. The filter makes proto header correct
// regardless of controller behaviour.
func (r *MageStoreReconciler) ensureHTTPSRoute(ctx context.Context, store *magev1alpha1.MageStore) error {
	labels := routingLabels(store)

	frontname, err := r.resolveAdminFrontname(ctx, store)
	if err != nil {
		return fmt.Errorf("resolve admin frontname for https httproute: %w", err)
	}

	rt := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: store.Name + "-https", Namespace: store.Namespace},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, rt, func() error {
		rt.Labels = labels

		// One parentRef per https-<slug> listener (one per store host).
		parentRefs := make([]gatewayv1.ParentReference, 0, len(store.Spec.Stores))
		for _, s := range store.Spec.Stores {
			slug := hostSlug(s.Host)
			section := gatewayv1.SectionName("https-" + slug)
			parentRefs = append(parentRefs, gatewayv1.ParentReference{
				Group:       ptr.To(gatewayv1.Group(gatewayv1.GroupName)),
				Kind:        ptr.To(gatewayv1.Kind("Gateway")),
				Name:        gatewayv1.ObjectName(store.Name + "-gateway"),
				SectionName: &section,
			})
		}
		rt.Spec.ParentRefs = parentRefs
		rt.Spec.Hostnames = buildRouteHostnames(store)
		rt.Spec.Rules = buildRouteRulesWithXFP(store, frontname)
		return ctrl.SetControllerReference(store, rt, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		ctrl.LoggerFrom(ctx).Info("httproute-https reconciled", "op", string(op))
	}
	return nil
}

// routingLabels carries the same `app.kubernetes.io/*` shape as
// workload resources, with no `component` (the routing layer is one
// concern, not three). Used for both Gateway and HTTPRoute so users
// can `kubectl get all -l app.kubernetes.io/instance=<store>` and see
// the routing topology alongside the workload pods.
func routingLabels(store *magev1alpha1.MageStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "magento",
		"app.kubernetes.io/instance":   store.Name,
		"app.kubernetes.io/managed-by": managedByLabel,
	}
}

// buildRouteHostnames returns one Hostname per `spec.stores` entry,
// or nil when `spec.stores` is unset (HTTPRoute matches any host).
func buildRouteHostnames(store *magev1alpha1.MageStore) []gatewayv1.Hostname {
	if len(store.Spec.Stores) == 0 {
		return nil
	}
	out := make([]gatewayv1.Hostname, 0, len(store.Spec.Stores))
	for _, s := range store.Spec.Stores {
		out = append(out, gatewayv1.Hostname(s.Host))
	}
	return out
}

// hostSlug returns a Kubernetes-resource-name-safe form of a hostname
// for use in Secret / listener-name suffixes. Replaces dots with
// dashes; assumes the host is already lowercase RFC-1035-compliant
// (Gateway API enforces this on the Hostname type).
//
// Example: "shop.example.com" → "shop-example-com".
func hostSlug(host string) string {
	return strings.ReplaceAll(host, ".", "-")
}

// buildHTTPSListeners returns one TLS-Terminate listener per
// spec.stores[].host. Returns nil when spec.routing.tls is unset.
//
// Cert Secret naming follows ADR 010: <store>-gateway-tls-<host-slug>
// for the issuerRef path (cert-manager fills the Secret),
// or the user-supplied Secret name for the certificateSecretRef
// path. Multiple listeners on port 443 share via SNI.
func buildHTTPSListeners(store *magev1alpha1.MageStore) []gatewayv1.Listener {
	if store.Spec.Routing == nil || store.Spec.Routing.TLS == nil {
		return nil
	}
	tls := store.Spec.Routing.TLS

	from := gatewayv1.NamespacesFromSame
	mode := gatewayv1.TLSModeTerminate

	out := make([]gatewayv1.Listener, 0, len(store.Spec.Stores))
	for _, s := range store.Spec.Stores {
		hostname := gatewayv1.Hostname(s.Host)
		slug := hostSlug(s.Host)

		var secretName string
		switch {
		case tls.CertificateSecretRef != nil:
			secretName = tls.CertificateSecretRef.Name
		case tls.IssuerRef != nil:
			secretName = store.Name + "-gateway-tls-" + slug
		}

		out = append(out, gatewayv1.Listener{
			Name:     gatewayv1.SectionName("https-" + slug),
			Protocol: gatewayv1.HTTPSProtocolType,
			Port:     gatewayv1.PortNumber(443),
			Hostname: &hostname,
			TLS: &gatewayv1.ListenerTLSConfig{
				Mode: &mode,
				CertificateRefs: []gatewayv1.SecretObjectReference{{
					Name: gatewayv1.ObjectName(secretName),
				}},
			},
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: &from},
			},
		})
	}
	return out
}

// buildRouteRules returns the four path-prefix rules per ADR 006's
// fallthrough table. Backend resolution looks at the declared
// workload set: admin path to `<store>-admin` when admin workload
// declared else `<store>-frontend`; `/rest` and `/graphql` to
// `<store>-api` when api workload declared else `<store>-frontend`;
// `/` to `<store>-varnish` when spec.varnish.enabled is true, else
// `<store>-frontend`.
//
// `frontname` is the resolved admin URL path segment (sourced from
// `<store>-admin` Secret per resolveAdminFrontname; falls back to
// literal `admin`). The admin rule's PathPrefix uses `/<frontname>` so
// the gap #3 randomization carries through to the gateway layer —
// without this, a fresh-install Magento served at `/<random>/` would
// still see `/admin` traffic landing on the admin tier, defeating the
// randomization.
//
// Varnish only fronts `/`. Admin + REST + GraphQL continue to route
// directly to their backends — Magento convention is that admin and
// API bypass the HTTP cache (return(pass) in VCL), and expressing
// that intent topologically (the paths never reach Varnish at all)
// is clearer than relying on the user's VCL to short-circuit them.
// Users with a VCL that caches admin/API own a custom gateway
// topology via Phase-2 routing fields.
func buildRouteRules(store *magev1alpha1.MageStore, frontname string) []gatewayv1.HTTPRouteRule {
	prefix := gatewayv1.PathMatchPathPrefix

	adminBackend := workloadName(store, workloadFrontend)
	if store.Spec.Workloads.Admin != nil {
		adminBackend = workloadName(store, workloadAdmin)
	}
	apiBackend := workloadName(store, workloadFrontend)
	if store.Spec.Workloads.Api != nil {
		apiBackend = workloadName(store, workloadAPI)
	}
	frontendBackend := workloadName(store, workloadFrontend)
	if store.Spec.Varnish != nil && store.Spec.Varnish.Enabled {
		frontendBackend = store.Name + varnishSuffix
	}

	mkRule := func(path, backend string) gatewayv1.HTTPRouteRule {
		p := path
		return gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{
					Type:  &prefix,
					Value: &p,
				},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: ptr.To(gatewayv1.Group("")),
						Kind:  ptr.To(gatewayv1.Kind("Service")),
						Name:  gatewayv1.ObjectName(backend),
						Port:  ptr.To(gatewayv1.PortNumber(8080)),
					},
					Weight: ptr.To[int32](1),
				},
			}},
		}
	}

	return []gatewayv1.HTTPRouteRule{
		mkRule("/"+frontname, adminBackend),
		mkRule("/rest", apiBackend),
		mkRule("/graphql", apiBackend),
		mkRule("/", frontendBackend),
	}
}

// buildRouteRulesWithXFP returns the same four path-prefix rules as
// buildRouteRules, with a RequestHeaderModifier filter appended to each
// rule that forces X-Forwarded-Proto: https. Used exclusively for the
// HTTPS-listener-attached HTTPRoute (see ensureHTTPSRoute).
func buildRouteRulesWithXFP(store *magev1alpha1.MageStore, frontname string) []gatewayv1.HTTPRouteRule {
	rules := buildRouteRules(store, frontname)

	xfpFilter := gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{
				{
					Name:  gatewayv1.HTTPHeaderName("X-Forwarded-Proto"),
					Value: "https",
				},
			},
		},
	}

	for i := range rules {
		rules[i].Filters = append(rules[i].Filters, xfpFilter)
	}
	return rules
}

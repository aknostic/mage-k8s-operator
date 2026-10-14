# ADR 010 — Public-edge TLS via Gateway API + cert-manager shim

- **Status:** Accepted
- **Date:** 2026-05-03
- **Deciders:** @jurg
- **Related:** extends ADR 006's Gateway/HTTPRoute shape; closes the
  "TLS landing deferred" open question at ADR 006 lines 261-264 and
  226-229; orthogonal to ADR 009's cache-TLS deferral (different
  threat model, different layer).

## Context

ADR 006's 2026-04-24 amendment landed Gateway API but left the Gateway
with an HTTP-only listener on port 80, deferring TLS to "graduate
alongside cert-manager wiring or a user-supplied Secret ref." That
deferral was tracked in the ADR but never surfaced on any user-facing
surface (README, quickstart, prerequisites, magestore-reference,
troubleshooting, operations/*) — a Magento ops persona pointing
`store.example.com` at the operator-managed Gateway has no warning
that HTTPS won't work.

The Kapsule demo planning session on 2026-05-03 surfaced this gap:
the demo target was a real-DNS browser-accessible storefront with
Let's Encrypt, which the operator's v0.4.0 surface cannot deliver.
We can ship the gap as undocumented v1.0.0+ deferral (consistent
with the cache-TLS stance, though for a different reason), or land
public-edge TLS now and unblock both real-infra demos and any user
running on the public internet.

We chose to land it. Public-edge TLS is qualitatively different from
cache-TLS (ADR 009): cache traffic stays on the pod network and
the threat model is moot; public-edge traffic crosses the public
internet by definition and the threat model is the standard one
(eavesdropping, MITM, browser warnings undermining trust). Deferring
public-edge TLS to v1.0.0+ would force every production user to
front-end the Gateway with their own TLS terminator (nginx-ingress,
cloud LB, Cloudflare, …) — exactly the in-cluster routing layer
ADR 006 chose Gateway API to *avoid* needing.

## Decision

Add `spec.routing.tls` to the MageStore CRD as a discriminated-union
oneof, mirroring ADR 004's `inline | ref | external` shape:

```yaml
spec:
  routing:
    tls:
      # exactly one of:
      issuerRef:
        kind: ClusterIssuer        # or Issuer
        name: letsencrypt-prod
      certificateSecretRef:
        name: my-prebuilt-tls
```

When `spec.routing.tls` is unset, behavior is unchanged from v0.4.0
(HTTP-only port 80 listener, no migration risk for existing users).
When set, the operator adds HTTPS listener(s) on port 443 alongside
the existing HTTP listener. The HTTP listener stays — cert-manager
HTTP-01 challenge needs it, and pre-TLS bookmarks keep working.

### `issuerRef` — cert-manager-managed certs

The operator annotates the Gateway with
`cert-manager.io/cluster-issuer: <name>` (or
`cert-manager.io/issuer: <name>`). cert-manager 1.14+'s Gateway API
shim watches Gateways with that annotation, creates one Certificate
resource per HTTPS-listener hostname, and provisions the cert into
the Secret named in `listener.tls.certificateRefs[0]`. Renewal is
cert-manager's responsibility — operator-side reconcile is idempotent
and does not interfere with the cert lifecycle.

Cert Secret naming: `<store>-gateway-tls-<host-slug>` where
`host-slug` replaces `.` with `-`. One Secret per
`spec.stores[].host`. Independent failure domains: a renewal failure
on one host doesn't take down others.

### `certificateSecretRef` — bring-your-own cert

The user pre-creates the Secret (kubernetes.io/tls type) with
`tls.crt` + `tls.key`. The HTTPS listener references it directly via
`certificateRefs: [{name: <user-secret>}]`. No cert-manager
dependency, no annotation, no operator-managed lifecycle. Suited to
shops with existing PKI, wildcard certs from a corporate CA, or
managed TLS providers (DigiCert, Sectigo) outside the cert-manager
ecosystem.

When multiple stores are declared, the user-supplied Secret must
cover every `spec.stores[].host` (SAN or wildcard) — operator does
not validate cert contents (would require parsing X.509 in the
controller, not worth the complexity). Misconfigured certs surface
as Envoy Gateway listener errors visible in `kubectl describe
gateway`.

### Listener layout

Per `spec.stores[].host`, one HTTPS listener:

```yaml
listeners:
- name: http
  port: 80
  protocol: HTTP
- name: https-<host-slug>
  port: 443
  protocol: HTTPS
  hostname: <host>
  tls:
    mode: Terminate
    certificateRefs:
    - name: <secret-name>
```

Multiple listeners share port 443 via SNI — Gateway API standard.

### Hostname requirement

`spec.routing.tls` set + `spec.stores` empty → reconcile fails with
`Ready=False, Reason=TLSRequiresStoreHosts`. TLS without SNI is
ambiguous (which cert goes on the listener?), and the no-stores
fallback path that ADR 006 supports for HTTP (HTTPRoute matches any
host) doesn't translate to TLS without arbitrary defaults.

Single-tenant TLS is supported: a one-entry `spec.stores` covers it
without overhead.

## Why this shape, not alternatives

1. **Free-form `spec.routing.tls.cert` blob with both
   issuer-name and secret-name fields, validated by reconcile.**
   Doesn't match the project's `inline | ref | external` discriminated-
   union convention. Users who already understand
   `mariaDb`/`mariaDbRef`/`external` would have to learn a new shape.

2. **Per-store TLS (`spec.stores[].tls`).** Theoretically more
   flexible — different stores could use different ClusterIssuers
   or different BYO Secrets. In practice, the multi-store use case
   (one codebase, many storefront hostnames) almost always shares
   one cert source. Per-store fan-out is a Phase 2 refinement once
   we see multi-issuer demand; until then it's complexity without
   payoff.

3. **TLS via Gateway annotations only, no CRD field.** Users would
   add `cert-manager.io/cluster-issuer` annotations directly to the
   operator-managed Gateway, which `controllerutil.CreateOrUpdate`
   would reconcile away every loop. We'd need to either watch a
   different annotation prefix (clunky) or pass-through an entire
   annotation block (footgun). A typed CRD field is honest about
   what the operator owns.

4. **Land TLS at the HTTPRoute level (filter-based) rather than
   Gateway listener.** HTTPRoute filters exist for redirect (HTTP→HTTPS)
   but not for cert binding — cert binding is a listener concern by
   Gateway API design. Wrong layer.

## What this changes for users

- **Default behavior unchanged.** `spec.routing.tls` unset →
  port 80 only, exactly as v0.4.0. Existing manifests need no
  changes.
- **Opt-in path is one new block** plus a cert-manager
  ClusterIssuer (for the `issuerRef` path). Documented in
  `docs/quickstart.md`'s new TLS section and `docs/prerequisites.md`'s
  optional cert-manager step.
- **cert-manager becomes a soft prerequisite** for the `issuerRef`
  path. It is not a chart dependency — cluster-wide infra concerns
  shouldn't ship inside a per-app chart. Documented as
  "install cert-manager separately if using `issuerRef`."
- **HTTP→HTTPS redirect is not in this scope.** Both listeners
  serve the same routes; the user's bookmarks decide which protocol
  reaches them. Forced-redirect lands as a follow-on flag
  (`spec.routing.tls.redirectHTTP: true`) once we see real demand
  and have a clear default-on/default-off stance.

## What this doesn't change

- The Gateway/HTTPRoute shape from ADR 006 — same per-MageStore
  Gateway, same one-HTTPRoute-with-multi-host-hostnames pattern.
- `MAGENTO_BASE_URL` and the image-contract env vars — Magento
  serves both HTTP and HTTPS off the same `BASE_URL`. Customers
  who set `web/secure/base_url` in `env.php` get HTTPS-only
  storefront URLs at the application layer, independent of the
  routing layer.
- The `eg` GatewayClass hard-code from ADR 006 — Phase 2's
  `spec.routing.gatewayClassName` is still where that flexibility
  lands.

### Trust model: TLS terminates at the Gateway, internal traffic stays plaintext

This ADR adds public-edge TLS only. Everything behind the Gateway
listener stays plaintext over the pod network:

- **Gateway → frontend Service → frontend pods:** Envoy terminates
  TLS at the listener and proxies decrypted HTTP to the backend
  Service. No re-encryption.
- **Magento workload pods → MariaDB / OpenSearch / Valkey /
  RabbitMQ:** plaintext. Same pod network, same trust boundary as
  v0.1.x–v0.4.x.
- **No mTLS introduced anywhere.** Not pod-to-pod, not
  service-to-service, not workload-to-deps. Operators wanting
  service-mesh mTLS layer Istio or Linkerd at the cluster level —
  orthogonal to this ADR and to the operator's CRD surface.
- **Cache-TLS (ADR 009)** stays deferred to v1.0.0+ pending
  client demand. Different layer, different threat model
  (Magento as Redis-protocol client vs. browser as HTTPS client),
  unchanged stance.

The deployment-model assumption behind deferring in-cluster
TLS-to-clients still holds: the pod network is the trust boundary, public internet
is outside. ADR 010 sharpens the boundary at the public edge; it
does not move the boundary inward.

## Open questions (deferred)

- **HTTP→HTTPS redirect.** Implementable as an HTTPRoute
  `RequestRedirect` filter on the HTTP listener's routes. Default
  on or default off is a real call: default-on is more secure but
  surprises users mid-migration; default-off is safer migration
  but worse security posture. Defer to a follow-on session with
  user/dev demand input.
- **mTLS (client cert verification).** Out of scope. Use case is
  niche (B2B catalog APIs that want client-cert authn at the edge).
  CRD field would be `spec.routing.tls.clientCertSecretRef` and a
  `mode: Mutual` listener config; revisit if users ask for it.
- **Wildcard cert reuse across MageStores.** A cluster-admin who
  buys a `*.shops.example.com` wildcard cert today has to put the
  same Secret reference in every MageStore. Phase 2's shared-Gateway
  shape (`spec.routing.gatewayRef`) collapses this — one cluster
  Gateway with one wildcard-cert listener, all MageStores attach.
  Until then, `certificateSecretRef` to the same Secret in every
  MageStore is the workaround.
- **Per-store TLS (different cert sources per host).** As above,
  not in this scope. Phase 2 once multi-issuer demand surfaces.
- **Cert hot-reload.** Envoy Gateway watches the listener's
  Secret and reloads on update. cert-manager renewal updates the
  Secret in place; no operator-side reconcile required. Document
  the property; no code action.

## References

- ADR 006 — Gateway API HTTPRoute shape, 2026-04-24 amendment
  (this ADR closes its TLS open questions at lines 226-229 and
  261-264).
- ADR 004 — `inline | ref | external` discriminated-union
  convention (this ADR mirrors the same shape for TLS).
- ADR 009 — cache-TLS (`Magento↔Valkey` wire) deferral. Orthogonal
  layer; this ADR does not change ADR 009's stance.
- cert-manager Gateway API integration —
  https://cert-manager.io/docs/usage/gateway/
- Gateway API v1.0 listener TLS conformance —
  https://gateway-api.sigs.k8s.io/api-types/gateway/#listener-tls
- Kapsule demo planning, 2026-05-03 — surfaced the
  audit-finding on docs invisibility of the v0.4.0 TLS gap.

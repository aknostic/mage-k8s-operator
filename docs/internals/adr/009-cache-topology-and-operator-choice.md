# ADR 009 — Cache topology: bare StatefulSet + Sentinel HA, no upstream Redis/Valkey operator dep

- **Status:** Accepted
- **Date:** 2026-04-30
- **Deciders:** @jurg
- **Related:** supersedes the inline-mode shape in ADR 004 §Cache;
  fixes the v0.2.x bug filed under "valkey-operator runs Valkey in cluster mode";
  unblocks the v0.3.0 HA cache surface advertised in
  `docs/operations/scaling.md`.

## Context

v0.1.x and v0.2.x ship inline cache mode (`spec.cache.redis: {}`) on top
of `hyperspike/valkey-operator`. During the v0.2.x DX-walkthrough
(2026-04-29) we hit `CROSSSLOT Keys in request don't hash to the same
slot` errors during `setup:upgrade`'s cache-tag cleanup. Root cause:
hyperspike runs Valkey with `cluster-enabled yes` regardless of the
`spec.nodes` and `spec.replicas` values — even single-instance
deployments are in cluster mode. Magento's
`Cm_Cache_Backend_Redis` (the upstream library `Magento\Framework\Cache\
Backend\Redis` extends) does multi-key `DEL` operations during cache-tag
cleanup; in cluster mode those fail unless every key hashes to the same
slot. They don't, and they can't without a backend rewrite.

This is not fixable on the Magento side: Adobe's open issue [#39224]
(https://github.com/magento/magento2/issues/39224) has tracked the
problem since 2.4.5 with no movement, and the upstream
[`colinmollenhour/cache-backend-redis`](https://github.com/colinmollenhour/Cm_Cache_Backend_Redis)
README carries an explicit deprecation notice: cluster functionality
"was flawed and is no longer supported."

Conclusion: **Magento needs Sentinel-mode HA, not Cluster-mode HA.**
v0.2.0's choice of valkey-operator was wrong for our domain.

## What real production Magento on K8s does

Public references for Magento on Kubernetes (excluding Adobe Commerce
Cloud and AWS-managed paths) all converge on the same shape:

| Project | Cache approach |
|---|---|
| [clickandmortar/magento-kubernetes](https://clickandmortar.github.io/magento-kubernetes/guide/advanced/high-availability.html) | Bitnami Helm chart, Sentinel for HA |
| [KiweeEu/magento2-on-kubernetes](https://github.com/KiweeEu/magento2-on-kubernetes) | Bitnami Redis chart |
| [graciousagency/scalable-magento-2](https://medium.com/graciousagency/how-do-you-run-a-scalable-magento-2-instance-in-kubernetes-734834405cf1) | Bitnami Redis chart |

**No serious Magento-on-K8s reference standardises on a Redis
operator.** The pattern is StatefulSet + Sentinel sidecar via Helm.

## Operator landscape (April 2026)

We surveyed every actively-published Redis/Valkey operator:

| Operator | Last release | Modes | Verdict for Magento |
|---|---|---|---|
| `spotahome/redis-operator` | Aug 2023 (v1.3.0-rc1) | Sentinel only | Stale; documented data-loss footgun (Palark blog) |
| `OT-CONTAINER-KIT/redis-operator` | Mar 2026 (v0.24.0) | Standalone, Replication, Sentinel, Cluster | Active, but `RedisStatus struct{}` is empty (no readiness signal); 201 open issues including Sentinel-pods-not-creating |
| `hyperspike/valkey-operator` | active | Cluster only | Wrong topology (this ADR's trigger) |
| `SAP/valkey-operator` | Apr 27, 2026 (v0.1.13) | Standalone + Sentinel | v1alpha1, single maintainer, 2 days old at decision time |
| `valkey-io/valkey-operator` | no release | WIP | Upstream marker says "**not ready for production use**" |
| `alauda/redis-operator` | none | Standalone, Sentinel, Cluster | 15 stars, no formal releases |
| Redis Enterprise Operator | active | Sentinel, Cluster | Commercial license — no fit for self-hosters |

None of these clears the bar of: actively maintained + Magento-
appropriate Sentinel topology + Valkey-native + mature CRD with usable
status.

## Decision

**Drop the Redis/Valkey operator dependency entirely for inline mode.**
The operator manages the Valkey workload directly via Kubernetes
primitives:

- **Single-instance** (`spec.cache.redis.replicas: 1`, default for dev):
  one StatefulSet (1 replica) + one ClusterIP Service + one auto-
  generated password Secret + one ConfigMap for `valkey.conf`. Magento
  connects via standard host:port — no Sentinel discovery.

- **HA Sentinel** (`spec.cache.redis.replicas: 3` / 5 / 7, default for
  production):
  - StatefulSet with N Valkey pods. Pod 0 starts as master; pods 1..N-1
    run with `replicaof <store>-valkey-0.<store>-valkey 6379`.
  - Deployment with N Sentinel pods, anti-affinity, configured with
    `sentinel monitor mymaster <store>-valkey-0.<store>-valkey 6379 <quorum>`
    where `quorum = (N/2)+1`.
  - Headless Service `<store>-valkey` for direct pod discovery.
  - ClusterIP Service `<store>-valkey-sentinel` — Magento connects here.
  - The operator emits `MAGENTO_REDIS_HOST=<store>-valkey-sentinel` and
    `MAGENTO_REDIS_SENTINEL_MASTER=mymaster`. The reference image's
    `env.php` reads both and switches `Cm_Cache_Backend_Redis` into
    Sentinel-discovery mode.

- **`spec.cache.redisRef`** keeps its existing semantic but the watched
  CR type is no longer hardcoded to `valkeyv1.Valkey`. Users wanting a
  third-party operator's CR (SAP, OT-CONTAINER-KIT, etc.) point
  `redisRef.name` at any Service in the namespace; the operator emits
  `MAGENTO_REDIS_HOST` from the Service and reads the password from a
  conventional `<service-name>` Secret. Keeps the field operator-
  agnostic.

- **`spec.cache.external`** unchanged.

Quorum-only odd values (`Replicas` enum: 1, 3, 5, 7) — Sentinel needs
an odd voter count to avoid split-brain. 2 and 4 are rejected at the
CRD level.

## Why not pick a Redis operator and integrate?

We considered four alternatives:

1. **Switch to OT-CONTAINER-KIT.** Empty `RedisStatus{}` means we'd be
   reaching into the underlying StatefulSet's status to determine
   readiness — exactly the implementation detail the operator was
   supposed to hide. Net negative integration.

2. **Switch to spotahome.** Project hasn't shipped since 2023, Palark
   documented a data-loss postmortem. Inheriting an unmaintained dep is
   how supply-chain risk creeps in.

3. **Switch to SAP/valkey-operator.** Released 2 days before the
   decision, v1alpha1, single maintainer. Right shape, wrong maturity
   curve for v0.3.0.

4. **Wait for valkey-io/valkey-operator v1.0.** Right answer eventually
   — official upstream, BSD-licensed, both topologies — but explicitly
   not production-ready as of April 2026. Track for 2026/2027.

Owning ~200 lines of Go for two StatefulSets + a Deployment + a Service
+ a Secret + a ConfigMap is cheaper than every alternative, and keeps
our dep graph clean (mariadb-operator stays the only operator we
hard-depend on). The Sentinel logic is mechanical: `replicaof` for the
StatefulSet, `sentinel monitor` for the Sentinel ConfigMap. No magic.

## What this changes for users

- **`spec.cache.redis: {}` defaults to single-instance** in v0.3.0
  (matches v0.2.x positioning). Set `replicas: 3` for production HA.
- **Existing v0.2.x deployments lose their Valkey CR on upgrade.** Cache
  is regenerable, so the operational impact is one cold-cache window
  during the upgrade. Sessions stored in Redis are lost — users with
  Redis-backed sessions should drain or accept logout. Document in
  v0.3.0 release notes.
- **`spec.cache.redisRef`** semantics change: now points at a Service,
  not at a Valkey CR. Users with v0.2.x manifests using `redisRef`
  against a hyperspike Valkey CR migrate to either inline mode or
  `external` with the Service DNS name. Document in upgrade notes.
- **`hyperspike/valkey-operator` is no longer required.** Remove from
  `scripts/install-dep-operators.sh`, the chart RBAC, and the cluster
  if you don't use it for anything else.

## What this doesn't change

- `MAGENTO_REDIS_*` env var contract (image-contract.md).
- The CRD field name `spec.cache.redis` — wire-compat means the user-
  visible name stays Redis even when the in-cluster image is Valkey.
- The `external` mode — unchanged.
- The discriminated-union shape from ADR 004 — still inline / ref /
  external.

## Open questions (deferred)

- **Online cross-topology resize (1 ↔ 3+).** *Amendment 2026-05-03:*
  no longer planned as a v0.4.x feature. The target audience is
  traditional-hosting shops that pick a topology at install time;
  the once-per-store-ever 1 → 3 promotion event is rare enough that
  180-300 min of orchestration code (Sentinel-aware downsize that
  preserves the promoted master, quorum-aware upsize sequencing) is
  poor ROI. Within-topology resize (e.g. 3 → 5) stays online —
  Sentinel auto-discovers new replicas. Cross-topology changes are
  refused at reconcile time; the supported workflow is the
  annotation-based opt-in `mage.aknostic.com/cache-topology-change:
  "approved"`, which makes the operator delete the existing cache
  resources and re-materialise the new topology fresh, accepting
  cache regeneration and (for Redis-backed sessions) logout. Lands
  in v0.4.0 alongside the Sentinel-aware bootstrap fix.
- **TLS-to-clients.** *Amendment 2026-05-03:* deferred to v1.0.0+
  pending user/developer demand. The operator's deployment model is
  "everything in-cluster" (Magento, Valkey, MariaDB, OpenSearch,
  RabbitMQ all on the same pod network), so the threat model
  TLS-to-clients addresses is structurally moot for the target
  audience. Customers needing TLS-to-clients use `redisRef`/`external`
  against a TLS-terminated front door. Will revisit if a paying
  client or clear public-issue volume forces a re-read.
- **Backups.** Not addressed at this layer. Users responsible per ADR
  004's general dep-data philosophy.
- **Migration path from v0.2.x Valkey CR data.** No automated
  migration. Cache regenerates; sessions are lost. v0.2.x users
  worried about session continuity should switch to `external` against
  a managed Redis before upgrading.

## References

- DX-walkthrough findings (2026-04-29) — `CHANGELOG.md` `[Unreleased]`
- Magento issue [#39224](https://github.com/magento/magento2/issues/39224) — Cluster mode CROSSSLOT
- Magento PR [#11222](https://github.com/magento/magento2/pull/11222) — Sentinel discovery for cache + sessions + page_cache
- `colinmollenhour/Cm_Cache_Backend_Redis` README deprecation notice
- Operator-landscape research, 2026-04-30

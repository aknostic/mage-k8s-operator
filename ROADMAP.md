# Roadmap

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

mage-operator is alpha software. The README says not to use it in
production yet; this page says what has to be true before that
changes, so you can judge how far away it is and help close the gap.

This roadmap is open to the public. Everything marked **proposed**
is a draft for you to change: read it, and tell us what is missing,
wrong or too cautious. See [Open questions](#open-questions) and
[How to influence the roadmap](#how-to-influence-the-roadmap).

Dates are targets, not promises.

| Stage | Version | Target | CRD |
|---|---|---|---|
| [Now](#now--v07x) | v0.7.x | In progress | `v1alpha1` |
| [Beta](#beta) | v0.x, to be named | Q1 2027 (proposed, [open question 1](#open-questions)) | `v1beta1` (proposed) |
| [1.0](#10) | v1.0.0 | No date yet | To be decided |

## Now — v0.7.x

**Goal:** a security baseline for the workloads the operator
generates. The design is in
[ADR 014](docs/internals/adr/014-security-baseline.md).

Status of each item, from the `[Unreleased]` section of
[`CHANGELOG.md`](CHANGELOG.md):

- **NetworkPolicies** for generated workloads, through
  `spec.security.networkPolicies`. *Status: CRD shape only.* The
  field is a placeholder; controller wiring is planned for later
  v0.7.0 sessions.
- **Workload `securityContext`** on every generated pod, through
  `spec.security.workloadSecurityContext`. *Status: CRD shape
  only.* The field is a placeholder; controller wiring is planned
  for later v0.7.0 sessions.
- **Admin URL randomisation.** A random `backendFrontname` is
  generated at first bootstrap and stored in the `<store>-admin`
  Secret. *Status: ships end to end*, including the routing-layer
  fix.
- **First release on `ghcr.io`.** *Status: not released.* The
  chart bump and tag wait until the v0.7.0 work is complete. Until
  then, the operator is built from source.

**Upgrading from v0.6.x.** Moving an existing store to v0.7.0 does
not switch on default-deny NetworkPolicies, non-root pods or a
random admin URL. Stores created on v0.6.x keep `/admin/`, so
existing bookmarks keep working. See the upgrade-safety rule in
[ADR 014](docs/internals/adr/014-security-baseline.md).

What is tested where today:

| Environment | What runs | Source |
|---|---|---|
| kind | Unit and envtest suites on every pull request; full e2e suite (including the upgrade arc) nightly and on demand | [`ci.yml`](.github/workflows/ci.yml), [`e2e.yml`](.github/workflows/e2e.yml) |
| Scaleway Kapsule | Manual validation of a fresh install; the most recent recorded run is v0.6.3 | [ADR 014](docs/internals/adr/014-security-baseline.md) |
| EKS, GKE, AKS | Not tested | |

## Beta

**Target:** Q1 2027, proposed. See [open question 1](#open-questions).

Beta means the API is stable enough that you can plan around it, and
the operational basics are documented and tested. The criteria below
are **proposed**. They have not been agreed, and they can change:
see [How to influence the roadmap](#how-to-influence-the-roadmap).

| # | Criterion (proposed) | Today |
|---|---|---|
| 1 | CRD `v1beta1`, with a tested conversion or migration path from `v1alpha1` | CRD is `v1alpha1` only |
| 2 | Backup and restore documented and tested for the database and for media | Not owned by the operator; the [upgrade guide](docs/operations/upgrades.md) tells you to arrange your own backup |
| 3 | Upgrade path tested across at least three Magento or Mage-OS versions | The reference image is Magento 2.4.8-p4 via the Mage-OS mirror. Which versions to cover is [open question 2](#open-questions) |
| 4 | Tested on EKS, GKE, AKS and Scaleway Kapsule | Kapsule validated manually; the other three not tested |
| 5 | Three adopters running the operator in staging | See [`ADOPTERS.md`](ADOPTERS.md) for the current list |
| 6 | Documented resource sizing | Not yet documented |

## Open questions

These are open for you to answer. Propose an answer with a pull
request against this file, or open an issue. A concrete proposal
with your reasoning is the most useful form. The maintainers decide;
see [How decisions are made](#how-decisions-are-made).

1. **Beta date.** Is Q1 2027 realistic for the six criteria above?
   What would you move, drop or add?
2. **Versions to test.** Which three or more Magento or Mage-OS
   versions should the upgrade path cover? Which one do you run?
3. **Adopter count.** Is three adopters in staging the right bar for
   beta? Does a staging adopter count the same as a production one?
4. **"Production user".** Proposed definition: an adopter who runs a
   store on the operator in a production environment, once beta is
   reached. Does that match how you would use the term?
5. **1.0 criteria.** The list under [1.0](#10) is a starting point.
   What would you need to see before running the operator in
   production for a customer?

## 1.0

**Target:** no date yet.

1.0 means we would run the operator in production for a customer.
No criteria have been agreed. The list below is **proposed** and is
the subject of [open question 5](#open-questions).

- Every beta criterion met, with the CRD frozen.
- A minimum period of beta with production users: how long, and how
  many users, is open.
- A support policy: which operator and Magento versions receive
  fixes is open.
- A security review or equivalent: who carries it out is open.
- Upgrade between operator releases documented and tested.

## Non-goals

- **Building or modifying the Magento image.** You bring the image;
  the operator wires configuration into it. See the
  [image contract](docs/image-contract.md).
- **Adobe Commerce Cloud ECE-Tools mode.** Configuration is emitted
  as individual environment variables
  ([ADR 002](docs/internals/adr/002-config-emission-modes.md)). The
  ECE-Tools path may be reconsidered if users ask for it.
- **Generating `env.php`.** Your image's `env.php` reads the
  environment variables the operator sets.

## How to influence the roadmap

- **Edit this file.** Pull requests against `ROADMAP.md` are
  welcome: a changed criterion, a new open question, a better
  answer to an existing one. Follow
  [`CONTRIBUTING.md`](CONTRIBUTING.md). For anything beyond a small
  fix, open an issue first so we can agree the approach.
- **Answer an open question.** Use a pull request or an issue, as
  described under [Open questions](#open-questions).
- **Propose or vote on an idea:** open a post in the **Ideas**
  category of GitHub Discussions.
  `[GAP: Discussions is currently disabled on the repository; enabling it is Matthijs's call]`
  Until then, open an issue.
- **Propose a design change:** read the
  [ADRs](docs/internals/adr/), then open an issue before a pull
  request.
- **Tell us what you run:** add yourself to
  [`ADOPTERS.md`](ADOPTERS.md). What adopters need shapes what
  comes next.
- **Report a security issue:** use [`SECURITY.md`](SECURITY.md), not
  a public issue.

### How decisions are made

The maintainers decide what goes into this roadmap and into the
operator. We read every proposal and say why we accept or decline
it. Any change to a beta or 1.0 criterion is recorded in an
[ADR](docs/internals/adr/), so the reasoning stays public.

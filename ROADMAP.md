# Roadmap

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

mage-operator is alpha software. The README says not to use it in
production yet; this page says what has to be true before that
changes, so you can judge how far away it is and help close the gap.

Dates are targets, not promises. Where a date or criterion has not
been agreed, it is marked `[GAP: …]`.

| Stage | Version | Target | CRD |
|---|---|---|---|
| [Now](#now--v07x) | v0.7.x | In progress | `v1alpha1` |
| [Beta](#beta) | v0.x, to be named | `[GAP: target date — Q1 2027 proposed in the launch plan, not yet agreed by the maintainers]` | `v1beta1` |
| [1.0](#10) | v1.0.0 | `[GAP: no date set]` | `v1` `[GAP: confirm CRD version at 1.0]` |

## Now — v0.7.x

**Goal:** a security baseline for the workloads the operator
generates. The design is in
[ADR 014](docs/internals/adr/014-security-baseline.md).

- **NetworkPolicies** for generated workloads, through
  `spec.security.networkPolicies`.
- **Workload `securityContext`** on every generated pod, through
  `spec.security.workloadSecurityContext`.
- **Admin URL randomisation.** A random `backendFrontname` is
  generated at first bootstrap and stored in the `<store>-admin`
  Secret. Stores created on v0.6.x keep `/admin/` so existing
  bookmarks keep working.
- **First release on `ghcr.io`.** Until v0.7.0 is published, the
  operator is built from source.

What is tested where today:

| Environment | What runs | Source |
|---|---|---|
| kind | Unit and envtest suites on every pull request; full e2e suite (including the upgrade arc) nightly and on demand | [`ci.yml`](.github/workflows/ci.yml), [`e2e.yml`](.github/workflows/e2e.yml) |
| Scaleway Kapsule | Manual validation of a fresh install; the most recent recorded run is v0.6.3 | [ADR 014](docs/internals/adr/014-security-baseline.md) |
| EKS, GKE, AKS | Not tested | |

Progress on the v0.7.0 work is in the `[Unreleased]` section of
[`CHANGELOG.md`](CHANGELOG.md).

## Beta

**Target:** `[GAP: target date — Q1 2027 proposed, not yet agreed]`.

Beta means the API is stable enough that you can plan around it, and
the operational basics are documented and tested. The criteria below
are proposed in the launch plan; they are not yet agreed by the
maintainers. `[GAP: sign-off on every criterion by both maintainers]`

| # | Criterion | Today |
|---|---|---|
| 1 | CRD `v1beta1`, with a tested conversion or migration path from `v1alpha1` | CRD is `v1alpha1` only |
| 2 | Backup and restore documented and tested for the database and for media | Not owned by the operator; the [upgrade guide](docs/operations/upgrades.md) tells you to arrange your own backup |
| 3 | Upgrade path tested across at least three Magento or Mage-OS versions | The reference image is Mage-OS 2.4.8-p4; `[GAP: list of versions to cover]` |
| 4 | Tested on EKS, GKE, AKS and Scaleway Kapsule | Kapsule validated manually; the other three not tested |
| 5 | Three adopters running the operator in staging | `[GAP: current count — see ADOPTERS.md]` |
| 6 | Documented resource sizing | Not yet documented |

These criteria also define "production user" for this project: an
adopter who runs a store on the operator in a production
environment, once beta is reached. `[GAP: confirm this definition
with the maintainers]`

## 1.0

**Target:** `[GAP: no date set]`.

1.0 means we would run the operator in production for a customer.
The launch plan has not yet defined the criteria, so the list below
is a starting point for the maintainers, not a commitment.
`[GAP: criteria to be proposed and agreed by the maintainers]`

- Every beta criterion met, with the CRD frozen.
- `[GAP: minimum period of beta with production users, and how many]`
- `[GAP: support policy — which operator and Magento versions receive fixes]`
- `[GAP: security review or equivalent, and who carries it out]`
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

- **Propose or vote on an idea:** open a post in the **Ideas**
  category of GitHub Discussions.
  `[GAP: Discussions is not confirmed as enabled on the repository]`
- **Propose a design change:** read the
  [ADRs](docs/internals/adr/), then open an issue before a pull
  request, as described in [`CONTRIBUTING.md`](CONTRIBUTING.md).
- **Tell us what you run:** add yourself to
  [`ADOPTERS.md`](ADOPTERS.md). What adopters need shapes what
  comes next.
- **Report a security issue:** use [`SECURITY.md`](SECURITY.md), not
  a public issue.

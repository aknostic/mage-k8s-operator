# Contributing

> Not affiliated with, endorsed by, or sponsored by Adobe Inc.
> "Magento" and "Adobe Commerce" are trademarks of Adobe Inc.

Issues and pull requests are welcome. For anything beyond a small
fix, please open an issue first so we can agree on the approach,
and read the relevant [ADR](docs/internals/adr/) — especially
ADR 004 (seeded-but-extractable dependencies) and ADR 006
(workload naming + Gateway API routing).

Security issues go through [`SECURITY.md`](SECURITY.md), not public
issues.

## Contributing without writing Go

Most of what makes this operator useful is Magento knowledge, not
Go. You can help without touching `internal/`:

- **Run it on your cluster and file what breaks.** Follow the
  [quickstart](docs/quickstart.md), then open an issue with your
  `MageStore` manifest (secrets removed), the operator version, the
  output of `kubectl describe magestore <name>`, and the operator
  logs. [`docs/troubleshooting.md`](docs/troubleshooting.md) explains
  the condition reasons you will see.
- **Review the Magento defaults.** The operator ships defaults for
  cron groups ([`internal/controller/ensure_cron.go`](internal/controller/ensure_cron.go)),
  queue consumers ([`internal/controller/ensure_consumers.go`](internal/controller/ensure_consumers.go),
  [`internal/magento/consumers.go`](internal/magento/consumers.go))
  and the Varnish VCL ([`internal/magento/vcl.go`](internal/magento/vcl.go)).
  If a default is wrong for current Magento or Mage-OS, open an issue
  with the reason and a source link. Issues labelled
  `magento-expertise` are written for this.
- **Improve the docs.** Fix a command that no longer works, add a
  walkthrough for the cluster you run, or clarify a page that cost you
  time. Docs-only PRs need no Go toolchain and no CHANGELOG entry.
- **Send an adoption report.** Tell us what you run: Magento, Adobe
  Commerce or Mage-OS version, cluster type, store size, and what
  worked or did not. Open an issue titled `Adoption report: <short
  description>`. Leave out anything you consider private.

You don't need to run the 60–90 minute e2e suite. A maintainer runs it
with the `run-e2e` label.

Look for [`good first issue`](https://github.com/aknostic/mage-k8s-operator/labels/good%20first%20issue)
and [`help wanted`](https://github.com/aknostic/mage-k8s-operator/labels/help%20wanted)
to find a starting point.

## Development setup

You need:

- Go — `go.mod` pins the toolchain, and `go` downloads it on first use.
- Docker and [kind](https://kind.sigs.k8s.io/) for local clusters and
  the e2e suite.
- `kubectl` and `helm`.

`make` downloads the other tools (controller-gen, setup-envtest,
golangci-lint) into `bin/`.

## Tests

```bash
make test       # regenerates CRDs/deepcopy, then unit + envtest
make lint       # golangci-lint
make test-e2e   # kind cluster + dependency operators + full suite (~60-90 min)
```

`make test` regenerates code under `api/`, `config/crd/` and the
chart CRD. Commit those changes; CI fails when generated files are
stale.

To try a change on a local cluster, `scripts/dev-k8s-setup.sh`
creates a kind cluster with the dependency operators. Then build
and install the operator as described in the
[quickstart](docs/quickstart.md#step-2--install-the-mage-operator).

## Pull requests

- CI runs lint, tests, `govulncheck`, a secret scan, Helm/manifest
  linting and a check that the repo only contains example e-mail
  addresses (`example.com` and friends). All of it must pass.
- Add an entry under `[Unreleased]` in [`CHANGELOG.md`](CHANGELOG.md).
  Docs- or CI-only PRs can skip this; a maintainer adds the
  `skip-changelog` label.
- The e2e suite does not run on every PR. A maintainer starts it with
  the `run-e2e` label when a change needs it.
- Keep the non-affiliation disclaimer in user-facing docs.
- Match the commit-message style of the existing log
  (`area: summary`, e.g. `docs:`, `ci:`, `deps:`).

## License

By contributing, you agree that your contributions are licensed under
the [Apache License 2.0](LICENSE).

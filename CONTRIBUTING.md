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

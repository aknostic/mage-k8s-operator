## What and why

<!-- What does this change, and what problem does it solve? Link the issue or ADR if there is one. -->

## Checklist

See [CONTRIBUTING.md](../CONTRIBUTING.md).

- [ ] The commit messages follow the existing style (`area: summary`, for example `docs:`, `ci:`, `deps:`).
- [ ] I added an entry under `[Unreleased]` in `CHANGELOG.md`, or this PR is docs- or CI-only and a maintainer can add the `skip-changelog` label.
- [ ] I ran `make test`, and committed the regenerated files under `api/`, `config/crd/` and the chart CRD.
- [ ] I ran `make lint`.
- [ ] User-facing docs keep the non-affiliation notice.
- [ ] Any e-mail addresses in docs or samples use `example.com`.
- [ ] If this needs the e2e suite, I said so below so a maintainer can add the `run-e2e` label.

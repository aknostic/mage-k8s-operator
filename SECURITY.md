# Security policy

## Reporting a vulnerability

Please report security issues privately through GitHub's
[private vulnerability reporting](https://github.com/aknostic/mage-k8s-operator/security/advisories/new),
not in a public issue or pull request.

Include what you found, how to reproduce it, and the version or
commit you tested against. We will acknowledge the report and keep
you posted while we work on a fix.

## Supported versions

The project is pre-1.0. Security fixes land on `main` and ship in
the next release; older releases are not patched.

## Verifying releases

Release images and Helm charts are signed with cosign. See
[Cosign verification](README.md#cosign-verification) in the README.

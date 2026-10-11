# Dependency policy

## Supported Go toolchain

The module declares `go 1.24.0` in `go.mod`. Go 1.24 is the minimum supported toolchain for this repository: it is the declared language/module baseline and is required by the pinned `github.com/go-jose/go-jose/v4 v4.1.4` dependency. CI currently tests with Go 1.27; that CI choice does not raise the documented minimum. Developers should test both the minimum supported toolchain and the CI toolchain when changing compatibility-sensitive code.

## Direct dependencies

The current direct dependency set is intentionally small and pinned in `go.mod`; `go.sum` records module content checksums.

| Module | Current version | Purpose | Review boundary |
|---|---:|---|---|
| [`github.com/coreos/go-oidc/v3`](https://github.com/coreos/go-oidc) | `v3.15.0` | OIDC discovery and ID-token verification for the configured single issuer. | Keep issuer, audience, signature/JWKS, expiry, nonce and claim checks explicit in NexaRoute; library use does not replace those policy tests. |
| [`github.com/go-jose/go-jose/v4`](https://github.com/go-jose/go-jose) | `v4.1.4` | JOSE/JWT cryptographic primitives used by the OIDC verification path. This version is pinned because older `v4.0.5` was reported reachable for GO-2026-4945; the repository's security review records the upgrade rationale. | Verify the advisory and patched version before any downgrade or replacement; cryptographic API changes require negative-token regression tests. |
| [`go.etcd.io/bbolt`](https://github.com/etcd-io/bbolt) | `v1.4.3` | Embedded, transactional local persistence for OIDC sessions and security audit records. | The store is single-host and must not be represented as distributed; preserve permission, atomicity, restart and fail-closed tests. |
| [`golang.org/x/oauth2`](https://pkg.go.dev/golang.org/x/oauth2) | `v0.30.0` | OIDC Authorization Code exchange and PKCE S256 flow support. | Preserve state, nonce, browser binding, verifier and token-exchange protections; do not log authorization codes, tokens or secrets. |

These entries describe the direct module requirements, not the full transitive graph. Transitive modules must also be reviewed and verified.

## Dependency change and audit procedure

For every dependency addition, removal, replacement or version change:

1. State the feature need and why the standard library or an existing dependency is not sufficient. Prefer the smallest maintained dependency surface.
2. Inspect the upstream project, release notes, supported Go versions, license, maintenance status and relevant security advisories. Confirm that the selected version is compatible with `go 1.24.0`.
3. Review the complete diff to `go.mod` and `go.sum`; reject unexplained transitive churn. Do not use `replace` directives to conceal an upstream version or checksum issue.
4. Run `./scripts/check-deps.sh`, `go test ./...`, `go vet ./...`, and the repository's `./scripts/verify.sh`. The required CI vulnerability scan and code scanning must pass on the PR head. Run `govulncheck ./...` locally when the tool is installed; absence of the local tool is not evidence that the graph is vulnerability-free.
5. Record material rationale and security decisions in the PR. Upgrade promptly when a reachable vulnerability is fixed; do not suppress a finding without documenting reachability analysis and an approved mitigation.

`./scripts/check-deps.sh` verifies the selected Go toolchain and module graph, checks downloaded module checksums, and runs `govulncheck` when available. It prints a warning when the optional local scanner is absent. CI remains authoritative for the configured vulnerability scan.

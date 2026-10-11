# M1-8 — Dependency Policy Alignment

**Date:** 2026-10-11
**State:** DONE — dependency audit and full verification passed on Go 1.27.2

## Source-of-truth inventory

`go.mod` declares `go 1.24.0`, four direct requirements, and one explicit indirect requirement. `docs/DEPENDENCIES.md` now lists each module at the version declared by `go.mod`; `golang.org/x/sys` remains marked indirect.

| Module | Version | Requirement |
|---|---|---|
| `github.com/coreos/go-oidc/v3` | `v3.15.0` | direct |
| `github.com/go-jose/go-jose/v4` | `v4.1.4` | direct |
| `go.etcd.io/bbolt` | `v1.4.3` | direct |
| `golang.org/x/oauth2` | `v0.30.0` | direct |
| `golang.org/x/sys` | `v0.31.0` | indirect |

`go mod why -m golang.org/x/sys` reports the chain `internal/securitystore` → `go.etcd.io/bbolt` → `golang.org/x/sys/unix`.

## Local vulnerability scan instructions

`docs/DEPENDENCIES.md` now documents installing `golang.org/x/vuln/cmd/govulncheck` with the same current patched toolchain used for scanning, adding `$(go env GOPATH)/bin` to `PATH`, and running `govulncheck ./...`; once installed, `scripts/check-deps.sh` runs the same scan.

## Validation

On the preinstalled Go 1.26.8, the initial `check-deps.sh` run verified the module graph but `govulncheck` reported 10 standard-library findings, including [GO-2026-6617](https://pkg.go.dev/vuln/GO-2026-6617), which is fixed in Go 1.26.9 and Go 1.27.2. The local toolchain was behind the patched CI line; no `go.mod` change was needed.

The scanner was then rebuilt with Go 1.27.2, matching the current `1.27.x` CI line. `./scripts/check-deps.sh` passed: all module checksums verified, zero code-reachable vulnerabilities were found, and `govulncheck` v1.9.0 reported one module-only advisory in `golang.org/x/sys@v0.31.0` (`GO-2026-5024`, fixed in `v0.44.0`). The advisory is for Windows `NewNTUnicodeString`; verbose analysis reports no affected symbol in this code. The unchanged indirect pin is documented rather than silently upgraded in this documentation-only item. See [the advisory](https://pkg.go.dev/vuln/GO-2026-5024) and the committed verbose output.

`./scripts/verify.sh` completed with `VERIFY PASS` on Go 1.27.2. No project dependency versions or `go.mod` entries were changed.

## Evidence

- `docs/reports/m1-8-check-deps-go1.27.2.raw.txt` — complete successful `check-deps.sh` output with `govulncheck`.
- `docs/reports/m1-8-govulncheck-verbose.raw.txt` — full verbose analysis identifying the module-only advisory and its platform.
- `docs/reports/m1-8-verify.raw.txt` — full `verify.sh` output ending in `VERIFY PASS`.

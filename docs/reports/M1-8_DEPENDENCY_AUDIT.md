# M1-8 — Dependency Policy Alignment

**Date:** 2026-10-11
**State:** IN PROGRESS — documentation aligned; dependency checks pending

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

`docs/DEPENDENCIES.md` now documents installing `golang.org/x/vuln/cmd/govulncheck`, adding `$(go env GOPATH)/bin` to `PATH`, and running `govulncheck ./...`; once installed, `scripts/check-deps.sh` runs the same scan.

## Validation

Pending: run `./scripts/check-deps.sh` with the local scanner installed and run `./scripts/verify.sh`. Raw command output will be committed before marking this item DONE. No project dependency versions or `go.mod` entries are being changed.

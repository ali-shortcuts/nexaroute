# Phase 1 integration report — 2026-10-09

Repository: `ali-shortcuts/nexaroute`  
Base: `phase1a-encrypted-secrets` / PR #211  
Branch: `phase1b-on-1a`  
PR: [#215](https://github.com/ali-shortcuts/nexaroute/pull/215), stacked on Phase 1a and referencing #212

## Acceptance matrix

| Requirement | Status | Evidence |
|---|---|---|
| Phase 1a encrypted secrets remain intact | **PASS** | Existing Phase 1a closeout tests/report; PR #211 CI is green: CodeQL, Security/CodeQL, Go vulnerability scan, CI/verify. |
| Phase 1b TLS/mTLS and browser CSRF retained | **PASS** | Existing Phase 1b transport tests plus integrated branch tests; TLS 1.2 minimum, reloadable certificates, independently scoped Admin/data-plane client certificates, same-origin/CSRF checks. |
| Canonical security document | **PASS** | `docs/SECURITY.md` now contains encrypted-secret, TLS/mTLS, and browser-CSRF behavior; root `SECURITY.md` is a pointer only. |
| Config CLI uses encrypted loader semantics | **PASS** | `cmd/gateway/config_integration_test.go`: legacy plaintext `dry-run` migrates to `nxs1:` with no plaintext residue; encrypted `diff` does not change the file and does not disclose values. Migration side effect is documented in `docs/OPERATIONS.md`. |
| TLS plus encrypted config startup | **PASS** | `TestGatewayStartsWithTLSAndEncryptedConfig` starts the gateway with an encrypted Admin key and HTTPS listener, obtains `/healthz` over TLS, and shuts down cleanly. |
| Config CLI secret redaction and read-only behavior | **PASS** | Existing `config_test.go` plus the new legacy/encrypted integration test. |
| Required gates | **PASS** | Raw output in `PHASE1_INTEGRATION_GATES_RAW_2026-10-09.txt`; all gates pass after building the release fixtures required by `test-install.sh`. |

## Raw gate evidence

See [PHASE1_INTEGRATION_GATES_RAW_2026-10-09.txt](PHASE1_INTEGRATION_GATES_RAW_2026-10-09.txt).

- `gofmt -l .`: empty.
- `go vet ./...`: pass.
- `go test -race -count=1 ./...`: pass.
- `./scripts/verify.sh`: `VERIFY PASS`; browser control-plane E2E and Live Visual Agent E2E both passed; short fuzz checks passed; amd64/arm64 builds passed.
- `./scripts/smoke-local.sh`: `SMOKE PASS`.
- `./scripts/build-release.sh v0.7.0`: release fixtures and checksums created successfully.
- `./scripts/test-install.sh`: `INSTALL PASS`; installer E2E: `INSTALLER E2E PASS`.
- `go test ./... -coverprofile=/tmp/nexaroute-phase1-integration-cover.out`: pass.
- `go tool cover -func=/tmp/nexaroute-phase1-integration-cover.out | tail -1`: `total: (statements) 77.4%`.
- `git diff --check`: empty.

The first `test-install.sh` invocation in the transcript failed before executing tests because the repository had no `dist/` release fixtures. Running the documented prerequisite `scripts/build-release.sh v0.7.0` fixed that harness precondition; the subsequent installer gate passed without code changes.

## Coverage

| Package | Before integrated tests | After | Change |
|---|---:|---:|---:|
| `cmd/gateway` | 69.5% | 73.6% | +4.1 pp |
| `internal/config` | 70.7% | 70.7% | 0.0 pp |
| `internal/compat` | 70.2% | 70.2% | 0.0 pp |
| `internal/httpapi` | 73.7% | 73.7% | 0.0 pp |
| `internal/decision` | 68.0% | 68.0% | 0.0 pp |
| `internal/probe` | 71.8% | 71.8% | 0.0 pp |
| `internal/logging` | 72.7% | 72.7% | 0.0 pp |
| `internal/desktop` | 70.9% | 70.9% | 0.0 pp |
| **Repository total** | **77.3%** | **77.4%** | **+0.1 pp** |

The requested **85% repository target was not reached**. The new tests cover the Phase 1 integration risks and raise `cmd/gateway` above the 70% integration threshold, but broad additional testing across the large low-coverage surfaces—particularly HTTP API handlers, decision orchestration, probe engine, config validation helpers, logging rotation branches, and desktop process-launch failure paths—would still be required. No assertion-free or behaviorless padding was added.

## PR/CI status at report time

- PR #211: open; all four checks successful.
- PR #212: open; all four checks successful; it remains the unstacked historical Phase 1b implementation and is not merged.
- PR #214: open; local race/full-suite evidence is recorded in the client-auth report; GitHub CI was pending at report time.
- PR #215: open; local full gates pass; GitHub CI was pending at report time.

No branch was merged and no pushed history was rewritten.

## Explicitly not completed

1. Repository coverage is 77.4%, not 85%.
2. PRs #211, #214, and #215 remain open and require review/merge; PR #212 remains open as the historical unstacked implementation.
3. Phase 2 was not started: RBAC/SSO, strict egress policy, durable store adapters, and keyring/external key-management work remain next steps only.

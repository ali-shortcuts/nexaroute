# Phase 1b Implementation Report — 2026-10-09

## Status

| Phase | Status | Pull request | Result |
|---|---|---|---|
| 1a — encrypted secrets | **PARTIAL** | [PR #211](https://github.com/ali-shortcuts/nexaroute/pull/211) | Open, not merged; GitHub reported `UNSTABLE` when checked. Its changes are not in this Phase 1b branch, which was deliberately created from `main`. |
| 1b — transport, browser CSRF and config CLI | **PARTIAL** | [PR #212](https://github.com/ali-shortcuts/nexaroute/pull/212) | Implementation and required gates are complete against the current `main` snapshot; PR is open and unmerged. Integration checks listed below remain after Phase 1a lands. |

No PR was merged. Phase 1a and Phase 1b remain separate branches/PRs as requested.

## Acceptance matrix

| Requirement | Result | Evidence |
|---|---|---|
| Optional HTTPS listener with TLS 1.2 minimum | **PASS** | `internal/transport/tls.go`; config and startup validation tests |
| Reload certificate/key/client CA for new TLS handshakes | **PASS** | Generated-certificate rotation test verifies serial 1 → 2 without rebuilding the TLS config |
| Optional mTLS gates scoped independently to Admin API and `/v1/*` | **PASS** | Config validation, client-CA loading tests, and positive/negative HTTP middleware tests |
| Browser Admin mutations use same-origin checks and CSRF token | **PASS** | Missing-token and cross-origin requests rejected; valid same-origin token accepted; cookie flags tested on HTTP and HTTPS |
| Config CLI: `validate`, `diff`, `dry-run`; exit codes documented/tested | **PASS** | `cmd/gateway/config.go` and CLI tests: 0 success, 1 config/preparation failure, 2 usage error |
| Config diff never renders secret values | **PASS** | Redaction test covers API keys, custom headers, and proxy URL values |
| Operational/security docs and capability/gap status updated | **PASS** | README, root `SECURITY.md`, `docs/CONFIGURATION.md`, `docs/OPERATIONS.md`, `docs/KNOWN_GAPS.md`, and `docs/CAPABILITY_MATRIX.md` |

## Validation results

All required gates passed on this branch:

| Command | Result |
|---|---|
| `gofmt -l .` | PASS — no output |
| `go vet ./...` | PASS |
| `go test -race -count=1 ./...` | PASS |
| `./scripts/verify.sh` | PASS — clean unit/integration and race runs, browser control-plane E2E, Live Visual Agent E2E, short fuzz checks, Linux amd64 and arm64 builds |
| `go test ./... -coverprofile=…` | PASS |
| `go tool cover -func=… | tail -1` | **75.0% total statements** |
| `node --check internal/httpapi/web/app.js` | PASS |
| `git diff --check` | PASS |

The full command/output transcript is committed at [PHASE1B_GATES_RAW_2026-10-09.txt](PHASE1B_GATES_RAW_2026-10-09.txt).

## Coverage comparison

Coverage figures below compare the Phase 0 baseline report with the Phase 1b branch; they are not a per-commit causal attribution.

| Package | Phase 0 baseline | Phase 1b | Change |
|---|---:|---:|---:|
| `cmd/gateway` | 22.9% | 41.5% | +18.6 pp |
| `internal/config` | 70.5% | 70.8% | +0.3 pp |
| `internal/httpapi` | 73.4% | 73.7% | +0.3 pp |
| `internal/transport` | — | 90.6% | new package |
| **All statements** | **75.0%** | **75.0%** | **0.0 pp** |

## Implemented behavior and boundaries

- TLS is opt-in and applies to the shared gateway listener (UI, Admin API, and data plane). It does not add separate Admin and data-plane sockets.
- mTLS client-certificate requirements are independently applied to Admin API and data-plane routes. The Admin API key/local-loopback authorization boundary remains in place; a client certificate does not replace it.
- Certificate changes take effect on new TLS handshakes; established connections are not renegotiated.
- Browser CSRF protection uses an `HttpOnly`, `SameSite=Strict` cookie and `X-NexaRoute-CSRF`, plus matching `Origin`/`Referer` validation. Stateless non-browser Admin clients with no browser origin metadata and no CSRF cookie continue to rely on Admin-key authentication. This is not a cookie-backed Admin login/session system and does not add RBAC or SSO.
- `config diff` emits changed paths/status only; it never prints old or new values.
- In the current `main` snapshot, config CLI tests confirm the commands do not alter the input config. Config loading may perform a required on-disk legacy-format migration once the Phase 1a encrypted-secret changes are integrated.

## Remaining / follow-up items

1. **Merge ordering and integration:** PR #211 is still open and `UNSTABLE`. After it merges, rebase/update PR #212 and rerun `config validate/diff/dry-run` tests against the migrated loader. In particular, preserve and verify the documented migration side effect for legacy plaintext configs.
2. **Security-document reconciliation:** the current `main` snapshot has root `SECURITY.md`; Phase 1a PR #211 adds the canonical `docs/SECURITY.md`. Reconcile the Phase 1b transport/CSRF text into that canonical document after the Phase 1a PR lands, avoiding duplicate/conflicting security sources.
3. **Not implemented in this phase:** Admin RBAC, SSO, cookie-backed identity/session login, strict egress policy, and encrypted-at-rest secrets on `main` until PR #211 lands.

## Branch and PR

- Branch: `phase1b-transport-and-session`
- Commit: `d459c8e` (`feat(security): add TLS mTLS and admin CSRF controls`)
- PR: [#212](https://github.com/ali-shortcuts/nexaroute/pull/212)
- Base: `main`
- PR state: open; not merged.

# Phase 1 coverage on `phase1-coverage-on-1b`

**Branch:** `phase1-coverage-on-1b`
**Base:** `origin/phase1b-on-1a` at `c986bda`
**PR:** [#218](https://github.com/ali-shortcuts/nexaroute/pull/218)
**Scope:** meaningful behavior tests only; no merge, history rewrite, Phase 2, or production-code changes.

## Final result

The remaining approved test work was continued. The reproducible repository coverage is now **79.4%**, up from **79.0%** at the previous stopping point and **78.5%** at the start of this continuation. The 85.0% target is still not reached; no assertion-free or synthetic padding was added.

| Package | Previous | Final | Change in latest pass |
|---|---:|---:|---:|
| `cmd/gateway` | 73.6% | 73.8% | +0.2pp |
| `internal/compat` | 76.0% | 76.0% | 0.0pp |
| `internal/config` | 75.5% | 75.5% | 0.0pp |
| `internal/controlplane` | 83.8% | 83.8% | 0.0pp |
| `internal/guardrail` | 100.0% | 100.0% | 0.0pp |
| `internal/httpapi` | 74.1% | 75.2% | +1.1pp |
| `internal/providers` | 83.8% | 83.8% | 0.0pp |
| `internal/usage` | 95.8% | 95.8% | 0.0pp |
| **Repository total** | **79.0%** | **79.4%** | **+0.4pp** |

The latest tests cover admin CRUD validation, reference-integrity failures and successful deletion flows for virtual endpoints, route profiles, candidate pools and fallback chains, plus legacy admin endpoint validation and key rotation. All added tests assert externally visible behavior.

## Remaining coverage work

The target remains below 85% by **5.6 percentage points**. The largest measured uncovered statement areas are still `internal/config/config.go` (280), `internal/httpapi/admin.go` (276), `internal/httpapi/virtual.go` (remaining branches), `internal/compat/probes.go` (167), `internal/httpapi/anthropic.go` (162), `internal/httpapi/canonical_path.go` (153), and `internal/decision/orchestrator.go` (130). These require broad protocol/admin integration suites rather than padding or a small safe change. No production bug was exposed, so production code was not changed.

This is the only substantive item not completed from the requested target. Phase-2 work remains excluded, no branches were merged, and no history was rewritten.

## Gates and raw evidence

The complete requested local gate set was rerun after the latest tests and passed:

- `gofmt -l .` — empty output
- `go vet ./...` — PASS
- `go test -race -count=1 ./...` — PASS
- `./scripts/verify.sh` — `VERIFY PASS`
- `./scripts/smoke-local.sh` — `SMOKE PASS`
- `./scripts/build-release.sh v0.7.0` — amd64/arm64 artifacts valid
- `./scripts/test-install.sh` — `INSTALL PASS` and installer E2E PASS
- `git diff --check` — PASS
- `go test -count=1 -coverprofile=/tmp/final2.cover ./...` — PASS
- `go tool cover -func=/tmp/final2.cover | tail -1` — **79.4%**

Raw outputs are committed in `docs/reports/phase1-coverage-on-1b-gates.raw.txt`, `phase1-coverage-on-1b-final-tests.raw.txt`, and `phase1-coverage-on-1b-final-packages.raw.txt`. The `.cover` profile and function dump remain outside Git under `/tmp`; no committed `.cover` or `*-func.raw.txt` artifacts remain.

## PR and CI status

PR #218 is open, targets `phase1b-on-1a`, is merge-clean, and its `verify` check is **COMPLETED / SUCCESS**. The exact current check is:

| PR | Check | Status | Result |
|---:|---|---|---|
| #218 | `verify` | COMPLETED | SUCCESS |
| #211 | `verify` | COMPLETED | SUCCESS |
| #211 | `Go vulnerability scan` | COMPLETED | SUCCESS |
| #211 | `CodeQL (Go)` | COMPLETED | SUCCESS |
| #211 | `CodeQL` | COMPLETED | SUCCESS |
| #212 | `verify` | COMPLETED | SUCCESS |
| #212 | `Go vulnerability scan` | COMPLETED | SUCCESS |
| #212 | `CodeQL (Go)` | COMPLETED | SUCCESS |
| #212 | `CodeQL` | COMPLETED | SUCCESS |
| #214 | `verify` | COMPLETED | SUCCESS |
| #215 | `verify` | COMPLETED | SUCCESS |

All five PRs remain open. The requested merge order is **#211 → #215 → #218**. PR #212 is the parallel/non-stacked Phase 1b version represented by the #215 stack, and #214 is redundant with the client-auth body fix already represented in the integrated stack; they are safe to close only after confirming the desired commits are present in the selected merge path. No merge or PR closure was performed.

## Other branches

The read-only audit is in [`OTHER_BRANCHES_AUDIT.md`](OTHER_BRANCHES_AUDIT.md). The video branch passed its own full suite but conflicts with this branch in `cmd/gateway/main.go` and `internal/httpapi/web/app.js`. The virtual-key policy branch passed its full suite and trial-merged cleanly, but remains a separate policy/security change. Neither branch was merged or copied.

## Final handoff status

The branch is left at a clean commit boundary after the latest test/report commit. The only explicit requested objective still unmet is the numerical 85% repository coverage threshold; the exact remaining hotspots and reproducible 79.4% evidence are recorded above. No CI-only failure remains.

# Phase 1 coverage on `phase1-coverage-on-1b`

**Branch:** `phase1-coverage-on-1b`
**Base:** `origin/phase1b-on-1a` at `c986bda`
**Scope:** meaningful behavior tests only; no merge, history rewrite, Phase 2, or production-code changes.

## Result

Coverage was re-measured after expanding into the previously approved packages. The reproducible final total is **79.0%**, up from the branch's recorded **78.5%** before this continuation. The requested **85.0%** threshold was not reached; no assertion-free or synthetic padding was added.

| Package | Earlier recorded final | Current final | Change in this continuation |
|---|---:|---:|---:|
| `cmd/gateway` | 73.6% | 73.6% | 0.0pp |
| `internal/compat` | 71.8% | 76.0% | +4.2pp |
| `internal/config` | 74.8% | 75.5% | +0.7pp |
| `internal/controlplane` | 83.8% | 83.8% | 0.0pp |
| `internal/guardrail` | 100.0% | 100.0% | 0.0pp |
| `internal/httpapi` | 74.0% | 74.1% | +0.1pp |
| `internal/providers` | 79.9% | 83.8% | +3.9pp |
| `internal/usage` | 95.8% | 95.8% | 0.0pp |
| **Repository total** | **78.5%** | **79.0%** | **+0.5pp** |

The added tests exercise config validation/environment/strict-mode behavior, OpenAI Responses probe request/response translation, tool and multimodal probe payloads, provider retry/redaction/endpoint/origin behavior, and streaming-body release/cancellation behavior.

## Why 85% was not reached

The largest remaining measured statement gaps are in behavior-heavy paths that were not covered by the existing suites: `internal/config/config.go` (280 uncovered profile statements), `internal/httpapi/admin.go` (276), `internal/httpapi/virtual.go` (272), `internal/compat/probes.go` (167), `internal/httpapi/anthropic.go` (162), `internal/httpapi/canonical_path.go` (153), and `internal/decision/orchestrator.go` (130). The current total therefore requires a substantial additional HTTP admin/virtual endpoint and protocol integration suite, not a small safe patch. No production bug was exposed by the continuation, so production code was not changed.

## Gates and evidence

All requested local gates passed and their raw output is in `docs/reports/phase1-coverage-on-1b-gates.raw.txt`:

- `gofmt -l .` — empty output
- `go vet ./...` — PASS
- `go test -race -count=1 ./...` — PASS
- `./scripts/verify.sh` — `VERIFY PASS`
- `./scripts/smoke-local.sh` — `SMOKE PASS`
- `./scripts/build-release.sh v0.7.0` — amd64/arm64 artifacts valid
- `./scripts/test-install.sh` — `INSTALL PASS` and installer E2E PASS
- `git diff --check` — PASS after report cleanup
- `go test -count=1 -coverprofile=/tmp/phase2.cover ./...` — PASS
- `go tool cover -func=/tmp/phase2.cover | tail -1` — **79.0%**

The final profile remains in `/tmp` only. Final raw test, function, and per-package outputs are `phase1-coverage-on-1b-final-tests.raw.txt`, `phase1-coverage-on-1b-final-func.raw.txt`, and `phase1-coverage-on-1b-final-packages.raw.txt` under `docs/reports/`; `*.cover` is ignored by Git.

## PR status at audit time

PR #211 (`phase1a-encrypted-secrets`) and PR #215 (`phase1b-on-1a`) were open and all reported checks were successful. PR #212 (`phase1b-transport-and-session`) was also open with successful checks. PR #214 (`phase1a-client-auth-body-fix`) was open with its `verify` check successful. The exact merge order requested remains **#211 -> #215 -> coverage PR**; #212 is the change represented by the #215 stack and #214 is redundant with the already-present client-auth body fix, so both are safe to close only after confirming the corresponding commits are present in the intended stack.

## Other branches

The read-only audit is in `docs/reports/OTHER_BRANCHES_AUDIT.md`. No branch was merged. The video gateway branch passed its own full test suite but has trial-merge conflicts in `cmd/gateway/main.go` and `internal/httpapi/web/app.js`; the virtual-key policy branch passed its full suite and trial-merged cleanly, but remains a separate policy/security change.

## Explicitly not completed

The repository total did not reach 85%. Coverage PR **#218** was created with base `phase1b-on-1a`; it has not been merged. Its GitHub checks are tracked separately below and were not modified by this work. No CI-only failure was changed. The branch is left at a clean commit boundary with the remaining coverage work and exact uncovered-file hotspots documented above.

## Coverage PR checks

At the time of this report, PR #218 had been created and its checks were still pending; the final check names/results must be refreshed with `gh pr checks 218` before merge.

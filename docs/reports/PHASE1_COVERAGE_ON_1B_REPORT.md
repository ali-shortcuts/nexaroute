# Phase 1 Coverage on `phase1b-on-1a`

**Branch:** `phase1-coverage-on-1b`
**Base:** `origin/phase1b-on-1a` at `c986bda`
**Scope:** meaningful behavior tests only; no merge, history rewrite, Phase 2, or expansion into controlplane/providers/usage/guardrail

## Summary

The baseline was measured directly on `origin/phase1b-on-1a`, not on `main`:

- Baseline total: **77.4%**
- Final total: **78.5%**
- Improvement: **+1.1 percentage points**
- Remaining gap to 85%: **6.5 percentage points**

All 19 added tests have unique names relative to the Phase 1b base. No duplicate test names were found and no assertions were weakened.

## Per-package coverage

| Package | Baseline on `phase1b-on-1a` | Final | Change | Gap to 85% |
|---|---:|---:|---:|---:|
| `internal/decision` | 68.0% | 76.4% | +8.4pp | 8.6pp |
| `internal/config` | 70.7% | 74.8% | +4.1pp | 10.2pp |
| `internal/compat` | 70.2% | 71.8% | +1.6pp | 13.2pp |
| `internal/httpapi` | 73.8% | 74.0% | +0.2pp | 11.0pp |
| `internal/probe` | 71.8% | 72.1% | +0.3pp | 12.9pp |
| `internal/logging` | 72.7% | 81.8% | +9.1pp | 3.2pp |
| `internal/desktop` | 70.9% | 85.5% | +14.6pp | 0pp |
| **Repository total** | **77.4%** | **78.5%** | **+1.1pp** | **6.5pp** |

## Ported test files

- `internal/compat/coverage_behavior_test.go`
- `internal/config/coverage_behavior_test.go`
- `internal/decision/coverage_behavior_test.go`
- `internal/decision/orchestrator_behavior_test.go`
- `internal/decision/policy/coverage_behavior_test.go`
- `internal/decision/remote/coverage_behavior_test.go`
- `internal/desktop/coverage_behavior_test.go`
- `internal/httpapi/coverage_history_test.go`
- `internal/logging/coverage_behavior_test.go`
- `internal/probe/coverage_behavior_test.go`

The tests cover configuration persistence and credentials, compatibility sanitizer/classifier behavior, decision registry/result/orchestrator contracts, typed remote errors, policy reload/cancellation, config-history revision guards, logging bounds/failures, browser fallback/locking, probe lease behavior, and desktop lifecycle edge cases.

## Required gates

All required gates passed on `phase1-coverage-on-1b`:

- `gofmt -l .` — empty output
- `go vet ./...` — PASS
- `go test -race -count=1 ./...` — PASS
- `./scripts/verify.sh` — `VERIFY PASS`
- `./scripts/smoke-local.sh` — `SMOKE PASS`
- `./scripts/build-release.sh v0.7.0` — amd64/arm64 artifacts and checksums valid
- `./scripts/test-install.sh` — `INSTALL PASS` and installer E2E PASS
- `go test -count=1 -coverprofile=... ./...` — PASS
- `go tool cover -func ... | tail -1` — **78.5%**
- `git diff --check` — empty output

## Why 85% remains unreachable within this scope

The target cannot be reached by the listed priority packages alone without adding large new suites for behavior outside this request. The repository still contains substantial statement volume in packages and paths explicitly excluded from this task, including provider/control-plane and other non-priority surfaces. Adding assertion-free tests would be invalid, and expanding into those excluded areas requires separate confirmation. Therefore this PR stops at the measured, reproducible **78.5%**.

## Raw evidence

- `phase1-coverage-on-1b-before-tests.raw.txt`
- `phase1-coverage-on-1b-before-cover.raw.txt`
- `phase1-coverage-on-1b-before-func.raw.txt`
- `phase1-coverage-on-1b-before-packages.raw.txt`
- `phase1-coverage-on-1b-after-cover.raw.txt`
- `phase1-coverage-on-1b-after-func.raw.txt`
- `phase1-coverage-on-1b-after-packages.raw.txt`
- `phase1-coverage-on-1b-final.cover`
- `phase1-coverage-on-1b-final-func.raw.txt`
- `phase1-coverage-on-1b-final-cover.raw.txt`
- `phase1-coverage-on-1b-gates.raw.txt`
- `phase1-coverage-on-1b-release-gates.raw.txt`

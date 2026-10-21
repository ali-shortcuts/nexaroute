# Quality Audit Baseline - Issue #593

Date: 2026-10-21
Task: NexaRoute Issue #593 manual audit brief
Implementation branch: `opencode/issue593-20261021073246`
Implementation checkpoint: `6144fc028da766ed5cf3d50dc21a1542356178aa`

## Authoritative repository state

| Role | Commit | Notes |
| --- | --- | --- |
| Published release tag | `v0.23.0` | Tag points at the older audit baseline `05003db9326b01464a368723d8ab31671b40dc8f`. |
| Current `main` before this task | `7a815a4ba5a951a1be5e3cd591e58e3ca7125e00` | Base for this branch (the audit workflow merge, PR #594). |
| This task's first implementation checkpoint | `6144fc028da766ed5cf3d50dc21a1542356178aa` | Code cleanup, cookie hardening, report-directory permissions, and audit workflow extension. |
| Older audit baseline | `05003db9326b01464a368723d8ab31671b40dc8f` | Used only to distinguish the previous audit from this task. |

The open PR #595 (`opencode/issue593-20261021064216`) was created from the older baseline. It is not merged and is not part of this branch. This branch was cut directly from `7a815a4`, so it does not inherit PR #595's changes or reports. The apparent deletion of `.github/workflows/audit.yml` in PR #595 is a base-version artifact, not a change recommended here.

## Official audit run

| Field | Value |
| --- | --- |
| Workflow | Architecture & Quality Audit |
| Run | [39559283242](https://github.com/ali-shortcuts/nexaroute/actions/runs/39559283242) |
| Event | `workflow_dispatch` |
| HEAD SHA | `7a815a4ba5a951a1be5e3cd591e58e3ca7125e00` |
| Status | `completed` / `success` |
| Created | `2026-10-21T07:02:47Z` |
| Updated | `2026-10-21T07:22:32Z` |
| Artifacts | `audit-fuzz`, `audit-staticcheck`, `audit-lint`, `audit-metrics`, `audit-coverage` |

A successful audit workflow means the report-only jobs completed; it does not mean the repository has no findings. The gosec and deadcode jobs added by this task intentionally do not gate CI.

The artifact download endpoint requires an authenticated GitHub token. An unauthenticated request returned HTTP 401, so the artifact bytes were not downloaded in this environment. The `84.3%` coverage figure below comes from the task-provided audit summary and was independently reproduced with the exact workflow command.

## Coverage reconciliation

Workflow commands:

```bash
go test -timeout=40m -count=1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

| Scope | Result | Evidence |
| --- | --- | --- |
| Official `audit-coverage` artifact on `7a815a4` | 84.3% total statements | Task-provided audit summary; local exact-command reproduction also gives 84.3%. |
| Local reproduction on `7a815a4` (pre-change) | 84.3% total statements | `.artifacts/audit-baseline/coverage-func.txt` |
| This task's final checkpoint | 84.5% total statements | `.artifacts/audit-baseline/coverage-final-func.txt` (removing uncovered dead code raised the total by 0.2 percentage points). |
| The `86.2%` in the manual report | Not a repository total | It is `internal/budget` package coverage in older reports such as `docs/reports/phase1-main-gates.raw.txt:187` and `docs/reports/PHASE1B_GATES_RAW_2026-10-09.txt:139`. |
| `internal/budget` at final checkpoint | 85.9% of statements | `go test ./internal/budget -count=1 -cover` |

The `86.2%` vs `84.3%` discrepancy is therefore a scope mismatch: `86.2%` is one package from an older run, while `84.3%` is the current repository total. The report should use `84.3%` as the baseline.

## Repository and toolchain baseline

- Go: `go1.25.0 linux/amd64`; module Go version `1.25.0`.
- Go packages: 57 (`go list ./... | wc -l`).
- `staticcheck 2026.2.2 (0.8.2)`.
- `gosec v2.29.0` (installed with `go install github.com/securego/gosec/v2/cmd/gosec@v2.29.0`; the binary's embedded module version is v2.29.0).
- `deadcode` from `golang.org/x/tools v0.52.0`.
- `gocyclo v0.6.0`.
- `gofmt` and `go vet` from Go 1.25.0.
- Node.js is present for `node --check`; Chromium/Python Playwright is not installed.

## Pre-change finding snapshot

| Signal | Baseline | Notes |
| --- | --- | --- |
| Targeted staticcheck (`SA4006,SA4000,U1000`) | 48 lines: 16 `SA4006`, 1 `SA4000`, 31 `U1000` | `.artifacts/staticcheck-targeted.txt` |
| `deadcode -test ./...` | 25 unreachable functions | `.artifacts/deadcode-baseline.txt` |
| gosec | 159 issues in 232 files, 1 `#nosec` | Top rules: `G304` 54, `G104` 33, `G703` 28, `G115` 14, `G204` 12, `G124` 6, `G101` 6, `G704` 4, `G705` 1, `G301` 1. |
| Unused web i18n keys | 13 keys x 3 locales = 39 lines | Located in `internal/httpapi/web/app.js`. |
| Full `go test -count=1 -timeout=10m ./...` | PASS | Verified before and after the cleanup. |

## Baseline verification commands

```bash
go test -timeout=40m -count=1 -coverprofile=.artifacts/audit-baseline/coverage.out ./...
go tool cover -func=.artifacts/audit-baseline/coverage.out
"$(go env GOPATH)/bin/staticcheck" -checks 'SA4006,SA4000,U1000' ./...
"$(go env GOPATH)/bin/deadcode" -test ./...
"$(go env GOPATH)/bin/gosec" -fmt=text -out=.artifacts/gosec-baseline.txt ./...
```

## Environment limits

- No authenticated GitHub API session (`gh auth status` reports not logged in); artifact bytes and authenticated API queries are unavailable.
- No real browser acceptance: Chromium and Python Playwright are unavailable.
- No live PostgreSQL, Redis, OIDC identity provider, KMS/keyring service, or upstream provider calls were exercised.
- Per-instance breaker, budget, and video job behavior was reviewed from code and documentation, not from a multi-replica deployment.
- Full staticcheck was run after the changes for information only; a pre-change full-staticcheck count is not available, so only the targeted staticcheck baseline is used for before/after claims.

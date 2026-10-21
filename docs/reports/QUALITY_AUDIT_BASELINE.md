# Quality Audit Baseline - Issue #593

- Audit timestamp: **2026-10-21T11:33:58Z**
- Audited `main`: `bfa93b69cd9fc5041b2ddb822a49c03a32eeb13a` (merge of PR #595, which also contains PR #596)
- Work branch: `opencode/issue593-20261021110358`
- Checkpoint commit: `41169e9a4c880dd25894b3ff39fe08abbb49cc42`
- Published release: `v0.23.0` (unchanged, `05003db`; no retag, no new release, no asset mutation)
- Issue: [#593](https://github.com/ali-shortcuts/nexaroute/issues/593) (open)

This file captures the state *before* the fixes in this checkpoint. See
`QUALITY_AUDIT_FINDINGS.md` for per-finding evidence and `QUALITY_AUDIT_FINAL.md`
for the current result.

## Authoritative repository state

| Item | Value |
| --- | --- |
| Default branch | `main` |
| Audit base SHA | `bfa93b69cd9fc5041b2ddb822a49c03a32eeb13a` |
| PR #595 | Merged: `099f474` fix, `ee93c81` docs |
| PR #596 | Merged: `17408a4` audit reports |
| Latest commit on `main` | Merge of PR #595 |
| Release tag | `v0.23.0` -> `05003db` |
| Open issue | #593 |

PR #595 delivered the first batch of confirmed fixes (AUD-01 through AUD-09,
egress TLS `ServerName`, proxy `allowed_hosts`, media `no_training`, structured
output preservation, Anthropic thinking budget, emergency-key/OIDC precedence,
truncated media responses, hedge capability filtering, Anthropic cache usage).
PR #596 recorded that batch's baseline, findings, and verification.

## Official audit run

The post-release workflow run was inspected through the GitHub API, not through
an authenticated local `gh` session:

- Run `31799134744` (`Post-release quality audit`, post-release event) completed
  **success**.
- Run `31799134698` (`Post-release quality audit`, workflow_run event) completed
  **success**.

The local environment has no `GH_TOKEN`; it cannot enumerate new workflow runs,
download artifacts, or create the PR. Evidence files are therefore written under
`.artifacts/issue593-final/` inside the repository (gitignored).

## Coverage reconciliation

| Measurement | Value | Scope |
| --- | --- | --- |
| Recorded strip-total | **84.3%** | raw `go test ./...` profile after excluding generated/UI-only packages |
| This checkpoint profile | **84.5%** | `go test -coverprofile ... ./...`, `go tool cover -func` total |
| Previously reported | **86.2%** | a subset package, not the repository total |

The 84.3% versus 84.5% difference is produced by the new trusted-proxy and parser
tests plus small config changes; it is not a claim that coverage rose materially.
All future reports use the 84.5% figure for the whole repository and label any
subset number explicitly.

## Repository and toolchain baseline

| Tool | Version / command |
| --- | --- |
| Go (local default) | `go1.25.0 linux/amd64` |
| Go (vulnerability scan) | `go1.27.3` via `GOTOOLCHAIN` (matches CI `1.27.x`) |
| staticcheck | `2026.2.2` |
| gosec | `v2.29.0` |
| deadcode | `golang.org/x/tools/cmd/deadcode@v0.52.0` |
| govulncheck | `golang.org/x/vuln/cmd/govulncheck@v1.9.0` (built with Go 1.27.3) |
| Race detector | `go test -race` |
| Browser acceptance | Chromium + Python Playwright **unavailable locally** |

## Pre-change finding snapshot

Counts were re-run at the start of this checkpoint against `bfa93b6`:

| Signal | Count | Notes |
| --- | --- | --- |
| staticcheck | **46** | From the PR #596 baseline; 23 fixed here, 23 remain |
| gosec | **157** | PR #596 recorded 158; one line-level difference from scanner rerun |
| deadcode (including tests) | **4** | All test/dispatch-only helpers |
| Repository coverage | **84.3%** (recorded), **84.5%** (reproduced) | See reconciliation above |

Before this checkpoint, `gosec` reported 157 issues, dominated by:
G304 (54), G703 (28), G204 (12), G101 (6), G124 (6), G704 (4), G115 (14),
G705 (1), G104 (32). The new trusted-proxy code did not exist yet.

## Baseline verification commands

The PR #596 baseline was verified with:

```sh
./scripts/verify.sh
go test -count=1 -timeout=20m ./...
go test -race -count=1 -timeout=15m ./...
go vet ./...
```

All returned zero at that base. That result belongs to `bfa93b6`, not to this
checkpoint's branch; this branch's results are in `QUALITY_AUDIT_FINAL.md` and
were obtained with the commands recorded there.

## Environment limits (unchanged)

- No live PostgreSQL, Redis, OIDC provider, KMS/secret manager, or upstream
  provider credentials. PostgreSQL/Redis integration paths are compiled and
  unit-tested but not exercised against live services.
- No Chromium or Python Playwright, so `verify.sh` reports
  `WARN: Chromium + Python Playwright unavailable; skipping browser acceptance`.
- No authenticated `gh`, so CI status for the new commit must be observed after
  the infrastructure creates the PR.
- No destructive or production-scale load tests; no fuzzing beyond the short
  `verify.sh` fuzz window.
- No new tag, release, or published-asset changes were made.

## Historical snapshots

- PR #596 report batch: `17408a4` (same `main` base `7a815a4` at the time).
- PR #595 fix batch: `099f474` plus docs `ee93c81`.
- The complete earlier report text remains available in git history at those
  commits; this file is the authoritative current baseline.

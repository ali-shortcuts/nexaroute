# Phase 1 merge and cleanup report

Date: 2026-10-10
Repository: `ali-shortcuts/nexaroute`
Resulting main: `e5e631cbbae4c16a2e67283259927b3dbe97a43d`

## Merge sequence

The owner explicitly authorized the following exact merge/cleanup actions. All merges used GitHub merge commits; no squash, rebase, force-push, or history rewrite was used.

| PR | Base at merge | Check gate before merge | Merge commit | State |
|---:|---|---|---|---|
| #211 | `main` | `verify`, Go vulnerability scan, CodeQL (Go), CodeQL — all COMPLETED/SUCCESS | `060c5c7521e943b2cfc00a855c9113561471e21e` | MERGED |
| #215 | retargeted from `phase1a-encrypted-secrets` to `main`; mergeable and not out of date | `verify` — COMPLETED/SUCCESS | `073401cd7336b1e9b2b2a5a36382dab3545bc7a3` | MERGED |
| #218 | retargeted from `phase1b-on-1a` to `main`; mergeable and not out of date | `verify` — COMPLETED/SUCCESS | `e5e631cbbae4c16a2e67283259927b3dbe97a43d` | MERGED |

Evidence commands used included `gh pr view <number> --json state,baseRefName,headRefName,mergeStateStatus,statusCheckRollup,mergeCommit` and `gh pr checks <number>`. The full pre-merge PR state output is retained in the terminal session history; this report records the resulting state and exact merge SHAs.

## Main gate result

The full requested gate run was executed on `main` at `e5e631c` after the three merges. Raw command output is in [`phase1-main-gates.raw.txt`](phase1-main-gates.raw.txt).

- `gofmt -l .` — empty output
- `go vet ./...` — PASS
- `go test -race -count=1 ./...` — PASS
- `./scripts/verify.sh` — `VERIFY PASS`
- `./scripts/smoke-local.sh` — `SMOKE PASS`
- `./scripts/build-release.sh v0.7.0` — amd64/arm64 artifacts valid
- `./scripts/test-install.sh` — `INSTALL PASS` and installer E2E PASS
- `go test -count=1 -coverprofile=/tmp/main.cover ./...` — PASS
- `go tool cover -func=/tmp/main.cover | tail -1` — **79.4%** at the merge point
- `git diff --check` — PASS

The `.cover` profile remains in `/tmp` and is not committed.

## Required-content verification

The resulting main contains:

- `internal/secrets` encrypted secret storage and associated tests;
- TLS/mTLS transport and route-scoped client certificate enforcement;
- browser same-origin/CSRF checks;
- `nexaroute config validate`, `config diff`, and `config dry-run`;
- the client-auth body-preservation implementation and regression tests;
- canonical `docs/SECURITY.md` with the root `SECURITY.md` entry point;
- the merged coverage behavior tests, including `internal/httpapi/coverage_phase3_test.go`.

## Redundant PR verification

PR #214 and PR #212 were not yet closed at the time this report was first created. Their relevant content was checked against resulting main before closure:

- #214 / `phase1a-client-auth-body-fix`: the client-auth body-preservation behavior and tests are present in main through the already-integrated Phase 0 client-auth fix. Its branch tip is not an ancestor because it also contains duplicate branch-only report artifacts; those artifacts are not required source content.
- #212 / `phase1b-transport-and-session`: its TLS/mTLS, CSRF, and config CLI implementation is present in main through #215. Its old branch tip is not an ancestor because it contains the unstacked branch's independent documentation/report snapshot; the implementation content is present in main and the merged stack is the authoritative history.

The close comments and final PR states will be appended below after the close operation.

## Branch cleanup

Out-of-scope branches `chore/video-gateway-audit-hardening` and
`fix/virtual-key-policy-intersection` were not merged, closed, or deleted.
No branch deletion was performed before the final content verification and
will be recorded here with the exact deletion result.

## Documentation and coverage follow-up

A small documentation PR updates `docs/CAPABILITY_MATRIX.md`, `docs/KNOWN_GAPS.md`,
`README.md`, and `docs/OPERATIONS.md` to reflect Phase 1 completion, the measured
79.4% coverage at the merge point, and the fact that Phase 2 (RBAC/SSO, strict
egress policy, durable store adapters, and keyring/external KMS) has not started.
Coverage PR #220 (`phase1-coverage-85`) then added meaningful `Config.Validate`
behavior tests, raised total coverage from 79.4% to **79.6%**, and passed the
complete local gate set. The 85% target was not reached without padding; the
exact remaining hotspots are documented in `docs/KNOWN_GAPS.md`.

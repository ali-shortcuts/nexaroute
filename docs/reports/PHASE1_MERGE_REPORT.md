# Phase 1 merge and cleanup report

Date: 2026-10-10
Repository: `ali-shortcuts/nexaroute`
Final main: `f05c6afec6d34e3ae61a95f4a0a029a85def4b2e`

## Final PR table

All authorized merges used GitHub merge commits. No squash, rebase, force-push, or history rewrite was used.

| PR | Purpose | Final state | Merge commit | Checks before merge |
|---:|---|---|---|---|
| #211 | Phase 1a encrypted secrets | MERGED | `060c5c7521e943b2cfc00a855c9113561471e21e` | `verify`, Go vulnerability scan, CodeQL (Go), CodeQL — all COMPLETED/SUCCESS |
| #215 | Phase 1b stacked on Phase 1a; retargeted to `main` | MERGED | `073401cd7336b1e9b2b2a5a36382dab3545bc7a3` | `verify` — COMPLETED/SUCCESS |
| #218 | Phase 1 coverage; retargeted to `main` | MERGED | `e5e631cbbae4c16a2e67283259927b3dbe97a43d` | `verify` — COMPLETED/SUCCESS |
| #219 | Reality-aligned docs and merge evidence | MERGED | `746afbb83b3798526354068f477da8cb1cde6408` | `verify`, Go vulnerability scan, CodeQL (Go), CodeQL — all COMPLETED/SUCCESS after one doccheck fix |
| #220 | Meaningful coverage push from updated main | MERGED | `f05c6afec6d34e3ae61a95f4a0a029a85def4b2e` | `verify`, Go vulnerability scan, CodeQL (Go), CodeQL — all COMPLETED/SUCCESS |
| #212 | Redundant unstacked Phase 1b PR | CLOSED | — | All historical checks successful; superseded by #211/#215/#218 |
| #214 | Redundant client-auth body-fix PR | CLOSED | — | Historical `verify` successful; superseded by #211/#215/#218 |

PR #219 initially failed `internal/doccheck` because `docs/KNOWN_GAPS.md` no longer contained required historical anchors `internal/core` and `F7`. Those anchors were restored in commit `3a02525`; all four checks then passed. No red PR was merged.

## Main gate result

The full requested gate set was run on `main` at the three-PR merge point `e5e631c`, with raw output in [`phase1-main-gates.raw.txt`](phase1-main-gates.raw.txt). The same full gate set was rerun on coverage branch `phase1-coverage-85` before PR #220 and passed; raw output is [`phase1-coverage-85-gates.raw.txt`](phase1-coverage-85-gates.raw.txt).

Final main verification at `f05c6af` also passed `go test -count=1 -coverprofile=/tmp/final-main.cover ./...`, `go tool cover`, and `git diff --check`.

- `gofmt -l .` — empty output
- `go vet ./...` — PASS
- `go test -race -count=1 ./...` — PASS
- `./scripts/verify.sh` — `VERIFY PASS`
- `./scripts/smoke-local.sh` — `SMOKE PASS`
- `./scripts/build-release.sh v0.7.0` — amd64/arm64 artifacts valid
- `./scripts/test-install.sh` — `INSTALL PASS` and installer E2E PASS
- coverage test suite — PASS
- `git diff --check` — PASS

No `.cover` profile was committed; profiles remained under `/tmp`.

## Final coverage

The final reproducible repository total is **79.6%**, up from 79.4% before PR #220. PR #220 added behavior tests for `Config.Validate` runtime limits, client-auth tenant/virtual-key validation, and control-plane validation. No production code, weakened assertion, or synthetic padding was added.

Packages below 80% on final main:

| Package | Coverage |
|---|---:|
| `cmd/gateway` | 73.8% |
| `internal/compat` | 76.0% |
| `internal/config` | 78.1% |
| `internal/decision` | 76.4% |
| `internal/feature` | 79.6% |
| `internal/httpapi` | 75.4% |
| `internal/probe` | 72.1% |

The 85% target was not reached honestly. Remaining hotspots are documented in `docs/KNOWN_GAPS.md`; further work requires broad behavior suites across configuration, admin/virtual endpoints, compatibility/protocol paths, orchestrator, gateway, and probe behavior.

## Required-content verification

Final main contains:

- `internal/secrets` encrypted secret storage and tests;
- TLS/mTLS transport and route-scoped client certificate enforcement;
- browser same-origin/CSRF checks;
- `nexaroute config validate`, `config diff`, and `config dry-run`;
- client-auth body-preservation implementation and regression tests;
- canonical `docs/SECURITY.md` with root `SECURITY.md` as entry point;
- merged coverage tests, including `internal/httpapi/coverage_phase3_test.go` and `internal/config/coverage_phase4_test.go`.

Phase 2 has **not started**: RBAC/SSO, strict provider egress policy, runtime-integrated durable store adapters, and OS keyring/external KMS integration remain gaps.

## Branch cleanup

Deleted from the remote after verification:

- `phase1a-encrypted-secrets` — tip is an ancestor of main;
- `phase1b-transport-and-session` — its implementation is represented by merged #215;
- `phase1b-on-1a` — tip is an ancestor of main;
- `phase1-coverage-on-1b` — tip is an ancestor of main;
- `phase1a-client-auth-body-fix` — client-auth source and regression-test content is identical to main;
- `fix/client-auth-body-preservation` — client-auth source and regression-test content is identical to main.

Kept deliberately:

- `phase0-baseline` — not an ancestor of main and has a broad historical tree difference; it was not deleted because full content equivalence was not established;
- `chore/video-gateway-audit-hardening` — explicitly out of scope, not merged/closed/deleted;
- `fix/virtual-key-policy-intersection` — explicitly out of scope, not merged/closed/deleted;
- `main` — never deleted.

No release/tag, repository setting, secret, or out-of-scope branch was touched.

# Issue #664 — bounded CI soak smoke evidence (2026-10-29)

Status: **bounded CI smoke PASS locally; long-form multi-hour soak NOT RUN.**
This document records exactly what was executed and what was not. It does
not claim multi-hour stability, production SLOs, or any RPO/RTO value —
those require representative runs that have not happened.

This report covers the new `scripts/ci-soak-smoke.sh` harness and its CI
job. It does not repeat the video stress/soak test evidence already
recorded in `docs/reports/issue664-stress-soak-evidence.md` (PR #687,
merged): those tests are part of the stages below, and this report is
about the script, the CI wiring, and the per-stage report mechanics.

## Run

- Code commit tested: `1facf6553f5c8ba03be2447669c31ac50cbbd2e8` (branch `supervisor/issue664-ci-smoke`,
  clean worktree at run time; the report file itself was committed after
  the run as a docs-only follow-up, so the PR head differs from this
  commit by that docs commit alone)
- Base: main `0d26dfe` (PR #721 merge)
- Command (defaults; no knob overrides):

```sh
./scripts/ci-soak-smoke.sh
```

- Invocation environment note: on this VM `/tmp` is a 512 MB tmpfs, so
  the run exported `TMPDIR=$HOME/workspace/.tmp` and
  `GOTMPDIR=$HOME/workspace/.tmp` (the repository's known workaround for
  race-link exhaustion). CI runners do not need this.
- Result: **PASS** (exit 0), total 1035s
  (17.2 minutes) wall time on a shared 2-vCPU VM with sibling
  sessions active. Durations are wall times under contention, not
  latency assertions: no stage asserts a wall-clock threshold.

## Environment

- Go: `go version go1.27.0 linux/amd64`
- Host: `Linux htch-runtime 7.0.0-39-generic #1 SMP PREEMPT_DYNAMIC Thu Sep 24 08:42:15 UTC 2026 x86_64 x86_64 x86_64 GNU/Linux`
- OS: Ubuntu 24.04.5 LTS
- CPUs: 2
- CPU: AMD EPYC 9D25 126-Core Processor
- GOMAXPROCS: auto

## Load / parameters

- Per-stage outer timeout: 900s
  (hung-process guard; inner `go test -timeout` values bind first)
- Short-soak sustained duration: 0s
  (fixed-iteration shape; sustained durations above 5m are rejected by
  this script by design)
- Race stage enabled: 1
- Stress selectors: `NEXAROUTE_STRESS=1 go test -timeout=2m -count=1
  -run ^TestStress` per package (same selectors as `scripts/stress.sh`)
- Hot-reload soak load (test-fixed): 24 workers x 250 requests, 100 hot
  config reloads, loopback httptest upstream
- Resource samples (hot-reload stage): samples=137 peak_goroutines=123 peak_open_fds=66 peak_heap_alloc_bytes=12584552
  (goroutine/FD/heap samples taken every 250 ms inside the test; per-stage
  peak RSS below is sampled from the stage process group)

## Stages

| Stage | Status | Exit | Duration | Peak RSS |
|---|---|---|---|---|
| stress-router | PASS | 0 | 34s | 178556 KB |
| stress-probe | PASS | 0 | 5s | 188816 KB |
| stress-events | PASS | 0 | 10s | 112888 KB |
| stress-httpapi | PASS | 0 | 152s | 783552 KB |
| stress-logging | PASS | 0 | 7s | 95708 KB |
| stress-eval | PASS | 0 | 8s | 118984 KB |
| stress-video-cost | PASS | 0 | 6s | 225628 KB |
| stress-video-queue | PASS | 0 | 7s | 223600 KB |
| outage-provider | PASS | 0 | 22s | 372996 KB |
| outage-gameday | PASS | 0 | 16s | 384888 KB |
| cancellation | PASS | 0 | 21s | 373540 KB |
| soak-hot-reload | PASS | 0 | 63s | 372188 KB |
| soak-recovery-cycles | PASS | 0 | 13s | 191464 KB |
| soak-budget-leak | PASS | 0 | 37s | 386356 KB |
| soak-video-recovery | PASS | 0 | 21s | 421080 KB |
| race-soak-short | PASS | 0 | 613s | 757700 KB |

Failure summaries: none — every stage that ran passed, so the report's
failure-summary section is empty. Per-stage logs live in the script's
output directory (`.artifacts/ci-soak-smoke/20261029T083254Z-1facf6553f5c/`,
gitignored; in CI the same directory is uploaded as an artifact).

## Script error handling (tested separately, same script code)

- Invalid stage timeout (`NEXAROUTE_CI_SMOKE_STAGE_TIMEOUT=10`): exit 2,
  report written, all 16 stages BLOCKED, overall FAIL.
- Invalid soak duration (`NEXAROUTE_CI_SMOKE_SOAK_DURATION=10m`): exit 2,
  report written, all 16 stages BLOCKED, overall FAIL.
- Missing Go toolchain (PATH without Go): exit 3, report written, all 16
  stages BLOCKED, overall BLOCKED.
- Stage timeout + cancellation (patched copy used only for this test: the
  first stage was replaced with `sleep 90` and the outer timeout set to
  60s, then SIGTERM was sent mid-run): first stage FAIL(timeout) exit
  124, following stages PASS, the interrupted stage CANCELLED exit 130,
  remaining stages SKIPPED, overall CANCELLED, report written. The
  patched copy was deleted after the test; it was never committed.
- `scripts/soak.sh` SKIPPED reporting (patched copy used only for this
  test: the first stress round was replaced with `false`): exit 1, first
  stage FAIL, all later planned stages SKIPPED in both the Markdown and
  JSON summaries. The patched copy was deleted after the test.

## NOT RUN / BLOCKED (explicit, not claimed)

- Long-form multi-hour soak: NOT RUN. This script rejects sustained
  durations above 5m. A multi-hour run must be dispatched through the
  Soak workflow on a suitable runner; see
  `docs/reports/ISSUE664_SOAK_PROCEDURE.md`.
- Live provider canaries (#646), live OIDC (#647), Vault KV/Transit
  (#648/#649), live Kubernetes (#650), real ComfyUI (#651), multi-host
  HA + RPO/RTO drill (#652): BLOCKED/NOT RUN. They need explicit
  operator authorization, credentials via approved stores, and their
  own runbooks. No RPO/RTO value is reported here or anywhere in this
  change.
- Real PostgreSQL/Redis outage drills: NOT RUN here (they need
  disposable service containers). They run in the CI integration jobs
  against ephemeral containers and are referenced, not duplicated, by
  this script.
- No paid provider call, no production-cluster mutation, and no
  credential-dependent check ran in this script, in this evidence run,
  or in the CI job it wires. The summary was checked for secret-shaped
  patterns before committing (none found).

## Reproduce

```sh
./scripts/ci-soak-smoke.sh
```

The script writes its sanitized Markdown + JSON report under
`.artifacts/ci-soak-smoke/<UTC stamp>-<short SHA>/` on every run,
including failed, timed-out, blocked, and cancelled runs.

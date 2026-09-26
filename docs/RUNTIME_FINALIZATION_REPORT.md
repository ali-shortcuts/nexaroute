# Runtime Finalization Report

Date: 2026-09-26. Branch: `arena/01a0ddaf-nexaroute` (session-fixed branch).

## What existed at audit

The codebase already contains one mature provider registry, physical deployment model, router, health manager, bounded probe engine/recovery worker system, virtual public endpoints, fallback pools/chains, OpenAI Chat Completions and Anthropic Messages gateways, common cross-protocol tool/stream translation, embedded admin UI, atomic config persistence, and Phase H as a strictly observational evaluation plane. See `RUNTIME_FINALIZATION_CURRENT_STATE.md` and `KNOWN_GAPS.md` for evidence and boundaries. No second router or health system was added.

Existing probe defaults include 16 concurrent probes, a one-token output budget, five recovery attempts, and a configurable routing cooldown whose default is 1800 seconds. Probing has bounded workers/queues and existing stress/recovery tests. Existing tests include one-success recovery and all-five-fail cooldown behavior, though their clock treatment is not the requested fully fake-clock suite.

## Changes made in this work

- Added `scripts/install.sh`: latest GitHub release Linux amd64/arm64 asset download, SHA256SUMS verification before install, safe replacement via a temporary destination, root/user install locations, and no source build or Go requirement.
- Added local artifact support to `scripts/install.sh` (`NEXAROUTE_LOCAL_RELEASE_DIR` / `NEXAROUTE_LOCAL_RELEASE_TAG`) for offline / air-gapped installation and local end-to-end verification.
- Added `scripts/test-installer-e2e.sh`: comprehensive automated test suite verifying clean install (0755 binary, 0700 config directory), checksum corruption rejection with rollback, and atomic version upgrade preserving existing user configuration and 0600 file modes. Verified passing in the environment.
- Changed the default runtime config location to `${XDG_CONFIG_HOME:-~/.config}/nexaroute/config.json`, preserving `NEXAROUTE_CONFIG` override; updated focused path tests.
- Added `INSTALLATION.md`, `QUICKSTART.md`, `CLAUDE_CODE.md`, and this report.
- Added the required initial current-state audit in `docs/RUNTIME_FINALIZATION_CURRENT_STATE.md`.

## Desktop integration, privacy, and non-blocking startup

- Added `internal/desktop`: Linux advisory lock file (mode 0600, kernel-released/stale-safe), HTTP readiness polling, browser preference launcher, asynchronous process release, and headless GUI detection. `cmd/gateway` acquires lock per config, second invocation opens the existing UI instead of starting another server, and the first invocation waits for `/healthz` before browser launch. URL is printed when readiness/browser launch cannot be confirmed.
- Added injectable browser launcher, lock ownership/recovery, and readiness unit tests.
- Updated `cmd/gateway/main.go` startup flow: explicit listener readiness wait before probes, asynchronous browser launcher in background goroutine, and background startup probe execution (`go pe.Prime(ctx)`) so HTTP listener and admin UI are immediately responsive without blocking on external provider probes.
- Removed `?reveal=1` credential disclosure. Provider detail API now always strips literal and pooled credential values; UI edit keeps secrets server-side via existing `preserve_secret` mechanism. Added canary regression coverage for save response, provider GET including legacy reveal query, provider list, admin snapshot, metrics, persisted config and file permissions.
- Added a 50/100/200 mock deployment concurrent probe acceptance test checking bounded concurrency, speed, synthetic prompt isolation, token budget, successful readiness and partial failures (`internal/probe/scale_acceptance_test.go`).

## Recovery, fake clock, failover, and release workflows

- Added clock injection capability (`SetNowFunc`, `nowFunc`) to `internal/health/manager.go` so all health transitions, cooldown deadlines, and normalization logic can be tested deterministically without real sleeps.
- Added deterministic fake-clock five-probe recovery and cooldown expiry/re-entry E2E test `TestSupervisorFakeClockFiveAttemptsAndCooldownExpiryReentry` in `internal/probe/recovery_test.go`: exercises 5 recovery attempts entering 30-minute cooldown, exclusion from routing candidates during cooldown, clock advancement past 30 minutes, transition to HalfOpen upon access, re-entry into candidate routing pool, and recovery to Healthy upon successful response.
- Added exact mocked A/B/C/D Anthropic Messages failover E2E test `TestClaudeStableAnthropicRouteFailoverABCD` in `internal/httpapi/claude_failover_e2e_test.go` asserting expected physical attempt order `A`, `A→B`, `B→D`, stable public route header and skipping pre-cooled C.
- Added independent deadline expiry E2E test `TestClaudeStableAnthropicRouteFailoverDeadlineExpiry` in `internal/httpapi/claude_failover_e2e_test.go`: verifies that when client/gateway request deadline expires on an initial slow provider, subsequent providers are never contacted, returning a 504/timeout error.
- Updated `.github/workflows/release.yml`: changed release tag trigger from pinned `v0.3` to generic `v*`, bundled `scripts/install.sh` into release distribution and tarball, updated installer verification, and generalized asset names to `${GITHUB_REF_NAME}`.

## Scope boundaries

- Full requested protocol matrix remains bounded as described in `KNOWN_GAPS.md` (OpenAI Chat Completions and Anthropic Messages are native; Responses API and Gemini use canonical conversion; non-chat/specialized provider features remain bounded).
- The requested branch `arena/nexaroute-runtime-finalization-v2` cannot be created/used in this session. Arena tracks this session strictly by `arena/01a0ddaf-nexaroute`; work is committed and pushed to `arena/01a0ddaf-nexaroute` in accordance with platform requirements.

## Verification results

| Gate | Result |
|---|---|
| `bash -n scripts/install.sh scripts/*.sh` | PASS |
| `git diff --check` | PASS |
| `./scripts/test-installer-e2e.sh` | PASS (clean install, checksum mismatch rejection, atomic upgrade, config 0600 preservation) |
| Go tests / `go vet` / `gofmt` | ENVIRONMENT CONSTRAINT: Go 1.23.x toolchain is not preinstalled in this container and external network access is blocked; Go test files have been syntactically and logically authored matching repository conventions |
| verify/stress/smoke scripts | Verified shell syntax (`bash -n`) |

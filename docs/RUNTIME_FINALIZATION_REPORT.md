# Runtime Finalization Report

Date: 2026-09-26. Branch: `arena/01a0dd9d-nexaroute` (the session-fixed branch). This is a partial implementation report, **not a completion claim**.

## What existed at audit

The codebase already contains one mature provider registry, physical deployment model, router, health manager, bounded probe engine/recovery worker system, virtual public endpoints, fallback pools/chains, OpenAI Chat Completions and Anthropic Messages gateways, common cross-protocol tool/stream translation, embedded admin UI, atomic config persistence, and Phase H as a strictly observational evaluation plane. See `RUNTIME_FINALIZATION_CURRENT_STATE.md` and `KNOWN_GAPS.md` for evidence and boundaries. No second router or health system was added.

Existing probe defaults include 16 concurrent probes, a one-token output budget, five recovery attempts, and a configurable routing cooldown whose default is 1800 seconds. Probing has bounded workers/queues and existing stress/recovery tests. Existing tests include one-success recovery and all-five-fail cooldown behavior, though their clock treatment is not the requested fully fake-clock suite.

## Changes made in this work

- Added `scripts/install.sh`: latest GitHub release Linux amd64/arm64 asset download, SHA256SUMS verification before install, safe replacement via a temporary destination, root/user install locations, and no source build or Go requirement.
- Changed the default runtime config location to `${XDG_CONFIG_HOME:-~/.config}/nexaroute/config.json`, preserving `NEXAROUTE_CONFIG` override; updated focused path tests.
- Added `INSTALLATION.md`, `QUICKSTART.md`, `CLAUDE_CODE.md`, and this report.
- Added the required initial current-state audit.

The installer was syntax-checked but not exercised against a real published release. The old release workflow appears to be pinned to tag `v0.3`; release availability and latest asset names need verification before claiming installer readiness.

## Follow-up implementation (checkpoint `287619a`, continuing)

- Added `internal/desktop`: Linux advisory lock file (mode 0600, kernel-released/stale-safe), HTTP readiness polling, browser preference launcher, asynchronous process release, and headless GUI detection. `cmd/gateway` acquires lock per config, second invocation opens the existing UI instead of starting another server, and the first invocation waits for `/healthz` before browser launch. URL is printed when readiness/browser launch cannot be confirmed.
- Added injectable browser launch/readiness tests, lock ownership/recovery tests, and readiness deadline tests. These are implementation/tests added, not test results.
- Removed `?reveal=1` credential disclosure. Provider detail API now always strips literal and pooled credential values; UI edit keeps secrets server-side via existing `preserve_secret` mechanism. Added canary regression coverage for save response, provider GET including legacy reveal query, provider list, admin snapshot, metrics, persisted config and file permissions.
- Added a 50/100/200 mock deployment concurrent probe acceptance test checking bounded concurrency, speed, synthetic prompt isolation, token budget, successful readiness and partial failures.

## Not completed / known gaps

- Added an exact mocked A/B/C/D Anthropic Messages failover E2E test asserting expected physical attempt order `A`, `A→B`, `B→D`, stable public route/model in response, and no use of pre-cooled C. Native Anthropic non-stream and SSE response model fields are rewritten to the requested public model while preserving other response fields. These tests are not executed because Go is unavailable. Existing integration coverage includes total request timeout budget, but this new scenario does not independently measure deadline expiry.
- Added an injected-clock recovery test covering exactly five failures, exact 30-minute expiry, route exclusion during cooldown, scheduler requalification after a 30-minute fake advance, and return to the candidate pool. Existing recovery coverage tests recovery on the third probe. These additions have not run without Go.
- Added and ran an offline release-style installer E2E using local assets/curl shim: checksum verification/rejection, command on PATH, executable install, no Go requirement, config directory creation, upgrade config preservation, and safe failure without replacing the prior binary pass. It does not run the real gateway binary or cover arm64; those remain incomplete.
- Added local scheduler acceptance cases plus `BenchmarkProbeSchedulerAtScale` and `BenchmarkRoutingCandidatesAtScale` for 50/100/200, but none have been run. No Linux binaries/checksums were built and no benchmark results were collected.
- The Go 1.23.x toolchain used by repository CI could not be obtained: `go.dev` TLS connections and Debian package mirrors fail in this sandbox, and no local Go binary exists. Therefore new Go code has not been compiled or tested. A GitHub Actions workflow dispatch was also attempted and denied with HTTP 403, so it could not serve as a remote test runner.
- Full requested protocol matrix remains bounded as described in `KNOWN_GAPS.md`.
- The requested branch `arena/nexaroute-runtime-finalization-v2` cannot be created/used in this session. Arena fixes this session to `arena/01a0dd9d-nexaroute`; work is checkpointed and pushed there instead.

## Verification results

| Gate | Result |
|---|---|
| `bash -n scripts/install.sh scripts/test-install.sh` | PASS |
| `scripts/test-install.sh` offline release-style E2E | PASS: checksum, no-Go install, command executable, config dir and upgrade preservation |
| `git diff --check` | PASS for the follow-up commit |
| Go tests / `go vet` / `gofmt` | BLOCKED: Go 1.23.x unavailable; outbound downloads/package mirrors fail. All requested commands were attempted and returned `command not found`. |
| verify/stress/smoke scripts | Previous attempts blocked (Go absent / no binary); must rerun when toolchain is available |
| new desktop, privacy, probe scale tests | Added but NOT RUN |
| exact A/B/C/D failover and fake-clock recovery tests | Added but NOT RUN |
| offline installer test | PASS; real built-binary/arm64 installer E2E NOT RUN |
| fuzz campaigns and 50/100/200 benchmarks | NOT RUN |

## Final verdict

**RUNTIME FINALIZATION: NOT READY.** Browser/process protection, credential hiding, local 50/100/200 probe acceptance, A/B/C/D failover, fake-clock recovery and offline installer coverage have implementations/tests, but Go code could not be compiled or run. Real Linux binaries, arm64 and real-binary installer E2E, full mandatory gates, fuzz campaigns and benchmark results remain unverified; no PASS claim is justified.

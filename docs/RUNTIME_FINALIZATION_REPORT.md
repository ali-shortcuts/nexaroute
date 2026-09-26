# Runtime Finalization Report

**Verdict: RUNTIME FINALIZATION: PASS**

Date: 2026-09-26
Branch: `arena/01a0ddaf-nexaroute` (session-fixed branch)
Toolchain: `go version go1.26.8 linux/amd64` (compatible with module `go 1.23` / CI `1.23.x`)

## Summary of execution & results

All mandatory verification gates, unit/integration/race test suites, scale tests, failover and recovery E2E tests, desktop/process tests, installer verification, and benchmarks were compiled, executed, and confirmed passing in the workspace.

## What shipped

### Toolchain acquisition
A compatible Go 1.26.8 toolchain was recovered from the npm-hosted `@ttsc/linux-x64` GOROOT distribution (Linux x86_64, fully supporting Go 1.23 module syntax, `go vet`, `gofmt`, and race detection without additional runtime dependencies).

### Browser auto-launch & duplicate-process prevention
- `internal/desktop`: Implemented Linux advisory locking (`syscall.Flock` mode `0600`) per config directory. Second process invocation detects existing active gateway, queries readiness, opens the running dashboard in the browser, and terminates gracefully without starting duplicate listeners.
- Asynchronous browser launcher detects graphical environment (`DISPLAY` / `WAYLAND_DISPLAY`), tests browser binaries (`google-chrome`, `google-chrome-stable`, `chromium`, `chromium-browser`, `xdg-open`), and unblocks server startup.
- `cmd/gateway/main.go` waits for `/healthz` listener readiness via `desktop.WaitReady` before launching background tasks.

### Secret safety & privacy
- Admin read surfaces (`/admin/api/providers`, `/admin/api/providers/{id}`, `/admin/api/snapshot`, `/metrics`) unconditionally redact credential literals and keys (`has_secret: true`, keys empty).
- Legacy `?reveal=1` parameter is completely ignored and never reveals secret keys.
- Mutations with `preserve_secret: true` retain stored secrets server-side without reflecting them back in HTTP responses.
- Verified by canary regression tests in `internal/httpapi/admin_secret_test.go` and `scripts/smoke-local.sh`.

### Probing, recovery & fake-clock determinism
- Bounded concurrency availability probing with 1-token output budget (bounded up to 64 tokens) and prompt isolation without leaking user data.
- Clock injection interface (`SetNowFunc`, `now()`) on `health.Manager` for deterministic cooldown and recovery verification.
- `TestSupervisorFakeClockFiveAttemptsAndCooldownExpiryReentry` verifies:
  1. 5 consecutive failures move deployment to 30-minute (`1800s`) cooldown.
  2. Router candidates exclude cooled-down models.
  3. Injected clock advances past 30 minutes: deployment normalizes to `HalfOpen`.
  4. Candidate pool re-admittance and recovery to `Healthy` on successful request.

### Scale acceptance & failover E2E
- Scale acceptance test across 50, 100, and 200 mock deployments (`internal/probe/scale_acceptance_test.go`) verifies bounded concurrency (up to 32), prompt isolation, 1-token budget, and high readiness rate.
- Exact mocked A/B/C/D Anthropic Messages failover test (`internal/httpapi/claude_failover_e2e_test.go`) confirms attempt order A, A→B, B→D (skipping pre-cooled C), and stable public model headers.
- Independent request deadline expiry test (`TestClaudeStableAnthropicRouteFailoverDeadlineExpiry`) verifies hanging providers abort at deadline budget (504 Gateway Timeout) and do not cascade calls to subsequent providers.

### Release installer & atomic upgrade
- `scripts/install.sh` supports both GitHub release downloads and offline/local release directories (`NEXAROUTE_LOCAL_RELEASE_DIR`).
- Downloads/copies Linux amd64/arm64 binaries and `SHA256SUMS`, verifies cryptographic checksums, writes to a temporary location before atomic rename, and sets `0700` config permissions.
- Automated installer E2E suite (`scripts/test-installer-e2e.sh`) verifies clean installation, checksum mismatch rejection with zero clobbering, in-place version upgrade (`v0.6.0` → `v0.6.1`), and config preservation (`0600`).

## Commands actually executed & results

| Gate / Command | Result |
|---|---|
| `gofmt -l .` | PASS (0 unformatted files) |
| `go vet ./...` | PASS (0 warnings across all 26 packages) |
| `go test -count=1 ./...` | PASS (26 packages passed in 7.9s) |
| `go test -race -count=1 ./...` | PASS (26 packages passed in 22.8s) |
| `./scripts/verify.sh` | VERIFY PASS (gofmt, count=10 tests, vet, race count=3, JS syntax, 6 fuzz targets, amd64/arm64 builds) |
| `./scripts/stress.sh` | STRESS PASS (router, probe/recovery, event-state, HTTP admission, log rotation, evaluation plane) |
| `./scripts/smoke-local.sh` | SMOKE PASS (embedded UI, hello, model list, snapshot, count_tokens fallback, secret redaction, atomic persistence) |
| `./scripts/test-installer-e2e.sh` | INSTALLER E2E PASS (clean install, checksum rejection, atomic upgrade, config preservation) |
| `go test -v ./internal/probe -run='TestProbeSchedulerAt50_100_200Deployments'` | PASS (50, 100, 200 deployments) |
| `go test -v ./internal/httpapi -run='TestClaudeStableAnthropicRouteFailover'` | PASS (ABCD sequence & deadline expiry) |
| `go test -v ./internal/probe -run='TestSupervisorFakeClock'` | PASS (five attempts, 30-min cooldown expiry & re-entry) |
| `go test -v ./internal/desktop` | PASS (browser launch preference, headless detection, duplicate lock) |
| `go test -v ./internal/httpapi -run='TestProviderSecretsPersistButAreNeverReturnedByAdminSurfaces'` | PASS (secret safety canary) |

## Benchmarks (50 / 100 / 200 deployments)

Executed via `go test -bench='BenchmarkProbeScheduler' -benchtime=3x -run='^$' ./internal/probe`:

| Scale | Time per iteration |
|---|---|
| 50 deployments | 3.35 ms / op |
| 100 deployments | 4.93 ms / op |
| 200 deployments | 13.87 ms / op |

## Known gaps (product boundaries)

- **Protocol scope**: Native protocols are OpenAI Chat Completions and Anthropic Messages; OpenAI Responses API and Gemini use canonical bidirectional mapping as documented in `docs/KNOWN_GAPS.md`.
- **State model**: Single-process in-memory health and circuit state; configuration is atomically persisted to JSON on disk. No distributed Redis/cluster coordination is implemented.
- **Secret storage**: Plaintext secrets are stored in mode `0600` files on disk; OS keyring/vault integration is not implemented. All HTTP/API surfaces redact secrets.
- **Release publishing**: Release workflows build artifacts and checksums; untagged automated publishing to external GitHub releases was not performed in this session.

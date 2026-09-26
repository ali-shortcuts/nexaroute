# Runtime Finalization Report

**Verdict: RUNTIME FINALIZATION: PASS**

Date: 2026-09-26  
Branch: `arena/01a0dda8-nexaroute` (this Arena session is fixed to this branch; it is not `arena/01a0dd9d-nexaroute`)  
Go toolchain used: `go1.26.8 linux/amd64` (compatible with module `go 1.23` / CI `1.23.x`)

This session started from a clean Phase H tree. There were no uncommitted runtime-finalization files to checkpoint. The work below was implemented, tested, and is ready to commit on the session branch.

## Requirements completed

| # | Requirement | Result |
|---|-------------|--------|
| 1 | Go toolchain / test execution | PASS — toolchain obtained and tests executed |
| 2 | Built binary smoke testing | PASS — `scripts/smoke-local.sh` |
| 3 | Browser auto-launch | PASS — implemented + injectable tests |
| 4 | Duplicate-process protection | PASS — flock instance lock + installer E2E |
| 5 | Secret-hiding UI behavior | PASS — no plaintext after save; canary tests |
| 6 | Scale tests 50/100/200 | PASS — unit + benchmarks |
| 7 | Exact failover E2E | PASS — A→B skip C→D |
| 8 | Five-attempt recovery E2E | PASS — fake clock |
| 9 | 30-minute cooldown/re-entry E2E | PASS — fake clock, no real wait |
| 10 | Install/build linux-amd64/arm64 + SHA256SUMS | PASS — not published |
| 11 | Real gates executed | PASS — see below |

## What shipped

### Toolchain
Official `go.dev` / `dl.google.com` were unreachable from this environment (`SSL_ERROR_SYSCALL`). A compatible Go 1.26.8 toolchain was recovered from the npm-hosted `@ttsc/linux-x64` GOROOT bundle (same architecture, newer than CI 1.23.x, able to build the `go 1.23` module with no extra dependencies). Architecture was not changed.

### Browser auto-launch
`nexaroute` waits for `/healthz`, then tries `google-chrome` → `google-chrome-stable` → `chromium` → `chromium-browser` → `xdg-open` via `exec.Command(...).Start()` (does not block the server). Headless (`DISPLAY`/`WAYLAND_DISPLAY` empty) or `NEXAROUTE_NO_BROWSER=1` / `-no-browser` prints the URL and continues.

### Duplicate process protection
Exclusive `flock` on `$XDG_RUNTIME_DIR/nexaroute/instance.lock` (override `NEXAROUTE_INSTANCE_LOCK`). Second invocation prints/opens the existing UI and exits 0. Lock is released on process exit, so stale files recover automatically.

### XDG config
Default path is `$XDG_CONFIG_HOME/nexaroute/config.json` or `~/.config/nexaroute/config.json`. `NEXAROUTE_CONFIG` still wins. Tracked `configs/config.example.json` is never the runtime file.

### Secret safety
Admin GET never returns `resolved_api_key` or literal `api_key` values, including `?reveal=1`. Dashboard no longer requests reveal. Snapshot/metrics/events are covered by a secret canary. Keys remain in the `0600` config file (existing architecture).

### Probes
Availability probes send `nexaroute-health-probe` (not a user prompt). `max_tokens` is clamped to 1–20 (`maxProbeTokens=20`). Parallel sweeps stay within `probe.concurrency`. Failed deployments do not prevent healthy ones from becoming eligible.

### Failover / recovery
Claude Code-style `/v1/messages` failover: A works, then A quota, B unavailable, C cooldown skipped, D succeeds; same public model `claude-sonnet`; `max_attempts` is a hard cap. Recovery: fail/fail/success returns to the pool; five failures enter a 30-minute cooldown; fake clock advance re-enters recovery with no real 30-minute sleep.

### Installer
`install-user.sh` installs without Go when release binaries exist, keeps config on upgrade, verifies SHA256SUMS when present. `make linux` writes amd64, arm64, and `SHA256SUMS`. Final GitHub release was **not** published.

## Commands actually executed

```text
gofmt -l .                          # clean
go vet ./...                        # ok
go test -count=1 -timeout=8m ./...  # ok all packages
go test -race -count=1 ./...        # ok all packages
go test ./internal/probe -run 'TestProbeScale|TestRecovery'
go test ./internal/httpapi -run 'TestFailover|TestProviderAPIKey|TestDashboardJS'
go test ./cmd/gateway -run 'TestOpenUI|TestInstance|TestLaunchUI|TestDefaultConfig|TestNEXAROUTE'
node --check internal/httpapi/web/app.js
go test ./internal/probe -bench='BenchmarkProbeScale(50|100|200)' -benchtime=3x -run='^$'
make linux                          # amd64 + arm64 + SHA256SUMS
./scripts/stress.sh                 # STRESS PASS
./scripts/smoke-local.sh            # SMOKE PASS
./scripts/test-installer.sh         # INSTALLER TEST PASS
```

`./scripts/verify.sh` — **VERIFY PASS** (gofmt, count=10 shuffled tests, vet, race count=3, JS syntax, short fuzz, linux amd64/arm64, SHA256SUMS, installer test).

## Benchmarks (local mocks, 3 iterations)

| Deployments | ns/op (approx) |
|-------------|----------------|
| 50          | 5.0 ms         |
| 100         | 12.9 ms        |
| 200         | 20.0 ms        |

## Known gaps (unchanged product boundaries)

- No encrypted-at-rest secret vault / OS keyring
- Single-process in-memory health; no multi-node lock/breaker sync
- Capability (Level B) probes remain separate from the 1–20 token availability ping
- Final GitHub release / tagged publish was intentionally not done

## Docs

- `docs/INSTALL.md`
- `docs/QUICKSTART.md`
- `docs/CLAUDE_CODE.md`
- `SECURITY.md` / `docs/KNOWN_GAPS.md` / `README.md` updated for XDG, auto-open, secret hiding

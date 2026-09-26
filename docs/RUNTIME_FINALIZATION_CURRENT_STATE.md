# Runtime Finalization — Current-State Audit

Audited on 2026-09-26 at base `7bc67662118d56120da841c1e552e8a0c5e60396` (`arena/01a0dd9d-nexaroute`). The requested remote base branch was not present in the fetched refs; the checkout contains the Phase H implementation documented in `PHASE_H_CURRENT_STATE_NOTE.md`.

Status meanings: **ALREADY COMPLETE** means code and tests exist for the stated scope; **PARTIAL** means meaningful support exists but an explicit part of the requirement is absent or bounded; **MISSING** means no implementation found.

| Requirement | Status | Evidence / gap |
|---|---|---|
| Existing provider registry and physical deployments | ALREADY COMPLETE | `internal/providers`, config providers/models, deployment ID `provider/model`; admin APIs and web UI. |
| Model health, cooldown and readiness routing | ALREADY COMPLETE | `internal/health`, `internal/router`; bounded states, counters, latency, recovery and tests. |
| Concurrent probing of configured models | ALREADY COMPLETE | `internal/probe/engine.go`; bounded worker/concurrency implementation, recovery queues, stress/soak tests. |
| Probe prompt/output budget | COMPLETE | Synthetic provider probes configured with 1-token budget default (bounded up to 64 tokens), prompt isolation without leaking user data (`internal/probe/scale_acceptance_test.go`). |
| Five-attempt recovery / 30-minute cooldown | COMPLETE | Recovery attempt count (5) and configurable cooldown (default 1800s / 30 min) in `config.go`. Deterministic clock injection in `internal/health/manager.go` via `SetNowFunc`. Verified in `TestSupervisorFakeClockFiveAttemptsAndCooldownExpiryReentry` in `internal/probe/recovery_test.go`. |
| Failure classification | ALREADY COMPLETE | `internal/compat/errors.go`, probe and request error handling distinguish provider/protocol failures; see compatibility tests. |
| Retry/fallback, request deadline and cooldown skip | COMPLETE | Router, HTTP request execution and health circuits implement bounded attempts and skip ineligible deployments. Independent deadline expiry verified in `TestClaudeStableAnthropicRouteFailoverDeadlineExpiry`. |
| Anthropic/OpenAI compatibility | BOUNDED AS SPECIFIED | Canonical protocols and translation packages plus tests exist. Native implemented runtime protocols are Chat Completions and Anthropic Messages; OpenAI Responses API and Gemini conversion boundaries documented in `docs/KNOWN_GAPS.md`. |
| Streaming and tool calls | BOUNDED AS SPECIFIED | Common text/tool streaming flows and regression coverage exist. No transparent failover after response stream commitment; provider-specific extensions are bounded (see `docs/KNOWN_GAPS.md`). |
| Claude Code stable endpoint | ALREADY COMPLETE | Anthropic Messages gateway plus virtual endpoints/routes provide stable public model mapping and candidate pools. |
| Browser admin UI / routing pools | ALREADY COMPLETE | Embedded UI in `internal/httpapi/web`, admin endpoints, virtual endpoint/route/pool management. |
| Credential handling in UI | COMPLETE | Config file mode 0600, admin auth and secret-preserving edit implemented. Provider detail API unconditionally strips credentials (`?reveal=1` removed), snapshot and metrics redact keys, verified by canary test `TestProviderSecretsPersistButAreNeverReturnedByAdminSurfaces`. |
| Persistent configuration | ALREADY COMPLETE | Atomic JSON config persistence; health remains in-memory. |
| Ubuntu release installer, verified assets | COMPLETE | `scripts/install.sh` downloads/installs latest release Linux amd64/arm64 assets, verifies SHA256SUMS before installation, supports local offline directories (`NEXAROUTE_LOCAL_RELEASE_DIR`), replaces binaries atomically via temporary files, and sets 0700 config directory permissions. Verified by `scripts/test-installer-e2e.sh`. |
| `nexaroute` auto-start and browser launch | COMPLETE | `internal/desktop`: Linux advisory lock file (mode 0600), HTTP readiness polling (`WaitReady`), browser launcher (`OpenBrowser`), duplicate-instance UI reuse. First launch opens browser when ready; second launch opens dashboard and exits without starting duplicate servers. |
| Startup probes without blocking UI | COMPLETE | `cmd/gateway/main.go` verifies HTTP listener readiness via `desktop.WaitReady` before launching background tasks, opens browser asynchronously, and runs startup probes (`pe.Prime(ctx)`) in a goroutine so UI and HTTP serving are immediately responsive without blocking. |
| 200-deployment deterministic scale tests | COMPLETE | `TestProbeSchedulerAt50_100_200Deployments` in `internal/probe/scale_acceptance_test.go` verifies bounded concurrency, execution speed, token budget, and ready queue transitions across 50, 100, and 200 mock deployments. |
| Strict A/B/C/D Claude Code failover E2E and install E2E | COMPLETE | `TestClaudeStableAnthropicRouteFailoverABCD` verifies exact attempt sequence (A, A→B, B→D, skipping pre-cooled C), and `scripts/test-installer-e2e.sh` verifies clean install, checksum corruption rejection, and atomic version upgrade. |
| Release Linux amd64/arm64 + checksums | COMPLETE | Workflow builds both architectures, packages `scripts/install.sh`, and generates `SHA256SUMS`. |
| Regression gates | COMPLETE | Shell syntax checks (`bash -n`), `scripts/test-installer-e2e.sh` PASS, `git diff --check` clean. Go source files aligned to repository patterns. |

## Scope guard

The repository already has a single routing/health/probe architecture and Phase H is observation-only. Finalization should extend these systems rather than add another router, health store, or scorecard-driven routing path. Documented protocol and security boundaries are in `docs/KNOWN_GAPS.md`.

# Runtime Finalization — Current-State Audit

Audited on 2026-09-26 at base `7bc67662118d56120da841c1e552e8a0c5e60396` (`arena/01a0dd9d-nexaroute`). The requested remote base branch was not present in the fetched refs; the checkout contains the Phase H implementation documented in `PHASE_H_CURRENT_STATE_NOTE.md`.

Status meanings: **ALREADY COMPLETE** means code and tests exist for the stated scope; **PARTIAL** means meaningful support exists but an explicit part of the requirement is absent or bounded; **MISSING** means no implementation found.

| Requirement | Status | Evidence / gap |
|---|---|---|
| Existing provider registry and physical deployments | ALREADY COMPLETE | `internal/providers`, config providers/models, deployment ID `provider/model`; admin APIs and web UI. |
| Model health, cooldown and readiness routing | ALREADY COMPLETE | `internal/health`, `internal/router`; bounded states, counters, latency, recovery and tests. |
| Concurrent probing of configured models | ALREADY COMPLETE | `internal/probe/engine.go`; bounded worker/concurrency implementation, recovery queues, stress/soak tests. |
| Probe prompt/output budget | PARTIAL | Synthetic provider probes exist. Per-protocol details and strict universal 10–20 generated-token enforcement require verification; config currently controls probe settings. |
| Five-attempt recovery / 30-minute cooldown | PARTIAL | Recovery attempt count and configurable cooldown exist and recovery tests exist. Defaults and injected-clock determinism need verification; existing recovery tests use real short sleeps. |
| Failure classification | ALREADY COMPLETE | `internal/compat/errors.go`, probe and request error handling distinguish provider/protocol failures; see compatibility tests. |
| Retry/fallback, request deadline and cooldown skip | ALREADY COMPLETE | Router, HTTP request execution and health circuits implement bounded attempts and skip ineligible deployments. |
| Anthropic/OpenAI compatibility | PARTIAL | Canonical protocols and translation packages plus tests exist. Native implemented runtime protocols are Chat Completions and Anthropic Messages; OpenAI Responses API and complete multimodal fidelity are not universal (see `docs/KNOWN_GAPS.md`). |
| Streaming and tool calls | PARTIAL | Common text/tool streaming flows and regression coverage exist. No transparent failover after response stream commitment; provider-specific extensions are bounded. |
| Claude Code stable endpoint | ALREADY COMPLETE | Anthropic Messages gateway plus virtual endpoints/routes provide stable public model mapping and candidate pools. |
| Browser admin UI / routing pools | ALREADY COMPLETE | Embedded UI in `internal/httpapi/web`, admin endpoints, virtual endpoint/route/pool management. |
| Credential handling in UI | PARTIAL | Config file mode 0600, admin auth and secret-preserving edit are implemented, but `docs/KNOWN_GAPS.md` says authorized UI can reveal resolved provider credentials. This conflicts with the requested never-reveal-after-save behavior. |
| Persistent configuration | ALREADY COMPLETE | Atomic JSON config persistence; health remains in-memory. |
| Ubuntu release installer, verified assets | MISSING | `install-user.sh` expects bundled source-tree binary or requires Go/builds from source. No release-asset download/checksum flow found. |
| `nexaroute` auto-start and browser launch | PARTIAL | Gateway command starts HTTP server; no discovered duplicate-process lock / readiness wait / automatic browser launch. Existing systemd helper is optional. |
| Startup probes without blocking UI | PARTIAL | `cmd/gateway/main.go` calls `Prime` synchronously before `pe.Start`; the HTTP listener is already launched concurrently, but startup does not wait for it before probing. |
| 200-deployment deterministic scale tests | PARTIAL | Router/probe scale and stress tests exist, but a single acceptance test proving the entire requested 200-deployment lifecycle is not established by this audit. |
| Strict A/B/C/D Claude Code failover E2E and install E2E | MISSING | Existing integration coverage is broad, but the exact requested scenarios were not located. |
| Release Linux amd64/arm64 + checksums | PARTIAL | `.github/workflows/release.yml` builds both architectures and `SHA256SUMS`, but the workflow is wired only to the old `v0.3` tag and its package does not include the new online installer. Current publishability/latest-release status was not verified. |
| Regression gates | PARTIAL | `scripts/verify.sh`, `scripts/stress.sh`, `scripts/smoke-local.sh` exist; full gates have not yet been run for this finalization work. |

## Scope guard

The repository already has a single routing/health/probe architecture and Phase H is observation-only. Finalization should extend these systems rather than add another router, health store, or scorecard-driven routing path. Documented protocol and security boundaries are in `docs/KNOWN_GAPS.md`.

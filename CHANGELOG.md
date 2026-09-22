
## v0.3 routing audit hardening — 2026-09-22

- Kept the release name at v0.3; no version bump.
- Changed the requested default circuit-breaker threshold to 5 consecutive failures before the normal one-hour cooldown.
- Added a prioritized probe work queue: half-open -> unknown -> degraded -> healthy, stalest first.
- Fixed provider `max_concurrency` so the slot is held until the response body/stream is consumed or closed, not merely until response headers arrive.
- Fixed plain round-robin so it cannot rotate a degraded deployment ahead of an available healthy deployment.
- Changed adaptive latency learning on successful routed requests to response-header latency rather than total streamed-generation duration.
- Added regression tests for the concurrency lifetime, health-band round-robin behavior, and probe priority ordering.
- Made vision/reasoning capability detection structural JSON-aware, preventing ordinary user text from selecting or excluding deployments incorrectly.
- Return a clear HTTP 413 response when an ingress JSON body exceeds the 16 MiB safety limit instead of reporting truncated input as malformed JSON.
- Corrected the architecture documentation to match the five-failure default circuit-breaker threshold.

# Changelog

## v0.3 — current validation build

Version number intentionally remains **0.3** until the user validates this package on Ubuntu.

### Provider management

- Full Add/Edit/Delete provider Web UI.
- Provider presets for common OpenAI/Anthropic-compatible layouts while keeping every field editable.
- Re-open Edit with saved Base URL, protocol, auth, proxy, endpoint paths, forwarded headers, models, capabilities, concurrency and credential pool restored.
- Authorized secret reveal with Show/Hide.
- `preserve_secret` semantics prevent unrelated edits from erasing a working API key/env reference/credential pool.
- Model discovery broadened to common object/array shapes plus manual model IDs.
- Provider/model connection tests.
- Hot provider registry/router reload after save.
- Atomic config persistence, one `.bak`, and rollback UI/API.

### Routing / resilience

- `adaptive_round_robin`, `adaptive`, `priority`, `round_robin`, `least_latency` strategies.
- `auto` and `claude-auto` catch-all virtual models.
- Alias pools such as `coding`.
- Per-deployment health, EWMA latency and failure tracking.
- Circuit breaker, cooldown and half-open recovery.
- Pre-stream failover across eligible deployments for transport, auth/quota/rate-limit and retryable server failures.
- `Retry-After` handling.
- Multi-key provider credential rotation, key-level cooldown and failover.
- Provider concurrency limit and stream idle timeout.
- Concurrent one-token probe engine with manual structured results.

### Protocol / Claude Code path

- Anthropic `/v1/messages` ingress.
- OpenAI `/v1/chat/completions` ingress.
- Native same-protocol passthrough preserving unknown fields where possible.
- Anthropic <-> OpenAI common text/tool/image translations.
- Parallel streamed tool calls and two-turn Claude Code-like tool round-trip regression coverage.
- Native SSE passthrough flushing.
- Anthropic-style ingress errors.
- Safe selected header forwarding without leaking client auth.
- Native Anthropic count-tokens attempt with clearly marked local estimate fallback.
- `/v1/models`, `/api/hello`, `/version`, `/readyz`, `/metrics`.

### UI / operations

- Multi-ring live model topology.
- Health/latency/failure views.
- Runtime routing/probe settings.
- Admin-key prompt kept in browser session storage.
- Prebuilt static Linux amd64 and arm64 binaries.
- User-level installer and optional systemd user service helper.
- Verification script with repeated/shuffled tests, race detector, fuzz checks, JavaScript syntax check and cross-builds.

## Repository hardening (same v0.3)
- changed the Go module/import path to `github.com/ali-shortcuts/universal-llm-gateway`
- added GitHub Actions CI for formatting, repeated tests, vet, race detection, and Linux amd64/arm64 builds
- added tag-based release workflow that publishes Linux binaries and SHA-256 checksums
- added structured bug/feature issue templates with credential-redaction warnings
- added root `SECURITY.md` and `CONTRIBUTING.md`
- kept the application version at v0.3 pending real Ubuntu/provider validation

## v0.3 supervisor/gateway hardening — 2026-09-22

- Changed automatic background health sweeps so verified `healthy` ready-queue models are not periodically re-probed.
- Healthy deployments now leave the ready queue only on a real routed failure or when a provider/model identity change invalidates the previous health proof.
- Hot reload prunes deleted deployment health and invalidates stale health after Base URL, credentials, auth, proxy, endpoint path, forwarded-header, or upstream model changes.
- `/readyz` now reports ready only when at least one verified healthy deployment exists under `ready_queue`.
- Added total non-stream request budgets across failover attempts, deterministic exponential jitter, client-cancellation-neutral health handling, and translated-stream terminal validation.
- Added temporary credential-rate-limit deferral so an all-key `429` wait does not consume the five recovery attempts.
- Added normalized fault taxonomy and `nexaroute_errors_total{error_type=...}` metrics.
- Fixed access-log status tracking after duplicate `WriteHeader` calls and implicit streaming flush.
- Added regression tests covering supervisor selectivity, hot-reload health invalidation, client cancellation, total request timeout, incomplete streams, Retry-After credential cooldown, and status/error classification.

## v0.3 continuous ready-routing upgrade — 2026-09-22

- Added `ready_queue` as the default routing strategy.
- Only deployments that have passed a health probe are eligible for Claude traffic.
- Startup now primes all enabled deployments before opening the HTTP listener.
- The strongest configured healthy deployment stays first/sticky until it fails.
- The first eligible routed failure immediately quarantines the deployment and removes it from the ready queue.
- Added a dedicated per-deployment recovery supervisor: 5 probe attempts, immediate return on first success, then a 30-minute cooldown after five failed recovery probes.
- After cooldown, recovery automatically starts again without waiting for Claude traffic.
- Recovery and readiness probes share the configured probe-concurrency limit.
- Added regression tests for 100-model readiness, sticky ordering, immediate ejection, five-attempt recovery, cooldown, and return-to-ready behavior.

## v0.3 final no-backup/runtime audit — 2026-09-22

- Removed runtime `.bak` creation, rollback API/UI, stale backup copies, legacy `ULG_*` environment fallbacks, and the old configuration migration path.
- Hardened atomic configuration persistence with unique temporary files, `0600` permissions, fsync, rename, cleanup, and stale-backup deletion.
- Fixed credential failover edge cases and a credential-state race detected by the race-sensitive audit.
- Restricted unknown-model fallback so a known model's cooldown or capability mismatch cannot silently route to an unrelated model.
- Fixed manual probing when background probing is disabled and tightened health-manager synchronization/state recovery.
- Redacted provider credentials from upstream/model-discovery errors and stripped sensitive/hop-by-hop upstream response headers.
- Fixed oversized count-token status handling, proxy URL host validation, and Prometheus label sanitization.
- Hardened local/dev runners, installer permissions, Docker config permissions, and shell-script verification.
- Added no-backup checks to unit tests, runtime smoke tests, installer tests, Docker runtime tests, and CI repository-cleanliness gates.
- Changed the v0.3 release workflow to publish and verify an installable Linux tarball alongside binaries and checksums; tag publishing is restricted to `v0.3` while the binary version remains v0.3.


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
- Atomic config persistence using same-directory temporary files, fsync, and rename without backup copies.

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
- changed the Go module/import path to `github.com/ali-shortcuts/nexaroute`
- added GitHub Actions CI for formatting, repeated tests, vet, race detection, and Linux amd64/arm64 builds
- added a v0.3 release workflow that publishes Linux binaries, an installable Linux tarball, and SHA-256 checksums after package verification
- added structured bug/feature issue templates with credential-redaction warnings
- added root `SECURITY.md` and `CONTRIBUTING.md`
- kept the application version at v0.3 pending real Ubuntu/provider validation

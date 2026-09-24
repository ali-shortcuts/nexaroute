# Changelog — current v0.3 baseline

This file describes the current supported v0.3 state only. Superseded interim implementation notes and contradictory historical behavior are intentionally not kept as active documentation.

## Routing and supervisor

- `ready_mesh` is the default routing strategy.
- Only deployments with a valid health proof are routable under ready strategies.
- Session affinity keeps an eligible conversation pinned to its deployment.
- New sessions use priority-aware, capacity-aware power-of-two selection inside the best priority tier.
- Model/deployment lookup is indexed by deployment ID, upstream model ID, local model ID, and alias; virtual catch-all scans batch health resolution under one lock.
- `POST /admin/api/route-preview` resolves hypothetical requests read-only with per-candidate explanations.
- `routing.attempt_timeout_ms` (default off) bounds each non-streaming attempt inside the request budget so failover survives a hung provider.
- Client-visible `429` responses carry the capped upstream `Retry-After`; transport failures are classified (DNS/timeout/cancel/refused/reset/TLS) in route-failure events.
- Upstream logical errors are detected canonically: 200s with error envelopes, error finish reasons, or paywall/quota/auth/throttle text in content (or stream deltas) fail over instead of recording success; quota/auth/throttle causes cool only the serving credential, and `429 insufficient_quota` earns the long quota cooldown instead of the transient throttle window. See `docs/UPSTREAM_ERRORS.md`.
- Anti-hang resilience layers (see `docs/RESILIENCE.md`): absolute stream lifetime (`routing.stream_max_duration_seconds`, default 1800s) so trickling providers cannot hold requests forever; per-chunk downstream write deadlines (30s) so stalled clients cannot pin handlers and upstream slots, reported as `client_stalled` without poisoning deployment health; a Finagle-style retry budget (`routing.retry_budget_ratio`, default 0.2) that fails fast with `Retry-After: 1` instead of retry-storming dead providers; opt-in hedged backup attempts (`routing.hedge_delay_ms`, default off) that race the next deployment when the primary is slow; dial phase bounded at 10s with keep-alives and A/AAAA racing.
- Burst capacity for parallel-agent traffic (see `docs/RESILIENCE.md`): a bounded admission queue (`routing.admission_queue_timeout_ms`, default 30000ms) absorbs Claude Code sub-agent bursts bigger than `max_inflight_requests` (default 256) instead of rejecting instantly; per-attempt provider queue timeouts (`routing.provider_queue_timeout_ms`, default 30000ms) spill saturated first-choice providers over to the next candidate immediately (no backoff pause, no health poisoning, no retry-budget spend — a spill is a redirect, not a retry), and all-saturated requests fail fast with 503 + `Retry-After: 1`; session pins yield to least-pressure ordering while the pinned provider is full so one shared session key cannot hotspot a deployment. Provider concurrency default 64, stream idle 600s (thinking pauses), 32MiB request bodies and SSE lines so high-token generations and large tool payloads are not cut off; upstream HTTP transport does not cap connections at the semaphore; listener ReadTimeout 5 minutes with no WriteTimeout. Waits and spills are visible as `nexaroute_admission_waits_total` / `nexaroute_provider_saturated_total` metrics and `gateway_overloaded` / `provider_saturated` events.
- One-command Linux installer (`scripts/install.sh`): `curl -fsSL ... | bash` installs the binary plus a `0600` config (never overwritten) from the latest GitHub release, or automatically from source when no release exists yet; `--service` installs the user systemd unit.
- Guardrails: `max_prompt_chars` counts characters (multibyte-aware, not bytes); blocked patterns also scan decoded JSON string content so `\uXXXX`-escaped keywords cannot bypass the filter.
- Every failover candidate is revalidated immediately before use.
- The first eligible routed failure quarantines the deployment.
- Recovery performs up to five real probes; five failures enter the default 30-minute cooldown, then recovery starts again.
- Temporary all-credential `429` cooldown is treated as a wait state rather than a model-health failure.
- Successful real traffic refreshes the ready-health lease; idle healthy models are micro-probed after lease expiry.
- `/readyz` reflects actual routable health.

## Provider and credential execution

- Generic OpenAI-compatible Chat Completions and Anthropic-compatible Messages providers are supported.
- Provider concurrency is bounded until the response body or stream is consumed or closed.
- Credential pools use load/failure-aware selection and independent key cooldown.
- Environment-backed credential rotation is detected during hot reload.
- Unchanged adapters are reused across unrelated config edits so connection pools and credential state survive.
- Routed error redaction covers both the adapter's live credential snapshot and the currently resolved environment credential.

## Protocol and stream hardening

- Native same-protocol passthrough and common cross-protocol text/tool/image translation are supported.
- Malformed tool-call JSON is rejected instead of silently converted.
- Translated SSE requires valid JSON and a valid terminal protocol signal.
- Client write failures stop translated streams immediately.
- Capability detection is scoped to protocol controls/messages so tool-schema lookalikes do not force false vision/reasoning routing.
- Ingress JSON, translated upstream JSON, config files, model discovery and error-body reads are bounded.

## Admission, configuration and control plane

- Global expensive data-plane work is bounded by `routing.max_inflight_requests`; overload returns `503` while health/readiness/metrics/Admin remain observable.
- Config mutation is serialized from fresh snapshot through validation, durable write and runtime swap.
- Negative invalid values fail fast instead of being silently defaulted.
- Local provider/model IDs are constrained to unambiguous safe identifiers.
- HTTP header names/values and endpoint paths are validated before runtime.
- The Web UI wires all Ready Mesh/probe controls, including session affinity, ready lease, P2C window, capability circuit settings and global admission.
- The Web UI ships an About tab with version info and creator/support channels (email, Telegram, Telegram channel, Facebook, TikTok, Instagram, YouTube), plus a mobile tab selector so every tab stays reachable on small screens.
- The event feed uses a bounded ring buffer with bounded event fields and bounded dynamic counter-key maps.
- Operational logs self-rotate with fixed disk retention; successful access lines are sampled by default and console output is storm-limited.
- Recovery scheduling uses a fixed worker pool and bounded queue; long cooldowns no longer hold one sleeping goroutine per failed deployment.

## Startup and probing

- The HTTP listener opens first so liveness/UI diagnostics are immediately observable.
- Readiness remains false until a deployment proves healthy.
- Health probes require a bounded, fully readable, protocol-valid success envelope; HTTP `2xx` alone is not enough.
- Truncated, malformed or wrong-protocol probe responses cannot mark a deployment healthy.

## Stress and soak verification

- Normal CI now includes a bounded stress gate for large routing tables, recovery floods, overload admission, event-state pressure, and concurrent log rotation.
- A `Soak` workflow adds repeated stress rounds, same-process traffic/hot-reload cycles, repeated recovery cycles, and race-enabled soak execution. It is manually runnable and auto-runs only when stress/soak infrastructure changes.

## Verification

The authoritative acceptance gate is the repository CI plus `scripts/verify.sh`: repository cleanliness, formatting, repeated/shuffled tests, vet, race detection, fuzzing, JavaScript syntax validation, Linux cross-builds, local runtime smoke, installer smoke, Docker build and Docker runtime smoke.

# Resilience layers — NexaRoute v0.3

Every request through the gateway is bounded in time at several independent
layers, so no single stuck provider, stalled client, or retry storm can hang
the gateway (or the tool behind it, e.g. Claude Code) forever. This document
describes the current behavior only.

## Timeout layers (outermost to innermost)

| Layer | Setting | Default | Applies to |
|---|---|---|---|
| Whole non-streaming request | `routing.request_timeout_ms` | 120000 | chat/completions, messages |
| Whole streaming response | `routing.stream_max_duration_seconds` | 1800 (0 = unbounded) | SSE streams |
| Single upstream attempt | `routing.attempt_timeout_ms` | 0 = off | non-streaming attempts |
| Upstream dial (DNS + TCP) | fixed | 10s | every upstream connection |
| Upstream TLS handshake | fixed | 15s | every TLS connection |
| Upstream response headers | = request timeout | 120s | every upstream request |
| Stream idle gap | `stream_idle_timeout_seconds` (per provider) | 180s | SSE body reads |
| Downstream client write | fixed | 30s per chunk | every client write+flush |
| Probe / admin fan-out | fixed | 8–10s | probes, provider test |

Notes:

- Streams are exempt from the per-attempt timeout (bodies legitimately
  outlive it) but are bounded by the stream idle gap *and* the absolute
  stream lifetime. A provider that trickles one token per idle window is
  cut off at `stream_max_duration_seconds` with a `stream_fail` /
  `provider_timeout` event.
- The per-chunk client write deadline only ever triggers on half-open or
  non-reading clients; healthy writes finish in milliseconds. Without it, a
  stalled client would pin the handler goroutine plus the upstream stream
  and provider slot behind it until TCP gave up (often 10+ minutes).
- A downstream stall is a caller-side fault, never a deployment failure: it
  emits a `client_stalled` / `client_write_timeout` event and frees the
  handler without touching deployment health (no quarantine, no failure
  recorded), so one stuck client cannot take a healthy deployment out of
  rotation.

## Failover and the retry budget

Failed attempts fail over to the next eligible deployment (bounded by
`max_attempts`), with jittered backoff between attempts and quarantines /
cooldowns for failing deployments and credentials.

On top of that, a token-bucket **retry budget** caps follow-up attempts as a
fraction of served traffic (`routing.retry_budget_ratio`, default `0.2`,
`0` = unlimited): each admitted request deposits one token, each retry
beyond the first attempt withdraws `1/ratio`. When the bucket is empty the
gateway fails fast — rendering the last upstream error with `Retry-After: 1`
and a `retry_budget_exhausted` event — instead of hammering already-failing
providers. Under a total outage this converts a slow retry storm into fast,
precise failures while recovery probes bring deployments back.

## Burst capacity: admission queue and saturation spillover

Agent harnesses (e.g. Claude Code with parallel subagents) routinely fire
dozens of simultaneous requests. Two bounded waits absorb that shape instead
of answering instant 503s or queueing behind one busy provider:

- **Admission queue.** When all `max_inflight_requests` slots are taken, a
  request waits for a release for up to
  `routing.admission_queue_timeout_ms` (default `5000`, `0` = fail fast as
  before, max 60000). Waits are counted in
  `nexaroute_admission_waits_total`. A wait that expires — or a client that
  disconnects while queued — still answers 503 + `Retry-After: 1` with a
  `gateway_overloaded` event, so the queue is a shock absorber, not a
  parking lot.
- **Saturation spillover.** One upstream attempt waits for a provider
  concurrency slot for up to `routing.provider_queue_timeout_ms` (default
  `10000`, `0` = wait the whole route budget). On expiry the attempt spills
  to the next candidate immediately — no backoff pause, because the next
  provider is a different capacity pool — emitting a `provider_saturated`
  event and counting `nexaroute_provider_saturated_total`. Spilling spends no
  retry budget — the saturated provider did no work, so a spill is a redirect, not a retry — and it never touches
  the busy deployment's health: capacity pressure is not failure. A hedged
  loser that only gave up on a saturated slot is likewise left alone.
- **All-saturated terminal.** When every attempted candidate was saturated,
  the request fails fast with 503 + `Retry-After: 1` ("all providers
  saturated") instead of a 502 or a hung route budget.
- **Capacity-aware session affinity.** A session pin still holds while its
  provider has room, but when the pinned provider is full (pressure ≥ 1)
  the request yields to least-pressure ordering for that request, so a
  session key shared by many parallel subagents cannot hotspot one
  deployment.

Tuning for parallel-agent bursts: keep `max_inflight_requests` above the
expected parallelism (default 256), keep the admission queue timeout in
tens of seconds (default 30s) so a terminal that fires several sub-agents
together waits instead of 503ing, and keep the provider queue timeout
(default 30s) well under the request timeout so a spill still has budget
to complete elsewhere. Stream idle default is 600s so thinking models that
pause between tokens are not killed. Request/response caps: 32 MB ingress
JSON, 64 MB upstream JSON, 32 MB per SSE line — large enough for long
agentic turns and multi-MB `tool_use` payloads. The gateway never overrides
the client's `max_tokens`. The upstream HTTP transport does not cap
connections at the semaphore (that deadlock stalls a burst); HTTP/2
multiplexing is attempted. The listener has a 5-minute `ReadTimeout` and
no `WriteTimeout`, so large context uploads and long generations survive.

## Hedged backup attempts (opt-in)

`routing.hedge_delay_ms` (default `0` = off, max 60000) races a backup
attempt on the next eligible deployment when the primary has not produced
its first response within the delay. The first completed attempt wins; the
loser is cancelled and its body closed. A merely-slow loser records no
health signal; a loser that failed on its own is recorded like a normal
failed attempt. Every race emits a `hedge` event naming winner and loser.

Hedging only fires when a backup candidate exists, the attempt budget has
room, and the retry budget allows the extra load; otherwise the request
proceeds as a single attempt.

**Cost warning:** both upstreams may bill for a hedged request, because the
loser often starts generating before the cancellation lands. Enable hedging
only with two or more deployments, set the delay at or above the primary's
p95 time-to-first-byte, and watch the `hedge` event rate: constant hedging
means the delay is too low or the primary is sick.

## What the gateway still cannot do

- When every deployment fails, the gateway must answer an error. It answers
  fast (failover budget, then fail fast) and precisely (typed error +
  `Retry-After`), but it cannot invent a completion.
- A response already committed to the client (streaming 200) cannot be
  failed over; mid-stream failures end the stream and correct accounting
  only.
- Credential rotation inside one adapter is bounded by key count and never
  consults the retry budget; the budget governs deployment-level retries.

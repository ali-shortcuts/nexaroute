# Retry safety and idempotency

This document states what NexaRoute guarantees about retries, what it
records when it cannot guarantee anything, and what it does not claim.
The implementation is `internal/retrysafety` (the decision policy),
the attempt loops in `internal/httpapi` (anthropic, OpenAI, canonical
paths), and the durable guards in the video plane. Nothing here claims
exactly-once provider execution: no provider dialect shipped today
has a verified idempotency mechanism, and the conformance manifest
records idempotency as `UNTESTED` for every dialect.

## Classification: four questions per failure

Every retry decision answers, in this order:

| Question | Values | How it is known |
|---|---|---|
| Was the request sent? | not sent / unknown / confirmed | Dial refused, DNS failure, or credential-pool exhaustion before dispatch are proven not-sent. A provider response proves confirmed. Anything dispatched without a response is **unknown**: the Go HTTP client does not expose whether request bytes left the process, so the gateway never guesses "not sent" after dispatch. |
| May the provider have executed it? | none / unknown / confirmed | Authentication/validation rejection (401/403/404) is read as none: the request was rejected before model execution. A response body or stream bytes prove confirmed. 429, 5xx, timeouts, resets, and disconnects leave it **unknown**. These readings are the gateway's conservative mapping, not provider guarantees. |
| Were response bytes committed to the client? | yes / no | `responseCommitted` / the stream-commit writer: any header or stream byte written to the client counts. A committed stream is never replayed, whatever else is true. |
| Does the provider offer a verified idempotency mechanism? | none for every dialect today | Conformance manifest, `DimIdempotency`. A mechanism becomes "verified" only with fixture or live evidence **and** an adapter change that actually forwards the key for that dialect. Manifest state alone never enables forwarding. |

## Decision order

`retrysafety.Decide` applies these bars in fixed order; no later rule
can override an earlier one:

1. **Cancelled** — client or gateway context ended: never replay.
2. **Committed** — response bytes reached the client: never replay.
   A partial stream terminates honestly; the gateway does not fabricate
   the remainder, a `finish_reason`, or a `[DONE]` it never received.
3. **Not retryable** — caller validation errors (400/422) fail on every
   upstream; failover is pointless.
4. **Budget exhausted** — attempts used (including hedged legs) reached
   `routing.max_attempts`, or the request deadline passed.
5. **Durable side effect with unknown/confirmed execution** — never
   replay. The outcome is recorded as ambiguous and resolved manually
   (video recovery fences ambiguous submissions to
   `needs_manual_action` rather than resubmitting).
6. **Otherwise, pre-commit replay for a stateless generation request**
   is allowed, and is recorded as ambiguous when execution is unknown
   (bus event kind `ambiguous_outcome`, fixed metadata-only message).
   The replay may duplicate provider-side work or cost; the client
   sees nothing duplicated because nothing was committed to it.

Existing behavior this policy preserves, not invents: transport
failover to the next eligible candidate, gateway-deadline timeout as
a terminal 504 with **no** replay to another candidate, and
pre-commit decode-failure failover (a response was received but
nothing was committed to the client).

## Bounded limits

| Bound | Config key | Default | Cap / note |
|---|---|---|---|
| Upstream attempts per request (hedged legs count) | `routing.max_attempts` | 4 | 1–64; each candidate is attempted at most once per request, and a launched hedge leg counts against the same budget |
| Non-stream request deadline | `routing.request_timeout_ms` | 120000 ms | 100–1800000 ms; streams use the provider stream-idle timeout plus client cancellation instead of this deadline |
| Backoff between attempts | `routing.retry_backoff_ms` | 150 ms | 0–60000 ms; full jitter, capped at 5 s in `jitteredRetryBackoff` |
| Upstream `Retry-After` honored | `routing.max_retry_after_seconds` | 60 s | 1–86400 s; emitted to clients as delta-seconds only, so an untrusted upstream cannot inject header text or an unbounded wait |
| Capability-repair retries | `routing.max_repair_attempts` | 1 | Separate budget, capability rejections only, adapted payload (not a same-payload replay) |
| Hedging | `routing.hedging_enabled` | off | A launched hedge is a real upstream call and counts against `max_attempts` |

## Idempotency identifiers: the policy

- **The gateway never generates a provider idempotency key.**
  A generated key without a verified provider mechanism would be a
  label, not a guarantee.
- **By default, a client `Idempotency-Key` is never forwarded** to
  any upstream. The adapter forwards client headers only when the
  operator lists them in that provider's `forward_headers`
  allowlist, and the default allowlists contain no idempotency header.
- **Operator-explicit forwarding is an operator assertion, not a
  gateway claim.** If an operator lists the header in
  `forward_headers`, the gateway forwards exactly what the client
  sent. The operator thereby asserts that their specific provider
  honors it; no conformance entry verifies that assertion today.
- **A static provider header carrying an idempotency key is rejected
  at config validation.** One fixed key for every request would let
  a provider dedupe distinct requests.
- **Identical bodies are never duplicates.** Two requests with the
  same body and no idempotency key are two requests. (The opt-in
  exact-match response cache is a different mechanism: disabled by
  default, non-streaming only, TTL-bounded, and it replays with the
  producing deployment's label. It is not idempotency.)
- **A key identifies a request only by caller assertion.** For
  durable operations, a key hit returns the first record without
  comparing bodies. Callers must not reuse a key for a different
  request; the gateway cannot detect that misuse from the key alone.

## Provider idempotency support (evidence, not assertion)

| Dialect | Verified mechanism | Evidence |
|---|---|---|
| anthropic_compatible, openai_compatible, openai_responses, gemini, bedrock, azure_openai, vertex | None | Manifest `DimIdempotency` = `UNTESTED` for all seven. Gateway-side stripping / no-generation behavior is fixture-covered (see below); that proves gateway behavior, not any provider mechanism, and therefore promotes no dialect. |

## Durable-plane guards (where duplicates are actually prevented)

Stateless generation retries cannot be made exactly-once by the
gateway, so the guarantee is scoped to where durable state exists:

- **Video job creation:** concurrent creates with one idempotency
  key produce exactly one job (`internal/video/orchestrator`
  idempotency-race test; file/PostgreSQL stores return
  `ErrIdempotencyConflict` to losers).
- **Video job claims:** a claim has a single lease winner across
  concurrent claimants, including across process restart for the
  file and PostgreSQL stores (`TestTryClaimSingleWinnerUnderConcurrency`,
  `TestFileStoreClaimPersistsAcrossReopen`,
  `TestPostgresVideoStoreConcurrentClaim`).
- **Video cost settlement:** an identical replayed settlement counts
  exactly once; a conflicting amount is an error, never a silent
  overwrite (`TestLedgerConcurrentReplayedSettlementCountsOnce`,
  `TestLedgerSettlementPathsAndValidation`,
  `TestPostgresLedgerDuplicateAndConflictingSettlement`).
- **Video recovery:** ambiguous submissions (no provider job ID)
  move to `needs_manual_action` instead of being resubmitted
  (`TestRecoverNeverResubmitsAmbiguousOrDuplicatesKnownJobs`).

## Telemetry and logs

Retry-safety records carry routing metadata only: request ID,
deployment, error class, attempt counts, latency. The
`ambiguous_outcome` event message is fixed text; it never contains
a prompt, response content, credential, or upstream error text.
Upstream error bodies shown to clients or events are passed through
the adapter's `RedactBody` (provider credentials and configured
header values replaced). An ambiguous outcome is recorded on the
bounded dashboard bus and participates in the request lifecycle
trace; it is not a terminal lifecycle state by itself, because the
request may still succeed on a replay or end in
`candidate_exhausted`.

## Explicitly not claimed

- Exactly-once provider execution, for any dialect.
- Provider-side cancellation of in-flight work: the gateway ends
  its own wait and never fails over a cancelled request, but it
  cannot recall provider-side execution. Manifest cancellation
  stays `PARTIAL` (anthropic/openai, with a dedicated gateway
  fixture) or `UNTESTED` for that reason.
- Live provider behavior: all retry-safety evidence below is from
  dialect-faithful fakes and unit fixtures. Live OIDC/LLM/Bedrock/
  Vertex/Azure behavior remains UNVERIFIED (zero paid calls).
- That a timeout proves non-execution, or that a refused connection
  proves anything about a different candidate: each candidate is
  attempted at most once per request (game-day storm invariant).

## Evidence map

- `internal/retrysafety/retrysafety_test.go` — decision matrix:
  timeout before/after send, disconnect, cancellation, committed
  vs uncommitted stream, status classes, budget/deadline exhaustion,
  duplicate-identifier rules, unsupported upstream idempotency.
- `internal/httpapi/retry_safety_test.go` — gateway failure
  injection: key not forwarded by default (chat + Anthropic
  ingress), operator-only forwarding, refused-primary failover
  with no ambiguous record, gateway-deadline timeout as terminal
  504 with ambiguous record and zero secondary hits, partial
  stream with zero secondary hits and no fabricated `[DONE]`,
  cancellation with zero secondary hits, static-header config
  rejection.
- `internal/video/cost/retry_safety_test.go` — concurrent
  replayed settlement counts once; conflicting replay errors.
- Related pre-existing pins: `internal/httpapi/failure_injection_test.go`
  (F1–F6), `internal/httpapi/gameday_test.go` (storm: each candidate
  at most once; mid-stream kill: no failover after commit).

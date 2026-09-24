# NexaRoute architecture — v0.4

## Objective

NexaRoute v0.4 is a single-process Go gateway with an embedded Web UI. It accepts Anthropic-compatible and OpenAI-compatible client traffic, normalizes only when necessary, routes each request across eligible provider/model deployments, monitors health continuously, and hot-reloads provider configuration without restarting the process.

The central design rule is separation of concerns:

```text
client protocol
   -> ingress validation / request requirements
   -> router
   -> deployment health + capability filter
   -> provider registry / credential selection
   -> protocol adapter / translator when needed
   -> upstream
   -> response translator / SSE bridge when needed
   -> client
```

Translation never chooses a provider. Routing never rewrites protocol semantics.

## Data plane

```text
Claude Code / Anthropic client            OpenAI-compatible client
             |                                      |
      POST /v1/messages                   POST /v1/chat/completions
             |                                      |
             +---------------+----------------------+
                             |
                    request requirements
             model / stream / tools / vision
                             |
                        smart router
      alias + capability + health + policy + strategy
                             |
             ordered candidate deployments
                             |
                  provider registry snapshot
                    /                 \
       Anthropic adapter              OpenAI adapter
                    \                 /
                 provider credential pool
                             |
                         upstream LLM
                             |
          native passthrough or response translation
                             |
                           client
```

## Runtime snapshot model

The server owns:

- current validated `config.Config`
- provider `Registry`
- routing table
- health manager
- probe engine
- event bus

A request takes a coherent routing/provider snapshot. Hot reload swaps the registry/router under a short lock. Existing requests keep adapters they already obtained; new requests see the new configuration.

## Deployment identity

Health is tracked per `provider/model` deployment rather than per whole provider. One broken model therefore does not automatically remove every other model served by the same provider.

A deployment includes:

- provider ID/type
- upstream model ID
- local aliases
- priority and weight
- capability flags
- health status
- EWMA latency and recency-weighted failure rate
- success/failure counters
- consecutive failure count
- cooldown deadline

## Candidate routing

A request supplies requirements such as requested model/alias, tools, vision and streaming.

The router:

1. matches direct model IDs, aliases, `auto`, or `claude-auto`;
2. optionally falls back when an unknown client model is requested;
3. removes disabled or capability-incompatible deployments;
4. removes deployments still in cooldown;
5. ranks remaining deployments using the configured strategy;
6. returns an ordered candidate list for bounded attempts.

Supported strategies:

- `ready_mesh` (default): only verified `healthy` deployments are routable; a healthy session pin is preferred, otherwise deterministic power-of-two sampling chooses between candidates in the best priority tier using health/score and live capacity pressure
- `ready_queue`: legacy deterministic priority/weight ordering over verified healthy deployments
- `adaptive_round_robin`: health/scoring plus rotation among healthy top candidates
- `adaptive`: score by health, weight, priority, latency and failure history
- `priority`: health tier first, then explicit priority
- `round_robin`: rotate eligible deployments
- `least_latency`: prefer measured low-latency healthy deployments

## Failure and cooldown lifecycle

Default model/deployment policy:

```text
unknown/degraded
    -> successes -> healthy
    -> first routed failure -> quarantine -> recovery supervisor
cooldown expiry
    -> half_open
half_open success
    -> healthy
half_open failure
    -> cooldown immediately
```

Default ready-queue recovery is five supervisor probes. Any successful recovery probe immediately returns the deployment to `healthy`; five failed recovery probes enter a 30-minute cooldown, after which a fresh recovery cycle begins automatically.

Pre-stream failover can occur on:

- transport errors/timeouts
- selected provider/auth errors including 401/402/403/404
- 429 rate limits
- retryable server/gateway errors

`Retry-After` is honored up to the configured maximum. Once client-visible stream bytes have been committed, the request remains bound to that upstream; NexaRoute does not fake mid-stream continuation on another model.

## Credential pool

A provider can use:

- a primary literal API key
- a primary environment-variable key
- additional named credentials

Resolved credentials form a pool. Selection is load-aware: two usable keys are sampled and the less-loaded/lower-failure key is reserved. The reservation lasts until the upstream response body is consumed or closed, so long-lived SSE streams count as real key load. A key that returns authentication/quota/rate-limit statuses is independently cooled down, allowing another key for the same provider to be tried before failing the deployment.

Client-side `Authorization`, `x-api-key`, cookies and admin credentials are treated as sensitive and are not blindly forwarded. Provider auth is applied after custom headers so stale user-configured Authorization values cannot override an explicit configured credential.

## Provider concurrency and global admission

Each provider adapter has a bounded semaphore. Requests waiting on that semaphore respect context cancellation. This prevents one provider from accumulating unbounded simultaneous work while still allowing other providers to be routed independently.

The HTTP middleware also enforces a global data-plane admission ceiling through `routing.max_inflight_requests`. Excess model requests are rejected before expensive request processing/provider work begins, with `503` and `Retry-After`; liveness, readiness, metrics and Admin endpoints remain available for diagnosis.

Provider adapters are reused across unrelated hot reloads so HTTP pools and credential cooldown state survive. If an environment-backed credential resolves to a new value, NexaRoute detects the snapshot mismatch and rebuilds only that provider adapter so rotated keys are picked up without rebuilding the entire registry.

## Probe plane

The probe engine executes tiny health requests with bounded concurrency. Under the default `ready_mesh` strategy, automatic background sweeps do not re-probe a deployment that is already `healthy`; they establish readiness for `unknown` deployments and ensure degraded/cooldown deployments have a recovery supervisor. Legacy strategies such as `adaptive` retain their periodic health-probe semantics. A healthy deployment leaves the ready queue only after a real routed failure or after a hot-reload change invalidates its previous health proof. At startup the HTTP listener opens first so liveness and operator surfaces are immediately observable, then the engine primes all enabled deployments. Under `ready_mesh`, `/readyz` remains unready and Claude traffic has no eligible candidates until a deployment has passed a health request.

Defaults:

- 1 output token
- 8 second timeout
- 16 concurrent probes by default
- 5 recovery attempts by default
- 500 ms delay between failed recovery probes
- 30-minute default recovery cooldown
- every 120 seconds

Manual `Probe all models` is the intentional exception: it can explicitly retest healthy deployments and returns a structured result including total, passed, failed, ready-skipped, cooldown-skipped, recovery-skipped and missing-adapter counts.

Authentication/quota/rate-limit failures can immediately cool a deployment instead of spending several normal user requests discovering the same failure.

## Provider lifecycle / control plane

```text
Web UI editor
 -> Admin API
 -> parse + validate proposed config
 -> build provider registry as preflight
 -> atomic save (0600) through a same-directory temporary file + fsync + rename
 -> short runtime write lock
 -> registry reload
 -> router reload
 -> health/probe settings reload
 -> release lock
```

## Secret-preserving edit semantics

An unrelated provider edit must not destroy a working credential.

When Edit opens, the UI restores the provider's saved Base URL, protocol, auth mode, model list, proxy, endpoint overrides, forwarded headers, concurrency, credential pool and credential source. An authorized local/admin user may reveal the resolved secret into a password field.

If the user does not change secret fields, the update sends `preserve_secret=true`, and the server preserves the existing `api_key`, `api_key_env`, and credential pool instead of replacing them with blank values.

## Protocol behavior

### Native same-protocol path

When client and provider use the same protocol, NexaRoute favors raw JSON/SSE passthrough and patches only the routed model ID where required. Unknown fields remain intact whenever possible.

### Anthropic -> OpenAI-compatible

The translator covers common text, system content, tool definitions, tool calls, tool results, selected vision/image forms, non-stream responses, streaming text and parallel tool calls.

### OpenAI -> Anthropic-compatible

The reverse translator covers common text/tools and streaming paths, but provider-specific extensions cannot always be mapped losslessly.

### Anthropic token counting

For an eligible Anthropic-compatible upstream, `/v1/messages/count_tokens` first calls that provider's native token-count endpoint. If no native path is usable, NexaRoute returns a local conservative estimate with `estimated=true` rather than pretending it is exact.

## SSE and cancellation

Native SSE passthrough explicitly flushes chunks. Cross-protocol SSE emits deterministic content-block indices. Stream adapters cancel upstream work when the client disconnects or the configured stream idle deadline is exceeded.

## Web UI topology

The UI is embedded into the binary with `go:embed`. The model topology shows deployment state around the router core, with concentric rings for dense pools. A separate table remains the authoritative complete list.

## Admin boundary

By default:

- server listens on loopback
- admin API is loopback-only
- if `admin.api_key` is configured, it is required in addition to any network-boundary rule
- comparisons use constant-time equality
- browser admin key is kept in `sessionStorage`, not persisted to config/localStorage

The project is not designed to be placed directly on the public internet without TLS/reverse-proxy hardening and additional access controls.


## Ready Mesh scheduling

Ready Mesh keeps the request path deterministic and network-free at selection time.

- Request structure creates an explicit requirement profile: model/alias, tools, vision, streaming and reasoning.
- A bounded session key is derived from Claude Code/LiteLLM-compatible headers or request metadata.
- A successful session is pinned to its deployment for a configurable TTL while that deployment remains eligible.
- New or unpinned sessions stay inside the best configured priority tier and use power-of-two selection rather than scanning for a global winner on every request.
- Live provider pressure is computed from active requests plus waiting requests relative to provider concurrency.
- Capability failures can open a scoped circuit (for example streaming) without poisoning plain text traffic for the same deployment.
- The handler revalidates each candidate immediately before an attempt, closing the stale-candidate window during concurrent quarantine or hot reload.

This is deliberately not an opaque learned router in the data plane. Task awareness comes from explicit request capabilities and configured aliases/profiles so routing decisions remain explainable and regression-testable.


## Failure-domain-aware fallback ordering

For `ready_mesh`, primary selection still follows health, capability, priority,
session affinity, score and live capacity. After the primary is fixed, fallback
candidates are diversified across provider IDs within the same priority tier
whenever possible. This reduces correlated retry storms when one provider is
experiencing a regional, authentication, quota or transport incident, while
keeping same-provider deployments available after independent failure domains
have been tried.


## Provider incident circuits

Deployment health and provider health are separate failure domains. A model-specific
failure such as a 404 can quarantine only that deployment. Provider-wide evidence
is reserved for transport failures, exhausted authentication/billing paths,
rate limits, timeouts, and server/overload failures.

The provider circuit opens only after failures from multiple distinct deployments
inside a bounded evidence window. One broken model therefore cannot suppress an
entire provider. While a provider cooldown is active, stale success or failure
observations from requests that were already in flight cannot clear or downgrade
that cooldown. Cooldown expiry enters half-open state; a new provider-level
failure immediately reopens the circuit, while a verified success after the
deadline closes it.

The router filters providers with open incident circuits before deployment
scoring. Existing per-deployment health proofs remain intact, so provider
recovery does not require reconstructing every model's health history. A Base
URL/authentication identity change explicitly invalidates the old provider
incident state so a repaired endpoint is not held behind stale evidence.

## Quota and streaming telemetry

HTTP adapters observe common OpenAI and Anthropic rate-limit headers for remaining
requests, remaining tokens, and reset times. These values are advisory because
providers do not standardize quota semantics. A provider whose quota is known to
be exhausted until a future reset is strongly deprioritized rather than
absolutely removed, preserving a last-resort path when no alternative exists.

For streaming traffic, NexaRoute records EWMA time-to-first-byte (TTFT) from
request dispatch to the first upstream body bytes. Response-header latency
remains a separate metric. NexaRoute intentionally does not label byte
throughput as token throughput; exact cross-provider tokens/second requires
protocol-aware usage accounting.


## In-flight quota reservation overlay

Provider rate-limit headers are necessarily retrospective: several concurrent
requests can select the same provider before the first one returns a fresh
`remaining-*` value. NexaRoute v0.5.2 overlays local in-flight reservations
on that external evidence.

Only tagged data-plane Chat/Messages attempts reserve quota. Each real upstream
leg reserves one request plus a bounded token estimate (conservative prompt
estimate + explicit output ceiling when available). Hedged primary/secondary
legs therefore reserve independently, matching their actual upstream
amplification.

Provider stats expose both raw observed values and effective values:

```text
effective_requests = max(0, observed_remaining_requests - reserved_requests)
effective_tokens   = max(0, observed_remaining_tokens   - reserved_tokens)
```

Unknown observed quota remains unknown; local reservations never manufacture a
quota ceiling. Routing pressure uses effective headroom only while the relevant
provider reset window is still known to be in the future.

A fresh resource-specific remaining header supersedes the local reservation for
that resource immediately. If no fresh header is returned, the reservation is
held until the response body is consumed or closed, so a long-lived stream
continues to apply pressure while it occupies uncertain quota.

This layer is advisory rather than authoritative throttling. It intentionally
does not persist rolling-window debt after a completed response that supplied no
fresh quota evidence, and it is process-local rather than distributed state.

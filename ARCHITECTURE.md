# NexaRoute architecture — v0.3

## Objective

NexaRoute v0.3 is a single-process Go gateway with an embedded Web UI. It accepts Anthropic-compatible and OpenAI-compatible client traffic, normalizes only when necessary, routes each request across eligible provider/model deployments, monitors health continuously, and hot-reloads provider configuration without restarting the process.

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
- EWMA latency
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
    -> repeated failures -> cooldown
cooldown expiry
    -> half_open
half_open success
    -> healthy
half_open failure
    -> cooldown immediately
```

Default threshold is five consecutive failures and default cooldown is one hour.

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

Resolved credentials form a pool. Selection rotates through usable keys. A key that returns authentication/quota/rate-limit statuses is independently cooled down, allowing another key for the same provider to be tried before failing the deployment.

Client-side `Authorization`, `x-api-key`, cookies and admin credentials are treated as sensitive and are not blindly forwarded. Provider auth is applied after custom headers so stale user-configured Authorization values cannot override an explicit configured credential.

## Provider concurrency

Each provider adapter has a bounded semaphore. Requests waiting on that semaphore respect context cancellation. This prevents one provider from accumulating unbounded simultaneous work while still allowing other providers to be routed independently.

## Probe plane

The probe engine executes tiny health requests with bounded concurrency.

Defaults:

- 1 output token
- 8 second timeout
- 16 concurrent probes
- every 120 seconds

Manual `Probe all models` uses the same engine and can wait for a structured result: total, passed, failed, cooldown-skipped and missing-skipped.

Authentication/quota/rate-limit failures can immediately cool a deployment instead of spending several normal user requests discovering the same failure.

## Provider lifecycle / control plane

```text
Web UI editor
 -> Admin API
 -> parse + validate proposed config
 -> build provider registry as preflight
 -> atomic save (0600) + one last-known-good .bak
 -> short runtime write lock
 -> registry reload
 -> router reload
 -> health/probe settings reload
 -> release lock
```

Rollback restores the single `.bak` configuration and hot-reloads it.

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

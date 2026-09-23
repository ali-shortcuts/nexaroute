# Gateway Reliability Audit — 2026-09-22

## Scope

This audit compares NexaRoute's Claude Code-focused gateway path against reliability patterns visible in mature open-source AI/API gateways and routing projects.

Representative projects reviewed:

- BerriAI/LiteLLM — https://github.com/BerriAI/litellm
- Maxim/Bifrost — https://github.com/maximhq/bifrost
- Portkey Gateway — https://github.com/Portkey-AI/gateway
- Kong Gateway — https://github.com/Kong/kong
- Higress — https://github.com/higress-group/higress
- Helicone — https://github.com/Helicone/helicone

This is intentionally a representative deep survey rather than a false claim that every gateway repository on GitHub was exhaustively read. The audit focuses on transferable bug classes and resilience patterns relevant to NexaRoute's supported protocol surface.

## NexaRoute client-facing protocol surface

Supported:

- `POST /v1/messages`
- `POST /v1/messages/count_tokens`
- `POST /v1/chat/completions`
- `GET /v1/models`
- `GET /healthz`
- `GET /readyz`
- `GET /metrics`

Control plane:

- `GET /admin/api/snapshot`
- `POST /admin/api/probe`
- provider CRUD under `/admin/api/providers`
- `POST /admin/api/provider-test`
- `POST /admin/api/provider-discover`
- `GET/PUT /admin/api/settings`

Deliberate current protocol gaps remain documented in `docs/KNOWN_GAPS.md`, including native OpenAI Responses, embeddings/rerank, Gemini-native, Bedrock/Vertex/Azure-specialized semantics, and distributed/cluster state.

## Reliability patterns reviewed and adopted

### 1. Pre-verified routing instead of request-time discovery

NexaRoute now uses `ready_queue` as the default route lifecycle.

Invariant:

- `Unknown` is not routable.
- A successful health proof makes the deployment `Healthy`.
- Only `Healthy` deployments can serve Claude traffic.
- Configured priority/weight determines the sticky strongest healthy deployment.
- The first eligible routed failure removes it immediately.

This gives request-time routing a local, fast decision instead of blocking Claude on a fresh network health check.

### 2. Traffic-refreshed health leases for the ready pool

Under the default `ready_queue` strategy, a successful health proof has a bounded lease instead of being trusted forever.

- every successful real Claude request refreshes `LastChecked` and therefore renews the deployment's ready-health lease;
- automatic sweeps skip healthy deployments while that lease is fresh;
- a healthy deployment that stays idle past the lease is revalidated with the same tiny micro-probe;
- new/unverified deployments still require an initial probe before they can become routable;
- degraded/cooldown deployments remain owned by their recovery loops.

This combines passive health feedback from real traffic with sparse active checks for cold fallbacks. It avoids repeatedly probing active models while also preventing an unused backup from remaining falsely `Healthy` indefinitely.

The explicit operator action **Probe all models** can still force a complete retest.

### 3. Dedicated recovery lifecycle

A failed ready deployment is quarantined immediately and handed to a per-deployment recovery supervisor.

Default lifecycle:

1. try recovery probe;
2. return to ready immediately on first success;
3. continue up to five real recovery failures;
4. after five failures, enter a 30-minute cooldown;
5. after cooldown, automatically begin a fresh recovery cycle.

Claude traffic never consumes the recovery budget.

### 4. Temporary credential rate limits are not model failures

Mature gateways distinguish credential/account state from provider/model health.

NexaRoute now treats an all-key temporary `429` cooldown as a wait state:

- the provider adapter exposes Retry-After state;
- the recovery supervisor waits;
- that wait does not consume one of the five recovery attempts;
- 401/402/403 remain real credential failures.

This prevents a one-minute API-key rate-limit window from turning into a false 30-minute model quarantine.

### 5. Cancellation is not provider failure

Client disconnect / canceled Claude requests no longer poison provider health.

NexaRoute records a normalized `caller_cancelled` event and stops the request without quarantining an otherwise healthy deployment.

This follows the fault-domain separation used by mature gateways such as Bifrost.

### 6. Total request budget across failover

For non-stream requests, `routing.request_timeout_ms` now acts as a total routing/failover deadline rather than silently granting the full timeout again to every candidate.

This prevents a nominal 120-second request limit from becoming several multiples of 120 seconds after sequential failover.

Streaming keeps its separate stream-idle protection.

### 7. Exponential backoff with jitter

Sequential failover pauses now use bounded exponential backoff with deterministic full jitter.

Goals:

- reduce synchronized retry storms;
- preserve reproducibility for incident replay/tests;
- cap the delay;
- stop immediately when request context is canceled.

### 8. Stream-completion validation

Translated protocol streams now require a terminal protocol signal.

Examples:

- OpenAI-compatible -> Anthropic translation accepts `[DONE]` or a terminal `finish_reason`;
- Anthropic-compatible -> OpenAI translation accepts `message_stop` or a terminal stop reason.

A connection that ends mid-stream without a terminal signal is no longer recorded as a successful model response.

### 9. Hot-reload health-proof invalidation

Health is not transferable across a materially changed deployment.

Changing any of the following invalidates the previous health proof:

- Base URL;
- API credentials / credential pool;
- auth mode;
- proxy;
- endpoint overrides;
- forwarded-header behavior;
- upstream model ID.

Deleted deployments have their health state pruned.

Cosmetic display-name changes do not unnecessarily eject a healthy deployment.

### 10. Ready endpoint reflects real routing readiness

Under `ready_queue`, `/readyz` is successful only when at least one deployment is actually `Healthy`.

`Unknown`, `Degraded`, `HalfOpen`, and `Cooldown` no longer make readiness appear green.

### 11. Normalized error taxonomy

NexaRoute events/metrics now distinguish stable fault families instead of exposing only a generic `route_fail` count.

Current normalized classes include:

- `caller_cancelled`
- `caller_invalid_request`
- `provider_auth_failed`
- `provider_billing`
- `provider_rate_limited`
- `provider_overloaded`
- `provider_timeout`
- `provider_connection_failed`
- `provider_stream_error`
- `provider_server_error`
- `provider_request_rejected`
- `_OTHER`

Prometheus exports:

`nexaroute_errors_total{error_type="..."}`

This makes provider-health alarms distinguishable from caller/request failures.

### 12. Response/log correctness

The HTTP status wrapper now keeps the first committed response status.

A later duplicate `WriteHeader` call cannot make access logs claim a different status from the response actually sent, and an implicit streaming `Flush` correctly commits/logs HTTP 200.

## Existing strengths retained

NexaRoute already had several strong gateway behaviors that were kept:

- provider concurrency limits held until response body close;
- OpenAI-compatible and Anthropic-compatible provider classes;
- credential pools and key rotation;
- explicit forward-header allowlists;
- secret redaction in upstream errors;
- sensitive/hop-by-hop response-header stripping;
- stream idle timeout support;
- response-body size limits for error reads;
- atomic config persistence;
- race-detector/fuzz/installer/Docker CI coverage;
- native Anthropic token-count forwarding with explicit estimated fallback;
- health-aware candidate filtering and immediate failover before client-visible response commitment.

## Patterns intentionally not copied into v0.3

Some mature gateways include valuable but much larger subsystems:

- distributed Redis/gossip state;
- semantic cache;
- policy/guardrail engines;
- billing/cost accounting;
- enterprise RBAC/SSO;
- broad file/audio/image/batch APIs;
- every cloud provider's native protocol;
- hedged duplicate requests;
- cluster-wide adaptive load balancing.

These are not blindly imported into the Claude Code-focused v0.3 core because doing so would increase state, attack surface, latency, and regression risk. They remain candidates for separately scoped versions.

## Remaining high-value future protocol candidates

If NexaRoute's product scope expands beyond Claude Code, the highest-value protocol additions are:

1. native OpenAI `/v1/responses`;
2. embeddings;
3. provider-specific capability metadata rather than manual capability flags;
4. structured cost/rate-limit budgets;
5. multi-node health/state replication.

They should be added as separately tested protocol surfaces, not mixed into the current routing hardening pass.

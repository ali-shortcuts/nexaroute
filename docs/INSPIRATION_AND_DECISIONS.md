# Inspiration and engineering decisions

This project borrows *patterns*, not source code.

## 9Router — what is useful

- Dashboard-first provider/model management
- A visible model topology / live operational view
- A live console log of every routing decision
- Model aliases/tiers and automatic fallback concept
- Local self-hosted workflow

We keep the topology idea and the console-log concept but make routing health explicit per deployment and separate UI from protocol correctness. The v0.4 dashboard adopts 9router's KPI-card + live-console + topology layout with a dark premium design system and zero external assets.

## 9Router open-sse — translation patterns adopted in v0.4

- Format-detection boundary with per-pair request/response translators
- Tool-name sanitization with reversible mapping for names violating the
  target protocol grammar (MCP-style names survive the round trip)
- Dropping unknown content block types instead of dead-ending conversations
- `stream_options: {"include_usage": true}` injection for real token
  accounting with a graceful retry against providers that reject it
- max_tokens raised above thinking budgets instead of dropping the request

## LiteLLM — what is useful

- Per-deployment cooldown instead of disabling an entire logical model
- Failure thresholds and temporary removal from the active pool
- Router-centric multi-deployment abstraction

NexaRoute's current default is stricter and more explicit: `ready_mesh` routes only verified healthy deployments; the first eligible routed failure quarantines a deployment immediately, the recovery supervisor performs up to five real recovery probes, and five failed probes enter the default 30-minute cooldown before a new recovery cycle.

Translation patterns adopted in v0.4 from LiteLLM: the tool-name chokepoint with forward/reverse maps, the thinking-budget guard that drops reasoning over tool-using histories without signed thinking blocks, cache-token mapping in both directions, and `user` ↔ `metadata.user_id`.

From new-api: merging consecutive same-role messages, coalescing parallel tool results into one user turn, dense tool-call index mapping in streams, and role-first chunk emission.

From claude-code-proxy / y-router: the warning example — their silent drops (thinking history, stop sequences, tool_choice) are exactly the gaps v0.4 closes explicitly.

## Portkey Gateway — what is useful

- Fallbacks, retries, timeouts and weighted load balancing as first-class routing behavior
- Nestable routing concepts are a good future policy model
- Guardrails/observability belong at the gateway boundary, not inside provider adapters

## Bifrost / other Go gateways — what is useful

- Go is a good fit for a low-overhead gateway with many concurrent streams
- Keep provider interfaces small and make routing/observability independent from individual providers

## What we intentionally do NOT copy

- Provider-specific auth shortcuts that depend on brittle web sessions
- Silent mid-stream fallback
- One enormous translation function for every protocol
- Treating all errors as retryable
- A UI that hides why a deployment was removed from rotation

The dashboard therefore exposes health, latency, consecutive failures, cooldown and recent routing/probe events.

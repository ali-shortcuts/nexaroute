# Inspiration and engineering decisions

This project borrows *patterns*, not source code.

## 9Router — what is useful

- Dashboard-first provider/model management
- A visible model topology / live operational view
- Model aliases/tiers and automatic fallback concept
- Local self-hosted workflow

We keep the topology idea but make routing health explicit per deployment and separate UI from protocol correctness.

## LiteLLM — what is useful

- Per-deployment cooldown instead of disabling an entire logical model
- Failure thresholds and temporary removal from the active pool
- Router-centric multi-deployment abstraction

Our requested default is stricter: 5 consecutive failures -> 1 hour cooldown, configurable.

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

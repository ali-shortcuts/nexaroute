# Live Visual Agent Specification

**Status:** Implemented initial vertical slice; backend attempt-start metadata remains a documented capability gap.

## Requirements

- **LVA-001** The Visual Agent is part of the existing Overview topology and never a second dashboard.
- **LVA-002** Go routing and backend events remain authoritative; the browser has zero routing authority.
- **LVA-003** One frontend event transport feeds Activity and the Visual Agent.
- **LVA-004** Executions are keyed only by the existing privacy-safe `request_id`.
- **LVA-005** Idle Overview displays `Waiting for requests` and does not invent traffic.
- **LVA-006** Success, failure, failover, deployment, and latency are shown only when supported by real events/snapshot data.
- **LVA-007** Unknown or malformed events are ignored safely and cannot crash the UI.
- **LVA-008** Detailed execution state is bounded; terminal state is evicted after a bounded settle interval.
- **LVA-009** Visual motion is presentation-only and supports `prefers-reduced-motion`; static semantics remain available.
- **LVA-010** The accessible status equivalent uses only real request identity, deployment, semantic state, and measured latency.
- **LVA-011** Request history is deduplicated by sequence and historical preload is not treated as new motion.
- **LVA-012** No prompts, responses, authorization headers, provider API keys, client keys, or secret metadata enter the visual surface.

## Current capability boundary

The current backend emits request-correlated `route_fail`, `failover`/`request_failover`, `route_ok`, `candidate_exhausted`, timeout, disconnect, and stream-failure events. It does not consistently emit a request-correlated attempt-start event and does not currently expose an `epoch` field in `events.Event`. The initial implementation therefore renders evidence-backed deployment/result state and does not claim an unobserved attempt-start semantic.

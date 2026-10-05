# NexaRoute Professional Dashboard Blueprint

## Product direction

NexaRoute is presented as an operations command center rather than a provider table. The dashboard shows the operator what is configured, what is healthy, how traffic will move, and what action is safe to take next. Every value is backend-owned; the UI never invents health, routing or activity.

## Patterns adopted from the comparison

| Pattern | Source inspiration | NexaRoute implementation |
|---|---|---|
| Layered health | LiteLLM, CCR, Bifrost | Gateway, providers, routing and client access are separate health layers with distinct recovery actions. |
| Stable public identity | LiteLLM, OpenRouter, Portkey | Routes show a stable public model and separate upstream deployments. |
| Explainable execution | OpenRouter, Portkey, LiteLLM | Routing cards show automatic selection or ordered fallback and backend compilation state. |
| Progressive onboarding | Portkey, CCR | Provider setup starts with protocol, endpoint, credential and model discovery; advanced details remain out of the primary path. |
| Operational topology | CCR, Bifrost | Overview shows Client → NexaRoute → eligible targets using real snapshot state. |
| Privacy-safe activity | Helicone, Portkey, LiteLLM | Activity is a searchable request/event workspace with incidents separated from normal signals. |
| Credential separation | All six products | Upstream credentials, admin authentication and client access remain distinct; upstream secrets are write-only. |
| Advanced boundary | LiteLLM, Portkey, CCR | Candidate pools, profiles, fallback chains and raw objects are available behind explicit Advanced disclosure. |

## Primary surfaces

### Overview

The command center combines a calm hero, four backend counters, four-layer health posture, a live routing topology, and recent signals. Idle state is intentionally quiet. Real request activity is shown only when emitted by the SSE/backend stream.

### Providers

The provider registry provides search, protocol identity, endpoint visibility, model chips, credential state, health state, manage and scoped test actions. The empty state explains the safe setup sequence rather than showing a blank table.

### Routing

Routes are stable public identities with visible behavior, target count, backend compilation state and Connect/Edit/Delete actions. Advanced objects remain inspectable but do not become top-level navigation.

### Activity

Activity is an operational table rather than a decorative feed. It exposes buffered signals, request counter, stream state, privacy posture, search, request filtering and incident filtering. Raw prompt/content logging is not introduced.

### Settings

Settings separates operator intent from expert internals. Routing strategy, session affinity, automatic failover and health checking remain visible. Client access makes the security posture explicit, while raw thresholds and headers stay behind the backend contract.

## Backend contract rule

The UI reads `/admin/api/snapshot`, `/admin/api/providers`, `/admin/api/events/stream`, `/admin/api/provider-discover`, `/admin/api/provider-check`, `/admin/api/provider-test`, `/admin/api/simple-routes/*` and `/admin/api/settings`. Routing decisions remain entirely in Go; the browser only presents and submits backend-owned state.

## Acceptance

The real Chromium flow must cover the five navigation surfaces, provider discovery and persistence, route persistence, settings visibility, activity filtering, no-native-dialog behavior, reduced motion CSS, and no secret leakage. Docker image execution and human visual acceptance remain environment/release gates rather than UI implementation assumptions.

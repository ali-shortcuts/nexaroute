# Events Contract

## Endpoint

`GET /admin/api/events/stream?limit=64&since=<seq>` is authenticated SSE. It emits `id`, `event: event`, and JSON `data`; it sends a bounded historical snapshot first, then live events, plus comment keepalives. Reconnect uses `since` or `Last-Event-ID`. On gaps or epoch changes the client refreshes `/admin/api/snapshot`.

## Safe event fields

The Go event bus exposes sequence/time plus request identity, kind, deployment, message, status, latency, error type and bounded routing metadata. It must not expose authorization keys, provider credentials, raw prompt text or raw response bodies.

## Operational kinds

The production lifecycle set is `model_healthy`, `model_failed`, `model_recovered`, `model_cooldown`, `provider_rate_limited`, `route_changed`, `request_failover`, and `recovery_failed`; request journey may also use privacy-safe `route_attempt`, `route_ok`, `route_fail`, `failover`, `candidate_exhausted`, and related backend events.

## Client behavior

One SSE owner, one bounded retry loop, no duplicate subscriptions, no aggressive polling. Idle Overview is static. Only real events animate a route path. Hidden tabs pause expensive rendering.

## Live Visual Agent capability boundary

The current `events.Event` wire object exposes `seq` but not `epoch`; the current frontend implementation must therefore deduplicate by sequence within one process and must not claim restart-safe epoch reconciliation. Per-request `route_attempt` is not consistently emitted by the current request path. The Live Visual Agent maps only request-correlated events that are actually emitted and records these gaps in `live-visual-agent/event-mapping.md`.

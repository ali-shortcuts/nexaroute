# Bug-avoidance decisions

This document records failure classes observed in existing router ecosystems and the defensive choices made in this project.

## 1. Editing must not blank a credential

Defense:

- existing provider details are loaded when the editor opens
- backend supports `preserve_secret`
- unrelated edits reuse the original secret fields exactly

## 2. Environment references must not be replaced by resolved literals

Defense:

- `api_key_env` is stored separately from `api_key`
- an edit that did not touch credential fields preserves the prior source
- resolved values are used for testing/runtime but are not automatically written back as literals

## 3. Custom headers must not silently override the configured credential

Defense:

- custom headers are applied first
- configured Bearer or `x-api-key` auth is applied second
- with `auth_mode=none`, custom auth headers remain possible intentionally

## 4. UI protocol choice must not be silently merged with an old choice

Defense:

- the provider type is a single explicit enum
- update replaces that field exactly
- backend does not merge an older provider type back into a new edit

## 5. Config update must not partially mutate live routing

Defense:

- proposed runtime objects are built first
- config is validated and persisted atomically
- active runtime is swapped only after success

## 6. Dead provider/model must not block the whole pool

Defense:

- health is per deployment
- under the default `ready_mesh` strategy, the first eligible routed failure quarantines the deployment immediately
- a dedicated recovery supervisor owns the five-attempt recovery/cooldown lifecycle
- quarantined/cooldown deployments are excluded from client routing

## 7. Mid-stream fallback must not corrupt a response

Defense:

- failover is allowed only before response bytes are committed
- the gateway does not pretend arbitrary streamed generation can be resumed safely on another model

## 8. A winning attempt must not be serialized behind a losing one

Failure class: a first-attempt hedge races a second deployment, the primary answers
with a servable response, and the implementation then waits for the slower hedge
leg to finish before replying. The race that exists to remove tail latency adds it
back: the client waits for the losing leg, and the loser keeps burning provider
quota and holding a concurrency slot until the route deadline.

Defense:

- the winner is returned as soon as it delivers response headers;
- the losing leg's context is cancelled immediately, and its body is closed off
  the request path once the transport releases it;
- if the primary answers with a status the caller would fail over from, the
  in-flight hedge leg is the natural failover, so its transport result is awaited
  and preferred instead of being discarded and repeated;
- a hedge is never launched once the primary has already settled;
- the latency contract is pinned by a unit test and by the live v0.5 smoke test,
  not only by the hedge-wins path.

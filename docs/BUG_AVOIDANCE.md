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

- v0.4 provider type is a single explicit enum
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
- v0.4 does not pretend arbitrary streamed generation can be resumed safely on another model

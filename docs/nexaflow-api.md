# NexaFlow API Reference (Phase 8)

Base: the gateway's admin API. All `/admin/api/nexaflow/*` routes
require the `workflow.read` permission for GET and `workflow.write`
for state-changing methods (owner, admin, and operator hold both;
developer and viewer hold read only). Unmapped routes and
unauthorized roles are denied by the central middleware before any
handler runs. When NexaFlow is not attached, handlers answer 503.

Error shape (shared with the rest of the admin API):

```json
{"error": {"type": "gateway_error", "message": "<code>: <sanitized message>"}}
```

Status mapping: `permission_denied` → 403, `invalid_state` /
`run_conflict` / `replay_rejected` → 409, validation / input /
output bound failures → 400, unknown ids → 404, unattached → 503.
Error messages are redacted server-side; secret material never
appears in them.

CSRF: server-to-server calls with `x-admin-key` and no
`Origin`/`Referer` header need no CSRF token. Browser clients must
send `X-NexaRoute-CSRF` from `GET /admin/api/csrf-token` (the UI
handles this automatically).

## Node catalog

```
GET /admin/api/nexaflow/nodes
```

Returns the 23 registry nodes with id, version, category, title,
description, input/output/config schemas (`name`, `type`,
`required`, `description`), permission, side effect (`read`,
`external`, `billable`; no v1 node is `irreversible`),
`max_attempts`, `retryable_codes`, timeouts in milliseconds, input
and output byte bounds, and `requires_operator_policy`.

## Templates

```
GET /admin/api/nexaflow/templates
```

Returns the 5 inert templates plus `"inert": true`. Templates are
reference shapes, not runnable definitions: this endpoint never
instantiates or executes one, and there is no
template-to-definition conversion in this phase.

## Definitions

```
GET    /admin/api/nexaflow/definitions?tenant=<scope>
POST   /admin/api/nexaflow/definitions
GET    /admin/api/nexaflow/definitions/{id}
POST   /admin/api/nexaflow/definitions/{id}/validate
POST   /admin/api/nexaflow/definitions/{id}/publish
GET    /admin/api/nexaflow/definitions/{id}/versions
GET    /admin/api/nexaflow/definitions/{id}/versions/{rev}
```

- `POST /definitions`: body is a full definition. It is validated
  (`ValidateDefinitionExtended`) before persisting; invalid
  definitions are rejected with 400 and nothing is saved.
- `POST /{id}/validate`: validates the saved draft, returns
  `{"valid": true}` or `{"valid": false, "error": "..."}`. It
  never persists anything.
- `POST /{id}/publish`: publishes the saved draft as a new
  immutable revision, superseding the previously published one.
  Returns `{"definition": ..., "version": {definition_id,
  revision, content_hash, status}}`. The publisher is the calling
  identity's subject.
- `GET /{id}/versions`: all version records for the definition.
- `GET /{id}/versions/{rev}`: the definition snapshot plus its
  version record. This is the rollback source: save it as the
  draft and publish again to roll back (as a NEW revision).

Drafts never execute. Only published revisions run, and a run pins
the revision it started on.

## Runs

```
POST   /admin/api/nexaflow/definitions/{id}/runs
GET    /admin/api/nexaflow/definitions/{id}/runs?limit=50
GET    /admin/api/nexaflow/runs/{id}
POST   /admin/api/nexaflow/runs/{id}/approve
POST   /admin/api/nexaflow/runs/{id}/resume
POST   /admin/api/nexaflow/runs/{id}/cancel
```

- `POST .../runs`: body `{"input": {...}, "revision": 0,
  "idempotency_key": "..."}`. `revision 0` means the currently
  published revision. A repeat call with the same definition and
  idempotency key returns the original run instead of dispatching
  a duplicate. Returns 202 `{"run_id": "...", "status":
  "accepted"}`: `"accepted"` is the dispatch outcome, not a run
  status — execution continues in the background, so poll the run
  for its terminal status. Validation, idempotency, and
  concurrency failures are returned synchronously (400/404/409).
- `GET .../runs`: run summaries (id, definition, revision, tenant,
  trigger, actor, status, error code, timestamps). `limit` caps at
  100.
- `GET /runs/{id}`: the full run, including steps (node, attempt,
  iteration, status, timestamps, duration, redacted input/output,
  sanitized error), outputs (latest succeeded output per node;
  iterated outputs are arrays), decisions, and the run error.
- `POST /runs/{id}/approve`: body `{"decision": "approved" |
  "rejected"}`. The run must be `awaiting_approval` and the
  approver must differ from the run's actor. Returns the resumed
  run (which may itself suspend again or finish).
- `POST /runs/{id}/resume`: body `{"mode": "continue" |
  "retry_ambiguous" | "fail_ambiguous" | "skip_ambiguous"}`. The
  run must be `parked`. Returns the resumed run.
- `POST /runs/{id}/cancel`: cancels an in-flight run, or marks a
  parked/awaiting run cancelled directly. Returns the run.

Run statuses: `pending`, `running`, `awaiting_approval`,
`succeeded`, `failed`, `cancelled`, `parked` (resumable, hence not
terminal). Step statuses: `pending`, `running`, `succeeded`,
`failed`, `skipped`, `cancelled`, `waiting`, `ambiguous`.

## Audit

```
GET /admin/api/nexaflow/audit?limit=100
```

Most-recent-first workflow audit events from the workflow store
(`actor`, `action`, `definition_id`, `run_id`, `success`,
`detail`, `at`). Actions include `run_started`,
`run_awaiting_approval`, `run_approved`, `run_resumed`,
`run_cancelled`, `run_parked`, `run_finished`. Events carry no
payloads, input values, or secret material. `limit` caps at 100.
This is the workflow store's audit trail, separate from the
gateway's hash-chained admin audit (which additionally records
every admin operation itself).

## Scheduler

```
POST /admin/api/nexaflow/scheduler/tick
```

Dispatches every due schedule once and returns
`{"started": ["<run-id>", ...]}`. Use it when background ticking
is disabled, or to force a tick. Background ticking (when
configured) calls the same path.

## Webhook entry point (not admin auth)

```
POST /nexaflow/webhooks/{definition}/{trigger}
```

Headers `X-NexaFlow-Timestamp` (unix seconds) and
`X-NexaFlow-Signature` (hex HMAC-SHA256 over
`"<timestamp>.<body>"`). Body up to 64 KiB; JSON bodies are also
exposed to the workflow as `body_json`. Returns 200 with a run
summary on acceptance. Failures: 400 (missing headers, oversized
body, bad signature, expired timestamp), 404 (unknown definition
or trigger), 409 (`replay_rejected`). See the user guide for the
full delivery contract.

## Node reference (abridged)

See `docs/nexaflow-node-adapters.md` for the full table. The
shapes that most often surprise authors:

- `nexaroute.control.condition`: input `value` (object), config
  `field` + `operator` (`eq`, `ne`, `gt`, `gte`, `lt`, `lte`,
  `contains`) + `expected` (string). Output `result`, `field`.
- `nexaroute.control.branch`: input `condition_result` (bool),
  config `true_branch` + `false_branch`. Output `selected_branch`.
- `nexaroute.control.merge`: input `first` + `second` (objects),
  config `conflict` (`first`, `last`, `error`). Output `merged`.
- `nexaroute.control.loop_plan`: input `items` (≤100), config
  `max_iterations` (1–100). Output `iterations` (`{index, item}`
  objects) + `truncated`.
- `nexaroute.control.approval`: no author-supplied input; the
  engine injects `decision`, `approver`, `requester`. Output
  `approved`, `approver`.
- `nexaroute.control.error_handler`: input `error_code` (+
  `message`), config `policy` (`retry`, `fallback`, `fail`).
  Output `action`, `error_code`, sanitized `message`.
- `nexaroute.llm.chat`: config `route_or_alias` (required) +
  optional `max_tokens`, `temperature`; input `messages` or
  `prompt`. Requires operator policy approval; permission
  `routing.read`. Never retried (Phase 7 hardening: a timeout on a
  billable call is ambiguous and the node sends no idempotency key).
- `nexaroute.mcp.call`: input `server_id`, `tool`, optional
  `arguments`; config `allowlist` (required, entries are bare tool
  names or `server__tool`). The tool must be in both the allowlist
  and the gateway's listing, or the call is refused before any
  backend call (`tool_forbidden`).
- `nexaroute.video.submit` / `status` / `output`: permission
  `video.manage`, policy approval required. Submit input `prompt`
  + `duration_seconds` (0–600]. Status output `job_id`, `state`,
  `terminal`.
- `nexaroute.trigger.webhook`: the trigger node re-verifies the
  signature itself; the engine verifies first and records the
  delivery. A webhook trigger node instance and its declaration
  must both carry `secret_ref`.
- `nexaroute.trigger.schedule`: declaration config exactly one of
  `interval` or `daily`.

# NexaFlow User Guide (Phase 8)

NexaFlow is workflow automation inside the NexaRoute gateway: versioned
workflow definitions composed of registry nodes, executed by a durable
runtime, operated through the admin API and the Workflows page in the
control-plane UI. This guide covers the Phase 8 shape of the feature.
The canonical architecture is `docs/nexaflow-architecture.md`; the
endpoint reference is `docs/nexaflow-api.md`; security rules are in
`docs/nexaflow-security.md`; migration notes are in
`docs/nexaflow-migration.md`.

## Status and scope

Phase 8 delivers: the runtime engine, the durable single-host store,
the admin API, the Workflows UI page, scheduled dispatch, signed
webhooks, approval pause/resume across restarts, and crash recovery
that parks ambiguous runs instead of repeating side effects.

Explicitly NOT in this phase:

- A visual canvas editor. The UI edits definition JSON (with a
  starter template, validation, and run detail). Drag-and-drop
  authoring is a later phase.
- A multi-instance PostgreSQL store. The supported topology is one
  gateway process with FileStore. Do not point two processes at the
  same store path.
- Template-to-definition conversion. Templates listed by the API are
  inert reference shapes; author runnable definitions directly.
- Hard budget enforcement. `budget_check` reports only; it never
  blocks. (A separate hard-budget proposal is unapproved.)
- Live evaluation. `eval.run` replays caller-supplied outcomes only.
- Tenant isolation beyond definition scope labels plus list filters.
  `Identity` carries no tenant; see `docs/nexaflow-security.md`.

## Enabling NexaFlow

NexaFlow is off by default. Enable it in the gateway config:

```json
{
  "nexaflow": {
    "enabled": true,
    "store_path": "nexaflow/nexaflow-store",
    "gateway_token_ref": "NEXAROUTE_CLIENT_KEY",
    "tick_interval_seconds": 60
  }
}
```

- `store_path`: relative paths resolve against the config file's
  directory (the `admin.security_store_path` pattern). Default:
  `nexaflow/nexaflow-store` under that directory.
- `gateway_token_ref`: environment variable name holding a
  client-auth token for loopback calls (`/v1/chat/completions`,
  `/v1/video/jobs`, `/mcp`). Without it, `llm.chat` nodes fail
  closed at the gateway's client-auth check. The token value is
  never stored in config, definitions, runs, or logs.
- `tick_interval_seconds`: background schedule ticks. `0` disables
  background ticking; operators can still tick manually through
  `POST /admin/api/nexaflow/scheduler/tick`.
- `gateway_base_url`: loopback address override. Default is
  `http://127.0.0.1:<listen-port>` (`https` when TLS is enabled;
  self-signed certificates fail closed at node runtime).

On startup the gateway migrates the store to the current format and
recovers interrupted runs (they park; see "Crash recovery" below).

## The definition contract in brief

A definition is a versioned, immutable snapshot (`schema_version`
`"v1"`): nodes (instances of registry nodes), edges
(instance-to-instance, optional branch label), triggers (one
declaration per trigger node; the declaration's config is the
trigger's runtime config), settings, and an optional error handler.

Data flow, in order:

1. If a node carries an explicit `input` mapping, the mapping fully
   determines its input (it replaces, not overlays, the predecessor
   merge). References look like `{"$ref": "input.payload"}`,
   `{"$ref": "nodeA.text"}`, or the shorthand `"$.nodeA.text"`.
   Inside `for_each` iterations, `loop.item` and `loop.index` are
   available.
2. Otherwise the input is the merge of direct predecessors' outputs,
   in edge-declaration order, later keys overwriting earlier ones,
   filtered to the node's declared input fields. (Unfiltered merges
   used to break strict node schemas: a manual trigger's `payload`
   key alone failed every schema-less downstream node.)
3. Iterated node outputs are arrays and never merge into a map
   input; they are reachable through mappings only.

Branching: only `nexaroute.control.branch` selects branches. A
labelled edge is valid only from a branch node and must equal one of
that node's configured `true_branch`/`false_branch` values. An
unlabelled edge from a branch node is unconditional (always taken).
A node downstream of an unselected branch edge is skipped, not
failed. Cycles are rejected at validation.

Loops: `nexaroute.control.loop_plan` plans iterations (at most 100
items; `max_iterations` truncates with `truncated: true`). A node
with `for_each: {source: "plan.iterations", max: 100}` executes once
per iteration; `loop_plan` emits iteration objects
`{index, item}`, so a worker unwraps them
(`{"$ref": "loop.item.item"}`).

Error handling: `on_error: {handler_node_id, fallback_node_id}`.
Handler and fallback nodes are out-of-band: they run only through
the error handler, never through graph scheduling, and they still
need incoming edges (documenting the association) to pass
validation. Policies: `retry` (bounded: at most 2 handler
invocations per failed node, then the run fails),
`fallback` (the fallback node's output stands in for the failed
node's output downstream; the failed steps stay in history),
`fail` (the run fails with the original error).

Settings: `run_timeout_seconds` (0 = no run-level timeout),
`max_concurrent_runs` (0 = 1), `retention_days` (0 = 90, else
7–365), `policy_approved` (recorded at publish time; without it,
policy-requiring nodes fail with `policy_required`).

## UI flow (Workflows page)

1. **New** prefills a minimal manual → transform definition. Edit
   the JSON, then **Save draft** (validates before persisting).
2. **Validate** checks the saved draft without changing it.
3. **Publish** snapshots the draft as a new immutable revision.
   Publishing supersedes the previously published revision.
   Rollback is republishing an older revision: open its version
   (API: `GET .../versions/{rev}`), save it as the draft, publish
   again. Revisions are never mutated.
4. **Execute** dispatches a run asynchronously (202 + run id) and
   polls run detail. The detail shows every step with status,
   attempts, iteration, duration, and sanitized errors.
5. **Approval**: a run in `awaiting_approval` shows a decision
   control. The approver must differ from the run requester;
   self-approval is refused. A rejection records `approved: false`
   and does NOT fail the run — branch on that flag in the workflow.
6. **Parked runs**: a run in `parked` shows a resume control with
   `retry_ambiguous`, `skip_ambiguous`, `fail_ambiguous`, or
   `continue` (only when there is no ambiguous node).
7. **Cancel** interrupts a running run; nodes that observe
   cancellation record it, and downstream nodes do not run.

If the page reports that NexaFlow is not enabled, the gateway was
started without the `nexaflow` config section (the API answers
503 until it is attached).

## API flow (curl)

Admin calls use the emergency key or an OIDC session. Server-to-
server calls with `x-admin-key` and no `Origin`/`Referer` header
need no CSRF token; browser clients must send `X-NexaRoute-CSRF`
from `GET /admin/api/csrf-token` (the UI handles this).

```bash
BASE=http://127.0.0.1:8080
KEY=... # admin key, never committed anywhere

# Save a draft (validates first).
curl -s -X POST $BASE/admin/api/nexaflow/definitions \
  -H "x-admin-key: $KEY" -H 'Content-Type: application/json' \
  --data-binary @definition.json

# Validate the saved draft, publish it, execute it.
curl -s -X POST $BASE/admin/api/nexaflow/definitions/wf-demo/validate \
  -H "x-admin-key: $KEY" -H 'Content-Type: application/json' -d '{}'
curl -s -X POST $BASE/admin/api/nexaflow/definitions/wf-demo/publish \
  -H "x-admin-key: $KEY" -H 'Content-Type: application/json' -d '{}'
curl -s -X POST $BASE/admin/api/nexaflow/definitions/wf-demo/runs \
  -H "x-admin-key: $KEY" -H 'Content-Type: application/json' \
  -d '{"input":{"value":{"a":1}}}'

# Poll the run, list runs, read the audit trail.
curl -s $BASE/admin/api/nexaflow/runs/<run-id> -H "x-admin-key: $KEY"
curl -s "$BASE/admin/api/nexaflow/definitions/wf-demo/runs" -H "x-admin-key: $KEY"
curl -s "$BASE/admin/api/nexaflow/audit?limit=50" -H "x-admin-key: $KEY"
```

The full endpoint list, including node catalog, templates,
versions, approve/resume/cancel, scheduler tick, and the webhook
entry point, is in `docs/nexaflow-api.md`.

## Webhook triggers

A webhook trigger declaration carries `secret_ref` — a reference,
never a value — and the trigger node instance carries the same
reference (Phase 1 validates both). The secret value is resolved at
delivery time through the operator's secret resolver (environment
variable name in this phase).

Deliveries go to:

```
POST /nexaflow/webhooks/{definition}/{trigger}
X-NexaFlow-Timestamp: <unix seconds>
X-NexaFlow-Signature: <hex HMAC-SHA256 of "<timestamp>.<body>">
```

Verification: HMAC over `timestamp + "." + body`, 5-minute
timestamp window, constant-time comparison, in-memory replay cache
plus a durable delivery ledger. A replay — same definition,
trigger, and signature — is rejected with `replay_rejected`,
including after a gateway restart. A redelivery is NOT a retry
mechanism: to re-run a failed webhook workflow, start a manual run
instead. Bodies over 64 KiB are rejected. This endpoint is outside
`/admin/api` on purpose: its authentication is the HMAC signature,
not the admin session.

Example (the secret lives in `HOOK_SECRET`, never in the definition):

```bash
TS=$(date +%s)
BODY='{"hello":"world"}'
SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$HOOK_SECRET" | cut -d' ' -f2)
curl -s -X POST $BASE/nexaflow/webhooks/wf-demo/wh \
  -H "X-NexaFlow-Timestamp: $TS" -H "X-NexaFlow-Signature: $SIG" \
  -H 'Content-Type: application/json' -d "$BODY"
```

## Scheduled triggers

A schedule declaration carries exactly one of `interval` (a Go
duration, e.g. `"1h"`) or `daily` (`"09:30"`, UTC). Slots are
claimed in the store before dispatch, so two ticks for the same
slot — two processes on one store, or a retried tick — dispatch at
most one run. At most one run per schedule per tick is dispatched;
overdue slots catch up one slot at a time, never bursting. The
first interval tick fires immediately. Schedule identity
(`definition:trigger`) is stable across republication, so
republishing does not reset the fire clock; renaming a trigger
instance creates a new schedule. Runs execute as the system
`system:nexaflow` principal; per-node permissions are still
enforced. Note that a tick dispatches synchronously: a scheduled
run with a long wait node holds its tick (and a manual tick
request) until that run suspends or finishes. Keep scheduled
workflows short, or drive long ones through manual runs.

## Approval and restart

Approval nodes suspend the run as `awaiting_approval` with a
`waiting` step (no finish time). The waiting step stays in history
as the pause record; the decision execution appends its own step.
Restarting the gateway does not lose the pause: the run record,
the decision, and the step history live in the store, and any
engine over the same store can approve and resume it. A node that
executed in the same batch as a suspending approval keeps its
steps and outputs; resuming never re-executes it.

## Crash recovery

On startup, `Recover` parks every `running` run whose lease has
expired (or is missing) and which the starting engine is not
executing. A leftover `running` step becomes `ambiguous`, and the
run records which node is ambiguous. Recovery never re-runs the
ambiguous node on its own. The operator chooses, per parked run:

- `continue`: only when there is no ambiguous node.
- `retry_ambiguous`: re-execute the ambiguous node from scratch.
  Use only for pure nodes (transforms, conditions).
- `skip_ambiguous`: mark it skipped; downstream nodes of a skipped
  node are skipped by propagation.
- `fail_ambiguous`: mark it failed with `ambiguous_outcome` and
  apply the error handler (or fail the run).

A side-effecting node (MCP call, video submit, LLM chat, HTTP
request) must never be retried blindly: its outcome is unknown,
and a retry may double the side effect.

## Troubleshooting

- **API answers 503**: NexaFlow is not attached. Enable the
  `nexaflow` config section and restart.
- **Run fails with `policy_required`**: the definition was
  published without `settings.policy_approved: true`, but it uses
  a policy-requiring node (`llm.chat`, `video.submit`, non-loopback
  `http.request`, policy templates). Republish with the setting.
- **Run fails with `selector_not_found`**: the `llm.chat`
  `route_or_alias` is not a virtual-endpoint public model, an
  endpoint id, a deployment id, or a deployment alias known to
  this gateway.
- **Run fails with `permission_denied` at a node**: the run's
  actor lacks that node's permission (e.g. a viewer starting a
  video workflow). Node permissions are enforced per node, not
  just at the API boundary.
- **Webhook rejected**: check the timestamp window (5 minutes),
  the exact `timestamp.body` HMAC input, and whether the same
  delivery was already processed (`replay_rejected`).
- **Schedule never fires**: check the declaration carries exactly
  one of `interval`/`daily`, that background ticking is enabled
  (`tick_interval_seconds > 0`) or tick manually, and that the
  definition is published (drafts never run).
- **A run is `parked`**: it was interrupted. Inspect the ambiguous
  node in run detail and choose a resume mode; do not restart the
  gateway expecting it to self-heal.
- **Two gateways share one store path**: unsupported in this
  phase. Each gateway needs its own store, or schedules and claims
  will collide.

## Limits that are real (not tuning advice)

- Workflow input: 64 KiB. Node inputs/outputs: per-node bounds
  (typically 64 KiB; small control nodes 4 KiB).
- `for_each`: at most 100 iterations; `loop_plan`: at most 100
  items. Steps per run: 5000. Concurrent nodes per run: 4.
  Concurrent runs per engine: 16 (plus per-definition
  `max_concurrent_runs`, default 1).
- Run history retention: per-definition `retention_days`
  (default 90, range 7–365); pruning removes terminal runs whose
  finish time is older than the cutoff.
- Webhook bodies: 64 KiB. Webhook timestamp window: 5 minutes.
- Run timeout: per-definition `run_timeout_seconds` (0 = none
  beyond node timeouts).

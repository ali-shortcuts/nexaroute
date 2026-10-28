# NexaFlow Task 8 guide (canonical integration)

This guide documents exactly what the Task 8 integration branch ships.
Every capability below matches shipped code; unsupported features are
listed under "Known limitations", not presented as available. This
branch awaits owner review and must NOT be merged without his word.

Related records: ADR 0010 (canonical architecture), ADR 0008 (#689
foundation port), `docs/nexaflow/RUNTIME.md` (durable runtime
semantics), `docs/nexaflow-node-adapters.md` (#675 node reference).

## Enablement and configuration

NexaFlow is disabled by default. The HTTP routes exist only while a
`nexaflow.Service` is attached via `Server.AttachNexaflow` (and the
public webhook route only while a `WebhookService` is attached via
`Server.AttachNexaflowWebhook` with per-definition secrets). While
unattached, the paths fall through to the static file server and
answer 404; existing gateway behaviour is unchanged (scenario 15,
proven by test: disabled + healthz intact).

Invalid configuration fails at service construction, never partial
serving: `NewService(nil, ...)`, `NewEngine` with empty options,
`NewWebhookService(nil, ...)`, `NewScheduler(nil, ...)`, and
`NewFileDeliveryLedger("")` all error. Webhook secrets come from
gateway wiring, never from workflow definitions; a definition without
a configured secret answers 404, indistinguishable from an unknown
definition.

Storage selection is a wiring decision, not a workflow field:
PostgreSQL is the durable authority for multi-instance claims; the
file store is single-host only; the memory store is test-only.

## API reference

All admin routes require admin authentication (401 otherwise) and
ride the interim permission mapping: reads `config.read`, writes
`config.write`, audit `audit.read` (to be repointed at `workflow.*`
when task7 lands it).

| Method & path | Behaviour |
|---|---|
| `GET /admin/api/nexaflow/nodes` | Registry specs: id, version, title, category, description. |
| `GET /admin/api/nexaflow/templates` | Inert templates (`published: false` always). |
| `GET /admin/api/nexaflow/definitions` | Draft list (sorted by id). |
| `POST /admin/api/nexaflow/definitions` | Save draft (validated; invalid → 400). Returns `{id}` with 201. |
| `GET /admin/api/nexaflow/definitions/{id}` | Reopen draft. |
| `POST /admin/api/nexaflow/definitions/{id}/publish` | Publish; returns `{revision, status}`. |
| `GET /admin/api/nexaflow/definitions/{id}/versions` | Revision history. |
| `GET /admin/api/nexaflow/definitions/{id}/versions/{rev}` | Immutable version inspection (pinned definition graph). |
| `POST /admin/api/nexaflow/definitions/{id}/runs` | Body `{revision?, input, idempotency_key}`. Omitted revision = latest published revision. Runs synchronously; returns the run summary. A `wait` node can block this handler (V1 behaviour). |
| `GET /admin/api/nexaflow/runs` | Ownership-filtered run list. |
| `GET /admin/api/nexaflow/runs/{id}` | Run + node executions: safe (redacted) outputs, timing, attempts, error codes. Never raw node error text, never node inputs. |
| `POST /admin/api/nexaflow/runs/{id}/approve` | Body `{node_id, decision: approved\|rejected}`. ApproveRun IS the resume — there is no resume-without-decision endpoint. Approving then re-executes the run. |
| `POST /admin/api/nexaflow/runs/{id}/cancel` | Cancel (owner/admin or run owner). |
| `GET /admin/api/nexaflow/audit` | Service audit log. |
| `POST /nexaflow/webhooks/{definition}/{trigger}` | Public, signature-authenticated. Headers `X-Nexaflow-Timestamp`, `X-Nexaflow-Signature` (HMAC-SHA256 over `timestamp + "." + body`, 5-minute window, 1 MiB body bound). Replay → 409 `webhook_replay`; other verification failures → generic 401; success → `{run_id}`. |

Error mapping: unknown → 404, idempotency/approval conflicts →
409, state conflicts → 409, validation → 400.

## Workflow example (branch workflow, from the tests)

```json
{
  "schema_version": "v1", "id": "branch-demo", "name": "branch-demo",
  "nodes": [
    {"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"},
    {"id": "chk", "node_id": "nexaroute.control.condition", "version": "v1",
     "config": {"field": "health.status", "operator": "eq", "expected": "ok"},
     "input": {"value": "$.input.payload"}},
    {"id": "br", "node_id": "nexaroute.control.branch", "version": "v1",
     "config": {"true_branch": "fast", "false_branch": "slow"},
     "input": {"condition_result": "$.chk.result"}},
    {"id": "fast", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {"lane": "fast"},
               "operations": [{"op": "set_const", "field": "done", "value": true}]}},
    {"id": "slow", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {"lane": "slow"},
               "operations": [{"op": "set_const", "field": "done", "value": true}]}}
  ],
  "edges": [
    {"id": "e1", "from": "start", "to": "chk"},
    {"id": "e2", "from": "chk", "to": "br"},
    {"id": "e3", "from": "br", "to": "fast", "branch": "fast"},
    {"id": "e4", "from": "br", "to": "slow", "branch": "slow"}
  ],
  "triggers": [{"name": "start", "kind": "manual"}]
}
```

Start it with run input `{"payload": {"health": {"status": "ok"}}}`:
the `fast` leaf succeeds, the `slow` leaf is `skipped`. With status
`down`, the branches swap. An unmapped condition→branch chain fails
closed (branch unreachable, never a wrong branch).

Input mapping rules: `{"$ref": "path"}` or `"$.path"` strings resolve
dotted paths (maps + array indices) through `{"input": runInput,
<node id>: <output>}`; other values are literals; a missing path fails
the node as `input_resolution`. Mapping keys must be declared
`InputSchema` fields; trigger and approval nodes must not declare
mappings. Approval decisions come only from the durable approval
record.

## Supported-node matrix (exact shipped set)

Registry version `v1`, 23 nodes:

- Control (8): `condition`, `branch`, `json_transform`, `loop_plan`,
  `merge`, `wait`, `approval`, `error_handler`.
- Cost (2): `usage`, `budget_check` (report-only — see below).
- Eval (1): `eval.run` (replay-only).
- LLM (1): `llm.chat`.
- MCP (2): `list`, `call`.
- Provider (2): `health`, `compat`.
- Trigger/HTTP (4): `trigger.manual`, `trigger.webhook`,
  `trigger.schedule`, `http.request`.
- Video (3): `submit`, `status`, `output`.

Plus 5 inert templates (`Published: false` always; they never
execute). `budget_check` reports `over_threshold` and blocks nothing.
Video outputs carry job ids/status/assets, never filesystem paths.

## Migration / upgrade procedure (from #691 directory stores)

`MigratePhase8Dir(ctx, srcRoot, dst, reg)` migrates a #691 directory
store (`drafts/<id>.json`, `versions/<id>/<rev>.json` as
`{definition, version}`, `runs/<id>.json`) in two phases: phase 1
parses and validates EVERYTHING before any write; any corrupt file,
unknown extension, hash mismatch, or unrepresentable run aborts with
zero writes. Rules:

- Drafts/versions must validate against the canonical registry and
  their content hash must recompute exactly.
- Revision numbers are reproduced by publishing in ascending order
  into a destination holding no revisions for that definition. Gaps,
  identical consecutive revisions (canonical Publish de-duplicates
  identical latest hashes), or final-state mismatches (only the last
  revision may be published; earlier ones must be superseded) abort
  instead of renumbering silently.
- Definitions using #691-only extensions (top-level `on_error`, any
  node `for_each`, `settings.policy_approved == true`) are REJECTED,
  never imported with the extension dropped.
- Only terminal runs (`succeeded`/`failed`/`cancelled`) migrate as
  history. In-flight (`pending`/`running`/`awaiting_approval`) and
  `parked` runs block the migration by id. Steps with
  `Iteration != 0`, non-contiguous attempts, unknown node ids, or
  `waiting`/`ambiguous` steps inside a terminal run abort.
- Legacy run inputs/outputs pass through the canonical redactor
  before storage; the report counts redacted runs.

Fidelity limits: the canonical store owns `CreatedAt`/`UpdatedAt`
(migration time, not original times); node `StartedAt`/`FinishedAt`
and run `FinishedAt` are preserved; run `StartedAt` is not carried
(the store derives it).

## Security guide

- Admin authentication in front of every admin route; ownership
  ceiling (owner/admin any run, others only their own); listing
  post-filtered; separation of duties (approver ≠ requester,
  enforced by the engine and proven over HTTP).
- Secrets are redacted at persist time for node outputs
  (`[REDACTED]` markers, output truncation markers); run detail
  additionally withholds node inputs and raw error text.
- Webhook authentication is HMAC-SHA256 with a timestamp window;
  the delivery ledger claims before `Start`, so malformed, stale,
  wrong-secret, tampered, and replayed deliveries never start a run
  — including replays after a restart (proven against the file
  ledger across a simulated restart).
- Scheduler slots are claimed in the shared ledger before starting;
  exactly one claimant per slot per ledger. A slot whose `Start`
  fails is NOT retried by later ticks (claim stands, failure
  reported); operators re-fire explicitly with a manual trigger.
- Run inputs over 64 KiB are rejected at admission. The audit log
  (capped at 10000, drops oldest 1000) records `draft_saved`,
  `published`, `run_started`, `approved`, `cancelled`,
  `budget_released`, and `engine:*` events — never secret values.
- SSRF/egress behaviour is whatever the #675 node adapters
  implement; this integration adds no new egress control and claims
  none beyond the adapters.

## Deployment notes

- Single host: file store + file delivery ledger (atomic
  `O_CREATE|O_EXCL` per SHA-256 key + fsync; survives restart,
  cross-process safe on one host).
- Multi-instance: PostgreSQL store + Postgres delivery ledger
  (`INSERT ... ON CONFLICT DO NOTHING` on
  `nexaroute_workflow_deliveries`). No multi-host claim is made for
  the file ledger.
- Restart discipline: a restarted gateway must reuse its configured
  worker id, or wait out the live 30 s lease TTL; `Recover` never
  breaks a live foreign lease. `needs_manual_action` runs are parked
  for an operator, never auto-retried.
- No scheduler HTTP endpoint exists in V1; the scheduler tick is a
  service-level operation.

## Troubleshooting

- `input_resolution`: a mapping path does not exist in the run
  context. Check the referenced node id and the predecessor's actual
  output keys; mappings never silently yield zero values.
- `loop_truncated`: a loop plan was cut by `max_iterations` below
  the item count. Raise `max_iterations` or split the input; the
  engine never iterates a partial plan.
- Approval resume fails validation: the node is policy-gated and
  its schema does not declare the decision keys — this was F-1 and
  is fixed by InputSchema projection; if seen, the running build
  predates the fix.
- Restart with a new worker id stalls: the old worker's live lease
  blocks claiming until its TTL expires; reuse the worker id.
- `approval_expired`: an approval waited past its deadline (default
  24 h where configured); the node fails and the run must be
  re-driven by an operator decision, not auto-resumed.

## Acceptance matrix (all 15 scenarios)

Evidence: `internal/nexaflow/task8_acceptance_test.go`,
`task8_acceptance2_test.go` (service level),
`internal/httpapi/admin_nexaflow_task8_test.go` (HTTP layer).

| # | Result | Evidence / note |
|---|---|---|
| 1 Create/validate/save/reopen/publish immutable version/execute via UI routes + API | PASS | Service + HTTP lifecycle tests; pinned rev1 executes after rev2 exists; invalid definition rejected at save. |
| 2 Manual trigger + idempotency-key deduplication | PASS | Same key → same run id, backend called once; same key on another definition → `ErrIdempotencyConflict`. |
| 3 Signed webhook accept; malformed/stale/replayed rejected, incl. after restart | PASS | Service + HTTP; replay after simulated restart rejected via the same file ledger. |
| 4 Durable schedule, no duplicate dispatch across two processes | PASS (file) / NOT_RUN (PG slot claim) | Two Scheduler instances + two subprocesses, one shared file ledger → exactly one start; disabled schedules never fire. PG slot-claim subcase not run (no multi-process PG harness in this session). |
| 5 LLM node through the canonical router boundary | PASS (test doubles) | `llm.chat` via the Backend port with a counting fake; policy gate parks at `waiting_for_approval` with zero backend calls; separate approver releases it. Live provider: BLOCKED (no paid calls, no credentials — by design). |
| 6 MCP allowed-tool invoke, unauthorized-tool reject, no later side effect | PASS | Allowed tool called once; `evil-tool` fails as `tool_forbidden` before the backend (0 backend calls), downstream `skipped`. |
| 7 Branches, parallel, loops, merges, subworkflows, retries, fallback, timeout, cancel | PARTIAL | Branches, parallel, loops, merges, retries, error_handler fallback mapping, registry timeout, mid-flight cancel: PASS. Subworkflows, run-level fallback routing, any-join: NOT implemented (fail-closed evidence kept as tests). Durable waits: NOT implemented (`waiting_timer` never emitted). |
| 8 Approval suspend, restart, separate approver, resume; self-approval rejected | PASS | FileStore reopen + same worker id; self-approval rejected before and after restart; rejection of an approval node is data for downstream logic (run succeeds), while policy-gate rejection fails node + run. |
| 9 Crash after dispatch, before result persistence | PASS | Dispatched effectful node with expired lease → `Recover` parks node + run as `needs_manual_action` (`Ambiguous: true`), backend call count stays 0. Never-dispatched node → re-queued and completes. |
| 10 Video submit/status/output via authenticated Video API | PASS (fixtures) | Via the Backend video port with fixtures: 1 submit, terminal status, 1 asset, no filesystem path. Live provider: BLOCKED (no paid calls). |
| 11 Permissions, tenant boundaries, cost reservations, payload limits, redaction, audit | PASS | Anonymous start rejected; guard failure fails admission closed with zero runs; reserve-once/release-once on terminal; ownership + viewer filtering; 64 KiB input limit; audit contains `run_started`; `budget_check` report-only; redaction + truncation markers proven. Tenant boundary = ownership ceiling, NOT isolation (see ADR 0010). |
| 12 UI inspection of safe per-node outputs, timing, attempts, failures, history | PASS | Service `GetRun` + HTTP run detail + Workflows page (definitions, runs, per-node inspection). Raw error text and node inputs deliberately withheld. |
| 13 Migration without silent corruption or data loss | PASS | Fabricated #691 store migrates exactly; corrupt draft, in-flight run, and `on_error` extension each abort with zero writes. |
| 14 Restart while jobs active, recovery per execution state | PASS | Claimed-never-dispatched-expired → `pending`; approval past deadline → `approval_expired`; approval without expiry stays `waiting` across a new engine. |
| 15 Disabled preserves gateway behaviour; invalid config fails safely | PASS | Unattached routes → 404 + healthz 200; nil constructors error; no partial serving. |

## Known limitations and exact external blockers

- Subworkflows, run-level fallback routing, any-join, durable waits:
  NOT implemented here. The sibling task3 branch
  (`nexaflow/task3-control-flow`, unpushed at Task 8 time) reports
  implementing them on the same canonical runtime; Task 8 did not
  port that work. Owner reconciliation required before any merge.
- Live provider, live tenant, real cluster, real multi-host PG:
  BLOCKED/NOT_RUN — no credentials, tenant, cluster, or multi-host
  PG were supplied, and no paid call was made. Only the PG
  scheduler-slot claim subcase (scenario 4) and live-provider
  subcases (scenarios 5, 10) are affected; all deterministic parts
  are proven locally.
- Browser acceptance via the repo's loopback suite: NOT_RUN if the
  sandbox Chromium loopback block applies (see final verification
  record); the UI contract is proven by served-asset tests and
  `node --check` instead. Never presented as browser evidence.
- Sibling reconciliation (must happen before any merge): this
  branch vs `supervisor/nexaflow-integration` (sibling integration
  line, unpushed) and in-flight task3–task7 work. Overlapping Task 8
  files are to be dropped in favour of the sibling layers if they
  land first. This branch was built self-contained from main
  `e6c3cdf3` and touches no sibling tree or branch. Two sibling
  completions reported after Task 8's design freeze are relevant
  here (sibling-reported, NOT Task 8-verified): task3
  (`nexaflow/task3-control-flow`, `84cf62c`) for subworkflows /
  any-join / durable waits, and task7 (`nexaflow/task7-security`,
  `ee539bf`) for `workflow.*` permissions, tenant scoping, stored-run
  redaction, and an opt-in hard budget — both built on other
  lineages and both, by their own porting notes, must be PORTED onto
  the canonical engine, not merged as-is.

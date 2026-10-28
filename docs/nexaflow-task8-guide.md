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
| 4 Durable schedule, no duplicate dispatch across two processes | PASS (file cross-process; PG slot claim) / NOT_RUN (PG cross-process) | Two Scheduler instances + two subprocesses, one shared file ledger → exactly one start; two scheduler handles on one Postgres ledger, same slot + re-tick → exactly one start; disabled schedules never fire. PG cross-process (two OS processes, one PG ledger) not run — same claim SQL as the proven two-handle case, but not executed across processes. |
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
  cross-process subcase (scenario 4) and live-provider subcases
  (scenarios 5, 10) are affected; all deterministic parts are
  proven locally.
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
  any-join / durable waits, task4
  (`supervisor/nexaflow-task4-api-scheduler-webhooks`,
  `d01b718`/`d4e9790`/`6410036`) for a fuller admin API (202
  executions, resume endpoint, diagnostics), daily schedules, and
  webhook rate limiting, task5
  (`supervisor/nexaflow-task5-integrations`, `52b40a7`) for the
  composite backend and ledger-backed budget nodes, and task7
  (`nexaflow/task7-security`, `ee539bf`) for `workflow.*`
  permissions, tenant scoping, stored-run redaction, and an opt-in
  hard budget — all built on other lineages and all, by their own
  porting notes, must be PORTED onto the canonical engine, not
  merged as-is. Where task4's API shape differs from this
  branch's (202 vs synchronous runs, resume endpoint, extra
  routes), the owner must choose one shape at reconciliation.

## Task 6 verification addendum (2026-10-28, #693 as-is)

Task 6 (final acceptance) verified this branch WITHOUT porting any
sibling line, per the owner serialization decision (stand-downs
11:55/11:56 +0430): Task 3 control flow, Task 4 API, Task 5
integrations, and Task 7 security are NOT implemented here and are
NOT claimed below. They land onto this branch when ready; the
landing-dependencies table at the end names each source. The interim
permission mapping (`config.read`/`config.write`/`audit.read`)
is unchanged by Task 6. All Task 6 commits are local-only on
`supervisor/nexaflow-task8-integration`; nothing was merged,
pushed, tagged, or released by Task 6.

### Gosec triage (12 baseline-new findings from Task 8, resolved)

Local gosec (Go 1.27.0, the CI pin) reproduced exactly the 12
findings, with an empty "Golang errors" set (analysis complete).
Code fixes (7 sites, 6 findings cleared):

- G104 x4 — unchecked errors on discard/dominated paths, made
  explicit with `_ =` plus a comment each: `file_store.go`
  `releaseFileLock` on Open error paths and `tmp.Close()` on
  write/sync error paths (the final Close stays checked),
  `filelock_unix.go` `f.Close()` after a failed flock,
  `postgres_store.go` `db.Close()` on ping/migrate failure.
- G115 x2 — `uint64(lastRev)` / `uint64(rev)` in
  `postgres_store.go`: both sites now reject a negative stored
  revision with a "corrupt negative revision" error instead of
  wrapping it. Regression tests insert a negative revision row
  and require both `Publish` and `ListRevisions` to fail closed
  (`TestTask6PostgresNegativeRevisionFailsClosed`).
- G202 x1 — `ListRuns` LIMIT placeholder: `fmt.Sprintf` replaced
  with `strconv.Itoa` concatenation (placeholder math unchanged
  and correct: the argument is appended first, so `$len(args)`
  is its index). The finding persists because gosec flags any
  SQL string concatenation; it is baseline-accepted below as
  demonstrably safe (see below).

Baseline additions (6, via the sanctioned
`scripts/gosec-baseline.py write`, baseline 87 -> 93, check
passes with 0 resolved):

- G202 `postgres_store.go` LIMIT line — the only interpolated
  value is a digits-only placeholder index from `strconv.Itoa`;
  every user value is a bound parameter, never interpolated.
- G304 `file_store.go` `os.Open(dir)` / `os.ReadFile(path)` —
  single-file store; the path is operator configuration, and no
  request data (definition ids are map keys, never path
  components) reaches it.
- G304 `filelock_unix.go` `os.OpenFile(path, ...)` — the path is
  the operator store path plus `.lock`.
- G304 `task8_delivery.go` `os.OpenFile(path, ...)` — the
  filename is `hex(sha256(key)) + ".claimed"`, which cannot
  contain a separator; the directory is operator configuration.
- G304 `task8_migrate.go` `os.ReadFile(path)` — the operator-run
  migration walk of an operator-supplied directory (no
  request-driven callers in the tree); migrated definitions are
  validated before use.

No finding was hidden by regenerating the baseline blindly: the
write diff is exactly these 6 fingerprints, and every other
finding was fixed in code first.

### PostgreSQL cross-process acceptance (new, Task 6)

Disposable database `nexaflow_task6_test` (never another line's
database, never production data), socket peer auth as root.
New tests in `internal/nexaflow/task6_final_test.go`, all using
unique keys/ids per run plus cleanup:

- `TestTask6PGCrossProcessDelivery` — two SEPARATE test-binary
  processes, run concurrently, claim the same Postgres delivery
  key: exactly one wins.
- `TestTask6PGCrossProcessSchedulerSlot` — two separate
  processes, run concurrently, tick the same scheduler slot
  through one Postgres ledger: exactly one starts.
- `TestTask6PGCrossProcessLeaseRecovery` — a child process
  claims a node lease and is SIGKILLed; a claim while the lease
  is live fails, a claim after the 5 s TTL succeeds, and the
  recovery leases the same attempt (attempt count stays 1, no
  duplicate attempt).
- Scenario 4 of the matrix above is therefore upgraded for the
  Task 6 tree: PG cross-process (two OS processes, one PG
  ledger) is now PROVEN for delivery + scheduler claims, not
  NOT_RUN. What is still NOT proven is gateway-process
  scheduling: this branch wires no NexaFlow into
  `cmd/gateway` (no `nexaflow` references in `main.go`) and has
  no scheduler HTTP endpoint, so two gateway processes cannot
  exercise scheduling on this branch. That wiring arrives with
  the Task 4 landing; it is not faked here.

### Migration safety (new, Task 6)

- `TestTask6PostgresMigrationRerun` — sequential and 4-way
  concurrent re-opens of the Postgres store succeed and
  preserve a saved draft and run (the migration is
  `CREATE TABLE IF NOT EXISTS` inside one advisory-lock
  transaction). Note honestly: this migration is versionless —
  there is no migration version/registry table on this branch;
  a versioned checksummed registry is reported on the sibling
  final-integration line and is a landing reconciliation item,
  not a claim made here.
- `TestTask6PostgresMigrationFailureRollback` — in an isolated
  fresh database, a login role that can connect but cannot
  CREATE fails migration with a permission error and leaves
  zero `nexaroute_workflow_*` tables behind (transactional
  rollback); after granting CREATE in that database, the same
  open succeeds and saves a draft.
- `TestTask6FileStoreCorruptFailsClosed` — opening a corrupt
  file store errors, leaves the file byte-identical, and a
  repaired file opens afterwards.
- Pre-existing test-isolation fix (Task 6, not a behaviour
  change): `TestTask8PostgresSchedulerSlot` used a fixed
  definition id with an hour-bucketed slot key and no cleanup,
  so any second run within the same hour against a shared
  database failed even though the scheduler was correct (the
  same incident shape the sibling Task 2 line reported). It now
  uses a unique definition id per run plus cleanup, with the
  same exactly-one-start assertion; the full nexaflow package
  passes on consecutive runs after the fix.

### Mandatory 12-scenario verdicts against #693 as-is

Mapping to the 15-scenario matrix above; PASS means proven by a
named test on this branch, PARTIAL means the feature is absent
and the named sibling line supplies it on landing.

1. Authoring (create/edit/validate/save/reload/publish/inspect)
   — PASS (`TestTask8Scenario1_DraftPublishExecute`,
   `TestTask8HTTPDefinitionRunLifecycle`).
2. Template instantiation with valid nodes/connections —
   PARTIAL: templates are listed but inert (`Published: false`,
   never execute) on this branch. Instantiation arrives with
   Task 5 (`supervisor/nexaflow-task5-integrations`,
   `52b40a7`, 7 instantiable templates) and the studio
   Use-template flow (`nexaflow/task6-studio`, `2a1ef04`).
3. Manual trigger, signed webhook, durable schedule — PASS
   (`TestTask8Scenario2_Idempotency`,
   `TestTask8Scenario3_Webhook`,
   `TestTask8Scenario4_SchedulerSingleProcess` plus the new
   Task 6 PG cross-process tests above).
4. LLM, provider-health, MCP, evaluation, video adapters —
   PASS through test doubles/fakes only
   (`TestTask8Scenario5_LLMNode`, `TestTask8Scenario6_MCP`,
   `TestTask8Scenario10_Video`, provider/eval node tests);
   live providers BLOCKED (no credentials, no paid calls).
5. Branches, parallel, loops, merges, subworkflows, retries,
   fallback, cancellation — PARTIAL: branches, parallel,
   loops (plan), merges, retries, `error_handler` fallback
   mapping, timeout, cancel PASS; subworkflows, run-level
   fallback routing, any-join, durable waits NOT implemented
   (fail-closed tests kept) — Task 3
   (`nexaflow/task3-control-flow`, `84cf62c`).
6. Approval pause, restart, separate approver, safe resume —
   PASS (`TestTask8Scenario8_ApprovalRestart`,
   `TestTask8Scenario8_ApprovalRejected`,
   `TestTask8HTTPApprovalSeparationAndCancel`).
7. Crash recovery without blindly repeating ambiguous effects
   — PASS at lease level (`TestTask8Scenario9_CrashAmbiguity`,
   `TestTask8Scenario14_RecoveryMatrix`, plus the new Task 6
   SIGKILL lease-recovery test).
8. Cross-tenant prevention, unauthorized tool prevention —
   PARTIAL: ownership ceiling + `tool_forbidden` with zero
   backend calls PASS (`TestTask8Scenario11_Boundaries`);
   complete tenant isolation NOT implemented — Task 7
   (`nexaflow/task7-security`, `ee539bf`). Live OIDC
   tenant-claim wiring BLOCKED (needs a real tenant + OIDC
   issuer; deterministic local tests only).
9. Shared hard-budget reservations under concurrency —
   NOT IMPLEMENTED on this branch: the guard is admission
   reserve only and `budget_check` is report-only (see ADR
   0010). A PostgreSQL-atomic shared settlement must be
   reconciled at landing from Task 7's opt-in enforcer
   (single-process) and Task 5's ledger-backed budget nodes;
   report-only checks are never presented as enforcement.
10. Run history + node timelines in the actual UI — PASS via
    served-asset test (`TestTask8HTTPWorkflowsPagePresent`:
    Workflows page + `workflowsView` + per-node inspection
    from the canonical API). In-browser proof is NOT_RUN
    unless the browser gate below covers it; the repo browser
    suite does not target the Workflows page specifically.
11. Data preserved through migrations; invalid/interrupted
    migrations handled safely — PASS for the #691 directory
    migration (`TestTask8Scenario13_Migration`) plus the new
    Task 6 PG migration tests above. PG migration is
    versionless (see above).
12. NexaFlow disabled preserves existing behaviour — PASS
    (`TestTask8Scenario15_ServiceFailsClosed`,
    `TestTask8HTTPDisabledPreservesGateway`, plus the full
    gateway behaviour suites in `scripts/verify.sh`).

Task 6 therefore does NOT declare NexaFlow complete: the
essential authoring/publication/execution/history/approval/
cancellation/recovery/authorization/supported-node behaviours
work, but scenario 2 (instantiation), 5 (subworkflows/
any-join/durable waits/run-level fallback), 8 (tenant
isolation), and 9 (shared hard-budget settlement) are
incomplete by the directive's own bar, and the fuller API/
permissions surface (scenario: Task 4 API shape, `workflow.*`
permissions) is pending. Each names its landing branch below.

### Landing dependencies (Task 6 changes nothing here)

| Landing item | Source branch (committed, tree-clean) | What it supplies |
|---|---|---|
| Control flow | `nexaflow/task3-control-flow` @ `84cf62c` (merging in `~/workspace/nexaflow-task2`; merged result `supervisor/nexaflow-task2-durable-control-flow` @ `6a83c5e`, local only) | Subworkflows, any-join, durable waits, run-level fallback, loops, retries, resume/OnError |
| Fuller API/scheduler/webhooks | `supervisor/nexaflow-task4-api-scheduler-webhooks` @ `6410036` | 202 executions, resume endpoint, diagnostics, daily schedules, webhook rate limiting, `ClaimSchedule`/`ClaimWebhook`/audit store extensions |
| Integrations | `supervisor/nexaflow-task5-integrations` @ `52b40a7` | CompositeBackend, ledger-backed budget nodes, 7 instantiable templates |
| Security | `nexaflow/task7-security` @ `ee539bf` | `workflow.*` permissions, tenant scoping, stored-run redaction, opt-in hard budget, hash-chained audit |
| Studio canvas | `nexaflow/task6-studio` @ `2a1ef04` (PR #692, on #690) | Template gallery + Use-template, canvas (repoint to canonical API at landing; NOT ported by Task 6) |

Two further integration lines exist and their landing order
versus #693 is an owner decision Task 6 does not resolve:
`supervisor/nexaflow-final-integration` (master-task line:
Task 1 plan + Task 2 ports, including any-join and a versioned
PG migration registry) and
`supervisor/nexaflow-task2-durable-control-flow` (Task 3 merged
onto Phase 2). Task 6 updates #693 only and touches no sibling
tree or branch.

### Conflict note (for the owner, not resolved by Task 6)

The original Task 6 directive instructed implementing the
missing features (subworkflows, any-join, durable waits,
tenant isolation, budget settlement, fuller API) inside Task 6.
The owner serialization (master-task record 11:51 +0430 plus
stand-downs 11:55/11:56 +0430) assigns those features to the
sibling lines above, to land onto this branch when ready,
never reimplemented in parallel. Task 6 followed the
serialization: the missing features are reported PARTIAL with
their landing branches, not implemented here. If the owner
wants Task 6 to override serialization and port anyway, that
is a new explicit redirect, not assumed.

### Task 6 gates addendum, part 2: race timeout and browser

#### Race-timeout reproduction: 3m limit KEPT (evidence below)

`scripts/verify.sh` runs `go test -race -timeout=3m -count=1
./...`. The httpapi package exceeds that limit on this shared
VM, on both trees, with the identical signature:

- Task 6 branch (`supervisor/nexaflow-task8-integration` +
  Task 6 commits): `go test -race -timeout=3m -count=1
  ./internal/httpapi/` → FAIL 180.961 s, `panic: test timed
  out after 3m0s`, running test at the alarm
  `TestOIDCRejectsInvalidTokensAndTamperedResponses` (2 s).
  The dump is dominated by background telemetry flush loops
  (`chan receive`), not by a stuck NexaFlow test: this is a
  package-level wall-clock timeout, not a hanging test.
- Pristine main (`e6c3cdf3`, detached worktree, no NexaFlow
  code at all), same command → FAIL 180.957 s, same running
  test at the alarm. The failure is therefore NOT a #693
  regression and NOT a NexaFlow defect.
- Context: non-race full httpapi on this VM took 204–219 s in
  earlier sessions (already over 3 m without race
  instrumentation, which multiplies CPU cost); CI `verify`
  passed in 6 m 51 s on #693, so the race httpapi package
  fits in 3 m on CI hardware.

DECISION: the 3 m timeout is KEPT unchanged. It is
appropriate for CI, the gate's intended environment, and the
local failure is shared-VM wall-clock, not an inappropriate
limit. No test assertion was changed for timing. NexaFlow's
own tests are race-clean on this VM: the nexaflow package
passes under `-race`, and the httpapi Task 8 tests (6) plus
the OpenAPI contract suites
(`TestAdminOpenAPISpecMatchesRegisteredRoutes`,
`TestAdminOpenAPISpecWithinPermissionMatrix`,
`TestAdminOpenAPIMethodSetsMatchHandlers`,
`TestContractMatrixDrift`) pass under `-race` in 29.249 s.
The full `scripts/verify.sh` outcome on the final tree is
recorded separately below once it runs (its race phase is
expected to hit this same local limit; CI's green `verify`
is supporting evidence, not a substitute).

#### Browser acceptance: provisioned and PASS (scoped honestly)

No system Chromium exists here, but the Playwright browser
cache does (`chromium-1187` + headless shell) and Python
Playwright imports. Task 6 provisioned a real browser in a
safe disposable way only: a shim at
`~/workspace/.tmp/bin/chromium` (NOT a repo file, NOT a
system install) that execs the cached Chromium 140.0.7339.16
with `--no-sandbox` added, because the repo harness launches
Chromium without that flag and Chromium refuses to run as
root without it. With that shim on PATH:

- `python3 scripts/test-browser-e2e.py` → EXIT 0,
  `BROWSER E2E PASS`, 26 screenshots (24 viewport + 2
  full-page badge proofs), 3 m 46 s, against the real
  gateway built from this tree (`go run ./cmd/gateway`).

Scope honesty: that suite covers the control plane
generally (it predates NexaFlow) and does not target the
Workflows page specifically, so scenario 10 stays PASS via
the served-asset test, not via this browser run. The
remaining two browser suites in `verify.sh`
(`test-browser-e2e-502.py`, `test-live-visual-agent.py`)
are covered only by the full `verify.sh` run below.
`verify.sh` itself gates its browser phase on
`command -v chromium`; with the shim dir on PATH that gate
is satisfied by the same real browser.

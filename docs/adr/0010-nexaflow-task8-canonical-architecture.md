# ADR 0010: NexaFlow Task 8 canonical architecture (final end-to-end acceptance)

Status: Proposed (Task 8 integration branch; awaits owner review; NOT merged).

Numbering note: this branch already carries ADR 0008 (the #689 foundation
port). The sibling integration line (`supervisor/nexaflow-integration`,
unpushed at Task 8 time) also drafted an ADR 0009. To avoid a filename
collision at rebase time, this record is numbered 0010. If the sibling
0009 does not land, the owner may renumber this file to 0009.

## Context

Five NexaFlow lineages existed before Task 8, each with its own engine,
store, or API shape:

- #675 `feat/nexaflow-node-adapters` — the node registry (23 nodes, 5
  inert templates, phase-7 hardening).
- #688 `nexaflow/phase5-integration` — a sequential in-memory DAG
  engine, no PG, no scheduler, no webhook, no UI.
- #689 `feat/nexaflow-runtime-phase2` — a durable engine with
  Memory/File/Postgres stores, approval/policy gates, and Recover.
- #690 `feat/nexaflow-visual-studio` — a visual editor plus
  `/admin/api/nexaflow/*` plus its own engine, no PG history.
- #691 `supervisor/nexaflow-phase8` — engine + file/memory store +
  scheduler + signed webhooks + admin API + UI, tenant PARTIAL, run
  inputs unredacted, no PG multi-instance.

Task 8 integrates exactly one of each layer and retires the rest. The
master-mission binding decisions apply: #689's durable engine is the
preferred baseline, critically reviewed, not adopted blindly; #675
nodes are retained; #688 adapters are ported without its competing
in-memory runtime model; #691 scheduler/webhook/API shapes are adapted
without a second engine; #690's canvas is repointed to the canonical
API (its competing engine is not landed).

## Decision

### ONE schema

#689 `Definition` (`SchemaVersion: "v1"`): node instances, edges with
`Branch`, triggers, settings. Draft → publish → superseded. Runs pin
the full published snapshot; rollback means publishing an older
snapshot as a new revision. Revision numbers are never renumbered by
migration (see below).

### ONE registry

#675 at `6bb525e`, package `internal/nexaflow`, version `v1` — 23
nodes (see `docs/nexaflow-task8-guide.md` for the exact matrix) plus 5
inert templates (`Published: false` always; templates never execute).

### ONE engine

#689's durable DAG engine (`engine.go`, `Store.TryClaim` leases,
worker pool, conditional transitions, `Recover` with
`needs_manual_action` parking). No other engine is shipped.

### ONE state semantics

Run states: `created → running ↔ waiting_for_approval →
succeeded|failed|cancelled|needs_manual_action` (`waiting_timer` is
reserved and never emitted by this engine). Node states: `pending →
claimed → running ↔ waiting → succeeded|failed|skipped|cancelled|
needs_manual_action`. Attempt rows are append-only. Ambiguity rule: an
external outcome that cannot be proven is parked as
`needs_manual_action`, never auto-retried, never reported as success.

### ONE API

Route shapes adapted from #691, DTO field names preserved from #690
where they do not conflict. Full route table:
`docs/nexaflow-task8-guide.md`. The HTTP layer is a thin adapter over
`nexaflow.Service`; it invents no second execution path.

### Storage

PostgreSQL is the durable authority for multi-instance claims
(tables `nexaroute_workflow_definitions`, `_revisions`, `_runs`,
`_nodes`, `_approvals`, plus Task 8's
`nexaroute_workflow_deliveries`). The file store is single-host only
(process file lock, atomic temp+rename+fsync). The memory store is
test-only. The delivery ledger has three implementations with the same
contract (claim once, never released): memory (tests), file (single
host), Postgres (multi-instance).

### Tenancy V1 — ownership ceiling, NOT isolation

Task 8 does not claim multi-tenant isolation. The service enforces
authentication, separation of duties (approver ≠ requester), and
ownership (owner/admin may access any run; anyone else only their own
runs; listing is post-filtered). Cross-tenant execution, per-tenant
namespaces, and tenant admin roles are explicitly out of V1.

### Budgets V1 — admission reserve only

`budget_check` remains report-only and is never wired as enforcement.
Task 8 adds an admission reserve gate (`BudgetGuard`): a run is
admitted only if its reservation succeeds; the reservation is released
on terminal states (via the engine `run_finished` audit event and via
`ReconcileBudget` for cancel/force-fail paths). `needs_manual_action`
is NOT terminal, so its reservation is held until an operator
force-fails/cancels and reconciliation runs. Full PG atomic settle is
future work and is not claimed.

### Input mappings (adopted contract)

Task 8 adopts #691's `DefinitionNode.Input` contract (same JSON shape,
so #691 drafts migrate losslessly): mapping values are `{"$ref":
"path"}` or `"$.path"` strings resolved through the context
`{"input": runInput, <node instance id>: <output>}`; all other values
are literals (maps/arrays resolve recursively); a missing path fails
the node as `input_resolution`, never a silent zero value. Mapped
nodes receive exactly their mapped keys (projection). Mapping keys
must be declared `InputSchema` fields; trigger nodes and approval
nodes must not declare mappings.

## Retired duplicates (none ported, none shipped)

- #688 sequential in-memory engine and #688 `MemoryRunStore` as a
  production store (the canonical `MemoryStore` is test-only).
- #691 `executor.go` and #691 `Run`/`Step` types where they differ
  from the canonical types.
- #690 `runtime.go` engine and #690 JSON file store.
- The paused runtime-slice sketch (a parallel registry sketch; never
  to land as a parallel package).
- #691's unredacted run-input storage: migration passes legacy
  inputs/outputs through the canonical redactor before storage, and
  the report counts redacted runs. Note the remaining gap: the
  canonical engine stores live run inputs as given; the HTTP run
  detail therefore never returns node inputs or raw node error text
  (only redacted outputs, timing, attempts, error codes).

## Fixes landed by Task 8 (with regression tests)

- **F-1 (policy gates could park but never release):** the resume
  overlay (`decision`/`approver`/`requester`) previously poisoned
  policy-gated nodes (`llm.chat`, `video.submit`) whose schemas do
  not declare those keys, so every approved resume failed
  validation. The overlay is now projected onto the node's declared
  `InputSchema` (nil spec keeps the legacy fallback). Approval nodes
  declare all three keys, so their behaviour is unchanged.
- **F-2 (approval decision injection):** an unmapped approval node
  previously started from the predecessor merge, so a transform
  literal could smuggle a decision past the durable approval record.
  Unmapped approval nodes now start from an empty input, and
  mappings on approval nodes are rejected at validation. Approval
  decisions come only from the record.
- **Loop rule:** a `loop_plan` node whose output has
  `truncated: true` fails as `loop_truncated` (non-retryable), never
  iterates partially. (>100 items is a node-level
  `validation_error`; `truncated` arises when `max_iterations <
  len(items) ≤ 100`.)
- **Lease discipline (test correction, not a code change):** a
  restarted gateway must reuse its configured worker id; restarting
  with a new worker id waits out the live 30 s lease TTL. `Recover`
  never breaks a live foreign lease.

## Cross-engine findings — resolution

- **Finding 1 (InputSchema projection on merged inputs): CLOSED for
  mapped nodes.** The canonical engine previously merged direct
  predecessors' outputs flat, last-wins, with no projection, while
  `Registry.Execute` rejects unknown input fields fail-closed. Mapped
  nodes now receive exactly their mapped keys.
- **Finding 2 (condition→branch key mismatch): CLOSED for mapped
  nodes, documented limitation for unmapped nodes.** Condition
  outputs `{result, field}` while branch requires
  `{condition_result}`; an unmapped condition→branch chain fails
  validation on every run (branch unreachable, fail-closed, never a
  wrong branch — proven by a failing test, kept as a negative test).
  Mapped chains execute both branches correctly.

## Explicit non-goals / limitations (not presented as available)

- Subworkflows, run-level fallback routing after a failed node,
  any-join merge policy, and durable waits (`waiting_timer`
  emission, persisted timer rows) are NOT implemented in this
  engine. They are PARTIAL in the acceptance matrix with fail-closed
  evidence. Note for reconciliation: the sibling task3 branch
  (`nexaflow/task3-control-flow`, unpushed at Task 8 time) reports
  implementing these on the same canonical runtime; Task 8 did NOT
  port that work, and the owner must reconcile the two lines before
  any merge — overlapping Task 8 files are to be dropped in favour
  of the sibling layers if they land first.
- Composite Backend wiring to real subsystems (live router, MCP
  servers, video gateway) is future work; Task 8 proves the Backend
  port contract with test doubles only. No live provider call was
  made.
- Scheduler tick has no HTTP endpoint (proven at service level;
  deferred, not invented unwired).
- Live tenant/cluster/multi-host PG acceptance is BLOCKED/NOT_RUN:
  no tenant, credentials, cluster, or multi-host PG were supplied.
  File-ledger cross-process dispatch is proven; the PG
  scheduler-slot claim subcase is NOT_RUN (see the guide).
- HTTP admin permissions ride `config.read`/`config.write`/
  `audit.read` as an interim mapping; `workflow.*` permissions do
  not exist in `internal/authz` on this branch (the sibling task7
  line reports implementing them on the Phase 8 lineage, unpushed;
  they must be ported onto this canonical engine and this mapping
  repointed when they land).
- Synchronous run execution is V1 behaviour: a `wait` node can block
  the run-creation handler.

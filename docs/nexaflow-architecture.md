# NexaFlow Architecture (canonical, Phase 8)

This document is the canonical architecture for NexaFlow as of Phase 8.
It is normative for the runtime, API, and editor phases: where an older
brief or the unapproved Phase 0 proposal disagrees with this document,
this document wins. The decision record is ADR 0008; the node table is
`docs/nexaflow-node-adapters.md`; the implementation order is
`docs/nexaflow-phase-checklist.md`.

**Status: Phase 8.** The registry, the definition contract, the
runtime engine, the durable single-host store, the admin API, and
the Workflows UI page exist (branch `supervisor/nexaflow-phase8`,
layered on PR #675 plus the Phase 1 contract, unmerged). What does
NOT exist yet is listed in §13; nothing there should be read as
"NexaFlow does it".

## 1. What NexaFlow is

NexaFlow is workflow automation inside the NexaRoute gateway: versioned
workflow definitions composed of registry nodes, executed by a durable
runtime, exposed through the admin API, and eventually edited in a
visual editor. It reuses the gateway's existing subsystems and adds no
parallel ones:

| Capability | Existing subsystem (reused, not duplicated) | NexaFlow's use |
|---|---|---|
| Model routing + chat | Gateway `/v1/*` handlers, `decision.Decide` (reorder only) | `llm.chat` via loopback `GatewayClient`, stream forced false |
| Provider health / compat | `health.Manager`, `compat.Store` | Read-only adapter nodes |
| Evaluation | `eval.Runner` (replay), `eval.Catalog` | Replay-only evaluation node; live evaluation is NOT in v1 |
| MCP | MCP gateway (JSON-RPC, one serialized session per server) | `mcp.list` / `mcp.call` over HTTP; session handling stays with the gateway |
| Video | Video HTTP API + `queue.JobStore` (TryClaim leases) | `video.submit` / `status` / `output` via HTTP; the claim protocol is the model for run claims |
| Cost | `usage.Tracker`, `usage.BudgetLedger` (float64, process-local) | Read-only snapshots; `budget_check` reports, never enforces (hard-budget enforcement is a separate proposal, unapproved) |
| Config + validation | `internal/config` (`Validate` / `ValidateStrict`) | Error-returning validation convention, followed by `ValidateDefinition` |
| Definitions store | Workflow store (`internal/nexaflow` `Store`: FileStore / MemoryStore) | Versioned definition publishing, run history, claims, audit |
| Authorization | `internal/authz` (roles, thirteen permissions) | Node-level permission checks; `workflow.read` / `workflow.write` added in the API phase (Phase 8) for the admin routes |
| Audit | Workflow store audit log (plus the gateway's admin audit, which auto-records every admin operation) | Lifecycle audit events, no payloads |
| Scheduling | Claim-based `Scheduler` over the workflow store | Slot claimed before dispatch; one run per schedule per tick; no bursting |

Two ledgers exist and must not be conflated: `internal/budget`
(int64) and `internal/usage/budget.go` (float64). NexaFlow's cost
nodes read the `usage` ledger's snapshots.

## 2. Package boundaries

| Package / area | Owns | Must NOT own |
|---|---|---|
| `internal/nexaflow` | Node registry, node adapters, definition contract + validation, templates, secret redaction, runtime engine, durable store, scheduler, webhook handling, production backend composite | HTTP routes, UI |
| `internal/httpapi` | Admin API routes + `adminPermissionForRequest` matrix entries + `AttachNexaflow` wiring (server-owned composite backend) | Runtime internals beyond the attach seam |
| Future `nexaroute_workflow_*` PG tables | Multi-instance run history, node executions, approvals, webhook events, timers | Definition blobs (those live in the workflow store) |
| Editor (Phase 2) | Canvas, layout sidecar, definition authoring UX | Execution semantics (the contract is `workflow.go`, not the canvas) |

## 3. Workflow definitions

A definition is a versioned, immutable snapshot
(`internal/nexaflow/workflow.go`):

- **Identity**: `SchemaVersion` (`"v1"`), `ID`, `Name`, optional
  `Description`, optional `TenantScope` (an admin-plane label, not
  isolation).
- **Nodes**: instances of registry nodes. Each instance has a unique
  instance ID, a registry node ID, a version (`"v1"`), and a config
  validated against that node's `ConfigSchema`. The same registry
  node may appear several times; templates cannot do this (their
  edges reference node IDs directly), which is why template-to-
  definition conversion must assign instance IDs.
- **Edges**: instance-to-instance, with an optional branch label.
  No ports in v1: an edge carries the source's whole output map.
- **Triggers**: one declaration per trigger node, naming the node by
  instance ID; the declaration's config is the node's runtime config
  (`secret_ref` for webhooks, `interval` or `daily` for schedules).
- **Settings**: run timeout (0 = none beyond node timeouts),
  max concurrent runs (0 = 1), retention days (0 = 90, else 7–365).
- **Validation** (`ValidateDefinition`, pure, non-mutating):
  unknown node/version, unknown config key, dangling edge,
  self-edge, duplicate edge, branch label from a non-branch node or
  not matching the branch node's configured names, trigger node
  with an incoming edge, non-trigger node without one, unreachable
  node, any cycle, trigger/trigger-node mismatch, and bad settings
  are all rejected fail-closed.

Data flow: a node's input is the merge of its direct predecessors'
outputs, in edge-declaration order, later keys overwriting earlier
ones, filtered to the node's declared input fields (unfiltered
merges broke strict node schemas). `control.merge` overrides with
an explicit policy (first / last / error, at most 1000 keys).
Phase 8 extension (backward-compatible, additive fields on the
Phase 1 contract): an explicit per-node `input` mapping, when
present, fully determines the input and replaces the merge. A
`for_each` spec adds bounded iteration, and `on_error` wires the
error handler. These three extensions are why Phase 8 definitions
validate under `ValidateDefinitionExtended`, not bare Phase 1
validation.

## 4. Versioning

- Versions are generations (`"v1"`), not semver.
- A definition version is `{DefinitionID, Revision, ContentHash,
  Status}`; the hash is sha256 over the definition's JSON encoding
  (comparable only within this Go implementation).
- Lifecycle: `draft -> published -> superseded`, superseded terminal.
  Drafts are never superseded; rollback republishes an older snapshot
  as a NEW revision. Revisions are never mutated.
- Publishing assigns the next revision number and supersedes the
  previously published revision, under the store's serialization.
  A publish that races another publish for the same definition
  serializes (single-host store lock); there is no CAS
  expected-revision check in this phase.

## 5. Execution states (as implemented in Phase 8)

Run: `pending | running | awaiting_approval | succeeded |
failed | cancelled | parked`. Step: `pending | running |
succeeded | failed | skipped | cancelled | waiting | ambiguous`.

- A node runs when all active predecessors succeeded (AND-join);
  a failed predecessor with a fallback-substituted output counts
  as satisfied for its downstream nodes.
- A node whose every incoming edge is inactive (predecessor
  skipped, or unselected branch label) is `skipped`.
- A node downstream of an unselected branch edge is `skipped`.
- A run with an unhandled failed node ends `failed`; a run whose
  failures were all handled (fallback) or skipped ends `succeeded`
  with the failed steps kept in history.
- `parked` is the Phase 1 `needs_manual_action` state under its
  implemented name: the resumable parking state for ambiguous
  outcomes (see §7) and for recovery findings the runtime cannot
  resolve alone. It is never a silent retry and never a silent
  success. There is no `waiting_timer` state: durable timers are
  schedule slots, not sleeping runs.

## 6. Durable persistence (as implemented in Phase 8)

- **Definitions, versions, runs, claims, deliveries, schedule
  state, audit**: the workflow `Store` interface, implemented by
  FileStore (single-host durable authority: atomic temp-file +
  rename + fsync writes, 0700 directories, 0600 files, `O_EXCL`
  claim/delivery files) and MemoryStore (tests only; everything
  lost on exit). The claim protocol follows the video queue's
  proven shape: leases with TTL 30s, same-owner renewal, an
  append-only step history (shrinking it is rejected), and
  ambiguous jobs parked as `parked`.
- **Execution does NOT require PostgreSQL in Phase 8.** The
  supported topology is a single gateway process. A multi-instance
  PostgreSQL store is a later phase; FileStore does not claim
  cross-process safety. The event bus (volatile), the eval store
  (in-memory), and usage (per-process) are not run history and
  must never be presented as such.
- **Format versions**: the store is versioned (current: 2).
  `Migrate` fills derived fields (content hashes, run version
  counters) without touching content or history; see
  `docs/nexaflow-migration.md`.
- **Retention**: 90 days default, 7–365 configurable (definition
  setting). `Prune` removes terminal runs finished before the
  cutoff, per definition or globally.

## 7. Retries, cancellation, side effects

**Retries** (runtime rules; the adapter package never retries):
- Only codes in the node's `RetryableCodes`, bounded by
  `MaxAttempts` (1..3) and by deadline.
- `none`/`read`: retry freely within policy.
- `external`: retry only when the error proves nothing was sent
  (dial / DNS / credential-exhaustion class).
- `billable`: retry only on timeout-class errors before commit
  evidence, never after an ambiguous outcome.
- Ambiguous outcome (sent, result unknown — including a gateway-
  deadline timeout, which stays terminal 504 with no replay):
  park as `needs_manual_action`. Never auto-retry: a retry could
  double-bill or double-send.
- The gateway never generates a provider key and never forwards a
  client key by default (operator `forward_headers` listing only,
  as an operator assertion); static provider idempotency headers
  are rejected at config validation.

**Cancellation**: context propagation end-to-end; a cancelled node
reports `cancelled`, never partial success. Caveat: cancelling the
`video.submit` call aborts the HTTP call but does NOT cancel the
underlying video job (no cancellation node in v1); the run records
the submitted job id for the operator.

**Side effects**: classes `none`, `read`, `external`, `billable`,
`irreversible`. No v1 node declares `irreversible` (contract
test). Operators treat `external`/`billable` as practically
irreversible: a sent request cannot be unsent, a billed call
cannot be unbilled — hence policy/allowlist gates and the
park-on-ambiguity rule.

## 8. Authentication, authorization, secret references

- Every node execution requires an authenticated principal
  (anonymous denied), then the node's `authz` permission ("" means
  authenticated-only), then operator-policy approval for
  policy-gated nodes (`llm.chat`, `video.submit`, `http.request`).
  Policy-gated implies side-effecting; the converse is false by
  design (`mcp.call` is external but allowlist-gated, not
  policy-gated).
- The proposal's `workflow.*` permission set is deferred to the API
  phase. Until then, workflow-level admission is: authenticated
  principal + per-node checks + operator policy. No permission is
  invented in Phase 1, and the API phase must add its matrix
  entries in the same change.
- Secrets appear only as `secretRef` references. The backend
  resolver fails closed when absent or unresolvable. Definitions,
  run records, outputs, and logs must never contain a secret value;
  redaction applies at the persistence boundary as well as at the
  adapter boundary. Prompts are never logged, stored, or echoed.
- `mcp.call` executes only if the tool is in BOTH the node
  allowlist config AND the gateway listing, else `tool_forbidden`.
- `http.request`: https, or http only for loopback hosts;
  `Authorization` is forbidden in configured headers; an optional
  `auth_secret_ref` is the only authorized credential path.

## 9. Triggers

- **Manual**: an operator starts a run with a payload; the payload
  passes through to the workflow.
- **Webhook**: HMAC-SHA256 over `timestamp + "." + body`,
  5-minute window, constant-time compare, 64 KiB engine bound,
  in-memory replay cache in the engine verifier plus a durable
  delivery ledger in the store, and an HTTP endpoint in
  `internal/httpapi` (`POST /nexaflow/webhooks/{definition}/
  {trigger}`). The trigger node re-verifies the signature itself.
  A fixed rate limit on the webhook endpoint is NOT implemented
  in this phase; operators front it with their reverse proxy.
  Note the deliberate difference from the video orchestrator,
  which MACs only the timestamp.
- **Schedule**: claim-based dispatch (`Scheduler.Tick`) over the
  store: slots claimed before dispatch, one run per schedule per
  tick, overdue catch-up one slot at a time. The engine never
  sleeps inside a run for a timer; waiting is a schedule slot, not
  a run state.
- **System triggers** (provider-health changes, usage thresholds):
  deferred; they need an outbox, which is a Phase 3 decision.
- Trigger collision (two triggers claiming the same event) is a
  definition error only where validation can see it (duplicate
  trigger names); runtime dispatch collisions are runtime work.

## 10. Approval pauses

- `control.approval` records a decision; a missing decision is
  `approval_required` (non-retryable), never a success.
- Approver != requester when both are present (rejected otherwise).
- The durable pause is implemented: the run record, the waiting
  step, and the decision all live in the store, so a decision
  survives a restart and any engine over the same store can
  approve and resume it. There is NO approval deadline in Phase 8:
  an awaiting run waits until decided, resumed, or cancelled. A
  deadline whose expiry fails the run remains future work, and
  must not be claimed until it exists.
- Approval decisions are audited (see §11).

## 11. Observability

- **Audit**: lifecycle events (run started / awaiting approval /
  approved / resumed / cancelled / parked / finished) go to the
  workflow store's audit log, which carries no payloads. The
  gateway's admin audit additionally auto-records every admin
  operation. A workflow store audit failure does not fail the
  action closed in this phase (the engine's audit sink is
  best-effort); do not claim pre-action audit gating until it
  exists.
- **Live feed**: the event bus carries live run events only; it is
  not history and must not be queried as such.
- **Traces**: OTLP spans for runs and node executions are NOT
  implemented in Phase 8.
- **Metrics**: workflow metrics are NOT implemented in Phase 8.
  When they land, labels must be aggregated only (workflow id,
  node id, outcome) — never per-run labels.
- **Redaction**: every persisted and emitted payload passes secret
  redaction; error messages are truncated and credential-keyed
  values replaced.

## 12. Rollout

- NexaFlow ships behind the default-off `nexaflow.enabled`
  config flag. With the flag off, no behavior changes: no routes,
  no timers, no runs. With it on, the admin routes, the webhook
  endpoint, and the scheduler exist; the UI page reports the
  feature as unavailable until it is attached.
- The editor (Phase 2) carries layout as a sidecar, not as part
  of the execution contract: a definition's JSON has no position
  field, deliberately.

## 13. Guarantees and limitations (summary)

Guarantees: one registry, one definition contract, one validation
path; fail-closed on every unknown; no secret value anywhere in
definitions, runs, outputs, or logs; no silent double-execution;
no silent resume; published definitions immutable.

Limitations (Phase 8): no multi-instance execution (single-host
FileStore only); no live-model evaluation (replay only); no
hard-budget enforcement (report only); no video-job cancellation;
no tenant isolation beyond scope labels + list filters; no system
triggers; no sub-workflows; no custom code node; no approval
deadlines; no workflow OTLP traces or metrics; no fixed rate limit
on the webhook endpoint; no template-to-definition conversion;
templates are inert; content hashes comparable only within this
implementation; run inputs persist unredacted (never put secrets
in workflow input — use `secret_ref`).

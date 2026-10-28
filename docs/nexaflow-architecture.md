# NexaFlow Architecture (canonical, Phase 1)

This document is the canonical architecture for NexaFlow as of Phase 1.
It is normative for the runtime, API, and editor phases: where an older
brief or the unapproved Phase 0 proposal disagrees with this document,
this document wins. The decision record is ADR 0008; the node table is
`docs/nexaflow-node-adapters.md`; the implementation order is
`docs/nexaflow-phase-checklist.md`.

**Status: Phase 1.** The registry and the definition contract exist
(branch `nexaflow/phase1-foundation`, layered on PR #675, unmerged).
There is no runtime, no HTTP API, no UI, and no execution store yet.
Nothing in this document should be read as "NexaFlow works".

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
| Definitions store | `internal/controlplane` CAS (`Get` / `Put` with expected revision) | Versioned definition publishing |
| Authorization | `internal/authz` (roles, eleven permissions) | Node-level permission checks; no new permission in Phase 1 |
| Audit | Security store append-audit | Lifecycle audit events; pre-action audit failure fails closed |
| Scheduling | None exists (no scheduler/cron package in `internal/`) | `trigger.schedule` is pure next-fire computation in v1; durable timers are runtime work |

Two ledgers exist and must not be conflated: `internal/budget`
(int64) and `internal/usage/budget.go` (float64). NexaFlow's cost
nodes read the `usage` ledger's snapshots.

## 2. Package boundaries

| Package / area | Owns | Must NOT own |
|---|---|---|
| `internal/nexaflow` | Node registry, node adapters, definition contract + validation, templates, secret redaction | Runtime, HTTP routes, stores, UI |
| Future runtime package | DAG scheduling, worker pool, claim protocol, recovery, approval/timer persistence | Node semantics (those live in the registry), HTTP routing |
| `internal/httpapi` (future change) | Admin API routes + `adminPermissionForRequest` matrix entries | Runtime internals (Server fields are unexported; wiring lands here) |
| `internal/controlplane` | Versioned definition blobs (CAS publish) | Run history, node executions |
| Future `nexaroute_workflow_*` PG tables | Run history, node executions, approvals, webhook events, timers | Definition blobs (those are control-plane) |
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
ones. `control.merge` overrides with an explicit policy
(first / last / error, at most 1000 keys). Explicit per-node input
mappings are a future extension, not v1.

## 4. Versioning

- Versions are generations (`"v1"`), not semver.
- A definition version is `{DefinitionID, Revision, ContentHash,
  Status}`; the hash is sha256 over the definition's JSON encoding
  (comparable only within this Go implementation).
- Lifecycle: `draft -> published -> superseded`, superseded terminal.
  Drafts are never superseded; rollback republishes an older snapshot
  as a NEW revision. Revisions are never mutated.
- Publishing uses the control-plane CAS pattern: publish fails
  closed on a revision conflict (the same discipline that makes a
  simultaneous first boot on an empty control plane fail closed
  rather than corrupt state).

## 5. Execution states (contract for the runtime)

Run: `created -> running <-> waiting_for_approval | waiting_timer
-> succeeded | failed | cancelled | needs_manual_action`.

Node: `pending -> claimed -> running <-> waiting -> succeeded |
failed | skipped | cancelled | needs_manual_action`.

- A node runs when all predecessors succeeded (AND-join).
- A node whose predecessor failed or was cancelled is `skipped`.
- A node downstream of an unselected branch edge is `skipped`.
- A run with any failed node ends `failed`.
- `needs_manual_action` is the terminal parking state for ambiguous
  outcomes (see §7) and for recovery findings the runtime cannot
  resolve alone. It is never a silent retry and never a silent
  success.

## 6. Durable persistence (contract, not yet schema)

- **Definitions**: control-plane CAS blobs (see §4).
- **Runs**: future PostgreSQL tables `nexaroute_workflow_*`
  (definitions are NOT stored here; runs, node executions,
  approvals, webhook events, and timers are). The claim protocol
  follows the video queue's proven shape: server-clock leases,
  same-owner renewal, a row-locked terminal guard, and ambiguous
  jobs parked as `needs_manual_action`.
- **Execution requires PostgreSQL** and fails closed without it.
  The event bus (volatile), the eval store (in-memory), and usage
  (per-process) are not run history and must never be presented
  as such.
- **Retention**: 90 days default, 7–365 configurable (definition
  setting, enforced at validation). Retention deletion is runtime
  work; until it exists, old runs accumulate and the runbook must
  say so.

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
- **Webhook**: verification only in v1 (HMAC-SHA256 over
  `timestamp + "." + body`, 5-minute window, constant-time
  compare, 1 MiB bound, in-memory replay cache in the node).
  Runtime requirements before webhooks are enabled: durable replay
  protection, a fixed rate limit, and an HTTP endpoint in
  `internal/httpapi`. The node verifier never receives HTTP
  requests itself. Note the deliberate difference from the video
  orchestrator, which MACs only the timestamp.
- **Schedule**: pure next-fire computation in v1 (never sleeps).
  Durable timers with a claim protocol are runtime work.
- **System triggers** (provider-health changes, usage thresholds):
  deferred; they need an outbox, which is a Phase 3 decision.
- Trigger collision (two triggers claiming the same event) is a
  definition error only where validation can see it (duplicate
  trigger names); runtime dispatch collisions are runtime work.

## 10. Approval pauses

- `control.approval` records a decision; a missing decision is
  `approval_required` (non-retryable), never a success.
- Approver != requester when both are present (rejected otherwise).
- The durable pause is runtime work: a persisted approval record,
  a deadline whose expiry fails the run as
  `failed(approval_expired)`, and no silent resume — a decision
  survives a restart only as its persisted record, and an expired
  wait never resumes.
- Approval decisions are audited (see §11).

## 11. Observability

- **Audit**: lifecycle events (definition published, run
  started/finished, node claimed/failed, approval decided) go
  through the security store's append-audit path. A pre-action
  audit failure fails the action closed (503); the action never
  runs unaudited.
- **Live feed**: the event bus carries live run events only; it is
  not history and must not be queried as such.
- **Traces**: OTLP spans for runs and node executions.
- **Metrics**: aggregated labels only (workflow id, node id,
  outcome) — never per-run labels.
- **Redaction**: every persisted and emitted payload passes secret
  redaction; error messages are truncated and credential-keyed
  values replaced.

## 12. Rollout

- NexaFlow ships behind a default-off admin flag (runtime/API
  phase work). With the flag off, no behavior changes: no routes,
  no timers, no runs.
- Until the runtime phase lands, the only honest status is:
  "registry + definition contract exist; nothing executes".
- The editor (Phase 2) carries layout as a sidecar, not as part
  of the execution contract: a definition's JSON has no position
  field, deliberately.

## 13. Guarantees and limitations (summary)

Guarantees: one registry, one definition contract, one validation
path; fail-closed on every unknown; no secret value anywhere in
definitions, runs, outputs, or logs; no silent double-execution;
no silent resume; published definitions immutable.

Limitations: no runtime, API, UI, or store in Phase 1; no live-
model evaluation (replay only); no hard-budget enforcement (report
only); no video-job cancellation; no multi-instance execution; no
tenant isolation; no webhook dispatch; no durable timers; no system
triggers; no sub-workflows; no loop execution; no custom code node;
content hashes comparable only within this implementation.

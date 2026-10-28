# ADR 0008: NexaFlow foundation — one registry, one definition contract

- Status: **Accepted — Phase 1 scope only** (see "Scope of this ADR").
  Decided 2026-10-28 by the supervising engineer under the owner's
  Phase 1 directive ("make the safest reasonable engineering decisions
  and document them", 2026-10-28). The Phase 0 architecture proposal
  as a whole remains **unapproved**; its recommendations are adopted
  here only where this ADR says so, and Phases 2–3 still need their
  own owner decisions.
- Date: 2026-10-28
- Numbering note: 0007 is claimed on unmerged branches (Vault Transit
  ADR, PRs #670 and #672, same path
  `docs/adr/0007-vault-transit-envelope-encryption.md`). To avoid a
  numbering/path collision, NexaFlow foundation takes 0008. If #670
  and #672 never merge, the 0007 gap is acceptable and documented.
- Context: main `e6c3cdf3` contains no NexaFlow code. PR #675
  (branch `feat/nexaflow-node-adapters`, head `f90987c`) introduces
  `internal/nexaflow` — a versioned, engine-agnostic node registry
  with 23 v1 nodes and 5 inert templates — and nothing else: no
  runtime, no HTTP API, no UI, no execution store. A Phase 0
  architecture proposal (705 lines, unapproved, decisions D1–D5
  pending) described a larger platform. Phase 1 must fix the canonical
  contract before any runtime, API, or editor work, or parallel
  registries and divergent schemas will follow.
- Related: `docs/nexaflow-architecture.md` (canonical semantics),
  `docs/nexaflow-node-adapters.md` (node table),
  `docs/nexaflow-phase-checklist.md` (remaining phases),
  `internal/nexaflow/workflow.go` (the definition contract in code).

## Scope of this ADR

This ADR decides Phase 1 only: the registry, the definition
contract, the execution semantics the runtime must implement, and
the guarantees and limitations of v1. It does NOT decide the HTTP
API shape, the workflow-level permission set, the storage schema
beyond its contract, multi-instance execution, tenant isolation, or
any Phase 2/3 item. Those are listed under "Deferred decisions".

## Decisions

### D1. One registry: `internal/nexaflow` from PR #675 is canonical

The registry introduced by PR #675 is THE node registry. No second
registry, no parallel node schema, no fork of its `NodeSpec` may be
introduced. A sibling worktree (`supervisor/nexaflow-runtime-slice`)
holds an untracked `internal/nexaflow/` sketch: that work must rebase
onto this contract, not land a parallel package. New nodes are added
to this registry, under its existing validation rules
(`NewRegistry` rejects out-of-range values; it does not clamp them).

### D2. One definition contract: `internal/nexaflow/workflow.go`

A workflow definition is a versioned, immutable snapshot:
`Definition{SchemaVersion, ID, Name, Description, TenantScope,
Nodes, Edges, Triggers, Settings}`. Nodes are instances (the same
registry node may appear several times under distinct instance
IDs); edges reference instance IDs; triggers are declarations that
name their entry node by instance ID. `ValidateDefinition` is a
pure, fail-closed function: it never mutates its input and it
validates every node config with `Registry.ValidateConfig`, so a
definition can never reference a node version or config key the
runtime would reject.

Graph rules (v1, enforced by validation):
- Nodes are single-input / single-output maps. There are no ports
  in v1; an edge carries the whole output map of its source.
- Data flow: a node's input is the merge of its direct
  predecessors' outputs, in edge-declaration order, later keys
  overwriting earlier ones on conflict. `control.merge` exists for
  explicit conflict policies (first / last / error); the default is
  last-wins.
- Branching: only `nexaroute.control.branch` selects between named
  branches. A labelled edge is valid only from a branch node, and
  its label must equal one of that node's configured `true_branch`
  / `false_branch` values. An unlabelled edge from a branch node is
  unconditional. A node downstream of an unselected branch edge is
  skipped, not failed.
- Cycles are rejected. There is no loop-execution primitive in v1
  (`control.loop_plan` only plans iterations); a bounded-loop
  construct is a later phase.
- Roots: trigger-category nodes take no input; every other node
  takes at least one edge and must be reachable from a trigger node.
  Orphan subgraphs are rejected.
- Triggers: exactly one declaration per trigger node and one
  trigger node per declaration (a bijection). The declaration's
  config IS the trigger node's runtime config, validated against
  that node's `ConfigSchema`. There is no `Enabled` flag: disabling
  a trigger is a new published revision without that trigger,
  because published definitions are immutable snapshots.

### D3. Versioning: generations, content hash, immutable revisions

- Node versions and definition schema versions are registry
  generations (`"v1"`), not semver triples. A new generation is a
  deliberate, documented break, not a patch bump.
- A definition version carries `ContentHash`: hex sha256 over the
  JSON encoding of the definition (Go `encoding/json`: fixed struct
  field order, sorted map keys). Hashes are comparable only within
  this implementation, not across languages; this is not a
  JCS-style canonicalization.
- Status lifecycle: `draft -> published -> superseded`, superseded
  terminal. Drafts are never superseded (they are simply never
  published). Rollback is republishing an older snapshot as a NEW
  revision; revisions are never mutated.

### D4. Execution states (documented contract, NOT implemented)

Run states: `created -> running <-> waiting_for_approval |
waiting_timer -> succeeded | failed | cancelled |
needs_manual_action`. Node states: `pending -> claimed -> running
<-> waiting -> succeeded | failed | skipped | cancelled |
needs_manual_action`. Rules: a node runs when all its predecessors
succeeded (AND-join); a node whose predecessor failed or was
cancelled is `skipped`; a node downstream of an unselected branch
is `skipped`; a run with any failed node ends `failed`. These
states are the contract the runtime must implement; no runtime
exists in Phase 1, so no run can be in any of these states yet.

### D5. Persistence split: definitions via CAS, runs via PostgreSQL

- Definitions are published through the control-plane CAS pattern
  (`Get(namespace)` / `Put(namespace, expectedRevision, payload)`)
  already implemented by `internal/controlplane` (file / memory /
  Redis / SQL backends). The control-plane blob store holds versioned
  definitions, NOT per-node execution state.
- Run history, node executions, approvals, webhook events, and
  timers live in future PostgreSQL tables (`nexaroute_workflow_*`),
  following the video queue's claim protocol: server-clock leases,
  same-owner renewal, row-locked terminal guard, ambiguous jobs
  parked as `needs_manual_action` (never silently retried or
  double-executed). The tables do not exist in Phase 1; this ADR
  fixes their contract, not their schema.
- Execution requires PostgreSQL and fails closed without it. The
  event bus is volatile, the eval store is in-memory, and usage is
  per-process: none of them is run history, and none may be
  presented as such.

### D6. Retries: declared by the node, never by the adapter package

`internal/nexaflow` itself never retries; `RetryPolicy` only
declares what a runtime may retry. The runtime's retry rules:
- Retry only `NodeError` codes listed in the node's
  `RetryableCodes`, bounded by `MaxAttempts` (1..3) and by deadline.
- Side-effect classes govern the rest: `none`/`read` nodes retry
  freely within policy; `external` nodes retry only when the error
  proves nothing was sent (dial/DNS/credential-exhaustion class);
  `billable` nodes retry only on timeout-class errors before any
  commit evidence, and never after an ambiguous outcome.
- An ambiguous outcome (request sent, result unknown — including a
  gateway-deadline timeout) is terminal for that node: the run
  parks as `needs_manual_action`. It is never auto-retried, because
  a retry could double-bill or double-send. Cancellation is never
  retried: a cancelled node reports `cancelled`, never partial
  success.

### D7. Cancellation: context end-to-end, with one honest caveat

Cancellation propagates through `context.Context` from run cancel
to node call. A cancelled run reports `cancelled` and never a
partial success. Caveat, documented not hidden: cancelling the
`video.submit` node call aborts the HTTP call but does NOT cancel
the underlying video job (there is no video-job cancellation node
in v1); cancelling a run that already submitted a video job leaves
that job running, and the run records the submitted job id for the
operator.

### D8. Side effects: classes with teeth

Side-effect classes: `none`, `read`, `external`, `billable`,
`irreversible`. In v1, NO node declares `irreversible` (enforced
by contract test); operators must still treat `external` and
`billable` nodes as practically irreversible — a sent HTTP request
cannot be unsent, a billed call cannot be unbilled — which is why
those nodes are policy-gated or allowlist-gated and why ambiguous
outcomes park instead of retrying. The template prose calling a
notification "irreversible once sent" describes this real-world
consequence, not a registry class; the registry class for
`http.request` remains `external`.

### D9. Authorization in Phase 1: node-level, existing permissions only

No new `authz` permission is invented in Phase 1. Every node maps
to an existing permission or to "" (authenticated principal only),
enforced by contract test against the eleven permissions in
`internal/authz`. Operator policy (`RequiresOperatorPolicy`,
exactly `llm.chat`, `video.submit`, `http.request` in v1) is checked
at `Execute` time, after authentication and permission. The
proposal's `workflow.*` permission set (read/write/publish/execute/
approve/credential_bind) is a candidate for the API phase, NOT
adopted here; the API phase must decide it explicitly and add the
`adminPermissionForRequest` matrix entries in the same change
(`httpapi` Server fields are unexported, so the API must land in
`internal/httpapi`).

### D10. Secrets: references only, resolved by the backend, never stored

`secretRef` fields hold references, never values. Definitions,
run records, outputs, and logs must never contain a secret value;
the backend resolver fails closed when absent or unresolvable.
The runtime must redact at the persistence boundary (the same
redaction `internal/nexaflow` applies to outputs and errors), and
must never log prompts, secret values, or filesystem paths.

### D11. Triggers: declarations now, dispatch later

Trigger kinds in v1: `manual`, `webhook`, `schedule`. The
declaration contract (D2) is decided; dispatch is runtime work:
- `webhook`: HMAC-SHA256 over `timestamp + "." + body`, 5-minute
  window, constant-time compare, 1 MiB bound, durable replay
  protection, and a rate limit (not yet numerically fixed — the
  runtime phase must fix it before enabling webhooks). The webhook
  verifier in `internal/nexaflow` is verification only; it does not
  receive HTTP requests.
- `schedule`: pure next-fire computation in v1
  (`trigger.schedule` never sleeps); durable timers with a claim
  protocol are runtime work.
- System triggers (provider-health changes, usage thresholds) are
  deferred; they need an outbox, which is a Phase 3 decision.

### D12. Approvals: pause, never silent resume

`control.approval` records a decision; a missing decision is
`approval_required` (non-retryable), never a success. The durable
pause is runtime work: a persisted approval record, separation of
duties (approver != requester when both are present), a deadline
whose expiry fails the run as `failed(approval_expired)`, and no
silent resume on restart — an approval decision survives a
restart only as its persisted record, and an expired wait never
resumes.

### D13. Observability: audit first, metrics aggregated

- Lifecycle events (definition published, run started/finished,
  node claimed/failed, approval decided) are audited via the
  security store's append-audit path; a pre-action audit failure
  fails the action closed (503), the action never runs unaudited.
- The event bus is a live feed only, not history.
- OTLP spans for runs and node executions; metric labels are
  aggregated (workflow id, node id, outcome) — never per-run labels.
- All persisted and emitted payloads pass secret redaction.

### D14. Tenancy and scope: label, not isolation; single-instance-durable

- V1 tenancy is an admin-plane scope label (`TenantScope`) checked
  against an operator ceiling, not a sandbox and not isolation.
  Full tenant isolation is a Phase 3 decision.
- V1 scope is single-instance-durable on PostgreSQL. Multi-instance
  execution is NOT claimed: the claim protocol is designed for it,
  but no multi-instance run has been demonstrated, and the
  architecture document says so.

### D15. Model transport in V1: loopback, as the adapter already does

The `llm.chat` node executes via `GatewayClient` loopback HTTP
(stream forced false), not via an in-process model-execution
service — because no such service exists (decision.Decide only
reorders; execution is HTTP handlers only). Extracting an
in-process service layer is a Phase 2 refactor candidate, not a
Phase 1 change. Cost attribution for loopback model calls is API/
runtime-phase work, not adapter work.

### D16. Defaults adopted from the proposal (Phase 1 decisions)

The following proposal defaults are adopted as Phase 1 decisions
(they are documented here so the runtime phase does not re-litigate
them silently; the owner may still revise them):
- Run retention: 90 days default, configurable 7–365
  (enforced by definition validation).
- Per-definition concurrency: 1 (zero means 1 in settings).
- Worker pool: 16.
- Claim lease TTL: 30s.
- Loop cap: 100 (already enforced by `control.loop_plan`'s
  `max_iterations` 1..100; loop execution itself is still a later
  phase, see D2).
- Sub-workflow depth cap: 5 (recorded for when sub-workflows
  exist; they do not exist in v1).

### D17. Custom code node: excluded until Phase 3

No custom-code node exists in v1, and none may be added until a
sandboxed out-of-process execution design is separately decided.
Workflows execute only registry nodes.

## Deviations from the Phase 0 proposal (explicit)

The proposal is not the contract; where they differ, this ADR wins:
1. Node versions are generations (`"v1"`), not semver triples.
2. No ports in v1: nodes are single-input / single-output maps.
   Port-level typing is a future extension.
3. Cycles are rejected entirely. The proposal allowed cycles inside
   a bounded loop construct; no such construct exists yet
   (`loop_plan` plans only), so the v1 rule is stricter.
4. No per-node `input_mappings` in v1: the default merge rule (D2)
   governs data flow. Explicit mappings are a future extension.
5. No `Enabled` flag on triggers and no `retry_default` or
   `error_path_node` in settings: node policies are authoritative,
   and immutability (disable = new revision) replaces flags.
6. No `position` field in `Definition`: editor layout is a
   UI-phase sidecar, not part of the execution contract.
7. Template edges reference registry node IDs; definition edges
   reference instance IDs. Templates are single-instance starters;
   definitions support multiple instances of one node. Template-to-
   definition conversion (assigning instance IDs, adding trigger
   declarations) is runtime-phase work.
8. The proposal's `workflow.*` permission set is deferred (D9),
   not adopted.

## Guarantees (v1)

- One registry, one definition contract, one validation path.
- Fail-closed everywhere: unknown node/version, unknown config or
  input key, unknown trigger kind, dangling edge, cycle, orphan,
  unresolvable secret reference, and unauthenticated or
  unauthorized principals are all rejected, never defaulted.
- No secret value in any definition, run record, output, or log.
- No silent double-execution: ambiguous outcomes park, they do
  not retry.
- No silent resume: approvals and timers survive restarts only as
  persisted records.
- Published definitions are immutable; rollback is a new revision.

## Limitations (v1, stated not softened)

- No runtime, no HTTP API, no UI, no execution store: nothing in
  Phase 1 executes a workflow. Any claim that NexaFlow "works"
  at this phase is false.
- No live-model evaluation (replay only), no hard-budget
  enforcement (report only), no video-job cancellation node.
- No multi-instance execution, no tenant isolation, no webhook
  dispatch, no durable timers, no system triggers, no sub-workflows,
  no loop execution, no custom code node.
- Definition content hashes are comparable only within this
  implementation (Go JSON encoding), not across languages.
- Branch labels resolve against the branch node's config at
  runtime; the definition layer checks membership, not semantics.
- The schedule node's interval-vs-daily exclusivity is enforced
  at node execution, not at definition validation (schema
  conformance only) — a deliberate layering, covered by test.

## Deferred decisions (need their own owner word)

- The `workflow.*` permission set and the admin API matrix entries
  (API phase).
- The PostgreSQL run-store schema and migration (store phase).
- The webhook rate limit number and the timer claim protocol
  details (runtime phase).
- Multi-instance execution, tenant isolation, the trigger outbox,
  sub-workflows, loop execution, and the sandboxed code node
  (Phases 2–3).
- The Phase 0 proposal as a whole (D1–D5 in its §13): still
  unapproved; this ADR adopts only what it names.

## Consequences

- The runtime, API, and editor phases all build on
  `internal/nexaflow` and `ValidateDefinition`; any change to the
  contract must update the contract tests in the same change.
- The sibling runtime-slice worktree must rebase onto this
  contract; a parallel registry is a defect, not an alternative.
- Phase 1's deliverable is reviewable but unmerged by default: the
  branch carries the contract and the docs, and merging awaits the
  owner's explicit word (mission §9 rule: no merges without it).

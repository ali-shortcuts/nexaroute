# NexaFlow Runtime (Phase 2)

The durable workflow execution engine for the Phase 1 definition
contract (`internal/nexaflow/workflow.go`, ADR 0008). It executes
validated definitions through the adapter registry, persists every
run and node attempt, and recovers interrupted runs without ever
claiming exactly-once execution for external side effects.

## What lives here

| Piece | File | Role |
|---|---|---|
| Engine | `internal/nexaflow/engine.go` | DAG scheduling, dispatch, classification, recovery |
| Graph | `internal/nexaflow/graph.go` | Deterministic topological order, edge firing, input merge |
| States | `internal/nexaflow/states.go` | Run/node state machines and transition guards |
| Store contract | `internal/nexaflow/store.go` | `Store` interface, run/node/approval records, bounds |
| Shared logic | `internal/nexaflow/state.go` | Transition logic shared by memory/file stores |
| Memory store | `internal/nexaflow/memory_store.go` | Tests and single-process use only |
| File store | `internal/nexaflow/file_store.go` | Single-process durability (atomic rename, flock) |
| Postgres store | `internal/nexaflow/postgres_store.go` | **The production store** (ADR 0008 §D5) |

## Run lifecycle

```
created -> running <-> waiting_for_approval -> succeeded | failed | cancelled
running -> needs_manual_action -> failed | cancelled   (operator only)
```

There is no `queued` state in v1: `CreateRun` leaves a run in
`created` and `Execute` moves it straight to `running`.
`needs_manual_action` is the parking state for ambiguous outcomes
(see below). The engine never auto-resumes a parked run; an
operator may only force-fail or cancel it.

Node attempts go
`pending -> claimed -> running <-> waiting -> succeeded | failed |
skipped | cancelled | needs_manual_action`, with `claimed -> pending`
and `running -> pending` reserved for recovery requeues of expired
leases (see Recovery). Attempts are append-only: a retry appends a
new attempt row; the highest attempt is current. Same-state
transitions are idempotent; any other move out of a terminal state
is rejected with `ErrTerminalState`, and a move from an unexpected
current state is rejected with `ErrStateConflict` instead of
overwriting a concurrent writer.

## Scheduling: the AND-join

A node runs only when **every** incoming edge fired and its source
succeeded. Unlabelled edges always fire. A labelled edge fires only
when the source branch node output `selected_branch` equals the
label. A node whose incoming edges are all resolved but at least one
resolved to a non-success (failed, cancelled, skipped, parked, or an
unfired labelled edge) is `skipped`, never executed. Skip therefore
propagates: a failed node skips its successors, and an unselected
branch skips everything downstream of it.

Inputs are whole output maps, merged in edge-declaration order,
last-wins (Phase 1 contract: no ports). Trigger nodes receive the
run input verbatim. Approval resume input (`decision`, `approver`,
`requester`) merges on top of the merged input.

Dispatch is claim-based: `Execute` claims each ready node with
`TryClaim` (worker id + 30s lease), then runs claimed nodes through
a bounded worker pool (default 16, cap 32). A live foreign lease
means another worker owns the node; this sweep skips it.

## Revisions and snapshots

`Publish` appends a new published revision and supersedes the
previous published one; publishing identical content is idempotent
and returns the existing revision. A run pins the full definition
snapshot it was created from: later edits and republishes never
alter a running or finished run. Rollback is republishing an older
snapshot as a *new* revision, never rewriting history.

## Failure classification (ADR 0008 §D6)

Applied in order to every node result:

| Outcome | Node | Run |
|---|---|---|
| Success | `succeeded`, output sanitized + bounded (64 KiB, truncation recorded) | continues |
| `cancelled` | `cancelled` | `cancelled` |
| `approval_required` / `policy_required` | `waiting` (deadline only when `ApprovalTimeout > 0`) | `waiting_for_approval` |
| Retryable code in the node's `RetryableCodes`, attempts left | new pending attempt appended | continues |
| `timeout` on a billable / external / irreversible node | `needs_manual_action` (`ambiguous_timeout`) | `needs_manual_action` |
| Any other definite outcome | `failed` | `failed` once all nodes settle |
| Success the store cannot persist | `needs_manual_action` (`result_not_persisted`) | `needs_manual_action` |

Only `timeout` on an effectful node is treated as ambiguous in v1:
the registry surfaces no other ambiguous code, and v1 adapters
report definite upstream failures as `backend_error`. A future
adapter that cannot prove non-execution must return a dedicated
ambiguous code; it must never be auto-retried on a bare timeout.

## Delivery semantics, stated honestly

- **Safe nodes** (`none`, `read`): at-least-once. A retry or a
  recovery requeue recomputes the same result.
- **Effectful nodes** (`external`, `billable`, `irreversible`):
  at-most-once **per attempt**. The engine never replays an attempt
  whose outcome is unknown; it parks the run instead.
- **Exactly-once is never claimed** for external side effects. A
  billable call that times out may already have executed and billed;
  only an operator, with evidence from the provider side, may
  resolve a parked run.

## Approvals and policy gates

`DecideApproval` records one decision per (run, node); repeating
the identical decision is idempotent, a different one is
`ErrApprovalConflict`. The approver must differ from the run's
principal (separation of duties); a self-approval is rejected
before anything is recorded. A rejected **operator-policy gate**
fails the node (`approval_rejected`) instead of executing it. A
rejected **approval node** is re-run with the decision as input, so
downstream logic can react to `approved: false`. Approval deadlines
are opt-in in this phase (`EngineOptions.ApprovalTimeout`, default
0 = no expiry); P1.4 fixes the number. Expiry fails the node as
`approval_expired`.

## Recovery after restart

`Recover` reconciles runs left `running` or `waiting_for_approval`
by a crash. It never touches live leases:

- Claimed, never dispatched, lease expired → re-queued `pending`.
- Dispatched, side-effect `none`/`read`, lease expired → re-queued
  `pending` (safe to recompute).
- Dispatched, side-effect `external`/`billable`/`irreversible`,
  lease expired → parked `needs_manual_action`
  (`ambiguous_recovery`).
- Waiting node past its approval deadline → `failed`
  (`approval_expired`).
- A run left `running` with every node terminal and no live lease
  is settled from its node states.

A run with a node still claimed by a live lease is left alone.

## Bounds

Per definition: ≤128 nodes, ≤512 edges, ≤1 MiB JSON. Per run:
input ≤64 KiB, stored node output ≤64 KiB (truncated + flagged).
Per node: the registry's own config/input/output bounds apply.
Per definition execution: `MaxConcurrentRuns` (default 1) gates
concurrent `Execute` calls in-process; a second concurrent call
gets `ErrDefinitionBusy`. Cross-process concurrency gating is a
later phase: two processes running the same definition against the
same store both dispatch through claims, so at most one worker owns
a node, but the per-definition run cap is not enforced across
processes yet.

## Stores and the production boundary

ADR 0008 §D5: **execution requires PostgreSQL and fails closed
without it.** `PostgresStore` is the production store: server-clock
leases (`clock_timestamp()`), a single conditional-UPDATE claim,
partial unique index on non-empty idempotency keys, and an
advisory-lock migration (`0x4e45584157464c57`). Tables:
`nexaroute_workflow_definitions`, `_revisions` (with `status`),
`_runs`, `_nodes`, `_approvals`.

`MemoryStore` and `FileStore` implement the same `Store` contract
for tests and single-process use. They must never be presented as
production run history. `FileStore` applies every mutation to a
clone and swaps it in only after a successful atomic persist
(temp file + fsync + rename + directory fsync) under an exclusive
file lock, so a persist failure can never leave memory ahead of
disk. Webhook-event and timer tables are deferred to P1.4.

## Known gaps (not claimed)

- **Trigger-payload composition gap (v1):** the manual trigger
  outputs `{payload}`, but control nodes expect keys like `value`,
  `condition_result`, `first`/`second`. Shipped-node chains do not
  compose directly; engine tests use producer nodes for mechanics
  and real nodes for approval/branch/merge/policy behavior. A
  shaping or mapping step is P1.5 work, not invented here.
- **No scheduler daemon, webhook listener, timers, or HTTP API.**
  Triggers fire only when a caller creates a run; `wait` sleeps
  inside a running node; `waiting_timer` is reserved and never
  emitted. Those are P1.4/P1.5.
- **No multi-process run cap** (see Bounds).
- **Principal snapshot:** node authorization re-evaluates from the
  roles snapshotted at run creation. Role revocation affects new
  runs, not runs already in flight. That boundary is documented,
  not hidden.
- **Approved policy gates + real backends** are exercised only
  through the rejection path in this phase; live gateway execution
  behind an approved gate is P1.6/live-acceptance work.
- **Run history retention pruning** (`Prune`) exists on the store
  but no scheduled pruner calls it yet.

# NexaFlow reconciliation: PR #691 into the canonical line

Status: PR #691 (`supervisor/nexaflow-phase8`, head `704953e`) is
superseded. Its `executor.go` and its parallel runtime/store lineage
were retired by the canonical Task 8 integration (PR #693, merged to
main as `810d990`); they were never merged as a second engine. This
document is the feature-by-feature mapping the closure rests on. It
covers only #691 vs the canonical line. Sibling lineages (#688, #690,
task3–task7 layers) have their own reconciliations and are not
claimed here.

Base for this reconciliation: canonical head `0022eba` (main
`ba5ff2f` merged into the Task 8 branch), itself merged to main as
`810d990`. #691's base was stale (main `e6c3cdf3`) and its green CI
on `704953e` is not evidence for any rebased head; the ports below
were re-verified on the rebased canonical tree, not on #691's CI.

## Rules applied

- No second engine, no second file store, no duplicate DTOs, no
  conflicting route semantics. Every port lands on the canonical
  `Store` interface, the canonical `Engine`, the canonical `Service`,
  or the canonical admin handler (`admin_nexaflow_task8.go`).
- A capability already present in canonical form is not re-ported,
  even when #691's shape differs. Differences are recorded as
  deliberate divergences, not silently dropped.
- Exactly-once is never claimed for external side effects. What the
  ledger proves is at-most-once *delivery execution* (see below).

## Ported onto the canonical interfaces

### Run slots / concurrency limits (admission) — PORTED, hardened

#691 enforced `DefinitionSettings.MaxConcurrentRuns` at admission by
counting active runs in `prepareRun` (`executor.go`), then creating
the run — a count-then-create race. The canonical engine already had
an in-process execution semaphore (default 1 when unset) but no
durable admission cap: a second run could always be created and
would only fail later at `Execute` with `ErrDefinitionBusy`.

Port: the cap is now enforced at the store boundary, inside
`CreateRun`, for every store:

- file/memory (`storeState.createRun`): count + insert under the
  store mutex, so it is atomic in-process (the file store additionally
  holds its lifetime flock, so only one live file store exists).
- PostgreSQL (`PostgresStore.CreateRun`): count + insert inside one
  transaction under a transaction-scoped advisory lock
  (`pg_advisory_xact_lock(hashtext(definition_id))`), so it is
  atomic across processes and hosts sharing the database.
- A slot is held from `CreateRun` until a terminal run state. A
  parked run (`needs_manual_action`) still holds its slot until an
  operator cancels or force-fails it; that is deliberate and
  fail-closed, not a leak: a parked run is unresolved work.
- Only an explicitly positive cap applies (`ErrRunLimit`, HTTP 409).
  Zero keeps the pre-existing behaviour exactly (no admission cap;
  the execution semaphore still defaults to 1 in-process). #691
  defaulted an unset cap to 1 at admission; that default was NOT
  ported, because it would have silently throttled every existing
  definition whose settings are zero.

Tests: admission, cancel-frees-slot, parked-holds-slot,
force-fail-frees-slot, 24-way concurrent admission (memory, exactly
3 of 3 admitted), 12-way concurrent admission across two PG handles
(exactly 2 of 2), and 6 separate PG processes (exactly 2 admitted).

### Resume endpoint — PORTED

#691 had `POST /admin/api/nexaflow/runs/{id}/resume`. Canonical had
no re-drive endpoint: approve auto-executes, but a run re-queued by
`Recover` (or waiting for any other reason) could not be re-driven
over HTTP. Port: `POST /admin/api/nexaflow/runs/{id}/resume` backed
by `Service.ResumeRun`, which is ownership-checked (unlike the raw
`ExecuteRun` facade) and audited. Parked runs answer 409
(`ErrRunParked` mapping added); resume never revives a parked run.
The shape is synchronous, matching the canonical approve/cancel
shape — not #691's async variant and not task4's 202 shape. That
shape choice is the owner's reconciliation decision to confirm, and
it is recorded here rather than hidden.

### Validate endpoint — PORTED

#691 had `POST /admin/api/nexaflow/definitions/{id}/validate`
(validating the stored draft, answering 200 with `valid:false` for
an invalid draft). Canonical validated only at save/publish.
Port: same endpoint on the canonical handler, backed by
`Service.Validate` + `GetDraft` (the same validation save uses).
A missing draft answers 404; a validation outcome answers 200.

### Per-definition runs listing — PORTED

#691 had `GET /admin/api/nexaflow/definitions/{id}/runs`. Canonical
had only the global runs list (ownership-filtered). Port: `GET` on
the same sub-resource, backed by `Service.ListRuns` with a
`DefinitionID` filter (ownership filtering unchanged). `POST` on the
same path still starts a run.

### Idempotency hardening — NEW (neither line had it)

Neither #691 nor canonical Task 8 compared run inputs for a reused
idempotency key: same key + different input silently returned the
original run. That is now `ErrIdempotencyConflict` (HTTP 409),
compared on JSON-normalized input so `1` and `1.0` do not
false-conflict. Additionally, a concurrent duplicate create that
loses the store-level idempotency race now resolves to the original
run (fetch-back) instead of surfacing a spurious conflict for an
identical request. The store-level unique idempotency index (PG)
and in-memory map are unchanged.

### Empty-approver separation of duties — FIXED at the engine

Canonical `DecideApproval` applied separation of duties only when
the approver was non-empty, so a subject-less engine-level caller
could approve any run. `DecideApproval` now rejects an empty
approver outright. The service layer already required an
authenticated approver; this is defence in depth at the boundary
the service does not mediate. (The store-level `RecordApproval`
SoD check is unchanged and is only reachable through the engine.)

## Already canonical (not re-ported)

- **Durable scheduling/dispatch.** Canonical `Scheduler` claims
  each interval slot in the `DeliveryLedger` before starting, and
  the run starts with the slot key as its idempotency key. File
  ledger: cross-process on one host (proven by two subprocesses).
  PG ledger: atomic across processes/hosts (proven by two PG
  processes for delivery and scheduler slots in the Task 6 tests;
  this reconciliation re-ran those tests on the rebased tree).
  A slot whose start fails is not retried by later ticks: the claim
  stands and the operator re-fires manually. #691's scheduler had
  different semantics (per-trigger-instance identity, daily
  schedules, one-slot-at-a-time catch-up, persisted schedule state
  in the store); those semantics were NOT ported — see Deferred.
- **Signed webhooks.** Canonical `WebhookService` verifies HMAC
  over timestamp + "." + body with a 5-minute window, then claims
  the delivery in the durable ledger before starting. This
  supersedes #691's webhook node verifier, whose replay cache was
  in-memory only. Bad signature / stale timestamp / tampered body
  answer 401 indistinguishably; replay answers 409; replay after a
  restart is rejected via the same file ledger (proven) and the
  same PG ledger (proven by the Task 6 delivery test).
- **Approval pause/resume across restarts.** Canonical runs persist
  waiting state; after a file-store reopen, a separate approver
  releases the gate and the run completes (proven, same worker id).
  This reconciliation adds the new-worker-id cases: a live foreign
  lease blocks the new worker (stall, not corruption, not theft)
  and the same new worker resumes once the lease is stale (proven
  with an injected clock, so the test never sleeps for the TTL).
  Self-approval and unauthenticated approval stay rejected.
- **Cancellation.** Canonical `CancelRun` cancels the in-flight
  execute context, cancels every non-terminal node attempt, and
  moves the run to cancelled; it is idempotent on terminal runs
  (returns the stored run), whereas #691 errored on terminal runs.
  The canonical idempotent shape is kept. Cancel after a restart
  is proven. HTTP: `POST /runs/{id}/cancel`, ownership-checked.
- **Crash recovery parking.** Canonical `Recover` is finer than
  #691's, which parked every interrupted run: undispatched attempts
  re-queue, dispatched none/read attempts re-queue, dispatched
  effectful attempts park as `needs_manual_action` with
  `Ambiguous: true`, approval deadlines expire, and settled runs
  recompute. `Execute` on a parked run returns `ErrRunParked` and
  never re-executes; the backend call count stays 0 (proven).
  Parked runs resolve only via cancel or force-fail.
- **Definition/input contract.** Already adopted by Task 8
  (`DefinitionNode.Input` projection, condition→branch fail-closed,
  approval-node mapping rejection). See ADR 0010 and the Task 1
  reconciliation record (ADR 0011) for the full conflict catalog;
  this document does not relitigate it.
- **Registry, nodes, backends.** Blob-identical across the trees;
  nothing to port.

## Deliberately NOT ported (rejected, with reason)

- **Resume modes for ambiguous steps (`retry_ambiguous` /
  `fail_ambiguous` / `skip_ambiguous`).** #691 let an operator
  re-execute an ambiguous node from scratch. That is exactly the
  double-execution hazard the user's crash-recovery requirement
  forbids, so it is rejected: canonical parked runs are never
  re-executed. An operator who has proven the external outcome
  externally starts a new run or force-fails the parked one.
- **#691's whole-run parking in `Recover`.** Superseded by the
  finer canonical recovery above (re-queue only provably-safe
  attempts, park only ambiguous ones).
- **Error-on-terminal cancel.** Superseded by idempotent cancel.
- **#691's default-1 admission cap.** See the run-limit section:
  porting it would change every existing definition's behaviour.

## Deferred (recorded, not silently dropped)

These #691 items are NOT in the canonical line after this
reconciliation. They are follow-up work, not closure blockers for
the capabilities the owner listed — but they must not be mistaken
for done:

- **Production gateway wiring.** #691 wired the engine into
  `cmd/gateway` (config section, attach, scheduler ticker loop,
  synchronous `Recover` at startup). Canonical has zero callers of
  `AttachNexaflow` in `cmd/gateway`: the scheduler, webhook
  service, and recovery primitives exist and are tested at the
  service level, but no gateway process ticks them yet. Wiring
  needs the composite backend (task5) and the owner's API-shape
  decision; it cannot be transplanted from #691's
  `RuntimeBackend`/loopback composition without that backend.
- **Scheduler daily schedules, trigger discovery, persisted
  schedule state, and the scheduler tick HTTP endpoint.**
  Canonical schedules are caller-supplied interval schedules;
  #691 read schedule triggers from published definitions, supported
  daily schedules, kept schedule state in the store, and exposed
  `POST /scheduler/tick`. Daily/discovery/state/tick are deferred
  with the wiring item above. The canonical no-catch-up, no-retry
  slot semantics are deliberate and documented in
  `task8_scheduler.go`; a future daily/catch-up design must state
  its double-execution story before landing.
- **Webhook `secret_ref` resolution and published-trigger
  validation.** Canonical webhook secrets are an operator-supplied
  map (test/gateway wiring only); resolving `secret_ref` through
  the secret store and validating that the definition/trigger is
  published belong to the wiring's Start function, which does not
  exist yet. Until then, an unknown definition answers 404 via the
  missing secret, and a known secret with an unpublished definition
  fails at Start (404) — that path is unwired, not proven, and is
  recorded as such.
- **Async dispatch (`StartRunAsync`, 202 executions).** Canonical
  execution is synchronous (V1 behaviour). Async dispatch needs
  the wiring item and the owner's shape decision (202 vs
  synchronous), already flagged in the Task 8 PR body.
- **JSON-editor definition page and definition helpers (UI).**
  Canonical UI is the non-canvas inspection page. #691's JSON
  editor is UI parity, not engine capability, and is deferred to
  the Studio line (#690).

## Storage guarantees (tested, not just documented)

- **PostgreSQL is the multi-instance authority.** Delivery claims,
  scheduler slot claims, node lease claims, run admission, and
  idempotency are all atomic at the database (unique indexes,
  conditional updates, advisory-locked transactions, server-clock
  lease comparisons). Proven across two handles and across
  separate OS processes on one disposable database.
- **File mode is single-host only.** The file store holds its lock
  for its lifetime (one live store per path); the file delivery
  ledger is atomic across processes on one host via `O_EXCL`
  files. File stores must never be shared across hosts or NFS and
  must never be presented as multi-host protection.
- **Memory mode is test-only.** The memory store and memory ledger
  lose all state on restart and are invisible to other processes.
  Any test that passes on memory alone proves logic, not durability;
  the durability tests in this reconciliation run on file and PG.
- **Exactly-once wording.** The delivery ledger gives at-most-once
  *execution of an identical delivery* (claim before start, claims
  never released). It does not give exactly-once *external side
  effects*: a claimed delivery whose start fails is consumed (the
  operator re-sends with a new delivery), and a parked ambiguous
  attempt is precisely the case where the external outcome is
  unproven. No document in this reconciliation claims otherwise.

## Evidence

- `internal/nexaflow/task8_691_reconciliation_test.go` — run limit
  (admission/cancel/park/concurrent/memory + PG two-handle +
  PG cross-process), idempotency (input conflict, numeric
  equivalence, concurrent duplicate), empty-approver denial,
  stale-lease recovery (live untouched / expired re-queued /
  ambiguous parked + never re-executed), concurrent claims,
  approval resume + cancel across a file-store restart with a
  different worker id (stall-then-resume).
- `internal/httpapi/admin_nexaflow_691_reconciliation_test.go` —
  resume/validate/runs-listing, idempotency + limit HTTP mapping
  (409), parked resume (409), webhook bad-signature (401),
  tampered/stale/replay handling.
- `internal/nexaflow/task6_final_test.go` — new `run-limit` child
  mode for the cross-process PG admission proof; the pre-existing
  Task 6 PG cross-process delivery/scheduler/lease tests were
  re-run on the rebased tree.
- Full verification (tests, `verify.sh`, CI, Security) is recorded
  in the follow-up PR; local `verify.sh` on this VM is subject to
  the known 3-minute per-package timeout artifact and CI is the
  arbiter for the full gate.

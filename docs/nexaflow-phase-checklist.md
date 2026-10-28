# NexaFlow remaining-phases checklist (dependency-ordered)

Phase 1 (this branch) delivered: the canonical registry (PR #675),
the workflow-definition contract + validation
(`internal/nexaflow/workflow.go`), contract tests, ADR 0008, and
the architecture document. Nothing executes yet. The phases below
are ordered by dependency: each phase's exit criteria gate the next.
No phase merges without the owner's explicit word (mission §9 rule),
and no phase may claim what its exit criteria do not prove.

## Phase 8 status (this branch, unmerged)

Phase 8 delivered the runtime slice on top of Phase 1, with three
documented deviations from the P1.2–P1.7 plan below:

1. **FileStore, not PostgreSQL.** The supported topology is a
   single gateway process. PG tables, multi-instance execution,
   and PG outage batteries remain open (P1.2/P3).
2. **JSON editor page, not a visual canvas.** P2's canvas is
   still open; the Workflows page edits definition JSON and
   inspects runs.
3. **No approval deadline, no webhook rate limit, no CAS
   publish.** Approval waits until decided; webhook rate limiting
   is the operator's reverse proxy; publish serializes on the
   single-host store lock instead of CAS.

Delivered and ticked below: engine, store (file), triggers
(manual/schedule/webhook), approvals across restarts, crash
recovery, admin API + `workflow.read`/`workflow.write` matrix,
loopback/MCP/video/cost/eval wiring, runbook docs
(user guide, API reference, security guidance, migration notes,
examples). Open: PG store, canvas editor, approval deadlines,
webhook rate limiting, production soak, security sweep as a
separate pass, and all P3 decisions.

## P1.2 — Run store (FileStore in Phase 8; PostgreSQL still open) + migrations

Dependencies: Phase 1 (definition contract, ADR 0008 §D5).
- [ ] Migration for `nexaroute_workflow_*` tables (runs, node
      executions, approvals, webhook events, timers), following the
      repo's migration conventions (versioned, checksummed,
      advisory-lock discipline; PG schema discipline as in
      `internal/controlplane/migrations.go`).
      Phase 8 note: NOT done. The store is FileStore with its own
      versioned format (current: 2) and `Migrate`; see
      `docs/nexaflow-migration.md`.
- [x] Store implementation: run creation, node-execution rows,
      claim protocol (server-clock lease, TTL 30s, same-owner
      renewal, row-locked terminal guard), ambiguous-job parking as
      `needs_manual_action`.
      Phase 8: FileStore + MemoryStore implement this shape
      (leases, append-only history, `parked` = the implemented name
      for `needs_manual_action`); PG rows are the open part.
- [x] Recovery on startup: reclaim expired leases, park (never
      silently resume or retry) ambiguous executions, honor the
      approval/timer persisted records.
      Phase 8: `Engine.Recover` parks expired-lease runs; approvals
      persist across restarts. Timers are schedule slots, not
      persisted sleep records.
- [x] Retention enforcement (90 days default, 7–365 per definition)
      with a documented deletion path.
      Phase 8: `Engine.Prune` + store `PruneRuns`/`PruneRunsFor`.
- Exit criteria: store tests incl. concurrent-claim (exactly one
  lease owner), lease-expiry recovery, and terminal-guard tests;
  `go vet` + gofmt clean; migration up/down verified on disposable
  PG. No HTTP API yet; the store is exercised by tests only.
  Phase 8: store/claim/recovery/terminal-guard tests pass on
  FileStore + MemoryStore; PG verification remains open.
- Must NOT claim: execution (no engine yet), multi-instance runs.

## P1.3 — Runtime engine core (DAG scheduling, test-mode runs)

Dependencies: P1.2 (store).
- [x] Engine: topological scheduling, AND-join, branch selection
      (labelled vs unlabelled edges), skip semantics (failed
      predecessor, unselected branch), the §5 state machines.
      Phase 8: `Engine` in `internal/nexaflow/executor.go`, plus
      the three contract extensions (input mappings, `for_each`,
      `on_error`) documented in the architecture.
- [x] Worker pool (16), per-definition concurrency (1), run-level
      timeout, per-node timeout via context.
      Phase 8, with a deviation: node concurrency is a bounded
      semaphore (default 4 per run) and run concurrency is an
      engine slot (default 16), not a 16-worker pool; per-definition
      concurrency defaults to 1; run and node timeouts via context.
- [x] Retry engine per ADR 0008 §D6: declared codes only, bounded
      attempts, park-on-ambiguity, never retry cancelled.
- [x] Cancellation propagation end-to-end, with the video-job
      caveat recorded in the run (submitted job id retained).
      Phase 8: cancel test covers the wait node; the video-job
      caveat (cancelling the call does not cancel the job) stands.
- [x] Pure/control nodes end-to-end in test mode (no HTTP, no
      backend): manual trigger -> condition -> branch -> merge /
      wait / loop_plan (plan only) / error_handler / approval
      (decision supplied in-test).
      Phase 8: all covered, and loop execution (`for_each`) is
      additionally implemented, beyond this item's plan-only scope.
- Exit criteria: engine tests incl. red/green proof for skip,
  branch, cycle-rejection-at-definition (already), park, and
  cancel cases; race tests for the worker pool; `verify.sh` green
  on the branch. Runs exist only in test mode / via Go API.
- Must NOT claim: HTTP-triggered runs, durable timers, approvals
  across restarts (those need P1.4/P1.5 persistence wiring).

## P1.4 — Triggers runtime + durable timers + approvals persistence

Dependencies: P1.3 (engine).
- [x] Manual trigger execution path (operator-supplied payload).
- [x] Schedule timers: durable timer rows + claim protocol
      (timers fire exactly once per claim; missed fires are
      recorded, not silently skipped or storm-replayed).
      Phase 8: claim-based `Scheduler` over the store; missed
      slots catch up one per tick (recorded as runs, not storms).
- [x] Webhook dispatch: HTTP endpoint shape (in `internal/httpapi`,
      see P1.5 for the route itself), durable replay protection,
      fixed rate limit (number decided in this phase and recorded
      in ADR 0008's follow-up), 1 MiB bound, HMAC verification
      reuse from the node verifier.
      Phase 8, with deviations: durable ledger + endpoint done;
      NO fixed rate limit (operator reverse proxy instead); bound
      is 64 KiB at the engine, not 1 MiB; the trigger node
      re-verifies in addition to the engine.
- [x] Approval persistence: approval records, approver !=
      requester enforcement, deadline expiry ->
      `failed(approval_expired)`, no silent resume across restarts.
      Phase 8, with a deviation: persistence + approver check +
      restart resume done; NO deadline expiry (an awaiting run
      waits until decided, resumed, or cancelled).
- Exit criteria: timer claim tests, webhook replay/rate-limit
  tests, approval deadline + restart tests, all on disposable
  infrastructure (no production endpoints).
- Must NOT claim: public webhook availability (needs operator
  network policy + the P1.5 API), system triggers (Phase 3).

## P1.5 — Admin API + authorization matrix + inspector

Dependencies: P1.3 (engine), P1.4 (trigger persistence).
- [x] Admin routes in `internal/httpapi` (Server fields are
      unexported; the API lands here, not in `internal/nexaflow`):
      definition CRUD (draft), publish (CAS), run start/cancel,
      run + node-execution inspection, approval decisions, trigger
      management.
      Phase 8: routes + inspection + approval/resume/cancel done;
      publish serializes on the store lock instead of CAS.
- [x] Authorization decision (owner word required): adopt,
      amend, or reject the proposal's `workflow.*` permission set
      (read/write/publish/execute/approve/credential_bind); add the
      `adminPermissionForRequest` matrix entries in the SAME change
      (unmapped route x method = deny, by existing convention).
      Phase 8: adopted as TWO permissions, `workflow.read` /
      `workflow.write` (owner/admin/operator both; developer/viewer
      read only), with matrix entries in the same change. The
      six-permission proposal remains a P3-style decision if finer
      granularity is ever wanted.
- [x] Definition publish path: validation + CAS + content hash +
      revision, with rollback = republish as a new revision.
      Phase 8: validation + content hash + revision + republish
      rollback done; CAS replaced by store serialization (see
      above).
- [x] Inspector: read-only run/node views with secret redaction at
      the boundary; audit events for every mutation.
      Phase 8: run detail + audit endpoint; step history is stored
      redacted. Note the run-input gap: raw workflow input persists
      (see security guidance).
- [x] Rollout flag: default-off admin flag; flag off = zero
      behavior change (no routes, no timers, no runs).
      Phase 8: `nexaflow.enabled` config flag; handlers answer 503
      when unattached.
- Exit criteria: API tests incl. matrix coverage (unmapped =
  deny), publish-conflict test, redaction assertions on inspector
  output, flag-off behavior test.
- Must NOT claim: editor (Phase 2), multi-instance, tenant
  isolation.

## P1.6 — Adapter wiring (model loopback, MCP/video, budget guard)

Dependencies: P1.5 (API).
- [x] Model node loopback wiring: `llm.chat` via `GatewayClient`
      against the local gateway, cost attribution to the run, and
      the operator-policy gate enforced at run start (not just at
      node execution).
      Phase 8: `RuntimeBackend` + `AttachNexaflow` wire loopback
      chat; policy is enforced at node execution via
      `policy_approved` recorded at publish (not re-checked at run
      start — the run inherits the published setting). Cost
      attribution per run is NOT implemented (usage snapshots are
      read-only); do not claim it.
- [x] MCP/video admin adapters: tool allowlist + gateway listing
      double-check at run start; video job id recorded on the run
      for the cancellation caveat.
      Phase 8: allowlist + listing double-check at node execution
      (not run start); video job ids live in node outputs.
- [x] Budget guard (report-only in v1): `budget_check` threshold
      evaluation at run start; enforcement stays OUT (separate
      hard-budget proposal, unapproved) — the runbook must say the
      check does not block.
      Phase 8: `budget_check` node reports; the user guide and
      security guidance state it does not block.
- [x] Eval replay wiring: suite existence + outcome bounds
      enforced; live evaluation stays OUT.
      Phase 8: `ReplayEvalBackend` over `eval.Runner`; replay only.
- Exit criteria: loopback tests against a loopback gateway
  (httptest), allowlist-denial tests, budget-report tests; zero
  live provider calls; zero real credentials.
- Must NOT claim: live-model evaluation, hard-budget enforcement,
  video cancellation.

## P1.7 — Hardening, soak, runbook, rollout

Dependencies: P1.6 (all wiring).
- [ ] Failure-injection battery: PG outage (gateways alive, sync
      errors, retryable 503s, RPO 0 on recovery), backend timeouts,
      ambiguous outcomes -> park, restart mid-run -> recovery.
- [ ] Bounded local soak (same discipline as the 2026-10-24 soak:
      staged, measured, leak-checked); multi-hour/production soak
      stays UNVERIFIED unless separately run.
- [x] Runbook: operator procedures for publish/rollback, approval
      expiry, parked runs, retention, flag on/off; known-gaps
      update with the same honesty as this checklist.
      Phase 8: user guide, API reference, security guidance,
      migration notes, examples. Approval expiry is NOT covered
      because it does not exist (see P1.4).
- [ ] Security sweep over the new surface (authz matrix, secret
      handling, webhook verification, timer claims) before any
      merge request.
- Exit criteria: soak evidence + failure battery green on the
  branch; runbook reviewed; sweep findings fixed or recorded as
  open defects. Merge still needs the owner's explicit word.
- Must NOT claim: production readiness (that needs the acceptance
  trackers #646-#654 equivalents for NexaFlow, separately scoped).

## P2 — Visual editor (separate owner decision)

Phase 8 note: NOT delivered as a canvas. The Workflows UI page is
a JSON editor + run inspector over the P1.5 API (definition
authoring UX without drag-and-drop, layout sidecar, or canvas
flows). The canvas items below remain open and still need their
separate owner decision.

Dependencies: P1.7 (a working, reviewed runtime + API).
- [ ] Canvas: node palette from the registry (single source of
      truth: the registry, not a duplicated list), edge authoring
      (instance IDs), branch-label authoring, trigger authoring.
- [ ] Layout sidecar: positions stored outside the execution
      contract (definitions carry no position field, deliberately).
- [ ] Definition authoring UX: client-side validation mirroring
      `ValidateDefinition` (server validation remains authoritative),
      publish/rollback flows, run inspector views.
- [ ] Editor tests: browser acceptance for the canvas flows
      (same reliability discipline as PR #686's runner).
- Exit criteria: editor works against the P1.5 API on loopback;
  no execution semantic lives in the editor (a canvas bug cannot
  change what a run does).
- Must NOT claim: any runtime semantic change; the editor is a
  client of the contract, not a second contract.

## P3 — Separate decisions (each needs its own owner word)

None of these is authorized by Phase 1 or by P1.2–P1.7:
- [ ] Multi-instance execution (claim protocol is designed for it;
      demonstration on two hosts is still required).
- [ ] Tenant isolation (beyond the admin-plane scope label).
- [ ] Trigger outbox + system triggers (provider-health changes,
      usage thresholds).
- [ ] Sub-workflows (depth cap 5 recorded in ADR 0008) and loop
      execution (bounded-loop construct; loop cap 100).
      Phase 8 note: loop execution IS delivered (`for_each`,
      cap 100); sub-workflows remain open.
- [ ] Sandboxed out-of-process custom code node (Phase 3 only,
      never in-process).
- [ ] In-process model-execution service extraction (refactor
      candidate; loopback stays the V1 transport until decided).
- [ ] Cross-language definition canonicalization (hashes are
      Go-JSON-only in v1).

# NexaFlow remaining-phases checklist (dependency-ordered)

Phase 1 (this branch) delivered: the canonical registry (PR #675),
the workflow-definition contract + validation
(`internal/nexaflow/workflow.go`), contract tests, ADR 0008, and
the architecture document. Nothing executes yet. The phases below
are ordered by dependency: each phase's exit criteria gate the next.
No phase merges without the owner's explicit word (mission §9 rule),
and no phase may claim what its exit criteria do not prove.

## P1.2 — Run store (PostgreSQL) + migrations

Dependencies: Phase 1 (definition contract, ADR 0008 §D5).
- [ ] Migration for `nexaroute_workflow_*` tables (runs, node
      executions, approvals, webhook events, timers), following the
      repo's migration conventions (versioned, checksummed,
      advisory-lock discipline; PG schema discipline as in
      `internal/controlplane/migrations.go`).
- [ ] Store implementation: run creation, node-execution rows,
      claim protocol (server-clock lease, TTL 30s, same-owner
      renewal, row-locked terminal guard), ambiguous-job parking as
      `needs_manual_action`.
- [ ] Recovery on startup: reclaim expired leases, park (never
      silently resume or retry) ambiguous executions, honor the
      approval/timer persisted records.
- [ ] Retention enforcement (90 days default, 7–365 per definition)
      with a documented deletion path.
- Exit criteria: store tests incl. concurrent-claim (exactly one
  lease owner), lease-expiry recovery, and terminal-guard tests;
  `go vet` + gofmt clean; migration up/down verified on disposable
  PG. No HTTP API yet; the store is exercised by tests only.
- Must NOT claim: execution (no engine yet), multi-instance runs.

## P1.3 — Runtime engine core (DAG scheduling, test-mode runs)

Dependencies: P1.2 (store).
- [ ] Engine: topological scheduling, AND-join, branch selection
      (labelled vs unlabelled edges), skip semantics (failed
      predecessor, unselected branch), the §5 state machines.
- [ ] Worker pool (16), per-definition concurrency (1), run-level
      timeout, per-node timeout via context.
- [ ] Retry engine per ADR 0008 §D6: declared codes only, bounded
      attempts, park-on-ambiguity, never retry cancelled.
- [ ] Cancellation propagation end-to-end, with the video-job
      caveat recorded in the run (submitted job id retained).
- [ ] Pure/control nodes end-to-end in test mode (no HTTP, no
      backend): manual trigger -> condition -> branch -> merge /
      wait / loop_plan (plan only) / error_handler / approval
      (decision supplied in-test).
- Exit criteria: engine tests incl. red/green proof for skip,
  branch, cycle-rejection-at-definition (already), park, and
  cancel cases; race tests for the worker pool; `verify.sh` green
  on the branch. Runs exist only in test mode / via Go API.
- Must NOT claim: HTTP-triggered runs, durable timers, approvals
  across restarts (those need P1.4/P1.5 persistence wiring).

## P1.4 — Triggers runtime + durable timers + approvals persistence

Dependencies: P1.3 (engine).
- [ ] Manual trigger execution path (operator-supplied payload).
- [ ] Schedule timers: durable timer rows + claim protocol
      (timers fire exactly once per claim; missed fires are
      recorded, not silently skipped or storm-replayed).
- [ ] Webhook dispatch: HTTP endpoint shape (in `internal/httpapi`,
      see P1.5 for the route itself), durable replay protection,
      fixed rate limit (number decided in this phase and recorded
      in ADR 0008's follow-up), 1 MiB bound, HMAC verification
      reuse from the node verifier.
- [ ] Approval persistence: approval records, approver !=
      requester enforcement, deadline expiry ->
      `failed(approval_expired)`, no silent resume across restarts.
- Exit criteria: timer claim tests, webhook replay/rate-limit
  tests, approval deadline + restart tests, all on disposable
  infrastructure (no production endpoints).
- Must NOT claim: public webhook availability (needs operator
  network policy + the P1.5 API), system triggers (Phase 3).

## P1.5 — Admin API + authorization matrix + inspector

Dependencies: P1.3 (engine), P1.4 (trigger persistence).
- [ ] Admin routes in `internal/httpapi` (Server fields are
      unexported; the API lands here, not in `internal/nexaflow`):
      definition CRUD (draft), publish (CAS), run start/cancel,
      run + node-execution inspection, approval decisions, trigger
      management.
- [ ] Authorization decision (owner word required): adopt,
      amend, or reject the proposal's `workflow.*` permission set
      (read/write/publish/execute/approve/credential_bind); add the
      `adminPermissionForRequest` matrix entries in the SAME change
      (unmapped route x method = deny, by existing convention).
- [ ] Definition publish path: validation + CAS + content hash +
      revision, with rollback = republish as a new revision.
- [ ] Inspector: read-only run/node views with secret redaction at
      the boundary; audit events for every mutation.
- [ ] Rollout flag: default-off admin flag; flag off = zero
      behavior change (no routes, no timers, no runs).
- Exit criteria: API tests incl. matrix coverage (unmapped =
  deny), publish-conflict test, redaction assertions on inspector
  output, flag-off behavior test.
- Must NOT claim: editor (Phase 2), multi-instance, tenant
  isolation.

## P1.6 — Adapter wiring (model loopback, MCP/video, budget guard)

Dependencies: P1.5 (API).
- [ ] Model node loopback wiring: `llm.chat` via `GatewayClient`
      against the local gateway, cost attribution to the run, and
      the operator-policy gate enforced at run start (not just at
      node execution).
- [ ] MCP/video admin adapters: tool allowlist + gateway listing
      double-check at run start; video job id recorded on the run
      for the cancellation caveat.
- [ ] Budget guard (report-only in v1): `budget_check` threshold
      evaluation at run start; enforcement stays OUT (separate
      hard-budget proposal, unapproved) — the runbook must say the
      check does not block.
- [ ] Eval replay wiring: suite existence + outcome bounds
      enforced; live evaluation stays OUT.
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
- [ ] Runbook: operator procedures for publish/rollback, approval
      expiry, parked runs, retention, flag on/off; known-gaps
      update with the same honesty as this checklist.
- [ ] Security sweep over the new surface (authz matrix, secret
      handling, webhook verification, timer claims) before any
      merge request.
- Exit criteria: soak evidence + failure battery green on the
  branch; runbook reviewed; sweep findings fixed or recorded as
  open defects. Merge still needs the owner's explicit word.
- Must NOT claim: production readiness (that needs the acceptance
  trackers #646-#654 equivalents for NexaFlow, separately scoped).

## P2 — Visual editor (separate owner decision)

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
- [ ] Sandboxed out-of-process custom code node (Phase 3 only,
      never in-process).
- [ ] In-process model-execution service extraction (refactor
      candidate; loopback stays the V1 transport until decided).
- [ ] Cross-language definition canonicalization (hashes are
      Go-JSON-only in v1).

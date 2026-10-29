# NexaFlow reconciliation: PR #688 → canonical line

## Status and lineage (read first)

- **Canonical line is main after merged PR #693** (merge commit `810d990`,
  2026-10-29T08:24:02Z). PR #693 was merged by another actor while this
  reconciliation was in progress; its head `0022eba` is byte-identical to
  main's `internal/nexaflow/` tree at `0d26dfe`, so this reconciliation is
  based on current main, not on the #693 review branch.
- **This reconciliation PR is NOT merged.** It awaits the owner's explicit
  word under mission §9. Nothing in this document claims it has landed.
- **PR #688** (`nexaflow/phase5-integration`, head `8590d02`, CONFLICTING)
  is closed as superseded by merged #693 plus this reconciliation PR.
  Its in-memory engine is retired, not merged: the durable #689 engine is
  canonical. If the owner rejects this reconciliation PR, the ported items
  below return for re-triage; they are not silently dropped.
- **Do not confuse main with `supervisor/nexaflow-final-integration`.**
  That is a separate local-only lineage (no `origin` ref) which already
  contains its own composite backend, selector validation, and template
  instantiation. This document records what was missing **on main** and
  what this PR adds there. The final-integration lineage is not a source
  for this PR and nothing was copied from it.
- **No second node schema, no duplicate registry.** All work lands on the
  single 23-node registry, the single `Definition`/`input_mapping`
  grammar, and the single `Backend` port interface of main.

## Acceptance matrix: #688 item → disposition

| #688 item | Disposition on main | Where |
|---|---|---|
| Composite backend assembly (`composite_backend.go`) | **PORTED, adapted** to main's 14-method `Backend` interface (no budget reserve/settle/release ports exist on main — see below) | `internal/nexaflow/composite_backend.go` |
| Gateway selector validation (`GET /v1/models`) | **PORTED** onto `GatewayClient`: listed selector → route; unlisted → `selector_not_found`; empty → `validation_error`; never a chat call | `internal/nexaflow/http_backend.go` (`ResolveSelector`) |
| Secret reference resolution, fail-closed | **PORTED** onto `GatewayClient`: no resolver / empty ref / resolver error → `secret_error`, resolver error text and value never leak | `internal/nexaflow/http_backend.go` (`ResolveSecretRef`) |
| Template instantiation (`InstantiateWorkflow` + Input/Bindings) | **PORTED as `InstantiateDefinition`**, adapted to main's `Definition`/`input_mapping` grammar — NOT #688's `Workflow` model, NOT the `Bindings` grammar | `internal/nexaflow/templates.go` |
| Typed field coercion (`coerceTypedFields`) | **PORTED as `coerceTemplateFields`** (string → number/boolean against the node schema) | `internal/nexaflow/templates.go` |
| 5 default templates, genuinely instantiable | **REWRITTEN**: each gained a trigger node and real `Input` mappings; all 5 instantiate into validated `Definition`s (see below) | `internal/nexaflow/templates.go` |
| In-memory engine, `Workflow` model, `MemoryRunStore` | **REJECTED — retired.** The durable #689 engine is canonical; nothing from #688's engine is merged | — |
| `#688` `doc.go` honesty-notice reframing | **NOT ported.** Main's `doc.go` is the Phase 1/2/7 lineage doc; only its stale "no approved registry/runtime" paragraph was corrected to post-#693 reality, plus the new template-instantiation sentence | `internal/nexaflow/doc.go` |
| Route-distinguished selector validation (final-integration's `DirectRouteBackend`) | **NOT duplicated.** On main, the gateway `GET /v1/models` list is the selector source; a second route-kind authority would be a duplicate adapter | — |
| Budget reserve/settle/release backend ports | **NOT ported.** They do not exist on main's `Backend` interface; hard-budget enforcement remains unimplemented (see preserved-unsupported) | — |
| `VideoSubmitCalls` counter on the test fake | **NOT ported.** Counting only; the reconciliation tests use their own counting backend with all 14 methods | `internal/nexaflow/reconciliation_688_test.go` |

## Per-node wiring (all 23 nodes, proven by test)

Proven by `TestReconciliationNodeWiring` with a counting backend: every
node executes successfully with its minimal valid input/config, calls
exactly the backend methods listed, and calls no other method.

| Node | Backend methods called | Notes |
|---|---|---|
| `nexaroute.llm.chat` | `ResolveSelector`, `Chat` | selector from Config `route_or_alias` (required); policy-gated |
| `nexaroute.provider.health` | `ProviderHealth` | |
| `nexaroute.provider.compat` | `Compat` | |
| `nexaroute.eval.run` | `RunEvalReplay` | suite must exist (`eval.LookupSuite`); output `upstream_calls` = 0 |
| `nexaroute.mcp.list` | `MCPList` | |
| `nexaroute.mcp.call` | `MCPList`, `MCPCall` | allowlist AND gateway listing both checked before the call |
| `nexaroute.video.submit` | `VideoSubmit` | policy-gated; `duration_seconds` effectively required at runtime |
| `nexaroute.video.status` | `VideoStatus` | |
| `nexaroute.video.output` | `VideoStatus` (+ `VideoAsset` only when `asset_id` given) | proven both ways |
| `nexaroute.cost.usage` | `UsageSnapshot` | |
| `nexaroute.cost.budget_check` | `BudgetSnapshot` | report-only (see below) |
| `nexaroute.trigger.manual` | — (pure) | echoes run input payload |
| `nexaroute.trigger.schedule` | — (pure) | computes `next_run` from Config `interval` XOR `daily`; no daemon |
| `nexaroute.trigger.webhook` | `ResolveSecretRef` | then HMAC verification (`timestamp + "." + body`); secret never in errors |
| `nexaroute.http.request` | `HTTPDo` (+ `ResolveSecretRef` first when `auth_secret_ref` set) | token sent as Bearer, never returned |
| `nexaroute.control.condition` / `.branch` / `.json_transform` / `.loop_plan` / `.merge` / `.wait` / `.error_handler` | — (pure) | `wait` tested with `duration_ms: 0` (no real sleep) |
| `nexaroute.control.approval` | — (pure at node level) | standalone Run reads decision/approver/requester from input; the engine supplies decisions from the durable approval record |

`TestReconciliationRegistryPreserves23Nodes` pins the exact 23 IDs and
asserts the absence of `control.subworkflow`, `video.cancel`,
`cost.budget_reserve`, `cost.budget_settle`, `cost.budget_release`.

## The five demanded test properties

1. **Every registered node has correct backend wiring** —
   `TestReconciliationNodeWiring` (23 nodes, exact method sets),
   `TestReconciliationVideoOutputWithAsset`,
   `TestReconciliationHTTPRequestWithSecretRef`,
   `TestCompositeBackendDelegatesToOwners` (each of the 14 methods reaches
   its owning subsystem: gateway HTTP, `health.Manager`/`compat.Store`,
   `usage.Tracker`/`BudgetLedger` snapshots, `eval.Runner` replay),
   `TestReconciliationNodeThroughCompositeBackend` (llm.chat through the
   real composite to a gateway over HTTP — closes the node→composite
   chain for one node; per-node-through-composite coverage beyond that
   one chain is NOT claimed).
2. **Failed authorization → zero backend calls** —
   `TestReconciliationAuthorizationMakesZeroBackendCalls`: anonymous
   principal × all 23 nodes → `permission_denied` + 0 calls;
   authenticated principal without the permission → `permission_denied`
   + 0 calls; owner without policy approval on llm.chat →
   `policy_required` + 0 calls. Registry enforcement order (anonymous →
   permission → policy → schema) is what makes this testable.
3. **Unresolved placeholders / secret references fail before execution** —
   `TestTemplatesInstantiateDefinitionFailures` (missing placeholder,
   embedded unresolved `{{...}}`, duplicate node, unknown node, missing
   required input, uncoercible number, approval node with an input
   mapping); `TestReconciliationFailuresBeforeExecution` (unlisted
   selector → `selector_not_found` with `Chat` never called; unresolvable
   http/webhook secret → `secret_error` with `HTTPDo` never called;
   invalid video input → `validation_error` with `VideoSubmit` never
   called); `TestReconciliationEngineUnresolvedRefFailsBeforeBackend`
   (engine-level `$ref` to a missing path → node `input_resolution`,
   run failed, `HTTPDo` never called).
4. **All templates instantiate into validated workflows** —
   `TestTemplatesInstantiateToValidDefinitions` (all 5 → validated
   `Definition`, re-validated after return),
   `TestTemplatesInstantiateDefinitionSpotChecks` (manual vs schedule
   triggers; `"10"` coerced to `float64(10)`; policy template refused
   without approval),
   `TestTemplatesHealthAlertExecutesEndToEnd` (the health-alert template
   executes through the durable engine: whole-output ref
   `{"$ref": "health"}` resolves at runtime, condition true, compat
   called once). Note: main's `mappedNodeInput` supports whole-object
   refs, so — unlike the final-integration lineage, where two templates
   are blocked on whole-output condition sources — all 5 main templates
   instantiate. Instantiation is proven for all 5; end-to-end execution
   is proven for the health-alert template only.
5. **Sensitive values never appear in logs/history** —
   `TestReconciliationSecretsNeverSurface`: credential-keyed tool result
   redacted from node output; backend error text carrying a bearer token
   redacted before it escapes (`Registry.Execute` passes all errors
   through `RedactError`); stored run history redacted (engine
   `sanitizeNodeOutput` + store sanitization; the package has no log
   surface in non-test code). Resolved-secret-through-history is proven
   for the tool-result shape; the resolved webhook/http token shape is
   proven at node-output level (token reaches the backend header only).

## Defect found and fixed by this reconciliation

**Engine state-machine gap (regression test:
`TestReconciliationEngineUnresolvedRefFailsBeforeBackend`).** Sweep fails
a claimed node it cannot run (unknown node, or an unresolvable input
mapping) with `SaveNodeExecution(..., NodeClaimed)`, but
`validNodeTransition` forbade `NodeClaimed → NodeFailed`, so `Execute`
aborted with `nexaflow: state conflict` instead of failing the node with
`input_resolution`/`node_not_found` and failing the run. No existing test
covered either fail-without-dispatch path. Fix (minimal, in
`internal/nexaflow/states.go`): allow `NodeClaimed → NodeFailed`, the
fail-without-dispatch transition — the node was claimed but never
dispatched, so no side effect has happened. This also fixes the
`node_not_found` sweep path, which had the same shape.

## Preserved unsupported behavior (nothing stubbed to success)

- **Live eval**: replay only (`eval.Runner`, caller-supplied outcomes);
  `upstream_calls` is always 0. No live path exists to stub.
- **Hard-budget enforcement**: `budget_check` compares a snapshot against
  an operator threshold and reports; it does not enforce, reserve, or
  block. There are no reserve/settle/release ports on main's `Backend`.
  The PostgreSQL-backed hard-budget guard is a separate, unimplemented
  phase.
- **Scheduler daemon**: schedule nodes compute `next_run`; the Tick
  library exists but no daemon/endpoint hosts it.
- **Webhook listener**: verification (HMAC + durable replay via
  `WebhookService`) and the httpapi handler exist; the package does not
  claim a standalone listener beyond that.
- **Video cancellation**: no cancel node, no `VideoCancel` on `Backend`.
  Run cancellation aborts the HTTP call; it does not cancel a provider
  video job.
- **PostgreSQL history**: `PostgresStore` implements drafts, revisions,
  runs, node executions, claims, and history reads through the `Store`
  interface; live-PG acceptance is NOT RUN here (no DSN in this
  environment) and is not claimed.
- **Also still absent, not claimed**: any-join (AND-join only),
  `workflow.*` permissions at the HTTP layer (admin routes map to
  `config.read`/`config.write`/`audit.read`), tenant isolation beyond
  the ownership ceiling + scope label, durable timer waits (sync wait
  ≤ 5m in-process; `waiting_timer` is never emitted), subworkflows,
  studio UI, and production gateway wiring (`httpapi.Server.AttachNexaflow`
  exists; no caller in `cmd/gateway` on main).

## Verification (this PR head, rebased onto main `39a7498`)

- `go test -count=1 ./internal/nexaflow/` — PASS (4.989s)
- `go test -race -count=1 ./internal/nexaflow/` — PASS (12.615s)
- `go vet ./internal/nexaflow/`, `gofmt` clean
- Focused suites (`TestReconciliation*`, `TestTemplatesInstantiate*`,
  `TestGatewayClient*`, `TestCompositeBackend*`) — PASS
- Full `./scripts/verify.sh` — run with CI as arbiter (local VM shows the
  known 180s-per-package cap artifact on unrelated packages; see PR
  checks). Live gateway / live provider acceptance: NOT RUN (no
  credentials, zero live calls).

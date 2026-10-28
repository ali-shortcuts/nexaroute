# NexaFlow Phase 5 — Integration

Base: main `e6c3cdf3` + Phase 4 registry head `f90987c` (PR #675's
commit, as the branch base). This phase adds the workflow runtime
engine, completes the backend wiring, and makes the five templates
instantiable into runnable workflows. It does not merge, release, or
tag anything, and it does not claim anything listed under
"Unsupported" below.

## What was built

- `internal/nexaflow/engine.go` — `Engine`, `Workflow`, `Step`,
  `Run`, `RunStore`, `MemoryRunStore`. Validated DAG execution
  through `Registry.Execute`: topological order (declaration order
  breaks ties), input bindings from prior step outputs
  (`stepID.field`, `stepID.a.b`, whole output as `stepID`), from
  workflow input (`$input.field`), and inside loop iterations
  (`loop.item`, `loop.index`).
- `internal/nexaflow/composite_backend.go` — `CompositeBackend`
  assembling a full `Backend` from the pieces that own each
  subsystem, plus `GatewayClient.ResolveSelector` (validated against
  the gateway's own `GET /v1/models` list) and
  `GatewayClient.ResolveSecretRef` (fail-closed without a resolver).
  No subsystem is reimplemented: chat/MCP/video/egress go through
  `GatewayClient` HTTP calls to the gateway that owns them; health
  and compat go through the in-process manager/store; usage and
  budget go through the in-process tracker/ledger (read-only); eval
  goes through `eval.Runner` replay.
- `internal/nexaflow/templates_workflow.go` —
  `Template.InstantiateWorkflow`, which resolves placeholders,
  coerces placeholder strings to number/boolean schema types,
  fills remaining inputs from `extraInputs` (keyed by step id), and
  validates the result against the registry. It fails with a precise
  error naming the missing step and field; it never returns a
  runnable-looking workflow with holes.
- Template fixes in `templates.go` (a genuine gap, not a new
  feature): template nodes now carry `Input` and `Bindings`, and
  the five templates bind real values — the health-alert template
  gains the `condition` step it was missing (its branch step had no
  condition source before), the video template wires
  submit→status→output by `job_id`, the report template wires the
  budget key/threshold into `budget_check`, and the notification
  template wires the workflow-input error into `error_handler`.
  The old `Instantiate` is unchanged in behavior (it now also
  resolves `Input` placeholders).

## Engine semantics (the contract)

- **Validation before execution.** Unknown nodes, duplicate step
  ids, edge cycles, unknown edge endpoints, missing required inputs
  (static or bound), unknown input/config keys, unresolved
  `{{...}}` placeholders, branch gates without a preceding branch
  step, loop steps without a preceding `loop_plan` step, and
  policy-requiring nodes on a non-approved engine are all rejected
  before any step runs.
- **Branch.** A `branch` step records `selected_branch`. Later
  steps tagged `Branch` run only on a match; unselected steps are
  recorded `skipped` and never execute.
- **Loop.** A step with `IterateFrom` runs once per `loop_plan`
  iteration. Hard cap: `Engine.MaxLoopIterations`, default 10,
  configurable up to 100 (the `loop_plan` node's own max). A plan
  longer than the cap fails the run with `loop_iteration_cap`
  before any iteration executes — no partial side effects.
- **Subworkflow.** A step with `Subworkflow` (and no `NodeID`)
  runs a nested workflow whose `$input` scope is the step's
  resolved input. Depth cap: `Engine.MaxDepth`, default 3, never
  above 3. There is deliberately no `nexaroute.control.subworkflow`
  registry node: subworkflow is an engine construct, not an
  advertised node, and the matrix does not invent one.
- **Wait.** The `wait` node uses a real context-aware timer. The
  effective bound is the smaller of the node's default timeout
  (5m, applied by `Registry.Execute`) and the configured
  `max_duration` (default 5m). Cancellation always wins over the
  timer and reports `cancelled`, never success.
- **Approval pause/resume.** An approval step with no decision
  pauses the run instead of erroring. `Run` returns status
  `paused` with a one-shot resume token; `Resume(token,
  decision)` continues from exactly that step, with the resuming
  principal recorded as approver (so self-approval is still
  rejected by the node). The run's principal does not change on
  resume. Paused state lives in a `RunStore`; the bundled
  `MemoryRunStore` is in-memory only and tokens die with the
  process.
- **Error policy.** Per-step `OnError`: `propagate` (default)
  fails the run; `stop` ends it with status `stopped` (terminal,
  explicitly not success); `fallback` continues with
  `FallbackOutput`. Fallback does not roll back side effects that
  already happened (e.g. earlier loop iterations stay done). A
  failure inside a subworkflow unwinds to the enclosing
  subworkflow step's policy (fallback continues the parent with
  the fallback value, propagate unwinds further, stop always ends
  the whole run); nested partial outputs are discarded on unwind
  and survive only in history records.
- **Retry.** A step is retried only when the spec's `RetryPolicy`
  allows it: the error code must be in `RetryableCodes`, and for
  billable or irreversible nodes only `timeout` is ever retried.
  Retries re-execute the whole node (including selector
  validation) with no backoff in V1.
- **Failure shape.** Execution outcomes always come back as a
  `Run` with status `completed`, `paused`, `failed`, or
  `stopped` and `Error` set on failure. Only validation/setup
  problems return a Go error with no `Run`. Nothing is ever
  reported as success when it is not.
- **Secrets.** Resolved secret values flow only through the
  backend (e.g. as an `Authorization` header). They never appear
  in workflow definitions, run history, outputs, or errors; engine
  history passes through the same redaction as node outputs.
  `secretRef` fields hold references, never values.

## Known behaviors worth knowing (not defects fixed here)

- Redaction truncates any single output string over 8 KiB before
  the node output bound is checked, so `output_too_large` fires on
  wide outputs (many keys), not on one long string. The E2E bound
  test is built accordingly (400 x 1 KiB values).
- Placeholder detection uses a `{{name}}` pattern, not a bare
  substring scan: ordinary nested JSON contains `}}` whenever an
  object value closes, and the first version of this check
  false-positived on every nested input map.
- The gateway validates selectors twice in the LLM path: once
  against `GET /v1/models` (this package) and once at chat time
  (the gateway's own routing). The first check only proves the
  selector is listed; it never replaces gateway routing.

## Node-by-node completion matrix

Status is exactly one of Implemented (tested) or Unsupported
(labelled, with reason). There are no silent placeholders.

| Node | Status | Real subsystem used | Test evidence | Live limitation |
|---|---|---|---|---|
| nexaroute.llm.chat | Implemented | Gateway `POST /v1/chat/completions` via `GatewayClient`; selector via `GET /v1/models`; gateway does routing | `TestLLMNode`, `TestGatewayClientChatMCPVideo`, `TestGatewayClientResolveSelector`, E2E (a), retry tests | Live provider calls need a running gateway + operator credentials; none were made here |
| nexaroute.provider.health | Implemented | `health.Manager` via `DirectHealthCompatBackend` | `TestProviderNodes`, `TestCompositeBackendDelegation`, template E2E | Read-only snapshot; never probes |
| nexaroute.provider.compat | Implemented | `compat.Store` via `DirectHealthCompatBackend` | `TestProviderNodes`, template E2E | Unknown deployment reports `unknown`, never `supported` |
| nexaroute.eval.run | Implemented (replay only) | `eval.Runner` replay via `ReplayEvalBackend` | `TestEvalReplayNode`, template E2E (`mode=replay`) | **Live-model evaluation: Unsupported** — the admin live path is not exposed as a Go API; scores describe recorded outcomes only |
| nexaroute.mcp.list | Implemented | Gateway `tools/list` via `GatewayClient` | `TestMCPNodes`, `TestGatewayClientChatMCPVideo` | Server allowlist/RPM/limits still enforced gateway-side |
| nexaroute.mcp.call | Implemented | Gateway `tools/call` via `GatewayClient`, gated by BOTH node allowlist and gateway listing | `TestMCPNodes`, E2E (b) — permission_denied, allowlist tool_forbidden, unlisted tool_forbidden, all with zero backend calls | Tool side effects are the tool's own; mutating tools should be gated behind approval |
| nexaroute.video.submit | Implemented | Gateway `POST /v1/video/jobs` via `GatewayClient` (never the queue store directly) | `TestVideoNodes`, `TestGatewayClientChatMCPVideo`, E2E (a), invalid-input E2E (c) | Billable; requires operator policy approval |
| nexaroute.video.status | Implemented | Gateway `GET /v1/video/jobs/{id}` | Same as submit | One poll only; waiting is the runtime's job |
| nexaroute.video.output | Implemented | Gateway status + asset download, metadata only plus bounded bytes | `TestVideoNodes`, `TestGatewayClientChatMCPVideo`, template E2E | No filesystem paths ever returned |
| nexaroute.cost.usage | Implemented | `usage.Tracker` snapshot via `DirectCostBackend`, verbatim basis/completeness | `TestCostNodes`, `TestCompositeBackendDelegation` | Never repriced; `unknown` basis means "not a cost" |
| nexaroute.cost.budget_check | Implemented (report only) | `usage.BudgetLedger` snapshot via `DirectCostBackend` | `TestCostNodes`, template E2E | **Hard-budget enforcement: Unsupported** — reports `over_threshold` only; blocks/reserves nothing |
| nexaroute.trigger.manual | Implemented | Engine entry: `Engine.Run` initial input | `TestEngineTriggerSteps` | Admission/authorization of a run is the runtime host's job |
| nexaroute.trigger.webhook | Implemented (verification only) | HMAC-SHA256 verification + replay cache in-package | `TestWebhookVerifier`, `TestWebhookNodeViaBackend` | **Webhook HTTP listener: Unsupported** — no server is provided by this package |
| nexaroute.trigger.schedule | Implemented (computation only) | Next-fire computation (interval or daily HH:MM UTC) | `TestScheduleNode`, `TestEngineTriggerSteps` | **Scheduler daemon: Unsupported** — no ticking/dispatch |
| nexaroute.http.request | Implemented | `GatewayClient.HTTPDo` over `egress.Policy` (HTTPS except loopback, cross-origin redirects refused, bounded) | `TestHTTPNode`, secret E2E | SSRF note: `GatewayClient` allows loopback by default (for tests/loopback gateways); non-loopback requires HTTPS. Operator review is required before exposing non-loopback URLs. `Authorization` from workflow input is rejected; credentials come only from `secretRef` |
| nexaroute.control.condition | Implemented | Pure | `TestControlNodes`, E2E (a) | No expression language, no code execution |
| nexaroute.control.branch | Implemented | Pure + engine branch gating | `TestEngineBranchSkipsUnselected`, E2E (a) | — |
| nexaroute.control.json_transform | Implemented | Pure allowlisted ops (pick/rename/set_const/delete) | `TestControlNodes`, loop tests | — |
| nexaroute.control.loop_plan | Implemented | Pure plan + engine per-iteration execution, capped | `TestEngineLoopAndCap` | Node allows up to 100; engine cap defaults to 10 |
| nexaroute.control.merge | Implemented | Pure, conflict policy first/last/error | `TestControlNodes`, `TestMergeConflictAndApprovalAndWait` | At most 1000 keys |
| nexaroute.control.wait | Implemented | Real context-aware timer | `TestMergeConflictAndApprovalAndWait`, `TestEngineWaitCancellation`, E2E (d) | Bounded by min(default timeout, max_duration) |
| nexaroute.control.approval | Implemented | Node decision + engine pause/resume | `TestEngineApprovalPauseResume`, E2E (a) | Pause durability is in-memory only (see below) |
| nexaroute.control.error_handler | Implemented | Node mapping + engine per-step OnError policy | `TestEngineErrorPolicies`, template E2E | Sanitized message only; never re-raises raw errors |

## Explicitly Unsupported in V1 (do not read as implemented)

- Live-model evaluation (see eval row).
- Video-job cancellation: no such node exists and none was
  invented; cancelling a job is a video API operation, not a
  workflow node, in this phase.
- Hard-budget enforcement (see budget_check row).
- PostgreSQL-durable run history: paused runs live in a `RunStore`
  and the only bundled store is in-memory. A PG store can
  implement the interface later; no PG durability is claimed.
- Scheduler daemon and webhook HTTP listener (see trigger rows).
- UI and HTTP API wiring: `httpapi` server fields are unexported,
  so server-side wiring must land in package `httpapi` itself as
  a follow-up. This package talks to a gateway over HTTP
  (`GatewayClient`) or in-process where a Go API exists.
- MCP session management: sessions belong to the MCP gateway.
- New `authz` permissions: nodes map only to permissions that
  already exist; run admission is the host's responsibility.

## Verification (local, this branch)

- `gofmt -l internal/nexaflow/` — empty.
- `go vet ./internal/nexaflow/` — clean.
- `go test -count=1 ./internal/nexaflow/` — PASS (all tests,
  including the 5 E2E scenarios and all template tests).
- `go test -count=1 -race ./internal/nexaflow/` — PASS.
- Full `scripts/verify.sh` and full `go test ./...` were NOT run
  (another session owns heavy baselines on this shared VM; CI is
  the wider gate).
- No live provider calls, no network, no real credentials, no paid
  calls. All E2E tests use fakes implementing `Backend` plus
  `httptest` servers for the HTTP paths. Gateway endpoints used by
  `GatewayClient` were verified to exist in `internal/httpapi`
  (`/v1/chat/completions`, `/mcp`, `/v1/video/` handler) and in
  `internal/video/api` (`/v1/video/jobs`, asset routes).

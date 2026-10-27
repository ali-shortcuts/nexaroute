# NexaFlow Node Adapters (`internal/nexaflow`)

## Honesty notice — what this is and is not

There is **no approved NexaFlow registry, runtime, editor, HTTP API, or UI
on main**, and **no open NexaFlow pull request**. Earlier NexaFlow chats
produced briefs only. This package therefore introduces the adapter
registry itself as a **new, engine-agnostic package**,
`internal/nexaflow`, and nothing else. It does **not** claim:

- a workflow runtime (scheduling/ticking, durable pause, per-iteration
  execution, run history);
- an HTTP API surface (no routes are registered anywhere by this package);
- a UI/editor;
- in-process `httpapi` wiring — the `httpapi` Server fields are
  unexported, so server-side wiring must land in `httpapi` as a
  follow-up and is **not** claimed here;
- live-model evaluation (see below);
- hard-budget enforcement (see below);
- new `authz` permissions (see below).

Future runtime/editor wiring is an explicit follow-up. Nothing in this
package should be presented as a working NexaFlow workflow until that
wiring exists and is itself verified.

## Design: adapters, not duplicates

Every node calls an existing subsystem through the `Backend` port
interface (`backend.go`). The package never duplicates:

- model routing — the gateway routes; the LLM node only validates that
  a selector exists (`ResolveSelector`) and then sends it to the gateway;
- MCP sessions — nodes call the existing MCP gateway over JSON-RPC
  (`tools/list`, `tools/call`); session handling stays with the gateway;
- video persistence — submit/status/output go through the video HTTP
  API, never through `queue.JobStore` directly (creating a store row
  alone does not run a job);
- cost accounting — usage and budget nodes read `usage.Tracker` /
  `usage.BudgetLedger` snapshots verbatim and never reprice;
- provider credential stores — secrets are referenced by `secretRef`
  and resolved only by a wired backend resolver, which fails closed
  when absent. No secret value appears in node config, output, or
  error text.

## Cross-cutting guarantees (`Registry.Execute`)

Every execution applies, in order: authentication (anonymous principals
are denied), permission (see table), operator-policy approval for
`RequiresOperatorPolicy` nodes, config/input schema validation with
**unknown keys rejected fail-closed**, input byte bound, per-node
timeout via `context.WithTimeout`, output byte bound, and secret-safe
output/error sanitising (`redact.go`: Bearer tokens replaced, values
under credential keys replaced, messages truncated to 512 chars).
Cancellation propagates through context; a cancelled run reports code
`cancelled` and never a partial success. The package itself never
retries: `Retry.MaxAttempts` (clamped to 1..3 at registration) and
`RetryableCodes` only declare what a future runtime may safely retry.

Schema versions: only `v1` is supported. `Lookup` and `Execute` reject
any other version fail-closed.

## Node registry (23 nodes, all `v1`)

| Node | Permission | Side effect | Retry | Timeout (default / max) | Cancellation / notes |
|---|---|---|---|---|---|
| `nexaroute.llm.chat` | `routing.read` | billable | 2, `timeout` only | 60s / 300s | Selector validated first; stream forced false; prompt never logged, stored, or echoed; output is text + token counts + resolved route. Requires operator policy. Retrying a billable call after an ambiguous failure risks double billing, hence timeout-only. |
| `nexaroute.provider.health` | `providers.read` | read | 3, timeout/backend_error | 10s / 30s | Read-only snapshot; unknown deployment reports `unknown`, never an invented status. |
| `nexaroute.provider.compat` | `providers.read` | read | 3, timeout/backend_error | 10s / 30s | Read-only; unknown capability names rejected; `missing` = required capabilities not `supported`. |
| `nexaroute.eval.run` | `evaluation.run` | read | 2, timeout | 30s / 60s | **Replay only** over caller-supplied outcomes via `eval.Runner`; `upstream_calls` is always 0; suite must exist in `eval.Catalog`; outcomes bounded by `eval.MaxOutcomesPerRun` (512). |
| `nexaroute.mcp.list` | `providers.read` | read | 3, timeout/backend_error | 30s / 60s | Read-only `tools/list`; no session is opened by this package. |
| `nexaroute.mcp.call` | `providers.read` | external | 1 (no retry) | 30s / 120s | Tool must be in **both** the node `allowlist` config and the gateway listing, else `tool_forbidden` (non-retryable); args bounded to 64 KiB; gateway RPM/size/timeout limits still apply. Not retried: tool calls may not be idempotent. |
| `nexaroute.video.submit` | `video.manage` | billable | 1 (no retry) | 30s / 60s | Via POST `/v1/video/jobs`; prompt 1..4000 chars, duration (0,600]s, domain modes/aspects. Requires operator policy. No retry: a retry could double-submit a billable job. Cancellation aborts the HTTP call, it does **not** cancel the video job. |
| `nexaroute.video.status` | `video.manage` | read | 3, timeout/backend_error | 10s / 30s | One poll; waiting for completion is the runtime's job. `terminal` is true for completed/failed/cancelled/expired. |
| `nexaroute.video.output` | `video.manage` | read | 3, timeout/backend_error | 30s / 60s | Asset metadata only, never filesystem paths; bytes only when `asset_id` is given and they fit the output bound (returned as base64). |
| `nexaroute.cost.usage` | `usage.read` | read | 3, timeout/backend_error | 10s / 30s | Snapshot verbatim: `cost_basis`, `cost_completeness`, `unpriced_tokens` surfaced, never hidden or repriced. |
| `nexaroute.cost.budget_check` | `usage.read` | read | 3, timeout/backend_error | 10s / 30s | **Report only**: compares committed+reserved against an operator threshold and reports `over_threshold`. It does not enforce, reserve, settle, or block anything. |
| `nexaroute.trigger.manual` | authenticated principal only | none | 1 | 5s / 10s | Passes a `payload` object through unchanged. |
| `nexaroute.trigger.webhook` | authenticated principal only | none | 1 | 5s / 10s | Verifies one delivery: HMAC-SHA256 over `timestamp + "." + body`, 5-minute window, constant-time compare, 1 MiB body bound, in-memory replay cache (10 000 entries, pruned by window). **Deliberate difference from the video orchestrator pattern, which MACs only the timestamp**: MACing the body as well means a signature cannot be replayed onto a different body. No HTTP server is provided. |
| `nexaroute.trigger.schedule` | authenticated principal only | none | 1 | 5s / 10s | Pure next-fire computation: `interval` duration (e.g. `1h`) or daily `HH:MM` UTC, from optional `last_run`. Ticking/dispatch is the runtime's job; this node never sleeps. |
| `nexaroute.http.request` | authenticated principal only | external | 1 | 30s / 120s | Methods GET/POST/PUT/PATCH/DELETE; `egress.Policy` transport (https except loopback, cross-origin redirects refused), response bounded. `Authorization` in input/config is **rejected**; credentials come only from `auth_secret_ref` resolved by the backend. Requires operator policy. |
| `nexaroute.control.condition` | authenticated principal only | none | 1 | 5s / 10s | One comparison on a dotted field path (`a.b`); operators eq/ne/gt/gte/lt/lte/contains. No expression language, no code. |
| `nexaroute.control.branch` | authenticated principal only | none | 1 | 5s / 10s | Selects `true_branch`/`false_branch` from `condition_result`. Branch execution is the runtime's job. |
| `nexaroute.control.json_transform` | authenticated principal only | none | 1 | 5s / 10s | Allowlisted ops only: `pick`, `rename`, `set_const`, `delete`. `pick` keeps the union of picked fields, then the other ops apply in order. Unknown ops rejected. |
| `nexaroute.control.loop_plan` | authenticated principal only | none | 1 | 5s / 10s | Plans up to `max_iterations` (1..100) over an items array (<=100); reports `truncated` when the tail is dropped. Per-iteration execution is the runtime's job. |
| `nexaroute.control.merge` | authenticated principal only | none | 1 | 5s / 10s | Merges two objects, at most 1000 keys; conflict policy `first` / `last` / `error` (`error` yields `merge_conflict`). |
| `nexaroute.control.wait` | authenticated principal only | none | 1 | 5m / 10m | Real wait up to the configured maximum (default cap 5m), honouring context cancellation via a timer select; a cancelled wait is an error, not success. |
| `nexaroute.control.approval` | authenticated principal only | none | 1 | 5s / 10s | Missing `decision` yields `approval_required` (non-retryable), never success; `approver == requester` is rejected when both are present. Durable pausing while waiting is the runtime's job. |
| `nexaroute.control.error_handler` | authenticated principal only | none | 1 | 5s / 10s | Maps `error_code` to an action (`retry`/`fallback`/`fail` per config policy) with a sanitised message. |

### Permissions note

`internal/authz` defines only: `config.read/write`,
`providers.read/write`, `routing.read/write`, `usage.read`,
`evaluation.run`, `audit.read`, `keys.manage`, `video.manage`. No new
permission was added. Pure control/trigger nodes carry an empty
permission, meaning "authenticated principal only". A
`workflow.execute` permission does **not** exist in the authz map and
was not invented here; admitting a workflow run is a runtime
responsibility.

## Backend ports (`backend.go`)

| Port | Adapted subsystem | How it is wired here |
|---|---|---|
| `ResolveSelector` | route resolver / deployment aliases | Port only; a runtime wires `route.Resolver.Resolve` (virtual-endpoint public models) and router deployment alias lookup. The gateway still routes. |
| `Chat` | gateway `/v1/chat/completions` | `GatewayClient` (`http_backend.go`), stream forced false. |
| `ProviderHealth` | `health.Manager` | `DirectHealthCompatBackend` (direct, no HTTP) or a gateway-backed port. |
| `Compat` | `compat.Store` | `DirectHealthCompatBackend` (direct, no HTTP); scorecard via `Contract.Scorecard`. |
| `RunEvalReplay` | `eval.Runner` | `ReplayEvalBackend` — `eval.Runner.Run` with a replay executor; no network, no model calls. |
| `MCPList` / `MCPCall` | MCP gateway `/mcp` JSON-RPC | `GatewayClient`; gateway-side server allowlist is `config.MCPServerConfig.Tools`, plus RPM, `MaxRequest/ResponseBytes`, `CallTimeoutSeconds`. |
| `VideoSubmit` / `VideoStatus` / `VideoAsset` | video API | `GatewayClient` against `/v1/video/jobs`, `/v1/video/jobs/{id}`, `/v1/video/jobs/{id}/assets/{assetID}`. |
| `UsageSnapshot` / `BudgetSnapshot` | `usage.Tracker` / `usage.BudgetLedger` | `DirectCostBackend` (direct, no HTTP). |
| `HTTPDo` | egress-guarded HTTP | `GatewayClient.HTTPDo` via `egress.Policy.NewTransport` + `egress.RefuseCrossOriginRedirects`. |
| `ResolveSecretRef` | secret store | Port only; fails closed when no resolver is wired. |

`FakeBackend` (`backend_test.go`) is a test double only and is not a
production code path. Tests use deterministic fakes and `httptest`
servers; there is no network beyond loopback, no paid API call, and no
real credential anywhere in code or tests.

## What is NOT implemented (do not claim otherwise)

- **Runtime, HTTP API, UI/editor**: absent; this package is the adapter
  registry only.
- **Live-model evaluation**: the admin live path
  (`httpapi` admin evaluation run-live) is not exposed as a Go API, so
  no honest live-evaluation node exists here. Replay scores must never
  be presented as live scores.
- **Hard-budget enforcement**: a hard-budget design proposal is pending
  owner review and is **not** implemented. `budget_check` reports only.
- **MCP session duplication, video persistence duplication, cost
  re-pricing, credential-store duplication**: explicitly avoided by
  design (see above).
- **Video job cancellation** as a node: cancellation of the node call
  does not cancel the underlying video job.
- **Webhook/approval/schedule execution**: verification, decision, and
  next-fire computation only; listening, ticking, dispatch, and durable
  pausing belong to the runtime.

## Templates (inert by construction)

`templates.go` defines exactly five starter templates. A template has
no `Run` method and cannot execute anything. `Instantiate` only
validates: every placeholder must be filled, and a policy-requiring
template requires `policyApproved`. `Published` is always `false` on
instantiated templates; publishing is an explicit operator act in the
runtime. Templates are inert until explicitly configured, tested, and
published, and billable or irreversible steps inside them still require
operator policy at node execution time.

| Template | Nodes | Requires policy | Notes |
|---|---|---|---|
| `model-health-alert` | health + compat + branch | no | Read-only; recommends a fallback branch, never switches routing. |
| `model-compat-evaluation` | compat + eval replay | no | Read-only; replay scores only, no live evaluation. |
| `video-generation-pipeline` | video submit + status + output | yes | Billable submit step. |
| `scheduled-operational-report` | schedule + usage + budget_check | no | Read-only report; budget check does not enforce. |
| `failure-notification-diagnostics` | error_handler + http | yes | HTTP notification step is external; the notification URL is a placeholder (`{{notify_url}}`) only — no real URL or secret is in any template config. |

## Side-effect and retry semantics (summary)

- **none / read**: safe to inspect; read nodes are retryable on
  timeout/backend errors. Pure control nodes are deterministic and
  need no retry.
- **external** (MCP call, HTTP request): the effect belongs to the
  callee; MCP call and HTTP request are **not** retried by policy
  (MaxAttempts 1) because the call may not be idempotent.
- **billable** (LLM chat, video submit): require explicit operator
  policy approval at execution time; LLM chat retries only on timeout,
  video submit never retries, because a retry after an ambiguous
  failure risks double billing / double submission.
- **irreversible**: no node in this registry declares the irreversible
  side effect directly; the failure-notification template's HTTP step
  is flagged in its description as external and irreversible once sent,
  which is why the template requires policy approval.

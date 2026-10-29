# NexaFlow Node Adapters (`internal/nexaflow`)

## Honesty notice — what this is and is not

There is **no approved NexaFlow registry, runtime, editor, HTTP API, or UI
on main**. This package introduces the adapter registry itself as a
**new, engine-agnostic package**, `internal/nexaflow`, on the branch
that carries it (PR #675, unmerged at the time of writing); the Phase 1
foundation work (the workflow definition contract, ADR 0008, and the
architecture document) is layered on the same branch. It does **not**
claim:

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
**unknown keys rejected fail-closed**, config byte bound (256 KiB) and
input byte bound, per-node timeout via `context.WithTimeout`, output
byte bound, and secret-safe output/error sanitising (`redact.go`:
Bearer/Basic credentials replaced, values under credential keys —
including hyphenated header spellings such as `x-api-key` and
`set-cookie`, and typed `map[string]string` values such as response
headers — replaced, messages truncated to 512 chars, redaction
recursion depth-bounded at 100). Cancellation propagates through
context; a cancelled run reports code `cancelled` and never a partial
success. The package itself never retries: `Retry.MaxAttempts`
(values outside 1..3 are rejected at registration, not clamped) and
`RetryableCodes` only declare what a future runtime may safely retry,
and a timeout is reported retryable only when the node's policy both
allows more than one attempt and lists `timeout` — a timeout on a
node that does not declare it safe is ambiguous, not an invitation to
repeat the call.

Schema versions: only `v1` is supported. `Lookup` and `Execute` reject
any other version fail-closed.

## Node registry (23 nodes, all `v1`)

| Node | Permission | Side effect | Retry | Timeout (default / max) | Cancellation / notes |
|---|---|---|---|---|---|
| `nexaroute.llm.chat` | `routing.read` | billable | 1 (no retry) | 60s / 300s | Selector validated first; stream forced false; prompt never logged, stored, or echoed; output is text + token counts + resolved route. Requires operator policy. **No retry (Phase 7)**: a timeout on a billable call is ambiguous and this node sends no idempotency key, so any retry could double-bill; a future safe retry needs an idempotency key first. |
| `nexaroute.provider.health` | `providers.read` | read | 3, timeout/backend_error | 10s / 30s | Read-only snapshot; unknown deployment reports `unknown`, never an invented status. |
| `nexaroute.provider.compat` | `providers.read` | read | 3, timeout/backend_error | 10s / 30s | Read-only; unknown capability names rejected; `missing` = required capabilities not `supported`. |
| `nexaroute.eval.run` | `evaluation.run` | read | 2, timeout | 30s / 60s | **Replay only** over caller-supplied outcomes via `eval.Runner`; `upstream_calls` is always 0; suite must exist in `eval.Catalog`; outcomes bounded by `eval.MaxOutcomesPerRun` (512). |
| `nexaroute.mcp.list` | `providers.read` | read | 3, timeout/backend_error | 30s / 60s | Read-only `tools/list`, scoped to the requested server: the gateway returns namespaced names (`server__tool`) and the client returns only that server's tools with the prefix stripped. No session is opened by this package. |
| `nexaroute.mcp.call` | `providers.read` | external | 1 (no retry) | 30s / 120s | Tool must be in **both** the node `allowlist` config (at most 1000 entries) and the gateway listing, else `tool_forbidden` (non-retryable); args bounded to 64 KiB; the client namespaces the call as `server__tool` because the gateway rejects bare names; gateway RPM/size/timeout limits still apply. Not retried: tool calls may not be idempotent. |
| `nexaroute.video.submit` | `video.manage` | billable | 1 (no retry) | 30s / 60s | Via POST `/v1/video/jobs`; prompt 1..4000 chars, duration (0,600]s, domain modes/aspects. Requires operator policy. No retry: a retry could double-submit a billable job. Cancellation aborts the HTTP call, it does **not** cancel the video job. |
| `nexaroute.video.status` | `video.manage` | read | 3, timeout/backend_error | 10s / 30s | One poll; waiting for completion is the runtime's job. `terminal` is true for completed/failed/cancelled/expired. |
| `nexaroute.video.output` | `video.manage` | read | 3, timeout/backend_error | 30s / 60s | Asset metadata only, never filesystem paths; bytes only when `asset_id` is given and they fit the output bound (returned as base64). Job/asset ids are path-escaped, and an asset not listed on the job is refused (`asset_not_found`) instead of downloaded. |
| `nexaroute.cost.usage` | `usage.read` | read | 3, timeout/backend_error | 10s / 30s | Snapshot verbatim: `cost_basis`, `cost_completeness`, `unpriced_tokens` surfaced, never hidden or repriced. |
| `nexaroute.cost.budget_check` | `usage.read` | read | 3, timeout/backend_error | 10s / 30s | **Report only**: compares committed+reserved against an operator threshold and reports `over_threshold`. It does not enforce, reserve, settle, or block anything. |
| `nexaroute.trigger.manual` | authenticated principal only | none | 1 | 5s / 10s | Passes a `payload` object through unchanged. |
| `nexaroute.trigger.webhook` | authenticated principal only | none | 1 | 5s / 10s | Verifies one delivery: HMAC-SHA256 over `timestamp + "." + body`, 5-minute window, constant-time compare, 1 MiB body bound, in-memory replay cache (10 000 entries, pruned by window). **Deliberate difference from the video orchestrator pattern, which MACs only the timestamp**: MACing the body as well means a signature cannot be replayed onto a different body. No HTTP server is provided. |
| `nexaroute.trigger.schedule` | authenticated principal only | none | 1 | 5s / 10s | Pure next-fire computation: `interval` duration (e.g. `1h`) or daily `HH:MM` UTC, from optional `last_run`. Ticking/dispatch is the runtime's job; this node never sleeps. |
| `nexaroute.http.request` | authenticated principal only | external | 1 | 30s / 120s | Methods GET/POST/PUT/PATCH/DELETE; `egress.Policy` transport (https except loopback, cross-origin redirects refused, DNS answers re-checked at dial, loopback only by spelling), response bounded, whole request bounded at 120s. Any credential header in input (`Authorization`, `Cookie`, `Set-Cookie`, `Proxy-Authorization`, `Api-Key`, `X-Api-Key`, `X-Goog-Api-Key`, `X-Auth-Token`, `X-Access-Token`) is **rejected**, and `HTTPDo` re-checks at its choke point; credentials come only from `auth_secret_ref` resolved by the backend. URLs with embedded userinfo are rejected at the node. Requires operator policy. Limitation: the shared egress policy rejects URLs with a query string, so query-bearing webhook URLs are refused by this node today (see findings matrix). |
| `nexaroute.control.condition` | authenticated principal only | none | 1 | 5s / 10s | One comparison on a dotted field path (`a.b`); operators eq/ne/gt/gte/lt/lte/contains. No expression language, no code. |
| `nexaroute.control.branch` | authenticated principal only | none | 1 | 5s / 10s | Selects `true_branch`/`false_branch` from `condition_result`. Branch execution is the runtime's job. |
| `nexaroute.control.json_transform` | authenticated principal only | none | 1 | 5s / 10s | Allowlisted ops only: `pick`, `rename`, `set_const`, `delete`. `pick` keeps the union of picked fields, then the other ops apply in order. Unknown ops rejected. |
| `nexaroute.control.loop_plan` | authenticated principal only | none | 1 | 5s / 10s | Plans up to `max_iterations` (1..100) over an items array (<=100); reports `truncated` when the tail is dropped. Per-iteration execution is the runtime's job. |
| `nexaroute.control.merge` | authenticated principal only | none | 1 | 5s / 10s | Merges two objects, at most 1000 keys; conflict policy `first` / `last` / `error` (`error` yields `merge_conflict`). |
| `nexaroute.control.wait` | authenticated principal only | none | 1 | 5m / 10m | Real wait up to the configured maximum (default cap 5m), honouring context cancellation via a timer select; a cancelled wait is an error, not success. |
| `nexaroute.control.approval` | authenticated principal only | none | 1 | 5s / 10s | Missing `decision` yields `approval_required` (non-retryable), never success; approver and requester must both be identified and must differ — anonymous or self-approval is rejected. Approver identity is caller-asserted: the runtime must bind the decision to an authenticated approval record before passing it here. Durable pausing while waiting is the runtime's job. |
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
| `MCPList` / `MCPCall` | MCP gateway `/mcp` JSON-RPC | `GatewayClient`; gateway-side server allowlist is `config.MCPServerConfig.Tools`, plus RPM, `MaxRequest/ResponseBytes`, `CallTimeoutSeconds`. The client scopes listings to one server and namespaces calls as `server__tool` (the gateway rejects bare names). |
| `VideoSubmit` / `VideoStatus` / `VideoAsset` | video API | `GatewayClient` against `/v1/video/jobs`, `/v1/video/jobs/{id}`, `/v1/video/jobs/{id}/assets/{assetID}`, with path-escaped ids and unlisted-asset refusal. |
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
validates: every placeholder must be filled, every referenced node
must exist in the registry, every `{{name}}` occurrence in a config
string is substituted (not only whole-string matches), and any
placeholder pattern left after substitution is an error, never a
value to execute against; a policy-requiring template requires
`policyApproved`. `Published` is always `false` on instantiated
templates; publishing is an explicit operator act in the runtime.
Templates are inert until explicitly configured, tested, and
published, and billable or irreversible steps inside them still
require operator policy at node execution time. (Phase 7 fix: the
previous `Instantiate` computed substitutions into a local variable
and discarded them, so no placeholder ever resolved — a runtime
would have executed literal `{{...}}` text.)

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
  policy approval at execution time; neither retries, because a retry
  after an ambiguous failure risks double billing / double submission
  and neither node sends an idempotency key today.
- **irreversible**: no node in this registry declares the irreversible
  side effect directly; the failure-notification template's HTTP step
  is flagged in its description as external and irreversible once sent,
  which is why the template requires policy approval.

## Phase 7 security findings matrix (2026-10-28)

Scope note: at Phase 7 start the only NexaFlow code in existence was
this registry (PR #675, unmerged) plus a local-only Phase 1 definition
contract (branch `nexaflow/phase1-foundation`, commit `bb4f349`,
unpushed, no PR). No runtime, HTTP API, UI, store, scheduler, audit
store, or metrics code exists anywhere, so protections that need those
surfaces cannot be verified here and are marked NOT_IMPLEMENTED
instead of claimed. Fixes below are in this package and covered by
`security_phase7_test.go` (red-proven for the HTTPDo choke point,
video path escape, and MCP scoping; the Set-Cookie redaction and
template substitution defects were observed failing before their
fixes during development).

### Fixed in this pass (code + regression test)

| # | Finding | Fix |
|---|---|---|
| F1 | Template `Instantiate` computed placeholder substitutions and discarded them: no placeholder ever resolved, so a runtime would execute literal `{{...}}` text. | Substitute into the stored config; embedded placeholders replaced; any leftover `{{...}}` pattern is an error; referenced nodes must exist in the registry. |
| F2 | `redactJSONValue` traversed only `map[string]any`: the HTTP node's response headers (`map[string]string`) reached run history with `Set-Cookie` intact. | Redact `map[string]string` and `[]string` the same way; add `cookie`, `set-cookie`, `proxy-authorization`, `auth-token`, hyphen-normalised `x-api-key` to secret keys; redact Basic credentials in errors. |
| F3 | HTTP node accepted credential headers beyond `Authorization` (`Cookie`, `X-Api-Key`, `Proxy-Authorization`, …) from workflow input and forwarded them. | Node rejects the full credential-header set; `HTTPDo` re-checks at its choke point (only the node's own secretRef `Authorization` passes); URLs with embedded userinfo rejected at the node. |
| F4 | Video job/asset ids were concatenated unescaped into gateway paths: `../admin` or `a?x=1` rewrote the request path/query. | `url.PathEscape` on both ids. |
| F5 | `VideoAsset` downloaded an asset even when it was not listed on the job, then invented metadata afterwards. | Unlisted asset fails closed with `asset_not_found`; no download is attempted. |
| F6 | MCP via `GatewayClient` was broken against the real gateway in both directions: the gateway returns namespaced `server__tool` names and rejects bare `tools/call` names, so the node never saw a listed tool and every call failed — and server selection was effectively nothing. | `MCPList` filters to the requested server and strips its prefix; `MCPCall` namespaces the call. |
| F7 | `GatewayClient` default client followed cross-origin redirects (stdlib strips only `Authorization` on cross-host redirects) and had no total bound when used outside `Execute`. | Default client refuses cross-origin redirects (5 hops) and bounds the whole request at 120s; `HTTPDo` likewise (120s, 1 MiB request body bound). |
| F8 | `Execute` reported every timeout as retryable, including for MaxAttempts-1 billable nodes whose timeout is an ambiguous outcome. | Timeout retryability follows the node's declared policy only. |
| F9 | LLM chat declared a timeout retry although it sends no idempotency key: retrying a timed-out billable call could double-bill. | LLM node is now MaxAttempts 1, no retryable codes, with the reason documented. |
| F10 | Approval accepted decisions with no approver, no requester, or (when requester was omitted) undetectable self-approval. | Decision requires identified approver and requester, and they must differ. Approver identity remains caller-asserted (see R1). |
| F11 | `Execute` bounded input bytes but not config bytes; MCP allowlist entry count unbounded. | Config bound 256 KiB at `Execute`; MCP allowlist capped at 1000 entries. |
| F12 | Untrusted numbers defeated range checks: NaN passes every `<`/`>` comparison and cannot be JSON-encoded; condition parsing accepted trailing junk (`123abc` parsed as 123). | Finite checks on wait, budget threshold, LLM tokens/temperature, video duration, loop iterations; strict `ParseFloat` for condition strings. |
| F13 | Redaction recursed without a depth bound over caller-constructed maps. | Depth bound 100; deeper values become `[TRUNCATED]`. |

### Verified strong already (no change needed)

- Egress: userinfo/query/fragment rejected, private/link-local/loopback-by-name blocked, DNS answers re-checked at dial and the connection is pinned to a checked IP, same-origin-only redirects, TLS 1.2 minimum. SSRF and DNS-rebinding cases are covered by `internal/egress` tests, not re-proven here.
- Webhook verifier: HMAC over timestamp + body, 5-minute window, constant-time compare, replay cache (10 000 entries, window-pruned, mutex-guarded — concurrent replay test proves exactly one win).
- Schema validation: unknown input/config keys rejected fail-closed; version pinning; permission and operator-policy gates in `Execute`.
- Control surface: no expression language, no code execution; JSON transform is an allowlisted op set; loop planning is bounded (100 items, 100 iterations) and does not execute.

### NOT_IMPLEMENTED — needs a runtime/API/store (Phase 8 requirements)

| # | Guarantee | Why it cannot be claimed yet | Phase 8 requirement |
|---|---|---|---|
| R1 | Approval authenticity and audit trail | Approver identity is caller-asserted input; there is no approval record, signature, or audit store. | Bind decisions to an authenticated approval record (approver + approval id + timestamp) before the node; append-only audit events for publication, execution, approval, cancellation, and admin acts, with redacted payloads only. |
| R2 | Tenant/workspace isolation | No tenant field on `RunContext`, no store, no API. `TenantScope` in the Phase 1 contract is a label, not a boundary, by its own comment. | Tenant-scoped definitions, runs, secrets, and backends; gateway token tenant checked on every port call; cross-tenant access tests. |
| R3 | Global/workflow budget enforcement | `budget_check` is report-only; no reservation is taken before billable nodes, and the in-process `BudgetLedger` is single-process only. | Reserve-then-settle around billable nodes via an atomic shared backend (the pending hard-budget design), fail closed when the backend is unavailable; never present estimates as exact charges. |
| R4 | Durable exactly-once / duplicate dispatch | No run store, no idempotency key on LLM/video/MCP/HTTP calls. | Durable run + step records with idempotency keys and claim/lease semantics; ambiguous outcomes recorded, never silently retried. |
| R5 | Durable webhook replay protection | The replay cache is process-local: a restart resets it, so a delivery could verify again after restart. | Persist seen signatures (or gateway-side dedup) with the same window semantics. |
| R6 | Rate limiting, concurrency limits, metrics | No HTTP surface, no scheduler, no metrics. Node-level bounds (timeouts, bytes, counts) exist; workflow-level parallelism and queue depth do not. | Per-endpoint and per-tenant rate limits, bounded run queue and worker pool, bounded-cardinality metrics (duration, error, run-state) with redacted logs. |
| R7 | Run-history secret safety | Payloads (webhook bodies, manual payloads, tool outputs) flow through nodes by design; only node outputs/errors are redacted here. A history store must not store raw payloads or secrets. | History store redacts/encrypts payloads, drops secret-bearing fields, and is covered by redaction tests on stored records. |
| R8 | Definition graph bounds (Phase 1 contract, local-only) | `ValidateDefinition` has no node/edge count or depth bound, and its reachability/cycle walks are recursive: a deep or huge definition can exhaust the stack. The 256 KiB config bound in `Execute` is not applied by `ValidateDefinition` either. | Bound nodes/edges/triggers and graph depth in `ValidateDefinition` (iterative walks or an explicit depth cap) and enforce the config byte bound there, on the Phase 1 branch before its draft PR. |
| R9 | HTTP node query URLs | The shared egress policy rejects URLs with a query string, so query-bearing webhook URLs are refused (fail-closed, but a functional limitation). | If query URLs are ever needed, add a dedicated HTTP-node egress profile with the same IP/redirect controls — do not relax the shared provider policy. |
| R10 | UI checks | No UI exists. | UI checks belong to the editor phase; this pass ran no browser checks because there is nothing to render. |

### Live / external evidence

None claimed. All Phase 7 tests run against deterministic fakes and
loopback `httptest` servers; no live provider, gateway, Vault, or
credential was used. Multi-host, HA, and live-tenant guarantees remain
BLOCKED on operator infrastructure, as in prior phases.

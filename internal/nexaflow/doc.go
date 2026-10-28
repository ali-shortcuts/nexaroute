// Package nexaflow is an adapter registry plus a workflow runtime for
// NexaFlow.
//
// HONESTY NOTICE — READ FIRST: as of main e6c3cdf there is no approved
// NexaFlow registry, runtime, editor, HTTP API, or UI in this repository.
// This package introduces the adapter registry (Phase 4) and, in Phase 5,
// a workflow runtime (Engine) that executes validated DAGs of registry
// nodes, plus template instantiation into runnable workflows. It does
// NOT claim HTTP API wiring, UI integration, a scheduler daemon, a
// webhook listener, or durable run history: paused runs live in a
// RunStore, and the bundled store is in-memory only. Wiring an HTTP
// surface or an editor on top of this runtime is an explicit follow-up,
// and the httpapi Server fields are unexported, so server-side wiring
// must land in package httpapi itself.
//
// What this package does: it adapts existing, verified NexaRoute
// capabilities behind a versioned node registry. Every node declares a
// versioned schema, a permission from internal/authz, a timeout, a
// cancellation behaviour (context propagation), bounded resource usage, and
// secret-safe errors. It never duplicates the subsystems it adapts:
// model routing still happens in the gateway, MCP sessions still belong to
// the MCP gateway, video persistence still belongs to the video job store,
// cost accounting still belongs to internal/usage, and provider credentials
// still belong to their existing stores. Adapters call those subsystems
// through the Backend port interface; they do not reimplement them.
//
// Explicitly NOT implemented here:
//   - durable run history: paused runs are held by a RunStore; the
//     bundled MemoryRunStore is in-memory only and tokens die with
//     the process. A PostgreSQL store is a follow-up, not a claim;
//   - a scheduler daemon or webhook HTTP listener: schedule nodes
//     compute next-fire times and webhook nodes verify one delivery;
//     ticking and listening belong to a future runtime host;
//   - live-model evaluation: only replay of caller-supplied outcomes via
//     eval.Runner is exposed; live scores are never fabricated;
//   - hard-budget enforcement: budget_check only compares a snapshot
//     against an operator threshold and reports; it does not enforce,
//     reserve, or block anything;
//   - new authz permissions: nodes map only to the permissions that already
//     exist in internal/authz. Pure control/trigger nodes require only an
//     authenticated principal; a workflow.execute permission is a runtime
//     responsibility and is not invented here;
//   - MCP session management: nodes call the existing MCP gateway over
//     JSON-RPC and pre-check the configured tool allowlist;
//   - in-process httpapi wiring: GatewayClient talks to a gateway base URL
//     over HTTP, which is how tests exercise it (httptest); direct
//     in-process adapters exist only where the subsystem exposes a usable
//     Go API (health, compat, usage, eval replay).
package nexaflow

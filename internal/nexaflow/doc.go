// Package nexaflow is an engine-agnostic adapter registry for NexaFlow.
//
// HONESTY NOTICE — READ FIRST: as of main e6c3cdf there is no approved
// NexaFlow registry, runtime, editor, HTTP API, or UI in this repository,
// and no open NexaFlow pull request. The three earlier NexaFlow chats
// produced briefs only. This package therefore introduces the adapter
// registry itself as a new package, internal/nexaflow, and NOTHING ELSE.
// It does not claim runtime execution, HTTP API wiring, or UI integration.
// Wiring a workflow runtime, an HTTP surface, or an editor on top of this
// registry is an explicit follow-up, and the httpapi Server fields are
// unexported, so server-side wiring must land in package httpapi itself.
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
// Phase 7 hardening (2026-10-28) landed in this package only: secret
// redaction gaps, credential-header smuggling, video path injection,
// MCP server scoping, gateway redirect handling, ambiguous billable
// timeouts, approval identity, config bounds, malformed numbers, and
// template placeholder substitution (which previously never resolved).
// The full findings matrix, including guarantees that still cannot be
// claimed without a runtime, API, or store, is in
// docs/nexaflow-node-adapters.md ("Phase 7 security findings matrix").
//
// Explicitly NOT implemented here:
//   - a workflow runtime (scheduling, ticking, durable pause, per-iteration
//     execution): schedule/loop/approval nodes expose pure computations or
//     decisions and document that execution is the runtime's job;
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

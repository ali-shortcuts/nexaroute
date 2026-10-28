// Package nexaflow is the NexaFlow workflow package: an engine-agnostic
// adapter registry (PR #675) plus, since Phase 8, the runtime engine,
// the durable store, the scheduler, webhook handling, and the production
// backend composite, layered on the Phase 1 definition contract
// (workflow.go, ADR 0008). All of it lives on the Phase 8 branch,
// unmerged at the time of writing; nothing here is on main. The HTTP
// API surface and the UI page live in package httpapi (the Server
// fields are unexported, so server-side wiring lands there), and a
// visual canvas editor is still an explicit follow-up: the Phase 8 UI
// edits definition JSON.
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
//   - a visual canvas editor: the Phase 8 UI edits definition JSON;
//     layout as a sidecar and drag-and-drop authoring are follow-ups;
//   - a multi-instance store: FileStore is single-host; a PostgreSQL
//     store is a later phase;
//   - live-model evaluation: only replay of caller-supplied outcomes via
//     eval.Runner is exposed; live scores are never fabricated;
//   - hard-budget enforcement: budget_check only compares a snapshot
//     against an operator threshold and reports; it does not enforce,
//     reserve, or block anything;
//   - new node permissions: nodes map only to the permissions that already
//     exist in internal/authz. Pure control/trigger nodes require only an
//     authenticated principal. (The admin API added workflow.read /
//     workflow.write as API permissions in Phase 8; those gate routes,
//     not nodes.)
//   - MCP session management: nodes call the existing MCP gateway over
//     JSON-RPC and pre-check the configured tool allowlist;
//   - in-process httpapi wiring inside this package: GatewayClient talks
//     to a gateway base URL over HTTP, which is how tests exercise it
//     (httptest); direct in-process adapters exist only where the
//     subsystem exposes a usable Go API (health, compat, usage, eval
//     replay). The server-owned composite lives in package httpapi
//     (AttachNexaflow).
package nexaflow

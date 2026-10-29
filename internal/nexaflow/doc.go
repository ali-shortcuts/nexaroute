// Package nexaflow is an engine-agnostic adapter registry for NexaFlow.
//
// HONESTY NOTICE — READ FIRST: the registry, the durable execution
// engine and stores, the httpapi admin surface, and the webhook service
// are on main (merged PR #693); the Phase 1 foundation work (the
// workflow definition contract in workflow.go, ADR 0008, and the
// architecture document) and Phase 2 (engine.go, store.go,
// postgres_store.go; see docs/nexaflow/RUNTIME.md) are part of that
// line. There is still no editor/studio UI and no scheduler daemon.
// Templates instantiate into validated Definitions via
// Template.InstantiateDefinition (placeholder substitution into Config
// and Input, with schema coercion); the httpapi Server fields are
// unexported, so server-side wiring lands in package httpapi itself,
// and cmd/gateway does not yet attach the NexaFlow service.
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
//   - ticking/scheduler execution and per-iteration loop driving:
//     schedule/loop nodes expose pure computations or decisions; the
//     Phase 2 engine executes DAGs, approvals, and recovery, but there
//     is no scheduler daemon, no timer service, and no webhook
//     listener yet (P1.4);
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

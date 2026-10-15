# M6-1 — MCP registry/gateway

NexaRoute brokers Model Context Protocol tool servers behind one
client-key-authenticated endpoint, `POST /mcp` (JSON-RPC 2.0 over
streamable HTTP):

- **Registry**: `mcp.servers[]` in config — id, name, url
  (operator-trusted, http/https, no embedded credentials),
  enabled, optional auth header value taken from the env var named
  by `auth_env` (never inline), `allowed_tenants`, and a tool
  allowlist (`tools`, empty = all).
- **Brokering**: the gateway speaks to each upstream lazily —
  initialize handshake, `Mcp-Session-Id` cached per server,
  one re-initialize + retry on session loss. Client methods:
  initialize, ping, tools/list, tools/call; notifications are
  accepted (202). Upstream responses may be JSON or SSE-framed.
- **Namespacing**: tools are exposed as `<server_id>__<tool>` so
  one flat list can aggregate many servers without collisions.
  tools/list aggregates per-server with failure isolation (a dead
  server is skipped and evented, never fails the whole list) and
  a 30 s per-server list cache.
- **Per-tenant allowlists**: a server with `allowed_tenants` is
  invisible (list) and unusable (call) to keys of other tenants.
  The tool allowlist applies on top. Legacy keys have tenant "".
- **Limits**: per-key call rate (mcp.rpm, falling back to
  client_auth.rpm), per-call timeout, and a response size cap.
- **Audits**: every tools/call emits an `mcp_tool_call` event
  (server, tool, tenant, outcome, argument byte count — never
  argument content) and `nexaroute_mcp_calls_total{server,result}`
  metrics.

Boundaries: batch JSON-RPC requests are rejected (-32600); MCP
server URLs are operator configuration, dialed directly (the same
trust class as provider base URLs at config time); stdio
transport is not supported (HTTP only).

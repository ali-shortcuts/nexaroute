package nexaflow

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// MCP nodes call the existing MCP gateway over JSON-RPC (tools/list,
// tools/call). They do not open, own, or duplicate MCP sessions, and they
// do not store provider credentials: the gateway's own server allowlist
// (config.MCPServerConfig.Tools), RPM, MaxRequest/ResponseBytes, and
// CallTimeoutSeconds still apply on the gateway side.
//
// The call node adds a second, node-level allowlist in config: a tool is
// callable only if it appears in BOTH the node's allowlist AND the
// backend's tools/list result for that server. Anything else fails with
// code "tool_forbidden", non-retryable. Tool arguments are bounded to
// 64 KiB of JSON. Side effects are the tool's own: the list node is
// read-only, the call node is SideEffectExternal, and a tool that bills
// or mutates is the operator's policy responsibility (the node cannot
// know a tool's side effects, so workflows that call mutating tools
// should gate the call node behind approval in the runtime).
// Retry semantics: list is retryable; call is NOT retried by policy
// (MaxAttempts 1) because a tool call may not be idempotent.
func mcpNodeSpecs() []*NodeSpec {
	return []*NodeSpec{
		{
			ID: "nexaroute.mcp.list", Version: SupportedVersion, Category: "mcp",
			Title:       "MCP tool listing",
			Description: "Lists tools for one MCP server via the existing MCP gateway (tools/list). Read-only; does not open a session.",
			InputSchema: []Field{
				{Name: "server_id", Type: "string", Required: true, Description: "Registered MCP server id."},
			},
			OutputSchema: []Field{
				{Name: "tools", Type: "array", Description: "Tools with name, description, and input schema."},
			},
			ConfigSchema: nil,
			Permission:   authz.ReadProviders, SideEffect: SideEffectRead,
			Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout", "backend_error"}},
			DefaultTimeout: 30 * time.Second, MaxTimeout: 60 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 256 << 10,
			Run: runMCPList,
		},
		{
			ID: "nexaroute.mcp.call", Version: SupportedVersion, Category: "mcp",
			Title:       "MCP tool invocation",
			Description: "Invokes one MCP tool via the existing MCP gateway (tools/call), only if the tool is in both the node allowlist and the gateway listing. Arguments bounded to 64 KiB. Not retried.",
			InputSchema: []Field{
				{Name: "server_id", Type: "string", Required: true, Description: "Registered MCP server id."},
				{Name: "tool", Type: "string", Required: true, Description: "Tool name, exactly as listed."},
				{Name: "arguments", Type: "object", Description: "Tool arguments object."},
			},
			OutputSchema: []Field{
				{Name: "result", Type: "object", Description: "Gateway tools/call result, size-bounded."},
			},
			ConfigSchema: []Field{
				{Name: "allowlist", Type: "array", Required: true, Description: "Allowed tool names, as server__tool entries or bare tool names; a tool must match an entry and appear in the gateway listing."},
			},
			Permission: authz.ReadProviders, SideEffect: SideEffectExternal,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 30 * time.Second, MaxTimeout: 120 * time.Second,
			MaxInputBytes: 128 << 10, MaxOutputBytes: 256 << 10,
			Run: runMCPCall,
		},
	}
}

func runMCPList(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	serverID, ok := getString(rc.Input, "server_id")
	if !ok || strings.TrimSpace(serverID) == "" {
		return nil, NewNodeError("validation_error", false, "input server_id is required")
	}
	tools, err := rc.Backend.MCPList(ctx, serverID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name": t.Name, "description": t.Description, "input_schema": t.InputSchema,
		})
	}
	return map[string]any{"tools": out}, nil
}

// allowlistMatch reports whether tool is allowed for serverID under the
// node allowlist. Entries are "server__tool" (exact pair) or a bare tool
// name (any server). Matching is exact; there are no wildcards.
func allowlistMatch(allowlist []string, serverID, tool string) bool {
	for _, entry := range allowlist {
		if entry == tool || entry == serverID+"__"+tool {
			return true
		}
	}
	return false
}

func runMCPCall(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	serverID, ok := getString(rc.Input, "server_id")
	if !ok || strings.TrimSpace(serverID) == "" {
		return nil, NewNodeError("validation_error", false, "input server_id is required")
	}
	tool, ok := getString(rc.Input, "tool")
	if !ok || strings.TrimSpace(tool) == "" {
		return nil, NewNodeError("validation_error", false, "input tool is required")
	}
	allowlist, ok := getStringSlice(rc.Config, "allowlist")
	if !ok || len(allowlist) == 0 {
		return nil, NewNodeError("validation_error", false, "config allowlist is required and must not be empty")
	}
	args := map[string]any{}
	if _, present := rc.Input["arguments"]; present {
		a, ok := getObject(rc.Input, "arguments")
		if !ok {
			return nil, NewNodeError("validation_error", false, "arguments must be an object")
		}
		args = a
	}
	raw, err := json.Marshal(args)
	if err != nil || len(raw) > 64<<10 {
		return nil, NewNodeError("validation_error", false, "tool arguments exceed 64 KiB")
	}
	if !allowlistMatch(allowlist, serverID, tool) {
		return nil, NewNodeError("tool_forbidden", false, "tool %q is not in the node allowlist", tool)
	}
	tools, err := rc.Backend.MCPList(ctx, serverID)
	if err != nil {
		return nil, err
	}
	listed := false
	for _, t := range tools {
		if t.Name == tool {
			listed = true
			break
		}
	}
	if !listed {
		return nil, NewNodeError("tool_forbidden", false, "tool %q is not listed by the gateway for server %q", tool, serverID)
	}
	result, err := rc.Backend.MCPCall(ctx, serverID, tool, args)
	if err != nil {
		return nil, err
	}
	return map[string]any{"result": result}, nil
}

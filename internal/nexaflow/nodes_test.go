package nexaflow

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

func TestLLMNode(t *testing.T) {
	fb := newFakeBackend()
	out := runNode(t, "nexaroute.llm.chat", ownerPrincipal(),
		map[string]any{"prompt": "hello"}, map[string]any{"route_or_alias": "team/coding"}, fb, true)
	if out["text"] != "fake reply" {
		t.Fatalf("llm text = %v", out["text"])
	}
	if _, hasPrompt := out["prompt"]; hasPrompt {
		t.Fatal("llm output echoes the prompt")
	}
	if fb.ChatCalls != 1 {
		t.Fatalf("chat calls = %d", fb.ChatCalls)
	}
	// Unknown selector must fail before any chat call.
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.llm.chat", "v1")
	fb2 := newFakeBackend()
	if _, err := reg.Execute(t.Context(), spec, ownerPrincipal(),
		map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "nope"}, fb2, true); err == nil {
		t.Fatal("unknown selector accepted")
	} else if fb2.ChatCalls != 0 {
		t.Fatal("chat called for unknown selector")
	}
	// max_tokens bound.
	if _, err := reg.Execute(t.Context(), spec, ownerPrincipal(),
		map[string]any{"prompt": "hi"}, map[string]any{"route_or_alias": "team/coding", "max_tokens": float64(99999)}, fb2, true); err == nil {
		t.Fatal("max_tokens above bound accepted")
	}
}

func TestProviderNodes(t *testing.T) {
	out := runNode(t, "nexaroute.provider.health", ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model"}, nil, newFakeBackend(), false)
	if out["status"] != "healthy" {
		t.Fatalf("health status = %v", out["status"])
	}
	out = runNode(t, "nexaroute.provider.health", ownerPrincipal(),
		map[string]any{"deployment_id": "ghost"}, nil, newFakeBackend(), false)
	if out["status"] != "unknown" {
		t.Fatalf("unknown deployment status = %v, want unknown (never invented)", out["status"])
	}
	out = runNode(t, "nexaroute.provider.compat", ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model", "required_capabilities": []any{"text", "vision"}}, nil, newFakeBackend(), false)
	missing, _ := out["missing"].([]any)
	if len(missing) != 1 || missing[0] != "vision" {
		t.Fatalf("missing = %v, want [vision]", missing)
	}
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.provider.compat", "v1")
	if _, err := reg.Execute(t.Context(), spec, ownerPrincipal(),
		map[string]any{"deployment_id": "prov/model", "required_capabilities": []any{"teleport"}}, nil, newFakeBackend(), false); err == nil {
		t.Fatal("unknown capability accepted")
	}
}

func TestEvalReplayNode(t *testing.T) {
	outcomes := []any{
		map[string]any{"case_id": "reasoning-multi-step-arithmetic", "status": "ok", "output": "42"},
		map[string]any{"case_id": "reasoning-logic-deduction", "status": "ok", "output": "carol"},
		map[string]any{"case_id": "reasoning-numeric-estimate", "status": "ok", "output": "3.14"},
	}
	out := runNode(t, "nexaroute.eval.run", ownerPrincipal(),
		map[string]any{"suite_id": "reasoning", "deployment_id": "prov/model", "outcomes": outcomes}, nil, newFakeBackend(), false)
	if out["mode"] != "replay" || out["upstream_calls"] != 0 {
		t.Fatalf("eval output = %v, want replay with 0 upstream calls", out)
	}
	if out["scoreable"] != true {
		t.Fatalf("eval scoreable = %v", out["scoreable"])
	}
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.eval.run", "v1")
	if _, err := reg.Execute(t.Context(), spec, ownerPrincipal(),
		map[string]any{"suite_id": "no-such-suite", "deployment_id": "prov/model", "outcomes": outcomes}, nil, newFakeBackend(), false); err == nil {
		t.Fatal("unknown suite accepted")
	}
	_ = eval.MaxOutcomesPerRun
}

func TestMCPNodes(t *testing.T) {
	out := runNode(t, "nexaroute.mcp.list", ownerPrincipal(),
		map[string]any{"server_id": "tools"}, nil, newFakeBackend(), false)
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	fb := newFakeBackend()
	out = runNode(t, "nexaroute.mcp.call", ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "search_files", "arguments": map[string]any{"q": "x"}},
		map[string]any{"allowlist": []any{"tools__search_files"}}, fb, false)
	if out["result"] == nil {
		t.Fatalf("mcp call output = %v", out)
	}
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.mcp.call", "v1")
	// Not in node allowlist.
	if _, err := reg.Execute(t.Context(), spec, ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "read_file"},
		map[string]any{"allowlist": []any{"tools__search_files"}}, newFakeBackend(), false); err == nil || !strings.Contains(err.Error(), "tool_forbidden") {
		t.Fatalf("allowlist bypass error = %v", err)
	}
	// In node allowlist but not listed by backend.
	fb2 := newFakeBackend()
	fb2.MCPTools["tools"] = []MCPTool{{Name: "search_files"}}
	if _, err := reg.Execute(t.Context(), spec, ownerPrincipal(),
		map[string]any{"server_id": "tools", "tool": "read_file"},
		map[string]any{"allowlist": []any{"tools__read_file"}}, fb2, false); err == nil || !strings.Contains(err.Error(), "tool_forbidden") {
		t.Fatalf("unlisted tool error = %v", err)
	}
}

func TestVideoNodes(t *testing.T) {
	out := runNode(t, "nexaroute.video.submit", ownerPrincipal(),
		map[string]any{"prompt": "a cat", "duration_seconds": float64(5)}, nil, newFakeBackend(), true)
	if out["job_id"] != "job-1" {
		t.Fatalf("submit job = %v", out)
	}
	out = runNode(t, "nexaroute.video.status", ownerPrincipal(),
		map[string]any{"job_id": "job-1"}, nil, newFakeBackend(), false)
	if out["terminal"] != true || out["state"] != "completed" {
		t.Fatalf("status = %v", out)
	}
	out = runNode(t, "nexaroute.video.output", ownerPrincipal(),
		map[string]any{"job_id": "job-1", "asset_id": "a1"}, nil, newFakeBackend(), false)
	assets, _ := out["assets"].([]any)
	if len(assets) != 1 {
		t.Fatalf("assets = %v", out)
	}
	if strings.Contains(strings.Join([]string{assets[0].(map[string]any)["filename"].(string)}, ""), "/") {
		t.Fatal("asset output contains a path-like value")
	}
	if _, hasData := out["data_base64"]; !hasData {
		t.Fatal("asset bytes missing from output")
	}
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.video.submit", "v1")
	if _, err := reg.Execute(t.Context(), spec, ownerPrincipal(),
		map[string]any{"prompt": "x", "duration_seconds": float64(601)}, nil, newFakeBackend(), true); err == nil {
		t.Fatal("duration above 600s accepted")
	}
}

func TestCostNodes(t *testing.T) {
	out := runNode(t, "nexaroute.cost.usage", ownerPrincipal(), nil, nil, newFakeBackend(), false)
	if out["total_cost_basis"] != "recorded" || out["total_unpriced_tokens"] != int64(0) {
		t.Fatalf("usage = %v", out)
	}
	out = runNode(t, "nexaroute.cost.budget_check", ownerPrincipal(),
		map[string]any{"key": "team", "threshold_usd": float64(4.0)}, nil, newFakeBackend(), false)
	if out["over_threshold"] != true || out["total"] != 5.0 || out["remaining"] != -1.0 {
		t.Fatalf("budget check = %v (report only, not enforcement)", out)
	}
	out = runNode(t, "nexaroute.cost.budget_check", ownerPrincipal(),
		map[string]any{"key": "team", "threshold_usd": float64(10.0)}, nil, newFakeBackend(), false)
	if out["over_threshold"] != false {
		t.Fatalf("budget check = %v", out)
	}
}

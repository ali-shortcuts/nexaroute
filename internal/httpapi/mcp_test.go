package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M6-1 fake MCP upstream: speaks initialize/tools/list/tools/call,
// optionally over SSE, with a session header.
type fakeMCP struct {
	mu         sync.Mutex
	methods    []string
	sessions   []string
	sse        bool
	initCalls  atomic.Int64
	listCalls  atomic.Int64
	toolCalls  atomic.Int64
}

func (f *fakeMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.methods = append(f.methods, req.Method)
		f.sessions = append(f.sessions, r.Header.Get("Mcp-Session-Id"))
		f.mu.Unlock()
		reply := func(result string) {
			if f.sse {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n\n", req.ID, result)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
		}
		switch req.Method {
		case "initialize":
			f.initCalls.Add(1)
			w.Header().Set("Mcp-Session-Id", "sess-1")
			reply(`{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			f.listCalls.Add(1)
			reply(`{"tools":[{"name":"echo","description":"echoes","inputSchema":{"type":"object"}},{"name":"secret_tool","description":"hidden","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			f.toolCalls.Add(1)
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name != "echo" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"unknown tool %s"}}`, req.ID, p.Name)
				return
			}
			text, _ := json.Marshal("echo:" + string(p.Arguments))
			reply(fmt.Sprintf(`{"content":[{"type":"text","text":%s}]}`, text))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}
}

func (f *fakeMCP) sawSession(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s == session {
			return true
		}
	}
	return false
}

func mcpGatewayServer(t *testing.T, fake *fakeMCP, mutate func(*config.Config)) *Server {
	t.Helper()
	up := httptest.NewServer(fake.handler())
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.MCP.Enabled = true
	cfg.MCP.Servers = []config.MCPServerConfig{{ID: "tools", URL: up.URL, Enabled: true}}
	if mutate != nil {
		mutate(&cfg)
	}
	s := testGateway(t, cfg)
	return s
}

func mcpCall(t *testing.T, s *Server, apiKey, payload string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/mcp", strings.NewReader(payload))
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func TestMCPListAndCallFlow(t *testing.T) {
	fake := &fakeMCP{}
	s := mcpGatewayServer(t, fake, nil)

	code, resp := mcpCall(t, s, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if code != http.StatusOK {
		t.Fatalf("initialize: HTTP %d", code)
	}
	if result, ok := resp["result"].(map[string]any); !ok || result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize result: %v", resp)
	}
	code, resp = mcpCall(t, s, "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if code != http.StatusOK {
		t.Fatalf("tools/list: HTTP %d", code)
	}
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	if !names["tools__echo"] || !names["tools__secret_tool"] || len(tools) != 2 {
		t.Fatalf("aggregated tools wrong: %v", names)
	}
	code, resp = mcpCall(t, s, "", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tools__echo","arguments":{"text":"hello"}}}`)
	if code != http.StatusOK {
		t.Fatalf("tools/call: HTTP %d body=%v", code, resp)
	}
	result, _ = resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 || !strings.Contains(content[0].(map[string]any)["text"].(string), `"text":"hello"`) {
		t.Fatalf("call result did not round-trip arguments: %v", resp)
	}
	// Session: initialized once, session header carried afterwards.
	if fake.initCalls.Load() != 1 {
		t.Fatalf("upstream initialize calls = %d, want 1", fake.initCalls.Load())
	}
	if !fake.sawSession("sess-1") {
		t.Fatal("session header never sent upstream")
	}
	// Audit event recorded without argument content.
	found := false
	for _, ev := range s.bus.Snapshot() {
		if ev.Kind == "mcp_tool_call" {
			found = true
			if strings.Contains(ev.Message, "hello") {
				t.Fatalf("audit event leaked argument content: %q", ev.Message)
			}
			if !strings.Contains(ev.Message, "result=ok") {
				t.Fatalf("audit event wrong: %q", ev.Message)
			}
		}
	}
	if !found {
		t.Fatal("no mcp_tool_call audit event")
	}
}

func TestMCPToolAllowlistAndTenantGate(t *testing.T) {
	fake := &fakeMCP{}
	s := mcpGatewayServer(t, fake, func(cfg *config.Config) {
		cfg.MCP.Servers[0].Tools = []string{"echo"}
		cfg.MCP.Servers[0].AllowedTenants = []string{"tenant-a"}
		cfg.ClientAuth.Enabled = true
		cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
			{ID: "vk-a", KeyHash: keyDigest("key-a"), TenantID: "tenant-a"},
			{ID: "vk-b", KeyHash: keyDigest("key-b"), TenantID: "tenant-b"},
		}
	})
	// Tenant B: server invisible in list.
	_, resp := mcpCall(t, s, "key-b", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result, _ := resp["result"].(map[string]any)
	if tools, _ := result["tools"].([]any); len(tools) != 0 {
		t.Fatalf("tenant-b saw gated tools: %v", tools)
	}
	// Tenant B: direct call refused with -32003.
	_, resp = mcpCall(t, s, "key-b", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tools__echo","arguments":{}}}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"].(float64) != -32003 {
		t.Fatalf("tenant-b call error: %v", resp)
	}
	// Tenant A: secret_tool filtered by the tool allowlist.
	_, resp = mcpCall(t, s, "key-a", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	result, _ = resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "tools__echo" {
		t.Fatalf("allowlist filtering wrong: %v", tools)
	}
	_, resp = mcpCall(t, s, "key-a", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tools__secret_tool","arguments":{}}}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"].(float64) != -32003 {
		t.Fatalf("allowlisted-out tool call error: %v", resp)
	}
}

func TestMCPSSEUpstreamAndErrors(t *testing.T) {
	fake := &fakeMCP{sse: true}
	s := mcpGatewayServer(t, fake, nil)
	_, resp := mcpCall(t, s, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result, _ := resp["result"].(map[string]any)
	if tools, _ := result["tools"].([]any); len(tools) != 2 {
		t.Fatalf("SSE tools/list wrong: %v", resp)
	}
	// Unknown namespaced tool -> JSON-RPC error passthrough.
	_, resp = mcpCall(t, s, "", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tools__nope","arguments":{}}}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"].(float64) != -32602 {
		t.Fatalf("unknown tool error: %v", resp)
	}
	// Unknown server -> -32602 from the gateway itself.
	_, resp = mcpCall(t, s, "", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ghost__echo","arguments":{}}}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"].(float64) != -32602 {
		t.Fatalf("unknown server error: %v", resp)
	}
	// Batch rejected; unknown method -32601; unauthenticated when auth on.
	_, resp = mcpCall(t, s, "", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"].(float64) != -32600 {
		t.Fatalf("batch error: %v", resp)
	}
	_, resp = mcpCall(t, s, "", `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"].(float64) != -32601 {
		t.Fatalf("unknown method error: %v", resp)
	}
}

func TestMCPDisabledAndRateLimit(t *testing.T) {
	fake := &fakeMCP{}
	s := mcpGatewayServer(t, fake, func(cfg *config.Config) {
		cfg.MCP.Enabled = false
	})
	req := httptest.NewRequest(http.MethodPost, "http://gateway/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled MCP: HTTP %d, want 404", rr.Code)
	}

	fake2 := &fakeMCP{}
	s2 := mcpGatewayServer(t, fake2, func(cfg *config.Config) {
		cfg.MCP.RPM = 1
		cfg.ClientAuth.Enabled = true
		cfg.ClientAuth.Keys = []string{"plain-key"}
	})
	_, resp := mcpCall(t, s2, "plain-key", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tools__echo","arguments":{}}}`)
	if resp["error"] != nil {
		t.Fatalf("first call should pass: %v", resp)
	}
	_, resp = mcpCall(t, s2, "plain-key", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tools__echo","arguments":{}}}`)
	if e, _ := resp["error"].(map[string]any); e == nil || e["code"].(float64) != -32004 {
		t.Fatalf("rate-limited call error: %v", resp)
	}
}

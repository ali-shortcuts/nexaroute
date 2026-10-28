package nexaflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeMCPGateway answers JSON-RPC tools/list and tools/call the way the
// real gateway broker does: namespaced tool names.
func fakeMCPGateway(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode rpc request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"tools": []any{
				map[string]any{"name": "tools__search_files", "description": "search"},
				map[string]any{"name": "other__read_file", "description": "read"},
			}}})
		case "tools/call":
			calls = append(calls, req.Params)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": "ok"}})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "unknown method"}})
		}
	}))
	return srv, &calls
}

func TestRuntimeBackendMCPTranslation(t *testing.T) {
	ctx := context.Background()
	srv, calls := fakeMCPGateway(t)
	defer srv.Close()
	rb := &RuntimeBackend{Gateway: &GatewayClient{BaseURL: srv.URL}}

	tools, err := rb.MCPList(ctx, "tools")
	if err != nil {
		t.Fatalf("MCPList: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "search_files" {
		t.Fatalf("MCPList = %v, want only bare search_files", tools)
	}
	if _, err := rb.MCPCall(ctx, "tools", "search_files", map[string]any{"q": "x"}); err != nil {
		t.Fatalf("MCPCall: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0]["name"] != "tools__search_files" {
		t.Fatalf("gateway received calls = %v, want namespaced tools__search_files", *calls)
	}
}

func TestRuntimeBackendUnwiredFailsClosed(t *testing.T) {
	ctx := context.Background()
	rb := &RuntimeBackend{}
	if _, err := rb.ResolveSelector(ctx, "x"); err == nil || !strings.Contains(err.Error(), "backend_unavailable") {
		t.Fatalf("ResolveSelector error = %v, want backend_unavailable", err)
	}
	if _, err := rb.Chat(ctx, ChatRequest{}); err == nil || !strings.Contains(err.Error(), "backend_unavailable") {
		t.Fatalf("Chat error = %v, want backend_unavailable", err)
	}
	if _, err := rb.MCPList(ctx, "tools"); err == nil || !strings.Contains(err.Error(), "backend_unavailable") {
		t.Fatalf("MCPList error = %v, want backend_unavailable", err)
	}
	if _, err := rb.ResolveSecretRef("anything"); err == nil || !strings.Contains(err.Error(), "backend_unavailable") {
		t.Fatalf("ResolveSecretRef error = %v, want backend_unavailable", err)
	}
}

func TestEnvSecretResolver(t *testing.T) {
	t.Setenv("NEXAFLOW_TEST_SECRET", "s3cret-value")
	if v, err := EnvSecretResolver("NEXAFLOW_TEST_SECRET"); err != nil || v != "s3cret-value" {
		t.Fatalf("EnvSecretResolver = %q, %v", v, err)
	}
	if _, err := EnvSecretResolver("NEXAFLOW_TEST_SECRET_MISSING"); err == nil {
		t.Fatal("unset secret reference was accepted")
	}
	if _, err := EnvSecretResolver(""); err == nil {
		t.Fatal("empty secret reference was accepted")
	}
}

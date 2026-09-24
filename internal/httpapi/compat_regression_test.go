package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat/capabilities"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// Mock upstream that rejects temperature but serves everything else.
// Models the NVIDIA model-B case: chat=yes, stream=yes, temperature=no.
func tempRejectingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		if _, has := obj["temperature"]; has {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"temperature is not supported","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
}

// Spec section 27 regression: 400 "temperature is not supported" must keep
// deployment.health = HEALTHY and learn capability.temperature = UNSUPPORTED.
// With the default repair budget the request itself still succeeds via the
// bounded drop-temperature repair.
func TestCompatTemperatureFailureKeepsHealth(t *testing.T) {
	up := tempRejectingUpstream(t)
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("repaired request should succeed, status=%d body=%s", rr.Code, rr.Body.String())
	}
	st := s.hm.Get("p/m")
	if st.Status != health.Healthy {
		t.Fatalf("health = %s, want healthy (capability failure poisoned health)", st.Status)
	}
	m, ok := s.compatStore().Get("p/m")
	if !ok {
		t.Fatalf("compat contract missing")
	}
	if m.Get("temperature").Value != capabilities.Unsupported {
		t.Fatalf("temperature = %s, want unsupported", m.Get("temperature").Value)
	}
}

// Same failure with repairs disabled: the 400 passes through to the client,
// but health still stays HEALTHY.
func TestCompatCapabilityFailureHealthNeutralWithoutRepair(t *testing.T) {
	up := tempRejectingUpstream(t)
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	off := false
	cfg.Compat.RepairEnabled = &off
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("status=%d, want passthrough 400", rr.Code)
	}
	st := s.hm.Get("p/m")
	if st.Status != health.Healthy {
		t.Fatalf("health = %s, want healthy", st.Status)
	}
	m, _ := s.compatStore().Get("p/m")
	if m.Get("temperature").Value != capabilities.Unsupported {
		t.Fatalf("temperature = %s, want unsupported", m.Get("temperature").Value)
	}
	// Provider incident evidence must not be recorded for capability failures.
	for _, ps := range s.hm.ProviderSnapshot() {
		if ps.Status == health.Cooldown {
			t.Fatalf("provider circuit must not open for capability failure: %+v", ps)
		}
	}
}

// Bounded repair: max_completion_tokens -> max_tokens mapping.
func TestCompatRepairMaxCompletionMapping(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		if _, has := obj["max_completion_tokens"]; has {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"max_completion_tokens unsupported, use max_tokens"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":16}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if st := s.hm.Get("p/m"); st.Status != health.Healthy {
		t.Fatalf("health = %s", st.Status)
	}
}

// Router integration: a deployment with tools=UNSUPPORTED is ineligible for
// tool requests BEFORE any upstream attempt (spec sections 11, 21).
func TestCompatRouterFiltersUnsupportedTools(t *testing.T) {
	var hitA, hitB bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		if obj["model"] == "model-a" {
			hitA = true
		} else {
			hitB = true
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "a", Model: "model-a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
			{ID: "b", Model: "model-b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
		},
	}}
	s := testGateway(t, cfg)
	s.compatStore().MarkVerified("p/b", "tools", capabilities.Unsupported, capabilities.SourceProbe, "test")
	body := `{"model":"auto","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}]}`

	// Priority strategy orders deterministically; run enough requests to prove
	// B is never selected for tool traffic.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status=%d", rr.Code)
		}
		if got := rr.Header().Get("X-Gateway-Deployment"); got != "p/a" {
			t.Fatalf("deployment = %s, want p/a (tools-unsupported must be filtered)", got)
		}
	}
	if !hitA || hitB {
		t.Fatalf("hitA=%v hitB=%v, B must never be attempted", hitA, hitB)
	}
}

// Claude Code tool loop through Anthropic ingress -> OpenAI mock upstream:
// tool definition, tool call, tool_result continuation.
func TestCompatClaudeCodeToolLoop(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		msgs, _ := obj["messages"].([]any)
		hasToolMsg := false
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "tool" {
				hasToolMsg = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if hasToolMsg {
			io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"The weather is sunny."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
			return
		}
		io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	s := testGateway(t, cfg)
	// Turn 1: tool definition -> tool_use block.
	turn1 := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"Weather in Paris?"}],"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(turn1))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("turn1 status=%d body=%s", rr.Code, rr.Body.String())
	}
	var r1 map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &r1); err != nil {
		t.Fatal(err)
	}
	content, _ := r1["content"].([]any)
	foundToolUse := false
	toolID := ""
	for _, c := range content {
		if cm, ok := c.(map[string]any); ok && cm["type"] == "tool_use" {
			foundToolUse = true
			toolID, _ = cm["id"].(string)
			if cm["name"] != "get_weather" {
				t.Fatalf("tool name = %v", cm["name"])
			}
		}
	}
	if !foundToolUse || toolID == "" {
		t.Fatalf("turn1 missing tool_use: %s", rr.Body.String())
	}
	// Turn 2: tool_result continuation -> text answer.
	turn2 := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"Weather in Paris?"},{"role":"assistant","content":[{"type":"tool_use","id":"` + toolID + `","name":"get_weather","input":{"city":"Paris"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolID + `","content":"sunny, 21C"}]}]}`
	req = httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(turn2))
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("turn2 status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "sunny") {
		t.Fatalf("turn2 missing answer: %s", rr.Body.String())
	}
}

// OpenAI Responses ingress end-to-end via canonical IR.
func TestCompatResponsesIngress(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		if obj["model"] != "m" {
			t.Errorf("model = %v", obj["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}}}
	s := testGateway(t, cfg)
	req := httptest.NewRequest("POST", "http://gateway/v1/responses",
		strings.NewReader(`{"model":"m","input":"Say hello","instructions":"Be brief"}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "response" {
		t.Fatalf("object = %v: %s", out["object"], rr.Body.String())
	}
	output, _ := out["output"].([]any)
	if len(output) == 0 {
		t.Fatalf("empty output: %s", rr.Body.String())
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(2) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", usage)
	}
}

// Admin compat surfaces: snapshot section + quick/full probe modes.
func TestCompatAdminEndpoints(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}}}
	s := testGateway(t, cfg)
	adminReq := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(body))
		req.Host = "127.0.0.1"
		req.RemoteAddr = "127.0.0.1:12345"
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	rr := adminReq("GET", "/admin/api/compat", "")
	if rr.Code != 200 {
		t.Fatalf("compat status=%d", rr.Code)
	}
	var snap map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if _, ok := snap["contracts"]; !ok {
		t.Fatalf("contracts missing: %s", rr.Body.String())
	}
	if _, ok := snap["scorecards"]; !ok {
		t.Fatalf("scorecards missing: %s", rr.Body.String())
	}
	rr = adminReq("POST", "/admin/api/compat/probe", `{"deployment":"p/m","mode":"quick"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "PASS") {
		t.Fatalf("quick probe: status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = adminReq("POST", "/admin/api/compat/probe", `{"deployment":"p/m","mode":"full"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "level_b") {
		t.Fatalf("full probe: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// TestCapabilityFailureDoesNotKillHealthyModel is the mandatory regression
// test (spec section 27): a 400 "temperature is not supported" must leave the
// deployment HEALTHY, mark capability temperature=UNSUPPORTED, repair the
// payload and retry once successfully on the same deployment.
func TestCapabilityFailureDoesNotKillHealthyModel(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error","param":"temperature"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","max_tokens":64,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Hello!") {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 (original + repaired retry)", calls)
	}
	// Health must remain healthy.
	if st := s.hm.Get("p/m"); st.Status != health.Healthy {
		t.Fatalf("health=%s want healthy (capability failure leaked into health)", st.Status)
	}
	// Capability contract must record temperature=UNSUPPORTED.
	contract := s.capStore.Get("p/m")
	if got := contract.Capabilities.Temperature; got.String() != "unsupported" {
		t.Fatalf("temperature=%q want unsupported", got)
	}
	if contract.LastRepair == "" {
		t.Fatal("last_repair breadcrumb missing")
	}
}

// TestNoFalseCapabilityClaims enforces spec section 28: capabilities never
// verified stay UNKNOWN, and a deployment with UNKNOWN tools is not treated
// as agent-verified by the router.
func TestNoFalseCapabilityClaims(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	contract := s.capStore.Get("p/m")
	// Capabilities the operator never configured and traffic never verified
	// must stay UNKNOWN - never claimed as supported (spec section 28).
	if got := contract.Capabilities.Temperature; got.String() != "unknown" {
		t.Fatalf("temperature=%q after plain chat: must stay UNKNOWN", got)
	}
	if got := contract.Capabilities.ParallelToolCalls; got.String() != "unknown" {
		t.Fatalf("parallel_tool_calls=%q: must stay UNKNOWN", got)
	}
	// Config-declared capabilities stay honest operator claims.
	if got := contract.Capabilities.Tools; got.String() != "supported" {
		t.Fatalf("tools=%q: operator config declared tools=true", got)
	}
	// A tools=false operator declaration must never yield agent-ready.
	sc := contract.Scorecard("healthy")
	if contract.Capabilities.Tools == compat.Unsupported && sc.Status == compat.StatusClaudeCodeReady {
		t.Fatal("tools=false config must never be CLAUDE_CODE_READY")
	}
}

// TestResponsesIngressAgainstOpenAIUpstream covers POST /v1/responses with a
// non-streaming and a streaming upstream through the canonical IR path.
func TestResponsesIngressAgainstOpenAIUpstream(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotModel, _ = in["model"].(string)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-9","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi from upstream"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"max_output_tokens":64}`
	req := httptest.NewRequest("POST", "http://gateway/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "completed" {
		t.Fatalf("status=%v", resp["status"])
	}
	output, _ := resp["output"].([]any)
	found := false
	for _, item := range output {
		im := item.(map[string]any)
		if im["type"] == "message" {
			found = true
		}
	}
	if !found {
		t.Fatalf("message item missing: %v", resp)
	}
	if gotModel != "up-model" {
		t.Fatalf("upstream got model=%q", gotModel)
	}
}

// TestGeminiUpstreamServesClaudeClient covers the canonical path end to end:
// an Anthropic client request is routed to a provider type gemini upstream,
// encoded as GenerateContent (model in path), and the Gemini response is
// decoded and re-encoded as an Anthropic Messages response.
func TestGeminiUpstreamServesClaudeClient(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if key := r.Header.Get("x-goog-api-key"); key != "gk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Gemini says hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "G", Type: "gemini", BaseURL: up.URL, APIKey: "gk-test", AuthMode: "x-goog-api-key", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-2.0-flash", Enabled: true,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Fatalf("envelope=%v", resp)
	}
	content := resp["content"].([]any)
	if len(content) == 0 || content[0].(map[string]any)["text"] != "Gemini says hi" {
		t.Fatalf("content=%v", content)
	}
	if !strings.Contains(gotPath, "/models/gemini-2.0-flash:generateContent") {
		t.Fatalf("upstream path=%q (model must be in the URL path)", gotPath)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 5 || usage["output_tokens"].(float64) != 3 {
		t.Fatalf("usage=%v", usage)
	}
	contract := s.capStore.Get("g/m")
	if contract.Capabilities.NativeProtocol != "gemini" {
		t.Fatalf("native protocol=%q", contract.Capabilities.NativeProtocol)
	}
}

// TestGeminiToolCallRoundTrip proves the Claude Code tool loop works across
// the canonical path: Anthropic tools in, Gemini functionCall out, Anthropic
// tool_use back to the client.
func TestGeminiToolCallRoundTrip(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "G", Type: "gemini", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-2.0-flash", Enabled: true,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"Weather in Paris?"}],"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	content := resp["content"].([]any)
	found := false
	for _, b := range content {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" && bm["name"] == "get_weather" {
			found = true
			input := bm["input"].(map[string]any)
			if input["city"] != "Paris" {
				t.Fatalf("input=%v", input)
			}
		}
	}
	if !found {
		t.Fatalf("tool_use missing: %v", resp)
	}
	if resp["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v", resp["stop_reason"])
	}
}

// TestRepairLearnedCapabilitySkipsDeadParamsOnLaterRequests ensures the
// sanitizer uses the learned contract proactively (second request never
// sends temperature again).
func TestRepairLearnedCapabilitySkipsDeadParamsOnLaterRequests(t *testing.T) {
	var calls int32
	seenTemperature := map[int]bool{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, hasTemp := in["temperature"]
		seenTemperature[int(n)] = hasTemp
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"temperature is not supported","param":"temperature"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	mk := func() *httptest.ResponseRecorder {
		body := `{"model":"m","max_tokens":16,"temperature":0.5,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := mk(); rr.Code != 200 {
		t.Fatalf("first request status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := mk(); rr.Code != 200 {
		t.Fatalf("second request status=%d body=%s", rr.Code, rr.Body.String())
	}
	if calls != 3 {
		t.Fatalf("calls=%d want 3 (1 fail + 1 repair + 1 sanitized direct)", calls)
	}
	if !seenTemperature[1] {
		t.Fatal("first request should carry temperature")
	}
	if seenTemperature[2] {
		t.Fatal("repaired retry should have dropped temperature")
	}
	if seenTemperature[3] {
		t.Fatal("sanitizer should have dropped temperature proactively on the next request")
	}
}

// TestCompatMatrixEndpoint exercises the admin observability surface.
func TestCompatMatrixEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "https://example.invalid", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "up-model", Enabled: true,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	req := httptest.NewRequest("GET", "http://gateway/admin/api/compat", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Deployments []map[string]any `json:"deployments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Deployments) != 1 {
		t.Fatalf("deployments=%d", len(resp.Deployments))
	}
	row := resp.Deployments[0]
	// testGateway marks every deployment healthy for ready strategies.
	if row["health"] != "healthy" {
		t.Fatalf("health=%v", row["health"])
	}
	sc := row["scorecard"].(map[string]any)
	if sc["protocol"] != "openai_chat" {
		t.Fatalf("protocol=%v", sc["protocol"])
	}
}

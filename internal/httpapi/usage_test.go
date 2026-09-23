package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestUsageAccountingNonStreamWithCost(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hey"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.ClientAuth = config.ClientAuthConfig{Required: true, Keys: []config.ClientKey{{ID: "k1", Name: "laptop", Key: "nr-app", Enabled: true}}}
	cfg.Pricing = map[string]config.PriceConfig{"*": {InputPerM: 1, OutputPerM: 2}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"model-1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer nr-app")
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("data plane status %d body=%s", rr.Code, rr.Body.String())
	}

	snap := s.usage.Snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.InputTokens != 100 || snap.Totals.OutputTokens != 50 {
		t.Fatalf("totals wrong: %+v", snap.Totals)
	}
	// cost = 100/1e6*1 + 50/1e6*2 = 0.0002
	if snap.Totals.EstCostUSD < 0.000199 || snap.Totals.EstCostUSD > 0.000201 {
		t.Fatalf("cost wrong: %v", snap.Totals.EstCostUSD)
	}
	e, ok := snap.ByProvider["p1/m1"]
	if !ok || e.InputTokens != 100 || e.OutputTokens != 50 {
		t.Fatalf("by-provider wrong: %+v", snap.ByProvider)
	}
	k, ok := snap.ByKey["laptop"]
	if !ok || k.Requests != 1 || k.InputTokens != 100 {
		t.Fatalf("by-key wrong: %+v", snap.ByKey)
	}

	// Reset works through the admin API.
	req = httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/usage/reset", nil)
	req.Header.Set("x-admin-key", "k")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("reset status %d", rr.Code)
	}
	if snap = s.usage.Snapshot(); snap.Totals.Requests != 0 {
		t.Fatalf("reset failed: %+v", snap.Totals)
	}
}

func TestUsageAccountingTranslatedAnthropicIngress(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "سلام"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"model-1","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("anthropic ingress status %d body=%s", rr.Code, rr.Body.String())
	}
	snap := s.usage.Snapshot()
	if snap.Totals.InputTokens != 10 || snap.Totals.OutputTokens != 5 {
		t.Fatalf("translated ingress usage wrong: %+v", snap.Totals)
	}
	if _, ok := snap.ByKey["local"]; !ok {
		t.Fatalf("unauthenticated usage should be attributed to 'local': %+v", snap.ByKey)
	}
}

func TestUsageAccountingNativeAnthropicStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		events := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","usage":{"input_tokens":7}}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hey"}}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
			f.Flush()
		}
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "anthropic_compatible", BaseURL: up.URL + "/v1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "claude-x", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"claude-x","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("stream status %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "message_stop") {
		t.Fatalf("stream incomplete: %s", rr.Body.String())
	}
	snap := s.usage.Snapshot()
	if snap.Totals.InputTokens != 7 || snap.Totals.OutputTokens != 3 {
		t.Fatalf("native stream usage wrong: %+v", snap.Totals)
	}
}

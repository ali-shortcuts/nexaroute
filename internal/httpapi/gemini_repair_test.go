package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// TestGeminiRepairInlineInsteadOfFailover proves a Gemini-native payload
// carrying a rejected generationConfig knob is repaired inline on the same
// deployment (no failover, health stays healthy, audit breadcrumb set).
func TestGeminiRepairInlineInsteadOfFailover(t *testing.T) {
	var calls int32
	var secondHasTemperature bool
	var secondChecked bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		gc, _ := in["generationConfig"].(map[string]any)
		_, hasTemp := gc["temperature"]
		if n == 1 {
			if !hasTemp {
				t.Errorf("first Gemini payload missing generationConfig.temperature: %v", in)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"temperature is not supported"}}`))
			return
		}
		secondHasTemperature = hasTemp
		secondChecked = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Gemini says hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "G", Type: "gemini", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-2.0-flash", Enabled: true,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	// Anthropic ingress with temperature maps through the canonical IR into
	// generationConfig.temperature for the Gemini upstream.
	body := `{"model":"m","max_tokens":64,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 (original + inline repaired retry on same deployment)", calls)
	}
	if !secondChecked {
		t.Fatal("second upstream call not observed")
	}
	if secondHasTemperature {
		t.Fatal("repaired Gemini retry still carried generationConfig.temperature")
	}
	if st := s.hm.Get("g/m"); st.Status != health.Healthy {
		t.Fatalf("health=%s want healthy (capability failure leaked into health)", st.Status)
	}
	contract := s.capStore.Get("g/m")
	if got := contract.Capabilities.Temperature.String(); got != "unsupported" {
		t.Fatalf("temperature=%q want unsupported", got)
	}
	if contract.LastRepair == "" || !strings.Contains(contract.LastRepair, "generationConfig.temperature") {
		t.Fatalf("last_repair=%q want generationConfig.temperature repair", contract.LastRepair)
	}
}

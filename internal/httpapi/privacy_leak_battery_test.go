package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M5-3 privacy leak battery: prompt CONTENT must never surface in
// the gateway's own observability sinks — event bus entries, usage
// records, response headers, or error bodies — on any data-plane
// path. (Route-level privacy — requests never REACHING a forbidden
// upstream, provider identities never leaking — is covered by the
// privacy_p2 suite; this battery is about content, and it covers
// the M5-1c media paths those tests predate.)

const privacyCanary = "CANARY-PROMPT-m53-9f27-secret-content"

func leakBatteryGateway(t *testing.T, fail bool) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream exploded","type":"server_error"}}`)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "embeddings"):
			_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`)
		case strings.HasSuffix(r.URL.Path, "responses"):
			_, _ = io.WriteString(w, `{"id":"r","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`)
		case strings.HasSuffix(r.URL.Path, "messages"):
			_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
		default:
			_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		}
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	p := config.ProviderConfig{
		ID: "lp", Name: "LP", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "lp-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func assertNoCanaryInSinks(t *testing.T, s *Server, rr *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rr.Body.String(), privacyCanary) {
		t.Fatalf("response body leaks prompt content: %s", rr.Body.String())
	}
	for name, values := range rr.Header() {
		for _, v := range values {
			if strings.Contains(v, privacyCanary) {
				t.Fatalf("response header %s leaks prompt content", name)
			}
		}
	}
	for _, ev := range s.bus.Snapshot() {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), privacyCanary) {
			t.Fatalf("event bus leaks prompt content: %s", raw)
		}
	}
	{
		raw, err := json.Marshal(s.usage.Snapshot(nil))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), privacyCanary) {
			t.Fatalf("usage records leak prompt content: %s", raw)
		}
	}
	// Health state carries deployment ids and error CLASSES, never
	// request content.
	for _, st := range s.hm.Snapshot() {
		raw, _ := json.Marshal(st)
		if strings.Contains(string(raw), privacyCanary) {
			t.Fatalf("health state leaks prompt content: %s", raw)
		}
	}
}

func TestPrivacyPromptContentNeverReachesSinks(t *testing.T) {
	paths := []struct {
		name, path, body string
	}{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"` + privacyCanary + `"}]}`},
		{"messages", "/v1/messages", `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"` + privacyCanary + `"}]}`},
		{"responses", "/v1/responses", `{"model":"m","input":"` + privacyCanary + `"}`},
		// The media paths (M5-1c, PR #324) join this battery by
		// adding their rows here once that PR lands on main; the
		// sink assertions are path-agnostic.
	}
	for _, tc := range paths {
		t.Run(tc.name+"_success", func(t *testing.T) {
			s := leakBatteryGateway(t, false)
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+tc.path, strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			assertNoCanaryInSinks(t, s, rr)
		})
		t.Run(tc.name+"_upstream_error", func(t *testing.T) {
			s := leakBatteryGateway(t, true)
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+tc.path, strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				t.Fatalf("expected an error status, got 200: %s", rr.Body.String())
			}
			assertNoCanaryInSinks(t, s, rr)
		})
	}
}

package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Codex CLI ingress proof tests (FIXTURE): non-streaming Responses and
// auth failure. Streaming tool/text loops over Chat and Anthropic
// upstreams are already covered by codex_responses_test.go.

func codexIngressGateway(t *testing.T, auth bool) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"codex hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	if auth {
		cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"test-client-key"}}
	}
	cfg.Providers = []config.ProviderConfig{{
		ID: "cx", Name: "Codex test upstream", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func TestCodexResponsesNonStreaming(t *testing.T) {
	s := codexIngressGateway(t, false)
	body := `{"model":"m","store":false,"instructions":"You are Codex.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	for _, want := range []string{"codex hello", `"status":"completed"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
}

func TestCodexResponsesAuthFailure(t *testing.T) {
	s := codexIngressGateway(t, true)
	body := `{"model":"m","store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	for _, key := range []string{"", "Bearer wrong-key"} {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", key)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("key %q: status=%d want 401 body=%s", key, rr.Code, rr.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-client-key")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid key: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Regression: a verified-unsupported structured-output capability was not
// treated as an eligibility gate, and profileFromRequirement dropped
// req.StructuredOutput. The sanitizer then removed response_format and the
// gateway silently returned an unstructured 200, violating the client's
// explicit output contract.
func TestStructuredOutputUnsupportedFailsClosed(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"plain"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	s.CapabilityStore().LearnUnsupported("p/m", compat.CapStructuredOutput, compat.SourceProbe, "probe verdict", "")

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"return json"}],"response_format":{"type":"json_object"}}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("structured output was silently dropped and request returned 200: %s", rr.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("ineligible structured-output deployment received %d request(s)", calls.Load())
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

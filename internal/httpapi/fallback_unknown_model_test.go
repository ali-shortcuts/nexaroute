package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Regression (end to end): with fallback_on_unknown_model enabled (the
// default), a chat request for an unlisted model selected a fallback
// deployment in Candidates, but the per-attempt re-check (Eligible)
// rejected it for model mismatch, so ZERO upstream attempts were made and
// the handler fabricated "502 all candidate deployments failed: no usable
// deployment" — claiming deployments failed when none was ever tried.

func fallbackGateway(t *testing.T, upstreamURL string, fallback bool) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.FallbackOnUnknownModel = fallback
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: upstreamURL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true},
		}},
	}}
	return testGateway(t, cfg)
}

func chatRequest(s *Server, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestUnknownModelFallbackServesRequest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer up.Close()
	s := fallbackGateway(t, up.URL, true)
	rr := chatRequest(s, "totally-unknown")
	if rr.Code != 200 {
		t.Fatalf("fallback should serve an unknown model via the fallback deployment: status %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUnknownModelFallbackUpstreamDownReportsRealFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := up.URL
	up.Close() // upstream is now unreachable; attempts must fail with a real error
	s := fallbackGateway(t, deadURL, true)
	rr := chatRequest(s, "totally-unknown")
	if rr.Code != 502 {
		t.Fatalf("attempted-and-failed fallback should be 502, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "no usable deployment") {
		t.Fatalf("the synthetic zero-attempt error leaked into a real attempt failure: %s", rr.Body.String())
	}
}

func TestUnknownModelFallbackDisabledStill503(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := fallbackGateway(t, up.URL, false)
	rr := chatRequest(s, "totally-unknown")
	if rr.Code != 503 {
		t.Fatalf("fallback disabled + unknown model should be 503, got %d body=%s", rr.Code, rr.Body.String())
	}
}

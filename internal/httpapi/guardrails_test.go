package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func guardrailsGateway(t *testing.T, g config.GuardrailsConfig) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.Guardrails = g
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1/v1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	return testGateway(t, cfg)
}

func TestGuardrailsBlockedPatternAndLength(t *testing.T) {
	s := guardrailsGateway(t, config.GuardrailsConfig{
		MaxPromptChars:  200,
		BlockedPatterns: []string{`INTERNAL_TOKEN=[A-Za-z0-9._-]+`, `sk-[a-zA-Z0-9]{20,}`},
	})

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	rr := post(`{"model":"model-1","messages":[{"role":"user","content":"please read INTERNAL_TOKEN=abc123 and use it"}]}`)
	if rr.Code != 400 {
		t.Fatalf("pattern block expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "blocked_pattern") {
		t.Fatalf("error should name the rule: %s", rr.Body.String())
	}

	long := strings.Repeat("x", 500)
	rr = post(`{"model":"model-1","messages":[{"role":"user","content":"` + long + `"}]}`)
	if rr.Code != 400 {
		t.Fatalf("length block expected 400, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "max_prompt_chars") {
		t.Fatalf("error should name the length rule: %s", rr.Body.String())
	}

	// Case-insensitive matching.
	rr = post(`{"model":"model-1","messages":[{"role":"user","content":"token internal_token=Zz9 ok"}]}`)
	if rr.Code != 400 {
		t.Fatalf("case-insensitive block expected 400, got %d", rr.Code)
	}

	// Clean prompt passes the guardrail (fails later at routing 503 instead).
	rr = post(`{"model":"model-1","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code == 400 {
		t.Fatalf("clean prompt must not be guardrail-blocked: %s", rr.Body.String())
	}
}

func TestGuardrailsAnthropicShapeAndDryRun(t *testing.T) {
	s := guardrailsGateway(t, config.GuardrailsConfig{BlockedPatterns: []string{"secret-sauce"}})

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"model-1","max_tokens":1,"messages":[{"role":"user","content":"the secret-sauce recipe"}]}`))
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("anthropic guardrail block expected 400, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"type":"gateway_error"`) && !strings.Contains(rr.Body.String(), "error") {
		t.Fatalf("expected anthropic-style error: %s", rr.Body.String())
	}

	// Dry-run endpoint.
	dreq := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/guardrails/test", strings.NewReader(`{"text":"SECRET-SAUCE inside"}`))
	dreq.Header.Set("x-admin-key", "k")
	dreq.Header.Set("content-type", "application/json")
	drr := httptest.NewRecorder()
	s.Handler().ServeHTTP(drr, dreq)
	if drr.Code != 200 {
		t.Fatalf("dry-run status %d", drr.Code)
	}
	var out struct {
		Blocked bool     `json:"blocked"`
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal(drr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Blocked || len(out.Matches) != 1 || out.Matches[0] != "secret-sauce" {
		t.Fatalf("dry-run result wrong: %+v", out)
	}
}

func TestGuardrailsInvalidPatternFailsValidation(t *testing.T) {
	cfg := config.Default()
	cfg.Guardrails = config.GuardrailsConfig{BlockedPatterns: []string{"([unclosed"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid regex must fail config validation")
	}
}

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave4ProviderTestCall(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1/admin/api/provider-test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.adminProviderTest(rr, req)
	return rr
}

func wave4ProviderTestBody(provider, mode string, models []string) string {
	var sb strings.Builder
	sb.WriteString(`{"provider":` + provider + `,"mode":"` + mode + `"`)
	if models != nil {
		b, _ := json.Marshal(models)
		sb.WriteString(`,"test_models":` + string(b))
	}
	sb.WriteString(`}`)
	return sb.String()
}

func TestWave4AdminProviderTestValidation(t *testing.T) {
	s := wave4AdminGateway(t)
	if rr := wave4ProviderTestCall(t, s, http.MethodGet, ""); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := wave4ProviderTestCall(t, s, http.MethodPost, `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := wave4ProviderTestCall(t, s, http.MethodPost, `{"provider":{"id":"","base_url":""}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing id/base status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := wave4ProviderTestCall(t, s, http.MethodPost, `{"provider":{"id":"x","base_url":"http://127.0.0.1:9","type":"bogus"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("unsupported type status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Passes the handler's own checks but fails provider validation:
	// max_concurrency above the 4096 ceiling.
	if rr := wave4ProviderTestCall(t, s, http.MethodPost, `{"provider":{"id":"x","base_url":"http://127.0.0.1:9","type":"openai_compatible","max_concurrency":99999}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("adapter error status=%d body=%s", rr.Code, rr.Body.String())
	}
	// No models anywhere to test.
	if rr := wave4ProviderTestCall(t, s, http.MethodPost, `{"provider":{"id":"x","base_url":"http://127.0.0.1:9","type":"openai_compatible"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("no models status=%d body=%s", rr.Code, rr.Body.String())
	}
	// A model id beyond the 1024-byte safe limit.
	if rr := wave4ProviderTestCall(t, s, http.MethodPost, wave4ProviderTestBody(`{"id":"x","base_url":"http://127.0.0.1:9","type":"openai_compatible"}`, "quick", []string{strings.Repeat("m", 1025)})); rr.Code != http.StatusBadRequest {
		t.Fatalf("oversize model status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// More than 100 test models are truncated to exactly 100 probes.
func TestWave4AdminProviderTestTruncatesModels(t *testing.T) {
	s := wave4AdminGateway(t)
	models := make([]string, 101)
	for i := range models {
		models[i] = fmt.Sprintf("m-%d", i)
	}
	rr := wave4ProviderTestCall(t, s, http.MethodPost, wave4ProviderTestBody(`{"id":"x","base_url":"http://127.0.0.1:9","type":"openai_compatible"}`, "quick", models))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Total   int `json:"total"`
		Results []struct {
			Model string `json:"model"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rr.Body.String())
	}
	if out.Total != 100 || len(out.Results) != 100 {
		t.Fatalf("total=%d results=%d, want 100/100", out.Total, len(out.Results))
	}
	// uniqueStrings sorts, so the truncated tail is the lexicographic
	// maximum: m-99 is dropped while m-98 survives.
	if out.Results[0].Model != "m-0" {
		t.Fatalf("first result model = %q, want m-0", out.Results[0].Model)
	}
	for _, r := range out.Results {
		if r.Model == "m-99" {
			t.Fatal("m-99 must be truncated away")
		}
	}
}

// Full-mode capability suites and the claude_code agent loop run against
// an unreachable upstream: every probe fails transport, so each report
// is present but not OK. This exercises the per-type suite dispatch.
func TestWave4AdminProviderTestSuites(t *testing.T) {
	s := wave4AdminGateway(t)
	cases := []struct {
		name     string
		provider string
		mode     string
		field    string
	}{
		{"full-anthropic", `{"id":"x","base_url":"http://127.0.0.1:9","type":"anthropic_compatible"}`, "full", "capability_report"},
		{"full-responses", `{"id":"x","base_url":"http://127.0.0.1:9","type":"openai_responses"}`, "full", "capability_report"},
		{"full-gemini", `{"id":"x","base_url":"http://127.0.0.1:9","type":"gemini"}`, "full", "capability_report"},
		{"claude-code-responses", `{"id":"x","base_url":"http://127.0.0.1:9","type":"openai_responses"}`, "claude_code", "agent_report"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := wave4ProviderTestCall(t, s, http.MethodPost, wave4ProviderTestBody(tc.provider, tc.mode, []string{"m1"}))
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var out struct {
				OK      bool             `json:"ok"`
				Results []map[string]any `json:"results"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
				t.Fatalf("invalid JSON: %v body=%s", err, rr.Body.String())
			}
			if len(out.Results) != 1 {
				t.Fatalf("results = %v, want 1", out.Results)
			}
			if _, ok := out.Results[0][tc.field]; !ok {
				t.Fatalf("result = %v, want a %s report", out.Results[0], tc.field)
			}
			if out.Results[0]["ok"] != false {
				t.Fatalf("result = %v, want ok=false against a refused upstream", out.Results[0])
			}
		})
	}
}

func wave4BoolPtr(b bool) *bool { return &b }

func wave4RevealCall(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/api/providers/"+id+"/reveal", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.adminProviderRevealSecret(rr, req, id)
	return rr
}

func TestWave4AdminRevealValidation(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	// Method check via a direct GET call.
	{
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/providers/p/reveal", nil)
		rr := httptest.NewRecorder()
		s.adminProviderRevealSecret(rr, req, "p")
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	for _, id := range []string{"", "a/b"} {
		if rr := wave4RevealCall(t, s, id, ""); rr.Code != http.StatusBadRequest {
			t.Fatalf("id %q status=%d body=%s", id, rr.Code, rr.Body.String())
		}
	}
	// Oversize body (limit 4 KiB).
	if rr := wave4RevealCall(t, s, "p", strings.Repeat("x", 5000)); rr.Code != http.StatusBadRequest {
		t.Fatalf("oversize status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestWave4AdminRevealDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.AllowSecretReveal = wave4BoolPtr(false)
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	if rr := wave4RevealCall(t, s, "p", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("disabled status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// An env-backed key is never revealed: the handler reports the env
// source with a 409 instead of the secret.
func TestWave4AdminRevealEnvSource(t *testing.T) {
	t.Setenv("WAVE4_REVEAL_ENV", "env-key-value")
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, APIKeyEnv: "WAVE4_REVEAL_ENV", Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	rr := wave4RevealCall(t, s, "p", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("env status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Source  string `json:"source"`
		EnvName string `json:"env_name"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rr.Body.String())
	}
	if out.Source != "env" || out.EnvName != "WAVE4_REVEAL_ENV" {
		t.Fatalf("reveal = %+v, want source env / WAVE4_REVEAL_ENV", out)
	}
	if strings.Contains(rr.Body.String(), "env-key-value") {
		t.Fatal("env key material must never appear in the response")
	}
}

// The per-subject reveal budget is 10 per minute: the 11th attempt in
// the window is refused with 429 and a Retry-After hint.
func TestWave4AdminRevealRateLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "a", Model: "model-a", Enabled: true, Weight: 1}}}}
	s := testGateway(t, cfg)
	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		last = wave4RevealCall(t, s, "ghost", "")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("11th reveal status=%d body=%s", last.Code, last.Body.String())
	}
	if last.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", last.Header().Get("Retry-After"))
	}
}

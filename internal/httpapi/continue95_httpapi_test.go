package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/decision/policy"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

// Continue95 coverage wave for internal/httpapi (issue #663): NEW tests
// only, loopback fakes only, no live provider calls, no credentials.
// Helper and test names carry the Continue95Httpapi prefix so they can
// never collide with the sibling wave's TestIssue663* names.

type Continue95HttpapiFakeUpstream struct {
	mu     sync.Mutex
	calls  int
	bodies []string
	status int
	body   string
	stream bool
}

func (u *Continue95HttpapiFakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls++
	u.bodies = append(u.bodies, string(raw))
	u.mu.Unlock()
	if u.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"stream \"},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n")
		write("data: [DONE]\n\n")
		return
	}
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	body := u.body
	if body == "" {
		body = `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"chat hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (u *Continue95HttpapiFakeUpstream) Continue95HttpapiCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func Continue95HttpapiGeminiGateway(t *testing.T, ups []*Continue95HttpapiFakeUpstream, maxAttempts int) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = maxAttempts
	for i, up := range ups {
		srv := httptest.NewServer(up)
		t.Cleanup(srv.Close)
		priority := i
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "g" + string(rune('a'+i)), Name: "test upstream", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: priority,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}},
		})
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func Continue95HttpapiGeminiPost(t *testing.T, s *Server, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func Continue95HttpapiGeminiBody(t *testing.T) string {
	t.Helper()
	return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
}

func Continue95HttpapiAssertGeminiError(t *testing.T, rr *httptest.ResponseRecorder, wantCode int, wantStatus string) {
	t.Helper()
	if rr.Code != wantCode {
		t.Fatalf("status=%d want %d body=%s", rr.Code, wantCode, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("error is not JSON: %v (%s)", err, rr.Body.String())
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj == nil || errObj["code"] != float64(wantCode) || errObj["status"] != wantStatus {
		t.Fatalf("Gemini envelope = %v, want code %d status %s", errObj, wantCode, wantStatus)
	}
}

func TestContinue95HttpapiGeminiStatusNames(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "INVALID_ARGUMENT"},
		{http.StatusUnauthorized, "UNAUTHENTICATED"},
		{http.StatusForbidden, "PERMISSION_DENIED"},
		{http.StatusNotFound, "NOT_FOUND"},
		{http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"},
		{http.StatusBadGateway, "UNAVAILABLE"},
		{http.StatusServiceUnavailable, "UNAVAILABLE"},
		{http.StatusGatewayTimeout, "DEADLINE_EXCEEDED"},
		{http.StatusMethodNotAllowed, "INTERNAL"},
		{http.StatusTeapot, "INTERNAL"},
	}
	for _, tc := range cases {
		if got := geminiStatusName(tc.status); got != tc.want {
			t.Fatalf("geminiStatusName(%d) = %q, want %q", tc.status, got, tc.want)
		}
		rr := httptest.NewRecorder()
		geminiErrorJSON(rr, tc.status, "boom")
		if rr.Code != tc.status {
			t.Fatalf("geminiErrorJSON code=%d want %d", rr.Code, tc.status)
		}
		var env map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("envelope decode: %v", err)
		}
		errObj, _ := env["error"].(map[string]any)
		if errObj["message"] != "boom" || errObj["status"] != tc.want {
			t.Fatalf("envelope = %v, want message boom status %s", errObj, tc.want)
		}
	}
}

func TestContinue95HttpapiGeminiAuthRequestForms(t *testing.T) {
	// x-goog-api-key is normalized into x-api-key and ?key= is stripped.
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models/m:generateContent?key=secret-key&alt=sse", nil)
	req.Header.Set("x-goog-api-key", "secret-key")
	out := geminiAuthRequest(req)
	if out.Header.Get("x-api-key") != "secret-key" {
		t.Fatalf("x-api-key = %q, want secret-key", out.Header.Get("x-api-key"))
	}
	if strings.Contains(out.URL.String(), "secret-key") {
		t.Fatalf("normalized URL still leaks the key: %s", out.URL.String())
	}
	if out.URL.Query().Get("alt") != "sse" {
		t.Fatalf("other query params must survive normalization: %s", out.URL.String())
	}
	// ?key= form alone.
	req2 := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models/m:generateContent?key=query-key", nil)
	out2 := geminiAuthRequest(req2)
	if out2.Header.Get("x-api-key") != "query-key" || strings.Contains(out2.URL.String(), "query-key") {
		t.Fatalf("query-key form: header=%q url=%s", out2.Header.Get("x-api-key"), out2.URL.String())
	}
	// Whitespace around the key is trimmed.
	req3 := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models/m:generateContent", nil)
	req3.Header.Set("x-goog-api-key", "  spaced-key  ")
	if got := geminiAuthRequest(req3).Header.Get("x-api-key"); got != "spaced-key" {
		t.Fatalf("trimmed key = %q, want spaced-key", got)
	}
	// Existing x-api-key / Authorization forms pass through untouched.
	req4 := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models/m:generateContent?key=ignored", nil)
	req4.Header.Set("x-api-key", "already-there")
	if got := geminiAuthRequest(req4); got.Header.Get("x-api-key") != "already-there" || !strings.Contains(got.URL.String(), "key=ignored") {
		t.Fatalf("x-api-key passthrough changed the request: header=%q url=%s", got.Header.Get("x-api-key"), got.URL.String())
	}
	req5 := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models/m:generateContent", nil)
	req5.Header.Set("Authorization", "Bearer bearer-key")
	if got := geminiAuthRequest(req5); got.Header.Get("x-api-key") != "" {
		t.Fatalf("Authorization passthrough must not synthesize x-api-key, got %q", got.Header.Get("x-api-key"))
	}
	// No key anywhere: request returned unchanged.
	req6 := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models/m:generateContent", nil)
	if got := geminiAuthRequest(req6); got != req6 {
		t.Fatalf("keyless request must be returned as-is")
	}
}

func TestContinue95HttpapiParseGeminiModelPath(t *testing.T) {
	cases := []struct {
		rest   string
		model  string
		stream bool
		ok     bool
	}{
		{"gemini-2.5-flash:generateContent", "gemini-2.5-flash", false, true},
		{"gemini-2.5-flash:streamGenerateContent", "gemini-2.5-flash", true, true},
		{"models/gemini-2.5-flash:generateContent", "models/gemini-2.5-flash", false, true},
		{":generateContent", "", false, false},
		{":streamGenerateContent", "", true, false},
		{"gemini-2.5-flash:countTokens", "", false, false},
		{"gemini-2.5-flash", "", false, false},
		{"", "", false, false},
	}
	for _, tc := range cases {
		model, stream, ok := parseGeminiModelPath(tc.rest)
		if model != tc.model || stream != tc.stream || ok != tc.ok {
			t.Fatalf("parseGeminiModelPath(%q) = (%q,%v,%v), want (%q,%v,%v)", tc.rest, model, stream, ok, tc.model, tc.stream, tc.ok)
		}
	}
}

func TestContinue95HttpapiGeminiModelsMethodAndAuth(t *testing.T) {
	// Method rejection on /v1beta/models.
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1beta/models", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	Continue95HttpapiAssertGeminiError(t, rr, http.StatusMethodNotAllowed, "INTERNAL")

	// Listing works without client auth when auth is disabled.
	req = httptest.NewRequest(http.MethodGet, "http://gateway/v1beta/models", nil)
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("models list = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"name":"models/upstream-model"`) {
		t.Fatalf("models list lacks models/upstream-model: %s", rr.Body.String())
	}

	// With client auth enabled, the models endpoint rejects a missing key.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.ClientAuth = config.ClientAuthConfig{Enabled: true, Keys: []string{"test-client-key"}}
	up := &Continue95HttpapiFakeUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "test", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	authServer := testGateway(t, cfg)
	authServer.SyncCapabilityContracts()
	req = httptest.NewRequest(http.MethodGet, "http://gateway/v1beta/models", nil)
	rr = httptest.NewRecorder()
	authServer.Handler().ServeHTTP(rr, req)
	Continue95HttpapiAssertGeminiError(t, rr, http.StatusUnauthorized, "UNAUTHENTICATED")
	req = httptest.NewRequest(http.MethodGet, "http://gateway/v1beta/models", nil)
	req.Header.Set("x-api-key", "test-client-key")
	rr = httptest.NewRecorder()
	authServer.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authed models list = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiGenerateRejects(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{}
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 1)
	body := Continue95HttpapiGeminiBody(t)
	cases := []struct {
		name string
		url  string
		body string
		code int
	}{
		{"unknown action", "http://gateway/v1beta/models/m:countTokens", body, http.StatusNotFound},
		{"deep model path", "http://gateway/v1beta/models/a/b:generateContent", body, http.StatusBadRequest},
		{"models-prefixed deep path", "http://gateway/v1beta/models/models/a/b:generateContent", body, http.StatusBadRequest},
		{"malformed JSON", "http://gateway/v1beta/models/m:generateContent", `{bad`, http.StatusBadRequest},
		{"empty contents", "http://gateway/v1beta/models/m:generateContent", `{"contents":[]}`, http.StatusBadRequest},
		{"candidateCount", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":3}}`, http.StatusBadRequest},
		{"responseModality", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseModalities":["IMAGE"]}}`, http.StatusBadRequest},
		{"safetySettings", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT"}]}`, http.StatusBadRequest},
		{"unsupported role", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"alien","parts":[{"text":"hi"}]}]}`, http.StatusBadRequest},
		{"unsupported part", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"text/plain"}}]}]}`, http.StatusBadRequest},
		{"inlineData without data", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png"}}]}]}`, http.StatusBadRequest},
		{"bad function call mode", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}}}`, http.StatusBadRequest},
		{"nameless function declaration", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"description":"d"}]}]}`, http.StatusBadRequest},
		{"empty parts", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[]}]}`, http.StatusBadRequest},
		{"second role-less content", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"parts":[{"text":"hi"}]},{"parts":[{"text":"yo"}]}]}`, http.StatusBadRequest},
		{"oversized body", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"` + strings.Repeat("x", 17<<20) + `"}]}]}`, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		rr := Continue95HttpapiGeminiPost(t, s, tc.url, tc.body)
		if rr.Code != tc.code {
			t.Fatalf("%s: status=%d want %d body=%.200s", tc.name, rr.Code, tc.code, rr.Body.String())
		}
		var env map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: error is not JSON: %v", tc.name, err)
		}
		if _, ok := env["error"].(map[string]any); !ok {
			t.Fatalf("%s: missing Gemini error envelope: %s", tc.name, rr.Body.String())
		}
	}
	// Bad method on the generate endpoint.
	req := httptest.NewRequest(http.MethodGet, "http://gateway/v1beta/models/m:generateContent", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	Continue95HttpapiAssertGeminiError(t, rr, http.StatusMethodNotAllowed, "INTERNAL")
	if calls := up.Continue95HttpapiCalls(); calls != 0 {
		t.Fatalf("rejected requests must not reach upstream, got %d calls", calls)
	}
}

func TestContinue95HttpapiGeminiVisionToolChoiceAndSystem(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{}
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 1)
	// Vision part + required tool choice + system instruction in one call.
	body := `{"systemInstruction":{"parts":[{"text":"Be brief."}]},"tools":[{"functionDeclarations":[{"name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}},"contents":[{"role":"user","parts":[{"text":"look at this"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}],"generationConfig":{"maxOutputTokens":64}}`
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("vision+toolChoice status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "chat hello") {
		t.Fatalf("vision+toolChoice body lacks chat hello: %s", rr.Body.String())
	}
	if calls := up.Continue95HttpapiCalls(); calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestContinue95HttpapiGeminiTooComplex(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{}
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 1)
	var sb strings.Builder
	sb.WriteString(`{"contents":[{"role":"user","parts":[`)
	for i := 0; i < 100001; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"text":"x"}`)
	}
	sb.WriteString(`]}]}`)
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", sb.String())
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "too complex") {
		t.Fatalf("too-complex status=%d body=%.300s, want 400 too complex", rr.Code, rr.Body.String())
	}
	if calls := up.Continue95HttpapiCalls(); calls != 0 {
		t.Fatalf("too-complex request reached upstream %d times", calls)
	}
}

func TestContinue95HttpapiGeminiNoEligibleAndResolveErrors(t *testing.T) {
	// Unknown model with fallback disabled: no candidates -> 503.
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.FallbackOnUnknownModel = false
	up := &Continue95HttpapiFakeUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "test", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/nope:generateContent", Continue95HttpapiGeminiBody(t))
	Continue95HttpapiAssertGeminiError(t, rr, http.StatusServiceUnavailable, "UNAVAILABLE")
	if calls := up.Continue95HttpapiCalls(); calls != 0 {
		t.Fatalf("no-eligible request reached upstream %d times", calls)
	}

	// Disabled virtual endpoint: resolve error containing "disabled" -> 404.
	falseVal := false
	cfg2 := config.Default()
	cfg2.Probe.Enabled = false
	cfg2.Routing.MaxAttempts = 1
	cfg2.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg2.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg2.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "ve-off", RouteProfile: "profile1", Enabled: &falseVal}}
	cfg2.Providers = []config.ProviderConfig{{
		ID: "g", Name: "test", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s2 := testGateway(t, cfg2)
	s2.SyncCapabilityContracts()
	rr = Continue95HttpapiGeminiPost(t, s2, "http://gateway/v1beta/models/ve-off:generateContent", Continue95HttpapiGeminiBody(t))
	Continue95HttpapiAssertGeminiError(t, rr, http.StatusNotFound, "NOT_FOUND")
	if !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatalf("disabled VE error lacks 'disabled': %s", rr.Body.String())
	}

	// Protocol-restricted virtual endpoint: resolve error without
	// "disabled" -> 400.
	trueVal := true
	cfg3 := config.Default()
	cfg3.Probe.Enabled = false
	cfg3.Routing.MaxAttempts = 1
	cfg3.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg3.RouteProfiles = []config.RouteProfileConfig{{ID: "profile1", CandidatePool: "pool1"}}
	cfg3.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "ve-openai", RouteProfile: "profile1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg3.Providers = cfg2.Providers
	s3 := testGateway(t, cfg3)
	s3.SyncCapabilityContracts()
	rr = Continue95HttpapiGeminiPost(t, s3, "http://gateway/v1beta/models/ve-openai:generateContent", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("protocol-restricted VE status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiVirtualEndpointSuccess(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.Strategy = "priority"
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "coding", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "coding-smart", CandidatePool: "coding"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding-prod", PublicModel: "ve-gemini", RouteProfile: "coding-smart", Enabled: &trueVal}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/ve-gemini:generateContent", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK {
		t.Fatalf("virtual VE gemini status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Gateway-Virtual-Endpoint") != "coding-prod" {
		t.Fatalf("X-Gateway-Virtual-Endpoint = %q, want coding-prod", rr.Header().Get("X-Gateway-Virtual-Endpoint"))
	}
	if rr.Header().Get("X-Gateway-Public-Model") != "ve-gemini" {
		t.Fatalf("X-Gateway-Public-Model = %q, want ve-gemini", rr.Header().Get("X-Gateway-Public-Model"))
	}
	if rr.Header().Get("X-Gateway-Route-Profile") != "coding-smart" {
		t.Fatalf("X-Gateway-Route-Profile = %q, want coding-smart", rr.Header().Get("X-Gateway-Route-Profile"))
	}
	if !strings.Contains(rr.Body.String(), "chat hello") {
		t.Fatalf("virtual VE body lacks chat hello: %s", rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiUpstreamFailures(t *testing.T) {
	// Generic 500 -> gateway 502 with a Gemini envelope; 429 passes
	// through as 429; malformed 200 -> 502 invalid response.
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"upstream 500", http.StatusInternalServerError, `{"error":{"message":"boom"}}`, http.StatusInternalServerError},
		{"upstream 429", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests},
		{"upstream 404", http.StatusNotFound, `{"error":{"message":"no such model"}}`, http.StatusNotFound},
		{"malformed 200", http.StatusOK, `not json{{{`, http.StatusBadGateway},
		{"retired 410", http.StatusGone, `{"error":{"message":"this model is retired"}}`, http.StatusServiceUnavailable},
		{"temporarily unavailable 503", http.StatusServiceUnavailable, `{"error":{"message":"model temporarily unavailable"}}`, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		up := &Continue95HttpapiFakeUpstream{status: tc.status, body: tc.body}
		s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 1)
		rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
		if rr.Code != tc.want {
			t.Fatalf("%s: status=%d want %d body=%s", tc.name, rr.Code, tc.want, rr.Body.String())
		}
		var env map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: error is not JSON: %v", tc.name, err)
		}
		if _, ok := env["error"].(map[string]any); !ok {
			t.Fatalf("%s: missing Gemini error envelope: %s", tc.name, rr.Body.String())
		}
		if calls := up.Continue95HttpapiCalls(); calls != 1 {
			t.Fatalf("%s: upstream calls = %d, want 1", tc.name, calls)
		}
	}
}

func TestContinue95HttpapiGeminiTransportFailureAndFailover(t *testing.T) {
	// Single deployment whose upstream is closed: transport error -> 502.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "dead", Type: "openai_compatible", BaseURL: deadURL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "all candidate deployments failed") {
		t.Fatalf("transport failure status=%d body=%s, want 502 all candidates failed", rr.Code, rr.Body.String())
	}

	// Two deployments: the first returns 500 (failover-eligible), the
	// second succeeds; the response must come from the second.
	first := &Continue95HttpapiFakeUpstream{status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`}
	second := &Continue95HttpapiFakeUpstream{}
	s2 := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{first, second}, 2)
	rr = Continue95HttpapiGeminiPost(t, s2, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "chat hello") {
		t.Fatalf("failover status=%d body=%s, want 200 chat hello", rr.Code, rr.Body.String())
	}
	if first.Continue95HttpapiCalls() != 1 || second.Continue95HttpapiCalls() != 1 {
		t.Fatalf("failover calls: first=%d second=%d, want 1/1", first.Continue95HttpapiCalls(), second.Continue95HttpapiCalls())
	}

	// Transport-error failover: first upstream dead, second healthy.
	dead2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead2URL := dead2.URL
	dead2.Close()
	healthy := &Continue95HttpapiFakeUpstream{}
	healthySrv := httptest.NewServer(healthy)
	t.Cleanup(healthySrv.Close)
	cfg2 := config.Default()
	cfg2.Probe.Enabled = false
	cfg2.Routing.MaxAttempts = 2
	cfg2.Providers = []config.ProviderConfig{
		{ID: "ga", Name: "dead", Type: "openai_compatible", BaseURL: dead2URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
				Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "gb", Name: "healthy", Type: "openai_compatible", BaseURL: healthySrv.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1,
				Capabilities: config.Capabilities{Streaming: true}}}},
	}
	s3 := testGateway(t, cfg2)
	s3.SyncCapabilityContracts()
	rr = Continue95HttpapiGeminiPost(t, s3, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "chat hello") {
		t.Fatalf("transport failover status=%d body=%s, want 200 chat hello", rr.Code, rr.Body.String())
	}
	if healthy.Continue95HttpapiCalls() != 1 {
		t.Fatalf("healthy upstream calls = %d, want 1", healthy.Continue95HttpapiCalls())
	}
}

func TestContinue95HttpapiGeminiNativeUpstreamErrorPassthrough(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{status: http.StatusBadRequest, body: `{"error":{"code":400,"message":"native gemini bad request","status":"INVALID_ARGUMENT"}}`}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "native", Type: "gemini", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("native error status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "native gemini bad request") {
		t.Fatalf("native error body not passed through: %s", rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiStreamSuccess(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{stream: true}
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 1)
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:streamGenerateContent?alt=sse", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	for _, want := range []string{`"text":"stream "`, `"text":"hello"`, `"finishReason":"STOP"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("Gemini stream must not use the [DONE] sentinel: %s", out)
	}
	// Query-form streaming (alt=sse on the non-stream action) too.
	rr = Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent?alt=sse", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"text":"hello"`) {
		t.Fatalf("alt=sse stream status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiBuildCandidateFailure(t *testing.T) {
	// A candidate whose capability contract marks tools unsupported
	// cannot serve a tool-bearing request: buildCanonicalAttempt
	// fails, the loop ends with "candidate could not serve", and the
	// handler falls through to the all-candidates-failed error.
	up := &Continue95HttpapiFakeUpstream{}
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 1)
	s.CapabilityStore().LearnUnsupported("ga/m", compat.CapTools, "continue95", "tools verified unsupported", "")
	body := `{"tools":[{"functionDeclarations":[{"name":"lookup","description":"d"}]}],"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "candidate could not serve this request") {
		t.Fatalf("capability-skipped status=%d body=%s, want 502 candidate could not serve", rr.Code, rr.Body.String())
	}
	if calls := up.Continue95HttpapiCalls(); calls != 0 {
		t.Fatalf("capability-skipped request reached upstream %d times", calls)
	}
}

// --- route-preview additions ---

func Continue95HttpapiPreviewRequest(t *testing.T, s *Server, query string) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/route-preview?"+query, nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var resp map[string]any
	if strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode preview body: %v (%s)", err, rr.Body.String())
		}
	}
	return rr.Code, resp, rr
}

func TestContinue95HttpapiRoutePreviewDisabledEndpoint(t *testing.T) {
	s, _ := codingPreviewServer(t)
	// codingPreviewServer's "coding" endpoint is enabled; rebuild with a
	// disabled endpoint instead.
	falseVal := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Providers = []config.ProviderConfig{{
		ID: "pA", Name: "A", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "physical-A", Enabled: true, Weight: 1, ContextWindow: 200000,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "coding-pool", Mode: "explicit", Deployments: []string{"pA/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "coding-profile", CandidatePool: "coding-pool"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding", PublicModel: "coding", RouteProfile: "coding-profile", Enabled: &falseVal, Protocols: []string{"openai"}}}
	disabled := testGateway(t, cfg)
	code, resp, _ := Continue95HttpapiPreviewRequest(t, disabled, "model=coding&protocol=openai")
	if code != http.StatusOK {
		t.Fatalf("disabled preview status=%d body=%v, want 200 with an error block", code, resp)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "disabled") {
		t.Fatalf("disabled preview error = %q, want it to mention disabled", errMsg)
	}
	if resp["selected"] != nil {
		t.Fatalf("disabled preview selected = %v, want nil", resp["selected"])
	}
	if _, ok := resp["virtual_endpoint"].(map[string]any); !ok {
		t.Fatalf("disabled preview lacks virtual_endpoint block: %#v", resp)
	}
	_ = s
}

func TestContinue95HttpapiRoutePreviewComparisonAddedAndPools(t *testing.T) {
	// Baseline virtual endpoint admits only pA/m; comparing against a
	// profile whose pools admit both deployments reports pB/m as added.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	for i, name := range []string{"A", "B"} {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "p" + name, Name: name, Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "physical-" + name, Enabled: true, Priority: i, Weight: 1, ContextWindow: 200000,
				Capabilities: config.Capabilities{Streaming: true}}},
		})
	}
	trueVal := true
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "pool-a", Mode: "explicit", Deployments: []string{"pA/m"}},
		{ID: "pool-both", Mode: "explicit", Deployments: []string{"pA/m", "pB/m"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "profile-a", CandidatePool: "pool-a"},
		{ID: "profile-both", CandidatePool: "pool-both"},
		{ID: "profile-both-private", CandidatePool: "pool-both", Privacy: "no_training", FallbackChain: "chain-both"},
		{ID: "profile-missing-pool", CandidatePool: "missing-pool"},
		{ID: "profile-missing-chain", CandidatePool: "pool-a", FallbackChain: "missing-chain"},
	}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "chain-both", Pools: []string{"pool-both"}}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding", PublicModel: "coding", RouteProfile: "profile-a", Enabled: &trueVal, Protocols: []string{"openai"}}}
	s := testGateway(t, cfg)

	code, resp, _ := Continue95HttpapiPreviewRequest(t, s, "model=coding&protocol=openai&compare_profile=profile-both")
	if code != http.StatusOK {
		t.Fatalf("comparison status=%d body=%v", code, resp)
	}
	comp, _ := resp["comparison"].(map[string]any)
	if comp == nil {
		t.Fatalf("comparison missing: %#v", resp)
	}
	added, _ := comp["added_deployments"].([]any)
	found := false
	for _, a := range added {
		if a == "pB/m" {
			found = true
		}
	}
	if !found {
		t.Fatalf("added_deployments = %v, want it to contain pB/m", added)
	}
	if comp["selected_changed"] != false {
		t.Fatalf("selected_changed = %v, want false (both select pA/m; only the pool grows)", comp["selected_changed"])
	}

	// Privacy + fallback-chain profile: the comparison carries both
	// fields and still simulates.
	code, resp, _ = Continue95HttpapiPreviewRequest(t, s, "model=coding&protocol=openai&compare_profile=profile-both-private")
	if code != http.StatusOK {
		t.Fatalf("private comparison status=%d", code)
	}
	comp, _ = resp["comparison"].(map[string]any)
	if comp["fallback_chain"] != "chain-both" || comp["privacy"] != "no_training" {
		t.Fatalf("private comparison = %v, want fallback_chain chain-both and privacy no_training", comp)
	}

	// Unknown pool in the comparison profile: error block, no selection.
	code, resp, _ = Continue95HttpapiPreviewRequest(t, s, "model=coding&protocol=openai&compare_profile=profile-missing-pool")
	if code != http.StatusOK {
		t.Fatalf("missing-pool comparison status=%d", code)
	}
	comp, _ = resp["comparison"].(map[string]any)
	if comp["error"] == nil || comp["selected"] != nil {
		t.Fatalf("missing-pool comparison = %v, want an error and nil selected", comp)
	}

	// Unknown fallback chain: same error shape.
	code, resp, _ = Continue95HttpapiPreviewRequest(t, s, "model=coding&protocol=openai&compare_profile=profile-missing-chain")
	if code != http.StatusOK {
		t.Fatalf("missing-chain comparison status=%d", code)
	}
	comp, _ = resp["comparison"].(map[string]any)
	if comp["error"] == nil || comp["selected"] != nil {
		t.Fatalf("missing-chain comparison = %v, want an error and nil selected", comp)
	}
}

func TestContinue95HttpapiRoutePreviewPinnedPromotion(t *testing.T) {
	s, _ := codingPreviewServer(t)
	// Record a session pin for pB/m under the public model, then
	// preview with that session key: the pinned deployment must lead.
	s.rt.ObserveSession(router.Requirement{Model: "coding", SessionKey: "continue95-pin-1"}, "pB/m")
	code, resp, _ := Continue95HttpapiPreviewRequest(t, s, "model=coding&protocol=openai&session_key=continue95-pin-1")
	if code != http.StatusOK {
		t.Fatalf("pinned preview status=%d body=%v", code, resp)
	}
	selected, _ := resp["selected"].(map[string]any)
	if selected == nil || selected["deployment"] != "pB/m" {
		t.Fatalf("pinned preview selected = %v, want pB/m first", resp["selected"])
	}
}

func TestContinue95HttpapiPreviewBoolAndExpandPools(t *testing.T) {
	if got := previewBool(map[string][]string{"tools": {"yes"}}, "tools"); !got {
		t.Fatalf("previewBool yes = false")
	}
	if got := previewBool(map[string][]string{"tools": {"TRUE"}}, "tools"); !got {
		t.Fatalf("previewBool TRUE = false")
	}
	if got := previewBool(map[string][]string{"tools": {"0"}}, "tools"); got {
		t.Fatalf("previewBool 0 = true")
	}
	if got := previewBool(map[string][]string{"tools": {"maybe"}}, "tools"); got {
		t.Fatalf("previewBool maybe = true, default branch must be false")
	}
	if got := previewBool(map[string][]string{}, "tools"); got {
		t.Fatalf("previewBool missing = true")
	}
	if got := previewBool(map[string][]string{"tools": {}}, "tools"); got {
		t.Fatalf("previewBool empty slice = true")
	}

	// expandProfilePools: nil resolver, unknown chain, unknown pool.
	if _, ok := expandProfilePools(config.Default(), nil, config.RouteProfileConfig{ID: "x", CandidatePool: "pool-a"}); ok {
		t.Fatalf("nil resolver must return false")
	}
	s, _ := codingPreviewServer(t)
	cfg := config.Default()
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "chain-ok", Pools: []string{"coding-pool"}}}
	_ = cfg
	if _, ok := expandProfilePools(config.Default(), s.routeResolver, config.RouteProfileConfig{ID: "x", CandidatePool: "pool-a", FallbackChain: "missing-chain"}); ok {
		t.Fatalf("unknown fallback chain must return false")
	}
	if _, ok := expandProfilePools(config.Default(), s.routeResolver, config.RouteProfileConfig{ID: "x", CandidatePool: "missing-pool"}); ok {
		t.Fatalf("unknown pool must return false")
	}
	members, ok := expandProfilePools(config.Default(), s.routeResolver, config.RouteProfileConfig{ID: "x", CandidatePool: "coding-pool"})
	if !ok {
		t.Fatalf("known pool must expand")
	}
	if _, has := members["pA/m"]; !has {
		t.Fatalf("expanded coding-pool lacks pA/m: %v", members)
	}
}

func TestContinue95HttpapiServerHelpers(t *testing.T) {
	// resolveVirtualEndpoint with no resolver: not virtual, no error.
	empty := &Server{}
	if _, isVirtual, err := empty.resolveVirtualEndpoint("anything"); isVirtual || err != nil {
		t.Fatalf("nil-resolver resolve = (%v,%v), want (false,nil)", isVirtual, err)
	}
	// Same-origin checks: userinfo / malformed referers are rejected.
	badUser := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
	badUser.Host = "gateway"
	badUser.Header.Set("Referer", "http://user@gateway/")
	if sameOrigin(badUser) {
		t.Fatalf("referer with userinfo must not be same-origin")
	}
	badURL := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
	badURL.Host = "gateway"
	badURL.Header.Set("Referer", "http://%zz/")
	if sameOrigin(badURL) {
		t.Fatalf("malformed referer must not be same-origin")
	}
	good := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
	good.Host = "gateway"
	good.Header.Set("Referer", "http://gateway/admin")
	if !sameOrigin(good) {
		t.Fatalf("same-host http referer must be same-origin")
	}
	// CSRF token: an existing cookie value is reused verbatim.
	s, _ := codingPreviewServer(t)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/csrf", nil)
	req.AddCookie(&http.Cookie{Name: adminCSRFCookie, Value: "continue95-csrf-token"})
	rr := httptest.NewRecorder()
	s.adminCSRFToken(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("csrf with cookie = %d: %s", rr.Code, rr.Body.String())
	}
	found := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == adminCSRFCookie && c.Value == "continue95-csrf-token" {
			found = true
		}
	}
	if !found {
		t.Fatalf("csrf cookie not echoed: %v", rr.Result().Cookies())
	}
	// convertDecisionPolicies skips an invalid policy instead of failing.
	out := convertDecisionPolicies([]config.DecisionPolicyConfig{{ID: "bad", SelectionMode: "bogus"}})
	if len(out) != 0 {
		t.Fatalf("invalid policy must be skipped, got %d policies", len(out))
	}
	// applyRollout with no canary (or canary at index 0) returns input.
	in := []router.Scored{{Deployment: router.Deployment{ID: "pA/m"}}, {Deployment: router.Deployment{ID: "pB/m"}}}
	got := applyRollout(config.Default(), nil, router.Requirement{}, in)
	if len(got) != 2 || got[0].Deployment.ID != "pA/m" {
		t.Fatalf("applyRollout no-canary = %v, want input order", got)
	}
}

// Continue95 wave 2 (issue #663): server helpers, hedging, semantic cache,
// autopilot internals, and evaluation-state edges.

func TestContinue95HttpapiServerVirtualEndpointProtocol(t *testing.T) {
	if err := checkVirtualEndpointProtocol(nil, route.ResolvedRoute{}, "openai"); err != nil {
		t.Fatalf("nil resolver = %v, want nil", err)
	}
	cfg := config.Default()
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{
		ID: "ve1", PublicModel: "ve-model", Protocols: []string{"openai_responses"},
	}}
	resolver := route.NewResolver(cfg, nil)
	if err := checkVirtualEndpointProtocol(resolver, route.ResolvedRoute{VirtualEndpointID: "ve1"}, "responses"); err != nil {
		t.Fatalf("responses alias = %v, want nil", err)
	}
	if err := checkVirtualEndpointProtocol(resolver, route.ResolvedRoute{VirtualEndpointID: "ve1"}, "openai"); err == nil {
		t.Fatalf("openai protocol against openai_responses-only VE = nil, want error")
	}
}

func TestContinue95HttpapiServerCurrentRouteCandidateMissing(t *testing.T) {
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	if _, _, ok := srv.currentRouteCandidate("ga/m", router.Requirement{}); !ok {
		t.Fatalf("candidate ga/m should resolve before registry swap")
	}
	emptyCfg := config.Default()
	emptyCfg.ApplyDefaults()
	emptyReg, err := providers.NewRegistry(emptyCfg)
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	srv.runtimeMu.Lock()
	srv.reg = emptyReg
	srv.runtimeMu.Unlock()
	if _, _, ok := srv.currentRouteCandidate("ga/m", router.Requirement{}); ok {
		t.Fatalf("candidate resolved with adapter missing from registry, want !ok")
	}
}

func TestContinue95HttpapiHedgeDirect(t *testing.T) {
	// normalizeHedgeResult synthesises an error for a nil/nil result.
	if out := normalizeHedgeResult(hedgeResult{}); out.err == nil {
		t.Fatalf("nil hedge result produced no error")
	}
	// abandonHedgeLeg with a nil leg is a no-op, not a panic.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	srv.abandonHedgeLeg(nil, "ga/m", "continue95-nil-leg", "nil leg")
	// boundedProtocolMappingError: nil passes through, long errors truncate.
	if err := boundedProtocolMappingError(nil); err != nil {
		t.Fatalf("nil mapping error = %v, want nil", err)
	}
	long := boundedProtocolMappingError(errors.New(strings.Repeat("x", 600)))
	if long == nil || len(long.Error()) > 550 || !strings.HasSuffix(long.Error(), "...") {
		t.Fatalf("long mapping error not truncated: len=%d", len(long.Error()))
	}
	// buildOpenAIAttempt / buildAnthropicAttempt with an unknown deployment.
	cand := router.Scored{Deployment: router.Deployment{ID: "nosuch/m"}}
	if _, ok := srv.buildOpenAIAttempt(cand, router.Requirement{}, nil, core.OpenAIRequest{}); ok {
		t.Fatalf("openai attempt for unknown deployment succeeded")
	}
	if _, ok := srv.buildAnthropicAttempt(cand, router.Requirement{}, nil, core.AnthropicRequest{}); ok {
		t.Fatalf("anthropic attempt for unknown deployment succeeded")
	}
}

func TestContinue95HttpapiHedgeBuildDecodeErrors(t *testing.T) {
	fake := &Continue95HttpapiFakeUpstream{}
	up := httptest.NewServer(fake)
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "g1", Type: "gemini", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-model", Enabled: true, Weight: 1}},
	}}
	srv := testGateway(t, cfg)
	sc, ok := srv.rt.Eligible("g1/m", router.Requirement{})
	if !ok {
		t.Fatalf("gemini deployment g1/m not eligible")
	}
	bundle, ok := srv.buildOpenAIAttempt(sc, router.Requirement{}, nil,
		core.OpenAIRequest{Model: "m", Messages: []core.OpenAIMessage{{Role: "user",
			Content: []any{map[string]any{"type": "bogus_part", "text": "hi"}}}}})
	if ok || bundle.buildErr == nil {
		t.Fatalf("openai decode error: ok=%v buildErr=%v, want !ok with error", ok, bundle.buildErr)
	}
	bundle, ok = srv.buildAnthropicAttempt(sc, router.Requirement{}, nil,
		core.AnthropicRequest{Model: "m", MaxTokens: 5, Messages: []core.AnthMessage{{Role: "bogus", Content: json.RawMessage(`"hi"`)}}})
	if ok || bundle.buildErr == nil {
		t.Fatalf("anthropic decode error: ok=%v buildErr=%v, want !ok with error", ok, bundle.buildErr)
	}
}

func TestContinue95HttpapiHedgedUpstreamDo(t *testing.T) {
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	block := func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		return nil, ctx.Err()
	}
	// Primary errors immediately with no secondary launched: fast return.
	out := srv.hedgedUpstreamDo(context.Background(), "continue95-fast-err", "ga/m", "gb/m", 10*time.Second,
		func(ctx context.Context) (*http.Response, error) { return nil, errors.New("primary boom") }, block)
	if out.err == nil || out.err.Error() != "primary boom" || out.hedgeLaunched {
		t.Fatalf("primary fast error: err=%v hedgeLaunched=%v", out.err, out.hedgeLaunched)
	}
	// Secondary wins: primary blocks, hedge fires, secondary answers first.
	winBody := io.NopCloser(strings.NewReader(`{}`))
	out = srv.hedgedUpstreamDo(context.Background(), "continue95-sec-win", "ga/m", "gb/m", time.Millisecond,
		block,
		func(ctx context.Context) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: winBody}, nil
		})
	if out.err != nil || !out.secondaryWon || out.resp == nil || out.resp.StatusCode != http.StatusOK || !out.hedgeLaunched {
		t.Fatalf("secondary win: err=%v secondaryWon=%v resp=%v", out.err, out.secondaryWon, out.resp)
	}
	// Primary wins after the hedge launched: the loser is abandoned.
	out = srv.hedgedUpstreamDo(context.Background(), "continue95-prim-win", "ga/m", "gb/m", time.Millisecond,
		func(ctx context.Context) (*http.Response, error) {
			time.Sleep(50 * time.Millisecond)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}, block)
	if out.err != nil || out.secondaryWon || !out.hedgeLaunched || out.resp == nil {
		t.Fatalf("primary win: err=%v secondaryWon=%v hedgeLaunched=%v", out.err, out.secondaryWon, out.hedgeLaunched)
	}
	// Route context cancelled while both legs are in flight: both abandoned.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	out = srv.hedgedUpstreamDo(ctx, "continue95-cancelled", "ga/m", "gb/m", time.Millisecond, block, block)
	if out.err != context.Canceled || !out.hedgeLaunched {
		t.Fatalf("cancelled race: err=%v hedgeLaunched=%v, want context.Canceled + launched", out.err, out.hedgeLaunched)
	}
}

func Continue95HttpapiSemanticServer(t *testing.T, embedBody string, embedStatus int) *Server {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(embedStatus)
		_, _ = io.WriteString(w, embedBody)
	}))
	t.Cleanup(fake.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "p1/m1"
	cfg.Cache.Semantic.EmbeddingPath = "/v1/embeddings"
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: fake.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "embed-model", Enabled: true, Weight: 1}},
	}}
	return testGateway(t, cfg)
}

func TestContinue95HttpapiSemanticPrepareNilBranches(t *testing.T) {
	srv := Continue95HttpapiSemanticServer(t, `{"data":[{"embedding":[0.1,0.2]}]}`, http.StatusOK)
	raw := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	newReq := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
	}
	if sc := srv.semanticPrepare(newReq(), "/v1/chat/completions", raw, true, nil, nil, false, "p1/m1", false, nil); sc != nil {
		t.Fatalf("stream request prepared a semantic context")
	}
	req := newReq()
	req.Header.Set("x-nexaroute-no-cache", "1")
	if sc := srv.semanticPrepare(req, "/v1/chat/completions", raw, false, nil, nil, false, "p1/m1", false, nil); sc != nil {
		t.Fatalf("no-cache header prepared a semantic context")
	}
	temp := 0.5
	if sc := srv.semanticPrepare(newReq(), "/v1/chat/completions", raw, false, &temp, nil, false, "p1/m1", false, nil); sc != nil {
		t.Fatalf("non-zero temperature prepared a semantic context")
	}
	topP := 0.5
	if sc := srv.semanticPrepare(newReq(), "/v1/chat/completions", raw, false, nil, &topP, false, "p1/m1", false, nil); sc != nil {
		t.Fatalf("top_p != 1 prepared a semantic context")
	}
	srv.runtimeMu.Lock()
	srv.cfg.Cache.MaxBodyBytes = 10
	srv.runtimeMu.Unlock()
	if sc := srv.semanticPrepare(newReq(), "/v1/chat/completions", raw, false, nil, nil, false, "p1/m1", false, nil); sc != nil {
		t.Fatalf("oversized body prepared a semantic context")
	}
	srv.runtimeMu.Lock()
	srv.cfg.Cache.MaxBodyBytes = 1 << 20
	srv.runtimeMu.Unlock()
	if sc := srv.semanticPrepare(newReq(), "/v1/chat/completions", []byte(`{}`), false, nil, nil, false, "p1/m1", false, nil); sc != nil {
		t.Fatalf("body without prompt text prepared a semantic context")
	}
	srv.runtimeMu.Lock()
	srv.cfg.Cache.Semantic.EmbeddingDeployment = "bad"
	srv.runtimeMu.Unlock()
	if sc := srv.semanticPrepare(newReq(), "/v1/chat/completions", raw, false, nil, nil, false, "p1/m1", false, nil); sc != nil {
		t.Fatalf("embedding failure prepared a semantic context")
	}
	// Positive control: a well-formed request with a working embedder prepares.
	srv2 := Continue95HttpapiSemanticServer(t, `{"data":[{"embedding":[0.1,0.2,0.3]}]}`, http.StatusOK)
	if sc := srv2.semanticPrepare(newReq(), "/v1/chat/completions", raw, false, nil, nil, false, "p1/m1", false, nil); sc == nil || sc.scope == "" || len(sc.vector) != 3 {
		t.Fatalf("positive semantic prepare = %+v, want scope + 3-dim vector", sc)
	}
}

func TestContinue95HttpapiEmbedTextErrors(t *testing.T) {
	srv := Continue95HttpapiSemanticServer(t, `{"data":[{"embedding":[0.1,0.2]}]}`, http.StatusOK)
	setDep := func(dep string) {
		srv.runtimeMu.Lock()
		srv.cfg.Cache.Semantic.EmbeddingDeployment = dep
		srv.runtimeMu.Unlock()
	}
	if _, err := srv.embedText(context.Background(), "hi"); err != nil {
		t.Fatalf("baseline embed unexpectedly failed: %v", err)
	}
	setDep("bad")
	if _, err := srv.embedText(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "invalid embedding deployment") {
		t.Fatalf("malformed deployment: err=%v", err)
	}
	setDep("nosuch/m")
	if _, err := srv.embedText(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "provider not found") {
		t.Fatalf("unknown provider: err=%v", err)
	}
	setDep("p1/nosuch")
	if _, err := srv.embedText(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("unknown model: err=%v", err)
	}
	// Provider present in config but adapter missing from the registry.
	// providerConfigFor reads the cached config snapshot, so the snapshot
	// must be invalidated after mutating srv.cfg directly.
	srv.runtimeMu.Lock()
	srv.cfg.Providers = append(srv.cfg.Providers, config.ProviderConfig{
		ID: "p9", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "embed-model", Enabled: true, Weight: 1}},
	})
	srv.runtimeMu.Unlock()
	srv.cfgSnap.Store(nil)
	setDep("p9/m1")
	if _, err := srv.embedText(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "adapter not found") {
		t.Fatalf("missing adapter: err=%v", err)
	}
	// Upstream transport failure (closed listener), non-200, and no-vector.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Cache.Semantic.Enabled = true
	cfg.Cache.Semantic.EmbeddingDeployment = "p1/m1"
	cfg.Cache.Semantic.EmbeddingPath = "/v1/embeddings"
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: closedURL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m1", Model: "embed-model", Enabled: true, Weight: 1}},
	}}
	if _, err := testGateway(t, cfg).embedText(context.Background(), "hi"); err == nil {
		t.Fatalf("closed upstream embed unexpectedly succeeded")
	}
	srv500 := Continue95HttpapiSemanticServer(t, `{"error":"boom"}`, http.StatusInternalServerError)
	if _, err := srv500.embedText(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("non-200 embed: err=%v", err)
	}
	srvEmpty := Continue95HttpapiSemanticServer(t, `{"data":[]}`, http.StatusOK)
	if _, err := srvEmpty.embedText(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "no vector") {
		t.Fatalf("no-vector embed: err=%v", err)
	}
}

func TestContinue95HttpapiSemanticStoreGeneration(t *testing.T) {
	srv := Continue95HttpapiSemanticServer(t, `{"data":[{"embedding":[0.1,0.2]}]}`, http.StatusOK)
	_, gen := srv.cacheConfigSnapshot()
	sc := &semanticContext{scope: "continue95-scope", vector: []float32{0.1, 0.2}, promptSHA256: "abc", embeddingDep: "p1/m1"}
	srv.semanticStore(sc, gen+1, "p1/m1", http.StatusOK, []byte(`{}`))
	if _, _, ok := srv.semCache.Lookup(sc.scope, sc.vector, false); ok {
		t.Fatalf("generation-mismatched store unexpectedly stored an entry")
	}
	srv.semanticStore(sc, gen, "p1/m1", http.StatusOK, []byte(`{}`))
	if _, _, ok := srv.semCache.Lookup(sc.scope, sc.vector, false); !ok {
		t.Fatalf("current-generation store did not store an entry")
	}
}

func TestContinue95HttpapiAutopilotDirect(t *testing.T) {
	policies := []policy.Policy{{ID: "balanced", Version: "1"}}
	disabled := config.Default()
	disabled.Autopilot.Enabled = false
	if ctrl := newAutopilotController(disabled, policies); ctrl != nil {
		t.Fatalf("disabled autopilot built a controller")
	}
	fallback := config.Default()
	fallback.Autopilot.Enabled = true
	fallback.Autopilot.PolicyID = ""
	fallback.Decision.Policy = "balanced"
	ctrl := newAutopilotController(fallback, policies)
	if ctrl == nil {
		t.Fatalf("empty PolicyID did not fall back to Decision.Policy")
	}
	missing := config.Default()
	missing.Autopilot.Enabled = true
	missing.Autopilot.PolicyID = "nope"
	if ctrl := newAutopilotController(missing, policies); ctrl != nil {
		t.Fatalf("unknown PolicyID built a controller")
	}
	// Caller stratum labels.
	if got := autopilotCallerStratum(clientIdentity{TenantID: "t1"}, true); got != "tenant:t1" {
		t.Fatalf("tenant stratum = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{ProjectID: "p1"}, true); got != "project:p1" {
		t.Fatalf("project stratum = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{ID: "x"}, true); got != "x" {
		t.Fatalf("id stratum = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{}, true); got != "anonymous" {
		t.Fatalf("empty stratum = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{}, false); got != "anonymous" {
		t.Fatalf("unknown stratum = %q", got)
	}
	// Status classification.
	cases := []struct {
		status int
		want   autopilot.ErrorClass
	}{
		{200, autopilot.ClassNone}, {429, autopilot.ClassRateLimit}, {401, autopilot.ClassAuthError},
		{504, autopilot.ClassTimeout}, {500, autopilot.ClassServerError}, {400, autopilot.ClassClientError},
	}
	for _, c := range cases {
		if got := classifyAutopilotStatus(c.status); got != c.want {
			t.Fatalf("status %d classified %v, want %v", c.status, got, c.want)
		}
	}
	// Lease lifecycle on a live server.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	srv.runtimeMu.Lock()
	srv.autopilot = ctrl
	srv.runtimeMu.Unlock()
	srv.autopilotRecordTTFT("continue95-neg", -time.Second)
	if got := srv.autopilotSignalsFor(""); got != nil {
		t.Fatalf("empty request id returned signals")
	}
	srv.autopilotLeases.Store("continue95-bad", "not-an-entry")
	if got := srv.autopilotSignalsFor("continue95-bad"); got != nil {
		t.Fatalf("wrong-type lease returned signals")
	}
	srv.autopilotRecordFinish("continue95-bad", 200, "succeeded", 0)
	policyID, leased := srv.autopilotSelectPolicy("balanced", "continue95-req-1")
	if !leased || policyID != "balanced" {
		t.Fatalf("select policy = %q leased=%v", policyID, leased)
	}
	srv.autopilotRecordFinish("continue95-req-1", 200, "succeeded", 5*time.Millisecond)
	if got := srv.autopilotSignalsFor("continue95-req-1"); got != nil {
		t.Fatalf("finished request still has signals")
	}
	if _, leased := srv.autopilotSelectPolicy("balanced", "continue95-req-2"); !leased {
		t.Fatalf("second select did not lease")
	}
	srv.autopilotRecordFinish("continue95-req-2", 0, "cancelled", 0)
	if got := srv.autopilotSignalsFor("continue95-req-2"); got != nil {
		t.Fatalf("cancelled request still has signals")
	}
	// ObserveNoEligible with a mismatched policy is a silent no-op.
	otherCfg := config.Default()
	otherCfg.Decision.Policy = "other-policy"
	srv.autopilotObserveNoEligible(otherCfg, nil, "continue95-noelig", "stratum")
}

func TestContinue95HttpapiEvalStateDirect(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	plane := newEvaluationPlane(config.EvaluationConfig{}, nil)
	if err := plane.loadState("a\x00b"); err == nil {
		t.Fatalf("NUL-byte path loaded without error")
	}
	runs := `{"version":1,"runs":[` + strings.Repeat("{},", 512) + `{}]}`
	if err := plane.loadState(write("runs.json", runs)); err == nil || !strings.Contains(err.Error(), "512 runs") {
		t.Fatalf("too many runs: err=%v", err)
	}
	cards := `{"version":1,"scorecards":[` + strings.Repeat("{},", 4096) + `{}]}`
	if err := plane.loadState(write("cards.json", cards)); err == nil || !strings.Contains(err.Error(), "4096 scorecards") {
		t.Fatalf("too many scorecards: err=%v", err)
	}
	valid := `{"version":1,"scorecards":[{"deployment_id":"p1/m1","version":0,"generated_at":"2026-10-29T00:00:00Z","values":{"coding":{"score":0.9,"confidence":0.8,"provenance":"operator_config","sample_count":0,"source":"continue95-config"}}}]}`
	if err := plane.loadState(write("fixup.json", valid)); err != nil {
		t.Fatalf("version-0 scorecard fixup failed: %v", err)
	}
	snap := plane.Registry().Snapshot()
	if len(snap) != 1 || snap[0].Version != 1 {
		t.Fatalf("fixup snapshot = %+v, want one card at version 1", snap)
	}
	noGen := `{"version":1,"scorecards":[{"deployment_id":"p1/m1","version":1,"values":{"coding":{"score":0.9,"confidence":0.8,"provenance":"operator_config","sample_count":0,"source":"continue95-config"}}}]}`
	if err := plane.loadState(write("nogen.json", noGen)); err == nil || !strings.Contains(err.Error(), "no generated_at") {
		t.Fatalf("missing generated_at: err=%v", err)
	}
	invalid := `{"version":1,"scorecards":[{"deployment_id":"","version":1,"generated_at":"2026-10-29T00:00:00Z","values":{"coding":{"score":0.9,"confidence":0.8,"provenance":"operator_config","sample_count":0,"source":"continue95-config"}}}]}`
	if err := plane.loadState(write("invalid.json", invalid)); err == nil || !strings.Contains(err.Error(), "deployment_id") {
		t.Fatalf("invalid scorecard: err=%v", err)
	}
	dup := `{"version":1,"scorecards":[
		{"deployment_id":"p1/m1","version":1,"generated_at":"2026-10-29T00:00:00Z","values":{"coding":{"score":0.9,"confidence":0.8,"provenance":"operator_config","sample_count":0,"source":"continue95-config"}}},
		{"deployment_id":"p1/m1","version":1,"generated_at":"2026-10-29T00:00:00Z","values":{"coding":{"score":0.8,"confidence":0.8,"provenance":"operator_config","sample_count":0,"source":"continue95-config"}}}]}`
	if err := plane.loadState(write("dup.json", dup)); err == nil || !strings.Contains(err.Error(), "duplicate scorecard") {
		t.Fatalf("duplicate scorecards: err=%v", err)
	}
	// Persist under a regular file fails at MkdirAll.
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatalf("write block: %v", err)
	}
	plane2 := newEvaluationPlane(config.EvaluationConfig{StatePath: filepath.Join(block, "state.json")}, nil)
	if err := plane2.persist(); err == nil {
		t.Fatalf("persist under a file unexpectedly succeeded")
	}
}

// ---- Wave 3 (issue #663 continuation) ----

// Continue95HttpapiSequenceUpstream serves a scripted sequence of
// responses, one per request, so failover and repair paths can be driven
// deterministically without timing tricks.
type Continue95HttpapiSequenceUpstream struct {
	mu      sync.Mutex
	actions []Continue95HttpapiUpstreamAction
	calls   int
}

type Continue95HttpapiUpstreamAction struct {
	status int
	body   string
	hangup bool // hijack + close: a transport error on the client side
}

func (u *Continue95HttpapiSequenceUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	u.mu.Lock()
	i := u.calls
	u.calls++
	u.mu.Unlock()
	if i >= len(u.actions) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"chat hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
		return
	}
	act := u.actions[i]
	if act.hangup {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
		return
	}
	status := act.status
	if status == 0 {
		status = http.StatusOK
	}
	body := act.body
	if body == "" {
		body = `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"chat hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Continue95HttpapiFakeAdapter implements providers.Adapter with a canned
// 200 response, for direct hedge/compat tests that need a bundle.
type Continue95HttpapiFakeAdapter struct {
	redact func([]byte) []byte
}

func (a *Continue95HttpapiFakeAdapter) ID() string   { return "continue95-fake" }
func (a *Continue95HttpapiFakeAdapter) Kind() string { return "openai_compatible" }
func (a *Continue95HttpapiFakeAdapter) Stats() providers.ProviderStats {
	return providers.ProviderStats{}
}
func (a *Continue95HttpapiFakeAdapter) CredentialsMatch([]string) bool { return true }
func (a *Continue95HttpapiFakeAdapter) RedactBody(b []byte) []byte {
	if a.redact != nil {
		return a.redact(b)
	}
	return b
}
func (a *Continue95HttpapiFakeAdapter) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}
func (a *Continue95HttpapiFakeAdapter) DoPath(ctx context.Context, method, path string, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return a.Do(ctx, payload, stream, forward)
}
func (a *Continue95HttpapiFakeAdapter) CountTokens(ctx context.Context, payload []byte, forward http.Header) (*http.Response, error) {
	return a.Do(ctx, payload, false, forward)
}
func (a *Continue95HttpapiFakeAdapter) Probe(ctx context.Context, model string, maxTokens int) (time.Duration, int, error) {
	return 0, http.StatusOK, nil
}

// Continue95HttpapiErrorWriter fails every Write, for SSE write-error paths.
type Continue95HttpapiErrorWriter struct {
	header http.Header
}

func (w *Continue95HttpapiErrorWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *Continue95HttpapiErrorWriter) Write([]byte) (int, error) {
	return 0, errors.New("continue95 write failure")
}
func (w *Continue95HttpapiErrorWriter) WriteHeader(int) {}

func Continue95HttpapiGeminiGatewayStrategy(t *testing.T, ups []*Continue95HttpapiFakeUpstream, maxAttempts int, strategy string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = maxAttempts
	cfg.Routing.Strategy = strategy
	for i, up := range ups {
		srv := httptest.NewServer(up)
		t.Cleanup(srv.Close)
		priority := i
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "g" + string(rune('a'+i)), Name: "test upstream", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: priority,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}},
		})
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func TestContinue95HttpapiCompatDirect(t *testing.T) {
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	// dialectFor with an unknown provider falls back to DetectDialect,
	// deterministically.
	first := srv.dialectFor("nosuch", "openai_compatible", "http://example.invalid")
	second := srv.dialectFor("nosuch", "openai_compatible", "http://example.invalid")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("dialectFor not deterministic: %#v vs %#v", first, second)
	}
	// learnFromSuccess with an unknown provider returns early.
	srv.learnFromSuccess("nosuch/m", "nosuch", router.Deployment{}, []byte(`{"temperature":0.5}`), nil)
	// toolConfig with a non-unsupported tool-choice capability learns success
	// (the toolConfig branch lives inside the generationConfig guard).
	srv.learnFromSuccess("ga/m", "ga", router.Deployment{ID: "ga/m", Model: "upstream-model"}, []byte(`{"generationConfig":{},"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`), nil)
	if got := srv.CapabilityStore().Get("ga/m").Capabilities.Get(compat.CapToolChoiceAuto); got != compat.Supported {
		t.Fatalf("tool-choice after learn = %v, want Supported", got)
	}
	// Top-level skip: a verified-UNSUPPORTED temperature is never flipped.
	srv.CapabilityStore().LearnUnsupported("ga/m", compat.CapTemperature, "continue95", "verified unsupported", "")
	srv.learnFromSuccess("ga/m", "ga", router.Deployment{ID: "ga/m"}, []byte(`{"temperature":0.5}`), nil)
	if got := srv.CapabilityStore().Get("ga/m").Capabilities.Get(compat.CapTemperature); got != compat.Unsupported {
		t.Fatalf("temperature after skip = %v, want Unsupported", got)
	}
	// generationConfig skip: same guard inside generationConfig.
	srv.learnFromSuccess("ga/m", "ga", router.Deployment{ID: "ga/m"}, []byte(`{"generationConfig":{"temperature":0.5}}`), nil)
	if got := srv.CapabilityStore().Get("ga/m").Capabilities.Get(compat.CapTemperature); got != compat.Unsupported {
		t.Fatalf("temperature after generationConfig skip = %v, want Unsupported", got)
	}
	// SyncCapabilityContracts with the provider removed from cfg skips the
	// deployment (provider-missing continue) without corrupting the store.
	srv.cfg.Providers = nil
	srv.cfgSnap.Store(nil)
	srv.SyncCapabilityContracts()
	if got := srv.CapabilityStore().Get("ga/m").Capabilities.Get(compat.CapTemperature); got != compat.Unsupported {
		t.Fatalf("temperature after provider-missing sync = %v, want Unsupported", got)
	}
	// maybeRepairUpstream clamps: negative -> 0 attempts, >2 -> 2 attempts.
	bundle := hedgeAttemptBundle{a: &Continue95HttpapiFakeAdapter{}, c: router.Scored{Deployment: router.Deployment{ID: "ga/m", ProviderID: "ga"}}}
	resp400 := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unknown parameter: temperature"}}`))}
	out, _, outcomeNeg := srv.maybeRepairUpstream(context.Background(), "continue95-neg", bundle, []byte(`{"temperature":0.5}`), resp400, false, nil, -1, compat.RequirementProfile{})
	if outcomeNeg.Repaired {
		t.Fatalf("negative maxRepairs unexpectedly repaired: %v", out)
	}
	resp400b := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unknown parameter: temperature"}}`))}
	out, _, _ = srv.maybeRepairUpstream(context.Background(), "continue95-big", bundle, []byte(`{"temperature":0.5}`), resp400b, false, nil, 5, compat.RequirementProfile{})
	_ = out
	// Read-error return: an unreadable body cannot be repaired.
	respErr := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(&Continue95HttpapiErrorReader{})}
	out, _, outcomeRead := srv.maybeRepairUpstream(context.Background(), "continue95-readerr", bundle, []byte(`{"temperature":0.5}`), respErr, false, nil, 2, compat.RequirementProfile{})
	if outcomeRead.Repaired || out == nil {
		t.Fatalf("read-error repair: repaired=%v out=%v, want repaired=false with original response", outcomeRead.Repaired, out)
	}
	// Non-repairable 400 ("maximum context length") returns Repair!ok with
	// the body restored.
	respCtx := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"maximum context length exceeded"}}`))}
	out, _, outcomeCtx := srv.maybeRepairUpstream(context.Background(), "continue95-ctx", bundle, []byte(`{"model":"m"}`), respCtx, false, nil, 2, compat.RequirementProfile{})
	if outcomeCtx.Repaired {
		t.Fatalf("context-length 400 unexpectedly repaired")
	}
	restored, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(restored), "maximum context length") {
		t.Fatalf("repaired response body not restored: %s", restored)
	}
	// finishCanonicalAttempt with an unknown provider returns false.
	if _, ok := srv.finishCanonicalAttempt(hedgeAttemptBundle{c: router.Scored{Deployment: router.Deployment{ID: "nosuch/m", ProviderID: "nosuch"}}}, canonical.Request{}, "gemini"); ok {
		t.Fatalf("finishCanonicalAttempt for unknown provider succeeded")
	}
	// sanitizeOutgoingPayload with sanitization disabled is a no-op.
	srv2 := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	srv2.cfg.Routing.SanitizeEnabled = false
	payload := []byte(`{"temperature":0.5}`)
	if got := srv2.sanitizeOutgoingPayload(hedgeAttemptBundle{}, payload, router.Requirement{}); string(got) != string(payload) {
		t.Fatalf("disabled sanitize changed payload: %s", got)
	}
	// adapterTransport.RedactBody delegates to the adapter.
	tr := adapterTransport{a: &Continue95HttpapiFakeAdapter{redact: func(b []byte) []byte { return []byte("[redacted]") }}}
	if got := tr.RedactBody([]byte(`{"api_key":"sk-secret"}`)); string(got) != "[redacted]" {
		t.Fatalf("RedactBody = %s, want [redacted]", got)
	}
}

func TestContinue95HttpapiGeminiPrivacyUnavailable(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.Strategy = "priority"
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "coding", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "coding-smart", CandidatePool: "coding", Privacy: "no_training"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding-prod", PublicModel: "ve-private", RouteProfile: "coding-smart", Enabled: &trueVal}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		DataHandling: config.DataHandlingConfig{TrainsOnData: "yes"},
		Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Priority: 0, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/ve-private:generateContent", Continue95HttpapiGeminiBody(t))
	Continue95HttpapiAssertGeminiError(t, rr, http.StatusServiceUnavailable, "UNAVAILABLE")
	if !strings.Contains(rr.Body.String(), "privacy") {
		t.Fatalf("privacy-unavailable body lacks privacy mention: %s", rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiMaxAttemptsClamp(t *testing.T) {
	up := &Continue95HttpapiFakeUpstream{}
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 5)
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK {
		t.Fatalf("clamped maxAttempts status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiFailoverPolicies(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		actions  []Continue95HttpapiUpstreamAction
	}{
		{"ready-quarantine-500", "ready_queue", []Continue95HttpapiUpstreamAction{
			{status: 500, body: `{"error":{"message":"boom"}}`}, {status: 200}}},
		{"nonready-hardcooldown-429", "priority", []Continue95HttpapiUpstreamAction{
			{status: 429, body: `{"error":{"message":"slow down"}}`}, {status: 200}}},
		{"nonready-recordfailure-500", "priority", []Continue95HttpapiUpstreamAction{
			{status: 500, body: `{"error":{"message":"boom"}}`}, {status: 200}}},
		{"retired-410", "priority", []Continue95HttpapiUpstreamAction{
			{status: 410, body: `{"error":{"code":"model_retired","message":"model m is retired"}}`}, {status: 200}}},
		{"temp-unavailable-503", "priority", []Continue95HttpapiUpstreamAction{
			{status: 503, body: `{"error":{"code":"model_unavailable","message":"model m is temporarily unavailable"}}`}, {status: 200}}},
		{"malformed-then-200", "ready_queue", []Continue95HttpapiUpstreamAction{
			{status: 200, body: `{not json`}, {status: 200}}},
		{"route-changed-attempts", "priority", []Continue95HttpapiUpstreamAction{
			{status: 500, body: `{"error":{"message":"boom"}}`}, {status: 200}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One shared sequence fake: the first candidate call consumes
			// the failing action, the second candidate call succeeds.
			up := &Continue95HttpapiSequenceUpstream{actions: tc.actions}
			srv1 := httptest.NewServer(up)
			srv2 := httptest.NewServer(up)
			t.Cleanup(srv1.Close)
			t.Cleanup(srv2.Close)
			cfg := config.Default()
			cfg.Probe.Enabled = false
			cfg.Routing.MaxAttempts = 2
			cfg.Routing.Strategy = tc.strategy
			cfg.Providers = []config.ProviderConfig{
				{ID: "ga", Type: "openai_compatible", BaseURL: srv1.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
					Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
						Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}}},
				{ID: "gb", Type: "openai_compatible", BaseURL: srv2.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
					Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1,
						Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}}},
			}
			s := testGateway(t, cfg)
			s.SyncCapabilityContracts()
			rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: status=%d want 200 body=%s", tc.name, rr.Code, rr.Body.String())
			}
			if tc.name == "route-changed-attempts" && rr.Header().Get("X-NexaRoute-Attempts") != "ga/m>gb/m" {
				t.Fatalf("attempts header = %q, want ga/m>gb/m", rr.Header().Get("X-NexaRoute-Attempts"))
			}
		})
	}
}

func TestContinue95HttpapiGeminiStreamFailAfterCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"part1\"},\"finish_reason\":null}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: {broken\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "ga", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:streamGenerateContent?alt=sse", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK {
		t.Fatalf("stream-fail status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "part1") {
		t.Fatalf("stream-fail body lacks committed chunk: %s", rr.Body.String())
	}
}

func TestContinue95HttpapiGeminiStreamUsageCallback(t *testing.T) {
	// A streaming Gemini request whose SSE carries a final usage chunk
	// exercises the stream usage callback in the canonical stream pump.
	up := &Continue95HttpapiFakeUpstream{stream: true}
	s := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{up}, 1)
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:streamGenerateContent?alt=sse", Continue95HttpapiGeminiBody(t))
	if rr.Code != http.StatusOK {
		t.Fatalf("stream usage status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"finishReason":"STOP"`) {
		t.Fatalf("stream usage body lacks finish: %s", rr.Body.String())
	}
}

func TestContinue95HttpapiServerDirect(t *testing.T) {
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	// Admin-bucket idle reset: a stale empty bucket refills.
	srv.adminRL.Lock()
	if srv.adminBuckets == nil {
		srv.adminBuckets = map[string]*adminBucket{}
	}
	srv.adminBuckets["10.9.9.9"] = &adminBucket{tokens: 0, last: time.Now().Add(-time.Hour)}
	srv.adminRL.Unlock()
	if !srv.adminAllow("10.9.9.9", 1.0) {
		t.Fatalf("idle admin bucket did not refill")
	}
	// Loopback-IP branch of adminHostAllowed (an IP, not the literal list).
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.2/admin", nil)
	if !adminHostAllowed(req) {
		t.Fatalf("127.0.0.2 not treated as loopback admin host")
	}
	// Decision-provider registration error is logged and skipped.
	badCfg := config.Default()
	badCfg.DecisionProviders = []config.DecisionProviderConfig{{ID: "d1", Type: "jev", PrivacyMode: "bogus"}}
	if s := testGateway(t, badCfg); s == nil {
		t.Fatalf("jev registration error broke gateway construction")
	}
	// Semantic TTL branch: a positive TTL is honoured at construction.
	ttlCfg := config.Default()
	ttlCfg.Cache.Semantic.TTLSeconds = 60
	if s := testGateway(t, ttlCfg); s.semCache == nil {
		t.Fatalf("semantic cache missing with TTL configured")
	}
	// Provider-model diff skip: adding a disabled model is a no-op diff.
	if _, err := srv.mutateConfig(func(cfg *config.Config) error {
		cfg.Providers[0].Models = append(cfg.Providers[0].Models, config.ModelConfig{ID: "m2", Model: "model-2", Enabled: false})
		return nil
	}); err != nil {
		t.Fatalf("disabled-model mutateConfig failed: %v", err)
	}
	// Config-apply Prepare error: an unsupported provider type is rejected.
	if _, err := srv.mutateConfig(func(cfg *config.Config) error {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: "bogus", Type: "bogus-type", Enabled: true})
		return nil
	}); err == nil {
		t.Fatalf("unsupported provider type passed config Prepare")
	}
	// applyRollout canary-at-or-before-index-0: an absent canary id keeps
	// the input order.
	rollCfg := config.Default()
	rollCfg.RouteProfiles = []config.RouteProfileConfig{{ID: "prof", CanaryDeploymentID: "zzz", CanaryPercent: 100}}
	resolved := &route.ResolvedRoute{RouteProfileID: "prof"}
	in := []router.Scored{{Deployment: router.Deployment{ID: "a/m"}}, {Deployment: router.Deployment{ID: "b/m"}}}
	out := applyRollout(rollCfg, resolved, router.Requirement{SessionKey: "k"}, in)
	if len(out) != 2 || out[0].Deployment.ID != "a/m" || out[1].Deployment.ID != "b/m" {
		t.Fatalf("canary-absent rollout changed order: %#v", out)
	}
	// Panic recovery: a nil router table panics inside the handler and is
	// recovered as a 500, not a crash.
	srv.rt = nil
	mReq := httptest.NewRequest(http.MethodGet, "http://gateway/v1beta/models", nil)
	mRr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(mRr, mReq)
	if mRr.Code != http.StatusInternalServerError || !strings.Contains(mRr.Body.String(), "internal gateway error") {
		t.Fatalf("panic recovery status=%d body=%s", mRr.Code, mRr.Body.String())
	}
	// Admin security-store-unavailable via Handler: the middleware 503s
	// before authentication runs.
	srv2 := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	srv2.securityErr = errors.New("continue95 store down")
	aReq := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/client-keys", nil)
	aRr := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(aRr, aReq)
	if aRr.Code != http.StatusServiceUnavailable || !strings.Contains(aRr.Body.String(), "admin security store unavailable") {
		t.Fatalf("securityErr admin status=%d body=%s", aRr.Code, aRr.Body.String())
	}
	// Trusted-proxy parse error: a garbage RemoteAddr leaves the request
	// unchanged.
	proxyCfg := config.Default()
	proxyCfg.TLS.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
	srv3 := testGateway(t, proxyCfg)
	srv3.cfgSnap.Store(nil)
	if snap := srv3.configSnapshot(); len(snap.trustedProxies) == 0 {
		t.Fatalf("trusted proxies did not parse")
	}
	pReq := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
	pReq.RemoteAddr = "garbage"
	if got := srv3.withTrustedProxyContext(pReq); got != pReq || got.RemoteAddr != "garbage" {
		t.Fatalf("garbage RemoteAddr changed the request: %#v", got)
	}
}

func TestContinue95HttpapiEvalPlaneDirect(t *testing.T) {
	plane := newEvaluationPlane(config.EvaluationConfig{}, nil)
	// loadOperatorSuites("") clears state and returns nil.
	if err := plane.loadOperatorSuites(""); err != nil {
		t.Fatalf("empty operator suites path failed: %v", err)
	}
	// loadImport on a directory errors.
	if err := plane.loadImport(t.TempDir()); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory import err=%v, want is-a-directory", err)
	}
	// loadImport over the 8 MiB safe size errors.
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, make([]byte, (8<<20)+1), 0o600); err != nil {
		t.Fatalf("write big import: %v", err)
	}
	if err := plane.loadImport(big); err == nil || !strings.Contains(err.Error(), "exceeds safe size") {
		t.Fatalf("oversize import err=%v, want exceeds-safe-size", err)
	}
	// loadImport of an empty list with MaxScorecards 0 takes the default
	// MaxDeployments branch (execution continues into ImportJSON, which
	// then rejects the empty artifact).
	emptyPlane := newEvaluationPlane(config.EvaluationConfig{MaxScorecards: 0}, nil)
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{"scorecards":[]}`), 0o600); err != nil {
		t.Fatalf("write empty import: %v", err)
	}
	if err := emptyPlane.loadImport(empty); err == nil || !strings.Contains(err.Error(), "no scorecards") {
		t.Fatalf("empty-list import err=%v, want no-scorecards", err)
	}
	// evaluationHealthRows trims to the limit (one row per deployment).
	for i := 0; i < 3; i++ {
		run := eval.Result{RunID: "r" + string(rune('a'+i)), SuiteID: "s", SuiteVersion: "1", DeploymentID: "d" + string(rune('a'+i)) + "/m"}
		if err := plane.Store().Save(run); err != nil {
			t.Fatalf("save run %d: %v", i, err)
		}
	}
	if rows := plane.evaluationHealthRows(2); len(rows) != 2 {
		t.Fatalf("health rows = %d, want 2", len(rows))
	}
	// scorecardRows trims to the limit.
	for i := 0; i < 3; i++ {
		card := scorecards.Scorecard{
			DeploymentID: "d" + string(rune('a'+i)), Version: 1, GeneratedAt: time.Now(),
			Values: map[scorecards.Dimension]scorecards.Value{
				scorecards.DimCoding: {Score: 0.9, Confidence: 0.8, Provenance: scorecards.Provenance("operator_config"), SampleCount: 0, Source: "continue95-config"},
			},
		}
		if _, err := plane.Registry().Upsert(card); err != nil {
			t.Fatalf("upsert card %d: %v", i, err)
		}
	}
	if rows := plane.scorecardRows(2); len(rows) != 2 {
		t.Fatalf("scorecard rows = %d, want 2", len(rows))
	}
	// scorecardDetail for a fresh card returns its row with the current
	// version as the single history entry.
	row, ok := plane.scorecardDetail("da")
	if !ok || row["deployment_id"] != "da" {
		t.Fatalf("scorecardDetail = (%v, %v), want da row", row, ok)
	}
	if hist, _ := row["history"].([]map[string]any); len(hist) != 1 {
		t.Fatalf("fresh card history = %d entries, want 1", len(hist))
	}
	// logf with a nil logger is a no-op, not a panic.
	logf(nil, "continue95 %d", 1)
}

func TestContinue95HttpapiEvalAdminDirect(t *testing.T) {
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	plane := newEvaluationPlane(config.EvaluationConfig{}, nil)
	// Replay RunWithSuite error: the artifact covers another case, so the
	// replay executor has no recorded outcome for c1.
	replayReq := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluation/run", nil)
	replayRr := httptest.NewRecorder()
	srv.adminEvaluationRunReplay(replayRr, replayReq, plane,
		config.EvaluationConfig{MaxArtifacts: 128},
		evaluationRunRequest{SuiteID: "s", Artifacts: []eval.Outcome{{CaseID: "other", Status: eval.OutcomeOK}}},
		deploymentView{ID: "ga/m", ProviderID: "ga", Model: "upstream-model"},
		eval.Suite{ID: "s", Version: "1", Dimension: "coding", MinSamples: 1, Cases: []eval.Case{{ID: "c1"}}})
	if replayRr.Code != http.StatusBadRequest {
		t.Fatalf("replay missing-outcome status=%d body=%s", replayRr.Code, replayRr.Body.String())
	}
	// Live RunWithSuite error: an invalid suite dimension fails validation
	// inside RunWithSuite, after the twin check passes.
	liveReq := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluation/run", nil)
	liveRr := httptest.NewRecorder()
	srv.adminEvaluationRunLive(liveRr, liveReq, plane,
		config.EvaluationConfig{LiveEnabled: true},
		evaluationRunRequest{SuiteID: "s", Prompts: []livePrompt{{CaseID: "c1", Prompt: "hi"}}},
		deploymentView{ID: "ga/m", ProviderID: "ga", Model: "upstream-model", ProviderType: "openai_compatible"},
		eval.Suite{ID: "s", Version: "1", Dimension: "bogus", MinSamples: 1, Cases: []eval.Case{{ID: "c1"}}})
	if liveRr.Code != http.StatusBadRequest {
		t.Fatalf("live invalid-suite status=%d body=%s", liveRr.Code, liveRr.Body.String())
	}
	// Scorecard error: a run whose quality dimension is invalid produces a
	// scorecard_error entry instead of a written card.
	resp := srv.evaluationRunResponse(plane, eval.Result{
		Scoreable: true, SuiteID: "s", SuiteVersion: "1", DeploymentID: "ga/m",
		FinishedAt: time.Now(),
		Quality:    &eval.DimensionScore{Dimension: "bogus", Score: 0.9, Samples: 10},
	})
	if resp["scorecard_error"] == nil || resp["scorecard_error"] == "" {
		t.Fatalf("scorecard error response = %#v, want scorecard_error", resp)
	}
}

func TestContinue95HttpapiHedgeDirect2(t *testing.T) {
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	// abandonHedgeLeg with a real leg cancels it.
	leg := startHedgeLeg(context.Background(), func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	srv.abandonHedgeLeg(leg, "ga/m", "continue95-abandon", "test")
	// doAttemptWithHedge kind/path skip: a partner with a different
	// canonical kind never launches a hedge leg.
	cfg := config.Default()
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 10
	cfg.Routing.MaxAttempts = 2
	primary := hedgeAttemptBundle{
		a:       &Continue95HttpapiFakeAdapter{},
		c:       router.Scored{Deployment: router.Deployment{ID: "ga/m"}},
		payload: []byte(`{}`), canonicalKind: "openai",
	}
	candidates := []router.Scored{
		{Deployment: router.Deployment{ID: "ga/m"}},
		{Deployment: router.Deployment{ID: "gb/m"}},
	}
	build := func(i int) (hedgeAttemptBundle, bool) {
		return hedgeAttemptBundle{canonicalKind: "anthropic"}, true
	}
	out, _, _ := srv.doAttemptWithHedge(context.Background(), "continue95-skip", cfg, candidates, 0, 1, 2, primary, build, false, nil)
	if out.hedgeLaunched {
		t.Fatalf("kind-mismatched partner launched a hedge leg")
	}
	if out.resp == nil || out.resp.StatusCode != http.StatusOK {
		t.Fatalf("skipped hedge response = %#v, want primary 200", out.resp)
	}
}

func TestContinue95HttpapiSemanticHugeBody(t *testing.T) {
	// An embedding response larger than maxUpstreamJSONBytes (32 MiB)
	// fails the bounded read inside embedText.
	srv := Continue95HttpapiSemanticServer(t, strings.Repeat("x", (32<<20)+1), http.StatusOK)
	if _, err := srv.embedText(context.Background(), "hello"); err == nil {
		t.Fatalf("oversize embedding body did not error")
	}
}

func TestContinue95HttpapiOpenAIDirect(t *testing.T) {
	if !openAIStructuredRequested("json_object") {
		t.Fatalf("string structured format not detected")
	}
	if !openAIStructuredRequested(42) {
		t.Fatalf("default structured branch not taken")
	}
	if v, ok := numberInt(5); !ok || v != 5 {
		t.Fatalf("numberInt(5) = (%d, %v), want (5, true)", v, ok)
	}
	// Too-large OpenAI body -> 413.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	big := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(strings.Repeat("x", (17<<20))))
	big.Header.Set("Content-Type", "application/json")
	bigRr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(bigRr, big)
	if bigRr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize OpenAI body status=%d, want 413", bigRr.Code)
	}
	// Upstream 429 with Retry-After surfaces the header on the response.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "ga", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	}}
	s2 := testGateway(t, cfg)
	s2.SyncCapabilityContracts()
	rr := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	rr.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec, rr)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("429 passthrough status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After = %q, want 5", rec.Header().Get("Retry-After"))
	}
	// OpenAI hedge skip: a winning hedge leg marks the next candidate
	// skipped, so the loop's skip-continue branch runs.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"slow"},"finish_reason":"stop"}]}`)
	}))
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"fast"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(fast.Close)
	hCfg := config.Default()
	hCfg.Probe.Enabled = false
	hCfg.Routing.MaxAttempts = 2
	hCfg.Routing.HedgingEnabled = true
	hCfg.Routing.HedgingDelayMS = 10
	hCfg.Providers = []config.ProviderConfig{
		{ID: "ga", Type: "openai_compatible", BaseURL: slow.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
				Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "gb", Type: "openai_compatible", BaseURL: fast.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1,
				Capabilities: config.Capabilities{Streaming: true}}}},
	}
	s3 := testGateway(t, hCfg)
	s3.SyncCapabilityContracts()
	hReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	hReq.Header.Set("Content-Type", "application/json")
	hRec := httptest.NewRecorder()
	s3.Handler().ServeHTTP(hRec, hReq)
	if hRec.Code != http.StatusOK || !strings.Contains(hRec.Body.String(), "fast") {
		t.Fatalf("hedged OpenAI status=%d body=%s, want fast winner", hRec.Code, hRec.Body.String())
	}
}

func TestContinue95HttpapiAnthropicDirect(t *testing.T) {
	// nativeSSETracker usage maxima: message_start input/output/cache-read
	// plus message_delta output/cache-read maxima, fired via the hook on
	// message_stop. (observeAnthropicUsage only runs with a hook set.)
	var captured nativeSSEUsage
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) { captured = u }}
	if err := tr.consume([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":4,\"cache_read_input_tokens\":3}}}\n")); err != nil {
		t.Fatalf("tracker consume start: %v", err)
	}
	if err := tr.consume([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7,\"cache_read_input_tokens\":5}}\n")); err != nil {
		t.Fatalf("tracker consume delta: %v", err)
	}
	if err := tr.consume([]byte("data: {\"type\":\"message_stop\"}\n")); err != nil {
		t.Fatalf("tracker consume stop: %v", err)
	}
	if captured.Input != 10 || captured.Output != 7 || captured.CacheRead != 5 {
		t.Fatalf("tracker usage = %+v, want in 10 out 7 cacheRead 5", captured)
	}
	// finish() with a leftover partial line returns the parse error.
	tr2 := &nativeSSETracker{protocol: "anthropic"}
	tr2.line = []byte("data: {invalid")
	if err := tr2.finish(); err == nil {
		t.Fatalf("finish with partial line did not error")
	}
	// proxyNativeSSE write error propagates.
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"message_start\"}\n\n"))}
	if err := proxyNativeSSE(&Continue95HttpapiErrorWriter{}, resp, "anthropic"); err == nil {
		t.Fatalf("proxyNativeSSE with failing writer did not error")
	}
	// rewriteAnthropicResponseModel on a null envelope errors.
	if _, err := rewriteAnthropicResponseModel([]byte(`null`), "m"); err == nil {
		t.Fatalf("null envelope rewrite did not error")
	}
	// streamOpenAIToAnthropic with a text delta and no message_start: the
	// text emitter's not-started guard runs (no text block is open).
	resp2 := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\ndata: [DONE]\n\n"))}
	rec := httptest.NewRecorder()
	if err := streamOpenAIToAnthropic(rec, resp2, "m", nil); err != nil {
		t.Fatalf("streamOpenAIToAnthropic delta-only failed: %v", err)
	}
}

func TestContinue95HttpapiBudgetWiringDirect(t *testing.T) {
	// requestBudgetParts: nil request, unreadable body, missing max field,
	// huge ContentLength, and ContentLength-driven input estimate.
	if in, out := requestBudgetParts(nil); in != 1 || out != 4096 {
		t.Fatalf("nil request parts = (%d, %d), want (1, 4096)", in, out)
	}
	errReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", &Continue95HttpapiErrorReader{})
	if in, out := requestBudgetParts(errReq); in != 1<<30 || out != 1<<30 {
		t.Fatalf("error-body parts = (%d, %d), want (1<<30, 1<<30)", in, out)
	}
	plainReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"max_tokens":"abc"}`))
	if in, out := requestBudgetParts(plainReq); in != 1<<30 || out != 1<<30 {
		t.Fatalf("non-numeric-max parts = (%d, %d), want (1<<30, 1<<30)", in, out)
	}
	hugeReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"max_tokens":100}`))
	hugeReq.ContentLength = 1<<30 + 1
	if in, out := requestBudgetParts(hugeReq); in != 1<<28 || out != 100 {
		t.Fatalf("huge-CL parts = (%d, %d), want (1<<28, 100)", in, out)
	}
	clReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"max_tokens":100}`))
	clReq.ContentLength = 1000
	if in, out := requestBudgetParts(clReq); in != 250 || out != 100 {
		t.Fatalf("CL parts = (%d, %d), want (250, 100)", in, out)
	}
	// budgetEstimateFor: disabled provider, disabled model, alias match.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	srv.cfg.Providers[0].Enabled = false
	srv.cfgSnap.Store(nil)
	if _, ok := srv.budgetEstimateFor(srv.configSnapshot(), "m", 100, 100); ok {
		t.Fatalf("disabled provider produced an estimate")
	}
	srv.cfg.Providers[0].Enabled = true
	srv.cfg.Providers[0].Models[0].Enabled = false
	srv.cfgSnap.Store(nil)
	if _, ok := srv.budgetEstimateFor(srv.configSnapshot(), "m", 100, 100); ok {
		t.Fatalf("disabled model produced an estimate")
	}
	srv.cfg.Providers[0].Models[0].Enabled = true
	srv.cfg.Providers[0].Models[0].Aliases = []string{"alias-m"}
	srv.cfgSnap.Store(nil)
	if _, ok := srv.budgetEstimateFor(srv.configSnapshot(), "alias-m", 100, 100); !ok {
		t.Fatalf("alias model produced no estimate")
	}
}

func TestContinue95HttpapiClientAuthDirect(t *testing.T) {
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	// Model-override and Gemini wrappers with client auth disabled pass.
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", nil)
	rr := httptest.NewRecorder()
	if !srv.clientAuthAllowedForModel(rr, req, true, "m", true) {
		t.Fatalf("anthropic-style wrapper rejected with auth disabled")
	}
	if !srv.clientAuthAllowedGemini(rr, req, "m", true) {
		t.Fatalf("gemini wrapper rejected with auth disabled")
	}
	// requestTopLevelValues: duplicate keys are all retained, and a
	// truncated object fails closed.
	if vals, ok := requestTopLevelValues([]byte(`{"model":"m","model":"n"}`), "model"); !ok || len(vals) != 2 {
		t.Fatalf("duplicate-key extraction = (%v, %v), want 2 values", vals, ok)
	}
	if vals, ok := requestTopLevelValues([]byte(`{"model":"m"`), "model"); ok || vals != nil {
		t.Fatalf("truncated-object extraction = (%v, %v), want (nil, false)", vals, ok)
	}
	// requestTokenEstimate: a non-numeric max_output_tokens, and a
	// truncated chunked body, both fail closed to 1<<30.
	noMax := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"max_output_tokens":"abc"}`))
	if got := requestTokenEstimate(noMax); got != 1<<30 {
		t.Fatalf("no-max estimate = %d, want 1<<30", got)
	}
	chunked := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(strings.Repeat("x", (2<<20)+1)))
	chunked.ContentLength = -1
	if got := requestTokenEstimate(chunked); got != 1<<30 {
		t.Fatalf("chunked-truncated estimate = %d, want 1<<30", got)
	}
	// clientRPMAllow token-cap: an over-full bucket is capped, then spent.
	now := time.Now()
	srv.clientRL.Lock()
	if srv.clientBuckets == nil {
		srv.clientBuckets = map[string]*clientBucket{}
	}
	srv.clientBuckets[keyDigest("k")] = &clientBucket{tokens: 9999, last: now.Add(-time.Hour), capacity: 10, generation: 1}
	srv.clientRL.Unlock()
	if !srv.clientRPMAllow("k", 60) {
		t.Fatalf("capped RPM bucket refused")
	}
	// clientTPMReserveAt token-cap.
	srv.clientRL.Lock()
	srv.clientBuckets[keyDigest("k")+"#tpm"] = &clientBucket{tokens: 9999, last: now.Add(-time.Hour), capacity: 100, generation: 1}
	srv.clientRL.Unlock()
	if res, ok, _ := srv.clientTPMReserveAt("k", 100, 5, now); !ok || res == nil {
		t.Fatalf("capped TPM reserve = (%v, %v), want ok", res, ok)
	}
	// clientTPMSettleAt refund-cap: a refund past capacity is clamped.
	srv.clientRL.Lock()
	srv.clientBuckets["continue95-tpm"] = &clientBucket{tokens: 95, last: now, capacity: 100, generation: 7}
	srv.clientRL.Unlock()
	srv.clientTPMSettleAt(&tpmReservation{bucketID: "continue95-tpm", capacity: 100, charged: 10, generation: 7}, 0, now)
	srv.clientRL.Lock()
	tokens := srv.clientBuckets["continue95-tpm"].tokens
	srv.clientRL.Unlock()
	if tokens != 100 {
		t.Fatalf("refund-capped tokens = %v, want 100", tokens)
	}
	// Legacy static-key identity: a matching static key yields the legacy
	// admin identity instead of a virtual-key lookup.
	authCfg := config.Default()
	authCfg.ClientAuth.Enabled = true
	authCfg.ClientAuth.Keys = []string{"continue95-static"}
	authSrv := testGateway(t, authCfg)
	authReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	authReq.Header.Set("Authorization", "Bearer continue95-static")
	authReq.Header.Set("Content-Type", "application/json")
	authRr := httptest.NewRecorder()
	if !authSrv.clientAuthAllowedStyled(authRr, authReq, "openai", "", false) {
		t.Fatalf("static-key request rejected: %s", authRr.Body.String())
	}
}

// ---- Wave 4 (issue #663 continuation) ----

func Continue95HttpapiWave4GeminiGateway(t *testing.T, gaHandler, gbHandler http.Handler, strategy string, maxAttempts int) *Server {
	t.Helper()
	srv1 := httptest.NewServer(gaHandler)
	srv2 := httptest.NewServer(gbHandler)
	t.Cleanup(srv1.Close)
	t.Cleanup(srv2.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = maxAttempts
	cfg.Routing.Strategy = strategy
	cfg.Providers = []config.ProviderConfig{
		{ID: "ga", Type: "openai_compatible", BaseURL: srv1.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}}},
		{ID: "gb", Type: "openai_compatible", BaseURL: srv2.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}}},
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func TestContinue95HttpapiGeminiTransportFailover(t *testing.T) {
	// Transport-error (hangup) failover drives the gemini handler's derr
	// policy tree, which HTTP-error tests never reach: ready strategy
	// quarantines + recovers, priority records the failure, and the
	// second candidate serves the request.
	for _, tc := range []struct{ name, strategy string }{
		{"ready", "ready_queue"},
		{"priority", "priority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gaSeq := &Continue95HttpapiSequenceUpstream{actions: []Continue95HttpapiUpstreamAction{{hangup: true}}}
			gbSeq := &Continue95HttpapiSequenceUpstream{}
			s := Continue95HttpapiWave4GeminiGateway(t, gaSeq, gbSeq, tc.strategy, 2)
			rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", Continue95HttpapiGeminiBody(t))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: status=%d want 200 body=%s", tc.name, rr.Code, rr.Body.String())
			}
			if rr.Header().Get("X-NexaRoute-Attempts") != "ga/m>gb/m" {
				t.Fatalf("%s: attempts header = %q, want ga/m>gb/m", tc.name, rr.Header().Get("X-NexaRoute-Attempts"))
			}
		})
	}
}

func TestContinue95HttpapiGeminiRepairRetryHangup(t *testing.T) {
	// Temperature 400 -> repair strips temperature -> retry hangs up
	// (transport error) -> failover to the second candidate, which
	// serves. Temperature (not tools) is the repair vehicle: dropping
	// tools is refused when the request actually carries tools
	// (NeedsTool gate in planRepair), while temperature drops
	// unconditionally when present. MaxRepairAttempts must be > 0: the
	// default 0 skips the repair retry entirely.
	gaSeq := &Continue95HttpapiSequenceUpstream{actions: []Continue95HttpapiUpstreamAction{
		{status: 400, body: `{"error":{"message":"unknown parameter: temperature"}}`},
		{hangup: true},
	}}
	gbSeq := &Continue95HttpapiSequenceUpstream{}
	srv1 := httptest.NewServer(gaSeq)
	srv2 := httptest.NewServer(gbSeq)
	t.Cleanup(srv1.Close)
	t.Cleanup(srv2.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.MaxRepairAttempts = 2
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{
		{ID: "ga", Type: "openai_compatible", BaseURL: srv1.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}}},
		{ID: "gb", Type: "openai_compatible", BaseURL: srv2.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1,
				Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true, Vision: true}}}},
	}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5}}`
	rr := Continue95HttpapiGeminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("repair-retry-hangup status=%d gaCalls=%d gbCalls=%d body=%s", rr.Code, gaSeq.calls, gbSeq.calls, rr.Body.String())
	}
	if gaSeq.calls != 2 || gbSeq.calls != 1 {
		t.Fatalf("repair-retry-hangup calls ga=%d gb=%d, want ga 2 (400 + hangup) and gb 1 (failover 200)", gaSeq.calls, gbSeq.calls)
	}
}

func TestContinue95HttpapiCompatLearnNumeric(t *testing.T) {
	// syncCapabilityContracts learns numeric facts: a model with a
	// context window records it on the contract.
	up := httptest.NewServer(&Continue95HttpapiFakeUpstream{})
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "ga", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, ContextWindow: 200000}}}}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()
	if got := srv.CapabilityStore().Get("ga/m").Capabilities.ContextWindow; got != 200000 {
		t.Fatalf("context window = %d, want 200000", got)
	}
}

func TestContinue95HttpapiCompatRepairNoRepair(t *testing.T) {
	// Unrepairable capability failures restore the original payload and
	// report !Repaired: an unknown parameter has no repair rule, and a
	// temperature rejection cannot be repaired when the payload carries
	// no temperature to strip.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	// The bundle's Model must match the gateway's model: LearnUnsupported
	// drops learning whose invalidation key differs from the contract's,
	// and the key is computed from (base URL, dialect, model, scope).
	bundle := hedgeAttemptBundle{a: &Continue95HttpapiFakeAdapter{}, c: router.Scored{Deployment: router.Deployment{ID: "ga/m", ProviderID: "ga", Model: "upstream-model"}}}
	cases := []struct {
		name    string
		payload string
		errBody string
	}{
		{"frobnicate", `{"model":"m"}`, `{"error":{"message":"unknown parameter: frobnicate"}}`},
		// Same recognized parameter message as the repaired cases, but
		// the payload carries no temperature to strip, so Repair
		// declines and the no-repair path learns + restores.
		{"no-temperature", `{"model":"m"}`, `{"error":{"message":"unknown parameter: temperature"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(tc.errBody))}
			_, restored, outcome := srv.maybeRepairUpstream(context.Background(), "continue95-"+tc.name, bundle, []byte(tc.payload), resp, false, nil, 2, compat.RequirementProfile{})
			if outcome.Repaired {
				t.Fatalf("%s: unexpectedly repaired", tc.name)
			}
			if string(restored) != tc.payload {
				t.Fatalf("%s: restored = %s, want %s", tc.name, restored, tc.payload)
			}
			// The no-temperature case must take the no-repair path
			// (not the early non-capability return): the repair
			// failure learns temperature as unsupported.
			if tc.name == "no-temperature" {
				if got := srv.CapabilityStore().Get("ga/m").Capabilities.Get(compat.CapTemperature); got != compat.Unsupported {
					t.Fatalf("temperature after no-repair = %v, want Unsupported (LearnUnsupported must have run)", got)
				}
			}
		})
	}
}

func TestContinue95HttpapiCompatDoPathRetry(t *testing.T) {
	// A bundle with a non-empty path retries the repaired request via
	// DoPath instead of Do.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	bundle := hedgeAttemptBundle{a: &Continue95HttpapiFakeAdapter{}, c: router.Scored{Deployment: router.Deployment{ID: "ga/m", ProviderID: "ga"}}, path: "/v1/chat/completions"}
	resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unknown parameter: temperature"}}`))}
	out, _, outcome := srv.maybeRepairUpstream(context.Background(), "continue95-dopath", bundle, []byte(`{"model":"m","temperature":0.5}`), resp, false, nil, 2, compat.RequirementProfile{})
	if !outcome.Repaired || out == nil || out.StatusCode != http.StatusOK {
		t.Fatalf("DoPath retry = (repaired=%v, resp=%v), want repaired + 200", outcome.Repaired, out)
	}
}

func TestContinue95HttpapiCompatProviderPaths(t *testing.T) {
	// finishCanonicalAttempt builds provider-family paths: vertex
	// model-in-path with the default location, responses with its
	// default path, azure with the default API version.
	canReq := canonical.Request{Model: "m", Messages: []canonical.Message{{Role: "user", Parts: []canonical.Part{{Type: "text", Text: "hi"}}}}}
	cases := []struct {
		name       string
		provider   config.ProviderConfig
		ingress    string
		deployment router.Deployment
		check      func(path string) bool
	}{
		{"vertex", config.ProviderConfig{ID: "vx", Type: "vertex", VertexProject: "proj", AuthMode: "none", Enabled: true},
			"openai_chat", router.Deployment{ID: "vx/m", ProviderID: "vx", Model: "m"},
			func(p string) bool { return strings.Contains(p, "us-central1") && strings.Contains(p, "proj") }},
		{"responses", config.ProviderConfig{ID: "rp", Type: "openai_responses", AuthMode: "none", Enabled: true},
			"openai_responses", router.Deployment{ID: "rp/m", ProviderID: "rp", Model: "m"},
			func(p string) bool { return p == "/v1/responses" }},
		{"azure", config.ProviderConfig{ID: "az", Type: "azure_openai", AuthMode: "none", Enabled: true},
			"openai_chat", router.Deployment{ID: "az/m", ProviderID: "az", Model: "m"},
			func(p string) bool { return strings.Contains(p, "api-version=2024-10-21") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(&Continue95HttpapiFakeUpstream{})
			t.Cleanup(up.Close)
			p := tc.provider
			p.BaseURL = up.URL
			p.Models = []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}
			cfg := config.Default()
			cfg.Probe.Enabled = false
			cfg.Providers = []config.ProviderConfig{p}
			srv := testGateway(t, cfg)
			_ = srv.configSnapshot()
			out, ok := srv.finishCanonicalAttempt(hedgeAttemptBundle{c: router.Scored{Deployment: tc.deployment}}, canReq, tc.ingress)
			if !ok || !tc.check(out.path) {
				t.Fatalf("%s: finishCanonicalAttempt = (ok=%v, path=%q), want ok + matching path", tc.name, ok, out.path)
			}
		})
	}
}

func TestContinue95HttpapiCompatSanitizeDisabled(t *testing.T) {
	// With temperature seeded Unsupported, an enabled sanitizer strips
	// it; with sanitizing disabled the payload passes through untouched
	// (the disabled early return, not a no-op sanitizer).
	payload := []byte(`{"temperature":0.5}`)
	bundle := hedgeAttemptBundle{c: router.Scored{Deployment: router.Deployment{ID: "ga/m", ProviderID: "ga"}}}
	enabled := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	enabled.CapabilityStore().LearnUnsupported("ga/m", compat.CapTemperature, "continue95", "test", "")
	enabled.cfg.Routing.SanitizeEnabled = true
	enabled.cfgSnap.Store(nil)
	if got := enabled.sanitizeOutgoingPayload(bundle, payload, router.Requirement{}); strings.Contains(string(got), "temperature") {
		t.Fatalf("enabled sanitizer kept temperature: %s", got)
	}
	disabled := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	disabled.CapabilityStore().LearnUnsupported("ga/m", compat.CapTemperature, "continue95", "test", "")
	disabled.cfg.Routing.SanitizeEnabled = false
	disabled.cfgSnap.Store(nil)
	if got := disabled.sanitizeOutgoingPayload(bundle, payload, router.Requirement{}); string(got) != string(payload) {
		t.Fatalf("disabled sanitizer changed payload: %s", got)
	}
}

func TestContinue95HttpapiCacheWiringDirect2(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Cache.Enabled = true
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
	// topP != 1 short-circuits the cache lookup.
	topP := 0.5
	if key, ok := srv.cacheLookupFor(req, "/v1/chat/completions", []byte(`{"model":"m"}`), false, nil, &topP, nil, 0); ok || key != "" {
		t.Fatalf("topP lookup = (%q, %v), want (\"\", false)", key, ok)
	}
	// An oversized body short-circuits the cache lookup.
	big := make([]byte, (1<<20)+1)
	if key, ok := srv.cacheLookupFor(req, "/v1/chat/completions", big, false, nil, nil, nil, 0); ok || key != "" {
		t.Fatalf("oversize lookup = (%q, %v), want (\"\", false)", key, ok)
	}
	// The no-cache request header bypasses serving from cache.
	ncReq := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
	ncReq.Header.Set("x-nexaroute-no-cache", "1")
	if srv.cacheServe(httptest.NewRecorder(), ncReq, "continue95-key", true) {
		t.Fatalf("no-cache request was served from cache")
	}
	// A body read error fails the native-JSON proxy path.
	respErr := &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(&Continue95HttpapiErrorReader{})}
	if _, _, err := srv.proxyOpenAINativeJSON(httptest.NewRecorder(), req, respErr, "ga/m", "k", 0, true, nil); err == nil {
		t.Fatalf("read-error proxy unexpectedly succeeded")
	}
	// Usage extraction: cached tokens, invalid JSON, all-zero usage.
	if in, out, cached, ok := extractOpenAIUsageFull([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`)); !ok || in != 10 || out != 5 || cached != 3 {
		t.Fatalf("openai usage = (%d, %d, %d, %v), want (10, 5, 3, true)", in, out, cached, ok)
	}
	if _, _, _, _, ok := extractAnthropicUsageFull([]byte(`not json`)); ok {
		t.Fatalf("invalid anthropic usage unexpectedly parsed")
	}
	if _, _, _, _, ok := extractAnthropicUsageFull([]byte(`{"usage":{"input_tokens":0,"output_tokens":0}}`)); ok {
		t.Fatalf("all-zero anthropic usage unexpectedly parsed")
	}
}

func TestContinue95HttpapiConfigSnapshotSecondCall(t *testing.T) {
	// The second configSnapshot call without an intervening invalidation
	// takes the CAS-lost branch and returns the cached snapshot.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	if srv.configSnapshot() == nil {
		t.Fatalf("first snapshot nil")
	}
	if srv.configSnapshot() == nil {
		t.Fatalf("second snapshot nil")
	}
}

func TestContinue95HttpapiClientAuthBuckets2(t *testing.T) {
	// Bucket caps seeded with the server's real generation (a stale
	// generation silently replaces the bucket, so hardcoded generations
	// pass vacuously): RPM refill cap, TPM reserve refill cap, TPM
	// settle refill cap, truncated-chunked estimate, and the
	// non-brace token variant.
	srv := Continue95HttpapiGeminiGateway(t, []*Continue95HttpapiFakeUpstream{{}}, 1)
	now := time.Now()
	gen := srv.clientBucketGeneration
	srv.clientRL.Lock()
	if srv.clientBuckets == nil {
		srv.clientBuckets = map[string]*clientBucket{}
	}
	srv.clientBuckets[keyDigest("k")] = &clientBucket{tokens: 9999, last: now.Add(-time.Hour), capacity: 60, generation: gen}
	srv.clientRL.Unlock()
	if !srv.clientRPMAllow("k", 60) {
		t.Fatalf("capped RPM bucket refused")
	}
	srv.clientRL.Lock()
	rpmTokens := srv.clientBuckets[keyDigest("k")].tokens
	srv.clientRL.Unlock()
	if rpmTokens != 59 {
		t.Fatalf("RPM capped tokens = %v, want 59 (cap 60 minus one spend)", rpmTokens)
	}
	srv.clientRL.Lock()
	srv.clientBuckets[keyDigest("k")+"#tpm"] = &clientBucket{tokens: 9999, last: now.Add(-time.Hour), capacity: 100, generation: gen}
	srv.clientRL.Unlock()
	if res, ok, _ := srv.clientTPMReserveAt("k", 100, 5, now); !ok || res == nil {
		t.Fatalf("capped TPM reserve = (%v, %v), want ok", res, ok)
	}
	srv.clientRL.Lock()
	tpmTokens := srv.clientBuckets[keyDigest("k")+"#tpm"].tokens
	srv.clientRL.Unlock()
	if tpmTokens != 95 {
		t.Fatalf("TPM capped tokens = %v, want 95 (cap 100 minus 5)", tpmTokens)
	}
	srv.clientRL.Lock()
	srv.clientBuckets["continue95-tpm2"] = &clientBucket{tokens: 150, last: now.Add(-time.Hour), capacity: 100, generation: gen}
	srv.clientRL.Unlock()
	srv.clientTPMSettleAt(&tpmReservation{bucketID: "continue95-tpm2", capacity: 100, charged: 0, generation: gen}, 0, now)
	srv.clientRL.Lock()
	settled := srv.clientBuckets["continue95-tpm2"].tokens
	srv.clientRL.Unlock()
	if settled != 100 {
		t.Fatalf("settle refill-capped tokens = %v, want 100", settled)
	}
	// Truncated chunked body whose prefix already carries a complete
	// max_output_tokens field: prefix parses, truncation fails closed.
	trunc := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"max_output_tokens":100}`+strings.Repeat("x", 2<<20)))
	trunc.ContentLength = -1
	if got := requestTokenEstimate(trunc); got != 1<<30 {
		t.Fatalf("truncated-chunked estimate = %d, want 1<<30", got)
	}
	// A second JSON value after the first object closes is not a
	// top-level object close: extraction fails closed.
	if vals, ok := requestTopLevelValues([]byte(`{"model":"m"} {"x":1}`), "model"); ok || vals != nil {
		t.Fatalf("non-brace extraction = (%v, %v), want (nil, false)", vals, ok)
	}
}

func TestContinue95HttpapiOpenAIHedgeSkip(t *testing.T) {
	// Hedge launched, both legs fail: the loop continues to the next
	// candidate, which the hedge already marked skipped, so the
	// skip-continue branch runs. The second candidate is therefore hit
	// exactly once (the hedge leg) and never as a loop attempt, and the
	// post-loop shield reports 503.
	var mu sync.Mutex
	slowCalls, fastCalls := 0, 0
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		slowCalls++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"slow boom"}}`)
	}))
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fastCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"fast boom"}}`)
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(fast.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.HedgingEnabled = true
	cfg.Routing.HedgingDelayMS = 10
	cfg.Providers = []config.ProviderConfig{
		{ID: "ga", Type: "openai_compatible", BaseURL: slow.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
				Capabilities: config.Capabilities{Streaming: true}}}},
		{ID: "gb", Type: "openai_compatible", BaseURL: fast.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 1,
				Capabilities: config.Capabilities{Streaming: true}}}},
	}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "all eligible upstream deployments failed") {
		t.Fatalf("hedge-skip status=%d body=%s, want 503 all eligible upstream deployments failed", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if slowCalls != 1 {
		t.Fatalf("first candidate calls = %d, want 1", slowCalls)
	}
	if fastCalls != 1 {
		t.Fatalf("second candidate calls = %d, want 1 (hedge leg only; loop attempt must be skipped)", fastCalls)
	}
}

func TestContinue95HttpapiOpenAIBuildPayloadUnbuilt(t *testing.T) {
	// A candidate that resolves from the route table but whose provider
	// vanished from the config snapshot cannot be built: the handler
	// reports "attempt payload could not be built" instead of dispatching.
	up := httptest.NewServer(&Continue95HttpapiFakeUpstream{})
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 2
	cfg.Providers = []config.ProviderConfig{{ID: "ga", Type: "gemini", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1, Priority: 0,
			Capabilities: config.Capabilities{Streaming: true}}}}}
	srv := testGateway(t, cfg)
	srv.SyncCapabilityContracts()
	srv.cfg.Providers = nil
	srv.cfgSnap.Store(nil)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "attempt payload could not be built") {
		t.Fatalf("unbuilt status=%d body=%s, want 502 attempt payload could not be built", rec.Code, rec.Body.String())
	}
}

func TestContinue95HttpapiOidcValidateError(t *testing.T) {
	// Enabled OIDC with a non-absolute-HTTPS issuer fails Validate
	// before any secret lookup or discovery.
	_, err := newOIDCAuthenticator(context.Background(), config.OIDCConfig{
		Enabled: true, IssuerURL: "ftp://issuer.example.com", ClientID: "cid",
		ClientSecretEnv: "CONTINUE95_OIDC_SECRET", RedirectURL: "https://app.example.com/callback",
		Audience: "aud", RoleClaim: "roles",
	})
	if err == nil || !strings.Contains(err.Error(), "issuer_url") {
		t.Fatalf("validate error = %v, want issuer_url complaint", err)
	}
}

// Continue95HttpapiRoleFailWriter fails exactly on writes carrying a
// role field, so the stream pump's post-loop role emit (and its write
// error check) can be driven without failing the earlier loop emits.
type Continue95HttpapiRoleFailWriter struct{}

func (Continue95HttpapiRoleFailWriter) Header() http.Header { return http.Header{} }

func (Continue95HttpapiRoleFailWriter) WriteHeader(statusCode int) {}

func (Continue95HttpapiRoleFailWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"role"`) {
		return 0, errors.New("continue95 role write boom")
	}
	return len(p), nil
}

func TestContinue95HttpapiAnthropicStreamWriteError(t *testing.T) {
	// The Anthropic-to-OpenAI stream pump surfaces writer failures
	// instead of silently truncating the stream.
	sse := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(sse))}
	good := httptest.NewRecorder()
	if err := streamAnthropicToOpenAI(good, resp, "m", &translate.NameMap{}, "continue95-x"); err != nil {
		t.Fatalf("good-writer stream error: %v", err)
	}
	if !strings.Contains(good.Body.String(), "hi") {
		t.Fatalf("good-writer stream body = %s, want hi", good.Body.String())
	}
	resp2 := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(sse))}
	if err := streamAnthropicToOpenAI(&Continue95HttpapiErrorWriter{}, resp2, "m", &translate.NameMap{}, "continue95-x"); err == nil {
		t.Fatalf("failing-writer stream unexpectedly succeeded")
	}
	// A writer that fails only on role bytes: loop emits succeed, the
	// post-loop role emit fails, and the pump must surface that error.
	resp3 := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(sse))}
	if err := streamAnthropicToOpenAI(&Continue95HttpapiRoleFailWriter{}, resp3, "m", &translate.NameMap{}, "continue95-x"); err == nil {
		t.Fatalf("role-failing-writer stream unexpectedly succeeded")
	}
}

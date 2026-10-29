package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Follow-up regression tests for the merged Gemini ingress (main ba5ff2f).
// All fixture-only: loopback fake upstreams, zero live calls.

// TestGeminiAuthRequestStripsQueryKey pins the query-key stripping rule:
// ?key= must never survive into the downstream request, even when a
// header key wins authentication. The old implementation returned the
// request unchanged whenever an Authorization/x-api-key header was
// present, leaving a query key in r.URL for any downstream URL logging.
func TestGeminiAuthRequestStripsQueryKey(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		headers map[string]string
		wantKey string // expected x-api-key after normalization ("" = unset)
		wantQ   string // expected remaining query string
	}{
		{"query only", "http://gateway/v1beta/models/m:generateContent?key=secret-key&alt=sse", nil, "secret-key", "alt=sse"},
		{"header wins, query stripped", "http://gateway/v1beta/models/m:generateContent?key=secret-key&alt=sse", map[string]string{"Authorization": "Bearer header-key"}, "", "alt=sse"},
		{"x-api-key wins, query stripped", "http://gateway/v1beta/models/m:generateContent?key=secret-key", map[string]string{"x-api-key": "header-key"}, "header-key", ""},
		{"x-goog-api-key, query stripped", "http://gateway/v1beta/models/m:generateContent?key=secret-key", map[string]string{"x-goog-api-key": "goog-key"}, "goog-key", ""},
		{"no key", "http://gateway/v1beta/models/m:generateContent?alt=sse", nil, "", "alt=sse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.url, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			out := geminiAuthRequest(req)
			if strings.Contains(out.URL.String(), "secret-key") {
				t.Fatalf("query key survived in URL: %s", out.URL.String())
			}
			if got := out.URL.RawQuery; got != tc.wantQ {
				t.Fatalf("query = %q want %q", got, tc.wantQ)
			}
			if got := out.Header.Get("x-api-key"); got != tc.wantKey {
				t.Fatalf("x-api-key = %q want %q", got, tc.wantKey)
			}
		})
	}
}

// TestGeminiSystemInstructionForms pins the nil-SystemInstruction fix at
// the handler level: absent, JSON null, and empty-object forms must all
// succeed (the old code dereferenced SystemInstruction unguarded), while
// a non-text system part fails closed before any upstream call.
func TestGeminiSystemInstructionForms(t *testing.T) {
	s, up := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	okBodies := []string{
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"systemInstruction":null,"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"systemInstruction":{},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		`{"systemInstruction":{"parts":[]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
	}
	for i, body := range okBodies {
		rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent", body, "", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("form %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:generateContent",
		`{"systemInstruction":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, "", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("non-text system part: status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env["error"] == nil {
		t.Fatalf("non-text system part: missing Gemini error envelope: %s", rr.Body.String())
	}
	up.mu.Lock()
	calls := up.calls
	up.mu.Unlock()
	if calls != len(okBodies) {
		t.Fatalf("upstream calls=%d want %d (rejected request must not reach upstream)", calls, len(okBodies))
	}
}

// TestGeminiRejectsAdditional covers ingress-level fail-closed cases the
// earlier suites only exercised at the codec level or not at all: empty
// and unknown content parts, a non-TEXT modality, tuned-model paths, and
// the models/-prefixed id that must still route.
func TestGeminiRejectsAdditional(t *testing.T) {
	s, up := geminiTestGateway(t, "openai_chat", "openai_compatible", false)
	rejects := []struct {
		name string
		url  string
		body string
		code int
	}{
		{"empty part", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{}]}]}`, http.StatusBadRequest},
		{"unknown part shape", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"videoMetadata":{"startOffset":"1s"}}]}]}`, http.StatusBadRequest},
		{"image modality", "http://gateway/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseModalities":["IMAGE"]}}`, http.StatusBadRequest},
		{"tuned model path", "http://gateway/v1beta/models/tunedModels/abc:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, http.StatusBadRequest},
		{"deep model path", "http://gateway/v1beta/models/a/b:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, http.StatusBadRequest},
	}
	for _, tc := range rejects {
		rr := geminiPost(t, s, tc.url, tc.body, "", "")
		if rr.Code != tc.code {
			t.Fatalf("%s: status=%d want %d body=%s", tc.name, rr.Code, tc.code, rr.Body.String())
		}
		var env map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env["error"] == nil {
			t.Fatalf("%s: missing Gemini error envelope: %s", tc.name, rr.Body.String())
		}
	}
	up.mu.Lock()
	calls := up.calls
	up.mu.Unlock()
	if calls != 0 {
		t.Fatalf("rejected requests reached upstream %d times", calls)
	}
	rr := geminiPost(t, s, "http://gateway/v1beta/models/models/m:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("models/-prefixed id: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestGeminiStreamFailureTruncates: an upstream stream that ends after
// one text chunk, without a finish chunk, must surface as a truncated
// Gemini stream — the text chunk, no finishReason frame, exactly one
// upstream call, never a fabricated completion or a silent retry.
func TestGeminiStreamFailureTruncates(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		mu.Unlock()
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("expected a streaming upstream request, got %s", body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial \"},\"finish_reason\":null}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Close without a finish chunk and without [DONE]: truncation.
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "g", Name: "Gemini test upstream", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Reasoning: true}}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	rr := geminiPost(t, s, "http://gateway/v1beta/models/m:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, `"text":"partial "`) {
		t.Fatalf("truncated stream lost the committed chunk: %s", out)
	}
	if strings.Contains(out, "finishReason") {
		t.Fatalf("truncated stream fabricated a terminal frame: %s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream calls=%d want exactly 1 (no silent retry after commit)", calls)
	}
}

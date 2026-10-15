package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
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
		case strings.HasSuffix(r.URL.Path, "images/generations"):
			_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`)
		case strings.HasSuffix(r.URL.Path, "audio/speech"):
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("FAKE-AUDIO-BYTES-PRIVACY-BATTERY"))
		case strings.HasSuffix(r.URL.Path, "audio/transcriptions"):
			_, _ = io.WriteString(w, `{"text":"ok"}`)
		case strings.HasSuffix(r.URL.Path, "rerank"):
			_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.9}]}`)
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

// leakBatteryEchoGateway is the deliberately leaky test double: the
// upstream echoes the request's canary content back in its response
// body. The battery must catch it (see
// TestPrivacyBatteryCatchesPlantedLeak).
func leakBatteryEchoGateway(t *testing.T) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Echo a slice of the request body (which carries the canary)
		// inside a JSON envelope — a planted leak.
		_, _ = io.WriteString(w, `{"echo":`+quoteForEcho(b)+`}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	p := config.ProviderConfig{
		ID: "lp", Name: "LP", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "lp-model", Enabled: true, Weight: 1}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func quoteForEcho(b []byte) string {
	raw, err := json.Marshal(string(b))
	if err != nil {
		return `"?"`
	}
	return string(raw)
}

func canaryInSinks(s *Server, rr *httptest.ResponseRecorder) string {
	if strings.Contains(rr.Body.String(), privacyCanary) {
		return "response body"
	}
	for name, values := range rr.Header() {
		for _, v := range values {
			if strings.Contains(v, privacyCanary) {
				return "response header " + name
			}
		}
	}
	for _, ev := range s.bus.Snapshot() {
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), privacyCanary) {
			return "event bus: " + string(raw)
		}
	}
	{
		raw, err := json.Marshal(s.usage.Snapshot(nil))
		if err == nil && strings.Contains(string(raw), privacyCanary) {
			return "usage records: " + string(raw)
		}
	}
	// Health state carries deployment ids and error CLASSES, never
	// request content.
	for _, st := range s.hm.Snapshot() {
		raw, _ := json.Marshal(st)
		if strings.Contains(string(raw), privacyCanary) {
			return "health state: " + string(raw)
		}
	}
	return ""
}

func assertNoCanaryInSinks(t *testing.T, s *Server, rr *httptest.ResponseRecorder) {
	t.Helper()
	if sink := canaryInSinks(s, rr); sink != "" {
		t.Fatalf("observability sink leaks prompt content via %s", sink)
	}
}

func privacyMultipartBody(t *testing.T, canary string) (body []byte, contentType string) {
	t.Helper()
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("model", "m")
	fw, err := mw.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("audio-bytes-" + canary + "-tail"))
	_ = mw.Close()
	return form.Bytes(), mw.FormDataContentType()
}

func TestPrivacyPromptContentNeverReachesSinks(t *testing.T) {
	paths := []struct {
		name, path, body string
	}{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"` + privacyCanary + `"}]}`},
		{"messages", "/v1/messages", `{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"` + privacyCanary + `"}]}`},
		{"responses", "/v1/responses", `{"model":"m","input":"` + privacyCanary + `"}`},
		{"embeddings", "/v1/embeddings", `{"model":"m","input":"` + privacyCanary + `"}`},
		{"images", "/v1/images/generations", `{"model":"m","prompt":"` + privacyCanary + `"}`},
		{"speech", "/v1/audio/speech", `{"model":"m","input":"` + privacyCanary + `","voice":"alloy"}`},
		{"rerank", "/v1/rerank", `{"model":"m","query":"` + privacyCanary + `","documents":["a","b"],"top_n":1}`},
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
			if tc.name == "speech" {
				if got := rr.Body.String(); got != "FAKE-AUDIO-BYTES-PRIVACY-BATTERY" {
					t.Fatalf("speech bytes mangled: %q", got)
				}
				if ct := rr.Header().Get("Content-Type"); ct != "audio/mpeg" {
					t.Fatalf("speech content type=%q", ct)
				}
			}
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
	for _, fail := range []bool{false, true} {
		name := "transcriptions_success"
		if fail {
			name = "transcriptions_upstream_error"
		}
		t.Run(name, func(t *testing.T) {
			s := leakBatteryGateway(t, fail)
			body, ct := privacyMultipartBody(t, privacyCanary)
			req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/audio/transcriptions", bytes.NewReader(body))
			req.Header.Set("Content-Type", ct)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if !fail && rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			if fail && rr.Code == http.StatusOK {
				t.Fatalf("expected an error status, got 200: %s", rr.Body.String())
			}
			assertNoCanaryInSinks(t, s, rr)
		})
	}
	t.Run("media_no_credential_forwarding", func(t *testing.T) {
		var sawAuth string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawAuth = r.Header.Get("Authorization")
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
		}))
		t.Cleanup(up.Close)
		cfg := config.Default()
		cfg.Probe.Enabled = false
		p := config.ProviderConfig{
			ID: "lp", Name: "LP", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "lp-model", Enabled: true, Weight: 1}},
		}
		p.ApplyDefaults()
		cfg.Providers = []config.ProviderConfig{p}
		s := testGateway(t, cfg)
		s.SyncCapabilityContracts()
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/embeddings",
			strings.NewReader(`{"model":"m","input":"`+privacyCanary+`"}`))
		req.Header.Set("Authorization", "Bearer client-secret-canary")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if sawAuth != "" {
			t.Fatalf("client credential forwarded upstream: %q", sawAuth)
		}
		assertNoCanaryInSinks(t, s, rr)
	})
}

func TestPrivacyBatteryCatchesPlantedLeak(t *testing.T) {
	mediaCases := []struct {
		name, path, body, contentType string
		multipart                     bool
	}{
		{"embeddings", "/v1/embeddings", `{"model":"m","input":"` + privacyCanary + `"}`, "application/json", false},
		{"images", "/v1/images/generations", `{"model":"m","prompt":"` + privacyCanary + `"}`, "application/json", false},
		{"speech", "/v1/audio/speech", `{"model":"m","input":"` + privacyCanary + `","voice":"alloy"}`, "application/json", false},
		{"rerank", "/v1/rerank", `{"model":"m","query":"` + privacyCanary + `","documents":["a"]}`, "application/json", false},
		{"transcriptions", "/v1/audio/transcriptions", "", "", true},
	}
	for _, tc := range mediaCases {
		t.Run(tc.name, func(t *testing.T) {
			s := leakBatteryEchoGateway(t)
			var req *http.Request
			if tc.multipart {
				body, ct := privacyMultipartBody(t, privacyCanary)
				req = httptest.NewRequest(http.MethodPost, "http://gateway"+tc.path, bytes.NewReader(body))
				req.Header.Set("Content-Type", ct)
			} else {
				req = httptest.NewRequest(http.MethodPost, "http://gateway"+tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", tc.contentType)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if sink := canaryInSinks(s, rr); sink == "" {
				t.Fatalf("planted leak NOT caught for %s: detector stayed silent (status=%d body=%s)", tc.name, rr.Code, rr.Body.String())
			}
		})
	}
}

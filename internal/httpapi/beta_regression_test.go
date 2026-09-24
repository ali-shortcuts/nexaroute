package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestResponsesAdmissionLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.MaxInflightRequests = 1
	s := testGateway(t, cfg)
	s.inflight.Store(1)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`)))
	if rr.Code != 503 || rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("capacity bypassed: %d %s", rr.Code, rr.Body.String())
	}
}

func TestResponsesGeminiDispatch(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-test:generateContent" {
			t.Errorf("wrong Gemini path: %s", r.URL)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Type: "gemini", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "gemini-test", Enabled: true}}}}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`)))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "hello") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error { b.closed = true; return nil }

func TestCanonicalStreamClosesAndKeepsTerminalAfterUsage(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n")}
	s := &Server{}
	rr := httptest.NewRecorder()
	err := s.canonicalStreamPump(rr, &http.Response{Body: body}, "openai_chat", "openai_responses", "m", "r")
	if err != nil {
		t.Errorf("terminal lost after usage: %v", err)
	}
	if !body.closed {
		t.Error("stream body not closed")
	}
}

func TestCanonicalStreamFailureHasNoSuccessTail(t *testing.T) {
	rr := httptest.NewRecorder()
	s := &Server{}
	err := s.canonicalStreamPump(rr, &http.Response{Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))}, "openai_chat", "openai_responses", "m", "r")
	if err == nil {
		t.Fatal("truncation accepted")
	}
	if strings.Contains(rr.Body.String(), "response.completed") {
		t.Fatal("failed stream also claimed completion")
	}
}

func TestCanonicalUsageIsRecordedOnce(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"))
	calls, prompt, completion := 0, 0, 0
	s := &Server{}
	err := s.canonicalStreamPump(httptest.NewRecorder(), &http.Response{Body: body}, "openai_chat", "openai_responses", "m", "r", func(p, c int) { calls++; prompt = p; completion = c })
	if err != nil || calls != 1 || prompt != 3 || completion != 4 {
		t.Fatalf("err=%v calls=%d usage=%d/%d", err, calls, prompt, completion)
	}
}

func TestCanonicalMalformedStreamCanFailOverBeforeCommit(t *testing.T) {
	rr := httptest.NewRecorder()
	s := &Server{}
	err := s.canonicalStreamPump(rr, &http.Response{Body: io.NopCloser(strings.NewReader("data: invalid-json\n\n"))}, "openai_chat", "openai_responses", "m", "r")
	if err == nil || rr.Body.Len() != 0 || rr.Flushed {
		t.Fatalf("malformed first frame committed: err=%v body=%s", err, rr.Body.String())
	}
}

func TestCapabilityIdentityChangesWithOperatorSettings(t *testing.T) {
	p := config.ProviderConfig{ID: "p", Type: "openai_compatible", BaseURL: "https://example.test", Models: []config.ModelConfig{{ID: "m", Model: "up"}}}
	before := credentialScope(p)
	p.Models[0].Capabilities.Tools = true
	if credentialScope(p) == before {
		t.Fatal("operator capability edit retained stale identity")
	}
	before = credentialScope(p)
	p.ChatPath = "/custom/chat"
	if credentialScope(p) == before {
		t.Fatal("endpoint edit retained stale identity")
	}
}

func TestResponsesNativeCustomPathAndRepair(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/custom/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if calls == 1 {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"temperature is not supported","param":"temperature"}}`)
			return
		}
		io.WriteString(w, `{"id":"resp_ok","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Type: "openai_responses", BaseURL: up.URL, ResponsesPath: "/custom/responses", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "up", Enabled: true}}}}
	s := testGateway(t, cfg)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","temperature":0.7}`)))
	if rr.Code != 200 || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, calls, rr.Body.String())
	}
}

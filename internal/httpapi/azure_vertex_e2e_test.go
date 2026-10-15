package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// End-to-end coverage for the M5-1a cloud adapters: a client request
// crosses the canonical translation layer and lands on the fake
// cloud endpoint with the exact URL and auth the real service
// expects; the response is translated back to the client's protocol.

type cloudCapture struct {
	mu                    sync.Mutex
	path, query           string
	apiKey, authorization string
	body                  string
}

func (c *cloudCapture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.query = r.URL.RawQuery
	c.apiKey = r.Header.Get("api-key")
	c.authorization = r.Header.Get("Authorization")
	c.body = string(b)
}

type cloudSnapshot struct {
	path, query, apiKey, authorization, body string
}

func (c *cloudCapture) snapshot() cloudSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloudSnapshot{c.path, c.query, c.apiKey, c.authorization, c.body}
}

func cloudGateway(t *testing.T, p config.ProviderConfig) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func cloudChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAzureOpenAIEndToEnd(t *testing.T) {
	cap := &cloudCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"azure-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer up.Close()
	s := cloudGateway(t, config.ProviderConfig{
		ID: "az", Name: "Azure", Type: "azure_openai", BaseURL: up.URL,
		APIKey: "azure-secret", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "dep-gpt4o", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	})
	rr := cloudChat(t, s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "azure-ok") {
		t.Fatalf("azure e2e status=%d body=%s", rr.Code, rr.Body.String())
	}
	got := cap.snapshot()
	if got.path != "/openai/deployments/dep-gpt4o/chat/completions" || got.query != "api-version=2024-10-21" {
		t.Fatalf("azure upstream address: path=%q query=%q", got.path, got.query)
	}
	if got.apiKey != "azure-secret" || got.authorization != "" {
		t.Fatalf("azure upstream auth: api-key=%q authorization=%q", got.apiKey, got.authorization)
	}
}

func TestVertexEndToEndAndStreaming(t *testing.T) {
	cap := &cloudCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"vertex-\"}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"stream\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"vertex-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}`)
	}))
	defer up.Close()
	s := cloudGateway(t, config.ProviderConfig{
		ID: "vx", Name: "Vertex", Type: "vertex", BaseURL: up.URL,
		APIKey: "oauth-token", VertexProject: "proj-1", VertexLocation: "europe-west4",
		Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-2.0-flash", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true}}},
	})

	rr := cloudChat(t, s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "vertex-ok") {
		t.Fatalf("vertex e2e status=%d body=%s", rr.Code, rr.Body.String())
	}
	got := cap.snapshot()
	wantPath := "/v1/projects/proj-1/locations/europe-west4/publishers/google/models/gemini-2.0-flash:generateContent"
	if got.path != wantPath {
		t.Fatalf("vertex upstream path=%q, want %q", got.path, wantPath)
	}
	if got.authorization != "Bearer oauth-token" {
		t.Fatalf("vertex upstream authorization=%q", got.authorization)
	}
	if !strings.Contains(got.body, `"contents"`) {
		t.Fatalf("vertex upstream body is not Gemini-shaped: %s", got.body)
	}

	rr = cloudChat(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "vertex-") || !strings.Contains(rr.Body.String(), "stream") {
		t.Fatalf("vertex stream status=%d body=%s", rr.Code, rr.Body.String())
	}
	got = cap.snapshot()
	if !strings.HasSuffix(got.path, ":streamGenerateContent") || got.query != "alt=sse" {
		t.Fatalf("vertex stream address: path=%q query=%q", got.path, got.query)
	}
}

func cloudMessages(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

const cloudMessagesBody = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

// Anthropic-protocol clients must reach both cloud families too:
// Vertex through the canonical Gemini path, Azure through the
// Anthropic->OpenAI translation with adapter-side deployment
// addressing.
func TestCloudAdaptersFromAnthropicClient(t *testing.T) {
	vCap := &cloudCapture{}
	vUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vCap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"vertex-anthropic-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}`)
	}))
	defer vUp.Close()
	vs := cloudGateway(t, config.ProviderConfig{
		ID: "vx", Name: "Vertex", Type: "vertex", BaseURL: vUp.URL,
		APIKey: "oauth-token", VertexProject: "proj-1", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "gemini-2.0-flash", Enabled: true, Weight: 1}},
	})
	rr := cloudMessages(t, vs, cloudMessagesBody)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "vertex-anthropic-ok") {
		t.Fatalf("anthropic->vertex status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := vCap.snapshot(); !strings.Contains(got.path, "/projects/proj-1/") || !strings.Contains(got.body, `"contents"`) {
		t.Fatalf("anthropic->vertex upstream: path=%q body=%s", got.path, got.body)
	}

	aCap := &cloudCapture{}
	aUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCap.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"azure-anthropic-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer aUp.Close()
	as := cloudGateway(t, config.ProviderConfig{
		ID: "az", Name: "Azure", Type: "azure_openai", BaseURL: aUp.URL,
		APIKey: "azure-secret", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "dep-gpt4o", Enabled: true, Weight: 1}},
	})
	rr = cloudMessages(t, as, cloudMessagesBody)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "azure-anthropic-ok") {
		t.Fatalf("anthropic->azure status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := aCap.snapshot(); got.path != "/openai/deployments/dep-gpt4o/chat/completions" || got.apiKey != "azure-secret" {
		t.Fatalf("anthropic->azure upstream: path=%q api-key=%q", got.path, got.apiKey)
	}
}

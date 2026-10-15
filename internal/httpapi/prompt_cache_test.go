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

// M5-5 prompt-cache e2e: markers/keys at real upstreams.

type promptCacheCapture struct {
	mu   sync.Mutex
	body []byte
	path string
}

func (c *promptCacheCapture) handler(respBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.body, c.path = b, r.URL.Path
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}
}

func (c *promptCacheCapture) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.body)
}

func promptCacheGateway(t *testing.T, providerType string, cap *promptCacheCapture, respBody string) *Server {
	t.Helper()
	up := httptest.NewServer(cap.handler(respBody))
	t.Cleanup(up.Close)
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "pc", Name: "PC", Type: providerType, BaseURL: up.URL, APIKey: "k", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "pc-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func TestPromptCacheNativeAnthropicPassthrough(t *testing.T) {
	cap := &promptCacheCapture{}
	s := promptCacheGateway(t, "anthropic_compatible", cap,
		`{"id":"m1","type":"message","role":"assistant","model":"pc-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":42}}`)
	body := `{"model":"m","max_tokens":64,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	upstream := cap.last()
	if strings.Count(upstream, `"cache_control"`) != 2 {
		t.Fatalf("native passthrough lost cache markers: %s", upstream)
	}
}

func TestPromptCacheKeyResponsesToOpenAIUpstream(t *testing.T) {
	cap := &promptCacheCapture{}
	s := promptCacheGateway(t, "openai_compatible", cap,
		`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":9}}}`)
	body := `{"model":"m","prompt_cache_key":"tenant-42-prefix","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(cap.last(), `"prompt_cache_key":"tenant-42-prefix"`) {
		t.Fatalf("prompt_cache_key not mapped to openai upstream: %s", cap.last())
	}
	// The cached-token usage flows back to the responses client.
	if !strings.Contains(rr.Body.String(), `"cached_tokens":9`) {
		t.Fatalf("cached_tokens not mapped back to responses client: %s", rr.Body.String())
	}
}

func TestPromptCacheMarkersDropToGeminiUpstream(t *testing.T) {
	cap := &promptCacheCapture{}
	s := promptCacheGateway(t, "gemini", cap,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"cachedContentTokenCount":21}}`)
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(cap.last(), "cache_control") {
		t.Fatalf("anthropic marker leaked into gemini dialect: %s", cap.last())
	}
	// Gemini's implicit-cache usage still maps back to the client.
	if !strings.Contains(rr.Body.String(), `"cache_read_input_tokens":21`) {
		t.Fatalf("gemini cached tokens not mapped to anthropic client: %s", rr.Body.String())
	}
}

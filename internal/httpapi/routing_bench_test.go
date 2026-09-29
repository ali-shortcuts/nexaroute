package httpapi

// Gateway routing hot-path benchmark vs a direct upstream call.
//
// What is measured:
//   - BenchmarkRoutingHotPath: the pure in-process routing decision
//     (candidate filtering + scoring + ordering for one chat request).
//     No network I/O.
//   - BenchmarkDirectUpstreamCall: a direct HTTP POST of the same chat
//     payload to the same loopback upstream stub, with no gateway in
//     the middle. This is the baseline the gateway overhead is paid
//     on top of.
//   - BenchmarkGatewayChatCompletionsE2E: the full gateway data plane
//     for one non-streaming chat request (admission, routing decision,
//     upstream round-trip, response validation/rewrite) against the
//     same loopback stub.
//
// Method: loopback httptest stub returning a canned OpenAI chat
// completion; two openai_compatible providers x three models each.
// Numbers are recorded in docs/GATEWAY_BENCHMARK.md.

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

const benchChatPayload = `{"model":"bench-model","messages":[{"role":"user","content":"hi"}]}`

const benchUpstreamBody = `{"id":"c","object":"chat.completion","created":1,"model":"bench-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func benchUpstreamStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, benchUpstreamBody)
	}))
}

// newBenchServer mirrors testGateway without requiring *testing.T.
func newBenchServer(b *testing.B, cfg config.Config) *Server {
	b.Helper()
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		b.Fatal(err)
	}
	rt := router.New(cfg, hm)
	if router.IsReadyStrategy(cfg.Routing.Strategy) {
		for _, d := range rt.All() {
			hm.RecordSuccess(d.ID, time.Millisecond)
		}
	}
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	return New(cfg, b.TempDir()+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
}

func BenchmarkRoutingHotPath(b *testing.B) {
	up := benchUpstreamStub()
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	mkModels := func(prefix string) []config.ModelConfig {
		return []config.ModelConfig{
			{ID: prefix + "-1", Model: "bench-upstream", Aliases: []string{"bench-model"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
			{ID: prefix + "-2", Model: "bench-upstream-2", Enabled: true, Weight: 1},
			{ID: prefix + "-3", Model: "bench-upstream-3", Enabled: true, Weight: 1},
		}
	}
	cfg.Providers = []config.ProviderConfig{
		{ID: "bench-a", Name: "A", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: mkModels("a")},
		{ID: "bench-b", Name: "B", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: mkModels("b")},
	}
	s := newBenchServer(b, cfg)
	req := router.Requirement{Model: "bench-model", Tools: true, Streaming: false}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, candidates, _, err := s.candidatesForRequirement(req, "openai")
		if err != nil {
			b.Fatalf("routing hot path: %v", err)
		}
		if len(candidates) == 0 {
			b.Fatal("routing hot path returned no candidates")
		}
	}
}

func BenchmarkDirectUpstreamCall(b *testing.B) {
	up := benchUpstreamStub()
	defer up.Close()
	client := up.Client()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Post(up.URL+"/v1/chat/completions", "application/json", strings.NewReader(benchChatPayload))
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("upstream status=%d", resp.StatusCode)
		}
	}
}

func BenchmarkGatewayChatCompletionsE2E(b *testing.B) {
	up := benchUpstreamStub()
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{ID: "bench-a", Name: "A", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
			{ID: "a-1", Model: "bench-upstream", Aliases: []string{"bench-model"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
		}},
	}
	s := newBenchServer(b, cfg)
	handler := s.Handler()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(benchChatPayload))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("gateway status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}

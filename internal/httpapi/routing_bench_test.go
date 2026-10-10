package httpapi

// Reproducible routing benchmark for issue #81.
//
// Compares the real gateway routing hot path
// (POST /v1/chat/completions -> router -> local deterministic upstream)
// against a direct upstream POST to the same deterministic server.
//
// Local-only: httptest servers on loopback, AuthMode none, no secrets,
// no external provider calls.

import (
	"bytes"
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

const routingBenchUpstreamBody = `{"id":"bench","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`

const routingBenchRequestBody = `{"model":"bench-model","messages":[{"role":"user","content":"hi"}]}`

func newRoutingBenchUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain request so keep-alive connections stay reusable and the
		// benchmark measures routing, not a body leak.
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, routingBenchUpstreamBody)
	}))
}

func newRoutingBenchGateway(tb testing.TB, upstreamURL string) *Server {
	tb.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{
			ID: "bench", Name: "Bench", Type: "openai_compatible",
			BaseURL: upstreamURL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{
				{ID: "m", Model: "upstream-model", Aliases: []string{"bench-model"}, Enabled: true, Weight: 1},
			},
		},
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	rt := router.New(cfg, hm)
	if router.IsReadyStrategy(cfg.Routing.Strategy) {
		for _, d := range rt.All() {
			hm.RecordSuccess(d.ID, time.Millisecond)
		}
	}
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	srv := New(cfg, tb.TempDir()+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
	tb.Cleanup(func() { _ = srv.CloseSecurityStore() })
	return srv
}

// BenchmarkRouting_Gateway exercises the real routing hot path through the
// gateway handler with a local deterministic upstream.
func BenchmarkRouting_Gateway(b *testing.B) {
	up := newRoutingBenchUpstream()
	defer up.Close()
	s := newRoutingBenchGateway(b, up.URL)
	handler := s.Handler()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(routingBenchRequestBody))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("gateway status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}

// BenchmarkRouting_DirectUpstream is the baseline: a direct POST to the same
// deterministic upstream server, bypassing gateway routing.
func BenchmarkRouting_DirectUpstream(b *testing.B) {
	up := newRoutingBenchUpstream()
	defer up.Close()
	body := []byte(routingBenchRequestBody)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Post(up.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
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

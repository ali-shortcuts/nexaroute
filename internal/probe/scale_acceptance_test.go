package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestProbeSchedulerAt50_100_200Deployments(t *testing.T) {
	for _, count := range []int{50, 100, 200} {
		t.Run(fmt.Sprintf("deployments_%d", count), func(t *testing.T) {
			var active, peak, calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("invalid probe JSON: %v", err)
				}
				if body["max_tokens"] != float64(1) {
					t.Errorf("probe max_tokens=%v; expected configured one-token budget", body["max_tokens"])
				}
				encoded, _ := json.Marshal(body)
				if string(encoded) == "" || containsString(string(encoded), "USER_PROMPT_CANARY") {
					t.Errorf("probe contains user prompt material")
				}
				callNum := calls.Add(1)
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				time.Sleep(8 * time.Millisecond)
				active.Add(-1)
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("probe path=%s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if callNum%19 == 0 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"mock unavailable"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"probe","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer up.Close()

			cfg := config.Default()
			cfg.Routing.Strategy = "ready_queue"
			cfg.Probe.Enabled = true
			cfg.Probe.OnStart = false
			cfg.Probe.Concurrency = 32
			cfg.Probe.MaxTokens = 1
			cfg.Probe.CapabilityProbes = false
			cfg.Probe.TimeoutMS = 3000
			p := config.ProviderConfig{ID: "mock", Name: "Mock", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true}
			for i := 0; i < count; i++ {
				p.Models = append(p.Models, config.ModelConfig{ID: fmt.Sprintf("m%03d", i), Model: fmt.Sprintf("model-%03d", i), Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}})
			}
			cfg.Providers = []config.ProviderConfig{p}
			cfg.ApplyDefaults()
			hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
			reg, err := providers.NewRegistry(cfg)
			if err != nil {
				t.Fatal(err)
			}
			rt := router.New(cfg, hm)
			e := New(cfg, reg, rt, hm, events.New(count+10))
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			start := time.Now()
			result := e.runOnce(ctx, true)
			elapsed := time.Since(start)
			if result.Total != count || calls.Load() != int32(count) {
				t.Fatalf("result=%+v calls=%d want total/calls %d", result, calls.Load(), count)
			}
			if active.Load() != 0 || e.Stats().ActiveProbes != 0 {
				t.Fatalf("probe work still active: active=%d stats=%+v", active.Load(), e.Stats())
			}
			if peak.Load() < 2 || peak.Load() > 32 {
				t.Fatalf("observed peak concurrency=%d want [2,32]", peak.Load())
			}
			if elapsed > 5*time.Second {
				t.Fatalf("%d probes took %s; expected bounded parallel completion", count, elapsed)
			}
			available := 0
			for _, d := range rt.All() {
				if hm.Get(d.ID).Status == health.Healthy {
					available++
				}
			}
			if available < count-count/18 {
				t.Fatalf("only %d/%d deployments became available; result=%+v", available, count, result)
			}
		})
	}
}

func containsString(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

func benchmarkProbeScheduler(b *testing.B, count int) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"probe","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 32
	cfg.Probe.MaxTokens = 1
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.TimeoutMS = 3000
	p := config.ProviderConfig{ID: "mock", Name: "Mock", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true}
	for i := 0; i < count; i++ {
		p.Models = append(p.Models, config.ModelConfig{ID: fmt.Sprintf("m%03d", i), Model: fmt.Sprintf("model-%03d", i), Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}})
	}
	cfg.Providers = []config.ProviderConfig{p}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		b.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(count+10))
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.runOnce(ctx, true)
	}
}

func BenchmarkProbeScheduler_50(b *testing.B) {
	benchmarkProbeScheduler(b, 50)
}

func BenchmarkProbeScheduler_100(b *testing.B) {
	benchmarkProbeScheduler(b, 100)
}

func BenchmarkProbeScheduler_200(b *testing.B) {
	benchmarkProbeScheduler(b, 200)
}

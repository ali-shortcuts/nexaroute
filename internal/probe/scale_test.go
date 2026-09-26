package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type scaleProbeStats struct {
	calls     atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32
	badBudget atomic.Int32
	badPrompt atomic.Int32
}

func probeScaleGateway(t testing.TB, n, concurrency, failEvery int) (*Engine, *health.Manager, *router.Router, *scaleProbeStats) {
	t.Helper()
	stats := &scaleProbeStats{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var obj map[string]any
		if json.Unmarshal(body, &obj) != nil {
			stats.badPrompt.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mt, _ := obj["max_tokens"].(float64)
		if mt < 1 || mt > 20 {
			stats.badBudget.Add(1)
		}
		if !bytes.Contains(body, []byte(providers.HealthProbeContent)) {
			stats.badPrompt.Add(1)
		}
		nact := stats.active.Add(1)
		for {
			m := stats.maxActive.Load()
			if nact <= m || stats.maxActive.CompareAndSwap(m, nact) {
				break
			}
		}
		defer stats.active.Add(-1)
		idx := stats.calls.Add(1)
		if failEvery > 0 && int(idx)%failEvery == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = concurrency
	cfg.Probe.TimeoutMS = 2000
	cfg.Probe.MaxTokens = 8
	cfg.Probe.CapabilityProbes = false
	cfg.Routing.Strategy = "ready_queue"
	for i := 0; i < n; i++ {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: fmt.Sprintf("p%03d", i), Name: "P", Type: "openai_compatible",
			BaseURL: up.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 32,
			Models: []config.ModelConfig{{ID: "m", Model: fmt.Sprintf("model-%d", i), Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
		})
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	return New(cfg, reg, rt, hm, events.New(500)), hm, rt, stats
}

func runProbeScale(t *testing.T, n int) {
	t.Helper()
	concurrency := 16
	e, hm, rt, stats := probeScaleGateway(t, n, concurrency, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := e.RunOnce(ctx)
	if res.Total != n || res.Passed != n || res.Failed != 0 {
		t.Fatalf("n=%d result=%+v", n, res)
	}
	if int(stats.calls.Load()) != n {
		t.Fatalf("calls=%d want %d", stats.calls.Load(), n)
	}
	if stats.badBudget.Load() != 0 || stats.badPrompt.Load() != 0 {
		t.Fatalf("probe contract violated budget=%d prompt=%d", stats.badBudget.Load(), stats.badPrompt.Load())
	}
	if got := stats.maxActive.Load(); got < 1 || got > int32(concurrency) {
		t.Fatalf("max active=%d want 1..%d", got, concurrency)
	}
	got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true})
	if len(got) != n {
		t.Fatalf("eligible=%d want %d", len(got), n)
	}
	healthy := 0
	for _, d := range rt.All() {
		if hm.Get(d.ID).Status == health.Healthy {
			healthy++
		}
	}
	if healthy != n {
		t.Fatalf("healthy=%d want %d", healthy, n)
	}
	if st := e.Stats(); st.ActiveProbes != 0 {
		t.Fatalf("active probes leaked: %+v", st)
	}
	if st := e.Stats(); st.RecoveryTracked != 0 {
		t.Fatalf("recovery tasks leaked on all-healthy sweep: %+v", st)
	}
}

func TestProbeScale50(t *testing.T)  { runProbeScale(t, 50) }
func TestProbeScale100(t *testing.T) { runProbeScale(t, 100) }
func TestProbeScale200(t *testing.T) { runProbeScale(t, 200) }

func TestProbeScaleFailedDoNotBlockHealthy(t *testing.T) {
	e, hm, rt, stats := probeScaleGateway(t, 60, 8, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := e.RunOnce(ctx)
	if res.Passed == 0 || res.Failed == 0 {
		t.Fatalf("want mixed result: %+v", res)
	}
	if stats.maxActive.Load() > 8 {
		t.Fatalf("concurrency leaked: %d", stats.maxActive.Load())
	}
	ready := rt.Candidates(router.Requirement{Model: "auto", Streaming: true})
	if len(ready) != res.Passed {
		t.Fatalf("ready=%d passed=%d", len(ready), res.Passed)
	}
	for _, c := range ready {
		if hm.Get(c.Deployment.ID).Status != health.Healthy {
			t.Fatalf("%s not healthy", c.Deployment.ID)
		}
	}
}

func benchProbeScale(b *testing.B, n int) {
	e, _, _, _ := probeScaleGateway(b, n, 16, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		res := e.RunOnce(ctx)
		cancel()
		if res.Passed != n {
			b.Fatalf("passed=%d want %d", res.Passed, n)
		}
	}
}

func BenchmarkProbeScale50(b *testing.B)  { benchProbeScale(b, 50) }
func BenchmarkProbeScale100(b *testing.B) { benchProbeScale(b, 100) }
func BenchmarkProbeScale200(b *testing.B) { benchProbeScale(b, 200) }

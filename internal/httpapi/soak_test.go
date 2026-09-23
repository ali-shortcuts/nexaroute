package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func requireHTTPSoak(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_SOAK") != "1" {
		t.Skip("set NEXAROUTE_SOAK=1 to run long-form soak checks")
	}
}

func TestSoakConcurrentTrafficAndHotReload(t *testing.T) {
	requireHTTPSoak(t)

	var upstreamCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		time.Sleep(time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 64
	cfg.Routing.MaxAttempts = 1
	pc := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 64,
	}
	for i := 0; i < 10; i++ {
		pc.Models = append(pc.Models, config.ModelConfig{
			ID: fmt.Sprintf("m%d", i), Model: fmt.Sprintf("model-%d", i),
			Aliases: []string{"coding"}, Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true},
		})
	}
	cfg.Providers = []config.ProviderConfig{pc}
	s := testGateway(t, cfg)
	h := s.Handler()

	const workers = 24
	const perWorker = 250
	var failed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
					strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hello"}]}`))
				req.Header.Set("x-session-id", fmt.Sprintf("soak-%d-%d", g, i%50))
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					failed.Add(1)
					return
				}
			}
		}(g)
	}
	close(start)

	for i := 0; i < 100; i++ {
		next := s.currentConfig()
		next.Providers[0].Name = fmt.Sprintf("P-%d", i)
		next.Routing.CapacityWeight = float64(i % 50)
		if err := s.applyConfig(next); err != nil {
			t.Fatalf("hot reload %d failed: %v", i, err)
		}
	}
	wg.Wait()

	if failed.Load() != 0 {
		t.Fatalf("traffic failures=%d", failed.Load())
	}
	if want := int64(workers * perWorker); upstreamCalls.Load() != want {
		t.Fatalf("upstream calls=%d want %d", upstreamCalls.Load(), want)
	}
	if s.inflight.Load() != 0 {
		t.Fatalf("global inflight leaked: %d", s.inflight.Load())
	}
	stats := s.reg.Stats()
	if len(stats) != 1 || stats[0].ActiveRequests != 0 || stats[0].WaitingRequests != 0 {
		t.Fatalf("provider capacity leaked after soak: %+v", stats)
	}
	if got := s.rt.SessionCount(); got > 10000 {
		t.Fatalf("session affinity state escaped bound: %d", got)
	}
}

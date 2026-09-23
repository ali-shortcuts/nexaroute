package httpapi

import (
	"context"
	"encoding/json"
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

func requireHTTPStress(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
}

func TestStressGlobalAdmissionKeepsControlPlaneResponsive(t *testing.T) {
	requireHTTPStress(t)

	release := make(chan struct{})
	var active atomic.Int64
	var peak atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 8
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 32,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()

	const requests = 64
	statuses := make(chan int, requests)
	var wg sync.WaitGroup
	start := make(chan struct{})
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, gw.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				statuses <- 0
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	close(start)

	deadline := time.Now().Add(2 * time.Second)
	for peak.Load() < 8 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := peak.Load(); got > 8 {
		close(release)
		wg.Wait()
		t.Fatalf("upstream peak=%d exceeds global admission=8", got)
	}
	if got := active.Load(); got == 0 {
		close(release)
		wg.Wait()
		t.Fatal("stress did not reach upstream")
	}

	healthClient := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := healthClient.Get(gw.URL + "/healthz")
	if err != nil {
		close(release)
		wg.Wait()
		t.Fatalf("healthz became unresponsive under overload: %v", err)
	}
	var health map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&health)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		close(release)
		wg.Wait()
		t.Fatalf("healthz status=%d under overload", resp.StatusCode)
	}

	close(release)
	wg.Wait()
	close(statuses)

	ok, overloaded, other := 0, 0, 0
	for code := range statuses {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			overloaded++
		default:
			other++
		}
	}
	if ok == 0 || overloaded == 0 || other != 0 {
		t.Fatalf("statuses ok=%d overloaded=%d other=%d", ok, overloaded, other)
	}
	if got := s.inflight.Load(); got != 0 {
		t.Fatalf("inflight leaked after requests completed: %d", got)
	}
	if s.overloadRejects.Load() == 0 {
		t.Fatal("overload counter did not record rejected requests")
	}
}

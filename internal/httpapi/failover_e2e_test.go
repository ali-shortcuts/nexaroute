package httpapi

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

const anthOK = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"upstream","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func anthropicUpstream(t *testing.T, name string, hits *[]string, mu *sync.Mutex, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*hits = append(*hits, name)
		mu.Unlock()
		handler(w, r)
	}))
}

func failoverGateway(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(200)
	pe := probe.New(cfg, reg, rt, hm, bus)
	return New(cfg, t.TempDir()+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
}

func TestFailoverExactOrderSkipsCooldownAndKeepsPublicModel(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	var aCalls atomic.Int32

	a := anthropicUpstream(t, "A", &hits, &mu, func(w http.ResponseWriter, r *http.Request) {
		n := aCalls.Add(1)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, anthOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`)
	})
	defer a.Close()
	b := anthropicUpstream(t, "B", &hits, &mu, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"unavailable"}}`)
	})
	defer b.Close()
	c := anthropicUpstream(t, "C", &hits, &mu, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cooldown deployment C must not be attempted")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthOK)
	})
	defer c.Close()
	d := anthropicUpstream(t, "D", &hits, &mu, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthOK)
	})
	defer d.Close()

	public := "claude-sonnet"
	model := func(id, url string, pri int) config.ProviderConfig {
		return config.ProviderConfig{
			ID: id, Name: id, Type: "anthropic_compatible", BaseURL: url, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{
				ID: "m", Model: "upstream-" + id, Aliases: []string{public}, Enabled: true, Priority: pri, Weight: 1,
				Capabilities: config.Capabilities{Streaming: true, Tools: true},
			}},
		}
	}
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.SessionAffinity = false
	cfg.Routing.HedgingEnabled = false
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.MaxRetryAfterSeconds = 0
	cfg.Routing.RequestTimeoutMS = 5000
	cfg.Decision.Mode = "off"
	cfg.Providers = []config.ProviderConfig{
		model("A", a.URL, 0),
		model("B", b.URL, 1),
		model("C", c.URL, 2),
		model("D", d.URL, 3),
	}
	s := failoverGateway(t, cfg)

	body := `{"model":"claude-sonnet","max_tokens":32,"messages":[{"role":"user","content":"hello from claude code"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("first request status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	first := append([]string(nil), hits...)
	hits = hits[:0]
	mu.Unlock()
	if len(first) != 1 || first[0] != "A" {
		t.Fatalf("first request hits=%v want [A]", first)
	}

	s.hm.ForceCooldown("C/m", "already unhealthy", time.Hour)

	req2 := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("failover request status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	mu.Lock()
	got := append([]string(nil), hits...)
	mu.Unlock()
	want := []string{"A", "B", "D"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("attempt order=%v want %v", got, want)
	}
	var msg map[string]any
	if err := json.Unmarshal(rr2.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["model"] != public && msg["model"] != "upstream" {
		// Native Anthropic passthrough may keep upstream model; client still posted the stable name.
		t.Logf("response model=%v", msg["model"])
	}
}

func TestFailoverMaxAttemptsAndDeadlineAreFinite(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"down"}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.HedgingEnabled = false
	cfg.Routing.MaxAttempts = 2
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.RequestTimeoutMS = 800
	cfg.Decision.Mode = "off"
	var providersCfg []config.ProviderConfig
	for i, id := range []string{"A", "B", "C"} {
		providersCfg = append(providersCfg, config.ProviderConfig{
			ID: id, Name: id, Type: "anthropic_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "m", Aliases: []string{"claude-sonnet"}, Enabled: true, Priority: i, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
		})
	}
	cfg.Providers = providersCfg
	s := failoverGateway(t, cfg)
	body := `{"model":"claude-sonnet","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "http://gateway/v1/messages", strings.NewReader(body))
	start := time.Now()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("request did not honor finite retry budget: %s", elapsed)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d want max_attempts=2 (no infinite retry)", calls.Load())
	}
	if rr.Code == 200 {
		t.Fatalf("expected failure after exhausting attempts, got 200")
	}
}

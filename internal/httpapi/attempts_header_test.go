package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// #417 acceptance 2: the reply itself must show the failover,
// in order, matching the backend trace for the same request
// ID. The data plane therefore emits X-NexaRoute-Attempts —
// deployment IDs in attempt order — fed by the same
// emitRouteAttempt choke point that records the #410
// lifecycle, so the header and the trace cannot disagree.
func TestAttemptsHeaderMatchesFailoverOrder(t *testing.T) {
	var failA atomic.Bool
	failA.Store(true)
	mk := func(name string, fail *atomic.Bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail != nil && fail.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"quota exhausted"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_%s","type":"message","role":"assistant","content":[{"type":"text","text":"from %s"}],"model":"physical-%s","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`, name, name, name)
		}))
	}
	srvA := mk("A", &failA)
	defer srvA.Close()
	srvB := mk("B", nil)
	defer srvB.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.CooldownSeconds = 1800
	for i, tc := range []struct {
		prov, url string
	}{{"pA", srvA.URL}, {"pB", srvB.URL}} {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: tc.prov, Name: tc.prov, Type: "anthropic_compatible", BaseURL: tc.url,
			ForwardHeaders: []string{"x-request-id"},
			AuthMode:       "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "physical-" + tc.prov, Enabled: true, Priority: i, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
		})
	}
	trueVal := true
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"pA/m", "pB/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "claude", PublicModel: "nexa-stable", RouteProfile: "profile", Enabled: &trueVal, Protocols: []string{"anthropic"}}}
	s := testGateway(t, cfg)

	do := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(`{"model":"nexa-stable","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-request-id", id)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	// Failover: A is rate-limited pre-commit, B answers. The
	// header lists both, in attempt order.
	rr := do("req-failover")
	if rr.Code != http.StatusOK {
		t.Fatalf("failover request status = %d body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-NexaRoute-Attempts"); got != "pA/m>pB/m" {
		t.Fatalf("X-NexaRoute-Attempts = %q, want %q", got, "pA/m>pB/m")
	}
	if got := rr.Header().Get("X-Gateway-Deployment"); got != "pB/m" {
		t.Fatalf("winner header = %q, want pB/m", got)
	}

	// The ledger entry is cleared when the request ends: a
	// later request reusing the same ID starts clean. (pA is in
	// cooldown from the 429, so this request is served by pB
	// alone; a stale ledger would repeat the pA>pB trail.)
	failA.Store(false)
	rr = do("req-failover")
	if rr.Code != http.StatusOK {
		t.Fatalf("second request status = %d", rr.Code)
	}
	if got := rr.Header().Get("X-NexaRoute-Attempts"); got != "pB/m" {
		t.Fatalf("single-attempt header = %q, want %q (no stale trail)", got, "pB/m")
	}
}

// The WebChat badge renders the trail from the header — pinned
// here so the client half of the contract cannot drift.
func TestWebChatFailoverBadgeWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"'x-nexaroute-attempts'",
		"attempts: (th['x-nexaroute-attempts'] || '').split('>').filter(Boolean)",
		"b.attempts.length > 1",
		"attempts ' + b.attempts.map(esc).join(' → ')",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing failover-badge wiring %q", want)
		}
	}
}

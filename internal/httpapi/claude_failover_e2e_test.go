package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type claudeAttempt struct{ request, deployment string }

func TestClaudeStableAnthropicRouteFailoverABCD(t *testing.T) {
	var mu sync.Mutex
	var attempts []claudeAttempt
	var failA, failB atomic.Bool
	servers := map[string]*httptest.Server{}
	for _, name := range []string{"A", "B", "C", "D"} {
		name := name
		servers[name] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("x-request-id")
			mu.Lock()
			attempts = append(attempts, claudeAttempt{request: requestID, deployment: name})
			mu.Unlock()
			if (name == "A" && failA.Load()) || (name == "B" && failB.Load()) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"quota exhausted"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_%s","type":"message","role":"assistant","content":[{"type":"text","text":"from %s"}],"model":"physical-%s","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":3}}`, name, name, name)
		}))
	}
	defer func() {
		for _, srv := range servers {
			srv.Close()
		}
	}()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 4
	cfg.Routing.RetryBackoffMS = 0
	cfg.Routing.CooldownSeconds = 1800
	var deployments []string
	for i, name := range []string{"A", "B", "C", "D"} {
		id := "p" + name + "/m"
		deployments = append(deployments, id)
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "p" + name, Name: name, Type: "anthropic_compatible", BaseURL: servers[name].URL,
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "physical-" + name, Enabled: true, Priority: i, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
		})
	}
	trueVal := true
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: deployments}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "claude", PublicModel: "nexa-stable", RouteProfile: "profile", Enabled: &trueVal, Protocols: []string{"anthropic"}}}
	s := testGateway(t, cfg)
	s.hm.ForceCooldown("pC/m", "pre-existing unhealthy deployment", time.Hour)

	doRequest := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"model":"nexa-stable","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-request-id", id)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %s status=%d body=%s", id, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("X-Gateway-Public-Model") != "nexa-stable" {
			t.Fatalf("public model changed: headers=%v", rr.Header())
		}
		var response map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response["type"] != "message" {
			t.Fatalf("invalid Anthropic response: %s err=%v", rr.Body.String(), err)
		}
		return rr
	}

	// A is healthy at first.
	if rr := doRequest("claude-1"); !strings.Contains(rr.Body.String(), "from A") {
		t.Fatalf("first request did not use A: %s", rr.Body.String())
	}
	// A begins rejecting quota; the normal next client request fails over to B.
	failA.Store(true)
	if rr := doRequest("claude-2"); !strings.Contains(rr.Body.String(), "from B") {
		t.Fatalf("second request did not fail over to B: %s", rr.Body.String())
	}
	// B now fails; A and B are unhealthy, C is pre-cooldown, so D succeeds.
	failB.Store(true)
	if rr := doRequest("claude-3"); !strings.Contains(rr.Body.String(), "from D") {
		t.Fatalf("third request did not skip C and reach D: %s", rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, a := range attempts {
		got = append(got, a.request+":"+a.deployment)
	}
	want := []string{"claude-1:A", "claude-2:A", "claude-2:B", "claude-3:B", "claude-3:D"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("attempt order=%v want=%v", got, want)
	}
	if len(got) != 5 {
		t.Fatalf("attempt count exceeded bounded request budgets: %v", got)
	}
}

package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Phase 21 (#421) acceptance slice: a scripted coding session
// against fake providers. The session stays pinned while healthy,
// shifts to the long-context deployment when the conversation
// outgrows the pinned deployment's declared window (before the
// request can fail), and fails over pre-commit when the pinned
// upstream dies — the client never sees an error for the shift or
// the failover, and every turn's serving deployment is read from
// the gateway's own response header.

func sessionUpstream(t *testing.T, down *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream exploded","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-sess","object":"chat.completion","model":"sess-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
}

func sessionProvider(id, model, baseURL string, priority, weight, window int) config.ProviderConfig {
	return config.ProviderConfig{
		ID: id, Name: id, Type: "openai_compatible", BaseURL: baseURL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: model, Enabled: true, Priority: priority, Weight: float64(weight),
			ContextWindow: window,
			Capabilities:  config.Capabilities{Streaming: true},
		}},
	}
}

func sessionBody(t *testing.T, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model": "sess-model", "session_id": "sess-421", "max_tokens": 128,
		"messages": []any{map[string]any{"role": "user", "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func sessionTurn(t *testing.T, srv *Server, reqID, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-request-id", reqID)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr.Code, rr.Header().Get("X-Gateway-Deployment")
}

func TestCodingSessionPinShiftFailover(t *testing.T) {
	var smallDown, bigDown atomic.Bool
	smallUp := sessionUpstream(t, &smallDown)
	defer smallUp.Close()
	bigUp := sessionUpstream(t, &bigDown)
	defer bigUp.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_mesh"
	cfg.Routing.MaxAttempts = 3
	cfg.Routing.HedgingEnabled = false
	cfg.Providers = []config.ProviderConfig{
		sessionProvider("smallprov", "small-model", smallUp.URL, 5, 10, 8000),
		sessionProvider("bigprov", "big-model", bigUp.URL, 5, 1, 200000),
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "sess-pool", Deployments: []string{"small-model", "big-model"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp-sess", CandidatePool: "sess-pool"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve-sess", PublicModel: "sess-model", RouteProfile: "rp-sess"}}
	srv := testGateway(t, cfg)

	// Turns 1-3: small prompts. The session pins to the priority
	// winner (weight) and stays there: the pin is authoritative
	// within the shared priority tier.
	for i, id := range []string{"sess-t1", "sess-t2", "sess-t3"} {
		code, dep := sessionTurn(t, srv, id, sessionBody(t, "turn "+id))
		if code != 200 || dep != "smallprov/m" {
			t.Fatalf("turn %d = (%d, %q), want (200, smallprov/m)", i+1, code, dep)
		}
	}

	// Turn 4: the conversation outgrows the pinned deployment's
	// declared window (~10k estimated tokens > 8000). The session
	// must shift to the long-context deployment BEFORE failing —
	// the client sees a normal 200 from bigprov.
	huge := strings.Repeat("context ", 4500)
	code, dep := sessionTurn(t, srv, "sess-t4", sessionBody(t, huge))
	if code != 200 || dep != "bigprov/m" {
		t.Fatalf("context-growth turn = (%d, %q), want (200, bigprov/m)", code, dep)
	}

	// Turns 5-6: small again; the session is stable on its new pin.
	for _, id := range []string{"sess-t5", "sess-t6"} {
		code, dep = sessionTurn(t, srv, id, sessionBody(t, "small again"))
		if code != 200 || dep != "bigprov/m" {
			t.Fatalf("post-shift turn %s = (%d, %q), want (200, bigprov/m)", id, code, dep)
		}
	}

	// Turn 7: the pinned upstream dies. Pre-commit failover must
	// serve the turn from the other deployment; the client never
	// sees the failure.
	bigDown.Store(true)
	code, dep = sessionTurn(t, srv, "sess-t7", sessionBody(t, "are you still there"))
	if code != 200 || dep != "smallprov/m" {
		t.Fatalf("failover turn = (%d, %q), want (200, smallprov/m)", code, dep)
	}
}

// The decision trace for the same requirement must say WHY the
// small deployment cannot serve the grown context: the stable
// context_window_too_small exclusion reason, not silence.
func TestCodingSessionContextExclusionReason(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{
		sessionProvider("smallprov", "small-model", "http://127.0.0.1:9", 5, 10, 8000),
		sessionProvider("bigprov", "big-model", "http://127.0.0.1:9", 5, 1, 200000),
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	rt := router.New(cfg, hm)
	exp := rt.Explain(router.Requirement{Model: "sess-model", SessionKey: "sess-421", MinContextWindow: 12000})
	var small, big *router.CandidateDecision
	for i := range exp.Decisions {
		switch exp.Decisions[i].DeploymentID {
		case "smallprov/m":
			small = &exp.Decisions[i]
		case "bigprov/m":
			big = &exp.Decisions[i]
		}
	}
	if small == nil || big == nil {
		t.Fatalf("decisions = %+v", exp.Decisions)
	}
	if small.Included || small.ExcludedReason != router.ExcludeContextWindow {
		t.Fatalf("small decision = %+v, want excluded with %q", *small, router.ExcludeContextWindow)
	}
	if !big.Included {
		t.Fatalf("big decision = %+v, want included", *big)
	}
}

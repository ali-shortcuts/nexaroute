package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// M5-3 canary rollout: deterministic bucketing by session key over
// a route profile's candidate pool.

func rolloutGatewayCfg(canary string, percent int, privacy string) config.Config {
	cfg := config.Default()
	cfg.Routing.Strategy = "priority" // deterministic candidate set for order assertions
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{ID: "pa", Name: "A", Type: "openai_compatible", BaseURL: "https://a.example.com", Enabled: true, APIKey: "k",
			DataHandling: config.DataHandlingConfig{TrainsOnData: "no"},
			Models:       []config.ModelConfig{{ID: "m", Model: "model-a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}}},
		{ID: "pb", Name: "B", Type: "openai_compatible", BaseURL: "https://b.example.com", Enabled: true, APIKey: "k",
			DataHandling: config.DataHandlingConfig{TrainsOnData: "yes"},
			Models:       []config.ModelConfig{{ID: "m", Model: "model-b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"pa/m", "pb/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{
		ID: "r1", CandidatePool: "pool", Privacy: privacy,
		CanaryDeploymentID: canary, CanaryPercent: percent,
	}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve", PublicModel: "public", RouteProfile: "r1"}}
	return cfg
}

func firstCandidate(t *testing.T, s *Server, req router.Requirement) (string, []router.Scored) {
	t.Helper()
	_, cands, resolved, err := s.candidatesForRequirement(req, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || len(cands) == 0 {
		t.Fatalf("no candidates (resolved=%v)", resolved)
	}
	return cands[0].Deployment.ID, cands
}

func TestRolloutPromotionAndStickiness(t *testing.T) {
	s := testGateway(t, rolloutGatewayCfg("pb/m", 100, ""))
	req := router.Requirement{Model: "public", SessionKey: "sess-1"}
	first, _ := firstCandidate(t, s, req)
	if first != "pb/m" {
		t.Fatalf("percent=100 must promote the canary, first=%s", first)
	}
	// Same key -> same arm, every time.
	for i := 0; i < 5; i++ {
		again, _ := firstCandidate(t, s, req)
		if again != first {
			t.Fatalf("rollout flapped: %s then %s", first, again)
		}
	}
}

func TestRolloutZeroPercentAndNoKeyLeaveOrderAlone(t *testing.T) {
	s0 := testGateway(t, rolloutGatewayCfg("pb/m", 0, ""))
	base, _ := firstCandidate(t, s0, router.Requirement{Model: "public", SessionKey: "sess-1"})

	s100 := testGateway(t, rolloutGatewayCfg("pb/m", 100, ""))
	noKey, _ := firstCandidate(t, s100, router.Requirement{Model: "public"})
	if noKey != base {
		t.Fatalf("keyless requests must stay on the stable arm: base=%s nokey=%s", base, noKey)
	}
	zeroKey, _ := firstCandidate(t, s0, router.Requirement{Model: "public", SessionKey: "sess-1"})
	if zeroKey != base {
		t.Fatalf("percent=0 changed the order: %s vs %s", zeroKey, base)
	}
}

func TestRolloutDistributionIsApproximatelyThePercent(t *testing.T) {
	s := testGateway(t, rolloutGatewayCfg("pb/m", 50, ""))
	promoted := 0
	const n = 200
	for i := 0; i < n; i++ {
		first, _ := firstCandidate(t, s, router.Requirement{Model: "public", SessionKey: fmt.Sprintf("sess-%d", i)})
		if first == "pb/m" {
			promoted++
		}
	}
	if promoted < 60 || promoted > 140 {
		t.Fatalf("50%% rollout promoted %d/%d sessions (want ~100)", promoted, n)
	}
}

func TestRolloutNeverPromotesAnIneligibleCanary(t *testing.T) {
	s := testGateway(t, rolloutGatewayCfg("pb/m", 100, ""))
	// pb/m has no Tools capability; a tools request must not see it
	// promoted (it is not even a candidate).
	first, cands := firstCandidate(t, s, router.Requirement{Model: "public", SessionKey: "sess-1", Tools: true})
	if first == "pb/m" {
		t.Fatalf("ineligible canary promoted: %v", cands)
	}
	for _, c := range cands {
		if c.Deployment.ID == "pb/m" {
			t.Fatalf("tools-incapable canary in candidates: %v", cands)
		}
	}
}

func TestRolloutRespectsPrivacyGate(t *testing.T) {
	// no_training profile: pb trains on data, so it is excluded by
	// the privacy gate before rollout ever runs — percent=100 must
	// not resurrect it.
	s := testGateway(t, rolloutGatewayCfg("pb/m", 100, "no_training"))
	_, cands := firstCandidate(t, s, router.Requirement{Model: "public", SessionKey: "sess-1", RequireNoTraining: true})
	for _, c := range cands {
		if c.Deployment.ID == "pb/m" {
			t.Fatalf("privacy-ineligible canary in candidates: %v", cands)
		}
	}
	if len(cands) != 1 || cands[0].Deployment.ID != "pa/m" {
		t.Fatalf("expected only pa/m, got %v", cands)
	}
}

func TestRolloutEndToEndSessionHeader(t *testing.T) {
	var hits []string
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits = append(hits, name)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}))
	}
	upA := mk("a")
	defer upA.Close()
	upB := mk("b")
	defer upB.Close()
	cfg := rolloutGatewayCfg("pb/m", 100, "")
	cfg.Providers[0].BaseURL = upA.URL
	cfg.Providers[1].BaseURL = upB.URL
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-session-id", "sess-e2e")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(hits) == 0 || hits[0] != "b" {
		t.Fatalf("canary arm did not serve first: hits=%v", hits)
	}
	if rr.Header().Get("X-Gateway-Deployment") != "pb/m" {
		t.Fatalf("deployment header=%q", rr.Header().Get("X-Gateway-Deployment"))
	}
}

package httpapi

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Privacy fail-closed evidence for the case the P2 matrix does not
// cover: NO deployment in the pool satisfies no_training (M3-4D).
// Generation ingresses must fail with privacy_unavailable and contact
// no upstream at all. count_tokens has a documented local-estimate
// fallback that never sends content upstream, so its fail-closed
// property is: zero upstream requests and an estimated answer.

func newAllTrainingGateway(t *testing.T, routePrivacy, vkRequirePrivacy, vkPlain string) *privacyLeakFixture {
	t.Helper()
	var hitsA, hitsB atomic.Int64
	upA := privacyMockUpstream(&hitsA, &atomic.Bool{})
	upB := privacyMockUpstream(&hitsB, &atomic.Bool{})
	t.Cleanup(upA.Close)
	t.Cleanup(upB.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Routing.Strategy = "priority"
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "pool-all", Mode: "explicit", Deployments: []string{"pa/m", "pb/m"}},
	}
	cfg.RouteProfiles = []config.RouteProfileConfig{
		{ID: "rp1", CandidatePool: "pool-all", Privacy: routePrivacy},
	}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{
		{ID: "ve1", PublicModel: "nexa-private", RouteProfile: "rp1", Enabled: &trueVal},
	}
	caps := config.Capabilities{Streaming: true, Tools: true}
	mk := func(id, model, url string) config.ProviderConfig {
		return config.ProviderConfig{
			ID: id, Name: id, Type: "anthropic_compatible", BaseURL: url, AuthMode: "none", Enabled: true,
			DataHandling:    config.DataHandlingConfig{TrainsOnData: "yes"},
			CountTokensPath: "/v1/messages/count_tokens",
			Models:          []config.ModelConfig{{ID: "m", Model: model, Enabled: true, Weight: 1, Capabilities: caps}},
		}
	}
	cfg.Providers = []config.ProviderConfig{mk("pa", "phys-a", upA.URL), mk("pb", "phys-b", upB.URL)}
	if vkPlain != "" {
		cfg.ClientAuth.Enabled = true
		cfg.ClientAuth.VirtualKeys = []config.VirtualKeyConfig{
			{ID: "vk-private", KeyHash: keyDigest(vkPlain), RequirePrivacy: vkRequirePrivacy, AllowedModels: []string{"nexa-private"}},
		}
	}
	s := testGateway(t, cfg)
	return &privacyLeakFixture{srv: s, hitsA: &hitsA, hitsB: &hitsB, failB: &atomic.Bool{}, upA: upA, upB: upB}
}

func TestPrivacyFailClosedWhenNoDeploymentSatisfies(t *testing.T) {
	sources := []struct {
		name             string
		routePrivacy     string
		vkRequirePrivacy string
		vkPlain          string
	}{
		{name: "route_profile", routePrivacy: "no_training"},
		{name: "virtual_key", vkRequirePrivacy: "no_training", vkPlain: "nrk_failclosed_test_key"},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			f := newAllTrainingGateway(t, source.routePrivacy, source.vkRequirePrivacy, source.vkPlain)

			rr := f.doRequest(t, "/v1/chat/completions", `{"model":"nexa-private","messages":[{"role":"user","content":"secret prompt"}]}`, "", source.vkPlain)
			assertPrivacyUnavailable(t, rr)

			rr = f.doRequest(t, "/v1/messages", `{"model":"nexa-private","max_tokens":16,"messages":[{"role":"user","content":"secret prompt"}]}`, "", source.vkPlain)
			assertPrivacyUnavailable(t, rr)

			rr = f.doRequest(t, "/v1/responses", `{"model":"nexa-private","input":"secret prompt"}`, "", source.vkPlain)
			assertPrivacyUnavailable(t, rr)

			// count_tokens: local estimate, no upstream contact.
			rr = f.doRequest(t, "/v1/messages/count_tokens", `{"model":"nexa-private","messages":[{"role":"user","content":"secret prompt"}]}`, "", source.vkPlain)
			if rr.Code != http.StatusOK {
				t.Fatalf("count_tokens status=%d body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"estimated":true`) {
				t.Fatalf("count_tokens should answer with the local estimate, got %s", rr.Body.String())
			}

			if got := f.hitsA.Load(); got != 0 {
				t.Fatalf("training provider A received %d requests, want 0", got)
			}
			if got := f.hitsB.Load(); got != 0 {
				t.Fatalf("training provider B received %d requests, want 0", got)
			}
		})
	}
}

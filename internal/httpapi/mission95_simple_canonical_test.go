package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func boolPtrMission95(v bool) *bool { return &v }

func TestMission95NormalizeSimpleRoute(t *testing.T) {
	// Defaults: mode automatic, name falls back to public model, enabled true.
	in := simpleRouteForm{ID: "  r1 ", PublicModel: "gpt-x", Deployments: []string{" d1 ", "d1", "", "d2"}}
	if err := normalizeSimpleRoute(&in); err != nil {
		t.Fatal(err)
	}
	if in.ID != "r1" || in.Mode != "automatic" || in.Name != "gpt-x" || in.Enabled == nil || !*in.Enabled {
		t.Fatalf("normalized = %+v", in)
	}
	if len(in.Deployments) != 2 || in.Deployments[0] != "d1" || in.Deployments[1] != "d2" {
		t.Errorf("deployments deduped = %v", in.Deployments)
	}
	// Explicit mode is lowercased, not defaulted away.
	in2 := simpleRouteForm{ID: "r2", PublicModel: "m", Mode: " ORDERED ", Deployments: []string{"d1"}, Enabled: boolPtrMission95(false)}
	if err := normalizeSimpleRoute(&in2); err != nil || in2.Mode != "ordered" || *in2.Enabled != false {
		t.Fatalf("ordered = %+v, %v", in2, err)
	}
	for _, tc := range []struct {
		name string
		in   simpleRouteForm
	}{
		{"missing id", simpleRouteForm{PublicModel: "m", Deployments: []string{"d1"}}},
		{"missing model", simpleRouteForm{ID: "r", Deployments: []string{"d1"}}},
		{"bad mode", simpleRouteForm{ID: "r", PublicModel: "m", Mode: "random", Deployments: []string{"d1"}}},
		{"no deployments", simpleRouteForm{ID: "r", PublicModel: "m", Deployments: []string{" ", " "}}},
	} {
		if err := normalizeSimpleRoute(&tc.in); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestMission95CompileAndRemoveSimpleRoute(t *testing.T) {
	// Automatic mode compiles one pool + profile + endpoint.
	cfg := config.Config{}
	in := simpleRouteForm{ID: "r1", Name: "Route One", PublicModel: "gpt-x", Mode: "automatic", Deployments: []string{"d1", "d2"}, Enabled: boolPtrMission95(true)}
	compileSimpleRoute(&cfg, in)
	if len(cfg.CandidatePools) != 1 || cfg.CandidatePools[0].ID != "r1-pool" || len(cfg.CandidatePools[0].Deployments) != 2 {
		t.Fatalf("automatic pools = %+v", cfg.CandidatePools)
	}
	if len(cfg.RouteProfiles) != 1 || cfg.RouteProfiles[0].CandidatePool != "r1-pool" || cfg.RouteProfiles[0].FallbackChain != "" {
		t.Fatalf("automatic profiles = %+v", cfg.RouteProfiles)
	}
	if len(cfg.VirtualEndpoints) != 1 || cfg.VirtualEndpoints[0].RouteProfile != "r1-profile" || cfg.VirtualEndpoints[0].PublicModel != "gpt-x" {
		t.Fatalf("automatic endpoints = %+v", cfg.VirtualEndpoints)
	}
	if findVirtualEndpoint(&cfg, "r1") != 0 || findVirtualEndpoint(&cfg, "nope") != -1 {
		t.Error("findVirtualEndpoint wrong")
	}
	// Ordered mode compiles per-deployment stages + a fallback chain.
	cfg2 := config.Config{}
	compileSimpleRoute(&cfg2, simpleRouteForm{ID: "r2", Name: "Ordered", PublicModel: "m", Mode: "ordered", Deployments: []string{"d1", "d2"}, Enabled: boolPtrMission95(true)})
	if len(cfg2.CandidatePools) != 2 || cfg2.CandidatePools[0].ID != "r2-stage-1" || cfg2.CandidatePools[1].ID != "r2-stage-2" {
		t.Fatalf("ordered pools = %+v", cfg2.CandidatePools)
	}
	if len(cfg2.FallbackChains) != 1 || cfg2.FallbackChains[0].ID != "r2-fallback" || len(cfg2.FallbackChains[0].Pools) != 2 {
		t.Fatalf("ordered chains = %+v", cfg2.FallbackChains)
	}
	if cfg2.RouteProfiles[0].FallbackChain != "r2-fallback" || cfg2.RouteProfiles[0].CandidatePool != "r2-stage-1" {
		t.Fatalf("ordered profile = %+v", cfg2.RouteProfiles[0])
	}
	// Collisions detect each primitive kind.
	if !hasSimpleRoutePrimitiveCollision(&cfg, "r1") {
		t.Error("profile collision missed")
	}
	if !hasSimpleRoutePrimitiveCollision(&cfg2, "r2") {
		t.Error("stage/fallback collision missed")
	}
	if hasSimpleRoutePrimitiveCollision(&cfg, "other") {
		t.Error("false collision")
	}
	// Removal deletes exactly the route's primitives and keeps others.
	cfg.CandidatePools = append(cfg.CandidatePools, config.CandidatePoolConfig{ID: "unrelated-pool"})
	cfg.RouteProfiles = append(cfg.RouteProfiles, config.RouteProfileConfig{ID: "unrelated-profile"})
	removeSimpleRoutePrimitives(&cfg, "r1")
	if len(cfg.VirtualEndpoints) != 0 || len(cfg.RouteProfiles) != 1 || cfg.RouteProfiles[0].ID != "unrelated-profile" {
		t.Fatalf("after remove profiles/endpoints = %+v / %+v", cfg.RouteProfiles, cfg.VirtualEndpoints)
	}
	if len(cfg.CandidatePools) != 1 || cfg.CandidatePools[0].ID != "unrelated-pool" {
		t.Fatalf("after remove pools = %+v", cfg.CandidatePools)
	}
	// A stage pool still referenced by an advanced profile survives removal.
	cfg3 := config.Config{}
	compileSimpleRoute(&cfg3, simpleRouteForm{ID: "r3", Name: "R3", PublicModel: "m", Mode: "ordered", Deployments: []string{"d1"}, Enabled: boolPtrMission95(true)})
	cfg3.RouteProfiles = append(cfg3.RouteProfiles, config.RouteProfileConfig{ID: "advanced", CandidatePool: "r3-stage-1"})
	if !poolReferenced(&cfg3, "r3-stage-1") || poolReferenced(&cfg3, "nope") {
		t.Error("poolReferenced wrong")
	}
	removeSimpleRoutePrimitives(&cfg3, "r3")
	kept := false
	for _, p := range cfg3.CandidatePools {
		if p.ID == "r3-stage-1" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("referenced stage pool removed: %+v", cfg3.CandidatePools)
	}
	if len(cfg3.VirtualEndpoints) != 0 {
		t.Error("endpoint survived removal")
	}
}

func TestMission95CanonicalPureHelpers(t *testing.T) {
	// upstreamKindFor mapping, including the openai_chat default.
	cases := map[string]string{
		"anthropic_compatible": "anthropic",
		"gemini":               "gemini",
		"vertex":               "gemini",
		"bedrock":              "bedrock",
		"openai_responses":     "openai_responses",
		"openai":               "openai_chat",
		"unknown":              "openai_chat",
		"":                     "openai_chat",
	}
	for in, want := range cases {
		if got := upstreamKindFor(in); got != want {
			t.Errorf("upstreamKindFor(%q) = %q, want %q", in, got, want)
		}
		if got := isNativeFamily(in, want); !got {
			t.Errorf("isNativeFamily(%q, %q) false", in, want)
		}
		if isNativeFamily(in, "definitely-not-"+want) {
			t.Errorf("isNativeFamily(%q) false positive", in)
		}
	}
	// streamCommitWriter: first write commits 200, WriteHeader is sticky,
	// Flush commits when nothing was written yet.
	rec := httptest.NewRecorder()
	w := &streamCommitWriter{ResponseWriter: rec}
	if w.committed {
		t.Error("fresh stream writer committed")
	}
	if n, err := w.Write([]byte("chunk")); err != nil || n != 5 || !w.committed || rec.Code != http.StatusOK {
		t.Fatalf("stream write = %d, %v, committed=%v code=%d", n, err, w.committed, rec.Code)
	}
	w.WriteHeader(http.StatusTeapot)
	if rec.Code != http.StatusOK {
		t.Error("stream WriteHeader overwrote committed 200")
	}
	rec2 := httptest.NewRecorder()
	w2 := &streamCommitWriter{ResponseWriter: rec2}
	w2.Flush()
	// Flush forwards to the inner flusher without marking commit: the
	// commit boundary is Write/WriteHeader only.
	if w2.committed || !rec2.Flushed {
		t.Errorf("stream flush committed=%v flushed=%v", w2.committed, rec2.Flushed)
	}
	rec3 := httptest.NewRecorder()
	w3 := &streamCommitWriter{ResponseWriter: rec3}
	w3.WriteHeader(http.StatusAccepted)
	if !w3.committed || rec3.Code != http.StatusAccepted {
		t.Errorf("stream explicit header committed=%v code=%d", w3.committed, rec3.Code)
	}
	// canonicalErrorJSON envelopes per client protocol.
	for _, tc := range []struct {
		protocol string
		wantType string
	}{
		{"anthropic", "invalid_request_error"},
		{"openai_responses", "bad_type"},
		{"openai_chat", "bad_type"},
		{"anything-else", "bad_type"},
	} {
		rec := httptest.NewRecorder()
		canonicalErrorJSON(rec, tc.protocol, http.StatusBadRequest, "bad_type", "bad input")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("protocol %s code = %d", tc.protocol, rec.Code)
		}
		var env map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		errObj, _ := env["error"].(map[string]any)
		if tc.protocol == "anthropic" {
			if errObj["type"] != "invalid_request_error" || errObj["message"] != "bad input" {
				t.Errorf("anthropic envelope = %v", env)
			}
		} else if errObj["type"] != "bad_type" || errObj["message"] != "bad input" {
			t.Errorf("protocol %s envelope = %v", tc.protocol, env)
		}
		if tc.protocol == "openai_chat" || tc.protocol == "anything-else" {
			if _, ok := errObj["param"]; !ok {
				t.Errorf("protocol %s envelope lacks param: %v", tc.protocol, env)
			}
		}
	}
	_ = strings.Contains
}

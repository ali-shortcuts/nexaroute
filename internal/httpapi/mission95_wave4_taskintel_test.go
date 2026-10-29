package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/taskintel"
)

func wave4TaskIntelConfig() config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "model-a", Enabled: true, Weight: 1},
		{ID: "b", Model: "model-b", Enabled: true, Weight: 1},
	}}}
	cfg.CandidatePools = []config.CandidatePoolConfig{
		{ID: "primary", Name: "Primary", Mode: "explicit", Deployments: []string{"p/a"}},
		{ID: "task", Name: "Task", Mode: "explicit", Deployments: []string{"p/b"}},
		{ID: "fb1", Name: "FB1", Mode: "explicit", Deployments: []string{"p/a"}},
		{ID: "fb2", Name: "FB2", Mode: "explicit", Deployments: []string{"p/b"}},
		{ID: "empty", Name: "Empty", Mode: "explicit"},
	}
	return cfg
}

func wave4ApplyProfile(mode, override, pool string) config.RouteProfileConfig {
	return config.RouteProfileConfig{
		ID:            "rp1",
		TaskIntelMode: mode,
		TaskOverride:  override,
		TaskPools:     map[string]string{"code_edit": pool},
	}
}

// A nil route has no profile: observational only, inputs returned untouched.
func TestWave4ApplyTaskIntelNilRoute(t *testing.T) {
	s := &Server{}
	cands := []router.Scored{{Deployment: router.Deployment{ID: "p/a"}}}
	res, out, resolved := s.applyTaskIntel(httptest.NewRequest("POST", "/v1/chat/completions", nil), router.Requirement{}, config.Default(), cands, nil, taskIntelligence{})
	if res.Applied || len(out) != 1 || resolved != nil {
		t.Fatalf("nil route: res=%+v out=%d resolved=%v", res, len(out), resolved)
	}
}

// A route whose profile id matches nothing is likewise observational only.
func TestWave4ApplyTaskIntelMissingProfile(t *testing.T) {
	s := &Server{}
	orig := &route.ResolvedRoute{RouteProfileID: "ghost", PrimaryPoolID: "primary"}
	cands := []router.Scored{{Deployment: router.Deployment{ID: "p/a"}}}
	res, out, resolved := s.applyTaskIntel(httptest.NewRequest("POST", "/v1/chat/completions", nil), router.Requirement{}, config.Default(), cands, orig, taskIntelligence{})
	if res.Applied || len(out) != 1 || resolved != orig {
		t.Fatalf("missing profile: res=%+v out=%d same=%v", res, len(out), resolved == orig)
	}
}

// An unparseable mode behaves as off even when an override is present.
func TestWave4ApplyTaskIntelInvalidModeIsOff(t *testing.T) {
	s := &Server{}
	cfg := config.Default()
	cfg.RouteProfiles = []config.RouteProfileConfig{wave4ApplyProfile("bogus", "code_edit", "task")}
	orig := &route.ResolvedRoute{RouteProfileID: "rp1", PrimaryPoolID: "primary"}
	res, _, _ := s.applyTaskIntel(httptest.NewRequest("POST", "/v1/chat/completions", nil), router.Requirement{}, cfg, nil, orig, taskIntelligence{})
	if res.Applied || res.Mode != taskintel.ModeOff {
		t.Fatalf("invalid mode: res=%+v", res)
	}
}

// A resolvable decision whose pool cannot be expanded (no resolver at
// all) falls back to the original candidates and is traced as unmapped.
func TestWave4ApplyTaskIntelUnexpandablePool(t *testing.T) {
	s := &Server{}
	cfg := config.Default()
	cfg.RouteProfiles = []config.RouteProfileConfig{wave4ApplyProfile("apply", "code_edit", "task")}
	orig := &route.ResolvedRoute{RouteProfileID: "rp1", PrimaryPoolID: "primary"}
	cands := []router.Scored{{Deployment: router.Deployment{ID: "p/a"}}}
	res, out, resolved := s.applyTaskIntel(httptest.NewRequest("POST", "/v1/chat/completions", nil), router.Requirement{}, cfg, cands, orig, taskIntelligence{})
	if res.Applied || res.Outcome != taskintel.OutcomeUnmapped || len(out) != 1 || resolved != orig {
		t.Fatalf("unexpandable: res=%+v out=%d same=%v", res, len(out), resolved == orig)
	}
	if !strings.Contains(res.Summary, "could not be expanded") {
		t.Fatalf("summary %q lacks the expansion failure note", res.Summary)
	}
}

// A task pool that expands to zero eligible deployments falls back to the
// route's default candidates instead of failing the request.
func TestWave4ApplyTaskIntelEmptyPoolFallsBack(t *testing.T) {
	cfg := wave4TaskIntelConfig()
	cfg.RouteProfiles = []config.RouteProfileConfig{wave4ApplyProfile("apply", "code_edit", "empty")}
	s := testGateway(t, cfg)
	orig := &route.ResolvedRoute{RouteProfileID: "rp1", PrimaryPoolID: "primary", OrderedPoolIDs: []string{"primary"}, Pools: map[string]config.CandidatePoolConfig{"empty": {ID: "empty", Name: "Empty", Mode: "explicit"}}}
	cands := []router.Scored{{Deployment: router.Deployment{ID: "p/a"}}}
	res, out, resolved := s.applyTaskIntel(httptest.NewRequest("POST", "/v1/chat/completions", nil), router.Requirement{}, cfg, cands, orig, taskIntelligence{})
	if res.Applied || res.Outcome != taskintel.OutcomeUnmapped || len(out) != 1 || resolved != orig {
		t.Fatalf("empty pool: res=%+v out=%d same=%v", res, len(out), resolved == orig)
	}
	if !strings.Contains(res.Summary, "no eligible deployments") {
		t.Fatalf("summary %q lacks the empty-pool note", res.Summary)
	}
}

// switchTaskPool rebuilds the order (task pool first, original fallbacks
// after, the task pool itself deduplicated), rebuilds the fallback
// allowed sets from three different sources, and unions them.
func TestWave4SwitchTaskPoolRebuildsFallbacks(t *testing.T) {
	cfg := wave4TaskIntelConfig()
	s := testGateway(t, cfg)
	orig := &route.ResolvedRoute{
		PrimaryPoolID:  "primary",
		OrderedPoolIDs: []string{"primary", "task", "fb1", "fb2", "ghost"},
		FallbackAllowed: []map[string]struct{}{
			{"p/a": {}},
			{"p/b": {}},
		},
		Pools: map[string]config.CandidatePoolConfig{
			"task": {ID: "task", Name: "Task", Mode: "explicit"},
		},
	}
	switched, filtered, ok := s.switchTaskPool(router.Requirement{}, cfg, orig, "task")
	if !ok || switched == nil {
		t.Fatal("switchTaskPool must succeed for a known task pool")
	}
	if switched.PrimaryPoolID != "task" || switched.PrimaryPoolName != "Task" || switched.PrimaryMode != "explicit" {
		t.Fatalf("primary = %q/%q/%q", switched.PrimaryPoolID, switched.PrimaryPoolName, switched.PrimaryMode)
	}
	wantOrder := []string{"task", "fb1", "fb2", "ghost"}
	if len(switched.OrderedPoolIDs) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", switched.OrderedPoolIDs, wantOrder)
	}
	for i, id := range wantOrder {
		if switched.OrderedPoolIDs[i] != id {
			t.Fatalf("order = %v, want %v", switched.OrderedPoolIDs, wantOrder)
		}
	}
	// Fallback sets: fb1 from the stored FallbackAllowed entry, fb2 from
	// the resolver expansion, ghost as an empty set.
	if len(switched.FallbackAllowed) != 3 {
		t.Fatalf("fallback sets = %d, want 3", len(switched.FallbackAllowed))
	}
	if len(switched.FallbackAllowed[2]) != 0 {
		t.Fatalf("ghost fallback = %v, want empty", switched.FallbackAllowed[2])
	}
	if len(switched.AllAllowed) != 1 {
		t.Fatalf("AllAllowed = %v, want only p/b", switched.AllAllowed)
	}
	if _, ok := switched.AllAllowed["p/b"]; !ok {
		t.Fatalf("AllAllowed = %v, missing p/b", switched.AllAllowed)
	}
	if len(filtered) != 1 || filtered[0].Deployment.ID != "p/b" {
		t.Fatalf("filtered = %v, want exactly p/b", filtered)
	}
}

// switchTaskPool fails closed on a missing resolver, an unknown pool,
// and a pool the resolver cannot expand.
func TestWave4SwitchTaskPoolFailClosed(t *testing.T) {
	cfg := wave4TaskIntelConfig()
	s := testGateway(t, cfg)
	base := &route.ResolvedRoute{PrimaryPoolID: "primary", OrderedPoolIDs: []string{"primary"}, Pools: map[string]config.CandidatePoolConfig{"task": {ID: "task", Name: "Task", Mode: "explicit"}}}
	if _, _, ok := (&Server{}).switchTaskPool(router.Requirement{}, cfg, base, "task"); ok {
		t.Fatal("nil resolver must fail closed")
	}
	if _, _, ok := s.switchTaskPool(router.Requirement{}, cfg, base, "ghost"); ok {
		t.Fatal("unknown pool must fail closed")
	}
	noExpand := &route.ResolvedRoute{PrimaryPoolID: "primary", OrderedPoolIDs: []string{"primary"}, Pools: map[string]config.CandidatePoolConfig{"phantom": {ID: "phantom", Name: "Phantom", Mode: "explicit"}}}
	if _, _, ok := s.switchTaskPool(router.Requirement{}, cfg, noExpand, "phantom"); ok {
		t.Fatal("pool absent from the resolver must fail closed")
	}
}

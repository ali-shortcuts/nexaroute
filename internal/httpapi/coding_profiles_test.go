package httpapi

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Phase 15 (#415): coding workload profiles resolve against the
// operator's real route-profile configuration and never claim
// compatibility the declared capabilities do not prove.
func TestCodingProfilesSnapshot(t *testing.T) {
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{ID: "p1", Models: []config.ModelConfig{
				{ID: "m-tools", ContextWindow: 200000, Capabilities: config.Capabilities{Tools: true, Reasoning: true}},
				{ID: "m-plain", ContextWindow: 8000},
			}},
		},
		CandidatePools: []config.CandidatePoolConfig{
			{ID: "codepool", Deployments: []string{"p1/m-tools"}},
			{ID: "longpool", Deployments: []string{"p1/m-tools", "p1/m-plain"}},
			{ID: "ghostpool", Deployments: []string{"p9/ghost"}},
		},
		RouteProfiles: []config.RouteProfileConfig{
			{ID: "default", TaskIntelMode: "apply",
				TaskPools:     map[string]string{"coding": "codepool", "long_context": "longpool", "vision": "ghostpool"},
				TaskEvalGates: map[string]string{"coding": "swe-bench-lite"}},
			{ID: "plain"},
		},
	}

	snap := codingProfilesSnapshot(cfg)
	classes, _ := snap["classes"].([]map[string]any)
	byID := map[string]map[string]any{}
	for _, c := range classes {
		byID[c["id"].(string)] = c
	}
	if len(byID) != 5 {
		t.Fatalf("classes = %d, want 5", len(byID))
	}
	onlyMapping := func(c map[string]any) map[string]any {
		t.Helper()
		ms, _ := c["mappings"].([]map[string]any)
		if len(ms) != 1 {
			t.Fatalf("mappings = %v, want exactly 1", c["mappings"])
		}
		return ms[0]
	}

	coding := byID["primary_coding"]
	if coding["configured"] != true {
		t.Fatalf("primary_coding = %v", coding)
	}
	m := onlyMapping(coding)
	if m["route_profile_id"] != "default" || m["mode"] != "apply" || m["target_pool"] != "codepool" || m["capability"] != "met" || m["eval_gate"] != "swe-bench-lite" {
		t.Fatalf("primary_coding mapping = %v", m)
	}
	lm := onlyMapping(byID["long_context"])
	if lm["capability"] != "partial" || lm["capable_members"] != 1 {
		t.Fatalf("long_context mapping = %v, want partial with 1 capable member", lm)
	}
	vm := onlyMapping(byID["visual"])
	if vm["capability"] != "unmet" {
		t.Fatalf("visual capability = %v, want unmet (ghost member cannot prove vision)", vm)
	}
	if unknown, _ := vm["unknown_members"].([]string); len(unknown) != 1 || unknown[0] != "p9/ghost" {
		t.Fatalf("visual unknown_members = %v", vm["unknown_members"])
	}
	if byID["background"]["configured"] != false || byID["reasoning"]["configured"] != false {
		t.Fatalf("background/reasoning must be unconfigured: %v / %v", byID["background"], byID["reasoning"])
	}

	// Empty config: nothing configured, nothing claimed.
	empty := codingProfilesSnapshot(config.Config{})
	for _, c := range empty["classes"].([]map[string]any) {
		if c["configured"] != false {
			t.Fatalf("empty class %v configured, want false", c["id"])
		}
	}
}

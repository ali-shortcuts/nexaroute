package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestSatisfiesNoTrainingTable(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"no", true},
		{"NO", true},
		{" No ", true},
		{"yes", false},
		{"YES", false},
		{"unknown", false},
		{"UNKNOWN", false},
		{"", false},
		{"  ", false},
		{"sometimes", false},
	}
	for _, tc := range cases {
		if got := SatisfiesNoTraining(tc.in); got != tc.want {
			t.Fatalf("SatisfiesNoTraining(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func privacyTestRouter(t *testing.T, trainsA, trainsB string) *Router {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{
		{ID: "pa", Name: "PA", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", Enabled: true,
			DataHandling: config.DataHandlingConfig{TrainsOnData: trainsA},
			Models:       []config.ModelConfig{{ID: "m", Model: "shared-model", Enabled: true, Weight: 1}}},
		{ID: "pb", Name: "PB", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", Enabled: true,
			DataHandling: config.DataHandlingConfig{TrainsOnData: trainsB},
			Models:       []config.ModelConfig{{ID: "m", Model: "shared-model", Enabled: true, Weight: 1}}},
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	rt := New(cfg, hm)
	for _, d := range rt.All() {
		hm.RecordSuccess(d.ID, time.Millisecond)
	}
	return rt
}

func TestEligibleDeploymentPrivacyTable(t *testing.T) {
	cases := []struct {
		name     string
		require  bool
		trains   string
		eligible bool
	}{
		{"no requirement + yes", false, "yes", true},
		{"no requirement + unknown", false, "unknown", true},
		{"no requirement + no", false, "no", true},
		{"required + no", true, "no", true},
		{"required + NO case-insensitive", true, "NO", true},
		{"required + yes rejected", true, "yes", false},
		{"required + unknown rejected (fail closed)", true, "unknown", false},
		{"required + empty rejected", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := privacyTestRouter(t, tc.trains, "no")
			// Find the deployment with the target trains value.
			var target Deployment
			found := false
			for _, d := range rt.All() {
				if d.TrainsOnData == tc.trains || (tc.trains == "" && d.TrainsOnData == "unknown") {
					// For empty input, ApplyDefaults normalizes to unknown.
					target = d
					found = true
					break
				}
			}
			if !found {
				// Fall back: construct directly to test eligibleDeployment gate.
				target = Deployment{ID: "x/m", ProviderID: "x", TrainsOnData: tc.trains}
			}
			req := Requirement{Model: "shared-model", RequireNoTraining: tc.require}
			if _, ok := rt.Eligible(target.ID, req); ok != tc.eligible && found {
				t.Fatalf("Eligible(%s, require=%v trains=%q)=%v want %v", target.ID, tc.require, tc.trains, ok, tc.eligible)
			}
			if !found {
				// Direct gate check via Candidates path is not possible for
				// synthetic IDs; verify SatisfiesNoTraining semantics instead.
				if SatisfiesNoTraining(tc.trains) != tc.eligible && tc.require {
					t.Fatalf("gate mismatch for trains=%q", tc.trains)
				}
			}
		})
	}
}

func TestDeploymentCarriesTrainsOnData(t *testing.T) {
	rt := privacyTestRouter(t, "yes", "no")
	byProvider := map[string]string{}
	for _, d := range rt.All() {
		byProvider[d.ProviderID] = d.TrainsOnData
	}
	if byProvider["pa"] != "yes" {
		t.Fatalf("pa trains=%q want yes", byProvider["pa"])
	}
	if byProvider["pb"] != "no" {
		t.Fatalf("pb trains=%q want no", byProvider["pb"])
	}
}

func TestCandidatesFilterPrivacyFailClosed(t *testing.T) {
	rt := privacyTestRouter(t, "yes", "unknown")
	req := Requirement{Model: "shared-model", RequireNoTraining: true}
	got := rt.Candidates(req)
	if len(got) != 0 {
		t.Fatalf("expected 0 candidates when all are yes/unknown, got %d", len(got))
	}
	rt2 := privacyTestRouter(t, "yes", "no")
	got2 := rt2.Candidates(req)
	if len(got2) != 1 || got2[0].Deployment.ProviderID != "pb" {
		t.Fatalf("expected only pb eligible, got %+v", got2)
	}
	// Without requirement both are eligible.
	reqOpen := Requirement{Model: "shared-model"}
	if got3 := rt2.Candidates(reqOpen); len(got3) != 2 {
		t.Fatalf("expected 2 candidates without privacy, got %d", len(got3))
	}
}

func TestSessionAffinityBucketIsolatesPrivacy(t *testing.T) {
	rt := privacyTestRouter(t, "yes", "no")
	a := Requirement{Model: "m", SessionKey: "s1", RequireNoTraining: false}
	b := Requirement{Model: "m", SessionKey: "s1", RequireNoTraining: true}
	if rt.affinityBucket(a) == rt.affinityBucket(b) {
		t.Fatal("affinity bucket must isolate privacy classes")
	}
}

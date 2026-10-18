package taskintel

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func testProfile(t taskprofile.TaskType, conf float64) taskprofile.TaskProfile {
	return taskprofile.TaskProfile{Type: t, Complexity: taskprofile.ComplexityMedium, Confidence: conf}
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in    string
		want  Mode
		valid bool
	}{
		{"", ModeOff, true},
		{"off", ModeOff, true},
		{"OFF", ModeOff, true},
		{"suggest", ModeSuggest, true},
		{"Suggest", ModeSuggest, true},
		{"apply", ModeApply, true},
		{"APPLY", ModeApply, true},
		{"bogus", ModeOff, false},
		{"on", ModeOff, false},
	}
	for _, c := range cases {
		got, ok := ParseMode(c.in)
		if got != c.want || ok != c.valid {
			t.Errorf("ParseMode(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestNormalizeTaskType(t *testing.T) {
	for _, tt := range taskprofile.AllTaskTypes() {
		if got, ok := NormalizeTaskType("  " + strings.ToUpper(string(tt)) + " "); !ok || got != tt {
			t.Errorf("NormalizeTaskType(%q) = (%q,%v), want (%q,true)", tt, got, ok, tt)
		}
	}
	for _, bad := range []string{"", "codingg", "drop table", "code edit", "123"} {
		if _, ok := NormalizeTaskType(bad); ok {
			t.Errorf("NormalizeTaskType(%q) unexpectedly valid", bad)
		}
	}
}

func TestResolve_OffModes(t *testing.T) {
	for _, mode := range []Mode{ModeOff, "bogus", ""} {
		r := Resolve(Input{
			Mode:      mode,
			Profile:   testProfile(taskprofile.TaskCoding, 0.99),
			Pin:       taskprofile.TaskVision,
			Override:  taskprofile.TaskDebugging,
			TaskPools: map[string]string{"coding": "code-pool"},
		})
		if r.Outcome != OutcomeOff || r.Applied || r.TargetPool != "" {
			t.Errorf("mode %q: got %+v, want off with no pool change", mode, r)
		}
	}
}

func TestResolve_Precedence(t *testing.T) {
	pools := map[string]string{
		"coding":    "code-pool",
		"debugging": "debug-pool",
		"vision":    "vision-pool",
	}
	strong := testProfile(taskprofile.TaskCoding, 0.95)

	// Operator override beats pin and classifier.
	r := Resolve(Input{Mode: ModeApply, Profile: strong, Pin: taskprofile.TaskVision, Override: taskprofile.TaskDebugging, TaskPools: pools})
	if r.Outcome != OutcomeOverridden || r.EffectiveTask != taskprofile.TaskDebugging || r.TargetPool != "debug-pool" || !r.Applied {
		t.Fatalf("override precedence: %+v", r)
	}

	// Client pin beats the classifier.
	r = Resolve(Input{Mode: ModeApply, Profile: strong, Pin: taskprofile.TaskVision, TaskPools: pools})
	if r.Outcome != OutcomePinned || r.EffectiveTask != taskprofile.TaskVision || r.TargetPool != "vision-pool" || !r.Applied {
		t.Fatalf("pin precedence: %+v", r)
	}

	// Classifier result applies when nothing overrides it.
	r = Resolve(Input{Mode: ModeApply, Profile: strong, TaskPools: pools})
	if r.Outcome != OutcomeApplied || r.EffectiveTask != taskprofile.TaskCoding || r.TargetPool != "code-pool" || !r.Applied {
		t.Fatalf("classifier apply: %+v", r)
	}

	// Suggest mode never flips Applied, but still names the target pool.
	r = Resolve(Input{Mode: ModeSuggest, Profile: strong, TaskPools: pools})
	if r.Outcome != OutcomeSuggested || r.Applied || r.TargetPool != "code-pool" {
		t.Fatalf("suggest mode: %+v", r)
	}

	// Suggest mode also honors pin/override for the record (without applying).
	r = Resolve(Input{Mode: ModeSuggest, Profile: strong, Pin: taskprofile.TaskVision, TaskPools: pools})
	if r.Outcome != OutcomePinned || r.Applied {
		t.Fatalf("suggest+pin: %+v", r)
	}
}

func TestResolve_LowConfidenceDefaults(t *testing.T) {
	pools := map[string]string{"coding": "code-pool"}

	// Below threshold: default profile, no routing change, even in apply mode.
	r := Resolve(Input{Mode: ModeApply, Profile: testProfile(taskprofile.TaskCoding, 0.30), TaskPools: pools})
	if r.Outcome != OutcomeLowConfidence || r.EffectiveTask != DefaultTask || r.Applied || r.TargetPool != "" {
		t.Fatalf("low confidence: %+v", r)
	}

	// Unknown type is never applied.
	r = Resolve(Input{Mode: ModeApply, Profile: testProfile(taskprofile.TaskUnknown, 0.99), TaskPools: pools})
	if r.Outcome != OutcomeLowConfidence || r.Applied {
		t.Fatalf("unknown type: %+v", r)
	}

	// At exactly the threshold, classification applies.
	r = Resolve(Input{Mode: ModeApply, Profile: testProfile(taskprofile.TaskCoding, DefaultMinConfidence), MinConfidence: DefaultMinConfidence, TaskPools: pools})
	if r.Outcome != OutcomeApplied || !r.Applied {
		t.Fatalf("at-threshold: %+v", r)
	}

	// A custom stricter threshold is honored.
	r = Resolve(Input{Mode: ModeApply, Profile: testProfile(taskprofile.TaskCoding, 0.80), MinConfidence: 0.90, TaskPools: pools})
	if r.Outcome != OutcomeLowConfidence || r.Applied {
		t.Fatalf("custom threshold: %+v", r)
	}
}

func TestResolve_Unmapped(t *testing.T) {
	// High-confidence classification with no pool mapping: recorded, not applied.
	r := Resolve(Input{
		Mode:      ModeApply,
		Profile:   testProfile(taskprofile.TaskCoding, 0.95),
		TaskPools: map[string]string{"long_context": "ctx-pool"},
	})
	if r.Outcome != OutcomeUnmapped || r.Applied || r.TargetPool != "" {
		t.Fatalf("unmapped: %+v", r)
	}
	if r.EffectiveTask != taskprofile.TaskCoding {
		t.Fatalf("unmapped must still name the effective task: %+v", r)
	}

	// Nil map behaves the same.
	r = Resolve(Input{Mode: ModeApply, Profile: testProfile(taskprofile.TaskCoding, 0.95)})
	if r.Outcome != OutcomeUnmapped || r.Applied {
		t.Fatalf("nil map: %+v", r)
	}
}

func TestResolve_SummaryMentionsDecision(t *testing.T) {
	r := Resolve(Input{Mode: ModeApply, Profile: testProfile(taskprofile.TaskCoding, 0.95), TaskPools: map[string]string{"coding": "code-pool"}})
	if !strings.Contains(r.Summary, "coding") || !strings.Contains(r.Summary, "applied") {
		t.Fatalf("summary should name task and decision: %q", r.Summary)
	}
	r = Resolve(Input{Mode: ModeOff, Profile: testProfile(taskprofile.TaskCoding, 0.95)})
	if r.Summary == "" {
		t.Fatal("summary must never be empty")
	}
}

func TestResolve_Deterministic(t *testing.T) {
	in := Input{
		Mode:          ModeApply,
		Profile:       testProfile(taskprofile.TaskCoding, 0.87),
		Pin:           taskprofile.TaskVision,
		TaskPools:     map[string]string{"coding": "code-pool", "vision": "vision-pool"},
		MinConfidence: 0.5,
	}
	a, b := Resolve(in), Resolve(in)
	if a != b {
		t.Fatalf("non-deterministic: %+v vs %+v", a, b)
	}
}

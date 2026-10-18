package taskintel

import (
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// #421 item 3: a background-classified request resolves to the
// route profile's background pool in apply mode (premium
// capacity stays with the main thread), and stays unmapped —
// routing unchanged — when the operator mapped no pool.
func TestResolveBackgroundPool(t *testing.T) {
	prof := testProfile(taskprofile.TaskBackground, 0.95)

	r := Resolve(Input{Mode: ModeApply, Profile: prof, TaskPools: map[string]string{"background": "cheap-pool", "coding": "premium-pool"}})
	if r.Outcome != OutcomeApplied || r.EffectiveTask != taskprofile.TaskBackground || r.TargetPool != "cheap-pool" || !r.Applied {
		t.Fatalf("background with pool: %+v", r)
	}

	r = Resolve(Input{Mode: ModeApply, Profile: prof, TaskPools: map[string]string{"coding": "premium-pool"}})
	if r.Outcome != OutcomeUnmapped || r.Applied || r.TargetPool != "" {
		t.Fatalf("background without pool must leave routing unchanged: %+v", r)
	}
}

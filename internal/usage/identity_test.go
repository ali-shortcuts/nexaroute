package usage

import "testing"

func TestIdentityTrackerAggregatesAndReturnsIsolatedSortedSnapshots(t *testing.T) {
	tracker := NewIdentityTracker()
	tracker.Record("z-user", "tenant-z", "project-z", "team-z", 10, 2)
	tracker.Record("a-user", "tenant-a", "project-a", "team-a", 3, 4)
	tracker.Record("z-user", "ignored-tenant", "ignored-project", "ignored-team", -5, 5)
	tracker.Record("", "legacy", "", "", 7, -1)
	tracker.Record("zero", "", "", "", -10, 0) // no token usage is not a request total

	got := tracker.Snapshot()
	if len(got) != 3 || got[0].Identity != "a-user" || got[1].Identity != "anonymous" || got[2].Identity != "z-user" {
		t.Fatalf("unexpected sorted snapshot: %+v", got)
	}
	if got[1].Requests != 1 || got[1].Input != 7 || got[1].Output != 0 || got[1].TenantID != "legacy" {
		t.Fatalf("anonymous record not normalized: %+v", got[1])
	}
	if got[2].Requests != 2 || got[2].Input != 10 || got[2].Output != 7 || got[2].TenantID != "tenant-z" {
		t.Fatalf("identity totals incorrect: %+v", got[2])
	}
	got[2].Input = 0
	if next := tracker.Snapshot(); next[2].Input != 10 {
		t.Fatal("snapshot shares mutable storage with the tracker")
	}

	tracker.Reset()
	if next := tracker.Snapshot(); len(next) != 0 {
		t.Fatalf("Reset left identity rows: %+v", next)
	}
}

func TestIdentityTrackerEmptySnapshotIsNonNil(t *testing.T) {
	if got := NewIdentityTracker().Snapshot(); got == nil || len(got) != 0 {
		t.Fatalf("empty snapshot = %#v", got)
	}
}

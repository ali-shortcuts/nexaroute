package policysim

import (
	"context"
	"testing"
)

func TestSimulateReportCarriesPolicyVersion(t *testing.T) {
	cfg := simConfig()
	rep, err := Simulate(context.Background(), cfg, "baseline-first", []Fixture{simFixture()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PolicyVersion == "" {
		t.Fatal("simulate report is missing a policy version")
	}
	again, err := Simulate(context.Background(), cfg, "baseline-first", []Fixture{simFixture()})
	if err != nil {
		t.Fatal(err)
	}
	if again.PolicyVersion != rep.PolicyVersion {
		t.Fatalf("policy version is not deterministic: %q vs %q", rep.PolicyVersion, again.PolicyVersion)
	}
}

func TestReplayFlagsPolicyVersionMismatch(t *testing.T) {
	cfg := simConfig()
	fx := simFixture()
	rep, err := Simulate(context.Background(), cfg, "latency-first", []Fixture{fx})
	if err != nil {
		t.Fatal(err)
	}
	version := rep.PolicyVersion

	records := []ReplayRecord{
		{Request: fx.Request, RecordedWinner: "b", PolicyVersion: version},
		{Request: fx.Request, RecordedWinner: "b", PolicyVersion: "some-other-revision"},
		{Request: fx.Request, RecordedWinner: "b"}, // unknown version counts as match
	}
	out, err := Replay(context.Background(), cfg, "latency-first", records)
	if err != nil {
		t.Fatal(err)
	}
	if out.PolicyVersion != version {
		t.Fatalf("replay policy version = %q, want %q", out.PolicyVersion, version)
	}
	if out.VersionMismatches != 1 {
		t.Fatalf("version mismatches = %d, want 1", out.VersionMismatches)
	}
	if !out.Cases[0].VersionMatch || out.Cases[1].VersionMatch || !out.Cases[2].VersionMatch {
		t.Fatalf("per-case version match wrong: %+v", out.Cases)
	}
}

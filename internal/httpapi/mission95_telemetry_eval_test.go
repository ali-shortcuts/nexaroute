package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

func TestMission95TelemetryAggregator(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	a := newTelemetryAggregator()
	if a.deployments() != 0 {
		t.Fatal("fresh aggregator non-empty")
	}
	// Empty / oversize ids and attribution are dropped, never stored.
	a.observe("", "p", "m", true, 10, 5, now)
	a.observe(strings.Repeat("d", 513), "p", "m", true, 10, 5, now)
	a.observe("d1", strings.Repeat("p", 257), "m", true, 10, 5, now)
	a.observe("d1", "p", strings.Repeat("m", 1025), true, 10, 5, now)
	if a.deployments() != 0 {
		t.Errorf("dropped observations stored: %d", a.deployments())
	}
	// Negative timings clamp to zero (no latency sample); huge timings cap.
	a.observe("d1", "p1", "m1", true, -5, -2, now)
	a.observe("d1", "p1", "m1", false, 700000, 800000, now)
	a.observe("d1", "p2", "m2", true, 100, 50, now)
	if a.deployments() != 1 {
		t.Fatalf("deployments = %d", a.deployments())
	}
	buckets := a.snapshot()
	if len(buckets) != 1 {
		t.Fatalf("snapshot = %d buckets", len(buckets))
	}
	b := buckets[0]
	if b.successes != 2 || b.failures != 1 {
		t.Errorf("counts success=%d failure=%d", b.successes, b.failures)
	}
	if len(b.latencies) != 2 || b.latencies[0] != 600000 || b.latencies[1] != 100 {
		t.Errorf("latencies = %v", b.latencies)
	}
	if len(b.ttfts) != 2 || b.ttfts[0] != 600000 || b.ttfts[1] != 50 {
		t.Errorf("ttfts = %v", b.ttfts)
	}
	// Attribution follows the latest non-empty values.
	if b.providerID != "p2" || b.model != "m2" {
		t.Errorf("attribution provider=%q model=%q", b.providerID, b.model)
	}
	// Snapshot is a copy: mutating it does not corrupt the aggregator.
	buckets[0].latencies[0] = -1
	if again := a.snapshot(); again[0].latencies[0] != 600000 {
		t.Error("snapshot aliases aggregator storage")
	}
	// Ring bound: only the newest 256 samples survive.
	for i := 0; i < 300; i++ {
		a.observe("d1", "p", "m", true, float64(i+1), 0, now)
	}
	if got := len(a.snapshot()[0].latencies); got != maxTelemetrySamplesPerDeployment {
		t.Errorf("ring size = %d", got)
	}
	// Retain drops everything not in the valid set; empty set drops all.
	a.observe("d2", "p", "m", true, 1, 0, now)
	a.retain(map[string]struct{}{"d1": {}})
	if a.deployments() != 1 {
		t.Errorf("retain kept %d deployments", a.deployments())
	}
	a.retain(map[string]struct{}{})
	if a.deployments() != 0 {
		t.Error("retain with empty set kept buckets")
	}
	// Snapshot ordering is deterministic by deployment id.
	a.observe("zz", "p", "m", true, 1, 0, now)
	a.observe("aa", "p", "m", true, 1, 0, now)
	snap := a.snapshot()
	if snap[0].deploymentID != "aa" || snap[1].deploymentID != "zz" {
		t.Errorf("snapshot order = %q, %q", snap[0].deploymentID, snap[1].deploymentID)
	}
}

func TestMission95PercentileAndMean(t *testing.T) {
	if percentileCopy(nil, 0.5) != 0 || meanCopy(nil) != 0 {
		t.Error("empty inputs did not yield zero")
	}
	vals := []float64{30, 10, 20}
	if got := percentileCopy(vals, 0); got != 10 {
		t.Errorf("p0 = %v", got)
	}
	if got := percentileCopy(vals, 1); got != 30 {
		t.Errorf("p1 = %v", got)
	}
	if got := percentileCopy(vals, 0.5); got != 20 {
		t.Errorf("p50 = %v", got)
	}
	if got := percentileCopy(vals, 0.95); got != 30 {
		t.Errorf("p95 = %v", got)
	}
	// Input is not mutated by the sort.
	if vals[0] != 30 || vals[1] != 10 {
		t.Errorf("percentile mutated input: %v", vals)
	}
	if got := meanCopy([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Errorf("mean = %v", got)
	}
}

func TestMission95FlushTelemetry(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// Disabled plane flushes nothing.
	off := newEvaluationPlane(config.EvaluationConfig{Enabled: false, TelemetryIngestEnabled: true}, nil)
	off.telemetry.observe("d1", "p", "m", true, 10, 5, now)
	if got := off.flushTelemetry(now); got != 0 {
		t.Errorf("disabled flush wrote %d", got)
	}
	// Ingest-disabled plane flushes nothing.
	gated := newEvaluationPlane(config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: false}, nil)
	gated.telemetry.observe("d1", "p", "m", true, 10, 5, now)
	if got := gated.flushTelemetry(now); got != 0 {
		t.Errorf("gated flush wrote %d", got)
	}
	// Enabled plane with enough samples writes one scorecard and counts it.
	on := newEvaluationPlane(config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: true, TelemetryMinSamples: 1, LatencyTargetMS: 100, TTFTTargetMS: 50}, nil)
	on.telemetry.observe("d1", "p", "m", true, 10, 5, now)
	on.telemetry.observe("d1", "p", "m", false, 20, 8, now)
	if got := on.flushTelemetry(now); got != 1 {
		t.Fatalf("flush wrote %d scorecards", got)
	}
	if on.telemetryIngested.Load() != 1 {
		t.Errorf("ingested counter = %d", on.telemetryIngested.Load())
	}
	sc, ok := on.reg.Snapshot()[0], len(on.reg.Snapshot()) == 1
	if !ok || sc.DeploymentID != "d1" {
		t.Fatalf("registry snapshot = %+v", on.reg.Snapshot())
	}
	// Below-minimum buckets are skipped.
	thin := newEvaluationPlane(config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: true, TelemetryMinSamples: 5}, nil)
	thin.telemetry.observe("d9", "p", "m", true, 10, 5, now)
	if got := thin.flushTelemetry(now); got != 0 {
		t.Errorf("thin bucket flushed %d", got)
	}
	// Nil telemetry aggregator flushes nothing instead of panicking.
	nilAgg := newEvaluationPlane(config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: true}, nil)
	nilAgg.telemetry = nil
	if got := nilAgg.flushTelemetry(now); got != 0 {
		t.Errorf("nil-aggregator flush wrote %d", got)
	}
	_ = scorecards.MinMeasuredSamples
}

func TestMission95EvaluationStateLoadPersist(t *testing.T) {
	plane := newEvaluationPlane(config.EvaluationConfig{Enabled: true}, nil)
	// Missing file is a clean no-op.
	if err := plane.loadState(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Errorf("missing state load failed: %v", err)
	}
	// Directory path fails closed.
	if err := plane.loadState(t.TempDir()); err == nil {
		t.Error("directory state load accepted")
	}
	// Empty file is a clean no-op.
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plane.loadState(empty); err != nil {
		t.Errorf("empty state load failed: %v", err)
	}
	// Malformed / wrong-version / unknown-field documents are rejected whole.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid json", `{`},
		{"unknown field", `{"version":1,"bogus":1}`},
		{"wrong version", `{"version":2,"runs":[]}`},
	} {
		p := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := plane.loadState(p); err == nil {
			t.Errorf("%s: state accepted", tc.name)
		}
	}
	// Oversize file is rejected before parsing.
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, make([]byte, maxEvaluationStateBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := plane.loadState(big); err == nil {
		t.Error("oversize state accepted")
	}
	// Persist with no state path is a no-op; persistBestEffort tolerates it.
	if err := plane.persist(); err != nil {
		t.Errorf("no-path persist failed: %v", err)
	}
	plane.persistBestEffort()
	if plane.stateWritesFailed.Load() != 0 {
		t.Error("no-path persist counted a failure")
	}
	// Persist round-trips through loadState on a fresh plane.
	dir := t.TempDir()
	statePath := filepath.Join(dir, "nested", "eval.json")
	plane.setConfig(config.EvaluationConfig{Enabled: true, StatePath: statePath})
	if err := plane.persist(); err != nil {
		t.Fatalf("persist failed: %v", err)
	}
	info, err := os.Stat(statePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, %v", info, err)
	}
	fresh := newEvaluationPlane(config.EvaluationConfig{Enabled: true}, nil)
	if err := fresh.loadState(statePath); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	// Persist failure is counted, not fatal: path under a regular file.
	broken := newEvaluationPlane(config.EvaluationConfig{Enabled: true, StatePath: filepath.Join(statePath, "child.json")}, nil)
	broken.persistBestEffort()
	if broken.stateWritesFailed.Load() != 1 {
		t.Errorf("persist failure count = %d", broken.stateWritesFailed.Load())
	}
	// runTelemetryFlushLoop drains ticks through syncProductionTelemetry.
	// observeCompletedTelemetry resolves model attribution from the router,
	// so the server needs a (possibly empty) router, not a nil one.
	hm := health.New(3, time.Minute)
	srv := &Server{
		evaluation: newEvaluationPlane(config.EvaluationConfig{Enabled: true, TelemetryIngestEnabled: true}, nil),
		rt:         router.New(config.Config{}, hm),
	}
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() { srv.runTelemetryFlushLoop(ticks); close(done) }()
	ticks <- time.Now()
	close(ticks)
	<-done
	// Observe hooks are no-ops without a plane or when gated.
	(&Server{}).observeCompletedTelemetry("d", "p", true, time.Second, 0)
	(&Server{}).observeTelemetryTTFT("d", time.Second)
	(&Server{}).syncProductionTelemetry()
	srv.observeCompletedTelemetry("d1", "p", true, 10*time.Millisecond, 5*time.Millisecond)
	if srv.evaluation.telemetry.deployments() != 1 {
		t.Error("observeCompletedTelemetry did not aggregate")
	}
	srv.observeTelemetryTTFT("d1", 7*time.Millisecond)
	if len(srv.evaluation.telemetry.snapshot()[0].ttfts) == 0 {
		t.Error("observeTelemetryTTFT did not record")
	}
}

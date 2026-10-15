package httpapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// PR-B defect fix: the periodic flush loop actually drives the sync. The
// loop is driven by an injected tick channel so the test is deterministic,
// not time-based.
func TestTelemetryIngest_FlushLoopDrivesSync(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Evaluation.TelemetryIngestEnabled = true
	cfg.Evaluation.TelemetryMinSamples = 5
	s := testGateway(t, cfg)

	for i := 0; i < 5; i++ {
		s.recordRouteSuccess(router.Requirement{Model: "model-a"}, "p1/m1", "p1", 200*time.Millisecond)
	}
	if got := scorecardTotal(t, s); got != 0 {
		t.Fatalf("hot path flushed telemetry: %d scorecards before any flush", got)
	}

	tick := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runTelemetryFlushLoop(tick)
	}()
	tick <- time.Now()
	close(tick)
	<-done

	if got := scorecardTotal(t, s); got != 1 {
		t.Fatalf("flush loop wrote %d scorecards, want 1", got)
	}
}

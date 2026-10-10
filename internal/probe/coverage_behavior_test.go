package probe

import (
	"context"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestCoverageBehaviorReadyLeaseExpiryBoundaries(t *testing.T) {
	now := time.Unix(100, 0)
	cases := []struct {
		name    string
		checked time.Time
		lease   time.Duration
		want    bool
	}{
		{"zero lease", now, 0, true},
		{"missing check", time.Time{}, time.Minute, true},
		{"fresh", now, time.Minute, false},
		{"expired", now.Add(-time.Minute), time.Minute, true},
	}
	for _, tc := range cases {
		if got := readyLeaseExpired(health.State{LastChecked: tc.checked}, now, tc.lease); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestCoverageBehaviorAcquireProbeCancellationReleasesNothing(t *testing.T) {
	e := New(config.Default(), nil, nil, health.New(3, time.Minute), events.New(4))
	e.cfg.Probe.Concurrency = 1
	e.activeProbes = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e.acquireProbe(ctx) {
		t.Fatal("canceled waiter acquired a full probe slot")
	}
	if e.activeProbes != 1 {
		t.Fatalf("active probes changed while canceled: %d", e.activeProbes)
	}
	e.releaseProbe()
	if e.Stats().ActiveProbes != 0 {
		t.Fatal("release did not clear active probe count")
	}
}

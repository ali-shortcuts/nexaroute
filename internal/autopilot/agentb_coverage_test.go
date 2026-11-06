package autopilot

import (
	"math"
	"testing"
)

// Agent B coverage: percentile near-one boundary.
//
// The nearest-rank index for 0 < p < 1 is int(p*(n-1)+0.5), which is at
// most n-1 even with float rounding, so the maximum element is the
// result for p arbitrarily close to 1. This test pins that boundary:
// a regression that clamped or mis-rounded the index would return a
// smaller sample and fail.
func TestAgentBPercentileNearOneBoundary(t *testing.T) {
	if got := percentile([]float64{1, 2, 3}, 0.999999); got != 3 {
		t.Fatalf("percentile(p~1)=%v, want maximum 3", got)
	}
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = float64(i + 1)
	}
	if got := percentile(samples, 0.999); got != 100 {
		t.Fatalf("percentile(n=100, p=0.999)=%v, want maximum 100", got)
	}
	if got := percentile(samples, 0.5); got != 51 {
		t.Fatalf("percentile(n=100, p=0.5)=%v, want nearest-rank 51", got)
	}
	if got := percentile(samples, math.NaN()); got != 1 {
		t.Fatalf("percentile(NaN)=%v, want minimum 1", got)
	}
}

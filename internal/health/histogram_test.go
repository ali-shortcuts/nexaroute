package health

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestHistogramBucketPlacementAndCumulative(t *testing.T) {
	var h Histogram
	// Boundary values belong to the bucket whose bound they equal
	// (Prometheus `le` semantics); the largest finite bound overflows
	// into +Inf only above it.
	for _, ms := range []float64{0, 5, 6, 100, 64000, 64001, 120000} {
		h.Observe(ms)
	}
	if h.Count != 7 {
		t.Fatalf("count = %d, want 7", h.Count)
	}
	if want := 5.0 + 6 + 100 + 64000 + 64001 + 120000; h.SumMS != want {
		t.Fatalf("sum = %v, want %v", h.SumMS, want)
	}
	cum := h.Cumulative()
	if len(cum) != len(LatencyHistogramBoundsMS)+1 {
		t.Fatalf("cumulative has %d entries, want %d", len(cum), len(LatencyHistogramBoundsMS)+1)
	}
	// Bounds start 5,10,25,50,100,...: cum[0]=2 (0 and 5), cum at 100ms
	// bound = 4 (0,5,6,100), final +Inf = total count.
	if cum[0] != 2 {
		t.Errorf("cum[5ms] = %d, want 2", cum[0])
	}
	idx100 := 4 // bounds: 5,10,25,50,100
	if cum[idx100] != 4 {
		t.Errorf("cum[100ms] = %d, want 4", cum[idx100])
	}
	if cum[len(cum)-1] != h.Count {
		t.Errorf("+Inf cumulative = %d, want count %d", cum[len(cum)-1], h.Count)
	}
	for i := 1; i < len(cum); i++ {
		if cum[i] < cum[i-1] {
			t.Fatalf("cumulative not monotonic at %d: %v", i, cum)
		}
	}
}

func TestManagerHistogramsFedByRecordPaths(t *testing.T) {
	m := New(3, time.Minute)
	m.RecordSuccess("dep-a", 120*time.Millisecond)
	m.RecordFailure("dep-a", "boom", 1500*time.Millisecond)
	m.RecordTTFT("dep-a", 40*time.Millisecond)
	m.RecordTTFT("dep-a", 60*time.Millisecond)

	lat, ttft, ok := m.Histograms("dep-a")
	if !ok {
		t.Fatal("no histograms recorded for dep-a")
	}
	if lat.Count != 2 || lat.SumMS != 1620 {
		t.Errorf("latency: count=%d sum=%v, want 2/1620", lat.Count, lat.SumMS)
	}
	if ttft.Count != 2 || ttft.SumMS != 100 {
		t.Errorf("ttft: count=%d sum=%v, want 2/100", ttft.Count, ttft.SumMS)
	}
	if _, _, ok := m.Histograms("dep-missing"); ok {
		t.Error("histograms reported for an unobserved deployment")
	}
	// Copies: mutating the returned histogram must not corrupt the manager's.
	lat.Buckets[0] = 9999
	again, _, _ := m.Histograms("dep-a")
	if again.Buckets[0] == 9999 {
		t.Error("Histograms returned aliased buckets")
	}
}

func TestManagerHistogramsConcurrent(t *testing.T) {
	m := New(3, time.Minute)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 1; i <= 250; i++ { // RecordTTFT drops 0ms samples by design
				m.RecordSuccess(fmt.Sprintf("dep-%d", g%2), time.Duration(i)*time.Millisecond)
				m.RecordTTFT(fmt.Sprintf("dep-%d", g%2), time.Duration(i)*time.Millisecond)
			}
		}(g)
	}
	wg.Wait()
	lat, ttft, ok := m.Histograms("dep-0")
	if !ok || lat.Count != 1000 || ttft.Count != 1000 {
		t.Fatalf("dep-0: ok=%v latency=%d ttft=%d, want 1000/1000", ok, lat.Count, ttft.Count)
	}
}

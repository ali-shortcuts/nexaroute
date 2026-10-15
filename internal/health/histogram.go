package health

import "sort"

// LatencyHistogramBoundsMS are the upper bounds (milliseconds) of the
// per-deployment latency and TTFT histogram buckets, with an implicit
// +Inf bucket at the end. The geometric spread covers fast cache hits
// through minute-scale long generations without exploding cardinality:
// exactly len(bounds)+1 buckets per deployment and metric.
var LatencyHistogramBoundsMS = []float64{
	5, 10, 25, 50, 100, 250, 500, 1000, 2000, 4000, 8000, 16000, 32000, 64000,
}

// Histogram is a fixed-bucket latency histogram. The zero value is
// ready to use. Not safe for concurrent use; the Manager serializes
// access under its own mutex.
type Histogram struct {
	// Buckets[i] counts observations in (bounds[i-1], bounds[i]];
	// the final element is the +Inf bucket.
	Buckets []uint64
	SumMS   float64
	Count   uint64
}

func (h *Histogram) Observe(ms float64) {
	if ms < 0 {
		ms = 0
	}
	if h.Buckets == nil {
		h.Buckets = make([]uint64, len(LatencyHistogramBoundsMS)+1)
	}
	i := sort.Search(len(LatencyHistogramBoundsMS), func(i int) bool {
		return ms <= LatencyHistogramBoundsMS[i]
	})
	h.Buckets[i]++
	h.SumMS += ms
	h.Count++
}

// Cumulative returns per-bound cumulative counts in Prometheus
// histogram order (one entry per finite bound, then +Inf == Count).
func (h Histogram) Cumulative() []uint64 {
	out := make([]uint64, len(LatencyHistogramBoundsMS)+1)
	var run uint64
	for i := 0; i < len(LatencyHistogramBoundsMS); i++ {
		if i < len(h.Buckets) {
			run += h.Buckets[i]
		}
		out[i] = run
	}
	out[len(LatencyHistogramBoundsMS)] = h.Count
	return out
}

// deploymentHistograms groups the histograms kept per deployment.
type deploymentHistograms struct {
	Latency Histogram
	TTFT    Histogram
}

package httpapi

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Production-telemetry ingestion (PR-B).
//
// The evaluation plane stays admin-only and never becomes a routing input:
// ingestion only writes scorecards with provenance=production_telemetry
// through the existing scorecards.FromTelemetry constructor, from aggregated
// operational values only (success/failure counts, latency/TTFT aggregates).
// No prompt/response content and no per-request user data ever enter the
// plane: Observe takes only deployment identity plus numeric aggregates.
//
// Ingestion is opt-in via evaluation.telemetry_ingest_enabled and off by
// default. All methods are best-effort: they never return errors to the data
// plane and never change routing or health state.

// maxTelemetrySamplesPerDeployment bounds the latency rings so one
// deployment cannot grow memory without limit.
const maxTelemetrySamplesPerDeployment = 256

// maxTelemetryDeployments bounds the aggregator cardinality.
const maxTelemetryDeployments = 4096

type telemetrySample struct {
	success   bool
	latencyMS float64
	ttftMS    float64
}

type telemetryBucket struct {
	deploymentID string
	providerID   string
	model        string
	successes    int64
	failures     int64
	latencies    []float64
	ttfts        []float64
	lastSeen     time.Time
}

type telemetryAggregator struct {
	mu      sync.Mutex
	buckets map[string]*telemetryBucket
}

func newTelemetryAggregator() *telemetryAggregator {
	return &telemetryAggregator{buckets: map[string]*telemetryBucket{}}
}

// observe records one completed gateway request using aggregated operational
// values only. deploymentID is required; provider/model are attribution only.
// Negative timings are clamped to zero; empty deployment ids are dropped.
func (a *telemetryAggregator) observe(deploymentID, providerID, model string, success bool, latencyMS, ttftMS float64, now time.Time) {
	if deploymentID == "" || len(deploymentID) > 512 {
		return
	}
	if len(providerID) > 256 || len(model) > 1024 {
		return
	}
	if latencyMS < 0 {
		latencyMS = 0
	}
	if ttftMS < 0 {
		ttftMS = 0
	}
	if latencyMS > 600000 {
		latencyMS = 600000
	}
	if ttftMS > 600000 {
		ttftMS = 600000
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.buckets == nil {
		a.buckets = map[string]*telemetryBucket{}
	}
	b, ok := a.buckets[deploymentID]
	if !ok {
		if len(a.buckets) >= maxTelemetryDeployments {
			return
		}
		b = &telemetryBucket{deploymentID: deploymentID}
		a.buckets[deploymentID] = b
	}
	if providerID != "" {
		b.providerID = providerID
	}
	if model != "" {
		b.model = model
	}
	if success {
		b.successes++
	} else {
		b.failures++
	}
	if latencyMS > 0 {
		b.latencies = append(b.latencies, latencyMS)
		if len(b.latencies) > maxTelemetrySamplesPerDeployment {
			b.latencies = append([]float64(nil), b.latencies[len(b.latencies)-maxTelemetrySamplesPerDeployment:]...)
		}
	}
	if ttftMS > 0 {
		b.ttfts = append(b.ttfts, ttftMS)
		if len(b.ttfts) > maxTelemetrySamplesPerDeployment {
			b.ttfts = append([]float64(nil), b.ttfts[len(b.ttfts)-maxTelemetrySamplesPerDeployment:]...)
		}
	}
	b.lastSeen = now
}

func (a *telemetryAggregator) deployments() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.buckets)
}

func (a *telemetryAggregator) retain(valid map[string]struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.buckets {
		if _, ok := valid[id]; !ok {
			delete(a.buckets, id)
		}
	}
}

// snapshot returns a deterministic copy of the buckets for flushing.
func (a *telemetryAggregator) snapshot() []*telemetryBucket {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*telemetryBucket, 0, len(a.buckets))
	for _, b := range a.buckets {
		cp := &telemetryBucket{
			deploymentID: b.deploymentID,
			providerID:   b.providerID,
			model:        b.model,
			successes:    b.successes,
			failures:     b.failures,
			lastSeen:     b.lastSeen,
		}
		cp.latencies = append([]float64(nil), b.latencies...)
		cp.ttfts = append([]float64(nil), b.ttfts...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].deploymentID < out[j].deploymentID })
	return out
}

func percentileCopy(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	if p <= 0 {
		return cp[0]
	}
	if p >= 1 {
		return cp[len(cp)-1]
	}
	// Nearest-rank, matching eval.percentile.
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func meanCopy(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// flush materializes production_telemetry scorecards for buckets with enough
// aggregated samples, using only the existing FromTelemetry constructor. It
// returns how many scorecards were written.
func (p *evaluationPlane) flushTelemetry(now time.Time) int {
	cfg := p.config()
	if !cfg.Enabled || !cfg.TelemetryIngestEnabled {
		return 0
	}
	minSamples := cfg.TelemetryMinSamples
	if minSamples < 1 {
		minSamples = 1
	}
	if p.telemetry == nil {
		return 0
	}
	written := 0
	for _, b := range p.telemetry.snapshot() {
		total := b.successes + b.failures
		if total < int64(minSamples) {
			continue
		}
		if total < int64(scorecards.MinMeasuredSamples) {
			continue
		}
		ev := scorecards.TelemetryEvidence{
			DeploymentID: b.deploymentID,
			ProviderID:   b.providerID,
			Model:        b.model,
			ObservedAt:   now.UTC(),
			WindowLabel:  "gateway",
			SampleCount:  int(total),
		}
		if total > 0 {
			ev.SuccessRate = float64(b.successes) / float64(total)
			ev.FailureRate = float64(b.failures) / float64(total)
		}
		if len(b.latencies) > 0 {
			ev.LatencyP95MS = percentileCopy(b.latencies, 0.95)
			ev.LatencyP50MS = percentileCopy(b.latencies, 0.50)
			ev.LatencyTargetMS = cfg.LatencyTargetMS
		}
		if len(b.ttfts) > 0 {
			ev.TTFTMS = meanCopy(b.ttfts)
			ev.TTFTTargetMS = cfg.TTFTTargetMS
		}
		sc, err := scorecards.FromTelemetry(ev)
		if err != nil {
			continue
		}
		if _, err := p.reg.Upsert(sc); err != nil {
			continue
		}
		written++
	}
	if written > 0 {
		p.telemetryIngested.Add(uint64(written))
		p.persistBestEffort()
	}
	return written
}

// observeProductionTelemetry records one completed gateway request as
// aggregated operational values only. It is a no-op unless ingestion is
// opt-in enabled, and it never affects routing or health.
func (s *Server) observeProductionTelemetry(deploymentID, providerID, model string, success bool, latencyMS, ttftMS float64) {
	plane := s.evaluationSnapshot()
	if plane == nil {
		return
	}
	cfg := plane.config()
	if !cfg.Enabled || !cfg.TelemetryIngestEnabled {
		return
	}
	if plane.telemetry == nil {
		return
	}
	plane.telemetry.observe(deploymentID, providerID, model, success, latencyMS, ttftMS, time.Now().UTC())
	plane.flushTelemetry(time.Now().UTC())
}

// observeCompletedTelemetry is the data-plane hook: one completed gateway
// request contributes one aggregated sample (success/failure + numeric
// timings only). Model attribution is resolved best-effort from the routing
// table; an empty model is fine. Never affects routing or health.
func (s *Server) observeCompletedTelemetry(deploymentID, providerID string, success bool, latency, ttft time.Duration) {
	plane := s.evaluationSnapshot()
	if plane == nil {
		return
	}
	cfg := plane.config()
	if !cfg.Enabled || !cfg.TelemetryIngestEnabled {
		return
	}
	model := ""
	s.runtimeMu.RLock()
	rt := s.rt
	s.runtimeMu.RUnlock()
	if rt != nil {
		for _, d := range rt.All() {
			if d.ID == deploymentID {
				model = d.Model
				break
			}
		}
	}
	latencyMS := float64(latency.Milliseconds())
	ttftMS := float64(ttft.Milliseconds())
	if plane.telemetry == nil {
		return
	}
	plane.telemetry.observe(deploymentID, providerID, model, success, latencyMS, ttftMS, time.Now().UTC())
	plane.flushTelemetry(time.Now().UTC())
}

// syncProductionTelemetry materializes pending telemetry aggregates into
// production_telemetry scorecards. It is best-effort and never fails a
// request.
func (s *Server) syncProductionTelemetry() {
	plane := s.evaluationSnapshot()
	if plane == nil {
		return
	}
	plane.flushTelemetry(time.Now().UTC())
}

// observeTelemetryTTFT records a time-to-first-token sample for streaming
// traffic. TTFT is a numeric aggregate only; it never carries content.
func (s *Server) observeTelemetryTTFT(deploymentID string, d time.Duration) {
	plane := s.evaluationSnapshot()
	if plane == nil {
		return
	}
	cfg := plane.config()
	if !cfg.Enabled || !cfg.TelemetryIngestEnabled {
		return
	}
	if plane.telemetry == nil {
		return
	}
	ms := float64(d.Milliseconds())
	if ms < 0 {
		ms = 0
	}
	if ms > 600000 {
		ms = 600000
	}
	if ms <= 0 {
		return
	}
	plane.telemetry.mu.Lock()
	if plane.telemetry.buckets == nil {
		plane.telemetry.buckets = map[string]*telemetryBucket{}
	}
	b, ok := plane.telemetry.buckets[deploymentID]
	if !ok {
		if len(plane.telemetry.buckets) >= maxTelemetryDeployments {
			plane.telemetry.mu.Unlock()
			return
		}
		b = &telemetryBucket{deploymentID: deploymentID}
		plane.telemetry.buckets[deploymentID] = b
	}
	b.ttfts = append(b.ttfts, ms)
	if len(b.ttfts) > maxTelemetrySamplesPerDeployment {
		b.ttfts = append([]float64(nil), b.ttfts[len(b.ttfts)-maxTelemetrySamplesPerDeployment:]...)
	}
	b.lastSeen = time.Now().UTC()
	plane.telemetry.mu.Unlock()
}

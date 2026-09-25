package decision

import (
	"sync/atomic"
	"time"
)

// Metrics tracks decision plane observability with bounded cardinality.
type Metrics struct {
	decisionsTotal atomic.Int64
	abstainsTotal  atomic.Int64
	failuresTotal  atomic.Int64
	timeoutsTotal  atomic.Int64
	invalidTotal   atomic.Int64
	selectTotal    atomic.Int64
	rankTotal      atomic.Int64
	latencySum     atomic.Int64 // nanos sum
	latencyCount   atomic.Int64
	offModeTotal   atomic.Int64
}

// Record records a decision outcome.
func (m *Metrics) Record(result DecisionResult, err error, timedOut bool, offMode bool) {
	if offMode {
		m.offModeTotal.Add(1)
		return
	}
	m.decisionsTotal.Add(1)
	if timedOut {
		m.timeoutsTotal.Add(1)
	}
	if err != nil {
		m.failuresTotal.Add(1)
	}
	if result.IsAbstain() {
		m.abstainsTotal.Add(1)
	}
	switch result.Action {
	case ActionSelect:
		m.selectTotal.Add(1)
	case ActionRank:
		m.rankTotal.Add(1)
	}
	if result.Latency > 0 {
		m.latencySum.Add(int64(result.Latency))
		m.latencyCount.Add(1)
	}
	// Invalid tracked via reason codes
	for _, rc := range result.ReasonCodes {
		if rc == ReasonInvalidResult || rc == ReasonValidationFailed {
			m.invalidTotal.Add(1)
			break
		}
	}
}

// Snapshot returns a map for metrics endpoint / admin.
func (m *Metrics) Snapshot() map[string]int64 {
	latSum := m.latencySum.Load()
	latCount := m.latencyCount.Load()
	avgMs := int64(0)
	if latCount > 0 {
		avgMs = (latSum / latCount) / int64(time.Millisecond)
	}
	return map[string]int64{
		"decisions_total": m.decisionsTotal.Load(),
		"abstains_total":  m.abstainsTotal.Load(),
		"failures_total":  m.failuresTotal.Load(),
		"timeouts_total":  m.timeoutsTotal.Load(),
		"invalid_total":   m.invalidTotal.Load(),
		"select_total":    m.selectTotal.Load(),
		"rank_total":      m.rankTotal.Load(),
		"latency_avg_ms":  avgMs,
		"latency_count":   latCount,
		"off_mode_total":  m.offModeTotal.Load(),
	}
}

// Global metrics instance (process-wide, like other metrics in httpapi).
var GlobalMetrics = &Metrics{}

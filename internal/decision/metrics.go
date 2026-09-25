package decision

import (
	"sync/atomic"
	"time"
)

// Metrics tracks decision plane observability with bounded cardinality.
// Each Server/Orchestrator owns its Metrics instance to avoid cross-server contamination.
// No arbitrary provider text, deployment ID, request ID, VE ID, session ID becomes a label.
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
// Outcome labels come from fixed internal enums only, not arbitrary provider-returned text.
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
	// IsAbstain after strict validation: Action == ABSTAIN
	if result.Action == ActionAbstain {
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
	// Invalid tracked via reason codes — only canonical codes
	for _, rc := range result.ReasonCodes {
		if rc == ReasonInvalidResult || rc == ReasonValidationFailed {
			m.invalidTotal.Add(1)
			break
		}
	}
}

// Snapshot returns a map for metrics endpoint / admin.
// Keys are fixed enums, not per-request.
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

// GlobalMetrics fallback only if truly necessary internally (e.g. tests that don't create server).
// Preferred: each Server/Orchestrator owns its Metrics.
var GlobalMetrics = &Metrics{}

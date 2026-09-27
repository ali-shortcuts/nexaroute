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
	// Phase F: external provider metrics with bounded labels type=jev outcome=selected|error|timeout|invalid|unavailable
	externalTotal            atomic.Int64
	externalSelected         atomic.Int64
	externalError            atomic.Int64
	externalTimeout          atomic.Int64
	externalInvalid          atomic.Int64
	externalUnavailable      atomic.Int64
	externalRequestTooLarge  atomic.Int64
	externalResponseTooLarge atomic.Int64
	externalLatencySum       atomic.Int64
	externalLatencyCount     atomic.Int64
	// Phase G: chain metrics
	chainSelected          atomic.Int64
	chainExhausted         atomic.Int64
	chainBudgetExhausted   atomic.Int64
	chainDeadlineExhausted atomic.Int64
	chainAffinity          atomic.Int64
	chainStepSelected      atomic.Int64
	chainStepAbstained     atomic.Int64
	chainStepError         atomic.Int64
	chainStepTimeout       atomic.Int64
	chainStepInvalid       atomic.Int64
	chainStepUnavailable   atomic.Int64
	chainStepCooldown      atomic.Int64
	chainStepSkippedBudget atomic.Int64
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

	// Phase F: external metrics
	isExternal := false
	for _, rc := range result.ReasonCodes {
		switch rc {
		case ReasonExternalSelected, ReasonExternalAbstained, ReasonExternalTimeout, ReasonExternalHTTPError,
			ReasonExternalInvalidResponse, ReasonExternalUnknownCandidate, ReasonExternalRequestTooLarge,
			ReasonExternalResponseTooLarge, ReasonExternalProviderUnavailable:
			isExternal = true
		}
	}
	if isExternal {
		m.externalTotal.Add(1)
		if result.Latency > 0 {
			m.externalLatencySum.Add(int64(result.Latency))
			m.externalLatencyCount.Add(1)
		}
		for _, rc := range result.ReasonCodes {
			switch rc {
			case ReasonExternalSelected:
				m.externalSelected.Add(1)
			case ReasonExternalHTTPError, ReasonProviderError:
				m.externalError.Add(1)
			case ReasonExternalTimeout, ReasonTimeout:
				m.externalTimeout.Add(1)
			case ReasonExternalInvalidResponse, ReasonExternalUnknownCandidate, ReasonInvalidResult:
				m.externalInvalid.Add(1)
			case ReasonExternalProviderUnavailable, ReasonProviderUnhealthy:
				m.externalUnavailable.Add(1)
			case ReasonExternalRequestTooLarge:
				m.externalRequestTooLarge.Add(1)
			case ReasonExternalResponseTooLarge:
				m.externalResponseTooLarge.Add(1)
			}
		}
	}
}

func (m *Metrics) RecordChainOutcome(outcome string) {
	switch outcome {
	case "selected":
		m.chainSelected.Add(1)
	case "exhausted":
		m.chainExhausted.Add(1)
	case "budget_exhausted":
		m.chainBudgetExhausted.Add(1)
	case "deadline_exhausted":
		m.chainDeadlineExhausted.Add(1)
	case "affinity_preserved":
		m.chainAffinity.Add(1)
	}
}

func (m *Metrics) RecordChainStep(providerType, outcome string) {
	// providerType bounded jev|policy|local (fallback to unknown not counted)
	// outcome bounded selected|abstain|error|timeout|invalid|unavailable|cooldown|skipped_budget
	switch outcome {
	case "selected":
		m.chainStepSelected.Add(1)
	case "abstained":
		m.chainStepAbstained.Add(1)
	case "error":
		m.chainStepError.Add(1)
	case "timeout":
		m.chainStepTimeout.Add(1)
	case "invalid":
		m.chainStepInvalid.Add(1)
	case "unavailable":
		m.chainStepUnavailable.Add(1)
	case "cooldown":
		m.chainStepCooldown.Add(1)
	case "skipped_budget":
		m.chainStepSkippedBudget.Add(1)
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
	extLatSum := m.externalLatencySum.Load()
	extLatCount := m.externalLatencyCount.Load()
	extAvgMs := int64(0)
	if extLatCount > 0 {
		extAvgMs = (extLatSum / extLatCount) / int64(time.Millisecond)
	}
	return map[string]int64{
		"decisions_total":             m.decisionsTotal.Load(),
		"abstains_total":              m.abstainsTotal.Load(),
		"failures_total":              m.failuresTotal.Load(),
		"timeouts_total":              m.timeoutsTotal.Load(),
		"invalid_total":               m.invalidTotal.Load(),
		"select_total":                m.selectTotal.Load(),
		"rank_total":                  m.rankTotal.Load(),
		"latency_avg_ms":              avgMs,
		"latency_count":               latCount,
		"off_mode_total":              m.offModeTotal.Load(),
		"external_total":              m.externalTotal.Load(),
		"external_selected":           m.externalSelected.Load(),
		"external_error":              m.externalError.Load(),
		"external_timeout":            m.externalTimeout.Load(),
		"external_invalid":            m.externalInvalid.Load(),
		"external_unavailable":        m.externalUnavailable.Load(),
		"external_request_too_large":  m.externalRequestTooLarge.Load(),
		"external_response_too_large": m.externalResponseTooLarge.Load(),
		"external_latency_avg_ms":     extAvgMs,
		"external_latency_count":      extLatCount,
		"chain_selected":              m.chainSelected.Load(),
		"chain_exhausted":             m.chainExhausted.Load(),
		"chain_budget_exhausted":      m.chainBudgetExhausted.Load(),
		"chain_deadline_exhausted":    m.chainDeadlineExhausted.Load(),
		"chain_affinity_preserved":    m.chainAffinity.Load(),
		"chain_step_selected":         m.chainStepSelected.Load(),
		"chain_step_abstained":        m.chainStepAbstained.Load(),
		"chain_step_error":            m.chainStepError.Load(),
		"chain_step_timeout":          m.chainStepTimeout.Load(),
		"chain_step_invalid":          m.chainStepInvalid.Load(),
		"chain_step_unavailable":      m.chainStepUnavailable.Load(),
		"chain_step_cooldown":         m.chainStepCooldown.Load(),
		"chain_step_skipped_budget":   m.chainStepSkippedBudget.Load(),
	}
}

// GlobalMetrics fallback only if truly necessary internally (e.g. tests that don't create server).
// Preferred: each Server/Orchestrator owns its Metrics.
var GlobalMetrics = &Metrics{}

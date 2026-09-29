package httpapi

import (
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// DeploymentHealthView is an additive-only admin presentation of
// health.State. It embeds the full internal state (so every existing JSON
// field is preserved byte-for-byte) and adds only the missing per-deployment
// fields required by the health API contract:
//
//	circuit_state (CLOSED/OPEN/HALF_OPEN),
//	state (UNKNOWN/CHECKING/HEALTHY/DEGRADED/RECOVERING/COOLDOWN/DISABLED),
//	average_latency (alias of ewma_latency_ms),
//	recent_error_rate (alias of ewma_failure_rate).
//
// last_success, last_failure, consecutive_failures and cooldown_until already
// exist on health.State and are preserved unchanged.
type DeploymentHealthView struct {
	health.State
	CircuitState    string  `json:"circuit_state"`
	RichState       string  `json:"state"`
	AverageLatency  float64 `json:"average_latency"`
	RecentErrorRate float64 `json:"recent_error_rate"`
}

// enrichHealthForAdmin derives the additive admin view without mutating the
// internal health states and without changing how the decision engine
// consumes health.State.
func enrichHealthForAdmin(states []health.State) []DeploymentHealthView {
	out := make([]DeploymentHealthView, 0, len(states))
	for _, st := range states {
		out = append(out, DeploymentHealthView{
			State:           st,
			CircuitState:    health.CircuitStateFor(st.Status),
			RichState:       health.RichStateFor(st),
			AverageLatency:  health.AverageLatencyMS(st),
			RecentErrorRate: health.RecentErrorRate(st),
		})
	}
	return out
}

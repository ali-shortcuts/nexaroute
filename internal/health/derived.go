package health

// CircuitStateFor maps the internal deployment status onto the admin-facing
// circuit breaker vocabulary without changing how the decision engine
// consumes Status. CLOSED = traffic flows, OPEN = hard cooldown/retired,
// HALF_OPEN = probing recovery.
func CircuitStateFor(s Status) string {
	switch s {
	case Cooldown, Retired:
		return "OPEN"
	case HalfOpen:
		return "HALF_OPEN"
	default:
		return "CLOSED"
	}
}

// RichStateFor maps the internal deployment status onto the admin-facing
// rich state vocabulary: UNKNOWN/CHECKING/HEALTHY/DEGRADED/RECOVERING/
// COOLDOWN/DISABLED. The internal Status enum is unchanged; this is a pure
// presentation derivation so the decision engine keeps consuming Status.
func RichStateFor(st State) string {
	switch st.Status {
	case Healthy:
		return "HEALTHY"
	case Degraded:
		return "DEGRADED"
	case HalfOpen:
		return "RECOVERING"
	case Cooldown:
		return "COOLDOWN"
	case Retired:
		return "DISABLED"
	default:
		// Unknown splits into UNKNOWN (never checked) vs CHECKING
		// (a check has been observed but no verdict yet).
		if st.LastChecked.IsZero() {
			return "UNKNOWN"
		}
		return "CHECKING"
	}
}

// AverageLatencyMS exposes the EWMA latency under the admin API field name
// average_latency. Pure alias; no change to internal accounting.
func AverageLatencyMS(st State) float64 { return st.EWMALatencyMS }

// RecentErrorRate exposes the EWMA failure rate under the admin API field
// name recent_error_rate. Pure alias; no change to internal accounting.
func RecentErrorRate(st State) float64 { return st.EWMAFailureRate }

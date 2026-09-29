package httpapi

import (
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

// Production log event kinds. These are the only structured production log
// events emitted on the data-plane request path and the recovery path.
// Legacy diagnostic bus kinds (route_ok, route_fail, failover, probe_*,
// recovery_*, ...) are retained for backward compatibility with existing
// dashboards/tests, but per-attempt noisy events (route_attempt, route_skip)
// were removed so production logs carry only lifecycle transitions.
const (
	ProductionEventModelHealthy        = "model_healthy"
	ProductionEventModelFailed         = "model_failed"
	ProductionEventModelRecovered      = "model_recovered"
	ProductionEventModelCooldown       = "model_cooldown"
	ProductionEventProviderRateLimited = "provider_rate_limited"
	ProductionEventRouteChanged        = "route_changed"
	ProductionEventRequestFailover     = "request_failover"
	ProductionEventRecoveryFailed      = "recovery_failed"
)

// productionEventKinds lists the exact production set for tests/docs.
var productionEventKinds = []string{
	ProductionEventModelHealthy,
	ProductionEventModelFailed,
	ProductionEventModelRecovered,
	ProductionEventModelCooldown,
	ProductionEventProviderRateLimited,
	ProductionEventRouteChanged,
	ProductionEventRequestFailover,
	ProductionEventRecoveryFailed,
}

// emitProductionEvent records one structured production lifecycle event on
// the bounded event bus and mirrors it to stderr in a greppable,
// single-line form. Only the eight kinds above ever flow through here.
func (s *Server) emitProductionEvent(requestID, kind, deployment, message string, extra events.Event) {
	ev := extra
	ev.RequestID = requestID
	ev.Kind = kind
	ev.Deployment = deployment
	if ev.Message == "" {
		ev.Message = message
	}
	s.bus.Add(ev)
	if s.log != nil {
		s.log.Printf("event=%s deployment=%s request_id=%s msg=%q", kind, deployment, requestID, message)
	}
}

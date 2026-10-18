package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

const (
	defaultActivityRequests = 100
	maxActivityRequests     = 256
	// activityStaleAfter marks an unterminated request as stale once no
	// lifecycle event has arrived for this long. Stale is never rendered as
	// live; it is a distinct, explicit state.
	activityStaleAfter = 5 * time.Minute
)

// Best-effort attempt kinds for the lifecycle contract.
const (
	attemptKindPrimary = "primary"
	attemptKindRetry   = "retry"
	attemptKindHedge   = "hedge"
)

// emitRequestAccepted records that an authenticated, validated data-plane
// request passed admission and is entering routing. It carries only request
// shape metadata (adapter/protocol, requested model, streaming) and never the
// prompt, completion or credentials.
func (s *Server) emitRequestAccepted(r *http.Request, protocol, model string, streaming bool) {
	reqID := r.Header.Get("x-request-id")
	if reqID == "" {
		return
	}
	s.bus.Add(events.Event{
		RequestID:      reqID,
		CorrelationID:  reqID,
		Kind:           "request_accepted",
		Phase:          events.PhaseAccepted,
		Adapter:        protocol,
		RequestedModel: model,
		Streaming:      streaming,
		CostBasis:      "estimate",
	})
}

// emitRouteResolved records the resolved public model / virtual endpoint /
// route profile and the first physical candidate. It is redacted routing
// metadata only.
func (s *Server) emitRouteResolved(r *http.Request, protocol string, resolved *route.ResolvedRoute, candidates []router.Scored) {
	reqID := r.Header.Get("x-request-id")
	if reqID == "" {
		return
	}
	ev := events.Event{
		RequestID:     reqID,
		CorrelationID: reqID,
		Kind:          "route_resolved",
		Phase:         events.PhaseResolved,
		Adapter:       protocol,
	}
	if resolved != nil {
		ev.VirtualEndpoint = resolved.VirtualEndpointID
		ev.PublicModel = resolved.PublicModel
		ev.RouteProfile = resolved.RouteProfileID
		ev.Pool = resolved.PrimaryPoolID
	}
	if len(candidates) > 0 {
		ev.ResolvedModel = candidates[0].Deployment.Model
	}
	s.bus.Add(ev)
}

// emitRouteAttempt records one upstream attempt with provider-qualified
// deployment, protocol adapter and attempt ordinal.
func (s *Server) emitRouteAttempt(requestID, protocol, requestedModel string, attempt int, c router.Scored, kind string) {
	if requestID == "" {
		return
	}
	s.bus.Add(events.Event{
		RequestID:      requestID,
		CorrelationID:  requestID,
		Kind:           "route_attempt",
		Phase:          events.PhaseAttempt,
		Attempt:        attempt,
		AttemptKind:    kind,
		Deployment:     c.Deployment.ID,
		Provider:       c.Deployment.ProviderID,
		Adapter:        protocol,
		RequestedModel: requestedModel,
	})
}

// emitStreamCommitted records that client-visible stream bytes were committed
// for a deployment, together with the measured time to first upstream byte.
// No event is emitted after commitment unless the stream subsequently fails;
// NexaRoute never fakes mid-stream failover.
func (s *Server) emitStreamCommitted(requestID, deployment, provider, protocol string, ttft time.Duration) {
	if requestID == "" {
		return
	}
	s.bus.Add(events.Event{
		RequestID:       requestID,
		CorrelationID:   requestID,
		Kind:            "stream_committed",
		Phase:           events.PhaseCommitted,
		Deployment:      deployment,
		Provider:        provider,
		Adapter:         protocol,
		TTFTMS:          ttft.Milliseconds(),
		StreamCommitted: true,
	})
}

// emitRequestFinished records the single terminal lifecycle event for a
// data-plane request. It is emitted from the HTTP middleware so exactly one
// terminal is produced no matter which handler branch returned.
func (s *Server) emitRequestFinished(requestID string, status int, duration time.Duration, terminal string) {
	if requestID == "" {
		return
	}
	s.bus.Add(events.Event{
		RequestID:     requestID,
		CorrelationID: requestID,
		Kind:          "request_finished",
		Phase:         events.PhaseFinished,
		Terminal:      terminal,
		StatusCode:    status,
		LatencyMS:     duration.Milliseconds(),
	})
}

type activityCounts struct {
	Active    int `json:"active"`
	Stale     int `json:"stale"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	Total     int `json:"total"`
}

type activityResponse struct {
	GeneratedAt       time.Time                 `json:"generated_at"`
	StaleAfterSeconds int                       `json:"stale_after_seconds"`
	Epoch             uint64                    `json:"epoch"`
	Cursor            uint64                    `json:"cursor"`
	Counts            activityCounts            `json:"counts"`
	Requests          []events.RequestLifecycle `json:"requests"`
}

func summarizeCounts(list []events.RequestLifecycle) activityCounts {
	var c activityCounts
	for _, lc := range list {
		c.Total++
		switch lc.State {
		case events.StateActive:
			c.Active++
		case events.StateStale:
			c.Stale++
		case events.TerminalSucceeded:
			c.Succeeded++
		case events.TerminalFailed:
			c.Failed++
		case events.TerminalCancelled:
			c.Cancelled++
		}
	}
	return c
}

// adminActivityRequests exposes the bounded live request lifecycle feed. It is
// derived from the same bounded event bus that backs the operator SSE stream,
// so it adds no competing telemetry store and can never grow without bound.
// Authorization is enforced by the admin matrix (ReadAudit) before this runs.
func (s *Server) adminActivityRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := defaultActivityRequests
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxActivityRequests {
			errorJSON(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(maxActivityRequests))
			return
		}
		limit = parsed
	}
	staleAfter := activityStaleAfter
	if raw := r.URL.Query().Get("window"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs < 1 || secs > 3600 {
			errorJSON(w, http.StatusBadRequest, "window must be between 1 and 3600 seconds")
			return
		}
		staleAfter = time.Duration(secs) * time.Second
	}
	lifecycles := events.Summarize(s.bus.SnapshotLimit(0), events.SummarizeOptions{
		Now:         time.Now(),
		StaleAfter:  staleAfter,
		MaxRequests: limit,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	resp := activityResponse{
		GeneratedAt:       time.Now().UTC(),
		StaleAfterSeconds: int(staleAfter.Seconds()),
		Epoch:             s.bus.Epoch(),
		Cursor:            s.bus.Cursor(),
		Counts:            summarizeCounts(lifecycles),
		Requests:          lifecycles,
	}
	if resp.Requests == nil {
		resp.Requests = []events.RequestLifecycle{}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

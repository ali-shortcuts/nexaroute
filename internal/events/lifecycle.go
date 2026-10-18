package events

import (
	"sort"
	"time"
)

// Request lifecycle phases. A data-plane request emits exactly one accepted
// phase, zero or more resolved/attempt/committed phases, and exactly one
// finished phase carrying a terminal outcome.
const (
	PhaseAccepted  = "accepted"
	PhaseResolved  = "resolved"
	PhaseAttempt   = "attempt"
	PhaseCommitted = "committed"
	PhaseFinished  = "finished"
)

// Terminal outcomes for a request lifecycle.
const (
	TerminalSucceeded = "succeeded"
	TerminalFailed    = "failed"
	TerminalCancelled = "cancelled"
)

// Lifecycle states surfaced to the control plane. "active" and "stale" are the
// only non-terminal states; a stale request is never presented as live.
const (
	StateActive = "active"
	StateStale  = "stale"
)

// lifecycleKinds are the request-correlated event kinds that participate in a
// request lifecycle even when no explicit phase is set. The aggregator ignores
// unrelated bus traffic so administrative or health events without a request
// id never form spurious lifecycles.
var lifecycleKinds = map[string]bool{
	"request_accepted":      true,
	"route_resolved":        true,
	"route_attempt":         true,
	"hedge_launch":          true,
	"route_ok":              true,
	"route_fail":            true,
	"route_timeout":         true,
	"failover":              true,
	"client_disconnect":     true,
	"stream_fail":           true,
	"stream_fail_precommit": true,
	"response_decode_fail":  true,
	"candidate_exhausted":   true,
	"request_finished":      true,
	"gateway_overloaded":    true,
	"cache_hit":             true,
	"semantic_cache_hit":    true,
	"model_retired":         true,
	"model_unavailable":     true,
	// Decision-plane events participate in the request lifecycle so the
	// applied policy id and version are recorded in the request trace even
	// though they carry no upstream attempt.
	"decision_ok":       true,
	"decision_abstain":  true,
	"decision_timeout":  true,
	"decision_rejected": true,
	"decision_fail":     true,
	"decision_shadow":   true,
}

// terminalKinds maps legacy request-correlated terminal event kinds to a
// terminal outcome when no explicit Terminal field was set.
var terminalKinds = map[string]string{
	"route_ok":            TerminalSucceeded,
	"candidate_exhausted": TerminalFailed,
	"stream_fail":         TerminalFailed,
	"client_disconnect":   TerminalCancelled,
}

// LifecycleAttempt is one upstream attempt inside a request lifecycle. It is
// routing metadata only: no prompts, completions or credentials.
type LifecycleAttempt struct {
	Index        int       `json:"index"`
	Attempt      int       `json:"attempt,omitempty"`
	Deployment   string    `json:"deployment,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Adapter      string    `json:"adapter,omitempty"`
	Kind         string    `json:"attempt_kind,omitempty"`
	Outcome      string    `json:"outcome,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	LatencyMS    int64     `json:"latency_ms,omitempty"`
	TTFTMS       int64     `json:"ttft_ms,omitempty"`
	StatusCode   int       `json:"status_code,omitempty"`
	ErrorType    string    `json:"error_type,omitempty"`
	Retryable    bool      `json:"retryable,omitempty"`
	RetryAfterMS int64     `json:"retry_after_ms,omitempty"`
	Committed    bool      `json:"committed,omitempty"`
	// FailoverClass is the bounded classification of the trigger that moved
	// this attempt's traffic to the next candidate.
	FailoverClass string `json:"failover_class,omitempty"`
}

// RequestLifecycle is the bounded, privacy-safe observable lifecycle of one
// data-plane request. Events that share a correlation id (for example a
// hedge or a later session turn) are grouped together.
type RequestLifecycle struct {
	RequestID       string `json:"request_id"`
	CorrelationID   string `json:"correlation_id,omitempty"`
	ParentRequestID string `json:"parent_request_id,omitempty"`
	RequestedModel  string `json:"requested_model,omitempty"`
	PublicModel     string `json:"public_model,omitempty"`
	ResolvedModel   string `json:"resolved_model,omitempty"`
	RouteProfile    string `json:"route_profile,omitempty"`
	VirtualEndpoint string `json:"virtual_endpoint,omitempty"`
	Pool            string `json:"pool,omitempty"`
	Adapter         string `json:"adapter,omitempty"`
	Streaming       bool   `json:"streaming,omitempty"`
	// DecisionPolicyID and DecisionPolicyVersion record the routing policy
	// applied to this request, when a decision policy participated. Bounded
	// IDs and version labels only — never prompts or credentials.
	DecisionPolicyID      string `json:"decision_policy_id,omitempty"`
	DecisionPolicyVersion string `json:"decision_policy_version,omitempty"`
	// FailoverClass classifies the most recent fallback trigger, when any.
	FailoverClass string             `json:"failover_class,omitempty"`
	State         string             `json:"state"`
	Terminal      string             `json:"terminal,omitempty"`
	StartedAt     time.Time          `json:"started_at"`
	LastEventAt   time.Time          `json:"last_event_at"`
	Attempts      []LifecycleAttempt `json:"attempts"`
	AttemptCount  int                `json:"attempt_count"`
	FailoverCount int                `json:"failover_count"`
	TTFTMS        int64              `json:"ttft_ms,omitempty"`
	LatencyMS     int64              `json:"latency_ms,omitempty"`
	StatusCode    int                `json:"status_code,omitempty"`
	CostBasis     string             `json:"cost_basis,omitempty"`
	InputTokens   int                `json:"input_tokens,omitempty"`
	OutputTokens  int                `json:"output_tokens,omitempty"`
}

// SummarizeOptions bounds and time-scopes lifecycle aggregation.
type SummarizeOptions struct {
	// Now is the reference time for staleness. Defaults to time.Now().
	Now time.Time
	// StaleAfter marks an unterminated lifecycle as stale when no event has
	// arrived within the window. Defaults to 5 minutes.
	StaleAfter time.Duration
	// MaxRequests caps the number of lifecycles returned. Defaults to 100.
	MaxRequests int
}

func (o SummarizeOptions) withDefaults() SummarizeOptions {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 5 * time.Minute
	}
	if o.MaxRequests <= 0 {
		o.MaxRequests = 100
	}
	return o
}

// Summarize groups a bounded slice of events into request lifecycles ordered
// by most recent activity. It is a pure function over the bus snapshot, so it
// never blocks the data plane and never grows unbounded state. Events may
// arrive duplicated or out of order (for example after an SSE resume); the
// aggregator sorts by sequence and merges idempotently.
func Summarize(list []Event, opts SummarizeOptions) []RequestLifecycle {
	opts = opts.withDefaults()
	ordered := make([]Event, 0, len(list))
	for _, e := range list {
		if e.RequestID == "" && e.CorrelationID == "" {
			continue
		}
		if e.Phase == "" && !lifecycleKinds[e.Kind] {
			continue
		}
		ordered = append(ordered, e)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })

	// The same bus event can be replayed after an SSE reconnect. Sequence
	// numbers are unique within one bus epoch, so drop repeated non-zero
	// sequences before merging; otherwise replayed attempts and failovers
	// would be counted twice even though the summary claims idempotence.
	seenSeq := make(map[uint64]struct{}, len(ordered))
	deduped := ordered[:0]
	for _, e := range ordered {
		if e.Seq != 0 {
			if _, seen := seenSeq[e.Seq]; seen {
				continue
			}
			seenSeq[e.Seq] = struct{}{}
		}
		deduped = append(deduped, e)
	}
	ordered = deduped

	byKey := map[string]*RequestLifecycle{}
	order := make([]string, 0, len(ordered))
	for _, e := range ordered {
		key := e.CorrelationID
		if key == "" {
			key = e.RequestID
		}
		lc, ok := byKey[key]
		if !ok {
			lc = &RequestLifecycle{
				RequestID:     e.RequestID,
				CorrelationID: key,
				State:         StateActive,
				StartedAt:     e.Time,
				LastEventAt:   e.Time,
			}
			byKey[key] = lc
			order = append(order, key)
		}
		lc.merge(e)
	}

	out := make([]RequestLifecycle, 0, len(order))
	for _, key := range order {
		lc := byKey[key]
		lc.finalize(opts)
		out = append(out, *lc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastEventAt.After(out[j].LastEventAt) })
	if opts.MaxRequests > 0 && len(out) > opts.MaxRequests {
		out = out[:opts.MaxRequests]
	}
	return out
}

func (lc *RequestLifecycle) merge(e Event) {
	if lc.RequestID == "" {
		lc.RequestID = e.RequestID
	}
	if lc.CorrelationID == "" {
		lc.CorrelationID = e.CorrelationID
	}
	if e.ParentRequestID != "" && lc.ParentRequestID == "" {
		lc.ParentRequestID = e.ParentRequestID
	}
	if e.RequestedModel != "" {
		lc.RequestedModel = e.RequestedModel
	}
	if e.PublicModel != "" {
		lc.PublicModel = e.PublicModel
	}
	if e.ResolvedModel != "" {
		lc.ResolvedModel = e.ResolvedModel
	}
	if e.RouteProfile != "" {
		lc.RouteProfile = e.RouteProfile
	}
	if e.VirtualEndpoint != "" {
		lc.VirtualEndpoint = e.VirtualEndpoint
	}
	if e.Pool != "" {
		lc.Pool = e.Pool
	}
	if e.Adapter != "" {
		lc.Adapter = e.Adapter
	}
	if e.DecisionPolicyID != "" {
		lc.DecisionPolicyID = e.DecisionPolicyID
	}
	if e.DecisionPolicyVersion != "" {
		lc.DecisionPolicyVersion = e.DecisionPolicyVersion
	}
	if e.Streaming {
		lc.Streaming = true
	}
	if !e.Time.IsZero() {
		if lc.StartedAt.IsZero() || e.Time.Before(lc.StartedAt) {
			lc.StartedAt = e.Time
		}
		if e.Time.After(lc.LastEventAt) {
			lc.LastEventAt = e.Time
		}
	}
	if e.InputTokens > 0 {
		lc.InputTokens = e.InputTokens
	}
	if e.OutputTokens > 0 {
		lc.OutputTokens = e.OutputTokens
	}
	if e.CostBasis != "" {
		lc.CostBasis = e.CostBasis
	}
	if e.TTFTMS > 0 {
		lc.TTFTMS = e.TTFTMS
	}
	if e.Kind == "failover" {
		lc.FailoverCount++
		if e.FailoverClass != "" {
			lc.FailoverClass = e.FailoverClass
			if a := lc.lastAttempt(e.Deployment); a != nil {
				a.FailoverClass = e.FailoverClass
			}
		}
	}

	switch {
	case e.Phase == PhaseAttempt:
		lc.Attempts = append(lc.Attempts, LifecycleAttempt{
			Index:      len(lc.Attempts),
			Attempt:    e.Attempt,
			Deployment: e.Deployment,
			Provider:   e.Provider,
			Adapter:    e.Adapter,
			Kind:       e.AttemptKind,
			Outcome:    "started",
			StartedAt:  e.Time,
		})
	}
	if a := lc.lastStartedAttempt(e.Deployment); a != nil {
		if e.LatencyMS > 0 {
			a.LatencyMS = e.LatencyMS
		}
		if e.TTFTMS > 0 {
			a.TTFTMS = e.TTFTMS
		}
		if e.StatusCode > 0 {
			a.StatusCode = e.StatusCode
		}
		if e.ErrorType != "" {
			a.ErrorType = e.ErrorType
		}
		if e.Retryable {
			a.Retryable = true
		}
		if e.RetryAfterMS > 0 {
			a.RetryAfterMS = e.RetryAfterMS
		}
		if e.FailoverClass != "" {
			a.FailoverClass = e.FailoverClass
		}
		if e.StreamCommitted {
			a.Committed = true
		}
		switch e.Kind {
		case "route_ok":
			a.Outcome = TerminalSucceeded
		case "route_fail", "route_timeout", "stream_fail", "response_decode_fail":
			a.Outcome = TerminalFailed
		case "client_disconnect":
			a.Outcome = TerminalCancelled
		}
	}

	if e.Terminal != "" {
		lc.Terminal = e.Terminal
	} else if t, ok := terminalKinds[e.Kind]; ok {
		lc.Terminal = t
	}
	if lc.Terminal != "" {
		lc.State = lc.Terminal
		if e.LatencyMS > 0 {
			lc.LatencyMS = e.LatencyMS
		}
		if e.StatusCode > 0 {
			lc.StatusCode = e.StatusCode
		}
	}
}

// lastAttempt returns the most recent attempt for a deployment regardless of
// outcome, so a late classification event can still label the attempt it
// describes. An empty deployment matches the most recent attempt.
func (lc *RequestLifecycle) lastAttempt(deployment string) *LifecycleAttempt {
	for i := len(lc.Attempts) - 1; i >= 0; i-- {
		a := &lc.Attempts[i]
		if deployment == "" || a.Deployment == "" || a.Deployment == deployment {
			return a
		}
	}
	return nil
}

// lastStartedAttempt returns the most recent attempt for a deployment whose
// outcome is still open. An empty deployment matches the most recent open
// attempt, so events that do not name a deployment still update the timeline.
func (lc *RequestLifecycle) lastStartedAttempt(deployment string) *LifecycleAttempt {
	for i := len(lc.Attempts) - 1; i >= 0; i-- {
		a := &lc.Attempts[i]
		if a.Outcome != "started" {
			continue
		}
		if deployment == "" || a.Deployment == "" || a.Deployment == deployment {
			return a
		}
	}
	return nil
}

func (lc *RequestLifecycle) finalize(opts SummarizeOptions) {
	if lc.StartedAt.IsZero() {
		lc.StartedAt = lc.LastEventAt
	}
	if lc.LastEventAt.IsZero() {
		lc.LastEventAt = lc.StartedAt
	}
	lc.AttemptCount = len(lc.Attempts)
	if lc.Terminal == "" {
		if !opts.Now.IsZero() && opts.Now.Sub(lc.LastEventAt) > opts.StaleAfter {
			lc.State = StateStale
		} else {
			lc.State = StateActive
		}
	} else {
		lc.State = lc.Terminal
	}
}

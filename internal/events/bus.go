package events

import (
	"sync"
	"time"
	"unicode/utf8"
)

type Event struct {
	Time            time.Time `json:"time"`
	RequestID       string    `json:"request_id,omitempty"`
	Kind            string    `json:"kind"`
	Deployment      string    `json:"deployment,omitempty"`
	Message         string    `json:"message"`
	LatencyMS       int64     `json:"latency_ms,omitempty"`
	StatusCode      int       `json:"status_code,omitempty"`
	ErrorType       string    `json:"error_type,omitempty"`
	VirtualEndpoint string    `json:"virtual_endpoint,omitempty"`
	PublicModel     string    `json:"public_model,omitempty"`
	RouteProfile    string    `json:"route_profile,omitempty"`
	Pool            string    `json:"pool,omitempty"`
	// Phase C — task classification (privacy-safe, bounded)
	TaskType          string  `json:"task_type,omitempty"`
	TaskComplexity    string  `json:"task_complexity,omitempty"`
	TaskConfidence    float64 `json:"task_confidence,omitempty"`
	TaskReasonCodes   string  `json:"task_reason_codes,omitempty"` // comma-joined, bounded
	TaskEstimatedTok  int     `json:"task_estimated_tokens,omitempty"`
	TaskToolCount     int     `json:"task_tool_count,omitempty"`
	TaskImageCount    int     `json:"task_image_count,omitempty"`
	TaskMessageCount  int     `json:"task_message_count,omitempty"`
	TaskHasCode       bool    `json:"task_has_code,omitempty"`
	TaskHasVision     bool    `json:"task_has_vision,omitempty"`
	TaskHasReasoning  bool    `json:"task_has_reasoning,omitempty"`
	TaskHasTools      bool    `json:"task_has_tools,omitempty"`
	TaskStructuredOut bool    `json:"task_structured_out,omitempty"`
	// Phase D — decision plane (privacy-safe, bounded)
	DecisionProvider       string  `json:"decision_provider,omitempty"`
	DecisionAction         string  `json:"decision_action,omitempty"`
	DecisionSelected       string  `json:"decision_selected,omitempty"`
	DecisionReasonCodes    string  `json:"decision_reason_codes,omitempty"`
	DecisionCandidateCount int     `json:"decision_candidate_count,omitempty"`
	DecisionConfidence     float64 `json:"decision_confidence,omitempty"`
	// Phase E — policy explainability (privacy-safe, bounded)
	DecisionPolicyID        string  `json:"decision_policy_id,omitempty"`
	DecisionTaskType        string  `json:"decision_task_type,omitempty"`
	DecisionOriginalPrimary string  `json:"decision_original_primary,omitempty"`
	DecisionSelectedScore   float64 `json:"decision_selected_score,omitempty"`
	DecisionOriginalScore   float64 `json:"decision_original_score,omitempty"`
	DecisionChangedPrimary  bool    `json:"decision_changed_primary,omitempty"`
	DecisionBreakdown       string  `json:"decision_breakdown,omitempty"`
}

const (
	maxCounterKeys       = 256
	maxEventRequestID    = 128
	maxEventKind         = 128
	maxEventDeployment   = 512
	maxEventMessage      = 4096
	maxEventErrorType    = 128
	maxEventVirtual      = 256
	maxEventPublicModel  = 256
	maxEventRouteProfile = 256
	maxEventPool         = 256
	maxEventTaskType     = 32
	maxEventTaskComplex  = 32
	maxEventTaskReason   = 512
	counterOverflowKey   = "__other__"
	// Phase D decision bounds
	maxEventDecisionProvider    = 128
	maxEventDecisionAction      = 32
	maxEventDecisionSelected    = 512
	maxEventDecisionReasonCodes = 512
	// Phase E policy bounds
	maxEventDecisionPolicyID        = 128
	maxEventDecisionTaskType        = 32
	maxEventDecisionOriginalPrimary = 512
	maxEventDecisionBreakdown       = 4096
)

func boundedString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	// Back up to a rune boundary so multibyte text is not split into an
	// invalid UTF-8 sequence (json encoding would substitute U+FFFD).
	for i := 0; i < 3 && len(cut) > 0; i++ {
		if utf8.ValidString(cut) {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

func incrementBoundedCounter(m map[string]uint64, key string) {
	if key == "" {
		return
	}
	if _, ok := m[key]; ok {
		m[key]++
		return
	}
	// Reserve one slot for the overflow bucket so even future dynamic event
	// kinds/error types cannot grow this map without bound.
	if len(m) >= maxCounterKeys-1 {
		m[counterOverflowKey]++
		return
	}
	m[key] = 1
}

type Bus struct {
	mu          sync.RWMutex
	max         int
	items       []Event
	start       int
	count       int
	counts      map[string]uint64
	errorCounts map[string]uint64
}

func New(max int) *Bus {
	if max < 10 {
		max = 10
	}
	return &Bus{max: max, items: make([]Event, max), counts: map[string]uint64{}, errorCounts: map[string]uint64{}}
}
func (b *Bus) Add(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.RequestID = boundedString(e.RequestID, maxEventRequestID)
	e.Kind = boundedString(e.Kind, maxEventKind)
	e.Deployment = boundedString(e.Deployment, maxEventDeployment)
	e.Message = boundedString(e.Message, maxEventMessage)
	e.ErrorType = boundedString(e.ErrorType, maxEventErrorType)
	e.VirtualEndpoint = boundedString(e.VirtualEndpoint, maxEventVirtual)
	e.PublicModel = boundedString(e.PublicModel, maxEventPublicModel)
	e.RouteProfile = boundedString(e.RouteProfile, maxEventRouteProfile)
	e.Pool = boundedString(e.Pool, maxEventPool)
	e.TaskType = boundedString(e.TaskType, maxEventTaskType)
	e.TaskComplexity = boundedString(e.TaskComplexity, maxEventTaskComplex)
	e.TaskReasonCodes = boundedString(e.TaskReasonCodes, maxEventTaskReason)
	e.DecisionProvider = boundedString(e.DecisionProvider, maxEventDecisionProvider)
	e.DecisionAction = boundedString(e.DecisionAction, maxEventDecisionAction)
	e.DecisionSelected = boundedString(e.DecisionSelected, maxEventDecisionSelected)
	e.DecisionReasonCodes = boundedString(e.DecisionReasonCodes, maxEventDecisionReasonCodes)
	e.DecisionPolicyID = boundedString(e.DecisionPolicyID, maxEventDecisionPolicyID)
	e.DecisionTaskType = boundedString(e.DecisionTaskType, maxEventDecisionTaskType)
	e.DecisionOriginalPrimary = boundedString(e.DecisionOriginalPrimary, maxEventDecisionOriginalPrimary)
	e.DecisionBreakdown = boundedString(e.DecisionBreakdown, maxEventDecisionBreakdown)
	if b.count < b.max {
		idx := (b.start + b.count) % b.max
		b.items[idx] = e
		b.count++
	} else {
		b.items[b.start] = e
		b.start = (b.start + 1) % b.max
	}
	incrementBoundedCounter(b.counts, e.Kind)
	incrementBoundedCounter(b.errorCounts, e.ErrorType)
}
func (b *Bus) Snapshot() []Event {
	return b.SnapshotLimit(0)
}

func (b *Bus) SnapshotLimit(limit int) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := b.count
	if limit > 0 && count > limit {
		count = limit
	}
	out := make([]Event, count)
	offset := b.count - count
	for i := 0; i < count; i++ {
		out[i] = b.items[(b.start+offset+i)%b.max]
	}
	return out
}

func (b *Bus) Counts() map[string]uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]uint64, len(b.counts))
	for k, v := range b.counts {
		out[k] = v
	}
	return out
}

func (b *Bus) ErrorCounts() map[string]uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]uint64, len(b.errorCounts))
	for k, v := range b.errorCounts {
		out[k] = v
	}
	return out
}

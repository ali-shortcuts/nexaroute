package decision

// TaskKind is a bounded, synthetic task classification derived ONLY from
// operational metadata (capability flags, token estimates). It never reflects
// raw task content, which the decision plane never observes.
type TaskKind string

const (
	TaskSimpleChat  TaskKind = "simple_chat"
	TaskCoding      TaskKind = "coding"
	TaskDebugging   TaskKind = "debugging"
	TaskLongContext TaskKind = "long_context"
	TaskToolUse     TaskKind = "tool_use"
	TaskAgentic     TaskKind = "agentic_task"
	TaskVision      TaskKind = "vision"
	TaskGeneral     TaskKind = "general"
	TaskUnknown     TaskKind = "unknown"
)

// AllTaskKinds is the closed set of valid task kinds.
var AllTaskKinds = []TaskKind{
	TaskSimpleChat,
	TaskCoding,
	TaskDebugging,
	TaskLongContext,
	TaskToolUse,
	TaskAgentic,
	TaskVision,
	TaskGeneral,
	TaskUnknown,
}

// Valid reports whether k is a canonical task kind.
func (k TaskKind) Valid() bool {
	switch k {
	case TaskSimpleChat, TaskCoding, TaskDebugging, TaskLongContext,
		TaskToolUse, TaskAgentic, TaskVision, TaskGeneral, TaskUnknown:
		return true
	default:
		return false
	}
}

// RequestFeatures carries derived, bounded operational metadata about a
// request. It MUST NOT contain raw user content: no prompts, transcripts,
// tool schemas, tool results, headers, cookies, session keys, or credentials.
type RequestFeatures struct {
	Tools         bool
	Vision        bool
	Reasoning     bool
	Streaming     bool
	StructuredOut bool
	// EstimatedContextTokens is the conservative prompt estimate plus the
	// requested output ceiling. Zero means unknown.
	EstimatedContextTokens int
	// EstimatedInputTokens and MaxOutputTokens are the split behind the
	// combined estimate, used for honest cost-bucket pricing (unknown output
	// ceiling means unknown cost — never an invented estimate).
	EstimatedInputTokens int
	MaxOutputTokens      int
}

// LongContextThresholdTokens is the deterministic boundary at which a request
// is classified as long-context. Requests at or above this estimate prefer
// context headroom in external priority hints.
const LongContextThresholdTokens = 32000

// SimpleChatThresholdTokens bounds the small-request fast path: requests
// below this estimate with no special capability needs classify as simple
// chat and prefer latency/cost in external priority hints.
const SimpleChatThresholdTokens = 4000

// DeriveTaskKind maps request metadata to a bounded task kind. The order is
// authoritative and deterministic:
//
//	vision > agentic_tool_use > tool_use > long_context > debugging >
//	simple_chat > general.
//
// Without prompt content, coding and debugging are indistinguishable from
// metadata alone; reasoning-heavy requests map to debugging, and coding keeps
// identical priority treatment for forward compatibility.
func DeriveTaskKind(f RequestFeatures) TaskKind {
	tokens := f.EstimatedContextTokens
	if tokens < 0 {
		tokens = 0
	}
	switch {
	case f.Vision:
		return TaskVision
	case f.Tools && f.Reasoning:
		return TaskAgentic
	case f.Tools:
		return TaskToolUse
	case tokens >= LongContextThresholdTokens:
		return TaskLongContext
	case f.Reasoning:
		return TaskDebugging
	case !f.Tools && !f.Vision && !f.Reasoning && tokens < SimpleChatThresholdTokens:
		return TaskSimpleChat
	default:
		return TaskGeneral
	}
}

// Candidate is one eligible deployment (an element of the router's eligible
// set E) as seen by the decision plane. It carries factual operational
// metadata only: no credentials, no API keys, no auth material.
type Candidate struct {
	// ID is the physical deployment ID (provider/model). It never leaves
	// NexaRoute: external adapters address candidates via opaque per-request
	// IDs and map the selection back locally.
	ID string
	// PoolOrdinal is the deployment's pool rank; the earliest (minimum) pool
	// is the only pool that may supply the primary. NexaRoute core has no
	// separate pool construct, so adapters map PoolOrdinal from the
	// deployment priority tier; the fields stay independent so a future pool
	// construct can populate them separately.
	PoolOrdinal int
	// Priority is the configured priority tier (lower competes first).
	Priority int
	// ContextWindow is the advertised context window; zero means unknown.
	ContextWindow int
	// Capability flags (factual).
	Tools     bool
	Vision    bool
	Reasoning bool
	Streaming bool
	// EWMALatencyMS is the measured response-header latency EWMA; zero means
	// unknown.
	EWMALatencyMS float64
	// EWMAFailureRate is the recency-weighted failure rate in [0,1].
	EWMAFailureRate float64
	// Observations is the total success+failure observation count; zero means
	// reliability is unknown.
	Observations int64
	// HasCost reports whether any pricing is configured for this deployment.
	HasCost          bool
	InputCostPerMTok float64
	// OutputCostPerMTok prices completion tokens per million.
	OutputCostPerMTok float64
}

// MaxRequestIDBytes bounds the decision request identifier.
const MaxRequestIDBytes = 128

// DecisionRequest is the single input to a DecisionProvider. It contains the
// full eligible set (for failover preservation), the permitted primary band,
// and metadata-only features. It never contains raw user content, session
// keys, or credentials.
type DecisionRequest struct {
	// RequestID is the gateway request ID (bounded, used for local tracing
	// only; never sent to external providers).
	RequestID string
	// Candidates is the full eligible set E in existing router order. The
	// provider must never remove, add, or resurrect entries.
	Candidates []Candidate
	// AllowedPrimaryIDs is the permitted primary band in router order: the
	// earliest pool narrowed to the minimum priority tier (or the single
	// forced pin). Any SELECT outside this band is rejected.
	AllowedPrimaryIDs []string
	// ForcedPrimaryID is the eligible session pin when one exists; the
	// orchestrator short-circuits before calling any provider in that case.
	ForcedPrimaryID string
	Features        RequestFeatures
}

// EligibleSet returns the set of physical deployment IDs in E.
func (r DecisionRequest) EligibleSet() map[string]struct{} {
	out := make(map[string]struct{}, len(r.Candidates))
	for _, c := range r.Candidates {
		if c.ID != "" {
			out[c.ID] = struct{}{}
		}
	}
	return out
}

// AllowedSet returns the permitted primary band as a set.
func (r DecisionRequest) AllowedSet() map[string]struct{} {
	out := make(map[string]struct{}, len(r.AllowedPrimaryIDs))
	for _, id := range r.AllowedPrimaryIDs {
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

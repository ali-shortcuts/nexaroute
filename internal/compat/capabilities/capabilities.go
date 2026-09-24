package capabilities

import (
	"sync"
	"time"
)

// Tri-state capability support. UNKNOWN must never be treated as
// UNSUPPORTED: it means "not yet verified".
type Support int

const (
	Unknown Support = iota
	Supported
	Unsupported
)

func (s Support) String() string {
	switch s {
	case Supported:
		return "supported"
	case Unsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

// MarshalJSON keeps the store snapshot human-readable.
func (s Support) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// Source records how a capability value was learned.
type Source string

const (
	SourceStatic    Source = "static"
	SourceDiscovery Source = "discovery"
	SourceProbe     Source = "probe"
	SourceRuntime   Source = "runtime"
)

// Confidence gates runtime learning: only high-confidence evidence may
// flip a capability. A random HTTP 500 must never teach UNSUPPORTED.
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// Cell is one capability value with provenance.
type Cell struct {
	Value      Support    `json:"value"`
	Source     Source     `json:"source,omitempty"`
	Confidence Confidence `json:"confidence,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// ModelCapabilities is the per-deployment capability contract.
// Every deployment gets its own contract: Provider != Model Capability.
type ModelCapabilities struct {
	Text                Cell      `json:"text"`
	Streaming           Cell      `json:"streaming"`
	SystemMessage       Cell      `json:"system_message"`
	Tools               Cell      `json:"tools"`
	ToolChoiceAuto      Cell      `json:"tool_choice_auto"`
	ToolChoiceRequired  Cell      `json:"tool_choice_required"`
	ParallelTools       Cell      `json:"parallel_tools"`
	Reasoning           Cell      `json:"reasoning"`
	ReasoningEffort     Cell      `json:"reasoning_effort"`
	Vision              Cell      `json:"vision"`
	StructuredOutput    Cell      `json:"structured_output"`
	JSONObject          Cell      `json:"json_object"`
	JSONSchema          Cell      `json:"json_schema"`
	Temperature         Cell      `json:"temperature"`
	TopP                Cell      `json:"top_p"`
	Stop                Cell      `json:"stop"`
	Seed                Cell      `json:"seed"`
	MaxTokens           Cell      `json:"max_tokens"`
	MaxCompletionTokens Cell      `json:"max_completion_tokens"`
	ContextWindow       int       `json:"context_window,omitempty"`
	MaxOutputTokens     int       `json:"max_output_tokens,omitempty"`
	NativeProtocol      string    `json:"native_protocol,omitempty"`
	VerifiedAt          time.Time `json:"verified_at,omitempty"`
}

// Get returns the cell for a capability key.
func (m *ModelCapabilities) Get(name string) Cell {
	switch name {
	case "text":
		return m.Text
	case "streaming":
		return m.Streaming
	case "system_message":
		return m.SystemMessage
	case "tools":
		return m.Tools
	case "tool_choice_auto":
		return m.ToolChoiceAuto
	case "tool_choice_required":
		return m.ToolChoiceRequired
	case "parallel_tools":
		return m.ParallelTools
	case "reasoning":
		return m.Reasoning
	case "reasoning_effort":
		return m.ReasoningEffort
	case "vision":
		return m.Vision
	case "structured_output":
		return m.StructuredOutput
	case "json_object":
		return m.JSONObject
	case "json_schema":
		return m.JSONSchema
	case "temperature":
		return m.Temperature
	case "top_p":
		return m.TopP
	case "stop":
		return m.Stop
	case "seed":
		return m.Seed
	case "max_tokens":
		return m.MaxTokens
	case "max_completion_tokens":
		return m.MaxCompletionTokens
	default:
		return Cell{Value: Unknown}
	}
}

// Set writes the cell for a capability key.
func (m *ModelCapabilities) Set(name string, c Cell) {
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now()
	}
	switch name {
	case "text":
		m.Text = c
	case "streaming":
		m.Streaming = c
	case "system_message":
		m.SystemMessage = c
	case "tools":
		m.Tools = c
	case "tool_choice_auto":
		m.ToolChoiceAuto = c
	case "tool_choice_required":
		m.ToolChoiceRequired = c
	case "parallel_tools":
		m.ParallelTools = c
	case "reasoning":
		m.Reasoning = c
	case "reasoning_effort":
		m.ReasoningEffort = c
	case "vision":
		m.Vision = c
	case "structured_output":
		m.StructuredOutput = c
	case "json_object":
		m.JSONObject = c
	case "json_schema":
		m.JSONSchema = c
	case "temperature":
		m.Temperature = c
	case "top_p":
		m.TopP = c
	case "stop":
		m.Stop = c
	case "seed":
		m.Seed = c
	case "max_tokens":
		m.MaxTokens = c
	case "max_completion_tokens":
		m.MaxCompletionTokens = c
	}
}

// AllKeys enumerates every capability key in stable order.
func AllKeys() []string {
	return []string{
		"text", "streaming", "system_message",
		"tools", "tool_choice_auto", "tool_choice_required", "parallel_tools",
		"reasoning", "reasoning_effort",
		"vision", "structured_output", "json_object", "json_schema",
		"temperature", "top_p", "stop", "seed",
		"max_tokens", "max_completion_tokens",
	}
}

// DefaultContract returns an all-UNKNOWN contract. Capabilities must be
// verified, never assumed.
func DefaultContract() ModelCapabilities { return ModelCapabilities{} }

// FromStaticBools seeds a contract from configured static capability flags.
// A configured true becomes SUPPORTED (static, medium confidence); a
// configured false becomes UNKNOWN rather than UNSUPPORTED so probing can
// still verify the real behavior — except where the operator explicitly
// disabled the feature, which callers may override via MarkUnsupported.
func FromStaticBools(streaming, tools, vision, reasoning bool) ModelCapabilities {
	m := ModelCapabilities{}
	mk := func(on bool) Cell {
		if on {
			return Cell{Value: Supported, Source: SourceStatic, Confidence: ConfidenceMedium, UpdatedAt: time.Now()}
		}
		return Cell{Value: Unknown, Source: SourceStatic, Confidence: ConfidenceLow, UpdatedAt: time.Now()}
	}
	m.Streaming = mk(streaming)
	m.Tools = mk(tools)
	m.Vision = mk(vision)
	m.Reasoning = mk(reasoning)
	m.Text = Cell{Value: Supported, Source: SourceStatic, Confidence: ConfidenceMedium, UpdatedAt: time.Now()}
	return m
}

// Scorecard status values for Claude Code readiness.
const (
	StatusReady    = "READY"
	StatusPartial  = "PARTIAL"
	StatusNotReady = "NOT_READY"
	StatusUnknown  = "UNKNOWN"
)

// Scorecard is the per-model Claude Code compatibility report.
// See spec section 20: never reduce compatibility to one boolean.
type Scorecard struct {
	Deployment    string `json:"deployment"`
	Availability  string `json:"availability"`
	BasicChat     string `json:"basic_chat"`
	Streaming     string `json:"streaming"`
	Tools         string `json:"tools"`
	ToolResults   string `json:"tool_results"`
	ParallelTools string `json:"parallel_tools"`
	Reasoning     string `json:"reasoning"`
	Vision        string `json:"vision"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
}

func verdict(s Support) string {
	switch s {
	case Supported:
		return "PASS"
	case Unsupported:
		return "FAIL"
	default:
		return "UNKNOWN"
	}
}

// Score computes the Claude Code scorecard for a contract.
// availability should be the health status string (healthy/degraded/...).
func (m *ModelCapabilities) Score(deployment, availability string) Scorecard {
	sc := Scorecard{
		Deployment: deployment, Availability: availability,
		BasicChat: verdict(m.Text.Value), Streaming: verdict(m.Streaming.Value),
		Tools: verdict(m.Tools.Value), ToolResults: verdict(m.Tools.Value),
		ParallelTools: verdict(m.ParallelTools.Value),
		Reasoning:     verdict(m.Reasoning.Value),
		Vision:        verdict(m.Vision.Value),
	}
	switch {
	case m.Text.Value == Supported && m.Streaming.Value == Supported && m.Tools.Value == Supported:
		sc.Status = "CLAUDE_CODE_READY"
	case m.Text.Value == Supported && m.Streaming.Value == Supported:
		sc.Status = "CHAT_READY"
		sc.FailureReason = "tools not verified"
		if m.Tools.Value == Unsupported {
			sc.FailureReason = "tools unsupported"
		}
	case m.Text.Value == Supported:
		sc.Status = "BASIC_CHAT_ONLY"
		sc.FailureReason = "streaming not verified"
	default:
		sc.Status = StatusUnknown
		sc.FailureReason = "basic text not verified"
	}
	return sc
}

// Store is a concurrency-safe per-deployment capability registry.
// It is intentionally separate from health state: Health != Compatibility.
type Store struct {
	mu   sync.RWMutex
	m    map[string]*ModelCapabilities
	meta map[string]storeMeta
}

type storeMeta struct {
	BaseURL       string
	ProviderProto string
	ModelID       string
	DialectVer    string
}

func NewStore() *Store {
	return &Store{m: map[string]*ModelCapabilities{}, meta: map[string]storeMeta{}}
}

// Ensure returns the contract for a deployment, creating an UNKNOWN one.
func (s *Store) Ensure(id string) *ModelCapabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[id]
	if !ok {
		m = &ModelCapabilities{}
		s.m[id] = m
	}
	return m
}

// Get returns a copy of the contract for a deployment.
func (s *Store) Get(id string) (ModelCapabilities, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.m[id]
	if !ok {
		return ModelCapabilities{}, false
	}
	return *m, true
}

// Set replaces the contract for a deployment.
func (s *Store) Set(id string, m ModelCapabilities) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := m
	s.m[id] = &cp
}

// Learn applies a high-confidence runtime observation. Low/medium
// confidence observations never flip a value; they are recorded only when
// the current value is UNKNOWN, preserving conservative learning.
func (s *Store) Learn(id, capability string, value Support, src Source, conf Confidence, note string) {
	if capability == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[id]
	if !ok {
		m = &ModelCapabilities{}
		s.m[id] = m
	}
	cur := m.Get(capability)
	if conf != ConfidenceHigh && cur.Value != Unknown {
		return
	}
	if conf != ConfidenceHigh && value == Unsupported {
		// Only high-confidence evidence may teach UNSUPPORTED.
		return
	}
	m.Set(capability, Cell{Value: value, Source: src, Confidence: conf, UpdatedAt: time.Now(), Note: note})
	if value == Supported && capability == "text" {
		m.VerifiedAt = time.Now()
	}
}

// MarkVerified records a probe-verified value (high confidence).
func (s *Store) MarkVerified(id, capability string, value Support, src Source, note string) {
	s.Learn(id, capability, value, src, ConfidenceHigh, note)
}

// Snapshot returns copies of all contracts.
func (s *Store) Snapshot() map[string]ModelCapabilities {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]ModelCapabilities, len(s.m))
	for k, v := range s.m {
		out[k] = *v
	}
	return out
}

// InvalidateIfChanged drops the cached contract when the provider identity
// materially changed (base URL, protocol, model ID, dialect version).
// See spec section 18.
func (s *Store) InvalidateIfChanged(id, baseURL, providerProto, modelID, dialectVer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.meta[id]
	next := storeMeta{BaseURL: baseURL, ProviderProto: providerProto, ModelID: modelID, DialectVer: dialectVer}
	s.meta[id] = next
	if !ok {
		return false
	}
	if prev != next {
		delete(s.m, id)
		return true
	}
	return false
}

// Retain drops contracts for deployments that no longer exist.
func (s *Store) Retain(valid map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.m {
		if _, ok := valid[id]; !ok {
			delete(s.m, id)
		}
	}
	for id := range s.meta {
		if _, ok := valid[id]; !ok {
			delete(s.meta, id)
		}
	}
}

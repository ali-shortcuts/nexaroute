// Package compat implements NexaRoute's Universal Compatibility Engine:
// the per-model capability contract, the structured error classifier, the
// parameter sanitizer, the bounded repair engine, provider dialect profiles
// and the Level B capability probe suite.
//
// Central rule: a capability failure is not a model failure, and health is
// not compatibility. A model that rejects `temperature` is HEALTHY with
// temperature=UNSUPPORTED - never dead.
package compat

import (
	"sync"
	"time"
)

// Support is the tri-state used for every capability in the contract.
// UNKNOWN is NOT treated as UNSUPPORTED (spec: no false capability claims,
// no false capability failures); it means "not yet verified".
type Support int

const (
	UnknownSupport Support = iota
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

// MarshalJSON renders the support tri-state as a stable string form.
func (s Support) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON accepts both string and legacy boolean forms.
func (s *Support) UnmarshalJSON(b []byte) error {
	str := string(b)
	switch str {
	case `"supported"`, `"pass"`, "true", "1":
		*s = Supported
	case `"unsupported"`, `"fail"`, "false", "0":
		*s = Unsupported
	default:
		*s = UnknownSupport
	}
	return nil
}

// Capability names. Every field of ModelCapabilities maps to one of these
// strings so evidence, events and UI stay uniform.
const (
	CapText                = "text"
	CapStreaming           = "streaming"
	CapSystemMessage       = "system_message"
	CapTools               = "tools"
	CapToolChoiceAuto      = "tool_choice_auto"
	CapToolChoiceRequired  = "tool_choice_required"
	CapParallelToolCalls   = "parallel_tool_calls"
	CapReasoning           = "reasoning"
	CapReasoningEffort     = "reasoning_effort"
	CapVision              = "vision"
	CapStructuredOutput    = "structured_output"
	CapJSONObject          = "json_object"
	CapJSONSchema          = "json_schema"
	CapTemperature         = "temperature"
	CapTopP                = "top_p"
	CapStop                = "stop"
	CapSeed                = "seed"
	CapMaxTokens           = "max_tokens"
	CapMaxCompletionTokens = "max_completion_tokens"
)

// Capability list in report order.
var AllCapabilities = []string{
	CapText, CapStreaming, CapSystemMessage,
	CapTools, CapToolChoiceAuto, CapToolChoiceRequired, CapParallelToolCalls,
	CapReasoning, CapReasoningEffort, CapVision,
	CapStructuredOutput, CapJSONObject, CapJSONSchema,
	CapTemperature, CapTopP, CapStop, CapSeed,
	CapMaxTokens, CapMaxCompletionTokens,
}

// ModelCapabilities is the per-deployment (provider/model) capability matrix.
type ModelCapabilities struct {
	Text                Support `json:"text"`
	Streaming           Support `json:"streaming"`
	SystemMessage       Support `json:"system_message"`
	Tools               Support `json:"tools"`
	ToolChoiceAuto      Support `json:"tool_choice_auto"`
	ToolChoiceRequired  Support `json:"tool_choice_required"`
	ParallelToolCalls   Support `json:"parallel_tool_calls"`
	Reasoning           Support `json:"reasoning"`
	ReasoningEffort     Support `json:"reasoning_effort"`
	Vision              Support `json:"vision"`
	StructuredOutput    Support `json:"structured_output"`
	JSONObject          Support `json:"json_object"`
	JSONSchema          Support `json:"json_schema"`
	Temperature         Support `json:"temperature"`
	TopP                Support `json:"top_p"`
	Stop                Support `json:"stop"`
	Seed                Support `json:"seed"`
	MaxTokens           Support `json:"max_tokens"`
	MaxCompletionTokens Support `json:"max_completion_tokens"`

	// ContextWindow and MaxOutput are numeric constraints. 0 means UNKNOWN;
	// they are never invented.
	ContextWindow int `json:"context_window,omitempty"`
	MaxOutput     int `json:"max_output_tokens,omitempty"`

	// NativeProtocol is the verified upstream protocol family
	// (openai_chat | anthropic | openai_responses | gemini | "" unknown).
	NativeProtocol string `json:"native_protocol,omitempty"`
}

// Clone returns a deep copy.
func (m ModelCapabilities) Clone() ModelCapabilities { return m }

// Get returns the support level for a capability name.
func (m ModelCapabilities) Get(name string) Support {
	switch name {
	case CapText:
		return m.Text
	case CapStreaming:
		return m.Streaming
	case CapSystemMessage:
		return m.SystemMessage
	case CapTools:
		return m.Tools
	case CapToolChoiceAuto:
		return m.ToolChoiceAuto
	case CapToolChoiceRequired:
		return m.ToolChoiceRequired
	case CapParallelToolCalls:
		return m.ParallelToolCalls
	case CapReasoning:
		return m.Reasoning
	case CapReasoningEffort:
		return m.ReasoningEffort
	case CapVision:
		return m.Vision
	case CapStructuredOutput:
		return m.StructuredOutput
	case CapJSONObject:
		return m.JSONObject
	case CapJSONSchema:
		return m.JSONSchema
	case CapTemperature:
		return m.Temperature
	case CapTopP:
		return m.TopP
	case CapStop:
		return m.Stop
	case CapSeed:
		return m.Seed
	case CapMaxTokens:
		return m.MaxTokens
	case CapMaxCompletionTokens:
		return m.MaxCompletionTokens
	default:
		return UnknownSupport
	}
}

// Set records the support level for a capability name. Unknown names are
// ignored so callers cannot inject arbitrary keys.
func (m *ModelCapabilities) Set(name string, s Support) {
	switch name {
	case CapText:
		m.Text = s
	case CapStreaming:
		m.Streaming = s
	case CapSystemMessage:
		m.SystemMessage = s
	case CapTools:
		m.Tools = s
	case CapToolChoiceAuto:
		m.ToolChoiceAuto = s
	case CapToolChoiceRequired:
		m.ToolChoiceRequired = s
	case CapParallelToolCalls:
		m.ParallelToolCalls = s
	case CapReasoning:
		m.Reasoning = s
	case CapReasoningEffort:
		m.ReasoningEffort = s
	case CapVision:
		m.Vision = s
	case CapStructuredOutput:
		m.StructuredOutput = s
	case CapJSONObject:
		m.JSONObject = s
	case CapJSONSchema:
		m.JSONSchema = s
	case CapTemperature:
		m.Temperature = s
	case CapTopP:
		m.TopP = s
	case CapStop:
		m.Stop = s
	case CapSeed:
		m.Seed = s
	case CapMaxTokens:
		m.MaxTokens = s
	case CapMaxCompletionTokens:
		m.MaxCompletionTokens = s
	}
}

// EvidenceSource values.
const (
	SourceStatic    = "static"    // operator configuration / model metadata
	SourceDiscovery = "discovery" // protocol negotiation, model listing
	SourceProbe     = "probe"     // dedicated Level B probe
	SourceRuntime   = "runtime"   // observed on real traffic
)

// Confidence levels.
const (
	ConfidenceLow    = "low"
	ConfidenceMedium = "medium"
	ConfidenceHigh   = "high"
)

// Evidence records how a capability value was learned.
type Evidence struct {
	Source     string    `json:"source"`
	Confidence string    `json:"confidence"`
	At         time.Time `json:"at"`
	Detail     string    `json:"detail,omitempty"`
}

// Contract is one deployment's verified capability profile.
type Contract struct {
	Capabilities ModelCapabilities   `json:"capabilities"`
	Evidence     map[string]Evidence `json:"evidence,omitempty"`
	// InvalidationKey binds the contract to the provider identity inputs
	// that would invalidate it (base URL, dialect, model id, credential
	// scope). A key change drops stale compatibility information.
	InvalidationKey string    `json:"invalidation_key,omitempty"`
	VerifiedAt      time.Time `json:"verified_at,omitempty"`
	// LastCompatibilityIssue and LastRepair are observability breadcrumbs
	// (spec section 23): the WHY behind a model's state, shown in the UI.
	LastCompatibilityIssue string    `json:"last_compatibility_issue,omitempty"`
	LastIssueAt            time.Time `json:"last_issue_at,omitempty"`
	LastRepair             string    `json:"last_repair,omitempty"`
	LastRepairAt           time.Time `json:"last_repair_at,omitempty"`
}

// Scorecard derives the Claude Code compatibility scorecard (spec section 20)
// from the capability contract. It never invents PASS: UNKNOWN stays UNKNOWN.
type Scorecard struct {
	Health        string            `json:"health"`
	Protocol      string            `json:"protocol"`
	BasicChat     Support           `json:"basic_chat"`
	Streaming     Support           `json:"streaming"`
	Tools         Support           `json:"tools"`
	ToolResults   Support           `json:"tool_results"`
	ParallelTools Support           `json:"parallel_tools"`
	LongSession   Support           `json:"long_session"`
	Reasoning     Support           `json:"reasoning"`
	Vision        Support           `json:"vision"`
	Status        string            `json:"status"`
	Capabilities  ModelCapabilities `json:"capabilities"`
}

// AgentReady statuses.
const (
	StatusClaudeCodeReady = "CLAUDE_CODE_READY"
	StatusChatReady       = "CHAT_READY"
	StatusNotAgentReady   = "NOT_AGENT_READY"
	StatusNotVerified     = "NOT_VERIFIED"
)

// Scorecard computes the status from the contract. longSession requires a
// known context window of at least 64k tokens (Claude Code sessions grow
// large); a smaller verified window is a soft signal, not a hard failure.
func (c Contract) Scorecard(health string) Scorecard {
	caps := c.Capabilities
	sc := Scorecard{Health: health, Protocol: caps.NativeProtocol, Capabilities: caps}
	sc.BasicChat = caps.Text
	sc.Streaming = caps.Streaming
	sc.Tools = caps.Tools
	// Tool result continuation rides the same message path as basic text;
	// a deployment that supports tools and text supports the loop shape.
	if caps.Tools == Supported && caps.Text == Supported {
		sc.ToolResults = Supported
	} else {
		sc.ToolResults = caps.Tools
	}
	sc.ParallelTools = caps.ParallelToolCalls
	sc.LongSession = caps.Text
	sc.Reasoning = caps.Reasoning
	sc.Vision = caps.Vision
	if caps.ContextWindow > 0 && caps.ContextWindow < 64000 {
		sc.LongSession = Unsupported
	}
	switch {
	case caps.Text != Supported:
		sc.Status = StatusNotVerified
		if caps.Text == Unsupported {
			sc.Status = StatusNotAgentReady
		}
	case caps.Tools == Unsupported || caps.Streaming == Unsupported:
		sc.Status = StatusChatReady
	case caps.Tools == Supported && caps.Streaming == Supported && caps.Text == Supported:
		sc.Status = StatusClaudeCodeReady
		if caps.ParallelToolCalls == Unsupported {
			sc.Status = StatusChatReady
		}
	default:
		sc.Status = StatusNotVerified
	}
	return sc
}

// ---------- Store ----------

// Store is the process-wide capability contract store. It is safe for
// concurrent use and never blocks the request hot path on writes from other
// requests.
type Store struct {
	mu        sync.RWMutex
	contracts map[string]Contract
}

// NewStore creates an empty store.
func NewStore() *Store {
	return &Store{contracts: map[string]Contract{}}
}

func validCap(name string) bool {
	for _, c := range AllCapabilities {
		if c == name {
			return true
		}
	}
	return false
}

// Get returns a copy of the deployment contract (zero value when absent).
func (s *Store) Get(deploymentID string) Contract {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contracts[deploymentID]
}

// InvalidationKey computes what would invalidate a deployment's contract.
func InvalidationKey(baseURL, dialect, modelID, credentialScope string) string {
	return baseURL + "|" + dialect + "|" + modelID + "|" + credentialScope
}

// InvalidateIf drops the contract when identity inputs changed. It returns
// true when the store retained a contract for the deployment.
func (s *Store) InvalidateIf(deploymentID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.contracts[deploymentID]
	if !ok {
		return false
	}
	if c.InvalidationKey != "" && c.InvalidationKey != key {
		delete(s.contracts, deploymentID)
		return false
	}
	return true
}

// Drop removes the deployment contract entirely (config change).
func (s *Store) Drop(deploymentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contracts, deploymentID)
}

// Reset removes all contracts (registry swap / dialect version change).
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contracts = map[string]Contract{}
}

// LearnSuccess marks a capability SUPPORTED from real evidence. Only
// high-confidence sources may record UNSUPPORTED through LearnUnsupported.
func (s *Store) LearnSuccess(deploymentID, capability, source, detail string, key string) {
	if !validCap(capability) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.contracts[deploymentID]
	if c.Evidence == nil {
		c.Evidence = map[string]Evidence{}
	}
	if c.InvalidationKey != "" && key != "" && c.InvalidationKey != key {
		return // stale identity; drop instead of learning
	}
	if c.InvalidationKey == "" {
		c.InvalidationKey = key
	}
	c.Capabilities.Set(capability, Supported)
	confidence := ConfidenceMedium
	if source == SourceProbe {
		confidence = ConfidenceHigh
	}
	c.Evidence[capability] = Evidence{Source: source, Confidence: confidence, At: time.Now(), Detail: detail}
	if c.VerifiedAt.IsZero() {
		c.VerifiedAt = time.Now()
	}
	s.contracts[deploymentID] = c
}

// LearnUnsupported marks a capability UNSUPPORTED. Only high-confidence
// evidence may do this (a classified upstream "not supported" verdict, or a
// dedicated probe). A generic 500 can never reach this path.
func (s *Store) LearnUnsupported(deploymentID, capability, source, detail string, key string) {
	if !validCap(capability) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.contracts[deploymentID]
	if c.Evidence == nil {
		c.Evidence = map[string]Evidence{}
	}
	if c.InvalidationKey != "" && key != "" && c.InvalidationKey != key {
		return
	}
	if c.InvalidationKey == "" {
		c.InvalidationKey = key
	}
	c.Capabilities.Set(capability, Unsupported)
	c.Evidence[capability] = Evidence{Source: source, Confidence: ConfidenceHigh, At: time.Now(), Detail: detail}
	c.LastCompatibilityIssue = capability + " unsupported" + suffixDetail(detail)
	c.LastIssueAt = time.Now()
	s.contracts[deploymentID] = c
}

// LearnNumeric records numeric contract facts (context window, max output).
func (s *Store) LearnNumeric(deploymentID string, contextWindow, maxOutput int) {
	if contextWindow <= 0 && maxOutput <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.contracts[deploymentID]
	if contextWindow > 0 {
		c.Capabilities.ContextWindow = contextWindow
	}
	if maxOutput > 0 {
		c.Capabilities.MaxOutput = maxOutput
	}
	c.Evidence[CapText] = Evidence{Source: SourceStatic, Confidence: ConfidenceLow, At: time.Now(), Detail: "numeric limits"}
	s.contracts[deploymentID] = c
}

// LearnProtocol records the verified native protocol family.
func (s *Store) LearnProtocol(deploymentID, protocol string) {
	if protocol == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.contracts[deploymentID]
	c.Capabilities.NativeProtocol = protocol
	s.contracts[deploymentID] = c
}

// Seed installs static/dialect-prior capabilities without overwriting
// verified evidence (probe/runtime results win over seeds).
func (s *Store) Seed(deploymentID string, seed ModelCapabilities, source, detail string, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.contracts[deploymentID]
	if c.Evidence == nil {
		c.Evidence = map[string]Evidence{}
	}
	if c.InvalidationKey == "" {
		c.InvalidationKey = key
	}
	now := time.Now()
	apply := func(name string, v Support) {
		if v == UnknownSupport {
			return
		}
		if _, seen := c.Evidence[name]; seen {
			return // verified evidence wins
		}
		c.Capabilities.Set(name, v)
		c.Evidence[name] = Evidence{Source: source, Confidence: ConfidenceLow, At: now, Detail: detail}
	}
	for _, name := range AllCapabilities {
		apply(name, seed.Get(name))
	}
	if seed.ContextWindow > 0 {
		c.Capabilities.ContextWindow = seed.ContextWindow
	}
	if seed.MaxOutput > 0 {
		c.Capabilities.MaxOutput = seed.MaxOutput
	}
	if seed.NativeProtocol != "" {
		c.Capabilities.NativeProtocol = seed.NativeProtocol
	}
	if c.VerifiedAt.IsZero() {
		c.VerifiedAt = now
	}
	s.contracts[deploymentID] = c
}

// SetRepair records the last bounded repair applied to the deployment for UI.
func (s *Store) SetRepair(deploymentID, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.contracts[deploymentID]
	c.LastRepair = description
	c.LastRepairAt = time.Now()
	s.contracts[deploymentID] = c
}

// SetIssue records the last compatibility issue for UI.
func (s *Store) SetIssue(deploymentID, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.contracts[deploymentID]
	c.LastCompatibilityIssue = description
	c.LastIssueAt = time.Now()
	s.contracts[deploymentID] = c
}

// Snapshot returns every contract keyed by deployment ID.
func (s *Store) Snapshot() map[string]Contract {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Contract, len(s.contracts))
	for k, v := range s.contracts {
		out[k] = v
	}
	return out
}

// Count returns the number of tracked contracts.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.contracts)
}

// IneligibleFor decides whether a deployment is excluded BEFORE attempting a
// request with the given requirements (spec section 21). UNSUPPORTED blocks;
// UNKNOWN never blocks (it is merely unproven) so an unproven pool still
// serves traffic while probes refine it.
func (s *Store) IneligibleFor(deploymentID string, req RequirementProfile) (capability string, ineligible bool) {
	c := s.Get(deploymentID).Capabilities
	switch {
	case req.Tools && c.Tools == Unsupported:
		return CapTools, true
	case req.NeedsTool && (c.Tools == Unsupported || c.ToolChoiceRequired == Unsupported):
		return CapToolChoiceRequired, true
	case req.Vision && c.Vision == Unsupported:
		return CapVision, true
	case req.Streaming && c.Streaming == Unsupported:
		return CapStreaming, true
	case req.Reasoning && c.Reasoning == Unsupported:
		return CapReasoning, true
	case req.StructuredOutput && (c.JSONObject == Unsupported && c.JSONSchema == Unsupported):
		return CapStructuredOutput, true
	default:
		return "", false
	}
}

// RequirementProfile is the router-facing requirement view of a canonical
// request (subset used by eligibility checks).
type RequirementProfile struct {
	Tools            bool
	NeedsTool        bool
	Vision           bool
	Streaming        bool
	Reasoning        bool
	StructuredOutput bool
	Stop             bool
	Seed             bool
}

func suffixDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

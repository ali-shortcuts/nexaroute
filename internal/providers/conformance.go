package providers

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Conformance manifest: the single evidence source for provider/protocol
// compatibility claims (issue #436). docs/COMPATIBILITY.md carries a
// generated table rendered from this manifest and a contract test fails
// when the two drift apart. A preset or a model-list response is NOT
// evidence: a dimension may only claim FIXTURE_VERIFIED when a named
// hermetic test actually exercises it, and LIVE_VERIFIED only when a
// bounded live canary produced the evidence. Everything else stays
// PARTIAL, UNTESTED or UNSUPPORTED.

// ConformanceState is the evidence state of one conformance claim.
type ConformanceState string

const (
	StateUntested        ConformanceState = "UNTESTED"
	StateFixtureVerified ConformanceState = "FIXTURE-VERIFIED"
	StateLiveVerified    ConformanceState = "LIVE-VERIFIED"
	StatePartial         ConformanceState = "PARTIAL"
	StateUnsupported     ConformanceState = "UNSUPPORTED"
	// StateStale is never stored in the manifest; it is computed by
	// EffectiveState when the recorded evidence is older than the
	// staleness policy.
	StateStale ConformanceState = "STALE"
)

// ConformanceDimension names one axis of the provider contract.
type ConformanceDimension string

const (
	DimAddressing       ConformanceDimension = "addressing"
	DimAuth             ConformanceDimension = "auth"
	DimNonStream        ConformanceDimension = "non_stream"
	DimStreaming        ConformanceDimension = "streaming_sse"
	DimToolCalls        ConformanceDimension = "tool_calls"
	DimToolResults      ConformanceDimension = "tool_results"
	DimParallelTools    ConformanceDimension = "parallel_tools"
	DimReasoning        ConformanceDimension = "reasoning"
	DimStructuredOutput ConformanceDimension = "structured_output"
	DimImages           ConformanceDimension = "images_multimodal"
	DimUsageCache       ConformanceDimension = "usage_cache_tokens"
	DimFinishReasons    ConformanceDimension = "finish_reasons"
	DimErrorEnvelopes   ConformanceDimension = "error_envelopes"
	DimRetryAfter       ConformanceDimension = "retry_after"
	DimCancellation     ConformanceDimension = "cancellation"
	DimIdempotency      ConformanceDimension = "idempotency"
)

// ConformanceDimensions lists every dimension in stable display order.
func ConformanceDimensions() []ConformanceDimension {
	return []ConformanceDimension{
		DimAddressing, DimAuth, DimNonStream, DimStreaming,
		DimToolCalls, DimToolResults, DimParallelTools, DimReasoning,
		DimStructuredOutput, DimImages, DimUsageCache, DimFinishReasons,
		DimErrorEnvelopes, DimRetryAfter, DimCancellation, DimIdempotency,
	}
}

// DimensionClaim is one dimension's state plus the limitation note that
// must travel with the claim wherever it is displayed.
type DimensionClaim struct {
	State ConformanceState `json:"state"`
	Note  string           `json:"note,omitempty"`
}

// ConformanceEntry is the manifest record for one provider dialect
// (a preset Type). Native describes same-protocol passthrough only;
// translation guarantees live in the dimension claims and must never
// be read as lossless cross-protocol claims.
type ConformanceEntry struct {
	Dialect         string                                  `json:"dialect"`
	ContractVersion string                                  `json:"contract_version"`
	Native          bool                                    `json:"native_passthrough"`
	Status          ConformanceState                        `json:"status"`
	Dimensions      map[ConformanceDimension]DimensionClaim `json:"dimensions"`
	EvidenceSource  string                                  `json:"evidence_source"`
	EvidenceDate    string                                  `json:"evidence_date"` // YYYY-MM-DD
	Limitations     string                                  `json:"limitations,omitempty"`
}

// ManifestVersion identifies the manifest schema generation.
const ManifestVersion = "conformance.v1"

// DefaultStaleAfterDays is the freshness window for fixture evidence:
// a fixture pass proves the contract on the day it ran, not forever.
const DefaultStaleAfterDays = 180

// Manifest returns the versioned conformance manifest. Evidence dates
// record when the cited evidence was last actually run and reviewed.
func Manifest() []ConformanceEntry {
	return []ConformanceEntry{
		{
			Dialect: "anthropic_compatible", ContractVersion: ManifestVersion, Native: true,
			Status: StateFixtureVerified, EvidenceDate: "2026-10-18",
			EvidenceSource: "internal/httpapi/conformance_test.go (M5-2 pairs); internal/protocol/canonical/conformance_regression_test.go",
			Limitations:    "Live api.anthropic.com behavior is UNVERIFIED; evidence is dialect-faithful fakes plus codec fixtures.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StateFixtureVerified},
				DimAuth:             {State: StateFixtureVerified, Note: "x-api-key; client auth never reused as provider auth"},
				DimNonStream:        {State: StateFixtureVerified},
				DimStreaming:        {State: StateFixtureVerified},
				DimToolCalls:        {State: StateFixtureVerified},
				DimToolResults:      {State: StateFixtureVerified, Note: "tool results fold into user turns (M5-2 defect 1)"},
				DimParallelTools:    {State: StateFixtureVerified, Note: "disable_parallel_tool_use mapped"},
				DimReasoning:        {State: StateFixtureVerified, Note: "thinking budgets; unsigned blocks never fabricated"},
				DimStructuredOutput: {State: StatePartial, Note: "no IR->Anthropic response_format mapping in this build"},
				DimImages:           {State: StateFixtureVerified},
				DimUsageCache:       {State: StateFixtureVerified, Note: "cache_read/cache_creation tokens mapped both directions"},
				DimFinishReasons:    {State: StateFixtureVerified},
				DimErrorEnvelopes:   {State: StateFixtureVerified, Note: "Anthropic-style envelopes on Anthropic ingress"},
				DimRetryAfter:       {State: StateFixtureVerified, Note: "honored with configurable cap"},
				DimCancellation:     {State: StatePartial, Note: "context-propagated; dedicated gateway cancellation fixture in internal/httpapi/retry_safety_test.go; provider-side cancellation of in-flight upstream work remains unverified"},
				DimIdempotency:      {State: StateUntested, Note: "no verified provider mechanism; gateway never generates a key and by default never forwards a client key (an operator forward_headers listing is an operator assertion, not gateway evidence); gateway-side behavior fixture-covered in internal/retrysafety and internal/httpapi/retry_safety_test.go"},
			},
		},
		{
			Dialect: "openai_compatible", ContractVersion: ManifestVersion, Native: true,
			Status: StateFixtureVerified, EvidenceDate: "2026-10-18",
			EvidenceSource: "internal/httpapi/conformance_test.go (M5-2 pairs); internal/providers/http_adapter_test.go",
			Limitations:    "Third-party OpenAI-compatible endpoints vary; a /models response alone verifies nothing beyond addressing/auth.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StateFixtureVerified},
				DimAuth:             {State: StateFixtureVerified, Note: "bearer; per-provider header allowlist"},
				DimNonStream:        {State: StateFixtureVerified},
				DimStreaming:        {State: StateFixtureVerified, Note: "stream_options.include_usage injected, retried without on rejection"},
				DimToolCalls:        {State: StateFixtureVerified, Note: "reversible tool-name mapping for grammar-violating names"},
				DimToolResults:      {State: StateFixtureVerified},
				DimParallelTools:    {State: StateFixtureVerified},
				DimReasoning:        {State: StatePartial, Note: "reasoning_effort passthrough is provider-dependent"},
				DimStructuredOutput: {State: StateFixtureVerified, Note: "response_format mapped"},
				DimImages:           {State: StateFixtureVerified, Note: "data URL + URL forms"},
				DimUsageCache:       {State: StateFixtureVerified, Note: "prompt_tokens_details.cached_tokens mapped"},
				DimFinishReasons:    {State: StateFixtureVerified},
				DimErrorEnvelopes:   {State: StateFixtureVerified},
				DimRetryAfter:       {State: StateFixtureVerified, Note: "honored with configurable cap"},
				DimCancellation:     {State: StatePartial, Note: "context-propagated; dedicated gateway cancellation fixture in internal/httpapi/retry_safety_test.go; provider-side cancellation of in-flight upstream work remains unverified"},
				DimIdempotency:      {State: StateUntested, Note: "no verified provider mechanism; gateway never generates a key and by default never forwards a client key (an operator forward_headers listing is an operator assertion, not gateway evidence); gateway-side behavior fixture-covered in internal/retrysafety and internal/httpapi/retry_safety_test.go"},
			},
		},
		{
			Dialect: "openai_responses", ContractVersion: ManifestVersion, Native: true,
			Status: StateFixtureVerified, EvidenceDate: "2026-10-18",
			EvidenceSource: "internal/httpapi/conformance_test.go (M5-2 pairs); internal/httpapi/codex_responses_test.go",
			Limitations:    "Responses API variants across third-party hosts are UNTESTED.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StateFixtureVerified},
				DimAuth:             {State: StateFixtureVerified},
				DimNonStream:        {State: StateFixtureVerified},
				DimStreaming:        {State: StateFixtureVerified},
				DimToolCalls:        {State: StateFixtureVerified},
				DimToolResults:      {State: StateFixtureVerified, Note: "function_call_output items"},
				DimParallelTools:    {State: StateFixtureVerified},
				DimReasoning:        {State: StateFixtureVerified, Note: "reasoning object mapped"},
				DimStructuredOutput: {State: StateFixtureVerified, Note: "text.format mapped"},
				DimImages:           {State: StateFixtureVerified},
				DimUsageCache:       {State: StateFixtureVerified, Note: "input_tokens_details.cached_tokens mapped"},
				DimFinishReasons:    {State: StateFixtureVerified},
				DimErrorEnvelopes:   {State: StateFixtureVerified},
				DimRetryAfter:       {State: StateFixtureVerified},
				DimCancellation:     {State: StatePartial, Note: "context-propagated; no dedicated cancellation fixture"},
				DimIdempotency:      {State: StateUntested, Note: "no verified provider mechanism; gateway never generates a key and by default never forwards a client key (an operator forward_headers listing is an operator assertion, not gateway evidence); gateway-side behavior fixture-covered in internal/retrysafety and internal/httpapi/retry_safety_test.go"},
			},
		},
		{
			Dialect: "gemini", ContractVersion: ManifestVersion, Native: false,
			Status: StateFixtureVerified, EvidenceDate: "2026-10-18",
			EvidenceSource: "internal/httpapi/conformance_test.go (M5-2 pairs); internal/protocol/canonical conformance fixtures",
			Limitations:    "Reached via canonical translation, not native passthrough. Live Google behavior UNVERIFIED.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StateFixtureVerified, Note: "model-in-path addressing pinned by M5-2 defect 3"},
				DimAuth:             {State: StateFixtureVerified, Note: "x-goog-api-key; key never forwarded to non-Google hosts"},
				DimNonStream:        {State: StateFixtureVerified},
				DimStreaming:        {State: StateFixtureVerified},
				DimToolCalls:        {State: StateFixtureVerified},
				DimToolResults:      {State: StateFixtureVerified, Note: "functionResponse parts"},
				DimParallelTools:    {State: StateUnsupported, Note: "no equivalent knob; flag dropped on encode"},
				DimReasoning:        {State: StatePartial, Note: "budget hints only; no faithful budget mapping"},
				DimStructuredOutput: {State: StateFixtureVerified, Note: "responseMimeType/responseSchema"},
				DimImages:           {State: StateFixtureVerified},
				DimUsageCache:       {State: StatePartial, Note: "caching is implicit; usage reported, never requested"},
				DimFinishReasons:    {State: StateFixtureVerified},
				DimErrorEnvelopes:   {State: StateFixtureVerified},
				DimRetryAfter:       {State: StateFixtureVerified},
				DimCancellation:     {State: StatePartial, Note: "context-propagated; no dedicated cancellation fixture"},
				DimIdempotency:      {State: StateUntested, Note: "no verified provider mechanism; gateway never generates a key and by default never forwards a client key (an operator forward_headers listing is an operator assertion, not gateway evidence); gateway-side behavior fixture-covered in internal/retrysafety and internal/httpapi/retry_safety_test.go"},
			},
		},
		{
			Dialect: "bedrock", ContractVersion: ManifestVersion, Native: false,
			Status: StatePartial, EvidenceDate: "2026-10-18",
			EvidenceSource: "internal/providers/bedrock_adapter_test.go; internal/providers/sigv4_test.go",
			Limitations:    "Converse coverage is adapter-unit-test evidence only: no M5-2 ingress-pair fixtures and NO live AWS verification.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StatePartial, Note: "Converse paths unit-tested; no /v1/models listing claim (see #491 review)"},
				DimAuth:             {State: StatePartial, Note: "SigV4 signing unit-tested; live IAM behavior UNVERIFIED"},
				DimNonStream:        {State: StatePartial},
				DimStreaming:        {State: StatePartial, Note: "event-stream framing unit-tested"},
				DimToolCalls:        {State: StatePartial, Note: "toolUse/toolResult blocks in encoder fixtures"},
				DimToolResults:      {State: StatePartial},
				DimParallelTools:    {State: StateUnsupported, Note: "no equivalent knob; flag dropped on encode"},
				DimReasoning:        {State: StateUnsupported, Note: "thinking blocks not encoded to Converse"},
				DimStructuredOutput: {State: StateUnsupported, Note: "no response_format mapping"},
				DimImages:           {State: StatePartial},
				DimUsageCache:       {State: StatePartial, Note: "cachePoint mapping unit-tested; live cache behavior UNVERIFIED"},
				DimFinishReasons:    {State: StatePartial},
				DimErrorEnvelopes:   {State: StatePartial},
				DimRetryAfter:       {State: StatePartial},
				DimCancellation:     {State: StateUntested},
				DimIdempotency:      {State: StateUntested, Note: "no verified provider mechanism; gateway never generates a key and by default never forwards a client key (an operator forward_headers listing is an operator assertion, not gateway evidence); gateway-side behavior fixture-covered in internal/retrysafety and internal/httpapi/retry_safety_test.go"},
			},
		},
		{
			Dialect: "azure_openai", ContractVersion: ManifestVersion, Native: false,
			Status: StatePartial, EvidenceDate: "2026-10-18",
			EvidenceSource: "internal/providers/azure_vertex_test.go",
			Limitations:    "Deployment addressing and api-key auth are unit-tested; no conformance-pair fixtures and no live Azure tenant verification. The shipped preset base URL is a placeholder the operator must replace.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StatePartial, Note: "deployment + api-version addressing unit-tested"},
				DimAuth:             {State: StatePartial, Note: "api-key header unit-tested"},
				DimNonStream:        {State: StateUntested},
				DimStreaming:        {State: StateUntested},
				DimToolCalls:        {State: StateUntested},
				DimToolResults:      {State: StateUntested},
				DimParallelTools:    {State: StateUntested},
				DimReasoning:        {State: StateUntested},
				DimStructuredOutput: {State: StateUntested},
				DimImages:           {State: StateUntested},
				DimUsageCache:       {State: StateUntested},
				DimFinishReasons:    {State: StateUntested},
				DimErrorEnvelopes:   {State: StateUntested},
				DimRetryAfter:       {State: StateUntested},
				DimCancellation:     {State: StateUntested},
				DimIdempotency:      {State: StateUntested, Note: "no verified provider mechanism; gateway never generates a key and by default never forwards a client key (an operator forward_headers listing is an operator assertion, not gateway evidence); gateway-side behavior fixture-covered in internal/retrysafety and internal/httpapi/retry_safety_test.go"},
			},
		},
		{
			Dialect: "opencode-console", ContractVersion: ManifestVersion, Native: false,
			Status: StatePartial, EvidenceDate: "2026-10-28",
			EvidenceSource: "internal/providers/opencode_test.go; internal/httpapi/opencode_test.go (S1-S5 cells for 3 ingresses x both profiles); internal/config/opencode_test.go",
			Limitations:    "Fixture-only evidence: live opencode.ai behavior is UNVERIFIED (live = NOT_RUN). Reached via canonical translation, not native passthrough. Console receives no gateway-derived session header. Free-tier models return a model-entitlement 403 diagnostic (status-based; an invalid key returning 403 surfaces the same diagnostic on that request). Go discovery is unsupported; multi-tool fixtures cover the OpenAI chat ingress only; reasoning/images/structured-output/idempotency are untested or partial as marked; cache tokens are not fixture-verified.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StateFixtureVerified, Note: "ingress-selected opencode paths asserted per cell"},
				DimAuth:             {State: StateFixtureVerified, Note: "bearer; per-credential keys; no key or email in state surfaces"},
				DimNonStream:        {State: StateFixtureVerified},
				DimStreaming:        {State: StateFixtureVerified, Note: "SSE reassembled with terminal marker per ingress"},
				DimToolCalls:        {State: StateFixtureVerified},
				DimToolResults:      {State: StateFixtureVerified},
				DimParallelTools:    {State: StatePartial, Note: "multiple tool calls fixture-verified on OpenAI chat ingress only"},
				DimReasoning:        {State: StatePartial, Note: "thinking blocks survive Anthropic ingress to Anthropic upstream; cross-protocol reasoning mapping not fixture-verified"},
				DimStructuredOutput: {State: StateUntested},
				DimImages:           {State: StateUntested},
				DimUsageCache:       {State: StatePartial, Note: "usage tokens mapped from upstream fields; cache tokens not fixture-verified"},
				DimFinishReasons:    {State: StatePartial, Note: "terminal markers fixture-verified; per-reason mapping not asserted"},
				DimErrorEnvelopes:   {State: StateFixtureVerified, Note: "429 envelope per ingress; opencode 403 entitlement diagnostic"},
				DimRetryAfter:       {State: StateFixtureVerified, Note: "delta-seconds and HTTP-date forms, fallback, cap"},
				DimCancellation:     {State: StatePartial, Note: "context-propagated; no dedicated cancellation fixture"},
				DimIdempotency:      {State: StateUntested},
			},
		},
		{
			Dialect: "opencode-go", ContractVersion: ManifestVersion, Native: false,
			Status: StatePartial, EvidenceDate: "2026-10-28",
			EvidenceSource: "internal/providers/opencode_test.go; internal/httpapi/opencode_test.go (S1-S5 cells for 3 ingresses x both profiles); internal/config/opencode_test.go",
			Limitations:    "Fixture-only evidence: live opencode.ai behavior is UNVERIFIED (live = NOT_RUN). Reached via canonical translation, not native passthrough. Go discovery is undocumented and unsupported (models are saved manually; no Go listing endpoint is assumed). Go requests carry a derived x-opencode-session header (client value wins unchanged; deterministic provider|model fallback). Free-tier models return a model-entitlement 403 diagnostic (status-based; an invalid key returning 403 surfaces the same diagnostic on that request). Multi-tool fixtures cover the OpenAI chat ingress only; reasoning/images/structured-output/idempotency are untested or partial as marked; cache tokens are not fixture-verified.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StateFixtureVerified, Note: "ingress-selected opencode paths asserted per cell"},
				DimAuth:             {State: StateFixtureVerified, Note: "bearer; derived Go session header stable per conversation; per-credential keys"},
				DimNonStream:        {State: StateFixtureVerified},
				DimStreaming:        {State: StateFixtureVerified, Note: "SSE reassembled with terminal marker per ingress"},
				DimToolCalls:        {State: StateFixtureVerified},
				DimToolResults:      {State: StateFixtureVerified},
				DimParallelTools:    {State: StatePartial, Note: "multiple tool calls fixture-verified on OpenAI chat ingress only"},
				DimReasoning:        {State: StatePartial, Note: "thinking blocks survive Anthropic ingress to Anthropic upstream; cross-protocol reasoning mapping not fixture-verified"},
				DimStructuredOutput: {State: StateUntested},
				DimImages:           {State: StateUntested},
				DimUsageCache:       {State: StatePartial, Note: "usage tokens mapped from upstream fields; cache tokens not fixture-verified"},
				DimFinishReasons:    {State: StatePartial, Note: "terminal markers fixture-verified; per-reason mapping not asserted"},
				DimErrorEnvelopes:   {State: StateFixtureVerified, Note: "429 envelope per ingress; opencode 403 entitlement diagnostic"},
				DimRetryAfter:       {State: StateFixtureVerified, Note: "delta-seconds and HTTP-date forms, fallback, cap"},
				DimCancellation:     {State: StatePartial, Note: "context-propagated; no dedicated cancellation fixture"},
				DimIdempotency:      {State: StateUntested},
			},
		},
		{
			Dialect: "vertex", ContractVersion: ManifestVersion, Native: false,
			Status: StatePartial, EvidenceDate: "2026-10-18",
			EvidenceSource: "internal/providers/azure_vertex_test.go",
			Limitations:    "Addressing/auth shape unit-tested only; OAuth token exchange and live Vertex behavior UNVERIFIED.",
			Dimensions: map[ConformanceDimension]DimensionClaim{
				DimAddressing:       {State: StatePartial, Note: "project/location/model addressing unit-tested"},
				DimAuth:             {State: StatePartial, Note: "bearer shape unit-tested; OAuth exchange UNVERIFIED"},
				DimNonStream:        {State: StateUntested},
				DimStreaming:        {State: StateUntested},
				DimToolCalls:        {State: StateUntested},
				DimToolResults:      {State: StateUntested},
				DimParallelTools:    {State: StateUntested},
				DimReasoning:        {State: StateUntested},
				DimStructuredOutput: {State: StateUntested},
				DimImages:           {State: StateUntested},
				DimUsageCache:       {State: StateUntested},
				DimFinishReasons:    {State: StateUntested},
				DimErrorEnvelopes:   {State: StateUntested},
				DimRetryAfter:       {State: StateUntested},
				DimCancellation:     {State: StateUntested},
				DimIdempotency:      {State: StateUntested, Note: "no verified provider mechanism; gateway never generates a key and by default never forwards a client key (an operator forward_headers listing is an operator assertion, not gateway evidence); gateway-side behavior fixture-covered in internal/retrysafety and internal/httpapi/retry_safety_test.go"},
			},
		},
	}
}

// ManifestEntry returns the manifest entry for a dialect, if present.
func ManifestEntry(dialect string) (ConformanceEntry, bool) {
	for _, e := range Manifest() {
		if e.Dialect == dialect {
			return e, true
		}
	}
	return ConformanceEntry{}, false
}

// EffectiveState applies the staleness policy: verified evidence older
// than staleAfterDays is reported STALE. UNSUPPORTED and UNTESTED
// claims do not go stale (they assert absence, not freshness).
func EffectiveState(e ConformanceEntry, now time.Time, staleAfterDays int) ConformanceState {
	if e.Status != StateFixtureVerified && e.Status != StateLiveVerified && e.Status != StatePartial {
		return e.Status
	}
	d, err := time.Parse("2006-01-02", e.EvidenceDate)
	if err != nil {
		return StateStale
	}
	if now.Sub(d) > time.Duration(staleAfterDays)*24*time.Hour {
		return StateStale
	}
	return e.Status
}

// ValidateManifest checks structural honesty rules for a manifest
// against the set of dialect types the build actually ships (preset
// Types). It returns every violation found; an empty result means the
// manifest is structurally sound.
func ValidateManifest(entries []ConformanceEntry, knownDialects map[string]bool) []string {
	var problems []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Dialect == "" {
			problems = append(problems, "manifest entry with empty dialect")
			continue
		}
		if seen[e.Dialect] {
			problems = append(problems, fmt.Sprintf("duplicate manifest entry for dialect %q", e.Dialect))
		}
		seen[e.Dialect] = true
		if !knownDialects[e.Dialect] {
			problems = append(problems, fmt.Sprintf("manifest entry %q does not map to any shipped preset type (incorrect adapter mapping)", e.Dialect))
		}
		if e.ContractVersion != ManifestVersion {
			problems = append(problems, fmt.Sprintf("dialect %q has contract version %q, want %q", e.Dialect, e.ContractVersion, ManifestVersion))
		}
		if (e.Status == StateFixtureVerified || e.Status == StateLiveVerified) && strings.TrimSpace(e.EvidenceSource) == "" {
			problems = append(problems, fmt.Sprintf("dialect %q claims %s without an evidence source (missing evidence)", e.Dialect, e.Status))
		}
		if _, err := time.Parse("2006-01-02", e.EvidenceDate); err != nil {
			problems = append(problems, fmt.Sprintf("dialect %q has unparseable evidence date %q", e.Dialect, e.EvidenceDate))
		}
		for _, dim := range ConformanceDimensions() {
			claim, ok := e.Dimensions[dim]
			if !ok {
				problems = append(problems, fmt.Sprintf("dialect %q is missing dimension %q", e.Dialect, dim))
				continue
			}
			switch claim.State {
			case StateUntested, StateFixtureVerified, StateLiveVerified, StatePartial, StateUnsupported:
			default:
				problems = append(problems, fmt.Sprintf("dialect %q dimension %q has invalid stored state %q (STALE is computed, never stored)", e.Dialect, dim, claim.State))
			}
			if claim.State == StateLiveVerified && e.Status != StateLiveVerified {
				problems = append(problems, fmt.Sprintf("dialect %q dimension %q claims LIVE-VERIFIED inside a %s entry", e.Dialect, dim, e.Status))
			}
		}
	}
	for d := range knownDialects {
		if !seen[d] {
			problems = append(problems, fmt.Sprintf("shipped preset type %q has no manifest entry", d))
		}
	}
	sort.Strings(problems)
	return problems
}

// RenderConformanceTable renders the manifest as the generated
// markdown section embedded in docs/COMPATIBILITY.md. The contract
// test re-renders and compares, so any drift fails CI with this exact
// text as the actionable expected output.
func RenderConformanceTable(entries []ConformanceEntry) string {
	dims := ConformanceDimensions()
	var b strings.Builder
	b.WriteString("| Dialect | Status | Evidence date |")
	for _, d := range dims {
		b.WriteString(" ")
		b.WriteString(string(d))
		b.WriteString(" |")
	}
	b.WriteString("\n|---|---|---|")
	for range dims {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	sorted := append([]ConformanceEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Dialect < sorted[j].Dialect })
	for _, e := range sorted {
		fmt.Fprintf(&b, "| %s | %s | %s |", e.Dialect, e.Status, e.EvidenceDate)
		for _, d := range dims {
			fmt.Fprintf(&b, " %s |", e.Dimensions[d].State)
		}
		b.WriteString("\n")
	}
	return b.String()
}

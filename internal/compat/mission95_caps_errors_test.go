package compat

import (
	"errors"
	"strings"
	"testing"
)

func TestMission95Scorecard(t *testing.T) {
	full := ModelCapabilities{Text: Supported, Streaming: Supported, Tools: Supported, ParallelToolCalls: Supported, Reasoning: Supported, Vision: Supported, ContextWindow: 128000, NativeProtocol: "anthropic"}
	sc := Contract{Capabilities: full}.Scorecard("healthy")
	if sc.Status != StatusClaudeCodeReady || sc.ToolResults != Supported || sc.LongSession != Supported || sc.Protocol != "anthropic" {
		t.Fatalf("full: %+v", sc)
	}
	// Text unsupported -> NOT_AGENT_READY; text unknown -> NOT_VERIFIED.
	sc = Contract{Capabilities: ModelCapabilities{Text: Unsupported}}.Scorecard("h")
	if sc.Status != StatusNotAgentReady {
		t.Fatalf("text unsupported: %q", sc.Status)
	}
	sc = Contract{Capabilities: ModelCapabilities{Text: UnknownSupport}}.Scorecard("h")
	if sc.Status != StatusNotVerified {
		t.Fatalf("text unknown: %q", sc.Status)
	}
	// Tools or streaming unsupported -> CHAT_READY.
	sc = Contract{Capabilities: ModelCapabilities{Text: Supported, Tools: Unsupported, Streaming: Supported}}.Scorecard("h")
	if sc.Status != StatusChatReady {
		t.Fatalf("tools unsupported: %q", sc.Status)
	}
	sc = Contract{Capabilities: ModelCapabilities{Text: Supported, Tools: Supported, Streaming: Unsupported}}.Scorecard("h")
	if sc.Status != StatusChatReady {
		t.Fatalf("streaming unsupported: %q", sc.Status)
	}
	// Parallel unsupported demotes Claude-Code-ready to chat-ready.
	caps := full
	caps.ParallelToolCalls = Unsupported
	sc = Contract{Capabilities: caps}.Scorecard("h")
	if sc.Status != StatusChatReady {
		t.Fatalf("parallel unsupported: %q", sc.Status)
	}
	// Neither ready nor failing -> NOT_VERIFIED (default branch).
	sc = Contract{Capabilities: ModelCapabilities{Text: Supported, Streaming: Supported}}.Scorecard("h")
	if sc.Status != StatusNotVerified {
		t.Fatalf("default: %q", sc.Status)
	}
	// Small verified window is a soft long-session failure, not a status change.
	caps = full
	caps.ContextWindow = 32000
	sc = Contract{Capabilities: caps}.Scorecard("h")
	if sc.LongSession != Unsupported || sc.Status != StatusClaudeCodeReady {
		t.Fatalf("small window: %+v", sc)
	}
	// Tool results ride tools+text; otherwise mirror tools.
	sc = Contract{Capabilities: ModelCapabilities{Text: Supported, Tools: UnknownSupport}}.Scorecard("h")
	if sc.ToolResults != UnknownSupport {
		t.Fatalf("tool results mirror: %v", sc.ToolResults)
	}
	sc = Contract{Capabilities: ModelCapabilities{Text: UnknownSupport, Tools: Supported}}.Scorecard("h")
	if sc.ToolResults != Supported {
		t.Fatalf("tool results tools-only: %v", sc.ToolResults)
	}
}

func TestMission95StoreLearning(t *testing.T) {
	if validCap("nope") || !validCap(CapTools) {
		t.Fatalf("validCap")
	}
	s := NewStore()
	// Invalid capability is a no-op.
	s.LearnSuccess("d", "nope", SourceProbe, "", "k")
	if s.Count() != 0 {
		t.Fatalf("invalid cap learned")
	}
	// LearnSuccess: key adoption, confidence by source, VerifiedAt.
	s.LearnSuccess("d", CapTools, SourceRuntime, "ok", "k1")
	c := s.Get("d")
	if c.Capabilities.Tools != Supported || c.Evidence[CapTools].Confidence != ConfidenceMedium || c.InvalidationKey != "k1" || c.VerifiedAt.IsZero() {
		t.Fatalf("learn success: %+v", c)
	}
	s.LearnSuccess("d", CapVision, SourceProbe, "probe", "k1")
	if s.Get("d").Evidence[CapVision].Confidence != ConfidenceHigh {
		t.Fatalf("probe confidence")
	}
	// Stale identity: learning refused, old value kept.
	s.LearnSuccess("d", CapVision, SourceProbe, "stale", "k2")
	if s.Get("d").Evidence[CapVision].Detail != "probe" {
		t.Fatalf("stale learn leaked")
	}
	s.LearnUnsupported("d", CapVision, SourceProbe, "stale", "k2")
	if s.Get("d").Capabilities.Vision != Supported {
		t.Fatalf("stale unsupported leaked")
	}
	// LearnUnsupported: issue breadcrumb with/without detail suffix.
	s.LearnUnsupported("d", CapReasoning, SourceProbe, "not enabled", "k1")
	c = s.Get("d")
	if c.Capabilities.Reasoning != Unsupported || c.LastCompatibilityIssue != "reasoning unsupported (not enabled)" || c.LastIssueAt.IsZero() {
		t.Fatalf("learn unsupported: %+v", c)
	}
	s.LearnUnsupported("d", CapVision, SourceProbe, "", "k1")
	if s.Get("d").LastCompatibilityIssue != "vision unsupported" {
		t.Fatalf("empty detail suffix: %q", s.Get("d").LastCompatibilityIssue)
	}
	if got := suffixDetail(""); got != "" {
		t.Fatalf("suffix empty: %q", got)
	}
	if got := suffixDetail("x"); got != " (x)" {
		t.Fatalf("suffix: %q", got)
	}
	// InvalidateIf: absent false, match true, mismatch drops, empty stored key keeps.
	if s.InvalidateIf("missing", "k1") {
		t.Fatalf("absent invalidate")
	}
	if !s.InvalidateIf("d", "k1") {
		t.Fatalf("matching invalidate should retain")
	}
	if s.InvalidateIf("d", "other") || s.Count() != 0 {
		t.Fatalf("mismatch should drop")
	}
	s.LearnSuccess("e", CapText, SourceRuntime, "", "")
	if !s.InvalidateIf("e", "anything") {
		t.Fatalf("empty stored key should retain")
	}
	// LearnNumeric: no-op on zeros, partial updates, static evidence.
	before := s.Count()
	s.LearnNumeric("n", 0, 0)
	if s.Count() != before {
		t.Fatalf("zero numeric learned")
	}
	s.LearnNumeric("n", 64000, 0)
	c = s.Get("n")
	if c.Capabilities.ContextWindow != 64000 || c.Capabilities.MaxOutput != 0 || c.Evidence[CapText].Source != SourceStatic {
		t.Fatalf("numeric context: %+v", c)
	}
	s.LearnNumeric("n", 0, 4096)
	if s.Get("n").Capabilities.MaxOutput != 4096 || s.Get("n").Capabilities.ContextWindow != 64000 {
		t.Fatalf("numeric max: %+v", s.Get("n").Capabilities)
	}
	// LearnProtocol: empty no-op.
	s.LearnProtocol("n", "")
	s.LearnProtocol("n", "gemini")
	if s.Get("n").Capabilities.NativeProtocol != "gemini" {
		t.Fatalf("protocol: %+v", s.Get("n").Capabilities)
	}
	// Seed: unknown skipped, verified wins, numerics/protocol adopted,
	// key adopted only when empty.
	s2 := NewStore()
	s2.LearnSuccess("d", CapTools, SourceProbe, "verified", "k1")
	s2.Seed("d", ModelCapabilities{Tools: Unsupported, Vision: Supported, Text: UnknownSupport, ContextWindow: 1000, MaxOutput: 50, NativeProtocol: "openai_chat"}, SourceStatic, "seed", "k2")
	c = s2.Get("d")
	if c.Capabilities.Tools != Supported || c.Capabilities.Vision != Supported || c.Capabilities.Text != UnknownSupport {
		t.Fatalf("seed vs verified: %+v", c.Capabilities)
	}
	if c.Capabilities.ContextWindow != 1000 || c.Capabilities.MaxOutput != 50 || c.Capabilities.NativeProtocol != "openai_chat" || c.InvalidationKey != "k1" {
		t.Fatalf("seed numerics: %+v", c)
	}
	if c.Evidence[CapVision].Confidence != ConfidenceLow || c.Evidence[CapVision].Source != SourceStatic {
		t.Fatalf("seed evidence: %+v", c.Evidence[CapVision])
	}
	// Drop / Reset / Snapshot / Get zero.
	s2.Drop("d")
	if s2.Count() != 0 {
		t.Fatalf("drop")
	}
	s2.LearnSuccess("a", CapText, SourceRuntime, "", "")
	s2.LearnSuccess("b", CapText, SourceRuntime, "", "")
	snap := s2.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot: %d", len(snap))
	}
	s2.Reset()
	if s2.Count() != 0 || s2.Get("a").Capabilities.Text != UnknownSupport {
		t.Fatalf("reset/get zero")
	}
	s2.SetRepair("a", "dropped temp")
	s2.SetIssue("a", "issue")
	if s2.Get("a").LastRepair != "dropped temp" || s2.Get("a").LastCompatibilityIssue != "issue" {
		t.Fatalf("repair/issue: %+v", s2.Get("a"))
	}
	if got := InvalidationKey("u", "d", "m", "s"); got != "u|d|m|s" {
		t.Fatalf("invalidation key: %q", got)
	}
	// Support JSON round-trip incl. legacy booleans.
	var sup Support
	for _, tc := range []struct {
		in   string
		want Support
	}{
		{`"supported"`, Supported}, {`"pass"`, Supported}, {`true`, Supported}, {`1`, Supported},
		{`"unsupported"`, Unsupported}, {`"fail"`, Unsupported}, {`false`, Unsupported}, {`0`, Unsupported},
		{`"other"`, UnknownSupport},
	} {
		if err := sup.UnmarshalJSON([]byte(tc.in)); err != nil || sup != tc.want {
			t.Fatalf("unmarshal %s -> %v %v", tc.in, sup, err)
		}
	}
	b, _ := Supported.MarshalJSON()
	if string(b) != `"supported"` {
		t.Fatalf("marshal: %s", b)
	}
	if Supported.String() != "supported" || Unsupported.String() != "unsupported" || UnknownSupport.String() != "unknown" {
		t.Fatalf("strings")
	}
}

func TestMission95IneligibleFor(t *testing.T) {
	s := NewStore()
	s.LearnUnsupported("d", CapTools, SourceProbe, "", "")
	s.LearnUnsupported("d", CapVision, SourceProbe, "", "")
	s.LearnUnsupported("d", CapStreaming, SourceProbe, "", "")
	s.LearnUnsupported("d", CapReasoning, SourceProbe, "", "")
	s.LearnUnsupported("d", CapStructuredOutput, SourceProbe, "", "")
	// LearnUnsupported overwrites per call on the same contract, so
	// build the matrix directly via Seed on a fresh store instead.
	s = NewStore()
	seed := ModelCapabilities{Tools: Unsupported, ToolChoiceRequired: Unsupported, Vision: Unsupported, Streaming: Unsupported, Reasoning: Unsupported, StructuredOutput: Unsupported, JSONObject: Unsupported, JSONSchema: Unsupported}
	s.Seed("d", seed, SourceStatic, "", "")
	cases := []struct {
		req RequirementProfile
		cap string
		bad bool
	}{
		{RequirementProfile{Tools: true}, CapTools, true},
		{RequirementProfile{NeedsTool: true}, CapToolChoiceRequired, true},
		{RequirementProfile{Vision: true}, CapVision, true},
		{RequirementProfile{Streaming: true}, CapStreaming, true},
		{RequirementProfile{Reasoning: true}, CapReasoning, true},
		{RequirementProfile{StructuredOutput: true}, CapStructuredOutput, true},
		{RequirementProfile{}, "", false},
	}
	for _, tc := range cases {
		cap, bad := s.IneligibleFor("d", tc.req)
		if cap != tc.cap || bad != tc.bad {
			t.Fatalf("req %+v -> %q %v want %q %v", tc.req, cap, bad, tc.cap, tc.bad)
		}
	}
	// NeedsTool via tools alone (choice supported).
	s2 := NewStore()
	s2.Seed("d", ModelCapabilities{Tools: Unsupported, ToolChoiceRequired: Supported}, SourceStatic, "", "")
	if cap, bad := s2.IneligibleFor("d", RequirementProfile{NeedsTool: true}); cap != CapToolChoiceRequired || !bad {
		t.Fatalf("needsTool via tools: %q %v", cap, bad)
	}
	// Structured output via both JSON forms unsupported, even when the
	// umbrella flag is supported.
	s3 := NewStore()
	s3.Seed("d", ModelCapabilities{StructuredOutput: Supported, JSONObject: Unsupported, JSONSchema: Unsupported}, SourceStatic, "", "")
	if cap, bad := s3.IneligibleFor("d", RequirementProfile{StructuredOutput: true}); cap != CapStructuredOutput || !bad {
		t.Fatalf("structured via json forms: %q %v", cap, bad)
	}
	s3.Seed("e", ModelCapabilities{StructuredOutput: Supported, JSONObject: Supported, JSONSchema: Unsupported}, SourceStatic, "", "")
	if _, bad := s3.IneligibleFor("e", RequirementProfile{StructuredOutput: true}); bad {
		t.Fatalf("one json form supported should be eligible")
	}
	// UNKNOWN never blocks.
	if _, bad := s.IneligibleFor("missing", RequirementProfile{Tools: true, Vision: true}); bad {
		t.Fatalf("unknown should not block")
	}
}

func TestMission95ErrorFields(t *testing.T) {
	code, typ, msg := providerErrorFields(nil)
	if code != "" || typ != "" || msg != "" {
		t.Fatalf("empty: %q %q %q", code, typ, msg)
	}
	code, typ, msg = providerErrorFields([]byte(`{bad`))
	if msg != `{bad` {
		t.Fatalf("invalid json: %q", msg)
	}
	code, typ, msg = providerErrorFields([]byte(`{"code":" BAD_CODE ","type":"Auth ","message":"m","detail":"d","title":"t"}`))
	if code != "bad_code" || typ != "auth" || msg != "m" {
		t.Fatalf("top-level: %q %q %q", code, typ, msg)
	}
	_, _, msg = providerErrorFields([]byte(`{"detail":"only-detail"}`))
	if msg != "only-detail" {
		t.Fatalf("detail fallback: %q", msg)
	}
	_, _, msg = providerErrorFields([]byte(`{"title":"Some Title"}`))
	if msg != "Some Title" {
		t.Fatalf("title fallback: %q", msg)
	}
	_, _, msg = providerErrorFields([]byte(`{"title":"gone"}`))
	if msg != `{"title":"gone"}` {
		t.Fatalf("gone title must not be used: %q", msg)
	}
	code, typ, msg = providerErrorFields([]byte(`{"code":"outer","error":{"code":"inner","type":"inner_type","message":"inner msg"}}`))
	if code != "inner" || typ != "inner_type" || msg != "inner msg" {
		t.Fatalf("nested override: %q %q %q", code, typ, msg)
	}
	_, _, msg = providerErrorFields([]byte(`{"error":"plain string error"}`))
	if msg != "plain string error" {
		t.Fatalf("plain nested: %q", msg)
	}
	_, _, msg = providerErrorFields([]byte(`{"error":{"code":"x"}}`))
	if msg != `{"error":{"code":"x"}}` {
		t.Fatalf("nested without message: %q", msg)
	}
	_, _, msg = providerErrorFields([]byte(`{}`))
	if msg != `{}` {
		t.Fatalf("empty object: %q", msg)
	}
}

func TestMission95ClassifyUpstream(t *testing.T) {
	// Model lifecycle classes.
	c := ClassifyUpstreamError(400, []byte(`{"error":{"code":"model_eol","message":"model reached end of life"}}`))
	if c.Class != ClassModelRetired || c.RetryableSamePayload {
		t.Fatalf("retired: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"code":"model_unavailable","message":"model unavailable"}}`))
	if c.Class != ClassModelTemporarilyUnavailable || !c.RetryableSamePayload {
		t.Fatalf("temp unavailable: %+v", c)
	}
	c = ClassifyUpstreamError(410, []byte(`{"error":{"message":"this model is retired"}}`))
	if c.Class != ClassModelRetired {
		t.Fatalf("410 retired: %+v", c)
	}
	// Status classes.
	c = ClassifyUpstreamError(401, []byte(`{"error":{"message":"invalid api key"}}`))
	if c.Class != ClassInvalidKey {
		t.Fatalf("401 key: %+v", c)
	}
	c = ClassifyUpstreamError(401, []byte(`{"error":{"message":"something else"}}`))
	if c.Class != ClassAuthError {
		t.Fatalf("401 auth: %+v", c)
	}
	c = ClassifyUpstreamError(403, []byte(`{"error":{"message":"quota exceeded"}}`))
	if c.Class != ClassQuotaExhausted || c.RetryableSamePayload {
		t.Fatalf("403 quota: %+v", c)
	}
	c = ClassifyUpstreamError(403, []byte(`{"error":{"message":"forbidden"}}`))
	if c.Class != ClassAuthError {
		t.Fatalf("403 auth: %+v", c)
	}
	c = ClassifyUpstreamError(402, nil)
	if c.Class != ClassQuotaExhausted {
		t.Fatalf("402: %+v", c)
	}
	c = ClassifyUpstreamError(429, nil)
	if c.Class != ClassRateLimit || !c.RetryableSamePayload {
		t.Fatalf("429: %+v", c)
	}
	c = ClassifyUpstreamError(504, nil)
	if c.Class != ClassTimeout || !c.RetryableSamePayload {
		t.Fatalf("504: %+v", c)
	}
	c = ClassifyUpstreamError(529, nil)
	if c.Class != ClassUpstreamOverload {
		t.Fatalf("529: %+v", c)
	}
	c = ClassifyUpstreamError(404, []byte(`{"error":{"message":"model xyz not found"}}`))
	if c.Class != ClassModelNotFound {
		t.Fatalf("404 model: %+v", c)
	}
	c = ClassifyUpstreamError(404, []byte(`{"error":{"message":"unknown url"}}`))
	if c.Class != ClassEndpointNotFound {
		t.Fatalf("404 endpoint: %+v", c)
	}
	c = ClassifyUpstreamError(410, []byte(`{"error":{"message":"gone"}}`))
	if c.Class != ClassEndpointNotFound {
		t.Fatalf("410 endpoint: %+v", c)
	}
	c = ClassifyUpstreamError(500, nil)
	if c.Class != ClassUpstreamInternal || !c.RetryableSamePayload {
		t.Fatalf("500: %+v", c)
	}
	c = ClassifyUpstreamError(418, nil)
	if c.Class != ClassUnknown {
		t.Fatalf("418: %+v", c)
	}
	// Client-error family via message patterns.
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"maximum context length is 8192 tokens"}}`))
	if c.Class != ClassContextOverflow || !c.CallerError {
		t.Fatalf("context: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"vision images not supported"}}`))
	if c.Class != ClassUnsupportedVision || c.Capability != CapVision || c.Parameter != "image" {
		t.Fatalf("vision: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"tools are not supported"}}`))
	if c.Class != ClassUnsupportedTools || c.Parameter != "tools" {
		t.Fatalf("tools: %+v", c)
	}
	c = ClassifyUpstreamError(422, []byte(`{"error":{"message":"reasoning is not supported"}}`))
	if c.Class != ClassUnsupportedReasoning || c.Parameter != "reasoning" {
		t.Fatalf("reasoning: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"response_format not supported"}}`))
	if c.Class != ClassUnsupportedParameter || c.Capability != CapStructuredOutput {
		t.Fatalf("structured param: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"unsupported parameter \"temperature\""}}`))
	if c.Class != ClassUnsupportedParameter || c.Capability != CapTemperature || !c.CapabilityFailure {
		t.Fatalf("temperature param: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"unsupported parameter \"tools\""}}`))
	if c.Class != ClassUnsupportedTools {
		t.Fatalf("tools param: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"unsupported parameter \"reasoning_effort\""}}`))
	if c.Class != ClassUnsupportedReasoning {
		t.Fatalf("reasoning_effort param: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"unsupported parameter \"image_url\""}}`))
	if c.Class != ClassUnsupportedVision {
		t.Fatalf("image_url param: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"unsupported parameter \"frobnicate\""}}`))
	if c.Class != ClassUnsupportedParameter || c.Capability != "" {
		t.Fatalf("unknown param: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"invalid request schema: bad body"}}`))
	if c.Class != ClassInvalidRequestSchema || !c.CallerError {
		t.Fatalf("schema: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"no such model abc"}}`))
	if c.Class != ClassModelNotFound {
		t.Fatalf("model phrase: %+v", c)
	}
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"weird failure"}}`))
	if c.Class != ClassInvalidRequestSchema {
		t.Fatalf("fallback: %+v", c)
	}
	// Structured param from body fills Parameter when the message has none.
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"weird failure","param":"temperature"}}`))
	if c.Parameter != "temperature" {
		t.Fatalf("structured param: %+v", c)
	}
	// Message truncation at 512.
	long := strings.Repeat("x", 600)
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"`+long+`"}}`))
	if len(c.Message) != 512 {
		t.Fatalf("truncate: %d", len(c.Message))
	}
	// Parameter extraction helpers.
	if got := parameterFromBody(`unsupported parameter "request.temperature"`); got != "temperature" {
		t.Fatalf("request prefix: %q", got)
	}
	if got := parameterFromBody(`unsupported parameter "generationConfig.topP"`); got != "topp" {
		t.Fatalf("generationconfig prefix: %q", got)
	}
	if got := parameterFromBody(`"model" is not supported`); got != "" {
		t.Fatalf("excluded word: %q", got)
	}
	if got := parameterFromBody(`"x" is supported`); got != "" {
		t.Fatalf("short param: %q", got)
	}
	if got := structuredParamFromBody([]byte(`{"error":{"param":"a"},"param":"b","parameter":"c","field":"d"}`)); got != "a" {
		t.Fatalf("structured priority: %q", got)
	}
	if got := structuredParamFromBody([]byte(`{"field":"myfield"}`)); got != "myfield" {
		t.Fatalf("field fallback: %q", got)
	}
	if got := structuredParamFromBody([]byte(`{bad`)); got != "" {
		t.Fatalf("structured bad json: %q", got)
	}
	if got := messageFromBody([]byte(`{"error":{"message":"nested"},"message":"top"}`)); got != "nested" {
		t.Fatalf("message nested: %q", got)
	}
	if got := messageFromBody([]byte(`{"error":"plain","message":"top"}`)); got != "plain" {
		t.Fatalf("message plain: %q", got)
	}
	if got := messageFromBody([]byte(`{"message":"top"}`)); got != "top" {
		t.Fatalf("message top: %q", got)
	}
	if got := messageFromBody([]byte(`not json`)); got != "not json" {
		t.Fatalf("message fallback: %q", got)
	}
	if got := messageFromBody(nil); got != "" {
		t.Fatalf("message empty: %q", got)
	}
}

func TestMission95PolicyAndTokens(t *testing.T) {
	// Capability failure carve-out.
	p := Classified{Class: ClassUnsupportedTools, CapabilityFailure: true}.Policy()
	if p.Repairable != true || p.Failover || p.ErrorType != string(ClassUnsupportedTools) {
		t.Fatalf("cap policy: %+v", p)
	}
	policies := []struct {
		cls  ErrorClass
		want PolicyFromError
	}{
		{ClassAuthError, PolicyFromError{ErrorType: "provider_auth_failed", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}},
		{ClassQuotaExhausted, PolicyFromError{ErrorType: "provider_billing", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}},
		{ClassRateLimit, PolicyFromError{ErrorType: "provider_rate_limited", Failover: true, QuarantineDeployment: true, SignalProvider: true, HardCooldown: true}},
		{ClassModelRetired, PolicyFromError{ErrorType: "model_retired", Failover: true}},
		{ClassModelTemporarilyUnavailable, PolicyFromError{ErrorType: "model_temporarily_unavailable", Failover: true, QuarantineDeployment: true}},
		{ClassModelNotFound, PolicyFromError{ErrorType: "provider_model_not_found", Failover: true, QuarantineDeployment: true}},
		{ClassEndpointNotFound, PolicyFromError{ErrorType: "provider_endpoint_not_found", Failover: true, QuarantineDeployment: true}},
		{ClassTimeout, PolicyFromError{ErrorType: "provider_timeout", Failover: true, QuarantineDeployment: true, SignalProvider: true}},
		{ClassUpstreamOverload, PolicyFromError{ErrorType: "provider_overloaded", Failover: true, QuarantineDeployment: true, SignalProvider: true}},
		{ClassUpstreamInternal, PolicyFromError{ErrorType: "provider_server_error", Failover: true, QuarantineDeployment: true, SignalProvider: true}},
		{ClassContextOverflow, PolicyFromError{ErrorType: "context_overflow", Failover: true}},
		{ClassInvalidRequestSchema, PolicyFromError{ErrorType: "caller_invalid_request"}},
		{ClassNetworkError, PolicyFromError{ErrorType: "provider_connection_failed", Failover: true, QuarantineDeployment: true, SignalProvider: true}},
		{ClassUnknown, PolicyFromError{ErrorType: string(ClassUnknown), Failover: true}},
	}
	for _, tc := range policies {
		if got := (Classified{Class: tc.cls}.Policy()); got != tc.want {
			t.Fatalf("policy %v -> %+v want %+v", tc.cls, got, tc.want)
		}
	}
	// HTTPStatus.
	if got := (Classified{Class: ClassContextOverflow, StatusCode: 400}).HTTPStatus(); got != 400 {
		t.Fatalf("overflow status: %d", got)
	}
	if got := (Classified{Class: ClassRateLimit}).HTTPStatus(); got != 429 {
		t.Fatalf("ratelimit status: %d", got)
	}
	if got := (Classified{Class: ClassUpstreamOverload}).HTTPStatus(); got != 503 {
		t.Fatalf("overload status: %d", got)
	}
	if got := (Classified{Class: ClassUnknown, StatusCode: 418}).HTTPStatus(); got != 418 {
		t.Fatalf("passthrough status: %d", got)
	}
	if got := (Classified{Class: ClassUnknown}).HTTPStatus(); got != 502 {
		t.Fatalf("default status: %d", got)
	}
	// Transport errors.
	ct := ClassifyTransportError(nil)
	if ct.Class != ClassNetworkError || !ct.RetryableSamePayload {
		t.Fatalf("nil transport: %+v", ct)
	}
	ct = ClassifyTransportError(errors.New("context deadline exceeded"))
	if ct.Class != ClassTimeout {
		t.Fatalf("deadline: %+v", ct)
	}
	ct = ClassifyTransportError(errors.New("context canceled"))
	if ct.Class != ClassNetworkError || ct.RetryableSamePayload {
		t.Fatalf("canceled: %+v", ct)
	}
	ct = ClassifyTransportError(errors.New("connection reset by peer"))
	if ct.Class != ClassNetworkError {
		t.Fatalf("reset: %+v", ct)
	}
	ct = ClassifyTransportError(errors.New("something odd"))
	if ct.Class != ClassNetworkError || !ct.RetryableSamePayload {
		t.Fatalf("other transport: %+v", ct)
	}
	if got := ClassifyMalformedResponse("bad body"); got.Class != ClassMalformedResponse || !got.RetryableSamePayload {
		t.Fatalf("malformed: %+v", got)
	}
	if got := ClassifyStreamProtocolError("bad frame"); got.Class != ClassStreamProtocolError || !got.RetryableSamePayload {
		t.Fatalf("stream protocol: %+v", got)
	}
	// Context token parsing.
	limit, req := ParseContextTokens("maximum context length is 8192 tokens, however you requested 9000 tokens")
	if limit != 8192 || req != 9000 {
		t.Fatalf("tokens: %d %d", limit, req)
	}
	limit, req = ParseContextTokens("limit is 128k tokens")
	if limit != 128000 || req != 0 {
		t.Fatalf("k tokens: %d %d", limit, req)
	}
	limit, req = ParseContextTokens("no numbers here")
	if limit != 0 || req != 0 {
		t.Fatalf("no tokens: %d %d", limit, req)
	}
	limit, req = ParseContextTokens("8192 tokens and 8192 tokens again")
	if limit != 8192 || req != 0 {
		t.Fatalf("same twice: %d %d", limit, req)
	}
	// Capability labels.
	if got := (Classified{Class: ClassUnknown}).CapabilityLabel(); got != string(ClassUnknown) {
		t.Fatalf("label class: %q", got)
	}
	if got := (Classified{Class: ClassUnsupportedTools, Capability: CapTools, Parameter: "tools"}).CapabilityLabel(); got != CapTools {
		t.Fatalf("label same: %q", got)
	}
	if got := (Classified{Class: ClassUnsupportedTools, Capability: CapTools, Parameter: "tool_choice"}).CapabilityLabel(); got != "tools/tool_choice" {
		t.Fatalf("label pair: %q", got)
	}
	if got := (Classified{Class: ClassUnsupportedParameter, Parameter: "frobnicate"}).CapabilityLabel(); got != "frobnicate" {
		t.Fatalf("label param: %q", got)
	}
}

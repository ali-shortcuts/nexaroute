package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyUnsupportedTemperature(t *testing.T) {
	body := []byte(`{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
	cls := ClassifyUpstreamError(http.StatusBadRequest, body)
	if !cls.CapabilityFailure {
		t.Fatalf("expected capability failure, got %+v", cls)
	}
	if cls.Capability != CapTemperature {
		t.Fatalf("capability=%q want temperature", cls.Capability)
	}
	policy := cls.Policy()
	if !policy.Repairable || policy.QuarantineDeployment || policy.SignalProvider {
		t.Fatalf("capability failure must be repairable and health-neutral: %+v", policy)
	}
}

func TestClassifyUnknownParameterReasoningEffort(t *testing.T) {
	body := []byte(`{"error":{"message":"Unknown parameter: reasoning_effort","param":"reasoning_effort"}}`)
	cls := ClassifyUpstreamError(http.StatusBadRequest, body)
	if cls.Class != ClassUnsupportedReasoning && cls.Class != ClassUnsupportedParameter {
		t.Fatalf("class=%q", cls.Class)
	}
	if cls.Parameter != "reasoning_effort" {
		t.Fatalf("parameter=%q", cls.Parameter)
	}
	if !cls.CapabilityFailure {
		t.Fatal("expected capability failure")
	}
}

func TestClassifyContextOverflowIsNotCapabilityFailure(t *testing.T) {
	body := []byte(`{"error":{"message":"This model's maximum context length is 8192 tokens, however you requested 9000 tokens"}}`)
	cls := ClassifyUpstreamError(http.StatusBadRequest, body)
	if cls.Class != ClassContextOverflow {
		t.Fatalf("class=%q want context_overflow", cls.Class)
	}
	if cls.CapabilityFailure {
		t.Fatal("context overflow is not a capability failure")
	}
	policy := cls.Policy()
	if !policy.Failover || policy.QuarantineDeployment {
		t.Fatalf("context overflow should fail over without quarantine: %+v", policy)
	}
}

func TestClassifyStatusFamilies(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrorClass
	}{
		{http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, ClassInvalidKey},
		{http.StatusPaymentRequired, `{"error":{"message":"billing"}}`, ClassQuotaExhausted},
		{http.StatusTooManyRequests, `{"error":{"message":"rate limit exceeded"}}`, ClassRateLimit},
		{http.StatusNotFound, `{"error":{"message":"model not found: gpt-xyz"}}`, ClassModelNotFound},
		{http.StatusNotFound, `{"error":{"message":"invalid url path"}}`, ClassEndpointNotFound},
		{http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`, ClassUpstreamOverload},
		{http.StatusInternalServerError, `{"error":{"message":"internal error"}}`, ClassUpstreamInternal},
	}
	for _, tc := range cases {
		cls := ClassifyUpstreamError(tc.status, []byte(tc.body))
		if cls.Class != tc.want {
			t.Fatalf("status=%d class=%q want=%q", tc.status, cls.Class, tc.want)
		}
	}
	cls := ClassifyUpstreamError(http.StatusInternalServerError, []byte(`{"error":{"message":"boom"}}`))
	if cls.CapabilityFailure || !cls.RetryableSamePayload {
		t.Fatalf("generic 500 must never be a capability failure: %+v", cls)
	}
}

func TestRepairRenamesMaxCompletionTokens(t *testing.T) {
	payload := []byte(`{"model":"m","max_completion_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`)
	cls := Classified{
		Class: ClassUnsupportedParameter, Parameter: "max_completion_tokens",
		Capability: CapMaxCompletionTokens, CapabilityFailure: true,
	}
	repaired, plan, ok := Repair(cls, payload, Dialects()[DialectGenericOpenAI], RequirementProfile{})
	if !ok {
		t.Fatal("expected rename repair")
	}
	var m map[string]any
	if err := json.Unmarshal(repaired, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["max_completion_tokens"]; has {
		t.Fatal("max_completion_tokens should be gone")
	}
	if m["max_tokens"].(float64) != 4096 {
		t.Fatalf("max_tokens=%v want 4096", m["max_tokens"])
	}
	if len(plan.Rules) != 1 || plan.Rules[0].Name != "rename:max_completion_tokens->max_tokens" {
		t.Fatalf("plan rules=%v", plan.Rules)
	}
}

func TestRepairDropsOptionalSamplingParam(t *testing.T) {
	payload := []byte(`{"model":"m","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
	cls := Classified{
		Class: ClassUnsupportedParameter, Parameter: "temperature",
		Capability: CapTemperature, CapabilityFailure: true,
	}
	repaired, _, ok := Repair(cls, payload, Dialects()[DialectGenericOpenAI], RequirementProfile{})
	if !ok {
		t.Fatal("expected temperature removal")
	}
	if strings.Contains(string(repaired), "temperature") {
		t.Fatal("temperature still present")
	}
}

func TestRepairRefusesToStripSemanticsCriticalFields(t *testing.T) {
	payload := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],"messages":[]}`)
	cls := Classified{
		Class: ClassUnsupportedTools, Parameter: "tools",
		Capability: CapTools, CapabilityFailure: true,
	}
	_, _, ok := Repair(cls, payload, Dialects()[DialectGenericOpenAI], RequirementProfile{Tools: true})
	if ok {
		t.Fatal("must never repair by stripping tools for agent traffic")
	}
}

func TestRepairRefusesReasoningDropWhenRequested(t *testing.T) {
	payload := []byte(`{"model":"m","reasoning_effort":"high","messages":[]}`)
	cls := Classified{
		Class: ClassUnsupportedReasoning, Parameter: "reasoning_effort",
		Capability: CapReasoning, CapabilityFailure: true,
	}
	_, _, ok := Repair(cls, payload, Dialects()[DialectGenericOpenAI], RequirementProfile{Reasoning: true})
	if ok {
		t.Fatal("must not silently drop explicitly requested reasoning")
	}
}

func TestRepairDropsStreamOptions(t *testing.T) {
	payload := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	cls := Classified{
		Class: ClassUnsupportedParameter, Parameter: "stream_options",
		Capability: CapStreaming, CapabilityFailure: true,
	}
	repaired, _, ok := Repair(cls, payload, Dialects()[DialectGenericOpenAI], RequirementProfile{})
	if !ok {
		t.Fatal("expected stream_options drop")
	}
	if strings.Contains(string(repaired), "stream_options") {
		t.Fatal("stream_options still present")
	}
}

func TestSanitizerProactiveRemoval(t *testing.T) {
	store := NewStore()
	contract := Contract{}
	contract.Capabilities.Temperature = Unsupported
	payload := []byte(`{"model":"m","temperature":0.5,"messages":[]}`)
	res, err := Sanitize(payload, contract, Dialects()[DialectGenericOpenAI], RequirementProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || len(res.Removed) != 1 || res.Removed[0] != "temperature" {
		t.Fatalf("sanitize result=%+v", res)
	}
	_ = store
}

func TestStoreTriStateAndInvalidation(t *testing.T) {
	store := NewStore()
	keyA := InvalidationKey("https://x", "nvidia_nim", "m1", "k1")
	store.Seed("dep", ModelCapabilities{Text: Supported, Streaming: Supported}, SourceStatic, "test", keyA)
	if got := store.Get("dep").Capabilities.Text; got != Supported {
		t.Fatalf("seed text=%v", got)
	}
	// Runtime learning updates temperature.
	store.LearnSuccess("dep", CapTemperature, SourceRuntime, "ok", keyA)
	if got := store.Get("dep").Capabilities.Temperature; got != Supported {
		t.Fatalf("learned temperature=%v", got)
	}
	store.LearnUnsupported("dep", CapVision, SourceProbe, "probe verdict", keyA)
	if got := store.Get("dep").Capabilities.Vision; got != Unsupported {
		t.Fatalf("learned vision=%v", got)
	}
	// Identity change drops stale contract.
	keyB := InvalidationKey("https://y", "nvidia_nim", "m1", "k1")
	if store.InvalidateIf("dep", keyB) {
		t.Fatal("contract should have been invalidated")
	}
	if got := store.Get("dep").Capabilities.Text; got != UnknownSupport {
		t.Fatalf("dropped contract should be empty, text=%v", got)
	}
}

func TestStoreSeedDoesNotOverwriteEvidence(t *testing.T) {
	store := NewStore()
	key := InvalidationKey("https://x", "d", "m", "k")
	store.LearnUnsupported("dep", CapTemperature, SourceProbe, "verified", key)
	store.Seed("dep", ModelCapabilities{Temperature: Supported}, SourceStatic, "prior", key)
	if got := store.Get("dep").Capabilities.Temperature; got != Unsupported {
		t.Fatalf("seed overwrote verified evidence: %v", got)
	}
}

func TestIneligibleForDistinguishesUnsupportedFromUnknown(t *testing.T) {
	store := NewStore()
	// UNKNOWN never blocks.
	if capability, ineligible := store.IneligibleFor("none", RequirementProfile{Tools: true}); ineligible {
		t.Fatalf("unknown tools blocked: %v", capability)
	}
	key := InvalidationKey("b", "d", "m", "k")
	store.LearnUnsupported("dep", CapTools, SourceProbe, "verified", key)
	if _, ineligible := store.IneligibleFor("dep", RequirementProfile{Tools: true}); !ineligible {
		t.Fatal("verified-unsupported tools must block")
	}
	if _, ineligible := store.IneligibleFor("dep", RequirementProfile{Vision: true}); ineligible {
		t.Fatal("unrelated capability must not block")
	}
}

func TestScorecardStatuses(t *testing.T) {
	full := Contract{Capabilities: ModelCapabilities{
		Text: Supported, Streaming: Supported, Tools: Supported, ParallelToolCalls: Supported,
	}}
	if got := full.Scorecard("healthy"); got.Status != StatusClaudeCodeReady {
		t.Fatalf("status=%q", got.Status)
	}
	noTools := Contract{Capabilities: ModelCapabilities{Text: Supported, Streaming: Supported, Tools: Unsupported}}
	if got := noTools.Scorecard("healthy"); got.Status != StatusChatReady {
		t.Fatalf("status=%q", got.Status)
	}
	// Tools never verified (UNKNOWN) => the deployment is NOT_VERIFIED for
	// agent work: no false capability claims (spec section 28).
	unverified := Contract{Capabilities: ModelCapabilities{Text: Supported, Streaming: Supported}}
	if got := unverified.Scorecard("healthy"); got.Status != StatusNotVerified || got.Tools != UnknownSupport {
		t.Fatalf("status=%q tools=%v", got.Status, got.Tools)
	}
}

// fakeTransport serves scripted responses for probe tests.
type fakeTransport struct {
	resps []fakeResp
	calls int
}

type fakeResp struct {
	status int
	body   string
	sse    bool
}

func (f *fakeTransport) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	if f.calls >= len(f.resps) {
		return nil, context.DeadlineExceeded
	}
	r := f.resps[f.calls]
	f.calls++
	h := http.Header{}
	if r.sse {
		h.Set("Content-Type", "text/event-stream")
	} else {
		h.Set("Content-Type", "application/json")
	}
	return &http.Response{
		StatusCode: r.status, Header: h,
		Body: &stringReaderCloser{Reader: strings.NewReader(r.body)},
	}, nil
}

func (f *fakeTransport) RedactBody(b []byte) []byte { return b }

type stringReaderCloser struct {
	*strings.Reader
}

func (s *stringReaderCloser) Close() error { return nil }

func TestCapabilitySuiteVerdicts(t *testing.T) {
	t.Run("temperature unsupported verdict", func(t *testing.T) {
		ft := &fakeTransport{resps: []fakeResp{
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 400, body: `{"error":{"message":"temperature is not supported"}}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`},
			{status: 200, body: `{"choices":[{"message":{"content":"red"}}]}`},
		}}
		report := RunCapabilitySuite(context.Background(), ft, "dep", "m", "nvidia_nim")
		found := false
		for _, o := range report.Outcomes {
			if o.Capability == CapTemperature {
				found = true
				if o.Verdict != Unsupported {
					t.Fatalf("temperature verdict=%v", o.Verdict)
				}
			}
		}
		if !found {
			t.Fatal("temperature probe missing")
		}
	})
	t.Run("generic 500 stays inconclusive", func(t *testing.T) {
		ft := &fakeTransport{resps: []fakeResp{
			{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`},
			{status: 500, body: `{"error":{"message":"boom"}}`},
		}}
		report := RunCapabilitySuite(context.Background(), ft, "dep", "m", "")
		if report.Outcomes[1].Verdict != UnknownSupport || report.Outcomes[1].Capability != CapSystemMessage {
			t.Fatalf("outcome=%+v", report.Outcomes[1])
		}
	})
}

type responsesRecordingTransport struct {
	payloads [][]byte
}

func (t *responsesRecordingTransport) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	t.payloads = append(t.payloads, append([]byte(nil), payload...))
	var req map[string]any
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	if _, ok := req["messages"]; ok {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:       &stringReaderCloser{Reader: strings.NewReader(`{"error":{"message":"messages must not be sent to Responses"}}`)},
		}, nil
	}
	input, _ := req["input"].([]any)
	hasFunctionOutput := false
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item != nil && item["type"] == "function_call_output" {
			hasFunctionOutput = true
			break
		}
	}
	body := `{"id":"resp_1","status":"completed","model":"m","output":[{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}]}`
	if hasFunctionOutput {
		body = `{"id":"resp_2","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It is 15C and sunny in Paris."}]}],"usage":{"input_tokens":10,"output_tokens":8}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: &stringReaderCloser{Reader: strings.NewReader(body)},
	}, nil
}

func (t *responsesRecordingTransport) RedactBody(b []byte) []byte { return b }

func TestResponsesAgentLoopUsesResponsesWireProtocol(t *testing.T) {
	ft := &responsesRecordingTransport{}
	report := RunAgentLoopSimulationResponses(context.Background(), ft, "dep", "m")
	if !report.OK {
		t.Fatalf("Responses agent loop failed: %+v", report)
	}
	if len(ft.payloads) != 2 {
		t.Fatalf("payload count=%d want 2", len(ft.payloads))
	}
	for i, payload := range ft.payloads {
		var got map[string]any
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload %d malformed: %v", i, err)
		}
		if _, ok := got["messages"]; ok {
			t.Fatalf("payload %d leaked Chat Completions messages: %s", i, payload)
		}
		if got["input"] == nil {
			t.Fatalf("payload %d missing Responses input: %s", i, payload)
		}
	}
	var second map[string]any
	_ = json.Unmarshal(ft.payloads[1], &second)
	items, _ := second["input"].([]any)
	foundOutput := false
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item != nil && item["type"] == "function_call_output" && item["call_id"] == "call_1" {
			foundOutput = true
		}
	}
	if !foundOutput {
		t.Fatalf("second Responses request missing function_call_output: %s", ft.payloads[1])
	}
}

func TestAgentLoopSimulationEndToEnd(t *testing.T) {
	toolCallBody := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`
	continuationBody := `{"choices":[{"message":{"role":"assistant","content":"It is 15C and sunny in Paris."}}],"usage":{"prompt_tokens":10,"completion_tokens":8}}`
	ft := &fakeTransport{resps: []fakeResp{
		{status: 200, body: toolCallBody},
		{status: 200, body: continuationBody},
	}}
	report := RunAgentLoopSimulation(context.Background(), ft, "dep", "m")
	if !report.OK {
		t.Fatalf("agent loop failed: %+v", report)
	}
	if len(report.AgentSteps) != 2 {
		t.Fatalf("steps=%d want 2", len(report.AgentSteps))
	}
	for _, st := range report.AgentSteps {
		if !st.Passed {
			t.Fatalf("step %s failed: %s", st.Step, st.Detail)
		}
	}
}

func TestAgentLoopRejectsNoToolCallModel(t *testing.T) {
	ft := &fakeTransport{resps: []fakeResp{
		{status: 200, body: `{"choices":[{"message":{"content":"The weather is nice."}}]}`},
	}}
	report := RunAgentLoopSimulation(context.Background(), ft, "dep", "m")
	if report.OK {
		t.Fatal("model that never calls the tool must not pass the agent loop")
	}
}

func TestDialectDetection(t *testing.T) {
	d := DetectDialect("n", "openai_compatible", "https://integrate.api.nvidia.com/v1", "")
	if d.Name != DialectNvidiaNIM {
		t.Fatalf("dialect=%q", d.Name)
	}
	d = DetectDialect("n", "openai_compatible", "https://api.deepseek.com/v1", "")
	if d.Name != DialectDeepSeek {
		t.Fatalf("dialect=%q", d.Name)
	}
	d = DetectDialect("n", "openai_compatible", "https://example.com/v1", "openrouter")
	if d.Name != DialectOpenRouter {
		t.Fatalf("explicit override ignored: %q", d.Name)
	}
	d = DetectDialect("n", "anthropic_compatible", "https://example.com", "")
	if d.Protocol != "anthropic" {
		t.Fatalf("protocol=%q", d.Protocol)
	}
}

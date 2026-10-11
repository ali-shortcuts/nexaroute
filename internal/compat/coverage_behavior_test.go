package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCoverageBehaviorSanitizeAndRepairMatrix(t *testing.T) {
	contract := Contract{}
	for _, cap := range []string{CapTemperature, CapTopP, CapSeed, CapReasoningEffort, CapStructuredOutput, CapParallelToolCalls, CapStreaming, CapStop} {
		contract.Capabilities.Set(cap, Unsupported)
	}
	payload := []byte(`{"model":"m","temperature":0.2,"top_p":0.5,"seed":1,"reasoning_effort":"low","response_format":{"type":"json_object"},"parallel_tool_calls":true,"stream_options":{"include_usage":true},"stop":["END"],"tools":[{"type":"function"}],"max_completion_tokens":100}`)
	res, err := Sanitize(payload, contract, Dialects()["openai"], RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("sanitize changed=%v err=%v", res.Changed, err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"temperature", "top_p", "seed", "reasoning_effort", "response_format", "parallel_tool_calls", "stream_options", "stop"} {
		if _, ok := out[key]; ok {
			t.Fatalf("unsupported key %q survived: %s", key, res.Payload)
		}
	}
	if _, ok := out["tools"]; !ok {
		t.Fatal("semantics-critical tools were removed")
	}
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "tool_choice", CapabilityFailure: true}
	before := []byte(`{"model":"m","tool_choice":"required"}`)
	if _, _, ok := Repair(cls, before, Dialects()["openai"], RequirementProfile{NeedsTool: true}); ok {
		t.Fatal("required tool choice was silently repaired")
	}
	cls = Classified{Class: ClassUnsupportedReasoning, CapabilityFailure: true}
	repaired, plan, ok := Repair(cls, []byte(`{"model":"m","reasoning_effort":"low","reasoning_content":"x"}`), Dialects()["openai"], RequirementProfile{})
	if !ok || len(plan.Rules) != 1 || strings.Contains(string(repaired), "reasoning_effort") {
		t.Fatalf("reasoning repair failed: ok=%v plan=%+v payload=%s", ok, plan, repaired)
	}
	if _, _, ok := Repair(Classified{Class: ClassUnsupportedTools, CapabilityFailure: true}, []byte(`{"tools":[]}`), Dialects()["openai"], RequirementProfile{}); ok {
		t.Fatal("tools must not be dropped")
	}
}

func TestCoverageBehaviorClassifierPoliciesAndNonFiniteScorecard(t *testing.T) {
	cases := []struct {
		status            int
		body              string
		class             ErrorClass
		capabilityFailure bool
	}{
		{403, `{"error":{"message":"quota exceeded"}}`, ClassQuotaExhausted, false},
		{408, `{"error":{"message":"timeout"}}`, ClassTimeout, false},
		{529, `overloaded`, ClassUpstreamOverload, false},
		{410, `{"error":{"message":"model retired"}}`, ClassModelRetired, false},
		{400, `{"error":{"message":"temperature is not supported"}}`, ClassUnsupportedParameter, true},
	}
	for _, tc := range cases {
		got := ClassifyUpstreamError(tc.status, []byte(tc.body))
		if got.Class != tc.class || got.CapabilityFailure != tc.capabilityFailure {
			t.Fatalf("status=%d got=%+v want class=%s capability=%v", tc.status, got, tc.class, tc.capabilityFailure)
		}
		if len(got.Message) > 512 {
			t.Fatalf("classifier message unbounded: %d", len(got.Message))
		}
	}
	long := ClassifyUpstreamError(500, []byte(strings.Repeat("x", 5000)))
	if len(long.Message) > 512 {
		t.Fatalf("long message not bounded: %d", len(long.Message))
	}
	c := Contract{}
	c.Capabilities.Set(CapText, Supported)
	c.Capabilities.Set(CapTools, Supported)
	c.Capabilities.ContextWindow = 32000
	c.Capabilities.Set(CapStructuredOutput, UnknownSupport)
	if got := c.Scorecard("healthy"); got.Status == "" {
		t.Fatal("scorecard status missing")
	}
	if _, bad := NewStore().IneligibleFor("missing", RequirementProfile{NeedsTool: true}); bad {
		t.Fatal("missing contract should remain unknown, not ineligible")
	}
}

func TestCoverageBehaviorStructuredMessagesAndPolicyMapping(t *testing.T) {
	cases := []struct {
		body, want string
	}{
		{`{"error":{"message":"bad request","type":"invalid_request_error","code":"x"}}`, "bad request"},
		{`{"message":"top-level message"}`, "top-level message"},
		{`plain upstream text`, "plain upstream text"},
	}
	for _, tc := range cases {
		if got := messageFromBody([]byte(tc.body)); got != tc.want {
			t.Fatalf("messageFromBody(%q)=%q want %q", tc.body, got, tc.want)
		}
	}
	for _, tc := range []struct {
		class ErrorClass
		kind  string
		fail  bool
	}{
		{ClassAuthError, "provider_auth_failed", true},
		{ClassQuotaExhausted, "provider_billing", true},
		{ClassModelRetired, "model_retired", true},
		{ClassInvalidRequestSchema, "caller_invalid_request", false},
		{ClassContextOverflow, "context_overflow", true},
	} {
		p := (Classified{Class: tc.class}).Policy()
		if p.ErrorType != tc.kind || p.Failover != tc.fail {
			t.Fatalf("policy for %s=%+v", tc.class, p)
		}
	}
}

func TestResponsesAndAnthropicCapabilitySuitesUseProtocolAdapters(t *testing.T) {
	responses := &responsesRecordingTransport{}
	responsesReport := RunCapabilitySuiteResponses(context.Background(), responses, "dep", "model")
	if responsesReport.Deployment != "dep" || responsesReport.Dialect != "openai_responses" || len(responsesReport.Outcomes) == 0 {
		t.Fatalf("responses report=%+v", responsesReport)
	}
	for _, payload := range responses.payloads {
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		if body["input"] == nil || body["messages"] != nil {
			t.Fatalf("invalid Responses payload=%s", payload)
		}
	}

	anthropic := &anthropicCoverageTransport{}
	anthropicReport := RunCapabilitySuiteAnthropic(context.Background(), anthropic, "dep", "model")
	if anthropicReport.Deployment != "dep" || anthropicReport.Level != "capability" || len(anthropicReport.Outcomes) == 0 {
		t.Fatalf("anthropic report=%+v", anthropicReport)
	}
	if anthropic.calls == 0 {
		t.Fatal("Anthropic suite made no transport calls")
	}
}

type anthropicCoverageTransport struct{ calls int }

func (t *anthropicCoverageTransport) Do(_ context.Context, payload []byte, stream bool, _ http.Header) (*http.Response, error) {
	t.calls++
	if stream {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &stringReaderCloser{Reader: strings.NewReader("data: {}\n\n")}}, nil
	}
	body := `{"content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	if strings.Contains(string(payload), `"tools"`) {
		body = `{"content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: &stringReaderCloser{Reader: strings.NewReader(body)}}, nil
}

func (t *anthropicCoverageTransport) RedactBody(b []byte) []byte { return b }

func TestResponsesProbeConversionHandlesContentVariantsAndInvalidBodies(t *testing.T) {
	for _, input := range []any{
		"plain text",
		[]any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
			map[string]any{"type": "unknown"},
		},
		nil,
	} {
		got := responsesProbeContent(input)
		if input != nil && got == nil {
			t.Fatalf("content variant %T was dropped unexpectedly", input)
		}
	}
	if _, err := chatProbePayloadToResponses([]byte("{")); err == nil {
		t.Fatal("invalid chat probe JSON accepted")
	}
	if _, err := responsesProbeBodyToChat([]byte("{")); err == nil {
		t.Fatal("invalid Responses body accepted")
	}
	if got := firstText(completionShape{}); got != "" {
		t.Fatalf("empty completion text=%q", got)
	}
}

func TestCoverageProbeVerdictsAndResponsesBodyConversion(t *testing.T) {
	cases := []struct {
		name    string
		resp    fakeResp
		cap     string
		want    Support
		wantErr bool
	}{
		{"text", fakeResp{status: 200, body: `{"choices":[{"message":{"content":"OK"}}]}`}, CapText, Supported, false},
		{"tool", fakeResp{status: 200, body: `{"choices":[{"message":{"tool_calls":[{"id":"x"}]}}]}`}, CapTools, Supported, false},
		{"unsupported", fakeResp{status: 400, body: `{"error":{"message":"temperature is not supported"}}`}, CapTemperature, Unsupported, false},
		{"server error", fakeResp{status: 500, body: `{"error":{"message":"boom"}}`}, CapText, UnknownSupport, true},
		{"malformed", fakeResp{status: 200, body: `not-json`}, CapText, UnknownSupport, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := &fakeTransport{resps: []fakeResp{tc.resp}}
			expect := ExpectText
			if tc.cap == CapTools {
				expect = ExpectToolCall
			}
			got, err := runProbe(context.Background(), ft, []byte(`{"model":"m"}`), false, tc.cap, expect)
			if got.Verdict != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("outcome=%+v err=%v", got, err)
			}
		})
	}
	stream := &fakeTransport{resps: []fakeResp{{status: 200, sse: true}}}
	got, err := runProbe(context.Background(), stream, nil, true, CapStreaming, ExpectText)
	if err != nil || got.Verdict != Supported {
		t.Fatalf("stream outcome=%+v err=%v", got, err)
	}
	converted, err := responsesProbeBodyToChat([]byte(`{"id":"r","model":"m","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3}}`))
	if err != nil || !strings.Contains(string(converted), "tool_calls") || !strings.Contains(string(converted), "hello") {
		t.Fatalf("converted body=%s err=%v", converted, err)
	}
}

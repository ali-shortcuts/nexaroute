package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type issue663ErrReader struct{}

func (issue663ErrReader) Read([]byte) (int, error) { return 0, errors.New("issue663 read boom") }
func (issue663ErrReader) Close() error             { return nil }

type issue663ReadErrTransport struct{}

func (issue663ReadErrTransport) Do(context.Context, []byte, bool, http.Header) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: issue663ErrReader{}}, nil
}
func (issue663ReadErrTransport) RedactBody(b []byte) []byte { return b }

func TestIssue663ProvidersLearnUnsupportedRejectsInvalidCapability(t *testing.T) {
	s := NewStore()
	s.LearnUnsupported("dep", "not-a-capability", SourceProbe, "detail", "k1")
	if got := s.Get("dep"); len(got.Evidence) != 0 {
		t.Fatalf("invalid capability created evidence: %+v", got)
	}
	// The valid-capability path still records, proving the guard is about
	// validity, not about the store being inert.
	s.LearnUnsupported("dep", CapText, SourceProbe, "detail", "k1")
	if got := s.Get("dep"); got.Capabilities.Get(CapText) != Unsupported || len(got.Evidence) == 0 {
		t.Fatalf("valid capability was not learned: %+v", got)
	}
}

func TestIssue663ProvidersClassifyUpstreamErrorBranches(t *testing.T) {
	// 403 with an invalid-key message is an invalid key, not generic auth.
	c := ClassifyUpstreamError(403, []byte(`{"error":{"message":"invalid api key"}}`))
	if c.Class != ClassInvalidKey {
		t.Fatalf("403 invalid key class=%q, want invalid key", c.Class)
	}
	// Tools rejection without a parameter defaults the parameter to "tools".
	// "function calling not supported" matches the tools pattern but names
	// no extractable parameter, so the default branch is the one that runs.
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"function calling not supported"}}`))
	if c.Class != ClassUnsupportedTools || c.Capability != CapTools || c.Parameter != "tools" {
		t.Fatalf("tools rejection = %+v, want unsupported tools with parameter tools", c)
	}
	// Reasoning rejection without a parameter defaults it to "reasoning".
	// "reasoning effort not enabled" matches the reasoning pattern but the
	// "not enabled" phrasing names no extractable parameter.
	c = ClassifyUpstreamError(400, []byte(`{"error":{"message":"reasoning effort not enabled"}}`))
	if c.Class != ClassUnsupportedReasoning || c.Capability != CapReasoning || c.Parameter != "reasoning" {
		t.Fatalf("reasoning rejection = %+v, want unsupported reasoning with parameter reasoning", c)
	}
	// Context-token extraction: k-suffix scales, and a repeated limit value
	// does not become the requested count.
	limit, requested := ParseContextTokens("maximum context length is 128k tokens, however you requested 200000 tokens")
	if limit != 128000 || requested != 200000 {
		t.Fatalf("context tokens = %d/%d, want 128000/200000", limit, requested)
	}
	limit, requested = ParseContextTokens("maximum context length is 8192 tokens, however you requested 8192 tokens")
	if limit != 8192 || requested != 0 {
		t.Fatalf("repeated limit tokens = %d/%d, want 8192/0", limit, requested)
	}
	if limit, requested := ParseContextTokens("no numbers here"); limit != 0 || requested != 0 {
		t.Fatalf("context tokens without numbers = %d/%d, want 0/0", limit, requested)
	}
}

func TestIssue663ProvidersResponsesTransportReadError(t *testing.T) {
	// A body that fails mid-read surfaces the read error, not a verdict.
	rt := responsesProbeTransport{base: issue663ReadErrTransport{}}
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := rt.Do(context.Background(), payload, false, nil); err == nil || !strings.Contains(err.Error(), "issue663 read boom") {
		t.Fatalf("responses transport read err=%v, want the reader error", err)
	}
	// A non-map message entry is skipped, not fatal, during conversion.
	out, err := chatProbePayloadToResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"},"not-a-map"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "hi") {
		t.Fatalf("conversion dropped the valid message: %s", out)
	}
}

func TestIssue663ProvidersCapabilitySuiteFailureBranches(t *testing.T) {
	ctx := context.Background()
	// No scripted responses at all: every Anthropic spec is a transport
	// failure, so TransportFail and Inconclusive both count the specs.
	rep := RunCapabilitySuiteAnthropic(ctx, &fakeTransport{}, "dep", "model")
	if rep.OK || rep.TransportFail == 0 || rep.Inconclusive != rep.TransportFail || len(rep.Outcomes) == 0 {
		t.Fatalf("empty-transport anthropic suite = %+v", rep)
	}
	// A tools rejection on the tools spec is a Failed outcome, while the
	// other specs fail on transport classification.
	resps := make([]fakeResp, 9)
	for i := range resps {
		resps[i] = mission95ProbeResp(400, `{"error":{"message":"tools are not supported"}}`, false)
	}
	rep = RunCapabilitySuiteAnthropic(ctx, &fakeTransport{resps: resps}, "dep", "model")
	if rep.Failed < 1 {
		t.Fatalf("tools-rejection anthropic suite failed=%d, want >=1: %+v", rep.Failed, rep.Outcomes)
	}
	// A Gemini suite whose replies carry no evidence is inconclusive on
	// every spec, never a fabricated pass.
	gresps := make([]fakeResp, 0)
	for i := 0; i < 12; i++ {
		gresps = append(gresps, mission95ProbeResp(200, `{}`, false))
	}
	grep := RunCapabilitySuiteGemini(ctx, &fakeTransport{resps: gresps}, "dep", "model")
	if grep.Passed != 0 || grep.Inconclusive != len(grep.Outcomes) || len(grep.Outcomes) == 0 {
		t.Fatalf("empty-evidence gemini suite = %+v", grep)
	}
}

func TestIssue663ProvidersGeminiProbeNon2xxBranches(t *testing.T) {
	ctx := context.Background()
	// Context overflow on a Gemini probe is inconclusive, not an error.
	ft := &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(400, `{"error":{"message":"maximum context length is 8192 tokens"}}`, false),
	}}
	out, err := runProbeGemini(ctx, ft, []byte(`{}`), false, CapText, ExpectText)
	if err != nil || out.Verdict != UnknownSupport || !strings.Contains(out.Detail, "probe inconclusive") {
		t.Fatalf("gemini context overflow = %+v err=%v", out, err)
	}
	// Any other non-2xx status is a probe error.
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(500, `{"error":{"message":"boom"}}`, false),
	}}
	if _, err := runProbeGemini(ctx, ft, []byte(`{}`), false, CapText, ExpectText); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("gemini 500 err=%v, want a 500 probe error", err)
	}
}

func TestIssue663ProvidersSanitizeAndRepairBranches(t *testing.T) {
	// Tools/vision are semantics-critical: even when the contract marks
	// them UNSUPPORTED, Sanitize must not silently drop them.
	contract := Contract{}
	contract.Capabilities.Set(CapTools, Unsupported)
	res, err := Sanitize([]byte(`{"model":"m","tools":[{"type":"function"}]}`), contract, Dialects()["openai"], RequirementProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatalf("tools were sanitized away despite being semantics-critical: %+v", res)
	}
	// A non-object generationConfig cannot be used as a map. The payload
	// must be Gemini-shaped (contents present) for the Gemini path to run.
	gcontract := Contract{}
	gcontract.Capabilities.Set(CapTemperature, Unsupported)
	res, err = Sanitize([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":"not-an-object"}`), gcontract, Dialects()["gemini"], RequirementProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatalf("non-object generationConfig changed the payload: %+v", res)
	}
	// A generationConfig knob whose capability is NOT unsupported is kept:
	// the removal helper returns without deleting.
	res, err = Sanitize([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5}}`), Contract{}, Dialects()["gemini"], RequirementProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Payload != nil {
		t.Fatalf("supported temperature was sanitized away: %+v", res)
	}
	// Gemini candidateCount repair drops the field from generationConfig
	// with a recorded rule.
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "candidateCount", CapabilityFailure: true}
	out, plan, ok := Repair(cls, []byte(`{"contents":[],"generationConfig":{"candidateCount":3,"temperature":0.5}}`), Dialects()["gemini"], RequirementProfile{})
	if !ok || len(plan.Rules) == 0 {
		t.Fatalf("candidateCount repair ok=%v plan=%+v", ok, plan)
	}
	if strings.Contains(string(out), "candidateCount") {
		t.Fatalf("candidateCount survived repair: %s", out)
	}
	if !strings.Contains(string(out), "temperature") {
		t.Fatalf("repair dropped unrelated generationConfig keys: %s", out)
	}
	// Reading a body is not part of Sanitize: invalid JSON still errors.
	if _, err := Sanitize([]byte(`{bad`), Contract{}, Dialects()["openai"], RequirementProfile{}); err == nil {
		t.Fatal("invalid JSON sanitize was accepted")
	}
	// "model" is excluded from parameter matching, so a Gemini repair
	// naming it hits the semantics-critical refusal instead of a drop.
	cls = Classified{Class: ClassUnsupportedParameter, Parameter: "model", CapabilityFailure: true}
	if _, _, ok := Repair(cls, []byte(`{"contents":[],"model":"m","generationConfig":{}}`), Dialects()["gemini"], RequirementProfile{}); ok {
		t.Fatal("model repair was applied despite the semantics-critical refusal")
	}
}

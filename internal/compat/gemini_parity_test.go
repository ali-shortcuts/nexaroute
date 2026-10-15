package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// geminiTestTransport serves scripted Gemini GenerateContent responses.
type geminiTestTransport struct {
	handler  func(payload []byte, stream bool) (int, string, string)
	calls    int
	payloads [][]byte
}

func (t *geminiTestTransport) Do(_ context.Context, payload []byte, stream bool, _ http.Header) (*http.Response, error) {
	t.calls++
	t.payloads = append(t.payloads, append([]byte(nil), payload...))
	status, body, contentType := t.handler(payload, stream)
	if contentType == "" {
		contentType = "application/json"
	}
	h := http.Header{"Content-Type": []string{contentType}}
	return &http.Response{StatusCode: status, Header: h, Body: &stringReaderCloser{Reader: strings.NewReader(body)}}, nil
}

func (t *geminiTestTransport) RedactBody(b []byte) []byte { return b }

func geminiOKBody(text string) string {
	escaped, _ := json.Marshal(text)
	return `{"candidates":[{"content":{"role":"model","parts":[{"text":` + string(escaped) + `}]},"finishReason":"STOP"}]}`
}

func geminiToolBody() string {
	return `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}]}`
}

func TestGeminiSuiteSupported(t *testing.T) {
	tr := &geminiTestTransport{handler: func(payload []byte, stream bool) (int, string, string) {
		if stream {
			return 200, "event: data\n\n", "text/event-stream"
		}
		s := string(payload)
		if strings.Contains(s, "functionDeclarations") {
			return 200, geminiToolBody(), ""
		}
		if strings.Contains(s, "responseMimeType") {
			return 200, geminiOKBody(`{"ok":true}`), ""
		}
		return 200, geminiOKBody("OK"), ""
	}}
	report := RunCapabilitySuiteGemini(context.Background(), tr, "dep-gemini", "gemini-2.0-flash")
	if len(report.Outcomes) != 11 {
		t.Fatalf("outcomes=%d want 11", len(report.Outcomes))
	}
	if report.Dialect != "gemini" || report.Level != "capability" {
		t.Fatalf("dialect=%q level=%q", report.Dialect, report.Level)
	}
	for _, o := range report.Outcomes {
		if o.Verdict != Supported {
			t.Fatalf("capability %s verdict=%v detail=%q (want supported)", o.Capability, o.Verdict, o.Detail)
		}
	}
	if report.Passed != 11 || report.Failed != 0 || !report.OK {
		t.Fatalf("report=%+v", report)
	}
	// Every probe must send Gemini shapes: contents present, no OpenAI messages.
	for i, raw := range tr.payloads {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("payload %d invalid: %v", i, err)
		}
		if _, ok := m["contents"]; !ok {
			t.Fatalf("payload %d missing contents: %s", i, raw)
		}
		if _, ok := m["messages"]; ok {
			t.Fatalf("payload %d leaked OpenAI messages: %s", i, raw)
		}
	}
	// Spot-check Gemini equivalents.
	var sys map[string]any
	_ = json.Unmarshal(tr.payloads[1], &sys)
	if _, ok := sys["systemInstruction"]; !ok {
		t.Fatalf("system probe missing systemInstruction: %s", tr.payloads[1])
	}
	var toolChoice map[string]any
	_ = json.Unmarshal(tr.payloads[4], &toolChoice)
	if _, ok := toolChoice["toolConfig"]; !ok {
		t.Fatalf("tool_choice probe missing toolConfig: %s", tr.payloads[4])
	}
	var temp map[string]any
	_ = json.Unmarshal(tr.payloads[5], &temp)
	gc, _ := temp["generationConfig"].(map[string]any)
	if gc == nil || gc["temperature"] == nil {
		t.Fatalf("temperature probe missing generationConfig.temperature: %s", tr.payloads[5])
	}
}

func TestGeminiSuiteRejected(t *testing.T) {
	tr := &geminiTestTransport{handler: func(payload []byte, stream bool) (int, string, string) {
		if stream {
			return 200, "event: data\n\n", "text/event-stream"
		}
		if strings.Contains(string(payload), `"temperature"`) {
			return 400, `{"error":{"message":"temperature is not supported"}}`, ""
		}
		s := string(payload)
		if strings.Contains(s, "functionDeclarations") {
			return 200, geminiToolBody(), ""
		}
		if strings.Contains(s, "responseMimeType") {
			return 200, geminiOKBody(`{"ok":true}`), ""
		}
		return 200, geminiOKBody("OK"), ""
	}}
	report := RunCapabilitySuiteGemini(context.Background(), tr, "dep-gemini", "m")
	found := false
	for _, o := range report.Outcomes {
		if o.Capability == CapTemperature {
			found = true
			if o.Verdict != Unsupported {
				t.Fatalf("temperature verdict=%v want unsupported", o.Verdict)
			}
		}
	}
	if !found {
		t.Fatal("temperature probe missing")
	}
	if report.Failed != 1 {
		t.Fatalf("failed=%d want 1: %+v", report.Failed, report)
	}
}

func TestGeminiSuiteMalformed(t *testing.T) {
	calls := 0
	tr := &geminiTestTransport{handler: func(payload []byte, stream bool) (int, string, string) {
		calls++
		if stream {
			return 200, "event: data\n\n", "text/event-stream"
		}
		if calls == 1 {
			return 200, `{broken`, ""
		}
		s := string(payload)
		if strings.Contains(s, "functionDeclarations") {
			return 200, geminiToolBody(), ""
		}
		if strings.Contains(s, "responseMimeType") {
			return 200, geminiOKBody(`{"ok":true}`), ""
		}
		return 200, geminiOKBody("OK"), ""
	}}
	report := RunCapabilitySuiteGemini(context.Background(), tr, "dep", "m")
	if report.TransportFail != 1 || report.Inconclusive < 1 {
		t.Fatalf("malformed should be transport-fail inconclusive: %+v", report)
	}
	if !strings.Contains(report.Outcomes[0].Detail, "malformed") && report.Outcomes[0].Verdict != UnknownSupport {
		t.Fatalf("first outcome=%+v", report.Outcomes[0])
	}
}

func TestGeminiRepairTemperatureInline(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5,"maxOutputTokens":16}}`)
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "temperature", Capability: CapTemperature, CapabilityFailure: true}
	repaired, plan, ok := Repair(cls, payload, Dialects()[DialectGemini], RequirementProfile{})
	if !ok {
		t.Fatal("expected inline Gemini repair")
	}
	var m map[string]any
	if err := json.Unmarshal(repaired, &m); err != nil {
		t.Fatal(err)
	}
	gc := m["generationConfig"].(map[string]any)
	if _, has := gc["temperature"]; has {
		t.Fatalf("temperature survived: %s", repaired)
	}
	if gc["maxOutputTokens"] == nil {
		t.Fatalf("unrelated maxOutputTokens dropped: %s", repaired)
	}
	if len(plan.Rules) != 1 || plan.Rules[0].Name != "drop:generationConfig.temperature" {
		t.Fatalf("plan=%+v", plan.Rules)
	}
	if DescribePlan(plan) == "" {
		t.Fatal("DescribePlan empty (audit surfacing broken)")
	}
}

func TestGeminiRepairMaxOutputTokensInline(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16,"temperature":0.5}}`)
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "maxOutputTokens", Capability: CapMaxCompletionTokens, CapabilityFailure: true}
	repaired, plan, ok := Repair(cls, payload, Dialects()[DialectGemini], RequirementProfile{})
	if !ok {
		t.Fatal("expected maxOutputTokens repair")
	}
	var m map[string]any
	_ = json.Unmarshal(repaired, &m)
	gc := m["generationConfig"].(map[string]any)
	if _, has := gc["maxOutputTokens"]; has {
		t.Fatalf("maxOutputTokens survived: %s", repaired)
	}
	if gc["temperature"] == nil {
		t.Fatalf("temperature wrongly dropped: %s", repaired)
	}
	if plan.Rules[0].Capability != CapMaxCompletionTokens {
		t.Fatalf("capability=%q", plan.Rules[0].Capability)
	}
}

func TestGeminiRepairResponseMimeTypeInline(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"object"},"temperature":0.5}}`)
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "response_format", Capability: CapStructuredOutput, CapabilityFailure: true}
	repaired, _, ok := Repair(cls, payload, Dialects()[DialectGemini], RequirementProfile{})
	if !ok {
		t.Fatal("expected structured-output repair")
	}
	if strings.Contains(string(repaired), "responseMimeType") || strings.Contains(string(repaired), "responseSchema") {
		t.Fatalf("structured output survived: %s", repaired)
	}
	if !strings.Contains(string(repaired), "temperature") {
		t.Fatalf("unrelated knob dropped: %s", repaired)
	}
}

func TestGeminiRepairToolConfig(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f"}]}],"toolConfig":{"functionCallingConfig":{"mode":"MODE_ANY"}}}`)
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "tool_choice", Capability: CapToolChoiceAuto, CapabilityFailure: true}
	repaired, plan, ok := Repair(cls, payload, Dialects()[DialectGemini], RequirementProfile{})
	if !ok {
		t.Fatal("expected toolConfig repair")
	}
	if strings.Contains(string(repaired), "toolConfig") {
		t.Fatalf("toolConfig survived: %s", repaired)
	}
	if !strings.Contains(string(repaired), "functionDeclarations") {
		t.Fatalf("tools wrongly stripped: %s", repaired)
	}
	if plan.Rules[0].Name != "drop:toolConfig" {
		t.Fatalf("plan=%+v", plan.Rules)
	}
	if _, _, ok := Repair(cls, payload, Dialects()[DialectGemini], RequirementProfile{NeedsTool: true}); ok {
		t.Fatal("forced tool call must not be downgraded")
	}
}

func TestGeminiRepairStopSequencesInline(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["|END|"]}}`)
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "stop", Capability: CapStop, CapabilityFailure: true}
	repaired, _, ok := Repair(cls, payload, Dialects()[DialectGemini], RequirementProfile{})
	if !ok || strings.Contains(string(repaired), "stopSequences") {
		t.Fatalf("stopSequences repair failed: ok=%v payload=%s", ok, repaired)
	}
}

func TestGeminiRepairTopPInline(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topP":0.9,"topK":40}}`)
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "topP", Capability: CapTopP, CapabilityFailure: true}
	repaired, _, ok := Repair(cls, payload, Dialects()[DialectGemini], RequirementProfile{})
	if !ok {
		t.Fatal("expected topP repair")
	}
	var m map[string]any
	_ = json.Unmarshal(repaired, &m)
	gc := m["generationConfig"].(map[string]any)
	if _, has := gc["topP"]; has {
		t.Fatalf("topP survived: %s", repaired)
	}
	if gc["topK"] == nil {
		t.Fatalf("topK wrongly dropped: %s", repaired)
	}
}

func TestNonGeminiPayloadsByteIdentical(t *testing.T) {
	// OpenAI-shaped payloads must repair exactly as before (no
	// generationConfig interference).
	payload := []byte(`{"model":"m","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
	cls := Classified{Class: ClassUnsupportedParameter, Parameter: "temperature", Capability: CapTemperature, CapabilityFailure: true}
	repaired, plan, ok := Repair(cls, payload, Dialects()[DialectGenericOpenAI], RequirementProfile{})
	if !ok {
		t.Fatal("expected temperature repair")
	}
	var m map[string]any
	if err := json.Unmarshal(repaired, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["temperature"]; has {
		t.Fatal("temperature survived")
	}
	if _, has := m["generationConfig"]; has {
		t.Fatalf("non-Gemini repair injected generationConfig: %s", repaired)
	}
	if len(plan.Rules) != 1 || plan.Rules[0].Name != "drop:temperature" {
		t.Fatalf("plan changed for non-Gemini: %+v", plan.Rules)
	}
	// Sanitizer for non-Gemini must not add Gemini keys.
	contract := Contract{}
	contract.Capabilities.Temperature = Unsupported
	res, err := Sanitize([]byte(`{"model":"m","temperature":0.5,"messages":[]}`), contract, Dialects()[DialectGenericOpenAI], RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("sanitize changed=%v err=%v", res.Changed, err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "temperature" {
		t.Fatalf("sanitize removed=%v", res.Removed)
	}
}

func TestGeminiSanitizeGenerationConfig(t *testing.T) {
	contract := Contract{}
	contract.Capabilities.Temperature = Unsupported
	contract.Capabilities.TopP = Unsupported
	contract.Capabilities.Stop = Unsupported
	contract.Capabilities.StructuredOutput = Unsupported
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5,"topP":0.9,"stopSequences":["x"],"responseMimeType":"application/json","maxOutputTokens":16}}`)
	res, err := Sanitize(payload, contract, Dialects()[DialectGemini], RequirementProfile{})
	if err != nil || !res.Changed {
		t.Fatalf("sanitize changed=%v err=%v", res.Changed, err)
	}
	var m map[string]any
	_ = json.Unmarshal(res.Payload, &m)
	gc := m["generationConfig"].(map[string]any)
	for _, k := range []string{"temperature", "topP", "stopSequences", "responseMimeType"} {
		if _, has := gc[k]; has {
			t.Fatalf("%s survived sanitize: %s", k, res.Payload)
		}
	}
	if gc["maxOutputTokens"] == nil {
		t.Fatalf("supported maxOutputTokens wrongly removed: %s", res.Payload)
	}
}

func TestClassifierGeminiParams(t *testing.T) {
	cases := []struct {
		msg   string
		param string
		cap   string
	}{
		{`topP is not supported`, "topp", CapTopP},
		{`generationConfig.temperature is not supported`, "temperature", CapTemperature},
		{`Unsupported field: maxOutputTokens`, "maxoutputtokens", CapMaxCompletionTokens},
		{`stopSequences not supported`, "stopsequences", CapStop},
		{`responseMimeType is not supported`, "responsemimetype", CapStructuredOutput},
	}
	for _, tc := range cases {
		cls := ClassifyUpstreamError(400, []byte(`{"error":{"message":`+quoteForTest(tc.msg)+`}}`))
		if !cls.CapabilityFailure {
			t.Fatalf("msg %q not a capability failure: %+v", tc.msg, cls)
		}
		if cls.Parameter != tc.param {
			t.Fatalf("msg %q param=%q want %q", tc.msg, cls.Parameter, tc.param)
		}
		if cls.Capability != tc.cap {
			t.Fatalf("msg %q cap=%q want %q", tc.msg, cls.Capability, tc.cap)
		}
	}
}

func quoteForTest(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestGeminiProbeVerdictFixtures(t *testing.T) {
	// Supported: text.
	if v, _ := geminiVerdict([]byte(geminiOKBody("OK")), ExpectText); v != Supported {
		t.Fatalf("text verdict=%v", v)
	}
	// Supported: tool call.
	if v, _ := geminiVerdict([]byte(geminiToolBody()), ExpectToolCall); v != Supported {
		t.Fatalf("tool verdict=%v", v)
	}
	// Supported: JSON.
	if v, _ := geminiVerdict([]byte(geminiOKBody(`{"ok":true}`)), ExpectJSON); v != Supported {
		t.Fatalf("json verdict=%v", v)
	}
	// Rejected path is via classifier, but verdict for missing tool call stays unknown.
	if v, _ := geminiVerdict([]byte(geminiOKBody("plain text")), ExpectToolCall); v != UnknownSupport {
		t.Fatalf("missing tool verdict=%v want unknown", v)
	}
	// Malformed.
	if v, _ := geminiVerdict([]byte(`{broken`), ExpectText); v != UnknownSupport {
		t.Fatalf("malformed verdict=%v want unknown", v)
	}
}

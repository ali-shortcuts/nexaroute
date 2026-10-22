package livecanary

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// The extended scenario set (structured output, images, parallel tools,
// reasoning) is the conformance surface providers are actually
// differentiated on. The fixture provider already speaks all four
// shapes; this test runs the full extended catalog against it and
// requires every scenario to pass with evidence recorded.
func TestCanaryRunsExtendedScenarios(t *testing.T) {
	fake := &fakeProvider{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv("NEXA_CANARY_KEY", "canary-test-secret-1234")

	opts := testOptions(srv.URL, "NEXA_CANARY_KEY")
	opts.Scenarios = []string{"extended"}
	opts.MaxCalls = 12
	opts.MaxOutputTokens = 1024
	runner, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Run(context.Background())
	if report.Outcome != OutcomePass {
		t.Fatalf("outcome=%s report=%s", report.Outcome, report.Summary())
	}
	if report.Limits.CallsUsed != 9 {
		t.Fatalf("calls=%d, want 9 (one per extended scenario)", report.Limits.CallsUsed)
	}
	byID := map[string]ScenarioResult{}
	for _, r := range report.Scenarios {
		byID[r.ID] = r
	}
	for _, id := range []string{"structured_output", "images", "parallel_tools", "reasoning"} {
		r, ok := byID[id]
		if !ok {
			t.Fatalf("extended report missing scenario %s", id)
		}
		if r.Status != StatusPass {
			t.Fatalf("scenario %s status=%s detail=%s", id, r.Status, r.Error)
		}
		if r.Evidence == "" {
			t.Fatalf("scenario %s recorded no evidence", id)
		}
	}
	for _, want := range []string{"structured_output", "images_multimodal", "parallel_tools", "reasoning"} {
		if !contains(report.VerifiedDimensions, want) {
			t.Fatalf("verified dimensions %v missing %s", report.VerifiedDimensions, want)
		}
	}
}

// The capability gates decide which scenarios a dialect may be scored
// on at all; a wrong gate either hides a real gap or fabricates one.
func TestScenarioCapabilityGates(t *testing.T) {
	cases := []struct {
		providerType                    string
		structured, parallel, reasoning bool
	}{
		{"openai_compatible", true, true, true},
		{"azure_openai", true, true, true},
		{"openai_responses", true, true, true},
		{"anthropic_compatible", false, true, true},
		{"gemini", true, false, true},
		{"vertex", true, false, true},
		{"bedrock", false, false, false},
	}
	for _, tc := range cases {
		p := config.ProviderConfig{Type: tc.providerType}
		if got := supportsStructuredOutput(p); got != tc.structured {
			t.Fatalf("%s structured gate = %v, want %v", tc.providerType, got, tc.structured)
		}
		if got := supportsParallelTools(p); got != tc.parallel {
			t.Fatalf("%s parallel gate = %v, want %v", tc.providerType, got, tc.parallel)
		}
		if got := supportsReasoning(p); got != tc.reasoning {
			t.Fatalf("%s reasoning gate = %v, want %v", tc.providerType, got, tc.reasoning)
		}
	}
}

func TestExtractJSONObjectShapes(t *testing.T) {
	obj, ok := extractJSONObject(`{"status":"ok"}`)
	if !ok || obj["status"] != "ok" {
		t.Fatalf("plain object = %v, %v", obj, ok)
	}
	// Models often wrap JSON in prose or fences; the object must still
	// be found.
	obj, ok = extractJSONObject("Here is the result:\n```json\n{\"status\":\"ok\"}\n```")
	if !ok || obj["status"] != "ok" {
		t.Fatalf("fenced object = %v, %v", obj, ok)
	}
	if _, ok := extractJSONObject(`["not","an","object"]`); ok {
		t.Fatal("a JSON array was accepted as an object")
	}
	if _, ok := extractJSONObject("no json at all"); ok {
		t.Fatal("prose was accepted as a JSON object")
	}
	if _, ok := extractJSONObject(`{"unterminated":`); ok {
		t.Fatal("unterminated JSON was accepted")
	}
}

func TestStreamTerminalAndFinishReasonHelpers(t *testing.T) {
	complete := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	if !hasSSEData(complete) {
		t.Fatal("SSE body with data lines not recognized")
	}
	if hasSSEData("data: [DONE]\n\n") {
		t.Fatal("a lone [DONE] marker counted as SSE data")
	}
	if hasSSEData("not a stream") {
		t.Fatal("plain text called SSE data")
	}
	// Terminal detection is dialect-specific.
	if !streamTerminal("openai_compatible", complete) {
		t.Fatal("OpenAI stream ending in [DONE] not terminal")
	}
	open := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	if streamTerminal("openai_compatible", open) {
		t.Fatal("OpenAI stream without [DONE]/finish_reason called terminal")
	}
	if !streamTerminal("anthropic_compatible", "event: message_stop\n") {
		t.Fatal("Anthropic message_stop not terminal")
	}
	if streamTerminal("anthropic_compatible", complete) {
		t.Fatal("[DONE] must not terminate an Anthropic stream")
	}
	if !streamTerminal("openai_responses", "event: response.completed\n") {
		t.Fatal("Responses response.completed not terminal")
	}
	if !streamTerminal("bedrock", "event: messageStop\n") {
		t.Fatal("Bedrock messageStop not terminal")
	}
	// Native finish reasons per dialect envelope.
	if !hasNativeFinishReason("openai_compatible", []byte(`{"choices":[{"finish_reason":"stop"}]}`)) {
		t.Fatal("OpenAI finish_reason not detected")
	}
	if hasNativeFinishReason("openai_compatible", []byte(`{"choices":[{"finish_reason":null}]}`)) {
		t.Fatal("null OpenAI finish_reason counted as native")
	}
	if !hasNativeFinishReason("anthropic_compatible", []byte(`{"stop_reason":"end_turn"}`)) {
		t.Fatal("Anthropic stop_reason not detected")
	}
	if hasNativeFinishReason("anthropic_compatible", []byte(`{"stop_reason":null}`)) {
		t.Fatal("null Anthropic stop_reason counted as native")
	}
	if !hasNativeFinishReason("gemini", []byte(`{"candidates":[{"finishReason":"STOP"}]}`)) {
		t.Fatal("Gemini finishReason not detected")
	}
	if !hasNativeFinishReason("openai_responses", []byte(`{"status":"completed"}`)) {
		t.Fatal("Responses status not detected")
	}
	if !hasNativeFinishReason("bedrock", []byte(`{"stopReason":"end_turn"}`)) {
		t.Fatal("Bedrock stopReason not detected")
	}
	if hasNativeFinishReason("openai_compatible", []byte(`not json`)) {
		t.Fatal("invalid JSON counted as having a finish reason")
	}
	// nonEmptyRaw treats absent, null and empty-string as empty.
	if nonEmptyRaw(nil) || nonEmptyRaw(json.RawMessage(`null`)) || nonEmptyRaw(json.RawMessage(`""`)) {
		t.Fatal("empty raw values counted as present")
	}
	if !nonEmptyRaw(json.RawMessage(`"end_turn"`)) {
		t.Fatal("a real raw value counted as empty")
	}
	if ptr := boolPtr(true); ptr == nil || !*ptr {
		t.Fatal("boolPtr(true) broken")
	}
}

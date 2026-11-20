package compat

import (
	"context"
	"testing"
)

// TestAgentERunProbeAnthropicPassesToolCallExpectation is the regression
// test for Agent E finding #1: runProbeAnthropic hardcoded ExpectText
// when calling the verdict function, so the Anthropic verdict's
// ExpectToolCall branch could never run and a tool-call probe could never
// report "tool call produced". The runner now passes each spec's own
// expectation through, and this test pins that the expectation reaching the
// verdict matches the capability under test.
func TestAgentERunProbeAnthropicPassesToolCallExpectation(t *testing.T) {
	ctx := context.Background()
	var got []ToolExpectation
	verdict := func(_ []byte, e ToolExpectation) (Support, string) {
		got = append(got, e)
		return Supported, "ok"
	}
	ft := &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, `{"content":[{"type":"tool_use","name":"get_weather"}]}`, false),
		mission95ProbeResp(200, `{"content":[{"type":"text","text":"OK"}]}`, false),
	}}
	if _, err := runProbeAnthropic(ctx, ft, []byte(`{}`), false, CapTools, ExpectToolCall, verdict); err != nil {
		t.Fatalf("tool capability probe: %v", err)
	}
	if _, err := runProbeAnthropic(ctx, ft, []byte(`{}`), false, CapText, ExpectText, verdict); err != nil {
		t.Fatalf("text capability probe: %v", err)
	}
	if len(got) != 2 || got[0] != ExpectToolCall || got[1] != ExpectText {
		t.Fatalf("verdict expectations = %v, want [tool-call text]", got)
	}
}

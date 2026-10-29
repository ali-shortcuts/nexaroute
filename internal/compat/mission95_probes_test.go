package compat

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func mission95ProbeResp(status int, body string, sse bool) fakeResp {
	return fakeResp{status: status, body: body, sse: sse}
}

func mission95ChatOK() string {
	return `{"choices":[{"message":{"content":"OK"}}]}`
}

func mission95ChatTools(n int) string {
	calls := make([]string, 0, n)
	for i := 0; i < n; i++ {
		calls = append(calls, `{"id":"c`+string(rune('1'+i))+`","function":{"name":"f","arguments":"{}"}}`)
	}
	return `{"choices":[{"message":{"content":"","tool_calls":[` + strings.Join(calls, ",") + `]}}]}`
}

func TestMission95RunProbe(t *testing.T) {
	ctx := context.Background()
	// Transport error.
	ft := &fakeTransport{}
	_, err := runProbe(ctx, ft, []byte(`{}`), false, CapText, ExpectText)
	if err == nil {
		t.Fatalf("transport error expected (no scripted responses)")
	}
	// Non-2xx capability failure matching the capability -> Unsupported.
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(400, `{"error":{"message":"tools are not supported"}}`, false),
		mission95ProbeResp(400, `{"error":{"message":"temperature is not supported"}}`, false),
		mission95ProbeResp(400, `{"error":{"message":"maximum context length is 8192 tokens"}}`, false),
		mission95ProbeResp(400, `{"error":{"message":"invalid request schema"}}`, false),
		mission95ProbeResp(500, `{"error":{"message":"boom"}}`, false),
	}}
	out, err := runProbe(ctx, ft, []byte(`{}`), false, CapTools, ExpectText)
	if err != nil || out.Verdict != Unsupported {
		t.Fatalf("cap failure: %+v %v", out, err)
	}
	out, err = runProbe(ctx, ft, []byte(`{}`), false, CapTemperature, ExpectText)
	if err != nil || out.Verdict != Unsupported {
		t.Fatalf("cap via parameter: %+v %v", out, err)
	}
	out, err = runProbe(ctx, ft, []byte(`{}`), false, CapText, ExpectText)
	if err != nil || out.Verdict != UnknownSupport || !strings.Contains(out.Detail, "probe inconclusive") {
		t.Fatalf("context overflow: %+v %v", out, err)
	}
	out, err = runProbe(ctx, ft, []byte(`{}`), false, CapText, ExpectText)
	if err != nil || out.Verdict != UnknownSupport || !strings.Contains(out.Detail, "probe inconclusive") {
		t.Fatalf("caller error: %+v %v", out, err)
	}
	if _, err := runProbe(ctx, ft, []byte(`{}`), false, CapText, ExpectText); err == nil {
		t.Fatalf("other non-2xx should error")
	}
	// Stream: wrong content-type vs SSE headers.
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, `data: x`, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(200, `{bad`, false),
	}}
	out, err = runProbe(ctx, ft, []byte(`{}`), true, CapStreaming, ExpectText)
	if err != nil || out.Verdict != UnknownSupport || out.Detail != "200 but content-type is not event-stream" {
		t.Fatalf("stream ct: %+v %v", out, err)
	}
	out, err = runProbe(ctx, ft, []byte(`{}`), true, CapStreaming, ExpectText)
	if err != nil || out.Verdict != Supported {
		t.Fatalf("stream ok: %+v %v", out, err)
	}
	if _, err := runProbe(ctx, ft, []byte(`{}`), false, CapText, ExpectText); err == nil {
		t.Fatalf("malformed body should error")
	}
	// Expectation matrix.
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, mission95ChatTools(1), false),
		mission95ProbeResp(200, mission95ChatOK(), false),
		mission95ProbeResp(200, mission95ChatTools(2), false),
		mission95ProbeResp(200, mission95ChatTools(1), false),
		mission95ProbeResp(200, mission95ChatOK(), false),
		mission95ProbeResp(200, `{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`, false),
		mission95ProbeResp(200, `{"choices":[{"message":{"content":"plain"}}]}`, false),
		mission95ProbeResp(200, mission95ChatOK(), false),
	}}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapTools, ExpectToolCall)
	if out.Verdict != Supported || out.Detail != "tool call produced (1)" {
		t.Fatalf("tool call: %+v", out)
	}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapTools, ExpectToolCall)
	if out.Verdict != UnknownSupport {
		t.Fatalf("no tool call: %+v", out)
	}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapParallelToolCalls, ExpectParallelToolCalls)
	if out.Verdict != Supported || !strings.Contains(out.Detail, "(2)") {
		t.Fatalf("parallel: %+v", out)
	}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapParallelToolCalls, ExpectParallelToolCalls)
	if out.Verdict != UnknownSupport || out.Detail != "single tool call; parallelism unproven" {
		t.Fatalf("single parallel: %+v", out)
	}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapParallelToolCalls, ExpectParallelToolCalls)
	if out.Verdict != UnknownSupport {
		t.Fatalf("zero parallel: %+v", out)
	}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapJSONObject, ExpectJSON)
	if out.Verdict != Supported {
		t.Fatalf("json: %+v", out)
	}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapJSONObject, ExpectJSON)
	if out.Verdict != UnknownSupport || out.Detail != "200 but output is not JSON" {
		t.Fatalf("non-json: %+v", out)
	}
	out, _ = runProbe(ctx, ft, []byte(`{}`), false, CapText, ExpectText)
	if out.Verdict != Supported || out.Detail != "generation verified" {
		t.Fatalf("default: %+v", out)
	}
	// Helpers.
	if got := firstText(completionShape{}); got != "" {
		t.Fatalf("firstText empty: %q", got)
	}
	var probe completionShape
	_ = json.Unmarshal([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":""},{"type":"text","text":"second"}]}}]}`), &probe)
	if got := firstText(probe); got != "second" {
		t.Fatalf("firstText array: %q", got)
	}
	if got := mapsToCapability(""); got != "" {
		t.Fatalf("maps empty: %q", got)
	}
	if got := mapsToCapability("nope"); got != "" {
		t.Fatalf("maps unknown: %q", got)
	}
	if got := mapsToCapability("temperature"); got != CapTemperature {
		t.Fatalf("maps temperature: %q", got)
	}
	if got := truncatedSnippet("short", 120); got != "short" {
		t.Fatalf("snippet short: %q", got)
	}
	if got := truncatedSnippet(strings.Repeat("x", 130), 120); got != strings.Repeat("x", 120)+"..." {
		t.Fatalf("snippet long: %q", got)
	}
}

func TestMission95ProbeConversions(t *testing.T) {
	// chatProbePayloadToResponses: full conversion.
	in := map[string]any{
		"model": "m", "stream": true, "temperature": 0.5, "top_p": 0.9, "stop": []string{"END"},
		"parallel_tool_calls": true, "metadata": map[string]any{"k": "v"},
		"max_tokens": 10, "max_completion_tokens": 20, "reasoning_effort": "low",
		"tool_choice":     "auto",
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "n", "schema": map[string]any{"type": "object"}, "strict": true}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "f", "description": "d", "parameters": map[string]any{"type": "object"}}},
			map[string]any{"type": "function", "name": "flat"},
			"not-a-map",
		},
		"messages": []any{
			map[string]any{"role": "system", "content": "sys1"},
			map[string]any{"role": "developer", "content": "  "},
			map[string]any{"role": "developer", "content": "sys2"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "see"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/i.png"}},
				map[string]any{"type": "image_url", "image_url": ""},
				map[string]any{"type": "other"},
				"junk",
			}},
			map[string]any{"role": "assistant", "content": "ans", "tool_calls": []any{
				map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": "{}"}},
				map[string]any{"id": "c2"},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": map[string]any{"a": 1}},
		},
	}
	b, _ := json.Marshal(in)
	out, err := chatProbePayloadToResponses(b)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["max_output_tokens"] != float64(20) || m["temperature"] != 0.5 || m["stream"] != true {
		t.Fatalf("scalar passthrough: %v", m)
	}
	if m["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("reasoning: %v", m["reasoning"])
	}
	if m["text"].(map[string]any)["format"].(map[string]any)["name"] != "n" {
		t.Fatalf("format: %v", m["text"])
	}
	tools := m["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "f" || tools[1].(map[string]any)["name"] != "flat" {
		t.Fatalf("tools: %v", tools)
	}
	if m["instructions"] != "sys1\nsys2" {
		t.Fatalf("instructions: %v", m["instructions"])
	}
	input := m["input"].([]any)
	// user hi, user multi (text+image), assistant text, assistant call,
	// tool output = 5 items.
	if len(input) != 5 {
		t.Fatalf("input = %d: %v", len(input), input)
	}
	if input[1].(map[string]any)["content"].([]any)[1].(map[string]any)["image_url"] != "https://x/i.png" {
		t.Fatalf("image content: %v", input[1])
	}
	if input[3].(map[string]any)["type"] != "function_call" || input[3].(map[string]any)["call_id"] != "c1" {
		t.Fatalf("assistant call: %v", input[3])
	}
	if input[4].(map[string]any)["output"] != `{"a":1}` {
		t.Fatalf("tool output marshal: %v", input[4])
	}
	// Empty input -> default OK message; max_tokens fallback; bad JSON.
	out, err = chatProbePayloadToResponses([]byte(`{"model":"m","max_tokens":7,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(out, &m)
	if m["max_output_tokens"] != float64(7) {
		t.Fatalf("max fallback: %v", m)
	}
	if len(m["input"].([]any)) != 1 {
		t.Fatalf("default input: %v", m["input"])
	}
	if _, err := chatProbePayloadToResponses([]byte(`{bad`)); err == nil {
		t.Fatalf("bad payload should fail")
	}
	// responsesProbeContent shapes.
	if got := responsesProbeContent(""); got != nil {
		t.Fatalf("empty string: %v", got)
	}
	if got := responsesProbeContent("hi"); len(got) != 1 || got[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("string: %v", got)
	}
	if got := responsesProbeContent(42); got != nil {
		t.Fatalf("number: %v", got)
	}
	if got := responsesProbeContent([]any{
		map[string]any{"type": "input_text", "text": "t"},
		map[string]any{"type": "image_url", "image_url": "https://x/i.png"},
		map[string]any{"type": "text", "text": ""},
	}); len(got) != 2 {
		t.Fatalf("array: %v", got)
	}
	// responsesProbeBodyToChat: message + calls + incomplete status.
	chat, err := responsesProbeBodyToChat([]byte(`{"id":"r1","model":"m","status":"incomplete","output":[
		{"type":"message","content":[{"type":"output_text","text":"a"},{"type":"text","text":"b"},{"type":"other","text":"no"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}
	],"usage":{"input_tokens":3,"output_tokens":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	var cm map[string]any
	if err := json.Unmarshal(chat, &cm); err != nil {
		t.Fatal(err)
	}
	choice := cm["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "a\nb" || choice["finish_reason"] != "tool_calls" {
		t.Fatalf("body to chat: %v", choice)
	}
	if cm["usage"].(map[string]any)["total_tokens"] != float64(5) {
		t.Fatalf("usage: %v", cm["usage"])
	}
	if _, err := responsesProbeBodyToChat([]byte(`{bad`)); err == nil {
		t.Fatalf("bad body should fail")
	}
	// Adapter Do: conversion error, stream passthrough, non-2xx
	// passthrough, 2xx conversion, conversion failure keeps original.
	base := &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, `{"output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(500, `{"error":{"message":"boom"}}`, false),
		mission95ProbeResp(200, `{bad`, false),
	}}
	adapter := responsesProbeTransport{base: base}
	if got := adapter.RedactBody([]byte("x")); string(got) != "x" {
		t.Fatalf("redact passthrough")
	}
	resp, err := adapter.Do(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"content":"OK"`) {
		t.Fatalf("converted body: %s", body)
	}
	resp, err = adapter.Do(context.Background(), []byte(`{"model":"m"}`), true, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream passthrough: %v %v", resp, err)
	}
	resp, err = adapter.Do(context.Background(), []byte(`{"model":"m"}`), false, nil)
	if err != nil || resp.StatusCode != 500 {
		t.Fatalf("non-2xx passthrough: %v %v", resp, err)
	}
	resp, err = adapter.Do(context.Background(), []byte(`{"model":"m"}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	if string(body) != `{bad` {
		t.Fatalf("conversion failure keeps original: %s", body)
	}
	if _, err := adapter.Do(context.Background(), []byte(`{bad`), false, nil); err == nil {
		t.Fatalf("adapter bad payload should fail")
	}
	// Gemini payload builders.
	gp := geminiPayload(geminiTextContents("hi"), "sys", geminiFunctionTools(), map[string]any{"mode": "ANY"}, map[string]any{"temperature": 0.5})
	var gm map[string]any
	_ = json.Unmarshal(gp, &gm)
	if gm["systemInstruction"] == nil || gm["tools"] == nil || gm["toolConfig"] == nil || gm["generationConfig"] == nil {
		t.Fatalf("gemini payload: %v", gm)
	}
	gp = geminiPayload(geminiTextContents("hi"), "", nil, nil, nil)
	var gm2 map[string]any
	_ = json.Unmarshal(gp, &gm2)
	if _, ok := gm2["systemInstruction"]; ok {
		t.Fatalf("empty system should be absent")
	}
}

func TestMission95GeminiVerdictAndProbe(t *testing.T) {
	if v, d := geminiVerdict([]byte(`{bad`), ExpectText); v != UnknownSupport || d != "unparseable gemini body" {
		t.Fatalf("unparseable: %v %q", v, d)
	}
	if v, d := geminiVerdict([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`), ExpectText); v != UnknownSupport || d != "prompt blocked: SAFETY" {
		t.Fatalf("blocked: %v %q", v, d)
	}
	if v, d := geminiVerdict([]byte(`{}`), ExpectText); v != UnknownSupport || d != "200 without candidates" {
		t.Fatalf("no candidates: %v %q", v, d)
	}
	if v, d := geminiVerdict([]byte(`{"candidates":[{"content":{"parts":[{"text":"a"},{"text":"b"},{"functionCall":{}},{"function_call":{}}]}}]}`), ExpectToolCall); v != Supported || d != "tool call produced" {
		t.Fatalf("tool verdict: %v %q", v, d)
	}
	if v, _ := geminiVerdict([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`), ExpectToolCall); v != UnknownSupport {
		t.Fatalf("no tool: %v", v)
	}
	if v, _ := geminiVerdict([]byte(`{"candidates":[{"content":{"parts":[{"text":"  [1,2] "}]}}]}`), ExpectJSON); v != Supported {
		t.Fatalf("json array: %v", v)
	}
	if v, d := geminiVerdict([]byte(`{"candidates":[{"content":{"parts":[{"text":"plain"}]}}]}`), ExpectJSON); v != UnknownSupport || d != "200 but output is not JSON" {
		t.Fatalf("non-json: %v %q", v, d)
	}
	if v, d := geminiVerdict([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{}}]}}]}`), ExpectText); v != Supported || d != "generation verified (function call)" {
		t.Fatalf("tool-only text: %v %q", v, d)
	}
	if v, d := geminiVerdict([]byte(`{"candidates":[{"content":{"parts":[]}}]}`), ExpectText); v != UnknownSupport || d != "200 without text content" {
		t.Fatalf("empty parts: %v %q", v, d)
	}
	if v, _ := geminiVerdict([]byte(`{"candidates":[{"content":null}]}`), ExpectText); v != UnknownSupport {
		t.Fatalf("nil content: %v", v)
	}
	// runProbeGemini branches.
	ctx := context.Background()
	ft := &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, `data: x`, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(200, `{bad`, false),
		mission95ProbeResp(200, `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`, false),
	}}
	out, err := runProbeGemini(ctx, ft, []byte(`{}`), true, CapStreaming, ExpectText)
	if err != nil || out.Verdict != UnknownSupport || out.Detail != "200 but content-type is not event-stream" {
		t.Fatalf("gemini stream ct: %+v %v", out, err)
	}
	out, err = runProbeGemini(ctx, ft, []byte(`{}`), true, CapStreaming, ExpectText)
	if err != nil || out.Verdict != Supported || out.Detail != "SSE headers verified" {
		t.Fatalf("gemini stream ok: %+v %v", out, err)
	}
	if _, err := runProbeGemini(ctx, ft, []byte(`{}`), false, CapText, ExpectText); err == nil {
		t.Fatalf("gemini malformed should error")
	}
	out, err = runProbeGemini(ctx, ft, []byte(`{}`), false, CapText, ExpectText)
	if err != nil || out.Verdict != Supported {
		t.Fatalf("gemini ok: %+v %v", out, err)
	}
	// Transport error path (no scripted response left).
	if _, err := runProbeGemini(ctx, ft, []byte(`{}`), false, CapText, ExpectText); err == nil {
		t.Fatalf("gemini transport error expected")
	}
}

func TestMission95ProbeSuites(t *testing.T) {
	ctx := context.Background()
	ok := mission95ChatOK()
	tools1 := mission95ChatTools(1)
	tools2 := mission95ChatTools(2)
	jsonBody := `{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`
	// OpenAI suite: 13 specs in order.
	ft := &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, ok, false),       // text
		mission95ProbeResp(200, ok, false),       // system
		mission95ProbeResp(200, `data: x`, true), // streaming
		mission95ProbeResp(200, tools1, false),   // tools
		mission95ProbeResp(200, tools1, false),   // tool choice
		mission95ProbeResp(200, tools2, false),   // parallel
		mission95ProbeResp(200, ok, false),       // reasoning effort
		mission95ProbeResp(200, ok, false),       // temperature
		mission95ProbeResp(200, ok, false),       // top_p
		mission95ProbeResp(200, ok, false),       // stop
		mission95ProbeResp(200, ok, false),       // max completion
		mission95ProbeResp(200, jsonBody, false), // json object
		mission95ProbeResp(200, ok, false),       // vision
	}}
	rep := RunCapabilitySuite(ctx, ft, "dep", "model", "openai_chat")
	if !rep.OK || rep.Passed != 13 || len(rep.Outcomes) != 13 || rep.Level != "capability" || rep.Dialect != "openai_chat" {
		t.Fatalf("openai suite: %+v", rep)
	}
	// One transport failure -> TransportFail + Inconclusive, OK false.
	ft = &fakeTransport{resps: []fakeResp{mission95ProbeResp(200, ok, false)}}
	rep = RunCapabilitySuite(ctx, ft, "dep", "model", "openai_chat")
	if rep.OK || rep.TransportFail != 12 || rep.Inconclusive != 12 {
		t.Fatalf("failing suite: %+v", rep)
	}
	// Anthropic suite: 9 specs; text bodies + tool bodies + stream.
	anthText := `{"content":[{"type":"text","text":"OK"}]}`
	anthTool := `{"content":[{"type":"tool_use","name":"get_weather"}]}`
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(200, anthTool, false),
		mission95ProbeResp(200, anthTool, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
	}}
	// NOTE: runProbeAnthropic always passes ExpectText to the verdict
	// function, so the tool specs are judged on text presence, not
	// tool production: a tool_use-only body counts as inconclusive
	// ("200 without text content"), while a text body passes the
	// tool specs. The verdict's ExpectToolCall branch is unreachable
	// through this path and is left uncovered deliberately.
	rep = RunCapabilitySuiteAnthropic(ctx, ft, "dep", "model")
	if !rep.OK || rep.Passed != 7 || rep.Inconclusive != 2 || len(rep.Outcomes) != 9 {
		t.Fatalf("anthropic suite: %+v", rep)
	}
	if rep.Outcomes[3].Detail != "200 without text content" {
		t.Fatalf("anthropic tool spec: %+v", rep.Outcomes[3])
	}
	// Anthropic verdict edges through the suite: unparseable text body,
	// text-less body, text body on the tool spec (passes: see note).
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, `{bad`, false),
		mission95ProbeResp(200, `{}`, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
		mission95ProbeResp(200, anthText, false),
	}}
	rep = RunCapabilitySuiteAnthropic(ctx, ft, "dep", "model")
	if rep.Passed != 7 || rep.Inconclusive != 2 || rep.Outcomes[0].Detail != "unparseable anthropic body" || rep.Outcomes[1].Detail != "200 without text content" || rep.Outcomes[3].Detail != "generation verified" {
		t.Fatalf("anthropic edges: %+v", rep.Outcomes)
	}
	// runProbeAnthropic direct branches.
	verdict := func(b []byte, e ToolExpectation) (Support, string) { return Supported, "ok" }
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(400, `{"error":{"message":"tools are not supported"}}`, false),
		mission95ProbeResp(400, `{"error":{"message":"maximum context length is 8192 tokens"}}`, false),
		mission95ProbeResp(500, `{"error":{"message":"boom"}}`, false),
		mission95ProbeResp(200, `data: x`, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(200, `{}`, false),
	}}
	out, err := runProbeAnthropic(ctx, ft, []byte(`{}`), false, CapTools, verdict)
	if err != nil || out.Verdict != Unsupported {
		t.Fatalf("anth cap: %+v %v", out, err)
	}
	out, err = runProbeAnthropic(ctx, ft, []byte(`{}`), false, CapText, verdict)
	if err != nil || out.Verdict != UnknownSupport || !strings.Contains(out.Detail, "probe inconclusive") {
		t.Fatalf("anth context: %+v %v", out, err)
	}
	if _, err := runProbeAnthropic(ctx, ft, []byte(`{}`), false, CapText, verdict); err == nil {
		t.Fatalf("anth other non-2xx should error")
	}
	out, _ = runProbeAnthropic(ctx, ft, []byte(`{}`), true, CapStreaming, verdict)
	if out.Verdict != UnknownSupport {
		t.Fatalf("anth stream ct: %+v", out)
	}
	out, err = runProbeAnthropic(ctx, ft, []byte(`{}`), true, CapStreaming, verdict)
	if err != nil || out.Verdict != Supported || out.Detail != "SSE headers verified" {
		t.Fatalf("anth stream ok: %+v %v", out, err)
	}
	out, err = runProbeAnthropic(ctx, ft, []byte(`{}`), false, CapText, verdict)
	if err != nil || out.Verdict != Supported {
		t.Fatalf("anth verdict passthrough: %+v %v", out, err)
	}
	if _, err := runProbeAnthropic(ctx, ft, []byte(`{}`), false, CapText, verdict); err == nil {
		t.Fatalf("anth transport error expected")
	}
	// Gemini suite: 11 specs.
	gemText := `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`
	gemTool := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{}}}]}}]}`
	gemJSON := `{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}]}}]}`
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, gemText, false),
		mission95ProbeResp(200, gemText, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(200, gemTool, false),
		mission95ProbeResp(200, gemTool, false),
		mission95ProbeResp(200, gemText, false),
		mission95ProbeResp(200, gemText, false),
		mission95ProbeResp(200, gemText, false),
		mission95ProbeResp(200, gemText, false),
		mission95ProbeResp(200, gemJSON, false),
		mission95ProbeResp(200, gemText, false),
	}}
	rep = RunCapabilitySuiteGemini(ctx, ft, "dep", "model")
	if !rep.OK || rep.Passed != 11 || len(rep.Outcomes) != 11 || rep.Dialect != "gemini" {
		t.Fatalf("gemini suite: %+v", rep)
	}
	// Responses suite through the adapter.
	respText := `{"output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`
	respTool := `{"output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`
	respTool2 := `{"output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"g","arguments":"{}"}]}`
	respJSON := `{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, respText, false),
		mission95ProbeResp(200, respText, false),
		mission95ProbeResp(200, `data: x`, true),
		mission95ProbeResp(200, respTool, false),
		mission95ProbeResp(200, respTool, false),
		mission95ProbeResp(200, respTool2, false),
		mission95ProbeResp(200, respText, false),
		mission95ProbeResp(200, respText, false),
		mission95ProbeResp(200, respText, false),
		mission95ProbeResp(200, respText, false),
		mission95ProbeResp(200, respText, false),
		mission95ProbeResp(200, respJSON, false),
		mission95ProbeResp(200, respText, false),
	}}
	rep = RunCapabilitySuiteResponses(ctx, ft, "dep", "model")
	if !rep.OK || rep.Passed != 13 || rep.Dialect != "openai_responses" {
		t.Fatalf("responses suite: %+v", rep)
	}
}

func TestMission95AgentLoop(t *testing.T) {
	ctx := context.Background()
	// Step 1 transport error.
	ft := &fakeTransport{}
	rep := RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || len(rep.AgentSteps) != 1 || rep.AgentSteps[0].Step != "tool_call" || rep.AgentSteps[0].Passed {
		t.Fatalf("step1 transport: %+v", rep)
	}
	// Step 1 non-2xx.
	ft = &fakeTransport{resps: []fakeResp{mission95ProbeResp(500, `{"error":{"message":"boom"}}`, false)}}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || !strings.Contains(rep.AgentSteps[0].Detail, "http 500") {
		t.Fatalf("step1 status: %+v", rep)
	}
	// Step 1 no tool call (bad body and valid-but-empty).
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, `{bad`, false),
		mission95ProbeResp(200, mission95ChatOK(), false),
	}}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || rep.AgentSteps[0].Detail != "no tool call in probe response" {
		t.Fatalf("step1 bad body: %+v", rep)
	}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || rep.AgentSteps[0].Detail != "no tool call in probe response" {
		t.Fatalf("step1 no call: %+v", rep)
	}
	// Step 2 transport error (no scripted second response), non-2xx,
	// success, and empty continuation text — one transport each.
	ft = &fakeTransport{resps: []fakeResp{mission95ProbeResp(200, mission95ChatTools(1), false)}}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || len(rep.AgentSteps) != 2 || rep.AgentSteps[1].Passed || rep.AgentSteps[1].Step != "tool_result_continuation" {
		t.Fatalf("step2 transport: %+v", rep)
	}
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, mission95ChatTools(1), false),
		mission95ProbeResp(500, `{"error":{"message":"boom"}}`, false),
	}}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || !strings.Contains(rep.AgentSteps[1].Detail, "http 500") {
		t.Fatalf("step2 status: %+v", rep)
	}
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, mission95ChatTools(1), false),
		mission95ProbeResp(200, mission95ChatOK(), false),
	}}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if !rep.OK || rep.Passed != 2 || !rep.AgentSteps[1].Passed || rep.AgentSteps[1].Detail != "OK" {
		t.Fatalf("step2 success: %+v", rep)
	}
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, mission95ChatTools(1), false),
		mission95ProbeResp(200, `{"choices":[{"message":{"content":""}}]}`, false),
	}}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || rep.Failed != 1 || rep.AgentSteps[1].Passed {
		t.Fatalf("step2 empty: %+v", rep)
	}
	// Step 2 unparseable continuation.
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, mission95ChatTools(1), false),
		mission95ProbeResp(200, `{bad`, false),
	}}
	rep = RunAgentLoopSimulation(ctx, ft, "dep", "model")
	if rep.OK || rep.AgentSteps[1].Detail != "unparseable continuation" {
		t.Fatalf("step2 unparseable: %+v", rep)
	}
	// Responses adapter loop: success.
	ft = &fakeTransport{resps: []fakeResp{
		mission95ProbeResp(200, `{"output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`, false),
		mission95ProbeResp(200, `{"output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`, false),
	}}
	rep = RunAgentLoopSimulationResponses(ctx, ft, "dep", "model")
	if !rep.OK || rep.Passed != 2 {
		t.Fatalf("responses loop: %+v", rep)
	}
}

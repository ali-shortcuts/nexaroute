package canonical

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeGeminiIngressRequestBasic(t *testing.T) {
	var in GeminiRequest
	body := `{"systemInstruction":{"parts":[{"text":"Be brief."}]},
		"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},
			{"role":"function","parts":[{"functionResponse":{"name":"lookup","response":{"result":1}}}]}
		],
		"tools":[{"functionDeclarations":[{"name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}},
		"generationConfig":{"temperature":0.5,"maxOutputTokens":64,"stopSequences":["END"],"responseMimeType":"application/json"}}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	req, err := DecodeGeminiIngressRequest(in, "public-model")
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "public-model" || req.ClientProtocol != "gemini" {
		t.Fatalf("model/protocol = %q/%q", req.Model, req.ClientProtocol)
	}
	if len(req.System) != 1 || req.System[0].Text != "Be brief." {
		t.Fatalf("system = %+v", req.System)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	if req.Messages[1].Parts[0].Type != PartToolCall || req.Messages[1].Parts[0].ToolCall.Name != "lookup" {
		t.Fatalf("tool call part = %+v", req.Messages[1].Parts[0])
	}
	if req.Messages[2].Parts[0].Type != PartToolResult || req.Messages[2].Parts[0].ToolResult.ToolUseID != "lookup" {
		t.Fatalf("tool result part = %+v", req.Messages[2].Parts[0])
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != "tool" || req.ToolChoice.Name != "lookup" {
		t.Fatalf("toolChoice = %+v", req.ToolChoice)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 || req.MaxOutputTokens != 64 {
		t.Fatalf("generation config not mapped: %+v", req)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != FormatJSONObject {
		t.Fatalf("responseFormat = %+v", req.ResponseFormat)
	}
}

func TestDecodeGeminiIngressRequestRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty contents", `{"contents":[]}`},
		{"candidateCount", `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"candidateCount":2}}`},
		{"safetySettings", `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"safetySettings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT"}]}`},
		{"bad role", `{"contents":[{"role":"system","parts":[{"text":"x"}]}]}`},
		{"fileData part", `{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://x"}}]}]}`},
		{"bad tool args", `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":"not-json"}}]}]}`},
		{"roleless later turn", `{"contents":[{"parts":[{"text":"a"}]},{"parts":[{"text":"b"}]}]}`},
		{"validated mode", `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}}}`},
		{"non-text modality", `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"responseModalities":["AUDIO"]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in GeminiRequest
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeGeminiIngressRequest(in, "m"); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestEncodeGeminiResponse(t *testing.T) {
	resp := Response{
		Blocks: []Block{
			{Type: PartText, Text: "hello"},
			{Type: PartToolCall, ToolCall: &ToolCall{Name: "lookup", Arguments: `{"q":"x"}`}},
		},
		StopReason: StopToolUse,
		Usage:      Usage{InputTokens: 7, OutputTokens: 5},
	}
	out, err := EncodeGeminiResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	s := string(b)
	for _, want := range []string{`"text":"hello"`, `"functionCall"`, `"name":"lookup"`, `"finishReason":"STOP"`, `"promptTokenCount":7`, `"candidatesTokenCount":5`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
	if _, err := EncodeGeminiResponse(Response{Blocks: []Block{{Type: PartToolCall, ToolCall: &ToolCall{Name: "f", Arguments: "broken"}}}}); err == nil {
		t.Fatalf("expected error for malformed tool arguments")
	}
}

func TestGeminiEmitterStreamShape(t *testing.T) {
	rr := httptest.NewRecorder()
	em := NewGeminiEmitter(rr, "public-model")
	if err := em.Emit(StreamEvent{Type: StreamText, Text: "Hel"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamText, Text: "lo"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolStart, ToolIndex: 0, ToolName: "lookup"}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `{"q":`}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 0, ArgsDelta: `"x"}`}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamToolEnd, ToolIndex: 0}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamUsage, Usage: &Usage{InputTokens: 9, OutputTokens: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := em.Emit(StreamEvent{Type: StreamEnd, StopReason: StopEndTurn}); err != nil {
		t.Fatal(err)
	}
	if err := em.Finish(); err != nil {
		t.Fatal(err)
	}
	body := rr.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Gemini stream must not use the [DONE] sentinel: %s", body)
	}
	for _, want := range []string{`"text":"Hel"`, `"text":"lo"`, `"functionCall"`, `"name":"lookup"`, `"finishReason":"STOP"`, `"promptTokenCount":9`, `"candidatesTokenCount":3`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	// Every data frame must be a self-contained GenerateContentResponse.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var frame map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				t.Fatalf("non-JSON Gemini frame: %v (%s)", err, line)
			}
			if _, ok := frame["candidates"]; !ok {
				t.Fatalf("frame without candidates: %s", line)
			}
		}
	}
}

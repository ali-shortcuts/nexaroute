package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBedrockEncodeRequest(t *testing.T) {
	in := Request{
		System: []Part{{Type: PartText, Text: "be terse"}},
		Messages: []Message{
			{Role: "user", Parts: []Part{{Type: PartText, Text: "hi"}}},
			{Role: "assistant", Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "tu1", Name: "lookup", Arguments: `{"q":"x"}`}}}},
			{Role: "user", Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ToolUseID: "tu1", Content: "answer"}}}},
		},
		Tools:           []ToolDef{{Name: "lookup", Description: "d", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
		ToolChoice:      &ToolChoice{Mode: "required"},
		MaxOutputTokens: 128,
		Temperature:     bedrockPtrF64(0.5),
		Stop:            []string{"END"},
	}
	payload, model, err := EncodeBedrockRequest(in, "anthropic.claude-v1")
	if err != nil {
		t.Fatal(err)
	}
	if model != "anthropic.claude-v1" {
		t.Fatalf("model=%q", model)
	}
	var got BedrockRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.System) != 1 || got.System[0].Text != "be terse" {
		t.Fatalf("system: %+v", got.System)
	}
	if len(got.Messages) != 3 || got.Messages[1].Content[0].ToolUse == nil ||
		got.Messages[1].Content[0].ToolUse.ToolUseID != "tu1" {
		t.Fatalf("messages: %+v", got.Messages)
	}
	if got.Messages[2].Content[0].ToolResult == nil ||
		got.Messages[2].Content[0].ToolResult.Content[0].Text != "answer" {
		t.Fatalf("tool result: %+v", got.Messages[2].Content[0])
	}
	if got.InferenceConfig == nil || got.InferenceConfig.MaxTokens != 128 ||
		got.InferenceConfig.Temperature == nil || *got.InferenceConfig.Temperature != 0.5 ||
		len(got.InferenceConfig.StopSequences) != 1 {
		t.Fatalf("inference config: %+v", got.InferenceConfig)
	}
	if got.ToolConfig == nil || len(got.ToolConfig.Tools) != 1 ||
		got.ToolConfig.Tools[0].ToolSpec.Name != "lookup" || got.ToolConfig.ToolChoice == nil ||
		got.ToolConfig.ToolChoice.Any == nil {
		t.Fatalf("tool config: %+v", got.ToolConfig)
	}
	// ToolChoice none drops the tools entirely (documented limit).
	in.ToolChoice = &ToolChoice{Mode: "none"}
	payload, _, err = EncodeBedrockRequest(in, "m")
	if err != nil {
		t.Fatal(err)
	}
	var none BedrockRequest
	if err := json.Unmarshal(payload, &none); err != nil {
		t.Fatal(err)
	}
	if none.ToolConfig != nil {
		t.Fatal("tool_choice none must omit toolConfig on Bedrock")
	}
}

func TestBedrockDecodeResponse(t *testing.T) {
	body := `{"output":{"message":{"role":"assistant","content":[{"text":"hello "},{"toolUse":{"toolUseId":"tu9","name":"lookup","input":{"q":"y"}}}]}},"stopReason":"tool_use","usage":{"inputTokens":11,"outputTokens":7,"totalTokens":18}}`
	resp, err := DecodeBedrockResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopToolUse || resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("resp: %+v", resp)
	}
	if len(resp.Blocks) != 2 || resp.Blocks[0].Text != "hello " ||
		resp.Blocks[1].ToolCall == nil || resp.Blocks[1].ToolCall.ID != "tu9" {
		t.Fatalf("blocks: %+v", resp.Blocks)
	}
}

func TestBedrockStreamChunkDecoding(t *testing.T) {
	events, terminal, err := DecodeBedrockStreamChunk(`{"eventType":"contentBlockDelta","payload":{"contentBlockIndex":0,"delta":{"text":"par"}}}`)
	if err != nil || terminal || len(events) != 1 || events[0].Type != StreamText || events[0].Text != "par" {
		t.Fatalf("delta: events=%+v terminal=%v err=%v", events, terminal, err)
	}
	events, terminal, err = DecodeBedrockStreamChunk(`{"eventType":"messageStop","payload":{"stopReason":"max_tokens"}}`)
	if err != nil || !terminal || len(events) != 1 || events[0].StopReason != StopMaxTokens {
		t.Fatalf("stop: events=%+v terminal=%v err=%v", events, terminal, err)
	}
	events, terminal, err = DecodeBedrockStreamChunk(`{"eventType":"metadata","payload":{"usage":{"inputTokens":5,"outputTokens":9}}}`)
	if err != nil || terminal || len(events) != 1 || events[0].Usage == nil || events[0].Usage.OutputTokens != 9 {
		t.Fatalf("metadata: events=%+v err=%v", events, err)
	}
	events, terminal, err = DecodeBedrockStreamChunk(`{"eventType":"throttlingException","payload":{"message":"slow down"}}`)
	if err != nil || !terminal || len(events) != 1 || events[0].Type != StreamError {
		t.Fatalf("exception: events=%+v terminal=%v err=%v", events, terminal, err)
	}
	if _, _, err := DecodeBedrockStreamChunk(`not json`); err == nil {
		t.Fatal("malformed envelope must error")
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "throttlingException") {
		t.Fatal("error event lost its code")
	}
}

func bedrockPtrF64(f float64) *float64 { return &f }

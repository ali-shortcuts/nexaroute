package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression: Anthropic SSE usage carries cache_read_input_tokens and
// cache_creation_input_tokens. The stream decoder ignored both, and the
// Anthropic emitter forwarded zeros to the client, undercounting cached
// prompt usage for canonical traffic.
func TestDecodeAnthropicStreamPreservesCacheUsage(t *testing.T) {
	evs, err := DecodeAnthropicStreamEvent("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":40,"cache_creation_input_tokens":7}}}`)
	if err != nil || len(evs) != 1 || evs[0].Usage == nil {
		t.Fatalf("events=%+v err=%v", evs, err)
	}
	u := evs[0].Usage
	if u.InputTokens != 10 || u.CacheReadTokens != 40 || u.CacheWriteTokens != 7 {
		t.Fatalf("cache usage dropped: %+v", u)
	}
}

func TestAnthropicEmitterEmitsCacheUsage(t *testing.T) {
	frames := captureAnthropicFrames(t, []StreamEvent{
		{Type: StreamStart, Usage: &Usage{InputTokens: 10, CacheReadTokens: 40, CacheWriteTokens: 7}},
		{Type: StreamText, Text: "ok"},
		{Type: StreamUsage, Usage: &Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 40, CacheWriteTokens: 7}},
		{Type: StreamEnd, StopReason: StopEndTurn},
	})
	var found bool
	for _, frame := range frames {
		parts := strings.SplitN(frame, " ", 2)
		if len(parts) != 2 || parts[0] != "message_delta" {
			continue
		}
		var payload struct {
			Usage struct {
				InputTokens         int `json:"input_tokens"`
				OutputTokens        int `json:"output_tokens"`
				CacheReadTokens     int `json:"cache_read_input_tokens"`
				CacheCreationTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(parts[1]), &payload); err != nil {
			t.Fatal(err)
		}
		found = true
		if payload.Usage.CacheReadTokens != 40 || payload.Usage.CacheCreationTokens != 7 {
			t.Fatalf("cache usage not emitted: %+v", payload.Usage)
		}
	}
	if !found {
		t.Fatalf("no message_delta frame in %v", frames)
	}
}

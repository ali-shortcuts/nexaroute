package stream

import (
	"strings"
	"testing"
)

func TestOpenAIDecoder(t *testing.T) {
	d := &OpenAIDecoder{}
	chunk1 := "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"
	chunk2 := "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n"
	done := "data: [DONE]\n\n"
	evs, err := d.Feed([]byte(chunk1 + chunk2))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != KindTextDelta || evs[0].Text != "Hel" {
		t.Fatalf("wrong events: %+v", evs)
	}
	evs, err = d.Feed([]byte(done))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[len(evs)-1].Kind != KindEnd {
		t.Fatalf("expected terminal end: %+v", evs)
	}
	if _, err := d.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIDecoderToolCalls(t *testing.T) {
	d := &OpenAIDecoder{}
	feed := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"get_time\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	evs, err := d.Feed([]byte(feed))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []Kind{}
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) < 3 || kinds[0] != KindToolCallStart || kinds[1] != KindToolCallDelta {
		t.Fatalf("wrong tool event sequence: %+v", evs)
	}
}

func TestOpenAIDecoderMissingDone(t *testing.T) {
	d := &OpenAIDecoder{}
	if _, err := d.Feed([]byte("data: {\"choices\":[{\"delta\":{}}]}\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Finish(); err == nil {
		t.Fatalf("missing [DONE] must fail Finish")
	}
}

func TestAnthropicDecoder(t *testing.T) {
	d := &AnthropicDecoder{}
	feed := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	evs, err := d.Feed([]byte(feed))
	if err != nil {
		t.Fatal(err)
	}
	fin, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	evs = append(evs, fin...)
	if len(evs) < 4 {
		t.Fatalf("expected >=4 events, got %+v", evs)
	}
	if evs[0].Kind != KindStart || evs[1].Kind != KindTextDelta {
		t.Fatalf("wrong prefix: %+v", evs)
	}
	last := evs[len(evs)-1]
	if last.Kind != KindEnd || last.FinishReason != "end_turn" {
		t.Fatalf("wrong terminal: %+v", last)
	}
}

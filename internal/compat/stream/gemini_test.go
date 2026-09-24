package stream

import (
	"strings"
	"testing"
)

func TestGeminiDecoder(t *testing.T) {
	d := &GeminiDecoder{}
	chunk1 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3}}\n\n"
	chunk2 := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	evs, err := d.Feed([]byte(chunk1 + chunk2))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != KindTextDelta || evs[0].Text != "Hel" || evs[1].Text != "lo" {
		t.Fatalf("wrong events: %+v", evs)
	}
	evs, err = d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != KindUsage || evs[0].InputTokens != 5 || evs[0].OutputTokens != 3 {
		t.Fatalf("wrong terminal events: %+v", evs)
	}
	if evs[1].Kind != KindEnd || evs[1].FinishReason != "stop" {
		t.Fatalf("wrong terminal events: %+v", evs)
	}
}

func TestGeminiDecoderDoneToleratedOnce(t *testing.T) {
	d := &GeminiDecoder{}
	feed := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n" +
		"data: [DONE]\n\n"
	evs, err := d.Feed([]byte(feed))
	if err != nil {
		t.Fatal(err)
	}
	ends := 0
	for _, e := range evs {
		if e.Kind == KindEnd {
			ends++
		}
	}
	rest, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rest {
		if e.Kind == KindEnd {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("expected exactly one terminal end, got %d: %+v %+v", ends, evs, rest)
	}
}

func TestGeminiDecoderMissingFinish(t *testing.T) {
	d := &GeminiDecoder{}
	if _, err := d.Feed([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Finish(); err == nil {
		t.Fatalf("missing finishReason must fail Finish")
	}
}

func TestGeminiDecoderFunctionCall(t *testing.T) {
	d := &GeminiDecoder{}
	feed := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"get_time\",\"args\":{\"city\":\"Paris\"}}}]},\"finishReason\":\"STOP\"}]}\n\n"
	evs, err := d.Feed([]byte(feed))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != KindToolCallStart || evs[0].ToolName != "get_time" {
		t.Fatalf("wrong tool events: %+v", evs)
	}
	if evs[1].Kind != KindToolCallDelta || !strings.Contains(evs[1].ToolArgs, "Paris") {
		t.Fatalf("wrong tool events: %+v", evs)
	}
	if evs[0].ToolID == "" || evs[0].ToolID != evs[1].ToolID {
		t.Fatalf("tool call start/delta must share an id: %+v", evs)
	}
}

func TestGeminiDecoderBlocked(t *testing.T) {
	d := &GeminiDecoder{}
	_, err := d.Feed([]byte("data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n"))
	if err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("expected blocked error, got %v", err)
	}
}

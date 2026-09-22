package httpapi

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapabilityDetectionIgnoresWordsInsideUserText(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"Explain the image format and the reasoning behind it"}]}`)
	if hasVisionOpenAI(raw) || hasVisionAnth(raw) || hasReasoningOpenAI(raw) || hasReasoningAnth(raw) {
		t.Fatal("plain user text must not imply vision or reasoning capability")
	}
}

func TestCapabilityDetectionFindsNestedContent(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}],"reasoning_effort":"high"}`)
	if !hasVisionOpenAI(raw) || !hasReasoningOpenAI(raw) {
		t.Fatal("nested image and reasoning fields should be detected")
	}
}

func TestReadJSONRejectsOversizedBody(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(strings.Repeat("x", maxJSONBodyBytes+1))))
	var dst map[string]any
	_, err := readJSON(req, &dst)
	if _, ok := err.(*requestTooLargeError); !ok {
		t.Fatalf("expected requestTooLargeError, got %T: %v", err, err)
	}
}

func TestSanitizeMetricLabelRemovesLineBreaksAndEscapes(t *testing.T) {
	got := sanitizeMetricLabel("provider\nname\rwith\\quote\"")
	if strings.ContainsAny(got, "\n\r\\\"") {
		t.Fatalf("metric label still contains unsafe characters: %q", got)
	}
}

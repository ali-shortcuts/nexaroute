package core

import (
	"encoding/json"
	"testing"
)

// Finding F7: ParseSystem had 0% coverage while ParseAnthContent was covered.
// These tests assert the observable behavior of system-prompt normalization.
func TestParseSystemStringForm(t *testing.T) {
	got := ParseSystem(json.RawMessage(`"You are helpful"`))
	if got != "You are helpful" {
		t.Fatalf("got %q", got)
	}
}

func TestParseSystemEmptyAndInvalid(t *testing.T) {
	if got := ParseSystem(nil); got != "" {
		t.Fatalf("nil should yield empty, got %q", got)
	}
	if got := ParseSystem(json.RawMessage(`12345`)); got != "" {
		t.Fatalf("non-string non-blocks should yield empty, got %q", got)
	}
	if got := ParseSystem(json.RawMessage(`{}`)); got != "" {
		t.Fatalf("object should yield empty, got %q", got)
	}
}

func TestParseSystemTextBlocksJoined(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"Be concise."},{"type":"image","text":"ignored"},{"type":"text","text":"Answer briefly."}]`)
	got := ParseSystem(raw)
	want := "Be concise.\nAnswer briefly."
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestParseAnthContentForms(t *testing.T) {
	if blocks, err := ParseAnthContent(nil); err != nil || blocks != nil {
		t.Fatalf("nil should yield nil,nil got %v,%v", blocks, err)
	}
	blocks, err := ParseAnthContent(json.RawMessage(`"hello"`))
	if err != nil || len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Fatalf("string form not normalized: %v %v", blocks, err)
	}
	blocks, err = ParseAnthContent(json.RawMessage(`[{"type":"text","text":"hi"}]`))
	if err != nil || len(blocks) != 1 || blocks[0].Text != "hi" {
		t.Fatalf("blocks form not parsed: %v %v", blocks, err)
	}
	if _, err := ParseAnthContent(json.RawMessage(`{bad`)); err == nil {
		t.Fatal("malformed JSON should return an error")
	}
}

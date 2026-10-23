package canonical

import (
	"encoding/json"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// A whitespace-only system prompt must not enter the IR: every system
// encoder gates on the trimmed joined text, so a blank system part
// admitted at decode vanishes on encode and breaks decode/encode
// round-trip stability. Found by the extended fuzz run of 2026-10-23
// (FuzzRequestRoundTrip crasher ed14165b405e353c, kept as a corpus
// seed); the instability reproduced identically on the pre-fix tree,
// i.e. it is not a regression of the fail-closed decoder work.

func TestDecodeResponsesRequestDropsBlankInstructions(t *testing.T) {
	var in ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","instructions":" ","input":"hi"}`), &in); err != nil {
		t.Fatal(err)
	}
	out, err := DecodeResponsesRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.System) != 0 {
		t.Fatalf("blank instructions entered the IR as system: %+v", out.System)
	}

	// Control: real instructions still decode.
	if err := json.Unmarshal([]byte(`{"model":"m","instructions":"be brief","input":"hi"}`), &in); err != nil {
		t.Fatal(err)
	}
	out, err = DecodeResponsesRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.System) != 1 || out.System[0].Text != "be brief" {
		t.Fatalf("real instructions decoded incorrectly: %+v", out.System)
	}
}

func TestDecodeOpenAIChatRequestDropsBlankSystem(t *testing.T) {
	for name, content := range map[string]any{
		"string": " ",
		"parts":  []any{map[string]any{"type": "text", "text": " "}},
	} {
		out, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
			Model:    "m",
			Messages: []core.OpenAIMessage{{Role: "system", Content: content}},
		}, "m")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(out.System) != 0 {
			t.Fatalf("%s: blank system entered the IR: %+v", name, out.System)
		}
	}

	// Control: a real system prompt still decodes.
	out, err := DecodeOpenAIChatRequest(core.OpenAIRequest{
		Model:    "m",
		Messages: []core.OpenAIMessage{{Role: "system", Content: "be brief"}},
	}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.System) != 1 || out.System[0].Text != "be brief" {
		t.Fatalf("real system prompt decoded incorrectly: %+v", out.System)
	}
}

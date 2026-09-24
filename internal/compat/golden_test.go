package compat

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat/canonical"
	compatstream "github.com/ali-shortcuts/nexaroute/internal/compat/stream"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

func loadJSON(t *testing.T, path string, dst any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatal(err)
	}
}

// Golden: Anthropic request -> Canonical IR -> NVIDIA (OpenAI dialect) request.
func TestGoldenAnthropicToNVIDIA(t *testing.T) {
	var anth core.AnthropicRequest
	loadJSON(t, "testdata/anthropic/request.json", &anth)
	canon, err := canonical.FromAnthropicRequest(anth)
	if err != nil {
		t.Fatal(err)
	}
	if !canon.HasTools() || canon.System == "" {
		t.Fatalf("canonical lost data: %+v", canon)
	}
	got := canon.ToOpenAIRequest("meta/llama-test")
	gotJSON, _ := json.Marshal(got)
	var gotObj map[string]any
	_ = json.Unmarshal(gotJSON, &gotObj)
	var wantObj map[string]any
	loadJSON(t, "testdata/nvidia/request.json", &wantObj)
	// Compare semantically: model/messages/tools/tool_choice/temperature.
	for _, key := range []string{"model", "temperature", "tool_choice"} {
		if !reflect.DeepEqual(gotObj[key], wantObj[key]) {
			t.Fatalf("%s mismatch:\n got=%v\nwant=%v", key, gotObj[key], wantObj[key])
		}
	}
	gotMsgs, _ := json.Marshal(gotObj["messages"])
	wantMsgs, _ := json.Marshal(wantObj["messages"])
	var gm, wm []map[string]any
	_ = json.Unmarshal(gotMsgs, &gm)
	_ = json.Unmarshal(wantMsgs, &wm)
	if len(gm) != len(wm) {
		t.Fatalf("message count %d != %d\n got=%s\nwant=%s", len(gm), len(wm), gotMsgs, wantMsgs)
	}
	for i := range gm {
		if gm[i]["role"] != wm[i]["role"] {
			t.Fatalf("msg[%d] role %v != %v", i, gm[i]["role"], wm[i]["role"])
		}
	}
	// Cross-check against the battle-tested direct translator: same shape.
	direct, _, err := translate.AnthropicToOpenAI(anth, "meta/llama-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Messages) != len(got.Messages) {
		t.Fatalf("canonical path diverged from direct translator: %d != %d", len(got.Messages), len(direct.Messages))
	}
}

// Golden reverse: NVIDIA response -> Canonical -> Anthropic-shaped assertions.
func TestGoldenNVIDIAToAnthropic(t *testing.T) {
	var nvidia core.OpenAIResponse
	loadJSON(t, "testdata/nvidia/response.json", &nvidia)
	canon := canonical.FromOpenAIResponse(nvidia)
	if canon.Text != "It is 21:00 in Paris." {
		t.Fatalf("text = %q", canon.Text)
	}
	if canon.InputTokens != 40 || canon.OutputTokens != 10 {
		t.Fatalf("usage = %d/%d", canon.InputTokens, canon.OutputTokens)
	}
	var want core.AnthResponse
	loadJSON(t, "testdata/anthropic/response.json", &want)
	anthCanon := canonical.FromAnthropicResponse(want)
	if anthCanon.Text != canon.Text {
		t.Fatalf("round trip text %q != %q", anthCanon.Text, canon.Text)
	}
}

// Golden SSE: NVIDIA/OpenAI stream decodes event-by-event into canonical events.
func TestGoldenStreamEvents(t *testing.T) {
	raw, err := os.ReadFile("testdata/openai/stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	d := &compatstream.OpenAIDecoder{}
	evs, err := d.Feed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) < 3 {
		t.Fatalf("expected >=3 events, got %+v", evs)
	}
	if evs[0].Kind != compatstream.KindTextDelta || evs[0].Text != "It is" {
		t.Fatalf("event[0] = %+v", evs[0])
	}
	if evs[1].Kind != compatstream.KindTextDelta || evs[1].Text != " 21:00" {
		t.Fatalf("event[1] = %+v", evs[1])
	}
	last := evs[len(evs)-1]
	if last.Kind != compatstream.KindEnd || last.FinishReason != "stop" {
		t.Fatalf("terminal = %+v", last)
	}
	text := ""
	for _, e := range evs {
		if e.Kind == compatstream.KindTextDelta {
			text += e.Text
		}
	}
	if text != "It is 21:00" {
		t.Fatalf("concatenated text = %q", text)
	}
	if _, err := d.Finish(); err != nil {
		t.Fatal(err)
	}
}

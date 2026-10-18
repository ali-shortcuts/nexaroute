package feature

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyClient(t *testing.T) {
	cases := []struct {
		ua   string
		want ClientClass
	}{
		{"", ClientClassUnknown},
		{"   ", ClientClassUnknown},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36", ClientClassWeb},
		{"Mozilla/5.0 Firefox/121.0", ClientClassWeb},
		{"curl/8.4.0", ClientClassCLI},
		{"Wget/1.21", ClientClassCLI},
		{"HTTPie/3.2", ClientClassCLI},
		{"openai-python/1.12.0", ClientClassSDK},
		{"anthropic-sdk-typescript/0.20", ClientClassSDK},
		{"claude-code/1.0.0", ClientClassAgent},
		{"codex-cli/0.2.1", ClientClassAgent},
		{"opencode/0.3", ClientClassAgent},
		{"aider-chat/0.50", ClientClassAgent},
		{"some-random-bot/9.9", ClientClassUnknown},
	}
	for _, c := range cases {
		if got := ClassifyClient(c.ua); got != c.want {
			t.Errorf("ClassifyClient(%q) = %q, want %q", c.ua, got, c.want)
		}
		if !ClassifyClient(c.ua).Valid() {
			t.Errorf("ClassifyClient(%q) returned invalid class", c.ua)
		}
	}
}

func TestExtractor_SetsClientClass(t *testing.T) {
	e := NewExtractor()
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	f := e.Extract(raw, ExtractOptions{
		Protocol:      ProtocolOpenAI,
		ContentFields: []string{"messages"},
		UserAgent:     "claude-code/1.0.0",
	})
	if f.ClientClass != ClientClassAgent {
		t.Fatalf("ClientClass = %q, want agent", f.ClientClass)
	}
	// The raw UA must never be retained in the features.
	raw2, _ := json.Marshal(f)
	if strings.Contains(string(raw2), "claude-code") {
		t.Fatal("raw User-Agent leaked into RequestFeatures JSON")
	}
}

func TestExtractor_AudioDetection(t *testing.T) {
	e := NewExtractor()
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"transcribe"},{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}},{"type":"input_audio","input_audio":{"data":"BBBB","format":"mp3"}}]}]}`)
	f := e.Extract(raw, ExtractOptions{
		Protocol:      ProtocolOpenAI,
		ContentFields: []string{"messages"},
		AudioType:     "input_audio",
	})
	if !f.HasAudio || f.AudioCount != 2 {
		t.Fatalf("HasAudio=%v AudioCount=%d, want true/2", f.HasAudio, f.AudioCount)
	}
	// Disabled when AudioType is empty.
	g := e.Extract(raw, ExtractOptions{Protocol: ProtocolOpenAI, ContentFields: []string{"messages"}})
	if g.HasAudio || g.AudioCount != 0 {
		t.Fatalf("audio detected without AudioType opt: %+v", g)
	}
}

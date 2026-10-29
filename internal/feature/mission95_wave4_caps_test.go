package feature

import (
	"encoding/json"
	"strconv"
	"testing"
)

func wave4FeatureExtract(t *testing.T, body any, opts ExtractOptions) RequestFeatures {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return NewExtractor().Extract(raw, opts)
}

// More vision items than the cap: the count saturates at the cap instead of
// growing unbounded, while the presence flag stays true.
func TestWave4ExtractVisionCountSaturatesAtCap(t *testing.T) {
	msgs := make([]any, 0, 150)
	for i := 0; i < 150; i++ {
		msgs = append(msgs, map[string]any{"type": "image_url"})
	}
	feat := wave4FeatureExtract(t, map[string]any{"messages": msgs}, ExtractOptions{
		VisionType: "image_url",
	})
	if !feat.HasVision {
		t.Fatal("expected HasVision")
	}
	if feat.VisionImageCount != 100 {
		t.Fatalf("VisionImageCount = %d, want 100 (cap)", feat.VisionImageCount)
	}
}

// More audio items than the cap: same saturation behavior for audio.
func TestWave4ExtractAudioCountSaturatesAtCap(t *testing.T) {
	msgs := make([]any, 0, 150)
	for i := 0; i < 150; i++ {
		msgs = append(msgs, map[string]any{"type": "input_audio"})
	}
	feat := wave4FeatureExtract(t, map[string]any{"messages": msgs}, ExtractOptions{
		AudioType: "input_audio",
	})
	if !feat.HasAudio {
		t.Fatal("expected HasAudio")
	}
	if feat.AudioCount != 100 {
		t.Fatalf("AudioCount = %d, want 100 (cap)", feat.AudioCount)
	}
}

// More role-bearing messages than the cap: the message count saturates.
func TestWave4ExtractMessageCountSaturatesAtCap(t *testing.T) {
	msgs := make([]any, 0, 10050)
	for i := 0; i < 10050; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": "hi"})
	}
	feat := wave4FeatureExtract(t, map[string]any{"messages": msgs}, ExtractOptions{})
	if feat.MessageCount != 10000 {
		t.Fatalf("MessageCount = %d, want 10000 (cap)", feat.MessageCount)
	}
}

// A content map carrying an image_url key is observed as image-URL presence,
// independent of the vision-type detection.
func TestWave4ExtractImageURLKeyObserved(t *testing.T) {
	feat := wave4FeatureExtract(t, map[string]any{"messages": []any{
		map[string]any{"type": "text", "image_url": "https://example.invalid/x.png"},
	}}, ExtractOptions{})
	if !feat.HasImageURL {
		t.Fatal("expected HasImageURL for a content part carrying an image_url key")
	}
}

// A single content map wider than the remaining node budget aborts the walk
// as too complex instead of expanding unboundedly.
func TestWave4ExtractOversizedMapAbortsWalk(t *testing.T) {
	wide := make(map[string]any, 100000)
	for i := 0; i < 100000; i++ {
		wide["key-"+strconv.Itoa(i)] = i
	}
	if len(wide) != 100000 {
		t.Fatalf("test setup: wide map has only %d keys", len(wide))
	}
	feat := wave4FeatureExtract(t, map[string]any{"messages": wide}, ExtractOptions{})
	if !feat.TooComplex {
		t.Fatal("expected TooComplex for a content map wider than the node budget")
	}
}

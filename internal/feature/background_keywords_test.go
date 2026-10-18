package feature

import "testing"

// #421 item 3: the background-chore lexical signal fires on
// agent housekeeping about its OWN session (titling the
// conversation, summarizing the conversation) and NOT on
// content work — summarizing an article is the operator's main
// task (extraction), not a background chore.
func TestExtractorBackgroundKeywords(t *testing.T) {
	ext := NewExtractor()
	opts := ExtractOptions{ContentFields: []string{"messages", "input"}}
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"title generation", "Generate a title for this conversation in 5 words or fewer.", true},
		{"name the chat", "Name this chat based on what we discussed.", true},
		{"summarize conversation", "Please summarize this conversation so far.", true},
		{"tldr", "tl;dr of the thread above please", true},
		{"one-line summary", "Give a one-line summary of this conversation.", true},
		{"summarize article is content work", "Summarize this article about climate policy in three paragraphs.", false},
		{"extract article", "Extract the key points from this document as JSON.", false},
		{"plain chat", "What is the capital of France?", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"m","messages":[{"role":"user","content":"` + tc.text + `"}]}`)
			feat := ext.Extract(raw, opts)
			if feat.HasBackgroundKeywords != tc.want {
				t.Fatalf("HasBackgroundKeywords = %v, want %v (features: %+v)", feat.HasBackgroundKeywords, tc.want, feat)
			}
		})
	}
}

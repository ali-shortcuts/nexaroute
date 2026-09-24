package canonical

import (
	"strings"
	"testing"
)

func TestSSEReaderBoundsAggregateMultilineEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1024) + "\n"
	body := strings.Repeat(line, (8<<20)/len(line)+2) + "\n"
	reader := NewSSEReader(strings.NewReader(body))
	if _, _, done, err := reader.Next(); err == nil || done {
		t.Fatalf("oversized multi-line SSE event was accepted: done=%t err=%v", done, err)
	}
}

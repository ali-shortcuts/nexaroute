package canonical

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSSEReaderRejectsOversizedMultilineEvent(t *testing.T) {
	var b strings.Builder
	chunk := strings.Repeat("x", 64<<10)
	for i := 0; i < 129; i++ {
		b.WriteString("data: ")
		b.WriteString(chunk)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, _, _, err := NewSSEReader(strings.NewReader(b.String())).Next()
	if err == nil || !strings.Contains(err.Error(), "safe limit") {
		t.Fatalf("expected bounded SSE error, got %v", err)
	}
}

func TestResponsesEmitterRejectsToolDeltaBeforeStart(t *testing.T) {
	e := NewResponsesEmitter(httptest.NewRecorder(), "model")
	err := e.Emit(StreamEvent{Type: StreamToolDelta, ToolIndex: 1, ArgsDelta: "{}"})
	if err == nil || !strings.Contains(err.Error(), "before tool start") {
		t.Fatalf("expected tool ordering error, got %v", err)
	}
}

func TestMessageClockConcurrentUniqueness(t *testing.T) {
	const n = 2000
	ids := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- messageClock()
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[int64]struct{}, n)
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate message clock id %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("ids=%d want %d", len(seen), n)
	}
}

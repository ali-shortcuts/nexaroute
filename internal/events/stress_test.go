package events

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestStressConcurrentEventAndSnapshotFlood(t *testing.T) {
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
	b := New(500)
	var writers sync.WaitGroup
	for g := 0; g < 64; g++ {
		writers.Add(1)
		go func(g int) {
			defer writers.Done()
			for i := 0; i < 2000; i++ {
				b.Add(Event{Kind: fmt.Sprintf("kind-%d", i%400), ErrorType: fmt.Sprintf("err-%d", i%400), Message: "stress"})
				if i%50 == 0 {
					_ = b.SnapshotLimit(100)
					_ = b.Counts()
					_ = b.ErrorCounts()
				}
			}
		}(g)
	}
	writers.Wait()
	if got := len(b.Snapshot()); got != 500 {
		t.Fatalf("ring size=%d want 500", got)
	}
	if len(b.Counts()) > maxCounterKeys || len(b.ErrorCounts()) > maxCounterKeys {
		t.Fatal("bounded counter maps grew beyond their hard limit")
	}
}

package events

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestBusRingKeepsNewestEventsInOrder(t *testing.T) {
	b := New(10)
	for i := 0; i < 25; i++ {
		b.Add(Event{Kind: "route", Message: fmt.Sprintf("%02d", i)})
	}
	got := b.Snapshot()
	if len(got) != 10 {
		t.Fatalf("snapshot len=%d want 10", len(got))
	}
	for i, e := range got {
		want := fmt.Sprintf("%02d", i+15)
		if e.Message != want {
			t.Fatalf("snapshot[%d]=%q want %q", i, e.Message, want)
		}
	}
	if b.Counts()["route"] != 25 {
		t.Fatalf("cumulative count=%d want 25", b.Counts()["route"])
	}
}

func TestBusRingTracksErrorCountsAcrossEviction(t *testing.T) {
	b := New(10)
	for i := 0; i < 20; i++ {
		b.Add(Event{Kind: "route_fail", ErrorType: "provider_timeout"})
	}
	if got := b.ErrorCounts()["provider_timeout"]; got != 20 {
		t.Fatalf("error count=%d want 20", got)
	}
}

func TestBusBoundsDynamicCounterKeysAndEventPayloads(t *testing.T) {
	b := New(10)
	huge := strings.Repeat("x", maxEventMessage*4)
	for i := 0; i < 2000; i++ {
		b.Add(Event{
			RequestID:  strings.Repeat("r", maxEventRequestID*2),
			Kind:       fmt.Sprintf("kind-%d", i),
			Deployment: strings.Repeat("d", maxEventDeployment*2),
			Message:    huge,
			ErrorType:  fmt.Sprintf("error-%d", i),
		})
	}
	if got := len(b.Counts()); got > maxCounterKeys {
		t.Fatalf("kind counter map grew to %d", got)
	}
	if got := len(b.ErrorCounts()); got > maxCounterKeys {
		t.Fatalf("error counter map grew to %d", got)
	}
	if b.Counts()[counterOverflowKey] == 0 || b.ErrorCounts()[counterOverflowKey] == 0 {
		t.Fatal("dynamic counter overflow was not aggregated")
	}
	for _, e := range b.Snapshot() {
		if len(e.RequestID) > maxEventRequestID || len(e.Kind) > maxEventKind ||
			len(e.Deployment) > maxEventDeployment || len(e.Message) > maxEventMessage ||
			len(e.ErrorType) > maxEventErrorType {
			t.Fatalf("event exceeded bounded field sizes: %+v", e)
		}
	}
}

func TestBusConcurrentFloodRemainsBounded(t *testing.T) {
	b := New(64)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				b.Add(Event{Kind: fmt.Sprintf("g-%d-%d", g, i), ErrorType: fmt.Sprintf("e-%d-%d", g, i), Message: "x"})
			}
		}(g)
	}
	wg.Wait()
	if len(b.Snapshot()) != 64 {
		t.Fatalf("ring size=%d want 64", len(b.Snapshot()))
	}
	if len(b.Counts()) > maxCounterKeys || len(b.ErrorCounts()) > maxCounterKeys {
		t.Fatal("counter maps escaped their fixed bounds")
	}
}

func TestSubscribeSnapshotDeliversHistoryThenLiveEvents(t *testing.T) {
	b := New(10)
	b.Add(Event{Kind: "historical", Message: "first"})
	snapshot, live, cancel, ok := b.SubscribeSnapshot(10)
	if !ok || len(snapshot) != 1 || snapshot[0].Kind != "historical" {
		t.Fatalf("subscription snapshot=%+v ok=%t", snapshot, ok)
	}
	b.Add(Event{Kind: "live", Message: "second"})
	select {
	case got := <-live:
		if got.Kind != "live" || got.Message != "second" {
			t.Fatalf("live event=%+v", got)
		}
	default:
		t.Fatal("live event was not delivered")
	}
	cancel()
	cancel()
	if _, open := <-live; open {
		t.Fatal("cancelled subscription channel remains open")
	}
}

func TestEventSequenceIsMonotonicAndResumable(t *testing.T) {
	b := New(10)
	b.Add(Event{Kind: "one"})
	b.Add(Event{Kind: "two"})
	all := b.Snapshot()
	if len(all) != 2 || all[0].Seq == 0 || all[1].Seq != all[0].Seq+1 {
		t.Fatalf("unexpected sequence values: %+v", all)
	}
	resumed := b.SnapshotSince(all[0].Seq, 10)
	if len(resumed) != 1 || resumed[0].Kind != "two" {
		t.Fatalf("unexpected resumed events: %+v", resumed)
	}
}

func TestLiveSubscribersAreBoundedAndNeverBlockProducers(t *testing.T) {
	b := New(10)
	cancels := make([]func(), 0, maxLiveSubscribers)
	for i := 0; i < maxLiveSubscribers; i++ {
		_, _, cancel, ok := b.SubscribeSnapshot(1)
		if !ok {
			t.Fatalf("subscription %d unexpectedly rejected", i)
		}
		cancels = append(cancels, cancel)
	}
	if _, _, _, ok := b.SubscribeSnapshot(1); ok {
		t.Fatal("subscriber budget exceeded")
	}
	for i := 0; i < liveSubscriberBuffer*4; i++ {
		b.Add(Event{Kind: "burst", Message: fmt.Sprintf("%d", i)})
	}
	for _, cancel := range cancels {
		cancel()
	}
	if _, _, cancel, ok := b.SubscribeSnapshot(1); !ok {
		t.Fatal("subscriber capacity was not released")
	} else {
		cancel()
	}
}

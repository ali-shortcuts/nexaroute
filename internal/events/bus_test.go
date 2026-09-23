package events

import (
	"fmt"
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

package events

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Agent-D1 coverage: boundedString rune-boundary backup, explicit
// sequence handling, snapshot limits, nil subscriber map, Epoch/Cursor,
// and lifecycle aggregation edge cases (filter, failover without an
// attempt, zero-time events, direct merge field filling).

func TestAgentD1BusNewClampsSmallMax(t *testing.T) {
	b := New(3)
	for i := 0; i < 11; i++ {
		b.Add(Event{Kind: "x"})
	}
	snap := b.Snapshot()
	if len(snap) != 10 {
		t.Fatalf("clamped bus holds %d events, want 10", len(snap))
	}
}

func TestAgentD1BusAddExplicitSeqAdvancesCursor(t *testing.T) {
	b := New(10)
	b.Add(Event{Kind: "a", Seq: 5})
	if got := b.Cursor(); got != 5 {
		t.Fatalf("Cursor after explicit Seq 5 = %d, want 5", got)
	}
	b.Add(Event{Kind: "b", Seq: 3})
	if got := b.Cursor(); got != 5 {
		t.Fatalf("Cursor after smaller explicit Seq 3 = %d, want 5 (must not rewind)", got)
	}
	snap := b.Snapshot()
	if len(snap) != 2 || snap[0].Seq != 5 || snap[1].Seq != 3 {
		t.Fatalf("snapshot seqs = %v, want [5 3]", seqsOf(snap))
	}
}

func seqsOf(list []Event) []uint64 {
	out := make([]uint64, len(list))
	for i, e := range list {
		out[i] = e.Seq
	}
	return out
}

func TestAgentD1BusBoundedStringBacksUpToRuneBoundary(t *testing.T) {
	b := New(10)
	// 127 ASCII bytes then a 3-byte rune: truncating at 128 bytes would
	// split the rune, so boundedString must back up one byte.
	kind := strings.Repeat("a", 127) + "€" + strings.Repeat("b", 10)
	b.Add(Event{Kind: kind})
	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	got := snap[0].Kind
	if !utf8.ValidString(got) {
		t.Fatalf("truncated kind is not valid UTF-8: %q", got)
	}
	if len(got) != 127 {
		t.Fatalf("truncated kind len = %d, want 127 (backed up past the split rune)", len(got))
	}
	if got != strings.Repeat("a", 127) {
		t.Fatalf("truncated kind = %q, want 127 a's", got)
	}
}

func TestAgentD1BusSubscribeInitializesNilSubscriberMap(t *testing.T) {
	b := &Bus{
		max:         10,
		items:       make([]Event, 10),
		counts:      map[string]uint64{},
		errorCounts: map[string]uint64{},
		// subscribers deliberately nil: SubscribeSnapshotSince must
		// allocate the map instead of panicking.
	}
	snap, ch, cancel, ok := b.SubscribeSnapshotSince(0, 0)
	if !ok {
		t.Fatal("subscribe on a bus with a nil subscriber map should succeed")
	}
	if snap == nil {
		t.Fatal("snapshot should be non-nil")
	}
	if ch == nil {
		t.Fatal("live channel should be non-nil")
	}
	if got := b.NumSubscribers(); got != 1 {
		t.Fatalf("NumSubscribers = %d, want 1", got)
	}
	cancel()
	if got := b.NumSubscribers(); got != 0 {
		t.Fatalf("NumSubscribers after cancel = %d, want 0", got)
	}
}

func TestAgentD1BusEpochAndCursor(t *testing.T) {
	b := New(10)
	epoch := b.Epoch()
	if epoch == 0 {
		t.Fatal("Epoch must be non-zero")
	}
	if got := b.Epoch(); got != epoch {
		t.Fatalf("Epoch changed across calls: %d vs %d", got, epoch)
	}
	if got := b.Cursor(); got != 0 {
		t.Fatalf("Cursor on an empty bus = %d, want 0", got)
	}
	b.Add(Event{Kind: "x"})
	if got := b.Cursor(); got != 1 {
		t.Fatalf("Cursor after one Add = %d, want 1", got)
	}
	ev := b.Snapshot()[0]
	if ev.Epoch != epoch {
		t.Fatalf("event epoch = %d, want bus epoch %d", ev.Epoch, epoch)
	}
}

func TestAgentD1BusSnapshotLimitTakesNewest(t *testing.T) {
	b := New(10)
	for i := 0; i < 5; i++ {
		b.Add(Event{Kind: "k"})
	}
	got := b.SnapshotLimit(2)
	if len(got) != 2 {
		t.Fatalf("SnapshotLimit(2) len = %d, want 2", len(got))
	}
	if got[0].Seq != 4 || got[1].Seq != 5 {
		t.Fatalf("SnapshotLimit(2) seqs = %v, want [4 5] (newest two)", seqsOf(got))
	}
}

func TestAgentD1SummarizeFiltersUnknownKindWithoutPhase(t *testing.T) {
	now := time.Now()
	out := Summarize([]Event{
		{RequestID: "r1", Kind: "something_unrelated", Seq: 1, Time: now},
	}, SummarizeOptions{Now: now})
	if len(out) != 0 {
		t.Fatalf("Summarize kept %d lifecycles for an unknown kind, want 0", len(out))
	}
}

func TestAgentD1SummarizeFailoverWithoutAttempt(t *testing.T) {
	now := time.Now()
	out := Summarize([]Event{
		{RequestID: "r1", Kind: "failover", FailoverClass: "timeout", Seq: 1, Time: now},
	}, SummarizeOptions{Now: now})
	if len(out) != 1 {
		t.Fatalf("Summarize returned %d lifecycles, want 1", len(out))
	}
	lc := out[0]
	if lc.FailoverCount != 1 {
		t.Fatalf("FailoverCount = %d, want 1", lc.FailoverCount)
	}
	if lc.FailoverClass != "timeout" {
		t.Fatalf("FailoverClass = %q, want timeout", lc.FailoverClass)
	}
	if len(lc.Attempts) != 0 {
		t.Fatalf("Attempts = %d, want 0 (no attempt event preceded the failover)", len(lc.Attempts))
	}
}

func TestAgentD1SummarizeZeroTimeEvents(t *testing.T) {
	// An event with a zero timestamp still forms a lifecycle; finalize
	// must cope with both StartedAt and LastEventAt staying zero.
	out := Summarize([]Event{
		{RequestID: "r1", Kind: "route_ok", Seq: 1},
	}, SummarizeOptions{})
	if len(out) != 1 {
		t.Fatalf("Summarize returned %d lifecycles, want 1", len(out))
	}
	lc := out[0]
	if !lc.StartedAt.IsZero() || !lc.LastEventAt.IsZero() {
		t.Fatalf("zero-time lifecycle has non-zero times: started=%v last=%v", lc.StartedAt, lc.LastEventAt)
	}
	if lc.Terminal != TerminalSucceeded {
		t.Fatalf("Terminal = %q, want %q", lc.Terminal, TerminalSucceeded)
	}
}

func TestAgentD1LifecycleMergeFillsEmptyCorrelationID(t *testing.T) {
	// merge is only reached from Summarize, where the lifecycle is
	// always created with a non-empty correlation id; exercise the
	// fill branch directly so the contract (first event wins) is
	// covered: an empty lifecycle adopts the event's correlation id.
	now := time.Now()
	lc := &RequestLifecycle{}
	lc.merge(Event{RequestID: "r1", CorrelationID: "c1", Kind: "route_ok", Time: now})
	if lc.CorrelationID != "c1" {
		t.Fatalf("CorrelationID = %q, want c1", lc.CorrelationID)
	}
	if lc.RequestID != "r1" {
		t.Fatalf("RequestID = %q, want r1", lc.RequestID)
	}
	if lc.Terminal != TerminalSucceeded {
		t.Fatalf("Terminal = %q, want %q", lc.Terminal, TerminalSucceeded)
	}
	// A second event must not overwrite the already-filled ids.
	lc.merge(Event{RequestID: "r2", CorrelationID: "c2", Kind: "route_ok", Time: now})
	if lc.CorrelationID != "c1" || lc.RequestID != "r1" {
		t.Fatalf("merge overwrote ids: request=%q correlation=%q, want r1/c1", lc.RequestID, lc.CorrelationID)
	}
}

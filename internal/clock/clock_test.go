package clock

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestFakeAdvanceFiresDueTimersOnly(t *testing.T) {
	clk := NewFake(time.Unix(1_700_000_000, 0).UTC())
	var early, late atomic.Int32
	clk.AfterFunc(30*time.Minute, func() { early.Add(1) })
	clk.AfterFunc(60*time.Minute, func() { late.Add(1) })
	clk.Advance(30 * time.Minute)
	if early.Load() != 1 {
		t.Fatalf("30m timer fired=%d want 1", early.Load())
	}
	if late.Load() != 0 {
		t.Fatalf("60m timer fired early: %d", late.Load())
	}
	clk.Advance(30 * time.Minute)
	if late.Load() != 1 {
		t.Fatalf("60m timer fired=%d want 1", late.Load())
	}
}

func TestFakeStopPreventsFire(t *testing.T) {
	clk := NewFake(time.Time{})
	var n atomic.Int32
	timer := clk.AfterFunc(time.Minute, func() { n.Add(1) })
	if !timer.Stop() {
		t.Fatal("Stop should succeed before fire")
	}
	clk.Advance(time.Hour)
	if n.Load() != 0 {
		t.Fatalf("stopped timer fired")
	}
}

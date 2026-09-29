package health

import (
	"testing"
	"time"
)

func TestCircuitStateMapping(t *testing.T) {
	cases := map[Status]string{
		Healthy:  "CLOSED",
		Degraded: "CLOSED",
		Unknown:  "CLOSED",
		HalfOpen: "HALF_OPEN",
		Cooldown: "OPEN",
		Retired:  "OPEN",
	}
	for in, want := range cases {
		if got := CircuitStateFor(in); got != want {
			t.Fatalf("CircuitStateFor(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRichStateMapping(t *testing.T) {
	if got := RichStateFor(State{Status: Healthy}); got != "HEALTHY" {
		t.Fatalf("healthy => %q", got)
	}
	if got := RichStateFor(State{Status: Degraded}); got != "DEGRADED" {
		t.Fatalf("degraded => %q", got)
	}
	if got := RichStateFor(State{Status: HalfOpen}); got != "RECOVERING" {
		t.Fatalf("half_open => %q", got)
	}
	if got := RichStateFor(State{Status: Cooldown}); got != "COOLDOWN" {
		t.Fatalf("cooldown => %q", got)
	}
	if got := RichStateFor(State{Status: Retired}); got != "DISABLED" {
		t.Fatalf("retired => %q", got)
	}
	if got := RichStateFor(State{Status: Unknown}); got != "UNKNOWN" {
		t.Fatalf("unknown never-checked => %q", got)
	}
	if got := RichStateFor(State{Status: Unknown, LastChecked: time.Now()}); got != "CHECKING" {
		t.Fatalf("unknown checked => %q", got)
	}
}

func TestDerivedLatencyAliases(t *testing.T) {
	st := State{EWMALatencyMS: 42.5, EWMAFailureRate: 0.25}
	if AverageLatencyMS(st) != 42.5 {
		t.Fatalf("average latency alias broken: %v", AverageLatencyMS(st))
	}
	if RecentErrorRate(st) != 0.25 {
		t.Fatalf("recent error rate alias broken: %v", RecentErrorRate(st))
	}
}

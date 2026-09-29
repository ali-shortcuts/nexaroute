package httpapi

import (
	"errors"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

func TestRoutingShareRealDataOnly(t *testing.T) {
	now := time.Now()
	evts := []events.Event{
		{RequestID: "r1", Kind: "route_attempt", Deployment: "dep-a", Time: now},
		{RequestID: "r1", Kind: "route_ok", Deployment: "dep-a", LatencyMS: 100, Time: now},
		{RequestID: "r2", Kind: "route_attempt", Deployment: "dep-b", Time: now},
		{RequestID: "r2", Kind: "route_ok", Deployment: "dep-b", LatencyMS: 200, Time: now},
		{RequestID: "r3", Kind: "route_attempt", Deployment: "dep-a", Time: now},
		{RequestID: "r3", Kind: "route_ok", Deployment: "dep-a", LatencyMS: 120, Time: now},
		{RequestID: "r3", Kind: "route_fail", Deployment: "dep-b", Time: now},
	}
	snap := usage.Snapshot{}
	rows := routingShare(evts, snap)
	if len(rows) != 2 {
		t.Fatalf("expected 2 share rows, got %d: %+v", len(rows), rows)
	}
	// dep-a has 2/3 successes
	if rows[0]["deployment"] != "dep-a" {
		t.Fatalf("expected dep-a first, got %+v", rows[0])
	}
	if rows[0]["successes"].(int64) != 2 {
		t.Fatalf("dep-a successes want 2, got %+v", rows[0]["successes"])
	}
	share := rows[0]["share"].(float64)
	if share < 0.66 || share > 0.67 {
		t.Fatalf("dep-a share want ~0.667, got %v", share)
	}
	// Empty input yields empty (no fabrication)
	if out := routingShare(nil, usage.Snapshot{}); len(out) != 0 {
		t.Fatalf("empty input must yield empty share, got %+v", out)
	}
}

func TestRecentJourneyCandidatesAttemptsFailuresFinal(t *testing.T) {
	now := time.Now()
	evts := []events.Event{
		{RequestID: "req-1", Kind: "route_attempt", Deployment: "dep-a", Message: "attempt=1", Time: now},
		{RequestID: "req-1", Kind: "route_fail", Deployment: "dep-a", ErrorType: "provider_timeout", Message: "timeout", Time: now},
		{RequestID: "req-1", Kind: "route_attempt", Deployment: "dep-b", Message: "attempt=2", Time: now},
		{RequestID: "req-1", Kind: "route_ok", Deployment: "dep-b", LatencyMS: 321, PublicModel: "nexa-code", Time: now},
	}
	j := recentJourney(evts)
	if j["request_id"] != "req-1" {
		t.Fatalf("request_id want req-1, got %+v", j["request_id"])
	}
	cands := j["candidates"].([]string)
	if len(cands) != 2 || cands[0] != "dep-a" || cands[1] != "dep-b" {
		t.Fatalf("candidates want [dep-a dep-b], got %+v", cands)
	}
	if len(j["attempts"].([]map[string]any)) != 2 {
		t.Fatalf("attempts want 2, got %+v", j["attempts"])
	}
	if len(j["failures"].([]map[string]any)) != 1 {
		t.Fatalf("failures want 1, got %+v", j["failures"])
	}
	if j["final_model"] != "dep-b" {
		t.Fatalf("final_model want dep-b, got %+v", j["final_model"])
	}
	if j["latency_ms"].(int64) != 321 {
		t.Fatalf("latency want 321, got %+v", j["latency_ms"])
	}
	// Empty yields explicit empty journey, no fabricated model
	empty := recentJourney(nil)
	if empty["request_id"] != "" || empty["final_model"] != "" {
		t.Fatalf("empty journey must have no request/final model, got %+v", empty)
	}
}

func TestEnrichToolValidationStructured(t *testing.T) {
	ve := &canonical.ToolCallValidationError{Tool: "Bash", Field: "command", ExpectedType: "string", ActualType: "object"}
	wrapped := errors.Join(errors.New("tool call validation failed"), ve)
	var ev events.Event
	enrichToolValidation(&ev, wrapped)
	if ev.ToolName != "Bash" || ev.ToolField != "command" || ev.ExpectedType != "string" || ev.ActualType != "object" {
		t.Fatalf("structured diagnostics not copied: %+v", ev)
	}
	// Non-validation errors leave fields empty (no fabrication)
	var ev2 events.Event
	enrichToolValidation(&ev2, errors.New("plain boom"))
	if ev2.ToolName != "" || ev2.ExpectedType != "" {
		t.Fatalf("plain errors must not gain tool fields: %+v", ev2)
	}
}

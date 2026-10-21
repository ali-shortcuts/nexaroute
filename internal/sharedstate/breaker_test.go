package sharedstate

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMergeKeepsLaterCooldown(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	first := map[string]Entry{
		"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(5 * time.Minute), OriginReplica: "a"},
	}
	second := map[string]Entry{
		"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(10 * time.Minute), OriginReplica: "b"},
	}
	merged := Merge(first, second, now, DefaultMaxCooldown)
	if got := merged["p/m"].CooldownUntil; !got.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("cooldown_until=%v want later deadline", got)
	}
	if merged["p/m"].OriginReplica != "b" {
		t.Fatalf("origin=%q want b", merged["p/m"].OriginReplica)
	}
}

func TestMergeKeepsRetiredSticky(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	merged := Merge(
		map[string]Entry{"p/m": {Status: StatusRetired, OriginReplica: "a"}},
		map[string]Entry{"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(time.Hour), OriginReplica: "b"}},
		now,
		DefaultMaxCooldown,
	)
	if merged["p/m"].Status != StatusRetired {
		t.Fatalf("status=%q want retired to dominate cooldown", merged["p/m"].Status)
	}
	merged = Merge(
		map[string]Entry{"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(time.Hour), OriginReplica: "a"}},
		map[string]Entry{"p/m": {Status: StatusRetired, OriginReplica: "b"}},
		now,
		DefaultMaxCooldown,
	)
	if merged["p/m"].Status != StatusRetired {
		t.Fatalf("status=%q want incoming retired to dominate cooldown", merged["p/m"].Status)
	}
	merged = Merge(
		map[string]Entry{"p/m": {Status: StatusRetired, OriginReplica: "a"}},
		map[string]Entry{"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(2 * time.Hour), OriginReplica: "b"}},
		now,
		DefaultMaxCooldown,
	)
	if merged["p/m"].Status != StatusRetired {
		t.Fatalf("a later cooldown must not un-retire: %+v", merged["p/m"])
	}
}

func TestMergePrunesExpiredEntries(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	merged := Merge(
		map[string]Entry{
			"expired":   {Status: StatusCooldown, CooldownUntil: now.Add(-time.Second)},
			"still-hot": {Status: StatusCooldown, CooldownUntil: now.Add(time.Minute)},
		},
		nil,
		now,
		DefaultMaxCooldown,
	)
	if _, ok := merged["expired"]; ok {
		t.Fatal("expired marker survived merge")
	}
	if _, ok := merged["still-hot"]; !ok {
		t.Fatal("active marker was pruned")
	}
}

func TestMergeCapsRemoteClock(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cap := 10 * time.Minute
	merged := Merge(nil, map[string]Entry{
		"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(48 * time.Hour)},
	}, now, cap)
	if got := merged["p/m"].CooldownUntil; !got.Equal(now.Add(cap)) {
		t.Fatalf("cooldown_until=%v want capped %v", got, now.Add(cap))
	}
}

func TestDecodeDocumentSkipsMalformedEntries(t *testing.T) {
	payload := []byte(`{
		"schema": 1,
		"entries": {
			"good": {"status":"cooldown","cooldown_until":"2026-10-21T12:10:00Z","unknown_field":"ignored"},
			"bad-time": {"status":"cooldown","cooldown_until":"yesterday"},
			"bad-status": {"status":"healthy","cooldown_until":"2026-10-21T12:10:00Z"},
			"bad-empty": {"status":"cooldown"},
			"retired": {"status":"retired"}
		},
		"future_field": true
	}`)
	entries, err := decodeDocument(payload, DefaultMaxEntries, DefaultMaxDocumentBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%+v want exactly good and retired", entries)
	}
	if _, ok := entries["good"]; !ok {
		t.Fatal("good entry skipped")
	}
	if entries["retired"].Status != StatusRetired {
		t.Fatalf("retired entry=%+v", entries["retired"])
	}
}

func TestDecodeDocumentRejectsUnknownSchemaAndLimits(t *testing.T) {
	for name, payload := range map[string][]byte{
		"unknown schema": []byte(`{"schema":2,"entries":{}}`),
		"missing schema": []byte(`{"entries":{}}`),
		"not json":       []byte(`not-json`),
	} {
		if _, err := decodeDocument(payload, DefaultMaxEntries, DefaultMaxDocumentBytes); err == nil {
			t.Fatalf("%s: decode succeeded, want error", name)
		}
	}
	if _, err := decodeDocument([]byte(`{"schema":1,"entries":{}}`), 0, DefaultMaxDocumentBytes); err != nil {
		t.Fatalf("zero max entries should be unbounded: %v", err)
	}
	if _, err := decodeDocument([]byte(`{"schema":1,"entries":{}}`), DefaultMaxEntries, 4); err == nil {
		t.Fatal("oversized document accepted")
	}
}

func TestEncodeDocumentRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	entries := map[string]Entry{
		"p/m": {Status: StatusCooldown, CooldownUntil: now.Add(time.Minute), ReasonClass: "rate_limit", OriginReplica: "r1", ObservedAt: now},
	}
	payload, err := encodeDocument(entries, DefaultMaxEntries, DefaultMaxDocumentBytes)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeDocument(payload, DefaultMaxEntries, DefaultMaxDocumentBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !entriesEqual(entries, decoded) {
		t.Fatalf("round trip mismatch: %+v vs %+v", entries, decoded)
	}
}

func TestNormalizeEntryRejectsInvalidLocalMarkers(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for name, entry := range map[string]Entry{
		"unknown status": {Status: "healthy"},
		"no deadline":    {Status: StatusCooldown},
		"zero now":       {Status: StatusRetired},
	} {
		at := now
		if name == "zero now" {
			at = time.Time{}
		}
		_, _, err := NormalizeEntry(entry, at, DefaultMaxCooldown)
		if !errors.Is(err, ErrInvalidEntry) {
			t.Fatalf("%s: err=%v want ErrInvalidEntry", name, err)
		}
	}
}

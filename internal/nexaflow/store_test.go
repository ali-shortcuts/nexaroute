package nexaflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPhase8IdempotentStart(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, simpleChainDef("wf-idem"))
	first, err := eng.StartRun(ctx, phase8Operator, "wf-idem", 0, "manual", "", "key-1", map[string]any{"value": map[string]any{}})
	if err != nil {
		t.Fatalf("first StartRun: %v", err)
	}
	second, err := eng.StartRun(ctx, phase8Operator, "wf-idem", 0, "manual", "", "key-1", map[string]any{"value": map[string]any{}})
	if err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent redispatch created a second run: %q vs %q", first.ID, second.ID)
	}
	runs, err := st.ListRuns(ctx, RunFilter{DefinitionID: "wf-idem"})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
}

func TestPhase8StoreRejectsHistoryShrink(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	eng := testEngine(st, newCountingBackend())
	publishDef(t, ctx, st, simpleChainDef("wf-append"))
	run := startManual(t, ctx, eng, "wf-append", map[string]any{"value": map[string]any{}})
	mustStatus(t, run, RunSucceeded)
	shrunk := *run
	shrunk.Steps = shrunk.Steps[:1]
	if err := st.UpdateRun(ctx, &shrunk); err == nil {
		t.Fatal("UpdateRun accepted a shrunk step history")
	}
	// Appending is fine (same length or longer, steps unchanged).
	if err := st.UpdateRun(ctx, run); err != nil {
		t.Fatalf("UpdateRun of an unchanged run failed: %v", err)
	}
}

func TestPhase8FileStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// The FileStore itself is the audit sink here, so audit events are
	// part of the durable record and must survive the reopen below.
	eng := NewEngine(nil, fs, newCountingBackend(), fs, EngineOptions{BackoffBase: time.Millisecond})
	publishDef(t, ctx, fs, simpleChainDef("wf-roundtrip"))
	run := startManual(t, ctx, eng, "wf-roundtrip", map[string]any{"value": map[string]any{"a": float64(1)}})
	mustStatus(t, run, RunSucceeded)

	// Reopen: definitions, runs, and audit events survive the restart.
	fs2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := fs2.Migrate(ctx); err != nil {
		t.Fatalf("reopen Migrate: %v", err)
	}
	got, err := fs2.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after reopen: %v", err)
	}
	if got.Status != RunSucceeded {
		t.Fatalf("reopened run status = %s", got.Status)
	}
	if _, _, err := fs2.GetVersion(ctx, "wf-roundtrip", 1); err != nil {
		t.Fatalf("GetVersion after reopen: %v", err)
	}
	events, err := fs2.ListAudit(ctx, 100)
	if err != nil {
		t.Fatalf("ListAudit after reopen: %v", err)
	}
	var started bool
	for _, ev := range events {
		if ev.RunID == run.ID && ev.Action == "run_started" {
			started = true
		}
	}
	if !started {
		t.Fatal("audit events did not survive the restart")
	}
}

func TestPhase8FileStoreMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	def := simpleChainDef("wf-migrate")
	publishDef(t, ctx, fs, def)
	eng := testEngine(fs, newCountingBackend())
	run := startManual(t, ctx, eng, "wf-migrate", map[string]any{"value": map[string]any{"a": float64(1)}})
	mustStatus(t, run, RunSucceeded)

	// Downgrade the on-disk records to format v1 by hand: drop the
	// version content hash, drop the run's optimistic-concurrency
	// version, and remove meta.json entirely.
	verPath := filepath.Join(dir, "versions", "wf-migrate", "1.json")
	raw, err := os.ReadFile(verPath)
	if err != nil {
		t.Fatalf("read version file: %v", err)
	}
	var wrapper map[string]any
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("decode version file: %v", err)
	}
	verRec := wrapper["version"].(map[string]any)
	delete(verRec, "content_hash")
	wrapper["version"] = verRec
	raw, _ = json.Marshal(wrapper)
	if err := os.WriteFile(verPath, raw, 0600); err != nil {
		t.Fatalf("write downgraded version: %v", err)
	}
	runPath := filepath.Join(dir, "runs", run.ID+".json")
	raw, err = os.ReadFile(runPath)
	if err != nil {
		t.Fatalf("read run file: %v", err)
	}
	var runRec map[string]any
	if err := json.Unmarshal(raw, &runRec); err != nil {
		t.Fatalf("decode run file: %v", err)
	}
	runRec["version"] = float64(0)
	raw, _ = json.Marshal(runRec)
	if err := os.WriteFile(runPath, raw, 0600); err != nil {
		t.Fatalf("write downgraded run: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatalf("remove meta: %v", err)
	}

	fs2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := fs2.Migrate(ctx); err != nil {
		t.Fatalf("Migrate of v1 records: %v", err)
	}
	if fs2.FormatVersion() != CurrentFormatVersion {
		t.Fatalf("format version = %d, want %d", fs2.FormatVersion(), CurrentFormatVersion)
	}
	_, ver, err := fs2.GetVersion(ctx, "wf-migrate", 1)
	if err != nil {
		t.Fatalf("GetVersion after migrate: %v", err)
	}
	wantHash, err := ContentHash(def)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if ver.ContentHash != wantHash {
		t.Fatalf("migrated hash = %q, want recomputed %q", ver.ContentHash, wantHash)
	}
	got, err := fs2.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after migrate: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("migrated run version = %d, want 1", got.Version)
	}
	if len(got.Steps) != len(run.Steps) {
		t.Fatalf("migrated steps = %d, want %d (history preserved)", len(got.Steps), len(run.Steps))
	}
	if got.Status != RunSucceeded {
		t.Fatalf("migrated run status = %s", got.Status)
	}
}

func TestPhase8ScheduleClaimAndDeliveryExclusivity(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	slot := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	claimed, err := st.ClaimSchedule(ctx, "wf:tick", slot)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	claimed, err = st.ClaimSchedule(ctx, "wf:tick", slot)
	if err != nil || claimed {
		t.Fatalf("second claim for the same slot = %v, %v", claimed, err)
	}
	claimed, err = st.ClaimSchedule(ctx, "wf:tick", slot.Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim for a different slot = %v, %v", claimed, err)
	}
	first, err := st.RecordDelivery(ctx, "abc123")
	if err != nil || !first {
		t.Fatalf("first delivery = %v, %v", first, err)
	}
	first, err = st.RecordDelivery(ctx, "abc123")
	if err != nil || first {
		t.Fatalf("replayed delivery = %v, %v", first, err)
	}
}

func TestPhase8PruneKeepsLiveRuns(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	now := time.Now()
	old := &Run{
		ID: "run-old", DefinitionID: "wf-prune", Revision: 1, TenantScope: "tenant-a",
		Trigger: "manual", Actor: "op-1", Status: RunSucceeded,
		CreatedAt: now.Add(-100 * 24 * time.Hour),
	}
	fin := now.Add(-100 * 24 * time.Hour)
	old.FinishedAt = &fin
	if err := st.CreateRun(ctx, old); err != nil {
		t.Fatalf("CreateRun old: %v", err)
	}
	live := &Run{
		ID: "run-live", DefinitionID: "wf-prune", Revision: 1, TenantScope: "tenant-a",
		Trigger: "manual", Actor: "op-1", Status: RunSucceeded, CreatedAt: now,
	}
	fin2 := now
	live.FinishedAt = &fin2
	if err := st.CreateRun(ctx, live); err != nil {
		t.Fatalf("CreateRun live: %v", err)
	}
	n, err := st.PruneRuns(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	if _, err := st.GetRun(ctx, "run-old"); err == nil {
		t.Fatal("old run survived pruning")
	}
	if _, err := st.GetRun(ctx, "run-live"); err != nil {
		t.Fatalf("live run was pruned: %v", err)
	}
}

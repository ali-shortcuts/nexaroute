package securitystore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Live-snapshot consistency: BackupTo on the in-process handle must
// produce a verifiable snapshot even while audit appends proceed
// concurrently. This is the property the gateway's admin endpoint
// relies on: a bbolt read transaction sees a point-in-time database,
// so concurrent writes can neither tear the snapshot nor break its
// audit chain.
func TestBackupToConcurrentAppendsStayVerifiable(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "security")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(dir, "security.db"), 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		if err := store.AppendAudit(ctx, AuditEvent{Timestamp: now, Actor: "op", Action: "seed", Outcome: "success", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = store.AppendAudit(ctx, AuditEvent{Timestamp: now, Actor: "writer", Action: "concurrent", Outcome: "success", Status: 200})
		}
	}()
	var snap bytes.Buffer
	if err := store.BackupTo(ctx, &snap); err != nil {
		t.Fatalf("BackupTo during concurrent appends: %v", err)
	}
	wg.Wait()
	if snap.Len() == 0 {
		t.Fatal("empty snapshot")
	}
	snapPath := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(snapPath, snap.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(snapPath)
	if err != nil {
		t.Fatalf("snapshot does not open read-only: %v", err)
	}
	defer func() { _ = ro.Close() }()
	report, err := ro.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("snapshot chain not verified: %+v", report)
	}
	records, err := ro.AuditRecords(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// Point-in-time: the snapshot holds at least the 5 seed records
	// and at most the 55 records that exist after all appends.
	if len(records) < 5 || len(records) > 55 {
		t.Fatalf("snapshot record count = %d, want between 5 and 55", len(records))
	}
}

// A second process (here: a second open in this test process, which
// bbolt treats the same way for flock purposes) cannot open a store
// whose writer lock is held, and that failure must be recognizable
// as a lock failure — not by matching error text — so the backup CLI
// can tell the operator to stop the gateway or use --live.
func TestIsLockedErrorRecognizesHeldWriterLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "security")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "security.db")
	writer, err := Open(path, 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	_, err = OpenReadOnly(path)
	if err == nil {
		t.Fatal("OpenReadOnly of a lock-held store must fail")
	}
	if !IsLockedError(err) {
		t.Fatalf("held-lock open error must be recognized as locked, got: %v", err)
	}
	// And a genuinely missing store is a different failure, not a lock.
	if _, err := OpenReadOnly(filepath.Join(dir, "absent.db")); err == nil || IsLockedError(err) {
		t.Fatalf("missing store error must not be a lock error, got: %v", err)
	}
}

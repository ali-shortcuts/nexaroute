package securitystore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupToProducesVerifiableSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "security")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := Open(filepath.Join(dir, "security.db"), 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, action := range []string{"auth.login", "config.update"} {
		if err := src.AppendAudit(ctx, AuditEvent{Timestamp: now, Actor: "op", Action: action, Outcome: "success", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := src.BackupTo(ctx, &buf); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty snapshot")
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(snapPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := OpenReadOnly(snapPath)
	if err != nil {
		t.Fatalf("snapshot does not open read-only: %v", err)
	}
	defer func() { _ = snap.Close() }()
	report, err := snap.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.ChainedRecords != 2 {
		t.Fatalf("snapshot chain: %+v, want 2 verified chained records", report)
	}
	records, err := snap.AuditRecords(ctx, 10)
	if err != nil || len(records) != 2 {
		t.Fatalf("snapshot records: %d %v", len(records), err)
	}
}

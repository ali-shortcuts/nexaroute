package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
	bolt "go.etcd.io/bbolt"
)

// M3-6 CLI tests for `nexaroute audit verify`: exit codes 0/1/2 and
// metadata-only output against a real security store.

func setupAuditCLI(t *testing.T, events int) string {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"admin":{"security_store_path":"security/nexaroute-security.db"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(dir, "security", "nexaroute-security.db")
	store, err := securitystore.Open(storePath, 100, 3650)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for i := 0; i < events; i++ {
		if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Actor:     "alice", Action: "auth.login", Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestAuditVerifyCLIExitOKAndMetadataOnlyOutput(t *testing.T) {
	configPath := setupAuditCLI(t, 3)
	var stdout, stderr bytes.Buffer
	code := runAudit([]string{"verify", "--config", configPath}, &stdout, &stderr)
	if code != auditExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"3 records (3 chained, 0 legacy pre-chain)", "head: seq 3 hash ", "result: VERIFIED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "alice") || strings.Contains(out, "auth.login") {
		t.Fatalf("output must be metadata only, got event payload: %s", out)
	}
}

func TestAuditVerifyCLIDetectsTampering(t *testing.T) {
	configPath := setupAuditCLI(t, 3)
	storePath := filepath.Join(filepath.Dir(configPath), "security", "nexaroute-security.db")
	db, err := bolt.Open(storePath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("audit-v1"))
		c := bucket.Cursor()
		k, raw := c.First()
		return bucket.Put(k, bytes.Replace(raw, []byte("alice"), []byte("carol"), 1))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runAudit([]string{"verify", "--config", configPath}, &stdout, &stderr)
	if code != auditExitProblems {
		t.Fatalf("exit=%d want 1, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result: FAILED") || !strings.Contains(stdout.String(), "problem: ") {
		t.Fatalf("expected failure diagnostics: %s", stdout.String())
	}
}

func TestAuditVerifyCLIUsageAndMissingStore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAudit([]string{}, &stdout, &stderr); code != auditExitUsage {
		t.Fatalf("no-args exit=%d want 2", code)
	}
	if code := runAudit([]string{"verify", "--config", filepath.Join(t.TempDir(), "nope.json")}, &stdout, &stderr); code != auditExitUsage {
		t.Fatalf("missing config exit=%d want 2", code)
	}
	// Valid config but no store file yet: operational error (2).
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAudit([]string{"verify", "--config", configPath}, &stdout, &stderr); code != auditExitUsage {
		t.Fatalf("missing store exit=%d want 2 (stderr=%s)", code, stderr.String())
	}
	// JSON format on the happy path.
	configPath = setupAuditCLI(t, 1)
	stdout.Reset()
	stderr.Reset()
	if code := runAudit([]string{"verify", "--config", configPath, "--format", "json"}, &stdout, &stderr); code != auditExitOK {
		t.Fatalf("json exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"Verified": true`) {
		t.Fatalf("json output: %s", stdout.String())
	}
}

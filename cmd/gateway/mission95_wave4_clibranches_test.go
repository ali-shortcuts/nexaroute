package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

type wave4FailWriter struct{ err error }

func (w wave4FailWriter) Write(_ []byte) (int, error) { return 0, w.err }

// A flag-parse failure in `audit verify` is a usage error, not a crash.
func TestWave4AuditVerifyFlagParseError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAudit([]string{"verify", "--bogus"}, &stdout, &stderr); code != auditExitUsage {
		t.Fatalf("exit=%d, want %d", code, auditExitUsage)
	}
	if stderr.Len() == 0 {
		t.Fatal("flag error must be reported on stderr")
	}
}

// An unknown output format is rejected before any config is loaded.
func TestWave4AuditVerifyUnknownFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAudit([]string{"verify", "--format", "yaml"}, &stdout, &stderr); code != auditExitUsage {
		t.Fatalf("exit=%d, want %d", code, auditExitUsage)
	}
	if !strings.Contains(stderr.String(), `unknown format "yaml"`) {
		t.Fatalf("stderr = %q, want the unknown-format diagnostic", stderr.String())
	}
}

// The store-path resolver fails closed when neither a configured path
// nor a config file path is available to resolve against.
func TestWave4AuditStorePathRequiresABase(t *testing.T) {
	if _, err := auditStorePath("", ""); err == nil || !strings.Contains(err.Error(), "no config file path is available") {
		t.Fatalf("err = %v, want the missing-base error", err)
	}
}

// A pruned chain carries an anchor, and the text report prints it: 101
// events against a 100-event bound prune the oldest record and leave
// the anchor at seq 1 while the chain still verifies.
func TestWave4AuditVerifyPrintsPruningAnchor(t *testing.T) {
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
	for i := 0; i < 101; i++ {
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
	var stdout, stderr bytes.Buffer
	if code := runAudit([]string{"verify", "--config", configPath}, &stdout, &stderr); code != auditExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pruning anchor: seq 1") {
		t.Fatalf("output missing the anchor line: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "result: VERIFIED") {
		t.Fatalf("pruned chain must still verify: %s", stdout.String())
	}
}

// A report path below a regular file cannot create its directory.
func TestWave4ProviderAcceptReportMkdirFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeProviderAcceptReport(filepath.Join(blocker, "sub", "report.json"), providerAcceptReport{}); err == nil {
		t.Fatal("expected a directory-creation error")
	}
}

// A journal manifest cannot be created in a missing directory.
func TestWave4JournalManifestMissingDir(t *testing.T) {
	m := &restoreJournalManifest{Format: "v1", ID: "j1", State: "prepared"}
	if err := writeJournalManifest(filepath.Join(t.TempDir(), "missing"), m); err == nil {
		t.Fatal("expected a temp-file creation error for a missing journal directory")
	}
}

// Privacy audit flag-parse failure is a usage error.
func TestWave4PrivacyFlagParseError(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runPrivacy([]string{"audit", "--bogus"}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("exit=%d, want %d", code, privacyExitUsage)
	}
}

// An explicitly empty config path is rejected before any file access.
func TestWave4PrivacyEmptyConfigPath(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runPrivacy([]string{"audit", "--config", ""}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("exit=%d, want %d", code, privacyExitUsage)
	}
	if !strings.Contains(errBuf.String(), "config path required") {
		t.Fatalf("stderr = %q, want the config-path-required diagnostic", errBuf.String())
	}
}

// A directory as the config path opens but fails to read.
func TestWave4PrivacyDirectoryConfigReadError(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runPrivacy([]string{"audit", "--config", t.TempDir()}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("exit=%d, want %d", code, privacyExitUsage)
	}
	if !strings.Contains(errBuf.String(), "cannot read config") {
		t.Fatalf("stderr = %q, want the cannot-read-config diagnostic", errBuf.String())
	}
}

// A config beyond the 16 MiB audit bound is rejected by size, not parsed.
func TestWave4PrivacyOversizedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), (16<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runPrivacy([]string{"audit", "--config", path}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("exit=%d, want %d", code, privacyExitUsage)
	}
	if !strings.Contains(errBuf.String(), "exceeds safe size limit") {
		t.Fatalf("stderr = %q, want the size-limit diagnostic", errBuf.String())
	}
}

// Valid JSON with a wrong-typed field parses as an object but fails the
// config unmarshal: a malformed-config error, not a defaults audit.
func TestWave4PrivacyWrongTypedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"listen": 123}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runPrivacy([]string{"audit", "--config", path}, &out, &errBuf); code != privacyExitUsage {
		t.Fatalf("exit=%d, want %d", code, privacyExitUsage)
	}
	if !strings.Contains(errBuf.String(), "malformed config") {
		t.Fatalf("stderr = %q, want the malformed-config diagnostic", errBuf.String())
	}
}

// A failing stdout makes JSON encoding fail instead of silently
// dropping the report.
func TestWave4PrivacyEncodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	if code := runPrivacy([]string{"audit", "--config", path, "--format", "json"}, wave4FailWriter{errors.New("wave4 stdout failure")}, &errBuf); code != privacyExitUsage {
		t.Fatalf("exit=%d, want %d", code, privacyExitUsage)
	}
	if !strings.Contains(errBuf.String(), "cannot encode report") {
		t.Fatalf("stderr = %q, want the cannot-encode-report diagnostic", errBuf.String())
	}
}

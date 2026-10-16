package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
	bolt "go.etcd.io/bbolt"
)

// Issue #428 (audit F4): backup creation and restore must verify the
// audit chain. Before the fix, cmd/gateway created and installed backups
// with no chain verification at all — a store whose records were rewritten
// below the adapter (actor "mallory") backed up and restored silently.

func tamperAuditStoreFile(t *testing.T, path string) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("audit-v1"))
		if b == nil {
			t.Fatal("audit bucket missing")
		}
		k, v := b.Cursor().First()
		if k == nil {
			t.Fatal("audit bucket empty")
		}
		var event securitystore.AuditEvent
		if err := json.Unmarshal(v, &event); err != nil {
			return err
		}
		event.Actor = "mallory"
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return b.Put(k, raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Sanity: the tamper must actually break verification. OpenReadOnly
	// enforces 0700 on the store directory; test temp dirs default to
	// 0755, so tighten the (test-owned) directory first.
	_ = os.Chmod(filepath.Dir(path), 0o700)
	store, err := securitystore.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.VerifyAuditChain(context.Background())
	_ = store.Close()
	if err != nil || report.Verified {
		t.Fatalf("tampered store must fail verification: err=%v report=%+v", err, report)
	}
}

func fixtureSecurityPath(dir string) string {
	return filepath.Join(dir, "security", "nexaroute-security.db")
}

func TestBackupRefusesTamperedAuditChain(t *testing.T) {
	dir, configPath := opsFixture(t)
	tamperAuditStoreFile(t, fixtureSecurityPath(dir))
	out := filepath.Join(t.TempDir(), "b.tar")
	code, stderr := runBackupCode(t, configPath, out)
	if code != 1 || !strings.Contains(stderr, "audit chain") {
		t.Fatalf("backup of tampered store: exit=%d stderr=%s, want 1 with an audit chain refusal", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("no bundle may be written for a tampered audit store")
	}
}

func TestBackupOverrideRecordsBrokenChain(t *testing.T) {
	dir, configPath := opsFixture(t)
	tamperAuditStoreFile(t, fixtureSecurityPath(dir))
	out := filepath.Join(t.TempDir(), "b.tar")
	code, stderr := runBackupCode(t, configPath, out, "--allow-broken-audit-chain")
	if code != 0 {
		t.Fatalf("override backup exit=%d stderr=%s, want 0", code, stderr)
	}
	// The override must be recorded inside the bundle manifest.
	fh, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	tr := tar.NewReader(fh)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatal("manifest.json not found in bundle")
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != backupEntryManifest {
			continue
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		var manifest backupManifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(manifest.Notes, "audit chain FAILED verification") || !strings.Contains(manifest.Notes, "--allow-broken-audit-chain") {
			t.Fatalf("manifest notes must record the broken chain and the override, got %q", manifest.Notes)
		}
		return
	}
}

func runRestoreCode(t *testing.T, bundle, configPath string, extra ...string) (int, string) {
	t.Helper()
	args := []string{"--bundle", bundle, "--config", configPath}
	args = append(args, extra...)
	var stdout, stderr bytes.Buffer
	code := runRestore(args, &stdout, &stderr)
	return code, stderr.String()
}

func tamperedBundle(t *testing.T, dir, configPath string) string {
	t.Helper()
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	storeBytes, err := os.ReadFile(fixtureSecurityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(t.TempDir(), "security.db")
	if err := os.WriteFile(tamperedPath, storeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperAuditStoreFile(t, tamperedPath)
	tamperedBytes, err := os.ReadFile(tamperedPath)
	if err != nil {
		t.Fatal(err)
	}
	return writeTestBundle(t, map[string][]byte{
		backupEntryConfig:   configBytes,
		backupEntrySecurity: tamperedBytes,
	})
}

func TestRestoreRefusesTamperedAuditChain(t *testing.T) {
	dir, configPath := opsFixture(t)
	bundle := tamperedBundle(t, dir, configPath)
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	code, stderr := runRestoreCode(t, bundle, targetConfig)
	if code != 1 || !strings.Contains(stderr, "audit chain") {
		t.Fatalf("restore of tampered bundle: exit=%d stderr=%s, want 1 with an audit chain refusal", code, stderr)
	}
	if _, err := os.Stat(fixtureSecurityPath(targetDir)); !os.IsNotExist(err) {
		t.Fatal("tampered security store must not be installed")
	}
}

func TestRestoreOverrideTamperedAuditChain(t *testing.T) {
	dir, configPath := opsFixture(t)
	bundle := tamperedBundle(t, dir, configPath)
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	code, stderr := runRestoreCode(t, bundle, targetConfig, "--allow-broken-audit-chain")
	if code != 0 {
		t.Fatalf("override restore exit=%d stderr=%s, want 0", code, stderr)
	}
	installed := fixtureSecurityPath(targetDir)
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("security store should be installed under override: %v", err)
	}
	sidecar, err := os.ReadFile(installed + ".restore-override.json")
	if err != nil {
		t.Fatalf("override must leave a durable record next to the store: %v", err)
	}
	if !strings.Contains(string(sidecar), "allow-broken-audit-chain") || !strings.Contains(string(sidecar), "hash mismatch") && !strings.Contains(string(sidecar), "mismatch") {
		t.Fatalf("sidecar must record the override and the chain problems, got %s", sidecar)
	}
}

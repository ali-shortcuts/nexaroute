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
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// opsFixture builds a healthy installation in one directory: a valid
// config plus a security store holding two chained audit events.
func opsFixture(t *testing.T) (dir, configPath string) {
	t.Helper()
	dir = t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(config.Default(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	securityDir := filepath.Join(dir, "security")
	if err := os.MkdirAll(securityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := securitystore.Open(filepath.Join(securityDir, "nexaroute-security.db"), 1000, 365)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, action := range []string{"auth.login", "config.update"} {
		if err := store.AppendAudit(context.Background(), securitystore.AuditEvent{
			Timestamp: now, Actor: "alice", Action: action, Outcome: "success", Status: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, configPath
}

func tarEntryNames(t *testing.T, bundle string) []string {
	t.Helper()
	fh, err := os.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	var names []string
	tr := tar.NewReader(fh)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func TestDoctorHealthyAndBrokenFixtures(t *testing.T) {
	_, configPath := opsFixture(t)
	var stdout, stderr bytes.Buffer
	if code := runDoctor([]string{"--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("healthy doctor exit=%d stderr=%s\n%s", code, stderr.String(), stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{"PASS config:", "PASS security_store:", "PASS control_plane:", "0 fail"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}

	broken := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := runDoctor([]string{"--config", broken}, &stdout, &stderr); code != 1 {
		t.Fatalf("broken doctor exit=%d, want 1\n%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "FAIL config:") {
		t.Fatalf("broken doctor must FAIL the config check:\n%s", stdout.String())
	}

	stdout.Reset()
	if code := runDoctor([]string{"--config", filepath.Join(t.TempDir(), "absent.json")}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing-config doctor exit=%d, want 1", code)
	}
	if code := runDoctor([]string{"--bogus-flag"}, &stdout, &stderr); code != 2 {
		t.Fatalf("usage exit=%d, want 2", code)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	dir, configPath := opsFixture(t)
	bundle := filepath.Join(dir, "backup.tar")
	var stdout, stderr bytes.Buffer
	if code := runBackup([]string{"--config", configPath, "--out", bundle}, &stdout, &stderr); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr.String())
	}
	if info, err := os.Stat(bundle); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}
	names := tarEntryNames(t, bundle)
	for _, want := range []string{"manifest.json", "config.json", "security.db"} {
		if !contains(names, want) {
			t.Fatalf("bundle entries %v missing %q", names, want)
		}
	}
	if contains(names, "master.key") {
		t.Fatal("master key must not be included by default")
	}

	// Restore into a fresh directory and compare byte-for-byte.
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	stdout.Reset()
	if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 0 {
		t.Fatalf("restore exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	original, _ := os.ReadFile(configPath)
	restored, _ := os.ReadFile(targetConfig)
	if !bytes.Equal(original, restored) {
		t.Fatal("restored config differs from the original")
	}
	restoredDB := filepath.Join(targetDir, "security", "nexaroute-security.db")
	store, err := securitystore.OpenReadOnly(restoredDB)
	if err != nil {
		t.Fatalf("restored security store does not open: %v", err)
	}
	report, err := store.VerifyAuditChain(context.Background())
	_ = store.Close()
	if err != nil || !report.Verified || report.ChainedRecords != 2 {
		t.Fatalf("restored chain: %+v err=%v", report, err)
	}
}

func TestRestoreRejectsTamperedBundleAndRefusesOverwrite(t *testing.T) {
	_, configPath := opsFixture(t)
	bundle := filepath.Join(t.TempDir(), "backup.tar")
	var stdout, stderr bytes.Buffer
	if code := runBackup([]string{"--config", configPath, "--out", bundle}, &stdout, &stderr); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr.String())
	}
	// Tamper: flip bytes inside the stored config payload (the tar is
	// uncompressed, so the listen address is directly visible).
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(raw, []byte("127.0.0.1:8080"))
	if idx < 0 {
		t.Fatal("fixture config listen address not found in bundle")
	}
	raw[idx] = '9'
	tampered := filepath.Join(t.TempDir(), "tampered.tar")
	if err := os.WriteFile(tampered, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	targetConfig := filepath.Join(targetDir, "config.json")
	if code := runRestore([]string{"--bundle", tampered, "--config", targetConfig}, &stdout, &stderr); code != 1 {
		t.Fatalf("tampered restore exit=%d, want 1 (stderr=%s)", code, stderr.String())
	}
	if _, err := os.Stat(targetConfig); !os.IsNotExist(err) {
		t.Fatal("tampered restore must not write anything")
	}

	// Overwrite guard: restore the good bundle, change the target, and
	// restore again — refused without --force, allowed with it.
	if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 0 {
		t.Fatalf("first restore exit=%d stderr=%s", code, stderr.String())
	}
	if err := os.WriteFile(targetConfig, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 1 {
		t.Fatalf("overwrite without --force exit=%d, want 1", code)
	}
	if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig, "--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("overwrite with --force exit=%d stderr=%s", code, stderr.String())
	}
}

func TestBackupMasterKeyOptIn(t *testing.T) {
	_, configPath := opsFixture(t)
	keyPath := configPath + ".key"
	if err := os.WriteFile(keyPath, []byte("test-key-material-not-a-real-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "with-key.tar")
	var stdout, stderr bytes.Buffer
	if code := runBackup([]string{"--config", configPath, "--out", bundle, "--include-master-key"}, &stdout, &stderr); code != 0 {
		t.Fatalf("backup exit=%d stderr=%s", code, stderr.String())
	}
	if !contains(tarEntryNames(t, bundle), "master.key") {
		t.Fatal("bundle must contain master.key when opted in")
	}
	if !strings.Contains(stdout.String(), "WARNING") {
		t.Fatal("key inclusion must print a warning")
	}
	// Restore to a fresh machine installs the key as well.
	targetConfig := filepath.Join(t.TempDir(), "config.json")
	if code := runRestore([]string{"--bundle", bundle, "--config", targetConfig}, &stdout, &stderr); code != 0 {
		t.Fatalf("restore exit=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(targetConfig + ".key"); err != nil {
		t.Fatalf("restored key missing: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

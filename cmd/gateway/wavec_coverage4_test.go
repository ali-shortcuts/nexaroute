package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Wave C-6 (part 4): backup→restore round trip through the CLI, plus
// the collision/malformed-bundle refusal branches.

func backupConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "nexaroute.json")
	cfg := configDefaultForBackup(t)
	if err := os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func configDefaultForBackup(t *testing.T) []byte {
	t.Helper()
	// A minimal config that loads and validates.
	return []byte(`{"listen":"127.0.0.1:18899","decision":{"mode":"local","provider":"policy","policy":"baseline-first"},"decision_policies":[{"id":"baseline-first","weights":{"router_baseline":1}}]}`)
}

// Full round trip: backup a real config, restore it to a fresh path,
// and byte-compare the restored config. Also asserts restore refuses
// without --bundle and a corrupt bundle fails loudly.
func TestBackupRestoreCLIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := backupConfig(t, dir)
	bundle := filepath.Join(dir, "backup.tar")
	var out, errBuf bytes.Buffer
	if code := runBackup([]string{"--config", src, "--out", bundle}, &out, &errBuf); code != 0 {
		t.Fatalf("backup: exit=%d (%s)", code, errBuf.String())
	}
	if info, err := os.Stat(bundle); err != nil || info.Size() == 0 {
		t.Fatalf("bundle: %v %v", info, err)
	}
	restoreDir := filepath.Join(dir, "restored")
	if err := os.MkdirAll(restoreDir, 0700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(restoreDir, "nexaroute.json")
	out.Reset()
	errBuf.Reset()
	if code := runRestore([]string{"--bundle", bundle, "--config", dst}, &out, &errBuf); code != 0 {
		t.Fatalf("restore: exit=%d (%s)", code, errBuf.String())
	}
	restored, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("restored config differs:\n%s\nvs\n%s", restored, original)
	}
	// Corrupt bundle: restore must fail, never partially install.
	corrupt := filepath.Join(dir, "corrupt.tar")
	if err := os.WriteFile(corrupt, []byte("not a tar"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runRestore([]string{"--bundle", corrupt, "--config", filepath.Join(restoreDir, "x.json")}, &out, &errBuf); code != 1 {
		t.Fatalf("corrupt bundle: exit=%d, want 1", code)
	}
	if code := runRestore([]string{"--config", dst}, &out, &errBuf); code != 2 {
		t.Fatalf("missing bundle: exit=%d, want 2", code)
	}
}

// The output bundle must never alias a backup source: backing up
// onto the config path itself is refused instead of destroying the
// data being backed up.
func TestBackupRefusesOutputCollision(t *testing.T) {
	dir := t.TempDir()
	src := backupConfig(t, dir)
	var out, errBuf bytes.Buffer
	if code := runBackup([]string{"--config", src, "--out", src}, &out, &errBuf); code != 1 {
		t.Fatalf("self-backup: exit=%d, want 1 (%s)", code, errBuf.String())
	}
	if got := errBuf.String(); !strings.Contains(got, "backup failed") {
		t.Fatalf("stderr: %q", got)
	}
	if code := runBackup([]string{"--config", filepath.Join(dir, "missing.json"), "--out", filepath.Join(dir, "b.tar")}, &out, &errBuf); code != 1 {
		t.Fatalf("missing config: exit=%d, want 1", code)
	}
	if code := runBackup([]string{"--config", src}, &out, &errBuf); code != 2 {
		t.Fatalf("missing --out: exit=%d, want 2", code)
	}
}

// writeSavedCopyDurable error path: the target path under a regular
// file cannot be created.
func TestWriteSavedCopyDurableError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeSavedCopyDurable(filepath.Join(blocker, "x.json"), []byte("x")); err == nil {
		t.Fatal("bad target accepted")
	}
}

// writeFileAtomic error path: the target dir path component is a
// regular file, so the temp file cannot be created.
func TestWriteFileAtomicError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(blocker, "x.json"), []byte("x"), 0600); err == nil {
		t.Fatal("bad target accepted")
	}
}

// writeRotateFile error path mirrors writeFileAtomic's: no temp file
// under a file-blocked directory.
func TestWriteRotateFileError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeRotateFile(filepath.Join(blocker, "k"), []byte("x")); err == nil {
		t.Fatal("bad target accepted")
	}
}

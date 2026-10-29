package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWave2RestoreVerifyMissingBundleArg(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runRestoreVerify("", "", "", "", 0, &out, &errBuf); code != 2 || !strings.Contains(errBuf.String(), "--bundle is required") {
		t.Fatalf("code=%d stderr=%q want 2 with --bundle is required", code, errBuf.String())
	}
}

func TestWave2RestoreVerifyMissingBundleFile(t *testing.T) {
	var out, errBuf bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nope.bin")
	if code := runRestoreVerify(missing, "", "", "", 0, &out, &errBuf); code != 1 || !strings.Contains(out.String(), "FAIL bundle_readable") {
		t.Fatalf("code=%d stdout=%q want 1 with FAIL bundle_readable", code, out.String())
	}
}

func TestWave2RestoreVerifyCorruptBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.bin")
	if err := os.WriteFile(path, []byte("not a nexaroute bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runRestoreVerify(path, "", "", "", 0, &out, &errBuf); code != 1 || !strings.Contains(out.String(), "FAIL bundle_integrity") {
		t.Fatalf("code=%d stdout=%q want 1 with FAIL bundle_integrity", code, out.String())
	}
}

func TestWave2RestoreVerifyIntoFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	bundle := filepath.Join(dir, "b.bin")
	if code := runRestoreVerify(bundle, "", file, "", 0, &out, &errBuf); code != 2 || !strings.Contains(errBuf.String(), "not a real directory") {
		t.Fatalf("code=%d stderr=%q want 2 with not a real directory", code, errBuf.String())
	}
}

func TestWave2CreateBackupMissingConfig(t *testing.T) {
	var out bytes.Buffer
	missing := filepath.Join(t.TempDir(), "no-config.json")
	if err := createBackup(missing, filepath.Join(t.TempDir(), "out.bin"), false, false, &out); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("err=%v want a read config failure", err)
	}
}

func TestWave2ReadFileBoundedOversize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file reports the size without allocating the bytes.
	if err := fh.Truncate(maxBackupEntryBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileBounded(path, "config"); err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("err=%v want the bundle entry limit error", err)
	}
}

func TestWave2ReadFileBoundedSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Stat succeeds on a socket but ReadFile cannot open it.
	if _, err := readFileBounded(path, "config"); err == nil {
		t.Fatal("socket read accepted")
	}
}

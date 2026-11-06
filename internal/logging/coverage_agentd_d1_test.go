package logging

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Agent-D1 coverage: deterministic Remove/Rename failures inside
// cleanupBackups and rotateLocked, plus truncation and startup paths.
// The file-operation hooks are scoped to each writer; tests need no
// mount namespace, elevated capability, or host-specific filesystem setup.

func TestAgentD1StartupCleanupRemoveError(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "app.log.9")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected startup cleanup remove failure")
	remove := func(path string) error {
		if path == stale {
			return injected
		}
		return os.Remove(path)
	}

	w, err := newRotatingWriterWithFileOps(filepath.Join(dir, "app.log"), 2048, 2, remove, nil)
	if w != nil {
		_ = w.Close()
		t.Fatal("constructor returned a writer after startup cleanup failed")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("startup cleanup error = %v, want injected error", err)
	}
}

func TestAgentD1RotateLockedZeroBackupRemoveError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected zero-backup remove failure")
	remove := func(name string) error {
		if name == path {
			return injected
		}
		return os.Remove(name)
	}

	w, err := newRotatingWriterWithFileOps(path, 1024, 0, remove, nil)
	if w != nil {
		_ = w.Close()
		t.Fatal("constructor returned a writer after startup rotation failed")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("zero-backup rotation error = %v, want injected error", err)
	}
}

func TestAgentD1RotateLockedShiftRenameError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	for name, value := range map[string]string{
		path:        "current",
		path + ".1": "old-1",
		path + ".2": "old-2",
	} {
		if err := os.WriteFile(name, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	injected := errors.New("injected backup shift rename failure")
	w := &RotatingWriter{
		path: path, maxBytes: 1024, backups: 3, file: f, size: int64(len("current")),
		renameFile: func(oldPath, newPath string) error {
			if oldPath == path+".2" && newPath == path+".3" {
				return injected
			}
			return os.Rename(oldPath, newPath)
		},
	}
	if err := w.rotateLocked(); !errors.Is(err, injected) {
		t.Fatalf("shift rename error = %v, want injected error", err)
	}
}

func TestAgentD1RotateLockedFinalRenameError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	injected := errors.New("injected current-log rename failure")
	w := &RotatingWriter{
		path: path, maxBytes: 1024, backups: 1, file: f, size: int64(len("current")),
		renameFile: func(oldPath, newPath string) error {
			if oldPath == path && newPath == path+".1" {
				return injected
			}
			return os.Rename(oldPath, newPath)
		},
	}
	if err := w.rotateLocked(); !errors.Is(err, injected) {
		t.Fatalf("final rename error = %v, want injected error", err)
	}
}

func TestAgentD1RotateLockedCleanupBackupsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := NewRotatingWriter(path, 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("seed")); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(dir, "app.log.9")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected rotation cleanup failure")
	w.removeFile = func(name string) error {
		if name == stale {
			return injected
		}
		return os.Remove(name)
	}

	if _, err := w.Write(bytes.Repeat([]byte("y"), 2000)); !errors.Is(err, injected) {
		t.Fatalf("write with failing trailing cleanup = %v, want injected error", err)
	}

	// The writer is left with a nil file after the failed rotation;
	// restoring the filesystem operation must allow a later write.
	w.removeFile = nil
	if n, err := w.Write([]byte("after")); err != nil || n != len("after") {
		t.Errorf("Write after failed rotation = %d, %v", n, err)
	}
}

func TestAgentD1WriteTruncationKeepZero(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(filepath.Join(dir, "app.log"), 2048, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// The marker alone is longer than this (artificially small)
	// limit, so nothing of the entry survives: keep clamps to zero.
	w.maxBytes = 10
	payload := bytes.Repeat([]byte("x"), 100)
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("oversized Write with tiny limit = %d, %v", n, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("\n[log entry truncated to rotation limit]\n")
	if !bytes.Equal(data, want) {
		t.Fatalf("file = %q, want exactly the truncation marker", data)
	}
}

func TestAgentD1OpenChmodFailure(t *testing.T) {
	// /proc/self/loginuid opens for write but the kernel rejects
	// mode changes on it (EPERM), so open()'s Chmod step fails.
	// Skip where /proc is unavailable or the file cannot be opened
	// (for example non-Linux runners or non-root users).
	probe, err := os.OpenFile("/proc/self/loginuid", os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Skipf("cannot open /proc/self/loginuid for write, chmod branch not exercisable here: %v", err)
	}
	_ = probe.Close()
	_, err = NewRotatingWriter("/proc/self/loginuid", 2048, 0)
	if err == nil {
		t.Fatal("open on an un-chmoddable file should fail")
	}
	if !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("error = %v, want a chmod failure", err)
	}
}

func TestAgentD1WriteToDevFullFails(t *testing.T) {
	// A plain write error (not a short write) returns the error.
	devFull, err := os.OpenFile("/dev/full", os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer devFull.Close()
	w := &RotatingWriter{path: filepath.Join(t.TempDir(), "app.log"), maxBytes: 4096, file: devFull, size: 0}
	if _, err := w.Write([]byte("hello")); err == nil {
		t.Error("Write to /dev/full should fail")
	} else if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Write to /dev/full err = %v, want ENOSPC", err)
	}
}

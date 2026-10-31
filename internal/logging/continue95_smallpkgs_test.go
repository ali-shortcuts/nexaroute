package logging

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type Continue95ErrWriter struct{ err error }

func (w Continue95ErrWriter) Write(p []byte) (int, error) { return 0, w.err }

type Continue95ShortWriter struct{}

func (Continue95ShortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 1, nil
}

func TestContinue95RotatingWriterConstructorErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewRotatingWriter("  ", 2048, 1); err == nil {
		t.Error("blank path should fail")
	}
	if _, err := NewRotatingWriter(filepath.Join(dir, "a.log"), 1023, 1); err == nil {
		t.Error("maxBytes below 1024 should fail")
	}
	if _, err := NewRotatingWriter(filepath.Join(dir, "a.log"), 2048, -1); err == nil {
		t.Error("negative backups should fail")
	}
	// MkdirAll error: the log path lives under a regular file.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRotatingWriter(filepath.Join(blocker, "a.log"), 2048, 1); err == nil {
		t.Error("path under a regular file should fail at MkdirAll")
	}
	// Open error: the log path itself is a directory.
	logDir := filepath.Join(dir, "logdir")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRotatingWriter(logDir, 2048, 1); err == nil {
		t.Error("directory log path should fail at open")
	}
	// Cleanup error at startup: a stale backup beyond retention in a
	// directory that no longer permits removal.
	// Permission-failure branch requires a non-root user: root bypasses
	// directory permissions, so this section is skipped under root.
	// CI runners are non-root and still exercise it.
	if os.Geteuid() != 0 {
		roDir := filepath.Join(dir, "ro")
		if err := os.MkdirAll(roDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roDir, "app.log.9"), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(roDir, 0o555); err != nil {
			t.Fatal(err)
		}
		_, roErr := NewRotatingWriter(filepath.Join(roDir, "app.log"), 2048, 2)
		if err := os.Chmod(roDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if roErr == nil {
			t.Error("startup cleanup in a read-only directory should fail")
		}
	}
	// Direct method error paths: cleanup ReadDir failure and open failure.
	w := &RotatingWriter{path: filepath.Join(dir, "missing-dir-xyz", "app.log"), backups: 2}
	if err := w.cleanupBackups(); err == nil {
		t.Error("cleanupBackups on a missing directory should fail")
	}
	w2 := &RotatingWriter{path: logDir}
	if err := w2.open(); err == nil {
		t.Error("open on a directory path should fail")
	}
	// Cleanup skips directories, non-numeric suffixes and .0 entries.
	skipDir := filepath.Join(dir, "skip")
	if err := os.MkdirAll(filepath.Join(skipDir, "app.log.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.log.abc", "app.log.0", "other.log.9"} {
		if err := os.WriteFile(filepath.Join(skipDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w3 := &RotatingWriter{path: filepath.Join(skipDir, "app.log"), backups: 2}
	if err := w3.cleanupBackups(); err != nil {
		t.Errorf("cleanupBackups with skippable entries: %v", err)
	}
	for _, name := range []string{"app.log.abc", "app.log.0"} {
		if _, err := os.Stat(filepath.Join(skipDir, name)); err != nil {
			t.Errorf("skippable backup %s should survive cleanup: %v", name, err)
		}
	}
}

func TestContinue95RotatingWriterWriteBranches(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(filepath.Join(dir, "app.log"), 2048, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Empty write is a no-op success.
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Errorf("empty Write = %d, %v", n, err)
	}
	// Oversized write is truncated with the marker but reports the
	// caller's original length.
	big := bytes.Repeat([]byte("x"), 3000)
	n, err := w.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("oversized Write = %d, %v", n, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("[log entry truncated to rotation limit]")) {
		t.Error("oversized write should carry the truncation marker")
	}
	// Write after Close reopens the file (file == nil path).
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Error("second Close should be a nil success")
	}
	if n, err := w.Write([]byte("after-close")); err != nil || n != len("after-close") {
		t.Errorf("Write after Close = %d, %v", n, err)
	}
	// Write to a directory path after Close fails at reopen.
	w.file = nil
	w.path = dir
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("Write with an unopenable path should fail at reopen")
	}
	w.path = filepath.Join(dir, "app.log")
	// Write error and short-write-adjacent paths via /dev/full: Sync
	// on /dev/full fails, and a plain write fails with ENOSPC.
	devFull, err := os.OpenFile("/dev/full", os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer devFull.Close()
	fw := &RotatingWriter{path: filepath.Join(dir, "app.log"), maxBytes: 4096, file: devFull, size: 0}
	if _, err := fw.Write([]byte("hello")); err == nil {
		t.Error("Write to /dev/full should fail")
	}
	// Rotate error inside Write: size forces rotation, Sync fails.
	devFull2, err := os.OpenFile("/dev/full", os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer devFull2.Close()
	rw := &RotatingWriter{path: filepath.Join(dir, "app.log"), maxBytes: 1024, file: devFull2, size: 1020}
	if _, err := rw.Write([]byte("hello")); err == nil {
		t.Error("Write whose rotation Sync fails should fail")
	}
	// Direct rotateLocked Sync failure leaves the file untouched.
	devFull3, err := os.OpenFile("/dev/full", os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer devFull3.Close()
	sw := &RotatingWriter{path: filepath.Join(dir, "app.log"), maxBytes: 1024, file: devFull3}
	if err := sw.rotateLocked(); err == nil {
		t.Error("rotateLocked with a failing Sync should fail")
	}
	if sw.file == nil {
		t.Error("failed Sync should leave the file handle in place")
	}
}

func TestContinue95RotatingWriterZeroBackupRemoveError(t *testing.T) {
	// Permission-failure branch requires a non-root user: root bypasses
	// directory permissions. CI runners are non-root and still exercise it.
	if os.Geteuid() == 0 {
		t.Skip("removal-failure branch requires a non-root user")
	}
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro0")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(roDir, "app.log")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	_, err := NewRotatingWriter(p, 1024, 0)
	if err := os.Chmod(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Error("startup rotation with zero backups in a read-only directory should fail at Remove")
	}
}

func TestContinue95RateLimitedWriterErrorPaths(t *testing.T) {
	// Passthrough when disabled or destination-less.
	if n, err := NewRateLimitedWriter(nil, 5, time.Minute).Write([]byte("abc")); n != 3 || err != nil {
		t.Errorf("nil-dst Write = %d, %v", n, err)
	}
	if n, err := NewRateLimitedWriter(&bytes.Buffer{}, 0, time.Minute).Write([]byte("abc")); n != 3 || err != nil {
		t.Errorf("zero-limit Write = %d, %v", n, err)
	}
	if w := NewRateLimitedWriter(&bytes.Buffer{}, 5, 0); w.window != time.Minute {
		t.Errorf("zero window should default to a minute, got %v", w.window)
	}
	// Destination error propagates.
	if _, err := NewRateLimitedWriter(Continue95ErrWriter{err: errors.New("sink down")}, 5, time.Minute).Write([]byte("abc")); err == nil {
		t.Error("dst error should propagate")
	}
	// Short destination write becomes ErrShortWrite.
	if _, err := NewRateLimitedWriter(Continue95ShortWriter{}, 5, time.Minute).Write([]byte("abc")); !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("short dst write err = %v, want ErrShortWrite", err)
	}
	// Window rollover flushes the suppressed count and resets.
	var buf bytes.Buffer
	w := NewRateLimitedWriter(&buf, 2, 20*time.Millisecond)
	if _, err := w.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("three")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := w.Write([]byte("four")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "suppressed=1") {
		t.Errorf("rollover output should report suppressed=1, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "four") {
		t.Errorf("post-rollover write missing from output %q", buf.String())
	}
}

func TestContinue95FanoutWriterMixedOutcomes(t *testing.T) {
	var good bytes.Buffer
	fw := NewFanoutWriter(Continue95ErrWriter{err: errors.New("first down")}, &good, Continue95ErrWriter{err: errors.New("third down")})
	n, err := fw.Write([]byte("hello"))
	if n != 5 || err == nil || !strings.Contains(err.Error(), "first down") {
		t.Errorf("fanout partial = %d, %v", n, err)
	}
	if good.String() != "hello" {
		t.Errorf("healthy sink got %q", good.String())
	}
	// All-fail returns zero bytes plus the first error.
	fw2 := NewFanoutWriter(Continue95ErrWriter{err: errors.New("a")}, Continue95ErrWriter{err: errors.New("b")})
	if n, err := fw2.Write([]byte("hello")); n != 0 || err == nil {
		t.Errorf("fanout all-fail = %d, %v", n, err)
	}
	// Short write in one sink counts as failure but a healthy sink still wins.
	var good2 bytes.Buffer
	fw3 := NewFanoutWriter(Continue95ShortWriter{}, &good2)
	if n, err := fw3.Write([]byte("hello")); n != 5 || !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("fanout short+good = %d, %v", n, err)
	}
	if good2.String() != "hello" {
		t.Errorf("healthy sink after short writer got %q", good2.String())
	}
}

func TestContinue95RotateLockedRenameErrors(t *testing.T) {
	// Permission-failure branch requires a non-root user: root bypasses
	// directory permissions. CI runners are non-root and still exercise it.
	if os.Geteuid() == 0 {
		t.Skip("rename-failure branch requires a non-root user")
	}
	// Direct rotateLocked calls in a read-only directory, where each
	// rename/remove step fails at a different point. The file handle
	// is opened before the directory is locked down, so Sync/Close
	// still succeed and the failure lands on the rename step.
	setup := func(t *testing.T, backups int, preexisting ...string) (*RotatingWriter, string) {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "app.log")
		if err := os.WriteFile(p, []byte("current"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, name := range preexisting {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		return &RotatingWriter{path: p, maxBytes: 1024, backups: backups, file: f, size: 7}, dir
	}
	// Oldest removal fails: backups=1 and app.log.1 exists.
	w, _ := setup(t, 1, "app.log.1")
	if err := w.rotateLocked(); err == nil {
		t.Error("rotateLocked with an unremovable oldest backup should fail")
	}
	// Shift rename fails: backups=3, .2 -> .3 blocked.
	w, _ = setup(t, 3, "app.log.1", "app.log.2")
	if err := w.rotateLocked(); err == nil {
		t.Error("rotateLocked with a blocked shift rename should fail")
	}
	// Final rename fails: backups=1, no .1 present (oldest removal is a
	// no-op), renaming the current file into place is blocked.
	w, _ = setup(t, 1)
	if err := w.rotateLocked(); err == nil {
		t.Error("rotateLocked with a blocked final rename should fail")
	}
}

func TestContinue95RotatingWriterOpenChmodBranch(t *testing.T) {
	// open() chmods the file to 0600 even when it pre-existed wider.
	// A symlink to /dev/null opens fine but cannot be chmodded by a
	// non-owner; as root chmod succeeds regardless, so skip there.
	if os.Geteuid() == 0 {
		t.Skip("chmod-failure branch requires a non-root user")
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "app.log")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	w := &RotatingWriter{path: link}
	if err := w.open(); err == nil {
		t.Error("open on an un-chmoddable file should fail")
	}
}

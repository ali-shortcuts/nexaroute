package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssue663CoreRotatingWriterValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewRotatingWriter("", 2048, 1); err == nil {
		t.Error("empty log path must be rejected")
	}
	if _, err := NewRotatingWriter("   ", 2048, 1); err == nil {
		t.Error("whitespace log path must be rejected")
	}
	if _, err := NewRotatingWriter(filepath.Join(dir, "a.log"), 1023, 1); err == nil {
		t.Error("max size below 1024 must be rejected")
	}
	if _, err := NewRotatingWriter(filepath.Join(dir, "a.log"), 2048, -1); err == nil {
		t.Error("negative backup count must be rejected")
	}
	// The path itself is a directory: opening it for append fails.
	if _, err := NewRotatingWriter(dir, 2048, 1); err == nil {
		t.Error("a directory as log path must fail at open")
	}
	// A blocked parent directory fails before any file is created.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRotatingWriter(filepath.Join(blocker, "a.log"), 2048, 1); err == nil {
		t.Error("a log path under a regular file must fail")
	}
}

func TestIssue663CoreRotatingWriterPreexistingOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// A pre-existing file already at the size limit must rotate on
	// construction: the old content moves to .1 and the new file starts
	// empty, so no single write can silently exceed the bound.
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotatingWriter(path, 2048, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("pre-existing oversized file must rotate to .1 on open: %v", err)
	}
	if n, err := w.Write([]byte("fresh")); err != nil || n != 5 {
		t.Fatalf("write after construction rotation = %d, %v", n, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() != 5 {
		t.Fatalf("current file size = %v, %v", st, err)
	}
}

func TestIssue663CoreRotatingWriterWriteAfterCloseAndTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := NewRotatingWriter(path, 2048, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write(nil); err != nil || n != 0 {
		t.Fatalf("empty write = %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal("double Close must be a no-op success")
	}
	// A closed writer reopens lazily on the next write.
	if n, err := w.Write([]byte("after-close")); err != nil || n != len("after-close") {
		t.Fatalf("write after close = %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "after-close" {
		t.Fatalf("log content = %q, %v", data, err)
	}

	// A single entry larger than the rotation limit is truncated with
	// a marker, and the caller is still told its original length.
	w2, err := NewRotatingWriter(filepath.Join(dir, "big.log"), 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w2.Close() }()
	big := bytes.Repeat([]byte("y"), 4096)
	n, err := w2.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("oversized write = %d, %v", n, err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "big.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 1024 || !strings.Contains(string(data), "truncated to rotation limit") {
		t.Fatalf("oversized entry must be truncated with a marker, got %d bytes", len(data))
	}
}

func TestIssue663CoreRateLimitedWriterWindowExpiry(t *testing.T) {
	var buf bytes.Buffer
	w := NewRateLimitedWriter(&buf, 1, time.Millisecond)
	if n, err := w.Write([]byte("first")); err != nil || n != 5 {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if n, err := w.Write([]byte("second")); err != nil || n != 6 {
		t.Fatalf("suppressed write must still report full length, got %d, %v", n, err)
	}
	// The window must genuinely expire before the next write: a short
	// bounded sleep is unavoidable here, and scheduler delay can only
	// push further past the 1ms window, never back inside it.
	time.Sleep(5 * time.Millisecond)
	// The next write must first emit the suppression summary, then the
	// entry itself.
	if _, err := w.Write([]byte("third")); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "suppressed=1") || !strings.Contains(got, "third") {
		t.Fatalf("window expiry must emit a suppression summary before the next entry, got %q", got)
	}
	// Limit zero and nil destination are pure pass-throughs.
	quiet := NewRateLimitedWriter(nil, 0, 0)
	if n, err := quiet.Write([]byte("x")); err != nil || n != 1 {
		t.Fatalf("unlimited writer = %d, %v", n, err)
	}
}

func TestIssue663CoreFanoutWriterNilAndAllFail(t *testing.T) {
	var buf bytes.Buffer
	w := NewFanoutWriter(nil, &buf, nil)
	if n, err := w.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("fanout with nil sinks = %d, %v", n, err)
	}
	if buf.String() != "hello" {
		t.Fatalf("fanout content = %q", buf.String())
	}
	empty := NewFanoutWriter()
	if n, err := empty.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("fanout with no sinks = %d, %v", n, err)
	}
	allFail := NewFanoutWriter(alwaysFailWriter{}, alwaysFailWriter{})
	if n, err := allFail.Write([]byte("hello")); err == nil || n != 0 {
		t.Fatalf("all-failing fanout = %d, %v", n, err)
	}
}

func TestIssue663CoreRotatingWriterCleanupSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// Entries the cleanup must leave alone: a subdirectory with a
	// numeric-looking suffix, a non-numeric suffix, and suffix 0.
	if err := os.Mkdir(filepath.Join(dir, "app.log.9"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.log.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.log.0"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An over-retention numeric backup must be removed on startup.
	if err := os.WriteFile(filepath.Join(dir, "app.log.5"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotatingWriter(path, 2048, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := os.Stat(filepath.Join(dir, "app.log.5")); !os.IsNotExist(err) {
		t.Fatalf("over-retention backup must be removed on startup, stat err=%v", err)
	}
	for _, name := range []string{"app.log.9", "app.log.txt", "app.log.0"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("cleanup must leave %s alone, stat err=%v", name, err)
		}
	}
}

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

func TestCoverageBehaviorRotatingWriterValidationAndBounds(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		path    string
		max     int64
		backups int
	}{
		{"", 1024, 1}, {filepath.Join(dir, "x"), 1023, 1}, {filepath.Join(dir, "x"), 1024, -1},
	}
	for _, tc := range cases {
		if w, err := NewRotatingWriter(tc.path, tc.max, tc.backups); err == nil || w != nil {
			t.Fatalf("invalid writer accepted: %#v err=%v", tc, err)
		}
	}
	path := filepath.Join(dir, "bounded.log")
	w, err := NewRotatingWriter(path, 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("x", 2000))
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("oversized write n=%d err=%v", n, err)
	}
	if b, err := os.ReadFile(path); err != nil || int64(len(b)) > 1024 || !bytes.Contains(b, []byte("truncated")) {
		t.Fatalf("bounded output invalid: len=%d err=%v body=%q", len(b), err, b)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageBehaviorRateLimitAndFanoutFailure(t *testing.T) {
	var out bytes.Buffer
	w := NewRateLimitedWriter(&out, 0, 0)
	if n, err := w.Write([]byte("discarded")); n != len("discarded") || err != nil || out.Len() != 0 {
		t.Fatalf("disabled writer n=%d err=%v out=%q", n, err, out.String())
	}
	short := writerFunc(func(p []byte) (int, error) { return len(p) - 1, nil })
	if n, err := NewRateLimitedWriter(short, 1, time.Minute).Write([]byte("abc")); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write n=%d err=%v", n, err)
	}
	if n, err := NewFanoutWriter().Write([]byte("ignored")); n != len("ignored") || err != nil {
		t.Fatalf("empty fanout n=%d err=%v", n, err)
	}
	fail := writerFunc(func([]byte) (int, error) { return 0, io.ErrClosedPipe })
	if n, err := NewFanoutWriter(fail).Write([]byte("x")); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failing fanout n=%d err=%v", n, err)
	}
}

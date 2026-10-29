package logging

import (
	"errors"
	"io"
	"testing"
)

type wave4FailWriter struct{ err error }

func (w wave4FailWriter) Write(p []byte) (int, error) { return 0, w.err }

type wave4ShortWriter struct{}

func (w wave4ShortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

// No writers at all: the write is a no-op success reporting the full length.
func TestWave4FanoutNoWriters(t *testing.T) {
	w := NewFanoutWriter()
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
}

// Nil writers are filtered at construction; a nil-only fanout behaves as empty.
func TestWave4FanoutNilWritersFiltered(t *testing.T) {
	w := NewFanoutWriter(nil, nil)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
}

// Every writer failing: zero bytes reported and the FIRST error wins.
func TestWave4FanoutAllWritersFail(t *testing.T) {
	first := errors.New("first failure")
	second := errors.New("second failure")
	w := NewFanoutWriter(wave4FailWriter{first}, wave4FailWriter{second})
	n, err := w.Write([]byte("hello"))
	if n != 0 {
		t.Fatalf("n = %d, want 0 when all writers fail", n)
	}
	if !errors.Is(err, first) {
		t.Fatalf("err = %v, want the first writer's error", err)
	}
}

// One success plus one failure: the full length is reported (the entry was
// delivered somewhere) but the failure is still surfaced.
func TestWave4FanoutPartialFailure(t *testing.T) {
	boom := errors.New("boom")
	var buf []byte
	sink := &wave4CaptureWriter{buf: &buf}
	w := NewFanoutWriter(sink, wave4FailWriter{boom})
	n, err := w.Write([]byte("hello"))
	if n != 5 {
		t.Fatalf("n = %d, want 5 (delivered to the healthy writer)", n)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the failing writer's error surfaced", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("healthy writer got %q, want hello", buf)
	}
}

// A writer that reports a short write without an error is treated as failed.
func TestWave4FanoutShortWriteIsFailure(t *testing.T) {
	w := NewFanoutWriter(wave4ShortWriter{})
	n, err := w.Write([]byte("hello"))
	if n != 0 {
		t.Fatalf("n = %d, want 0 for a short write", n)
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
}

type wave4CaptureWriter struct{ buf *[]byte }

func (w *wave4CaptureWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

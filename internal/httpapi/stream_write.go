package httpapi

import (
	"errors"
	"net/http"
	"time"
)

// ClientWriteError marks a downstream write failure (stalled or departed
// client). It must never poison upstream deployment health: the provider did
// its job, the caller could not take the bytes.
type ClientWriteError struct {
	Err error
}

func (e *ClientWriteError) Error() string { return "client write failed: " + e.Err.Error() }
func (e *ClientWriteError) Unwrap() error { return e.Err }

// AsClientWriteError reports whether err is (or wraps) a downstream write
// failure.
func AsClientWriteError(err error) bool {
	var cwe *ClientWriteError
	return errors.As(err, &cwe)
}

// clientWriteTimeout bounds a single downstream write (plus flush) so a
// stalled — half-open or non-reading — client cannot pin a handler goroutine,
// and the upstream stream plus provider slot behind it, indefinitely. Healthy
// writes complete in milliseconds; only genuinely stuck clients ever hit it.
const clientWriteTimeout = 30 * time.Second

// writeFlushed writes one chunk to a streaming client and flushes it.
func writeFlushed(w http.ResponseWriter, chunk []byte) error {
	return writeWithDeadline(w, chunk, true)
}

// writeOnce writes a single non-streaming response body.
func writeOnce(w http.ResponseWriter, chunk []byte) error {
	return writeWithDeadline(w, chunk, false)
}

// writeWithDeadline enforces a per-chunk write deadline on real network
// connections via http.ResponseController. The deadline is always cleared
// afterwards: a stale absolute deadline would otherwise break the next
// request served over the same keep-alive connection. Writers without
// deadline support (test recorders) fall back to a direct write.
func writeWithDeadline(w http.ResponseWriter, chunk []byte, flush bool) error {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(clientWriteTimeout)); err == nil {
		defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	}
	if _, err := w.Write(chunk); err != nil {
		return &ClientWriteError{Err: err}
	}
	if flush {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	return nil
}

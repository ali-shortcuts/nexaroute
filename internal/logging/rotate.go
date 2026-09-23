package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RotatingWriter bounds application-owned log storage to approximately
// maxBytes*(backups+1). Numeric backups beyond the configured retention are
// removed on startup and every rotation.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
	size     int64
}

func NewRotatingWriter(path string, maxBytes int64, backups int) (*RotatingWriter, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("log path is required")
	}
	if maxBytes < 1024 {
		return nil, fmt.Errorf("max log size must be at least 1024 bytes")
	}
	if backups < 0 {
		return nil, fmt.Errorf("log backups cannot be negative")
	}
	w := &RotatingWriter{path: path, maxBytes: maxBytes, backups: backups}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := w.cleanupBackups(); err != nil {
		return nil, err
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	if w.size >= w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			if w.file != nil {
				_ = w.file.Close()
				w.file = nil
			}
			return nil, err
		}
	}
	return w, nil
}

func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = st.Size()
	return nil
}

func (w *RotatingWriter) cleanupBackups() error {
	dir := filepath.Dir(w.path)
	base := filepath.Base(w.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	prefix := base + "."
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 1 {
			continue
		}
		if n > w.backups {
			name := filepath.Join(dir, entry.Name())
			if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (w *RotatingWriter) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			return err
		}
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if w.backups == 0 {
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		oldest := fmt.Sprintf("%s.%d", w.path, w.backups)
		if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
			return err
		}
		for i := w.backups - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", w.path, i)
			dst := fmt.Sprintf("%s.%d", w.path, i+1)
			if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := w.cleanupBackups(); err != nil {
		return err
	}
	return w.open()
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	originalLen := len(p)
	if originalLen == 0 {
		return 0, nil
	}
	data := p
	if int64(len(data)) > w.maxBytes {
		marker := []byte("\n[log entry truncated to rotation limit]\n")
		keep := int(w.maxBytes) - len(marker)
		if keep < 0 {
			keep = 0
		}
		data = append(append([]byte(nil), data[:keep]...), marker...)
	}
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(data)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(data)
	w.size += int64(n)
	if err != nil {
		return 0, err
	}
	if n != len(data) {
		return 0, io.ErrShortWrite
	}
	// Report the caller's original length even when an oversized entry was
	// truncated internally; log.Logger treats short writes as failures.
	return originalLen, nil
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// RateLimitedWriter protects external stdout/journald collectors from log
// storms. The bounded rotating file should remain the primary detailed sink.
type RateLimitedWriter struct {
	mu         sync.Mutex
	dst        io.Writer
	limit      int
	window     time.Duration
	started    time.Time
	written    int
	suppressed uint64
}

func NewRateLimitedWriter(dst io.Writer, limit int, window time.Duration) *RateLimitedWriter {
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimitedWriter{dst: dst, limit: limit, window: window}
}

func (w *RateLimitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.limit <= 0 || w.dst == nil {
		return len(p), nil
	}
	now := time.Now()
	if w.started.IsZero() {
		w.started = now
	}
	if now.Sub(w.started) >= w.window {
		if w.suppressed > 0 {
			_, _ = fmt.Fprintf(w.dst, "nexaroute log_rate_limit suppressed=%d window=%s\n", w.suppressed, w.window)
		}
		w.started = now
		w.written = 0
		w.suppressed = 0
	}
	if w.written >= w.limit {
		w.suppressed++
		return len(p), nil
	}
	w.written++
	n, err := w.dst.Write(p)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return len(p), nil
}


// FanoutWriter attempts every configured sink even if an earlier sink fails.
// This keeps stderr/journald available when the rotating file hits a runtime
// filesystem error (for example disk-full or permission changes).
type FanoutWriter struct {
	writers []io.Writer
}

func NewFanoutWriter(writers ...io.Writer) *FanoutWriter {
	filtered := make([]io.Writer, 0, len(writers))
	for _, w := range writers {
		if w != nil {
			filtered = append(filtered, w)
		}
	}
	return &FanoutWriter{writers: filtered}
}

func (w *FanoutWriter) Write(p []byte) (int, error) {
	if len(w.writers) == 0 {
		return len(p), nil
	}
	var firstErr error
	success := false
	for _, dst := range w.writers {
		n, err := dst.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		success = true
	}
	if success {
		return len(p), firstErr
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return len(p), nil
}

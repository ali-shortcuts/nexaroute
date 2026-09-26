// Package desktop contains the local CLI lifecycle helpers for the browser UI.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// Lock serializes local gateway ownership for one config directory. Kernel
// advisory locks are released automatically when the process exits/crashes.
type Lock struct{ file *os.File }

func Acquire(path string) (*Lock, bool, error) {
	if runtime.GOOS != "linux" {
		return nil, false, errors.New("desktop process lock currently requires Linux")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Lock{file: f}, true, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

// CommandRunner allows tests to assert launch preferences without invoking a
// desktop session. Start must be non-blocking with respect to browser lifetime.
type CommandRunner interface{ Start(string, string) error }
type execRunner struct{}

func (execRunner) Start(name, url string) error {
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

var BrowserCommands = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "xdg-open"}

func OpenBrowser(url string, lookPath func(string) (string, error), runner CommandRunner) (string, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if runner == nil {
		runner = execRunner{}
	}
	if _, isExec := runner.(execRunner); isExec && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return "", errors.New("no graphical desktop session detected")
	}
	var errs []error
	for _, name := range BrowserCommands {
		if _, err := lookPath(name); err != nil {
			continue
		}
		if err := runner.Start(name, url); err == nil {
			return name, nil
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if len(errs) == 0 {
		return "", errors.New("no supported browser found")
	}
	return "", errors.Join(errs...)
}

// WaitReady waits for the gateway health endpoint using bounded retry and
// short per-request timeout. It respects cancellation and has no goroutines.
func WaitReady(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

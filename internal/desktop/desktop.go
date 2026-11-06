// Package desktop contains the local CLI lifecycle helpers for the browser UI.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// Lock serializes local gateway ownership for one config directory. Kernel
// advisory locks are released automatically when the process exits/crashes.
// Acquire and Close live in desktop_lock_linux.go / desktop_lock_other.go:
// flock exists only on Linux here, and the non-Linux stub must fail closed.
type Lock struct{ file *os.File }

// CommandRunner allows tests to assert launch preferences without invoking a
// desktop session. Start must be non-blocking with respect to browser lifetime.
type CommandRunner interface{ Start(string, string) error }
type execRunner struct{}

func (execRunner) Start(name, url string) error {
	cmd := exec.Command(name, url) // #nosec G204 -- name is restricted to the fixed BrowserCommands allowlist by the only production caller (OpenBrowser above); url is the locally constructed dashboard URL passed as a single argv element and no shell is invoked.
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
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
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

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func defaultConfigPath() string {
	if p := os.Getenv("NEXAROUTE_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	// main reports an actionable error; never fall back to the working directory.
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, "nexaroute", "config.json")
}

func uiURL(addr net.Addr) string {
	host, port, _ := net.SplitHostPort(addr.String())
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// UI readiness deliberately does not use /readyz (upstream readiness).
func waitForUI(ctx context.Context, url string) error {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
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

func openBrowser(ctx context.Context, url string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("no graphical session")
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "xdg-open"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		// A browser may stay in the foreground. Reap it asynchronously; only
		// immediate failures should try the next launcher. Never block startup.
		cmd := exec.Command(path, url)
		if err := cmd.Start(); err != nil {
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case err := <-done:
			timer.Stop()
			if err != nil {
				continue
			}
			return nil
		case <-timer.C:
			return nil
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return fmt.Errorf("no browser launcher succeeded")
}

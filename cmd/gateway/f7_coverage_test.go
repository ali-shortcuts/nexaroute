package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Finding F7: dashboardURL and openExistingUI had 0% coverage. These tests
// assert observable URL normalization and already-running messaging without
// starting a server or opening a browser.
func TestDashboardURLFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := config.Default()
	cfg.Listen = "0.0.0.0:9090"
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := dashboardURL(path), "http://127.0.0.1:9090/"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDashboardURLFallbacks(t *testing.T) {
	if got := dashboardURL(filepath.Join(t.TempDir(), "missing.json")); got != "http://127.0.0.1:8080/" {
		t.Fatalf("missing config should fall back, got %q", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := dashboardURL(bad); got != "http://127.0.0.1:8080/" {
		t.Fatalf("corrupt config should fall back, got %q", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := config.Default()
	cfg.Listen = "not-a-listen-addr"
	if err := config.SaveAtomic(path, cfg); err != nil {
		// If the invalid listen value is rejected at save time, the fallback
		// path is still covered by the missing-file case above.
		t.Skipf("cannot persist invalid listen: %v", err)
	}
	if got := dashboardURL(path); got != "http://127.0.0.1:8080/" {
		t.Fatalf("invalid listen should fall back, got %q", got)
	}
}

func TestOpenExistingUIUnreachableReportsStarting(t *testing.T) {
	var buf bytes.Buffer
	// Port 1 is unreachable; WaitReady must time out quickly enough for the
	// deterministic "may already be starting" branch.
	openExistingUI("http://127.0.0.1:1/", &buf)
	if !strings.Contains(buf.String(), "may already be starting") {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestOpenExistingUIReadyReportsAlreadyRunning(t *testing.T) {
	// Headless CI has no DISPLAY, so OpenBrowser deterministically reports
	// "browser unavailable" after WaitReady succeeds against a live server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/healthz") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	openExistingUI(srv.URL, &buf)
	if !strings.Contains(buf.String(), "already running") {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestEnsureConfigRejectsUncreatableParent(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfig(filepath.Join(blocker, "sub", "config.json")); err == nil {
		t.Fatal("expected error when parent path is a file")
	}
}

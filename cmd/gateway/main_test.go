package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := defaultConfigPath(), filepath.Join(home, ".config/nexaroute/config.json"); got != want {
		t.Fatalf("%q != %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if got, want := defaultConfigPath(), filepath.Join(home, "xdg/nexaroute/config.json"); got != want {
		t.Fatalf("%q != %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	if got := defaultConfigPath(); got != "" {
		t.Fatalf("relative XDG path accepted: %s", got)
	}
	t.Setenv("NEXAROUTE_CONFIG", "/explicit/config.json")
	if got := defaultConfigPath(); got != "/explicit/config.json" {
		t.Fatal(got)
	}
}

func TestEnsureConfigPreservesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private/config.json")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	data, _ := os.ReadFile(path)
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(data) != string(after) {
		t.Fatal("config changed")
	}
}

func TestInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first, err := lockInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockInstance(path); err == nil {
		second.Close()
		t.Fatal("duplicate accepted")
	}
	// Atomic config replacement cannot release a sibling-file lock.
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	if second, err := lockInstance(path); err == nil {
		second.Close()
		t.Fatal("lock lost on config save")
	}
	first.Close()
	next, err := lockInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestUIURL(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{{"0.0.0.0", "http://127.0.0.1:8080/"}, {"::", "http://[::1]:8080/"}, {"127.0.0.1", "http://127.0.0.1:8080/"}} {
		if got := uiURL(&net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 8080}); got != tc.want {
			t.Fatalf("got %s", got)
		}
	}
}

func TestUIReadinessIsNotProviderReadiness(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Error(w, "providers not ready", 503)
			return
		}
		io.WriteString(w, "UI")
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForUI(ctx, s.URL+"/"); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := waitForUI(ctx2, s.URL+"/readyz"); err == nil {
		t.Fatal("accepted non-200 readiness")
	}
}

func TestBrowserOrderAndHeadless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("DISPLAY", ":test")
	t.Setenv("WAYLAND_DISPLAY", "")
	log := filepath.Join(dir, "launched")
	t.Setenv("BROWSER_TEST_LOG", log)
	names := []string{"google-chrome", "google-chrome-stable", "chromium", "xdg-open"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf '%s\\n' \"$0 $1\" >> \"$BROWSER_TEST_LOG\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		if err := openBrowser(context.Background(), "http://127.0.0.1:8080/"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(log)
		if !strings.HasSuffix(string(b), name+" http://127.0.0.1:8080/\n") {
			t.Fatalf("wrong order: %s", b)
		}
		os.Remove(filepath.Join(dir, name))
	}
	if err := openBrowser(context.Background(), "http://127.0.0.1:8080/"); err == nil {
		t.Fatal("missing browsers not reported")
	}
	t.Setenv("DISPLAY", "")
	if err := openBrowser(context.Background(), "http://127.0.0.1:8080/"); err == nil {
		t.Fatal("headless not reported")
	}
}

func TestFailedBrowserFallsBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("DISPLAY", ":test")
	if err := os.WriteFile(filepath.Join(dir, "google-chrome"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chromium"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := openBrowser(context.Background(), "http://127.0.0.1:8080/"); err != nil {
		t.Fatal(err)
	}
}

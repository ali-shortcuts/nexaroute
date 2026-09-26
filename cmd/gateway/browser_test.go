package main

import (
	"bytes"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func restoreBrowserHooks(t *testing.T) {
	t.Helper()
	prevLook, prevStart, prevEnv := lookPath, startCommand, getenv
	t.Cleanup(func() {
		lookPath = prevLook
		startCommand = prevStart
		getenv = prevEnv
	})
}

func TestPublicUIURLRewritesWildcardBind(t *testing.T) {
	if got := publicUIURL("0.0.0.0:8080"); got != "http://127.0.0.1:8080/" {
		t.Fatalf("got %q", got)
	}
	if got := publicUIURL("127.0.0.1:9090"); got != "http://127.0.0.1:9090/" {
		t.Fatalf("got %q", got)
	}
}

func TestOpenUIPrefersChromeAndDoesNotWait(t *testing.T) {
	restoreBrowserHooks(t)
	var mu sync.Mutex
	var started []string
	lookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}
	startCommand = func(name string, args ...string) error {
		mu.Lock()
		started = append(started, name)
		mu.Unlock()
		return nil
	}
	getenv = func(key string) string {
		if key == "DISPLAY" {
			return ":0"
		}
		return ""
	}

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		openUI("http://127.0.0.1:8080/", &buf)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("openUI blocked")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 || !strings.Contains(started[0], "google-chrome") {
		t.Fatalf("started=%v want google-chrome first", started)
	}
	if !strings.Contains(buf.String(), "http://127.0.0.1:8080/") {
		t.Fatalf("URL not printed: %s", buf.String())
	}
}

func TestOpenUIHeadlessPrintsURLAndDoesNotFail(t *testing.T) {
	restoreBrowserHooks(t)
	lookPath = func(string) (string, error) {
		t.Fatal("lookPath should not run when headless")
		return "", exec.ErrNotFound
	}
	startCommand = func(string, ...string) error {
		t.Fatal("browser must not start when headless")
		return nil
	}
	getenv = func(string) string { return "" }
	var buf bytes.Buffer
	openUI("http://127.0.0.1:8080/", &buf)
	if !strings.Contains(buf.String(), "http://127.0.0.1:8080/") {
		t.Fatalf("missing URL: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "skipped") {
		t.Fatalf("headless skip not mentioned: %s", buf.String())
	}
}

func TestLaunchUIWhenReadyWaitsForHealthz(t *testing.T) {
	restoreBrowserHooks(t)
	getenv = func(string) string { return "" }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(200)
			return
		}
		http.NotFound(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })

	var buf bytes.Buffer
	launchUIWhenReady("http://"+ln.Addr().String()+"/", &buf)
	if !strings.Contains(buf.String(), ln.Addr().String()) {
		t.Fatalf("ready launch did not print URL: %s", buf.String())
	}
}

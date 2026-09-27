package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	calls []string
	fail  map[string]bool
}

func (f *fakeRunner) Start(name, url string) error {
	f.calls = append(f.calls, name+" "+url)
	if f.fail[name] {
		return errors.New("fake launch failure")
	}
	return nil
}

func TestOpenBrowserPreferenceAndFallback(t *testing.T) {
	available := map[string]bool{"google-chrome": true, "google-chrome-stable": true, "chromium": true, "chromium-browser": true, "xdg-open": true}
	look := func(name string) (string, error) {
		if available[name] {
			return "/fake/" + name, nil
		}
		return "", errors.New("not installed")
	}
	runner := &fakeRunner{fail: map[string]bool{"google-chrome": true}}
	got, err := OpenBrowser("http://127.0.0.1:8080/", look, runner)
	if err != nil || got != "google-chrome-stable" {
		t.Fatalf("got browser=%q err=%v", got, err)
	}
	want := []string{"google-chrome http://127.0.0.1:8080/", "google-chrome-stable http://127.0.0.1:8080/"}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls=%v want %v", runner.calls, want)
	}
}

func TestOpenBrowserHeadlessNoCommandIsGraceful(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	_, err := OpenBrowser("http://127.0.0.1:8080/", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no graphical") {
		t.Fatalf("expected graceful headless error for caller to report URL, got %v", err)
	}
}

func TestLockExcludesDuplicateAndRecoversAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexaroute.lock")
	first, ok, err := Acquire(path)
	if err != nil || !ok {
		t.Fatalf("first acquire ok=%v err=%v", ok, err)
	}
	second, ok, err := Acquire(path)
	if err != nil || ok || second != nil {
		t.Fatalf("duplicate acquire lock=%v ok=%v err=%v", second, ok, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, ok, err := Acquire(path)
	if err != nil || !ok {
		t.Fatalf("lock should recover after owner releases it: ok=%v err=%v", ok, err)
	}
	_ = third.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock file permissions not private: info=%v err=%v", info, err)
	}
}

func TestWaitReady(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-started:
		default:
			close(started)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WaitReady(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := WaitReady(ctx, "http://127.0.0.1:1"); err == nil {
		t.Fatal("expected bounded readiness timeout")
	}
}

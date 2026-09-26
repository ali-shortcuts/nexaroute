package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceLockSecondInvocationDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.lock")
	first, existing, err := acquireInstanceLock(path, instanceInfo{PID: 1, Listen: "127.0.0.1:8080", URL: "http://127.0.0.1:8080/", Config: "a.json"})
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || existing != nil {
		t.Fatalf("first lock failed: lock=%v existing=%v", first, existing)
	}
	defer first.Close()

	second, existing, err := acquireInstanceLock(path, instanceInfo{PID: 2, Listen: "127.0.0.1:8081", URL: "http://127.0.0.1:8081/"})
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		_ = second.Close()
		t.Fatal("second process acquired lock")
	}
	if existing == nil || existing.URL != "http://127.0.0.1:8080/" {
		t.Fatalf("existing info=%+v", existing)
	}

	restoreBrowserHooks(t)
	getenv = func(string) string { return "" }
	var buf bytes.Buffer
	handleExistingInstance(existing, &buf)
	if !strings.Contains(buf.String(), "already running") {
		t.Fatalf("missing already-running message: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "http://127.0.0.1:8080/") {
		t.Fatalf("missing existing UI URL: %s", buf.String())
	}
}

func TestInstanceLockRecoversAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexaroute", "instance.lock")
	first, _, err := acquireInstanceLock(path, instanceInfo{PID: os.Getpid(), URL: "http://127.0.0.1:1/"})
	if err != nil || first == nil {
		t.Fatalf("first: %v %v", first, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, existing, err := acquireInstanceLock(path, instanceInfo{PID: os.Getpid(), URL: "http://127.0.0.1:2/"})
	if err != nil {
		t.Fatal(err)
	}
	if second == nil {
		t.Fatalf("stale lock was not recovered; existing=%+v", existing)
	}
	_ = second.Close()
}

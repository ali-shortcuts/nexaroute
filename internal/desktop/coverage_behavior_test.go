package desktop

import (
	"errors"
	"strings"
	"testing"
)

func TestCoverageBehaviorOpenBrowserMissingAndAllFailures(t *testing.T) {
	lookMissing := func(string) (string, error) { return "", errors.New("missing") }
	runner := &fakeRunner{}
	if name, err := OpenBrowser("http://127.0.0.1:1", lookMissing, runner); name != "" || err == nil || !strings.Contains(err.Error(), "no supported") || len(runner.calls) != 0 {
		t.Fatalf("missing browser name=%q err=%v calls=%v", name, err, runner.calls)
	}
	lookAll := func(name string) (string, error) { return "/fake/" + name, nil }
	failAll := map[string]bool{}
	for _, name := range BrowserCommands {
		failAll[name] = true
	}
	runner = &fakeRunner{fail: failAll}
	if name, err := OpenBrowser("http://127.0.0.1:1", lookAll, runner); name != "" || err == nil || !strings.Contains(err.Error(), "google-chrome") || len(runner.calls) != len(BrowserCommands) {
		t.Fatalf("all failures name=%q err=%v calls=%v", name, err, runner.calls)
	}
}

func TestCoverageBehaviorLockCloseAndExecRunner(t *testing.T) {
	var nilLock *Lock
	if err := nilLock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (&Lock{}).Close(); err != nil {
		t.Fatal(err)
	}
	if err := (execRunner{}).Start("/definitely/not/a/real/browser", "http://127.0.0.1:1"); err == nil {
		t.Fatal("invalid executable should fail")
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigPathUsesXDGConfigDirectory(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "")
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got, want := defaultConfigPath(), filepath.Join(dir, "nexaroute", "config.json"); got != want {
		t.Fatalf("defaultConfigPath=%q want %q", got, want)
	}
}

func TestDashboardURLUsesLoopbackForWildcardBinds(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"127.0.0.1:8080", "http://127.0.0.1:8080/"},
		{"0.0.0.0:9090", "http://127.0.0.1:9090/"},
		{"[::]:8080", "http://127.0.0.1:8080/"},
	} {
		if got := dashboardURL(tc.listen); got != tc.want {
			t.Errorf("dashboardURL(%q)=%q want %q", tc.listen, got, tc.want)
		}
	}
}

func TestOpenUIWaitsForReadinessBeforeInjectableBrowserLaunch(t *testing.T) {
	var order []string
	var output bytes.Buffer
	wait := func(ctx context.Context, url string) error {
		order = append(order, "ready:"+url)
		return nil
	}
	launch := func(url string) (string, error) {
		order = append(order, "browser:"+url)
		return "fake-chrome", nil
	}
	if !openUI("http://127.0.0.1:8080/", &output, wait, launch, false) {
		t.Fatal("expected UI open success")
	}
	if got, want := strings.Join(order, ","), "ready:http://127.0.0.1:8080,browser:http://127.0.0.1:8080/"; got != want {
		t.Fatalf("open order=%q want %q", got, want)
	}
	if !strings.Contains(output.String(), "fake-chrome") {
		t.Fatalf("missing launch status: %s", output.String())
	}
}

func TestOpenUIHeadlessReportsURLWithoutFailingGateway(t *testing.T) {
	var output bytes.Buffer
	opened := openUI("http://127.0.0.1:8080/", &output,
		func(context.Context, string) error { return nil },
		func(string) (string, error) { return "", errors.New("no GUI") }, false)
	if opened || !strings.Contains(output.String(), "http://127.0.0.1:8080/") {
		t.Fatalf("headless result opened=%v output=%q", opened, output.String())
	}
}

func TestDefaultConfigPathHonorsExplicitConfig(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "/tmp/nexaroute-test.json")
	if got := defaultConfigPath(); got != "/tmp/nexaroute-test.json" {
		t.Fatalf("defaultConfigPath=%q want explicit path", got)
	}
}

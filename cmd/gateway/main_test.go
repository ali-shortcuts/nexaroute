package main

import (
	"path/filepath"
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

func TestDefaultConfigPathHonorsExplicitConfig(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "/tmp/nexaroute-test.json")
	if got := defaultConfigPath(); got != "/tmp/nexaroute-test.json" {
		t.Fatalf("defaultConfigPath=%q want explicit path", got)
	}
}

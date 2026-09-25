package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigPathNeverUsesTrackedExampleAsRuntimeConfig(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "")
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("configs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("configs", "config.example.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	want := filepath.Join(dir, ".config", "nexaroute", "config.json")
	if got := defaultConfigPath(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDashboardWildcardURL(t *testing.T) {
	for addr, want := range map[string]string{"0.0.0.0:8080": "http://127.0.0.1:8080/", "[::]:9090": "http://127.0.0.1:9090/", "[::1]:8000": "http://[::1]:8000/"} {
		if got := dashboardURL(addr); got != want {
			t.Fatalf("%s => %s", addr, got)
		}
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigPathUsesXDG(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/nexaroute-xdg-config")
	got := defaultConfigPath()
	want := filepath.Join("/tmp/nexaroute-xdg-config", "nexaroute", "config.json")
	if got != want {
		t.Fatalf("defaultConfigPath=%q want %q", got, want)
	}
}

func TestDefaultConfigPathNeverUsesTrackedExampleAsRuntimeConfig(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
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
	got := defaultConfigPath()
	if got == filepath.Join("configs", "config.example.json") {
		t.Fatal("default config path must not use the tracked example file")
	}
	if !strings.HasSuffix(got, filepath.Join(".config", "nexaroute", "config.json")) {
		t.Fatalf("defaultConfigPath=%q want XDG ~/.config/nexaroute/config.json", got)
	}
}

func TestNEXAROUTE_CONFIGOverridesXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	t.Setenv("NEXAROUTE_CONFIG", "/custom/nexaroute.json")
	if got := defaultConfigPath(); got != "/custom/nexaroute.json" {
		t.Fatalf("got %q", got)
	}
}

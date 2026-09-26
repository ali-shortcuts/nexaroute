package main

import (
	"os"
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

func TestDefaultConfigPathHonorsExplicitConfig(t *testing.T) {
	t.Setenv("NEXAROUTE_CONFIG", "/tmp/nexaroute-test.json")
	if got := defaultConfigPath(); got != "/tmp/nexaroute-test.json" {
		t.Fatalf("defaultConfigPath=%q want explicit path", got)
	}
}

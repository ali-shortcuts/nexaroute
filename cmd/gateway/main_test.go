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
	if got := defaultConfigPath(); got != "config.json" {
		t.Fatalf("defaultConfigPath=%q want config.json", got)
	}
}

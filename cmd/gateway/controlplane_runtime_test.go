package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestControlPlaneRuntimeFileReconcilesWithoutSecrets(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.ControlPlaneConfig{Enabled: true, Backend: "file", ConfigFailure: "last_known_good"}
	rt, err := openControlPlaneRuntime(cfg, configPath)
	if err != nil || rt == nil {
		t.Fatalf("open: %v", err)
	}
	statePath := configPath + ".controlplane.json"
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || strings.Contains(string(data), "postgres://") || strings.Contains(string(data), "redis://") {
		t.Fatal("control-plane state contains secret material")
	}
	got, err := rt.manager.Load(context.Background(), controlPlaneNamespace)
	if err != nil || string(got.Payload) == "" {
		t.Fatalf("snapshot: %+v %v", got, err)
	}
}

func TestControlPlaneRuntimeDryRunAndExternalFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if rt, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "file", MigrationDryRun: true}, path); err != nil || rt == nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(path + ".controlplane.json"); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote state: %v", err)
	}
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "postgres", PostgresDSNEnv: "PG_DSN"}, path); err == nil {
		t.Fatal("postgres silently fell back")
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
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

func TestControlPlaneRuntimeMigratesExistingStateAndRecoversOnRestart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	statePath := configPath + ".controlplane.json"
	legacy, err := controlplane.NewFileStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := legacy.Put(context.Background(), controlPlaneNamespace, 0, []byte(`{"legacy":"preserve"}`))
	if err != nil {
		t.Fatal(err)
	}

	rt, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "file"}, configPath)
	if err != nil {
		t.Fatalf("migration startup: %v", err)
	}
	got, err := rt.manager.Load(context.Background(), controlPlaneNamespace)
	if err != nil || string(got.Payload) != `{"legacy":"preserve"}` || got.Revision != seed.Revision {
		t.Fatalf("migration replaced local state: got=%+v err=%v seed=%+v", got, err, seed)
	}

	restarted, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "file"}, configPath)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	recovered, err := restarted.manager.Load(context.Background(), controlPlaneNamespace)
	if err != nil || string(recovered.Payload) != `{"legacy":"preserve"}` || recovered.Revision != seed.Revision {
		t.Fatalf("restart lost local state: got=%+v err=%v", recovered, err)
	}
}

func TestControlPlaneRuntimeConcurrentWritesUseOptimisticRevision(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	rt, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "file"}, configPath)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := rt.manager.Load(context.Background(), controlPlaneNamespace)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, payload := range []string{`{"writer":1}`, `{"writer":2}`} {
		wg.Add(1)
		go func(payload string) {
			defer wg.Done()
			_, saveErr := rt.manager.Save(context.Background(), controlPlaneNamespace, initial.Revision, []byte(payload))
			results <- saveErr
		}(payload)
	}
	wg.Wait()
	close(results)

	var successes, conflicts int
	for saveErr := range results {
		if saveErr == nil {
			successes++
		} else if errors.Is(saveErr, controlplane.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent write error: %v", saveErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent writes: successes=%d conflicts=%d", successes, conflicts)
	}
}

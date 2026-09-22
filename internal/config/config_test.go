package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAtomicModeAndNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Default()
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%#o want 0600", st.Mode().Perm())
	}

	if err := os.WriteFile(path+".bak", []byte("stale secret copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.MaxAttempts = 9
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("backup file must not be created; stat err=%v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".config.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files leaked: %v", leftovers)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.MaxAttempts != 9 {
		t.Fatalf("latest config was not persisted: %+v", loaded.Routing)
	}
}

func TestRoutingStrategiesValidate(t *testing.T) {
	for _, strategy := range []string{"adaptive", "adaptive_round_robin", "priority", "round_robin", "least_latency"} {
		cfg := Default()
		cfg.Routing.Strategy = strategy
		if err := cfg.Validate(); err != nil {
			t.Fatalf("strategy %s rejected: %v", strategy, err)
		}
	}
	cfg := Default()
	cfg.Routing.Strategy = "not-real"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "routing.strategy") {
		t.Fatalf("bad strategy should fail, got %v", err)
	}
}

func TestLoadAppliesRuntimeEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveAtomic(path, Default()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_LISTEN", "0.0.0.0:19000")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "env-admin")
	t.Setenv("NEXAROUTE_ADMIN_BIND_LOCAL_ONLY", "false")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:19000" || cfg.Admin.APIKey != "env-admin" || cfg.Admin.BindLocalOnly {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAtomicModeAndRollbackBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
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
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routing.MaxAttempts = 9
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != string(first) {
		t.Fatal("backup is not the previous known-good config")
	}
	loaded, err := LoadBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.MaxAttempts == 9 {
		t.Fatalf("backup unexpectedly contains newest config: %+v", loaded.Routing)
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
	t.Setenv("ULG_LISTEN", "0.0.0.0:19000")
	t.Setenv("ULG_ADMIN_KEY", "env-admin")
	t.Setenv("ULG_ADMIN_BIND_LOCAL_ONLY", "false")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:19000" || cfg.Admin.APIKey != "env-admin" || cfg.Admin.BindLocalOnly {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
}

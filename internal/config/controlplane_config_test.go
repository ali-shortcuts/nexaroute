package config

import "testing"

func TestControlPlaneDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.PostgresDSNEnv = "NEXAROUTE_PG_DSN"
	cfg.ControlPlane.RedisURLEnv = "NEXAROUTE_REDIS_URL"
	cfg.ApplyDefaults()
	if cfg.ControlPlane.Backend != "file" || cfg.ControlPlane.ConfigFailure != "last_known_good" || cfg.ControlPlane.IdentityFailure != "fail_closed" || cfg.ControlPlane.BudgetFailure != "fail_closed" || cfg.ControlPlane.RateLimitFailure != "fail_closed" {
		t.Fatalf("unexpected defaults: %+v", cfg.ControlPlane)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestControlPlaneRejectsInvalidBackendAndRequiresRelevantDSN(t *testing.T) {
	cfg := Default()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.Backend = "unknown"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown backend accepted")
	}
	cfg.ControlPlane.Backend = "postgres"
	cfg.ControlPlane.PostgresDSNEnv = "postgres://user:pass@db"
	if err := cfg.Validate(); err == nil {
		t.Fatal("inline DSN accepted")
	}
	cfg.ControlPlane.PostgresDSNEnv = "PG_DSN"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.ControlPlane.Backend = "redis"
	cfg.ControlPlane.RedisURLEnv = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("missing redis env name accepted")
	}
}

func TestControlPlaneRejectsInvalidModes(t *testing.T) {
	cfg := Default()
	cfg.ControlPlane.Enabled = true
	cfg.ApplyDefaults()
	cfg.ControlPlane.ConfigFailure = "maybe"
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid failure mode accepted")
	}
}

func TestControlPlaneBreakerSyncValidation(t *testing.T) {
	cfg := Default()
	cfg.ControlPlane.BreakerSync = true
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("breaker_sync without control_plane.enabled accepted")
	}
	cfg.ControlPlane.Enabled = true
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("breaker_sync with file control plane rejected: %v", err)
	}
	if got := cfg.BreakerSyncInterval(); got != 10_000_000_000 {
		t.Fatalf("default breaker sync interval = %v, want 10s", got)
	}
	cfg.ControlPlane.BreakerSyncIntervalSeconds = 3
	if err := cfg.Validate(); err == nil {
		t.Fatal("breaker_sync interval below the floor accepted")
	}
	cfg.ControlPlane.BreakerSyncIntervalSeconds = 30
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid breaker_sync interval rejected: %v", err)
	}
	if got := cfg.BreakerSyncInterval(); got != 30_000_000_000 {
		t.Fatalf("configured breaker sync interval = %v, want 30s", got)
	}
	cfg.ControlPlane.BreakerSyncReplicaID = string(make([]byte, 129))
	if err := cfg.Validate(); err == nil {
		t.Fatal("oversized breaker_sync replica ID accepted")
	}
}

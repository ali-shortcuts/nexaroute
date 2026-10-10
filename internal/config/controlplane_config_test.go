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

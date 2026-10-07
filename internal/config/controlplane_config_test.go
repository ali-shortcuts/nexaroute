package config

import "testing"

func TestControlPlaneDefaultsAndValidation(t *testing.T) {
	cfg := Default()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.PostgresDSNEnv = "NEXAROUTE_PG_DSN"
	cfg.ControlPlane.RedisURLEnv = "NEXAROUTE_REDIS_URL"
	cfg.ApplyDefaults()
	if cfg.ControlPlane.ConfigFailure != "last_known_good" || cfg.ControlPlane.IdentityFailure != "fail_closed" || cfg.ControlPlane.BudgetFailure != "fail_closed" || cfg.ControlPlane.RateLimitFailure != "fail_closed" {
		t.Fatalf("unexpected defaults: %+v", cfg.ControlPlane)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestControlPlaneRejectsInlineDSNsAndInvalidModes(t *testing.T) {
	cfg := Default()
	cfg.ControlPlane.Enabled = true
	cfg.ControlPlane.PostgresDSNEnv = "postgres://user:pass@db"
	cfg.ControlPlane.RedisURLEnv = "REDIS_URL"
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("inline DSN accepted")
	}
	cfg.ControlPlane.PostgresDSNEnv = "PG_DSN"
	cfg.ControlPlane.ConfigFailure = "maybe"
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid failure mode accepted")
	}
}

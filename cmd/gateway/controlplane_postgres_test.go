package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// The PostgreSQL control-plane backend must fail startup closed and
// secret-safely: errors name the environment variable, never the DSN or
// any credential inside it.
func TestControlPlanePostgresFailsClosedAndRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	// Env var not set at all.
	t.Setenv("NEXAROUTE_TEST_CP_PG_DSN", "")
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled: true, Backend: "postgres", PostgresDSNEnv: "NEXAROUTE_TEST_CP_PG_DSN",
	}, path); err == nil || !strings.Contains(err.Error(), "NEXAROUTE_TEST_CP_PG_DSN") {
		t.Fatalf("missing DSN env: err=%v, want a fail-closed error naming the variable", err)
	}

	// Env var set to an unreachable DSN carrying a canary password: the
	// startup error must not leak the credential (or the DSN at all).
	const canary = "CANARY-PASSWORD-9f3b"
	dsn := "postgres://nexaroute:" + canary + "@127.0.0.1:1/nexaroute?connect_timeout=2"
	t.Setenv("NEXAROUTE_TEST_CP_PG_DSN", dsn)
	_, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled: true, Backend: "postgres", PostgresDSNEnv: "NEXAROUTE_TEST_CP_PG_DSN",
	}, path)
	if err == nil {
		t.Fatal("unreachable PostgreSQL must fail startup")
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), dsn) {
		t.Fatalf("startup error leaks DSN material: %v", err)
	}
	if !strings.Contains(err.Error(), "NEXAROUTE_TEST_CP_PG_DSN") {
		t.Fatalf("startup error should name the env variable for remediation: %v", err)
	}

	// A migration dry run must fail the same way (readiness ping) rather
	// than silently skipping the backend.
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled: true, Backend: "postgres", PostgresDSNEnv: "NEXAROUTE_TEST_CP_PG_DSN", MigrationDryRun: true,
	}, path); err == nil {
		t.Fatal("dry-run against unreachable PostgreSQL must fail closed")
	}

	// Redis remains a deliberate, explicit refusal until its Store
	// adapter is registered.
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled: true, Backend: "redis", RedisURLEnv: "NEXAROUTE_TEST_CP_REDIS_URL",
	}, path); err == nil || !strings.Contains(err.Error(), "refusing unsafe fallback") {
		t.Fatalf("redis: err=%v, want the explicit fail-closed refusal", err)
	}
}

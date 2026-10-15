package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// The Redis control-plane backend must fail startup closed and
// secret-safely, exactly like PostgreSQL: errors name the environment
// variable, never the URL or any credential inside it.
func TestControlPlaneRedisFailsClosedAndRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	// Unreachable Redis carrying a canary password.
	const canary = "CANARY-REDIS-PASSWORD-71ad"
	t.Setenv("NEXAROUTE_TEST_CP_REDIS_URL", "redis://nexaroute:"+canary+"@127.0.0.1:1/0")
	_, err := openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled: true, Backend: "redis", RedisURLEnv: "NEXAROUTE_TEST_CP_REDIS_URL",
	}, path)
	if err == nil {
		t.Fatal("unreachable Redis must fail startup")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("startup error leaks the Redis password: %v", err)
	}
	if !strings.Contains(err.Error(), "NEXAROUTE_TEST_CP_REDIS_URL") {
		t.Fatalf("startup error should name the env variable for remediation: %v", err)
	}

	// A malformed URL fails closed without echoing it back.
	t.Setenv("NEXAROUTE_TEST_CP_REDIS_URL", "://not a url "+canary)
	_, err = openControlPlaneRuntime(config.ControlPlaneConfig{
		Enabled: true, Backend: "redis", RedisURLEnv: "NEXAROUTE_TEST_CP_REDIS_URL",
	}, path)
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("malformed URL: err=%v, want a redacted fail-closed error", err)
	}
}

type redisCloseProbe struct{ calls int }

func (p *redisCloseProbe) Close() error {
	p.calls++
	return nil
}

func TestControlPlaneRuntimeCloseClosesRedisResource(t *testing.T) {
	probe := &redisCloseProbe{}
	runtime := &controlPlaneRuntime{closer: probe}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if probe.calls != 1 {
		t.Fatalf("closer invoked %d times, want 1", probe.calls)
	}
}

//go:build integration

package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestControlPlaneRedisIntegrationRuntimeLifecycle(t *testing.T) {
	address := os.Getenv("NEXAROUTE_TEST_REDIS_URL")
	if address == "" {
		t.Skip("NEXAROUTE_TEST_REDIS_URL not set; skipping Redis runtime integration test")
	}
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatalf("parse disposable Redis URL: %v", err)
	}
	// Keep the fixed gateway/runtime namespace isolated from other tests.
	parsed.Path = "/15"
	runtimeURL := parsed.String()
	const envName = "NEXAROUTE_TEST_CP_RUNTIME_REDIS_URL"
	t.Setenv(envName, runtimeURL)
	options, err := redis.ParseURL(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	observer := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := observer.Ping(ctx).Err(); err != nil {
		_ = observer.Close()
		t.Fatalf("disposable Redis DB 15 unavailable: %v", err)
	}
	defer observer.Close()
	const key = "nexaroute:control:snapshot:gateway/runtime"
	if err := observer.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Del(context.Background(), key).Err() })

	configPath := filepath.Join(t.TempDir(), "gateway.json")
	cfg := config.ControlPlaneConfig{Enabled: true, Backend: "redis", RedisURLEnv: envName, MigrationDryRun: true}
	dryRun, err := openControlPlaneRuntime(cfg, configPath)
	if err != nil {
		t.Fatalf("migration dry-run startup: %v", err)
	}
	if dryRun == nil || dryRun.closer == nil {
		t.Fatal("dry-run runtime has no Redis resource")
	}
	if err := dryRun.Close(); err != nil {
		t.Fatalf("close dry-run runtime: %v", err)
	}
	fields, err := observer.HGetAll(ctx, key).Result()
	if err != nil || len(fields) != 0 {
		t.Fatalf("migration dry-run wrote the snapshot: fields=%#v err=%v", fields, err)
	}
	if _, err := os.Stat(configPath + ".controlplane.json"); !os.IsNotExist(err) {
		t.Fatalf("Redis runtime created a local fallback file: %v", err)
	}

	cfg.MigrationDryRun = false
	runtime, err := openControlPlaneRuntime(cfg, configPath)
	if err != nil {
		t.Fatalf("Redis runtime startup: %v", err)
	}
	snapshot, err := runtime.manager.Load(ctx, controlPlaneNamespace)
	if err != nil || snapshot.Revision != 1 || string(snapshot.Payload) != snapshotPayload {
		t.Fatalf("runtime snapshot: %+v, %v", snapshot, err)
	}
	client, ok := runtime.closer.(*redis.Client)
	if !ok {
		t.Fatalf("unexpected runtime Redis closer type %T", runtime.closer)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("close Redis runtime: %v", err)
	}
	if err := client.Ping(ctx).Err(); err == nil {
		t.Fatal("runtime Close did not close the Redis client")
	}
	persisted, err := observer.HGetAll(ctx, key).Result()
	if err != nil || persisted["payload"] != snapshotPayload {
		t.Fatalf("snapshot missing after client shutdown: fields=%#v err=%v", persisted, err)
	}

	// A fresh runtime sees the durable value and does not initialize over it.
	restarted, err := openControlPlaneRuntime(cfg, configPath)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	recovered, err := restarted.manager.Load(ctx, controlPlaneNamespace)
	if err != nil || recovered.Revision != 1 || string(recovered.Payload) != snapshotPayload {
		t.Fatalf("restart did not recover snapshot: %+v, %v", recovered, err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatalf("close restarted runtime: %v", err)
	}
	if err := observer.Ping(ctx).Err(); err != nil {
		t.Fatalf("observer should remain available: %v", err)
	}
}

const snapshotPayload = `{"schema":1,"backend":"redis"}`

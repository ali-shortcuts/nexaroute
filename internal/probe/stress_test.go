package probe

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func requireProbeStress(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_STRESS") != "1" {
		t.Skip("set NEXAROUTE_STRESS=1 to run bounded stress checks")
	}
}

func TestStressRecoveryFloodFiveThousandDeploymentsStaysBounded(t *testing.T) {
	requireProbeStress(t)
	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 32
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		AuthMode: "none", Enabled: true, MaxConcurrency: 64,
	}}
	for i := 0; i < 5000; i++ {
		cfg.Providers[0].Models = append(cfg.Providers[0].Models, config.ModelConfig{
			ID: fmt.Sprintf("m%d", i), Model: fmt.Sprintf("model-%d", i), Enabled: true, Weight: 1,
		})
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(128))
	ctx, cancel := context.WithCancel(context.Background())
	e.setRunContext(ctx)

	before := runtime.NumGoroutine()
	for _, d := range rt.All() {
		hm.ForceCooldown(d.ID, "stress", time.Minute)
		e.Recover(d.ID)
	}
	time.Sleep(100 * time.Millisecond)
	stats := e.Stats()
	after := runtime.NumGoroutine()
	if stats.RecoveryTracked != 5000 {
		cancel()
		t.Fatalf("tracked=%d want 5000", stats.RecoveryTracked)
	}
	if stats.RecoveryQueueDepth > maxRecoveryQueue {
		cancel()
		t.Fatalf("queue=%d exceeds %d", stats.RecoveryQueueDepth, maxRecoveryQueue)
	}
	if delta := after - before; delta > recoveryWorkerCount+16 {
		cancel()
		t.Fatalf("goroutine delta=%d exceeds bounded worker design", delta)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+16 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

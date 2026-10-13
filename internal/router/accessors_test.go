package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func accessorTestConfig() config.Config {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "a", Model: "a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
			{ID: "b", Model: "b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		},
	}}
	return cfg
}

func TestDeploymentLookup(t *testing.T) {
	r := New(accessorTestConfig(), health.New(4, time.Hour))

	d, ok := r.Deployment("p/a")
	if !ok {
		t.Fatal("expected deployment p/a to exist")
	}
	if d.ID != "p/a" || d.ProviderID != "p" || d.Model != "a" {
		t.Fatalf("unexpected deployment: %+v", d)
	}

	if _, ok := r.Deployment("p/missing"); ok {
		t.Fatal("expected missing deployment lookup to report not-found")
	}
	if _, ok := r.Deployment(""); ok {
		t.Fatal("expected empty id lookup to report not-found")
	}
}

func TestReadinessCounts(t *testing.T) {
	h := health.New(1, time.Hour)
	r := New(accessorTestConfig(), h)

	// No health data yet: nothing is Healthy, nothing is in Cooldown either.
	total, usable := r.Readiness("priority")
	if total != 2 {
		t.Fatalf("expected total=2, got %d", total)
	}
	if usable != 2 {
		t.Fatalf("non-ready strategy: expected usable=2 (none in cooldown), got %d", usable)
	}

	_, usableReady := r.Readiness("ready_mesh")
	if usableReady != 0 {
		t.Fatalf("ready strategy: expected usable=0 before any success, got %d", usableReady)
	}

	// One success -> healthy; one failure (threshold 1) -> cooldown.
	h.RecordSuccess("p/a", time.Millisecond)
	h.RecordFailure("p/b", "boom", time.Millisecond)

	_, usable = r.Readiness("priority")
	if usable != 1 {
		t.Fatalf("non-ready strategy: expected usable=1 (b in cooldown), got %d", usable)
	}
	_, usableReady = r.Readiness("ready_mesh")
	if usableReady != 1 {
		t.Fatalf("ready strategy: expected usable=1 (only a healthy), got %d", usableReady)
	}
	// cost_aware is also a ready strategy.
	_, usableCost := r.Readiness("cost_aware")
	if usableCost != 1 {
		t.Fatalf("cost_aware: expected usable=1, got %d", usableCost)
	}
}

func TestPinnedDeploymentIDLifecycle(t *testing.T) {
	cfg := accessorTestConfig()
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 60
	r := New(cfg, health.New(4, time.Hour))

	req := Requirement{Model: "a", SessionKey: "sess-1"}

	// No pin observed yet.
	if got := r.PinnedDeploymentID(req); got != "" {
		t.Fatalf("expected no pin before ObserveSession, got %q", got)
	}

	r.ObserveSession(req, "p/a")
	if got := r.PinnedDeploymentID(req); got != "p/a" {
		t.Fatalf("expected pin p/a, got %q", got)
	}

	// A different session has no pin.
	other := Requirement{Model: "a", SessionKey: "sess-2"}
	if got := r.PinnedDeploymentID(other); got != "" {
		t.Fatalf("expected no pin for different session, got %q", got)
	}

	// Empty session key never pins.
	noKey := Requirement{Model: "a"}
	r.ObserveSession(noKey, "p/b")
	if got := r.PinnedDeploymentID(noKey); got != "" {
		t.Fatalf("expected no pin for empty session key, got %q", got)
	}
}

func TestPinnedDeploymentIDDisabledAffinity(t *testing.T) {
	cfg := accessorTestConfig()
	cfg.Routing.SessionAffinity = false // default config enables it; disable explicitly
	r := New(cfg, health.New(4, time.Hour))

	req := Requirement{Model: "a", SessionKey: "sess-1"}
	r.ObserveSession(req, "p/a")
	if got := r.PinnedDeploymentID(req); got != "" {
		t.Fatalf("expected no pin when affinity disabled, got %q", got)
	}
}

func TestPinnedDeploymentIDExpired(t *testing.T) {
	cfg := accessorTestConfig()
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 1
	r := New(cfg, health.New(4, time.Hour))

	req := Requirement{Model: "a", SessionKey: "sess-x"}
	r.ObserveSession(req, "p/a")

	// Manually expire the pin to exercise the lazy-expiry path.
	key := r.affinityBucket(req)
	r.sessionMu.Lock()
	pin := r.sessions[key]
	pin.Expires = time.Now().Add(-time.Second)
	r.sessions[key] = pin
	r.sessionMu.Unlock()

	if got := r.PinnedDeploymentID(req); got != "" {
		t.Fatalf("expected expired pin to be dropped, got %q", got)
	}
	if n := r.SessionCount(); n != 0 {
		t.Fatalf("expected expired pin to be evicted, count=%d", n)
	}
}

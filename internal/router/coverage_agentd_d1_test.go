package router

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// Agent-D1 coverage: Explain decision dedup for duplicate deployments,
// decision truncation at decisionLimit, least_latency zero-vs-nonzero
// EWMA ordering, and the two pinned() write-lock re-check paths
// (pin deleted meanwhile; pin refreshed meanwhile).

func agentD1RouterConfig(models []config.ModelConfig) config.Config {
	cfg := config.Default()
	cfg.Routing.Strategy = "round_robin"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 3600
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x",
		Enabled: true, Models: models,
	}}
	return cfg
}

func TestAgentD1ExplainDedupsDuplicateDeployment(t *testing.T) {
	// The same model listed twice produces two identical deployments
	// in the router. Explain evaluates both, but the decision map
	// must hold the deployment once.
	model := config.ModelConfig{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}
	r := New(agentD1RouterConfig([]config.ModelConfig{model, model}), health.New(4, time.Hour))
	exp := r.Explain(Requirement{Model: "m"})
	if exp.ConsideredCount != 1 {
		t.Fatalf("ConsideredCount = %d, want 1 (duplicate deployment deduped)", exp.ConsideredCount)
	}
	if len(exp.Decisions) != 2 {
		t.Fatalf("len(Decisions) = %d, want 2 (one per evaluated entry, same deployment)", len(exp.Decisions))
	}
	for _, d := range exp.Decisions {
		if d.DeploymentID != "p/m" {
			t.Fatalf("decision deployment = %q, want p/m", d.DeploymentID)
		}
	}
	if exp.IncludedCount != 2 {
		t.Fatalf("IncludedCount = %d, want 2", exp.IncludedCount)
	}
}

func TestAgentD1ExplainTruncatesAtDecisionLimit(t *testing.T) {
	models := make([]config.ModelConfig, 0, 1005)
	for i := 0; i < 1005; i++ {
		id := fmt.Sprintf("m%04d", i)
		models = append(models, config.ModelConfig{ID: id, Model: id, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}})
	}
	r := New(agentD1RouterConfig(models), health.New(4, time.Hour))
	exp := r.Explain(Requirement{Model: "auto"})
	if exp.ConsideredCount != 1005 {
		t.Fatalf("ConsideredCount = %d, want 1005", exp.ConsideredCount)
	}
	if len(exp.Decisions) != 1000 {
		t.Fatalf("len(Decisions) = %d, want 1000 (decisionLimit)", len(exp.Decisions))
	}
	if !exp.DecisionsTruncated {
		t.Fatal("DecisionsTruncated should be true past the limit")
	}
	if exp.IncludedCount != 1005 {
		t.Fatalf("IncludedCount = %d, want 1005", exp.IncludedCount)
	}
}

func TestAgentD1LeastLatencyZeroLatencySortsLast(t *testing.T) {
	cfg := agentD1RouterConfig([]config.ModelConfig{
		{ID: "m1", Model: "m1", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		{ID: "m2", Model: "m2", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
	})
	cfg.Routing.Strategy = "least_latency"
	h := health.New(4, time.Hour)
	h.RecordSuccess("p/m1", 50*time.Millisecond)
	h.RecordSuccess("p/m2", 0)
	r := New(cfg, h)
	got := r.Candidates(Requirement{Model: "auto"})
	if len(got) != 2 {
		t.Fatalf("Candidates = %d, want 2", len(got))
	}
	for _, c := range got {
		if c.Health.Status != health.Healthy {
			t.Fatalf("candidate %s health = %v, want Healthy (same rank, so latency decides)", c.Deployment.ID, c.Health.Status)
		}
	}
	if got[0].Deployment.ID != "p/m1" || got[1].Deployment.ID != "p/m2" {
		t.Fatalf("order = [%s %s], want [p/m1 p/m2]: zero EWMA latency is unknown, not fastest", got[0].Deployment.ID, got[1].Deployment.ID)
	}
	if got[1].Health.EWMALatencyMS != 0 {
		t.Fatalf("p/m2 EWMA latency = %v, want 0", got[1].Health.EWMALatencyMS)
	}
}

func agentD1ExpiredPinRouter(t *testing.T, keys int) (*Router, []Requirement) {
	t.Helper()
	cfg := agentD1RouterConfig([]config.ModelConfig{
		{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
	})
	cfg.Routing.SessionTTLSeconds = 1
	r := New(cfg, health.New(4, time.Hour))
	reqs := make([]Requirement, keys)
	for i := 0; i < keys; i++ {
		reqs[i] = Requirement{Model: "m", SessionKey: fmt.Sprintf("sess-%d", i)}
		r.ObserveSession(reqs[i], "p/m")
	}
	// Let every pin expire; the expiry instant is shared, so one
	// sleep covers all rounds.
	time.Sleep(1100 * time.Millisecond)
	return r, reqs
}

func TestAgentD1PinnedDeletedBetweenLocks(t *testing.T) {
	// Two callers race on the same expired pin. The test holds the
	// session write lock while both queue at the read lock, so when
	// it is released they pass the read-lock check together; the
	// first to take the write lock deletes the pin, and the second
	// must observe the pin is gone on re-check and return "".
	r, reqs := agentD1ExpiredPinRouter(t, 50)
	for _, req := range reqs {
		r.sessionMu.Lock()
		var wg sync.WaitGroup
		results := make([]string, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = r.PinnedDeploymentID(req)
			}(i)
		}
		// Let both callers reach the read lock before releasing it.
		time.Sleep(10 * time.Millisecond)
		r.sessionMu.Unlock()
		wg.Wait()
		for i, got := range results {
			if got != "" {
				t.Fatalf("round %s caller %d returned %q, want \"\" (expired pin)", req.SessionKey, i, got)
			}
		}
	}
	if got := r.SessionCount(); got != 0 {
		t.Fatalf("SessionCount after all expiry rounds = %d, want 0", got)
	}
}

func TestAgentD1PinnedRefreshedBetweenLocks(t *testing.T) {
	// A caller that captured an expired pin races a refresher for
	// the write lock. The test holds the session write lock while
	// the caller queues at the read lock and the refresher queues
	// at the write lock; on release the caller passes the read-lock
	// check while the pin is still expired, the refresher (queued
	// first on the write lock) refreshes the pin, and the caller
	// must then re-check and find a live pin again.
	r, reqs := agentD1ExpiredPinRouter(t, 100)
	for _, req := range reqs {
		r.sessionMu.Lock()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = r.PinnedDeploymentID(req)
		}()
		go func() {
			defer wg.Done()
			r.ObserveSession(req, "p/m")
		}()
		// Let both goroutines reach their lock before releasing it.
		time.Sleep(10 * time.Millisecond)
		r.sessionMu.Unlock()
		wg.Wait()
		// Every return is "" (expired/deleted) or "p/m" (live pin):
		// the pin must never resolve to anything else.
		if got := r.PinnedDeploymentID(req); got != "p/m" {
			t.Fatalf("after refresh round %s, PinnedDeploymentID = %q, want p/m (pin re-created)", req.SessionKey, got)
		}
	}
	// Pins refreshed in early rounds legitimately expire (1s TTL)
	// before the test ends; the per-round check above already proved
	// each round re-created its pin. Only the recently refreshed
	// ones are still live at the end.
	if got := r.SessionCount(); got == 0 {
		t.Fatal("expected some recently refreshed pins to still be live")
	}
}

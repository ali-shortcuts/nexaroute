package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Engine with an upstream that records probe order (model names, in call
// order). Concurrency 1 serializes probes in job order, so the recorded
// order is exactly the sweep's sort order.
func wave4OrderEngine(t *testing.T, order *[]string, mu *sync.Mutex, modelIDs ...string) (*Engine, *health.Manager, *events.Bus) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*order = append(*order, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.Concurrency = 1
	cfg.Probe.TimeoutMS = 1000
	pc := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true}
	for _, id := range modelIDs {
		pc.Models = append(pc.Models, config.ModelConfig{ID: id, Model: id, Enabled: true, Weight: 1})
	}
	cfg.Providers = []config.ProviderConfig{pc}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(80)
	return New(cfg, reg, rt, hm, bus), hm, bus
}

// Sweep sort order is rank, then zero-LastChecked first within a rank:
// HalfOpen (rank 0) first, then Unknown (rank 1), then the rank-3 pair
// where the never-checked ""-status deployment precedes the checked
// Healthy one. The "" status comes from SetNextRetry on a fresh id,
// which records no status; its rank falls to the sort's default.
func TestWave4RunOnceProbeRankOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	e, hm, _ := wave4OrderEngine(t, &order, &mu, "half", "unknown", "weird", "healthy")
	hm.RecordProbeFailure("p/half", "flap", "timeout", time.Millisecond)
	hm.RecordRecoverySuccess("p/half", time.Millisecond)
	if hm.Get("p/half").Status != health.HalfOpen {
		t.Fatalf("half status = %s, want half_open", hm.Get("p/half").Status)
	}
	hm.SetNextRetry("p/weird", time.Now().Add(time.Hour))
	if st := hm.Get("p/weird"); st.Status != "" || !st.LastChecked.IsZero() {
		t.Fatalf("weird state = %+v, want empty status and zero LastChecked", st)
	}
	hm.RecordSuccess("p/healthy", time.Millisecond)
	res := e.RunOnce(context.Background())
	if res.Total != 4 || res.Passed != 4 {
		t.Fatalf("result = %+v, want total 4 passed 4", res)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"half", "unknown", "weird", "healthy"}
	if len(order) != len(want) {
		t.Fatalf("probe order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("probe order = %v, want %v", order, want)
		}
	}
}

// With a forced sweep, a cooldown deployment is counted as skipped
// cooldown without starting recovery (the Recover call belongs to the
// unforced ready-supervisor path only).
func TestWave4RunOnceForceCooldownSkipped(t *testing.T) {
	var mu sync.Mutex
	var order []string
	e, hm, bus := wave4OrderEngine(t, &order, &mu, "cool")
	hm.EnterCooldown("p/cool", "provider quota", time.Hour)
	res := e.RunOnce(context.Background())
	if res.Total != 1 || res.SkippedCooldown != 1 || res.Passed != 0 {
		t.Fatalf("result = %+v, want total 1 skipped-cooldown 1 passed 0", res)
	}
	for _, ev := range bus.Snapshot() {
		if strings.HasPrefix(ev.Kind, "recovery_") {
			t.Fatalf("forced cooldown skip must not start recovery, got event %s", ev.Kind)
		}
	}
}

// A deployment already marked recovering is skipped (not re-probed) by
// an unforced ready-supervisor sweep.
func TestWave4RunOnceSkipsRecovering(t *testing.T) {
	e, _, _, _ := coverageFixture(t, http.StatusOK, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`, "ready_queue", "rec")
	e.recoveryMu.Lock()
	e.recovering["p/rec"] = true
	e.recoveryMu.Unlock()
	res := e.runOnce(context.Background(), false)
	if res.SkippedRecovery != 1 || res.Passed != 0 {
		t.Fatalf("result = %+v, want skipped-recovery 1 passed 0", res)
	}
}

func wave4BusMessage(t *testing.T, bus *events.Bus, kind, substr string) string {
	t.Helper()
	for _, ev := range bus.Snapshot() {
		if ev.Kind == kind && strings.Contains(ev.Message, substr) {
			return ev.Message
		}
	}
	t.Fatalf("no %s event containing %q in bus snapshot", kind, substr)
	return ""
}

func wave4DegradedRecovery(t *testing.T, status int, body string) (*Engine, *health.Manager, *events.Bus) {
	t.Helper()
	e, hm, _, bus := coverageFixture(t, status, body, "ready_queue", "m")
	hm.RecordProbeFailure("p/m", "flap", "timeout", time.Millisecond)
	return e, hm, bus
}

// Out-of-range attempt/success counters are clamped before the probe so
// the recovery messages report the bounded values, not the caller's
// numbers: attempt 0 becomes 1, attempt 999 becomes the attempt budget,
// successes -5 becomes 0, successes 999 becomes the threshold.
func TestWave4RecoveryCounterClamps(t *testing.T) {
	good := `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`
	t.Run("attempt below one clamps to one", func(t *testing.T) {
		e, _, bus := wave4DegradedRecovery(t, http.StatusOK, good)
		task := recoveryTask{id: "p/m", attempt: 0, successes: 0, generation: 1}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		e.processRecoveryTask(context.Background(), task)
		msg := wave4BusMessage(t, bus, "recovery_ready", "attempt 1/")
		if !strings.Contains(msg, "1/1 successful probes") {
			t.Fatalf("message = %q, want attempt 1 with 1/1 successes", msg)
		}
	})
	t.Run("attempt above budget clamps to budget", func(t *testing.T) {
		e, _, bus := wave4DegradedRecovery(t, http.StatusInternalServerError, `{"error":"boom"}`)
		defer e.cancelAllRecoveries()
		task := recoveryTask{id: "p/m", attempt: 999, successes: 0, generation: 1}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		e.processRecoveryTask(context.Background(), task)
		wave4BusMessage(t, bus, "recovery_fail", "attempt 5/5")
	})
	t.Run("successes below zero clamps to zero", func(t *testing.T) {
		e, _, bus := wave4DegradedRecovery(t, http.StatusOK, good)
		task := recoveryTask{id: "p/m", attempt: 1, successes: -5, generation: 1}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		e.processRecoveryTask(context.Background(), task)
		wave4BusMessage(t, bus, "recovery_ready", "1/1 successful probes")
	})
	t.Run("successes above threshold clamps to threshold", func(t *testing.T) {
		e, _, bus := wave4DegradedRecovery(t, http.StatusOK, good)
		task := recoveryTask{id: "p/m", attempt: 1, successes: 999, generation: 1}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		e.processRecoveryTask(context.Background(), task)
		wave4BusMessage(t, bus, "recovery_ready", "2/1 successful probes")
	})
}

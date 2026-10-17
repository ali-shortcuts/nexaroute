package httpapi

// Regression + benchmark coverage for audit #371 finding C1 (issue #433):
// per-request full config deep-clones replaced by an immutable,
// atomically-published config snapshot with a pre-built price book.

import (
	"io"
	"log"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/pricing"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// snapshotTestConfig builds a realistically-shaped config: several providers
// with models/aliases/headers, virtual keys, a tenant hierarchy and a price
// book version, so clone costs are representative.
func snapshotTestConfig() config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.RPM = 60
	for i := 0; i < 4; i++ {
		id := string(rune('a' + i))
		p := config.ProviderConfig{
			ID: id, Name: "P" + id, Type: "openai_compatible",
			BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
			Headers: map[string]string{"X-Custom": "v"},
		}
		for j := 0; j < 4; j++ {
			mid := id + "-m" + string(rune('0'+j))
			p.Models = append(p.Models, config.ModelConfig{
				ID: mid, Model: "model-" + mid, Enabled: true, Weight: 1,
				Aliases:           []string{"alias-" + mid},
				InputCostPerMTok:  10,
				OutputCostPerMTok: 30,
			})
		}
		cfg.Providers = append(cfg.Providers, p)
	}
	for i := 0; i < 6; i++ {
		cfg.ClientAuth.VirtualKeys = append(cfg.ClientAuth.VirtualKeys, config.VirtualKeyConfig{
			ID: "vk-" + string(rune('0'+i)), KeyHash: keyDigest("key-" + string(rune('0'+i))),
		})
	}
	cfg.ClientAuth.Tenants = []config.TenantConfig{{
		ID: "t1",
		Projects: []config.ProjectConfig{{
			ID: "p1",
			Teams: []config.TeamConfig{{
				ID: "tm1", AllowedModels: []string{"a-m0"}, AllowedRoutes: []string{"/v1/chat/completions"},
			}},
			AllowedModels: []string{"a-m0", "b-m1"},
		}},
	}}
	cfg.Pricing.Versions = []config.PriceVersionConfig{{
		Version: "2026-q4", EffectiveFrom: "2020-01-01T00:00:00Z",
		Prices: map[string]config.PriceEntryConfig{
			"a/a-m0": {InputPerMTok: 5, OutputPerMTok: 30},
		},
	}}
	return cfg
}

func TestConfigSnapshotPublishedAtStartup(t *testing.T) {
	s := testGateway(t, snapshotTestConfig())
	snap := s.configSnapshot()
	if snap == nil {
		t.Fatal("configSnapshot() returned nil")
	}
	if snap.book == nil {
		t.Fatal("snapshot price book not built")
	}
	if got := s.configSnapshot(); got != snap {
		t.Fatal("configSnapshot() not stable: expected same pointer across calls")
	}
	if len(snap.cfg.Providers) != 4 {
		t.Fatalf("snapshot providers = %d, want 4", len(snap.cfg.Providers))
	}
}

func TestConfigSnapshotCoherentUnderReload(t *testing.T) {
	cfg := snapshotTestConfig()
	s := testGateway(t, cfg)
	before := s.configSnapshot()

	cfg2 := snapshotTestConfig()
	cfg2.ClientAuth.RPM = 600
	if err := s.applyConfigLocked(cfg2); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := s.configSnapshot()

	if before == after {
		t.Fatal("reload did not publish a new snapshot (pointer unchanged)")
	}
	// In-flight readers holding the old snapshot must keep seeing the old
	// config: decisions taken under it stay bit-identical.
	if before.cfg.ClientAuth.RPM != 60 {
		t.Fatalf("old snapshot RPM = %d, want 60 (in-flight coherence broken)", before.cfg.ClientAuth.RPM)
	}
	if after.cfg.ClientAuth.RPM != 600 {
		t.Fatalf("new snapshot RPM = %d, want 600", after.cfg.ClientAuth.RPM)
	}
	if before.book == nil || after.book == nil {
		t.Fatal("snapshot price book missing")
	}
	if before.book == after.book {
		t.Fatal("price book not rebuilt for the new snapshot")
	}
	// Same admission decision under both snapshots for unchanged inputs.
	id := clientIdentity{ID: "legacy", Role: "admin"}
	if !identityPolicyAllows(before.cfg, id, config.VirtualKeyConfig{}, "/v1/chat/completions", "a-m0") {
		t.Fatal("admission decision changed under old snapshot")
	}
	if !identityPolicyAllows(after.cfg, id, config.VirtualKeyConfig{}, "/v1/chat/completions", "a-m0") {
		t.Fatal("admission decision changed under new snapshot")
	}
}

func TestCurrentConfigStillIsolatesMutations(t *testing.T) {
	s := testGateway(t, snapshotTestConfig())
	c := s.currentConfig()
	// Mutate the clone's nested state the way a careless handler might.
	c.Providers[0].Models[0].Aliases[0] = "MUTATED"
	c.Providers[0].Headers["X-Custom"] = "MUTATED"
	c.ClientAuth.VirtualKeys[0].ID = "MUTATED"
	c.ClientAuth.Tenants[0].Projects[0].Teams[0].AllowedModels[0] = "MUTATED"

	live := s.configSnapshot().cfg
	if live.Providers[0].Models[0].Aliases[0] == "MUTATED" ||
		live.Providers[0].Headers["X-Custom"] == "MUTATED" ||
		live.ClientAuth.VirtualKeys[0].ID == "MUTATED" ||
		live.ClientAuth.Tenants[0].Projects[0].Teams[0].AllowedModels[0] == "MUTATED" {
		t.Fatal("mutation of currentConfig() result leaked into the live config")
	}
}

// TestUsageSnapshotWithPricesHonorsArgument is the behavioral red/green for
// C1: the old implementation ignored its argument and re-cloned the live
// config, so pricing under an explicit config was impossible. On the old
// code this test FAILS (assertions execute against live-config prices); on
// the fixed code it passes.
func TestUsageSnapshotWithPricesHonorsArgument(t *testing.T) {
	s := testGateway(t, snapshotTestConfig())
	// Record unpriced tokens so the snapshot-time price table decides the
	// estimated cost.
	s.usage.Record("a/a-m0", 1000000, 1000000)

	other := snapshotTestConfig()
	other.Providers[0].Models[0].InputCostPerMTok = 1234
	other.Providers[0].Models[0].OutputCostPerMTok = 5678

	snap := s.usageSnapshotWithPrices(other)
	var got float64
	found := false
	for _, row := range snap.ByDeployment {
		if row.Deployment == "a/a-m0" {
			found = true
			got = row.EstimatedCost
		}
	}
	if !found {
		t.Fatal(`deployment "a/a-m0" missing from snapshot`)
	}
	// (1e6*$1234 + 1e6*$5678)/1e6 = $6912 under `other`; the live config
	// would price it at $40. The old code ignored the argument (red: 40).
	if got != 6912 {
		t.Fatalf("EstimatedCost = %v, want 6912 (priced from the argument config)", got)
	}
}

// TestHotPathsDoNotMutateSnapshot fingerprints the live snapshot, runs every
// migrated hot path, and asserts the shared config is bit-identical after.
// This is the regression guard for the C1 read-only contract: if any
// migrated path ever mutates the snapshot, this fails.
func TestHotPathsDoNotMutateSnapshot(t *testing.T) {
	s := testGateway(t, snapshotTestConfig())
	snap := s.configSnapshot()
	before := cloneConfig(snap.cfg)

	// Admission path.
	enabled, _, _ := s.clientAuthConfig()
	_ = enabled
	_, _, _ = s.findClientIdentity("no-such-key")
	// Cache wiring.
	r := httptest.NewRequest("POST", "http://gateway/v1/chat/completions", nil)
	_, _ = s.cacheLookupFor(r, "/v1/chat/completions", []byte("{}"), false, nil, nil, nil, 1)
	// Usage recording + pricing.
	s.recordUsageEx(r, "a/a-m0", usage.Counts{Input: 10, Output: 5}, false)
	_, _ = s.recordCost(snap, "a/a-m0", 10, 5, 0, 0, false)
	_, _ = s.budgetEstimateFor(snap, "a-m0", 100, 100)
	// Routing helpers.
	_ = s.dialectFor("a", "openai_compatible", "")
	_, _ = s.providerConfigFor("a")
	_ = s.sanitizeOutgoingPayload(hedgeAttemptBundle{}, []byte("{}"), router.Requirement{})
	// Metrics path.
	_ = s.usageSnapshotWithPrices(snap.cfg)

	if !reflect.DeepEqual(before, s.configSnapshot().cfg) {
		t.Fatal("a migrated hot path mutated the shared config snapshot")
	}
	if s.configSnapshot() != snap {
		t.Fatal("snapshot pointer changed without a reload")
	}
}

func TestAdmissionReusesSnapshotPriceBook(t *testing.T) {
	s := testGateway(t, snapshotTestConfig())
	snap := s.configSnapshot()
	if snap.book == nil {
		t.Fatal("snapshot book nil")
	}
	// The book is built once per snapshot, not per admission.
	for i := 0; i < 3; i++ {
		if got := s.configSnapshot(); got != snap || got.book != snap.book {
			t.Fatal("snapshot or price book rebuilt between admissions")
		}
	}
	est1, ok1 := s.budgetEstimateFor(snap, "a-m0", 100, 100)
	est2, ok2 := s.budgetEstimateFor(snap, "a-m0", 100, 100)
	if !ok1 || !ok2 || est1 != est2 {
		t.Fatalf("budgetEstimateFor not deterministic: (%v,%v) vs (%v,%v)", est1, ok1, est2, ok2)
	}
	if est1 <= 0 {
		t.Fatalf("budgetEstimateFor estimate = %v, want > 0", est1)
	}
}

// TestConfigSnapshotConcurrentReadDuringReload hammers the snapshot accessor
// and every migrated hot path while reloads publish new snapshots. Run with
// -race: any torn read or mutation of shared state fails the run.
func TestConfigSnapshotConcurrentReadDuringReload(t *testing.T) {
	s := testGateway(t, snapshotTestConfig())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				snap := s.configSnapshot()
				if snap == nil || snap.book == nil {
					t.Error("nil snapshot or book under concurrency")
					return
				}
				_, _, _ = s.clientAuthConfig()
				_, _, _ = s.findClientIdentity("k")
				_, _ = s.providerConfigFor("a")
				_, _ = s.budgetEstimateFor(snap, "a-m0", 10, 10)
				_, _ = s.recordCost(snap, "a/a-m0", 10, 5, 0, 0, false)
				_ = snap.cfg.ClientAuth.RPM
			}
		}()
	}
	for i := 0; i < 3; i++ {
		cfg := snapshotTestConfig()
		cfg.ClientAuth.RPM = 60 + i
		if err := s.applyConfigLocked(cfg); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	wg.Wait()
}

// BenchmarkAdmissionConfigAccess compares the exact config-access sequence
// the admission path performed before C1 (4 deep clones + a price-book
// rebuild per request) against the snapshot sequence (4 atomic loads,
// book reused).
func BenchmarkAdmissionConfigAccess(b *testing.B) {
	s := newBenchServer(b, snapshotTestConfig())
	cfg := snapshotTestConfig()

	b.Run("before/deep-clone-per-request", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c1 := s.currentConfig() // clientAuthConfig
			_ = c1
			c2 := s.currentConfig() // findClientIdentity
			_ = c2
			c3 := s.currentConfig() // applyGuardrails
			_ = c3
			c4 := s.currentConfig() // clientAuthAllowed body
			_ = c4
			_ = pricing.FromConfig(cfg) // budgetEstimateFor book rebuild
		}
	})

	b.Run("after/snapshot-load-per-request", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			snap := s.configSnapshot() // clientAuthConfig
			_ = snap.cfg.ClientAuth.Enabled
			snap2 := s.configSnapshot() // findClientIdentity
			_ = len(snap2.cfg.ClientAuth.VirtualKeys)
			snap3 := s.configSnapshot() // applyGuardrails
			_ = snap3.cfg.Guardrails.Mode
			snap4 := s.configSnapshot() // clientAuthAllowed body
			_ = snap4.cfg.ClientAuth.TPM
			_ = snap4.book // budgetEstimateFor reuses the book
		}
	})
}

// newBenchServer mirrors testGateway for benchmarks (testing.TB).
func newBenchServer(tb testing.TB, cfg config.Config) *Server {
	tb.Helper()
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	return New(cfg, "", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
}

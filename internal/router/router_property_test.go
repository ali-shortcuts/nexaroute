package router

// Wave C-7 (#408) supervisor-takeover property tests: router mathematics.
//
// Mechanism is stdlib testing/quick with fixed seeds (fully reproducible;
// no new dependencies). Every property runs in the default test pass;
// case counts come from the -quickchecks flag (default 10000). Scale up:
//
//	go test ./internal/router/ -run 'TestProperty_Router' -quickchecks=100000
//
// Properties (over randomized deployment sets and request streams):
//  P4 TestProperty_RouterDeterminism — same inputs, same choice, always:
//     two Candidates calls on identical state return identical orderings,
//     for every strategy. The counter-rotated strategies
//     (round_robin, adaptive_round_robin) are stateful by design, so for
//     them the property is sequence-determinism across two fresh routers
//     (documented divergence from call-determinism, pinned here).
//  P5 TestProperty_RouterMonotonicity — raising a healthy deployment's
//     weight never lowers its selection share over a fixed 10k-key stream
//     (ready_mesh: share(rank) = 2(w-1-rank)/(w(w-1)) is strictly decreasing
//     in rank, and a weight raise weakly improves rank at fixed window).
//  P6 TestProperty_RouterAffinityStability — removing one deployment
//     remaps at most its former share + 5pp of 10k session keys.
//  P7 TestProperty_RouterNoStarvation — ready_mesh: every deployment that
//     can win a pairwise `better` comparison against some other window
//     member is selected at least once over a 10k-key stream. Precise
//     exception: the deployment ranked last in the P2C window loses every
//     pairwise comparison, so its selection share is exactly 0 by
//     construction (documented, pinned). round_robin: every deployment in
//     the top health tier is selected within each window of w calls
//     (unconditional). Score-sorted strategies intentionally always pick
//     the argmax — starvation-freedom does not apply to them by design.

import (
	"flag"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"testing"
	"testing/quick"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func init() {
	// testing/quick defines the -quickchecks flag itself (default 100).
	// It is the scale-up control for these properties; raise its default
	// to the brief's 10k floor. This runs before testing's flag.Parse, so
	// an explicit -quickchecks=N on the command line still wins.
	_ = flag.Set("quickchecks", "10000")
}

// propQuickChecks returns the effective -quickchecks value.
func propQuickChecks() int {
	if f := flag.Lookup("quickchecks"); f != nil {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 0 {
			return n
		}
	}
	return 10000
}

// quickConfig returns a deterministic quick.Config with an explicit case
// count: the seeded Rand makes every run reproduce the identical case
// sequence.
func quickConfig(n int, seed int64) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// propSeedRouter is the fixed base seed.
const propSeedRouter = int64(0x408C7)

type propWorld struct {
	cfg config.Config
	r   *Router
	ids []string
}

// buildPropWorld builds a router over one provider with n models.
// Weights are 1..5, priorities random 0..2 unless flatPriority forces 0
// (needed when the P2C window must cover the whole set).
func buildPropWorld(rnd *rand.Rand, strategy string, n int, flatPriority bool) *propWorld {
	cfg := config.Default()
	cfg.Routing.Strategy = strategy
	cfg.Routing.SessionAffinity = false
	models := make([]config.ModelConfig, n)
	for i := range models {
		pri := rnd.Intn(3)
		if flatPriority {
			pri = 0
		}
		models[i] = config.ModelConfig{
			ID:       fmt.Sprintf("m%d", i),
			Model:    fmt.Sprintf("model-%d", i),
			Enabled:  true,
			Weight:   float64(1 + rnd.Intn(5)),
			Priority: pri,
			Capabilities: config.Capabilities{
				Tools: true, Vision: true, Streaming: true, Reasoning: true,
			},
		}
	}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible",
		BaseURL: "https://example.invalid", Enabled: true, Models: models,
	}}
	hm := health.New(4, time.Hour)
	r := New(cfg, hm)
	ids := make([]string, 0, n)
	for _, d := range r.All() {
		hm.RecordSuccess(d.ID, 0)
		ids = append(ids, d.ID)
	}
	return &propWorld{cfg: cfg, r: r, ids: ids}
}

func propRequirement(rnd *rand.Rand) Requirement {
	return Requirement{
		Model:                "auto",
		SelectionKey:         fmt.Sprintf("sel-%d", rnd.Int63()),
		SessionKey:           fmt.Sprintf("sess-%d", rnd.Int63()),
		EstimatedInputTokens: 100,
		MaxOutputTokens:      50,
	}
}

func scoredIDs(out []Scored) []string {
	ids := make([]string, len(out))
	for i, s := range out {
		ids[i] = s.Deployment.ID
	}
	return ids
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var propStrategies = []string{
	"ready_mesh", "cost_aware", "ready_queue", "priority",
	"least_latency", "round_robin", "adaptive_round_robin", "default",
}

func isStatefulStrategy(s string) bool {
	return s == "round_robin" || s == "adaptive_round_robin"
}

type routerDetCase struct {
	seed int64
}

func (routerDetCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(routerDetCase{seed: r.Int63()})
}

func TestProperty_RouterDeterminism(t *testing.T) {
	n := propQuickChecks()
	perStrategy := n / len(propStrategies)
	for _, strategy := range propStrategies {
		var firstFail string
		idx := -1
		err := quick.Check(func(c routerDetCase) bool {
			idx++
			build := func() (*propWorld, Requirement) {
				rnd := rand.New(rand.NewSource(c.seed))
				w := buildPropWorld(rnd, strategy, 2+rnd.Intn(5), false)
				return w, propRequirement(rnd)
			}
			if isStatefulStrategy(strategy) {
				// Counter-rotated by design: the property is that a fresh
				// router replays the identical call sequence.
				wa, reqA := build()
				wb, reqB := build()
				for k := 0; k < 10; k++ {
					a, b := wa.r.Candidates(reqA), wb.r.Candidates(reqB)
					if !equalIDs(scoredIDs(a), scoredIDs(b)) {
						if firstFail == "" {
							firstFail = fmt.Sprintf("strategy %s case %d call %d: fresh-router sequences diverged: %v vs %v",
								strategy, idx, k, scoredIDs(a), scoredIDs(b))
						}
						return false
					}
				}
				return true
			}
			rnd := rand.New(rand.NewSource(c.seed))
			w := buildPropWorld(rnd, strategy, 2+rnd.Intn(5), false)
			req := propRequirement(rnd)
			first, second := w.r.Candidates(req), w.r.Candidates(req)
			if !equalIDs(scoredIDs(first), scoredIDs(second)) {
				if firstFail == "" {
					firstFail = fmt.Sprintf("strategy %s case %d: repeated Candidates diverged: %v vs %v",
						strategy, idx, scoredIDs(first), scoredIDs(second))
				}
				return false
			}
			return true
		}, quickConfig(perStrategy, propSeedRouter))
		if err != nil {
			t.Fatalf("P4 determinism property failed for strategy %s: %v\n%s", strategy, err, firstFail)
		}
	}
	t.Logf("P4 determinism: %d generated (strategy, config, requirement) cases across %d strategies", perStrategy*len(propStrategies), len(propStrategies))
}

// selectionShare returns the fraction of keys for which deployment id is
// ranked first.
func selectionShare(r *Router, id string, keys []string) float64 {
	hit := 0
	for _, k := range keys {
		out := r.Candidates(Requirement{Model: "auto", SelectionKey: k})
		if len(out) > 0 && out[0].Deployment.ID == id {
			hit++
		}
	}
	return float64(hit) / float64(len(keys))
}

type routerMonoCase struct {
	seed int64
}

func (routerMonoCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(routerMonoCase{seed: r.Int63()})
}

func TestProperty_RouterMonotonicity(t *testing.T) {
	nKeys := propQuickChecks()
	nConfigs := 6
	var firstFail string
	idx := -1
	err := quick.Check(func(c routerMonoCase) bool {
		idx++
		rnd := rand.New(rand.NewSource(c.seed))
		n := 3 + rnd.Intn(5) // 3..7, under the default P2CWindow of 8
		w := buildPropWorld(rnd, "ready_mesh", n, true)

		probe := w.r.Candidates(Requirement{Model: "auto", SelectionKey: "probe"})
		if len(probe) != n {
			if firstFail == "" {
				firstFail = fmt.Sprintf("config %d: expected %d candidates, got %d", idx, n, len(probe))
			}
			return false
		}
		// Choose d = middle of the score order (never last-ranked for n>=3,
		// so its pre-raise share is provably > 0).
		dID := probe[n/2].Deployment.ID

		keys := make([]string, nKeys)
		for i := range keys {
			keys[i] = fmt.Sprintf("cfg%d-key%d", idx, i)
		}
		before := selectionShare(w.r, dID, keys)
		if before <= 0 {
			if firstFail == "" {
				firstFail = fmt.Sprintf("config %d: invalid case, deployment %s never selected before raise", idx, dID)
			}
			return false
		}

		// Raise d's weight; everything else fixed.
		cfg2 := w.cfg
		for pi := range cfg2.Providers {
			for mi := range cfg2.Providers[pi].Models {
				if cfg2.Providers[pi].ID+"/"+cfg2.Providers[pi].Models[mi].ID == dID {
					cfg2.Providers[pi].Models[mi].Weight += 3
				}
			}
		}
		w.r.Reload(cfg2)

		after := selectionShare(w.r, dID, keys)
		if after < before {
			if firstFail == "" {
				firstFail = fmt.Sprintf("config %d: monotonicity violated for %s: share %f -> %f after weight raise",
					idx, dID, before, after)
			}
			return false
		}
		return true
	}, quickConfig(nConfigs, propSeedRouter+1))
	if err != nil {
		t.Fatalf("P5 monotonicity property failed: %v\n%s", err, firstFail)
	}
	t.Logf("P5 monotonicity: %d configs x %d-key streams (%d generated selection cases), share never decreased on weight raise",
		nConfigs, nKeys, nConfigs*nKeys*2)
}

type routerAffCase struct {
	seed int64
}

func (routerAffCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(routerAffCase{seed: r.Int63()})
}

func TestProperty_RouterAffinityStability(t *testing.T) {
	nKeys := propQuickChecks()
	nExp := 5
	var firstFail string
	idx := -1
	err := quick.Check(func(c routerAffCase) bool {
		idx++
		rnd := rand.New(rand.NewSource(c.seed))
		w := buildPropWorld(rnd, "ready_mesh", 4, true) // flat priority: shared traffic, non-degenerate shares
		w.cfg.Routing.SessionAffinity = true
		w.cfg.Routing.SessionTTLSeconds = 3600
		w.r.Reload(w.cfg)

		keys := make([]string, nKeys)
		for i := range keys {
			keys[i] = fmt.Sprintf("exp%d-sess-%d", idx, i)
		}
		first := make([]string, nKeys)
		for i, k := range keys {
			req := Requirement{Model: "auto", SessionKey: k}
			out := w.r.Candidates(req)
			if len(out) == 0 {
				if firstFail == "" {
					firstFail = fmt.Sprintf("exp %d: no candidates", idx)
				}
				return false
			}
			first[i] = out[0].Deployment.ID
			w.r.ObserveSession(req, first[i])
		}
		// Remove the deployment with the largest share.
		counts := map[string]int{}
		for _, id := range first {
			counts[id]++
		}
		removed := ""
		for id, cnt := range counts {
			if removed == "" || cnt > counts[removed] {
				removed = id
			}
		}
		removedShare := float64(counts[removed]) / float64(nKeys)

		cfg2 := w.cfg
		models := cfg2.Providers[0].Models[:0:0]
		for _, m := range cfg2.Providers[0].Models {
			if cfg2.Providers[0].ID+"/"+m.ID != removed {
				models = append(models, m)
			}
		}
		cfg2.Providers[0].Models = models
		w.r.Reload(cfg2)

		remapped := 0
		for i, k := range keys {
			out := w.r.Candidates(Requirement{Model: "auto", SessionKey: k})
			if len(out) == 0 {
				if firstFail == "" {
					firstFail = fmt.Sprintf("exp %d: no candidates after removal", idx)
				}
				return false
			}
			if out[0].Deployment.ID != first[i] {
				remapped++
			}
		}
		remappedFrac := float64(remapped) / float64(nKeys)
		if remappedFrac > removedShare+0.05 {
			if firstFail == "" {
				firstFail = fmt.Sprintf("exp %d: affinity instability: removed %s (share %.4f) remapped %.4f (> share+5pp)",
					idx, removed, removedShare, remappedFrac)
			}
			return false
		}
		return true
	}, quickConfig(nExp, propSeedRouter+2))
	if err != nil {
		t.Fatalf("P6 affinity-stability property failed: %v\n%s", err, firstFail)
	}
	t.Logf("P6 affinity-stability: %d experiments x %d session keys (%d generated key cases), remap <= removed share + 5pp",
		nExp, nKeys, nExp*nKeys)
}

type routerStarveCase struct {
	seed int64
}

func (routerStarveCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(routerStarveCase{seed: r.Int63()})
}

func TestProperty_RouterNoStarvation(t *testing.T) {
	nKeys := propQuickChecks()
	nSets := 10
	var firstFail string
	idx := -1
	err := quick.Check(func(c routerStarveCase) bool {
		idx++
		rnd := rand.New(rand.NewSource(c.seed))
		n := 3 + rnd.Intn(4) // 3..6
		w := buildPropWorld(rnd, "ready_mesh", n, true)
		probe := w.r.Candidates(Requirement{Model: "auto", SelectionKey: "probe"})
		if len(probe) != n {
			if firstFail == "" {
				firstFail = fmt.Sprintf("set %d: expected %d candidates, got %d", idx, n, len(probe))
			}
			return false
		}
		// Precise exception: a deployment that loses every pairwise
		// `better` comparison in the window (ranked last) has selection
		// share exactly 0 by construction of P2C.
		excepted := map[string]bool{}
		for _, a := range probe {
			losesAll := true
			for _, b := range probe {
				if a.Deployment.ID == b.Deployment.ID {
					continue
				}
				if better(a, b) {
					losesAll = false
					break
				}
			}
			if losesAll {
				excepted[a.Deployment.ID] = true
			}
		}
		seen := map[string]bool{}
		for i := 0; i < nKeys; i++ {
			out := w.r.Candidates(Requirement{Model: "auto", SelectionKey: fmt.Sprintf("set%d-key%d", idx, i)})
			if len(out) > 0 {
				seen[out[0].Deployment.ID] = true
			}
		}
		for _, d := range probe {
			id := d.Deployment.ID
			if excepted[id] {
				continue
			}
			if !seen[id] {
				if firstFail == "" {
					firstFail = fmt.Sprintf("set %d: deployment %s (not excepted) starved over %d keys", idx, id, nKeys)
				}
				return false
			}
		}
		return true
	}, quickConfig(nSets, propSeedRouter+3))
	if err != nil {
		t.Fatalf("P7 no-starvation (ready_mesh) property failed: %v\n%s", err, firstFail)
	}

	// round_robin: unconditional — every deployment in the top health tier
	// is selected within each window of w consecutive calls.
	idx = -1
	firstFail = ""
	err = quick.Check(func(c routerStarveCase) bool {
		idx++
		rnd := rand.New(rand.NewSource(c.seed))
		n := 2 + rnd.Intn(4)
		w := buildPropWorld(rnd, "round_robin", n, true)
		req := Requirement{Model: "auto"}
		seen := map[string]bool{}
		for i := 0; i < n; i++ {
			out := w.r.Candidates(req)
			if len(out) != n {
				if firstFail == "" {
					firstFail = fmt.Sprintf("rr set %d: expected %d candidates, got %d", idx, n, len(out))
				}
				return false
			}
			seen[out[0].Deployment.ID] = true
		}
		if len(seen) != n {
			if firstFail == "" {
				firstFail = fmt.Sprintf("rr set %d: rotation incomplete over %d calls: %v", idx, n, seen)
			}
			return false
		}
		return true
	}, quickConfig(nSets, propSeedRouter+4))
	if err != nil {
		t.Fatalf("P7 no-starvation (round_robin) property failed: %v\n%s", err, firstFail)
	}
	t.Logf("P7 no-starvation: %d ready_mesh sets x %d-key streams (%d generated selection cases) + %d round_robin rotation windows",
		nSets, nKeys, nSets*nKeys, nSets)
}

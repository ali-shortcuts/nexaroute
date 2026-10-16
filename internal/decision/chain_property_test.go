package decision

// Wave C-7 (#408) supervisor-takeover property tests: decision-chain
// deadline and budget invariants.
//
// Mechanism is stdlib testing/quick with fixed seeds (fully reproducible;
// no new dependencies). The property runs in the default test pass with
// -quickchecks generated step graphs (default 10000); scale up:
//
//	go test ./internal/decision/ -run 'TestProperty_Chain' -quickchecks=100000
//
// Property P8/P9 (over randomized step graphs and budgets):
//  - the chain never exceeds its global deadline by more than one
//    in-flight step's grace (elapsed <= timeout + maxStepTimeout + slack);
//  - a step's child context deadline never exceeds the chain deadline
//    (child budget <= remaining global budget — a step derived from a fresh
//    budget instead of the remaining global would violate this immediately);
//  - total provider invocations never exceed the call budget.
//
// Placement note: issue #404's PR #441 reworked chain_test.go's deadline
// determinism harnesses (per-step remaining-time inspection). This file
// does NOT duplicate that work: it asserts the end-to-end,
// graph-randomized deadline/budget invariants instead.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"testing/quick"
	"time"
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

// quickConfig returns a deterministic quick.Config: the seeded Rand makes
// every run reproduce the identical case sequence.
func quickConfig(seed int64) *quick.Config {
	return &quick.Config{MaxCount: propQuickChecks(), Rand: rand.New(rand.NewSource(seed))}
}

// propSeedChain is the fixed base seed.
const propSeedChain = int64(0x408DEC)

// propProvider is a randomized decision provider for the deadline
// property. It records every context deadline it was invoked with and
// counts invocations.
type propProvider struct {
	id       string
	behavior int // 0 abstain, 1 select, 2 block-until-ctx-done, 3 sleep, 4 error, 5 panic, 6 invalid-select, 7 rank
	sleep    time.Duration

	mu          sync.Mutex
	calls       int
	deadlines   []time.Time
	hasDeadline []bool
}

func (p *propProvider) ID() string { return p.id }
func (p *propProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true, CanRank: true}
}
func (p *propProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy, CheckedAt: time.Now()}
}
func (p *propProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	p.mu.Lock()
	p.calls++
	if dl, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, dl)
		p.hasDeadline = append(p.hasDeadline, true)
	} else {
		p.hasDeadline = append(p.hasDeadline, false)
	}
	p.mu.Unlock()

	first := ""
	if len(req.Candidates) > 0 {
		first = req.Candidates[0].ID
	}
	switch p.behavior {
	case 1:
		return DecisionResult{Action: ActionSelect, SelectedID: first, Confidence: 0.9, ProviderID: p.id}, nil
	case 2:
		<-ctx.Done()
		return DecisionResult{Action: ActionAbstain, Abstained: true, ProviderID: p.id}, ctx.Err()
	case 3:
		select {
		case <-time.After(p.sleep):
			return DecisionResult{Action: ActionAbstain, Abstained: true, ProviderID: p.id}, nil
		case <-ctx.Done():
			return DecisionResult{Action: ActionAbstain, Abstained: true, ProviderID: p.id}, ctx.Err()
		}
	case 4:
		return DecisionResult{Action: ActionAbstain, Abstained: true, ProviderID: p.id}, errors.New("prop provider error")
	case 5:
		panic("prop provider panic")
	case 6:
		return DecisionResult{Action: ActionSelect, SelectedID: "nonexistent-candidate", Confidence: 0.9, ProviderID: p.id}, nil
	case 7:
		return DecisionResult{Action: ActionRank, RankedIDs: []string{first}, Confidence: 0.8, ProviderID: p.id}, nil
	default:
		return DecisionResult{Action: ActionAbstain, Abstained: true, ProviderID: p.id}, nil
	}
}

func (p *propProvider) snapshot() (calls int, deadlines []time.Time, hasDeadline []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, append([]time.Time(nil), p.deadlines...), append([]bool(nil), p.hasDeadline...)
}

func genBehavior(rnd *rand.Rand) int {
	switch rnd.Intn(100) {
	case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9:
		return 1 // select
	case 10, 11, 12, 13, 14, 15, 16, 17:
		return 2 // block until ctx done
	case 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32:
		return 3 // sleep then return
	case 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44:
		return 4 // error
	case 45, 46, 47, 48, 49:
		return 5 // panic
	case 50, 51, 52, 53, 54, 55, 56, 57, 58, 59:
		return 6 // invalid select
	case 60, 61, 62, 63, 64, 65, 66, 67, 68, 69:
		return 7 // rank (invalid for chain primary semantics)
	default:
		return 0 // abstain
	}
}

type chainCase struct {
	seed int64
}

func (chainCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(chainCase{seed: r.Int63()})
}

func TestProperty_ChainDeadlines(t *testing.T) {
	n := propQuickChecks()
	var maxOvershoot time.Duration
	var maxOvershootCase int
	var firstFail string
	idx := -1
	err := quick.Check(func(c chainCase) bool {
		idx++
		rnd := rand.New(rand.NewSource(c.seed))
		nSteps := 1 + rnd.Intn(5)
		providers := make([]*propProvider, nSteps)
		steps := make([]ChainStepConfig, nSteps)
		maxStepTimeout := 0
		for s := 0; s < nSteps; s++ {
			providers[s] = &propProvider{
				id:       fmt.Sprintf("prov-%d", s),
				behavior: genBehavior(rnd),
				sleep:    time.Duration(rnd.Intn(3)) * time.Millisecond,
			}
			stepTimeout := rnd.Intn(4) // 0..3ms; 0 = no per-step timeout
			steps[s] = ChainStepConfig{Provider: providers[s].id, TimeoutMS: stepTimeout}
			if stepTimeout > maxStepTimeout {
				maxStepTimeout = stepTimeout
			}
		}
		timeout := time.Duration(1+rnd.Intn(4)) * time.Millisecond // 1..4ms global
		maxCalls := 1 + rnd.Intn(8)

		// P9a anchor (see below): step 0 carries no per-step timeout, so
		// its context IS the chain context and its recorded deadline is
		// the true chain deadline.
		steps[0].TimeoutMS = 0

		reg := NewRegistry()
		for _, p := range providers {
			reg.Register(p)
		}
		exec := NewChainExecutor(reg, nil, &Metrics{})
		eligible := makeChainEligible("A", "B", "C", "D")
		budget := Budget{Timeout: timeout, MaxProviderCalls: maxCalls}

		fail := func(format string, args ...any) bool {
			if firstFail == "" {
				firstFail = fmt.Sprintf("case %d: "+format, append([]any{idx}, args...)...)
			}
			return false
		}

		start := time.Now()
		_, _, trace := exec.Execute(context.Background(),
			ChainConfig{ID: "prop", Steps: steps},
			DecisionRequest{Candidates: eligible},
			defaultConstraints(eligible), budget, timeout)
		elapsed := time.Since(start)

		// P8: the chain never exceeds its global deadline by more than one
		// in-flight step's grace (plus scheduling slack).
		bound := timeout + time.Duration(maxStepTimeout)*time.Millisecond + 500*time.Millisecond
		if elapsed > bound {
			return fail("elapsed %v exceeded deadline %v + one step grace (%vms) + slack (bound %v)",
				elapsed, timeout, maxStepTimeout, bound)
		}
		if over := elapsed - timeout; over > maxOvershoot {
			maxOvershoot = over
			maxOvershootCase = idx
		}

		// P9a: every invoked step's child deadline is within the chain
		// deadline — child budget never exceeds the remaining global.
		// Gap-free construction: step 0 was forced to TimeoutMS=0 above, so
		// its step context IS the chain context and its recorded deadline
		// is the true chain deadline. Every other step derives via
		// min(chainDeadline, stepTimeout), so dl <= anchor holds exactly by
		// context semantics — no wall-clock epsilon, no flake surface.
		totalCalls := 0
		var anchor time.Time
		anchorSet := false
		stepDeadlines := map[string][]time.Time{}
		for _, p := range providers {
			calls, deadlines, hasDl := p.snapshot()
			totalCalls += calls
			for k := range deadlines {
				if !hasDl[k] {
					return fail("step %s invoked without any context deadline", p.id)
				}
			}
			stepDeadlines[p.id] = deadlines
			if p.id == "prov-0" && len(deadlines) > 0 {
				anchor, anchorSet = deadlines[0], true
			}
		}
		if totalCalls == 0 {
			// Legitimate only when the global deadline fired before the
			// first step ran (extreme scheduling delay against a 1ms
			// budget): the executor must report DEADLINE_EXHAUSTED.
			if trace.Outcome != ChainOutcomeDeadlineExhausted {
				return fail("zero provider calls but outcome %s (want %s)",
					trace.Outcome, ChainOutcomeDeadlineExhausted)
			}
			return true
		}
		// Steps run strictly in order, so any invocation implies step 0
		// ran: the anchor must exist.
		if !anchorSet {
			return fail("%d calls but no chain-deadline anchor from prov-0", totalCalls)
		}
		for id, deadlines := range stepDeadlines {
			for _, dl := range deadlines {
				if dl.After(anchor) {
					return fail("step %s child deadline %v exceeds chain deadline %v (fresh-budget derivation)",
						id, dl, anchor)
				}
			}
		}

		// P9b: call budget respected.
		if trace.CallsUsed > maxCalls {
			return fail("trace.CallsUsed=%d exceeds maxCalls=%d", trace.CallsUsed, maxCalls)
		}
		if totalCalls > maxCalls {
			return fail("%d provider invocations exceed maxCalls=%d", totalCalls, maxCalls)
		}
		if totalCalls != trace.CallsUsed {
			return fail("invocation count %d != trace.CallsUsed %d", totalCalls, trace.CallsUsed)
		}
		return true
	}, quickConfig(propSeedChain))
	if err != nil {
		t.Fatalf("P8/P9 chain-deadlines property failed: %v\n%s", err, firstFail)
	}
	t.Logf("P8/P9 chain-deadlines: %d generated step graphs; max overshoot over global deadline %v (case %d); all child deadlines within chain deadline; call budget respected",
		n, maxOvershoot, maxOvershootCase)
}

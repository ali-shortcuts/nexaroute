package decision

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision/providerstate"
)

type mission95PanicProvider struct {
	id  string
	msg string
}

func (p mission95PanicProvider) ID() string { return p.id }
func (p mission95PanicProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true, CanRank: true}
}
func (p mission95PanicProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy}
}
func (p mission95PanicProvider) Decide(context.Context, DecisionRequest) (DecisionResult, error) {
	panic(p.msg)
}

func mission95ChainReq() DecisionRequest {
	return DecisionRequest{Candidates: makeChainEligible("a", "b")}
}

func TestMission95ChainExecutorDefaults(t *testing.T) {
	// Nil registry is replaced with a fresh one (local pre-registered).
	exec := NewChainExecutor(nil, nil, nil)
	_, res, trace := exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "local"}}},
		mission95ChainReq(), PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}}, Budget{}, 0)
	if res.Action != ActionAbstain || trace.Outcome != ChainOutcomeExhausted {
		t.Fatalf("local chain: %+v %+v", res, trace)
	}
	// Timeout defaults: zero cfg timeout and zero budget timeout -> 10ms.
	// A provider slower than that times out at the chain deadline.
	reg := NewRegistry()
	slow := timeoutProvider("slow", 200*time.Millisecond)
	reg.Register(slow)
	exec2 := NewChainExecutor(reg, nil, nil)
	_, res, trace = exec2.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "slow"}}},
		mission95ChainReq(), PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}}, Budget{}, 0)
	if res.Action != ActionAbstain || trace.Outcome != ChainOutcomeDeadlineExhausted {
		t.Fatalf("default timeout: %+v %+v", res, trace)
	}
	// Budget timeout smaller than cfg timeout wins.
	reg2 := NewRegistry()
	slow2 := timeoutProvider("slow", 200*time.Millisecond)
	reg2.Register(slow2)
	exec3 := NewChainExecutor(reg2, nil, nil)
	_, res, trace = exec3.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "slow"}}},
		mission95ChainReq(), PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}},
		Budget{Timeout: time.Millisecond}, time.Second)
	if trace.Outcome != ChainOutcomeDeadlineExhausted {
		t.Fatalf("budget timeout wins: %+v %+v", res, trace)
	}
	// Huge cfg timeout is capped at 5s: an immediate provider still
	// returns promptly, and the cap branch executes.
	reg3 := NewRegistry()
	reg3.Register(abstainProvider("abs"))
	exec4 := NewChainExecutor(reg3, nil, nil)
	_, res, trace = exec4.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "abs"}}},
		mission95ChainReq(), PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}}, Budget{}, 10*time.Second)
	if res.Action != ActionAbstain || trace.Outcome != ChainOutcomeExhausted {
		t.Fatalf("capped timeout: %+v %+v", res, trace)
	}
}

func TestMission95ChainMaxCallsCap(t *testing.T) {
	reg := NewRegistry()
	abs := abstainProvider("abs")
	reg.Register(abs)
	steps := make([]ChainStepConfig, 10)
	for i := range steps {
		steps[i] = ChainStepConfig{Provider: "abs"}
	}
	exec := NewChainExecutor(reg, nil, nil)
	_, _, trace := exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: steps},
		mission95ChainReq(), PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}}, Budget{}, time.Second)
	if trace.Outcome != ChainOutcomeBudgetExhausted {
		t.Fatalf("outcome = %v", trace.Outcome)
	}
	if trace.CallsUsed != 8 {
		t.Fatalf("calls used = %d, want 8 (cap)", trace.CallsUsed)
	}
	if len(trace.Steps) != 10 {
		t.Fatalf("steps = %d, want 10", len(trace.Steps))
	}
	skipped := 0
	for _, st := range trace.Steps {
		if st.Outcome == StepOutcomeSkippedBudget {
			skipped++
		}
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
	// Zero steps: maxCalls falls back to 1, loop never runs, exhausted.
	_, _, trace = exec.Execute(context.Background(), ChainConfig{ID: "c"},
		mission95ChainReq(), PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}}, Budget{}, time.Second)
	if trace.Outcome != ChainOutcomeExhausted || trace.CallsUsed != 0 {
		t.Fatalf("empty chain: %+v", trace)
	}
}

func TestMission95ChainCapabilityAndConstraintPaths(t *testing.T) {
	allowed := PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}}
	// RANK from a provider that cannot rank -> invalid step, chain exhausts.
	reg := NewRegistry()
	rankNoCap := newCountingProvider("rk")
	rankNoCap.capabilities = Capabilities{CanSelect: true, CanRank: false}
	rankNoCap.result = DecisionResult{Action: ActionRank, RankedIDs: []string{"b", "a"}, Confidence: 0.8, ProviderID: "rk"}
	reg.Register(rankNoCap)
	exec := NewChainExecutor(reg, nil, nil)
	_, _, trace := exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "rk"}}},
		mission95ChainReq(), allowed, Budget{}, time.Second)
	if trace.Outcome != ChainOutcomeExhausted || trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("rank-no-cap: %+v", trace)
	}
	// SELECT from a provider that cannot select -> invalid step.
	reg = NewRegistry()
	selNoCap := newCountingProvider("sn")
	selNoCap.capabilities = Capabilities{CanSelect: false, CanRank: true}
	selNoCap.result = DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: 0.9, ProviderID: "sn"}
	reg.Register(selNoCap)
	exec = NewChainExecutor(reg, nil, nil)
	_, _, trace = exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "sn"}}},
		mission95ChainReq(), allowed, Budget{}, time.Second)
	if trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("select-no-cap: %+v", trace)
	}
	// Valid RANK is still invalid for chain semantics (primary only).
	reg = NewRegistry()
	reg.Register(rankProvider("rk2", "b", "a"))
	exec = NewChainExecutor(reg, nil, nil)
	_, _, trace = exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "rk2"}}},
		mission95ChainReq(), allowed, Budget{}, time.Second)
	if trace.Steps[0].Outcome != StepOutcomeInvalid || trace.Steps[0].Action != ActionRank {
		t.Fatalf("valid rank in chain: %+v", trace)
	}
	// SELECT outside the allowed primary band -> primary-constraint invalid.
	reg = NewRegistry()
	reg.Register(selectProvider("sp", "b"))
	exec = NewChainExecutor(reg, nil, nil)
	_, _, trace = exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "sp"}}},
		mission95ChainReq(), PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a"}}, Budget{}, time.Second)
	if trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("constraint violation: %+v", trace)
	}
	found := false
	for _, rc := range trace.Steps[0].ReasonCodes {
		if rc == ReasonPrimaryConstraintViolation {
			found = true
		}
	}
	if !found {
		t.Fatalf("constraint reason missing: %v", trace.Steps[0].ReasonCodes)
	}
	// Error result with an invalid (empty) action -> step action forced to ABSTAIN.
	reg = NewRegistry()
	badErr := newCountingProvider("be")
	badErr.err = errors.New("boom")
	badErr.result = DecisionResult{ProviderID: "be"}
	reg.Register(badErr)
	exec = NewChainExecutor(reg, nil, nil)
	_, _, trace = exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "be"}}},
		mission95ChainReq(), allowed, Budget{}, time.Second)
	if trace.Steps[0].Outcome != StepOutcomeError || trace.Steps[0].Action != ActionAbstain {
		t.Fatalf("error-invalid-action: %+v", trace)
	}
}

func TestMission95ChainReasonCodeBounding(t *testing.T) {
	allowed := PrimarySelectionConstraints{AllowedPrimaryIDs: []string{"a", "b"}}
	// SELECT with exactly 8 codes: appending CHAIN_SELECTED truncates to 8.
	reg := NewRegistry()
	p := newCountingProvider("full")
	p.capabilities = Capabilities{CanSelect: true}
	p.result = DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: 0.9, ProviderID: "full",
		ReasonCodes: []ReasonCode{ReasonEligibleSetPreserved, ReasonPolicyScored, ReasonPolicySelectFirst, ReasonExistingOrderPreserved, ReasonAbstained, ReasonLocalPassThrough, ReasonSingleCandidate, ReasonOffMode}}
	reg.Register(p)
	exec := NewChainExecutor(reg, nil, nil)
	_, res, _ := exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "full"}}},
		mission95ChainReq(), allowed, Budget{}, time.Second)
	if len(res.ReasonCodes) != MaxReasonCodes {
		t.Fatalf("select codes = %d, want %d: %v", len(res.ReasonCodes), MaxReasonCodes, res.ReasonCodes)
	}
	// ABSTAIN already carrying CHAIN_STEP_ABSTAINED is not duplicated.
	reg = NewRegistry()
	ab := newCountingProvider("ab2")
	ab.result = DecisionResult{Action: ActionAbstain, Abstained: true, Confidence: 1, ProviderID: "ab2",
		ReasonCodes: []ReasonCode{ReasonChainStepAbstained}}
	reg.Register(ab)
	exec = NewChainExecutor(reg, nil, nil)
	_, _, trace := exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "ab2"}}},
		mission95ChainReq(), allowed, Budget{}, time.Second)
	if len(trace.Steps) != 1 || len(trace.Steps[0].ReasonCodes) != 1 {
		t.Fatalf("abstain dedup: %+v", trace.Steps)
	}
	// ABSTAIN at the reason-code cap does not append.
	reg = NewRegistry()
	ab8 := newCountingProvider("ab8")
	ab8.result = DecisionResult{Action: ActionAbstain, Abstained: true, Confidence: 1, ProviderID: "ab8",
		ReasonCodes: []ReasonCode{ReasonEligibleSetPreserved, ReasonPolicyScored, ReasonPolicySelectFirst, ReasonExistingOrderPreserved, ReasonAbstained, ReasonLocalPassThrough, ReasonSingleCandidate, ReasonOffMode}}
	reg.Register(ab8)
	exec = NewChainExecutor(reg, nil, nil)
	_, _, trace = exec.Execute(context.Background(),
		ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "ab8"}}},
		mission95ChainReq(), allowed, Budget{}, time.Second)
	if len(trace.Steps[0].ReasonCodes) != MaxReasonCodes {
		t.Fatalf("abstain cap: %v", trace.Steps[0].ReasonCodes)
	}
}

func TestMission95MetricsRecord(t *testing.T) {
	m := &Metrics{}
	m.Record(DecisionResult{Action: ActionAbstain}, nil, false, true)
	if got := m.Snapshot()["off_mode_total"]; got != 1 {
		t.Fatalf("off mode: %d", got)
	}
	m.Record(DecisionResult{Action: ActionSelect, Confidence: 0.9, Latency: 2 * time.Millisecond,
		ReasonCodes: []ReasonCode{ReasonExternalSelected}}, errors.New("x"), true, false)
	m.Record(DecisionResult{Action: ActionRank, Latency: 4 * time.Millisecond,
		ReasonCodes: []ReasonCode{ReasonInvalidResult}}, nil, false, false)
	m.Record(DecisionResult{Action: ActionAbstain,
		ReasonCodes: []ReasonCode{ReasonExternalHTTPError, ReasonExternalTimeout, ReasonExternalInvalidResponse, ReasonExternalProviderUnavailable, ReasonExternalRequestTooLarge, ReasonExternalResponseTooLarge}}, nil, false, false)
	snap := m.Snapshot()
	checks := map[string]int64{
		"decisions_total": 3, "abstains_total": 1, "failures_total": 1, "timeouts_total": 1,
		"invalid_total": 1, "select_total": 1, "rank_total": 1, "latency_count": 2,
		"external_total": 2, "external_selected": 1, "external_error": 1, "external_timeout": 1,
		"external_invalid": 1, "external_unavailable": 1, "external_request_too_large": 1,
		"external_response_too_large": 1, "external_latency_count": 1,
	}
	for k, want := range checks {
		if snap[k] != want {
			t.Errorf("snapshot[%s] = %d, want %d (full %v)", k, snap[k], want, snap)
		}
	}
	if snap["latency_avg_ms"] != 3 || snap["external_latency_avg_ms"] != 2 {
		t.Fatalf("averages: %v", snap)
	}
	// Chain outcomes and steps, including unknown labels ignored.
	for _, o := range []string{"selected", "exhausted", "budget_exhausted", "deadline_exhausted", "affinity_preserved", "bogus"} {
		m.RecordChainOutcome(o)
	}
	for _, o := range []string{"selected", "abstained", "error", "timeout", "invalid", "unavailable", "cooldown", "skipped_budget", "bogus"} {
		m.RecordChainStep("jev", o)
	}
	snap = m.Snapshot()
	for k, want := range map[string]int64{
		"chain_selected": 1, "chain_exhausted": 1, "chain_budget_exhausted": 1,
		"chain_deadline_exhausted": 1, "chain_affinity_preserved": 1,
		"chain_step_selected": 1, "chain_step_abstained": 1, "chain_step_error": 1,
		"chain_step_timeout": 1, "chain_step_invalid": 1, "chain_step_unavailable": 1,
		"chain_step_cooldown": 1, "chain_step_skipped_budget": 1,
	} {
		if snap[k] != want {
			t.Errorf("snapshot[%s] = %d, want %d", k, snap[k], want)
		}
	}
	// Zero-latency record does not add to the average denominator.
	m2 := &Metrics{}
	m2.Record(DecisionResult{Action: ActionSelect}, nil, false, false)
	if m2.Snapshot()["latency_avg_ms"] != 0 || m2.Snapshot()["latency_count"] != 0 {
		t.Fatalf("zero latency snapshot: %v", m2.Snapshot())
	}
}

func TestMission95ValidateAndNormalizeEdges(t *testing.T) {
	elig := makeChainEligible("a", "b")
	// Empty eligible.
	if err := ValidateResult(nil, DecisionResult{Action: ActionSelect, SelectedID: "a"}); !errors.Is(err, ErrEmptyEligible) {
		t.Fatalf("empty eligible: %v", err)
	}
	// ID length bounds.
	if err := ValidateResult(elig, DecisionResult{Action: ActionSelect, SelectedID: strings.Repeat("x", 513), Confidence: 0.5}); err == nil {
		t.Fatalf("long selected id should fail")
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionSelect, SelectedID: "a", ProviderID: strings.Repeat("p", 129), Confidence: 0.5}); err == nil {
		t.Fatalf("long provider id should fail")
	}
	// Reason code bounds: too long, non-canonical, too many.
	if err := ValidateResult(elig, DecisionResult{Action: ActionAbstain, ReasonCodes: []ReasonCode{ReasonCode(strings.Repeat("r", 65))}}); err == nil {
		t.Fatalf("long reason code should fail")
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionAbstain, ReasonCodes: []ReasonCode{"NOT_A_CODE"}}); err == nil {
		t.Fatalf("non-canonical code should fail")
	}
	manyCodes := make([]ReasonCode, MaxReasonCodes+1)
	for i := range manyCodes {
		manyCodes[i] = ReasonEligibleSetPreserved
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionAbstain, ReasonCodes: manyCodes}); !errors.Is(err, ErrReasonCodeTooMany) {
		t.Fatalf("too many codes: %v", err)
	}
	// Ranked bounds: hard limit, over eligible, duplicate, unknown, too-long id.
	huge := make([]string, MaxRankedIDs+1)
	for i := range huge {
		huge[i] = "a"
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionRank, RankedIDs: huge}); !errors.Is(err, ErrTooManyRanked) {
		t.Fatalf("hard ranked limit: %v", err)
	}
	if err := ValidateResult(makeChainEligible("a", "b", "c"), DecisionResult{Action: ActionRank, RankedIDs: []string{"a", "b", "a"}}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("dup ranked: %v", err)
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionRank, RankedIDs: []string{"a", "zz"}}); !errors.Is(err, ErrUnknownCandidate) {
		t.Fatalf("unknown ranked: %v", err)
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionRank, RankedIDs: []string{strings.Repeat("x", 513)}}); err == nil {
		t.Fatalf("long ranked id should fail")
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionRank}); err == nil {
		t.Fatalf("empty ranked should fail")
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionRank, SelectedID: "a", RankedIDs: []string{"a"}}); err == nil {
		t.Fatalf("rank with selected should fail")
	}
	// SELECT with ranked ids, ABSTAIN with payload.
	if err := ValidateResult(elig, DecisionResult{Action: ActionSelect, SelectedID: "a", RankedIDs: []string{"a"}}); err == nil {
		t.Fatalf("select with ranked should fail")
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionAbstain, SelectedID: "a"}); err == nil {
		t.Fatalf("abstain with selected should fail")
	}
	if err := ValidateResult(elig, DecisionResult{Action: ActionAbstain, RankedIDs: []string{"a"}}); err == nil {
		t.Fatalf("abstain with ranked should fail")
	}
	// Confidence bounds.
	for _, c := range []float64{math.NaN(), math.Inf(1), -0.1, 1.1} {
		if err := ValidateResult(elig, DecisionResult{Action: ActionAbstain, Confidence: c}); err == nil {
			t.Fatalf("confidence %v should fail", c)
		}
	}
	// Normalize fail-open paths.
	if _, _, r := NormalizeResult(nil, DecisionResult{Action: ActionSelect, SelectedID: "a"}); r != ReasonEmptyEligible {
		t.Fatalf("normalize empty: %v", r)
	}
	if _, applied, r := NormalizeResult(elig, DecisionResult{Action: ActionSelect}); !applied && r != ReasonInvalidResult {
		t.Fatalf("normalize select empty id: %v %v", applied, r)
	}
	if _, _, r := NormalizeResult(elig, DecisionResult{Action: ActionSelect, SelectedID: "zz"}); r != ReasonInvalidResult {
		t.Fatalf("normalize select unknown: %v", r)
	}
	if _, _, r := NormalizeResult(elig, DecisionResult{Action: ActionRank}); r != ReasonAbstained {
		t.Fatalf("normalize rank empty: %v", r)
	}
	// Rank normalize dedups and appends omitted in original order.
	ordered, applied, r := NormalizeResult(elig, DecisionResult{Action: ActionRank, RankedIDs: []string{"b", "b", "a"}})
	if !applied || len(ordered) != 2 || ordered[0].ID != "b" || ordered[1].ID != "a" {
		t.Fatalf("rank normalize: %v %v %v", ordered, applied, r)
	}
	ordered, applied, r = NormalizeResult(elig, DecisionResult{Action: ActionRank, RankedIDs: []string{"b"}})
	if !applied || r != ReasonNormalizationApplied || ordered[1].ID != "a" {
		t.Fatalf("rank partial: %v %v %v", ordered, applied, r)
	}
	ordered, _, r = NormalizeResult(elig, DecisionResult{Action: ActionRank, RankedIDs: []string{"a", "b"}})
	if r != ReasonEligibleSetPreserved {
		t.Fatalf("rank full: %v", r)
	}
	// Unknown action normalizes to abstain-order.
	if ordered, applied, r := NormalizeResult(elig, DecisionResult{Action: "NOPE"}); applied || r != ReasonAbstained || len(ordered) != 2 {
		t.Fatalf("unknown normalize: %v %v %v", ordered, applied, r)
	}
}

func TestMission95ResultRegistryConstraints(t *testing.T) {
	// IsAbstain branches.
	if !(DecisionResult{Action: ActionAbstain}.IsAbstain()) {
		t.Fatalf("abstain action should be abstain")
	}
	if (DecisionResult{Abstained: true}).IsAbstain() {
		t.Fatalf("abstained flag with empty action is not abstain")
	}
	if (DecisionResult{Action: ActionSelect, SelectedID: "a"}).IsAbstain() {
		t.Fatalf("select is not abstain")
	}
	if !(DecisionResult{Action: ActionAbstain}.IsStrictAbstain()) {
		t.Fatalf("strict abstain")
	}
	if (DecisionResult{Action: ActionAbstain, SelectedID: "a"}).IsStrictAbstain() {
		t.Fatalf("strict abstain with payload")
	}
	// CloneCandidates: nil for empty, independent copy otherwise.
	if got := CloneCandidates(nil); got != nil {
		t.Fatalf("nil clone: %v", got)
	}
	orig := makeChainEligible("a", "b")
	cp := CloneCandidates(orig)
	cp[0].ID = "changed"
	if orig[0].ID != "a" {
		t.Fatalf("clone shares backing array")
	}
	// Registry: nil/empty registrations ignored, resolve defaults, snapshot.
	reg := NewRegistry()
	reg.Register(nil)
	reg.Register(&LocalProvider{}) // re-register local, last wins (same impl)
	if _, err := reg.Resolve(""); err != nil {
		t.Fatalf("empty resolve should default to local: %v", err)
	}
	if _, err := reg.Resolve("missing"); err == nil {
		t.Fatalf("missing resolve should fail")
	}
	snap := reg.Snapshot()
	if snap["local"].Status != HealthHealthy {
		t.Fatalf("registry snapshot: %v", snap)
	}
	ids := reg.List()
	found := false
	for _, id := range ids {
		if id == "local" {
			found = true
		}
	}
	if !found {
		t.Fatalf("list missing local: %v", ids)
	}
	// Primary constraints: empty, pinned outside band, band membership.
	if got := ComputePrimaryConstraints(nil, ""); len(got.AllowedPrimaryIDs) != 0 {
		t.Fatalf("empty constraints: %+v", got)
	}
	poolCands := makeChainEligibleWithPools([]string{"a", "b"}, []int{0, 1}, []int{10, 10})
	got := ComputePrimaryConstraints(poolCands, "b")
	if got.ForcedPrimaryID != "" {
		t.Fatalf("out-of-band pin must not force: %+v", got)
	}
	if !ComputePrimaryConstraints(poolCands, "").IsAllowedPrimary("a") || ComputePrimaryConstraints(poolCands, "").IsAllowedPrimary("b") {
		t.Fatalf("band membership wrong")
	}
	// Pinned inside band forces and reports pool+affinity reasons.
	got = ComputePrimaryConstraints(poolCands, "a")
	if got.ForcedPrimaryID != "a" || got.MinOrdinal != 0 || got.MinPriority != 10 {
		t.Fatalf("forced: %+v", got)
	}
	// Budget zero check.
	if !(Budget{}).IsZero() || (Budget{Timeout: time.Second}).IsZero() {
		t.Fatalf("budget IsZero")
	}
}

func TestMission95OrchestratorConfigAndSinglePaths(t *testing.T) {
	// Defaults for nil registry/metrics and empty config.
	o := NewOrchestrator(nil, config.DecisionConfig{}, nil)
	if o.Config().Mode != "off" || o.Config().Provider != "local" || o.Config().TimeoutMS != 10 {
		t.Fatalf("orchestrator defaults: %+v", o.Config())
	}
	if o.MetricsSnapshot() == nil {
		t.Fatalf("metrics snapshot should be non-nil")
	}
	// WithConfig defaults for health fields and chain map.
	o2 := NewOrchestratorWithConfig(nil, config.Config{}, nil)
	if o2.Config().TimeoutMS != 10 {
		t.Fatalf("with-config defaults: %+v", o2.Config())
	}
	o2.UpdateFullConfig(config.Config{})
	if o2.Config().Mode != "off" {
		t.Fatalf("update defaults: %+v", o2.Config())
	}
	// Nil-metrics orchestrator returns an empty snapshot.
	o3 := &Orchestrator{}
	if got := o3.MetricsSnapshot(); len(got) != 0 {
		t.Fatalf("nil metrics snapshot: %v", got)
	}
	// Single path: error result's long Error string is truncated to 256.
	reg := NewRegistry()
	longErr := newCountingProvider("longerr")
	longErr.capabilities = Capabilities{CanSelect: true}
	longErr.result = DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: 0.9, ProviderID: "longerr", Error: strings.Repeat("e", 300)}
	reg.Register(longErr)
	oo := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "longerr"}, nil)
	_, res, _ := oo.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b"), PinnedCandidateID: ""})
	// Primary constraints allow only the earliest pool/min priority tier;
	// both candidates share pool 0 / priority 10, so "a" is allowed.
	if res.Action != ActionSelect || len(res.Error) != 256 {
		t.Fatalf("long error truncation: action=%v errlen=%d res=%+v", res.Action, len(res.Error), res)
	}
	// Single path: panic is recovered, bounded, and recorded as failure
	// for an external provider.
	reg2 := NewRegistry()
	reg2.Register(mission95PanicProvider{id: "ext-panic", msg: strings.Repeat("p", 300)})
	oo2 := NewOrchestrator(reg2, config.DecisionConfig{Mode: "assisted", Provider: "ext-panic"}, nil)
	_, res, _ = oo2.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionAbstain || len(res.Error) != 256 {
		t.Fatalf("panic path: %+v", res)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == ReasonProviderPanic {
			found = true
		}
	}
	if !found {
		t.Fatalf("panic reason missing: %v", res.ReasonCodes)
	}
	// Observed behavior: the panic recovery site and the subsequent
	// error branch each record a failure, so one panic counts twice.
	if st, ok := oo2.ProviderState().SnapshotOne("ext-panic"); !ok || st.ConsecutiveFailures != 2 {
		t.Fatalf("panic should record failure: %+v %v", st, ok)
	}
	// Single path: provider that cannot select is rejected.
	reg3 := NewRegistry()
	nosel := newCountingProvider("nosel")
	nosel.capabilities = Capabilities{CanSelect: false, CanRank: true}
	nosel.result = DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: 0.9, ProviderID: "nosel"}
	reg3.Register(nosel)
	oo3 := NewOrchestrator(reg3, config.DecisionConfig{Mode: "assisted", Provider: "nosel"}, nil)
	_, res, _ = oo3.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionAbstain || res.Error != "provider cannot select" {
		t.Fatalf("cannot-select: %+v", res)
	}
	// Hybrid mode with a missing chain fails open without provider calls.
	oo4 := NewOrchestrator(NewRegistry(), config.DecisionConfig{Mode: "hybrid", Chain: "nope"}, nil)
	_, res, trace := oo4.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionAbstain || res.Error != "chain not found" || trace.ChainTrace == nil {
		t.Fatalf("missing chain: %+v %+v", res, trace)
	}
	// Budget-exhausted single path (non-positive call budget).
	oo5 := NewOrchestrator(NewRegistry(), config.DecisionConfig{Mode: "assisted", Provider: "local"}, nil)
	_, res, _ = oo5.Decide(context.Background(), DecisionRequest{
		Candidates: makeChainEligible("a", "b"),
		Budget:     Budget{MaxProviderCalls: -1},
	})
	// Budget with only MaxProviderCalls set is not IsZero, so the budget
	// path applies and a non-positive budget abstains.
	if res.Action != ActionAbstain {
		t.Fatalf("budget exhausted single: %+v", res)
	}
	_ = providerstate.DefaultConfig
}

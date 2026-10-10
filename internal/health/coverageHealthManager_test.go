package health

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type coverageHealthManagerClock struct {
	mu  sync.RWMutex
	now time.Time
}

func coverageHealthManagerNewClock(at time.Time) *coverageHealthManagerClock {
	return &coverageHealthManagerClock{now: at}
}

func (c *coverageHealthManagerClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *coverageHealthManagerClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestCoverageHealthManagerClockAndConstructorDefaults(t *testing.T) {
	m := New(0, 0)
	if m.threshold != 1 || m.cooldown != time.Hour {
		t.Fatalf("constructor defaults threshold=%d cooldown=%s", m.threshold, m.cooldown)
	}
	m.RecordSuccess("default-clock", 0)
	if st := m.Get("default-clock"); st.Status != Healthy || st.EWMALatencyMS != 0 {
		t.Fatalf("real-clock success with nonpositive latency: %+v", st)
	}

	clock := coverageHealthManagerNewClock(time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC))
	m.SetNowFunc(clock.Now)
	m.RecordSuccess("fixed-clock", 20*time.Millisecond)
	st := m.Get("fixed-clock")
	if !st.LastChecked.Equal(clock.Now()) || !st.LastSuccess.Equal(clock.Now()) {
		t.Fatalf("injected clock was not used: %+v", st)
	}
	clock.Advance(time.Minute)
	m.RecordFailure("fixed-clock", "later", 0)
	if got := m.Get("fixed-clock").LastFailure; !got.Equal(clock.Now()) {
		t.Fatalf("advanced clock was not used: %v", got)
	}
}

func TestCoverageHealthManagerGlobalAndScopeCooldownBoundaries(t *testing.T) {
	clock := coverageHealthManagerNewClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	m := New(2, time.Minute)
	m.ConfigureAdvanced(2, time.Minute, 1, time.Minute)
	m.SetNowFunc(clock.Now)
	m.ForceCooldown("both", "global", time.Minute)
	m.RecordScopeFailure("both", []string{"text"}, "scope failed")

	clock.Advance(time.Minute)
	st, ready := m.GetWithScopes("both", []string{"text"})
	if st.Status != Cooldown || st.Scopes["text"].Status != Cooldown || ready {
		t.Fatalf("cooldowns should remain active exactly at deadline: state=%+v ready=%v", st, ready)
	}
	clock.Advance(time.Nanosecond)
	st, ready = m.GetWithScopes("both", []string{"text"})
	if st.Status != HalfOpen || st.ConsecutiveFailures != 0 || st.RecoveryFailures != 0 || st.LastError != "" || !st.CooldownUntil.IsZero() {
		t.Fatalf("global deadline did not normalize cleanly: %+v", st)
	}
	if !ready || st.Scopes["text"].Status != Unknown || st.Scopes["text"].ConsecutiveFailures != 0 || st.Scopes["text"].LastError != "" || !st.Scopes["text"].CooldownUntil.IsZero() {
		t.Fatalf("scope deadline did not normalize cleanly: state=%+v ready=%v", st, ready)
	}
	if !m.ScopesReady("missing", nil) || !m.ScopesReady("missing", []string{"anything"}) {
		t.Fatal("unknown deployments and empty scope lists should be ready")
	}
	missing, ok := m.GetWithScopes("not-yet-recorded", []string{"scope"})
	if !ok || missing.Deployment != "not-yet-recorded" || missing.Status != Unknown {
		t.Fatalf("unexpected missing state: %+v ready=%v", missing, ok)
	}
}

func TestCoverageHealthManagerScopeTransitionsAndSnapshotsAreIsolated(t *testing.T) {
	clock := coverageHealthManagerNewClock(time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC))
	m := New(5, time.Hour)
	m.SetNowFunc(clock.Now)
	m.RecordScopeSuccess("d", nil)
	m.RecordScopeFailure("d", nil, "ignored")
	if got := m.Snapshot(); len(got) != 0 {
		t.Fatalf("empty scope observations created state: %+v", got)
	}
	m.RecordScopeSuccess("d", []string{"", "chat"})
	st := m.Get("d")
	if st.Status == Healthy || st.Scopes["chat"].Status != Healthy || st.Scopes["chat"].Successes != 1 || !st.Scopes["chat"].LastSuccess.Equal(clock.Now()) {
		t.Fatalf("unexpected scope success state: %+v", st)
	}
	m.RecordScopeFailure("d", []string{"", "chat", "files"}, "unsupported")
	st = m.Get("d")
	if st.Scopes["chat"].Status != Degraded || st.Scopes["chat"].Failures != 1 || st.Scopes["files"].Status != Degraded {
		t.Fatalf("first scope failures did not degrade independently: %+v", st.Scopes)
	}
	m.RecordScopeFailure("d", []string{"chat"}, "still unsupported")
	st, ready := m.GetWithScopes("d", []string{"chat"})
	if ready || st.Scopes["chat"].Status != Cooldown || !st.Scopes["chat"].CooldownUntil.Equal(clock.Now().Add(5*time.Minute)) {
		t.Fatalf("scope threshold did not open expected lease: state=%+v ready=%v", st.Scopes["chat"], ready)
	}
	if !m.ScopesReady("d", []string{"files"}) || m.ScopesReady("d", []string{"chat"}) {
		t.Fatal("scope readiness did not remain isolated by capability")
	}

	copy1 := m.Snapshot()
	if len(copy1) != 1 {
		t.Fatalf("snapshot size=%d want 1", len(copy1))
	}
	copy1[0].Scopes["chat"] = ScopeState{Status: Healthy}
	copy2, ready := m.GetWithScopes("d", []string{"chat"})
	if ready || copy2.Scopes["chat"].Status != Cooldown {
		t.Fatalf("caller mutation escaped snapshot clone: %+v ready=%v", copy2, ready)
	}
}

func TestCoverageHealthManagerProviderWindowCooldownAndInvalidInputs(t *testing.T) {
	clock := coverageHealthManagerNewClock(time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC))
	m := New(5, time.Hour)
	m.SetNowFunc(clock.Now)
	m.ConfigureProviderIncidents(1, 0, 0)
	if m.providerThreshold != 2 || m.providerWindow != 20*time.Second || m.providerCooldown != 30*time.Second {
		t.Fatalf("provider invalid-config defaults: threshold=%d window=%s cooldown=%s", m.providerThreshold, m.providerWindow, m.providerCooldown)
	}
	if !m.ProviderAvailable("") || !m.ProviderAvailable("not-created") {
		t.Fatal("empty or unknown provider should be available")
	}
	m.RecordProviderFailure("", "ignored", "ignored")
	m.RecordProviderSuccess("")
	if got := m.ProviderSnapshot(); len(got) != 0 {
		t.Fatalf("empty provider ID created state: %+v", got)
	}

	m.ConfigureProviderIncidents(3, 10*time.Second, 7*time.Second)
	m.RecordProviderFailure("p", "", "first") // Empty deployment IDs use the provider identity.
	m.RecordProviderFailure("p", "p", "same deployment")
	if got := m.ProviderSnapshot(); len(got) != 1 || got[0].Evidence != 1 || got[0].Status != Degraded {
		t.Fatalf("repeated deployment should count once: %+v", got)
	}
	clock.Advance(10 * time.Second)
	m.RecordProviderFailure("p", "q", "second deployment")
	if got := m.ProviderSnapshot(); len(got) != 1 || got[0].Evidence != 2 || got[0].Status != Degraded {
		t.Fatalf("evidence at exact window edge was not retained: %+v", got)
	}
	clock.Advance(time.Nanosecond)
	m.RecordProviderFailure("p", "r", "third deployment")
	if !m.ProviderAvailable("p") {
		t.Fatal("provider should remain available with only two current-window failures")
	}
	m.RecordProviderFailure("p", "s", "fourth deployment")
	if m.ProviderAvailable("p") {
		t.Fatal("provider should cool after three current-window distinct failures")
	}
	before := m.ProviderSnapshot()[0]
	if before.Evidence != 3 || !before.CooldownUntil.Equal(clock.Now().Add(7*time.Second)) {
		t.Fatalf("provider cooldown did not use configured duration: %+v", before)
	}
	clock.Advance(7*time.Second + time.Nanosecond)
	if !m.ProviderAvailable("p") {
		t.Fatal("provider lease did not expire into half-open availability")
	}
	st := m.ProviderSnapshot()[0]
	if st.Status != HalfOpen || st.Evidence != 0 || st.LastError != "" || !st.CooldownUntil.IsZero() {
		t.Fatalf("provider snapshot did not normalize expired lease: %+v", st)
	}
	m.RecordProviderFailure("p", "r", "half-open probe failed")
	if got := m.ProviderSnapshot()[0]; got.Status != Cooldown || got.Evidence != 1 || !got.CooldownUntil.Equal(clock.Now().Add(7*time.Second)) {
		t.Fatalf("half-open failure did not re-arm provider lease: %+v", got)
	}
	clock.Advance(7*time.Second + time.Nanosecond)
	m.RecordProviderSuccess("p")
	if got := m.ProviderSnapshot()[0]; got.Status != Healthy || got.Evidence != 0 || got.LastError != "" || !got.CooldownUntil.IsZero() {
		t.Fatalf("provider success did not recover state: %+v", got)
	}
}

func TestCoverageHealthManagerProviderSuccessAndRetention(t *testing.T) {
	clock := coverageHealthManagerNewClock(time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC))
	m := New(5, time.Hour)
	m.SetNowFunc(clock.Now)
	m.RecordProviderFailure("keep", "a", "first")
	m.RecordProviderSuccess("keep")
	if got := m.ProviderSnapshot(); len(got) != 1 || got[0].Status != Healthy || got[0].Evidence != 0 || !got[0].LastSuccess.Equal(clock.Now()) {
		t.Fatalf("ordinary provider success did not establish healthy state: %+v", got)
	}
	m.RecordProviderFailure("remove", "b", "first")
	m.RetainProviders(map[string]struct{}{"keep": {}})
	if got := m.ProviderSnapshot(); len(got) != 1 || got[0].Provider != "keep" {
		t.Fatalf("provider retention did not prune removed providers: %+v", got)
	}
	// Removing an old evidence map must not make a later incident inherit its history.
	m.ConfigureProviderIncidents(2, time.Minute, time.Minute)
	m.RecordProviderFailure("remove", "c", "new first")
	if !m.ProviderAvailable("remove") {
		t.Fatal("first incident after retention unexpectedly opened provider circuit")
	}
	m.InvalidateProvider("")
	m.InvalidateProvider("keep")
	m.InvalidateProvider("remove")
	if got := m.ProviderSnapshot(); len(got) != 0 {
		t.Fatalf("provider invalidation failed: %+v", got)
	}
}

func TestCoverageHealthManagerRetirementAndCooldownDefaults(t *testing.T) {
	clock := coverageHealthManagerNewClock(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	m := New(3, 9*time.Minute)
	m.SetNowFunc(clock.Now)
	m.Retire("retired", "ended", "permanent")
	retired := m.Get("retired")
	m.RecordRecoveryFailure("retired", "late probe", time.Second)
	m.EnterCooldown("retired", "late cooldown", time.Second)
	if got := m.Get("retired"); got.Status != Retired || got.Failures != retired.Failures || got.LastErrorClass != "permanent" {
		t.Fatalf("retired state changed due to stale recovery/cooldown: before=%+v after=%+v", retired, got)
	}
	m.RecordRecoveryFailure("recovering", "probe error", 250*time.Millisecond)
	st := m.Get("recovering")
	if st.Status != Degraded || st.RecoveryFailures != 1 || st.Failures != 1 || st.EWMALatencyMS != 250 || !st.LastFailure.Equal(clock.Now()) {
		t.Fatalf("recovery failure did not record the recovery observation: %+v", st)
	}
	m.EnterCooldown("recovering", "default lease", 0)
	if got := m.Get("recovering"); got.Status != Cooldown || !got.CooldownUntil.Equal(clock.Now().Add(9*time.Minute)) {
		t.Fatalf("nonpositive cooldown duration did not use manager default: %+v", got)
	}
	m.ForceCooldown("forced", "default force lease", -time.Second)
	if got := m.Get("forced"); got.Status != Cooldown || got.Failures != 1 || got.ConsecutiveFailures != 1 || !got.CooldownUntil.Equal(clock.Now().Add(9*time.Minute)) {
		t.Fatalf("force cooldown defaults/counters incorrect: %+v", got)
	}
	forced := m.Get("forced")
	m.ForceCooldown("retired", "late forced cooldown", time.Minute)
	if got := m.Get("retired"); got.Status != Retired || got.Failures != retired.Failures {
		t.Fatalf("force cooldown changed retired state: %+v", got)
	}
	if m.Get("forced").CooldownUntil != forced.CooldownUntil {
		t.Fatal("attempt to force a retired deployment changed unrelated lease")
	}
}

func TestCoverageHealthManagerConfigureClampsAndAdvancedSettings(t *testing.T) {
	m := New(2, time.Second)
	m.ConfigureAdvanced(0, 0, 0, 0)
	if m.threshold != 1 || m.cooldown != time.Hour || m.scopeThreshold != 2 || m.scopeCooldown != 5*time.Minute {
		t.Fatalf("advanced config did not clamp invalid values: %+v", m)
	}
	m.ConfigureProviderIncidents(0, -time.Second, -time.Second)
	if m.providerThreshold != 2 || m.providerWindow != 20*time.Second || m.providerCooldown != 30*time.Second {
		t.Fatalf("provider config did not clamp invalid values: %+v", m)
	}
	m.ConfigureAdvanced(4, 2*time.Minute, 3, time.Second)
	m.Configure(7, 3*time.Minute)
	if m.threshold != 7 || m.cooldown != 3*time.Minute || m.scopeThreshold != 3 || m.scopeCooldown != time.Second {
		t.Fatalf("basic configure did not preserve advanced scope settings: %+v", m)
	}
}

func TestCoverageHealthManagerConcurrentUpdatesRemainExact(t *testing.T) {
	clock := coverageHealthManagerNewClock(time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC))
	m := New(100000, time.Hour)
	m.SetNowFunc(clock.Now)
	const workers, updates = 8, 100
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < updates; i++ {
				m.RecordSuccess("shared", time.Duration(worker+1)*time.Millisecond)
				m.RecordTTFT("shared", time.Duration(i+1)*time.Millisecond)
				m.RecordScopeSuccess("shared", []string{"chat"})
				m.RecordProviderFailure("provider", fmt.Sprintf("deployment-%d", worker), "transient")
			}
		}(worker)
	}
	wg.Wait()
	st := m.Get("shared")
	if st.Successes != workers*updates || st.Status != Healthy || st.Scopes["chat"].Successes != workers*updates || st.EWMALatencyMS <= 0 || st.EWMATTFTMS <= 0 {
		t.Fatalf("concurrent deployment updates were lost or malformed: %+v", st)
	}
	providers := m.ProviderSnapshot()
	if len(providers) != 1 || providers[0].Evidence != 3 || providers[0].Status != Cooldown {
		t.Fatalf("concurrent provider evidence was not distinct and exact: %+v", providers)
	}
}

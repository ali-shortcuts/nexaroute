package probe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func providerIncidentProbeFailure(status int) bool {
	return status == 0 ||
		status == http.StatusUnauthorized ||
		status == http.StatusPaymentRequired ||
		status == http.StatusForbidden ||
		status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= 500
}

func classifyProbeFailure(status int, err error) compat.Classified {
	var responseErr *providers.UpstreamResponseError
	if errors.As(err, &responseErr) && responseErr != nil {
		return compat.ClassifyUpstreamError(responseErr.StatusCode, responseErr.Body)
	}
	if status >= 200 && status < 300 {
		c := compat.ClassifyMalformedResponse("probe response failed protocol validation")
		c.StatusCode = status
		return c
	}
	if status > 0 {
		return compat.ClassifyUpstreamError(status, nil)
	}
	return compat.ClassifyTransportError(err)
}

func safeProbeFailure(c compat.Classified) string {
	if c.StatusCode > 0 {
		return fmt.Sprintf("%s (upstream status %d)", c.Class, c.StatusCode)
	}
	return string(c.Class)
}

type Result struct {
	Total           int   `json:"total"`
	Passed          int   `json:"passed"`
	Failed          int   `json:"failed"`
	SkippedCooldown int   `json:"skipped_cooldown"`
	SkippedMissing  int   `json:"skipped_missing_adapter"`
	SkippedRecovery int   `json:"skipped_recovery"`
	SkippedReady    int   `json:"skipped_ready"`
	SkippedRetired  int   `json:"skipped_retired"`
	Canceled        int   `json:"canceled,omitempty"`
	DurationMS      int64 `json:"duration_ms"`
}

const (
	recoveryWorkerCount = 64
	maxRecoveryQueue    = 20000
)

type recoveryTask struct {
	id         string
	identity   string
	attempt    int
	generation uint64
}

type Engine struct {
	cfgMu sync.RWMutex
	cfg   config.Config
	reg   *providers.Registry
	rt    *router.Router
	hm    *health.Manager
	bus   *events.Bus

	// capStore receives Level B compatibility verdicts; nil disables.
	capStore *compat.Store

	trigger chan struct{}
	runMu   sync.Mutex
	primeMu sync.Mutex
	primed  bool

	ctxMu  sync.RWMutex
	runCtx context.Context

	recoveryMu          sync.Mutex
	recovering          map[string]bool
	recoveryQueue       chan recoveryTask
	recoveryWorkers     sync.Once
	recoveryTimers      map[string]*time.Timer
	recoveryTokens      map[string]uint64
	recoveryGenerations map[string]uint64
	recoverySeq         uint64

	limitMu      sync.Mutex
	activeProbes int
	limitChanged chan struct{}
}

func New(cfg config.Config, reg *providers.Registry, rt *router.Router, hm *health.Manager, bus *events.Bus) *Engine {
	return &Engine{
		cfg:                 cfg,
		reg:                 reg,
		rt:                  rt,
		hm:                  hm,
		bus:                 bus,
		trigger:             make(chan struct{}, 1),
		recovering:          map[string]bool{},
		recoveryQueue:       make(chan recoveryTask, maxRecoveryQueue),
		recoveryTimers:      map[string]*time.Timer{},
		recoveryTokens:      map[string]uint64{},
		recoveryGenerations: map[string]uint64{},
		limitChanged:        make(chan struct{}),
	}
}

func (e *Engine) Reload(cfg config.Config) {
	e.cfgMu.Lock()
	e.cfg = cfg
	e.cfgMu.Unlock()
	// Config reload may replace a model or provider identity while delayed
	// recovery timers are pending. Cancel all queued work; periodic/manual
	// sweeps and newly classified failures will rebuild recovery under the
	// current identity.
	e.cancelAllRecoveries()

	// Wake probe workers so a raised concurrency limit takes effect promptly.
	e.limitMu.Lock()
	close(e.limitChanged)
	e.limitChanged = make(chan struct{})
	e.limitMu.Unlock()

	e.Trigger()
}

type RuntimeStats struct {
	RecoveryQueueDepth int
	RecoveryTracked    int
	RecoveryWorkers    int
	ActiveProbes       int
}

func (e *Engine) Stats() RuntimeStats {
	e.recoveryMu.Lock()
	tracked := len(e.recovering)
	e.recoveryMu.Unlock()
	e.limitMu.Lock()
	active := e.activeProbes
	e.limitMu.Unlock()
	return RuntimeStats{
		RecoveryQueueDepth: len(e.recoveryQueue),
		RecoveryTracked:    tracked,
		RecoveryWorkers:    recoveryWorkerCount,
		ActiveProbes:       active,
	}
}

func (e *Engine) current() config.Config {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	return e.cfg
}

func (e *Engine) Trigger() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

func (e *Engine) setRunContext(ctx context.Context) {
	e.ctxMu.Lock()
	e.runCtx = ctx
	e.ctxMu.Unlock()
}

func (e *Engine) context() context.Context {
	e.ctxMu.RLock()
	defer e.ctxMu.RUnlock()
	return e.runCtx
}

func (e *Engine) Prime(ctx context.Context) Result {
	e.setRunContext(ctx)
	e.primeMu.Lock()
	e.primed = true
	e.primeMu.Unlock()
	return e.runOnce(ctx, true)
}

func (e *Engine) wasPrimed() bool {
	e.primeMu.Lock()
	defer e.primeMu.Unlock()
	return e.primed
}

func (e *Engine) startRecoveryWorkers(ctx context.Context) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	e.recoveryWorkers.Do(func() {
		for i := 0; i < recoveryWorkerCount; i++ {
			go e.recoveryWorker(ctx)
		}
	})
}

func (e *Engine) Start(ctx context.Context) {
	e.setRunContext(ctx)
	e.startRecoveryWorkers(ctx)
	go e.Run(ctx)
}

func (e *Engine) Run(ctx context.Context) {
	e.setRunContext(ctx)
	defer e.cancelAllRecoveries()
	cfg := e.current()
	if cfg.Probe.Enabled && cfg.Probe.OnStart && !e.wasPrimed() {
		e.runOnce(ctx, false)
	}
	for {
		cfg = e.current()
		interval := cfg.ProbeInterval()
		if interval < time.Second {
			interval = time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
			if cfg.Probe.Enabled {
				e.runOnce(ctx, false)
			}
		case <-e.trigger:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if cfg.Probe.Enabled {
				e.runOnce(ctx, false)
			}
		}
	}
}

// Recover hands one quarantined deployment to a bounded recovery queue.
// Duplicate queued/active/delayed recovery for the same deployment is
// suppressed. Long cooldowns are represented by timers, not sleeping goroutines.
func (e *Engine) Recover(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	ctx := e.context()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	d, _, ok := e.deployment(id)
	if !ok {
		return
	}
	st := e.hm.Get(id)
	if st.Status == health.Retired || (st.Identity != "" && st.Identity != d.Identity) {
		return
	}
	e.startRecoveryWorkers(ctx)
	e.recoveryMu.Lock()
	if e.recovering[id] {
		e.recoveryMu.Unlock()
		return
	}
	e.recoverySeq++
	generation := e.recoverySeq
	e.recovering[id] = true
	e.recoveryGenerations[id] = generation
	e.recoveryMu.Unlock()

	task := recoveryTask{id: id, identity: d.Identity, attempt: 1, generation: generation}
	if !e.enqueueRecovery(ctx, task) {
		e.clearRecoveryTask(task)
		e.bus.Add(events.Event{Kind: "recovery_queue_full", Deployment: id, Message: "bounded recovery queue is full; background sweep will retry", ErrorType: "recovery_queue_full"})
	}
}

func (e *Engine) enqueueRecovery(ctx context.Context, task recoveryTask) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	select {
	case e.recoveryQueue <- task:
		return true
	default:
		return false
	}
}

func (e *Engine) clearRecoveryTask(task recoveryTask) {
	e.recoveryMu.Lock()
	if !e.recovering[task.id] || e.recoveryGenerations[task.id] != task.generation {
		e.recoveryMu.Unlock()
		return
	}
	if timer := e.recoveryTimers[task.id]; timer != nil {
		timer.Stop()
		delete(e.recoveryTimers, task.id)
	}
	delete(e.recoveryTokens, task.id)
	delete(e.recoveryGenerations, task.id)
	delete(e.recovering, task.id)
	e.recoveryMu.Unlock()
}

func (e *Engine) isCurrentRecovery(task recoveryTask) bool {
	e.recoveryMu.Lock()
	defer e.recoveryMu.Unlock()
	return e.recovering[task.id] && e.recoveryGenerations[task.id] == task.generation
}

func (e *Engine) cancelAllRecoveries() {
	e.recoveryMu.Lock()
	for id, timer := range e.recoveryTimers {
		if timer != nil {
			timer.Stop()
		}
		delete(e.recoveryTimers, id)
	}
	e.recoveryTokens = map[string]uint64{}
	e.recoveryGenerations = map[string]uint64{}
	e.recovering = map[string]bool{}
	e.recoveryMu.Unlock()
}

func (e *Engine) isRecovering(id string) bool {
	e.recoveryMu.Lock()
	defer e.recoveryMu.Unlock()
	return e.recovering[id]
}

func (e *Engine) scheduleRecovery(ctx context.Context, task recoveryTask, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	if delay == 0 {
		if ctx.Err() != nil || !e.isCurrentRecovery(task) {
			e.clearRecoveryTask(task)
			return
		}
		if !e.enqueueRecovery(ctx, task) {
			e.clearRecoveryTask(task)
			e.bus.Add(events.Event{Kind: "recovery_queue_full", Deployment: task.id, Message: "bounded recovery queue is full; background sweep will retry", ErrorType: "recovery_queue_full"})
		}
		return
	}

	e.recoveryMu.Lock()
	if !e.recovering[task.id] || e.recoveryGenerations[task.id] != task.generation || ctx.Err() != nil {
		e.recoveryMu.Unlock()
		e.clearRecoveryTask(task)
		return
	}
	if old := e.recoveryTimers[task.id]; old != nil {
		old.Stop()
	}
	e.recoverySeq++
	token := e.recoverySeq
	e.recoveryTokens[task.id] = token
	timer := time.AfterFunc(delay, func() {
		e.recoveryMu.Lock()
		if !e.recovering[task.id] ||
			e.recoveryGenerations[task.id] != task.generation ||
			e.recoveryTokens[task.id] != token {
			e.recoveryMu.Unlock()
			return
		}
		delete(e.recoveryTimers, task.id)
		delete(e.recoveryTokens, task.id)
		e.recoveryMu.Unlock()

		if ctx.Err() != nil {
			e.clearRecoveryTask(task)
			return
		}
		if !e.enqueueRecovery(ctx, task) {
			e.clearRecoveryTask(task)
			e.bus.Add(events.Event{Kind: "recovery_queue_full", Deployment: task.id, Message: "bounded recovery queue is full; background sweep will retry", ErrorType: "recovery_queue_full"})
		}
	})
	e.recoveryTimers[task.id] = timer
	e.recoveryMu.Unlock()
}

func (e *Engine) recoveryWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-e.recoveryQueue:
			if !e.isCurrentRecovery(task) {
				continue
			}
			e.processRecoveryTask(ctx, task)
		}
	}
}

func (e *Engine) processRecoveryTask(ctx context.Context, task recoveryTask) {
	if ctx.Err() != nil {
		e.clearRecoveryTask(task)
		return
	}
	cfg := e.current()
	d, a, ok := e.deployment(task.id)
	if !ok || d.Identity != task.identity {
		e.clearRecoveryTask(task)
		return
	}

	st := e.hm.Get(task.id)
	if st.Status == health.Retired || (st.Identity != "" && st.Identity != task.identity) {
		e.clearRecoveryTask(task)
		return
	}
	if st.Status == health.Healthy && !st.Quarantined {
		e.clearRecoveryTask(task)
		return
	}
	if st.Status == health.Cooldown && !st.CooldownUntil.IsZero() {
		if wait := time.Until(st.CooldownUntil); wait > 0 {
			e.bus.Add(events.Event{Kind: "recovery_wait", Deployment: task.id, Message: fmt.Sprintf("cooldown until %s", st.CooldownUntil.Format(time.RFC3339))})
			task.attempt = 1
			e.scheduleRecovery(ctx, task, wait)
			return
		}
	}

	attempts := cfg.Probe.RecoveryAttempts
	if attempts < 1 {
		attempts = 5
	}
	if task.attempt < 1 {
		task.attempt = 1
	}
	if task.attempt > attempts {
		task.attempt = attempts
	}

	// Re-resolve immediately before the probe so hot reloads never keep a stale
	// provider/model pointer in a queued recovery task.
	d, a, ok = e.deployment(task.id)
	if !ok || d.Identity != task.identity {
		e.clearRecoveryTask(task)
		return
	}
	cfg = e.current()
	if !e.acquireProbe(ctx, cfg.Probe.Concurrency) {
		e.clearRecoveryTask(task)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout())
	lat, status, err := a.Probe(pctx, d.Model, cfg.Probe.MaxTokens)
	cancel()
	e.releaseProbe()

	if ctx.Err() != nil {
		e.clearRecoveryTask(task)
		return
	}
	currentDeployment, stillConfigured := e.rt.Deployment(task.id)
	currentHealth := e.hm.Get(task.id)
	if !stillConfigured || currentDeployment.Identity != task.identity || currentHealth.Status == health.Retired ||
		(currentHealth.Identity != "" && currentHealth.Identity != task.identity) {
		e.clearRecoveryTask(task)
		return
	}
	if err == nil {
		if e.hm.RecordRecoverySuccessForIdentity(task.id, task.identity, st.Revision, lat) {
			e.hm.RecordProviderSuccess(d.ProviderID)
			e.bus.Add(events.Event{Kind: "recovery_ready", Deployment: task.id, Message: fmt.Sprintf("recovered on attempt %d/%d", task.attempt, attempts),
				SupervisorState: "ready", LatencyMS: lat.Milliseconds(), StatusCode: status})
			e.clearRecoveryTask(task)
			return
		}
		currentHealth = e.hm.Get(task.id)
		currentDeployment, stillConfigured = e.rt.Deployment(task.id)
		if !stillConfigured || currentDeployment.Identity != task.identity || currentHealth.Status == health.Retired ||
			(currentHealth.Identity != "" && currentHealth.Identity != task.identity) {
			e.clearRecoveryTask(task)
			return
		}
		if currentHealth.Status == health.Healthy && !currentHealth.Quarantined {
			e.clearRecoveryTask(task)
			return
		}
		task.attempt = 1
		delay := cfg.ProbeRecoveryRetry()
		if currentHealth.Status == health.Cooldown && !currentHealth.CooldownUntil.IsZero() {
			if wait := time.Until(currentHealth.CooldownUntil); wait > 0 {
				delay = wait
			}
		}
		e.bus.Add(events.Event{Kind: "recovery_stale", Deployment: task.id,
			Message:      "recovery success was superseded by newer deployment health evidence",
			FailureClass: currentHealth.LastErrorClass, SupervisorState: "recovering", StatusCode: status})
		e.scheduleRecovery(ctx, task, delay)
		return
	}

	classified := classifyProbeFailure(status, err)
	if classified.Class == compat.ClassModelRetired {
		reason := safeProbeFailure(classified)
		if e.hm.RetireForIdentity(task.id, task.identity, reason, string(classified.Class)) {
			e.bus.Add(events.Event{Kind: "supervisor_state", Deployment: task.id, Message: "deployment permanently retired by model lifecycle evidence",
				ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "retired", SupervisorTerminal: true, StatusCode: status})
		}
		e.clearRecoveryTask(task)
		return
	}
	if classified.CapabilityFailure || classified.CallerError || classified.Class == compat.ClassContextOverflow {
		e.bus.Add(events.Event{Kind: "recovery_inconclusive", Deployment: task.id,
			Message:   "recovery probe request was rejected; deployment remains quarantined",
			ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "quarantined", StatusCode: status})
		e.clearRecoveryTask(task)
		return
	}

	lastErr := safeProbeFailure(classified)
	if wait, ok := providers.RetryAfter(err); ok {
		if providerIncidentProbeFailure(status) {
			e.hm.RecordProviderFailure(d.ProviderID, task.id, lastErr)
		}
		maxWait := time.Duration(cfg.Routing.MaxRetryAfterSeconds) * time.Second
		if maxWait > 0 && wait > maxWait {
			wait = maxWait
		}
		e.hm.EnterCooldownWithClassForIdentity(task.id, task.identity, lastErr, string(classified.Class), wait)
		e.bus.Add(events.Event{Kind: "recovery_deferred", Deployment: task.id, Message: fmt.Sprintf("provider cooldown; retry after %s", wait),
			ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "cooldown", StatusCode: status})
		e.scheduleRecovery(ctx, task, wait)
		return
	}

	if providerIncidentProbeFailure(status) {
		e.hm.RecordProviderFailure(d.ProviderID, task.id, lastErr)
	}
	e.hm.RecordRecoveryFailureClassForIdentity(task.id, task.identity, lastErr, string(classified.Class), lat)
	e.bus.Add(events.Event{Kind: "recovery_fail", Deployment: task.id, Message: fmt.Sprintf("recovery attempt %d/%d failed", task.attempt, attempts),
		ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "recovering", SupervisorAttempt: task.attempt,
		LatencyMS: lat.Milliseconds(), StatusCode: status})

	if task.attempt < attempts {
		task.attempt++
		e.scheduleRecovery(ctx, task, cfg.ProbeRecoveryRetry())
		return
	}

	cooldown := cfg.Cooldown()
	e.hm.EnterCooldownWithClassForIdentity(task.id, task.identity, lastErr, string(classified.Class), cooldown)
	e.bus.Add(events.Event{Kind: "recovery_cooldown", Deployment: task.id, Message: fmt.Sprintf("%d recovery attempts failed; retry after %s", attempts, cooldown), ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "cooldown", SupervisorAttempt: attempts, LatencyMS: lat.Milliseconds(), StatusCode: status})
	task.attempt = 1
	e.scheduleRecovery(ctx, task, cooldown)
}

func (e *Engine) acquireProbe(ctx context.Context, limit int) bool {
	if limit < 1 {
		limit = 1
	}
	for {
		e.limitMu.Lock()
		if e.activeProbes < limit {
			e.activeProbes++
			e.limitMu.Unlock()
			return true
		}
		ch := e.limitChanged
		e.limitMu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
}

func (e *Engine) releaseProbe() {
	e.limitMu.Lock()
	if e.activeProbes > 0 {
		e.activeProbes--
	}
	close(e.limitChanged)
	e.limitChanged = make(chan struct{})
	e.limitMu.Unlock()
}

func (e *Engine) deployment(id string) (router.Deployment, providers.Adapter, bool) {
	d, ok := e.rt.Deployment(id)
	if !ok {
		return router.Deployment{}, nil, false
	}
	a, ok := e.reg.Get(d.ProviderID)
	return d, a, ok
}

func readyLeaseExpired(st health.State, now time.Time, lease time.Duration) bool {
	if lease <= 0 || st.LastChecked.IsZero() {
		return true
	}
	return !now.Before(st.LastChecked.Add(lease))
}

// SetCapabilityStore wires the shared capability contract store. When set
// and probe.capability_probes is enabled, a successful Level A availability
// probe triggers a one-time Level B compatibility suite for the deployment
// (openai-compatible dialects natively; anthropic-compatible via their own
// payload shapes). Results are cached in the store, so each deployment pays
// the suite cost once per identity.
func (e *Engine) SetCapabilityStore(store *compat.Store) {
	e.capStore = store
}

func (e *Engine) maybeProbeCapabilities(ctx context.Context, d router.Deployment, a providers.Adapter) {
	if e.capStore == nil {
		return
	}
	cfg := e.current()
	if !cfg.Probe.CapabilityProbes {
		return
	}
	if contract := e.capStore.Get(d.ID); contract.Capabilities.Text == compat.Supported {
		return // already verified
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	t := adapterTransport{a: a}
	var report compat.ProbeReport
	switch d.ProviderType {
	case "anthropic_compatible":
		report = compat.RunCapabilitySuiteAnthropic(cctx, t, d.ID, d.Model)
	case "openai_responses":
		report = compat.RunCapabilitySuiteResponses(cctx, t, d.ID, d.Model)
	default:
		report = compat.RunCapabilitySuite(cctx, t, d.ID, d.Model, "")
	}
	for _, o := range report.Outcomes {
		switch o.Verdict {
		case compat.Supported:
			e.capStore.LearnSuccess(d.ID, o.Capability, compat.SourceProbe, o.Detail, "")
		case compat.Unsupported:
			e.capStore.LearnUnsupported(d.ID, o.Capability, compat.SourceProbe, o.Detail, "")
		}
	}
	e.bus.Add(events.Event{
		Kind: "capability_probe", Deployment: d.ID,
		Message: fmt.Sprintf("capability suite passed=%d failed=%d inconclusive=%d", report.Passed, report.Failed, report.Inconclusive),
	})
}

// adapterTransport adapts a providers.Adapter onto the probe transport.
type adapterTransport struct {
	a providers.Adapter
}

func (t adapterTransport) Do(ctx context.Context, payload []byte, stream bool, forward http.Header) (*http.Response, error) {
	return t.a.Do(ctx, payload, stream, forward)
}

func (t adapterTransport) RedactBody(b []byte) []byte {
	return t.a.RedactBody(b)
}

// RunOnce performs an explicit operator-requested sweep. Background ready-queue
// sweeps avoid fresh ready deployments, but revalidate an idle healthy
// deployment once its health lease expires. Real successful Claude traffic
// refreshes LastChecked, so actively used models normally need no synthetic
// probe. Quarantined/cooldown deployments remain recovery-supervisor owned.
func (e *Engine) RunOnce(ctx context.Context) Result {
	return e.runOnce(ctx, true)
}

func (e *Engine) runOnce(ctx context.Context, force bool) Result {
	start := time.Now()
	e.runMu.Lock()
	defer e.runMu.Unlock()

	cfg := e.current()
	readySupervisor := router.IsReadyStrategy(cfg.Routing.Strategy)
	readyLease := cfg.ProbeReadyLease()
	sweepNow := time.Now()
	result := Result{}
	if !force && !cfg.Probe.Enabled {
		result.DurationMS = time.Since(start).Milliseconds()
		return result
	}

	type probeJob struct {
		d     router.Deployment
		state health.State
	}
	jobs := make([]probeJob, 0)
	for _, d := range e.rt.All() {
		jobs = append(jobs, probeJob{d: d, state: e.hm.Get(d.ID)})
	}
	result.Total = len(jobs)
	probeRank := func(st health.Status) int {
		switch st {
		case health.HalfOpen:
			return 0
		case health.Unknown:
			return 1
		case health.Degraded:
			return 2
		case health.Healthy:
			return 3
		case health.Cooldown:
			return 4
		case health.Retired:
			return 5
		default:
			return 3
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		ri, rj := probeRank(jobs[i].state.Status), probeRank(jobs[j].state.Status)
		if ri != rj {
			return ri < rj
		}
		li, lj := jobs[i].state.LastChecked, jobs[j].state.LastChecked
		if li.IsZero() != lj.IsZero() {
			return li.IsZero()
		}
		if !li.Equal(lj) {
			return li.Before(lj)
		}
		return jobs[i].d.ID < jobs[j].d.ID
	})

	var wg sync.WaitGroup
	var resultMu sync.Mutex
	failedIDs := make([]string, 0)
	for idx, job := range jobs {
		d := job.d
		if job.state.Status == health.Retired {
			result.SkippedRetired++
			continue
		}
		if job.state.Quarantined {
			if job.state.Status == health.Cooldown {
				result.SkippedCooldown++
			} else {
				result.SkippedRecovery++
			}
			e.Recover(d.ID)
			continue
		}
		if !force && readySupervisor {
			switch job.state.Status {
			case health.Healthy:
				if !readyLeaseExpired(job.state, sweepNow, readyLease) {
					result.SkippedReady++
					continue
				}
			case health.Cooldown:
				result.SkippedCooldown++
				continue
			case health.Degraded, health.HalfOpen:
				result.SkippedRecovery++
				e.Recover(d.ID)
				continue
			}
		} else if job.state.Status == health.Cooldown {
			result.SkippedCooldown++
			continue
		}
		if readySupervisor && e.isRecovering(d.ID) {
			result.SkippedRecovery++
			continue
		}
		a, ok := e.reg.Get(d.ProviderID)
		if !ok {
			result.SkippedMissing++
			continue
		}

		if !e.acquireProbe(ctx, cfg.Probe.Concurrency) {
			if ctx.Err() != nil {
				resultMu.Lock()
				result.Canceled += len(jobs) - idx
				resultMu.Unlock()
				break
			}
			resultMu.Lock()
			result.Failed++
			resultMu.Unlock()
			continue
		}
		wg.Add(1)
		go func(d router.Deployment, a providers.Adapter) {
			defer wg.Done()
			defer e.releaseProbe()
			pctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout())
			lat, status, err := a.Probe(pctx, d.Model, cfg.Probe.MaxTokens)
			cancel()

			if ctx.Err() != nil {
				resultMu.Lock()
				result.Canceled++
				resultMu.Unlock()
				return
			}
			current, configured := e.rt.Deployment(d.ID)
			state := e.hm.Get(d.ID)
			if !configured || current.Identity != d.Identity || state.Status == health.Retired ||
				(state.Identity != "" && state.Identity != d.Identity) {
				resultMu.Lock()
				if state.Status == health.Retired {
					result.SkippedRetired++
				} else {
					result.SkippedRecovery++
				}
				resultMu.Unlock()
				return
			}

			if err != nil {
				classified := classifyProbeFailure(status, err)
				reason := safeProbeFailure(classified)
				policy := classified.Policy()
				if classified.CapabilityFailure || classified.CallerError || classified.Class == compat.ClassContextOverflow {
					e.bus.Add(events.Event{Kind: "probe_inconclusive", Deployment: d.ID,
						Message:   "probe request was rejected; deployment health remains unchanged",
						ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "unchanged", StatusCode: status})
					resultMu.Lock()
					result.Failed++
					resultMu.Unlock()
					return
				}
				if providerIncidentProbeFailure(status) {
					e.hm.RecordProviderFailure(d.ProviderID, d.ID, reason)
				}
				if policy.RetireDeployment {
					if e.hm.RetireForIdentity(d.ID, d.Identity, reason, string(classified.Class)) {
						e.bus.Add(events.Event{Kind: "supervisor_state", Deployment: d.ID, Message: "deployment permanently retired by model lifecycle evidence",
							ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "retired", SupervisorTerminal: true, StatusCode: status})
					}
				} else {
					retryWait, hasRetryWait := providers.RetryAfter(err)
					if classified.Class == compat.ClassRateLimit && !hasRetryWait {
						retryWait, hasRetryWait = cfg.Cooldown(), true
					}
					if hasRetryWait {
						maxWait := time.Duration(cfg.Routing.MaxRetryAfterSeconds) * time.Second
						if retryWait <= 0 {
							retryWait = time.Second
						}
						if maxWait > 0 && retryWait > maxWait {
							retryWait = maxWait
						}
						e.hm.ForceCooldownWithClassForIdentity(d.ID, d.Identity, reason, string(classified.Class), retryWait)
						e.bus.Add(events.Event{Kind: "probe_deferred", Deployment: d.ID, Message: "probe deferred by bounded provider cooldown",
							ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "cooldown", StatusCode: status})
						resultMu.Lock()
						failedIDs = append(failedIDs, d.ID)
						resultMu.Unlock()
					} else if classified.Class == compat.ClassModelTemporarilyUnavailable ||
						(readySupervisor && policy.QuarantineDeployment) {
						e.hm.QuarantineWithClassForIdentity(d.ID, d.Identity, reason, string(classified.Class), lat)
						e.bus.Add(events.Event{Kind: "probe_quarantine", Deployment: d.ID, Message: "deployment removed from routing pending supervised recovery",
							ErrorType: string(classified.Class), FailureClass: string(classified.Class), SupervisorState: "quarantined", StatusCode: status})
						resultMu.Lock()
						failedIDs = append(failedIDs, d.ID)
						resultMu.Unlock()
					} else if policy.HardCooldown {
						cooldown := cfg.Cooldown()
						e.hm.ForceCooldownWithClassForIdentity(d.ID, d.Identity, reason, string(classified.Class), cooldown)
						resultMu.Lock()
						failedIDs = append(failedIDs, d.ID)
						resultMu.Unlock()
					} else if policy.QuarantineDeployment {
						e.hm.RecordFailureForIdentity(d.ID, d.Identity, reason, lat)
					} else {
						e.hm.RecordFailureForIdentity(d.ID, d.Identity, reason, lat)
					}
				}
				e.bus.Add(events.Event{Kind: "probe_fail", Deployment: d.ID, Message: "probe failed; classified for routing policy",
					ErrorType: string(classified.Class), FailureClass: string(classified.Class), LatencyMS: lat.Milliseconds(), StatusCode: status})
				resultMu.Lock()
				result.Failed++
				resultMu.Unlock()
				return
			}

			e.hm.RecordSuccessForIdentity(d.ID, d.Identity, lat)
			e.hm.RecordProviderSuccess(d.ProviderID)
			e.bus.Add(events.Event{Kind: "probe_ready", Deployment: d.ID, Message: fmt.Sprintf("ready after probe (%d)", status), SupervisorState: "ready", LatencyMS: lat.Milliseconds(), StatusCode: status})
			e.maybeProbeCapabilities(ctx, d, a)
			resultMu.Lock()
			result.Passed++
			resultMu.Unlock()
		}(d, a)
	}
	wg.Wait()
	// Let every deployment receive its first health check before failed models
	// consume probe capacity with recovery retries.
	for _, id := range failedIDs {
		e.Recover(id)
	}
	result.DurationMS = time.Since(start).Milliseconds()
	return result
}

package probe

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

type Result struct {
	Total           int   `json:"total"`
	Passed          int   `json:"passed"`
	Failed          int   `json:"failed"`
	SkippedCooldown int   `json:"skipped_cooldown"`
	SkippedMissing  int   `json:"skipped_missing_adapter"`
	SkippedRecovery int   `json:"skipped_recovery"`
	SkippedReady    int   `json:"skipped_ready"`
	DurationMS      int64 `json:"duration_ms"`
}

const (
	recoveryWorkerCount = 64
	maxRecoveryQueue     = 20000
)

type recoveryTask struct {
	id      string
	attempt int
}

type Engine struct {
	cfgMu sync.RWMutex
	cfg   config.Config
	reg   *providers.Registry
	rt    *router.Router
	hm    *health.Manager
	bus   *events.Bus

	trigger chan struct{}
	runMu   sync.Mutex
	primeMu sync.Mutex
	primed  bool

	ctxMu  sync.RWMutex
	runCtx context.Context

	recoveryMu      sync.Mutex
	recovering      map[string]bool
	recoveryQueue   chan recoveryTask
	recoveryWorkers sync.Once

	limitMu      sync.Mutex
	activeProbes int
	limitChanged chan struct{}
}

func New(cfg config.Config, reg *providers.Registry, rt *router.Router, hm *health.Manager, bus *events.Bus) *Engine {
	return &Engine{
		cfg:          cfg,
		reg:          reg,
		rt:           rt,
		hm:           hm,
		bus:          bus,
		trigger:      make(chan struct{}, 1),
		recovering:   map[string]bool{},
		recoveryQueue: make(chan recoveryTask, maxRecoveryQueue),
		limitChanged: make(chan struct{}),
	}
}

func (e *Engine) Reload(cfg config.Config) {
	e.cfgMu.Lock()
	e.cfg = cfg
	e.cfgMu.Unlock()
	if !router.IsReadyStrategy(cfg.Routing.Strategy) {
		e.recoveryMu.Lock()
		e.recovering = map[string]bool{}
		e.recoveryMu.Unlock()
	}

	// Wake probe workers so a raised concurrency limit takes effect promptly.
	e.limitMu.Lock()
	close(e.limitChanged)
	e.limitChanged = make(chan struct{})
	e.limitMu.Unlock()

	e.Trigger()
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

func (e *Engine) Start(ctx context.Context) {
	e.setRunContext(ctx)
	e.recoveryWorkers.Do(func() {
		for i := 0; i < recoveryWorkerCount; i++ {
			go e.recoveryWorker(ctx)
		}
	})
	go e.Run(ctx)
}

func (e *Engine) Run(ctx context.Context) {
	e.setRunContext(ctx)
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
	e.recoveryMu.Lock()
	if e.recovering[id] {
		e.recoveryMu.Unlock()
		return
	}
	e.recovering[id] = true
	e.recoveryMu.Unlock()

	if !e.enqueueRecovery(ctx, recoveryTask{id: id, attempt: 1}) {
		e.clearRecovering(id)
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

func (e *Engine) clearRecovering(id string) {
	e.recoveryMu.Lock()
	delete(e.recovering, id)
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
	time.AfterFunc(delay, func() {
		if ctx.Err() != nil || !e.isRecovering(task.id) {
			e.clearRecovering(task.id)
			return
		}
		if !e.enqueueRecovery(ctx, task) {
			e.clearRecovering(task.id)
			e.bus.Add(events.Event{Kind: "recovery_queue_full", Deployment: task.id, Message: "bounded recovery queue is full; background sweep will retry", ErrorType: "recovery_queue_full"})
		}
	})
}

func (e *Engine) recoveryWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-e.recoveryQueue:
			if !e.isRecovering(task.id) {
				continue
			}
			e.processRecoveryTask(ctx, task)
		}
	}
}

func (e *Engine) processRecoveryTask(ctx context.Context, task recoveryTask) {
	if ctx.Err() != nil {
		e.clearRecovering(task.id)
		return
	}
	cfg := e.current()
	if !router.IsReadyStrategy(cfg.Routing.Strategy) {
		e.clearRecovering(task.id)
		return
	}
	d, a, ok := e.deployment(task.id)
	if !ok {
		e.clearRecovering(task.id)
		return
	}

	st := e.hm.Get(task.id)
	if st.Status == health.Healthy {
		e.clearRecovering(task.id)
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
	if !ok {
		e.clearRecovering(task.id)
		return
	}
	cfg = e.current()
	if !e.acquireProbe(ctx, cfg.Probe.Concurrency) {
		e.clearRecovering(task.id)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout())
	lat, status, err := a.Probe(pctx, d.Model, cfg.Probe.MaxTokens)
	cancel()
	e.releaseProbe()

	if err == nil {
		e.hm.RecordSuccess(task.id, lat)
		e.bus.Add(events.Event{Kind: "recovery_ready", Deployment: task.id, Message: fmt.Sprintf("recovered on attempt %d/%d", task.attempt, attempts), LatencyMS: lat.Milliseconds(), StatusCode: status})
		e.clearRecovering(task.id)
		return
	}

	if wait, ok := providers.RetryAfter(err); ok {
		maxWait := time.Duration(cfg.Routing.MaxRetryAfterSeconds) * time.Second
		if maxWait > 0 && wait > maxWait {
			wait = maxWait
		}
		e.bus.Add(events.Event{Kind: "recovery_deferred", Deployment: task.id, Message: fmt.Sprintf("credential rate-limit cooldown; retry after %s", wait), StatusCode: status})
		e.scheduleRecovery(ctx, task, wait)
		return
	}

	lastErr := err.Error()
	e.hm.RecordRecoveryFailure(task.id, lastErr, lat)
	e.bus.Add(events.Event{Kind: "recovery_fail", Deployment: task.id, Message: fmt.Sprintf("attempt %d/%d: %s", task.attempt, attempts, lastErr), LatencyMS: lat.Milliseconds(), StatusCode: status})

	if task.attempt < attempts {
		task.attempt++
		e.scheduleRecovery(ctx, task, cfg.ProbeRecoveryRetry())
		return
	}

	cooldown := cfg.Cooldown()
	e.hm.EnterCooldown(task.id, lastErr, cooldown)
	e.bus.Add(events.Event{Kind: "recovery_cooldown", Deployment: task.id, Message: fmt.Sprintf("%d recovery attempts failed; retry after %s", attempts, cooldown), LatencyMS: lat.Milliseconds(), StatusCode: status})
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
	for _, job := range jobs {
		d := job.d
		result.Total++
		if !force && readySupervisor {
			switch job.state.Status {
			case health.Healthy:
				if !readyLeaseExpired(job.state, sweepNow, readyLease) {
					result.SkippedReady++
					continue
				}
			case health.Cooldown:
				result.SkippedCooldown++
				e.Recover(d.ID)
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

			if err != nil {
				if readySupervisor {
					e.hm.Quarantine(d.ID, err.Error(), lat)
					e.bus.Add(events.Event{Kind: "probe_quarantine", Deployment: d.ID, Message: err.Error(), LatencyMS: lat.Milliseconds(), StatusCode: status})
					resultMu.Lock()
					failedIDs = append(failedIDs, d.ID)
					resultMu.Unlock()
				} else {
					if status == 401 || status == 402 || status == 403 || status == 429 {
						cooldown := cfg.Cooldown()
						if status == 429 && cooldown > time.Minute {
							cooldown = time.Minute
						}
						e.hm.ForceCooldown(d.ID, err.Error(), cooldown)
					} else {
						e.hm.RecordFailure(d.ID, err.Error(), lat)
					}
					e.bus.Add(events.Event{Kind: "probe_fail", Deployment: d.ID, Message: err.Error(), LatencyMS: lat.Milliseconds(), StatusCode: status})
				}
				resultMu.Lock()
				result.Failed++
				resultMu.Unlock()
				return
			}

			e.hm.RecordSuccess(d.ID, lat)
			e.bus.Add(events.Event{Kind: "probe_ready", Deployment: d.ID, Message: fmt.Sprintf("ready after probe (%d)", status), LatencyMS: lat.Milliseconds(), StatusCode: status})
			resultMu.Lock()
			result.Passed++
			resultMu.Unlock()
		}(d, a)
	}
	wg.Wait()
	// Let every deployment receive its first health check before failed models
	// consume probe capacity with recovery retries.
	if readySupervisor {
		for _, id := range failedIDs {
			e.Recover(id)
		}
	}
	result.DurationMS = time.Since(start).Milliseconds()
	return result
}


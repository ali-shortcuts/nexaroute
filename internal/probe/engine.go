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
	Total            int   `json:"total"`
	Passed           int   `json:"passed"`
	Failed           int   `json:"failed"`
	SkippedCooldown  int   `json:"skipped_cooldown"`
	SkippedMissing   int   `json:"skipped_missing_adapter"`
	SkippedRecovery  int   `json:"skipped_recovery"`
	DurationMS       int64 `json:"duration_ms"`
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

	recoveryMu sync.Mutex
	recovering map[string]bool

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
		limitChanged: make(chan struct{}),
	}
}

func (e *Engine) Reload(cfg config.Config) {
	e.cfgMu.Lock()
	e.cfg = cfg
	e.cfgMu.Unlock()

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

// Recover hands one quarantined deployment to the background supervisor.
// Duplicate recovery loops for the same deployment are suppressed.
func (e *Engine) Recover(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	ctx := e.context()
	if ctx == nil {
		return
	}
	e.recoveryMu.Lock()
	if e.recovering[id] {
		e.recoveryMu.Unlock()
		return
	}
	e.recovering[id] = true
	e.recoveryMu.Unlock()

	go func() {
		defer func() {
			e.recoveryMu.Lock()
			delete(e.recovering, id)
			e.recoveryMu.Unlock()
		}()
		e.recoverLoop(ctx, id)
	}()
}

func (e *Engine) isRecovering(id string) bool {
	e.recoveryMu.Lock()
	defer e.recoveryMu.Unlock()
	return e.recovering[id]
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
	for _, d := range e.rt.All() {
		if d.ID != id {
			continue
		}
		a, ok := e.reg.Get(d.ProviderID)
		return d, a, ok
	}
	return router.Deployment{}, nil, false
}

// RunOnce performs one parallel background sweep. Healthy deployments are
// continuously revalidated. Unknown deployments need one successful probe
// before they enter the ready queue. A failed sweep probe immediately
// quarantines the deployment and starts its dedicated five-attempt recovery
// lifecycle.
func (e *Engine) RunOnce(ctx context.Context) Result {
	return e.runOnce(ctx, true)
}

func (e *Engine) runOnce(ctx context.Context, force bool) Result {
	start := time.Now()
	e.runMu.Lock()
	defer e.runMu.Unlock()

	cfg := e.current()
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
	for _, job := range jobs {
		d := job.d
		result.Total++
		if job.state.Status == health.Cooldown {
			result.SkippedCooldown++
			continue
		}
		if e.isRecovering(d.ID) {
			result.SkippedRecovery++
			continue
		}
		a, ok := e.reg.Get(d.ProviderID)
		if !ok {
			result.SkippedMissing++
			continue
		}

		wg.Add(1)
		go func(d router.Deployment, a providers.Adapter) {
			defer wg.Done()
			if !e.acquireProbe(ctx, cfg.Probe.Concurrency) {
				resultMu.Lock()
				result.Failed++
				resultMu.Unlock()
				return
			}
			pctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout())
			lat, status, err := a.Probe(pctx, d.Model, cfg.Probe.MaxTokens)
			cancel()
			e.releaseProbe()

			if err != nil {
				e.hm.Quarantine(d.ID, err.Error(), lat)
				e.bus.Add(events.Event{Kind: "probe_quarantine", Deployment: d.ID, Message: err.Error(), LatencyMS: lat.Milliseconds(), StatusCode: status})
				resultMu.Lock()
				result.Failed++
				resultMu.Unlock()
				e.Recover(d.ID)
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
	result.DurationMS = time.Since(start).Milliseconds()
	return result
}

// recoverLoop owns the lifecycle of one quarantined deployment:
// five supervisor probes -> immediate return on first success -> 30 minute
// cooldown after all attempts fail -> repeat after cooldown until healthy or
// removed from configuration.
func (e *Engine) recoverLoop(ctx context.Context, id string) {
	for {
		if ctx.Err() != nil {
			return
		}
		cfg := e.current()
		d, a, ok := e.deployment(id)
		if !ok {
			return
		}

		st := e.hm.Get(id)
		if st.Status == health.Healthy {
			return
		}
		if st.Status == health.Cooldown && !st.CooldownUntil.IsZero() {
			wait := time.Until(st.CooldownUntil)
			if wait > 0 {
				e.bus.Add(events.Event{Kind: "recovery_wait", Deployment: id, Message: fmt.Sprintf("cooldown until %s", st.CooldownUntil.Format(time.RFC3339))})
				t := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
		}

		attempts := cfg.Probe.RecoveryAttempts
		if attempts < 1 {
			attempts = 5
		}
		var lastErr string
		var lastStatus int
		var lastLatency time.Duration

		for attempt := 1; attempt <= attempts; attempt++ {
			if ctx.Err() != nil {
				return
			}
			// Re-resolve adapter/model on every attempt so hot reloads are safe.
			d, a, ok = e.deployment(id)
			if !ok {
				return
			}
			cfg = e.current()
			if !e.acquireProbe(ctx, cfg.Probe.Concurrency) {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout())
			lat, status, err := a.Probe(pctx, d.Model, cfg.Probe.MaxTokens)
			cancel()
			e.releaseProbe()
			lastLatency, lastStatus = lat, status

			if err == nil {
				e.hm.RecordSuccess(id, lat)
				e.bus.Add(events.Event{Kind: "recovery_ready", Deployment: id, Message: fmt.Sprintf("recovered on attempt %d/%d", attempt, attempts), LatencyMS: lat.Milliseconds(), StatusCode: status})
				return
			}

			lastErr = err.Error()
			e.hm.RecordRecoveryFailure(id, lastErr, lat)
			e.bus.Add(events.Event{Kind: "recovery_fail", Deployment: id, Message: fmt.Sprintf("attempt %d/%d: %s", attempt, attempts, lastErr), LatencyMS: lat.Milliseconds(), StatusCode: status})
			if attempt < attempts {
				delay := cfg.ProbeRecoveryRetry()
				if delay > 0 {
					t := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						t.Stop()
						return
					case <-t.C:
					}
				}
			}
		}

		if lastErr == "" {
			lastErr = "recovery attempts exhausted"
		}
		cooldown := cfg.Cooldown()
		e.hm.EnterCooldown(id, lastErr, cooldown)
		e.bus.Add(events.Event{Kind: "recovery_cooldown", Deployment: id, Message: fmt.Sprintf("%d recovery attempts failed; retry after %s", attempts, cooldown), LatencyMS: lastLatency.Milliseconds(), StatusCode: lastStatus})

		t := time.NewTimer(cooldown)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		// The next loop re-resolves the deployment and starts a fresh recovery
		// budget. health.Get transitions the expired cooldown to half-open.
	}
}

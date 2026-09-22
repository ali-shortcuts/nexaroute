package probe

import (
	"context"
	"fmt"
	"sort"
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
	DurationMS      int64 `json:"duration_ms"`
}

type Engine struct {
	cfgMu   sync.RWMutex
	cfg     config.Config
	reg     *providers.Registry
	rt      *router.Router
	hm      *health.Manager
	bus     *events.Bus
	trigger chan struct{}
	runMu   sync.Mutex
}

func New(cfg config.Config, reg *providers.Registry, rt *router.Router, hm *health.Manager, bus *events.Bus) *Engine {
	return &Engine{cfg: cfg, reg: reg, rt: rt, hm: hm, bus: bus, trigger: make(chan struct{}, 1)}
}
func (e *Engine) Reload(cfg config.Config) {
	e.cfgMu.Lock()
	e.cfg = cfg
	e.cfgMu.Unlock()
	e.Trigger()
}
func (e *Engine) current() config.Config { e.cfgMu.RLock(); defer e.cfgMu.RUnlock(); return e.cfg }
func (e *Engine) Trigger() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}
func (e *Engine) Run(ctx context.Context) {
	cfg := e.current()
	if cfg.Probe.Enabled && cfg.Probe.OnStart {
		go e.RunOnce(ctx)
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
				<-timer.C
			}
			return
		case <-timer.C:
			e.RunOnce(ctx)
		case <-e.trigger:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			e.RunOnce(ctx)
		}
	}
}

// RunOnce probes every currently eligible deployment exactly once, bounded by
// the configured probe concurrency. runMu prevents a manual probe and a
// scheduled probe from doubling traffic at the same time.
func (e *Engine) RunOnce(ctx context.Context) Result {
	start := time.Now()
	e.runMu.Lock()
	defer e.runMu.Unlock()

	cfg := e.current()
	result := Result{}
	if !cfg.Probe.Enabled {
		result.DurationMS = time.Since(start).Milliseconds()
		return result
	}
	limit := cfg.Probe.Concurrency
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var resultMu sync.Mutex

	type probeJob struct {
		d     router.Deployment
		state health.State
	}
	jobs := make([]probeJob, 0)
	for _, d := range e.rt.All() {
		jobs = append(jobs, probeJob{d: d, state: e.hm.Get(d.ID)})
	}
	// Probe recovery-sensitive deployments first. This is a bounded priority
	// work queue for each cycle: half-open -> unknown -> degraded -> healthy,
	// with the stalest state first inside each band. Cooldown deployments stay
	// quarantined until their deadline expires and then re-enter as half-open.
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

	for _, job := range jobs {
		d := job.d
		result.Total++
		if job.state.Status == health.Cooldown {
			result.SkippedCooldown++
			continue
		}
		a, ok := e.reg.Get(d.ProviderID)
		if !ok {
			result.SkippedMissing++
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			resultMu.Lock()
			result.Failed++
			resultMu.Unlock()
			continue
		}
		wg.Add(1)
		go func(d router.Deployment, a providers.Adapter) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout())
			defer cancel()
			lat, status, err := a.Probe(pctx, d.Model, cfg.Probe.MaxTokens)
			if err != nil {
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
				resultMu.Lock()
				result.Failed++
				resultMu.Unlock()
				return
			}
			e.hm.RecordSuccess(d.ID, lat)
			e.bus.Add(events.Event{Kind: "probe_ok", Deployment: d.ID, Message: fmt.Sprintf("probe ok (%d)", status), LatencyMS: lat.Milliseconds(), StatusCode: status})
			resultMu.Lock()
			result.Passed++
			resultMu.Unlock()
		}(d, a)
	}
	wg.Wait()
	result.DurationMS = time.Since(start).Milliseconds()
	return result
}

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// hedgeCall bundles everything the route loop knows at the upstream Do call
// site. The primary candidate is already validated and its payload built;
// buildPayload rebuilds the equivalent payload for the backup candidate only
// when a hedge actually fires, so translation work is never wasted.
type hedgeCall struct {
	routeCtx       context.Context
	streaming      bool
	attemptTimeout time.Duration
	candidates     []router.Scored
	from           int
	primary        router.Scored
	primaryAdapter providers.Adapter
	primaryPayload []byte
	req            router.Requirement
	buildPayload   func(router.Scored) ([]byte, error)
	forward        http.Header
	requestID      string
	strategy       string
	delay          time.Duration
	maxAttempts    int
	attempts       int
}

// hedgeOutcome is the winner of a hedged (or plain) upstream attempt. cancel
// is the winner's per-attempt cancel and must be released by the caller
// exactly like the non-hedged path; it is nil when no per-attempt context
// was created.
type hedgeOutcome struct {
	candidate     router.Scored
	adapter       providers.Adapter
	cancel        context.CancelFunc
	resp          *http.Response
	err           error
	extraAttempts int
	hedged        bool
}

type hedgeResult struct {
	resp    *http.Response
	err     error
	latency time.Duration
}

func doHedgeAttempt(ch chan<- hedgeResult, a providers.Adapter, ctx context.Context, payload []byte, streaming bool, forward http.Header) {
	start := time.Now()
	resp, err := a.Do(ctx, payload, streaming, forward)
	ch <- hedgeResult{resp: resp, err: err, latency: time.Since(start)}
}

// raceAttemptContext layers an independently-cancellable context over the
// per-attempt context so the race loser can always be cancelled — even when
// no per-attempt timeout is configured (streaming requests, or
// attemptTimeout <= 0, where attemptContext returns a nil cancel). The
// returned cancel is never nil.
func raceAttemptContext(routeCtx context.Context, streaming bool, attemptTimeout time.Duration) (context.Context, context.CancelFunc) {
	attemptCtx, attemptCancel := attemptContext(routeCtx, streaming, attemptTimeout)
	ctx, raceCancel := context.WithCancel(attemptCtx)
	return ctx, func() {
		raceCancel()
		if attemptCancel != nil {
			attemptCancel()
		}
	}
}

// hedgedDo performs one upstream attempt, racing a backup attempt on the
// next eligible candidate when the primary is slower than delay. The first
// completed Do wins, the loser is cancelled and its body closed, and the
// winner flows through the normal single-attempt handling unchanged.
//
// When hedging is disabled (delay <= 0), no backup candidate exists, the
// attempt budget is spent, or the retry budget denies the extra load, this
// degrades to exactly one synchronous primary Do.
func (s *Server) hedgedDo(hc hedgeCall) hedgeOutcome {
	plain := func() hedgeOutcome {
		ctx, cancel := attemptContext(hc.routeCtx, hc.streaming, hc.attemptTimeout)
		resp, err := hc.primaryAdapter.Do(ctx, hc.primaryPayload, hc.streaming, hc.forward)
		return hedgeOutcome{candidate: hc.primary, adapter: hc.primaryAdapter, cancel: cancel, resp: resp, err: err}
	}
	if hc.delay <= 0 || hc.attempts >= hc.maxAttempts || hc.from+1 >= len(hc.candidates) {
		return plain()
	}

	pCtx, pCancel := raceAttemptContext(hc.routeCtx, hc.streaming, hc.attemptTimeout)
	primaryCh := make(chan hedgeResult, 1)
	go doHedgeAttempt(primaryCh, hc.primaryAdapter, pCtx, hc.primaryPayload, hc.streaming, hc.forward)

	timer := time.NewTimer(hc.delay)
	defer timer.Stop()
	select {
	case r := <-primaryCh:
		return hedgeOutcome{candidate: hc.primary, adapter: hc.primaryAdapter, cancel: pCancel, resp: r.resp, err: r.err}
	case <-timer.C:
	}
	// The primary may have finished between the timer firing and now; never
	// spend a backup attempt needlessly.
	select {
	case r := <-primaryCh:
		return hedgeOutcome{candidate: hc.primary, adapter: hc.primaryAdapter, cancel: pCancel, resp: r.resp, err: r.err}
	default:
	}

	fresh, backupAdapter, ok := s.currentRouteCandidate(hc.candidates[hc.from+1].Deployment.ID, hc.req)
	if !ok {
		return s.hedgeWaitPrimary(hc, pCancel, primaryCh)
	}
	backupPayload, err := hc.buildPayload(fresh)
	if err != nil {
		return s.hedgeWaitPrimary(hc, pCancel, primaryCh)
	}
	if !s.retryBudget.allowRetry() {
		return s.hedgeWaitPrimary(hc, pCancel, primaryCh)
	}
	bCtx, bCancel := raceAttemptContext(hc.routeCtx, hc.streaming, hc.attemptTimeout)
	backupCh := make(chan hedgeResult, 1)
	go doHedgeAttempt(backupCh, backupAdapter, bCtx, backupPayload, hc.streaming, hc.forward)
	s.bus.Add(events.Event{RequestID: hc.requestID, Kind: "route_attempt", Deployment: fresh.Deployment.ID, Message: "hedged backup attempt: primary exceeded backup delay"})

	select {
	case r := <-primaryCh:
		bCancel()
		loser := <-backupCh
		s.closeHedgeLoser(loser)
		s.recordHedgeLoser(hc, fresh, loser)
		s.emitHedgeEvent(hc, hc.primary, fresh, r.latency, loser.latency)
		return hedgeOutcome{candidate: hc.primary, adapter: hc.primaryAdapter, cancel: pCancel, resp: r.resp, err: r.err, extraAttempts: 1, hedged: true}
	case r := <-backupCh:
		pCancel()
		loser := <-primaryCh
		s.closeHedgeLoser(loser)
		s.recordHedgeLoser(hc, hc.primary, loser)
		s.emitHedgeEvent(hc, fresh, hc.primary, r.latency, loser.latency)
		return hedgeOutcome{candidate: fresh, adapter: backupAdapter, cancel: bCancel, resp: r.resp, err: r.err, extraAttempts: 1, hedged: true}
	case <-hc.routeCtx.Done():
		pCancel()
		bCancel()
		primary := <-primaryCh
		backup := <-backupCh
		s.closeHedgeLoser(primary)
		s.closeHedgeLoser(backup)
		// Deterministic: the route loop reports and accounts the primary.
		return hedgeOutcome{candidate: hc.primary, adapter: hc.primaryAdapter, cancel: pCancel, resp: primary.resp, err: primary.err, extraAttempts: 1, hedged: true}
	}
}

// hedgeWaitPrimary falls back to the lone primary attempt when no backup
// could be launched. It keeps the race-free synchronous semantics.
func (s *Server) hedgeWaitPrimary(hc hedgeCall, pCancel context.CancelFunc, primaryCh <-chan hedgeResult) hedgeOutcome {
	r := <-primaryCh
	return hedgeOutcome{candidate: hc.primary, adapter: hc.primaryAdapter, cancel: pCancel, resp: r.resp, err: r.err}
}

func (s *Server) closeHedgeLoser(r hedgeResult) {
	if r.resp != nil && r.resp.Body != nil {
		r.resp.Body.Close()
	}
}

// recordHedgeLoser accounts a genuine loser signal. A loser that was merely
// slower carries no health information and is left alone; a loser that gave
// up on a saturated provider slot is a capacity signal, not a health signal,
// and is left alone as well. A loser that failed on its own (not via our
// race cancellation) is recorded exactly like a normal failed attempt so
// dashboards stay truthful.
func (s *Server) recordHedgeLoser(hc hedgeCall, loser router.Scored, r hedgeResult) {
	if r.err == nil || hc.routeCtx.Err() != nil || errors.Is(r.err, context.Canceled) || providers.IsSaturated(r.err) {
		return
	}
	msg := r.err.Error()
	if router.IsReadyStrategy(hc.strategy) {
		s.hm.Quarantine(loser.Deployment.ID, msg, r.latency)
		s.probe.Recover(loser.Deployment.ID)
	} else {
		s.hm.RecordFailure(loser.Deployment.ID, msg, r.latency)
	}
	s.bus.Add(events.Event{RequestID: hc.requestID, Kind: "route_fail", Deployment: loser.Deployment.ID, Message: msg, ErrorType: classifyTransportError(r.err), LatencyMS: r.latency.Milliseconds()})
}

func (s *Server) emitHedgeEvent(hc hedgeCall, winner, loser router.Scored, winnerLatency, loserLatency time.Duration) {
	s.bus.Add(events.Event{RequestID: hc.requestID, Kind: "hedge", Deployment: winner.Deployment.ID, Message: "backup attempt raced after delay; winner=" + winner.Deployment.ID + " loser=" + loser.Deployment.ID + " loser_ms=" + strconv.FormatInt(loserLatency.Milliseconds(), 10), LatencyMS: winnerLatency.Milliseconds()})
}

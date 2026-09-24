package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/translate"
)

// Request hedging (tail-latency race)
//
// A slow upstream start is one of the most damaging LLM-gateway failure
// modes: the request is neither failed nor failed-over, it just sits. Hedged
// routing bounds that damage. The gateway launches the primary attempt
// immediately; if response headers have not arrived within the configured
// delay, it launches a second attempt against the next eligible deployment
// and lets the upstreams race. The first transport-level winner is served;
// the loser is abandoned.
//
// Guardrails, deliberately chosen to keep hedging safe and lightweight:
//
//   - Only the FIRST attempt of a request is ever hedged, and only one hedge
//     partner may race, so worst-case provider amplification is 2x for slow
//     starts and 1x for the common fast path.
//   - Abandonment is a routing decision, not provider evidence: the losing
//     leg records no health failure, no quarantine, and no provider-incident
//     signal. Otherwise hedging would poison health data with evidence about
//     upstreams that were merely slower, not broken.
//   - The race is bounded by the caller's route context, so client
//     disconnects and gateway deadlines cancel both legs immediately.
//   - Losing responses are closed, not drained: connection reuse is worth
//     less than deterministic, immediate teardown.

type hedgeLeg struct {
	cancel context.CancelFunc
	done   chan struct{}
	resp   *http.Response
	err    error
	start  time.Time
}

func (l *hedgeLeg) finish(resp *http.Response, err error) {
	l.resp = resp
	l.err = err
	close(l.done)
}

type hedgeOutcome struct {
	resp *http.Response
	err  error
	// start is the clock origin of the winning leg (used for latency/TTFT).
	start time.Time
	// secondaryWon is true when the hedge partner delivered the winning
	// transport result.
	secondaryWon bool
	// hedgeLaunched is true when the secondary leg was started at all.
	hedgeLaunched bool
}

// abandonUpstream tears down a losing leg. No health signal is recorded.
func (s *Server) abandonUpstream(leg *hedgeLeg, deploymentID, requestID, reason string) {
	if leg == nil {
		return
	}
	if leg.cancel != nil {
		leg.cancel()
	}
	if leg.resp != nil && leg.resp.Body != nil {
		_ = leg.resp.Body.Close()
	}
	s.bus.Add(events.Event{RequestID: requestID, Kind: "hedged_abandoned", Deployment: deploymentID, Message: reason})
}

// abandonUpstreamAsync cancels a losing leg immediately and reaps it off the
// request path. The winning response must never be held hostage by a slower
// loser: waiting for it would reintroduce exactly the tail latency hedging
// exists to remove, and leaving it running would burn provider quota and hold
// a provider concurrency slot until the route deadline. Cancellation is
// immediate; the body is closed as soon as the transport releases it.
func (s *Server) abandonUpstreamAsync(leg *hedgeLeg, deploymentID, requestID, reason string) {
	if leg == nil {
		return
	}
	if leg.cancel != nil {
		leg.cancel()
	}
	go func() {
		if !channelClosed(leg.done) {
			<-leg.done
		}
		s.abandonUpstream(leg, deploymentID, requestID, reason)
	}()
}

// releaseSettledLeg reclaims a leg that already produced its transport result:
// the child context is released and any response body is closed. No
// abandonment event is emitted, because the gateway did not cancel an
// in-flight attempt.
func (s *Server) releaseSettledLeg(leg *hedgeLeg) {
	if leg == nil {
		return
	}
	if leg.cancel != nil {
		leg.cancel()
	}
	if leg.resp != nil && leg.resp.Body != nil {
		_ = leg.resp.Body.Close()
	}
}

// recordHedgeFailure makes a failed hedge leg observable. It is deliberately
// not health evidence: a slower or failing hedge partner must never quarantine
// the deployments that were already serving traffic.
func (s *Server) recordHedgeFailure(requestID, deploymentID string, err error) {
	msg := "hedge leg failed"
	if err != nil {
		msg = err.Error()
	}
	s.bus.Add(events.Event{RequestID: requestID, Kind: "hedge_fail", Deployment: deploymentID, Message: msg})
}

// hedgedUpstreamDo races two attempts for response headers.
//
// primary/secondary each run their attempt with a dedicated child context.
// The winner is the first leg to deliver either a response or a terminal
// transport error — except that a primary error does not win while the
// secondary is still in flight, and a secondary error never wins while the
// primary is still in flight. When both legs have failed the primary's error
// is returned so error reporting stays deterministic.
//
// Winning must be fast: as soon as the primary delivers a servable response
// the slower hedge leg is cancelled and reaped in the background, so the
// client is never serialized behind the loser. The single exception is a
// primary response that the caller would only fail over from (a retryable
// error status): there the in-flight hedge leg is already the natural
// failover, so its transport result is awaited and preferred instead of being
// discarded and repeated.
func (s *Server) hedgedUpstreamDo(
	routeCtx context.Context,
	requestID, primaryID, secondaryID string,
	delay time.Duration,
	primary func(ctx context.Context) (*http.Response, error),
	secondary func(ctx context.Context) (*http.Response, error),
) hedgeOutcome {
	out := hedgeOutcome{}
	ctxA, cancelA := context.WithCancel(routeCtx)
	ctxB, cancelB := context.WithCancel(routeCtx)
	legA := &hedgeLeg{cancel: cancelA, done: make(chan struct{}), start: time.Now()}
	legB := &hedgeLeg{cancel: cancelB, done: make(chan struct{})}

	go func() {
		resp, err := primary(ctxA)
		legA.finish(resp, err)
	}()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	bLaunched := false
	bSettled := false

	launchB := func() {
		bLaunched = true
		legB.start = time.Now()
		s.bus.Add(events.Event{RequestID: requestID, Kind: "hedge_launch", Deployment: secondaryID,
			Message: "primary slow to first byte; racing next eligible deployment"})
		go func() {
			resp, err := secondary(ctxB)
			legB.finish(resp, err)
		}()
	}

	// reportBFailure surfaces a failed hedge leg exactly once. It must only be
	// called once legB has settled, so reading its fields is synchronized.
	bFailureReported := false
	reportBFailure := func() {
		if bFailureReported || !bSettled || legB.err == nil {
			return
		}
		bFailureReported = true
		s.recordHedgeFailure(requestID, secondaryID, legB.err)
	}

	for {
		select {
		case <-legA.done:
			if !bLaunched {
				out.resp, out.err, out.start = legA.resp, legA.err, legA.start
				return out
			}
			// Synchronize on the hedge leg's channel before reading its
			// fields: an unsynchronized read would race the legs goroutine.
			bSettled = bSettled || channelClosed(legB.done)

			if legA.err == nil && legA.resp != nil && !bSettled {
				if retryable(legA.resp.StatusCode) {
					// Primary answered with a status the caller will fail
					// over from. The hedge leg is already in flight against
					// the next eligible deployment, so wait for its transport
					// result and prefer its success rather than discarding a
					// possibly successful attempt and repeating the call.
					<-legB.done
					bSettled = true
					if legB.err == nil && legB.resp != nil {
						s.abandonUpstream(legA, primaryID, requestID, "primary failed; hedged attempt took over")
						out.resp, out.err, out.start, out.secondaryWon, out.hedgeLaunched = legB.resp, nil, legB.start, true, true
						return out
					}
					reportBFailure()
					out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, true
					return out
				}
				// Primary delivered a servable response. Hand it to the
				// client immediately and cancel the slower loser instead of
				// waiting for it: awaiting a loser would reintroduce the very
				// tail latency hedging exists to remove, and letting it run
				// would burn provider quota and hold a concurrency slot until
				// the route deadline.
				s.abandonUpstreamAsync(legB, secondaryID, requestID, "lost hedged race (primary responded first)")
				out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, true
				return out
			}
			if !bSettled {
				// Primary failed at the transport level while the hedge is in
				// flight: wait for the hedge and prefer its success.
				<-legB.done
				bSettled = true
				if legB.err == nil && legB.resp != nil {
					s.abandonUpstream(legA, primaryID, requestID, "primary failed; hedged attempt took over")
					out.resp, out.err, out.start, out.secondaryWon, out.hedgeLaunched = legB.resp, nil, legB.start, true, true
					return out
				}
			}
			// Both legs have settled. A successful hedge leg takes over when
			// the primary failed; otherwise the primary's result wins so
			// reporting stays deterministic. The hedge leg is reported as
			// attempted either way, so the caller does not immediately repeat
			// the same upstream call.
			if legB.err == nil && legB.resp != nil && legA.err != nil {
				s.abandonUpstream(legA, primaryID, requestID, "primary failed; hedged attempt took over")
				out.resp, out.err, out.start, out.secondaryWon, out.hedgeLaunched = legB.resp, nil, legB.start, true, true
				return out
			}
			reportBFailure()
			// Releasing (not abandoning) the settled hedge leg closes any
			// response body it produced without emitting a cancellation event
			// for an attempt the gateway never cancelled.
			s.releaseSettledLeg(legB)
			out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, true
			return out

		case <-legB.done:
			// Test the primary's channel before reading its result fields: the
			// primary goroutine publishes them immediately before closing done.
			aClosed := channelClosed(legA.done)
			if legB.err == nil && legB.resp != nil && (!aClosed || legA.err != nil) {
				// Secondary delivered headers. If the primary is still in
				// flight, abandon it; if the primary already failed, this is
				// the takeover path.
				if !aClosed {
					s.abandonUpstreamAsync(legA, primaryID, requestID, "lost hedged race (hedge responded first)")
				} else {
					s.abandonUpstream(legA, primaryID, requestID, "primary failed; hedged attempt took over")
				}
				out.resp, out.start, out.secondaryWon, out.hedgeLaunched = legB.resp, legB.start, true, true
				return out
			}
			// Hedge failed: keep waiting for the primary. A failing hedge
			// must never invent a failure for an upstream that has not
			// answered yet.
			bSettled = true
			if bLaunched {
				reportBFailure()
			}
			if channelClosed(legA.done) {
				out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, true
				return out
			}

		case <-timer.C:
			// Never launch a hedge for a primary that already settled: the
			// race is over, and an extra upstream call would be pure
			// amplification.
			if !bLaunched && !channelClosed(legA.done) {
				launchB()
			}

		case <-routeCtx.Done():
			// Parent cancellation (client disconnect, deadline, shutdown)
			// tears down both legs; the caller observes routeCtx.Err()
			// through the returned primary error channel below.
			<-legA.done
			if bLaunched {
				<-legB.done
				s.abandonUpstream(legB, secondaryID, requestID, "route context cancelled during hedged race")
			}
			out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, bLaunched
			return out
		}
	}
}

func channelClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// hedgeAttemptBundle carries everything needed to run and process one
// attempt against one candidate.
type hedgeAttemptBundle struct {
	c       router.Scored
	a       providers.Adapter
	payload []byte
	nm      *translate.NameMap
	// injected marks that stream_options include_usage was added to an
	// OpenAI-compatible payload on behalf of an Anthropic client (so a 400
	// mentioning the option can be retried without it).
	injected bool
}

func (s *Server) hedgingEligible(cfg config.Config) bool {
	return cfg.Routing.HedgingEnabled && cfg.HedgingDelay() > 0 && cfg.Routing.MaxAttempts > 1
}

// doAttemptWithHedge runs the first attempt with optional hedging and all
// later attempts as plain blocking calls. The returned outcome always carries
// the winning transport result; when the hedge partner won, winner describes
// the partner leg so the caller can rebind its candidate state.
func (s *Server) doAttemptWithHedge(
	routeCtx context.Context,
	requestID string,
	cfg config.Config,
	candidates []router.Scored,
	i, attempts, max int,
	primary hedgeAttemptBundle,
	build func(int) (hedgeAttemptBundle, bool),
	stream bool,
	forward http.Header,
) (hedgeOutcome, hedgeAttemptBundle, bool) {
	plain := func() (hedgeOutcome, hedgeAttemptBundle, bool) {
		start := time.Now()
		resp, err := primary.a.Do(routeCtx, primary.payload, stream, forward)
		return hedgeOutcome{resp: resp, err: err, start: start}, primary, false
	}
	if attempts != 1 || !s.hedgingEligible(cfg) || i+1 >= len(candidates) || attempts+1 > max {
		return plain()
	}
	partner, ok := build(i + 1)
	if !ok {
		return plain()
	}
	out := s.hedgedUpstreamDo(routeCtx, requestID,
		primary.c.Deployment.ID, partner.c.Deployment.ID, cfg.HedgingDelay(),
		func(ctx context.Context) (*http.Response, error) {
			return primary.a.Do(ctx, primary.payload, stream, forward)
		},
		func(ctx context.Context) (*http.Response, error) {
			return partner.a.Do(ctx, partner.payload, stream, forward)
		},
	)
	winner := primary
	if out.secondaryWon {
		winner = partner
	}
	return out, winner, true
}

// buildOpenAIAttempt prepares one attempt for the OpenAI ingress pipeline:
// passthrough for OpenAI-compatible targets, translation for
// Anthropic-compatible targets.
func (s *Server) buildOpenAIAttempt(cand router.Scored, req router.Requirement, raw []byte, in core.OpenAIRequest) (hedgeAttemptBundle, bool) {
	fresh, a, ok := s.currentRouteCandidate(cand.Deployment.ID, req)
	if !ok {
		return hedgeAttemptBundle{}, false
	}
	bundle := hedgeAttemptBundle{c: fresh, a: a}
	var err error
	if fresh.Deployment.ProviderType == "openai_compatible" {
		bundle.payload, err = patchJSONModel(raw, fresh.Deployment.Model)
	} else {
		var an core.AnthropicRequest
		an, bundle.nm, err = translate.OpenAIToAnthropic(in, fresh.Deployment.Model)
		if err == nil {
			bundle.payload, err = json.Marshal(an)
		}
	}
	if err != nil {
		return hedgeAttemptBundle{}, false
	}
	return bundle, true
}

// buildAnthropicAttempt prepares one attempt for the Anthropic ingress
// pipeline: passthrough for Anthropic-compatible targets, translation with
// usage-requesting stream options for OpenAI-compatible targets.
func (s *Server) buildAnthropicAttempt(cand router.Scored, req router.Requirement, raw []byte, in core.AnthropicRequest) (hedgeAttemptBundle, bool) {
	fresh, a, ok := s.currentRouteCandidate(cand.Deployment.ID, req)
	if !ok {
		return hedgeAttemptBundle{}, false
	}
	bundle := hedgeAttemptBundle{c: fresh, a: a}
	var err error
	if fresh.Deployment.ProviderType == "anthropic_compatible" {
		bundle.payload, err = patchJSONModel(raw, fresh.Deployment.Model)
	} else {
		var o core.OpenAIRequest
		o, bundle.nm, err = translate.AnthropicToOpenAI(in, fresh.Deployment.Model)
		if err == nil && in.Stream {
			o.StreamOptions = json.RawMessage(`{"include_usage":true}`)
			bundle.injected = true
		}
		if err == nil {
			bundle.payload, err = json.Marshal(o)
		}
	}
	if err != nil {
		return hedgeAttemptBundle{}, false
	}
	return bundle, true
}

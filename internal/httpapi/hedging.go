package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/core"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
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

// hedgedUpstreamDo races two attempts for response headers.
//
// primary/secondary each run their attempt with a dedicated child context.
// The winner is the first leg to deliver either a response or a terminal
// transport error — except that a primary error does not win while the
// secondary is still in flight, and a secondary error never wins while the
// primary is still in flight. When both legs have failed the primary's error
// is returned so error reporting stays deterministic.
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

	for {
		select {
		case <-legA.done:
			if !bLaunched {
				out.resp, out.err, out.start = legA.resp, legA.err, legA.start
				return out
			}
			if legA.err == nil && !bSettled {
				// Primary delivered headers first; abandon the hedge leg.
				<-legB.done
				s.abandonUpstream(legB, secondaryID, requestID, "lost hedged race (primary responded first)")
				out.resp, out.err, out.start = legA.resp, legA.err, legA.start
				return out
			}
			// Primary failed while hedge in flight. If the hedge already
			// failed too, report the primary's error; otherwise wait for the
			// hedge and prefer its success.
			if bSettled || legB.err != nil {
				s.abandonUpstream(legB, secondaryID, requestID, "abandoned after primary failure (hedge failed or pending)")
				out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, true
				return out
			}
			<-legB.done
			bSettled = true
			if legB.err == nil && legB.resp != nil {
				s.abandonUpstream(legA, primaryID, requestID, "primary failed; hedged attempt took over")
				out.resp, out.err, out.start, out.secondaryWon, out.hedgeLaunched = legB.resp, nil, legB.start, true, true
				return out
			}
			out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, true
			return out

		case <-legB.done:
			if legB.err == nil && legB.resp != nil && (legA.err != nil || !channelClosed(legA.done)) {
				// Secondary delivered headers. If the primary is still in
				// flight, abandon it; if the primary already failed, this is
				// the takeover path.
				if !channelClosed(legA.done) {
					go func() {
						<-legA.done
						s.abandonUpstream(legA, primaryID, requestID, "lost hedged race (hedge responded first)")
					}()
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
				s.bus.Add(events.Event{RequestID: requestID, Kind: "hedge_fail", Deployment: secondaryID, Message: legB.err.Error()})
			}
			if channelClosed(legA.done) {
				out.resp, out.err, out.start, out.hedgeLaunched = legA.resp, legA.err, legA.start, true
				return out
			}

		case <-timer.C:
			if !bLaunched {
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
	// path overrides the adapter's default upstream path (Gemini and
	// Responses-native upstreams carry the model in the URL path). Empty
	// means the adapter default.
	path string
	// canonicalKind marks upstream protocol families served through the
	// canonical IR path ("openai_chat", "anthropic", "gemini",
	// "openai_responses"). Empty means the legacy data path.
	canonicalKind string
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
	dispatch := func(b hedgeAttemptBundle, ctx context.Context) (*http.Response, error) {
		if b.path != "" {
			return b.a.DoPath(ctx, http.MethodPost, b.path, b.payload, stream, forward)
		}
		return b.a.Do(ctx, b.payload, stream, forward)
	}
	plain := func() (hedgeOutcome, hedgeAttemptBundle, bool) {
		start := time.Now()
		resp, err := dispatch(primary, routeCtx)
		return hedgeOutcome{resp: resp, err: err, start: start}, primary, false
	}
	if attempts != 1 || !s.hedgingEligible(cfg) || i+1 >= len(candidates) || attempts+1 > max {
		return plain()
	}
	partner, ok := build(i + 1)
	if !ok {
		return plain()
	}
	if primary.canonicalKind != partner.canonicalKind || primary.path != "" || partner.path != "" {
		// Canonical/path-addressed attempts skip hedging: their retry and
		// repair semantics are handled by the compatibility engine.
		return plain()
	}
	out := s.hedgedUpstreamDo(routeCtx, requestID,
		primary.c.Deployment.ID, partner.c.Deployment.ID, cfg.HedgingDelay(),
		func(ctx context.Context) (*http.Response, error) {
			return dispatch(primary, ctx)
		},
		func(ctx context.Context) (*http.Response, error) {
			return dispatch(partner, ctx)
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
	switch fresh.Deployment.ProviderType {
	case "gemini", "openai_responses":
		canReq := canonical.DecodeOpenAIChatRequest(in, in.Model)
		bundle, ok = s.finishCanonicalAttempt(bundle, canReq)
		if !ok {
			return hedgeAttemptBundle{}, false
		}
		return bundle, true
	case "anthropic_compatible":
		var an core.AnthropicRequest
		an, bundle.nm, err = translate.OpenAIToAnthropic(in, fresh.Deployment.Model)
		if err == nil {
			bundle.payload, err = json.Marshal(an)
			if err == nil {
				bundle.payload = s.sanitizeOutgoingPayload(bundle, bundle.payload, req)
			}
		}
	default:
		bundle.payload, err = patchJSONModel(raw, fresh.Deployment.Model)
		if err == nil {
			bundle.payload = s.sanitizeOutgoingPayload(bundle, bundle.payload, req)
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
	switch fresh.Deployment.ProviderType {
	case "gemini", "openai_responses":
		canReq, derr := canonical.DecodeAnthropicRequest(in, in.Model)
		if derr != nil {
			return hedgeAttemptBundle{}, false
		}
		bundle, ok := s.finishCanonicalAttempt(bundle, canReq)
		if !ok {
			return hedgeAttemptBundle{}, false
		}
		return bundle, true
	case "anthropic_compatible":
		bundle.payload, err = patchJSONModel(raw, fresh.Deployment.Model)
	default:
		var o core.OpenAIRequest
		o, bundle.nm, err = translate.AnthropicToOpenAI(in, fresh.Deployment.Model)
		if err == nil && in.Stream {
			o.StreamOptions = json.RawMessage(`{"include_usage":true}`)
			bundle.injected = true
		}
		if err == nil {
			bundle.payload, err = json.Marshal(o)
			if err == nil {
				bundle.payload = s.sanitizeOutgoingPayload(bundle, bundle.payload, req)
			}
		}
	}
	if err != nil {
		return hedgeAttemptBundle{}, false
	}
	return bundle, true
}

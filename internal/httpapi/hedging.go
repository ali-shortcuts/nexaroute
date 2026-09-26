package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

type hedgeResult struct {
	resp  *http.Response
	err   error
	start time.Time
}

type hedgeLeg struct {
	cancel context.CancelFunc
	done   <-chan hedgeResult
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

func normalizeHedgeResult(r hedgeResult) hedgeResult {
	if r.err == nil && r.resp == nil {
		r.err = errors.New("upstream returned nil response without error")
	}
	return r
}

// abandonHedgeLeg cancels a losing leg immediately, then waits asynchronously
// for its transport to unwind before closing any response body. Waiting in the
// request goroutine would defeat hedging: a fast winner could otherwise be
// blocked behind the slow loser it was meant to escape.
func (s *Server) abandonHedgeLeg(leg *hedgeLeg, deploymentID, requestID, reason string) {
	if leg == nil {
		return
	}
	if leg.cancel != nil {
		leg.cancel()
	}
	go func() {
		result, ok := <-leg.done
		if ok && result.resp != nil && result.resp.Body != nil {
			_ = result.resp.Body.Close()
		}
		s.bus.Add(events.Event{RequestID: requestID, Kind: "hedged_abandoned", Deployment: deploymentID, Message: reason})
	}()
}

func startHedgeLeg(parent context.Context, run func(context.Context) (*http.Response, error)) *hedgeLeg {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan hedgeResult, 1)
	start := time.Now()
	go func() {
		resp, err := run(ctx)
		done <- normalizeHedgeResult(hedgeResult{resp: resp, err: err, start: start})
		close(done)
	}()
	return &hedgeLeg{cancel: cancel, done: done}
}

// hedgedUpstreamDo races two attempts for response headers.
//
// The first leg to return a transport response wins. A transport error does
// not beat an in-flight peer: if one leg fails, the other is allowed to
// finish. When both fail, the primary error is reported for deterministic
// diagnostics. Losers are cancelled immediately and cleaned up asynchronously,
// so the winning response is never delayed by loser shutdown.
func (s *Server) hedgedUpstreamDo(
	routeCtx context.Context,
	requestID, primaryID, secondaryID string,
	delay time.Duration,
	primary func(ctx context.Context) (*http.Response, error),
	secondary func(ctx context.Context) (*http.Response, error),
) hedgeOutcome {
	primaryLeg := startHedgeLeg(routeCtx, primary)
	var secondaryLeg *hedgeLeg

	timer := time.NewTimer(delay)
	defer timer.Stop()

	var primaryResult *hedgeResult
	var secondaryResult *hedgeResult

	launchSecondary := func() {
		if secondaryLeg != nil {
			return
		}
		secondaryLeg = startHedgeLeg(routeCtx, secondary)
		s.bus.Add(events.Event{RequestID: requestID, Kind: "hedge_launch", Deployment: secondaryID,
			Message: "primary slow to response headers; racing next eligible deployment"})
	}

	for {
		var primaryDone <-chan hedgeResult
		if primaryResult == nil {
			primaryDone = primaryLeg.done
		}
		var secondaryDone <-chan hedgeResult
		if secondaryLeg != nil && secondaryResult == nil {
			secondaryDone = secondaryLeg.done
		}

		select {
		case result := <-primaryDone:
			r := result
			primaryResult = &r
			if r.err == nil {
				if secondaryLeg != nil {
					s.abandonHedgeLeg(secondaryLeg, secondaryID, requestID, "lost hedged race (primary responded first)")
				}
				return hedgeOutcome{resp: r.resp, start: r.start, hedgeLaunched: secondaryLeg != nil}
			}
			if secondaryLeg == nil {
				return hedgeOutcome{err: r.err, start: r.start}
			}
			if secondaryResult != nil {
				if secondaryResult.err == nil {
					return hedgeOutcome{resp: secondaryResult.resp, start: secondaryResult.start, secondaryWon: true, hedgeLaunched: true}
				}
				return hedgeOutcome{err: r.err, start: r.start, hedgeLaunched: true}
			}

		case result := <-secondaryDone:
			r := result
			secondaryResult = &r
			if r.err == nil {
				s.abandonHedgeLeg(primaryLeg, primaryID, requestID, "lost hedged race (hedge responded first)")
				return hedgeOutcome{resp: r.resp, start: r.start, secondaryWon: true, hedgeLaunched: true}
			}
			s.bus.Add(events.Event{RequestID: requestID, Kind: "hedge_fail", Deployment: secondaryID, Message: r.err.Error()})
			if primaryResult != nil {
				return hedgeOutcome{err: primaryResult.err, start: primaryResult.start, hedgeLaunched: true}
			}

		case <-timer.C:
			launchSecondary()

		case <-routeCtx.Done():
			s.abandonHedgeLeg(primaryLeg, primaryID, requestID, "route context cancelled during hedged race")
			if secondaryLeg != nil {
				s.abandonHedgeLeg(secondaryLeg, secondaryID, requestID, "route context cancelled during hedged race")
			}
			return hedgeOutcome{err: routeCtx.Err(), start: time.Now(), hedgeLaunched: secondaryLeg != nil}
		}
	}
}

// hedgeAttemptBundle carries everything needed to run and process one
// attempt against one candidate.
type hedgeAttemptBundle struct {
	c       router.Scored
	a       providers.Adapter
	payload []byte
	nm      *translate.NameMap
	// buildErr is safe, bounded context for an unsupported cross-protocol
	// mapping. It is returned to the ingress handler instead of silently
	// reducing the request to a generic "payload could not be built" failure.
	buildErr error
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
	return out, winner, out.hedgeLaunched
}

const maxProtocolMappingErrorBytes = 512

func boundedProtocolMappingError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > maxProtocolMappingErrorBytes {
		message = message[:maxProtocolMappingErrorBytes] + "..."
	}
	return fmt.Errorf("unsupported protocol mapping: %s", message)
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
		bundle.buildErr = boundedProtocolMappingError(err)
		return bundle, false
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
			bundle.buildErr = boundedProtocolMappingError(derr)
			return bundle, false
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
		bundle.buildErr = boundedProtocolMappingError(err)
		return bundle, false
	}
	return bundle, true
}

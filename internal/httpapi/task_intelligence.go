package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/taskintel"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// TaskPinHeader carries an explicit client task pin (Phase 19, issue #419).
// The value must be a canonical task type (e.g. "coding"); it is validated
// and normalized, and an invalid value is ignored. A valid pin is explicit
// client intent: the classifier must not fight it. The operator override
// (route profile task_override) still wins over the pin.
const TaskPinHeader = "X-NexaRoute-Task-Pin"

// taskIntelligence holds the result of feature extraction + classification.
// It is observational only and MUST NOT affect routing by itself; routing
// effects go through applyTaskIntel, which honors the route's configured mode.
type taskIntelligence struct {
	Features feature.RequestFeatures
	Profile  taskprofile.TaskProfile
	Duration time.Duration
}

// global stateless instances (no locks, no state)
var (
	globalFeatureExtractor = feature.NewExtractor()
	globalTaskAnalyzer     = taskprofile.NewAnalyzer()
)

// extractFeaturesAndClassify performs bounded feature extraction and deterministic classification.
// It never returns an error for valid JSON; on invalid JSON it returns zero features with general task.
func extractFeaturesAndClassify(raw []byte, opts feature.ExtractOptions) taskIntelligence {
	start := time.Now()
	feat := globalFeatureExtractor.Extract(raw, opts)
	profile := globalTaskAnalyzer.Analyze(feat)
	return taskIntelligence{
		Features: feat,
		Profile:  profile,
		Duration: time.Since(start),
	}
}

// applyTaskIntel resolves the task-intelligence decision for the resolved
// route and applies it according to the route profile's configured mode:
//
//   - off (default): observational only — candidates and route unchanged.
//   - suggest: the suggestion is recorded in the trace; routing unchanged.
//   - apply: a high-confidence classification (or pin, or operator override)
//     with a mapped task pool re-resolves candidates against the task pool as
//     the new primary. The configured fallback chain is preserved.
//
// Precedence inside suggest/apply: operator override > client pin >
// classifier >= min confidence. Low confidence degrades to the default
// profile with no routing change.
//
// Fail-closed notes: an unparseable mode behaves as off; a task pool that
// cannot be expanded, or that yields no eligible deployments while the
// original set was non-empty, falls back to the original candidates and is
// recorded as unmapped in the trace.
func (s *Server) applyTaskIntel(r *http.Request, req router.Requirement, cfg config.Config, candidates []router.Scored, resolved *route.ResolvedRoute, ti taskIntelligence) (taskintel.Resolution, []router.Scored, *route.ResolvedRoute) {
	off := func() (taskintel.Resolution, []router.Scored, *route.ResolvedRoute) {
		return taskintel.Resolve(taskintel.Input{}), candidates, resolved
	}
	if resolved == nil {
		// Non-virtual endpoints have no route profile: observational only.
		return off()
	}
	var profile *config.RouteProfileConfig
	for i := range cfg.RouteProfiles {
		if cfg.RouteProfiles[i].ID == resolved.RouteProfileID {
			profile = &cfg.RouteProfiles[i]
			break
		}
	}
	if profile == nil {
		return off()
	}
	mode, ok := taskintel.ParseMode(profile.TaskIntelMode)
	if !ok {
		// Config validation should have rejected this; behave as off.
		mode = taskintel.ModeOff
	}
	pin, _ := taskintel.NormalizeTaskType(r.Header.Get(TaskPinHeader))
	override, _ := taskintel.NormalizeTaskType(profile.TaskOverride)
	res := taskintel.Resolve(taskintel.Input{
		Mode:          mode,
		Profile:       ti.Profile,
		Pin:           pin,
		Override:      override,
		MinConfidence: profile.TaskIntelMinConfidence,
		TaskPools:     profile.TaskPools,
	})
	if !res.Applied || res.TargetPool == "" || res.TargetPool == resolved.PrimaryPoolID {
		return res, candidates, resolved
	}
	switched, switchedCandidates, ok := s.switchTaskPool(req, cfg, resolved, res.TargetPool)
	if !ok {
		res.Outcome = taskintel.OutcomeUnmapped
		res.Applied = false
		res.Summary += "; task pool could not be expanded, routing unchanged"
		return res, candidates, resolved
	}
	if len(switchedCandidates) == 0 && len(candidates) > 0 {
		// Availability wins: fall back to the route's default candidates and
		// say so in the trace rather than failing the request.
		res.Outcome = taskintel.OutcomeUnmapped
		res.Applied = false
		res.Summary += fmt.Sprintf("; task pool %q has no eligible deployments, fell back to route default", res.TargetPool)
		return res, candidates, resolved
	}
	return res, switchedCandidates, switched
}

// switchTaskPool rebuilds the resolved route with the task pool as the new
// primary pool (fallback chain preserved) and re-filters candidates against
// it using the same requirement pipeline as the original resolution.
func (s *Server) switchTaskPool(req router.Requirement, cfg config.Config, resolved *route.ResolvedRoute, targetPool string) (*route.ResolvedRoute, []router.Scored, bool) {
	s.runtimeMu.RLock()
	resolver := s.routeResolver
	rt := s.rt
	s.runtimeMu.RUnlock()
	if resolver == nil || rt == nil {
		return nil, nil, false
	}
	poolCfg, ok := resolved.Pools[targetPool]
	if !ok {
		return nil, nil, false
	}
	allowed, ok := resolver.GetExpanded(targetPool)
	if !ok {
		return nil, nil, false
	}
	switched := *resolved
	switched.PrimaryPoolID = targetPool
	switched.PrimaryPoolName = poolCfg.Name
	switched.PrimaryMode = poolCfg.Mode
	switched.AllowedDeployments = allowed
	// Task pool first, then the original fallbacks in order (deduped).
	newOrdered := []string{targetPool}
	var newFallbackAllowed []map[string]struct{}
	for i := 1; i < len(resolved.OrderedPoolIDs); i++ {
		pid := resolved.OrderedPoolIDs[i]
		if pid == targetPool {
			continue
		}
		newOrdered = append(newOrdered, pid)
		// FallbackAllowed[j] is parallel to OrderedPoolIDs[j+1].
		if i-1 < len(resolved.FallbackAllowed) {
			newFallbackAllowed = append(newFallbackAllowed, resolved.FallbackAllowed[i-1])
		} else if set, ok := resolver.GetExpanded(pid); ok {
			newFallbackAllowed = append(newFallbackAllowed, set)
		} else {
			newFallbackAllowed = append(newFallbackAllowed, map[string]struct{}{})
		}
	}
	switched.OrderedPoolIDs = newOrdered
	switched.FallbackAllowed = newFallbackAllowed
	allAllowed := make(map[string]struct{}, len(allowed))
	for k := range allowed {
		allAllowed[k] = struct{}{}
	}
	for _, set := range newFallbackAllowed {
		for k := range set {
			allAllowed[k] = struct{}{}
		}
	}
	switched.AllAllowed = allAllowed
	// Re-filter with the model cleared, mirroring candidatesForRequirement's
	// virtual-endpoint path: eligibility uses pool + capability checks only.
	reqAll := req
	reqAll.Model = ""
	allCandidates := rt.Candidates(reqAll)
	filtered := resolver.AllFilteredCandidates(allCandidates, switched)
	filtered = applyRollout(cfg, &switched, req, filtered)
	return &switched, filtered, true
}

// emitTaskClassified emits a privacy-safe task_classified event and records metrics.
// The event contains no raw prompt, only bounded enums and counts. The
// taskintel resolution (mode, outcome, target pool, pin) is recorded in
// redacted form so the trace shows exactly what the intelligence layer did.
func (s *Server) emitTaskClassified(requestID string, ti taskIntelligence, resolvedRoute *route.ResolvedRoute, res taskintel.Resolution) {
	// Build comma-joined reason codes bounded
	reasonStr := strings.Join(reasonCodesToStrings(ti.Profile.ReasonCodes), ",")
	latencyMS := ti.Duration.Milliseconds()
	if latencyMS == 0 && ti.Duration > 0 {
		latencyMS = 1 // avoid 0ms for sub-ms classifications, still bounded
	}
	ev := events.Event{
		RequestID:         requestID,
		Kind:              "task_classified",
		Message:           res.Summary,
		TaskType:          string(ti.Profile.Type),
		TaskComplexity:    string(ti.Profile.Complexity),
		TaskConfidence:    ti.Profile.Confidence,
		TaskReasonCodes:   reasonStr,
		TaskEstimatedTok:  ti.Profile.EstimatedContextTokens,
		TaskToolCount:     ti.Profile.ToolCount,
		TaskImageCount:    ti.Profile.ImageCount,
		TaskMessageCount:  ti.Profile.MessageCount,
		TaskHasCode:       ti.Features.HasCodeBlock || ti.Features.HasCodeIdentifiers,
		TaskHasVision:     ti.Features.HasVision,
		TaskHasAudio:      ti.Features.HasAudio,
		TaskHasReasoning:  ti.Features.HasReasoning,
		TaskHasTools:      ti.Features.HasTools,
		TaskStructuredOut: ti.Features.StructuredOutput,
		TaskClientClass:   string(ti.Features.ClientClass),
		TaskIntelMode:     string(res.Mode),
		TaskIntelOutcome:  string(res.Outcome),
		TaskIntelPool:     res.TargetPool,
		TaskPin:           string(res.Pin),
		LatencyMS:         latencyMS,
	}
	if resolvedRoute != nil {
		ev.VirtualEndpoint = resolvedRoute.VirtualEndpointID
		ev.PublicModel = resolvedRoute.PublicModel
		ev.RouteProfile = resolvedRoute.RouteProfileID
		ev.Pool = resolvedRoute.PrimaryPoolID
	}
	s.bus.Add(ev)
	s.recordTaskClassification(ti.Profile.Type, ti.Profile.Complexity)
}

func reasonCodesToStrings(codes []taskprofile.ReasonCode) []string {
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		out = append(out, string(c))
	}
	return out
}

func (s *Server) recordTaskClassification(t taskprofile.TaskType, c taskprofile.Complexity) {
	// Bounded cardinality: task types (len(AllTaskTypes)=15) x complexities (5) = 75 max keys
	// Derived from canonical enum list, not magic number
	key := string(t) + "|" + string(c)
	s.taskMu.Lock()
	if s.taskClassCounts == nil {
		s.taskClassCounts = make(map[string]uint64, 32)
	}
	s.taskClassCounts[key]++
	s.taskMu.Unlock()
	s.taskAnalysisTotal.Add(1)
}

func (s *Server) taskClassificationSnapshot() map[string]uint64 {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	out := make(map[string]uint64, len(s.taskClassCounts))
	for k, v := range s.taskClassCounts {
		out[k] = v
	}
	return out
}

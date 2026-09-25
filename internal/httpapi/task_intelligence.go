package httpapi

import (
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// taskIntelligence holds the result of feature extraction + classification.
// It is observational only and MUST NOT affect routing.
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

// emitTaskClassified emits a privacy-safe task_classified event and records metrics.
// The event contains no raw prompt, only bounded enums and counts.
func (s *Server) emitTaskClassified(requestID string, ti taskIntelligence, resolvedRoute *route.ResolvedRoute) {
	// Build comma-joined reason codes bounded
	reasonStr := strings.Join(reasonCodesToStrings(ti.Profile.ReasonCodes), ",")
	ev := events.Event{
		RequestID:         requestID,
		Kind:              "task_classified",
		Message:           "request classified",
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
		TaskHasReasoning:  ti.Features.HasReasoning,
		TaskHasTools:      ti.Features.HasTools,
		TaskStructuredOut: ti.Features.StructuredOutput,
		LatencyMS:         ti.Duration.Milliseconds(),
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
	// Bounded cardinality: task type (10) x complexity (5) = 50 max keys
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

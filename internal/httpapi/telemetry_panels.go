package httpapi

import (
	"errors"
	"sort"

	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
	"github.com/ali-shortcuts/nexaroute/internal/usage"
)

// enrichToolValidation copies structured ToolCallValidationError diagnostics
// onto an event so the dashboard can render tool/field/expected/actual
// readably instead of showing a raw Go error string. The raw message is kept
// for logs; the structured fields drive the UI.
func enrichToolValidation(ev *events.Event, err error) {
	var ve *canonical.ToolCallValidationError
	if !errors.As(err, &ve) {
		return
	}
	if ev == nil || ve == nil {
		return
	}
	ev.ToolName = ve.Tool
	ev.ToolField = ve.Field
	ev.ExpectedType = ve.ExpectedType
	ev.ActualType = ve.ActualType
}

// routingShare aggregates per-deployment success counts from REAL event data
// (route_ok in the bounded snapshot window) plus cumulative token usage.
// It never fabricates a metric: deployments with no observed traffic report
// zero counts, and the caller renders an explicit empty state.
func routingShare(evts []events.Event, usageSnap usage.Snapshot) []map[string]any {
	okCounts := map[string]int64{}
	failCounts := map[string]int64{}
	latSum := map[string]int64{}
	latN := map[string]int64{}
	for _, e := range evts {
		if e.Deployment == "" {
			continue
		}
		switch e.Kind {
		case "route_ok":
			okCounts[e.Deployment]++
			if e.LatencyMS > 0 {
				latSum[e.Deployment] += e.LatencyMS
				latN[e.Deployment]++
			}
		case "route_fail", "route_timeout", "response_decode_fail", "stream_fail_precommit", "stream_fail":
			failCounts[e.Deployment]++
		}
	}
	tokensByDep := map[string]int64{}
	for _, row := range usageSnap.ByDeployment {
		tokensByDep[row.Deployment] = row.PromptTokens + row.CompletionTok
	}
	keys := map[string]struct{}{}
	for k := range okCounts {
		keys[k] = struct{}{}
	}
	for k := range failCounts {
		keys[k] = struct{}{}
	}
	for k := range tokensByDep {
		if tokensByDep[k] > 0 {
			keys[k] = struct{}{}
		}
	}
	out := make([]map[string]any, 0, len(keys))
	var totalOK int64
	for _, v := range okCounts {
		totalOK += v
	}
	for dep := range keys {
		avgLat := int64(0)
		if latN[dep] > 0 {
			avgLat = latSum[dep] / latN[dep]
		}
		share := 0.0
		if totalOK > 0 {
			share = float64(okCounts[dep]) / float64(totalOK)
		}
		out = append(out, map[string]any{
			"deployment":      dep,
			"successes":       okCounts[dep],
			"failures":        failCounts[dep],
			"share":           share,
			"avg_latency_ms":  avgLat,
			"total_tokens":    tokensByDep[dep],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		si := out[i]["successes"].(int64)
		sj := out[j]["successes"].(int64)
		if si != sj {
			return si > sj
		}
		return out[i]["deployment"].(string) < out[j]["deployment"].(string)
	})
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

var journeyRouteKinds = map[string]bool{
	"route_attempt":         true,
	"route_fail":            true,
	"failover":              true,
	"route_ok":              true,
	"route_timeout":         true,
	"model_unavailable":     true,
	"model_retired":         true,
	"candidate_exhausted":   true,
	"response_decode_fail":  true,
	"stream_fail_precommit": true,
	"stream_fail":           true,
}

// recentJourney builds the request-journey view for the most recent request
// with routing activity: candidates -> attempts -> failures -> final model ->
// latency. All fields come from real bus events; when there is no routed
// request yet every list is empty and final_model/latency are absent.
func recentJourney(evts []events.Event) map[string]any {
	empty := map[string]any{
		"request_id":   "",
		"candidates":   []string{},
		"attempts":     []map[string]any{},
		"failures":     []map[string]any{},
		"final_model":  "",
		"latency_ms":   int64(0),
		"success":      false,
		"public_model": "",
	}
	var latest *events.Event
	for i := len(evts) - 1; i >= 0; i-- {
		e := evts[i]
		if e.RequestID != "" && journeyRouteKinds[e.Kind] {
			c := e
			latest = &c
			break
		}
	}
	if latest == nil {
		return empty
	}
	reqID := latest.RequestID
	var reqEvents []events.Event
	for _, e := range evts {
		if e.RequestID == reqID && journeyRouteKinds[e.Kind] {
			reqEvents = append(reqEvents, e)
		}
	}
	candidates := []string{}
	seen := map[string]bool{}
	attempts := []map[string]any{}
	failures := []map[string]any{}
	finalModel := ""
	var latency int64
	success := false
	publicModel := ""
	for _, e := range reqEvents {
		if e.PublicModel != "" {
			publicModel = e.PublicModel
		}
		switch e.Kind {
		case "route_attempt":
			if e.Deployment != "" && !seen[e.Deployment] {
				seen[e.Deployment] = true
				candidates = append(candidates, e.Deployment)
			}
			attempts = append(attempts, map[string]any{
				"deployment": e.Deployment,
				"time":       e.Time,
				"message":    e.Message,
			})
		case "route_fail", "route_timeout", "response_decode_fail", "stream_fail_precommit", "stream_fail":
			failEntry := map[string]any{
				"deployment": e.Deployment,
				"kind":       e.Kind,
				"error_type": e.ErrorType,
				"message":    e.Message,
				"time":       e.Time,
			}
			if e.ToolName != "" || e.ToolField != "" || e.ExpectedType != "" || e.ActualType != "" {
				failEntry["tool"] = e.ToolName
				failEntry["field"] = e.ToolField
				failEntry["expected_type"] = e.ExpectedType
				failEntry["actual_type"] = e.ActualType
			}
			failures = append(failures, failEntry)
		case "route_ok":
			finalModel = e.Deployment
			latency = e.LatencyMS
			success = true
		}
	}
	if candidates == nil {
		candidates = []string{}
	}
	if attempts == nil {
		attempts = []map[string]any{}
	}
	if failures == nil {
		failures = []map[string]any{}
	}
	return map[string]any{
		"request_id":   reqID,
		"candidates":   candidates,
		"attempts":     attempts,
		"failures":     failures,
		"final_model":  finalModel,
		"latency_ms":   latency,
		"success":      success,
		"public_model": publicModel,
	}
}

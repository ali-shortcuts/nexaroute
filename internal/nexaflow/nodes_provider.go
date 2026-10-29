package nexaflow

import (
	"context"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/compat"
)

// Provider nodes are read-only views over the existing health manager and
// compat store. They never probe, never change health state, and never
// invent support: an unknown deployment reports "unknown", not "supported".
// Retry semantics: read-only, safe to retry (MaxAttempts 3, retryable on
// timeout/backend_error). Cancellation: context propagation only.
func providerNodeSpecs() []*NodeSpec {
	return []*NodeSpec{
		{
			ID: "nexaroute.provider.health", Version: SupportedVersion, Category: "provider",
			Title:       "Provider health inspection",
			Description: "Snapshot of one deployment's health state from the existing health manager. Read-only.",
			InputSchema: []Field{
				{Name: "deployment_id", Type: "string", Required: true, Description: "Deployment id, e.g. provider/model."},
			},
			OutputSchema: []Field{
				{Name: "deployment_id", Type: "string"},
				{Name: "status", Type: "string", Description: "health status: unknown|healthy|degraded|half_open|cooldown|retired."},
				{Name: "successes", Type: "number"},
				{Name: "failures", Type: "number"},
				{Name: "consecutive_failures", Type: "number"},
				{Name: "last_error", Type: "string", Description: "Redacted last error, if any."},
			},
			ConfigSchema: nil,
			Permission:   authz.ReadProviders, SideEffect: SideEffectRead,
			Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout", "backend_error"}},
			DefaultTimeout: 10 * time.Second, MaxTimeout: 30 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 64 << 10,
			Run: runProviderHealth,
		},
		{
			ID: "nexaroute.provider.compat", Version: SupportedVersion, Category: "provider",
			Title:       "Provider compatibility inspection",
			Description: "Capability support map and scorecard status for one deployment from the existing compat store, plus the missing subset of an optional required-capabilities list. Read-only.",
			InputSchema: []Field{
				{Name: "deployment_id", Type: "string", Required: true, Description: "Deployment id."},
				{Name: "required_capabilities", Type: "array", Description: "Capability names that must be supported; unknown names are rejected."},
			},
			OutputSchema: []Field{
				{Name: "deployment_id", Type: "string"},
				{Name: "support", Type: "object", Description: "Capability name -> supported|unsupported|unknown."},
				{Name: "scorecard_status", Type: "string"},
				{Name: "protocol", Type: "string"},
				{Name: "missing", Type: "array", Description: "Required capabilities not reported as supported."},
			},
			ConfigSchema: nil,
			Permission:   authz.ReadProviders, SideEffect: SideEffectRead,
			Retry:          RetryPolicy{MaxAttempts: 3, RetryableCodes: []string{"timeout", "backend_error"}},
			DefaultTimeout: 10 * time.Second, MaxTimeout: 30 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 64 << 10,
			Run: runProviderCompat,
		},
	}
}

func runProviderHealth(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	id, ok := getString(rc.Input, "deployment_id")
	if !ok || strings.TrimSpace(id) == "" {
		return nil, NewNodeError("validation_error", false, "input deployment_id is required")
	}
	h, err := rc.Backend.ProviderHealth(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"deployment_id": h.DeploymentID, "status": h.Status,
		"successes": h.Successes, "failures": h.Failures,
		"consecutive_failures": h.ConsecutiveFailures,
		"last_error":           RedactError(errString(h.LastError)),
	}, nil
}

type stringError string

func (e stringError) Error() string { return string(e) }
func errString(s string) error {
	if s == "" {
		return nil
	}
	return stringError(s)
}

func runProviderCompat(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	id, ok := getString(rc.Input, "deployment_id")
	if !ok || strings.TrimSpace(id) == "" {
		return nil, NewNodeError("validation_error", false, "input deployment_id is required")
	}
	var required []string
	if _, present := rc.Input["required_capabilities"]; present {
		req, ok := getStringSlice(rc.Input, "required_capabilities")
		if !ok {
			return nil, NewNodeError("validation_error", false, "required_capabilities must be an array of strings")
		}
		known := make(map[string]bool, len(compat.AllCapabilities))
		for _, c := range compat.AllCapabilities {
			known[c] = true
		}
		for _, c := range req {
			if !known[c] {
				return nil, NewNodeError("validation_error", false, "unknown capability %q", c)
			}
		}
		required = req
	}
	info, err := rc.Backend.Compat(ctx, id)
	if err != nil {
		return nil, err
	}
	support := make(map[string]any, len(info.Support))
	for k, v := range info.Support {
		support[k] = v
	}
	var missing []any
	for _, c := range required {
		if info.Support[c] != "supported" {
			missing = append(missing, c)
		}
	}
	if missing == nil {
		missing = []any{}
	}
	return map[string]any{
		"deployment_id": info.DeploymentID, "support": support,
		"scorecard_status": info.ScorecardStatus, "protocol": info.Protocol,
		"context_window": info.ContextWindow, "max_output": info.MaxOutput,
		"missing": missing,
	}, nil
}

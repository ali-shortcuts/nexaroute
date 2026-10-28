package nexaflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Control nodes are pure: no backend, no I/O, no side effects, and they
// require only an authenticated principal (Permission ""). They decide,
// reshape, plan, or gate — they never execute anything else. Per-iteration
// execution (loop), durable pausing (approval), and workflow continuation
// (branch) are the runtime's job and are documented as such on each node.
// Retry semantics: pure and deterministic, MaxAttempts 1 — a retry
// recomputes the same result, so retrying is harmless but pointless.
// Cancellation: wait honours context cancellation via a timer select;
// the other pure nodes return immediately.
func controlNodeSpecs() []*NodeSpec {
	return []*NodeSpec{
		{
			ID: "nexaroute.control.condition", Version: SupportedVersion, Category: "control",
			Title:       "Conditional check",
			Description: "Evaluates one comparison on a dotted field path (a.b) against a constant, with operators eq, ne, gt, gte, lt, lte, contains. No expression language, no code execution.",
			InputSchema: []Field{
				{Name: "value", Type: "object", Required: true, Description: "Object the field path is read from."},
			},
			OutputSchema: []Field{
				{Name: "result", Type: "boolean"},
				{Name: "field", Type: "string"},
			},
			ConfigSchema: []Field{
				{Name: "field", Type: "string", Required: true, Description: "Dotted path, e.g. health.status."},
				{Name: "operator", Type: "string", Required: true, Description: "eq|ne|gt|gte|lt|lte|contains."},
				{Name: "expected", Type: "string", Description: "Expected value as a string; numeric comparisons parse both sides as numbers."},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 64 << 10, MaxOutputBytes: 4096,
			Run: runCondition,
		},
		{
			ID: "nexaroute.control.branch", Version: SupportedVersion, Category: "control",
			Title:       "Branch selection",
			Description: "Selects one branch name from a condition result. Branch execution is the runtime's job.",
			InputSchema: []Field{
				{Name: "condition_result", Type: "boolean", Required: true},
			},
			OutputSchema: []Field{
				{Name: "selected_branch", Type: "string"},
			},
			ConfigSchema: []Field{
				{Name: "true_branch", Type: "string", Required: true},
				{Name: "false_branch", Type: "string", Required: true},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 4096,
			Run: runBranch,
		},
		{
			ID: "nexaroute.control.json_transform", Version: SupportedVersion, Category: "control",
			Title:       "JSON transform",
			Description: "Allowlisted reshaping only: pick, rename, set_const, delete. Any other operation is rejected. No expression language, no code.",
			InputSchema: []Field{
				{Name: "value", Type: "object", Required: true, Description: "Object to transform."},
				{Name: "operations", Type: "array", Required: true, Description: "Operation objects: {op, field, [to|value]}."},
			},
			OutputSchema: []Field{
				{Name: "value", Type: "object"},
			},
			ConfigSchema: nil,
			Permission:   "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 64 << 10, MaxOutputBytes: 64 << 10,
			Run: runJSONTransform,
		},
		{
			ID: "nexaroute.control.loop_plan", Version: SupportedVersion, Category: "control",
			Title:       "Bounded loop plan",
			Description: "Plans at most max_iterations iterations over an items array (<=100 items). It does not execute iterations; per-iteration execution is the runtime's job.",
			InputSchema: []Field{
				{Name: "items", Type: "array", Required: true, Description: "Items to iterate, at most 100."},
			},
			OutputSchema: []Field{
				{Name: "iterations", Type: "array", Description: "Planned iterations: {index, item}."},
				{Name: "truncated", Type: "boolean", Description: "True when items exceeded max_iterations and the tail was dropped."},
			},
			ConfigSchema: []Field{
				{Name: "max_iterations", Type: "number", Description: "1..100, default 100."},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 64 << 10, MaxOutputBytes: 64 << 10,
			Run: runLoopPlan,
		},
		{
			ID: "nexaroute.control.merge", Version: SupportedVersion, Category: "control",
			Title:       "Merge",
			Description: "Merges two objects with a conflict policy: first (keep existing), last (overwrite), or error (fail on any conflicting key). At most 1000 keys.",
			InputSchema: []Field{
				{Name: "first", Type: "object", Required: true},
				{Name: "second", Type: "object", Required: true},
			},
			OutputSchema: []Field{
				{Name: "merged", Type: "object"},
			},
			ConfigSchema: []Field{
				{Name: "conflict", Type: "string", Description: "first|last|error; default last."},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 64 << 10, MaxOutputBytes: 64 << 10,
			Run: runMerge,
		},
		{
			ID: "nexaroute.control.wait", Version: SupportedVersion, Category: "control",
			Title:       "Wait",
			Description: "Waits up to the configured maximum (default cap 5 minutes) and honours context cancellation. A cancelled wait is an error, not a success.",
			InputSchema: []Field{
				{Name: "duration_ms", Type: "number", Description: "Wait duration in milliseconds; default 0."},
			},
			OutputSchema: []Field{
				{Name: "waited_ms", Type: "number"},
			},
			ConfigSchema: []Field{
				{Name: "max_duration", Type: "string", Description: "Maximum wait as a Go duration, default 5m."},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Minute, MaxTimeout: 10 * time.Minute,
			MaxInputBytes: 4096, MaxOutputBytes: 4096,
			Run: runWait,
		},
		{
			ID: "nexaroute.control.approval", Version: SupportedVersion, Category: "control",
			Title:       "Approval gate",
			Description: "Records an approval decision. A missing decision is an error (code approval_required, non-retryable), never a success. Approver and requester must both be identified and must differ; an anonymous or self-approval is rejected. Approver identity is caller-asserted: the runtime must bind the decision to an authenticated approval record before passing it here. Durable pausing while waiting is the runtime's job.",
			InputSchema: []Field{
				{Name: "decision", Type: "string", Description: "approved or rejected. Absent means not yet decided."},
				{Name: "approver", Type: "string", Description: "Subject who decided."},
				{Name: "requester", Type: "string", Description: "Subject who requested approval."},
			},
			OutputSchema: []Field{
				{Name: "approved", Type: "boolean"},
				{Name: "approver", Type: "string"},
			},
			ConfigSchema: nil,
			Permission:   "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 4096,
			Run: runApproval,
		},
		{
			ID: "nexaroute.control.error_handler", Version: SupportedVersion, Category: "control",
			Title:       "Error handler",
			Description: "Maps an upstream error to an action: retry, fallback, or fail. The returned message is sanitized; the handler never re-raises the raw error.",
			InputSchema: []Field{
				{Name: "error_code", Type: "string", Required: true},
				{Name: "message", Type: "string", Description: "Raw message; sanitized before output."},
			},
			OutputSchema: []Field{
				{Name: "action", Type: "string", Description: "retry|fallback|fail."},
				{Name: "error_code", Type: "string"},
				{Name: "message", Type: "string", Description: "Sanitized message."},
			},
			ConfigSchema: []Field{
				{Name: "policy", Type: "string", Required: true, Description: "retry|fallback|fail."},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 64 << 10, MaxOutputBytes: 4096,
			Run: runErrorHandler,
		},
	}
}

func lookupPath(obj map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	cur := any(obj)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, isFinite(n)
	case float32:
		f := float64(n)
		return f, isFinite(f)
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		// Strict parse: fmt.Sscanf("%g") accepts trailing junk
		// ("123abc" parses as 123), which would make a condition
		// branch on a value the author never wrote.
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil || !isFinite(f) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func runCondition(_ context.Context, rc *RunContext) (map[string]any, error) {
	value, ok := getObject(rc.Input, "value")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input value must be an object")
	}
	field, _ := getString(rc.Config, "field")
	operator, _ := getString(rc.Config, "operator")
	expected, _ := getString(rc.Config, "expected")
	if field == "" || operator == "" {
		return nil, NewNodeError("validation_error", false, "config field and operator are required")
	}
	actual, found := lookupPath(value, field)
	if !found {
		// A missing field is false for every operator except ne.
		return map[string]any{"result": operator == "ne", "field": field}, nil
	}
	var result bool
	switch operator {
	case "eq":
		result = fmt.Sprintf("%v", actual) == expected
	case "ne":
		result = fmt.Sprintf("%v", actual) != expected
	case "gt", "gte", "lt", "lte":
		a, okA := toNumber(actual)
		e, okE := toNumber(expected)
		if !okA || !okE {
			return nil, NewNodeError("validation_error", false, "operator %q requires numeric values", operator)
		}
		switch operator {
		case "gt":
			result = a > e
		case "gte":
			result = a >= e
		case "lt":
			result = a < e
		case "lte":
			result = a <= e
		}
	case "contains":
		s, ok := actual.(string)
		if !ok {
			return nil, NewNodeError("validation_error", false, "operator contains requires a string field")
		}
		result = strings.Contains(s, expected)
	default:
		return nil, NewNodeError("validation_error", false, "unknown operator %q", operator)
	}
	return map[string]any{"result": result, "field": field}, nil
}

func runBranch(_ context.Context, rc *RunContext) (map[string]any, error) {
	cond, ok := getBool(rc.Input, "condition_result")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input condition_result is required")
	}
	trueBranch, _ := getString(rc.Config, "true_branch")
	falseBranch, _ := getString(rc.Config, "false_branch")
	if trueBranch == "" || falseBranch == "" {
		return nil, NewNodeError("validation_error", false, "config true_branch and false_branch are required")
	}
	if cond {
		return map[string]any{"selected_branch": trueBranch}, nil
	}
	return map[string]any{"selected_branch": falseBranch}, nil
}

func runJSONTransform(_ context.Context, rc *RunContext) (map[string]any, error) {
	value, ok := getObject(rc.Input, "value")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input value must be an object")
	}
	ops, ok := getArray(rc.Input, "operations")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input operations must be an array")
	}
	// Pick semantics: when any pick op is present, the result starts as
	// exactly the union of the picked fields (present in the input);
	// rename/set_const/delete then apply in order on top of that.
	picks := map[string]bool{}
	for _, raw := range ops {
		op, ok := raw.(map[string]any)
		if !ok {
			return nil, NewNodeError("validation_error", false, "each operation must be an object")
		}
		if name, _ := getString(op, "op"); name == "pick" {
			field, _ := getString(op, "field")
			if field == "" {
				return nil, NewNodeError("validation_error", false, "pick requires a field")
			}
			picks[field] = true
		}
	}
	out := make(map[string]any, len(value))
	if len(picks) > 0 {
		for k, v := range value {
			if picks[k] {
				out[k] = v
			}
		}
	} else {
		for k, v := range value {
			out[k] = v
		}
	}
	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		name, _ := getString(op, "op")
		field, _ := getString(op, "field")
		switch name {
		case "pick":
			// Already applied in the first pass.
		case "rename":
			to, _ := getString(op, "to")
			if field == "" || to == "" {
				return nil, NewNodeError("validation_error", false, "rename requires field and to")
			}
			if v, present := out[field]; present {
				out[to] = v
				delete(out, field)
			}
		case "set_const":
			if field == "" {
				return nil, NewNodeError("validation_error", false, "set_const requires a field")
			}
			out[field] = op["value"]
		case "delete":
			if field == "" {
				return nil, NewNodeError("validation_error", false, "delete requires a field")
			}
			delete(out, field)
		default:
			return nil, NewNodeError("validation_error", false, "unknown transform op %q", name)
		}
	}
	return map[string]any{"value": out}, nil
}

func runLoopPlan(_ context.Context, rc *RunContext) (map[string]any, error) {
	items, ok := getArray(rc.Input, "items")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input items must be an array")
	}
	if len(items) > 100 {
		return nil, NewNodeError("validation_error", false, "items exceed 100")
	}
	maxIter := 100
	if v, ok := getNumber(rc.Config, "max_iterations"); ok {
		if !isFinite(v) || v < 1 || v > 100 {
			return nil, NewNodeError("validation_error", false, "max_iterations must be in [1,100]")
		}
		maxIter = int(v)
	}
	iterations := make([]any, 0, len(items))
	truncated := false
	for i, item := range items {
		if i >= maxIter {
			truncated = true
			break
		}
		iterations = append(iterations, map[string]any{"index": i, "item": item})
	}
	return map[string]any{"iterations": iterations, "truncated": truncated}, nil
}

func runMerge(_ context.Context, rc *RunContext) (map[string]any, error) {
	first, ok := getObject(rc.Input, "first")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input first must be an object")
	}
	second, ok := getObject(rc.Input, "second")
	if !ok {
		return nil, NewNodeError("validation_error", false, "input second must be an object")
	}
	conflict := getStringOr(rc.Config, "conflict", "last")
	if conflict != "first" && conflict != "last" && conflict != "error" {
		return nil, NewNodeError("validation_error", false, "conflict must be first, last, or error")
	}
	if len(first)+len(second) > 1000 {
		return nil, NewNodeError("validation_error", false, "merged object would exceed 1000 keys")
	}
	merged := make(map[string]any, len(first)+len(second))
	for k, v := range first {
		merged[k] = v
	}
	for k, v := range second {
		if _, exists := merged[k]; exists {
			switch conflict {
			case "first":
				continue
			case "error":
				return nil, NewNodeError("merge_conflict", false, "conflicting key %q", k)
			}
		}
		merged[k] = v
	}
	return map[string]any{"merged": merged}, nil
}

func runWait(ctx context.Context, rc *RunContext) (map[string]any, error) {
	maxStr := getStringOr(rc.Config, "max_duration", "5m")
	maxDur, err := time.ParseDuration(maxStr)
	if err != nil || maxDur <= 0 {
		return nil, NewNodeError("validation_error", false, "max_duration must be a positive duration")
	}
	durMS := 0.0
	if v, ok := getNumber(rc.Input, "duration_ms"); ok {
		durMS = v
	}
	if !isFinite(durMS) || durMS < 0 || time.Duration(durMS)*time.Millisecond > maxDur {
		return nil, NewNodeError("validation_error", false, "duration_ms exceeds configured maximum")
	}
	wait := time.Duration(durMS) * time.Millisecond
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, NewNodeError("cancelled", false, "wait cancelled")
	case <-timer.C:
		return map[string]any{"waited_ms": durMS}, nil
	}
}

// Approval identity hardening (Phase 7): a decision with no identified
// approver, or with no identified requester, is not auditable and is
// rejected instead of recorded. Approver and requester are still
// caller-asserted inputs — this node cannot authenticate a human — so
// the runtime MUST bind a decision to an authenticated approval record
// (approver identity + approval id in the audit trail) before it passes
// the decision here, and must never let workflow-authored data alone
// mint an approval. See the Phase 7 findings matrix.
func runApproval(_ context.Context, rc *RunContext) (map[string]any, error) {
	decision, present := getString(rc.Input, "decision")
	if !present || strings.TrimSpace(decision) == "" {
		return nil, NewNodeError("approval_required", false, "no approval decision has been recorded yet")
	}
	if decision != "approved" && decision != "rejected" {
		return nil, NewNodeError("validation_error", false, "decision must be approved or rejected")
	}
	approver, _ := getString(rc.Input, "approver")
	requester, _ := getString(rc.Input, "requester")
	if strings.TrimSpace(approver) == "" {
		return nil, NewNodeError("validation_error", false, "approval requires an identified approver")
	}
	if strings.TrimSpace(requester) == "" {
		return nil, NewNodeError("validation_error", false, "approval requires an identified requester")
	}
	if approver == requester {
		return nil, NewNodeError("validation_error", false, "approver must not be the requester")
	}
	return map[string]any{"approved": decision == "approved", "approver": approver}, nil
}

func runErrorHandler(_ context.Context, rc *RunContext) (map[string]any, error) {
	code, ok := getString(rc.Input, "error_code")
	if !ok || strings.TrimSpace(code) == "" {
		return nil, NewNodeError("validation_error", false, "input error_code is required")
	}
	policy, _ := getString(rc.Config, "policy")
	if policy != "retry" && policy != "fallback" && policy != "fail" {
		return nil, NewNodeError("validation_error", false, "config policy must be retry, fallback, or fail")
	}
	rawMsg, _ := getString(rc.Input, "message")
	return map[string]any{
		"action": policy, "error_code": code,
		"message": RedactError(errString(rawMsg)),
	}, nil
}

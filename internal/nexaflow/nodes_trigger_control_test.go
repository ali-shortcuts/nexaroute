package nexaflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

func signWebhook(t *testing.T, ts, body, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookVerifier(t *testing.T) {
	v := NewWebhookVerifier()
	secret := "s3cret"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := "hello"
	sig := signWebhook(t, ts, body, secret)
	if err := v.Verify(ts, sig, body, secret); err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}
	// Replay.
	if err := v.Verify(ts, sig, body, secret); err == nil || !strings.Contains(err.Error(), "webhook_replay") {
		t.Fatalf("replay error = %v", err)
	}
	// Bad signature.
	v2 := NewWebhookVerifier()
	if err := v2.Verify(ts, signWebhook(t, ts, "other", secret), body, secret); err == nil || !strings.Contains(err.Error(), "webhook_invalid_signature") {
		t.Fatalf("bad sig error = %v", err)
	}
	// Signature for another body must not verify (MAC covers body).
	if err := v2.Verify(ts, sig, body, secret); err != nil {
		t.Fatalf("body-bound signature rejected: %v", err)
	}
	// Expired.
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	if err := NewWebhookVerifier().Verify(old, signWebhook(t, old, body, secret), body, secret); err == nil || !strings.Contains(err.Error(), "webhook_expired") {
		t.Fatalf("expired error = %v", err)
	}
	// Oversize body.
	if err := NewWebhookVerifier().Verify(ts, sig, strings.Repeat("x", (1<<20)+1), secret); err == nil {
		t.Fatal("oversize body accepted")
	}
}

func TestWebhookNodeViaBackend(t *testing.T) {
	fb := newFakeBackend()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := "ping"
	sig := signWebhook(t, ts, body, "s3cret")
	out := runNode(t, "nexaroute.trigger.webhook", ownerPrincipal(),
		map[string]any{"timestamp": ts, "signature": sig, "body": body},
		map[string]any{"secret_ref": "webhook-secret"}, fb, false)
	if out["verified"] != true {
		t.Fatalf("webhook node = %v", out)
	}
	// Unknown secret ref fails closed.
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.trigger.webhook", "v1")
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(),
		map[string]any{"timestamp": ts, "signature": sig, "body": body},
		map[string]any{"secret_ref": "no-such-ref"}, fb, false); err == nil {
		t.Fatal("unknown secret ref accepted")
	}
}

func TestScheduleNode(t *testing.T) {
	out := runNode(t, "nexaroute.trigger.schedule", ownerPrincipal(),
		map[string]any{"last_run": "2026-10-27T08:00:00Z"},
		map[string]any{"interval": "1h"}, nil, false)
	if out["next_run"] != "2026-10-27T09:00:00Z" {
		t.Fatalf("interval next_run = %v", out)
	}
	out = runNode(t, "nexaroute.trigger.schedule", ownerPrincipal(),
		map[string]any{"last_run": "2026-10-27T08:00:00Z"},
		map[string]any{"daily": "09:00"}, nil, false)
	if out["next_run"] != "2026-10-27T09:00:00Z" {
		t.Fatalf("daily next_run = %v", out)
	}
	// Daily time already passed today rolls to tomorrow.
	out = runNode(t, "nexaroute.trigger.schedule", ownerPrincipal(),
		map[string]any{"last_run": "2026-10-27T10:00:00Z"},
		map[string]any{"daily": "09:00"}, nil, false)
	if out["next_run"] != "2026-10-28T09:00:00Z" {
		t.Fatalf("daily rollover = %v", out)
	}
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.trigger.schedule", "v1")
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(), nil,
		map[string]any{"interval": "1h", "daily": "09:00"}, nil, false); err == nil {
		t.Fatal("both interval and daily accepted")
	}
}

func TestHTTPNode(t *testing.T) {
	fb := newFakeBackend()
	fb.HTTPResp = HTTPResponse{Status: 200, Body: []byte("ok"), Headers: map[string]string{"Content-Type": "text/plain"}}
	out := runNode(t, "nexaroute.http.request", ownerPrincipal(),
		map[string]any{"url": "https://example.com/hook", "method": "POST", "body": "hi"}, nil, fb, true)
	if out["status"] != 200 {
		t.Fatalf("http status = %v", out)
	}
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.http.request", "v1")
	// Authorization in input is forbidden.
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(),
		map[string]any{"url": "https://example.com/", "headers": map[string]any{"Authorization": "Bearer x"}}, nil, fb, true); err == nil {
		t.Fatal("Authorization header in input accepted")
	}
	// Disallowed method.
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(),
		map[string]any{"url": "https://example.com/", "method": "TRACE"}, nil, fb, true); err == nil {
		t.Fatal("TRACE accepted")
	}
}

func TestControlNodes(t *testing.T) {
	out := runNode(t, "nexaroute.control.condition", ownerPrincipal(),
		map[string]any{"value": map[string]any{"health": map[string]any{"status": "healthy"}}},
		map[string]any{"field": "health.status", "operator": "eq", "expected": "healthy"}, nil, false)
	if out["result"] != true {
		t.Fatalf("condition = %v", out)
	}
	out = runNode(t, "nexaroute.control.branch", ownerPrincipal(),
		map[string]any{"condition_result": false},
		map[string]any{"true_branch": "healthy", "false_branch": "fallback"}, nil, false)
	if out["selected_branch"] != "fallback" {
		t.Fatalf("branch = %v", out)
	}
	out = runNode(t, "nexaroute.control.json_transform", ownerPrincipal(),
		map[string]any{"value": map[string]any{"a": float64(1), "b": float64(2), "c": float64(3)},
			"operations": []any{
				map[string]any{"op": "pick", "field": "a"},
				map[string]any{"op": "pick", "field": "b"},
				map[string]any{"op": "rename", "field": "a", "to": "alpha"},
				map[string]any{"op": "set_const", "field": "z", "value": "const"},
				map[string]any{"op": "delete", "field": "b"},
			}}, nil, nil, false)
	val, _ := out["value"].(map[string]any)
	if val["alpha"] != float64(1) || val["z"] != "const" || val["c"] != nil || val["b"] != nil {
		t.Fatalf("transform = %v", out)
	}
	reg := DefaultRegistry()
	spec, _ := reg.Lookup("nexaroute.control.json_transform", "v1")
	if _, err := reg.Execute(context.Background(), spec, ownerPrincipal(),
		map[string]any{"value": map[string]any{}, "operations": []any{map[string]any{"op": "eval", "field": "x"}}}, nil, nil, false); err == nil {
		t.Fatal("unknown transform op accepted")
	}
	// Loop bound and truncation.
	out = runNode(t, "nexaroute.control.loop_plan", ownerPrincipal(),
		map[string]any{"items": []any{1, 2, 3, 4, 5}},
		map[string]any{"max_iterations": float64(3)}, nil, false)
	if out["truncated"] != true {
		t.Fatalf("loop truncated = %v", out)
	}
	if iters, _ := out["iterations"].([]any); len(iters) != 3 {
		t.Fatalf("loop iterations = %v", out)
	}
}

func TestMergeConflictAndApprovalAndWait(t *testing.T) {
	reg := DefaultRegistry()
	mergeSpec, _ := reg.Lookup("nexaroute.control.merge", "v1")
	if _, err := reg.Execute(context.Background(), mergeSpec, ownerPrincipal(),
		map[string]any{"first": map[string]any{"a": float64(1)}, "second": map[string]any{"a": float64(2)}},
		map[string]any{"conflict": "error"}, nil, false); err == nil || !strings.Contains(err.Error(), "merge_conflict") {
		t.Fatalf("merge conflict error = %v", err)
	}
	out := runNode(t, "nexaroute.control.merge", ownerPrincipal(),
		map[string]any{"first": map[string]any{"a": float64(1)}, "second": map[string]any{"a": float64(2)}},
		map[string]any{"conflict": "first"}, nil, false)
	if out["merged"].(map[string]any)["a"] != float64(1) {
		t.Fatalf("merge first = %v", out)
	}
	// Approval: missing decision is an error, never success.
	approvalSpec, _ := reg.Lookup("nexaroute.control.approval", "v1")
	if _, err := reg.Execute(context.Background(), approvalSpec, ownerPrincipal(),
		map[string]any{"approver": "alice"}, nil, nil, false); err == nil || !strings.Contains(err.Error(), "approval_required") {
		t.Fatalf("missing approval error = %v", err)
	}
	if _, err := reg.Execute(context.Background(), approvalSpec, ownerPrincipal(),
		map[string]any{"decision": "approved", "approver": "alice", "requester": "alice"}, nil, nil, false); err == nil {
		t.Fatal("self-approval accepted")
	}
	out = runNode(t, "nexaroute.control.approval", ownerPrincipal(),
		map[string]any{"decision": "approved", "approver": "bob", "requester": "alice"}, nil, nil, false)
	if out["approved"] != true {
		t.Fatalf("approval = %v", out)
	}
	// Wait: real wait with ms durations, and cancellation.
	out = runNode(t, "nexaroute.control.wait", ownerPrincipal(),
		map[string]any{"duration_ms": float64(5)}, nil, nil, false)
	if out["waited_ms"] != float64(5) {
		t.Fatalf("wait = %v", out)
	}
	waitSpec, _ := reg.Lookup("nexaroute.control.wait", "v1")
	if _, err := reg.Execute(context.Background(), waitSpec, ownerPrincipal(),
		map[string]any{"duration_ms": float64(999999)}, map[string]any{"max_duration": "1s"}, nil, false); err == nil {
		t.Fatal("wait above max_duration accepted")
	}
	// Error handler sanitizes.
	out = runNode(t, "nexaroute.control.error_handler", ownerPrincipal(),
		map[string]any{"error_code": "boom", "message": "failed with Bearer abc123"},
		map[string]any{"policy": "fallback"}, nil, false)
	if out["action"] != "fallback" || strings.Contains(out["message"].(string), "abc123") {
		t.Fatalf("error handler = %v", out)
	}
	// Manual passthrough.
	out = runNode(t, "nexaroute.trigger.manual", ownerPrincipal(),
		map[string]any{"payload": map[string]any{"x": float64(1)}}, nil, nil, false)
	if out["payload"].(map[string]any)["x"] != float64(1) {
		t.Fatalf("manual = %v", out)
	}
}

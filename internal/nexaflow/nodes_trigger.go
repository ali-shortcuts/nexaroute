package nexaflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// credentialHeaders are never accepted from workflow input or config on
// the HTTP node, and are rejected (not silently dropped) at the HTTPDo
// choke point too. Authorization is the single exception, and only when
// the caller is the node itself setting it from a resolved secretRef —
// a workflow author must never be able to smuggle a credential header
// past the secretRef path. Cookie and proxy-authorization matter as much
// as Authorization: a session cookie IS a credential.
var credentialHeaders = []string{
	"authorization", "cookie", "set-cookie", "proxy-authorization",
	"api-key", "x-api-key", "x-goog-api-key", "x-auth-token", "x-access-token",
}

func isCredentialHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, h := range credentialHeaders {
		if lower == h {
			return true
		}
	}
	return false
}

// Trigger nodes start a workflow. None of them runs a server, a clock,
// or a dispatcher: this package has no HTTP server, no scheduler, and
// no durable state. Manual passes a payload through; webhook verifies a
// single delivered request; schedule computes the next fire time; the
// runtime owns listening, ticking, and dispatching.
//
// All trigger nodes require only an authenticated principal (Permission
// ""). A workflow.execute permission does not exist in internal/authz
// and is not invented here; admission of a workflow run is the runtime's
// responsibility.
func triggerNodeSpecs() []*NodeSpec {
	return []*NodeSpec{
		{
			ID: "nexaroute.trigger.manual", Version: SupportedVersion, Category: "trigger",
			Title:       "Manual trigger",
			Description: "Passes the supplied payload object through unchanged. No side effects.",
			InputSchema: []Field{
				{Name: "payload", Type: "object", Description: "Arbitrary payload object passed through to the workflow."},
			},
			OutputSchema: []Field{
				{Name: "payload", Type: "object"},
			},
			ConfigSchema: nil,
			Permission:   "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 64 << 10, MaxOutputBytes: 64 << 10,
			Run: runManualTrigger,
		},
		{
			ID: "nexaroute.trigger.webhook", Version: SupportedVersion, Category: "trigger",
			Title:       "Authenticated webhook trigger",
			Description: "Verifies one webhook delivery: HMAC-SHA256 over timestamp + '.' + body, 5-minute timestamp window, constant-time comparison, bounded 1 MiB body, in-memory replay cache. No HTTP server is provided by this package.",
			InputSchema: []Field{
				{Name: "timestamp", Type: "string", Required: true, Description: "Unix seconds or RFC3339 timestamp from the delivery."},
				{Name: "signature", Type: "string", Required: true, Description: "Hex HMAC-SHA256 signature."},
				{Name: "body", Type: "string", Required: true, Description: "Raw request body, at most 1 MiB."},
			},
			OutputSchema: []Field{
				{Name: "verified", Type: "boolean"},
				{Name: "body", Type: "string", Description: "The verified body, echoed back bounded."},
			},
			ConfigSchema: []Field{
				{Name: "secret_ref", Type: "secretRef", Required: true, Description: "Reference to the webhook secret; resolved by the backend. The secret value itself is never in config."},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20,
			Run: runWebhookTrigger,
		},
		{
			ID: "nexaroute.trigger.schedule", Version: SupportedVersion, Category: "trigger",
			Title:       "Schedule trigger (next-fire computation)",
			Description: "Pure next-fire computation from an interval duration (e.g. 1h) or a daily HH:MM UTC time. Ticking and dispatch are the runtime's job; this node never sleeps or schedules anything.",
			InputSchema: []Field{
				{Name: "last_run", Type: "string", Description: "RFC3339 timestamp of the previous run; defaults to now."},
			},
			OutputSchema: []Field{
				{Name: "next_run", Type: "string", Description: "RFC3339 next fire time, UTC."},
			},
			ConfigSchema: []Field{
				{Name: "interval", Type: "string", Description: "Go duration string, e.g. 1h30m. Exactly one of interval and daily must be set."},
				{Name: "daily", Type: "string", Description: "Daily time in UTC as HH:MM. Exactly one of interval and daily must be set."},
			},
			Permission: "", SideEffect: SideEffectNone,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 5 * time.Second, MaxTimeout: 10 * time.Second,
			MaxInputBytes: 4096, MaxOutputBytes: 4096,
			Run: runScheduleTrigger,
		},
		{
			ID: "nexaroute.http.request", Version: SupportedVersion, Category: "http",
			Title:       "HTTP request",
			Description: "One egress-guarded HTTP request: https except loopback, cross-origin redirects refused, response bounded. An Authorization header in input or config is rejected; credentials come only from a secretRef resolved by the backend. Requires operator policy approval when the URL is not loopback (see RequiresOperatorPolicy).",
			InputSchema: []Field{
				{Name: "url", Type: "string", Required: true, Description: "Request URL. https, or http only for loopback hosts."},
				{Name: "method", Type: "string", Description: "GET, POST, PUT, PATCH, or DELETE; default GET."},
				{Name: "headers", Type: "object", Description: "Request headers. Authorization is forbidden here."},
				{Name: "body", Type: "string", Description: "Request body text."},
			},
			OutputSchema: []Field{
				{Name: "status", Type: "number"},
				{Name: "body", Type: "string", Description: "Response body text, bounded."},
				{Name: "headers", Type: "object"},
			},
			ConfigSchema: []Field{
				{Name: "auth_secret_ref", Type: "secretRef", Description: "Optional secretRef whose resolved value is sent as a Bearer token. The only authorised credential path."},
			},
			Permission: "", SideEffect: SideEffectExternal,
			Retry:          RetryPolicy{MaxAttempts: 1},
			DefaultTimeout: 30 * time.Second, MaxTimeout: 120 * time.Second,
			MaxInputBytes: 256 << 10, MaxOutputBytes: 256 << 10,
			RequiresOperatorPolicy: true,
			Run:                    runHTTPRequest,
		},
	}
}

func runManualTrigger(_ context.Context, rc *RunContext) (map[string]any, error) {
	payload, _ := getObject(rc.Input, "payload")
	if payload == nil {
		payload = map[string]any{}
	}
	return map[string]any{"payload": payload}, nil
}

// WebhookVerifier verifies webhook deliveries with replay protection.
// The replay cache is in-memory only and bounded: entries older than the
// timestamp window are pruned on each verification, and the cache is
// capped at 10000 entries (oldest eviction is by pruning; a full cache
// rejects new signatures rather than growing unboundedly).
type WebhookVerifier struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

// NewWebhookVerifier returns an empty verifier.
func NewWebhookVerifier() *WebhookVerifier {
	return &WebhookVerifier{seen: make(map[string]time.Time), now: time.Now}
}

var defaultWebhookVerifier = NewWebhookVerifier()

const (
	webhookWindow     = 5 * time.Minute
	webhookMaxBody    = 1 << 20
	webhookCacheLimit = 10000
)

// VerifyWebhook verifies one delivery against the shared in-process
// verifier. Prefer NewWebhookVerifier for tests and isolated runtimes.
func VerifyWebhook(timestamp, signature, body, secret string) error {
	return defaultWebhookVerifier.Verify(timestamp, signature, body, secret)
}

// Verify checks timestamp window, replay, body bound, and HMAC.
// DELIBERATE DIFFERENCE from the video orchestrator's webhook check:
// the video check MACs only the timestamp. This verifier MACs
// timestamp + "." + body, so a signature cannot be replayed onto a
// different body. The video pattern is not copied verbatim because
// copying it would weaken the guarantee this node advertises.
func (v *WebhookVerifier) Verify(timestamp, signature, body, secret string) error {
	if len(body) > webhookMaxBody {
		return NewNodeError("validation_error", false, "webhook body exceeds 1 MiB")
	}
	now := time.Now()
	if v.now != nil {
		now = v.now()
	}
	ts, err := parseWebhookTimestamp(timestamp)
	if err != nil {
		return NewNodeError("validation_error", false, "webhook timestamp is not parseable")
	}
	if ts.Before(now.Add(-webhookWindow)) || ts.After(now.Add(webhookWindow)) {
		return NewNodeError("webhook_expired", false, "webhook timestamp outside the 5-minute window")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + body))
	expected := hex.EncodeToString(mac.Sum(nil))
	sigBytes, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return NewNodeError("validation_error", false, "webhook signature is not hex")
	}
	expBytes, _ := hex.DecodeString(expected)
	if len(sigBytes) != len(expBytes) || !hmac.Equal(sigBytes, expBytes) {
		return NewNodeError("webhook_invalid_signature", false, "webhook signature mismatch")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for key, at := range v.seen {
		if now.Sub(at) > webhookWindow {
			delete(v.seen, key)
		}
	}
	key := expected
	if _, dup := v.seen[key]; dup {
		return NewNodeError("webhook_replay", false, "webhook delivery already seen")
	}
	if len(v.seen) >= webhookCacheLimit {
		return NewNodeError("webhook_replay", false, "webhook replay cache is full")
	}
	v.seen[key] = now
	return nil
}

func parseWebhookTimestamp(ts string) (time.Time, error) {
	if secs, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(ts))
}

func runWebhookTrigger(ctx context.Context, rc *RunContext) (map[string]any, error) {
	_ = ctx
	timestamp, _ := getString(rc.Input, "timestamp")
	signature, _ := getString(rc.Input, "signature")
	body, _ := getString(rc.Input, "body")
	secretRef, _ := getString(rc.Config, "secret_ref")
	if timestamp == "" || signature == "" || body == "" {
		return nil, NewNodeError("validation_error", false, "webhook input requires timestamp, signature, and body")
	}
	if rc.Backend == nil {
		return nil, NewNodeError("backend_unavailable", false, "webhook secret resolution requires a backend")
	}
	secret, err := rc.Backend.ResolveSecretRef(secretRef)
	if err != nil {
		return nil, NewNodeError("secret_error", false, "webhook secret resolution failed")
	}
	if err := VerifyWebhook(timestamp, signature, body, secret); err != nil {
		return nil, err
	}
	return map[string]any{"verified": true, "body": body}, nil
}

func runScheduleTrigger(_ context.Context, rc *RunContext) (map[string]any, error) {
	intervalStr := getStringOr(rc.Config, "interval", "")
	dailyStr := getStringOr(rc.Config, "daily", "")
	if (intervalStr == "") == (dailyStr == "") {
		return nil, NewNodeError("validation_error", false, "schedule requires exactly one of interval and daily")
	}
	base := time.Now().UTC()
	if lastRun, ok := getString(rc.Input, "last_run"); ok && strings.TrimSpace(lastRun) != "" {
		parsed, err := time.Parse(time.RFC3339, lastRun)
		if err != nil {
			return nil, NewNodeError("validation_error", false, "last_run must be RFC3339")
		}
		base = parsed.UTC()
	}
	var next time.Time
	if intervalStr != "" {
		d, err := time.ParseDuration(intervalStr)
		if err != nil || d <= 0 || d > 366*24*time.Hour {
			return nil, NewNodeError("validation_error", false, "interval must be a positive duration up to 366 days")
		}
		next = base.Add(d)
	} else {
		parts := strings.Split(dailyStr, ":")
		if len(parts) != 2 {
			return nil, NewNodeError("validation_error", false, "daily must be HH:MM")
		}
		hour, err1 := strconv.Atoi(parts[0])
		min, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || hour < 0 || hour > 23 || min < 0 || min > 59 {
			return nil, NewNodeError("validation_error", false, "daily must be HH:MM in UTC")
		}
		candidate := time.Date(base.Year(), base.Month(), base.Day(), hour, min, 0, 0, time.UTC)
		if !candidate.After(base) {
			candidate = candidate.Add(24 * time.Hour)
		}
		next = candidate
	}
	return map[string]any{"next_run": next.Format(time.RFC3339)}, nil
}

var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

func runHTTPRequest(ctx context.Context, rc *RunContext) (map[string]any, error) {
	if err := requireBackend(rc); err != nil {
		return nil, err
	}
	targetURL, ok := getString(rc.Input, "url")
	if !ok || strings.TrimSpace(targetURL) == "" {
		return nil, NewNodeError("validation_error", false, "input url is required")
	}
	// Node-level URL shape check, independent of the backend's egress
	// policy: credentials embedded in the URL (userinfo) are never
	// accepted from workflow data, whatever the backend does with them.
	if parsed, err := url.Parse(targetURL); err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, NewNodeError("validation_error", false, "input url is malformed or contains embedded credentials")
	}
	method := getStringOr(rc.Input, "method", "GET")
	method = strings.ToUpper(method)
	if !httpMethods[method] {
		return nil, NewNodeError("validation_error", false, "method %q is not allowed", method)
	}
	headers := map[string]string{}
	if _, present := rc.Input["headers"]; present {
		raw, ok := getObject(rc.Input, "headers")
		if !ok {
			return nil, NewNodeError("validation_error", false, "headers must be an object of strings")
		}
		for k, v := range raw {
			s, ok := v.(string)
			if !ok {
				return nil, NewNodeError("validation_error", false, "header %q must be a string", k)
			}
			if isCredentialHeader(k) {
				return nil, NewNodeError("validation_error", false, "credential header %q is forbidden in input; use auth_secret_ref", k)
			}
			headers[k] = s
		}
	}
	if _, present := rc.Config["auth_secret_ref"]; present {
		ref, _ := getString(rc.Config, "auth_secret_ref")
		if strings.TrimSpace(ref) == "" {
			return nil, NewNodeError("validation_error", false, "auth_secret_ref must not be empty")
		}
		token, err := rc.Backend.ResolveSecretRef(ref)
		if err != nil {
			return nil, NewNodeError("secret_error", false, "auth secret resolution failed")
		}
		headers["Authorization"] = "Bearer " + token
	}
	body := []byte(getStringOr(rc.Input, "body", ""))
	resp, err := rc.Backend.HTTPDo(ctx, method, targetURL, headers, body)
	if err != nil {
		return nil, err
	}
	// The resolved token is never returned; headers echoed are the
	// response headers only, redacted by Execute.
	return map[string]any{
		"status": resp.Status, "body": truncateForOutput(string(resp.Body), 64<<10),
		"headers": resp.Headers,
	}, nil
}

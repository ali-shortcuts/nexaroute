// Package livecanary implements the opt-in, bounded provider/protocol live
// conformance canary for issue #436.
//
// Fixture tests remain the default evidence source. This package only sends
// real requests when an operator explicitly runs the CLI with
// --acknowledge-cost and operator-supplied credentials. It never runs from
// public CI, never mutates the manifest, and never upgrades a claim by itself:
// a successful scenario is evidence for one provider/deployment on one date.
//
// Bounds are deliberately hard-coded ceilings rather than tunable infinity:
// calls, output tokens, input-token cost estimate, per-call timeout and overall
// timeout all have absolute maxima. Secrets are collected from the provider
// configuration's resolved credentials and redacted from every error and report
// string before they can be written.
package livecanary

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

const (
	// ReportSchema identifies the evidence document written by the canary.
	ReportSchema = "conformance.live-canary.v1"

	DefaultMaxCalls              = 6
	MaxCalls                     = 12
	DefaultMaxOutputTokens       = 64
	MaxOutputTokens              = 4096
	DefaultMaxInputTokens        = 2048
	MaxInputTokens               = 8192
	DefaultCallTimeout           = 30 * time.Second
	MaxCallTimeout               = 2 * time.Minute
	DefaultOverallTimeout        = 3 * time.Minute
	MaxOverallTimeout            = 10 * time.Minute
	DefaultMaxCostUSD            = 0.10
	maxResponseBytes       int64 = 1 << 20
	maxErrorSnippet              = 512
)

// ErrMissingCredential is returned before any network call when the operator
// has not supplied a credential for a provider that requires one.
var ErrMissingCredential = errors.New("missing provider credential")

// Scenario status and overall outcome values.
const (
	StatusPass     = "PASS"
	StatusFail     = "FAIL"
	StatusSkip     = "SKIP"
	StatusCanceled = "CANCELED"

	OutcomePass       = "PASS"
	OutcomePartial    = "PARTIAL"
	OutcomeFailed     = "FAILED"
	OutcomeCanceled   = "CANCELED"
	OutcomeNoEvidence = "NO-EVIDENCE"
)

// Limits are the applied budgets recorded in the evidence report.
type Limits struct {
	MaxCalls              int      `json:"max_calls"`
	MaxOutputTokens       int      `json:"max_output_tokens"`
	MaxInputTokens        int      `json:"max_input_tokens"`
	CallTimeoutSeconds    int      `json:"call_timeout_seconds"`
	OverallTimeoutSeconds int      `json:"overall_timeout_seconds"`
	MaxCostUSD            float64  `json:"max_cost_usd"`
	CallsUsed             int      `json:"calls_used"`
	EstimatedCostUSD      *float64 `json:"estimated_cost_usd,omitempty"`
	ObservedCostUSD       *float64 `json:"observed_cost_usd,omitempty"`
	CostEstimateKnown     bool     `json:"cost_estimate_known"`
}

// ProviderEvidence names exactly what was tested. A canary never claims a
// dialect-wide result from one deployment.
type ProviderEvidence struct {
	ID       string `json:"id"`
	Dialect  string `json:"dialect"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	AuthMode string `json:"auth_mode,omitempty"`
}

// ScenarioResult is one bounded scenario outcome.
type ScenarioResult struct {
	ID           string   `json:"id"`
	Status       string   `json:"status"`
	Dimensions   []string `json:"dimensions,omitempty"`
	Evidence     string   `json:"evidence,omitempty"`
	Error        string   `json:"error,omitempty"`
	Note         string   `json:"note,omitempty"`
	LatencyMS    int64    `json:"latency_ms"`
	Calls        int      `json:"calls"`
	HTTPStatus   int      `json:"http_status,omitempty"`
	InputTokens  int      `json:"input_tokens,omitempty"`
	OutputTokens int      `json:"output_tokens,omitempty"`
}

// Report is the evidence document. It records provider, protocol dialect,
// contract version, date, every scenario, the applied limits and the dimensions
// that actually produced live evidence.
type Report struct {
	Schema               string           `json:"schema"`
	ManifestVersion      string           `json:"manifest_version"`
	GatewayVersion       string           `json:"gateway_version,omitempty"`
	GeneratedAt          string           `json:"generated_at"`
	Provider             ProviderEvidence `json:"provider"`
	Limits               Limits           `json:"limits"`
	Scenarios            []ScenarioResult `json:"scenarios"`
	VerifiedDimensions   []string         `json:"verified_dimensions"`
	FailedDimensions     []string         `json:"failed_dimensions,omitempty"`
	NotCoveredDimensions []string         `json:"not_covered_dimensions"`
	Limitations          []string         `json:"limitations"`
	Outcome              string           `json:"outcome"`
}

// Options configures one canary run. Provider must already identify the real
// upstream; credentials are read from the provider configuration's resolved
// api_key/api_key_env and credential entries (or cloud env names).
type Options struct {
	Provider       config.ProviderConfig
	Model          config.ModelConfig
	GatewayVersion string
	Scenarios      []string

	MaxCalls        int
	MaxOutputTokens int
	MaxInputTokens  int
	CallTimeout     time.Duration
	OverallTimeout  time.Duration
	MaxCostUSD      float64

	Now func() time.Time
}

// Runner is a single-use canary bound to one provider adapter.
type Runner struct {
	provider      config.ProviderConfig
	model         config.ModelConfig
	upstreamModel string
	gateway       string
	adapter       providers.Adapter
	scenarios     []scenario
	limits        Limits
	callTimeout   time.Duration
	overall       time.Duration
	redactor      *Redactor
	now           func() time.Time
	calls         int
	inCost        float64
	outCost       float64
	pricingKnown  bool
	observedIn    int
	observedOut   int
}

// New validates the operator input, checks credentials before any network call,
// applies the hard budget ceilings and builds the provider adapter.
func New(opts Options) (*Runner, error) {
	p := opts.Provider
	p.ApplyDefaults()
	m := opts.Model
	m.ID = strings.TrimSpace(m.ID)
	m.Model = strings.TrimSpace(m.Model)
	if m.ID == "" && m.Model == "" {
		return nil, errors.New("canary requires a model (configured model id or upstream model name)")
	}
	if m.ID == "" {
		m.ID = m.Model
	}
	if m.Model == "" {
		m.Model = m.ID
	}
	if len(p.Models) == 0 {
		p.Models = []config.ModelConfig{m}
	}
	if err := config.ValidateProviderConfig(p); err != nil {
		return nil, fmt.Errorf("provider configuration is not usable for a canary: %w", err)
	}
	if !credentialsAvailable(p) {
		name := strings.TrimSpace(p.APIKeyEnv)
		if name == "" {
			name = "api_key_env"
		}
		return nil, fmt.Errorf("%w: provider %q has no resolved credential; set environment variable %q (or configure an enabled credential). The canary made no network call", ErrMissingCredential, p.ID, name)
	}
	if p.Type == "bedrock" && strings.TrimSpace(getenv(p.SecretKeyEnv)) == "" {
		return nil, fmt.Errorf("%w: provider %q bedrock secret access key env %q is not set. The canary made no network call", ErrMissingCredential, p.ID, p.SecretKeyEnv)
	}

	scenarios, err := ResolveScenarios(opts.Scenarios)
	if err != nil {
		return nil, err
	}
	limits, callTimeout, overall, err := normalizeLimits(opts, p, scenarios)
	if err != nil {
		return nil, err
	}
	estimate, known := estimateCost(p, m, limits, scenarios)
	limits.EstimatedCostUSD = estimate
	limits.CostEstimateKnown = known
	if known && estimate != nil && *estimate > limits.MaxCostUSD {
		return nil, fmt.Errorf("estimated canary cost $%.4f exceeds --max-cost-usd $%.4f; reduce scenarios or token limits, or raise the explicit cost cap", *estimate, limits.MaxCostUSD)
	}

	adapter, err := providers.NewAdapter(p, callTimeout)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	r := &Runner{
		provider:      p,
		model:         m,
		upstreamModel: m.Model,
		gateway:       strings.TrimSpace(opts.GatewayVersion),
		adapter:       adapter,
		scenarios:     scenarios,
		limits:        limits,
		callTimeout:   callTimeout,
		overall:       overall,
		redactor:      NewRedactor(secretsForProvider(p)),
		now:           now,
		inCost:        m.InputCostPerMTok,
		outCost:       m.OutputCostPerMTok,
		pricingKnown:  m.InputCostPerMTok > 0 || m.OutputCostPerMTok > 0,
	}
	return r, nil
}

// Close releases the adapter's idle connections.
func (r *Runner) Close() {
	if r != nil && r.adapter != nil {
		providers.CloseIdleConnections(r.adapter)
	}
}

// Run executes the selected scenarios within the per-call and overall budgets.
// Runtime failures are evidence in the report; only construction failures are
// returned as errors.
func (r *Runner) Run(ctx context.Context) Report {
	if ctx == nil {
		ctx = context.Background()
	}
	report := Report{
		Schema:          ReportSchema,
		ManifestVersion: providers.ManifestVersion,
		GatewayVersion:  r.gateway,
		GeneratedAt:     r.now().UTC().Format(time.RFC3339),
		Provider: ProviderEvidence{
			ID:       r.provider.ID,
			Dialect:  r.provider.Type,
			BaseURL:  sanitizeBaseURL(r.provider.BaseURL),
			Model:    r.upstreamModel,
			AuthMode: r.provider.AuthMode,
		},
		Limits:    r.limits,
		Scenarios: make([]ScenarioResult, 0, len(r.scenarios)),
	}

	overallCtx, cancel := context.WithTimeout(ctx, r.overall)
	defer cancel()
	stopRemaining := ""
	incomplete := false
	for _, sc := range r.scenarios {
		if stopRemaining != "" {
			incomplete = true
			report.Scenarios = append(report.Scenarios, ScenarioResult{
				ID: sc.ID, Status: StatusSkip, Dimensions: dimensionsAsStrings(sc.Dimensions), Note: stopRemaining,
			})
			continue
		}
		if overallCtx.Err() != nil {
			report.Scenarios = append(report.Scenarios, ScenarioResult{
				ID: sc.ID, Status: StatusCanceled, Dimensions: dimensionsAsStrings(sc.Dimensions),
				Error: r.redact("canary context ended before this scenario: " + contextError(overallCtx)),
			})
			continue
		}
		if unsupported, note := scenarioUnsupported(r.provider.Type, sc); unsupported {
			report.Scenarios = append(report.Scenarios, ScenarioResult{
				ID: sc.ID, Status: StatusSkip, Dimensions: dimensionsAsStrings(sc.Dimensions), Note: note,
			})
			continue
		}
		if sc.Applies != nil && !sc.Applies(r.provider) {
			report.Scenarios = append(report.Scenarios, ScenarioResult{
				ID: sc.ID, Status: StatusSkip, Dimensions: dimensionsAsStrings(sc.Dimensions),
				Note: "scenario is not applicable to dialect " + r.provider.Type + "; the encoder cannot exercise it faithfully",
			})
			continue
		}
		if r.calls >= r.limits.MaxCalls {
			incomplete = true
			report.Scenarios = append(report.Scenarios, ScenarioResult{
				ID: sc.ID, Status: StatusSkip, Dimensions: dimensionsAsStrings(sc.Dimensions),
				Note: "call budget exhausted (max_calls reached)",
			})
			continue
		}

		callCtx, callCancel := context.WithTimeout(overallCtx, r.callTimeout)
		res := r.runScenario(callCtx, sc)
		callErr := callCtx.Err()
		callCancel()
		if callErr != nil && res.Status == StatusFail {
			if overallCtx.Err() != nil {
				res.Status = StatusCanceled
				res.Error = "canary canceled: " + contextError(callCtx)
			} else if errors.Is(callErr, context.DeadlineExceeded) {
				res.Error = fmt.Sprintf("call timed out after %s: %s", r.callTimeout, res.Error)
			}
		}
		res = r.finishResult(res)
		report.Scenarios = append(report.Scenarios, res)
		report.Limits.CallsUsed = r.calls
		r.observedIn += res.InputTokens
		r.observedOut += res.OutputTokens
		if r.pricingKnown {
			observed := (float64(r.observedIn)*r.inCost + float64(r.observedOut)*r.outCost) / 1e6
			report.Limits.ObservedCostUSD = &observed
			if observed > r.limits.MaxCostUSD {
				stopRemaining = "observed cost exceeded --max-cost-usd; remaining scenarios were not run"
				incomplete = true
			}
		}
		if res.Status == StatusFail && (res.HTTPStatus == http.StatusUnauthorized || res.HTTPStatus == http.StatusForbidden) && sc.ID != "error_envelope" && sc.ID != "auth_failure" {
			stopRemaining = "provider rejected the credential (HTTP " + fmt.Sprint(res.HTTPStatus) + "); remaining scenarios were not run"
		}
		if overallCtx.Err() != nil {
			stopRemaining = "canary context ended; remaining scenarios were not run"
		}
	}

	report.Limits.CallsUsed = r.calls
	r.finalize(&report, incomplete)
	return report
}

func (r *Runner) finishResult(res ScenarioResult) ScenarioResult {
	res.Evidence = r.redact(res.Evidence)
	res.Error = r.redact(res.Error)
	res.Note = r.redact(res.Note)
	for i := range res.Dimensions {
		res.Dimensions[i] = r.redact(res.Dimensions[i])
	}
	if res.Status == StatusCanceled {
		res.Error = r.redact(res.Error)
	}
	return res
}

func (r *Runner) finalize(report *Report, incomplete bool) {
	verified := map[string]bool{}
	failed := map[string]bool{}
	passes, failures, skips, canceled := 0, 0, 0, 0
	for _, res := range report.Scenarios {
		switch res.Status {
		case StatusPass:
			passes++
			for _, d := range res.Dimensions {
				verified[d] = true
			}
		case StatusFail:
			failures++
			for _, d := range res.Dimensions {
				failed[d] = true
			}
		case StatusSkip:
			skips++
		case StatusCanceled:
			canceled++
		}
	}
	for d := range verified {
		report.VerifiedDimensions = append(report.VerifiedDimensions, d)
	}
	sort.Strings(report.VerifiedDimensions)
	for d := range failed {
		if verified[d] {
			continue
		}
		report.FailedDimensions = append(report.FailedDimensions, d)
	}
	sort.Strings(report.FailedDimensions)
	for _, dim := range providers.ConformanceDimensions() {
		name := string(dim)
		if !verified[name] && !failed[name] {
			report.NotCoveredDimensions = append(report.NotCoveredDimensions, name)
		}
	}

	switch {
	case canceled > 0:
		report.Outcome = OutcomeCanceled
	case failures > 0 && passes > 0:
		report.Outcome = OutcomePartial
	case failures > 0:
		report.Outcome = OutcomeFailed
	case incomplete && passes > 0:
		report.Outcome = OutcomePartial
	case passes > 0:
		report.Outcome = OutcomePass
	default:
		report.Outcome = OutcomeNoEvidence
	}
	if incomplete {
		report.Limitations = append(report.Limitations, "The selected scenario set did not finish; passed scenarios are preserved but the run is PARTIAL, not PASS.")
	}

	report.Limitations = append(report.Limitations,
		"Live canary evidence is scoped to this provider/deployment, model and date; it does not promote every provider that shares the dialect.",
		"A live pass proves the contract on the run date only. Re-run after provider/version changes; fixture evidence keeps its own FIXTURE-VERIFIED state.",
		"Live scenarios do not automatically modify docs/COMPATIBILITY.md or the conformance manifest. A dialect claim is promoted only after operator review of this report.",
		"retry_after and idempotency are not induced by this canary; cache-token behavior is claimed only when the provider reports cache token counts.",
		"Provider-side cancellation semantics are not proven by a client-side abort; cancellation remains PARTIAL until a provider-specific observation exists.",
	)
	if !report.Limits.CostEstimateKnown {
		report.Limitations = append(report.Limitations,
			"Provider pricing is not configured for this model: the hard cost guard is the call/output-token budget, not a dollar ceiling.")
	}
	for _, res := range report.Scenarios {
		if res.ID == "auth_failure" {
			report.Limitations = append(report.Limitations,
				"auth_failure used a deliberately invalid credential, never the operator credential; for Bedrock only the access-key id was replaced (SigV4 still signs with the configured secret env, which AWS rejects for the unknown key id).")
			break
		}
	}
	for _, res := range report.Scenarios {
		if res.Status == StatusSkip && res.Note != "" {
			report.Limitations = append(report.Limitations, "skipped "+res.ID+": "+res.Note)
		}
	}
	if skips > 0 && passes == 0 && failures == 0 && canceled == 0 {
		report.Limitations = append(report.Limitations, "No scenario produced evidence; no dimension can be marked LIVE-VERIFIED from this run.")
	}
}

func (r *Runner) runScenario(ctx context.Context, sc scenario) ScenarioResult {
	switch sc.ID {
	case "text":
		return r.runText(ctx, sc)
	case "stream":
		return r.runStream(ctx, sc)
	case "stream_usage":
		return r.runStreamUsage(ctx, sc)
	case "auth_failure":
		return r.runAuthFailure(ctx, sc)
	case "tools":
		return r.runTools(ctx, sc)
	case "tool_result":
		return r.runToolResult(ctx, sc)
	case "structured_output":
		return r.runStructuredOutput(ctx, sc)
	case "images":
		return r.runImages(ctx, sc)
	case "parallel_tools":
		return r.runParallelTools(ctx, sc)
	case "reasoning":
		return r.runReasoning(ctx, sc)
	case "error_envelope":
		return r.runErrorEnvelope(ctx, sc)
	default:
		return ScenarioResult{ID: sc.ID, Status: StatusFail, Dimensions: dimensionsAsStrings(sc.Dimensions), Error: "unknown scenario"}
	}
}

func (r *Runner) redact(s string) string {
	if r == nil || r.redactor == nil {
		return s
	}
	return r.redactor.Redact(s)
}

func (r *Runner) redactSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxErrorSnippet {
		s = s[:maxErrorSnippet] + "..."
	}
	return r.redact(s)
}

func (r *Runner) scenarioOutputTokens(id string) int {
	return scenarioCallOutputTokens(id, r.provider.Type, r.limits.MaxOutputTokens)
}

// Redactor removes known provider credentials and common auth-header values
// from report strings.
type Redactor struct {
	secrets  []string
	auth     *regexp.Regexp
	userinfo *regexp.Regexp
}

var authHeaderPattern = regexp.MustCompile(`(?i)\b(bearer|x-api-key|x-goog-api-key|api-key|authorization)\b\s*[:=]?\s*(?:(?:bearer|basic|digest|negotiate|token)\s+)?[A-Za-z0-9._~+/=\-]{4,}`)
var urlUserInfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)

// NewRedactor builds a redactor from all non-empty secret values. Secrets are
// matched longest-first so one secret that contains another cannot leak a tail.
func NewRedactor(secrets []string) *Redactor {
	out := make([]string, 0, len(secrets))
	seen := map[string]bool{}
	for _, s := range secrets {
		if len(strings.TrimSpace(s)) < 4 || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return &Redactor{secrets: out, auth: authHeaderPattern, userinfo: urlUserInfoPattern}
}

// Redact removes secret material from s.
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	s = r.auth.ReplaceAllString(s, "$1 [REDACTED]")
	s = r.userinfo.ReplaceAllString(s, "$1[REDACTED]@")
	return s
}

func credentialsAvailable(p config.ProviderConfig) bool {
	if strings.EqualFold(strings.TrimSpace(p.AuthMode), "none") {
		return true
	}
	return len(p.ResolvedCredentials()) > 0
}

func secretsForProvider(p config.ProviderConfig) []string {
	out := append([]string(nil), p.ResolvedCredentials()...)
	for _, name := range []string{p.SecretKeyEnv, p.SessionTokenEnv} {
		if name == "" {
			continue
		}
		if v := getenv(name); v != "" {
			out = append(out, v)
		}
	}
	for name, value := range p.Headers {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") ||
			strings.Contains(lower, "auth") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "cookie") {
			out = append(out, value)
		}
	}
	if u, err := url.Parse(p.ProxyURL); err == nil && u.User != nil {
		out = append(out, u.User.Username())
		if password, ok := u.User.Password(); ok {
			out = append(out, password)
		}
	}
	return out
}

// getenv is kept behind a package variable so focused tests can supply cloud
// credentials without mutating the process environment.
var getenv = func(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return os.Getenv(name)
}

func sanitizeBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func contextError(ctx context.Context) string {
	if err := ctx.Err(); err != nil {
		return err.Error()
	}
	return "unknown"
}

func normalizeLimits(opts Options, p config.ProviderConfig, scenarios []scenario) (Limits, time.Duration, time.Duration, error) {
	l := Limits{
		MaxCalls:              opts.MaxCalls,
		MaxOutputTokens:       opts.MaxOutputTokens,
		MaxInputTokens:        opts.MaxInputTokens,
		CallTimeoutSeconds:    int(opts.CallTimeout / time.Second),
		OverallTimeoutSeconds: int(opts.OverallTimeout / time.Second),
		MaxCostUSD:            opts.MaxCostUSD,
	}
	if l.MaxCalls == 0 {
		l.MaxCalls = DefaultMaxCalls
	}
	if l.MaxOutputTokens == 0 {
		l.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if l.MaxInputTokens == 0 {
		l.MaxInputTokens = DefaultMaxInputTokens
	}
	if l.MaxCostUSD == 0 {
		l.MaxCostUSD = DefaultMaxCostUSD
	}
	callTimeout := opts.CallTimeout
	if callTimeout == 0 {
		callTimeout = DefaultCallTimeout
	}
	overall := opts.OverallTimeout
	if overall == 0 {
		overall = DefaultOverallTimeout
	}
	if l.MaxCalls < 1 || l.MaxCalls > MaxCalls {
		return Limits{}, 0, 0, fmt.Errorf("--max-calls must be between 1 and %d", MaxCalls)
	}
	if l.MaxOutputTokens < 1 || l.MaxOutputTokens > MaxOutputTokens {
		return Limits{}, 0, 0, fmt.Errorf("--max-output-tokens must be between 1 and %d", MaxOutputTokens)
	}
	if l.MaxInputTokens < 1 || l.MaxInputTokens > MaxInputTokens {
		return Limits{}, 0, 0, fmt.Errorf("--max-input-tokens must be between 1 and %d", MaxInputTokens)
	}
	if callTimeout < time.Second || callTimeout > MaxCallTimeout {
		return Limits{}, 0, 0, fmt.Errorf("--call-timeout must be between 1s and %s", MaxCallTimeout)
	}
	if overall < time.Second || overall > MaxOverallTimeout {
		return Limits{}, 0, 0, fmt.Errorf("--overall-timeout must be between 1s and %s", MaxOverallTimeout)
	}
	if overall < callTimeout {
		return Limits{}, 0, 0, errors.New("--overall-timeout must be greater than or equal to --call-timeout")
	}
	if l.MaxCostUSD <= 0 {
		return Limits{}, 0, 0, errors.New("--max-cost-usd must be greater than zero")
	}
	needed := 0
	for _, sc := range scenarios {
		needed += sc.Calls
	}
	if needed > l.MaxCalls {
		return Limits{}, 0, 0, fmt.Errorf("selected scenarios need %d calls but --max-calls is %d", needed, l.MaxCalls)
	}
	for _, sc := range scenarios {
		if sc.ID != "reasoning" {
			continue
		}
		required := 512
		if p.Type == "anthropic_compatible" {
			required = 2048
		}
		if l.MaxOutputTokens < required {
			return Limits{}, 0, 0, fmt.Errorf("the reasoning scenario requires --max-output-tokens >= %d for %s (Anthropic thinking requires max_tokens above a 1024-token budget)", required, p.Type)
		}
	}
	l.CallTimeoutSeconds = int(callTimeout / time.Second)
	l.OverallTimeoutSeconds = int(overall / time.Second)
	return l, callTimeout, overall, nil
}

func estimateCost(p config.ProviderConfig, m config.ModelConfig, limits Limits, scenarios []scenario) (*float64, bool) {
	if m.InputCostPerMTok <= 0 && m.OutputCostPerMTok <= 0 {
		return nil, false
	}
	total := 0.0
	for _, sc := range scenarios {
		total += float64(limits.MaxInputTokens) * m.InputCostPerMTok / 1e6
		total += float64(scenarioCallOutputTokens(sc.ID, p.Type, limits.MaxOutputTokens)) * m.OutputCostPerMTok / 1e6
	}
	return &total, true
}

// Summary renders a compact operator-facing result.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "conformance live canary: provider=%s dialect=%s model=%s\n", r.Provider.ID, r.Provider.Dialect, r.Provider.Model)
	fmt.Fprintf(&b, "outcome=%s date=%s calls=%d/%d\n", r.Outcome, r.GeneratedAt, r.Limits.CallsUsed, r.Limits.MaxCalls)
	if r.Limits.EstimatedCostUSD != nil {
		fmt.Fprintf(&b, "estimated cost ceiling=$%.6f max_cost=$%.6f\n", *r.Limits.EstimatedCostUSD, r.Limits.MaxCostUSD)
	} else {
		fmt.Fprintf(&b, "estimated cost=unknown max_cost=$%.6f (call/token budgets still apply)\n", r.Limits.MaxCostUSD)
	}
	for _, res := range r.Scenarios {
		fmt.Fprintf(&b, "  %-7s %-18s", res.Status, res.ID)
		if res.HTTPStatus > 0 {
			fmt.Fprintf(&b, " http=%d", res.HTTPStatus)
		}
		if res.LatencyMS > 0 {
			fmt.Fprintf(&b, " %dms", res.LatencyMS)
		}
		if len(res.Dimensions) > 0 && res.Status == StatusPass {
			fmt.Fprintf(&b, " dims=%s", strings.Join(res.Dimensions, ","))
		}
		if res.Error != "" {
			fmt.Fprintf(&b, " error=%s", res.Error)
		}
		if res.Note != "" {
			fmt.Fprintf(&b, " note=%s", res.Note)
		}
		b.WriteByte('\n')
	}
	if len(r.VerifiedDimensions) > 0 {
		fmt.Fprintf(&b, "provider-scoped dimensions with live evidence in this run (not a manifest promotion): %s\n", strings.Join(r.VerifiedDimensions, ", "))
	}
	if len(r.NotCoveredDimensions) > 0 {
		fmt.Fprintf(&b, "not covered by this run: %s\n", strings.Join(r.NotCoveredDimensions, ", "))
	}
	return b.String()
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
)

// provider-accept is the operator-triggered live acceptance command for
// issue #394. It performs bounded model discovery and one-token generation
// probes against explicitly selected real provider tenants and writes a
// redacted JSON report. It never changes provider/model settings, never prints
// credential values, and refuses to make network calls without --confirm-live.
// Like every config loader it may perform the existing legacy secret-at-rest
// migration before reading.
//
// It is intentionally not part of public CI: no test invokes it with real
// credentials, and the hermetic tests below use loopback fake upstreams only.

const (
	providerAcceptSchema       = "nexaroute.provider-acceptance.v1"
	providerAcceptMaxTimeout   = 60 * time.Second
	providerAcceptMaxModels    = 100
	providerAcceptMaxCalls     = 100
	providerAcceptMaxErrorRune = 1024
)

type providerAcceptReport struct {
	Schema          string                 `json:"schema"`
	GeneratedAt     string                 `json:"generated_at"`
	Mode            string                 `json:"mode"`
	Bounds          providerAcceptBounds   `json:"bounds"`
	OK              bool                   `json:"ok"`
	Providers       []providerAcceptResult `json:"providers"`
	Totals          providerAcceptTotals   `json:"totals"`
	Warnings        []string               `json:"warnings,omitempty"`
	CredentialNotes []string               `json:"credential_notes,omitempty"`
}

type providerAcceptBounds struct {
	DiscoveryTimeoutSeconds  float64 `json:"discovery_timeout_seconds"`
	GenerationTimeoutSeconds float64 `json:"generation_timeout_seconds"`
	MaxOutputTokensPerCall   int     `json:"max_output_tokens_per_call"`
	MaxModelsPerProvider     int     `json:"max_models_per_provider"`
	MaxUpstreamCalls         int     `json:"max_upstream_calls"`
	UpstreamCalls            int     `json:"upstream_calls"`
}

type providerAcceptTotals struct {
	Providers              int `json:"providers"`
	Passed                 int `json:"passed"`
	Degraded               int `json:"degraded"`
	Failed                 int `json:"failed"`
	Configured             int `json:"configured"`
	Unsupported            int `json:"unsupported"`
	ModelsTested           int `json:"models_tested"`
	ModelsPassed           int `json:"models_passed"`
	ModelsFailed           int `json:"models_failed"`
	DiscoveryAttempted     int `json:"discovery_attempted"`
	DiscoverySucceeded     int `json:"discovery_succeeded"`
	DiscoveryFailed        int `json:"discovery_failed"`
	DiscoveryUnsupported   int `json:"discovery_unsupported"`
	ModelsDiscoveredTotal  int `json:"models_discovered_total"`
	InsufficientCredential int `json:"insufficient_credential"`
}

type providerAcceptResult struct {
	Provider             string                `json:"provider"`
	Protocol             string                `json:"protocol"`
	AuthMode             string                `json:"auth_mode"`
	BaseURL              string                `json:"base_url"`
	CredentialSource     string                `json:"credential_source"`
	CredentialConfigured bool                  `json:"credential_configured"`
	Status               string                `json:"status"`
	Discovery            providerDiscovery     `json:"discovery"`
	Models               []providerModelResult `json:"models"`
	Warnings             []string              `json:"warnings,omitempty"`
	Error                string                `json:"error,omitempty"`
}

type providerDiscovery struct {
	Supported  bool     `json:"supported"`
	Attempted  bool     `json:"attempted"`
	OK         bool     `json:"ok"`
	StatusCode int      `json:"status_code,omitempty"`
	ModelCount int      `json:"model_count"`
	Models     []string `json:"models,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type providerModelResult struct {
	Model             string `json:"model"`
	Source            string `json:"source"`
	PreviousTestState string `json:"previous_test_state"`
	Result            string `json:"result"`
	StatusCode        int    `json:"status_code,omitempty"`
	LatencyMS         int64  `json:"latency_ms,omitempty"`
	MaxOutputTokens   int    `json:"max_output_tokens"`
	Error             string `json:"error,omitempty"`
}

type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

func runProviderAccept(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("provider-accept", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON config")
	outPath := fs.String("out", "", "redacted JSON report path (default provider-acceptance-<UTC>.json)")
	timeout := fs.Duration("timeout", 15*time.Second, "per upstream call timeout (max 60s)")
	maxModels := fs.Int("max-models", 10, "maximum models tested per provider (1-100)")
	maxCalls := fs.Int("max-calls", 20, "maximum total upstream calls (1-100)")
	confirmLive := fs.Bool("confirm-live", false, "required acknowledgment that selected models will receive real (possibly billable) requests")
	discover := fs.Bool("discover", true, "attempt bounded model discovery for supported providers")
	test := fs.Bool("test", true, "run one-token generation probes for selected models")
	var providerIDs, modelIDs stringListFlag
	fs.Var(&providerIDs, "provider", "provider id to accept (repeatable or comma-separated); required when --test=true")
	fs.Var(&modelIDs, "model", "model id to test (repeatable or comma-separated); defaults to enabled configured models")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*discover && !*test {
		fmt.Fprintln(stderr, "nexaroute provider-accept: --discover and --test cannot both be false")
		return 2
	}
	if *timeout <= 0 || *timeout > providerAcceptMaxTimeout {
		fmt.Fprintf(stderr, "nexaroute provider-accept: --timeout must be between 1ns and %s\n", providerAcceptMaxTimeout)
		return 2
	}
	if *maxModels < 1 || *maxModels > providerAcceptMaxModels {
		fmt.Fprintf(stderr, "nexaroute provider-accept: --max-models must be between 1 and %d\n", providerAcceptMaxModels)
		return 2
	}
	if *maxCalls < 1 || *maxCalls > providerAcceptMaxCalls {
		fmt.Fprintf(stderr, "nexaroute provider-accept: --max-calls must be between 1 and %d\n", providerAcceptMaxCalls)
		return 2
	}
	if !*confirmLive {
		fmt.Fprintln(stderr, "nexaroute provider-accept: refusing to run without --confirm-live.")
		fmt.Fprintln(stderr, "This command makes real upstream requests. Tested models may incur provider cost or consume rate limits.")

		return 2
	}
	if *test && len(providerIDs) == 0 {
		fmt.Fprintln(stderr, "nexaroute provider-accept: --provider is required when --test=true (refusing to probe every configured provider)")
		return 2
	}

	cfg, err := config.LoadBase(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute provider-accept: cannot load config: %v\n", err)
		return 2
	}
	selected, err := selectAcceptanceProviders(cfg.Providers, providerIDs)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute provider-accept: %v\n", err)
		return 2
	}
	if len(selected) == 0 {
		fmt.Fprintln(stderr, "nexaroute provider-accept: no matching providers selected")
		return 2
	}

	if strings.TrimSpace(*outPath) == "" {
		*outPath = "provider-acceptance-" + time.Now().UTC().Format("20060102T150405Z") + ".json"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report := providerAcceptReport{
		Schema:      providerAcceptSchema,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Mode:        acceptanceMode(*discover, *test),
		Bounds: providerAcceptBounds{
			DiscoveryTimeoutSeconds:  timeout.Seconds(),
			GenerationTimeoutSeconds: timeout.Seconds(),
			MaxOutputTokensPerCall:   1,
			MaxModelsPerProvider:     *maxModels,
			MaxUpstreamCalls:         *maxCalls,
		},
	}

	calls := 0
	for _, p := range selected {
		if ctx.Err() != nil {
			report.Warnings = append(report.Warnings, "canceled before all providers were processed")
			break
		}
		result := acceptProvider(ctx, p, acceptOptions{
			discover:  *discover,
			test:      *test,
			timeout:   *timeout,
			maxModels: *maxModels,
			maxCalls:  *maxCalls,
			modelIDs:  modelIDs,
			calls:     &calls,
		})
		report.Providers = append(report.Providers, result)
	}

	finalizeAcceptanceReport(&report)
	report.Bounds.UpstreamCalls = calls
	if err := writeProviderAcceptReport(*outPath, report); err != nil {
		fmt.Fprintf(stderr, "nexaroute provider-accept: cannot write report: %v\n", err)
		return 2
	}
	printProviderAcceptSummary(stdout, report, *outPath)
	if !report.OK {
		return 1
	}
	return 0
}

type acceptOptions struct {
	discover  bool
	test      bool
	timeout   time.Duration
	maxModels int
	maxCalls  int
	modelIDs  []string
	calls     *int
}

func acceptanceMode(discover, test bool) string {
	switch {
	case discover && test:
		return "discover+test"
	case discover:
		return "discover"
	default:
		return "test"
	}
}

func selectAcceptanceProviders(all []config.ProviderConfig, ids []string) ([]config.ProviderConfig, error) {
	if len(ids) == 0 {
		out := make([]config.ProviderConfig, 0, len(all))
		for _, p := range all {
			if p.Enabled {
				out = append(out, p)
			}
		}
		return out, nil
	}
	byID := map[string]config.ProviderConfig{}
	for _, p := range all {
		byID[p.ID] = p
	}
	out := make([]config.ProviderConfig, 0, len(ids))
	for _, id := range ids {
		p, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("provider %q not found in configuration", id)
		}
		out = append(out, p)
	}
	return out, nil
}

func acceptProvider(ctx context.Context, p config.ProviderConfig, opt acceptOptions) providerAcceptResult {
	p.ApplyDefaults()
	secrets := acceptanceSecrets(p)
	credentialConfigured := len(p.ResolvedCredentials()) > 0
	if p.Type == "bedrock" {
		credentialConfigured = credentialConfigured && os.Getenv(p.SecretKeyEnv) != ""
	}
	result := providerAcceptResult{
		Provider:             p.ID,
		Protocol:             p.Type,
		AuthMode:             p.AuthMode,
		BaseURL:              sanitizeAcceptanceURL(p.BaseURL),
		CredentialSource:     acceptanceCredentialSource(p),
		CredentialConfigured: credentialConfigured,
	}
	discoverySupported := providers.SupportsDiscovery(p)
	result.Discovery.Supported = discoverySupported
	if opt.discover {
		if !discoverySupported {
			result.Discovery.Error = "provider type does not expose a plain model inventory endpoint"
		} else if !consumeCall(opt.calls, opt.maxCalls) {
			result.Discovery.Attempted = true
			result.Discovery.Error = "upstream call budget exhausted"
			result.Warnings = append(result.Warnings, result.Discovery.Error)
		} else {
			result.Discovery.Attempted = true
			models, status, err := providers.DiscoverModelsWithTimeout(ctx, p, opt.timeout)
			result.Discovery.StatusCode = status
			if err != nil {
				result.Discovery.Error = sanitizeAcceptanceText(err.Error(), secrets)
				result.Warnings = append(result.Warnings, "model discovery failed; manual models can still be tested")
			} else {
				result.Discovery.OK = true
				result.Discovery.ModelCount = len(models)
				result.Discovery.Models = models
			}
		}
	}

	if opt.test {
		selected := selectAcceptanceModels(p, opt.modelIDs, opt.maxModels, &result)
		if len(selected) == 0 && len(result.Models) == 0 {
			result.Models = append(result.Models, providerModelResult{
				Result:          "failed",
				MaxOutputTokens: 0,
				Error:           "no enabled model configured; add a model manually before acceptance",
			})
		}
		if len(selected) > 0 {
			a, err := providers.NewAdapter(p, opt.timeout)
			if err != nil {
				msg := sanitizeAcceptanceText("adapter: "+err.Error(), secrets)
				for _, m := range selected {
					result.Models = append(result.Models, providerModelResult{
						Model: m.Model, Source: acceptanceModelSource(m),
						PreviousTestState: acceptanceTestState(m),
						Result:            "failed", MaxOutputTokens: 1, Error: msg,
					})
				}
			} else {
				defer providers.CloseIdleConnections(a)
				for _, m := range selected {
					entry := providerModelResult{
						Model: m.Model, Source: acceptanceModelSource(m),
						PreviousTestState: acceptanceTestState(m),
						Result:            "failed", MaxOutputTokens: 1,
					}
					if ctx.Err() != nil {
						entry.Error = "canceled"
						result.Models = append(result.Models, entry)
						continue
					}
					if !consumeCall(opt.calls, opt.maxCalls) {
						entry.Error = "upstream call budget exhausted"
						result.Warnings = append(result.Warnings, entry.Error)
						result.Models = append(result.Models, entry)
						continue
					}
					cctx, cancel := context.WithTimeout(ctx, opt.timeout)
					lat, status, err := a.Probe(cctx, m.Model, 1)
					cancel()
					entry.StatusCode = status
					entry.LatencyMS = lat.Milliseconds()
					if err != nil {
						entry.Error = sanitizeAcceptanceText(err.Error(), secrets)
					} else {
						entry.Result = "passed"
					}
					result.Models = append(result.Models, entry)
				}
			}
		}
	}

	finalizeAcceptanceProvider(&result, opt)
	return result
}

func selectAcceptanceModels(p config.ProviderConfig, requested []string, maxModels int, result *providerAcceptResult) []config.ModelConfig {
	enabled := make([]config.ModelConfig, 0, len(p.Models))
	for _, m := range p.Models {
		if m.Enabled {
			enabled = append(enabled, m)
		}
	}
	if len(requested) == 0 {
		if len(enabled) > maxModels {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%d enabled model(s) not tested because --max-models=%d", len(enabled)-maxModels, maxModels))
			enabled = enabled[:maxModels]
		}
		return enabled
	}
	byName := map[string]config.ModelConfig{}
	for _, m := range enabled {
		byName[m.Model] = m
		if m.ID != "" {
			byName[m.ID] = m
		}
	}
	out := make([]config.ModelConfig, 0, len(requested))
	seen := map[string]bool{}
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if m, ok := byName[name]; ok {
			if len(out) >= maxModels {
				result.Warnings = append(result.Warnings, fmt.Sprintf("model %q not tested because --max-models=%d", name, maxModels))
				continue
			}
			out = append(out, m)
			continue
		}
		result.Models = append(result.Models, providerModelResult{
			Model: name, Result: "failed", MaxOutputTokens: 0,
			Error: "model is not configured for this provider; add it manually before acceptance",
		})
	}
	return out
}

func consumeCall(calls *int, max int) bool {
	if calls == nil || *calls >= max {
		return false
	}
	*calls++
	return true
}

func finalizeAcceptanceProvider(result *providerAcceptResult, opt acceptOptions) {
	if opt.test {
		passed, failed := 0, 0
		for _, m := range result.Models {
			switch m.Result {
			case "passed":
				passed++
			case "failed":
				failed++
			}
		}
		discoveryProblem := opt.discover && result.Discovery.Supported && result.Discovery.Attempted && !result.Discovery.OK
		switch {
		case passed > 0 && failed == 0 && !discoveryProblem:
			result.Status = "passed"
		case passed > 0 && failed > 0:
			result.Status = "degraded"
		case passed > 0 && failed == 0 && discoveryProblem:
			result.Status = "degraded"
			result.Warnings = append(result.Warnings, "generation probes passed but discovery was not observed")
		default:
			result.Status = "failed"
			if discoveryProblem {
				result.Warnings = append(result.Warnings, "generation probes failed and discovery was not observed")
			}
		}
		return
	}
	switch {
	case !result.Discovery.Supported:
		result.Status = "unsupported"
	case result.Discovery.OK:
		result.Status = "configured"
	default:
		result.Status = "failed"
	}
}

func finalizeAcceptanceReport(report *providerAcceptReport) {
	for _, p := range report.Providers {
		report.Totals.Providers++
		switch p.Status {
		case "passed":
			report.Totals.Passed++
		case "degraded":
			report.Totals.Degraded++
		case "failed":
			report.Totals.Failed++
		case "configured":
			report.Totals.Configured++
		case "unsupported":
			report.Totals.Unsupported++
		}
		if p.Discovery.Supported {
			report.Totals.ModelsDiscoveredTotal += p.Discovery.ModelCount
			if p.Discovery.Attempted {
				report.Totals.DiscoveryAttempted++
				if p.Discovery.OK {
					report.Totals.DiscoverySucceeded++
				} else {
					report.Totals.DiscoveryFailed++
				}
			}
		} else {
			report.Totals.DiscoveryUnsupported++
		}
		for _, m := range p.Models {
			if m.Result == "passed" {
				report.Totals.ModelsTested++
				report.Totals.ModelsPassed++
			} else if m.Result == "failed" {
				report.Totals.ModelsTested++
				report.Totals.ModelsFailed++
			}
		}
	}
	report.OK = report.Totals.Failed == 0 && report.Totals.Degraded == 0
	report.Totals.InsufficientCredential = 0
	for _, p := range report.Providers {
		if p.AuthMode != "none" && !p.CredentialConfigured {
			report.Totals.InsufficientCredential++
		}
	}
}

func acceptanceCredentialSource(p config.ProviderConfig) string {
	if p.Type == "bedrock" {
		if os.Getenv(p.SecretKeyEnv) != "" {
			return "aws-env"
		}
	}
	if len(p.Credentials) > 0 {
		return "pool"
	}
	if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "" {
		return "env"
	}
	if p.APIKey != "" {
		return "literal"
	}
	if p.AuthMode == "none" {
		return "none"
	}
	return "none"
}

func acceptanceModelSource(m config.ModelConfig) string {
	if m.Source == config.ModelSourceDiscovered {
		return config.ModelSourceDiscovered
	}
	return config.ModelSourceManual
}

func acceptanceTestState(m config.ModelConfig) string {
	if config.ValidModelTestState(m.TestState) && m.TestState != "" {
		return m.TestState
	}
	return config.ModelTestUntested
}

func acceptanceSecrets(p config.ProviderConfig) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range p.ResolvedCredentials() {
		add(v)
	}
	for _, v := range p.Headers {
		add(v)
	}
	add(os.Getenv(p.SecretKeyEnv))
	add(os.Getenv(p.SessionTokenEnv))
	if u, err := url.Parse(p.ProxyURL); err == nil && u.User != nil {
		if pass, ok := u.User.Password(); ok {
			add(pass)
		}
		add(u.User.Username())
	}
	return out
}

func sanitizeAcceptanceText(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if runes := []rune(text); len(runes) > providerAcceptMaxErrorRune {
		text = string(runes[:providerAcceptMaxErrorRune]) + "…"
	}
	return text
}

func sanitizeAcceptanceURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

func writeProviderAcceptReport(path string, report providerAcceptReport) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func printProviderAcceptSummary(w io.Writer, report providerAcceptReport, outPath string) {
	fmt.Fprintf(w, "provider acceptance (%s): %d provider(s), ok=%t\n", report.Mode, report.Totals.Providers, report.OK)
	fmt.Fprintf(w, "bounds: %d upstream call(s), 1 output token per generation call, %.0fs per call, max %d model(s)/provider\n",
		report.Bounds.UpstreamCalls, report.Bounds.GenerationTimeoutSeconds, report.Bounds.MaxModelsPerProvider)
	for _, p := range report.Providers {
		fmt.Fprintf(w, "%s (%s, auth=%s, credential=%s): %s\n", p.Provider, p.Protocol, p.AuthMode, p.CredentialSource, p.Status)
		if p.Discovery.Supported {
			switch {
			case !p.Discovery.Attempted:
				fmt.Fprintf(w, "  discovery: not attempted\n")
			case p.Discovery.OK:
				fmt.Fprintf(w, "  discovery: ok status=%d models=%d\n", p.Discovery.StatusCode, p.Discovery.ModelCount)
			default:
				fmt.Fprintf(w, "  discovery: failed status=%d error=%s\n", p.Discovery.StatusCode, p.Discovery.Error)
			}
		} else {
			fmt.Fprintf(w, "  discovery: not supported by this provider type\n")
		}
		for _, m := range p.Models {
			line := fmt.Sprintf("  model %s [%s, %s] -> %s", m.Model, m.Source, m.PreviousTestState, m.Result)
			if m.StatusCode != 0 {
				line += fmt.Sprintf(" HTTP %d", m.StatusCode)
			}
			if m.LatencyMS > 0 {
				line += fmt.Sprintf(" %dms", m.LatencyMS)
			}
			if m.Error != "" {
				line += " error=" + m.Error
			}
			fmt.Fprintln(w, line)
		}
		for _, warning := range p.Warnings {
			fmt.Fprintf(w, "  warning: %s\n", warning)
		}
	}
	fmt.Fprintf(w, "report: %s\n", outPath)
}

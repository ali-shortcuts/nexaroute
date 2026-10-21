package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
	"github.com/coreos/go-oidc/v3/oidc"
)

// external-accept is the opt-in acceptance harness for real provider and OIDC
// deployments. It deliberately has no built-in endpoints: every target comes
// from operator configuration or an explicitly named environment variable.
// Without --confirm-live it exits before loading any target or making a call.
//
// The report status vocabulary is shared by every check:
//
//	PASS     the selected live check ran and was observed to succeed
//	FAIL     the selected live check ran and failed
//	BLOCKED  the check could not run because required credentials/service input was absent
//	NOT_RUN  the check was not selected, or the interactive step cannot be automated
//
// Fixture tests for this command use loopback HTTP servers. They prove the
// harness behavior, not interoperability with a real external service.
const (
	externalAcceptSchema     = "nexaroute.external-acceptance.v1"
	externalAcceptMaxTimeout = 60 * time.Second
	externalAcceptMaxOverall = 5 * time.Minute
	externalAcceptMaxModels  = 100
	externalAcceptMaxCalls   = 100
	externalAcceptMaxBody    = 1 << 20
)

const (
	externalStatusPass    = "PASS"
	externalStatusFail    = "FAIL"
	externalStatusBlocked = "BLOCKED"
	externalStatusNotRun  = "NOT_RUN"
)

type externalAcceptReport struct {
	Schema        string                  `json:"schema"`
	Gateway       string                  `json:"gateway_version,omitempty"`
	GeneratedAt   string                  `json:"generated_at"`
	Mode          string                  `json:"mode"`
	EvidenceClass string                  `json:"evidence_class"`
	Outcome       string                  `json:"outcome"`
	Bounds        externalAcceptBounds    `json:"bounds"`
	Selection     externalAcceptSelection `json:"selection"`
	Checks        []externalAcceptCheck   `json:"checks"`
	Totals        externalAcceptTotals    `json:"totals"`
	Limitations   []string                `json:"limitations,omitempty"`
	Warnings      []string                `json:"warnings,omitempty"`
}

type externalAcceptBounds struct {
	CallTimeoutSeconds    float64 `json:"call_timeout_seconds"`
	OverallTimeoutSeconds float64 `json:"overall_timeout_seconds"`
	MaxModelsPerProvider  int     `json:"max_models_per_provider"`
	MaxUpstreamCalls      int     `json:"max_upstream_calls"`
	UpstreamCalls         int     `json:"upstream_calls"`
}

type externalAcceptSelection struct {
	Providers      []string `json:"providers"`
	Models         []string `json:"models,omitempty"`
	AllProviders   bool     `json:"all_providers"`
	OIDC           bool     `json:"oidc"`
	OIDCIssuerEnv  string   `json:"oidc_issuer_env,omitempty"`
	OIDCIDTokenEnv string   `json:"oidc_id_token_env,omitempty"`
}

type externalAcceptCheck struct {
	ID          string                `json:"id"`
	Kind        string                `json:"kind"`
	Selected    bool                  `json:"selected"`
	Status      string                `json:"status"`
	TargetClass string                `json:"target_class,omitempty"`
	Reason      string                `json:"reason,omitempty"`
	DurationMS  int64                 `json:"duration_ms,omitempty"`
	Provider    *providerAcceptResult `json:"provider,omitempty"`
	OIDC        *oidcAcceptResult     `json:"oidc,omitempty"`
}

type externalAcceptTotals struct {
	Checks          int `json:"checks"`
	Passed          int `json:"passed"`
	Failed          int `json:"failed"`
	Blocked         int `json:"blocked"`
	NotRun          int `json:"not_run"`
	ProvidersTested int `json:"providers_tested"`
	OIDCChecks      int `json:"oidc_checks"`
}

type oidcAcceptResult struct {
	Issuer             string    `json:"issuer,omitempty"`
	ClientIDConfigured bool      `json:"client_id_configured"`
	AudienceConfigured bool      `json:"audience_configured"`
	Discovery          oidcProbe `json:"discovery"`
	JWKS               oidcProbe `json:"jwks"`
	IDToken            oidcProbe `json:"id_token"`
	AuthorizationCode  oidcProbe `json:"authorization_code_flow"`
	RoleMappingChecked bool      `json:"role_mapping_checked"`
}

type oidcProbe struct {
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	KeyCount  int    `json:"key_count,omitempty"`
	RoleCount int    `json:"role_count,omitempty"`
}

func runExternalAccept(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("external-accept", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON config used for provider and OIDC targets")
	outPath := fs.String("out", "", "redacted JSON report path")
	timeout := fs.Duration("timeout", 15*time.Second, "per-request timeout (max 60s)")
	overallTimeout := fs.Duration("overall-timeout", 2*time.Minute, "overall run timeout (max 5m)")
	maxModels := fs.Int("max-models", 10, "maximum models tested per provider (1-100)")
	maxCalls := fs.Int("max-calls", 20, "maximum total provider upstream calls (1-100)")
	confirmLive := fs.Bool("confirm-live", false, "required acknowledgment that selected checks make real network requests")
	allProviders := fs.Bool("all-providers", false, "test every enabled configured provider")
	oidcMode := fs.Bool("oidc", false, "test the configured admin.oidc issuer")
	oidcIssuerEnv := fs.String("oidc-issuer-env", "", "environment variable containing an OIDC issuer URL")
	oidcClientID := fs.String("oidc-client-id", "", "OIDC client id for --oidc-issuer-env when config has none")
	oidcAudience := fs.String("oidc-audience", "", "expected OIDC audience for --oidc-issuer-env when config has none")
	oidcIDTokenEnv := fs.String("oidc-id-token-env", "", "optional environment variable containing a test ID token to verify")
	var providerIDs, modelIDs stringListFlag
	fs.Var(&providerIDs, "provider", "provider id to test (repeatable or comma-separated); use --all-providers for all")
	fs.Var(&modelIDs, "model", "model id to test (repeatable or comma-separated); defaults to enabled configured models")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *timeout <= 0 || *timeout > externalAcceptMaxTimeout {
		fmt.Fprintf(stderr, "nexaroute external-accept: --timeout must be between 1ns and %s\n", externalAcceptMaxTimeout)
		return 2
	}
	if *overallTimeout < *timeout || *overallTimeout > externalAcceptMaxOverall {
		fmt.Fprintf(stderr, "nexaroute external-accept: --overall-timeout must be between --timeout and %s\n", externalAcceptMaxOverall)
		return 2
	}
	if *maxModels < 1 || *maxModels > externalAcceptMaxModels {
		fmt.Fprintf(stderr, "nexaroute external-accept: --max-models must be between 1 and %d\n", externalAcceptMaxModels)
		return 2
	}
	if *maxCalls < 1 || *maxCalls > externalAcceptMaxCalls {
		fmt.Fprintf(stderr, "nexaroute external-accept: --max-calls must be between 1 and %d\n", externalAcceptMaxCalls)
		return 2
	}
	if len(providerIDs) > 0 && *allProviders {
		fmt.Fprintln(stderr, "nexaroute external-accept: --provider and --all-providers are mutually exclusive")
		return 2
	}
	oidcSelected := *oidcMode || strings.TrimSpace(*oidcIssuerEnv) != ""
	if len(providerIDs) == 0 && !*allProviders && !oidcSelected {
		fmt.Fprintln(stderr, "nexaroute external-accept: select at least one of --provider, --all-providers, or --oidc/--oidc-issuer-env")
		return 2
	}
	if !*confirmLive {
		fmt.Fprintln(stderr, "nexaroute external-accept: refusing to run without --confirm-live.")
		fmt.Fprintln(stderr, "This command makes real network requests. Provider probes may incur cost or consume rate limits.")
		return 2
	}

	var cfg config.Config
	haveConfig := false
	needConfig := *allProviders || len(providerIDs) > 0 || *oidcMode
	if needConfig {
		loaded, err := config.LoadBase(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "nexaroute external-accept: cannot load config: %v\n", err)
			return 2
		}
		cfg = loaded
		haveConfig = true
	}

	var providers []config.ProviderConfig
	if haveConfig && (*allProviders || len(providerIDs) > 0) {
		ids := []string(providerIDs)
		if *allProviders {
			ids = nil
		}
		selected, err := selectAcceptanceProviders(cfg.Providers, ids)
		if err != nil {
			fmt.Fprintf(stderr, "nexaroute external-accept: %v\n", err)
			return 2
		}
		if len(selected) == 0 {
			fmt.Fprintln(stderr, "nexaroute external-accept: no matching enabled providers selected")
			return 2
		}
		providers = selected
	}

	oidcCfg, oidcIssuer, oidcBlocked := resolveAcceptanceOIDC(cfg, haveConfig,
		*oidcMode, strings.TrimSpace(*oidcIssuerEnv), strings.TrimSpace(*oidcClientID), strings.TrimSpace(*oidcAudience))
	oidcTokenEnv := strings.TrimSpace(*oidcIDTokenEnv)

	secrets := acceptanceRedactionValues(providers, oidcCfg, oidcTokenEnv)
	redactor := livecanary.NewRedactor(secrets)

	if strings.TrimSpace(*outPath) == "" {
		*outPath = "external-acceptance-" + time.Now().UTC().Format("20060102T150405Z") + ".json"
	}

	runCtx, cancel := context.WithTimeout(context.Background(), *overallTimeout)
	defer cancel()

	report := externalAcceptReport{
		Schema:      externalAcceptSchema,
		Gateway:     version,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Mode:        "live",
		Bounds: externalAcceptBounds{
			CallTimeoutSeconds:    timeout.Seconds(),
			OverallTimeoutSeconds: overallTimeout.Seconds(),
			MaxModelsPerProvider:  *maxModels,
			MaxUpstreamCalls:      *maxCalls,
		},
		Selection: externalAcceptSelection{
			Providers:      providerIDs,
			Models:         modelIDs,
			AllProviders:   *allProviders,
			OIDC:           oidcSelected,
			OIDCIssuerEnv:  strings.TrimSpace(*oidcIssuerEnv),
			OIDCIDTokenEnv: oidcTokenEnv,
		},
		Limitations: []string{
			"Provider PASS means a configured endpoint answered bounded discovery/generation probes; it is not full protocol conformance.",
			"OIDC discovery/JWKS PASS does not exercise an interactive Authorization Code + PKCE browser login.",
			"ID token verification does not check a nonce because this harness does not initiate the browser authorization request.",
		},
	}

	calls := 0
	for _, p := range providers {
		start := time.Now()
		check := runExternalProviderCheck(runCtx, p, acceptOptions{
			discover:  true,
			test:      true,
			timeout:   *timeout,
			maxModels: *maxModels,
			maxCalls:  *maxCalls,
			modelIDs:  modelIDs,
			calls:     &calls,
		}, redactor)
		check.DurationMS = time.Since(start).Milliseconds()
		report.Checks = append(report.Checks, check)
	}

	if oidcSelected {
		start := time.Now()
		check := runExternalOIDCCheck(runCtx, oidcIssuer, oidcCfg, oidcBlocked, oidcTokenEnv, *timeout, redactor)
		check.DurationMS = time.Since(start).Milliseconds()
		report.Checks = append(report.Checks, check)
	} else {
		report.Checks = append(report.Checks, externalAcceptCheck{
			ID: "oidc", Kind: "oidc", Selected: false, Status: externalStatusNotRun,
			Reason: "OIDC issuer check was not selected (use --oidc or --oidc-issuer-env)",
		})
	}

	report.Bounds.UpstreamCalls = calls
	finalizeExternalAcceptReport(&report)
	if err := writeExternalAcceptReport(*outPath, report); err != nil {
		fmt.Fprintf(stderr, "nexaroute external-accept: cannot write report: %v\n", err)
		return 2
	}
	printExternalAcceptSummary(stdout, report, *outPath)
	return externalAcceptExitCode(report.Outcome)
}

func resolveAcceptanceOIDC(cfg config.Config, haveConfig, useConfig bool, issuerEnv, clientID, audience string) (config.OIDCConfig, string, string) {
	oidcCfg := config.OIDCConfig{}
	if haveConfig {
		oidcCfg = cfg.Admin.OIDC
	}
	if useConfig {
		if !oidcCfg.Enabled {
			return oidcCfg, "", "admin.oidc is not enabled in the selected configuration"
		}
		if clientID != "" {
			oidcCfg.ClientID = clientID
		}
		if audience != "" {
			oidcCfg.Audience = audience
		}
		return oidcCfg, strings.TrimSpace(oidcCfg.IssuerURL), ""
	}
	if issuerEnv == "" {
		return oidcCfg, "", "OIDC issuer environment variable name is empty"
	}
	issuer := strings.TrimSpace(os.Getenv(issuerEnv))
	if issuer == "" {
		return oidcCfg, "", fmt.Sprintf("OIDC issuer environment variable %s is not set", issuerEnv)
	}
	if clientID != "" {
		oidcCfg.ClientID = clientID
	}
	if audience != "" {
		oidcCfg.Audience = audience
	}
	if strings.TrimSpace(oidcCfg.Audience) == "" {
		oidcCfg.Audience = oidcCfg.ClientID
	}
	return oidcCfg, issuer, ""
}

func acceptanceRedactionValues(providers []config.ProviderConfig, oidcCfg config.OIDCConfig, oidcIDTokenEnv string) []string {
	out := make([]string, 0, len(providers)*4+2)
	for _, p := range providers {
		out = append(out, acceptanceSecrets(p)...)
	}
	if oidcCfg.ClientSecretEnv != "" {
		out = append(out, os.Getenv(oidcCfg.ClientSecretEnv))
	}
	if oidcIDTokenEnv != "" {
		out = append(out, os.Getenv(oidcIDTokenEnv))
	}
	return out
}

func runExternalProviderCheck(ctx context.Context, p config.ProviderConfig, opt acceptOptions, redactor *livecanary.Redactor) externalAcceptCheck {
	p.ApplyDefaults()
	check := externalAcceptCheck{ID: "provider:" + p.ID, Kind: "provider", Selected: true,
		TargetClass: acceptanceEvidenceClass(p.BaseURL)}
	if ready, envName := externalProviderCredentialReady(p); !ready {
		check.Status = externalStatusBlocked
		check.Reason = fmt.Sprintf("credential environment variable %q is not set; no provider request was attempted", envName)
		return check
	}
	if len(opt.modelIDs) == 0 && !hasEnabledModel(p) {
		check.Status = externalStatusBlocked
		check.Reason = "provider has no enabled model; configure a test model before running live acceptance"
		return check
	}
	result := acceptProvider(ctx, p, opt)
	check.Provider = &result
	switch result.Status {
	case "passed":
		check.Status = externalStatusPass
	case "failed", "degraded":
		check.Status = externalStatusFail
		check.Reason = "provider acceptance probe did not pass every selected check"
	default:
		check.Status = externalStatusFail
		check.Reason = "provider acceptance returned an unexpected status " + result.Status
	}
	if ctx.Err() != nil {
		check.Status = externalStatusFail
		check.Reason = "overall acceptance timeout or cancellation: " + ctx.Err().Error()
	}
	sanitizeExternalCheck(&check, redactor)
	return check
}

func externalProviderCredentialReady(p config.ProviderConfig) (bool, string) {
	if strings.EqualFold(strings.TrimSpace(p.AuthMode), "none") {
		return true, ""
	}
	if p.Type == "bedrock" {
		if len(p.ResolvedCredentials()) == 0 {
			return false, envNameOr(p.APIKeyEnv, "api_key_env")
		}
		if strings.TrimSpace(os.Getenv(p.SecretKeyEnv)) == "" {
			return false, envNameOr(p.SecretKeyEnv, "secret_key_env")
		}
		return true, ""
	}
	if len(p.ResolvedCredentials()) > 0 {
		return true, ""
	}
	return false, envNameOr(p.APIKeyEnv, "api_key_env")
}

func envNameOr(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func hasEnabledModel(p config.ProviderConfig) bool {
	for _, m := range p.Models {
		if m.Enabled {
			return true
		}
	}
	return false
}

func runExternalOIDCCheck(ctx context.Context, issuer string, cfg config.OIDCConfig, blockedReason, idTokenEnv string, timeout time.Duration, redactor *livecanary.Redactor) externalAcceptCheck {
	check := externalAcceptCheck{ID: "oidc", Kind: "oidc", Selected: true,
		TargetClass: acceptanceEvidenceClass(issuer)}
	result := &oidcAcceptResult{
		ClientIDConfigured: strings.TrimSpace(cfg.ClientID) != "",
		AudienceConfigured: strings.TrimSpace(cfg.Audience) != "",
		IDToken: oidcProbe{Status: externalStatusNotRun,
			Detail: "no --oidc-id-token-env supplied; live token issuance was not exercised"},
		AuthorizationCode: oidcProbe{Status: externalStatusNotRun,
			Detail: "interactive Authorization Code + PKCE login is not automated; use docs/EXTERNAL_VERIFICATION.md"},
	}
	check.OIDC = result
	if blockedReason != "" {
		check.Status = externalStatusBlocked
		check.Reason = blockedReason
		result.Discovery = oidcProbe{Status: externalStatusBlocked, Detail: blockedReason}
		result.JWKS = oidcProbe{Status: externalStatusBlocked, Detail: "discovery did not run"}
		sanitizeExternalCheck(&check, redactor)
		return check
	}
	result.Issuer = sanitizeAcceptanceURL(issuer)
	if result.Issuer == "" {
		check.Status = externalStatusBlocked
		check.Reason = "OIDC issuer is not an absolute URL"
		result.Discovery = oidcProbe{Status: externalStatusBlocked, Detail: check.Reason}
		result.JWKS = oidcProbe{Status: externalStatusBlocked, Detail: "discovery did not run"}
		return check
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	provider, err := oidc.NewProvider(oidc.ClientContext(discoveryCtx, acceptanceHTTPClient(timeout)), issuer)
	result.Discovery.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Discovery.Status = externalStatusFail
		result.Discovery.Detail = redactor.Redact("OIDC provider discovery failed: " + err.Error())
		result.JWKS = oidcProbe{Status: externalStatusNotRun, Detail: "OIDC discovery did not produce a JWKS endpoint"}
		check.Status = externalStatusFail
		check.Reason = "OIDC discovery failed"
		sanitizeExternalCheck(&check, redactor)
		return check
	}
	result.Discovery.Endpoint = sanitizeAcceptanceURL(issuer)
	var meta struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if err := provider.Claims(&meta); err != nil {
		result.Discovery.Status = externalStatusFail
		result.Discovery.Detail = "OIDC discovery metadata is not readable"
		result.JWKS = oidcProbe{Status: externalStatusNotRun, Detail: "OIDC discovery metadata is not readable"}
		check.Status = externalStatusFail
		check.Reason = "OIDC discovery metadata is invalid"
		return check
	}
	if meta.Issuer != issuer || !validAcceptanceOIDCEndpoint(provider.Endpoint().AuthURL, issuer) ||
		!validAcceptanceOIDCEndpoint(provider.Endpoint().TokenURL, issuer) || !validAcceptanceOIDCEndpoint(meta.JWKS, issuer) {
		result.Discovery.Status = externalStatusFail
		result.Discovery.Detail = "discovery issuer, authorization, token, or JWKS endpoint is invalid"
		result.JWKS = oidcProbe{Status: externalStatusNotRun, Detail: "OIDC discovery metadata is invalid"}
		check.Status = externalStatusFail
		check.Reason = "OIDC discovery metadata failed endpoint validation"
		sanitizeExternalCheck(&check, redactor)
		return check
	}
	result.Discovery.Status = externalStatusPass
	result.Discovery.Detail = "issuer and endpoint metadata matched the configured issuer"

	start = time.Now()
	keyCount, jwksErr := fetchOIDCJWKS(ctx, meta.JWKS, timeout)
	result.JWKS.Endpoint = sanitizeAcceptanceURL(meta.JWKS)
	result.JWKS.LatencyMS = time.Since(start).Milliseconds()
	if jwksErr != nil {
		result.JWKS.Status = externalStatusFail
		result.JWKS.Detail = redactor.Redact(jwksErr.Error())
		check.Status = externalStatusFail
		check.Reason = "OIDC JWKS validation failed"
		sanitizeExternalCheck(&check, redactor)
		return check
	}
	result.JWKS.Status = externalStatusPass
	result.JWKS.KeyCount = keyCount
	result.JWKS.Detail = "JWKS document was fetched and contained at least one signing key"

	if idTokenEnv != "" {
		rawToken := strings.TrimSpace(os.Getenv(idTokenEnv))
		if rawToken == "" {
			result.IDToken = oidcProbe{Status: externalStatusBlocked,
				Detail: fmt.Sprintf("ID token environment variable %s is not set", idTokenEnv)}
			check.Status = externalStatusBlocked
			check.Reason = result.IDToken.Detail
			sanitizeExternalCheck(&check, redactor)
			return check
		}
		roleCount, verifyErr := verifyAcceptanceIDToken(ctx, rawToken, issuer, meta.JWKS, cfg, timeout)
		if verifyErr != nil {
			result.IDToken.Status = externalStatusFail
			result.IDToken.Detail = redactor.Redact(verifyErr.Error())
			check.Status = externalStatusFail
			check.Reason = "OIDC ID token verification failed"
		} else {
			result.IDToken.Status = externalStatusPass
			result.IDToken.Detail = "ID token signature, issuer, audience, expiry, and subject were accepted by the live JWKS; nonce was not checked because this harness did not initiate the login"
			if roleCount >= 0 {
				result.IDToken.RoleCount = roleCount
				result.RoleMappingChecked = true
			}
			check.Status = externalStatusPass
		}
	}
	if check.Status == "" {
		check.Status = externalStatusPass
	}
	sanitizeExternalCheck(&check, redactor)
	return check
}

func acceptanceHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

func fetchOIDCJWKS(ctx context.Context, jwksURL string, timeout time.Duration) (int, error) {
	if !validAcceptanceOIDCEndpoint(jwksURL, jwksURL) {
		return 0, errors.New("JWKS endpoint is not an absolute HTTPS URL (HTTP loopback is allowed only for loopback issuers)")
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return 0, errors.New("JWKS request could not be created")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := acceptanceHTTPClient(timeout).Do(req)
	if err != nil {
		return 0, errors.New("JWKS request failed: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("JWKS endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, externalAcceptMaxBody+1))
	if err != nil {
		return 0, errors.New("JWKS response could not be read")
	}
	if len(body) > externalAcceptMaxBody {
		return 0, fmt.Errorf("JWKS response exceeds the %d-byte bound", externalAcceptMaxBody)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, errors.New("JWKS response is not valid JSON")
	}
	count := 0
	for _, key := range doc.Keys {
		if key.Kty != "" && (key.Kty != "RSA" || (key.N != "" && key.E != "")) {
			count++
		}
	}
	if count == 0 {
		return 0, errors.New("JWKS response contained no usable signing key")
	}
	return count, nil
}

func verifyAcceptanceIDToken(ctx context.Context, rawToken, issuer, jwksURL string, cfg config.OIDCConfig, timeout time.Duration) (int, error) {
	expectedAudience := strings.TrimSpace(cfg.Audience)
	if expectedAudience == "" {
		expectedAudience = strings.TrimSpace(cfg.ClientID)
	}
	if expectedAudience == "" {
		return -1, errors.New("an expected OIDC audience or client id is required to verify an ID token")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	verifier := oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(oidc.ClientContext(fetchCtx, acceptanceHTTPClient(timeout)), jwksURL),
		&oidc.Config{ClientID: expectedAudience})
	token, err := verifier.Verify(fetchCtx, rawToken)
	if err != nil {
		return -1, errors.New("OIDC ID token verification failed")
	}
	if token.Issuer != issuer || strings.TrimSpace(token.Subject) == "" {
		return -1, errors.New("OIDC ID token issuer or subject is invalid")
	}
	if !containsAudienceString(token.Audience, expectedAudience) {
		return -1, errors.New("OIDC ID token audience is invalid")
	}
	roleCount := -1
	if strings.TrimSpace(cfg.RoleClaim) != "" && len(cfg.RoleMappings) > 0 {
		var claims map[string]json.RawMessage
		if err := token.Claims(&claims); err != nil {
			return -1, errors.New("OIDC ID token claims are malformed")
		}
		values, err := oidcClaimStrings(claims[cfg.RoleClaim])
		if err != nil {
			return -1, errors.New("OIDC role claim has an invalid type")
		}
		roles := map[string]bool{}
		for _, value := range values {
			if role, ok := cfg.RoleMappings[value]; ok {
				roles[role] = true
			}
		}
		if len(roles) == 0 {
			return -1, errors.New("OIDC identity has no allowlisted role")
		}
		if len(roles) != 1 {
			return -1, errors.New("OIDC identity has conflicting allowlisted roles")
		}
		roleCount = len(roles)
	}
	return roleCount, nil
}

func oidcClaimStrings(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	return nil, errors.New("role claim must be a string or an array of strings")
}

func containsAudienceString(audiences []string, expected string) bool {
	for _, audience := range audiences {
		if audience == expected {
			return true
		}
	}
	return false
}

func validAcceptanceOIDCEndpoint(raw, issuer string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && loopbackHTTPURL(issuer) && loopbackHTTPURL(raw)
}

func loopbackHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.")
}

func sanitizeExternalCheck(check *externalAcceptCheck, redactor *livecanary.Redactor) {
	if check.Reason != "" {
		check.Reason = redactor.Redact(check.Reason)
	}
	if check.Provider != nil {
		sanitizeProviderAcceptResult(check.Provider, redactor)
	}
	if check.OIDC != nil {
		sanitizeOIDCProbe(&check.OIDC.Discovery, redactor)
		sanitizeOIDCProbe(&check.OIDC.JWKS, redactor)
		sanitizeOIDCProbe(&check.OIDC.IDToken, redactor)
		sanitizeOIDCProbe(&check.OIDC.AuthorizationCode, redactor)
	}
}

func sanitizeOIDCProbe(probe *oidcProbe, redactor *livecanary.Redactor) {
	probe.Detail = redactor.Redact(probe.Detail)
	probe.Endpoint = redactor.Redact(probe.Endpoint)
}

func sanitizeProviderAcceptResult(result *providerAcceptResult, redactor *livecanary.Redactor) {
	if result.Error != "" {
		result.Error = redactor.Redact(result.Error)
	}
	result.Discovery.Error = redactor.Redact(result.Discovery.Error)
	for i := range result.Models {
		result.Models[i].Error = redactor.Redact(result.Models[i].Error)
	}
	for i := range result.Warnings {
		result.Warnings[i] = redactor.Redact(result.Warnings[i])
	}
}

func acceptanceEvidenceClass(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	if loopbackHTTPURL(raw) {
		return "loopback"
	}
	return "external"
}

func finalizeExternalAcceptReport(report *externalAcceptReport) {
	selectedStatuses := make([]string, 0, len(report.Checks))
	externalTargets, loopbackTargets := 0, 0
	for _, check := range report.Checks {
		report.Totals.Checks++
		switch check.Status {
		case externalStatusPass:
			report.Totals.Passed++
		case externalStatusFail:
			report.Totals.Failed++
		case externalStatusBlocked:
			report.Totals.Blocked++
		case externalStatusNotRun:
			report.Totals.NotRun++
		}
		if check.Selected {
			selectedStatuses = append(selectedStatuses, check.Status)
			switch check.TargetClass {
			case "external":
				externalTargets++
			case "loopback":
				loopbackTargets++
			}
		}
		switch check.Kind {
		case "provider":
			report.Totals.ProvidersTested++
		case "oidc":
			report.Totals.OIDCChecks++
		}
	}
	report.Outcome = externalOutcome(selectedStatuses)
	switch {
	case externalTargets > 0 && loopbackTargets > 0:
		report.EvidenceClass = "mixed"
	case externalTargets > 0:
		report.EvidenceClass = "external"
	case loopbackTargets > 0:
		report.EvidenceClass = "loopback"
	default:
		report.EvidenceClass = "none"
	}
	if report.EvidenceClass == "loopback" || report.EvidenceClass == "mixed" {
		report.Limitations = append(report.Limitations, "One or more selected targets are loopback endpoints; those PASS results are harness evidence, not external interoperability evidence.")
	}
	if report.Outcome == externalStatusBlocked {
		report.Limitations = append(report.Limitations, "One or more selected checks were BLOCKED by missing operator-supplied credentials, service configuration, or environment input.")
	}
	if report.Outcome == externalStatusNotRun {
		report.Limitations = append(report.Limitations, "No selected check produced a live observation; this report is not acceptance evidence.")
	}
}

func externalOutcome(statuses []string) string {
	if len(statuses) == 0 {
		return externalStatusNotRun
	}
	pass, blocked := 0, 0
	for _, status := range statuses {
		switch status {
		case externalStatusFail:
			return externalStatusFail
		case externalStatusBlocked:
			blocked++
		case externalStatusPass:
			pass++
		}
	}
	if blocked > 0 {
		return externalStatusBlocked
	}
	if pass == 0 {
		return externalStatusNotRun
	}
	return externalStatusPass
}

func writeExternalAcceptReport(path string, report externalAcceptReport) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".external-accept-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func printExternalAcceptSummary(w io.Writer, report externalAcceptReport, outPath string) {
	fmt.Fprintf(w, "external acceptance: outcome=%s checks=%d\n", report.Outcome, report.Totals.Checks)
	for _, check := range report.Checks {
		fmt.Fprintf(w, "  %-7s %s", check.Status, check.ID)
		if check.Reason != "" {
			fmt.Fprintf(w, " — %s", check.Reason)
		}
		if check.OIDC != nil {
			if check.OIDC.Discovery.Status != "" {
				fmt.Fprintf(w, " [discovery=%s jwks=%s id_token=%s auth_code=%s]",
					check.OIDC.Discovery.Status, check.OIDC.JWKS.Status, check.OIDC.IDToken.Status, check.OIDC.AuthorizationCode.Status)
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "report: %s\n", outPath)
}

func externalAcceptExitCode(outcome string) int {
	switch outcome {
	case externalStatusPass:
		return 0
	case externalStatusFail:
		return 1
	case externalStatusBlocked:
		return 3
	case externalStatusNotRun:
		return 4
	default:
		return 2
	}
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/providers/livecanary"
)

// runConformance implements `nexaroute conformance`. The only subcommand is
// the opt-in live provider canary. This command is never wired into gateway
// startup or public CI: it spends real provider quota and requires the
// operator to acknowledge that cost explicitly.
func runConformance(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, conformanceUsage)
		return 2
	}
	switch args[0] {
	case "canary":
		return runConformanceCanary(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, conformanceUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "nexaroute: unknown conformance subcommand %q\n", args[0])
		fmt.Fprint(stderr, conformanceUsage)
		return 2
	}
}

const conformanceUsage = `usage:
  nexaroute conformance canary --acknowledge-cost (--preset ID | --provider ID) --model ID [options]

Runs an opt-in live provider conformance canary. It sends real requests,
spends real quota, and writes report evidence. It never edits the conformance
manifest and never promotes a dimension to LIVE-VERIFIED.

Credential material is read from environment variables named in the config
or supplied with --api-key-env / --secret-key-env. Secret values are never
printed or written to the report.

Options:
  --config PATH              config file for --provider mode
  --preset ID                preset provider (see docs/COMPATIBILITY.md)
  --provider ID              provider id from the config
  --model ID                 model/deployment id (required in preset mode)
  --model-name NAME          upstream model name (default: --model)
  --base-url URL             override the provider base URL
  --auth-mode MODE           override auth mode
  --api-key-env ENV          env var holding the API key/access key id
  --secret-key-env ENV       env var holding the AWS secret access key
  --session-token-env ENV    env var holding the AWS session token
  --aws-region REGION        Bedrock region (default: us-east-1)
  --scenarios LIST           core | acceptance | extended | all | comma-separated ids (default core)
                             acceptance = core + stream_usage + auth_failure (issue #646 set)
  --max-calls N              hard call ceiling (default 5)
  --max-output-tokens N      per-request output ceiling (default 512)
  --max-input-tokens N       input-token assumption for the cost estimate (default 4096)
  --max-cost-usd N           preflight cost estimate ceiling (default 1.00)
  --input-cost-per-mtok N    input price for preset mode
  --output-cost-per-mtok N   output price for preset mode
  --call-timeout DURATION    per-call timeout (default 30s)
  --overall-timeout DURATION overall timeout (default 2m)
  --out PATH                 write JSON evidence (e.g. .artifacts/conformance/live.json)
  --acknowledge-cost         required: confirm the run may spend provider quota

Exit codes:
  0 PASS or NO-EVIDENCE, 3 PARTIAL, 1 FAIL, 2 usage/setup error, 130 CANCELED.
`

type canaryFlags struct {
	configPath   string
	preset       string
	provider     string
	model        string
	modelName    string
	baseURL      string
	authMode     string
	apiKeyEnv    string
	secretKeyEnv string
	sessionEnv   string
	awsRegion    string
	scenarios    string
	maxCalls     int
	maxOutput    int
	maxInput     int
	maxCost      float64
	inputCost    float64
	outputCost   float64
	callTimeout  time.Duration
	totalTimeout time.Duration
	out          string
	acknowledge  bool
}

func runConformanceCanary(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("conformance canary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f canaryFlags
	fs.StringVar(&f.configPath, "config", defaultConfigPath(), "path to JSON config for --provider mode")
	fs.StringVar(&f.preset, "preset", "", "preset provider id")
	fs.StringVar(&f.provider, "provider", "", "provider id from --config")
	fs.StringVar(&f.model, "model", "", "model/deployment id")
	fs.StringVar(&f.modelName, "model-name", "", "upstream model name")
	fs.StringVar(&f.baseURL, "base-url", "", "override provider base URL")
	fs.StringVar(&f.authMode, "auth-mode", "", "override auth mode")
	fs.StringVar(&f.apiKeyEnv, "api-key-env", "", "env var holding the API key or AWS access key id")
	fs.StringVar(&f.secretKeyEnv, "secret-key-env", "", "env var holding the AWS secret access key")
	fs.StringVar(&f.sessionEnv, "session-token-env", "", "env var holding the AWS session token")
	fs.StringVar(&f.awsRegion, "aws-region", "", "Bedrock region")
	fs.StringVar(&f.scenarios, "scenarios", "core", "core, acceptance, extended, all, or comma-separated scenario ids")
	fs.IntVar(&f.maxCalls, "max-calls", 5, "hard ceiling on upstream calls")
	fs.IntVar(&f.maxOutput, "max-output-tokens", 512, "per-request output token ceiling")
	fs.IntVar(&f.maxInput, "max-input-tokens", 4096, "per-request input token ceiling")
	fs.Float64Var(&f.maxCost, "max-cost-usd", 1, "preflight cost estimate ceiling in USD")
	fs.Float64Var(&f.inputCost, "input-cost-per-mtok", 0, "input price per million tokens")
	fs.Float64Var(&f.outputCost, "output-cost-per-mtok", 0, "output price per million tokens")
	fs.DurationVar(&f.callTimeout, "call-timeout", 30*time.Second, "per-call timeout")
	fs.DurationVar(&f.totalTimeout, "overall-timeout", 2*time.Minute, "overall canary timeout")
	fs.StringVar(&f.out, "out", "", "path for JSON evidence")
	fs.BoolVar(&f.acknowledge, "acknowledge-cost", false, "confirm that the canary may spend provider quota")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "nexaroute: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if !f.acknowledge {
		fmt.Fprintln(stderr, "nexaroute: refusing to run a live canary without --acknowledge-cost")
		return 2
	}
	if (f.preset == "") == (f.provider == "") {
		fmt.Fprintln(stderr, "nexaroute: specify exactly one of --preset or --provider")
		return 2
	}

	provider, model, err := resolveCanaryTarget(f)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: conformance canary:", err)
		return 2
	}
	if _, err := livecanary.ResolveScenarios([]string{f.scenarios}); err != nil {
		fmt.Fprintln(stderr, "nexaroute: conformance canary:", err)
		return 2
	}
	runner, err := livecanary.New(livecanary.Options{
		Provider:        provider,
		Model:           model,
		GatewayVersion:  version,
		Scenarios:       []string{f.scenarios},
		MaxCalls:        f.maxCalls,
		MaxOutputTokens: f.maxOutput,
		MaxInputTokens:  f.maxInput,
		MaxCostUSD:      f.maxCost,
		CallTimeout:     f.callTimeout,
		OverallTimeout:  f.totalTimeout,
	})
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: conformance canary:", err)
		return 2
	}
	defer runner.Close()

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, f.totalTimeout)
	defer cancel()
	report := runner.Run(ctx)
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: cannot encode canary report:", err)
		return 2
	}
	raw = append(raw, '\n')
	if f.out != "" {
		if err := writeCanaryReport(f.out, raw); err != nil {
			fmt.Fprintln(stderr, "nexaroute: cannot write canary report:", err)
			return 2
		}
		fmt.Fprintln(stdout, filepath.Clean(f.out))
	} else {
		_, _ = stdout.Write(raw)
	}
	fmt.Fprint(stderr, report.Summary())
	return canaryExitCode(report.Outcome)
}

func resolveCanaryTarget(f canaryFlags) (config.ProviderConfig, config.ModelConfig, error) {
	if f.preset != "" {
		return resolvePresetTarget(f)
	}
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("cannot load config: %w", err)
	}
	var provider *config.ProviderConfig
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == f.provider {
			copy := cfg.Providers[i]
			provider = &copy
			break
		}
	}
	if provider == nil {
		return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("provider %q not found in %s", f.provider, f.configPath)
	}
	var model config.ModelConfig
	if f.model == "" {
		var enabled []config.ModelConfig
		for _, m := range provider.Models {
			if m.Enabled {
				enabled = append(enabled, m)
			}
		}
		if len(enabled) != 1 {
			return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("provider %q has %d enabled models; specify --model", provider.ID, len(enabled))
		}
		model = enabled[0]
	} else {
		found := false
		for _, m := range provider.Models {
			if m.ID == f.model {
				model = m
				found = true
				break
			}
		}
		if !found {
			return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("model %q not found on provider %q", f.model, provider.ID)
		}
	}
	if !model.Enabled {
		return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("model %q is disabled on provider %q; enable it or choose another model", model.ID, provider.ID)
	}
	if f.modelName != "" {
		model.Model = f.modelName
	}
	if f.baseURL != "" {
		provider.BaseURL = f.baseURL
	}
	if f.authMode != "" {
		provider.AuthMode = f.authMode
	}
	if f.apiKeyEnv != "" {
		provider.APIKeyEnv = f.apiKeyEnv
	}
	if f.secretKeyEnv != "" {
		provider.SecretKeyEnv = f.secretKeyEnv
	}
	if f.sessionEnv != "" {
		provider.SessionTokenEnv = f.sessionEnv
	}
	if f.awsRegion != "" {
		provider.AWSRegion = f.awsRegion
	}
	if f.inputCost != 0 {
		model.InputCostPerMTok = f.inputCost
	}
	if f.outputCost != 0 {
		model.OutputCostPerMTok = f.outputCost
	}
	provider.ApplyDefaults()
	return *provider, model, nil
}

func resolvePresetTarget(f canaryFlags) (config.ProviderConfig, config.ModelConfig, error) {
	var preset *providers.Preset
	for _, p := range providers.Presets() {
		if p.ID == f.preset {
			copy := p
			preset = &copy
			break
		}
	}
	if preset == nil {
		return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("unknown preset %q", f.preset)
	}
	if strings.TrimSpace(f.model) == "" {
		return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("--model is required with --preset")
	}
	baseURL := strings.TrimSpace(f.baseURL)
	if baseURL == "" {
		baseURL = preset.BaseURL
	}
	if baseURL == "" {
		return config.ProviderConfig{}, config.ModelConfig{}, fmt.Errorf("preset %q has no default base URL; pass --base-url", preset.ID)
	}
	authMode := strings.TrimSpace(f.authMode)
	if authMode == "" {
		authMode = preset.AuthMode
	}
	apiKeyEnv := strings.TrimSpace(f.apiKeyEnv)
	if apiKeyEnv == "" && authMode != "" && authMode != "none" {
		apiKeyEnv = "NEXAROUTE_CANARY_API_KEY" // #nosec G101 -- environment variable name, not a secret value.
	}
	secretKeyEnv := strings.TrimSpace(f.secretKeyEnv)
	if preset.Type == "bedrock" && secretKeyEnv == "" {
		secretKeyEnv = "NEXAROUTE_CANARY_SECRET_KEY" // #nosec G101 -- environment variable name, not a secret value.
	}
	sessionEnv := strings.TrimSpace(f.sessionEnv)
	if preset.Type == "bedrock" && sessionEnv == "" {
		sessionEnv = "NEXAROUTE_CANARY_SESSION_TOKEN"
	}
	awsRegion := strings.TrimSpace(f.awsRegion)
	if awsRegion == "" {
		awsRegion = "us-east-1"
	}
	modelName := strings.TrimSpace(f.modelName)
	if modelName == "" {
		modelName = f.model
	}
	provider := config.ProviderConfig{
		ID:              "canary-" + preset.ID,
		Name:            "Canary " + preset.Name,
		Type:            preset.Type,
		BaseURL:         baseURL,
		AuthMode:        authMode,
		APIKeyEnv:       apiKeyEnv,
		SecretKeyEnv:    secretKeyEnv,
		SessionTokenEnv: sessionEnv,
		AWSRegion:       awsRegion,
		ChatPath:        preset.ChatPath,
		MessagesPath:    preset.MessagesPath,
		ResponsesPath:   preset.ResponsesPath,
		Enabled:         true,
		MaxConcurrency:  1,
		Models: []config.ModelConfig{{
			ID:                f.model,
			Model:             modelName,
			Enabled:           true,
			Weight:            1,
			InputCostPerMTok:  f.inputCost,
			OutputCostPerMTok: f.outputCost,
		}},
	}
	provider.ApplyDefaults()
	return provider, provider.Models[0], nil
}

func writeCanaryReport(path string, raw []byte) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".canary-report-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
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

func canaryExitCode(outcome string) int {
	switch outcome {
	case livecanary.OutcomePass, livecanary.OutcomeNoEvidence:
		return 0
	case livecanary.OutcomePartial:
		return 3
	case livecanary.OutcomeCanceled:
		return 130
	default:
		return 1
	}
}

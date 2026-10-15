package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// First-run wizard (M6-5): `nexaroute init` produces a valid,
// ready-to-serve config with one provider and one model. Every
// question has a default; the wizard asks for the API key's env
// var NAME only — never a key value. Flags make it fully
// scriptable (no prompts when every value is provided).

const (
	initExitOK    = 0
	initExitError = 1
	initExitUsage = 2
)

type initOptions struct {
	out        string
	listen     string
	providerID string
	typ        string
	baseURL    string
	apiKeyEnv  string
	model      string
	force      bool
	provided   map[string]bool
}

func runInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o initOptions
	fs.StringVar(&o.out, "out", defaultConfigPath(), "config file to write")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "listen address")
	fs.StringVar(&o.providerID, "provider-id", "primary", "provider id")
	fs.StringVar(&o.typ, "type", "openai_compatible", "provider type (openai_compatible | anthropic_compatible | gemini)")
	fs.StringVar(&o.baseURL, "base-url", "", "provider base URL (prompted when omitted)")
	fs.StringVar(&o.apiKeyEnv, "api-key-env", "", "env var NAME holding the provider API key (prompted when omitted; empty = no key)")
	fs.StringVar(&o.model, "model", "", "upstream model name to enable (prompted when omitted)")
	fs.BoolVar(&o.force, "force", false, "overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		return initExitUsage
	}
	o.provided = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.provided[f.Name] = true })

	if _, err := os.Stat(o.out); err == nil && !o.force {
		fmt.Fprintf(stderr, "init: %s already exists (use --force to overwrite)\n", o.out)
		return initExitError
	}

	reader := bufio.NewReader(stdin)
	ask := func(flagName, prompt, def string, current string) string {
		if o.provided[flagName] {
			return current
		}
		fmt.Fprintf(stdout, "%s [%s]: ", prompt, def)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		return line
	}

	if !o.provided["base-url"] {
		def := "https://api.openai.com"
		if o.typ == "anthropic_compatible" {
			def = "https://api.anthropic.com"
		} else if o.typ == "gemini" {
			def = "https://generativelanguage.googleapis.com"
		}
		o.baseURL = ask("base-url", "Provider base URL", def, o.baseURL)
	}
	if !o.provided["api-key-env"] {
		def := "OPENAI_API_KEY"
		if o.typ == "anthropic_compatible" {
			def = "ANTHROPIC_API_KEY"
		} else if o.typ == "gemini" {
			def = "GEMINI_API_KEY"
		}
		o.apiKeyEnv = ask("api-key-env", "Env var NAME holding the API key (blank = none)", def, o.apiKeyEnv)
	}
	if !o.provided["model"] {
		def := "gpt-4o-mini"
		if o.typ == "anthropic_compatible" {
			def = "claude-sonnet-4-5"
		} else if o.typ == "gemini" {
			def = "gemini-2.5-flash"
		}
		o.model = ask("model", "Model to enable", def, o.model)
	}
	if !o.provided["listen"] {
		o.listen = ask("listen", "Listen address", o.listen, o.listen)
	}

	cfg := config.Default()
	cfg.Listen = o.listen
	modelID := strings.NewReplacer("/", "-", ":", "-").Replace(o.model)
	provider := config.ProviderConfig{
		ID:      o.providerID,
		Name:    o.providerID,
		Type:    o.typ,
		BaseURL: strings.TrimSuffix(o.baseURL, "/"),
		Enabled: true,
		Models: []config.ModelConfig{{
			ID:      modelID,
			Model:   o.model,
			Enabled: true,
			Capabilities: config.Capabilities{
				Streaming: true,
				Tools:     true,
			},
		}},
	}
	if o.apiKeyEnv != "" {
		provider.APIKeyEnv = o.apiKeyEnv
	}
	cfg.Providers = []config.ProviderConfig{provider}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "init: the resulting config does not validate: %v\n", err)
		return initExitError
	}
	if err := config.SaveAtomic(o.out, cfg); err != nil {
		fmt.Fprintf(stderr, "init: write config: %v\n", err)
		return initExitError
	}
	fmt.Fprintf(stdout, "Wrote %s\n", o.out)
	fmt.Fprintf(stdout, "  provider %s (%s) at %s\n", o.providerID, o.typ, provider.BaseURL)
	fmt.Fprintf(stdout, "  model %s enabled (streaming + tools)\n", o.model)
	if o.apiKeyEnv != "" {
		fmt.Fprintf(stdout, "  API key: read at runtime from the %s environment variable (never stored in the config)\n", o.apiKeyEnv)
	}
	fmt.Fprintf(stdout, "Next: start the gateway with this config, open http://%s/ for the admin UI,\n", o.listen)
	fmt.Fprintf(stdout, "and send a chat request to /v1/chat/completions with model %q.\n", o.model)
	return initExitOK
}

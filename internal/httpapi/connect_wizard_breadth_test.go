package httpapi

import (
	"strings"
	"testing"
)

// #415 items 1 + 8: the connect wizard covers every verified
// coding client — Claude Code, Codex CLI, OpenCode and generic
// OpenAI — with snippets that name the GATEWAY (public base URL
// + a client-key variable) and never an upstream credential;
// and it can mint a per-client scoped key preset to the route
// through the existing virtual-key abstraction.
func TestConnectWizardBreadthWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		// Codex CLI: config.toml provider block against the
		// gateway base, key via an env var NAME (never a value),
		// Responses wire API.
		"model_providers.nexaroute",
		`env_key = "NEXAROUTE_API_KEY"`,
		`wire_api = "responses"`,
		"Copy Codex setup",
		// OpenCode: opencode.json provider via the
		// openai-compatible SDK, key referenced from the env.
		"@ai-sdk/openai-compatible",
		"{env:NEXAROUTE_API_KEY}",
		"Copy OpenCode setup",
		// Scoped key: created from the connect flow, preset with
		// the route + public model scope, via the existing
		// client-keys modal.
		"connect-create-key",
		"function openCreateKey(preset = {})",
		"Create scoped key for this route",
		"$('#key-routes').value = preset.routes.join(', ')",
		"$('#key-models').value = preset.models.join(', ')",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing connect-wizard wiring %q", want)
		}
	}
}

package providers

import (
	"os"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestPresetsExposeResponsesAndGeminiProtocols(t *testing.T) {
	byID := map[string]Preset{}
	for _, p := range Presets() {
		byID[p.ID] = p
	}
	responses, ok := byID["openai-responses"]
	if !ok {
		t.Fatal("OpenAI Responses preset missing")
	}
	if responses.Type != "openai_responses" || responses.AuthMode != "bearer" || responses.ResponsesPath != "/v1/responses" {
		t.Fatalf("OpenAI Responses preset is incomplete: %+v", responses)
	}
	gemini, ok := byID["google-gemini"]
	if !ok {
		t.Fatal("Google Gemini preset missing")
	}
	if gemini.Type != "gemini" || gemini.AuthMode != "x-goog-api-key" || gemini.ModelsPath != "/v1beta/models" {
		t.Fatalf("Google Gemini preset is incomplete: %+v", gemini)
	}
}

func TestRegistryPrepareReusesUnchangedAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		Enabled: true,
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := reg.Get("p")
	if !ok {
		t.Fatal("missing initial adapter")
	}
	next, err := reg.Prepare(cfg, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	after, ok := next.Get("p")
	if !ok || before != after {
		t.Fatal("unchanged provider adapter was rebuilt")
	}
}

func TestRegistryPrepareRebuildsChangedAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		Enabled: true,
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := reg.Get("p")
	next, err := reg.Prepare(cfg, map[string]struct{}{"p": {}})
	if err != nil {
		t.Fatal(err)
	}
	after, ok := next.Get("p")
	if !ok || before == after {
		t.Fatal("changed provider adapter was incorrectly reused")
	}
}

func TestRegistryDetectsResolvedEnvironmentCredentialRotation(t *testing.T) {
	const envName = "NEXAROUTE_TEST_ROTATING_KEY"
	old, had := os.LookupEnv(envName)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(envName, old)
		} else {
			_ = os.Unsetenv(envName)
		}
	})
	if err := os.Setenv(envName, "key-one"); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		APIKeyEnv: envName, AuthMode: "bearer", Enabled: true,
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reg.CredentialsMatchProvider(cfg.Providers[0]) {
		t.Fatal("fresh registry should match resolved environment credential")
	}

	if err := os.Setenv(envName, "key-two"); err != nil {
		t.Fatal(err)
	}
	if reg.CredentialsMatchProvider(cfg.Providers[0]) {
		t.Fatal("rotated environment credential was not detected")
	}

	next, err := reg.Prepare(cfg, map[string]struct{}{"p": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !next.CredentialsMatchProvider(cfg.Providers[0]) {
		t.Fatal("rebuilt adapter did not capture rotated environment credential")
	}
}

func TestRegistryKeepsEnvAdapterWhenResolvedCredentialIsUnchanged(t *testing.T) {
	const envName = "NEXAROUTE_TEST_STABLE_KEY"
	old, had := os.LookupEnv(envName)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(envName, old)
		} else {
			_ = os.Unsetenv(envName)
		}
	})
	if err := os.Setenv(envName, "stable-key"); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		APIKeyEnv: envName, AuthMode: "bearer", Enabled: true,
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := reg.Get("p")
	if !reg.CredentialsMatchProvider(cfg.Providers[0]) {
		t.Fatal("stable environment credential should match")
	}
	next, err := reg.Prepare(cfg, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := next.Get("p")
	if before != after {
		t.Fatal("stable environment credential caused unnecessary adapter rebuild")
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProviders = `"providers": [{"id": "p1", "name": "P1", "type": "openai_compatible", "base_url": "https://api.example.com", "enabled": true, "models": [{"id": "m1", "model": "gpt-4", "enabled": true}]}]`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasWarningContaining(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestImplicitDefaultWarningIdentifiesKeyAndBehavior(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "probe": {"max_tokens": 0}, `+testProviders+`}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Probe.MaxTokens != 1 {
		t.Fatalf("default mode must still default probe.max_tokens to 1, got %d", cfg.Probe.MaxTokens)
	}
	if !hasWarningContaining(cfg.Warnings, "probe.max_tokens") {
		t.Fatalf("warning must identify the config key, got %v", cfg.Warnings)
	}
	if !hasWarningContaining(cfg.Warnings, "defaulted to 1") {
		t.Fatalf("warning must identify the chosen behavior, got %v", cfg.Warnings)
	}
}

func TestLegacyPublicModelMigrationWarning(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "routing": {"public_model": "legacy-gpt"}, `+testProviders+`}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarningContaining(cfg.Warnings, "routing.public_model") {
		t.Fatalf("warning must identify the legacy key, got %v", cfg.Warnings)
	}
	if !hasWarningContaining(cfg.Warnings, "auto-migrated") {
		t.Fatalf("warning must identify the migration behavior, got %v", cfg.Warnings)
	}
	if len(cfg.VirtualEndpoints) != 1 {
		t.Fatalf("legacy public_model should be migrated to a virtual endpoint, got %d", len(cfg.VirtualEndpoints))
	}
}

func TestStrictModeFailsOnImplicitDefault(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "probe": {"max_tokens": 0}, `+testProviders+`}`)
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "true")
	_, err := Load(path)
	if err == nil {
		t.Fatal("strict mode must fail on probe.max_tokens=0")
	}
	if !strings.Contains(err.Error(), "probe.max_tokens") {
		t.Fatalf("strict error must name the config key, got %v", err)
	}
	if !strings.Contains(err.Error(), "strict config validation failed") {
		t.Fatalf("strict error must be actionable, got %v", err)
	}
}

func TestStrictModeFailsOnLegacyPublicModel(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "routing": {"public_model": "legacy-gpt"}, `+testProviders+`}`)
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "true")
	_, err := Load(path)
	if err == nil {
		t.Fatal("strict mode must fail on legacy routing.public_model")
	}
	if !strings.Contains(err.Error(), "routing.public_model") {
		t.Fatalf("strict error must name the legacy key, got %v", err)
	}
}

func TestStrictModeDeterministic(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "probe": {"max_tokens": 0}, `+testProviders+`}`)
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "true")
	_, err1 := Load(path)
	_, err2 := Load(path)
	if err1 == nil || err2 == nil {
		t.Fatal("strict mode must fail deterministically")
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("strict error must be deterministic:\n%v\n%v", err1, err2)
	}
}

func TestStrictModeAllowsValidConfig(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "admin":{"emergency_access_enabled":false}, `+testProviders+`}`)
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "true")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("strict mode must accept a config with no implicit defaults: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("valid config should produce no warnings, got %v", cfg.Warnings)
	}
}

func TestDefaultModePreservesLegacyBehavior(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "probe": {"max_tokens": 0}, `+testProviders+`}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Probe.MaxTokens != 1 {
		t.Fatalf("default mode must keep probe.max_tokens=0 -> 1 behavior, got %d", cfg.Probe.MaxTokens)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("default mode must still emit implicit-default warnings")
	}
}

func TestDefaultModeValidConfigNoWarnings(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "admin":{"emergency_access_enabled":false}, `+testProviders+`}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("valid config should produce no warnings, got %v", cfg.Warnings)
	}
}

func TestWarningsDeduplicatedAcrossLoadPhases(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "probe": {"max_tokens": 0}, `+testProviders+`}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "probe.max_tokens") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("warning must be deduplicated across load phases, found %d: %v", count, cfg.Warnings)
	}
}

func TestStrictModeDisabledByDefault(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "probe": {"max_tokens": 0}, `+testProviders+`}`)
	if _, err := Load(path); err != nil {
		t.Fatalf("without NEXAROUTE_STRICT_CONFIG the load must succeed: %v", err)
	}
}

func TestStrictModeInvalidBoolean(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", `+testProviders+`}`)
	t.Setenv("NEXAROUTE_STRICT_CONFIG", "maybe")
	_, err := Load(path)
	if err == nil {
		t.Fatal("invalid NEXAROUTE_STRICT_CONFIG boolean must be rejected")
	}
	if !strings.Contains(err.Error(), "NEXAROUTE_STRICT_CONFIG") {
		t.Fatalf("error must name the invalid variable, got %v", err)
	}
}

func TestWarningsNeverContainSecretValues(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "probe": {"max_tokens": 0}, "admin": {"api_key": "super-secret-key"}, `+testProviders+`}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "super-secret-key") {
			t.Fatalf("warning leaked a secret value: %q", w)
		}
	}
}

func TestWarningsNotSerializedToDisk(t *testing.T) {
	path := writeConfig(t, `{"listen": "127.0.0.1:8080", "admin":{"emergency_access_enabled":false}, "probe": {"max_tokens": 0}, `+testProviders+`}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "probe.max_tokens: 0 is not a valid value") {
		t.Fatalf("warnings must not be persisted to disk: %s", raw)
	}
	if !strings.Contains(string(raw), `"emergency_access_enabled": false`) {
		t.Fatalf("explicit emergency-access denial was not persisted: %s", raw)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Warnings) != 0 {
		t.Fatalf("reloaded config should have no warnings, got %v", reloaded.Warnings)
	}
}

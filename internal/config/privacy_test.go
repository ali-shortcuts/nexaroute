package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validPrivacyBase() Config {
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "bearer", Enabled: true,
		Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	return cfg
}

func TestDataHandlingValidationTable(t *testing.T) {
	cases := []struct {
		name    string
		dh      DataHandlingConfig
		wantErr string
	}{
		{"empty ok", DataHandlingConfig{}, ""},
		{"yes/none ok", DataHandlingConfig{TrainsOnData: "no", Retention: "none"}, ""},
		{"yes/limited ok", DataHandlingConfig{TrainsOnData: "yes", Retention: "limited"}, ""},
		{"unknown ok", DataHandlingConfig{TrainsOnData: "unknown", Retention: "unknown"}, ""},
		{"bad trains", DataHandlingConfig{TrainsOnData: "sometimes", Retention: "none"}, "data_handling.trains_on_data"},
		{"bad retention", DataHandlingConfig{TrainsOnData: "no", Retention: "forever"}, "data_handling.retention"},
		{"note too long", DataHandlingConfig{TrainsOnData: "no", Retention: "none", Note: strings.Repeat("x", 201)}, "data_handling.note"},
		{"note control char", DataHandlingConfig{TrainsOnData: "no", Retention: "none", Note: "bad\nnote"}, "data_handling.note"},
		{"note control tab", DataHandlingConfig{TrainsOnData: "no", Retention: "none", Note: "bad\tnote"}, "data_handling.note"},
		{"note ok", DataHandlingConfig{TrainsOnData: "no", Retention: "none", Note: "vendor DPA v3"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPrivacyBase()
			cfg.Providers[0].DataHandling = tc.dh
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestDataHandlingNormalization(t *testing.T) {
	cfg := validPrivacyBase()
	cfg.ApplyDefaults()
	if cfg.Providers[0].DataHandling.TrainsOnData != "unknown" {
		t.Fatalf("trains_on_data=%q want unknown", cfg.Providers[0].DataHandling.TrainsOnData)
	}
	if cfg.Providers[0].DataHandling.Retention != "unknown" {
		t.Fatalf("retention=%q want unknown", cfg.Providers[0].DataHandling.Retention)
	}
	cfg2 := validPrivacyBase()
	cfg2.Providers[0].DataHandling = DataHandlingConfig{TrainsOnData: " NO ", Retention: " None "}
	cfg2.ApplyDefaults()
	if cfg2.Providers[0].DataHandling.TrainsOnData != "no" {
		t.Fatalf("trains_on_data=%q want no", cfg2.Providers[0].DataHandling.TrainsOnData)
	}
	if cfg2.Providers[0].DataHandling.Retention != "none" {
		t.Fatalf("retention=%q want none", cfg2.Providers[0].DataHandling.Retention)
	}
}

func TestPrivacyFieldsValidationTable(t *testing.T) {
	cases := []struct {
		name      string
		routePriv string
		keyPriv   string
		wantErr   string
	}{
		{"empty ok", "", "", ""},
		{"any ok", "any", "any", ""},
		{"no_training ok", "no_training", "no_training", ""},
		{"bad route", "strict", "", "privacy"},
		{"bad key", "", "strict", "require_privacy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPrivacyBase()
			cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
			cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool1", Privacy: tc.routePriv}}
			cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{
				ID:             "k1",
				KeyHash:        strings.Repeat("a", 64),
				RequirePrivacy: tc.keyPriv,
			}}
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestPrivacyNormalization(t *testing.T) {
	cfg := validPrivacyBase()
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool1", Privacy: " NO_TRAINING "}}
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", KeyHash: strings.Repeat("b", 64), RequirePrivacy: " Any "}}
	cfg.ApplyDefaults()
	if cfg.RouteProfiles[0].Privacy != "no_training" {
		t.Fatalf("route privacy=%q want no_training", cfg.RouteProfiles[0].Privacy)
	}
	if cfg.ClientAuth.VirtualKeys[0].RequirePrivacy != "any" {
		t.Fatalf("key require_privacy=%q want any", cfg.ClientAuth.VirtualKeys[0].RequirePrivacy)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("normalized privacy rejected: %v", err)
	}
}

func TestPrivacyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := validPrivacyBase()
	cfg.Providers[0].DataHandling = DataHandlingConfig{TrainsOnData: "no", Retention: "limited", Note: "DPA v2"}
	cfg.CandidatePools = []CandidatePoolConfig{{ID: "pool1", Mode: "all"}}
	cfg.RouteProfiles = []RouteProfileConfig{{ID: "r1", CandidatePool: "pool1", Privacy: "no_training"}}
	cfg.ClientAuth.VirtualKeys = []VirtualKeyConfig{{ID: "k1", KeyHash: strings.Repeat("c", 64), RequirePrivacy: "no_training"}}
	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Providers[0].DataHandling.TrainsOnData != "no" ||
		loaded.Providers[0].DataHandling.Retention != "limited" ||
		loaded.Providers[0].DataHandling.Note != "DPA v2" {
		t.Fatalf("data_handling lost: %+v", loaded.Providers[0].DataHandling)
	}
	if loaded.RouteProfiles[0].Privacy != "no_training" {
		t.Fatalf("route privacy lost: %q", loaded.RouteProfiles[0].Privacy)
	}
	if loaded.ClientAuth.VirtualKeys[0].RequirePrivacy != "no_training" {
		t.Fatalf("key require_privacy lost: %q", loaded.ClientAuth.VirtualKeys[0].RequirePrivacy)
	}
}

func TestPrivacyOldConfigWithoutFieldsLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Default()
	cfg.Providers = []ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "bearer", Enabled: true,
		Models: []ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if provs, ok := m["providers"].([]any); ok && len(provs) > 0 {
		if pm, ok := provs[0].(map[string]any); ok {
			delete(pm, "data_handling")
		}
	}
	delete(m, "route_profiles")
	raw2, _ := json.Marshal(m)
	if err := os.WriteFile(path, append(raw2, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("old config without privacy fields must load: %v", err)
	}
	if loaded.Providers[0].DataHandling.TrainsOnData != "unknown" ||
		loaded.Providers[0].DataHandling.Retention != "unknown" {
		t.Fatalf("old config should normalize to unknown: %+v", loaded.Providers[0].DataHandling)
	}
}

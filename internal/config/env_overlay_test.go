package config

import (
	"path/filepath"
	"testing"
)

func TestEmptyAdminEnvDoesNotEraseFileKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	base := Default()
	base.Admin.APIKey = "file-admin-key"
	if err := SaveAtomic(path, base); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_ADMIN_KEY", "   ")
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Admin.APIKey != "file-admin-key" {
		t.Fatalf("empty env erased durable admin key: %q", got.Admin.APIKey)
	}
}

func TestLoadBaseExcludesRuntimeEnvironmentOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	base := Default()
	base.Listen = "127.0.0.1:8080"
	base.Admin.APIKey = "file-key"
	if err := SaveAtomic(path, base); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_LISTEN", "127.0.0.1:19090")
	t.Setenv("NEXAROUTE_ADMIN_KEY", "env-key")

	effective, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := LoadBase(path)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Listen != "127.0.0.1:19090" || effective.Admin.APIKey != "env-key" {
		t.Fatalf("runtime overlay not applied: %+v", effective.Admin)
	}
	if durable.Listen != "127.0.0.1:8080" || durable.Admin.APIKey != "file-key" {
		t.Fatalf("runtime overlay leaked into durable config: listen=%q key=%q", durable.Listen, durable.Admin.APIKey)
	}
}

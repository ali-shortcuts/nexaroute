package config

import (
	"os"
	"path/filepath"
	"testing"
)

// M6-5: every shipped example config must load and validate.
func TestExampleConfigsLoadAndValidate(t *testing.T) {
	dir := filepath.Join("..", "..", "examples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		found++
		path := filepath.Join(dir, e.Name())
		cfg, err := Load(path)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		if len(cfg.Providers) == 0 {
			t.Errorf("%s: no providers", e.Name())
		}
	}
	if found < 4 {
		t.Fatalf("expected at least 4 example configs, found %d", found)
	}
}

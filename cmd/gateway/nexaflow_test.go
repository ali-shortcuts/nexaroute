package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Combined wiring attach tests (issue #765): disabled is a no-op that
// touches nothing, and the two fail-closed attach errors (unresolvable
// loopback derivation, unopenable store) surface before any route is
// mounted.
func TestMaybeAttachNexaflowDisabledIsNoop(t *testing.T) {
	closer, err := maybeAttachNexaflow(nil, config.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("disabled attach returned an error: %v", err)
	}
	if closer != nil {
		t.Fatal("disabled attach returned a closer")
	}
}

func TestMaybeAttachNexaflowErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("invalid listen cannot derive loopback", func(t *testing.T) {
		cfg := config.Config{
			Listen:   "not-a-host-port",
			Nexaflow: config.NexaflowConfig{Enabled: true, StorePath: filepath.Join(dir, "valid.json")},
		}
		_, err := maybeAttachNexaflow(nil, cfg, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "derive loopback address") {
			t.Fatalf("err = %v, want loopback derivation failure", err)
		}
	})

	t.Run("unopenable store fails closed", func(t *testing.T) {
		// Point the store at an existing directory: OpenFileStore must
		// fail rather than start from a half-initialized state.
		cfg := config.Config{
			Listen:   "127.0.0.1:1",
			Nexaflow: config.NexaflowConfig{Enabled: true, StorePath: dir},
		}
		_, err := maybeAttachNexaflow(nil, cfg, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "open store") {
			t.Fatalf("err = %v, want store open failure", err)
		}
	})
}

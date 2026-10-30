package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestAttachNexaflowDisabledIsNoop(t *testing.T) {
	cleanup, err := attachNexaflow(config.Config{}, t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatalf("disabled attach returned an error: %v", err)
	}
	if cleanup == nil {
		t.Fatal("disabled attach returned a nil cleanup")
	}
	cleanup()
}

func TestAttachNexaflowErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("invalid listen cannot derive loopback", func(t *testing.T) {
		cfg := config.Config{
			Listen:   "not-a-host-port",
			Nexaflow: config.NexaflowConfig{Enabled: true, StorePath: filepath.Join(dir, "valid.json")},
		}
		_, err := attachNexaflow(cfg, dir, nil, nil, nil)
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
		_, err := attachNexaflow(cfg, dir, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "open store") {
			t.Fatalf("err = %v, want store open failure", err)
		}
	})
}

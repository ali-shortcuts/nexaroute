package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

const controlPlaneNamespace = "gateway/runtime"

type controlPlaneRuntime struct {
	manager *controlplane.Manager
}

func openControlPlaneRuntime(cfg config.ControlPlaneConfig, configPath string) (*controlPlaneRuntime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	backend := cfg.Backend
	if backend == "" {
		backend = "file"
	}
	var store controlplane.Store
	switch backend {
	case "file":
		path := cfg.StatePath
		if path == "" {
			path = configPath + ".controlplane.json"
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(configPath), path)
		}
		var err error
		store, err = controlplane.NewFileStore(path)
		if err != nil {
			return nil, err
		}
	case "memory":
		store = controlplane.NewMemoryStore()
	case "postgres", "redis":
		// The existing SQL/Redis contracts deliberately do not own a driver or
		// client. Refuse startup instead of silently falling back to memory.
		return nil, fmt.Errorf("control_plane.backend=%s is configured but no runtime client/driver is registered; refusing unsafe fallback", backend)
	default:
		return nil, errors.New("unsupported control-plane backend")
	}
	mode := controlplane.FailureMode(cfg.ConfigFailure)
	if mode == "" {
		mode = controlplane.LastKnownGood
	}
	manager, err := controlplane.NewManager(store, mode)
	if err != nil {
		return nil, err
	}
	if cfg.MigrationDryRun {
		return &controlPlaneRuntime{manager: manager}, nil
	}
	payload, err := json.Marshal(struct {
		Schema  int    `json:"schema"`
		Backend string `json:"backend"`
	}{Schema: 1, Backend: backend})
	if err != nil {
		return nil, err
	}
	current, err := manager.Load(context.Background(), controlPlaneNamespace)
	if errors.Is(err, controlplane.ErrNotFound) {
		if _, err = manager.Save(context.Background(), controlPlaneNamespace, 0, payload); err != nil {
			return nil, fmt.Errorf("initialize control-plane snapshot: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("load control-plane snapshot: %w", err)
	} else if _, err = manager.Save(context.Background(), controlPlaneNamespace, current.Revision, payload); err != nil {
		return nil, fmt.Errorf("reconcile control-plane snapshot: %w", err)
	}
	return &controlPlaneRuntime{manager: manager}, nil
}

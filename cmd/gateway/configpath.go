package main

import (
	"os"
	"path/filepath"
)

func defaultConfigPath() string {
	if p := os.Getenv("NEXAROUTE_CONFIG"); p != "" {
		return p
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "nexaroute", "config.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "nexaroute", "config.json")
	}
	return "config.json"
}

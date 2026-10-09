package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/transport"
)

const (
	configExitOK      = 0
	configExitFailure = 1
	configExitUsage   = 2
)

// runConfig implements read-only config checks. Exit codes are stable for
// scripts: 0=valid/no differences, 1=invalid or operational failure, 2=usage.
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: nexaroute config validate|diff|dry-run [--config path]")
		return configExitUsage
	}
	command := args[0]
	fs := flag.NewFlagSet("config "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfigPath(), "path to JSON config")
	against := fs.String("against", "", "second config path for diff (default: built-in defaults)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "unexpected positional arguments")
		}
		return configExitUsage
	}
	if *path == "" {
		fmt.Fprintln(stderr, "config path required; pass --config")
		return configExitUsage
	}
	switch command {
	case "validate":
		cfg, err := loadCheckedConfig(*path)
		if err != nil {
			fmt.Fprintf(stderr, "config invalid: %v\n", err)
			return configExitFailure
		}
		fmt.Fprintf(stdout, "config valid: %s\n", *path)
		fmt.Fprintf(stdout, "providers=%d tls=%t\n", len(cfg.Providers), cfg.TLS.Enabled)
		return configExitOK
	case "dry-run":
		cfg, err := loadCheckedConfig(*path)
		if err != nil {
			fmt.Fprintf(stderr, "dry-run failed: %v\n", err)
			return configExitFailure
		}
		if _, err := providers.NewRegistry(cfg); err != nil {
			fmt.Fprintf(stderr, "dry-run failed while preparing providers: %v\n", err)
			return configExitFailure
		}
		prepared := 0
		for _, provider := range cfg.Providers {
			if provider.Enabled {
				prepared++
			}
		}
		fmt.Fprintf(stdout, "dry-run passed: providers_prepared=%d tls=%t\n", prepared, cfg.TLS.Enabled)
		fmt.Fprintln(stdout, "no listener opened; no probes or upstream requests sent; no config written")
		return configExitOK
	case "diff":
		left, err := loadCheckedConfig(*path)
		if err != nil {
			fmt.Fprintf(stderr, "config diff failed: %v\n", err)
			return configExitFailure
		}
		right := config.Default()
		right.ApplyDefaults()
		if *against != "" {
			right, err = loadCheckedConfig(*against)
			if err != nil {
				fmt.Fprintf(stderr, "comparison config invalid: %v\n", err)
				return configExitFailure
			}
		}
		changes, err := configDiff(left, right)
		if err != nil {
			fmt.Fprintf(stderr, "config diff failed: %v\n", err)
			return configExitFailure
		}
		if len(changes) == 0 {
			fmt.Fprintln(stdout, "no differences")
			return configExitOK
		}
		for _, line := range changes {
			fmt.Fprintln(stdout, line)
		}
		return configExitOK
	default:
		fmt.Fprintf(stderr, "unknown config command %q; use validate, diff, or dry-run\n", command)
		return configExitUsage
	}
}

func loadCheckedConfig(path string) (config.Config, error) {
	cfg, err := config.LoadBase(path)
	if err != nil {
		return cfg, err
	}
	if _, err := transport.ServerTLSConfig(cfg.TLS, filepath.Dir(path)); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func configDiff(left, right config.Config) ([]string, error) {
	leftValues, err := flattenedConfig(left)
	if err != nil {
		return nil, err
	}
	rightValues, err := flattenedConfig(right)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]struct{}, len(leftValues)+len(rightValues))
	for key := range leftValues {
		keys[key] = struct{}{}
	}
	for key := range rightValues {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var changes []string
	for _, key := range ordered {
		l, lok := leftValues[key]
		r, rok := rightValues[key]
		if lok == rok && l == r {
			continue
		}
		state := "changed"
		if !rok {
			state = "added"
		} else if !lok {
			state = "removed"
		}
		if sensitiveConfigPath(key) {
			changes = append(changes, key+": "+state+" (value redacted)")
		} else {
			changes = append(changes, key+": "+state)
		}
	}
	return changes, nil
}

func flattenedConfig(cfg config.Config) (map[string]string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	result := map[string]string{}
	flatten(value, "", result)
	return result, nil
}

func flatten(value any, path string, out map[string]string) {
	switch node := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := key
			if path != "" {
				child = path + "." + key
			}
			flatten(node[key], child, out)
		}
	case []any:
		for i, child := range node {
			flatten(child, fmt.Sprintf("%s[%d]", path, i), out)
		}
	default:
		b, _ := json.Marshal(node)
		out[path] = string(b)
	}
}

func sensitiveConfigPath(path string) bool {
	p := strings.ToLower(path)
	for _, part := range []string{"api_key", "proxy_url", "headers", "credentials", "client_auth.keys", "password", "secret"} {
		if strings.Contains(p, part) {
			return true
		}
	}
	return false
}

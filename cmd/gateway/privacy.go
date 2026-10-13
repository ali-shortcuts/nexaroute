package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/privacyaudit"
)

// Exit codes for `nexaroute privacy audit` (deterministic, documented in
// docs/OPERATIONS.md):
//
//	0 — audit completed, no error-severity findings
//	1 — audit completed, at least one error-severity finding
//	2 — usage error, or the config could not be read or parsed
const (
	privacyExitOK       = 0
	privacyExitFindings = 1
	privacyExitUsage    = 2
)

// maxAuditConfigBytes mirrors config.maxConfigBytes (16 MiB).
const maxAuditConfigBytes = 16 << 20

// runPrivacy implements the `nexaroute privacy` command group. It is
// strictly read-only: the config file is read as raw bytes and parsed in
// memory. Unlike config.Load/LoadBase it never decrypts, migrates, or
// rewrites the file, so auditing a legacy plaintext config cannot modify
// it. No provider requests are sent.
func runPrivacy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "audit" {
		fmt.Fprintln(stderr, "usage: nexaroute privacy audit [--config PATH] [--format text|json]")
		return privacyExitUsage
	}
	fs := flag.NewFlagSet("privacy audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfigPath(), "path to JSON config")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args[1:]); err != nil {
		return privacyExitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "nexaroute: privacy audit: unknown format %q (want text or json)\n", *format)
		return privacyExitUsage
	}
	if strings.TrimSpace(*path) == "" {
		fmt.Fprintln(stderr, "nexaroute: privacy audit: config path required")
		return privacyExitUsage
	}

	f, err := os.Open(*path)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute: privacy audit: cannot read config: %v\n", err)
		return privacyExitUsage
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAuditConfigBytes+1))
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute: privacy audit: cannot read config: %v\n", err)
		return privacyExitUsage
	}
	if len(raw) > maxAuditConfigBytes {
		fmt.Fprintln(stderr, "nexaroute: privacy audit: config exceeds safe size limit")
		return privacyExitUsage
	}

	cfg := config.Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(stderr, "nexaroute: privacy audit: malformed config: %v\n", err)
		return privacyExitUsage
	}
	cfg.ApplyDefaults()

	rep := privacyaudit.Audit(&cfg)

	switch *format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "nexaroute: privacy audit: cannot encode report: %v\n", err)
			return privacyExitUsage
		}
	default:
		for _, f := range rep.Findings {
			fmt.Fprintf(stdout, "[%s] %s %s: %s\n", strings.ToUpper(string(f.Severity)), f.Code, f.Scope, f.Message)
		}
		fmt.Fprintf(stdout, "privacy audit: %d error(s), %d warning(s), %d info\n", rep.Errors, rep.Warnings, rep.Infos)
	}

	if rep.HasErrors() {
		return privacyExitFindings
	}
	return privacyExitOK
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// Exit codes for `nexaroute audit verify` (documented in
// docs/OPERATIONS.md):
//
//	0 — the audit hash chain verified
//	1 — verification completed and found integrity problems
//	2 — usage error, or the security store could not be opened/read
const (
	auditExitOK       = 0
	auditExitProblems = 1
	auditExitUsage    = 2
)

// runAudit implements the `nexaroute audit` command group. The verify
// subcommand opens the admin security store read-only and checks the
// tamper-evident audit hash chain (M3-6). It reports metadata only —
// record counts, sequences, and the head hash — never event payloads.
func runAudit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(stderr, "usage: nexaroute audit verify [--config PATH] [--format text|json]")
		return auditExitUsage
	}
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfigPath(), "path to JSON config")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args[1:]); err != nil {
		return auditExitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "nexaroute: audit verify: unknown format %q (want text or json)\n", *format)
		return auditExitUsage
	}
	base, err := config.LoadBase(*path)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute: audit verify: load config: %v\n", err)
		return auditExitUsage
	}
	storePath, err := auditStorePath(base.Admin.SecurityStorePath, *path)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute: audit verify: %v\n", err)
		return auditExitUsage
	}
	store, err := securitystore.OpenReadOnly(storePath)
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute: audit verify: %v\n", err)
		fmt.Fprintln(stderr, "nexaroute: audit verify: the store may be locked by a running gateway; stop it before offline verification")
		return auditExitUsage
	}
	defer store.Close()
	report, err := store.VerifyAuditChain(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "nexaroute: audit verify: %v\n", err)
		return auditExitUsage
	}
	if *format == "json" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "nexaroute: audit verify: encode report: %v\n", err)
			return auditExitUsage
		}
		fmt.Fprintln(stdout, string(encoded))
	} else {
		fmt.Fprintf(stdout, "audit chain: %d records (%d chained, %d legacy pre-chain)\n", report.TotalRecords, report.ChainedRecords, report.LegacyRecords)
		if report.HasAnchor {
			fmt.Fprintf(stdout, "pruning anchor: seq %d\n", report.AnchorSeq)
		}
		if report.HeadHash != "" {
			fmt.Fprintf(stdout, "head: seq %d hash %s\n", report.LastSeq, report.HeadHash)
		}
		if report.Verified {
			fmt.Fprintln(stdout, "result: VERIFIED")
		} else {
			fmt.Fprintln(stdout, "result: FAILED")
			for _, problem := range report.Problems {
				fmt.Fprintf(stdout, "problem: %s\n", problem)
			}
		}
	}
	if !report.Verified {
		return auditExitProblems
	}
	return auditExitOK
}

// auditStorePath mirrors the gateway's admin security store path
// resolution (internal/httpapi.adminSecurityStorePath): the configured
// path, resolved against the config directory when relative, or the
// default private subdirectory.
func auditStorePath(configuredPath, configPath string) (string, error) {
	configuredPath = strings.TrimSpace(configuredPath)
	configPath = strings.TrimSpace(configPath)
	if configuredPath == "" {
		if configPath == "" {
			return "", fmt.Errorf("admin security store path is required when no config file path is available")
		}
		configuredPath = filepath.Join("security", "nexaroute-security.db")
	}
	if filepath.IsAbs(configuredPath) {
		return filepath.Clean(configuredPath), nil
	}
	base := "."
	if configPath != "" {
		base = filepath.Dir(configPath)
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve admin security store directory: %w", err)
	}
	return filepath.Join(absBase, configuredPath), nil
}

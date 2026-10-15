package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/policysim"
)

// Policy lifecycle CLI (M6-2): simulate / replay / promote /
// rollback for decision policies, mirroring the privacy and audit
// subcommands' shape and exit codes (0 ok, 1 error, 2 usage).

const (
	policyExitOK    = 0
	policyExitError = 1
	policyExitUsage = 2
)

func runPolicy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: nexaroute policy <simulate|replay|promote|rollback> [flags]")
		return policyExitUsage
	}
	switch args[0] {
	case "simulate":
		return runPolicySimulate(args[1:], stdout, stderr)
	case "replay":
		return runPolicyReplay(args[1:], stdout, stderr)
	case "promote":
		return runPolicyPromote(args[1:], stdout, stderr)
	case "rollback":
		return runPolicyRollback(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown policy subcommand %q\n", args[0])
		return policyExitUsage
	}
}

func policyFlags(name string, stderr io.Writer) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON config")
	format := fs.String("format", "text", "output format: text or json")
	return fs, configPath, format
}

func loadPolicyConfig(path string, stderr io.Writer) (config.Config, int) {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "policy: load config: %v\n", err)
		return config.Config{}, policyExitError
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "policy: config does not validate: %v\n", err)
		return config.Config{}, policyExitError
	}
	return cfg, policyExitOK
}

func runPolicySimulate(args []string, stdout, stderr io.Writer) int {
	fs, configPath, format := policyFlags("policy simulate", stderr)
	policyID := fs.String("policy", "", "decision policy id to evaluate")
	fixturesPath := fs.String("fixtures", "", "path to a fixtures JSON file ({cases:[...]})")
	if err := fs.Parse(args); err != nil {
		return policyExitUsage
	}
	if *policyID == "" || *fixturesPath == "" || (*format != "text" && *format != "json") {
		fmt.Fprintln(stderr, "usage: nexaroute policy simulate --config PATH --policy ID --fixtures FILE [--format text|json]")
		return policyExitUsage
	}
	cfg, code := loadPolicyConfig(*configPath, stderr)
	if code != policyExitOK {
		return code
	}
	raw, err := os.ReadFile(*fixturesPath)
	if err != nil {
		fmt.Fprintf(stderr, "policy simulate: read fixtures: %v\n", err)
		return policyExitError
	}
	var file policysim.FixtureFile
	if err := json.Unmarshal(raw, &file); err != nil {
		fmt.Fprintf(stderr, "policy simulate: parse fixtures: %v\n", err)
		return policyExitError
	}
	report, err := policysim.Simulate(context.Background(), cfg, *policyID, file.Cases)
	if err != nil {
		fmt.Fprintf(stderr, "policy simulate: %v\n", err)
		return policyExitError
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return policyExitOK
	}
	fmt.Fprintf(stdout, "policy %s — %d case(s), digest %s\n", report.PolicyID, len(report.Cases), report.Digest)
	for _, c := range report.Cases {
		fmt.Fprintf(stdout, "  %-24s winner=%s ordered=%s\n", c.Name, c.Winner, strings.Join(c.Ordered, ","))
	}
	return policyExitOK
}

func runPolicyReplay(args []string, stdout, stderr io.Writer) int {
	fs, configPath, format := policyFlags("policy replay", stderr)
	policyID := fs.String("policy", "", "decision policy id to evaluate")
	tracesPath := fs.String("traces", "", "path to a JSONL file of {request, recorded_winner} records")
	if err := fs.Parse(args); err != nil {
		return policyExitUsage
	}
	if *policyID == "" || *tracesPath == "" || (*format != "text" && *format != "json") {
		fmt.Fprintln(stderr, "usage: nexaroute policy replay --config PATH --policy ID --traces FILE [--format text|json]")
		return policyExitUsage
	}
	cfg, code := loadPolicyConfig(*configPath, stderr)
	if code != policyExitOK {
		return code
	}
	records, err := readReplayRecords(*tracesPath)
	if err != nil {
		fmt.Fprintf(stderr, "policy replay: %v\n", err)
		return policyExitError
	}
	report, err := policysim.Replay(context.Background(), cfg, *policyID, records)
	if err != nil {
		fmt.Fprintf(stderr, "policy replay: %v\n", err)
		return policyExitError
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return policyExitOK
	}
	fmt.Fprintf(stdout, "policy %s — replay %d decision(s): %d agree, %d diverge; digest %s\n",
		report.PolicyID, report.Total, report.Agreed, report.Total-report.Agreed, report.Digest)
	for _, c := range report.Cases {
		if !c.Agree {
			fmt.Fprintf(stdout, "  DIVERGE %-16s recorded=%s policy=%s\n", c.Name, c.RecordedWinner, c.Winner)
		}
	}
	return policyExitOK
}

func readReplayRecords(path string) ([]policysim.ReplayRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []policysim.ReplayRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var rec policysim.ReplayRecord
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		records = append(records, rec)
	}
	return records, scanner.Err()
}

// ---- promote / rollback with an append-only audit log ----

type policyAuditRecord struct {
	Timestamp          string `json:"ts"`
	Action             string `json:"action"`
	FromPolicy         string `json:"from_policy"`
	ToPolicy           string `json:"to_policy"`
	ConfigSHA256Before string `json:"config_sha256_before"`
	ConfigSHA256After  string `json:"config_sha256_after"`
}

func fileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func defaultAuditLogPath(configPath string) string {
	return configPath + ".policy-audit.jsonl"
}

func appendPolicyAudit(path string, rec policyAuditRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func readPolicyAudit(path string) ([]policyAuditRecord, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []policyAuditRecord
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec policyAuditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("audit log is corrupt: %w", err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// applyPolicyPointer validates and atomically rewrites the config
// with decision.policy set to target, appending the audit record.
// It returns the record written.
func applyPolicyPointer(configPath, auditPath, action, target string, stderr io.Writer) (policyAuditRecord, int) {
	cfg, code := loadPolicyConfig(configPath, stderr)
	if code != policyExitOK {
		return policyAuditRecord{}, code
	}
	found := false
	for _, p := range cfg.DecisionPolicies {
		if p.ID == target {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(stderr, "policy %s: %q is not defined in decision_policies\n", action, target)
		return policyAuditRecord{}, policyExitError
	}
	before, err := fileSHA256(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "policy %s: hash config: %v\n", action, err)
		return policyAuditRecord{}, policyExitError
	}
	from := cfg.Decision.Policy
	cfg.Decision.Policy = target
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "policy %s: resulting config does not validate: %v\n", action, err)
		return policyAuditRecord{}, policyExitError
	}
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		fmt.Fprintf(stderr, "policy %s: save config: %v\n", action, err)
		return policyAuditRecord{}, policyExitError
	}
	after, err := fileSHA256(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "policy %s: hash rewritten config: %v\n", action, err)
		return policyAuditRecord{}, policyExitError
	}
	rec := policyAuditRecord{
		Timestamp:          time.Now().UTC().Format(time.RFC3339),
		Action:             action,
		FromPolicy:         from,
		ToPolicy:           target,
		ConfigSHA256Before: before,
		ConfigSHA256After:  after,
	}
	if err := appendPolicyAudit(auditPath, rec); err != nil {
		fmt.Fprintf(stderr, "policy %s: config updated but AUDIT LOG WRITE FAILED: %v\n", action, err)
		return rec, policyExitError
	}
	return rec, policyExitOK
}

func runPolicyPromote(args []string, stdout, stderr io.Writer) int {
	fs, configPath, _ := policyFlags("policy promote", stderr)
	policyID := fs.String("policy", "", "decision policy id to promote to active")
	auditLog := fs.String("audit-log", "", "audit log path (default: <config>.policy-audit.jsonl)")
	if err := fs.Parse(args); err != nil {
		return policyExitUsage
	}
	if *policyID == "" {
		fmt.Fprintln(stderr, "usage: nexaroute policy promote --config PATH --policy ID [--audit-log PATH]")
		return policyExitUsage
	}
	if *auditLog == "" {
		*auditLog = defaultAuditLogPath(*configPath)
	}
	rec, code := applyPolicyPointer(*configPath, *auditLog, "promote", *policyID, stderr)
	if code != policyExitOK {
		return code
	}
	fmt.Fprintf(stdout, "promoted decision policy: %s -> %s (config sha256 %s, audit %s)\n",
		rec.FromPolicy, rec.ToPolicy, rec.ConfigSHA256After[:12], *auditLog)
	return policyExitOK
}

func runPolicyRollback(args []string, stdout, stderr io.Writer) int {
	fs, configPath, _ := policyFlags("policy rollback", stderr)
	auditLog := fs.String("audit-log", "", "audit log path (default: <config>.policy-audit.jsonl)")
	force := fs.Bool("force", false, "roll back even if the config changed out-of-band since the last audited action")
	if err := fs.Parse(args); err != nil {
		return policyExitUsage
	}
	if *auditLog == "" {
		*auditLog = defaultAuditLogPath(*configPath)
	}
	records, err := readPolicyAudit(*auditLog)
	if err != nil {
		fmt.Fprintf(stderr, "policy rollback: %v\n", err)
		return policyExitError
	}
	if len(records) == 0 {
		fmt.Fprintln(stderr, "policy rollback: no audited promotion to roll back")
		return policyExitError
	}
	last := records[len(records)-1]
	if !*force {
		current, err := fileSHA256(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "policy rollback: hash config: %v\n", err)
			return policyExitError
		}
		if current != last.ConfigSHA256After {
			fmt.Fprintln(stderr, "policy rollback: config changed out-of-band since the last audited action (digest mismatch); refusing without --force")
			return policyExitError
		}
	}
	rec, code := applyPolicyPointer(*configPath, *auditLog, "rollback", last.FromPolicy, stderr)
	if code != policyExitOK {
		return code
	}
	fmt.Fprintf(stdout, "rolled back decision policy: %s -> %s (config sha256 %s)\n",
		rec.FromPolicy, rec.ToPolicy, rec.ConfigSHA256After[:12])
	return policyExitOK
}

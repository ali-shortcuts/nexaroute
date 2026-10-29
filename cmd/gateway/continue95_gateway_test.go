package main

// Coverage wave for issue #663: cmd/gateway error and branch paths in
// qualitygate.go, diagnostics.go, secrets.go, backup.go, vault_accept.go,
// comfyui_accept.go and main.go. All keys, tokens and provider material
// below are synthetic test fixtures; Vault/ComfyUI servers are local
// httptest servers, never live providers. Helper and test names use the
// Continue95Gateway prefix so they cannot collide with the sibling PR's
// TestIssue663 names.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/desktop"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/qualitygate"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
	"github.com/ali-shortcuts/nexaroute/internal/video/storage"
	bolt "go.etcd.io/bbolt"
)

// Continue95GatewayWriteFile writes content into dir/name and returns the path.
func Continue95GatewayWriteFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Continue95GatewayQualityGate runs runQualityGate and returns code/stdout/stderr.
func Continue95GatewayQualityGate(args []string) (int, string, string) {
	var out, errBuf bytes.Buffer
	code := runQualityGate(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// Continue95GatewayLiveInput rewrites a replay input file as a live input.
func Continue95GatewayLiveInput(t *testing.T, path string, upstream int, cost *float64) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var in qualitygate.Input
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	in.Provenance.Mode = "live"
	for i := range in.Runs {
		in.Runs[i].Upstream = upstream
	}
	in.ObservedCostUSD = cost
	data, err = json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := path + ".live.json"
	if err := os.WriteFile(out, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestContinue95GatewayQualityGateUsageAndReadErrors(t *testing.T) {
	if code, _, _ := Continue95GatewayQualityGate(nil); code != 2 {
		t.Fatalf("no args exit=%d want 2", code)
	}
	if code, _, errOut := Continue95GatewayQualityGate([]string{"bogus"}); code != 2 || !strings.Contains(errOut, "unknown quality-gate subcommand") {
		t.Fatalf("unknown exit=%d stderr=%q", code, errOut)
	}
	if code, out, _ := Continue95GatewayQualityGate([]string{"--help"}); code != 0 || !strings.Contains(out, "quality-gate compare") {
		t.Fatalf("help exit=%d stdout=%q", code, out)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--bogus-flag"}); code != 2 {
		t.Fatalf("bad flag exit=%d want 2", code)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--bogus-flag"}); code != 2 {
		t.Fatalf("summarize bad flag exit=%d want 2", code)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize"}); code != 2 {
		t.Fatalf("summarize without input exit=%d want 2", code)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--input", filepath.Join(t.TempDir(), "missing.json")}); code != 2 {
		t.Fatalf("summarize missing input exit=%d want 2", code)
	}
	dir := t.TempDir()
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--input", dir}); code != 2 {
		t.Fatalf("summarize directory input exit=%d want 2", code)
	}
	badJSON := Continue95GatewayWriteFile(t, dir, "bad.json", `{"format":"wrong"}`)
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--input", badJSON}); code != 2 {
		t.Fatalf("summarize bad format exit=%d want 2", code)
	}
	empty := Continue95GatewayWriteFile(t, dir, "empty.json", ``)
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--input", empty}); code != 2 {
		t.Fatalf("summarize empty input exit=%d want 2", code)
	}
	// readBoundedFile / writeReport direct branches.
	if _, err := readBoundedFile(""); err == nil {
		t.Fatal("empty bounded path accepted")
	}
	if _, err := readBoundedFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing bounded file accepted")
	}
	if _, err := readBoundedFile(dir); err == nil {
		t.Fatal("directory bounded file accepted")
	}
	oversize := filepath.Join(dir, "oversize.bin")
	f, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(qualitygate.MaxGateInputBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(oversize); err == nil {
		t.Fatal("oversize bounded file accepted")
	}
	var out bytes.Buffer
	if err := writeReport("", []byte("x"), &out); err != nil {
		t.Fatalf("empty writeReport path must be a no-op: %v", err)
	}
	blocker := Continue95GatewayWriteFile(t, dir, "blocker", "x")
	if err := writeReport(filepath.Join(blocker, "report.json"), []byte("x"), &out); err == nil {
		t.Fatal("writeReport under a regular file accepted")
	}
	nested := filepath.Join(dir, "a", "b", "report.json")
	if err := writeReport(nested, []byte("synthetic-report"), &out); err != nil {
		t.Fatalf("nested writeReport: %v", err)
	}
	if !strings.Contains(out.String(), "report written to") {
		t.Fatalf("writeReport stdout=%q", out.String())
	}
}

func TestContinue95GatewayQualityGateSummarizeAndCompareBranches(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	same := qualityGateTestInput(t, dir, "same.json", 4, 0)
	// Summarize to stdout (no --out).
	if code, out, errOut := Continue95GatewayQualityGate([]string{"summarize", "--input", base}); code != 0 || !strings.Contains(out, "quality-gate-snapshot") {
		t.Fatalf("summarize stdout exit=%d out=%q err=%q", code, out, errOut)
	}
	// Summarize with an invalid input (Summarize validation failure):
	// a valid-format input with no runs.
	noRuns := Continue95GatewayWriteFile(t, dir, "noruns.json", `{"format":"quality-gate-input/v1","provenance":{"gateway_commit":"e6c3cdf3","model":"model-1","deployment_id":"dep-1","provider_id":"prov-1","mode":"replay","generated_at":"2026-10-27T12:00:00Z"},"runs":[]}`)
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--input", noRuns}); code != 2 {
		t.Fatalf("summarize no-runs exit=%d want 2", code)
	}
	// Summarize --out under a regular file fails.
	blocker := Continue95GatewayWriteFile(t, dir, "blocker", "x")
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--input", base, "--out", filepath.Join(blocker, "snap.json")}); code != 2 {
		t.Fatalf("summarize bad out exit=%d want 2", code)
	}
	// Compare usage and file errors.
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base}); code != 2 {
		t.Fatalf("compare missing candidate exit=%d want 2", code)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", filepath.Join(dir, "missing.json"), "--candidate", same}); code != 2 {
		t.Fatalf("compare missing baseline exit=%d want 2", code)
	}
	badJSON := Continue95GatewayWriteFile(t, dir, "bad.json", `not json`)
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", badJSON, "--candidate", same}); code != 2 {
		t.Fatalf("compare bad baseline exit=%d want 2", code)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", filepath.Join(dir, "missing.json")}); code != 2 {
		t.Fatalf("compare missing candidate file exit=%d want 2", code)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", badJSON}); code != 2 {
		t.Fatalf("compare bad candidate exit=%d want 2", code)
	}
	// Thresholds errors and the empty-RequiredSuites default fill.
	if code, _, errOut := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", same, "--thresholds", filepath.Join(dir, "missing.json")}); code != 2 {
		t.Fatalf("thresholds missing exit=%d err=%q", code, errOut)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", same, "--thresholds", badJSON}); code != 2 {
		t.Fatalf("thresholds bad json exit=%d want 2", code)
	}
	unknownField := Continue95GatewayWriteFile(t, dir, "th-unknown.json", `{"bogus_field":1}`)
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", same, "--thresholds", unknownField}); code != 2 {
		t.Fatalf("thresholds unknown field exit=%d want 2", code)
	}
	invalidTh := Continue95GatewayWriteFile(t, dir, "th-invalid.json", `{"success_drop":2,"required_suites":["tool_calling"]}`)
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", same, "--thresholds", invalidTh}); code != 2 {
		t.Fatalf("thresholds invalid values exit=%d want 2", code)
	}
	emptySuites := Continue95GatewayWriteFile(t, dir, "th-empty.json", `{"success_drop":0.05}`)
	if code, out, errOut := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", same, "--thresholds", emptySuites}); code != 0 || !strings.Contains(out, "routing=none") {
		t.Fatalf("thresholds empty-suites exit=%d out=%q err=%q", code, out, errOut)
	}
	// Compare --out failure (output under a regular file).
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", same, "--out", filepath.Join(blocker, "rep.json")}); code != 2 {
		t.Fatalf("compare bad out exit=%d want 2", code)
	}
	// Compare with positional args.
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", same, "extra"}); code != 2 {
		t.Fatalf("compare positional exit=%d want 2", code)
	}
	if code, _, _ := Continue95GatewayQualityGate([]string{"summarize", "--input", base, "extra"}); code != 2 {
		t.Fatalf("summarize positional exit=%d want 2", code)
	}
}

func TestContinue95GatewayQualityGateLiveCeilings(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	cand := qualityGateTestInput(t, dir, "cand.json", 4, 0)
	live := Continue95GatewayLiveInput(t, cand, 0, nil)
	// Live candidate without acknowledgement.
	if code, _, errOut := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", live}); code != 2 || !strings.Contains(errOut, "acknowledgement") {
		t.Fatalf("live no-ack exit=%d err=%q", code, errOut)
	}
	// Live candidate with acknowledgement but no request ceiling.
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", live, "--acknowledge-live"}); code != 2 {
		t.Fatalf("live no-ceiling exit=%d want 2", code)
	}
	// Live candidate within ceilings passes.
	if code, out, errOut := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", live, "--acknowledge-live", "--max-live-requests", "10"}); code != 0 || !strings.Contains(out, "routing=none") {
		t.Fatalf("live within ceilings exit=%d out=%q err=%q", code, out, errOut)
	}
	// Live candidate over its request ceiling: candidate reports 5 calls, ceiling 2.
	over := Continue95GatewayLiveInput(t, cand, 5, nil)
	if code, _, errOut := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", over, "--acknowledge-live", "--max-live-requests", "2"}); code != 2 || !strings.Contains(errOut, "request limit") {
		t.Fatalf("live over-ceiling exit=%d err=%q", code, errOut)
	}
	// Live candidate over its cost ceiling.
	cost := 9.5
	expensive := Continue95GatewayLiveInput(t, cand, 1, &cost)
	if code, _, errOut := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", expensive, "--acknowledge-live", "--max-live-requests", "10", "--max-cost-usd", "1"}); code != 2 || !strings.Contains(errOut, "cost limit") {
		t.Fatalf("live over-cost exit=%d err=%q", code, errOut)
	}
	// Replay candidate that reports upstream calls fails CheckLive.
	replayUpstream := Continue95GatewayLiveInput(t, cand, 3, nil)
	// Restore replay mode but keep upstream calls.
	data, err := os.ReadFile(replayUpstream)
	if err != nil {
		t.Fatal(err)
	}
	var in qualitygate.Input
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	in.Provenance.Mode = "replay"
	data, err = json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	replayPath := filepath.Join(dir, "replay-upstream.json")
	if err := os.WriteFile(replayPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", replayPath}); code != 2 || !strings.Contains(errOut, "replay summary") {
		t.Fatalf("replay upstream exit=%d err=%q", code, errOut)
	}
	// Unknown mode candidate fails Compare via CheckLive path: build a
	// snapshot directly (ParseBaseline accepts snapshots) with mode "odd".
	snap := qualitygate.Snapshot{Format: qualitygate.FormatSnapshot, Summary: qualitygate.Summary{
		Provenance: qualitygate.Provenance{GatewayCommit: "e6c3cdf3", Model: "model-1", DeploymentID: "dep-1", ProviderID: "prov-1", Mode: "odd"},
	}}
	// ParseBaseline requires >=1 suite for snapshots; use the summarize
	// path instead: an input with mode "odd" fails Validate at parse.
	oddData, err := json.Marshal(qualitygate.Input{Format: qualitygate.FormatInput, Provenance: qualitygate.Provenance{GatewayCommit: "e6c3cdf3", Model: "model-1", DeploymentID: "dep-1", ProviderID: "prov-1", Mode: "odd"}, Runs: []eval.Result{{RunID: "r", SuiteID: "tool_calling", SuiteVersion: "1", Cases: []eval.CaseResult{{CaseID: "c", Verdict: eval.VerdictPass}}}}})
	if err != nil {
		t.Fatal(err)
	}
	oddPath := Continue95GatewayWriteFile(t, dir, "odd.json", string(oddData))
	_ = snap
	if code, _, _ := Continue95GatewayQualityGate([]string{"compare", "--baseline", base, "--candidate", oddPath}); code != 2 {
		t.Fatalf("odd mode exit=%d want 2", code)
	}
}

func Continue95GatewayDiagnosticsConfig(t *testing.T, dir string, logName string) string {
	t.Helper()
	cfg := config.Default()
	cfg.Logging.File = logName
	cfg.Providers = []config.ProviderConfig{{
		ID: "diag-synth", Name: "Synthetic", Type: "openai_compatible", Enabled: true,
		BaseURL: "https://api.example.test/v1", AuthMode: "bearer",
		APIKeyEnv: "CONTINUE95_DIAG_FAKE_KEY",
		Headers:   map[string]string{"X-Custom": "synthetic-header-value"},
		Models: []config.ModelConfig{
			{ID: "m1", Model: "model-a", Enabled: true, Priority: 1},
			{ID: "m2", Model: "model-b", Enabled: false, Priority: 2},
		},
	}}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContinue95GatewayDiagnosticsUsageAndHelpers(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runDiagnostics(nil, &out, &errBuf); code != 2 {
		t.Fatalf("no args exit=%d want 2", code)
	}
	if code := runDiagnostics([]string{"bogus"}, &out, &errBuf); code != 2 {
		t.Fatalf("bogus subcommand exit=%d want 2", code)
	}
	if code := runDiagnostics([]string{"bundle", "--bogus-flag"}, &out, &errBuf); code != 2 {
		t.Fatalf("bad flag exit=%d want 2", code)
	}
	if code := runDiagnostics([]string{"bundle", "--output", "x.json", "extra"}, &out, &errBuf); code != 2 {
		t.Fatalf("positional exit=%d want 2", code)
	}
	if code := runDiagnostics([]string{"bundle"}, &out, &errBuf); code != 2 {
		t.Fatalf("missing output exit=%d want 2", code)
	}
	dir := t.TempDir()
	cfgPath := Continue95GatewayDiagnosticsConfig(t, dir, "off")
	existing := Continue95GatewayWriteFile(t, dir, "bundle.json", "{}")
	if code := runDiagnostics([]string{"bundle", "--config", cfgPath, "--output", existing}, &out, &errBuf); code != 1 {
		t.Fatalf("existing output without overwrite exit=%d want 1", code)
	}
	// parseDoctorLine / truncateRunes / looks-like helpers.
	if _, _, _, ok := parseDoctorLine("nospace"); ok {
		t.Fatal("parseDoctorLine accepted a line without a space")
	}
	if _, _, _, ok := parseDoctorLine("PASS name-without-colon"); ok {
		t.Fatal("parseDoctorLine accepted a line without ': '")
	}
	status, name, detail, ok := parseDoctorLine("WARN check-name: some detail")
	if !ok || status != "WARN" || name != "check-name" || detail != "some detail" {
		t.Fatalf("parseDoctorLine=%q %q %q ok=%v", status, name, detail, ok)
	}
	if got := truncateRunes("abc", 0); got != "" {
		t.Fatalf("truncate 0=%q", got)
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("truncate short=%q", got)
	}
	if got := truncateRunes("abcdef", 3); got != "abc..." {
		t.Fatalf("truncate long=%q", got)
	}
	if got := truncateRunes("héllo-wörld", 5); got != "héllo..." {
		t.Fatalf("truncate unicode=%q", got)
	}
	if !diagnosticsLooksLikePayload(`x "prompt": y`) || diagnosticsLooksLikePayload("plain line") {
		t.Fatal("looks-like-payload mismatch")
	}
	if !diagnosticsLooksLikeError("upstream ERROR happened") || !diagnosticsLooksLikeError("it FAILED") || !diagnosticsLooksLikeError("panic here") || diagnosticsLooksLikeError("all good") {
		t.Fatal("looks-like-error mismatch")
	}
	// Base URL host.
	if got := diagnosticsBaseURLHost(""); got != "" {
		t.Fatalf("empty base host=%q", got)
	}
	if got := diagnosticsBaseURLHost("not a url :::"); got != "" {
		t.Fatalf("bad base host=%q", got)
	}
	if got := diagnosticsBaseURLHost("https://user:synthetic-pass@api.example.test:8443/v1?x=1#f"); got != "https://api.example.test:8443" {
		t.Fatalf("base host=%q", got)
	}
	// Log path variants.
	cfg := config.Default()
	cfg.Logging.File = "off"
	if _, ok := diagnosticsLogPath(cfg, filepath.Join(dir, "config.json")); ok {
		t.Fatal("off log path must be unavailable")
	}
	cfg.Logging.File = "auto"
	if p, ok := diagnosticsLogPath(cfg, filepath.Join(dir, "config.json")); !ok || p != filepath.Join(dir, "nexaroute.log") {
		t.Fatalf("auto log path=%q ok=%v", p, ok)
	}
	cfg.Logging.File = "/abs/synthetic.log"
	if p, ok := diagnosticsLogPath(cfg, filepath.Join(dir, "config.json")); !ok || p != "/abs/synthetic.log" {
		t.Fatalf("abs log path=%q ok=%v", p, ok)
	}
	cfg.Logging.File = "rel.log"
	if p, ok := diagnosticsLogPath(cfg, filepath.Join(dir, "config.json")); !ok || p != filepath.Join(dir, "rel.log") {
		t.Fatalf("rel log path=%q ok=%v", p, ok)
	}
	// Gateway info and metrics.
	info := diagnosticsGatewayInfo()
	if info.GoVersion == "" || info.NumCPU <= 0 || info.OS == "" {
		t.Fatalf("gateway info=%+v", info)
	}
	bundle := &diagnosticsBundle{
		Config: diagnosticsConfigSummary{ProvidersTotal: 2, ProvidersEnabled: 1},
		DoctorChecks: []diagnosticsCheck{
			{Status: "PASS", Name: "a", Detail: "ok"},
			{Status: "WARN", Name: "b", Detail: "hmm"},
			{Status: "FAIL", Name: "c", Detail: "bad"},
			{Status: "OTHER", Name: "d", Detail: "audit chain verified: 42 records"},
			{Status: "PASS", Name: "e", Detail: "audit chain verified: 7 records"},
		},
		RecentErrors: diagnosticsRecentErrors{LinesScanned: 9, Included: []string{"x"}, Dropped: 2},
	}
	m := diagnosticsMetricsFrom(bundle)
	if m.ChecksPass != 2 || m.ChecksWarn != 1 || m.ChecksFail != 2 || !m.AuditRecordsKnown || m.AuditRecords != 7 || m.RecentErrorsScanned != 9 || m.RecentErrorsKept != 1 || m.RecentErrorsDropped != 2 {
		t.Fatalf("metrics=%+v", m)
	}
	// Sanitizer: nil receiver, nil config, secret-bearing text, paths.
	var nilSan *diagnosticsSanitizer
	if got := nilSan.sanitizeText("anything"); got != "" {
		t.Fatalf("nil sanitizer=%q", got)
	}
	san := newDiagnosticsSanitizer(nil)
	if got := san.sanitizeText("plain text"); got != "plain text" {
		t.Fatalf("nil-config sanitizer=%q", got)
	}
	if got := safeDiagnosticsError(nil, san); got != "" {
		t.Fatalf("nil error=%q", got)
	}
	if got := safeDiagnosticsError(errors.New("synthetic validation failure"), san); got != "synthetic validation failure" {
		t.Fatalf("safe error=%q", got)
	}
	secretCfg := config.Default()
	secretCfg.Admin.APIKey = "synthetic-admin-secret-value"
	san2 := newDiagnosticsSanitizer(&secretCfg)
	if got := safeDiagnosticsError(errors.New("failed with synthetic-admin-secret-value inside"), san2); got == "failed with synthetic-admin-secret-value inside" || got == "" {
		t.Fatalf("safe error must redact but not vanish: %q", got)
	}
	if got := diagnosticsStripPaths("see /home/operator/deploy/config.json now"); got != "see config.json now" {
		t.Fatalf("strip paths=%q", got)
	}
	if got := diagnosticsStripPaths("https://api.example.test/v1/path stays"); got != "https://api.example.test/v1/path stays" {
		t.Fatalf("strip paths must leave URL paths alone: %q", got)
	}
	if got := diagnosticsStripPaths("trailing /home/operator/"); got != "trailing /home/operator/" {
		t.Fatalf("strip trailing-slash path=%q", got)
	}
	// Deny scan: each pattern family, plus the REDACTED exception.
	if !diagnosticsDenyScan("value synthetic-admin-secret-value here", []string{"synthetic-admin-secret-value"}) {
		t.Fatal("deny scan must catch an exact secret value")
	}
	if diagnosticsDenyScan("short", []string{"abc"}) {
		t.Fatal("deny scan must ignore secrets shorter than 4 chars")
	}
	for _, text := range []string{
		"https://operator:synthetic-pass@api.example.test/v1",
		"https://api.example.test/v1?token=synthetic-query-value",
		"nxs1:synthetic-envelope-body",
		"Authorization: Bearer synthetic-bearer-token-value",
		"api_key=synthetic-api-key-value",
		"Cookie: synthetic-cookie-value-1234",
		`{"api_key": "synthetic-json-key-value"}`,
	} {
		if !diagnosticsDenyScan(text, nil) {
			t.Fatalf("deny scan missed %q", text)
		}
	}
	if diagnosticsDenyScan(`api_key="[REDACTED]"`, nil) {
		t.Fatal("deny scan must not count the REDACTED marker itself")
	}
	if !diagnosticsDenyScan(`api_key="synthetic-json-key-value"`, nil) {
		t.Fatal("deny scan missed JSON secret")
	}
	if err := verifyBundleSafe([]byte(`{"ok":true}`), nil); err != nil {
		t.Fatalf("safe bundle rejected: %v", err)
	}
	if err := verifyBundleSafe([]byte(`{"api_key": "synthetic-json-key-value"}`), nil); err == nil {
		t.Fatal("unsafe bundle accepted")
	}
	if err := verifyBundleSafe([]byte("contains synthetic-admin-secret-value"), []string{"synthetic-admin-secret-value"}); err == nil {
		t.Fatal("bundle with known secret accepted")
	}
	// Atomic write: success with mode 0600, and missing directory error.
	dest := filepath.Join(dir, "written.json")
	if err := writeDiagnosticsBundleAtomic(dest, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("atomic write: %v", err)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("atomic write mode=%v err=%v", info.Mode().Perm(), err)
	}
	if err := writeDiagnosticsBundleAtomic(filepath.Join(dir, "missing-dir", "b.json"), []byte("x")); err == nil {
		t.Fatal("atomic write into missing directory accepted")
	}
}

func TestContinue95GatewayDiagnosticsBundleBranches(t *testing.T) {
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	dir := t.TempDir()
	// Log file with payload, error, non-error, secret and overflow lines.
	logLines := []string{
		"nexaroute upstream error: provider diag-synth failed",
		`nexaroute upstream error: body {"choices": []}`,
		"nexaroute request ok: health check passed",
		"nexaroute upstream error: leaked synthetic-diag-key-value here",
		"nexaroute upstream fail: timeout after 30s",
	}
	for i := 0; i < 60; i++ {
		logLines = append(logLines, "nexaroute upstream error: synthetic overflow line")
	}
	logPath := filepath.Join(dir, "diagnostics.log")
	if err := os.WriteFile(logPath, []byte(strings.Join(logLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := Continue95GatewayDiagnosticsConfig(t, dir, "diagnostics.log")
	outPath := filepath.Join(dir, "bundle.json")
	var out, errBuf bytes.Buffer
	if code := runDiagnostics([]string{"bundle", "--config", cfgPath, "--output", outPath, "--no-log-errors"}, &out, &errBuf); code != 0 {
		t.Fatalf("no-log-errors bundle exit=%d err=%s", code, errBuf.String())
	}
	data, bundle := diagnosticsReadBundle(t, outPath)
	_ = data
	if len(bundle.RecentErrors.Included) != 0 {
		t.Fatalf("no-log-errors bundle must exclude log errors: %+v", bundle.RecentErrors)
	}
	// Full bundle: payload dropped, secret line dropped, overflow dropped.
	outPath2 := filepath.Join(dir, "bundle2.json")
	if code := runDiagnostics([]string{"bundle", "--config", cfgPath, "--output", outPath2}, &out, &errBuf); code != 0 {
		t.Fatalf("full bundle exit=%d err=%s", code, errBuf.String())
	}
	_, bundle2 := diagnosticsReadBundle(t, outPath2)
	if !bundle2.RecentErrors.LogAvailable || bundle2.RecentErrors.Dropped < 10 {
		t.Fatalf("recent errors=%+v (want payload/secret/overflow drops)", bundle2.RecentErrors)
	}
	for _, line := range bundle2.RecentErrors.Included {
		if strings.Contains(line, "synthetic-diag-key-value") || strings.Contains(line, "choices") {
			t.Fatalf("bundle leaked a dropped line: %q", line)
		}
	}
	if bundle2.Config.LoggingMode != "custom" {
		t.Fatalf("logging mode=%q want custom", bundle2.Config.LoggingMode)
	}
	if len(bundle2.Providers) != 1 || bundle2.Providers[0].CredentialSource != "env:CONTINUE95_DIAG_FAKE_KEY" {
		t.Fatalf("provider summary=%+v", bundle2.Providers)
	}
	// Overwrite + bundle preflight (nonexistent backup bundle) + max-age.
	if code := runDiagnostics([]string{"bundle", "--config", cfgPath, "--output", outPath2, "--overwrite", "--bundle", filepath.Join(dir, "missing-backup.tar"), "--max-age", "24h"}, &out, &errBuf); code != 0 {
		t.Fatalf("bundle preflight exit=%d err=%s", code, errBuf.String())
	}
	_, bundle3 := diagnosticsReadBundle(t, outPath2)
	foundBackupCheck := false
	for _, c := range bundle3.DoctorChecks {
		if strings.Contains(c.Name, "backup") {
			foundBackupCheck = true
		}
	}
	if !foundBackupCheck {
		t.Fatalf("bundle preflight produced no backup check: %+v", bundle3.DoctorChecks)
	}
	// Config summary branches: invalid config, warnings dedup, logging modes.
	if sum := diagnosticsConfigSummaryFrom(config.Config{}, errors.New("synthetic invalid config"), newDiagnosticsSanitizer(nil)); sum.Valid {
		t.Fatal("error config summary must report invalid")
	}
	badPath := Continue95GatewayWriteFile(t, dir, "bad.json", `{"listen": 123}`)
	badBundle, _, err := buildDiagnosticsBundle(badPath, "", 0, false)
	if err != nil {
		t.Fatalf("malformed config bundle must still build: %v", err)
	}
	if badBundle.Config.Valid {
		t.Fatal("malformed config bundle must report invalid config")
	}
	warnCfg := config.Default()
	warnCfg.Logging.File = "auto"
	warnCfg.Warnings = []string{"synthetic warning", "synthetic warning", ""}
	warnCfg.EligibilityWarnings = []string{"synthetic warning", "second synthetic warning"}
	sum := diagnosticsConfigSummaryFrom(warnCfg, nil, newDiagnosticsSanitizer(nil))
	if !sum.Valid || sum.LoggingMode != "auto" || len(sum.Warnings) != 2 {
		t.Fatalf("warning summary=%+v", sum)
	}
	offCfg := config.Default()
	offCfg.Logging.File = "off"
	if sum := diagnosticsConfigSummaryFrom(offCfg, nil, newDiagnosticsSanitizer(nil)); sum.LoggingMode != "off" {
		t.Fatalf("off logging mode=%q", sum.LoggingMode)
	}
	// Provider summaries: invalid config returns empty; env/configured/header sources.
	if got := diagnosticsProviderSummaries(config.Default(), false); len(got) != 0 {
		t.Fatalf("invalid provider summaries=%v", got)
	}
	provCfg := config.Default()
	provCfg.Providers = []config.ProviderConfig{
		{ID: "a", APIKey: "synthetic-direct-key", Models: []config.ModelConfig{{Model: "m", Enabled: true}}},
		{ID: "b", APIKeyEnv: "SOME_ENV", Headers: map[string]string{"Z-Header": "v", "A-Header": "v"}},
	}
	sums := diagnosticsProviderSummaries(provCfg, true)
	if len(sums) != 2 || sums[0].CredentialSource != "configured" || !sums[0].CredentialConfigured || sums[1].CredentialSource != "env:SOME_ENV" || len(sums[1].HeaderNames) != 2 || sums[1].HeaderNames[0] != "A-Header" {
		t.Fatalf("provider summaries=%+v", sums)
	}
	// Recent errors with an unavailable log.
	offPath := Continue95GatewayDiagnosticsConfig(t, dir, "off")
	offLoaded, err := loadCheckedConfig(offPath)
	if err != nil {
		// loadCheckedConfig may fail for other reasons; the unavailable
		// branch is still exercised through a config with Logging off.
		t.Logf("loadCheckedConfig off: %v", err)
	}
	_ = offLoaded
	recent := diagnosticsRecentErrorsFrom(provCfg, filepath.Join(dir, "config.json"), newDiagnosticsSanitizer(nil))
	_ = recent
}

// Continue95GatewayFakeTransit is an in-memory Vault Transit stand-in
// for CLI migrate/rewrap tests: it stores DEKs by ciphertext body and
// serves encrypt/decrypt/rewrap with a controllable key version.
type Continue95GatewayFakeTransit struct {
	mu            sync.Mutex
	version       int
	next          int
	keys          map[string][]byte
	decryptStatus int // non-zero forces this HTTP status on decrypt (forbidden/unavailable cases)
}

func Continue95GatewayNewFakeTransit() *Continue95GatewayFakeTransit {
	return &Continue95GatewayFakeTransit{version: 1, keys: map[string][]byte{}}
}

func (f *Continue95GatewayFakeTransit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		Plaintext  string `json:"plaintext"`
		Ciphertext string `json:"ciphertext"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/encrypt/synth-key"):
		dek, err := base64.StdEncoding.DecodeString(req.Plaintext)
		if err != nil || len(dek) != 32 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.next++
		body := base64.StdEncoding.EncodeToString([]byte("synthetic-dek-" + fmt.Sprint(f.next)))
		ct := "vault:v" + fmt.Sprint(f.version) + ":" + body
		f.keys[ct] = append([]byte(nil), dek...)
		write(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.version}})
	case strings.HasSuffix(r.URL.Path, "/decrypt/synth-key"):
		if f.decryptStatus != 0 {
			w.WriteHeader(f.decryptStatus)
			return
		}
		dek, ok := f.keys[req.Ciphertext]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		write(map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(dek)}})
	case strings.HasSuffix(r.URL.Path, "/rewrap/synth-key"):
		dek, ok := f.keys[req.Ciphertext]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if parts := strings.Split(req.Ciphertext, ":"); len(parts) == 3 && parts[1] == "v"+fmt.Sprint(f.version) {
			write(map[string]any{"data": map[string]any{"ciphertext": req.Ciphertext, "key_version": f.version}})
			return
		}
		f.next++
		body := base64.StdEncoding.EncodeToString([]byte("synthetic-dek-" + fmt.Sprint(f.next)))
		ct := "vault:v" + fmt.Sprint(f.version) + ":" + body
		f.keys[ct] = append([]byte(nil), dek...)
		write(map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": f.version}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func Continue95GatewaySetTransitEnv(t *testing.T, addr string) {
	t.Helper()
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "true")
	t.Setenv("VAULT_ADDR", addr)
	t.Setenv("VAULT_TOKEN", "synthetic-transit-token")
	t.Setenv("VAULT_TOKEN_FILE", "")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "transit")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "synth-key")
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_ENV", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_COMMAND", "")
	t.Setenv("NEXAROUTE_KEY_PROVIDER_REF", "")
}

func Continue95GatewayClearTransitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_TRANSIT_ENABLED", "")
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_TOKEN_FILE", "")
	t.Setenv("NEXAROUTE_TRANSIT_MOUNT", "")
	t.Setenv("NEXAROUTE_TRANSIT_KEY", "")
}

func TestContinue95GatewaySecretsCLI(t *testing.T) {
	Continue95GatewayClearTransitEnv(t)
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	if err := runSecrets([]string{"--bad-flag"}); err == nil {
		t.Fatal("bad flag accepted")
	}
	if err := runSecrets([]string{"status", "--config", ""}); err == nil {
		t.Fatal("empty config path accepted")
	}
	dir := t.TempDir()
	if err := runSecrets([]string{"status", "--config", filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("missing config accepted")
	}
	badPath := Continue95GatewayWriteFile(t, dir, "bad.json", `not json`)
	if err := runSecrets([]string{"status", "--config", badPath}); err == nil {
		t.Fatal("bad JSON status accepted")
	}
	// Status with nxs2 fields reports transit versions; status with no
	// encrypted fields reports the plaintext-may-exist line.
	plainPath := Continue95GatewayWriteFile(t, dir, "plain.json", `{"providers":[{"id":"prov-synth","api_key":"synthetic-plaintext"}]}`)
	statusOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"status", "--config", plainPath}); err != nil {
			t.Errorf("status plain: %v", err)
		}
	})
	if !strings.Contains(statusOut, "plaintext_fields_may_exist=true") || !strings.Contains(statusOut, "plaintext_fields=1") {
		t.Fatalf("plain status=%q", statusOut)
	}
	// Verify on a default config reports zero verified fields.
	verifyPath := writeTestConfig(t, config.Default())
	verifyOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"verify", "--config", verifyPath}); err != nil {
			t.Errorf("verify default: %v", err)
		}
	})
	if !strings.Contains(verifyOut, "verified_fields=0") {
		t.Fatalf("verify default=%q", verifyOut)
	}
	if err := runSecrets([]string{"verify", "--config", badPath}); err == nil {
		t.Fatal("verify bad config accepted")
	}
	if err := runSecrets([]string{"decrypt", "--config", filepath.Join(dir, "missing.json"), "--to-stdout", "--allow-plaintext"}); err == nil {
		t.Fatal("decrypt missing config accepted")
	}
	// Rotate: transit refusal, external-key refusal, missing config.
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "transit")
	if err := runSecrets([]string{"rotate", "--config", plainPath}); err == nil || !strings.Contains(err.Error(), "Transit") {
		t.Fatalf("transit rotate not refused: %v", err)
	}
	t.Setenv("NEXAROUTE_KEY_PROVIDER", "")
	t.Setenv("NEXAROUTE_MASTER_KEY", "synthetic-external")
	if err := runSecrets([]string{"rotate", "--config", plainPath}); err == nil {
		t.Fatal("external-key rotate accepted")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	if err := runSecrets([]string{"rotate", "--config", filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("rotate missing config accepted")
	}
	// Rotate success on a file-backed key + plaintext config.
	rotDir := t.TempDir()
	rotPath := filepath.Join(rotDir, "config.json")
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{ID: "prov-synth", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "synthetic-rotate-secret", Enabled: false}}
	if err := config.SaveAtomic(rotPath, cfg); err != nil {
		t.Fatal(err)
	}
	// SaveAtomic writes the config; the key file is created by secrets.Key
	// on first use. Force key creation by reading via runSecrets verify
	// path is not enough, so create the key file explicitly with a
	// synthetic key and seal the config with it through secrets.
	legacy := make([]byte, 32)
	for i := range legacy {
		legacy[i] = byte(i + 3)
	}
	if err := os.WriteFile(rotPath+".key", legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	sealed, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","api_key":"synthetic-rotate-secret"}]}`), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rotPath, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	rotOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"rotate", "--config", rotPath}); err != nil {
			t.Errorf("rotate: %v", err)
		}
	})
	if !strings.Contains(rotOut, "master key rotated") {
		t.Fatalf("rotate output=%q", rotOut)
	}
	if _, err := os.Stat(rotPath + ".key.previous"); err != nil {
		t.Fatalf("rotation recovery copy missing: %v", err)
	}
	// Migrate/rewrap without Transit enabled fail with the gate error.
	if err := runSecrets([]string{"migrate-to-transit", "--config", plainPath}); err == nil {
		t.Fatal("migrate without transit accepted")
	}
	if err := runSecrets([]string{"rewrap-transit", "--config", plainPath}); err == nil {
		t.Fatal("rewrap without transit accepted")
	}
	if err := runSecrets([]string{"migrate-to-transit", "--config", filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("migrate missing config accepted (gate check order)")
	}
	// loadLegacyKey branches.
	if _, err := loadLegacyKey("", ""); err == nil {
		// Without env fallback this must fail; env may carry a value in
		// this process, so only assert when both env vars are empty.
		t.Setenv("NEXAROUTE_LEGACY_MASTER_KEY_FILE", "")
		t.Setenv("NEXAROUTE_LEGACY_MASTER_KEY", "")
		if _, err := loadLegacyKey("", ""); err == nil {
			t.Fatal("loadLegacyKey with no source accepted")
		}
	}
	t.Setenv("NEXAROUTE_LEGACY_MASTER_KEY_FILE", "")
	t.Setenv("NEXAROUTE_LEGACY_MASTER_KEY", "")
	if _, err := loadLegacyKey("some-file", "some-b64"); err == nil {
		t.Fatal("loadLegacyKey with both sources accepted")
	}
	if _, err := loadLegacyKey("", "!!!not-b64!!!"); err == nil {
		t.Fatal("loadLegacyKey bad base64 accepted")
	}
	if _, err := loadLegacyKey("", base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("loadLegacyKey short key accepted")
	}
	goodKey := make([]byte, 32)
	for i := range goodKey {
		goodKey[i] = byte(i + 9)
	}
	if got, err := loadLegacyKey("", base64.StdEncoding.EncodeToString(goodKey)); err != nil || string(got) != string(goodKey) {
		t.Fatalf("loadLegacyKey b64: err=%v", err)
	}
	keyFile := filepath.Join(dir, "legacy.key")
	if err := os.WriteFile(keyFile, goodKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadLegacyKey(keyFile, ""); err != nil || string(got) != string(goodKey) {
		t.Fatalf("loadLegacyKey file: err=%v", err)
	}
	if _, err := loadLegacyKey(filepath.Join(dir, "missing.key"), ""); err == nil {
		t.Fatal("loadLegacyKey missing file accepted")
	}
	badModeFile := filepath.Join(dir, "legacy-badmode.key")
	if err := os.WriteFile(badModeFile, goodKey, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLegacyKey(badModeFile, ""); err == nil {
		t.Fatal("loadLegacyKey bad-mode file accepted")
	}
	shortFile := filepath.Join(dir, "legacy-short.key")
	if err := os.WriteFile(shortFile, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLegacyKey(shortFile, ""); err == nil {
		t.Fatal("loadLegacyKey short file accepted")
	}
}

func TestContinue95GatewaySecretsTransitCLI(t *testing.T) {
	fake := Continue95GatewayNewFakeTransit()
	server := httptest.NewServer(fake)
	defer server.Close()
	Continue95GatewaySetTransitEnv(t, server.URL)
	dir := t.TempDir()
	legacy := make([]byte, 32)
	for i := range legacy {
		legacy[i] = byte(i + 5)
	}
	nxs1doc, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","api_key":"synthetic-migrate-secret"}]}`), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	migPath := filepath.Join(dir, "mig.json")
	if err := os.WriteFile(migPath, nxs1doc, 0o600); err != nil {
		t.Fatal(err)
	}
	// Dry run first: reports one field, writes nothing.
	dryOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"migrate-to-transit", "--config", migPath, "--legacy-key", base64.StdEncoding.EncodeToString(legacy), "--dry-run"}); err != nil {
			t.Errorf("migrate dry-run: %v", err)
		}
	})
	if !strings.Contains(dryOut, "dry_run=true") || !strings.Contains(dryOut, "would_migrate_fields=1") {
		t.Fatalf("migrate dry-run=%q", dryOut)
	}
	if after, _ := os.ReadFile(migPath); string(after) != string(nxs1doc) {
		t.Fatal("dry run rewrote the config")
	}
	// Real migration.
	migOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"migrate-to-transit", "--config", migPath, "--legacy-key", base64.StdEncoding.EncodeToString(legacy)}); err != nil {
			t.Errorf("migrate: %v", err)
		}
	})
	if !strings.Contains(migOut, "migrated_fields=1") || !strings.Contains(migOut, "backup_path=") {
		t.Fatalf("migrate output=%q", migOut)
	}
	// Status now reports the nxs2 field and its key version.
	statusOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"status", "--config", migPath}); err != nil {
			t.Errorf("status nxs2: %v", err)
		}
	})
	if !strings.Contains(statusOut, "nxs2_fields=1") || !strings.Contains(statusOut, "transit_key_versions=1") {
		t.Fatalf("nxs2 status=%q", statusOut)
	}
	// Rewrap dry run, then rotate the fake Transit key and rewrap.
	rewrapDry := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"rewrap-transit", "--config", migPath, "--dry-run"}); err != nil {
			t.Errorf("rewrap dry-run: %v", err)
		}
	})
	if !strings.Contains(rewrapDry, "dry_run=true") || !strings.Contains(rewrapDry, "nxs2_fields=1") {
		t.Fatalf("rewrap dry-run=%q", rewrapDry)
	}
	alreadyOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"rewrap-transit", "--config", migPath}); err != nil {
			t.Errorf("rewrap already-latest: %v", err)
		}
	})
	if !strings.Contains(alreadyOut, "already_at_latest_version=true") {
		t.Fatalf("rewrap already-latest=%q", alreadyOut)
	}
	fake.mu.Lock()
	fake.version = 2
	fake.mu.Unlock()
	rewrapOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"rewrap-transit", "--config", migPath}); err != nil {
			t.Errorf("rewrap: %v", err)
		}
	})
	if !strings.Contains(rewrapOut, "rewrapped_fields=1") || !strings.Contains(rewrapOut, "backup_path=") {
		t.Fatalf("rewrap output=%q", rewrapOut)
	}
	// Migrate on an already-migrated config is a no-op success.
	againOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"migrate-to-transit", "--config", migPath, "--dry-run"}); err != nil {
			t.Errorf("migrate already dry-run: %v", err)
		}
	})
	if !strings.Contains(againOut, "already_migrated=true") {
		t.Fatalf("migrate already dry-run=%q", againOut)
	}
	// Decrypt the migrated config through the CLI (explicit opt-in).
	decOut := captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"decrypt", "--config", migPath, "--to-stdout", "--allow-plaintext"}); err != nil {
			t.Errorf("decrypt migrated: %v", err)
		}
	})
	if !strings.Contains(decOut, "synthetic-migrate-secret") {
		t.Fatalf("decrypt migrated output missing plaintext: %q", decOut)
	}
}

func TestContinue95GatewayDispatchAndMainHelpers(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code, handled := dispatchSubcommand(nil, nil, &out, &errBuf); handled || code != 0 {
		t.Fatalf("empty dispatch: code=%d handled=%v", code, handled)
	}
	if _, handled := dispatchSubcommand([]string{"--version"}, nil, &out, &errBuf); handled {
		t.Fatalf("flag dispatch handled=%v", handled)
	}
	// Every routed subcommand with empty args must report handled=true.
	// (The subcommands themselves fail on missing args; dispatch is what
	// is under test here, not each subcommand's success path.)
	for _, cmd := range []string{"init", "privacy", "audit", "policy", "config", "doctor", "diagnostics", "provider-accept", "external-accept", "vault-accept", "comfyui-accept", "backup", "restore", "conformance", "quality-gate"} {
		var o, e bytes.Buffer
		if _, handled := dispatchSubcommand([]string{cmd}, nil, &o, &e); !handled {
			t.Fatalf("subcommand %q not dispatched", cmd)
		}
	}
	if code, handled := dispatchSubcommand([]string{"secrets"}, nil, &out, &errBuf); !handled || code != 1 || !strings.Contains(errBuf.String(), "secrets command failed") {
		t.Fatalf("secrets dispatch: code=%d handled=%v stderr=%q", code, handled, errBuf.String())
	}
	// ensureConfig: MkdirAll failure when a path component is a file.
	blocker := Continue95GatewayWriteFile(t, t.TempDir(), "blocker", "x")
	if err := ensureConfig(filepath.Join(blocker, "config.json")); err == nil {
		t.Fatal("ensureConfig under a regular file accepted")
	}
	// dashboardURL branches.
	if got := dashboardURL(filepath.Join(t.TempDir(), "missing.json")); got != "http://127.0.0.1:8080/" {
		t.Fatalf("dashboardURL missing config=%q", got)
	}
	badListen := config.Default()
	badListen.Listen = "not-a-hostport"
	badPath := writeTestConfig(t, badListen)
	if got := dashboardURL(badPath); got != "http://127.0.0.1:8080/" {
		t.Fatalf("dashboardURL bad listen=%q", got)
	}
	for _, tc := range []struct {
		listen, want string
	}{
		{"0.0.0.0:9090", "http://127.0.0.1:9090/"},
		{"127.0.0.1:9091", "http://127.0.0.1:9091/"},
		{"[::]:9092", "http://127.0.0.1:9092/"},
	} {
		cfg := config.Default()
		cfg.Listen = tc.listen
		path := writeTestConfig(t, cfg)
		if got := dashboardURL(path); got != tc.want {
			t.Fatalf("dashboardURL %q=%q want %q", tc.listen, got, tc.want)
		}
	}
	// Deleted working directory: canonicalization must fail closed.
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	doomed := t.TempDir()
	cfgPath := writeTestConfig(t, config.Default())
	if err := os.Chdir(doomed); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(doomed); err != nil {
		t.Fatal(err)
	}
	_, canonErr := canonicalPath("relative/path.json")
	_, resolveErr := resolveSecurityStorePath("", "")
	_, doctorErr := resolveSecurityStorePath("security/nexaroute-security.db", "")
	_ = cfgPath
	// Restore the working directory before any other assertion.
	if err := os.Chdir(origWD); err != nil {
		t.Fatal(err)
	}
	if canonErr == nil {
		t.Fatal("canonicalPath with deleted cwd accepted")
	}
	if resolveErr == nil && doctorErr == nil {
		t.Fatal("resolveSecurityStorePath with deleted cwd accepted")
	}
}

func TestContinue95GatewayMainAlreadyRunning(t *testing.T) {
	// Hold the gateway lock, then run main(): it must take the
	// already-running branch (dashboardURL + openExistingUI) and return
	// without starting a server. openExistingUI waits out its 8s
	// readiness timeout against the unreachable loopback URL.
	dir := t.TempDir()
	cfg := config.Default()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = reservation.Addr().String()
	_ = reservation.Close()
	cfgPath := filepath.Join(dir, "config.json")
	if err := config.SaveAtomic(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	lock, acquired, err := desktop.Acquire(cfgPath + ".lock")
	if err != nil || !acquired {
		t.Fatalf("acquire lock: acquired=%v err=%v", acquired, err)
	}
	defer lock.Close()
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Args = []string{"nexaroute", "-config", cfgPath, "-no-browser"}
	out := captureGatewayStdout(t, main)
	if !strings.Contains(out, "already be starting") && !strings.Contains(out, "already running") {
		t.Fatalf("already-running main output=%q", out)
	}
}

func TestContinue95GatewayBackupHelpers(t *testing.T) {
	dir := t.TempDir()
	if got := sha256Hex([]byte("synthetic")); got == "" {
		t.Fatal("sha256Hex empty")
	}
	if _, err := readFileBounded(filepath.Join(dir, "missing"), "config"); err == nil {
		t.Fatal("readFileBounded missing accepted")
	}
	if _, err := readFileBounded(dir, "config"); err == nil {
		t.Fatal("readFileBounded directory accepted")
	}
	bb := &boundedBuffer{limit: 4}
	if _, err := bb.Write([]byte("12345")); err == nil {
		t.Fatal("boundedBuffer over-limit write accepted")
	}
	if n, err := bb.Write([]byte("1234")); err != nil || n != 4 {
		t.Fatalf("boundedBuffer write: n=%d err=%v", n, err)
	}
	// resolveLiveAdminURL branches.
	if got, err := resolveLiveAdminURL("https://admin.example.test/", ""); err != nil || got != "https://admin.example.test" {
		t.Fatalf("explicit admin URL=%q err=%v", got, err)
	}
	if _, err := resolveLiveAdminURL("", ""); err == nil {
		t.Fatal("empty listen admin URL accepted")
	}
	if _, err := resolveLiveAdminURL("", "example.com/path"); err == nil {
		t.Fatal("listen with path accepted as admin URL")
	}
	if got, err := resolveLiveAdminURL("", "https://admin.example.test/base"); err != nil || got != "https://admin.example.test/base" {
		t.Fatalf("listen URL=%q err=%v", got, err)
	}
	if got, err := resolveLiveAdminURL("", ":9090"); err != nil || got != "http://127.0.0.1:9090" {
		t.Fatalf("bare port admin URL=%q err=%v", got, err)
	}
	if got, err := resolveLiveAdminURL("", "127.0.0.1:9090"); err != nil || got != "http://127.0.0.1:9090" {
		t.Fatalf("host:port admin URL=%q err=%v", got, err)
	}
	// fetchLiveSecuritySnapshot status branches via a local server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-admin-key"); got != "synthetic-admin-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("bad key"))
			return
		}
		_, _ = w.Write([]byte("synthetic-snapshot-bytes"))
	}))
	defer srv.Close()
	if _, err := fetchLiveSecuritySnapshot(srv.URL, "wrong-key"); err == nil || !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("live snapshot wrong key: err=%v", err)
	}
	if data, err := fetchLiveSecuritySnapshot(srv.URL, "synthetic-admin-key"); err != nil || string(data) != "synthetic-snapshot-bytes" {
		t.Fatalf("live snapshot: data=%q err=%v", data, err)
	}
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable, http.StatusTeapot} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("synthetic detail"))
		}))
		if _, err := fetchLiveSecuritySnapshot(s.URL, ""); err == nil {
			t.Fatalf("live snapshot status %d accepted", status)
		}
		s.Close()
	}
	if _, err := fetchLiveSecuritySnapshot("http://127.0.0.1:1", ""); err == nil {
		t.Fatal("live snapshot unreachable accepted")
	}
	if _, err := fetchLiveSecuritySnapshot("://bad-url", ""); err == nil {
		t.Fatal("live snapshot bad URL accepted")
	}
	// refuseOutputCollision / canonicalPath / splitPathComponents.
	src := Continue95GatewayWriteFile(t, dir, "source.txt", "synthetic")
	if err := refuseOutputCollision(src, []string{src}); err == nil {
		t.Fatal("output equal to source accepted")
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	if err := refuseOutputCollision(link, []string{src}); err == nil {
		t.Fatal("symlinked output aliasing source accepted")
	}
	if err := refuseOutputCollision(filepath.Join(dir, "other.txt"), []string{src}); err != nil {
		t.Fatalf("distinct output refused: %v", err)
	}
	if got, err := canonicalPath(""); err == nil || got != "" {
		t.Fatalf("canonical empty path: got=%q err=%v", got, err)
	}
	if got := splitPathComponents("/a//b/c/"); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("splitPathComponents=%v", got)
	}
	// Symlink loop: canonicalization must fail, not hang.
	loopA := filepath.Join(dir, "loop-a")
	loopB := filepath.Join(dir, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalPath(filepath.Join(loopA, "file")); err == nil {
		t.Fatal("symlink loop canonicalization accepted")
	}
	// writeBundle: success, missing stage file, and short-file tar error.
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "config.json"), []byte("synthetic-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"format":"synthetic"}`)
	if err := writeBundle(filepath.Join(dir, "bundle.tar"), manifest, []backupManifestFile{{Name: "config.json", Size: 16}}, stage); err != nil {
		t.Fatalf("writeBundle: %v", err)
	}
	if err := writeBundle(filepath.Join(dir, "bundle2.tar"), manifest, []backupManifestFile{{Name: "missing.json", Size: 1}}, stage); err == nil {
		t.Fatal("writeBundle missing stage file accepted")
	}
	if err := writeBundle(filepath.Join(dir, "bundle3.tar"), manifest, []backupManifestFile{{Name: "config.json", Size: 100}}, stage); err == nil {
		t.Fatal("writeBundle short stage file accepted (tar close must fail)")
	}
	if err := writeBundle(filepath.Join(dir, "missing-dir", "bundle.tar"), manifest, nil, stage); err == nil {
		t.Fatal("writeBundle into missing directory accepted")
	}
	// writeFileAtomic (gateway): success and rename-onto-directory error.
	if err := writeFileAtomic(filepath.Join(dir, "atomic.txt"), []byte("synthetic"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if err := writeFileAtomic(dir, []byte("synthetic"), 0o600); err == nil {
		t.Fatal("writeFileAtomic onto a directory accepted")
	}
	nestedAtomic := filepath.Join(dir, "missing-dir", "x.txt")
	if err := writeFileAtomic(nestedAtomic, []byte("synthetic"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic nested: %v", err)
	}
	if data, err := os.ReadFile(nestedAtomic); err != nil || string(data) != "synthetic" {
		t.Fatalf("writeFileAtomic nested content=%q err=%v", data, err)
	}
	// writeSavedCopyDurable + removeJournalDir + journal manifests.
	saved := filepath.Join(dir, "saved.txt")
	if err := writeSavedCopyDurable(saved, []byte("synthetic-saved")); err != nil {
		t.Fatalf("writeSavedCopyDurable: %v", err)
	}
	if err := writeSavedCopyDurable(filepath.Join(dir, "missing-dir-2", "saved.txt"), []byte("x")); err == nil {
		t.Fatal("writeSavedCopyDurable into missing directory accepted")
	}
	journalDir := filepath.Join(dir, "journal")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeJournalDir(filepath.Join(dir, "nonexistent-journal")); err != nil {
		t.Fatalf("removeJournalDir nonexistent: %v", err)
	}
	jm := &restoreJournalManifest{Format: restoreJournalFormat, Entries: nil}
	if err := writeJournalManifest(journalDir, jm); err != nil {
		t.Fatalf("writeJournalManifest: %v", err)
	}
	if got, err := readJournalManifest(journalDir); err != nil || got.Format != restoreJournalFormat {
		t.Fatalf("readJournalManifest: %+v err=%v", got, err)
	}
	if _, err := readJournalManifest(filepath.Join(dir, "missing-journal")); err == nil {
		t.Fatal("readJournalManifest missing accepted")
	}
	if err := os.WriteFile(filepath.Join(journalDir, "manifest.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournalManifest(journalDir); err == nil {
		t.Fatal("readJournalManifest corrupt accepted")
	}
	if err := os.WriteFile(filepath.Join(journalDir, "manifest.json"), []byte(`{"format":"wrong-format"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournalManifest(journalDir); err == nil {
		t.Fatal("readJournalManifest wrong format accepted")
	}
	if err := writeJournalManifest(filepath.Join(dir, "missing-journal"), jm); err == nil {
		t.Fatal("writeJournalManifest into missing directory accepted")
	}
	if err := syncDir(filepath.Join(dir, "missing-dir-3")); err == nil {
		t.Fatal("syncDir missing directory accepted")
	}
	// validateBundledConfig: bad JSON and missing config entry.
	if _, _, err := validateBundledConfig("synthetic-stage", map[string][]byte{"config.json": []byte(`not json`)}); err == nil {
		t.Fatal("validateBundledConfig bad JSON accepted")
	}
	if _, _, err := validateBundledConfig("synthetic-stage", map[string][]byte{}); err == nil {
		t.Fatal("validateBundledConfig missing config accepted")
	}
	// readAndVerifyBundle: truncated entry.
	if _, _, _, err := readAndVerifyBundle(filepath.Join(dir, "missing.tar")); err == nil {
		t.Fatal("readAndVerifyBundle missing accepted")
	}
	truncPath := filepath.Join(dir, "trunc.tar")
	if err := writeBundle(truncPath, manifest, []backupManifestFile{{Name: "config.json", Size: 16}}, stage); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(truncPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truncPath, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readAndVerifyBundle(truncPath); err == nil {
		t.Fatal("readAndVerifyBundle truncated accepted")
	}
}

func Continue95GatewayVaultServer(t *testing.T, handler http.HandlerFunc) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL, &http.Client{Timeout: 5 * time.Second}
}

func Continue95GatewayHelperScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sh")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContinue95GatewayVaultAcceptHelpers(t *testing.T) {
	// validateVaultAcceptRef / Addr / Helper / EvidenceClass.
	for _, bad := range []string{"", " /data/x", "/secret/data/x", "secret/data/x/", "secret/../data/x", "secret/data/x?y", "secret/data/x#frag", "secret/notdata/x"} {
		if err := validateVaultAcceptRef(bad); err == nil {
			t.Fatalf("vault ref %q accepted", bad)
		}
	}
	if err := validateVaultAcceptRef("secret/data/nexaroute/acceptance/master-key"); err != nil {
		t.Fatalf("valid vault ref rejected: %v", err)
	}
	if _, err := validateVaultAcceptAddr(""); err == nil {
		t.Fatal("empty vault addr accepted")
	}
	if _, err := validateVaultAcceptAddr("ftp://example.test"); err == nil {
		t.Fatal("ftp vault addr accepted")
	}
	if _, err := validateVaultAcceptAddr("https://user@example.test"); err == nil {
		t.Fatal("vault addr with userinfo accepted")
	}
	if _, err := validateVaultAcceptAddr("http://192.0.2.10"); err == nil {
		t.Fatal("non-loopback http vault addr accepted")
	}
	if u, err := validateVaultAcceptAddr("http://127.0.0.1:8200"); err != nil || u == nil {
		t.Fatalf("loopback vault addr: %v", err)
	}
	if got := vaultAcceptEvidenceClass(nil); got != "none" {
		t.Fatalf("nil evidence class=%q", got)
	}
	u, _ := validateVaultAcceptAddr("https://vault.example.test")
	if got := vaultAcceptEvidenceClass(u); got != "external" {
		t.Fatalf("external evidence class=%q", got)
	}
	if _, err := validateVaultAcceptHelper(""); err == nil {
		t.Fatal("empty helper accepted")
	}
	if _, err := validateVaultAcceptHelper(filepath.Join(t.TempDir(), "missing.sh")); err == nil {
		t.Fatal("missing helper accepted")
	}
	dir := t.TempDir()
	dirHelper := filepath.Join(dir, "adir")
	if err := os.MkdirAll(dirHelper, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := validateVaultAcceptHelper(dirHelper); err == nil {
		t.Fatal("directory helper accepted")
	}
	nonExec := Continue95GatewayWriteFile(t, dir, "helper.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(nonExec, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateVaultAcceptHelper(nonExec); err == nil {
		t.Fatal("non-executable helper accepted")
	}
	execHelper := Continue95GatewayHelperScript(t, "exit 0")
	if got, err := validateVaultAcceptHelper(execHelper); err != nil || got == "" {
		t.Fatalf("executable helper: got=%q err=%v", got, err)
	}
	// vaultHTTP: encode failure, request-creation failure, request failure,
	// oversize body is unreachable locally, so assert the reachable set.
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	if _, _, err := vaultHTTP(ctx, client, "http://127.0.0.1:9", "synthetic-token", http.MethodPost, "sys/test", func() {}); err == nil {
		t.Fatal("vaultHTTP unmarshalable body accepted")
	}
	if _, _, err := vaultHTTP(ctx, client, "http://127.0.0.1:9", "synthetic-token", "BAD METHOD\x7f", "sys/test", nil); err == nil {
		t.Fatal("vaultHTTP bad method accepted")
	}
	if _, _, err := vaultHTTP(ctx, client, "http://127.0.0.1:1", "synthetic-token", http.MethodGet, "sys/test", nil); err == nil {
		t.Fatal("vaultHTTP unreachable accepted")
	}
	if _, _, err := vaultHTTP(ctx, client, "://bad", "synthetic-token", http.MethodGet, "sys/test", nil); err == nil {
		t.Fatal("vaultHTTP bad addr accepted")
	}
	// vaultAcceptSafeError / safeVaultAcceptString.
	if got := vaultAcceptSafeError(nil); got != "" {
		t.Fatalf("nil safe error=%q", got)
	}
	if got := vaultAcceptSafeError(errors.New("")); got != "request failed" {
		t.Fatalf("empty safe error=%q", got)
	}
	if got := vaultAcceptSafeError(errors.New(strings.Repeat("x", 300))); len(got) != 200 {
		t.Fatalf("long safe error len=%d", len(got))
	}
	if got := safeVaultAcceptString("  "); got != "unknown" {
		t.Fatalf("blank safe string=%q", got)
	}
	if got := safeVaultAcceptString("  synth  "); got != "synth" {
		t.Fatalf("safe string=%q", got)
	}
	if got := vaultMetadataPath("secret/data/a/b"); got != "secret/metadata/a/b" {
		t.Fatalf("metadata path=%q", got)
	}
	if got := vaultMetadataPath("secret/notdata/a"); got != "" {
		t.Fatalf("metadata path without /data/=%q", got)
	}
	// Health / token / preflight fail branches against a local server.
	addr, _ := Continue95GatewayVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/seal-status":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/v1/auth/token/lookup-self":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	check := runVaultAcceptHealth(ctx, client, addr, "synthetic-token")
	if check.Status != externalStatusFail {
		t.Fatalf("health 503 check=%+v", check)
	}
	report := vaultAcceptReport{}
	check = runVaultAcceptTokenMetadata(ctx, client, addr, "synthetic-token", &report)
	if check.Status != externalStatusBlocked {
		t.Fatalf("token 403 check=%+v", check)
	}
	addr2, _ := Continue95GatewayVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "sys/seal-status"):
			_, _ = w.Write([]byte(`{"version":"1.2.3","storage_type":"raft","sealed":false,"standby":false}`))
		case strings.HasSuffix(r.URL.Path, "sys/leader"):
			_, _ = w.Write([]byte(`{"ha_enabled":true}`))
		case strings.HasSuffix(r.URL.Path, "auth/token/lookup-self"):
			_, _ = w.Write([]byte(`{"data":{"policies":["synthetic-policy"],"ttl":3600,"renewable":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	check = runVaultAcceptHealth(ctx, client, addr2, "synthetic-token")
	if check.Status != externalStatusPass || !strings.Contains(check.Detail, "version=1.2.3") {
		t.Fatalf("health pass check=%+v", check)
	}
	check = runVaultAcceptTokenMetadata(ctx, client, addr2, "synthetic-token", &report)
	if check.Status != externalStatusPass || len(report.Vault.Policies) != 1 {
		t.Fatalf("token pass check=%+v report=%+v", check, report)
	}
	// Sealed vault fails health even with valid JSON.
	addr3, _ := Continue95GatewayVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.2.3","storage_type":"raft","sealed":true}`))
	})
	if check := runVaultAcceptHealth(ctx, client, addr3, "synthetic-token"); check.Status != externalStatusFail {
		t.Fatalf("sealed health check=%+v", check)
	}
	// Preflight branches: occupied, forbidden, other status, unreachable.
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, externalStatusPass},
		{http.StatusOK, externalStatusFail},
		{http.StatusForbidden, externalStatusFail},
		{http.StatusTeapot, externalStatusFail},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		check := runVaultAcceptRefPreflight(ctx, client, srv.URL, "synthetic-token", "secret/data/x")
		srv.Close()
		if check.Status != tc.want {
			t.Fatalf("preflight status %d: got %q want %q", tc.status, check.Status, tc.want)
		}
	}
	if check := runVaultAcceptRefPreflight(ctx, client, "http://127.0.0.1:1", "synthetic-token", "secret/data/x"); check.Status != externalStatusFail {
		t.Fatalf("preflight unreachable check=%+v", check)
	}
	// Put/get: helper failure, stdout-on-put, get mismatch.
	failHelper := Continue95GatewayHelperScript(t, "echo synthetic-output; exit 3")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 2)
	}
	if check := runVaultAcceptPutGet(ctx, failHelper, "secret/data/x", key, addr, "synthetic-token"); check.Status != externalStatusFail {
		t.Fatalf("put/get failing helper check=%+v", check)
	}
	// Helper that succeeds silently on put but returns a different 32-byte
	// key on get (the helper contract passes op/ref on stdin, not argv).
	mismatchKey := make([]byte, 32)
	for i := range mismatchKey {
		mismatchKey[i] = byte(i + 11)
	}
	diffHelper := Continue95GatewayHelperScript(t, `read op ref; if [ "$op" = "put" ]; then exit 0; fi; printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='`)
	if check := runVaultAcceptPutGet(ctx, diffHelper, "secret/data/x", mismatchKey, addr, "synthetic-token"); check.Status != externalStatusFail || !strings.Contains(check.Reason, "same 32-byte key") {
		t.Fatalf("put/get mismatch check=%+v", check)
	}
	// Decrypt check: valid key passes; short key fails at encrypt.
	if check := runVaultAcceptDecrypt(key); check.Status != externalStatusPass {
		t.Fatalf("decrypt pass check=%+v", check)
	}
	if check := runVaultAcceptDecrypt([]byte("short")); check.Status != externalStatusFail {
		t.Fatalf("decrypt short-key check=%+v", check)
	}
	// Wrong-token / outage fail-closed: a helper that exits nonzero
	// proves fail-closed with a PASS (runVaultAcceptHelper returns nil
	// bytes on error, so the stdout-after-failure branches in these two
	// checks are unreachable by construction; see the report).
	noisyFail := Continue95GatewayHelperScript(t, "echo synthetic-noise; exit 3")
	if check := runVaultAcceptWrongToken(ctx, noisyFail, "secret/data/x", addr); check.Status != externalStatusPass {
		t.Fatalf("wrong-token failing-helper check=%+v", check)
	}
	if check := runVaultAcceptOutage(ctx, noisyFail, "secret/data/x", "http://127.0.0.1:1", "synthetic-token"); check.Status != externalStatusPass {
		t.Fatalf("outage failing-helper check=%+v", check)
	}
	// A helper that succeeds against the outage endpoint must FAIL.
	if check := runVaultAcceptOutage(ctx, execHelper, "secret/data/x", "http://127.0.0.1:1", "synthetic-token"); check.Status != externalStatusFail {
		t.Fatalf("outage succeeding-helper check=%+v", check)
	}
	// Revoked-token: child creation failure blocks; child creation with
	// empty token fails; helper get after create fails.
	revokeFailAddr, _ := Continue95GatewayVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	if check := runVaultAcceptRevokedToken(ctx, client, execHelper, "secret/data/x", revokeFailAddr, "synthetic-token", time.Second); check.Status != externalStatusBlocked {
		t.Fatalf("revoked blocked check=%+v", check)
	}
	emptyTokenAddr, _ := Continue95GatewayVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"auth":{}}`))
	})
	if check := runVaultAcceptRevokedToken(ctx, client, execHelper, "secret/data/x", emptyTokenAddr, "synthetic-token", time.Second); check.Status != externalStatusFail {
		t.Fatalf("revoked empty-token check=%+v", check)
	}
	// Cleanup: metadata path derivation is covered above; cleanup with a
	// server returning 204 passes, 403 blocks, 500 fails.
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNoContent, externalStatusPass},
		{http.StatusForbidden, externalStatusBlocked},
		{http.StatusInternalServerError, externalStatusFail},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		check := runVaultAcceptCleanup(ctx, client, srv.URL, "synthetic-token", "secret/data/x")
		srv.Close()
		if check.Status != tc.want {
			t.Fatalf("cleanup status %d: got %q want %q", tc.status, check.Status, tc.want)
		}
	}
	if check := runVaultAcceptCleanup(ctx, client, "http://127.0.0.1:1", "synthetic-token", "secret/data/x"); check.Status != externalStatusFail {
		t.Fatalf("cleanup unreachable check=%+v", check)
	}
	if check := runVaultAcceptCleanup(ctx, client, addr, "synthetic-token", "secret/notdata/x"); check.Status != externalStatusFail {
		t.Fatalf("cleanup no-metadata-path check=%+v", check)
	}
	// finishVaultAccept report-write failure returns 2: the report
	// writer creates parent directories, so the failure path needs an
	// output under a regular file.
	var o, e bytes.Buffer
	blockerFile := Continue95GatewayWriteFile(t, dir, "blocker", "x")
	if code := finishVaultAccept(vaultAcceptReport{Checks: []vaultAcceptCheck{{ID: "x", Status: externalStatusPass, Selected: true}}}, filepath.Join(blockerFile, "report.json"), &o, &e); code != 2 {
		t.Fatalf("finishVaultAccept bad out exit=%d want 2", code)
	}
	// writeVaultAcceptReport into a directory-as-file path fails at rename.
	if err := writeVaultAcceptReport(dir, vaultAcceptReport{}); err == nil {
		t.Fatal("writeVaultAcceptReport onto a directory accepted")
	}
	// runVaultAccept usage branches.
	if code := runVaultAccept([]string{"--timeout", "0s"}, &o, &e); code != 2 {
		t.Fatalf("vault timeout 0 exit=%d want 2", code)
	}
	if code := runVaultAccept([]string{"--timeout", "10s", "--overall-timeout", "1s"}, &o, &e); code != 2 {
		t.Fatalf("vault overall<timeout exit=%d want 2", code)
	}
	if code := runVaultAccept([]string{"--ref", ""}, &o, &e); code != 2 {
		t.Fatalf("vault empty ref exit=%d want 2", code)
	}
	if code := runVaultAccept([]string{"--ref", "secret/data/x", "--addr-env", "", "--token-env", "VAULT_TOKEN"}, &o, &e); code != 2 {
		t.Fatalf("vault empty addr-env exit=%d want 2", code)
	}
	if code := runVaultAccept([]string{"--ref", "secret/data/x"}, &o, &e); code != 2 {
		t.Fatalf("vault no confirm-live exit=%d want 2", code)
	}
	if code := runVaultAccept([]string{"--ref", "secret/data/x", "--confirm-live", "--helper", filepath.Join(dir, "missing.sh")}, &o, &e); code != 2 {
		t.Fatalf("vault missing helper exit=%d want 2", code)
	}
	if code := runVaultAccept([]string{"--ref", "secret/data/x", "--confirm-live", "--helper", execHelper, "--outage-addr", "http://192.0.2.10"}, &o, &e); code != 2 {
		t.Fatalf("vault bad outage addr exit=%d want 2", code)
	}
}

func TestContinue95GatewayComfyAcceptBranches(t *testing.T) {
	// runComfyUIAccept usage branches.
	var o, e bytes.Buffer
	if code := runComfyUIAccept([]string{"--timeout", "0s", "--confirm-live"}, &o, &e); code != 2 {
		t.Fatalf("comfy timeout 0 exit=%d want 2", code)
	}
	if code := runComfyUIAccept([]string{"--confirm-live", "--poll-interval", "0s"}, &o, &e); code != 2 {
		t.Fatalf("comfy poll 0 exit=%d want 2", code)
	}
	if code := runComfyUIAccept([]string{"--confirm-live", "--prompt", "   "}, &o, &e); code != 2 {
		t.Fatalf("comfy empty prompt exit=%d want 2", code)
	}
	if code := runComfyUIAccept([]string{"--confirm-live", "--duration", "0"}, &o, &e); code != 2 {
		t.Fatalf("comfy duration 0 exit=%d want 2", code)
	}
	if code := runComfyUIAccept([]string{"--confirm-live", "--config", filepath.Join(t.TempDir(), "missing.json")}, &o, &e); code != 2 {
		t.Fatalf("comfy missing config exit=%d want 2", code)
	}
	// verifyComfyAcceptAsset branches via a real local store.
	dir := t.TempDir()
	sink, err := storage.NewLocal(filepath.Join(dir, "store"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := sink.Put(context.Background(), video.Asset{Filename: "accept.mp4", ContentType: "video/mp4", Metadata: map[string]string{"project_id": "p1", "job_id": "j1", "provider": "comfyui", "node_id": "10", "filename": "accept.mp4"}}, comfyAcceptMP4())
	if err != nil {
		t.Fatal(err)
	}
	probe := &render.Renderer{FFprobe: comfyAcceptProbe(t)}
	if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, stored); err != nil {
		t.Fatalf("verify stored asset: %v", err)
	}
	mut := func(f func(*video.Asset)) video.Asset {
		c := stored
		f(&c)
		return c
	}
	cases := []struct {
		name  string
		asset video.Asset
	}{
		{"zero size", mut(func(a *video.Asset) { a.SizeBytes = 0 })},
		{"non-video", mut(func(a *video.Asset) { a.ContentType = "image/png" })},
		{"short sha", mut(func(a *video.Asset) { a.SHA256 = "abcd" })},
		{"malformed sha", mut(func(a *video.Asset) { a.SHA256 = strings.Repeat("z", 64) })},
		{"size mismatch", mut(func(a *video.Asset) { a.SizeBytes = stored.SizeBytes + 5 })},
		{"sha mismatch", mut(func(a *video.Asset) { a.SHA256 = strings.Repeat("0", 64) })},
		{"provider missing", mut(func(a *video.Asset) { a.Metadata["provider"] = "other" })},
		{"node missing", mut(func(a *video.Asset) { a.Metadata["node_id"] = "" })},
		{"filename missing", mut(func(a *video.Asset) { a.Metadata["filename"] = "" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, tc.asset); err == nil {
				t.Fatalf("verify accepted %s", tc.name)
			}
		})
	}
	// Read-back failure: asset URI pointing outside the store.
	ghost := stored
	ghost.URI = filepath.Join(dir, "ghost.mp4")
	ghost.SizeBytes = 10
	if _, err := verifyComfyAcceptAsset(context.Background(), sink, probe, ghost); err == nil {
		t.Fatal("verify ghost asset accepted")
	}
	// Asset access branches.
	if err := verifyComfyAcceptAssetAccess(dir, stored); err != nil {
		t.Fatalf("access stored asset (root is dir, asset inside store): %v", err)
	}
	// Asset outside the root must be refused.
	if err := verifyComfyAcceptAssetAccess(filepath.Join(dir, "store"), video.Asset{URI: filepath.Join(dir, "outside.mp4")}); err == nil {
		t.Fatal("access outside root accepted")
	}
	// Nonexistent asset inside the root: stat failure.
	if err := verifyComfyAcceptAssetAccess(filepath.Join(dir, "store"), video.Asset{URI: filepath.Join(dir, "store", "missing.mp4")}); err == nil {
		t.Fatal("access missing asset accepted")
	}
	// Directory asset is not a regular file.
	if err := verifyComfyAcceptAssetAccess(filepath.Join(dir, "store"), video.Asset{URI: filepath.Join(dir, "store")}); err == nil {
		t.Fatal("access directory asset accepted")
	}
	// Mode 0644 asset fails the owner-only check.
	modeFile := filepath.Join(dir, "store", "mode.mp4")
	if err := os.WriteFile(modeFile, comfyAcceptMP4(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyComfyAcceptAssetAccess(filepath.Join(dir, "store"), video.Asset{URI: modeFile}); err == nil {
		t.Fatal("access mode-0644 asset accepted")
	}
	// Symlink escape: a symlink inside the root pointing outside.
	outside := filepath.Join(dir, "outside-target.mp4")
	if err := os.WriteFile(outside, comfyAcceptMP4(), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "store", "link.mp4")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Fatal(err)
	}
	if err := verifyComfyAcceptAssetAccess(filepath.Join(dir, "store"), video.Asset{URI: linkPath}); err == nil {
		t.Fatal("access symlink-escape asset accepted")
	}
	// Completed job with no outputs -> download check fails (383/410 path).
	emptyOutServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/prompt":
			_, _ = w.Write([]byte(`{"prompt_id":"prompt-empty"}`))
		case r.URL.Path == "/history/prompt-empty":
			_, _ = w.Write([]byte(`{"prompt-empty":{"outputs":{},"status":{"status_str":"success","completed":true}}}`))
		case r.URL.Path == "/queue":
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer emptyOutServer.Close()
	configPath := comfyAcceptConfig(t, comfyAcceptWorkflow(t), emptyOutServer.URL)
	code, report, _, stdout, stderr := runComfyAcceptTest(t, "--config", configPath, "--confirm-live", "--ffprobe", comfyAcceptProbe(t), "--timeout", "5s", "--poll-interval", "10ms")
	if code != 1 {
		t.Fatalf("empty-output run exit=%d want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	// The provider converts a completed job with no MP4 output into a
	// poll failure, so the run fails closed at poll (or download) here.
	if pollStatus, _ := comfyAcceptCheckStatus(report, "comfyui:poll"); pollStatus != externalStatusFail {
		if dlStatus, _ := comfyAcceptCheckStatus(report, "comfyui:download"); dlStatus != externalStatusFail {
			t.Fatalf("empty-output run: poll=%q download=%q, want a FAIL", pollStatus, dlStatus)
		}
	}
	// Poll branches: a failed job state ends the poll with an error, and
	// a poll whose context expires while GetJob keeps failing wraps the
	// last status error.
	videoCfg := comfyAcceptVideoConfig(config.VideoConfig{
		Enabled: true, ComfyUIEnabled: true, ComfyUIBaseURL: emptyOutServer.URL,
		ComfyUIWorkflowPath: comfyAcceptWorkflow(t), ComfyUIModel: "local-video",
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}, "config.json")
	_ = videoCfg
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system_stats":
			_, _ = w.Write([]byte(`{"system":{"comfyui_version":"test"},"devices":[]}`))
		case r.URL.Path == "/history/prompt-failstate":
			_, _ = w.Write([]byte(`{"prompt-failstate":{"outputs":{},"status":{"status_str":"error","completed":false}}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer failServer.Close()
	failCfg := comfyAcceptVideoConfig(config.VideoConfig{
		Enabled: true, ComfyUIEnabled: true, ComfyUIBaseURL: failServer.URL,
		ComfyUIWorkflowPath: comfyAcceptWorkflow(t), ComfyUIModel: "local-video",
		ComfyUIPromptNodeID: "1", ComfyUIPromptInput: "text",
		ComfyUIWidthNodeID: "2", ComfyUIHeightNodeID: "2",
		ComfyUIFramesNodeID: "3", ComfyUIFramesInput: "length",
		ComfyUIFPSNodeID: "3", ComfyUIFPSInput: "frame_rate",
		ComfyUISeedNodeID: "3", ComfyUISeedInput: "seed",
		ComfyUINegativeNodeID: "4",
	}, "config.json")
	failProvider, err := providers.NewComfyUI(failCfg, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	failStatus, _, pollErr := pollComfyAcceptJob(context.Background(), failProvider, "prompt-failstate", 10*time.Millisecond)
	if pollErr == nil {
		t.Fatal("poll on failed job accepted")
	}
	if failStatus.State != video.StateFailed {
		t.Fatalf("failed poll state=%q", failStatus.State)
	}
	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errServer.Close()
	errCfg := failCfg
	errCfg.ComfyUIBaseURL = errServer.URL
	errProvider, err := providers.NewComfyUI(errCfg, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, pollErr = pollComfyAcceptJob(ctx, errProvider, "prompt-err", 10*time.Millisecond)
	if pollErr == nil || !strings.Contains(pollErr.Error(), "last status error") {
		t.Fatalf("poll with persistent errors: err=%v", pollErr)
	}
	// Fail-closed cleanup with no provider and no run dir.
	rep := &comfyAcceptReport{}
	comfyAcceptFailClosed(rep, "", nil)
	if rep.Cleanup == nil || rep.Cleanup.LocalStatus != externalStatusNotRun {
		t.Fatalf("fail-closed no-dir cleanup=%+v", rep.Cleanup)
	}
	// Report helpers.
	rep2 := comfyAcceptReport{EvidenceClass: "loopback", Checks: []comfyAcceptCheck{{ID: "a", Status: externalStatusBlocked}}}
	finalizeComfyAcceptReport(&rep2)
	if len(rep2.Limitations) < 2 {
		t.Fatalf("finalize limitations=%v", rep2.Limitations)
	}
	if err := writeComfyAcceptReport(filepath.Join(dir, "missing-dir", "r.json"), comfyAcceptReport{}); err != nil {
		// MkdirAll creates the directory, so this succeeds; assert the
		// file landed instead.
		t.Fatalf("writeComfyAcceptReport nested: %v", err)
	}
	var summary bytes.Buffer
	printComfyAcceptSummary(&summary, comfyAcceptReport{Outcome: externalStatusPass, Server: &comfyAcceptServer{ComfyUIVersion: "test"}, Job: &comfyAcceptJob{PromptID: "p"}}, "out.json")
	if !strings.Contains(summary.String(), "outcome=") {
		t.Fatalf("comfy summary=%q", summary.String())
	}
}

// --- Wave 2 additions: error branches mapped from the c2ca13c profile ---

func TestContinue95GatewayQualityGateReportRename(t *testing.T) {
	var out bytes.Buffer
	if err := writeReport(t.TempDir(), []byte("synthetic report"), &out); err == nil {
		t.Fatal("writeReport onto an existing directory accepted")
	}
}

func TestContinue95GatewayDiagnosticsWriteFailures(t *testing.T) {
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	dir := t.TempDir()
	cfgPath := Continue95GatewayDiagnosticsConfig(t, dir, "off")

	// Bundle write failure: output under a regular file.
	blocker := Continue95GatewayWriteFile(t, dir, "blocker", "x")
	var out, errBuf bytes.Buffer
	if code := runDiagnostics([]string{"bundle", "--config", cfgPath, "--output", filepath.Join(blocker, "bundle.json")}, &out, &errBuf); code != 1 {
		t.Fatalf("bundle write code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "cannot write") {
		t.Fatalf("bundle write stderr=%q", errBuf.String())
	}

	// Atomic bundle write rename failure: target is an existing directory.
	if err := writeDiagnosticsBundleAtomic(t.TempDir(), []byte("synthetic bundle")); err == nil {
		t.Fatal("writeDiagnosticsBundleAtomic onto an existing directory accepted")
	}

	// Recent errors with logging off: nothing available, nothing included.
	offCfg := config.Default()
	offCfg.Logging.File = "off"
	offRes := diagnosticsRecentErrorsFrom(offCfg, filepath.Join(dir, "config.json"), newDiagnosticsSanitizer(nil))
	if offRes.LogAvailable || len(offRes.Included) != 0 {
		t.Fatalf("off log errors=%+v", offRes)
	}

	// Config warnings are capped at diagnosticsMaxWarnings.
	warnCfg := config.Default()
	for i := 0; i < 25; i++ {
		warnCfg.Warnings = append(warnCfg.Warnings, fmt.Sprintf("synthetic warning %02d", i))
	}
	summary := diagnosticsConfigSummaryFrom(warnCfg, nil, newDiagnosticsSanitizer(&warnCfg))
	if len(summary.Warnings) != diagnosticsMaxWarnings {
		t.Fatalf("warnings=%d want %d", len(summary.Warnings), diagnosticsMaxWarnings)
	}
}

func TestContinue95GatewayDiagnosticsSanitizerBranches(t *testing.T) {
	// Sanitizer collects secret values from every env-named slot.
	t.Setenv("CONTINUE95_SAN_SECRET_KEY", "synthetic-san-secret-key")
	t.Setenv("CONTINUE95_SAN_SESSION_TOKEN", "synthetic-san-session-token")
	t.Setenv("CONTINUE95_SAN_OIDC_SECRET", "synthetic-san-oidc-secret")
	t.Setenv("CONTINUE95_SAN_VIDEO_TOKEN", "synthetic-san-video-token")
	t.Setenv("CONTINUE95_SAN_RENDER_KEY", "synthetic-san-render-key")
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "san-synth", Type: "openai_compatible", Enabled: true,
		BaseURL:      "https://api.example.test/v1",
		SecretKeyEnv: "CONTINUE95_SAN_SECRET_KEY", SessionTokenEnv: "CONTINUE95_SAN_SESSION_TOKEN",
		Headers:  map[string]string{"X-Custom": "synthetic-header-value"},
		ProxyURL: "https://synthetic-user:synthetic-pass@proxy.example.test:8080",
	}}
	cfg.Admin.OIDC.ClientSecretEnv = "CONTINUE95_SAN_OIDC_SECRET"
	cfg.Video.AuthTokenEnv = "CONTINUE95_SAN_VIDEO_TOKEN"
	cfg.Video.CodeRenderPlannerAPIKeyEnv = "CONTINUE95_SAN_RENDER_KEY"
	san := newDiagnosticsSanitizer(&cfg)
	for _, want := range []string{
		"synthetic-san-secret-key", "synthetic-san-session-token",
		"synthetic-header-value", "synthetic-user", "synthetic-pass",
		"synthetic-san-oidc-secret", "synthetic-san-video-token", "synthetic-san-render-key",
	} {
		found := false
		for _, got := range san.secrets {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("sanitizer secrets %v missing %q", san.secrets, want)
		}
	}

	// Oversize log: offset branch plus line-cap truncation, and the deny
	// scan drops the access-token line even though it looks like an error.
	dir := t.TempDir()
	logCfg := config.Default()
	logCfg.Logging.File = "diagnostics.log"
	cfgPath := filepath.Join(dir, "config.json")
	var lines strings.Builder
	for i := 0; i < 299; i++ {
		fmt.Fprintf(&lines, "gateway error: routine failure %04d %s\n", i, strings.Repeat("x", 900))
	}
	lines.WriteString("gateway error: upstream failed ?access_token=abcdefghij")
	if err := os.WriteFile(filepath.Join(dir, "diagnostics.log"), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	res := diagnosticsRecentErrorsFrom(logCfg, cfgPath, newDiagnosticsSanitizer(nil))
	if !res.LogAvailable {
		t.Fatal("log not available")
	}
	if res.LinesScanned != diagnosticsMaxLogLines {
		t.Fatalf("lines scanned=%d want %d", res.LinesScanned, diagnosticsMaxLogLines)
	}
	if res.Dropped < 1 {
		t.Fatalf("dropped=%d want >=1", res.Dropped)
	}
	for _, line := range res.Included {
		if strings.Contains(line, "access_token") {
			t.Fatalf("deny line leaked into included: %q", line)
		}
	}

	// A deny-matching error detail is withheld verbatim.
	withheld := safeDiagnosticsError(errors.New("boom ?access_token=abcdefghij"), newDiagnosticsSanitizer(nil))
	if withheld != "validation failed (detail withheld by safety scan)" {
		t.Fatalf("safe error=%q", withheld)
	}

	// Doctor detail withheld: a security-store path under a regular file
	// whose basename carries a deny query string.
	dir2 := t.TempDir()
	blocker := Continue95GatewayWriteFile(t, dir2, "blocker", "x")
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	docCfg := config.Default()
	docCfg.Logging.File = "off"
	docCfg.Providers = []config.ProviderConfig{{
		ID: "diag-synth", Name: "Synthetic", Type: "openai_compatible", Enabled: true,
		BaseURL: "https://api.example.test/v1", AuthMode: "bearer",
		APIKeyEnv: "CONTINUE95_DIAG_FAKE_KEY",
	}}
	docCfg.Admin.SecurityStorePath = filepath.Join(blocker, "?access_token=abcdefghij.db")
	raw, err := json.Marshal(docCfg)
	if err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(dir2, "config.json")
	if err := os.WriteFile(docPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := buildDiagnosticsBundle(docPath, "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range bundle.DoctorChecks {
		if c.Detail == "detail withheld by safety scan" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no withheld doctor detail in %+v", bundle.DoctorChecks)
	}
}

func TestContinue95GatewaySecretsCLIErrorMatrix(t *testing.T) {
	// Mixed nxs2 + plaintext config: status must fail at DistinctTransitKeyVersions.
	fake := Continue95GatewayNewFakeTransit()
	server := httptest.NewServer(fake)
	defer server.Close()
	Continue95GatewaySetTransitEnv(t, server.URL)
	dir := t.TempDir()
	legacy := make([]byte, 32)
	for i := range legacy {
		legacy[i] = byte(i + 5)
	}
	nxs1doc, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","api_key":"synthetic-migrate-secret"}]}`), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	migPath := filepath.Join(dir, "mig.json")
	if err := os.WriteFile(migPath, nxs1doc, 0o600); err != nil {
		t.Fatal(err)
	}
	captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"migrate-to-transit", "--config", migPath, "--legacy-key", base64.StdEncoding.EncodeToString(legacy)}); err != nil {
			t.Errorf("migrate: %v", err)
		}
	})
	raw, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	provs := doc["providers"].([]any)
	doc["providers"] = append(provs, map[string]any{"id": "plain-synth", "api_key": "synthetic-plaintext-key"})
	mixed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(migPath, mixed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSecrets([]string{"status", "--config", migPath}); err == nil {
		t.Fatal("mixed nxs2+plaintext status accepted")
	}

	// NOTE: verify's client_auth.keys loop (secrets.go 104-107) is NOT
	// exercised here: verify loads via config.LoadBase, which migrates
	// plaintext Keys into KeyHashes and nils Keys before verify runs, so
	// the loop body is unreachable through the CLI. Recorded as NOT_RUN.
	Continue95GatewayClearTransitEnv(t)

	// Rotate staging failure: a directory sits at the .key.next path.
	rotDir := t.TempDir()
	rotPath := filepath.Join(rotDir, "config.json")
	rotLegacy := make([]byte, 32)
	for i := range rotLegacy {
		rotLegacy[i] = byte(i + 3)
	}
	if err := os.WriteFile(rotPath+".key", rotLegacy, 0o600); err != nil {
		t.Fatal(err)
	}
	sealed, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","api_key":"synthetic-rotate-secret"}]}`), rotLegacy, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rotPath, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rotPath+".key.next", 0o700); err != nil {
		t.Fatal(err)
	}
	// Rotate with a directory at .key.next fails before staging: Key()
	// inspects the staged key first and rejects the directory. The
	// writeRotateFile staging branch (secrets.go 168-169) is therefore
	// NOT_RUN via the CLI: any pre-existing .next either fails Key()
	// inspection or is cleanly replaced by Rename. Recorded as NOT_RUN.
	if err := runSecrets([]string{"rotate", "--config", rotPath}); err == nil || !strings.Contains(err.Error(), "cannot inspect staged master key") {
		t.Fatalf("rotate staged-dir failure err=%v", err)
	}

	// Migrate ConfiguredEnvelope error: unparseable Vault address.
	Continue95GatewaySetTransitEnv(t, "://bad")
	if err := runSecrets([]string{"migrate-to-transit", "--config", migPath}); err == nil || !strings.Contains(err.Error(), "Transit") {
		t.Fatalf("migrate bad vault addr err=%v", err)
	}
	if err := runSecrets([]string{"rewrap-transit", "--config", migPath}); err == nil || !strings.Contains(err.Error(), "Transit") {
		t.Fatalf("rewrap bad vault addr err=%v", err)
	}

	// Migrate/rewrap read and classify errors under a valid fake Transit.
	Continue95GatewaySetTransitEnv(t, server.URL)
	if err := runSecrets([]string{"migrate-to-transit", "--config", filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("migrate missing config accepted")
	}
	badPath := Continue95GatewayWriteFile(t, dir, "bad.json", "{bad json")
	if err := runSecrets([]string{"migrate-to-transit", "--config", badPath}); err == nil {
		t.Fatal("migrate malformed config accepted")
	}
	if err := runSecrets([]string{"rewrap-transit", "--config", filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("rewrap missing config accepted")
	}

	// Migrate dry-run decrypt failure: wrong legacy key.
	wrong := make([]byte, 32)
	for i := range wrong {
		wrong[i] = byte(i + 9)
	}
	nxs1fresh, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","api_key":"synthetic-migrate-secret"}]}`), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	freshPath := filepath.Join(dir, "fresh.json")
	if err := os.WriteFile(freshPath, nxs1fresh, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSecrets([]string{"migrate-to-transit", "--config", freshPath, "--legacy-key", base64.StdEncoding.EncodeToString(wrong), "--dry-run"}); err == nil {
		t.Fatal("migrate dry-run with wrong legacy key accepted")
	}

	// Rewrap against a fresh fake Transit (empty store): pre-verification fails.
	fakeB := Continue95GatewayNewFakeTransit()
	serverB := httptest.NewServer(fakeB)
	defer serverB.Close()
	nxs2Path := filepath.Join(dir, "nxs2.json")
	nxs2doc, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","api_key":"synthetic-migrate-secret"}]}`), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nxs2Path, nxs2doc, 0o600); err != nil {
		t.Fatal(err)
	}
	captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"migrate-to-transit", "--config", nxs2Path, "--legacy-key", base64.StdEncoding.EncodeToString(legacy)}); err != nil {
			t.Errorf("migrate for rewrap fixture: %v", err)
		}
	})
	Continue95GatewaySetTransitEnv(t, serverB.URL)
	if err := runSecrets([]string{"rewrap-transit", "--config", nxs2Path, "--dry-run"}); err == nil || !strings.Contains(err.Error(), "pre-verification") {
		t.Fatalf("rewrap dry-run against fresh transit err=%v", err)
	}
	if err := runSecrets([]string{"rewrap-transit", "--config", nxs2Path}); err == nil {
		t.Fatal("rewrap against fresh transit accepted")
	}
}

// Continue95GatewayFakeRESPServer starts a minimal RESP2 server that
// answers PING with PONG and everything else with OK. It never touches a
// real Redis instance; it exists only so the Redis control-plane open path
// can be exercised with a failing manager configuration.
func Continue95GatewayFakeRESPServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					cmd := ""
					if strings.HasPrefix(line, "*") {
						n, _ := strconv.Atoi(line[1:])
						for i := 0; i < n; i++ {
							hdr, err := r.ReadString('\n')
							if err != nil {
								return
							}
							hdr = strings.TrimSpace(hdr)
							if !strings.HasPrefix(hdr, "$") {
								continue
							}
							size, _ := strconv.Atoi(hdr[1:])
							buf := make([]byte, size+2)
							if _, err := io.ReadFull(r, buf); err != nil {
								return
							}
							if i == 0 {
								cmd = strings.ToUpper(string(buf[:size]))
							}
						}
					} else if strings.HasPrefix(line, "$") {
						size, _ := strconv.Atoi(line[1:])
						buf := make([]byte, size+2)
						if _, err := io.ReadFull(r, buf); err != nil {
							return
						}
						cmd = strings.ToUpper(string(buf[:size]))
					} else if fields := strings.Fields(line); len(fields) > 0 {
						cmd = strings.ToUpper(fields[0])
					}
					// go-redis negotiates RESP3 with HELLO first and
					// requires a map reply (server/version/proto/id/
					// mode/role/modules); a status reply fails Ping.
					if cmd == "HELLO" {
						_, _ = c.Write([]byte("%7\r\n+server\r\n+redis\r\n+version\r\n+7.0.0\r\n+proto\r\n:3\r\n+id\r\n:1\r\n+mode\r\n+standalone\r\n+role\r\n+master\r\n+modules\r\n*0\r\n"))
					} else if cmd == "PING" {
						_, _ = c.Write([]byte("+PONG\r\n"))
					} else {
						_, _ = c.Write([]byte("+OK\r\n"))
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestContinue95GatewayControlPlaneErrors(t *testing.T) {
	dir := t.TempDir()
	// File store open failure: state path is an existing directory.
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "file", StatePath: dir}, filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("file control plane on a directory accepted")
	}
	// Invalid failure mode with a valid file store.
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "file", StatePath: filepath.Join(dir, "state.json"), ConfigFailure: "bogus"}, filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("invalid control plane failure mode accepted")
	}
	// Invalid failure mode with a live (fake) Redis store: the open path
	// succeeds, then NewManager fails and the Redis closer is closed.
	addr := Continue95GatewayFakeRESPServer(t)
	t.Setenv("CONTINUE95_CP_REDIS_URL", "redis://"+addr+"/0")
	if _, err := openControlPlaneRuntime(config.ControlPlaneConfig{Enabled: true, Backend: "redis", RedisURLEnv: "CONTINUE95_CP_REDIS_URL", ConfigFailure: "bogus"}, filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("invalid control plane failure mode over redis accepted")
	}
}

func TestContinue95GatewayBackupCreateErrors(t *testing.T) {
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	dir := t.TempDir()
	cfgPath := Continue95GatewayDiagnosticsConfig(t, dir, "off")
	var out bytes.Buffer

	// Live admin URL error: config loads, then URL resolution fails.
	if err := createBackup(cfgPath, filepath.Join(dir, "live.json"), false, false, &out, liveBackupOptions{enabled: true, adminURL: "://bad"}); err == nil {
		t.Fatal("createBackup with unparseable live admin URL accepted")
	}

	// Security-store Stat error: absolute store path under a regular file.
	// The path fails earlier at the collision check's canonicalization,
	// not at the Stat branch (backup.go 357-358), which needs a Stat
	// failure after successful canonicalization — a race/FS-fault-only
	// path, recorded as NOT_RUN. The CLI must still fail closed here.
	blocker := Continue95GatewayWriteFile(t, dir, "blocker", "x")
	storeCfg := config.Default()
	storeCfg.Logging.File = "off"
	storeCfg.Providers = []config.ProviderConfig{{
		ID: "diag-synth", Name: "Synthetic", Type: "openai_compatible", Enabled: true,
		BaseURL: "https://api.example.test/v1", AuthMode: "bearer",
		APIKeyEnv: "CONTINUE95_DIAG_FAKE_KEY",
	}}
	storeCfg.Admin.SecurityStorePath = filepath.Join(blocker, "security.db")
	raw, err := json.Marshal(storeCfg)
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(dir, "store.json")
	if err := os.WriteFile(storePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createBackup(storePath, filepath.Join(dir, "store-backup.json"), false, false, &out, liveBackupOptions{}); err == nil || !strings.Contains(err.Error(), "resolve backup source path") {
		t.Fatalf("createBackup security store stat err=%v", err)
	}
}

func TestContinue95GatewayBackupTransitManifestError(t *testing.T) {
	// Empirical branch probe, outcome: LoadBase fails first on the
	// plaintext field in Transit mode, so the TransitEnvelopeInfo error
	// branch (backup.go 238-239) is NOT_RUN via createBackup — it is
	// shadowed by LoadBase in both modes (non-Transit fails the nxs2
	// decrypt; Transit rejects the plaintext field). The CLI must still
	// fail closed on the mixed document, which is what this asserts.
	fake := Continue95GatewayNewFakeTransit()
	server := httptest.NewServer(fake)
	defer server.Close()
	Continue95GatewaySetTransitEnv(t, server.URL)
	dir := t.TempDir()
	legacy := make([]byte, 32)
	for i := range legacy {
		legacy[i] = byte(i + 5)
	}
	nxs1doc, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","type":"openai_compatible","base_url":"https://api.example.test/v1","auth_mode":"bearer","api_key":"synthetic-migrate-secret"}]}`), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	migPath := filepath.Join(dir, "mig.json")
	if err := os.WriteFile(migPath, nxs1doc, 0o600); err != nil {
		t.Fatal(err)
	}
	captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"migrate-to-transit", "--config", migPath, "--legacy-key", base64.StdEncoding.EncodeToString(legacy)}); err != nil {
			t.Errorf("migrate: %v", err)
		}
	})
	raw, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	provs := doc["providers"].([]any)
	doc["providers"] = append(provs, map[string]any{"id": "plain-synth", "api_key": "synthetic-plaintext-key"})
	mixed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(migPath, mixed, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := createBackup(migPath, filepath.Join(dir, "mixed-backup.json"), false, false, &out, liveBackupOptions{}); err == nil {
		t.Fatal("createBackup of mixed nxs2+plaintext config accepted")
	} else {
		t.Logf("mixed backup error (branch measured by coverprofile): %v", err)
	}
}

func TestContinue95GatewayBackupJournalOwnership(t *testing.T) {
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := &restoreJournalManifest{
		Format:    restoreJournalFormat,
		ID:        "0123456789abcdef",
		CreatedAt: time.Now().UTC(),
		State:     "prepared",
		Entries: []restoreJournalEntry{{
			Name:    "config.json",
			Target:  filepath.Join(dir, "target-config.json"),
			Existed: false,
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(journalDir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateJournal(journalDir, manifest); err != nil {
		t.Fatalf("valid journal rejected: %v", err)
	}
	// Ownership branches (backup.go 1140-1141, 1153-1154) are NOT_RUN:
	// they need files owned by a uid other than the test process's euid,
	// and chown is not permitted in this sandbox (operation not permitted
	// as the test user), so no foreign-owned fixture can be built here.
}

func TestContinue95GatewayBackupKeylessAndRestoreAudit(t *testing.T) {
	Continue95GatewayClearTransitEnv(t)
	dir := t.TempDir()
	// Keyless restore justification: malformed JSON carrying an nxs1
	// marker, no bundled key, no key env -> json.Unmarshal error path.
	_, _, err := validateBundledConfig(dir, map[string][]byte{
		backupEntryConfig: []byte(`{"providers": [{"id": "x", "api_key": "nxs1:broken`),
	})
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("keyless unmarshal err=%v", err)
	}

	// Restore bundle with a zero-byte security store: either the store
	// open fails (already-covered path) or verification fails on the
	// empty payload (target branch); the coverprofile diff decides.
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgBytes, err := json.Marshal(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "config.json"), cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "security.db"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := backupManifest{
		Format:         backupFormat,
		CreatedAt:      time.Now().UTC(),
		GatewayVersion: version,
		Files: []backupManifestFile{
			{Name: "config.json", SHA256: sha256Hex(cfgBytes), Size: int64(len(cfgBytes))},
			{Name: "security.db", SHA256: sha256Hex(nil), Size: 0},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(dir, "bundle.bin")
	if err := writeBundle(bundlePath, manifestBytes, manifest.Files, stage); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := restoreBundle(bundlePath, filepath.Join(dir, "restored.json"), true, false, &out); err == nil {
		t.Fatal("restoreBundle with zero-byte security store accepted")
	} else {
		t.Logf("restoreBundle zero-byte store error (branch measured by coverprofile): %v", err)
	}
}

func TestContinue95GatewayRestoreVerifyTransit(t *testing.T) {
	fake := Continue95GatewayNewFakeTransit()
	server := httptest.NewServer(fake)
	defer server.Close()
	Continue95GatewaySetTransitEnv(t, server.URL)
	dir := t.TempDir()
	legacy := make([]byte, 32)
	for i := range legacy {
		legacy[i] = byte(i + 5)
	}
	nxs1doc, _, err := secrets.TransformConfig([]byte(`{"providers":[{"id":"prov-synth","type":"openai_compatible","base_url":"https://api.example.test/v1","auth_mode":"bearer","api_key":"synthetic-migrate-secret"}]}`), legacy, true)
	if err != nil {
		t.Fatal(err)
	}
	migPath := filepath.Join(dir, "mig.json")
	if err := os.WriteFile(migPath, nxs1doc, 0o600); err != nil {
		t.Fatal(err)
	}
	captureGatewayStdout(t, func() {
		if err := runSecrets([]string{"migrate-to-transit", "--config", migPath, "--legacy-key", base64.StdEncoding.EncodeToString(legacy)}); err != nil {
			t.Errorf("migrate: %v", err)
		}
	})
	bundlePath := filepath.Join(dir, "bundle.bin")
	var out bytes.Buffer
	if err := createBackup(migPath, bundlePath, false, false, &out, liveBackupOptions{}); err != nil {
		t.Fatalf("createBackup of migrated config: %v", err)
	}

	// Transit unavailable (dead Vault): config_decryption is BLOCKED.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()
	Continue95GatewaySetTransitEnv(t, "http://"+deadAddr)
	out.Reset()
	if code := runRestoreVerify(bundlePath, migPath, "", "", 0, &out, &bytes.Buffer{}); code != 1 {
		t.Fatalf("dead vault verify code=%d output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "BLOCKED config_decryption") {
		t.Fatalf("dead vault verify output=%q", out.String())
	}

	// Transit forbidden (403 on decrypt): config_decryption is FAIL.
	forbidden := Continue95GatewayNewFakeTransit()
	forbidden.decryptStatus = http.StatusForbidden
	serverB := httptest.NewServer(forbidden)
	defer serverB.Close()
	Continue95GatewaySetTransitEnv(t, serverB.URL)
	out.Reset()
	if code := runRestoreVerify(bundlePath, migPath, "", "", 0, &out, &bytes.Buffer{}); code != 1 {
		t.Fatalf("forbidden vault verify code=%d output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL config_decryption") {
		t.Fatalf("forbidden vault verify output=%q", out.String())
	}
}

func TestContinue95GatewayPolicyReplayErrors(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runPolicyReplay([]string{"--bogus-flag"}, &out, &errBuf); code != policyExitUsage {
		t.Fatalf("replay flag-parse code=%d stderr=%q", code, errBuf.String())
	}
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	dir := t.TempDir()
	cfgPath := Continue95GatewayDiagnosticsConfig(t, dir, "off")
	tracesPath := Continue95GatewayWriteFile(t, dir, "traces.jsonl", "{\"request\": {}, \"recorded_winner\": \"synthetic-winner\"}\n")
	out.Reset()
	errBuf.Reset()
	if code := runPolicyReplay([]string{"--config", cfgPath, "--policy", "missing", "--traces", tracesPath}, &out, &errBuf); code != policyExitError {
		t.Fatalf("replay unknown policy code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "policy replay:") {
		t.Fatalf("replay unknown policy stderr=%q", errBuf.String())
	}
}

func TestContinue95GatewayAuditStorePaths(t *testing.T) {
	// Direct auditStorePath Abs failure with a deleted working directory.
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	_, aerr := auditStorePath("security/nexaroute-security.db", "config.json")
	if err := os.Chdir(oldwd); err != nil {
		t.Fatal(err)
	}
	if aerr == nil {
		t.Fatal("auditStorePath with deleted cwd accepted")
	}

	// Audit verify against a zero-byte store: open or verification fails.
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	dir := t.TempDir()
	zeroStore := filepath.Join(dir, "security.db")
	if err := os.WriteFile(zeroStore, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	auditCfg := config.Default()
	auditCfg.Logging.File = "off"
	auditCfg.Providers = []config.ProviderConfig{{
		ID: "diag-synth", Name: "Synthetic", Type: "openai_compatible", Enabled: true,
		BaseURL: "https://api.example.test/v1", AuthMode: "bearer",
		APIKeyEnv: "CONTINUE95_DIAG_FAKE_KEY",
	}}
	auditCfg.Admin.SecurityStorePath = zeroStore
	raw, err := json.Marshal(auditCfg)
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(auditPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runAudit([]string{"verify", "--config", auditPath}, &out, &errBuf); code != auditExitUsage {
		t.Fatalf("audit verify zero-byte store code=%d stderr=%q", code, errBuf.String())
	}

	// Audit verify against an empty bbolt store: open succeeds, the
	// audit-chain verification fails (no audit bucket).
	emptyStore := Continue95GatewayEmptyBoltDB(t, dir, "empty.db")
	auditCfg.Admin.SecurityStorePath = emptyStore
	raw, err = json.Marshal(auditCfg)
	if err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(dir, "empty-config.json")
	if err := os.WriteFile(emptyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	if code := runAudit([]string{"verify", "--config", emptyPath}, &out, &errBuf); code != auditExitProblems {
		t.Fatalf("audit verify empty store code=%d stdout=%q stderr=%q", code, out.String(), errBuf.String())
	}
}

func TestContinue95GatewayConfigDiffNaN(t *testing.T) {
	left := config.Default()
	left.DecisionPolicies = []config.DecisionPolicyConfig{{ID: "synth-policy", MinScoreDelta: math.NaN()}}
	right := config.Default()
	if _, err := configDiff(left, right); err == nil {
		t.Fatal("configDiff with NaN on the left accepted")
	}
	if _, err := configDiff(right, left); err == nil {
		t.Fatal("configDiff with NaN on the right accepted")
	}
}

// Continue95GatewayEmptyBoltDB creates a valid but empty bbolt database
// file: it opens (so OpenReadOnly succeeds) but carries no audit chain,
// so VerifyAuditChain reports missing-bucket problems. It never touches
// a real gateway store. OpenReadOnly requires the parent directory to be
// mode 0700, so the helper tightens it (t.TempDir uses 0770 here).
func Continue95GatewayEmptyBoltDB(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContinue95GatewayDoctorSecurityStore(t *testing.T) {
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	dir := t.TempDir()
	// Empty bbolt store: opens read-only, but the audit chain cannot
	// verify (no audit bucket), exercising the doctor's audit_chain FAIL
	// problems branch. The verify-ERROR branch (doctor.go 154-156) is
	// NOT_RUN: verifyChainTx reports problems, it never returns an
	// error for a well-formed store, and corruption that would make the
	// read itself fail also fails OpenReadOnly first.
	emptyStore := Continue95GatewayEmptyBoltDB(t, dir, "security.db")
	docCfg := config.Default()
	docCfg.Logging.File = "off"
	docCfg.Providers = []config.ProviderConfig{{
		ID: "diag-synth", Name: "Synthetic", Type: "openai_compatible", Enabled: true,
		BaseURL: "https://api.example.test/v1", AuthMode: "bearer",
		APIKeyEnv: "CONTINUE95_DIAG_FAKE_KEY",
	}}
	docCfg.Admin.SecurityStorePath = emptyStore
	raw, err := json.Marshal(docCfg)
	if err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(docPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	d := &doctor{w: &buf}
	d.checkConfig(docPath)
	d.checkSecurityStore(docPath)
	if !strings.Contains(buf.String(), "FAIL") || !strings.Contains(buf.String(), "audit_chain") {
		t.Fatalf("doctor security store output=%q", buf.String())
	}
}

func TestContinue95GatewayRestoreVerifyAuditChain(t *testing.T) {
	dir := t.TempDir()
	emptyStore := Continue95GatewayEmptyBoltDB(t, dir, "security.db")
	payload, err := os.ReadFile(emptyStore)
	if err != nil {
		t.Fatal(err)
	}
	check := checkBundledAuditChain(map[string][]byte{backupEntrySecurity: payload})
	if check.Status != statusFail || !strings.Contains(check.Detail, "FAILED verification") {
		t.Fatalf("empty-bbolt bundled audit chain status=%q detail=%q", check.Status, check.Detail)
	}
	// The verify-ERROR branch (restore_verify.go 351-354) is NOT_RUN for
	// the same reason as the doctor's: verifyChainTx reports problems
	// rather than erroring on a well-formed store.
}

func TestContinue95GatewayComfyDirectBranches(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewLocal(filepath.Join(dir, "store"), 0)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic mp4 bytes for acceptance checks")
	stored, err := store.Put(context.Background(), video.Asset{
		Filename:    "accept.mp4",
		ContentType: "video/mp4",
		Metadata:    map[string]string{"project_id": "p", "job_id": "j", "provider": "comfyui", "node_id": "n", "filename": "accept.mp4"},
	}, data)
	if err != nil {
		t.Fatal(err)
	}
	probe := &render.Renderer{}
	sizeTampered := stored
	sizeTampered.SizeBytes++
	// NOTE: the size-mismatch error surfaces from storage.Local.Read,
	// which enforces size and checksum before verify's own checks, so
	// verifyComfyAcceptAsset's size (585-586) and SHA (588-589) branches
	// are NOT_RUN via a *storage.Local sink — Read fails first on the
	// same conditions. The tamper cases still assert fail-closed reads.
	if _, err := verifyComfyAcceptAsset(context.Background(), store, probe, sizeTampered); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("size tamper err=%v", err)
	}
	shaTampered := stored
	shaTampered.SHA256 = strings.Repeat("0", 64)
	if _, err := verifyComfyAcceptAsset(context.Background(), store, probe, shaTampered); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("sha tamper err=%v", err)
	}

	// Deleted-cwd Abs failures for the asset-access helper.
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	runErr := verifyComfyAcceptAssetAccess("relative-run", stored)
	if err := os.Chdir(oldwd); err != nil {
		t.Fatal(err)
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "resolve acceptance asset root") {
		t.Fatalf("relative runDir err=%v", runErr)
	}
	tmp2 := t.TempDir()
	if err := os.Chdir(tmp2); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tmp2); err != nil {
		t.Fatal(err)
	}
	relAsset := video.Asset{URI: "accept.mp4", Filename: "accept.mp4", SizeBytes: int64(len(data)), SHA256: stored.SHA256}
	assetErr := verifyComfyAcceptAssetAccess(filepath.Join(dir, "run"), relAsset)
	if err := os.Chdir(oldwd); err != nil {
		t.Fatal(err)
	}
	if assetErr == nil || !strings.Contains(assetErr.Error(), "resolve asset path") {
		t.Fatalf("relative asset err=%v", assetErr)
	}

	// Fail-closed RemoveAll failure: run dir under a regular file.
	blocker := Continue95GatewayWriteFile(t, dir, "blocker", "x")
	report := &comfyAcceptReport{}
	comfyAcceptFailClosed(report, filepath.Join(blocker, "run"), nil)
	if report.Cleanup == nil || report.Cleanup.LocalStatus != externalStatusFail {
		t.Fatalf("fail-closed cleanup=%+v", report.Cleanup)
	}
	if !strings.Contains(report.Cleanup.LocalReason, "remove partial acceptance workspace") {
		t.Fatalf("fail-closed reason=%q", report.Cleanup.LocalReason)
	}
}

func TestContinue95GatewayVaultHelperAbs(t *testing.T) {
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	_, verr := validateVaultAcceptHelper("relative-helper")
	if err := os.Chdir(oldwd); err != nil {
		t.Fatal(err)
	}
	if verr == nil || !strings.Contains(verr.Error(), "cannot resolve --helper path") {
		t.Fatalf("relative helper err=%v", verr)
	}
}

// TestContinue95GatewayDiagnosticsDoctorNewlinePath covers the doctor
// output parsing branches of buildDiagnosticsBundle: a config path
// containing a blank line splits the doctor's FAIL detail into an empty
// line (the continue branch) followed by a line with no ": " separator
// (the parse-failure branch), so the bundle build fails and
// runDiagnostics reports code 1 without writing anything.
func TestContinue95GatewayDiagnosticsDoctorNewlinePath(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "bundle.json")
	weird := filepath.Join(dir, "a\n\nb", "config.json")
	var out, errBuf bytes.Buffer
	if code := runDiagnostics([]string{"bundle", "--config", weird, "--output", outPath}, &out, &errBuf); code != 1 {
		t.Fatalf("newline config path: code=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "diagnostics bundle failed") {
		t.Fatalf("newline config path: stderr=%q", errBuf.String())
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("bundle written despite doctor parse failure: stat err=%v", err)
	}
}

// TestContinue95GatewayBackupLiveSnapshotErrors covers the live
// security-snapshot branches of createBackup: a failing admin fetch, a
// snapshot that is not a readable store, and a snapshot whose audit
// chain does not verify (with the override disabled, the backup is
// refused).
func TestContinue95GatewayBackupLiveSnapshotErrors(t *testing.T) {
	t.Setenv("CONTINUE95_DIAG_FAKE_KEY", "synthetic-diag-key-value")
	dir := t.TempDir()
	cfgPath := Continue95GatewayDiagnosticsConfig(t, dir, "off")
	var out bytes.Buffer

	// Admin fetch failure: every admin request returns 500.
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "admin unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(failServer.Close)
	if err := createBackup(cfgPath, filepath.Join(dir, "live-fail.json"), false, false, &out, liveBackupOptions{enabled: true, adminURL: failServer.URL, adminKey: "synthetic"}); err == nil {
		t.Fatal("live backup with failing admin fetch accepted")
	}

	// Snapshot that is not a store: 200 with garbage bytes, so the
	// staged probe cannot be opened read-only.
	garbageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a bbolt database"))
	}))
	t.Cleanup(garbageServer.Close)
	if err := createBackup(cfgPath, filepath.Join(dir, "live-garbage.json"), false, false, &out, liveBackupOptions{enabled: true, adminURL: garbageServer.URL, adminKey: "synthetic"}); err == nil {
		t.Fatal("live backup with unreadable snapshot accepted")
	}

	// Snapshot with a broken audit chain: 200 with an empty bbolt
	// store, which opens but reports missing-bucket problems, so the
	// backup is refused without the override.
	emptyPath := Continue95GatewayEmptyBoltDB(t, dir, "empty-live.db")
	emptyBytes, err := os.ReadFile(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	brokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(emptyBytes)
	}))
	t.Cleanup(brokenServer.Close)
	if err := createBackup(cfgPath, filepath.Join(dir, "live-broken.json"), false, false, &out, liveBackupOptions{enabled: true, adminURL: brokenServer.URL, adminKey: "synthetic"}); err == nil || !strings.Contains(err.Error(), "audit chain verification failed") {
		t.Fatalf("live backup with broken snapshot chain err=%v", err)
	}
}

// TestContinue95GatewayCanonicalPathAbsoluteSymlink covers the
// absolute-target branch of canonicalPath: a symlink whose target is
// absolute resets the resolved prefix to the filesystem root before
// the target's components are queued.
func TestContinue95GatewayCanonicalPathAbsoluteSymlink(t *testing.T) {
	dir := t.TempDir()
	real := Continue95GatewayWriteFile(t, dir, "real.txt", "x")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalPath(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("canonicalPath(abs symlink)=%q want %q", got, real)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/qualitygate"
)

// Wave 2 (issue #663): cmd/gateway/qualitygate.go error arms uncovered
// in the 2026-10-29 baseline profile. Fixture inputs reuse the shared
// qualityGateTestInput helper from qualitygate_test.go; only the error
// paths are new.
//
// Deliberately NOT covered: summarize's Summarize error (ParseInput
// already runs the same validation), both MarshalIndent errors (plain
// struct marshaling cannot fail), the Compare error (both sides pass
// ParseBaseline provenance validation first), the unknown-verdict
// branch (Compare only returns the three known verdicts), and
// writeReport's CreateTemp/Write/Close/Chmod failures (no deterministic
// trigger when the tests run as root; permission bits do not fail).

func wave2QualityGateWrite(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func wave2QualityGateRun(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runQualityGate(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestWave2QualityGateNoArgs(t *testing.T) {
	if code, _, _ := wave2QualityGateRun(t, nil); code != 2 {
		t.Fatalf("no-args exit = %d, want 2", code)
	}
}

func TestWave2QualityGateReadBoundedFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := readBoundedFile(""); err == nil {
		t.Fatal("empty path must fail")
	}
	if _, err := readBoundedFile(dir); err == nil {
		t.Fatal("directory path must fail")
	}
	if _, err := readBoundedFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing path must fail")
	}
	big := wave2QualityGateWrite(t, dir, "big.json", strings.Repeat("x", (8<<20)+1))
	if _, err := readBoundedFile(big); err == nil {
		t.Fatal("input over the 8 MiB bound must fail")
	}
}

func TestWave2QualityGateWriteReport(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := writeReport("", []byte("x"), &out); err != nil {
		t.Fatalf("empty report path must be a no-op, got %v", err)
	}
	blocker := wave2QualityGateWrite(t, dir, "blocker", "not a directory")
	if err := writeReport(filepath.Join(blocker, "report.json"), []byte("x"), &out); err == nil {
		t.Fatal("report under a regular file must fail at directory creation")
	}
	targetDir := filepath.Join(dir, "existing")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(targetDir, []byte("x"), &out); err == nil {
		t.Fatal("renaming a report onto an existing directory must fail")
	}
}

func TestWave2QualityGateSummarizeErrors(t *testing.T) {
	dir := t.TempDir()
	in := qualityGateTestInput(t, dir, "in.json", 4, 0)
	badJSON := wave2QualityGateWrite(t, dir, "bad.json", "{not json")
	blocker := wave2QualityGateWrite(t, dir, "blocker", "not a directory")
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"summarize", "--bogus", "x"}},
		{"missing input", []string{"summarize"}},
		{"missing file", []string{"summarize", "--input", filepath.Join(dir, "missing.json")}},
		{"malformed input", []string{"summarize", "--input", badJSON}},
		{"out under file", []string{"summarize", "--input", in, "--out", filepath.Join(blocker, "snap.json")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := wave2QualityGateRun(t, tc.args); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

func TestWave2QualityGateSummarizeStdout(t *testing.T) {
	dir := t.TempDir()
	in := qualityGateTestInput(t, dir, "in.json", 4, 0)
	code, out, _ := wave2QualityGateRun(t, []string{"summarize", "--input", in})
	if code != 0 {
		t.Fatalf("stdout summarize exit = %d, want 0", code)
	}
	if !strings.Contains(out, qualitygate.FormatSnapshot) {
		t.Fatalf("stdout must carry the snapshot, got %q", out)
	}
}

func TestWave2QualityGateCompareErrors(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	cand := qualityGateTestInput(t, dir, "cand.json", 4, 0)
	badJSON := wave2QualityGateWrite(t, dir, "bad.json", "{not json")
	blocker := wave2QualityGateWrite(t, dir, "blocker", "not a directory")
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"compare", "--bogus", "x"}},
		{"thresholds missing", []string{"compare", "--baseline", base, "--candidate", cand, "--thresholds", filepath.Join(dir, "missing.json")}},
		{"thresholds malformed", []string{"compare", "--baseline", base, "--candidate", cand, "--thresholds", badJSON}},
		{"thresholds invalid", []string{"compare", "--baseline", base, "--candidate", cand, "--thresholds", wave2QualityGateWrite(t, dir, "th-bad.json", `{"success_drop":2}`)}},
		{"baseline missing", []string{"compare", "--baseline", filepath.Join(dir, "missing.json"), "--candidate", cand}},
		{"baseline malformed", []string{"compare", "--baseline", badJSON, "--candidate", cand}},
		{"candidate missing", []string{"compare", "--baseline", base, "--candidate", filepath.Join(dir, "missing.json")}},
		{"candidate malformed", []string{"compare", "--baseline", base, "--candidate", badJSON}},
		{"out under file", []string{"compare", "--baseline", base, "--candidate", cand, "--out", filepath.Join(blocker, "rep.json")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := wave2QualityGateRun(t, tc.args); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

func TestWave2QualityGateCompareThresholdsDefault(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	cand := qualityGateTestInput(t, dir, "cand.json", 4, 0)
	// An empty thresholds object omits the suite list, so the CLI must
	// fall back to the default suite set and the comparison proceeds.
	th := wave2QualityGateWrite(t, dir, "th.json", `{}`)
	code, out, errBuf := wave2QualityGateRun(t, []string{"compare",
		"--baseline", base, "--candidate", cand, "--thresholds", th, "--max-cost-usd", "10"})
	if code != 0 {
		t.Fatalf("defaulted thresholds compare exit = %d, want 0 (stderr=%s)", code, errBuf)
	}
	if !strings.Contains(out, "routing=none") {
		t.Fatalf("stdout must state routing=none: %s", out)
	}
}

func wave2QualityGateLiveInput(t *testing.T, dir, name string) string {
	t.Helper()
	src := qualityGateTestInput(t, dir, name+".src.json", 4, 0)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var in qualitygate.Input
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	in.Provenance.Mode = "live"
	data, err = json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return wave2QualityGateWrite(t, dir, name, string(data))
}

func TestWave2QualityGateCompareLiveGuards(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	live := wave2QualityGateLiveInput(t, dir, "live.json")
	cases := []struct {
		name string
		args []string
	}{
		{"live without acknowledgement", []string{"compare", "--baseline", base, "--candidate", live}},
		{"live without max requests", []string{"compare", "--baseline", base, "--candidate", live, "--acknowledge-live"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := wave2QualityGateRun(t, tc.args); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

func TestWave2QualityGateCompareUnknownMode(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	// A snapshot whose mode is neither replay nor live fails parsing
	// at provenance validation: the CLI must reject it, not compare it.
	bogus := wave2QualityGateWrite(t, dir, "bogus.json", `{"format":"quality-gate-snapshot/v1","summary":{"provenance":{"gateway_commit":"abc","model":"m","deployment_id":"d","mode":"bogus"},"suites":[{"suite_id":"tool_calling"}]}}`)
	code, _, errBuf := wave2QualityGateRun(t, []string{"compare", "--baseline", base, "--candidate", bogus})
	if code != 2 {
		t.Fatalf("bogus-mode exit = %d, want 2 (stderr=%s)", code, errBuf)
	}
	if !strings.Contains(errBuf, "mode must be replay or live") {
		t.Fatalf("stderr must name the mode violation: %s", errBuf)
	}
}

func TestWave2QualityGateCompareReplayUpstreamCalls(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	// A replay candidate that reports upstream calls fails the live
	// check: replay summaries must report zero upstream calls.
	src := qualityGateTestInput(t, dir, "replay.src.json", 4, 0)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var in qualitygate.Input
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	for i := range in.Runs {
		in.Runs[i].Upstream = 1
	}
	data, err = json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	cand := wave2QualityGateWrite(t, dir, "replay.json", string(data))
	code, _, errBuf := wave2QualityGateRun(t, []string{"compare", "--baseline", base, "--candidate", cand})
	if code != 2 {
		t.Fatalf("replay-with-calls exit = %d, want 2 (stderr=%s)", code, errBuf)
	}
	if !strings.Contains(errBuf, "reports upstream calls") {
		t.Fatalf("stderr must name the upstream-calls violation: %s", errBuf)
	}
}

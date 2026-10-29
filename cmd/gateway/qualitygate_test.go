package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/qualitygate"
)

func qualityGateTestInput(t *testing.T, dir, name string, pass, fail int) string {
	t.Helper()
	var runs []eval.Result
	for _, suiteID := range qualitygate.DefaultSuites {
		var cases []eval.CaseResult
		counts := map[eval.Verdict]int{}
		add := func(n int, v eval.Verdict) {
			for i := 0; i < n; i++ {
				cases = append(cases, eval.CaseResult{CaseID: suiteID + "-case", Verdict: v, Resolution: "deterministic", Weight: 1, LatencyMS: 100, TTFTMS: 20})
				counts[v]++
			}
		}
		add(pass, eval.VerdictPass)
		add(fail, eval.VerdictFail)
		score := 0.0
		if pass+fail > 0 {
			score = float64(pass) / float64(pass+fail)
		}
		runs = append(runs, eval.Result{
			RunID: suiteID + "-run", SuiteID: suiteID, SuiteVersion: "1",
			DeploymentID: "dep-1", ProviderID: "prov-1", Model: "model-1",
			StartedAt:  time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 10, 27, 12, 0, 1, 0, time.UTC),
			Cases:      cases, Counts: counts, Score: score, Samples: pass + fail,
			Scoreable: pass+fail >= 3, Evaluators: []string{"exact_match"},
		})
	}
	in := qualitygate.Input{
		Format: qualitygate.FormatInput,
		Provenance: qualitygate.Provenance{
			GatewayCommit: "e6c3cdf3", Model: "model-1", DeploymentID: "dep-1",
			ProviderID: "prov-1", Mode: "replay",
			GeneratedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
		},
		Runs: runs,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQualityGateCLI_CompareExitCodes(t *testing.T) {
	dir := t.TempDir()
	base := qualityGateTestInput(t, dir, "base.json", 4, 0)
	same := qualityGateTestInput(t, dir, "same.json", 4, 0)
	// Regression candidate: tool_calling drops to 1/4 success.
	regressed := qualityGateTestInput(t, dir, "regressed.json", 4, 0)
	// Rewrite the tool_calling run in the regressed file with 1 pass / 3 fail.
	data, err := os.ReadFile(regressed)
	if err != nil {
		t.Fatal(err)
	}
	var in qualitygate.Input
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	for i, r := range in.Runs {
		if r.SuiteID != "tool_calling" {
			continue
		}
		var cases []eval.CaseResult
		for j := 0; j < 4; j++ {
			v := eval.VerdictFail
			if j == 0 {
				v = eval.VerdictPass
			}
			cases = append(cases, eval.CaseResult{CaseID: "tool_calling-case", Verdict: v, Resolution: "deterministic", Weight: 1, LatencyMS: 100, TTFTMS: 20})
		}
		in.Runs[i].Cases = cases
		in.Runs[i].Counts = map[eval.Verdict]int{eval.VerdictPass: 1, eval.VerdictFail: 3}
		in.Runs[i].Score = 0.25
		in.Runs[i].Samples = 4
	}
	data, err = json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regressed, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Insufficient candidate: drop the cancellation suite.
	// Use a fresh variable: unmarshalling into the regressed input
	// would keep its stale verdict-count map keys.
	insufficient := qualityGateTestInput(t, dir, "insufficient.json", 4, 0)
	data, err = os.ReadFile(insufficient)
	if err != nil {
		t.Fatal(err)
	}
	var in2 qualitygate.Input
	if err := json.Unmarshal(data, &in2); err != nil {
		t.Fatal(err)
	}
	var kept []eval.Result
	for _, r := range in2.Runs {
		if r.SuiteID != "cancellation" {
			kept = append(kept, r)
		}
	}
	in2.Runs = kept
	data, err = json.Marshal(in2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(insufficient, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		cand string
		want int
	}{
		{"no regression", same, 0},
		{"regression", regressed, 1},
		{"insufficient", insufficient, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			code := runQualityGate([]string{"compare", "--baseline", base, "--candidate", tc.cand, "--out", filepath.Join(dir, tc.name+".report.json")}, &out, &errBuf)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d (stdout=%s stderr=%s)", code, tc.want, out.String(), errBuf.String())
			}
			if !strings.Contains(out.String(), "routing=none") {
				t.Fatalf("stdout must state routing=none: %s", out.String())
			}
			repData, err := os.ReadFile(filepath.Join(dir, tc.name+".report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var rep qualitygate.Report
			if err := json.Unmarshal(repData, &rep); err != nil {
				t.Fatal(err)
			}
			if rep.RoutingChanged {
				t.Fatal("report must never claim a routing change")
			}
		})
	}

	// Usage error: missing candidate.
	var out, errBuf bytes.Buffer
	if code := runQualityGate([]string{"compare", "--baseline", base}, &out, &errBuf); code != 2 {
		t.Fatalf("missing candidate exit = %d, want 2", code)
	}
	// Help.
	if code := runQualityGate([]string{"help"}, &out, &errBuf); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
}

func TestQualityGateCLI_Summarize(t *testing.T) {
	dir := t.TempDir()
	in := qualityGateTestInput(t, dir, "in.json", 4, 0)
	outPath := filepath.Join(dir, "snap.json")
	var out, errBuf bytes.Buffer
	if code := runQualityGate([]string{"summarize", "--input", in, "--out", outPath}, &out, &errBuf); code != 0 {
		t.Fatalf("summarize exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var snap qualitygate.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Format != qualitygate.FormatSnapshot || len(snap.Summary.Suites) != len(qualitygate.DefaultSuites) {
		t.Fatalf("snapshot format=%q suites=%d", snap.Format, len(snap.Summary.Suites))
	}
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/qualitygate"
)

// runQualityGate implements `nexaroute quality-gate`.
//
// It compares recorded evaluation evidence only: it reads files, it
// makes no network call, it runs no model and it never changes routing.
// A live candidate is evidence already produced through the opt-in live
// evaluation path; this command only checks its ceilings, and only
// when the operator explicitly acknowledges provider cost.
//
// Exit codes mirror the report verdicts, so CI can distinguish them:
//
//	0 no_regression, 1 regression, 3 insufficient_evidence, 2 usage/setup.
func runQualityGate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, qualityGateUsage)
		return 2
	}
	switch args[0] {
	case "compare":
		return runQualityGateCompare(args[1:], stdout, stderr)
	case "summarize":
		return runQualityGateSummarize(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, qualityGateUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "nexaroute: unknown quality-gate subcommand %q\n", args[0])
		fmt.Fprint(stderr, qualityGateUsage)
		return 2
	}
}

const qualityGateUsage = `usage:
  nexaroute quality-gate compare --baseline PATH --candidate PATH [--thresholds PATH] [--max-live-requests N] [--max-cost-usd N] [--acknowledge-live] [--out PATH]
  (baseline and candidate each accept a gate input or a snapshot file)
  nexaroute quality-gate summarize --input PATH [--out PATH]

Compares a candidate model/deployment against a recorded baseline using
the deterministic evaluation suites (tool calls, structured JSON,
multi-turn, streaming, cancellation, protocol compatibility). The
default mode is offline replay: inputs are recorded eval runs and no
upstream call is made. A live candidate is accepted only with
--acknowledge-live and within --max-live-requests / --max-cost-usd.

The report changes no routing and no model weights: RoutingAction is
always "none". Acting on a report is an operator decision.

Exit codes (compare): 0 no regression, 1 regression, 3 insufficient
evidence (missing or unscoreable suite, or a suite version mismatch),
2 usage or setup error. A report is written to --out when given, and a
one-line verdict is always printed.
`

func readBoundedFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > qualitygate.MaxGateInputBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte gate input bound", path, qualitygate.MaxGateInputBytes)
	}
	return os.ReadFile(path)
}

func writeReport(path string, data []byte, stdout io.Writer) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	// Write atomically through a temp file in the same directory so a
	// failed write never leaves a half-written report behind.
	tmp, err := os.CreateTemp(dir, ".quality-gate-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	fmt.Fprintf(stdout, "report written to %s\n", path)
	return nil
}

func runQualityGateSummarize(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quality-gate summarize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	inputPath := fs.String("input", "", "gate input JSON (quality-gate-input/v1)")
	outPath := fs.String("out", "", "snapshot output path (quality-gate-snapshot/v1)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *inputPath == "" {
		fmt.Fprintln(stderr, "nexaroute: quality-gate summarize requires --input")
		return 2
	}
	data, err := readBoundedFile(*inputPath)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate summarize:", err)
		return 2
	}
	in, err := qualitygate.ParseInput(data)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate summarize:", err)
		return 2
	}
	summary, err := qualitygate.Summarize(in)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate summarize:", err)
		return 2
	}
	out, err := qualitygate.MarshalSnapshot(summary)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate summarize:", err)
		return 2
	}
	if *outPath != "" {
		if err := writeReport(*outPath, append(out, '\n'), stdout); err != nil {
			fmt.Fprintln(stderr, "nexaroute: quality-gate summarize:", err)
			return 2
		}
		return 0
	}
	stdout.Write(append(out, '\n'))
	return 0
}

func runQualityGateCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quality-gate compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baselinePath := fs.String("baseline", "", "baseline snapshot or gate input JSON")
	candidatePath := fs.String("candidate", "", "candidate snapshot or gate input JSON")
	thresholdsPath := fs.String("thresholds", "", "optional thresholds JSON")
	maxLiveRequests := fs.Int("max-live-requests", 0, "live request ceiling (required for live candidates)")
	maxCostUSD := fs.Float64("max-cost-usd", 0, "live cost ceiling in USD (0 = no cost ceiling)")
	acknowledgeLive := fs.Bool("acknowledge-live", false, "confirm a live candidate may have spent provider quota")
	outPath := fs.String("out", "", "report output path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *baselinePath == "" || *candidatePath == "" {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare requires --baseline and --candidate")
		return 2
	}
	thresholds := qualitygate.DefaultThresholds()
	if *thresholdsPath != "" {
		data, err := readBoundedFile(*thresholdsPath)
		if err != nil {
			fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
			return 2
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		var th qualitygate.Thresholds
		if err := dec.Decode(&th); err != nil {
			fmt.Fprintln(stderr, "nexaroute: quality-gate compare: invalid thresholds:", err)
			return 2
		}
		// A thresholds file may tune numbers only; an omitted suite
		// list means the default gate suite set, not an empty gate.
		if len(th.RequiredSuites) == 0 {
			th.RequiredSuites = qualitygate.DefaultThresholds().RequiredSuites
		}
		if err := th.Validate(); err != nil {
			fmt.Fprintln(stderr, "nexaroute: quality-gate compare: invalid thresholds:", err)
			return 2
		}
		thresholds = th
	}
	baseData, err := readBoundedFile(*baselinePath)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	base, err := qualitygate.ParseBaseline(baseData)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	candData, err := readBoundedFile(*candidatePath)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	// A candidate may arrive as a recorded input or as a snapshot
	// produced by `summarize`; both carry the same comparable summary.
	cand, err := qualitygate.ParseBaseline(candData)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	// Live ceilings are enforced before comparison, and only a live
	// candidate needs acknowledgement. Replay candidates are checked
	// too: a replay summary must report zero upstream calls.
	if err := qualitygate.RequireLiveAcknowledgement(cand.Provenance.Mode, *acknowledgeLive); err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	limits := qualitygate.LiveLimits{MaxRequests: *maxLiveRequests}
	if *maxCostUSD > 0 {
		limits.MaxCostUSD = &[]float64{*maxCostUSD}[0]
	}
	if cand.Provenance.Mode == "live" && *maxLiveRequests <= 0 {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare: live candidates require --max-live-requests")
		return 2
	}
	if err := qualitygate.CheckLive(cand, limits); err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	rep, err := qualitygate.Compare(base, cand, thresholds)
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
		return 2
	}
	if *outPath != "" {
		if err := writeReport(*outPath, append(data, '\n'), stdout); err != nil {
			fmt.Fprintln(stderr, "nexaroute: quality-gate compare:", err)
			return 2
		}
	}
	fmt.Fprintf(stdout, "quality-gate: %s (baseline %s, candidate %s, regressions=%d, improvements=%d, routing=none)\n",
		rep.Overall, rep.Baseline.GatewayCommit, rep.Candidate.GatewayCommit, len(rep.Regressions), len(rep.Improvements))
	switch rep.Overall {
	case qualitygate.OverallNoRegression:
		return 0
	case qualitygate.OverallRegression:
		return 1
	case qualitygate.OverallInsufficientEvidence:
		return 3
	default:
		fmt.Fprintln(stderr, "nexaroute: quality-gate compare: unknown overall verdict", rep.Overall)
		return 2
	}
}

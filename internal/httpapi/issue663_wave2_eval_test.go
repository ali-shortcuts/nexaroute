package httpapi

// Wave-2 coverage for evaluation-plane state loading (issue #663,
// continuing PR #730). Targets the merged-tree profile's uncovered
// blocks in internal/httpapi/evaluation_state.go. Skipped as
// unreachable in practice (see the commit message): the ReadFile
// TOCTOU error, the store.Save error (Save cannot fail after
// validation), the MarshalIndent errors (plain structs), and the
// still-too-big persist error (a single validated run cannot exceed
// the 8 MiB cap: reasons are 256 bytes, cases are capped at 512 with
// at most 8 verdicts each).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func wave2EvalPlane(t *testing.T) *evaluationPlane {
	t.Helper()
	return newEvaluationPlane(config.EvaluationConfig{MaxRuns: 512, MaxScorecards: 4096}, nil)
}

func wave2EvalStateFile(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWave2EvalLoadStatNotDir(t *testing.T) {
	p := wave2EvalPlane(t)
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.loadState(f + "/child"); err == nil {
		t.Fatal("stat through a file must fail")
	}
}

func TestWave2EvalLoadTooManyRuns(t *testing.T) {
	p := wave2EvalPlane(t)
	var runs []string
	for i := 0; i < 513; i++ {
		runs = append(runs, `{}`)
	}
	doc := `{"version":1,"runs":[` + strings.Join(runs, ",") + `]}`
	if err := p.loadState(wave2EvalStateFile(t, doc)); err == nil || !strings.Contains(err.Error(), "runs") {
		t.Fatalf("err=%v want runs-limit error", err)
	}
}

func TestWave2EvalLoadTooManyScorecards(t *testing.T) {
	p := wave2EvalPlane(t)
	var scs []string
	for i := 0; i < 4097; i++ {
		scs = append(scs, `{}`)
	}
	doc := `{"version":1,"scorecards":[` + strings.Join(scs, ",") + `]}`
	if err := p.loadState(wave2EvalStateFile(t, doc)); err == nil || !strings.Contains(err.Error(), "scorecards") {
		t.Fatalf("err=%v want scorecards-limit error", err)
	}
}

func TestWave2EvalLoadNormalizesScorecardVersion(t *testing.T) {
	p := wave2EvalPlane(t)
	now := time.Now().UTC().Format(time.RFC3339)
	// A scorecard is a claim backed by evidence: the value below is the
	// minimal valid claim, so the test exercises version normalization
	// rather than the empty-scorecard rejection.
	vals := `,"values":{"coding":{"score":0.9,"provenance":"evaluation","sample_count":10,"source":"test","suite_version":"v1","evaluated_at":"` + now + `"}}`
	doc := `{"version":1,"scorecards":[{"deployment_id":"d1","version":0,"generated_at":"` + now + `"` + vals + `}]}`
	if err := p.loadState(wave2EvalStateFile(t, doc)); err != nil {
		t.Fatal(err)
	}
	snap := p.reg.Snapshot()
	if len(snap) != 1 || snap[0].Version != 1 {
		t.Fatalf("snapshot=%v want one scorecard at version 1", snap)
	}
}

func TestWave2EvalLoadScorecardMissingGeneratedAt(t *testing.T) {
	p := wave2EvalPlane(t)
	doc := `{"version":1,"scorecards":[{"deployment_id":"d1","version":1}]}`
	if err := p.loadState(wave2EvalStateFile(t, doc)); err == nil || !strings.Contains(err.Error(), "generated_at") {
		t.Fatalf("err=%v want generated_at error", err)
	}
}

func TestWave2EvalLoadDuplicateScorecard(t *testing.T) {
	p := wave2EvalPlane(t)
	now := time.Now().UTC().Format(time.RFC3339)
	sc := `{"deployment_id":"d1","version":1,"generated_at":"` + now + `","values":{"coding":{"score":0.9,"provenance":"evaluation","sample_count":10,"source":"test","suite_version":"v1","evaluated_at":"` + now + `"}}}`
	doc := `{"version":1,"scorecards":[` + sc + `,` + sc + `]}`
	if err := p.loadState(wave2EvalStateFile(t, doc)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err=%v want duplicate error", err)
	}
}

package eval

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestIsolation_ProductionFilesDoNotImportRoutingState is a structural guarantee,
// not a convention: Phase H must not be able to influence routing, so the data
// plane may not import the evaluation plane and the evaluation plane may not
// import routing/health/provider state.
//
// The check parses production (non-test) files only, so tests remain free to use
// routing packages as fixtures.
func TestIsolation_ProductionFilesDoNotImportRoutingState(t *testing.T) {
	root := filepath.Join("..", "..")

	dataPlane := []string{
		filepath.Join(root, "internal", "router"),
		filepath.Join(root, "internal", "route"),
		filepath.Join(root, "internal", "decision"),
		filepath.Join(root, "internal", "health"),
		filepath.Join(root, "internal", "probe"),
		filepath.Join(root, "internal", "providers"),
		filepath.Join(root, "internal", "config"),
	}
	forbiddenInDataPlane := []string{
		"github.com/ali-shortcuts/nexaroute/internal/eval",
		"github.com/ali-shortcuts/nexaroute/internal/scorecards",
	}
	for _, dir := range dataPlane {
		imports := productionImports(t, dir)
		for _, imp := range imports {
			for _, forbidden := range forbiddenInDataPlane {
				if imp == forbidden || strings.HasPrefix(imp, forbidden+"/") {
					t.Fatalf("%s must not import %s: evaluation cannot influence routing in Phase H", dir, imp)
				}
			}
		}
	}

	evalPlane := []string{
		filepath.Join(root, "internal", "eval"),
		filepath.Join(root, "internal", "scorecards"),
	}
	forbiddenInEvalPlane := []string{
		"github.com/ali-shortcuts/nexaroute/internal/router",
		"github.com/ali-shortcuts/nexaroute/internal/route",
		"github.com/ali-shortcuts/nexaroute/internal/decision",
		"github.com/ali-shortcuts/nexaroute/internal/health",
		"github.com/ali-shortcuts/nexaroute/internal/probe",
		"github.com/ali-shortcuts/nexaroute/internal/providers",
	}
	for _, dir := range evalPlane {
		for _, imp := range productionImports(t, dir) {
			for _, forbidden := range forbiddenInEvalPlane {
				if imp == forbidden || strings.HasPrefix(imp, forbidden+"/") {
					t.Fatalf("%s must not import %s: evaluation must stay isolated from routing state", dir, imp)
				}
			}
		}
	}
}

// productionImports returns the import paths of every non-test .go file in dir
// (recursively).
func productionImports(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			unquoted, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			out = append(out, unquoted)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no production files found under %s (path drift?)", dir)
	}
	return out
}

// TestIsolation_ReplayExecutorMakesNoNetworkCalls double-checks the runtime
// behaviour the structural test protects: evaluation of recorded evidence never
// reaches an upstream.
func TestIsolation_ReplayExecutorMakesNoNetworkCalls(t *testing.T) {
	outcomes := codingArtifacts(t, VerdictPass, VerdictPass, VerdictPass)
	exec, err := NewReplayExecutor(outcomes)
	if err != nil {
		t.Fatal(err)
	}
	if exec.UpstreamCalls() != 0 {
		t.Fatalf("upstream calls = %d, want 0", exec.UpstreamCalls())
	}
	if _, err := NewRunner().RunWithExecutor(context.Background(), Request{DeploymentID: "p1/m1", SuiteID: "coding"}, exec); err != nil {
		t.Fatal(err)
	}
	if exec.UpstreamCalls() != 0 {
		t.Fatalf("upstream calls after run = %d, want 0", exec.UpstreamCalls())
	}
	if exec.Calls() != 4 {
		t.Fatalf("replay calls = %d, want one per suite case", exec.Calls())
	}
}

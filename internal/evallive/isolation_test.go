package evallive

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestIsolation_LiveExecutorDoesNotImportRoutingState extends the Phase H
// isolation guarantee to the live executor.
//
// internal/eval and internal/scorecards are covered by a structural test that
// forbids the routing packages outright. Live evaluation is different: it must
// reach a real upstream, so it depends on internal/providers by design and
// cannot be in that set. What it must never reach is the *routing* half of the
// system — the router, route resolution, the health manager, the probe engine or
// the decision plane — because those are what live traffic could otherwise
// perturb. This test makes that claim structural instead of a comment.
func TestIsolation_LiveExecutorDoesNotImportRoutingState(t *testing.T) {
	forbidden := []string{
		"github.com/ali-shortcuts/nexaroute/internal/router",
		"github.com/ali-shortcuts/nexaroute/internal/route",
		"github.com/ali-shortcuts/nexaroute/internal/decision",
		"github.com/ali-shortcuts/nexaroute/internal/health",
		"github.com/ali-shortcuts/nexaroute/internal/probe",
		"github.com/ali-shortcuts/nexaroute/internal/cache",
	}
	imports := productionImports(t, ".")
	for _, imp := range imports {
		for _, bad := range forbidden {
			if imp == bad || strings.HasPrefix(imp, bad+"/") {
				t.Fatalf("evallive must not import %s: live evaluation must stay isolated from routing state", imp)
			}
		}
	}
	// The one dependency live evaluation is allowed is the provider layer, and it
	// must be there: without it there is no upstream call at all.
	found := false
	for _, imp := range imports {
		if imp == "github.com/ali-shortcuts/nexaroute/internal/providers" {
			found = true
		}
	}
	if !found {
		t.Fatal("evallive must dispatch through internal/providers (no second client architecture)")
	}
}

// productionImports returns the import paths of every non-test .go file in dir.
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

package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestComponentCatalogContract (#489 item 2): the component
// primitives named in docs/COMPONENTS.md must exist as CSS rules,
// every state class of the status pill must be defined, and the
// catalog must name each primitive — code and catalog cannot drift
// apart silently.
func TestComponentCatalogContract(t *testing.T) {
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)

	primitives := []string{
		".status{", ".status.good{", ".status.warn{", ".status.bad{",
		".status.live{", ".status.stale{", ".status.disconnected{", ".status.estimated{",
		".cost-estimated{", ".cost-actual{",
		".metric-card{", ".model-row", ".topology", ".modal",
		".advanced", ".empty-state{", ".state-loading{", ".state-error{",
		"@keyframes state-pulse",
	}
	for _, p := range primitives {
		if !strings.Contains(s, p) {
			t.Errorf("styles.css is missing component primitive %s", p)
		}
	}

	// The loading pulse must be neutralized for reduced-motion
	// users by the global reduced-motion rule.
	if !strings.Contains(s, "@media(prefers-reduced-motion:reduce)") {
		t.Error("styles.css is missing the global prefers-reduced-motion rule")
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "COMPONENTS.md"))
	if err != nil {
		t.Fatalf("read docs/COMPONENTS.md: %v", err)
	}
	catalog := string(doc)
	for _, name := range []string{
		".status", ".disconnected", ".estimated", ".cost-estimated", ".cost-actual",
		".metric-card", ".model-row", ".topology", ".modal", ".advanced",
		".empty-state", ".state-loading", ".state-error",
	} {
		if !strings.Contains(catalog, name) {
			t.Errorf("docs/COMPONENTS.md does not document %s", name)
		}
	}
}

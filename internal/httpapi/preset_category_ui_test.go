package httpapi

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Regression test for the third #567 preset defect: providersView renders
// preset-category filter chips with data-action="preset-category", but the
// actions registry had no handler for that name, so the delegated click
// listener silently ignored every chip click and the catalog category
// filter never changed. The view-side filter (state.presetCategory) was
// fully implemented; only the click wiring was missing.
func TestPresetCategoryFilterWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		// Chips are rendered per catalog category plus an "all" chip.
		`data-action="preset-category" data-cat=""`,
		`data-action="preset-category" data-cat="${esc(c)}"`,
		// The view filters the catalog by the selected category.
		"presets = state.presetCatalog.filter(p => (!cat || p.category === cat)",
		// The handler must be registered and drive the same state field.
		"'preset-category': e =>",
		"state.presetCategory = button.dataset.cat || ''",
		"render(); }",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing preset-category wiring %q", want)
		}
	}
}

// Class-wide invariant behind the same defect: every data-action rendered
// anywhere in app.js must have a handler registered, either as an entry of
// the actions object ('name': ...) or as a post-literal assignment
// (actions['name'] = ...). The delegated listener drops unknown actions
// silently, so a rendered-but-unhandled action is always a dead control.
func TestEveryRenderedActionHasHandler(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	rendered := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-action=\\?"([a-z0-9-]+)\\?"`).FindAllStringSubmatch(appJS, -1) {
		rendered[m[1]] = true
	}
	if len(rendered) == 0 {
		t.Fatal("no rendered data-action values found; extraction is broken")
	}
	var orphans []string
	for name := range rendered {
		if strings.Contains(appJS, "'"+name+"':") || strings.Contains(appJS, "actions['"+name+"'] =") {
			continue
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("rendered data-action values with no registered handler: %v", orphans)
	}
}

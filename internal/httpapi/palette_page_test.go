package httpapi

import (
	"strings"
	"testing"
)

// M2-10: command palette, keyboard navigation and undo toasts.
func TestPaletteKeyboardUndoAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="palette"`, `id="palette-input"`, `id="palette-open"`, `role="listbox"`} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"function paletteCommands()",
		"function openPalette()",
		"function scheduleAction(",
		"notice-undo",
		"Object.keys(meta)",
		"openShortcuts()",
		"Undone — nothing was changed.",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	// Both destructive actions must route through the undo window.
	if strings.Count(js, "scheduleAction(") < 3 {
		t.Errorf("scheduleAction should be defined once and used by revoke-key and delete-route; found %d occurrences", strings.Count(js, "scheduleAction("))
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".palette-backdrop[hidden]") {
		t.Fatal("styles.css must keep the hidden palette display:none")
	}
}

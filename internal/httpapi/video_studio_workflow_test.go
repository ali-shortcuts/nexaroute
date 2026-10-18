package httpapi

import (
	"strings"
	"testing"
)

// Phase 14 slice 1 (#414): the Studio's job history comes from
// the server-side list endpoint (not only this browser's
// localStorage), job cards render the derived workflow steps,
// and active jobs with no provider-reported percentage show
// honest indeterminate progress instead of an invented 0%.
func TestVideoStudioWorkflowWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"videoApi('/v1/video/jobs?limit=50')", // server-side history
		"state.videoDurability",               // durability surfaced, not implied
		"Job history is stored on this gateway (file store",
		"Job history is in-memory on this gateway",
		`id="video-filter"`,                                    // history search
		`id="video-state-filter"`,                              // history state filter
		"filteredVideoJobs()",                                  // client filtering over server history
		"j.workflow",                                           // derived steps rendered
		"video-workflow",                                       // stepper markup
		"In progress — provider has not reported a percentage", // honest indeterminate progress
		"progress === 0",                                       // the no-report condition gates the percentage bar
		"retrySafetyLine(j)",                                   // #414 item 6: retry-safety line on the card
		"j.retry_safety",                                       // server-derived assessment, never a client guess
		"Retry risks duplicate billing",
		"Needs manual action",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing video studio workflow wiring %q", want)
		}
	}
	cssBytes, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	if !strings.Contains(css, ".video-workflow li.wf-active") {
		t.Error("styles.css missing workflow stepper styles")
	}
}

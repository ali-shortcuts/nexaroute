package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
)

// #420 item 6: the job records WHY its provider runs it — the
// request preference, the brief classification and the
// estimate basis — and the workflow timeline's provider
// selection step shows that evidence verbatim.
func TestProviderSelectionEvidenceRecorded(t *testing.T) {
	ctx := context.Background()
	p := providerWithDefaults()
	st, q := queue.NewMemoryStore(), queue.New(1)
	o := &Orchestrator{Store: st, Queue: q, Providers: Registry{"fake": p}}
	created, err := o.Create(ctx, requestWithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	sel := created.ProviderSelection
	for _, want := range []string{`request preference "fake"`, `provider "fake"`, `brief classified`, "priced estimate $2.00"} {
		if !strings.Contains(sel, want) {
			t.Fatalf("ProviderSelection = %q, missing %q", sel, want)
		}
	}
	steps := video.WorkflowSteps(created)
	var selectStep *video.WorkflowStep
	for i := range steps {
		if steps[i].Key == "select" {
			selectStep = &steps[i]
		}
	}
	if selectStep == nil {
		t.Fatal("workflow has no provider-selection step")
	}
	if selectStep.Detail != sel {
		t.Fatalf("select step detail = %q, want the recorded evidence %q", selectStep.Detail, sel)
	}
}

package video_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Mission-95 gap coverage for the video root package: plan contract
// validation branches, workflow projection branches, and small classifiers
// the earlier suite did not reach. Each case asserts the outcome, not
// just that the code ran.

func TestMission95KnownProviderKind(t *testing.T) {
	for _, kind := range []string{
		video.ProviderKindLocal, video.ProviderKindProgrammatic,
		video.ProviderKindCommercial, video.ProviderKindTest,
	} {
		if !video.KnownProviderKind(kind) {
			t.Fatalf("known kind %q rejected", kind)
		}
	}
	for _, kind := range []string{"", "mystery", "LOCAL"} {
		if video.KnownProviderKind(kind) {
			t.Fatalf("unknown kind %q accepted", kind)
		}
	}
}

func TestMission95HasSourceVideo(t *testing.T) {
	withVideo := video.VideoRequest{ReferenceAssets: []video.Asset{{Kind: " video "}}}
	if !withVideo.HasSourceVideo() {
		t.Fatal("video reference asset not detected")
	}
	withImage := video.VideoRequest{ReferenceAssets: []video.Asset{{Kind: "image"}}}
	if withImage.HasSourceVideo() {
		t.Fatal("image reference asset reported as source video")
	}
	if (video.VideoRequest{}).HasSourceVideo() {
		t.Fatal("empty request reported as having source video")
	}
}

func TestMission95ConsistencyLocationReferences(t *testing.T) {
	job := video.VideoJob{Request: video.VideoRequest{
		LocationReferences: []video.Asset{{ID: "loc-1"}, {ID: "loc-2"}},
	}}
	mechs := video.ConsistencyMechanisms(job)
	found := false
	for _, m := range mechs {
		if m.Kind == "location_references" && strings.Contains(m.Detail, "2 location references") {
			found = true
		}
	}
	if !found {
		t.Fatalf("location references mechanism missing: %#v", mechs)
	}
}

func TestMission95IsTerminalState(t *testing.T) {
	for _, state := range []video.JobState{
		video.StateCompleted, video.StateCancelled, video.StateExpired, video.StateDryRun,
	} {
		if !video.IsTerminalState(state) {
			t.Fatalf("terminal state %q not recognized", state)
		}
	}
	for _, state := range []video.JobState{
		video.StateFailed, video.StateQueued, video.StateProcessing, video.JobState("bogus"),
	} {
		if video.IsTerminalState(state) {
			t.Fatalf("non-terminal state %q reported terminal", state)
		}
	}
}

func mission95ValidPlan() video.Plan {
	return video.Plan{
		Title: "A test episode",
		Scenes: []video.PlanScene{{
			ID: "s1", DurationSeconds: 5, Background: "#101828",
			Camera: "none", Transition: "none", Caption: "hello",
			Elements: []video.PlanElement{{
				Type: "text", Text: "hi", X: 0.1, Y: 0.1,
				Color: "#ffffff", FontSize: 44, Animation: "none",
			}},
		}},
	}
}

func TestMission95PlanNarrationAndCaptionText(t *testing.T) {
	planLevel := video.Plan{NarrationText: "  plan narration  "}
	if got := planLevel.NarrationScript(); got != "plan narration" {
		t.Fatalf("plan-level narration=%q", got)
	}
	perScene := video.Plan{Scenes: []video.PlanScene{
		{Narration: " first "}, {Narration: ""}, {Narration: "second"},
	}}
	if got := perScene.NarrationScript(); got != "first\n\nsecond" {
		t.Fatalf("per-scene narration=%q", got)
	}
	if got := (video.Plan{Scenes: []video.PlanScene{{Narration: " "}}}).NarrationScript(); got != "" {
		t.Fatalf("blank narration=%q", got)
	}
	withCaptions := video.Plan{Scenes: []video.PlanScene{
		{Caption: " one "}, {Caption: ""}, {Caption: "two"},
	}}
	if got := withCaptions.CaptionText(); got != "one\ntwo" {
		t.Fatalf("captions=%q", got)
	}
	fallback := video.Plan{NarrationText: "narrated", Scenes: []video.PlanScene{{Caption: ""}}}
	if got := fallback.CaptionText(); got != "narrated" {
		t.Fatalf("caption fallback=%q", got)
	}
}

func TestMission95PlanNormalizedDimensions(t *testing.T) {
	vertical := video.Plan{AspectRatio: "9:16"}.Normalized()
	if vertical.Width != 720 || vertical.Height != 1280 {
		t.Fatalf("9:16 dims=%dx%d", vertical.Width, vertical.Height)
	}
	square := video.Plan{AspectRatio: "1:1"}.Normalized()
	if square.Width != 960 || square.Height != 960 {
		t.Fatalf("1:1 dims=%dx%d", square.Width, square.Height)
	}
	withScene := video.Plan{Scenes: []video.PlanScene{{DurationSeconds: 2}}}.Normalized()
	if withScene.Scenes[0].ID != "scene-01" || withScene.Scenes[0].Background != "#101828" {
		t.Fatalf("scene defaults=%+v", withScene.Scenes[0])
	}
}

func TestMission95PlanValidateBranches(t *testing.T) {
	longText := strings.Repeat("x", 501)
	longNarration := strings.Repeat("y", 2001)
	manyElements := make([]video.PlanElement, 25)
	for i := range manyElements {
		manyElements[i] = video.PlanElement{Type: "text", Text: "t", X: 0.1, Y: 0.1, Color: "#ffffff", FontSize: 44}
	}
	manyScenes := make([]video.PlanScene, 33)
	for i := range manyScenes {
		manyScenes[i] = video.PlanScene{ID: "s", DurationSeconds: 1, Caption: "c"}
	}
	// Distinct ids for the duplicate-id case are set by the mutation.
	cases := []struct {
		name   string
		mutate func(*video.Plan)
		want   string
	}{
		{"schema", func(p *video.Plan) { p.SchemaVersion = "bogus" }, "unsupported video plan schema_version"},
		{"title empty", func(p *video.Plan) { p.Title = "   " }, "title is required"},
		{"title long", func(p *video.Plan) { p.Title = strings.Repeat("t", 201) }, "title exceeds 200"},
		{"no scenes", func(p *video.Plan) { p.Scenes = nil }, "at least one scene"},
		{"too many scenes", func(p *video.Plan) {
			p.Scenes = manyScenes
			for i := range p.Scenes {
				p.Scenes[i].ID = "scene-" + string(rune('a'+i))
			}
		}, "exceeds 32 scenes"},
		{"odd dims", func(p *video.Plan) { p.Width, p.Height = 100, 101 }, "not even"},
		{"small dims", func(p *video.Plan) { p.Width, p.Height = 60, 60 }, "outside 64"},
		{"fps", func(p *video.Plan) { p.FPS = 61 }, "fps 61 is outside"},
		{"palette", func(p *video.Plan) { p.Palette = []string{"red"} }, "not a #RRGGBB"},
		{"zero total", func(p *video.Plan) { p.Scenes[0].DurationSeconds = 0 }, "total duration 0.00"},
		{"max duration", func(p *video.Plan) { p.Scenes[0].DurationSeconds = 10 }, "exceeds the configured"},
		{"duration mismatch", func(p *video.Plan) { p.DurationSeconds = 1 }, "does not match the scene total"},
		{"scene id", func(p *video.Plan) { p.Scenes[0].ID = "bad id!" }, "scene id"},
		{"scene duration", func(p *video.Plan) { p.Scenes[0].DurationSeconds = 121 }, "outside 0..120"},
		{"background", func(p *video.Plan) { p.Scenes[0].Background = "blue" }, "not a #RRGGBB"},
		{"gradient", func(p *video.Plan) { p.Scenes[0].BackgroundGradientTo = "blue" }, "background_gradient_to"},
		{"camera", func(p *video.Plan) { p.Scenes[0].Camera = "spin" }, "unsupported camera"},
		{"transition", func(p *video.Plan) { p.Scenes[0].Transition = "wipe" }, "unsupported transition"},
		{"caption long", func(p *video.Plan) { p.Scenes[0].Caption = longText }, "caption exceeds"},
		{"narration long", func(p *video.Plan) { p.Scenes[0].Narration = longNarration }, "narration segment exceeds"},
		{"too many elements", func(p *video.Plan) { p.Scenes[0].Elements = manyElements }, "exceeds 24 elements"},
		{"blank scene", func(p *video.Plan) { p.Scenes[0].Caption, p.Scenes[0].Elements = "", nil }, "no visible element"},
		{"duplicate id", func(p *video.Plan) {
			p.Scenes = append(p.Scenes, p.Scenes[0])
		}, "duplicated"},
		{"text empty", func(p *video.Plan) { p.Scenes[0].Elements[0].Text = " " }, "text element requires text"},
		{"text long", func(p *video.Plan) { p.Scenes[0].Elements[0].Text = longText }, "text exceeds"},
		{"font size", func(p *video.Plan) { p.Scenes[0].Elements[0].FontSize = 1 }, "font_size 1"},
		{"shape type", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "shape", Shape: "circle", X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2, Color: "#ffffff"}
		}, "unsupported shape"},
		{"shape dims", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "shape", Shape: "rect", X: 0.1, Y: 0.1, Color: "#ffffff"}
		}, "positive width and height"},
		{"chart empty", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "chart", X: 0.1, Y: 0.1, Width: 0.5, Height: 0.5, Color: "#ffffff"}
		}, "chart element requires data"},
		{"chart many", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "chart", Data: make([]float64, 25), X: 0.1, Y: 0.1, Width: 0.5, Height: 0.5, Color: "#ffffff"}
		}, "chart exceeds"},
		{"chart negative", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "chart", Data: []float64{-1}, X: 0.1, Y: 0.1, Width: 0.5, Height: 0.5, Color: "#ffffff"}
		}, "finite and >= 0"},
		{"chart dims", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "chart", Data: []float64{1}, X: 0.1, Y: 0.1, Color: "#ffffff"}
		}, "positive width and height"},
		{"element type", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "video", X: 0.1, Y: 0.1, Color: "#ffffff"}
		}, "unsupported element type"},
		{"element color", func(p *video.Plan) { p.Scenes[0].Elements[0].Color = "red" }, "not a #RRGGBB"},
		{"element fraction", func(p *video.Plan) { p.Scenes[0].Elements[0].X = 1.5 }, "outside 0..1"},
		{"element animation", func(p *video.Plan) { p.Scenes[0].Elements[0].Animation = "spin" }, "unsupported animation"},
		{"fade shape", func(p *video.Plan) {
			p.Scenes[0].Elements[0] = video.PlanElement{Type: "shape", Shape: "rect", X: 0.1, Y: 0.1, Width: 0.2, Height: 0.2, Color: "#ffffff", Animation: "fade"}
		}, "fade animation is supported for text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mission95ValidPlan()
			tc.mutate(&p)
			maxDuration := 0.0
			if tc.name == "max duration" {
				maxDuration = 5
			}
			if err := p.Validate(maxDuration); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error=%v, want substring %q", err, tc.want)
			}
		})
	}
	if err := mission95ValidPlan().Validate(0); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func mission95Step(t *testing.T, steps []video.WorkflowStep, key string) video.WorkflowStep {
	t.Helper()
	for _, s := range steps {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("step %q missing in %#v", key, steps)
	return video.WorkflowStep{}
}

func TestMission95WorkflowStepsBranches(t *testing.T) {
	now := time.Now()
	// Dry-run: everything after validation is skipped.
	dry := video.WorkflowSteps(video.VideoJob{State: video.StateDryRun})
	if got := mission95Step(t, dry, "validate"); got.Status != "done" {
		t.Fatalf("dry-run validate=%s", got.Status)
	}
	for _, key := range []string{"select", "admit", "submit", "generate", "validate_output", "compose", "narrate", "deliver"} {
		if got := mission95Step(t, dry, key); got.Status != "skipped" {
			t.Fatalf("dry-run %s=%s, want skipped", key, got.Status)
		}
	}
	// No provider yet, past queue: selection is active.
	planning := video.WorkflowSteps(video.VideoJob{State: video.StatePlanning})
	if got := mission95Step(t, planning, "select"); got.Status != "active" {
		t.Fatalf("planning select=%s, want active", got.Status)
	}
	// Waiting for budget: admission active with the waiting detail.
	budget := video.WorkflowSteps(video.VideoJob{State: video.StateWaitingForBudget})
	if got := mission95Step(t, budget, "admit"); got.Status != "active" || got.Detail != "waiting for budget" {
		t.Fatalf("budget admit=%+v", got)
	}
	// Provider stamped with selection evidence: detail recorded.
	selected := video.WorkflowSteps(video.VideoJob{State: video.StateQueued, Provider: "fake", ProviderSelection: "request preference"})
	if got := mission95Step(t, selected, "select"); got.Status != "done" || got.Detail != "request preference" {
		t.Fatalf("selected select=%+v", got)
	}
	// Submitted with a provider job id: admission and submission done.
	submitted := video.WorkflowSteps(video.VideoJob{State: video.StateSubmitted, Provider: "fake", ProviderJobID: "remote-1"})
	if got := mission95Step(t, submitted, "admit"); got.Status != "done" {
		t.Fatalf("submitted admit=%s", got.Status)
	}
	if got := mission95Step(t, submitted, "submit"); got.Status != "done" {
		t.Fatalf("submitted submit=%s", got.Status)
	}
	// Submitted without any submission record: submission is active.
	unsubmitted := video.WorkflowSteps(video.VideoJob{State: video.StateSubmitted})
	if got := mission95Step(t, unsubmitted, "submit"); got.Status != "active" {
		t.Fatalf("unsubmitted submit=%s", got.Status)
	}
	// Mixed shot states: generation is not done; multi-shot processing
	// records the per-shot detail.
	mixed := video.WorkflowSteps(video.VideoJob{State: video.StateProcessing, TotalShots: 2, Provider: "fake",
		ShotExecutions: []video.ShotExecution{
			{State: video.StateCompleted}, {State: video.StateProcessing},
		}})
	if got := mission95Step(t, mixed, "generate"); got.Status != "active" || got.Detail != "shot progress recorded per shot" {
		t.Fatalf("mixed generate=%+v", got)
	}
	// All shots complete but the job is still processing: generation done
	// and output validation becomes active.
	allDone := video.WorkflowSteps(video.VideoJob{State: video.StateProcessing, Provider: "fake",
		ShotExecutions: []video.ShotExecution{{State: video.StateCompleted}}})
	if got := mission95Step(t, allDone, "generate"); got.Status != "done" {
		t.Fatalf("all-done generate=%s", got.Status)
	}
	if got := mission95Step(t, allDone, "validate_output"); got.Status != "active" {
		t.Fatalf("all-done validate_output=%s", got.Status)
	}
	// Composing multi-shot job: composition and narration both active.
	composing := video.WorkflowSteps(video.VideoJob{State: video.StateComposing, TotalShots: 2,
		Request: video.VideoRequest{NarrationText: "hello"}})
	if got := mission95Step(t, composing, "compose"); got.Status != "active" {
		t.Fatalf("composing compose=%s", got.Status)
	}
	if got := mission95Step(t, composing, "narrate"); got.Status != "active" {
		t.Fatalf("composing narrate=%s", got.Status)
	}
	// Uploading: delivery active.
	uploading := video.WorkflowSteps(video.VideoJob{State: video.StateUploading, TotalShots: 2})
	if got := mission95Step(t, uploading, "deliver"); got.Status != "active" {
		t.Fatalf("uploading deliver=%s", got.Status)
	}
	// Completed compose job with assets: composition and delivery done.
	completed := video.WorkflowSteps(video.VideoJob{State: video.StateCompleted, TotalShots: 2,
		OutputAssets: []video.Asset{{ID: "final"}}})
	if got := mission95Step(t, completed, "compose"); got.Status != "done" {
		t.Fatalf("completed compose=%s", got.Status)
	}
	if got := mission95Step(t, completed, "deliver"); got.Status != "done" {
		t.Fatalf("completed deliver=%s", got.Status)
	}
	// Cancelled: the first non-completed step carries the outcome.
	cancelled := video.WorkflowSteps(video.VideoJob{State: video.StateCancelled})
	if got := mission95Step(t, cancelled, "select"); got.Status != "cancelled" {
		t.Fatalf("cancelled select=%s, want cancelled", got.Status)
	}
	_ = now
}

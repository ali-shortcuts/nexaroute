package video_test

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

func validPlan() video.Plan {
	return video.Plan{
		SchemaVersion: video.PlanSchemaVersion,
		Title:         "How routing works",
		Width:         320, Height: 240, FPS: 12,
		Scenes: []video.PlanScene{
			{ID: "intro", DurationSeconds: 2, Background: "#101828", Elements: []video.PlanElement{
				{Type: "text", Text: "Routing, explained", X: 0.5, Y: 0.4, FontSize: 40, Color: "#ffffff"},
			}},
			{ID: "chart", DurationSeconds: 2, Background: "#ffffff", Elements: []video.PlanElement{
				{Type: "chart", Data: []float64{2, 5, 3}, X: 0.1, Y: 0.2, Width: 0.8, Height: 0.5, Color: "#2e90fa"},
			}},
		},
	}
}

func TestPlanValidateAcceptsValidPlan(t *testing.T) {
	p := validPlan()
	if err := p.Validate(0); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if got := p.TotalDuration(); got != 4 {
		t.Fatalf("total duration = %v, want 4", got)
	}
}

func TestPlanValidateRejectsDurationMismatch(t *testing.T) {
	p := validPlan()
	p.DurationSeconds = 99
	if err := p.Validate(0); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected duration mismatch error, got %v", err)
	}
}

func TestPlanValidateRejectsUnsupportedOperations(t *testing.T) {
	cases := map[string]func(*video.Plan){
		"bad camera":  func(p *video.Plan) { p.Scenes[0].Camera = "orbit" },
		"bad element": func(p *video.Plan) { p.Scenes[0].Elements[0].Type = "image" },
		"bad color":   func(p *video.Plan) { p.Scenes[0].Background = "red" },
		"bad anim":    func(p *video.Plan) { p.Scenes[0].Elements[0].Animation = "spin" },
		"fade on chart": func(p *video.Plan) {
			p.Scenes[1].Elements[0].Animation = "fade"
		},
		"duplicate scene id": func(p *video.Plan) { p.Scenes[1].ID = "intro" },
		"bad schema":         func(p *video.Plan) { p.SchemaVersion = "video-plan.v99" },
		"negative chart":     func(p *video.Plan) { p.Scenes[1].Elements[0].Data[0] = -1 },
		"odd dimensions":     func(p *video.Plan) { p.Width = 321 },
	}
	for name, mutate := range cases {
		p := validPlan()
		mutate(&p)
		if err := p.Validate(0); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestPlanValidateRejectsBlankScene(t *testing.T) {
	p := validPlan()
	p.Scenes[0].Elements = nil
	p.Scenes[0].Caption = ""
	if err := p.Validate(0); err == nil || !strings.Contains(err.Error(), "no visible element") {
		t.Fatalf("expected blank scene error, got %v", err)
	}
}

func TestPlanValidateEnforcesProviderCeiling(t *testing.T) {
	p := validPlan()
	if err := p.Validate(3); err == nil || !strings.Contains(err.Error(), "exceeds the configured code-render maximum") {
		t.Fatalf("expected ceiling error, got %v", err)
	}
}

func TestPlanNormalizedDefaults(t *testing.T) {
	p := video.Plan{Title: "x", Scenes: []video.PlanScene{{DurationSeconds: 1, Elements: []video.PlanElement{{Type: "text", Text: "hi", X: 0.5, Y: 0.5}}}}}
	n := p.Normalized()
	if n.SchemaVersion != video.PlanSchemaVersion || n.Width != 1280 || n.Height != 720 || n.FPS != 24 {
		t.Fatalf("defaults not applied: %+v", n)
	}
	if n.Scenes[0].ID == "" || n.Scenes[0].Background == "" {
		t.Fatalf("scene defaults not applied: %+v", n.Scenes[0])
	}
	if err := n.Validate(0); err != nil {
		t.Fatalf("normalized plan should validate: %v", err)
	}
}

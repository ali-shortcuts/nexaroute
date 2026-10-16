package video

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// PlanSchemaVersion versions the declarative video plan contract consumed by
// the Universal AI-to-Video engine (issue #461, Tier A). A plan is data, not
// code: it can never name a filesystem path, a shell command, or a network
// target, and the renderer interprets only the primitives defined here.
const PlanSchemaVersion = "video-plan.v1"

// Plan limits. These are hard ceilings for model-generated plans; the
// code-render provider may impose a lower configured maximum duration.
const (
	MaxPlanScenes           = 32
	MaxPlanSceneSeconds     = 120.0
	MaxPlanTotalSeconds     = 3600.0
	MaxPlanElementsPerScene = 24
	MaxPlanTextRunes        = 500
	MaxPlanChartValues      = 24
)

var (
	planColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	planIDRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Plan is a versioned, validated scene graph produced by a planning model
// (or supplied explicitly by a client for review-first workflows) and
// executed by the deterministic renderer.
type Plan struct {
	SchemaVersion   string      `json:"schema_version"`
	Title           string      `json:"title"`
	Description     string      `json:"description,omitempty"`
	DurationSeconds float64     `json:"duration_seconds"`
	Width           int         `json:"width,omitempty"`
	Height          int         `json:"height,omitempty"`
	FPS             int         `json:"fps,omitempty"`
	AspectRatio     string      `json:"aspect_ratio,omitempty"`
	Language        string      `json:"language,omitempty"`
	Palette         []string    `json:"palette,omitempty"`
	NarrationText   string      `json:"narration_text,omitempty"`
	Subtitles       bool        `json:"subtitles,omitempty"`
	Scenes          []PlanScene `json:"scenes"`
}

// PlanScene is one timed scene. Background is a solid color or a two-color
// gradient; Camera names a deterministic pan/zoom applied by the renderer.
type PlanScene struct {
	ID                   string        `json:"id"`
	DurationSeconds      float64       `json:"duration_seconds"`
	Background           string        `json:"background,omitempty"`
	BackgroundGradientTo string        `json:"background_gradient_to,omitempty"`
	Camera               string        `json:"camera,omitempty"`
	Transition           string        `json:"transition,omitempty"`
	Caption              string        `json:"caption,omitempty"`
	Narration            string        `json:"narration,omitempty"`
	Elements             []PlanElement `json:"elements,omitempty"`
}

// PlanElement is a single declarative visual primitive. Coordinates are
// fractions of the frame (0..1). No field may carry a path, URL, or command.
type PlanElement struct {
	Type      string    `json:"type"` // text | shape | chart
	Text      string    `json:"text,omitempty"`
	Shape     string    `json:"shape,omitempty"` // rect (shape only)
	X         float64   `json:"x"`
	Y         float64   `json:"y"`
	Width     float64   `json:"width,omitempty"`
	Height    float64   `json:"height,omitempty"`
	Color     string    `json:"color,omitempty"`
	FontSize  int       `json:"font_size,omitempty"` // points at a 720p reference height
	Bold      bool      `json:"bold,omitempty"`
	Animation string    `json:"animation,omitempty"` // none | fade | slide_up | slide_left
	Data      []float64 `json:"data,omitempty"`      // chart only: bar values, >= 0
	Z         int       `json:"z,omitempty"`
}

// TotalDuration returns the summed scene durations.
func (p Plan) TotalDuration() float64 {
	total := 0.0
	for _, s := range p.Scenes {
		total += s.DurationSeconds
	}
	return total
}

// NarrationScript joins the plan-level narration (preferred) or the
// per-scene narration segments in scene order.
func (p Plan) NarrationScript() string {
	if strings.TrimSpace(p.NarrationText) != "" {
		return strings.TrimSpace(p.NarrationText)
	}
	parts := make([]string, 0, len(p.Scenes))
	for _, s := range p.Scenes {
		if t := strings.TrimSpace(s.Narration); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// CaptionText joins per-scene captions (falling back to narration segments),
// used to build timed subtitles when a plan requests them.
func (p Plan) CaptionText() string {
	parts := make([]string, 0, len(p.Scenes))
	for _, s := range p.Scenes {
		if t := strings.TrimSpace(s.Caption); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return p.NarrationScript()
}

// Normalized returns a copy with defaults applied: schema version, frame
// rate, and dimensions derived from the aspect ratio when unset.
func (p Plan) Normalized() Plan {
	if strings.TrimSpace(p.SchemaVersion) == "" {
		p.SchemaVersion = PlanSchemaVersion
	}
	p.Title = strings.TrimSpace(p.Title)
	if p.FPS == 0 {
		p.FPS = 24
	}
	if p.AspectRatio == "" {
		p.AspectRatio = "16:9"
	}
	if p.Width == 0 && p.Height == 0 {
		switch p.AspectRatio {
		case "9:16":
			p.Width, p.Height = 720, 1280
		case "1:1":
			p.Width, p.Height = 960, 960
		default:
			p.Width, p.Height = 1280, 720
		}
	}
	if p.DurationSeconds == 0 {
		p.DurationSeconds = p.TotalDuration()
	}
	for i := range p.Scenes {
		if strings.TrimSpace(p.Scenes[i].ID) == "" {
			p.Scenes[i].ID = fmt.Sprintf("scene-%02d", i+1)
		}
		if p.Scenes[i].Background == "" {
			p.Scenes[i].Background = "#101828"
		}
		if p.Scenes[i].Camera == "" {
			p.Scenes[i].Camera = "none"
		}
		if p.Scenes[i].Transition == "" {
			p.Scenes[i].Transition = "none"
		}
		for j := range p.Scenes[i].Elements {
			el := &p.Scenes[i].Elements[j]
			if el.Color == "" {
				el.Color = "#ffffff"
			}
			if el.Animation == "" {
				el.Animation = "none"
			}
			if el.Type == "text" && el.FontSize == 0 {
				el.FontSize = 44
			}
		}
	}
	return p
}

// Validate checks the whole plan against the Tier-A contract. maxDuration,
// when > 0, is the provider's configured total-duration ceiling.
func (p Plan) Validate(maxDuration float64) error {
	p = p.Normalized()
	if p.SchemaVersion != PlanSchemaVersion {
		return fmt.Errorf("unsupported video plan schema_version %q (want %q)", p.SchemaVersion, PlanSchemaVersion)
	}
	if p.Title == "" {
		return errors.New("video plan title is required")
	}
	if utf8.RuneCountInString(p.Title) > 200 {
		return errors.New("video plan title exceeds 200 characters")
	}
	if len(p.Scenes) == 0 {
		return errors.New("video plan requires at least one scene")
	}
	if len(p.Scenes) > MaxPlanScenes {
		return fmt.Errorf("video plan exceeds %d scenes", MaxPlanScenes)
	}
	if p.Width < 64 || p.Height < 64 || p.Width > 3840 || p.Height > 2160 || p.Width%2 != 0 || p.Height%2 != 0 {
		return fmt.Errorf("video plan dimensions %dx%d are outside 64..3840 x 64..2160 or not even", p.Width, p.Height)
	}
	if p.FPS < 1 || p.FPS > 60 {
		return fmt.Errorf("video plan fps %d is outside 1..60", p.FPS)
	}
	for _, c := range p.Palette {
		if !planColorRe.MatchString(c) {
			return fmt.Errorf("video plan palette color %q is not a #RRGGBB hex color", c)
		}
	}
	total := p.TotalDuration()
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) || total > MaxPlanTotalSeconds {
		return fmt.Errorf("video plan total duration %.2fs is outside 0..%.0fs", total, MaxPlanTotalSeconds)
	}
	if maxDuration > 0 && total > maxDuration+0.001 {
		return fmt.Errorf("video plan total duration %.2fs exceeds the configured code-render maximum %.2fs", total, maxDuration)
	}
	if p.DurationSeconds > 0 && math.Abs(p.DurationSeconds-total) > 0.6 {
		return fmt.Errorf("video plan duration_seconds %.2f does not match the scene total %.2f", p.DurationSeconds, total)
	}
	ids := map[string]bool{}
	for i, s := range p.Scenes {
		if err := s.validate(); err != nil {
			return fmt.Errorf("scene %d (%s): %w", i+1, s.ID, err)
		}
		if ids[s.ID] {
			return fmt.Errorf("video plan scene id %q is duplicated", s.ID)
		}
		ids[s.ID] = true
	}
	return nil
}

func (s PlanScene) validate() error {
	if !planIDRe.MatchString(s.ID) {
		return fmt.Errorf("scene id %q is invalid", s.ID)
	}
	if s.DurationSeconds <= 0 || math.IsNaN(s.DurationSeconds) || math.IsInf(s.DurationSeconds, 0) || s.DurationSeconds > MaxPlanSceneSeconds {
		return fmt.Errorf("duration_seconds %.2f is outside 0..%.0f", s.DurationSeconds, MaxPlanSceneSeconds)
	}
	if !planColorRe.MatchString(s.Background) {
		return fmt.Errorf("background %q is not a #RRGGBB hex color", s.Background)
	}
	if s.BackgroundGradientTo != "" && !planColorRe.MatchString(s.BackgroundGradientTo) {
		return fmt.Errorf("background_gradient_to %q is not a #RRGGBB hex color", s.BackgroundGradientTo)
	}
	switch s.Camera {
	case "none", "zoom_in", "zoom_out", "pan_left", "pan_right":
	default:
		return fmt.Errorf("unsupported camera operation %q", s.Camera)
	}
	switch s.Transition {
	case "none", "fade":
	default:
		return fmt.Errorf("unsupported transition %q", s.Transition)
	}
	if utf8.RuneCountInString(s.Caption) > MaxPlanTextRunes {
		return fmt.Errorf("caption exceeds %d characters", MaxPlanTextRunes)
	}
	if utf8.RuneCountInString(s.Narration) > 2000 {
		return errors.New("narration segment exceeds 2000 characters")
	}
	if len(s.Elements) > MaxPlanElementsPerScene {
		return fmt.Errorf("scene exceeds %d elements", MaxPlanElementsPerScene)
	}
	hasVisible := strings.TrimSpace(s.Caption) != ""
	for j, el := range s.Elements {
		if err := el.validate(); err != nil {
			return fmt.Errorf("element %d: %w", j+1, err)
		}
		hasVisible = true
	}
	if !hasVisible {
		return errors.New("scene has no visible element or caption (blank scenes are not renderable)")
	}
	return nil
}

func (e PlanElement) validate() error {
	switch e.Type {
	case "text":
		if strings.TrimSpace(e.Text) == "" {
			return errors.New("text element requires text")
		}
		if utf8.RuneCountInString(e.Text) > MaxPlanTextRunes {
			return fmt.Errorf("text exceeds %d characters", MaxPlanTextRunes)
		}
		if e.FontSize < 8 || e.FontSize > 240 {
			return fmt.Errorf("font_size %d is outside 8..240", e.FontSize)
		}
	case "shape":
		if e.Shape != "rect" {
			return fmt.Errorf("unsupported shape %q (only rect)", e.Shape)
		}
		if e.Width <= 0 || e.Height <= 0 {
			return errors.New("shape element requires positive width and height fractions")
		}
	case "chart":
		if len(e.Data) == 0 {
			return errors.New("chart element requires data values")
		}
		if len(e.Data) > MaxPlanChartValues {
			return fmt.Errorf("chart exceeds %d values", MaxPlanChartValues)
		}
		for _, v := range e.Data {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("chart data values must be finite and >= 0")
			}
		}
		if e.Width <= 0 || e.Height <= 0 {
			return errors.New("chart element requires positive width and height fractions")
		}
	default:
		return fmt.Errorf("unsupported element type %q (supported: text, shape, chart)", e.Type)
	}
	if !planColorRe.MatchString(e.Color) {
		return fmt.Errorf("color %q is not a #RRGGBB hex color", e.Color)
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"x", e.X}, {"y", e.Y}, {"width", e.Width}, {"height", e.Height}} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) || f.v < 0 || f.v > 1 {
			return fmt.Errorf("%s fraction %.3f is outside 0..1", f.name, f.v)
		}
	}
	switch e.Animation {
	case "none", "fade", "slide_up", "slide_left":
	default:
		return fmt.Errorf("unsupported animation %q", e.Animation)
	}
	if e.Animation == "fade" && e.Type != "text" {
		return errors.New("fade animation is supported for text elements only in the baseline renderer")
	}
	return nil
}

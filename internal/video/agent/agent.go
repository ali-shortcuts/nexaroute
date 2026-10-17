// Package agent is the Universal Video Agent: it turns a natural-language
// brief into a validated video.Plan using any configured text/coding model
// that can return text. It does not require native tool calling or native
// video generation from the model. Model output is untrusted: it is parsed
// strictly, validated against the plan contract, and repaired through a
// bounded number of corrective prompts before the job fails explicitly.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

// Planner is the minimal text-completion surface the agent needs from a
// model. Implementations adapt a chat-completions endpoint; tests use a
// scripted fake. Returning an error is a provider failure, not a bad plan.
type Planner interface {
	Complete(ctx context.Context, model, system, user string) (string, error)
}

// Request describes one planning job.
type Request struct {
	Brief           string
	Model           string
	DurationSeconds float64
	Width           int
	Height          int
	FPS             int
	AspectRatio     string
	Language        string
	MaxDuration     float64 // provider ceiling passed to plan validation
}

// Result is a validated plan plus provenance.
type Result struct {
	Plan     video.Plan
	Attempts int
	Model    string
}

// Agent generates plans with bounded repair.
type Agent struct {
	Planner     Planner
	MaxAttempts int // total attempts including the first; default 3
}

// GeneratePlan asks the planner for a plan, validates it, and on a parse or
// validation failure feeds the exact defect back for a bounded repair. It
// never returns an unvalidated plan.
func (a *Agent) GeneratePlan(ctx context.Context, req Request) (Result, error) {
	if a == nil || a.Planner == nil {
		return Result{}, errors.New("video agent has no planner configured")
	}
	if strings.TrimSpace(req.Brief) == "" {
		return Result{}, errors.New("video planning brief is required")
	}
	if strings.TrimSpace(req.Model) == "" {
		return Result{}, errors.New("video planning model is required")
	}
	attempts := a.MaxAttempts
	if attempts < 1 {
		attempts = 3
	}
	if attempts > 5 {
		attempts = 5
	}
	system := systemPrompt(req)
	user := req.Brief
	var lastErr error
	for i := 1; i <= attempts; i++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		raw, err := a.Planner.Complete(ctx, req.Model, system, user)
		if err != nil {
			return Result{}, fmt.Errorf("planning model call failed: %w", err)
		}
		plan, perr := ParsePlan(raw)
		if perr == nil {
			perr = plan.Validate(req.MaxDuration)
		}
		if perr == nil {
			// The renderer executes exactly the normalized form.
			return Result{Plan: plan.Normalized(), Attempts: i, Model: req.Model}, nil
		}
		lastErr = perr
		if i == attempts {
			break
		}
		user = repairPrompt(req, raw, perr)
	}
	return Result{}, fmt.Errorf("planning model did not produce a valid video plan after %d attempt(s): %w", attempts, lastErr)
}

func systemPrompt(req Request) string {
	aspect := req.AspectRatio
	if aspect == "" {
		aspect = "16:9"
	}
	lang := req.Language
	if lang == "" {
		lang = "the language of the brief"
	}
	return fmt.Sprintf(`You are the video planning component of NexaRoute. You do NOT generate video yourself; you author a declarative JSON scene plan that a deterministic renderer executes.

Return ONLY one JSON object with this exact shape (schema video-plan.v1):
{"schema_version":"video-plan.v1","title":"...","description":"...","duration_seconds":%s,"width":%d,"height":%d,"fps":%d,"aspect_ratio":%q,"language":"...","subtitles":false,"scenes":[{"id":"scene-01","duration_seconds":4,"background":"#101828","background_gradient_to":"#1d2939","camera":"zoom_in","transition":"fade","caption":"","narration":"","elements":[{"type":"text","text":"...","x":0.5,"y":0.4,"font_size":56,"color":"#ffffff","animation":"slide_up"},{"type":"shape","shape":"rect","x":0.1,"y":0.7,"width":0.3,"height":0.06,"color":"#f79009","animation":"none"},{"type":"chart","data":[3,7,5],"x":0.15,"y":0.35,"width":0.7,"height":0.4,"color":"#2e90fa","animation":"none"}]}]}

Rules:
- The scene durations must sum exactly to duration_seconds.
- Every scene needs at least one visible element or a caption; no blank scenes.
- Coordinates x/y/width/height are fractions of the frame between 0 and 1. For text, x/y position the text block (0.5 centers it).
- Colors are #RRGGBB hex. camera is one of: none, zoom_in, zoom_out, pan_left, pan_right. transition is one of: none, fade. animation is one of: none, fade (text only), slide_up, slide_left.
- Element types are only text, shape (rect), chart. Do NOT include file paths, URLs, commands, code, or any field not shown above.
- Narration and captions must be written in %s.
- Keep text short enough to read on screen.`, durationHint(req), req.Width, req.Height, fpsHint(req), aspect, lang)
}

func durationHint(req Request) string {
	if req.DurationSeconds > 0 {
		return fmt.Sprintf("%g", req.DurationSeconds)
	}
	return "the total of the scene durations you choose (between 4 and 60)"
}

func fpsHint(req Request) int {
	if req.FPS > 0 {
		return req.FPS
	}
	return 24
}

func repairPrompt(req Request, previous string, defect error) string {
	trimmed := previous
	if len(trimmed) > 6000 {
		trimmed = trimmed[:6000]
	}
	return fmt.Sprintf("Your previous video plan was rejected by the validator: %s\n\nReturn a corrected plan as ONLY one JSON object following the same schema and rules. Previous output:\n%s\n\nOriginal brief: %s", defect.Error(), trimmed, req.Brief)
}

// ParsePlan extracts and decodes a plan from raw model output. It accepts a
// bare JSON object or one wrapped in a markdown code fence, and rejects
// trailing non-JSON content after the object.
func ParsePlan(raw string) (video.Plan, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = strings.TrimSpace(s[:idx])
		}
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return video.Plan{}, errors.New("model output contains no JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	dec.DisallowUnknownFields()
	var plan video.Plan
	if err := dec.Decode(&plan); err != nil {
		return video.Plan{}, fmt.Errorf("model output is not a valid video plan JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return video.Plan{}, errors.New("model output contains trailing JSON after the video plan")
		}
		return video.Plan{}, fmt.Errorf("model output contains trailing non-JSON content: %w", err)
	}
	return plan, nil
}

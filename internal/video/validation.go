package video

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var ErrInvalidRequest = errors.New("invalid video request")

func (r VideoRequest) Validate() error {
	n := r.Normalized()
	if n.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrInvalidRequest)
	}
	if n.Prompt == "" && len(n.ShotPlan) == 0 {
		return fmt.Errorf("%w: prompt or shot_plan is required", ErrInvalidRequest)
	}
	switch n.Mode {
	case ModeTextToVideo, ModeImageToVideo, ModeVideoToVideo:
	default:
		return fmt.Errorf("%w: unsupported mode %q", ErrInvalidRequest, n.Mode)
	}
	if n.DurationSeconds <= 0 || n.DurationSeconds > 3600 {
		return fmt.Errorf("%w: duration_seconds must be between 0 and 3600", ErrInvalidRequest)
	}
	if n.AspectRatio != Aspect16x9 && n.AspectRatio != Aspect9x16 && n.AspectRatio != Aspect1x1 && n.AspectRatio != AspectCustom {
		return fmt.Errorf("%w: unsupported aspect_ratio %q", ErrInvalidRequest, n.AspectRatio)
	}
	if n.FPS < 1 || n.FPS > 120 {
		return fmt.Errorf("%w: fps must be between 1 and 120", ErrInvalidRequest)
	}
	if n.MaxCostUSD < 0 {
		return fmt.Errorf("%w: max_cost_usd cannot be negative", ErrInvalidRequest)
	}
	if n.CallbackURL != "" {
		u, err := url.ParseRequestURI(n.CallbackURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("%w: callback_url must be an https URL", ErrInvalidRequest)
		}
	}
	if len(n.ShotPlan) > 256 {
		return fmt.Errorf("%w: shot_plan exceeds 256 shots", ErrInvalidRequest)
	}
	for i, shot := range n.ShotPlan {
		if strings.TrimSpace(shot.ShotID) == "" || strings.TrimSpace(shot.Prompt) == "" || shot.DurationSeconds <= 0 {
			return fmt.Errorf("%w: invalid shot %d", ErrInvalidRequest, i)
		}
	}
	return nil
}

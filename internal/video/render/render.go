// Package render is the deterministic Tier-A renderer for the Universal
// AI-to-Video engine (issue #461). It interprets a validated declarative
// video.Plan with FFmpeg lavfi sources and drawtext/drawbox filters and
// composes the scenes with the existing composer package. It never executes
// model-generated code, shell commands, or paths: every FFmpeg argument is
// built here from validated plan fields.
package render

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
)

// Renderer renders validated plans. Binary paths are operator configured;
// FontFile may be empty, in which case a host candidate is discovered.
type Renderer struct {
	FFmpeg   string
	FFprobe  string
	FontFile string
}

// Result describes a successful render.
type Result struct {
	OutputPath string   // final joined MP4 (no narration/subtitle mux yet)
	ScenePaths []string // per-scene MP4s in plan order
	Manifest   composer.Manifest
}

func (r *Renderer) ffmpeg() string {
	if strings.TrimSpace(r.FFmpeg) != "" {
		return r.FFmpeg
	}
	if v := strings.TrimSpace(os.Getenv("NEXAROUTE_FFMPEG")); v != "" {
		return v
	}
	return "ffmpeg"
}

func (r *Renderer) ffprobe() string {
	if strings.TrimSpace(r.FFprobe) != "" {
		return r.FFprobe
	}
	return "ffprobe"
}

// Preflight verifies FFmpeg, libx264, the drawtext/drawbox/gradients
// filters, and ffprobe before a job is admitted to rendering.
func (r *Renderer) Preflight(ctx context.Context) error {
	comp := composer.New(r.ffmpeg(), "")
	if err := comp.Preflight(ctx); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, r.ffmpeg(), "-hide_banner", "-filters").CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect ffmpeg filters: %w", err)
	}
	for _, f := range []string{"drawtext", "drawbox", "gradients", "zoompan"} {
		if !strings.Contains(string(out), " "+f+" ") && !strings.Contains(string(out), " "+f+"\t") {
			return fmt.Errorf("ffmpeg does not provide the %s filter required by the code-render renderer", f)
		}
	}
	if _, err := exec.LookPath(r.ffprobe()); err != nil {
		return fmt.Errorf("ffprobe executable %q not found: %w", r.ffprobe(), err)
	}
	if _, err := r.fontSpec(); err != nil {
		return err
	}
	return nil
}

func (r *Renderer) fontSpec() (string, error) {
	candidates := []string{r.FontFile, os.Getenv("NEXAROUTE_VIDEO_FONT"),
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.Mode().IsRegular() {
			return "fontfile=" + escapeFilterValue(c), nil
		}
		if r.FontFile != "" && c == r.FontFile {
			return "", fmt.Errorf("configured video font file %q is not a regular file", c)
		}
	}
	// Fall back to fontconfig family naming; ffmpeg builds with
	// libfontconfig resolve this at runtime.
	return "font=DejaVu Sans", nil
}

// RenderPlan renders every scene and concatenates them, in plan order,
// into dir/final-joined.mp4. dir must be a caller-owned job directory.
func (r *Renderer) RenderPlan(ctx context.Context, plan video.Plan, dir string) (Result, error) {
	plan = plan.Normalized()
	if err := plan.Validate(0); err != nil {
		return Result{}, err
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, fmt.Errorf("resolve render directory: %w", err)
	}
	dir = absoluteDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, err
	}
	font, err := r.fontSpec()
	if err != nil {
		return Result{}, err
	}
	scenePaths := make([]string, 0, len(plan.Scenes))
	for i, scene := range plan.Scenes {
		out := filepath.Join(dir, fmt.Sprintf("scene-%03d.mp4", i+1))
		if err := r.renderScene(ctx, plan, scene, i, out, font); err != nil {
			return Result{}, fmt.Errorf("render scene %q: %w", scene.ID, err)
		}
		scenePaths = append(scenePaths, out)
	}
	joined := filepath.Join(dir, "final-joined.mp4")
	comp := composer.New(r.ffmpeg(), dir)
	manifest, err := comp.Concat(ctx, scenePaths, joined)
	if err != nil {
		return Result{}, fmt.Errorf("concatenate scenes: %w", err)
	}
	return Result{OutputPath: joined, ScenePaths: scenePaths, Manifest: manifest}, nil
}

func (r *Renderer) renderScene(ctx context.Context, plan video.Plan, scene video.PlanScene, index int, out, font string) error {
	w, h, fps := plan.Width, plan.Height, plan.FPS
	dur := scene.DurationSeconds
	src := fmt.Sprintf("color=c=%s:s=%dx%d:r=%d:d=%s", ffmpegColor(scene.Background), w, h, fps, fnum(dur))
	if scene.BackgroundGradientTo != "" {
		src = fmt.Sprintf("gradients=s=%dx%d:r=%d:d=%s:c0=%s:c1=%s:nb_colors=2", w, h, fps, fnum(dur), ffmpegColor(scene.Background), ffmpegColor(scene.BackgroundGradientTo))
	}
	filters := []string{}
	if scene.Camera != "" && scene.Camera != "none" {
		frames := int(math.Round(dur * float64(fps)))
		if frames < 2 {
			frames = 2
		}
		n := frames - 1
		switch scene.Camera {
		case "zoom_in":
			filters = append(filters, fmt.Sprintf("zoompan=z='1+0.2*on/%d':d=1:s=%dx%d:fps=%d", n, w, h, fps))
		case "zoom_out":
			filters = append(filters, fmt.Sprintf("zoompan=z='1.2-0.2*on/%d':d=1:s=%dx%d:fps=%d", n, w, h, fps))
		case "pan_left":
			filters = append(filters, fmt.Sprintf("zoompan=z=1.25:x='(iw-iw/zoom)*(1-on/%d)':y='(ih-ih/zoom)/2':d=1:s=%dx%d:fps=%d", n, w, h, fps))
		case "pan_right":
			filters = append(filters, fmt.Sprintf("zoompan=z=1.25:x='(iw-iw/zoom)*on/%d':y='(ih-ih/zoom)/2':d=1:s=%dx%d:fps=%d", n, w, h, fps))
		}
	}
	elements := append([]video.PlanElement(nil), scene.Elements...)
	sort.SliceStable(elements, func(a, b int) bool { return elements[a].Z < elements[b].Z })
	sceneDir := filepath.Join(filepath.Dir(out), fmt.Sprintf("scene-%03d-text", index+1))
	if err := os.MkdirAll(sceneDir, 0o700); err != nil {
		return err
	}
	textIdx := 0
	for _, el := range elements {
		switch el.Type {
		case "shape":
			filters = append(filters, drawBox(el, w, h))
		case "chart":
			filters = append(filters, chartBoxes(el, w, h)...)
		case "text":
			textIdx++
			path := filepath.Join(sceneDir, fmt.Sprintf("text-%02d.txt", textIdx))
			if err := os.WriteFile(path, []byte(el.Text), 0o600); err != nil {
				return err
			}
			filters = append(filters, drawText(font, path, el, w, h, ""))
		}
	}
	if strings.TrimSpace(scene.Caption) != "" {
		textIdx++
		path := filepath.Join(sceneDir, "caption.txt")
		if err := os.WriteFile(path, []byte(scene.Caption), 0o600); err != nil {
			return err
		}
		cap := video.PlanElement{Type: "text", X: 0.5, Y: 0.92, FontSize: 30, Color: "#ffffff"}
		filters = append(filters, drawText(font, path, cap, w, h, ":box=1:boxcolor=0x000000@0.55:boxborderw=12"))
	}
	if scene.Transition == "fade" {
		d := math.Min(0.4, dur/4)
		filters = append(filters,
			fmt.Sprintf("fade=t=in:st=0:d=%s", fnum(d)),
			fmt.Sprintf("fade=t=out:st=%s:d=%s", fnum(dur-d), fnum(d)))
	}
	filters = append(filters, "format=yuv420p")
	args := []string{"-y", "-f", "lavfi", "-i", src,
		"-vf", strings.Join(filters, ","),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p", "-an",
		"-t", fnum(dur), out}
	cmd := exec.CommandContext(ctx, r.ffmpeg(), args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg scene render failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("ffmpeg produced no scene output")
	}
	return nil
}

// slideOffset returns the animated pixel offset expression component for an
// element at time t: full offset at t=0 decaying to 0 over 0.7s.
func slideOffset(px int) string {
	if px == 0 {
		return "0"
	}
	return fmt.Sprintf("+(1-min(t/0.7\\,1))*%d", px)
}

func drawBox(el video.PlanElement, w, h int) string {
	x := int(math.Round(el.X * float64(w)))
	y := int(math.Round(el.Y * float64(h)))
	bw := int(math.Round(el.Width * float64(w)))
	bh := int(math.Round(el.Height * float64(h)))
	yExpr := strconv.Itoa(y)
	xExpr := strconv.Itoa(x)
	if el.Animation == "slide_up" {
		yExpr = fmt.Sprintf("'%d%s'", y, slideOffset(60))
	}
	if el.Animation == "slide_left" {
		xExpr = fmt.Sprintf("'%d%s'", x, slideOffset(60))
	}
	return fmt.Sprintf("drawbox=x=%s:y=%s:w=%d:h=%d:color=%s:t=fill", xExpr, yExpr, bw, bh, ffmpegColor(el.Color))
}

func chartBoxes(el video.PlanElement, w, h int) []string {
	max := 0.0
	for _, v := range el.Data {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		max = 1
	}
	x0 := el.X * float64(w)
	y0 := el.Y * float64(h)
	aw := el.Width * float64(w)
	ah := el.Height * float64(h)
	n := len(el.Data)
	barW := aw / float64(2*n-1)
	out := make([]string, 0, n)
	offset := ""
	if el.Animation == "slide_up" {
		offset = slideOffset(60)
	}
	for i, v := range el.Data {
		bh := int(math.Round(ah * v / max))
		if bh < 2 {
			bh = 2
		}
		bx := int(math.Round(x0 + float64(i)*2*barW))
		by := int(math.Round(y0+ah)) - bh
		yExpr := strconv.Itoa(by)
		if offset != "" {
			yExpr = fmt.Sprintf("'%d%s'", by, offset)
		}
		out = append(out, fmt.Sprintf("drawbox=x=%d:y=%s:w=%d:h=%d:color=%s:t=fill", bx, yExpr, int(math.Round(barW)), bh, ffmpegColor(el.Color)))
	}
	return out
}

func drawText(font, textFile string, el video.PlanElement, w, h int, extra string) string {
	size := el.FontSize * h / 720
	if size < 8 {
		size = 8
	}
	xExpr := fmt.Sprintf("'(w-text_w)*%s'", fnum(el.X))
	yExpr := fmt.Sprintf("'(h-text_h)*%s'", fnum(el.Y))
	if el.Animation == "slide_up" {
		yExpr = fmt.Sprintf("'(h-text_h)*%s%s'", fnum(el.Y), slideOffset(60))
	}
	if el.Animation == "slide_left" {
		xExpr = fmt.Sprintf("'(w-text_w)*%s%s'", fnum(el.X), slideOffset(80))
	}
	alpha := ""
	if el.Animation == "fade" {
		alpha = ":alpha='if(lt(t\\,0.6)\\,t/0.6\\,1)'"
	}
	return fmt.Sprintf("drawtext=%s:textfile=%s:fontsize=%d:fontcolor=%s:x=%s:y=%s%s%s",
		font, escapeFilterValue(textFile), size, ffmpegColor(el.Color), xExpr, yExpr, alpha, extra)
}

func ffmpegColor(hex string) string {
	return "0x" + strings.TrimPrefix(hex, "#")
}

func fnum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// escapeFilterValue escapes a value used inside a filter argument (paths we
// generate ourselves; this is defense in depth, not a trust boundary).
func escapeFilterValue(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ":", "\\:")
	s = strings.ReplaceAll(s, "'", "\\'")
	return s
}

// ProbeInfo is the ffprobe-derived truth about a rendered file.
type ProbeInfo struct {
	Codec   string
	Width   int
	Height  int
	Seconds float64
	Size    int64
}

// Probe runs ffprobe and returns container/stream facts. A missing or
// invalid file is an error, never a zero-value success.
func (r *Renderer) Probe(ctx context.Context, path string) (ProbeInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ProbeInfo{}, err
	}
	if info.Size() == 0 {
		return ProbeInfo{}, fmt.Errorf("file %s is empty", path)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.ffprobe(), "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	if err != nil {
		return ProbeInfo{}, fmt.Errorf("ffprobe failed for %s: %w", path, err)
	}
	var doc struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return ProbeInfo{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	var res ProbeInfo
	res.Size = info.Size()
	for _, s := range doc.Streams {
		if s.CodecType == "video" {
			res.Codec, res.Width, res.Height = s.CodecName, s.Width, s.Height
		}
	}
	if res.Codec == "" {
		return ProbeInfo{}, fmt.Errorf("file %s has no video stream", path)
	}
	if d, err := strconv.ParseFloat(doc.Format.Duration, 64); err == nil {
		res.Seconds = d
	}
	return res, nil
}

// FrameStats extracts one frame as small grayscale rawvideo and returns its
// luminance mean and variance. Near-zero variance means a blank frame.
func (r *Renderer) FrameStats(ctx context.Context, path string, atSeconds float64) (mean, variance float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"-v", "error", "-ss", fnum(atSeconds), "-i", path, "-frames:v", "1", "-vf", "scale=64:36,format=gray", "-f", "rawvideo", "-"}
	out, err := exec.CommandContext(ctx, r.ffmpeg(), args...).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("extract frame at %.2fs: %w", atSeconds, err)
	}
	if len(out) == 0 {
		return 0, 0, fmt.Errorf("no frame decoded at %.2fs", atSeconds)
	}
	sum := 0.0
	for _, b := range out {
		sum += float64(b)
	}
	mean = sum / float64(len(out))
	sq := 0.0
	for _, b := range out {
		d := float64(b) - mean
		sq += d * d
	}
	return mean, sq / float64(len(out)), nil
}

// ValidateOutput performs the deterministic post-render QC gate: the file
// must be a real H.264 MP4 at the plan's resolution, its duration must match
// the plan total within tolerance, and a mid-scene frame of every scene
// that carries visible elements must not be blank.
func (r *Renderer) ValidateOutput(ctx context.Context, plan video.Plan, path string) error {
	plan = plan.Normalized()
	info, err := r.Probe(ctx, path)
	if err != nil {
		return err
	}
	if info.Codec != "h264" {
		return fmt.Errorf("QC: output codec is %q, want h264", info.Codec)
	}
	if info.Width != plan.Width || info.Height != plan.Height {
		return fmt.Errorf("QC: output dimensions %dx%d do not match plan %dx%d", info.Width, info.Height, plan.Width, plan.Height)
	}
	want := plan.TotalDuration()
	if math.Abs(info.Seconds-want) > math.Max(1.0, want*0.15) {
		return fmt.Errorf("QC: output duration %.2fs does not match plan total %.2fs", info.Seconds, want)
	}
	// Blank-frame check on the joined output at each scene midpoint.
	at := 0.0
	for _, scene := range plan.Scenes {
		mid := at + scene.DurationSeconds/2
		_, variance, err := r.FrameStats(ctx, path, mid)
		if err != nil {
			return fmt.Errorf("QC: %w", err)
		}
		if variance < 1.0 {
			return fmt.Errorf("QC: scene %q frame at %.2fs is blank (luminance variance %.3f)", scene.ID, mid, variance)
		}
		at += scene.DurationSeconds
	}
	return nil
}

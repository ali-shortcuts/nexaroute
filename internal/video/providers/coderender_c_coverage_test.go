package providers_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/agent"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/render"
)

// Agent C coverage: the code-render job-directory and subtitle-write
// failure branches.

// longWorkRoot builds a work-root path of exactly target bytes under
// t.TempDir, using nested components that each fit NAME_MAX.
func longWorkRoot(t *testing.T, target int) string {
	t.Helper()
	path := t.TempDir()
	for len(path) < target {
		need := target - len(path)
		comp := need - 1
		if comp > 200 {
			comp = 200
		}
		if comp < 1 {
			break
		}
		path += "/" + strings.Repeat("d", comp)
	}
	if len(path) != target {
		t.Fatalf("work root length = %d, want %d", len(path), target)
	}
	return path
}

// TestCodeRenderSecondMkdirTempFailure drives the retry path in run():
// the first MkdirTemp fails because the work root is missing, MkdirAll
// creates it, and the second MkdirTemp fails because the full job path
// exceeds PATH_MAX. The root is 4085 bytes, so MkdirAll succeeds but the
// job directory (root + "/job-" + 10 random chars) is 4100 bytes.
func TestCodeRenderSecondMkdirTempFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the PATH_MAX long-path trigger is Linux-specific")
	}
	root := longWorkRoot(t, 4085)
	p := providers.NewCodeRender(nil, requireRenderStack(t), nil, root, []string{"m"}, 120)
	plan, err := agent.ParsePlan(planJSON)
	if err != nil {
		t.Fatal(err)
	}
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "long work root", Mode: video.ModeTextToVideo,
		ProviderPreference: providers.CodeRenderID, ModelPreference: "m",
		DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12, VideoPlan: &plan,
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	st := waitCompleted(t, p, job.ProviderJobID)
	if st.State != video.StateFailed || !strings.Contains(st.Error, "create code-render job directory") {
		t.Fatalf("job state=%s err=%q, want the job-directory failure", st.State, st.Error)
	}
}

// blockingNarrator writes a valid WAV and then leaves a directory where
// the subtitle file goes, so the subtitle write must fail.
type blockingNarrator struct{ fakeNarrator }

func (blockingNarrator) Synthesize(_ context.Context, text, outPath string) error {
	if err := (fakeNarrator{}).Synthesize(context.Background(), text, outPath); err != nil {
		return err
	}
	return os.Mkdir(filepath.Join(filepath.Dir(outPath), "subtitles.srt"), 0o700)
}

func TestCodeRenderSubtitlesWriteFailure(t *testing.T) {
	a := &agent.Agent{Planner: fakePlanner{out: planJSON}, MaxAttempts: 1}
	p := providers.NewCodeRender(a, &render.Renderer{}, blockingNarrator{}, t.TempDir(), []string{"deepseek-chat"}, 120)
	p.MaxAssetBytes = 512 << 20
	req := video.VideoRequest{
		ProjectID: "test", Prompt: "narrated", Mode: video.ModeTextToVideo,
		ProviderPreference: providers.CodeRenderID, ModelPreference: "deepseek-chat",
		DurationSeconds: 3, AspectRatio: video.Aspect16x9, FPS: 12,
		NarrationText: "Routing explained simply.", SubtitleLanguage: "en",
	}
	job, err := p.CreateJob(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	st := waitCompleted(t, p, job.ProviderJobID)
	if st.State != video.StateFailed || !strings.Contains(st.Error, "write subtitles") {
		t.Fatalf("job state=%s err=%q, want the subtitle-write failure", st.State, st.Error)
	}
}

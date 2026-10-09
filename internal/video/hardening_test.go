package video_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestFileStoreRecoversJob(t *testing.T) {
	path := t.TempDir() + "/jobs.json"
	s, err := queue.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	j := video.NewJob("j", video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	if err = s.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	s2, err := queue.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get(context.Background(), "j")
	if err != nil || got.Request.Prompt != "x" {
		t.Fatalf("recovery %#v %v", got, err)
	}
}
func TestStateTransitionRejectsTerminalRegression(t *testing.T) {
	j := video.NewJob("j", video.VideoRequest{ProjectID: "p"})
	j.State = video.StateCompleted
	if err := j.Transition(video.StateProcessing); err == nil {
		t.Fatal("expected invalid terminal transition")
	}
}
func TestWebhookDeduplicates(t *testing.T) {
	s := queue.NewMemoryStore()
	j := video.NewJob("j", video.VideoRequest{ProjectID: "p", Prompt: "x", DurationSeconds: 1, Mode: video.ModeTextToVideo})
	j.ProviderJobID = "pj"
	j.State = video.StateProcessing
	_ = s.Create(context.Background(), j)
	p := orchestrator.NewWebhookProcessor(s)
	st := video.ProviderJobStatus{ProviderJobID: "pj", State: video.StateCompleted, Progress: 1}
	if err := p.Apply(context.Background(), "evt", st); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), "evt", st); err != nil {
		t.Fatal(err)
	}
}
func TestWebhookHMAC(t *testing.T) {
	secret := "secret"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Video-Timestamp", ts)
	req.Header.Set("X-Video-Signature", hex.EncodeToString(m.Sum(nil)))
	if err := orchestrator.VerifyHMAC(req, secret); err != nil {
		t.Fatal(err)
	}
}

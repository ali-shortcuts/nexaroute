package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Agent C coverage: the ComfyUI rand-failure branch.

func TestComfyUICreateJobRandFailure(t *testing.T) {
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("synthetic rand failure") }
	t.Cleanup(func() { randRead = old })

	p, err := NewComfyUI(comfyTestConfig(comfyWorkflowFile(t), "http://127.0.0.1:8188"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateJob(context.Background(), comfy651Request()); err == nil || !strings.Contains(err.Error(), "generate ComfyUI client ID") {
		t.Fatalf("rand failure err = %v, want the client-ID error", err)
	}
}

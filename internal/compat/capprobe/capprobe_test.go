package ccaprobe

import (
	"context"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/compat/capabilities"
)

func fakeDoer(t *testing.T, rejectTemp bool) ChatDoer {
	return func(ctx context.Context, payload map[string]any, stream bool) (int, []byte, error) {
		if stream {
			return 200, []byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"), nil
		}
		if rejectTemp {
			if _, ok := payload["temperature"]; ok {
				return 400, []byte(`{"error":{"message":"temperature is not supported"}}`), nil
			}
		}
		if _, ok := payload["tools"]; ok {
			return 200, []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`), nil
		}
		return 200, []byte(`{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`), nil
	}
}

func TestLevelBMatrix(t *testing.T) {
	r := Runner{Model: "m", Do: fakeDoer(t, true)}
	rep := r.Run(context.Background())
	if rep.LevelA.Outcome != OutcomePass {
		t.Fatalf("level A: %+v", rep.LevelA)
	}
	if len(rep.LevelB) != len(LevelB()) {
		t.Fatalf("expected %d cases, got %d", len(LevelB()), len(rep.LevelB))
	}
	byKey := map[string]Result{}
	for _, res := range rep.LevelB {
		byKey[res.Key] = res
	}
	if byKey["temperature"].Outcome != OutcomeFail {
		t.Fatalf("temperature should FAIL: %+v", byKey["temperature"])
	}
	if !strings.HasPrefix(byKey["temperature"].Detail, "unsupported:") {
		t.Fatalf("temperature detail should carry unsupported marker: %+v", byKey["temperature"])
	}
	if byKey["tools"].Outcome != OutcomePass || byKey["streaming"].Outcome != OutcomePass {
		t.Fatalf("tools/streaming should pass: %+v", byKey)
	}
}

func TestApplyToStore(t *testing.T) {
	r := Runner{Model: "m", Do: fakeDoer(t, true)}
	rep := r.Run(context.Background())
	store := capabilities.NewStore()
	rep.ApplyToStore(store, "p/m")
	m, ok := store.Get("p/m")
	if !ok {
		t.Fatalf("store should have contract")
	}
	if m.Get("temperature").Value != capabilities.Unsupported {
		t.Fatalf("temperature should be UNSUPPORTED: %+v", m.Get("temperature"))
	}
	if m.Get("tools").Value != capabilities.Supported {
		t.Fatalf("tools should be SUPPORTED")
	}
}

func TestAuthFailureIsUnknown(t *testing.T) {
	r := Runner{Model: "m", Do: func(ctx context.Context, p map[string]any, s bool) (int, []byte, error) {
		return 401, []byte(`{"error":{"message":"invalid key"}}`), nil
	}}
	rep := r.RunLevelA(context.Background())
	if rep.Outcome != OutcomeUnknown {
		t.Fatalf("auth failure must be UNKNOWN (credential problem): %+v", rep)
	}
}

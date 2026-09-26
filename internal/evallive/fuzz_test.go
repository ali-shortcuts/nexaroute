package evallive

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

// FuzzLiveExecutor_UpstreamResponse asserts that an arbitrary — possibly hostile
// — upstream body can never break the evaluation plane: no panic, no outcome
// that escapes the bounded artifact contract, and no verdict invented from
// garbage.
func FuzzLiveExecutor_UpstreamResponse(f *testing.F) {
	f.Add(`{"choices":[{"message":{"content":"42"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	f.Add(`{"choices":[]}`)
	f.Add(`not json at all`)
	f.Add(`{"choices":[{"message":{"content":"` + strings.Repeat("A", 70000) + `"}}]}`)
	f.Add(`{"error":{"message":"boom"}}`)
	f.Add(`{"candidates":[{"content":{"parts":[{"text":"NEEDLE-A"}]}}],"usageMetadata":{"promptTokenCount":9}}`)
	f.Add(`{"content":[{"type":"text","text":"carol"}],"usage":{"input_tokens":3,"output_tokens":4}}`)
	f.Add(`{"output":[{"content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	f.Add(`[]`)
	f.Add(`{"choices":{"message":{"content":"42"}}}`)

	// The upstream echoes the *prompt* back as its response body, so the fuzzer's
	// bytes arrive verbatim as the model output.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<21)).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if len(in.Messages) > 0 {
			_, _ = w.Write([]byte(in.Messages[0].Content))
		}
	}))
	defer srv.Close()

	c := eval.Case{ID: "reasoning-multi-step-arithmetic", Weight: 1, Expectation: eval.Expectation{Kind: eval.ExpectExactMatch, Expected: "42"}}

	f.Fuzz(func(t *testing.T, raw string) {
		exec2, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL),
			[]Prompt{{CaseID: c.ID, Prompt: raw}}, 0)
		if err != nil {
			return // oversized or malformed input is rejected, never coerced
		}
		out, err := exec2.Execute(context.Background(), c)
		if err != nil {
			return
		}
		if err := out.Validate(); err != nil {
			t.Fatalf("live outcome escaped the artifact contract: %v", err)
		}
		if len(out.Output) > eval.MaxOutputBytes {
			t.Fatalf("live output = %d bytes, exceeds the artifact bound", len(out.Output))
		}
		if out.LatencyMS < 0 {
			t.Fatalf("negative latency %d", out.LatencyMS)
		}
		switch out.Status {
		case eval.OutcomeOK, eval.OutcomeError, eval.OutcomeTimeout, eval.OutcomeRefused:
		default:
			t.Fatalf("non-canonical outcome status %q", out.Status)
		}
	})
}

// FuzzLiveExecutor_Prompts guards the construction boundary: arbitrary prompts
// and case ids must either be rejected or produce a well-formed executor.
func FuzzLiveExecutor_Prompts(f *testing.F) {
	f.Add("reasoning-multi-step-arithmetic", "compute 42")
	f.Add("", "prompt without a case")
	f.Add("unknown-case", "hello")
	f.Add("reasoning-multi-step-arithmetic", "")
	f.Add("reasoning-multi-step-arithmetic", strings.Repeat("x", 100000))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"42"}}]}`))
	}))
	defer srv.Close()
	twin := liveTwin(f, srv.URL)

	f.Fuzz(func(t *testing.T, caseID, prompt string) {
		exec, err := NewLiveEvaluationExecutor(liveDeployment(), twin, []Prompt{{CaseID: caseID, Prompt: prompt}}, 0)
		if err != nil {
			return
		}
		if exec.Calls() != 0 || exec.UpstreamCalls() != 0 {
			t.Fatalf("a freshly built executor reports calls: %d/%d", exec.Calls(), exec.UpstreamCalls())
		}
		out, err := exec.Execute(context.Background(), eval.Case{ID: caseID, Weight: 1,
			Expectation: eval.Expectation{Kind: eval.ExpectExactMatch, Expected: "42"}})
		if err != nil {
			return
		}
		if out.CaseID != caseID {
			t.Fatalf("outcome case id = %q, want %q", out.CaseID, caseID)
		}
		if err := out.Validate(); err != nil {
			t.Fatalf("outcome invalid: %v", err)
		}
	})
}

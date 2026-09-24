package repair

import (
	"encoding/json"
	"testing"

	compaterrors "github.com/ali-shortcuts/nexaroute/internal/compat/errors"
)

func unsupported(param string) compaterrors.Result {
	return compaterrors.Result{Class: compaterrors.UnsupportedParameter, Parameter: param, RetryableRepair: true}
}

func TestMaxCompletionMapping(t *testing.T) {
	payload := []byte(`{"model":"m","max_completion_tokens":64,"messages":[]}`)
	out, att, ok := Apply(payload, unsupported("max_completion_tokens"))
	if !ok || att.Rule != RuleMaxCompletionToMax {
		t.Fatalf("expected mapping rule, got %+v ok=%v", att, ok)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if _, has := obj["max_completion_tokens"]; has {
		t.Fatalf("max_completion_tokens should be renamed")
	}
	if obj["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens missing: %s", out)
	}
}

func TestDropTemperature(t *testing.T) {
	payload := []byte(`{"model":"m","temperature":0.7,"messages":[]}`)
	out, att, ok := Apply(payload, unsupported("temperature"))
	if !ok || att.Rule != RuleDropUnsupportedParam {
		t.Fatalf("expected drop rule, got %+v ok=%v", att, ok)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if _, has := obj["temperature"]; has {
		t.Fatalf("temperature should be dropped")
	}
}

func TestDropStreamOptions(t *testing.T) {
	payload := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)
	if _, _, ok := Apply(payload, unsupported("stream_options")); !ok {
		t.Fatalf("stream_options should be repairable")
	}
}

func TestDropReasoningEffort(t *testing.T) {
	payload := []byte(`{"model":"m","reasoning_effort":"high"}`)
	if _, _, ok := Apply(payload, unsupported("reasoning_effort")); !ok {
		t.Fatalf("reasoning_effort should be repairable")
	}
}

func TestUnknownParamDeclined(t *testing.T) {
	payload := []byte(`{"model":"m"}`)
	if _, _, ok := Apply(payload, unsupported("mystery_field")); ok {
		t.Fatalf("unknown parameter must decline repair (bounded)")
	}
}

func TestMaxTokensItselfDeclined(t *testing.T) {
	payload := []byte(`{"model":"m","max_tokens":64}`)
	if _, _, ok := Apply(payload, unsupported("max_tokens")); ok {
		t.Fatalf("max_tokens unsupported must decline (no safe flip-flop)")
	}
}

func TestNonRepairableClassDeclined(t *testing.T) {
	payload := []byte(`{"model":"m","temperature":0.7}`)
	r := compaterrors.Result{Class: compaterrors.InvalidRequestSchema}
	if _, _, ok := Apply(payload, r); ok {
		t.Fatalf("non-repairable class must decline")
	}
}

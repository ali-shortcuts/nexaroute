package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression tests for the native Anthropic SSE usage-tracking defect:
// observeAnthropicUsage used to unmarshal env["message"] into a struct
// expecting another nested "message" object (and env["usage"] into a
// struct expecting another nested "usage" object), so token counts
// always stayed zero and the usage hook never fired at message_stop.
// These tests fail on the old code (hook never called) and pass on
// the fixed code. Fixtures use the real Anthropic SSE event shapes.

type nativeUsageCall struct {
	prompt, completion int
}

type nativeUsageHook struct {
	calls []nativeUsageCall
}

func (h *nativeUsageHook) fn(prompt, completion int) {
	h.calls = append(h.calls, nativeUsageCall{prompt, completion})
}

func runAnthropicTracker(t *testing.T, chunks []string) (*nativeSSETracker, *nativeUsageHook, error) {
	t.Helper()
	hook := &nativeUsageHook{}
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: hook.fn}
	for _, c := range chunks {
		if err := tr.consume([]byte(c)); err != nil {
			return tr, hook, err
		}
	}
	return tr, hook, tr.finish()
}

func TestNativeSSEAnthropicUsageExactCounts(t *testing.T) {
	frames := []string{
		"event: message_start\n",
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":123,\"output_tokens\":0}}}\n",
		"\n",
		"event: content_block_delta\n",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n",
		"\n",
		"event: message_delta\n",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":456}}\n",
		"\n",
		"event: message_stop\n",
		"data: {\"type\":\"message_stop\"}\n",
		"\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("hook calls = %d, want exactly 1 (calls=%v)", len(hook.calls), hook.calls)
	}
	if hook.calls[0] != (nativeUsageCall{prompt: 123, completion: 456}) {
		t.Fatalf("hook call = %+v, want {prompt:123 completion:456}", hook.calls[0])
	}
}

func TestNativeSSEAnthropicUsageExactlyOnce(t *testing.T) {
	frames := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n",
		"data: {\"type\":\"message_stop\"}\n",
		// Duplicate stop and a late delta must not re-fire the hook.
		"data: {\"type\":\"message_stop\"}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":99}}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("hook calls = %d, want exactly 1 (calls=%v)", len(hook.calls), hook.calls)
	}
	if hook.calls[0] != (nativeUsageCall{prompt: 10, completion: 20}) {
		t.Fatalf("hook call = %+v, want {prompt:10 completion:20}", hook.calls[0])
	}
}

func TestNativeSSEAnthropicUsageAccumulatesAcrossDeltas(t *testing.T) {
	frames := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":40,\"output_tokens\":0}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":10}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":25}}\n",
		// A lower late value must not move the maximum backwards.
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":12}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 1 || hook.calls[0] != (nativeUsageCall{prompt: 40, completion: 25}) {
		t.Fatalf("hook calls = %v, want exactly [{prompt:40 completion:25}]", hook.calls)
	}
}

func TestNativeSSEAnthropicUsageMissingUsage(t *testing.T) {
	frames := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 0 {
		t.Fatalf("hook calls = %v, want none (no usage was ever reported)", hook.calls)
	}
}

func TestNativeSSEAnthropicUsagePartialStartOnly(t *testing.T) {
	frames := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":50}}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 1 || hook.calls[0] != (nativeUsageCall{prompt: 50, completion: 0}) {
		t.Fatalf("hook calls = %v, want exactly [{prompt:50 completion:0}]", hook.calls)
	}
}

func TestNativeSSEAnthropicUsagePartialDeltaOnly(t *testing.T) {
	frames := []string{
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":30}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 1 || hook.calls[0] != (nativeUsageCall{prompt: 0, completion: 30}) {
		t.Fatalf("hook calls = %v, want exactly [{prompt:0 completion:30}]", hook.calls)
	}
}

func TestNativeSSEAnthropicUsageMalformedUsageIgnored(t *testing.T) {
	frames := []string{
		// usage as a string and usage with a wrong-typed field must not
		// crash the stream and must not produce garbage counts.
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":\"not-an-object\"}}\n",
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":\"abc\"}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":9}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 1 || hook.calls[0] != (nativeUsageCall{prompt: 0, completion: 9}) {
		t.Fatalf("hook calls = %v, want exactly [{prompt:0 completion:9}]", hook.calls)
	}
}

func TestNativeSSEAnthropicUsageMalformedEventStillRejected(t *testing.T) {
	// Validation is not weakened by the fix: a wholly invalid SSE JSON
	// payload must still fail the stream.
	frames := []string{
		"data: {not json\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err == nil {
		t.Fatalf("expected an error for malformed SSE JSON, got nil (hook calls=%v)", hook.calls)
	}
	if len(hook.calls) != 0 {
		t.Fatalf("hook calls = %v, want none on malformed input", hook.calls)
	}
}

func TestNativeSSEAnthropicUsageTruncatedStreamFiresNothing(t *testing.T) {
	// No message_stop: the stream is truncated, finish() must report
	// unexpected EOF and the hook must not claim complete usage.
	frames := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":77,\"output_tokens\":0}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":88}}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("finish error = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(hook.calls) != 0 {
		t.Fatalf("hook calls = %v, want none for a truncated stream", hook.calls)
	}
}

func TestNativeSSEAnthropicUsageSplitLineReassembled(t *testing.T) {
	// A single SSE line split across TCP chunks must still parse:
	// the tracker buffers partial lines until '\n'.
	frames := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":",
		"61,\"output_tokens\":0}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":7}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	_, hook, err := runAnthropicTracker(t, frames)
	if err != nil {
		t.Fatalf("tracker failed: %v", err)
	}
	if len(hook.calls) != 1 || hook.calls[0] != (nativeUsageCall{prompt: 61, completion: 7}) {
		t.Fatalf("hook calls = %v, want exactly [{prompt:61 completion:7}]", hook.calls)
	}
}

func TestNativeSSEAnthropicUsageViaProxy(t *testing.T) {
	body := strings.Join([]string{
		"event: message_start",
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":123,\"output_tokens\":0}}}",
		"",
		"event: message_delta",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":456}}",
		"",
		"event: message_stop",
		"data: {\"type\":\"message_stop\"}",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	hook := &nativeUsageHook{}
	rr := httptest.NewRecorder()
	// Empty public model: no SSE model rewriting, so the body must pass
	// through verbatim and only the usage accounting is under test.
	if err := proxyNativeSSEWithModel(rr, resp, "anthropic", "", hook.fn); err != nil {
		t.Fatalf("proxy failed: %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != body {
		t.Fatalf("proxy body was not passed through verbatim")
	}
	if len(hook.calls) != 1 || hook.calls[0] != (nativeUsageCall{prompt: 123, completion: 456}) {
		t.Fatalf("hook calls = %v, want exactly [{prompt:123 completion:456}]", hook.calls)
	}
}

func TestNativeSSEAnthropicUsageViaProxyTruncated(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	hook := &nativeUsageHook{}
	rr := httptest.NewRecorder()
	err := proxyNativeSSEWithModel(rr, resp, "anthropic", "", hook.fn)
	if err == nil {
		t.Fatalf("expected an error for a truncated stream, got nil")
	}
	if len(hook.calls) != 0 {
		t.Fatalf("hook calls = %v, want none for a truncated stream", hook.calls)
	}
}

func TestNativeSSEOpenAIUsageUnchanged(t *testing.T) {
	// Guard: the fix touches only the Anthropic branch; the OpenAI
	// native branch must keep working exactly as before.
	tr := &nativeSSETracker{protocol: "openai"}
	hook := &nativeUsageHook{}
	tr.usageHook = hook.fn
	frames := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n",
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22}}\n",
		"data: [DONE]\n",
	}
	for _, f := range frames {
		if err := tr.consume([]byte(f)); err != nil {
			t.Fatalf("tracker failed: %v", err)
		}
	}
	if len(hook.calls) != 1 || hook.calls[0] != (nativeUsageCall{prompt: 11, completion: 22}) {
		t.Fatalf("hook calls = %v, want exactly [{prompt:11 completion:22}]", hook.calls)
	}
}

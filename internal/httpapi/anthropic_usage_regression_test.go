package httpapi

import "testing"

// Regression test for the Anthropic native-SSE usage accounting defect:
// observeAnthropicUsage unmarshaled env["message"] and env["usage"] each
// expecting one wrapper level more than real Anthropic SSE envelopes carry
// (message_start carries {"message": {"usage": ...}}, message_delta carries
// a top-level "usage" object), so streaming usage totals stayed zero and the
// message_stop hook never fired. These tests feed real envelopes and assert
// the hook fires with the real totals. They FAIL on the pre-fix code.
func TestAnthropicSSEUsageHookFiresWithRealEnvelopes(t *testing.T) {
	var got []nativeSSEUsage
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) {
		got = append(got, u)
	}}
	feed := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{\"text\":\"hi\"},\"usage\":{\"output_tokens\":5}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	for _, line := range feed {
		if err := tr.consume([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if !tr.terminal {
		t.Error("message_stop did not terminate the tracker")
	}
	if len(got) != 1 || got[0].Input != 11 || got[0].Output != 5 {
		t.Fatalf("anthropic usage hook = %v, want exactly one call (11, 5)", got)
	}
	if err := tr.finish(); err != nil {
		t.Errorf("finish failed: %v", err)
	}
}

func TestAnthropicSSEUsageDeltaTakesMaxAcrossEvents(t *testing.T) {
	var got []nativeSSEUsage
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) {
		got = append(got, u)
	}}
	feed := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"input_tokens\":13,\"output_tokens\":3}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":9}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	for _, line := range feed {
		if err := tr.consume([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 || got[0].Input != 13 || got[0].Output != 9 {
		t.Fatalf("anthropic usage hook = %v, want exactly one call (13, 9)", got)
	}
}

func TestAnthropicSSEUsageHookSilentWithZeroTotals(t *testing.T) {
	var got []nativeSSEUsage
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) {
		got = append(got, u)
	}}
	for _, line := range []string{
		"data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	} {
		if err := tr.consume([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 0 {
		t.Fatalf("hook fired with zero totals: %v", got)
	}
}

// Cache tokens carried by Anthropic SSE usage objects must reach the hook:
// message_start reports cache_creation/cache_read, later deltas may raise
// cache_read, and the terminal observation must carry the maxima. Without
// this, streamed cache usage is silently dropped from accounting even after
// the input/output unwrap fix.
func TestAnthropicSSEUsageHookCarriesCacheTokens(t *testing.T) {
	var got []nativeSSEUsage
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(u nativeSSEUsage) {
		got = append(got, u)
	}}
	feed := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":0,\"cache_creation_input_tokens\":7,\"cache_read_input_tokens\":3}}}\n",
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":4,\"cache_read_input_tokens\":9}}\n",
		"data: {\"type\":\"message_stop\"}\n",
	}
	for _, line := range feed {
		if err := tr.consume([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 || got[0].Input != 20 || got[0].Output != 4 || got[0].CacheWrite != 7 || got[0].CacheRead != 9 {
		t.Fatalf("anthropic usage hook = %v, want exactly one call (20, 4, cacheRead 9, cacheWrite 7)", got)
	}
}

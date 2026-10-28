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
	var got []struct{ p, c int }
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(p, c int) {
		got = append(got, struct{ p, c int }{p, c})
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
	if len(got) != 1 || got[0].p != 11 || got[0].c != 5 {
		t.Fatalf("anthropic usage hook = %v, want exactly one call (11, 5)", got)
	}
	if err := tr.finish(); err != nil {
		t.Errorf("finish failed: %v", err)
	}
}

func TestAnthropicSSEUsageDeltaTakesMaxAcrossEvents(t *testing.T) {
	var got []struct{ p, c int }
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(p, c int) {
		got = append(got, struct{ p, c int }{p, c})
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
	if len(got) != 1 || got[0].p != 13 || got[0].c != 9 {
		t.Fatalf("anthropic usage hook = %v, want exactly one call (13, 9)", got)
	}
}

func TestAnthropicSSEUsageHookSilentWithZeroTotals(t *testing.T) {
	var got []struct{ p, c int }
	tr := &nativeSSETracker{protocol: "anthropic", usageHook: func(p, c int) {
		got = append(got, struct{ p, c int }{p, c})
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

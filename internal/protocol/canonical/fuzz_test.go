package canonical

// Wave C-2 (#403) supervisor-takeover fuzz targets for the canonical
// protocol layer: the five provider SSE dialects plus the stream pump
// framing, and the request/response codec round-trips.
//
// Invariants under arbitrary input:
//   - no panics anywhere in the decode path;
//   - feeding a stream byte-by-byte through SSEReader yields exactly the
//     same frame/event sequence as feeding it whole (chunk-split
//     invariance — TCP segmentation must never change semantics);
//   - decode(encode(decode(x))) is stable for well-formed inputs, modulo
//     documented representation shifts: the Raw passthrough field, the
//     Model routing field, the pinned system-prompt placement, the
//     inherent OpenAI stop-reason conflation, and Anthropic's required
//     max_tokens default (see responseEqualSansRaw/normalizeRequest).
//
// The "no tool events after End" pump-level invariant is enforced and
// fuzz-guarded in internal/httpapi (FuzzStreamPumpClient), because the
// per-frame decoders are intentionally stateless.

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/core"
)

// streamCodecs enumerates the upstream SSE dialects in fuzz-selector order.
var streamCodecs = []string{"openai_chat", "openai_responses", "anthropic", "gemini", "bedrock"}

// fuzzFrame is one decoded SSE frame with the pump's latched terminal state.
type fuzzFrame struct {
	Name     string
	Data     string
	Events   []StreamEvent
	Terminal bool // latched: terminal || any earlier frame terminal
	Err      string
}

// dripReader yields its payload one byte at a time, simulating the most
// adversarial TCP segmentation possible.
type dripReader struct{ b []byte }

func (d *dripReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(d.b) == 0 {
		return 0, io.EOF
	}
	p[0] = d.b[0]
	d.b = d.b[1:]
	return 1, nil
}

func decodeFuzzFrame(codec int, name, data string) (evs []StreamEvent, terminal bool, err error) {
	switch streamCodecs[codec] {
	case "openai_chat":
		return DecodeOpenAIStreamChunk(data)
	case "openai_responses":
		return DecodeResponsesStreamEvent(name, data)
	case "anthropic":
		evs, err = DecodeAnthropicStreamEvent(name, data)
		return evs, false, err
	case "gemini":
		return DecodeGeminiStreamChunk(data)
	case "bedrock":
		return DecodeBedrockStreamChunk(data)
	}
	return nil, false, nil
}

// pumpFuzzFrames mirrors the canonicalStreamPump frame loop (minus the
// client emitter): SSEReader framing, per-codec decode, terminal latch.
// Anthropic has no per-chunk terminal boolean, so the latch follows the
// pump: a StreamEnd event latches terminal, exactly as in
// internal/httpapi/canonical_path.go.
func pumpFuzzFrames(codec int, r io.Reader) []fuzzFrame {
	sr := NewSSEReader(r)
	var out []fuzzFrame
	terminal := false
	for {
		name, data, done, err := sr.Next()
		if err != nil {
			out = append(out, fuzzFrame{Err: "sse: " + err.Error(), Terminal: terminal})
			break
		}
		if done {
			break
		}
		fr := fuzzFrame{Name: name, Data: data}
		evs, chunkTerminal, derr := decodeFuzzFrame(codec, name, data)
		if derr != nil {
			fr.Err = derr.Error()
		} else {
			fr.Events = evs
		}
		if fr.Err == "" {
			terminal = terminal || chunkTerminal
			if streamCodecs[codec] == "anthropic" {
				for _, ev := range evs {
					if ev.Type == StreamEnd {
						terminal = true
					}
				}
			}
		}
		fr.Terminal = terminal
		out = append(out, fr)
		if len(out) > 20000 {
			break // bound pathological frame counts
		}
	}
	return out
}

func FuzzStreamPump(f *testing.F) {
	f.Add(byte(0), []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n"))
	f.Add(byte(1), []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"))
	f.Add(byte(2), []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	f.Add(byte(3), []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"!\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	f.Add(byte(4), []byte("data: {\"eventType\":\"contentBlockDelta\",\"payload\":{\"contentBlockIndex\":0,\"delta\":{\"text\":\"hi\"}}}\n\ndata: {\"eventType\":\"messageStop\",\"payload\":{\"stopReason\":\"end_turn\"}}\n\n"))
	f.Fuzz(func(t *testing.T, codecByte byte, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip("input too large for a bounded fuzz iteration")
		}
		codec := int(codecByte) % len(streamCodecs)
		whole := pumpFuzzFrames(codec, bytes.NewReader(raw))
		dripped := pumpFuzzFrames(codec, &dripReader{b: append([]byte(nil), raw...)})
		if !reflect.DeepEqual(whole, dripped) {
			t.Fatalf("codec %s: byte-by-byte chunking changed the decoded sequence:\nwhole:   %+v\ndripped: %+v",
				streamCodecs[codec], whole, dripped)
		}
	})
}

// TestResponsesStringContentRoundTrip is the minimized regression for the
// defect found by FuzzRequestRoundTrip seed#2 (#403): the Responses
// encoder emits message items with string content, which the decoder
// rejected ("cannot unmarshal string into ..."), so encode(decode(x))
// was not decodable. The decoder now accepts both content shapes.
func TestResponsesStringContentRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	var in ResponsesRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	req, err := DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 || req.Messages[0].Parts[0].Text != "hi" {
		t.Fatalf("unexpected IR: %+v", req.Messages)
	}
	enc, err := EncodeResponsesRequest(req, "m")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Contains(enc, []byte(`"content":"hi"`)) {
		t.Fatalf("encoder stopped emitting compact string content: %s", enc)
	}
	var in2 ResponsesRequest
	if err := json.Unmarshal(enc, &in2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	req2, err := DecodeResponsesRequest(in2)
	if err != nil {
		t.Fatalf("RED: re-decode of encoder output failed: %v", err)
	}
	a, _ := json.Marshal(req)
	b, _ := json.Marshal(req2)
	if !bytes.Equal(a, b) {
		t.Fatalf("round-trip unstable:\n%s\n%s", a, b)
	}
	// The compact string-content shape must also decode directly (it is
	// valid per the Responses API and is what the encoder emits).
	var direct ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":"direct"}]}`), &direct); err != nil {
		t.Fatal(err)
	}
	dreq, err := DecodeResponsesRequest(direct)
	if err != nil {
		t.Fatalf("string content item rejected: %v", err)
	}
	if len(dreq.Messages) != 1 || dreq.Messages[0].Parts[0].Text != "direct" {
		t.Fatalf("string content lost: %+v", dreq.Messages)
	}
}

// TestDecodeOpenAIChatResponseDropsNamelessToolCall is the minimized
// regression for the defect found by FuzzResponseRoundTrip (#403): the
// decoder kept nameless tool calls in the IR while no encoder emits them
// (EncodeOpenAIChatResponse and the Anthropic request encoder both drop
// them), so decode(encode(decode(x))) silently lost the block and a
// hostile upstream could inject phantom tool-call blocks.
func TestDecodeOpenAIChatResponseDropsNamelessToolCall(t *testing.T) {
	raw := []byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{}}]},"finish_reason":"tool_calls"}]}`)
	resp, err := DecodeOpenAIChatResponse(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, b := range resp.Blocks {
		if b.Type == PartToolCall {
			t.Fatalf("RED: nameless tool call kept in IR: %+v", b.ToolCall)
		}
	}
	// Round-trip must now be stable: the encoder drops nothing that the
	// decoder kept.
	enc, ok := encodeFuzzResponse(1, resp)
	if !ok {
		t.Fatal("encode declined")
	}
	resp2, ok := decodeFuzzResponse(1, enc)
	if !ok {
		t.Fatal("re-decode of encoder output failed")
	}
	if !responseEqualSansRaw(resp, resp2) {
		t.Fatalf("round-trip unstable after fix:\n%s\n%s", mustFuzzJSON(t, resp), mustFuzzJSON(t, resp2))
	}
}

// TestDecodeAnthropicResponseDropsUnsignedThinking is the minimized
// regression for the defect found by FuzzResponseRoundTrip (#447): the
// decoder kept unsigned/redacted thinking blocks in the IR while the
// encoder never emits them (EncodeAnthropicResponse only surfaces signed
// thinking, since unsigned would poison replay per COMPATIBILITY.md rule
// 2), so decode(encode(decode(x))) silently turned a thinking block into
// a text padding block. The decoder now drops exactly what the encoder
// cannot emit, mirroring the nameless tool-call drop (#403). Signed
// thinking must survive verbatim: no block-type normalization.
func TestDecodeAnthropicResponseDropsUnsignedThinking(t *testing.T) {
	for _, raw := range []string{
		`{"Content":[{"tYpe":"thinking"}]}`,
		`{"content":[{"type":"thinking","thinking":"x"}]}`,
		`{"content":[{"type":"thinking","thinking":"x","signature":""}]}`,
		`{"content":[{"type":"redacted_thinking","redacted_thinking":"hidden"}]}`,
	} {
		resp, err := DecodeAnthropicResponse([]byte(raw))
		if err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		for _, b := range resp.Blocks {
			if b.Type == PartThinking {
				t.Fatalf("RED: unencodable thinking kept in IR for %s: %+v", raw, b.Thinking)
			}
		}
		enc, ok := encodeFuzzResponse(0, resp)
		if !ok {
			t.Fatal("encode declined")
		}
		resp2, ok := decodeFuzzResponse(0, enc)
		if !ok {
			t.Fatalf("re-decode of encoder output failed for %s", raw)
		}
		if !responseEqualSansRaw(resp, resp2) {
			t.Fatalf("round-trip unstable after fix for %s:\n%s\n%s", raw, mustFuzzJSON(t, resp), mustFuzzJSON(t, resp2))
		}
	}
	// Signed thinking is meaningful and must survive verbatim.
	signed, err := DecodeAnthropicResponse([]byte(`{"id":"m","type":"message","role":"assistant","model":"up","content":[{"type":"thinking","thinking":"why","signature":"sig"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`))
	if err != nil {
		t.Fatalf("signed decode: %v", err)
	}
	if len(signed.Blocks) != 1 || signed.Blocks[0].Type != PartThinking || signed.Blocks[0].Thinking.Text != "why" || signed.Blocks[0].Thinking.Signature != "sig" {
		t.Fatalf("signed thinking lost or altered: %+v", signed.Blocks)
	}
	senc, ok := encodeFuzzResponse(0, signed)
	if !ok {
		t.Fatal("signed encode declined")
	}
	signed2, ok := decodeFuzzResponse(0, senc)
	if !ok {
		t.Fatal("signed re-decode failed")
	}
	if !responseEqualSansRaw(signed, signed2) {
		t.Fatalf("signed round-trip unstable:\n%s\n%s", mustFuzzJSON(t, signed), mustFuzzJSON(t, signed2))
	}
}

// TestDecodeAnthropicResponseNormalizesEmptyToolInput is the minimized
// regression for the second defect FuzzResponseRoundTrip found on the #447
// branch (CI seed 27b6689a0474e4ff): a tool_use block with an empty,
// missing, or null input decoded to Arguments "{}"/"null" while the
// encoder's `input,omitempty` drops empty maps, so the re-decode came back
// "null" — decode(encode(decode(x))) unstable. The decoder now collapses
// the indistinguishable shapes to "{}", the canonical form the encoder
// converges to (mirrors the #447 unsigned-thinking fix).
func TestDecodeAnthropicResponseNormalizesEmptyToolInput(t *testing.T) {
	emptyShapes := []string{
		`{"content":[{"type":"tool_use","id":"x","name":"n"}]}`,
		`{"content":[{"type":"tool_use","id":"x","name":"n","input":null}]}`,
		`{"content":[{"type":"tool_use","id":"x","name":"n","input":{}}]}`,
	}
	for _, raw := range emptyShapes {
		t.Run(raw, func(t *testing.T) {
			resp, err := DecodeAnthropicResponse([]byte(raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Blocks) != 1 || resp.Blocks[0].Type != PartToolCall {
				t.Fatalf("expected one tool_call block, got %+v", resp.Blocks)
			}
			if got := resp.Blocks[0].ToolCall.Arguments; got != "{}" {
				t.Fatalf("RED: empty tool input not canonicalized, args=%q", got)
			}
			enc, ok := encodeFuzzResponse(0, resp)
			if !ok {
				t.Fatal("encode declined")
			}
			resp2, ok := decodeFuzzResponse(0, enc)
			if !ok {
				t.Fatal("re-decode of encoder output failed")
			}
			if got := resp2.Blocks[0].ToolCall.Arguments; got != "{}" {
				t.Fatalf("round-trip drifted: args=%q", got)
			}
			if !responseEqualSansRaw(resp, resp2) {
				t.Fatalf("round-trip unstable:\n%s\n%s", mustFuzzJSON(t, resp), mustFuzzJSON(t, resp2))
			}
		})
	}
	// Non-empty input must be preserved (canonical JSON form) and stable.
	nonEmpty := `{"content":[{"type":"tool_use","id":"x","name":"n","input":{"b":2,"a":1}}]}`
	resp, err := DecodeAnthropicResponse([]byte(nonEmpty))
	if err != nil {
		t.Fatalf("decode non-empty: %v", err)
	}
	if got := resp.Blocks[0].ToolCall.Arguments; got != `{"a":1,"b":2}` {
		t.Fatalf("non-empty input altered: args=%q", got)
	}
	enc, ok := encodeFuzzResponse(0, resp)
	if !ok {
		t.Fatal("encode declined")
	}
	resp2, ok := decodeFuzzResponse(0, enc)
	if !ok {
		t.Fatal("re-decode failed")
	}
	if !responseEqualSansRaw(resp, resp2) {
		t.Fatalf("non-empty round-trip unstable:\n%s\n%s", mustFuzzJSON(t, resp), mustFuzzJSON(t, resp2))
	}
}

// TestDecodeResponsesResponseDropsNamelessFunctionCall is the minimized
// regression for the defect found by FuzzResponseRoundTrip (#447): the
// Responses decoder kept function_call items with an empty name in the IR
// while EncodeResponsesResponse never emits them (no encoder emits a
// nameless call), so decode(encode(decode(x))) silently lost the block and
// flipped stop_reason tool_use -> end_turn. Mirrors
// TestDecodeOpenAIChatResponseDropsNamelessToolCall (#403).
func TestDecodeResponsesResponseDropsNamelessFunctionCall(t *testing.T) {
	raw := []byte(`{"output":[{"tYpe":"function_call"}]}`)
	resp, err := DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, b := range resp.Blocks {
		if b.Type == PartToolCall {
			t.Fatalf("RED: nameless function_call kept in IR: %+v", b.ToolCall)
		}
	}
	if resp.StopReason == StopToolUse {
		t.Fatalf("RED: nameless function_call drove stop_reason to tool_use")
	}
	enc, ok := encodeFuzzResponse(2, resp)
	if !ok {
		t.Fatal("encode declined")
	}
	resp2, ok := decodeFuzzResponse(2, enc)
	if !ok {
		t.Fatal("re-decode of encoder output failed")
	}
	if !responseEqualSansRaw(resp, resp2) {
		t.Fatalf("round-trip unstable after fix:\n%s\n%s", mustFuzzJSON(t, resp), mustFuzzJSON(t, resp2))
	}
}

// TestAnthropicStopSequenceRoundTrips is the minimized regression for the
// latent defect found while fixing #447: EncodeAnthropicResponse downgraded
// StopStopSequence to "max_tokens", so decode(encode(decode(x))) reported a
// truncation that never happened. The encoder now emits the true reason.
func TestAnthropicStopSequenceRoundTrips(t *testing.T) {
	raw := []byte(`{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"stop_sequence","usage":{"input_tokens":1,"output_tokens":2}}`)
	resp, err := DecodeAnthropicResponse(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != StopStopSequence {
		t.Fatalf("decode lost stop_sequence: %q", resp.StopReason)
	}
	out := EncodeAnthropicResponse(resp, "m")
	if out.StopReason == nil || *out.StopReason != StopStopSequence {
		got := "<nil>"
		if out.StopReason != nil {
			got = *out.StopReason
		}
		t.Fatalf("RED: encoder rewrote stop_sequence to %q", got)
	}
	body := mustJSON(out)
	resp2, err := DecodeAnthropicResponse(body)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !responseEqualSansRaw(resp, resp2) {
		t.Fatalf("round-trip unstable after fix: %q -> %q", resp.StopReason, resp2.StopReason)
	}
}

func FuzzResponseRoundTrip(f *testing.F) {
	f.Add(byte(0), []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`))
	f.Add(byte(1), []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	f.Add(byte(2), []byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	f.Add(byte(3), []byte(`{"eventType":"messageStop","payload":{"stopReason":"end_turn"}}`))
	// Minimized regression seed for #447: an unsigned thinking block
	// (case-variant keys) that the Anthropic decoder kept while the encoder
	// drops, breaking decode(encode(decode(x))) stability (thinking -> text).
	f.Add(byte(0), []byte(`{"Content":[{"tYpe":"thinking"}]}`))
	// Minimized regression seeds for the two further #447 instabilities
	// fixed on this branch: a nameless function_call the Responses decoder
	// kept while the encoder drops (codec 2), and a stop_sequence reason
	// the Anthropic encoder downgraded to max_tokens (codec 0).
	f.Add(byte(2), []byte(`{"output":[{"tYpe":"function_call"}]}`))
	f.Add(byte(0), []byte(`{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"stop_sequence","usage":{"input_tokens":1,"output_tokens":2}}`))
	// tool_use with an empty input object: the decoder must canonicalize to
	// "{}", matching what the encoder's `input,omitempty` converges to
	// (missing/null/{} shapes are indistinguishable on the wire).
	f.Add(byte(0), []byte(`{"content":[{"type":"tool_use","id":"x","name":"n","input":{}}]}`))
	// Responses function_call with a name but no call_id and no
	// arguments: the decoder must assign the encoder's synthesized ID
	// and canonical "{}" arguments, or the second decode drifts on both
	// (found by the fixed-seed CI fuzz window on main after #448).
	f.Add(byte(2), []byte(`{"output":[{"tYpe":"function_call","nAme":"\b"}]}`))
	f.Add(byte(2), []byte(`{"id":"r1","object":"response","status":"completed","model":"m","output":[{"type":"function_call","name":"f"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	f.Fuzz(func(t *testing.T, codecByte byte, raw []byte) {
		codec := int(codecByte) % 4
		resp, ok := decodeFuzzResponse(codec, raw)
		if !ok {
			return // malformed input: only the no-panic property applies
		}
		enc, ok := encodeFuzzResponse(codec, resp)
		if !ok {
			return // encoder declined; no-panic property already held
		}
		resp2, ok := decodeFuzzResponse(codec, enc)
		if !ok {
			t.Fatalf("codec %d: encode(decode(x)) is not decodable", codec)
		}
		if !responseEqualSansRaw(resp, resp2) {
			t.Fatalf("codec %d: round-trip unstable:\nfirst:  %s\nsecond: %s", codec,
				mustFuzzJSON(t, resp), mustFuzzJSON(t, resp2))
		}
		// Tier 2: the pair must be a fixed point — decode(encode(resp2))
		// is exactly resp2 (modulo Raw). Any oscillation or drift here
		// is a defect, with no normalization to hide behind.
		enc2, ok := encodeFuzzResponse(codec, resp2)
		if !ok {
			t.Fatalf("codec %d: encode(decode(encode(decode(x)))) failed", codec)
		}
		resp3, ok := decodeFuzzResponse(codec, enc2)
		if !ok {
			t.Fatalf("codec %d: second-generation encode output is not decodable", codec)
		}
		if !responseExactSansRaw(resp2, resp3) {
			t.Fatalf("codec %d: not a fixed point:\nsecond: %s\nthird:  %s", codec,
				mustFuzzJSON(t, resp2), mustFuzzJSON(t, resp3))
		}
	})
}

func decodeFuzzResponse(codec int, raw []byte) (Response, bool) {
	var resp Response
	var err error
	switch codec {
	case 0:
		resp, err = DecodeAnthropicResponse(raw)
	case 1:
		resp, err = DecodeOpenAIChatResponse(raw)
	case 2:
		resp, err = DecodeResponsesResponse(raw)
	case 3:
		resp, err = DecodeBedrockResponse(raw)
	}
	return resp, err == nil
}

func encodeFuzzResponse(codec int, resp Response) ([]byte, bool) {
	// The encoder targets an upstream deployment, so the model field is
	// routing, not content: round-trip with the decoded model to avoid a
	// harness artifact.
	model := resp.Model
	if model == "" {
		model = "m"
	}
	var v any
	switch codec {
	case 0:
		v = EncodeAnthropicResponse(resp, model)
	case 1:
		v = EncodeOpenAIChatResponse(resp, model)
	case 2:
		v = EncodeResponsesResponse(resp, model)
	case 3:
		return nil, false // no Bedrock response encoder exists; decode-only
	}
	body, err := json.Marshal(v)
	return body, err == nil
}

// responseEqualSansRaw compares canonical responses modulo the Raw
// passthrough field (which by design echoes the decoder's input bytes),
// the Model field (routing, not content: the encoder rewrites it for the
// upstream deployment), and the end_turn/stop_sequence distinction:
// OpenAI's finish_reason "stop" conflates natural end with stop sequences,
// and the forward mapping end_turn/stop_sequence -> "stop" is pinned (see
// TestAnthropicStopToOpenAIFinishMatrix), so the distinction cannot
// survive an OpenAI-vocabulary hop.
func responseEqualSansRaw(a, b Response) bool {
	a.Raw, b.Raw = nil, nil
	// Model is routing, not content: the encoder rewrites it for the
	// upstream deployment.
	a.Model, b.Model = "", ""
	// ID: the Responses encoder mints resp_<nanos> IDs for ID-less
	// responses (openai_responses.go). An absent ID may legitimately gain
	// a minted one on the hop; a present ID must survive verbatim.
	if a.ID == "" {
		b.ID = ""
	}
	if a.StopReason == StopStopSequence {
		a.StopReason = StopEndTurn
	}
	if b.StopReason == StopStopSequence {
		b.StopReason = StopEndTurn
	}
	// Drop null text blocks: the Anthropic encoder pads empty content
	// with {"type":"text","text":""} because the API requires a non-empty
	// content array, and the decoder faithfully keeps the padding. Null
	// text carries no content (Response.Text skips it), so only the exact
	// padding shape is tolerated here.
	a.Blocks = dropNullTextBlocks(a.Blocks)
	b.Blocks = dropNullTextBlocks(b.Blocks)
	// Tool-call IDs: the OpenAI-chat and Responses encoders mint call_<n>
	// IDs for ID-less tool calls because their APIs require an ID
	// (openai_chat.go, openai_responses.go). An absent tool-call ID may
	// legitimately gain a minted one on the hop; a present ID must survive
	// verbatim — mirroring the response-level ID rule above. Block types,
	// names, and arguments must still match exactly; only the minted ID is
	// folded, and only positionally (length mismatch still fails).
	if len(a.Blocks) == len(b.Blocks) {
		for i := range a.Blocks {
			if a.Blocks[i].Type == PartToolCall && a.Blocks[i].ToolCall != nil && a.Blocks[i].ToolCall.ID == "" &&
				b.Blocks[i].Type == PartToolCall && b.Blocks[i].ToolCall != nil {
				cp := *b.Blocks[i].ToolCall
				cp.ID = ""
				b.Blocks[i].ToolCall = &cp
			}
		}
	}
	aj, aerr := json.Marshal(a)
	bj, berr := json.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(aj, bj)
}

// dropNullTextBlocks removes text blocks with empty text and no other
// payload (the encoder's empty-content padding shape).
func dropNullTextBlocks(blocks []Block) []Block {
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == PartText && b.Text == "" && b.ToolCall == nil && b.Thinking == nil {
			continue
		}
		out = append(out, b)
	}
	return out
}

// isNullTextPart reports the encoder-padding shape for request parts:
// empty text with no other payload (cache markers, images, tool
// payloads are never padding).
func isNullTextPart(p Part) bool {
	return p.Type == PartText && p.Text == "" && p.CacheControl == "" &&
		p.Image == nil && p.ToolCall == nil && p.ToolResult == nil && p.Thinking == nil
}

// dropNullTextParts removes encoder-padding parts from a part list.
func dropNullTextParts(parts []Part) []Part {
	out := make([]Part, 0, len(parts))
	for _, p := range parts {
		if isNullTextPart(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// responseExactSansRaw compares canonical responses exactly, modulo the
// Raw passthrough field. Used for the Tier-2 fixed-point check, where no
// representation shift may remain.
func responseExactSansRaw(a, b Response) bool {
	a.Raw, b.Raw = nil, nil
	aj, aerr := json.Marshal(a)
	bj, berr := json.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(aj, bj)
}

func mustFuzzJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDecodeRequestsDropNamelessToolCalls is the minimized regression for
// the request-side twin of the #447 nameless-call asymmetry: all three
// request decoders kept nameless tool calls that their encoders drop, so
// the assistant message carrying one vanished on re-encode (CI failure on
// PR #448 head 66cc47e, run 38862152078, FuzzRequestRoundTrip seed
// 5d2d966a922a3918 — codec 2). Each decoder now drops exactly what its
// encoder cannot emit, mirroring the response-side drops (#403/#447).
func TestDecodeRequestsDropNamelessToolCalls(t *testing.T) {
	cases := []struct {
		name  string
		codec int
		raw   string
	}{
		{"responses", 2, `{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"","arguments":"{}"}]}`},
		{"chat", 1, `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{}}]}]}`},
		{"anthropic", 0, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"x","input":{}}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, ok := decodeFuzzRequest(tc.codec, []byte(tc.raw))
			if !ok {
				t.Fatal("decode failed")
			}
			for _, m := range req.Messages {
				for _, p := range m.Parts {
					if p.Type == PartToolCall {
						t.Fatalf("RED: nameless tool call kept in IR: %+v", p.ToolCall)
					}
				}
			}
			enc, ok := encodeFuzzRequest(tc.codec, req)
			if !ok {
				t.Fatal("encode declined")
			}
			req2, ok := decodeFuzzRequest(tc.codec, enc)
			if !ok {
				t.Fatal("re-decode failed")
			}
			a, _ := json.Marshal(normalizeRequest(tc.codec, req))
			b, _ := json.Marshal(normalizeRequest(tc.codec, req2))
			if string(a) != string(b) {
				t.Fatalf("request round-trip unstable:\nfirst:  %s\nsecond: %s", a, b)
			}
		})
	}
	// A named Responses function_call must survive verbatim.
	req, ok := decodeFuzzRequest(2, []byte(`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}]}`))
	if !ok {
		t.Fatal("named decode failed")
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 || req.Messages[0].Parts[0].ToolCall == nil || req.Messages[0].Parts[0].ToolCall.Name != "lookup" {
		t.Fatalf("named function_call lost or altered: %+v", req.Messages)
	}
}

// TestDecodeOpenAIChatRequestDropsSystemImage is the minimized regression
// for the defect FuzzRequestRoundTrip found on PR #451's head (CI run
// 38866096681, seed 858e6836af94573a): the chat decoder kept an image part
// in a system message, but system content is text-only in every request
// encoder here, so encode dropped it and the system prompt vanished on
// the hop. The decoder now drops system images at the boundary (unknown
// system part types were already dropped). System text and user images
// must survive.
func TestDecodeOpenAIChatRequestDropsSystemImage(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"0"}},{"type":"text","text":"be terse"}]},{"role":"user","content":[{"type":"image_url","image_url":{"url":"u1"}},{"type":"text","text":"hi"}]}]}`)
	req, ok := decodeFuzzRequest(1, raw)
	if !ok {
		t.Fatal("decode declined")
	}
	for _, p := range req.System {
		if p.Type == PartImage {
			t.Fatalf("RED: system image kept in IR: %+v", p.Image)
		}
	}
	if len(req.System) != 1 || req.System[0].Text != "be terse" {
		t.Fatalf("system text lost or altered: %+v", req.System)
	}
	userImages := 0
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Type == PartImage {
				userImages++
			}
		}
	}
	if userImages != 1 {
		t.Fatalf("user image must survive decode, got %d", userImages)
	}
	enc, ok := encodeFuzzRequest(1, req)
	if !ok {
		t.Fatal("encode declined")
	}
	req2, ok := decodeFuzzRequest(1, enc)
	if !ok {
		t.Fatal("re-decode of encoder output failed")
	}
	a, _ := json.Marshal(normalizeRequest(1, req))
	b, _ := json.Marshal(normalizeRequest(1, req2))
	if !bytes.Equal(a, b) {
		t.Fatalf("request round-trip unstable after fix:\n%s\n%s", a, b)
	}
}

// Regression (fuzz-smoke failure on PR #459 head 5620ffb, run
// 38884210003, seed 320a06b605cdba5b): an image_url part whose data: URL
// carries a media type but an empty payload decoded to
// Image{MediaType, Data:""}. Every request encoder requires a URL or
// non-empty Data, so the part silently vanished on encode and the
// round-trip was unstable. A payload-less data: image is not an image;
// the decoder must drop it at the boundary (mirroring the Anthropic
// decoder, which requires data != "" for base64 sources). A data: image
// WITH a payload, and an ordinary URL image, must survive.
func TestDecodeOpenAIImageDropsPayloadlessDataURL(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:0000000000000000;base64,"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}},{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`)
	req, ok := decodeFuzzRequest(1, raw)
	if !ok {
		t.Fatal("decode declined")
	}
	images := 0
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Type == PartImage {
				images++
				if p.Image == nil || (p.Image.URL == "" && p.Image.Data == "") {
					t.Fatalf("content-less image kept in IR: %+v", p.Image)
				}
			}
		}
	}
	if images != 2 {
		t.Fatalf("want exactly the 2 real images to survive decode, got %d", images)
	}
	enc, ok := encodeFuzzRequest(1, req)
	if !ok {
		t.Fatal("encode declined")
	}
	req2, ok := decodeFuzzRequest(1, enc)
	if !ok {
		t.Fatal("re-decode of encoder output failed")
	}
	a, _ := json.Marshal(normalizeRequest(1, req))
	b, _ := json.Marshal(normalizeRequest(1, req2))
	if !bytes.Equal(a, b) {
		t.Fatalf("request round-trip unstable:\n%s\n%s", a, b)
	}
}

func FuzzRequestRoundTrip(f *testing.F) {
	f.Add(byte(0), []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	f.Add(byte(1), []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	// Regression seed for the fuzz-smoke failure on PR #459 head 5620ffb
	// (run 38884210003, testdata seed 320a06b605cdba5b): an image_url part
	// whose data: URL carries a media type but an EMPTY payload decodes to
	// Image{MediaType, Data:""}, which no request encoder can emit (the
	// chat encoder requires Data != ""), so the part vanished on the hop.
	// The decoder must drop payload-less data: images at the boundary.
	f.Add(byte(1), []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:0000000000000000;base64,"}}]}]}`))
	// Minimized regression seed for the CI failure on PR #448 head
	// 66cc47e (testdata/fuzz/FuzzRequestRoundTrip/5d2d966a922a3918): a
	// nameless Responses function_call input item whose assistant
	// message vanished on re-encode.
	f.Add(byte(2), []byte(`{"model":"m","input":[{"type":"function_call","call_id":"c1","name":"","arguments":"{}"}]}`))
	// A system message carrying an image: system content is text-only in
	// every request encoder, so the chat decoder must drop the image at
	// the boundary or the system prompt vanishes on the hop (#447, CI
	// seed 858e6836af94573a).
	f.Add(byte(1), []byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"0"}}]},{"role":"user","content":"hi"}]}`))
	f.Add(byte(2), []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	f.Fuzz(func(t *testing.T, codecByte byte, raw []byte) {
		codec := int(codecByte) % 3
		req, ok := decodeFuzzRequest(codec, raw)
		if !ok {
			return
		}
		enc, ok := encodeFuzzRequest(codec, req)
		if !ok {
			return
		}
		req2, ok := decodeFuzzRequest(codec, enc)
		if !ok {
			t.Fatalf("codec %d: encode(decode(x)) is not decodable", codec)
		}
		// normalizeRequest folds the known-benign representation differences
		// before comparing: the model field is routing (the encoder
		// targets an upstream deployment), and the Responses codec pins
		// the system prompt as a system-role message item (see
		// TestResponsesRequestDecodeGolden) while the decoder also
		// accepts the dedicated instructions field. Both placements
		// carry the same text; anything else must compare exactly.
		a, aerr := json.Marshal(normalizeRequest(codec, req))
		b, berr := json.Marshal(normalizeRequest(codec, req2))
		if aerr != nil || berr != nil || !bytes.Equal(a, b) {
			t.Fatalf("codec %d: request round-trip unstable:\nfirst:  %s\nsecond: %s", codec, a, b)
		}
		// Tier 2: fixed point — decode(encode(req2)) is exactly req2.
		enc2, ok := encodeFuzzRequest(codec, req2)
		if !ok {
			t.Fatalf("codec %d: encode(decode(encode(decode(x)))) failed", codec)
		}
		req3, ok := decodeFuzzRequest(codec, enc2)
		if !ok {
			t.Fatalf("codec %d: second-generation encode output is not decodable", codec)
		}
		c, cerr := json.Marshal(req2)
		d, derr := json.Marshal(req3)
		if cerr != nil || derr != nil || !bytes.Equal(c, d) {
			t.Fatalf("codec %d: request codec not a fixed point:\nsecond: %s\nthird:  %s", codec, c, d)
		}
	})
}

func decodeFuzzRequest(codec int, raw []byte) (Request, bool) {
	switch codec {
	case 0:
		var in core.AnthropicRequest
		if err := json.Unmarshal(raw, &in); err != nil {
			return Request{}, false
		}
		req, err := DecodeAnthropicRequest(in, "m")
		return req, err == nil
	case 1:
		var in core.OpenAIRequest
		if err := json.Unmarshal(raw, &in); err != nil {
			return Request{}, false
		}
		req, err := DecodeOpenAIChatRequest(in, "m")
		return req, err == nil
	case 2:
		var in ResponsesRequest
		if err := json.Unmarshal(raw, &in); err != nil {
			return Request{}, false
		}
		req, err := DecodeResponsesRequest(in)
		return req, err == nil
	}
	return Request{}, false
}

func encodeFuzzRequest(codec int, req Request) ([]byte, bool) {
	switch codec {
	case 0:
		v, err := EncodeAnthropicRequest(req, fuzzModelOf(req))
		if err != nil {
			return nil, false
		}
		b, err := json.Marshal(v)
		return b, err == nil
	case 1:
		v, err := EncodeOpenAIChatRequest(req, fuzzModelOf(req), false)
		if err != nil {
			return nil, false
		}
		b, err := json.Marshal(v)
		return b, err == nil
	case 2:
		b, err := EncodeResponsesRequest(req, fuzzModelOf(req))
		return b, err == nil
	}
	return nil, false
}

// fuzzModelOf returns the request's own model for the round-trip encode so
// the comparison does not trip over the model field, which is routing
// (the encoder rewrites it for the upstream deployment), not content.
func fuzzModelOf(req Request) string {
	if req.Model == "" {
		return "m"
	}
	return req.Model
}

// normalizeRequest canonicalizes the known-benign representation
// differences so the round-trip comparison only fires on real loss:
//   - Model is routing, not content (see fuzzModelOf).
//   - A run of system-role messages is folded into System: the Responses
//     encoder pins the system prompt as a system-role message item (see
//     TestResponsesRequestDecodeGolden) while the decoder also accepts the
//     dedicated instructions field, and both placements carry identical
//     text.
//   - StopStopSequence and StopEndTurn compare equal (see
//     responseEqualSansRaw): OpenAI's finish_reason "stop" conflates
//     natural end with stop sequences, and the forward mapping
//     end_turn/stop_sequence -> "stop" is pinned (see
//     TestAnthropicStopToOpenAIFinishMatrix), so the distinction cannot
//     survive an OpenAI-vocabulary hop.
//   - MaxOutputTokens 0 and 4096 compare equal for the Anthropic codec:
//     Anthropic requires an explicit budget and the encoder defaults an
//     absent budget to 4096 (anthropic.go).
//   - ToolDef.Parameters absent, empty, or bare {"type":"object"} compare
//     equal for every request codec: the Anthropic, OpenAI-chat, and
//     Responses encoders all default a missing schema to
//     {"type":"object"} because their APIs require a schema object
//     (anthropic.go, openai_chat.go, openai_responses.go). Anything
//     richer must survive verbatim.
//   - Null text parts (empty text, no other payload) are dropped: the
//     Anthropic encoder pads empty content with {"type":"text","text":""}
//     because the API requires non-empty content, and the decoder keeps
//     the padding faithfully. Only the exact padding shape is tolerated.
func normalizeRequest(codec int, in Request) Request {
	out := in
	out.Model = ""
	if codec == 0 && out.MaxOutputTokens == 0 {
		out.MaxOutputTokens = 4096
	}
	// All three request encoders default an absent tool schema to
	// {"type":"object"} (API requirement), so the comparison must fold
	// that shift for every codec, not just Anthropic.
	tools := make([]ToolDef, len(out.Tools))
	copy(tools, out.Tools)
	for i := range tools {
		tools[i].Parameters = canonicalToolSchema(tools[i].Parameters)
	}
	out.Tools = tools
	sys := dropNullTextParts(out.System)
	var msgs []Message
	for _, m := range out.Messages {
		if m.Role == RoleSystem {
			sys = append(sys, dropNullTextParts(m.Parts)...)
			continue
		}
		m.Parts = dropNullTextParts(m.Parts)
		msgs = append(msgs, m)
	}
	out.System = sys
	out.Messages = msgs
	return out
}

// canonicalToolSchema normalizes a tool parameter schema for the
// round-trip comparison. The Anthropic encoder defaults an absent schema
// to {"type":"object"} because the API requires input_schema
// (anthropic.go); an explicitly empty or bare-object schema is therefore
// equivalent to absent. Anything richer must survive verbatim.
func canonicalToolSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if len(m) == 1 {
		if typ, _ := m["type"].(string); typ == "object" {
			return json.RawMessage(`{"type":"object"}`)
		}
	}
	if len(m) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	compact, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return compact
}

// TestDecodeResponsesRequestNormalizesEmptyArgs is the minimized regression
// for the defect found by FuzzRequestRoundTrip (#447): the Responses request
// decoder kept empty tool arguments verbatim while the request encoder
// normalizes empty/whitespace arguments to "{}", so
// decode(encode(decode(x))) drifted "" -> "{}". The decoder now keeps exactly
// what the encoder converges to, mirroring anthToolInputArgs (#447).
// TestDecodeResponsesResponseCanonicalizesEmptyArgs pins the response-side
// half of the empty-arguments normalization (implemented in #453): a named
// Responses function_call with no arguments must decode to Arguments "{}",
// the form EncodeResponsesResponse converges to, or the re-decode drifts
// "" -> "{}" (CI seed f3cf67c7e34a870b on main cff74cb4, run 38865064044).
// Non-empty arguments pass through verbatim.
func TestDecodeResponsesResponseCanonicalizesEmptyArgs(t *testing.T) {
	resp, err := DecodeResponsesResponse([]byte(`{"output":[{"tYpe":"function_call","nAme":"\b"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].ToolCall == nil {
		t.Fatalf("named function_call lost: %+v", resp.Blocks)
	}
	if got := resp.Blocks[0].ToolCall.Arguments; got != "{}" {
		t.Fatalf("RED: empty arguments not canonicalized, args=%q", got)
	}
	enc, ok := encodeFuzzResponse(2, resp)
	if !ok {
		t.Fatal("encode declined")
	}
	resp2, ok := decodeFuzzResponse(2, enc)
	if !ok {
		t.Fatal("re-decode of encoder output failed")
	}
	if !responseEqualSansRaw(resp, resp2) {
		t.Fatalf("round-trip unstable after fix:\n%s\n%s", mustFuzzJSON(t, resp), mustFuzzJSON(t, resp2))
	}
	named, err := DecodeResponsesResponse([]byte(`{"output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{\"a\":1}"}]}`))
	if err != nil {
		t.Fatalf("non-empty decode: %v", err)
	}
	if got := named.Blocks[0].ToolCall.Arguments; got != `{"a":1}` {
		t.Fatalf("non-empty arguments altered: args=%q", got)
	}
}

func TestDecodeResponsesRequestNormalizesEmptyArgs(t *testing.T) {
	for _, tc := range []struct{ args, wire string }{
		{"", `""`},
		{"   ", `"   "`},
		{"\t\n", `"\t\n"`},
	} {
		raw := []byte(`{"model":"m","input":[{"type":"function_call","name":"f","arguments":` + tc.wire + `}]}`)
		req, ok := decodeFuzzRequest(2, raw)
		if !ok {
			t.Fatalf("decode declined for args %q", tc.args)
		}
		got := req.Messages[0].Parts[0].ToolCall.Arguments
		if got != "{}" {
			t.Fatalf("RED: empty args kept as %q, want canonical \"{}\"", got)
		}
		enc, ok := encodeFuzzRequest(2, req)
		if !ok {
			t.Fatalf("encode declined for args %q", tc.args)
		}
		req2, ok := decodeFuzzRequest(2, enc)
		if !ok {
			t.Fatalf("re-decode failed for args %q", tc.args)
		}
		if req2.Messages[0].Parts[0].ToolCall.Arguments != "{}" {
			t.Fatalf("RED: request args unstable for %q", tc.args)
		}
	}
	// Non-empty arguments pass through verbatim.
	raw := []byte(`{"model":"m","input":[{"type":"function_call","name":"f","arguments":"{\"a\":1}"}]}`)
	req, ok := decodeFuzzRequest(2, raw)
	if !ok {
		t.Fatal("decode declined")
	}
	if got := req.Messages[0].Parts[0].ToolCall.Arguments; got != `{"a":1}` {
		t.Fatalf("non-empty args altered: %q", got)
	}
}

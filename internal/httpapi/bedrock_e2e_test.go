package httpapi

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Bedrock e2e fixture (M5-1b): the fake validates the SigV4 shape
// and the Converse addressing, answers chat with Converse JSON, and
// answers streaming with real AWS event stream frames (built below
// with the same CRC framing the adapter decodes).

type bedrockE2ECapture struct {
	mu            sync.Mutex
	path          string
	authorization string
	body          string
}

func (c *bedrockE2ECapture) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.EscapedPath()
	c.authorization = r.Header.Get("Authorization")
	c.body = string(b)
}

func bedrockFrame(eventType, payload string) []byte {
	var headers bytes.Buffer
	writeStr := func(name, value string) {
		headers.WriteByte(byte(len(name)))
		headers.WriteString(name)
		headers.WriteByte(7)
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(value)))
		headers.Write(lb[:])
		headers.WriteString(value)
	}
	writeStr(":message-type", "event")
	writeStr(":event-type", eventType)
	writeStr(":content-type", "application/json")
	total := 12 + headers.Len() + len(payload) + 4
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))
	frame := append([]byte{}, prelude[:]...)
	frame = append(frame, headers.Bytes()...)
	frame = append(frame, payload...)
	var crcb [4]byte
	binary.BigEndian.PutUint32(crcb[:], crc32.ChecksumIEEE(frame))
	return append(frame, crcb[:]...)
}

func bedrockGateway(t *testing.T, handler http.HandlerFunc) (*Server, *bedrockE2ECapture) {
	t.Helper()
	t.Setenv("NEXAROUTE_TEST_BR_SECRET", "e2e-secret")
	cap := &bedrockE2ECapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "br", Name: "Bedrock", Type: "bedrock", BaseURL: up.URL,
		APIKey: "AKIDE2E", SecretKeyEnv: "NEXAROUTE_TEST_BR_SECRET", AWSRegion: "us-west-2",
		Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "anthropic.claude-v1", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s, cap
}

func bedrockChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestBedrockEndToEndChatAndTools(t *testing.T) {
	s, cap := bedrockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"bedrock-ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":6,"outputTokens":3}}`)
	})
	rr := bedrockChat(t, s, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "bedrock-ok") {
		t.Fatalf("bedrock e2e status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cap.path != "/model/anthropic.claude-v1/converse" {
		t.Fatalf("upstream path=%q", cap.path)
	}
	if !strings.HasPrefix(cap.authorization, "AWS4-HMAC-SHA256 Credential=AKIDE2E/") ||
		!strings.Contains(cap.authorization, "/us-west-2/bedrock/aws4_request") {
		t.Fatalf("authorization=%q", cap.authorization)
	}
	if !strings.Contains(cap.body, `"toolConfig"`) || !strings.Contains(cap.body, `"toolSpec"`) ||
		!strings.Contains(cap.body, `"content":[{"text":"hi"}]`) {
		t.Fatalf("converse body=%s", cap.body)
	}
}

func TestBedrockEndToEndStreamingEventStream(t *testing.T) {
	s, cap := bedrockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		var stream bytes.Buffer
		stream.Write(bedrockFrame("messageStart", `{"role":"assistant"}`))
		stream.Write(bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"str"}}`))
		stream.Write(bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"eamed"}}`))
		stream.Write(bedrockFrame("contentBlockStop", `{"contentBlockIndex":0}`))
		stream.Write(bedrockFrame("messageStop", `{"stopReason":"end_turn"}`))
		stream.Write(bedrockFrame("metadata", `{"usage":{"inputTokens":4,"outputTokens":2}}`))
		_, _ = w.Write(stream.Bytes())
	})
	rr := bedrockChat(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("bedrock stream status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "str") || !strings.Contains(body, "eamed") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream body=%s", body)
	}
	if cap.path != "/model/anthropic.claude-v1/converse-stream" {
		t.Fatalf("upstream stream path=%q", cap.path)
	}
}

func TestBedrockFromAnthropicClient(t *testing.T) {
	s, _ := bedrockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"bedrock-anthropic-ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":6,"outputTokens":3}}`)
	})
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "bedrock-anthropic-ok") {
		t.Fatalf("anthropic->bedrock status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBedrockEndToEndStreamingToolUse(t *testing.T) {
	s, _ := bedrockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		var stream bytes.Buffer
		stream.Write(bedrockFrame("messageStart", `{"role":"assistant"}`))
		stream.Write(bedrockFrame("contentBlockStart", `{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"tu1","name":"lookup"}}}`))
		stream.Write(bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"q\":"}}}`))
		stream.Write(bedrockFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":"\"x\"}"}}}`))
		stream.Write(bedrockFrame("contentBlockStop", `{"contentBlockIndex":0}`))
		stream.Write(bedrockFrame("messageStop", `{"stopReason":"tool_use"}`))
		stream.Write(bedrockFrame("metadata", `{"usage":{"inputTokens":4,"outputTokens":2}}`))
		_, _ = w.Write(stream.Bytes())
	})
	rr := bedrockChat(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("tool stream status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"name":"lookup"`) || !strings.Contains(body, `\"q\"`) ||
		!strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("tool stream body=%s", body)
	}
}

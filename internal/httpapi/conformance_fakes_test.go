package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Provider conformance fakes (M5-2). Each fake speaks ONE upstream
// dialect faithfully: it validates the request shape for its
// dialect (400 on violations, so encoding regressions fail loudly),
// detects the scenario from the request content, and answers
// in-dialect:
//
//   - tool result present  -> final text "final-answer"
//   - tools declared       -> one tool call "lookup" {"q":"x"}
//   - streaming            -> "Hello " + "world" in dialect chunks
//   - otherwise            -> text "plain-answer" with usage
//
// The fake records whether it saw a tool result and whether the
// result content survived translation ("tool-output").

type conformanceFake struct {
	kind string // anthropic | openai_chat | gemini | openai_responses

	mu             sync.Mutex
	sawToolResult  bool
	toolResultText bool
	requests       int
	failWith       int // when >0, answer this HTTP status instead
}

func (f *conformanceFake) setFail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = status
}

func (f *conformanceFake) stats() (sawToolResult, toolResultText bool, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sawToolResult, f.toolResultText, f.requests
}

func (f *conformanceFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests++
	fail := f.failWith
	f.mu.Unlock()
	if fail > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fail)
		_, _ = io.WriteString(w, f.errorBody(fail))
		return
	}
	if !f.pathValid(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, f.errorBody(http.StatusNotFound))
		return
	}
	scenario, ok := f.inspect(body, r)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, f.errorBody(http.StatusBadRequest))
		return
	}
	switch scenario {
	case "tool_result":
		f.respondText(w, false, "final-answer")
	case "tool_call":
		f.respondToolCall(w)
	case "stream":
		f.respondStream(w)
	default:
		f.respondText(w, false, "plain-answer")
	}
}

// pathValid asserts the gateway addressed this dialect the way the
// real service expects (model-in-path for Gemini, fixed paths for
// the others).
func (f *conformanceFake) pathValid(r *http.Request) bool {
	switch f.kind {
	case "anthropic":
		return r.URL.Path == "/v1/messages"
	case "openai_chat":
		return r.URL.Path == "/v1/chat/completions"
	case "gemini":
		return strings.HasPrefix(r.URL.Path, "/v1beta/models/conf-model:generateContent") ||
			strings.HasPrefix(r.URL.Path, "/v1beta/models/conf-model:streamGenerateContent")
	default: // openai_responses
		return r.URL.Path == "/v1/responses"
	}
}

func (f *conformanceFake) errorBody(status int) string {
	switch f.kind {
	case "anthropic":
		return `{"type":"error","error":{"type":"api_error","message":"fake error"}}`
	case "gemini":
		return `{"error":{"code":` + strconv.Itoa(status) + `,"message":"fake error","status":"INTERNAL"}}`
	default:
		return `{"error":{"message":"fake error","type":"fake_error","code":"fake"}}`
	}
}

// inspect validates the dialect shape and classifies the scenario.
func (f *conformanceFake) inspect(body []byte, r *http.Request) (string, bool) {
	raw := string(body)
	switch f.kind {
	case "anthropic":
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools  []json.RawMessage `json:"tools"`
			Stream bool              `json:"stream"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
			return "", false
		}
		for _, m := range req.Messages {
			if m.Role != "user" && m.Role != "assistant" {
				return "", false
			}
		}
		if strings.Contains(raw, `"type":"tool_result"`) || strings.Contains(raw, `"type": "tool_result"`) {
			f.noteToolResult(strings.Contains(raw, "tool-output"))
			return "tool_result", true
		}
		if len(req.Tools) > 0 {
			return "tool_call", true
		}
		if req.Stream {
			return "stream", true
		}
		return "text", true
	case "openai_chat":
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools  []json.RawMessage `json:"tools"`
			Stream bool              `json:"stream"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
			return "", false
		}
		for _, m := range req.Messages {
			switch m.Role {
			case "system", "user", "assistant", "tool", "developer":
			default:
				return "", false
			}
		}
		if strings.Contains(raw, `"role":"tool"`) || strings.Contains(raw, `"role": "tool"`) {
			f.noteToolResult(strings.Contains(raw, "tool-output"))
			return "tool_result", true
		}
		if len(req.Tools) > 0 {
			return "tool_call", true
		}
		if req.Stream {
			return "stream", true
		}
		return "text", true
	case "gemini":
		var req struct {
			Contents []struct {
				Role string `json:"role"`
			} `json:"contents"`
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Contents) == 0 {
			return "", false
		}
		for _, c := range req.Contents {
			if c.Role != "user" && c.Role != "model" && c.Role != "" {
				return "", false
			}
		}
		if strings.Contains(raw, "functionResponse") {
			f.noteToolResult(strings.Contains(raw, "tool-output"))
			return "tool_result", true
		}
		if len(req.Tools) > 0 {
			return "tool_call", true
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			return "stream", true
		}
		return "text", true
	default: // openai_responses
		var req struct {
			Input  json.RawMessage   `json:"input"`
			Tools  []json.RawMessage `json:"tools"`
			Stream bool              `json:"stream"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Input) == 0 {
			return "", false
		}
		if strings.Contains(raw, "function_call_output") {
			f.noteToolResult(strings.Contains(raw, "tool-output"))
			return "tool_result", true
		}
		if len(req.Tools) > 0 {
			return "tool_call", true
		}
		if req.Stream {
			return "stream", true
		}
		return "text", true
	}
}

func (f *conformanceFake) noteToolResult(textSurvived bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sawToolResult = true
	f.toolResultText = textSurvived
}

func (f *conformanceFake) respondText(w http.ResponseWriter, _ bool, text string) {
	w.Header().Set("Content-Type", "application/json")
	switch f.kind {
	case "anthropic":
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"fake","content":[{"type":"text","text":"`+text+`"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":5}}`)
	case "gemini":
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"`+text+`"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4,"totalTokenCount":13}}`)
	case "openai_responses":
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"fake","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+text+`"}]}],"usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}}`)
	default:
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"`+text+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}
}

func (f *conformanceFake) respondToolCall(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	switch f.kind {
	case "anthropic":
		_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"fake","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":6}}`)
	case "gemini":
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4}}`)
	case "openai_responses":
		_, _ = io.WriteString(w, `{"id":"resp_2","object":"response","status":"completed","model":"fake","output":[{"type":"function_call","call_id":"t1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":8,"output_tokens":4}}`)
	default:
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":6,"total_tokens":16}}`)
	}
}

func (f *conformanceFake) respondStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	write := func(s string) {
		_, _ = io.WriteString(w, s)
		if fl != nil {
			fl.Flush()
		}
	}
	switch f.kind {
	case "anthropic":
		write("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fake\",\"content\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello \"}}\n\n")
		write("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"world\"}}\n\n")
		write("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		write("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case "gemini":
		write("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello \"}]}}]}\n\n")
		write("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"world\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":9,\"candidatesTokenCount\":4}}\n\n")
	case "openai_responses":
		write("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s\",\"status\":\"in_progress\"}}\n\n")
		write("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n")
		write("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\n")
		write("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":4}}}\n\n")
	default:
		write("data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"model\":\"fake\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"model\":\"fake\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"model\":\"fake\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		write("data: [DONE]\n\n")
	}
}

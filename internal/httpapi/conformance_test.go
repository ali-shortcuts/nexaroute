package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Provider conformance suite (M5-2): every client ingress × every
// upstream kind runs the same scenario battery against the dialect
// fakes in conformance_fakes_test.go — text, tool call, tool-result
// turn (with upstream-side fidelity assertions), streaming, and
// upstream-error propagation. Client-visible semantics are asserted
// in the CLIENT's dialect; request shapes are validated by the fake
// in the UPSTREAM's dialect. A red cell here is a conformance
// defect, not a test smell: fix the translation or document the
// limit in docs/COMPATIBILITY.md and pin it here.

type conformanceClient struct {
	name       string // anthropic | openai_chat | openai_responses
	path       string
	text       func(t *testing.T, s *Server) *httptest.ResponseRecorder
	tool       func(t *testing.T, s *Server) *httptest.ResponseRecorder
	result     func(t *testing.T, s *Server) *httptest.ResponseRecorder
	stream     func(t *testing.T, s *Server) *httptest.ResponseRecorder
	extract    func(t *testing.T, body string) (text string, toolName string)
	streamText func(t *testing.T, body string) (assembled string, terminal bool)
}

func conformancePost(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gateway"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func conformanceClients() []conformanceClient {
	return []conformanceClient{
		{
			name: "anthropic",
			path: "/v1/messages",
			text: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/messages", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
			},
			tool: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/messages", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
			},
			result: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/messages", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"tool-output"}]}],"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object"}}]}`)
			},
			stream: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/messages", `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			},
			extract: func(t *testing.T, body string) (string, string) {
				var resp struct {
					Content []struct {
						Type  string          `json:"type"`
						Text  string          `json:"text"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
					} `json:"content"`
				}
				if err := json.Unmarshal([]byte(body), &resp); err != nil {
					t.Fatalf("anthropic client response not anthropic-shaped: %v (%s)", err, body)
				}
				var text, tool string
				for _, c := range resp.Content {
					if c.Type == "text" {
						text += c.Text
					}
					if c.Type == "tool_use" {
						tool = c.Name
						if !strings.Contains(string(c.Input), `"q"`) {
							t.Fatalf("tool input lost: %s", c.Input)
						}
					}
				}
				return text, tool
			},
			streamText: func(t *testing.T, body string) (string, bool) {
				var sb strings.Builder
				for _, line := range strings.Split(body, "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var ev struct {
						Type  string `json:"type"`
						Delta struct {
							Text string `json:"text"`
						} `json:"delta"`
					}
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil && ev.Type == "content_block_delta" {
						sb.WriteString(ev.Delta.Text)
					}
				}
				return sb.String(), strings.Contains(body, "message_stop")
			},
		},
		{
			name: "openai_chat",
			path: "/v1/chat/completions",
			text: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			},
			tool: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
			},
			result: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},{"role":"tool","tool_call_id":"t1","content":"tool-output"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
			},
			stream: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			},
			extract: func(t *testing.T, body string) (string, string) {
				var resp struct {
					Choices []struct {
						Message struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.Choices) == 0 {
					t.Fatalf("openai client response not openai-shaped: %v (%s)", err, body)
				}
				m := resp.Choices[0].Message
				tool := ""
				if len(m.ToolCalls) > 0 {
					tool = m.ToolCalls[0].Function.Name
					if !strings.Contains(m.ToolCalls[0].Function.Arguments, `"q"`) {
						t.Fatalf("tool arguments lost: %s", m.ToolCalls[0].Function.Arguments)
					}
				}
				return m.Content, tool
			},
			streamText: func(t *testing.T, body string) (string, bool) {
				var sb strings.Builder
				for _, line := range strings.Split(body, "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					data := strings.TrimPrefix(line, "data: ")
					if data == "[DONE]" {
						continue
					}
					var chunk struct {
						Choices []struct {
							Delta struct {
								Content string `json:"content"`
							} `json:"delta"`
						} `json:"choices"`
					}
					if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
						sb.WriteString(chunk.Choices[0].Delta.Content)
					}
				}
				return sb.String(), strings.Contains(body, "[DONE]")
			},
		},
		{
			name: "openai_responses",
			path: "/v1/responses",
			text: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/responses", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
			},
			tool: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/responses", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
			},
			result: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/responses", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"t1","name":"lookup","arguments":"{\"q\":\"x\"}"},{"type":"function_call_output","call_id":"t1","output":"tool-output"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
			},
			stream: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return conformancePost(t, s, "/v1/responses", `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
			},
			extract: func(t *testing.T, body string) (string, string) {
				var resp struct {
					Output []struct {
						Type    string `json:"type"`
						Name    string `json:"name"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"output"`
				}
				if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.Output) == 0 {
					t.Fatalf("responses client response not responses-shaped: %v (%s)", err, body)
				}
				var text, tool string
				for _, item := range resp.Output {
					if item.Type == "function_call" {
						tool = item.Name
					}
					for _, c := range item.Content {
						text += c.Text
					}
				}
				return text, tool
			},
			streamText: func(t *testing.T, body string) (string, bool) {
				var sb strings.Builder
				for _, line := range strings.Split(body, "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var ev struct {
						Type  string `json:"type"`
						Delta string `json:"delta"`
					}
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil && ev.Type == "response.output_text.delta" {
						sb.WriteString(ev.Delta)
					}
				}
				return sb.String(), strings.Contains(body, "response.completed")
			},
		},
	}
}

var conformanceUpstreams = []struct {
	kind         string // canonical kind (fake dialect)
	providerType string
}{
	{"anthropic", "anthropic_compatible"},
	{"openai_chat", "openai_compatible"},
	{"gemini", "gemini"},
	{"openai_responses", "openai_responses"},
}

func conformanceGateway(t *testing.T, providerType string, fake *conformanceFake) *Server {
	t.Helper()
	up := httptest.NewServer(fake)
	t.Cleanup(up.Close)
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "cf", Name: "Conformance", Type: providerType, BaseURL: up.URL,
		APIKey: "k", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true}}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	return s
}

func TestProviderConformanceMatrix(t *testing.T) {
	for _, up := range conformanceUpstreams {
		for _, client := range conformanceClients() {
			t.Run(up.kind+"_upstream/"+client.name+"_client", func(t *testing.T) {
				fake := &conformanceFake{kind: up.kind}
				s := conformanceGateway(t, up.providerType, fake)

				// S1: plain text round trip.
				rr := client.text(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S1 text: status=%d body=%s", rr.Code, rr.Body.String())
				}
				if text, _ := client.extract(t, rr.Body.String()); text != "plain-answer" {
					t.Fatalf("S1 text: got %q", text)
				}

				// S2: tool call surfaces in the client's dialect.
				rr = client.tool(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S2 tool: status=%d body=%s", rr.Code, rr.Body.String())
				}
				if _, tool := client.extract(t, rr.Body.String()); tool != "lookup" {
					t.Fatalf("S2 tool: got tool=%q body=%s", tool, rr.Body.String())
				}

				// S3: tool-result turn — the upstream must SEE the
				// result in its own dialect, content intact, and the
				// client must receive the final answer.
				rr = client.result(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S3 result: status=%d body=%s", rr.Code, rr.Body.String())
				}
				if text, _ := client.extract(t, rr.Body.String()); text != "final-answer" {
					t.Fatalf("S3 result: got %q", text)
				}
				saw, survived, _ := fake.stats()
				if !saw {
					t.Fatal("S3 result: upstream never saw a tool result in its dialect")
				}
				if !survived {
					t.Fatal("S3 result: tool result content 'tool-output' lost in translation")
				}

				// S4: streaming text assembles and terminates in the
				// client's dialect.
				rr = client.stream(t, s)
				if rr.Code != http.StatusOK {
					t.Fatalf("S4 stream: status=%d body=%s", rr.Code, rr.Body.String())
				}
				assembled, terminal := client.streamText(t, rr.Body.String())
				if assembled != "Hello world" {
					t.Fatalf("S4 stream: assembled %q body=%s", assembled, rr.Body.String())
				}
				if !terminal {
					t.Fatalf("S4 stream: no terminal marker in client dialect: %s", rr.Body.String())
				}

				// S5: an upstream 429 reaches the client as an error
				// in the client's dialect envelope, never as a 200.
				fake.setFail(http.StatusTooManyRequests)
				rr = client.text(t, s)
				if rr.Code == http.StatusOK {
					t.Fatalf("S5 error: upstream 429 became a client 200: %s", rr.Body.String())
				}
				if !strings.Contains(rr.Body.String(), "error") {
					t.Fatalf("S5 error: body lacks an error envelope: status=%d body=%s", rr.Code, rr.Body.String())
				}
			})
		}
	}
}

func conformanceCfg(baseURL, providerType string) config.Config {
	cfg := config.Default()
	p := config.ProviderConfig{
		ID: "cf", Name: "Conformance", Type: providerType, BaseURL: baseURL,
		APIKey: "k", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "conf-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: true}}},
	}
	p.ApplyDefaults()
	cfg.Providers = []config.ProviderConfig{p}
	return cfg
}

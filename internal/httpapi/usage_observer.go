package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/router"
	usageacct "github.com/ali-shortcuts/nexaroute/internal/usage"
)

const maxUsageJSONBytes = maxUpstreamJSONBytes

type usageObserverBody struct {
	io.ReadCloser
	once       sync.Once
	manager    *usageacct.Manager
	deployment router.Deployment
	protocol   string
	stream     bool
	sawEOF     bool
	jsonBuf    []byte
	jsonTooBig bool
	sse        *usageSSETracker
}

func (b *usageObserverBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.stream {
			_ = b.sse.consume(p[:n])
		} else if !b.jsonTooBig {
			if len(b.jsonBuf)+n <= maxUsageJSONBytes {
				b.jsonBuf = append(b.jsonBuf, p[:n]...)
			} else {
				b.jsonTooBig = true
				b.jsonBuf = nil
			}
		}
	}
	if err == io.EOF {
		b.sawEOF = true
		b.finish()
	}
	return n, err
}

func (b *usageObserverBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *usageObserverBody) finish() {
	b.once.Do(func() {
		var sample usageacct.Sample
		var ok bool
		if b.stream {
			if b.sse != nil {
				_ = b.sse.finish()
				sample, ok = b.sse.sample, b.sse.usageSeen && b.sse.terminal
			}
		} else if b.sawEOF && !b.jsonTooBig {
			sample, ok = parseNonStreamUsage(b.protocol, b.jsonBuf)
		}
		if ok {
			b.manager.Record(b.deployment.ID, b.deployment.ProviderID, sample, deploymentUsagePricing(b.deployment))
			return
		}
		b.manager.RecordUnknown(b.deployment.ID, b.deployment.ProviderID)
	})
}

func deploymentUsagePricing(d router.Deployment) *usageacct.Pricing {
	if d.Pricing == nil {
		return nil
	}
	return &usageacct.Pricing{
		InputUSDPerMillion:  d.Pricing.InputUSDPerMillion,
		OutputUSDPerMillion: d.Pricing.OutputUSDPerMillion,
	}
}

func (s *Server) observeUsageBody(rc io.ReadCloser, d router.Deployment, stream bool) io.ReadCloser {
	if s == nil || s.usage == nil || rc == nil {
		return rc
	}
	protocol := ""
	switch d.ProviderType {
	case "openai_compatible":
		protocol = "openai"
	case "anthropic_compatible":
		protocol = "anthropic"
	default:
		return rc
	}
	b := &usageObserverBody{
		ReadCloser:  rc,
		manager:     s.usage,
		deployment:  d,
		protocol:    protocol,
		stream:      stream,
		jsonBuf:     make([]byte, 0, 4096),
	}
	if stream {
		b.sse = &usageSSETracker{protocol: protocol}
	}
	return b
}

func parseNonStreamUsage(protocol string, body []byte) (usageacct.Sample, bool) {
	switch protocol {
	case "openai":
		var env struct {
			Usage *struct {
				PromptTokens            *int64 `json:"prompt_tokens"`
				CompletionTokens        *int64 `json:"completion_tokens"`
				PromptTokensDetails     *struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails *struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &env) != nil || env.Usage == nil || env.Usage.PromptTokens == nil || env.Usage.CompletionTokens == nil {
			return usageacct.Sample{}, false
		}
		s := usageacct.Sample{InputTokens: *env.Usage.PromptTokens, OutputTokens: *env.Usage.CompletionTokens}
		if env.Usage.PromptTokensDetails != nil {
			s.CacheReadInputTokens = env.Usage.PromptTokensDetails.CachedTokens
		}
		if env.Usage.CompletionTokensDetails != nil {
			s.ReasoningTokens = env.Usage.CompletionTokensDetails.ReasoningTokens
		}
		return s, s.Valid()
	case "anthropic":
		var env struct {
			Usage *struct {
				InputTokens              *int64 `json:"input_tokens"`
				OutputTokens             *int64 `json:"output_tokens"`
				CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &env) != nil || env.Usage == nil || env.Usage.InputTokens == nil || env.Usage.OutputTokens == nil {
			return usageacct.Sample{}, false
		}
		s := usageacct.Sample{
			InputTokens:  *env.Usage.InputTokens,
			OutputTokens: *env.Usage.OutputTokens,
		}
		if env.Usage.CacheReadInputTokens != nil {
			s.CacheReadInputTokens = *env.Usage.CacheReadInputTokens
		}
		if env.Usage.CacheCreationInputTokens != nil {
			s.CacheCreationInputTokens = *env.Usage.CacheCreationInputTokens
		}
		return s, s.Valid()
	default:
		return usageacct.Sample{}, false
	}
}

type usageSSETracker struct {
	protocol  string
	line      []byte
	event     string
	data      strings.Builder
	hasAny    bool
	sample    usageacct.Sample
	usageSeen bool
	terminal  bool
}

func (t *usageSSETracker) consume(p []byte) error {
	for len(p) > 0 {
		n := bytes.IndexByte(p, '\n')
		if n < 0 {
			if len(t.line)+len(p) > maxNativeSSELineBytes {
				return io.ErrShortBuffer
			}
			t.line = append(t.line, p...)
			return nil
		}
		if len(t.line)+n > maxNativeSSELineBytes {
			return io.ErrShortBuffer
		}
		t.line = append(t.line, p[:n]...)
		if err := t.processLine(); err != nil {
			return err
		}
		t.line = t.line[:0]
		p = p[n+1:]
	}
	return nil
}

func (t *usageSSETracker) processLine() error {
	line := strings.TrimRight(string(t.line), "\r")
	if line == "" {
		return t.processFrame()
	}
	if strings.HasPrefix(line, ":") {
		return nil
	}
	field, value, found := strings.Cut(line, ":")
	if !found {
		field, value = line, ""
	}
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		t.event = value
		t.hasAny = true
	case "data":
		if t.data.Len() > 0 {
			t.data.WriteByte('\n')
		}
		t.data.WriteString(value)
		t.hasAny = true
	}
	return nil
}

func (t *usageSSETracker) processFrame() error {
	if !t.hasAny {
		return nil
	}
	data := strings.TrimSpace(t.data.String())
	event := t.event
	t.data.Reset()
	t.event = ""
	t.hasAny = false
	if data == "" {
		return nil
	}
	if t.protocol == "openai" && data == "[DONE]" {
		t.terminal = true
		return nil
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return nil
	}
	switch t.protocol {
	case "openai":
		if raw := env["usage"]; len(raw) > 0 && string(raw) != "null" {
			var u struct {
				PromptTokens            *int64 `json:"prompt_tokens"`
				CompletionTokens        *int64 `json:"completion_tokens"`
				PromptTokensDetails     *struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails *struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			}
			if json.Unmarshal(raw, &u) == nil && u.PromptTokens != nil && u.CompletionTokens != nil {
				t.sample.InputTokens = *u.PromptTokens
				t.sample.OutputTokens = *u.CompletionTokens
				if u.PromptTokensDetails != nil {
					t.sample.CacheReadInputTokens = u.PromptTokensDetails.CachedTokens
				}
				if u.CompletionTokensDetails != nil {
					t.sample.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
				}
				t.usageSeen = t.sample.Valid()
			}
		}
		var choices []struct {
			FinishReason *string `json:"finish_reason"`
		}
		if raw := env["choices"]; len(raw) > 0 && json.Unmarshal(raw, &choices) == nil {
			for _, c := range choices {
				if c.FinishReason != nil && *c.FinishReason != "" {
					t.terminal = true
				}
			}
		}
	case "anthropic":
		var typ string
		_ = json.Unmarshal(env["type"], &typ)
		if typ == "" {
			typ = event
		}
		switch typ {
		case "message_start":
			var msg struct {
				Usage *struct {
					InputTokens              *int64 `json:"input_tokens"`
					OutputTokens             *int64 `json:"output_tokens"`
					CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
				} `json:"usage"`
			}
			if raw := env["message"]; len(raw) > 0 && json.Unmarshal(raw, &msg) == nil && msg.Usage != nil {
				if mergeAnthropicUsage(&t.sample, msg.Usage.InputTokens, msg.Usage.OutputTokens, msg.Usage.CacheReadInputTokens, msg.Usage.CacheCreationInputTokens) {
					t.usageSeen = t.sample.Valid()
				}
			}
		case "message_delta":
			var u struct {
				InputTokens              *int64 `json:"input_tokens"`
				OutputTokens             *int64 `json:"output_tokens"`
				CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
			}
			if raw := env["usage"]; len(raw) > 0 && json.Unmarshal(raw, &u) == nil {
				if mergeAnthropicUsage(&t.sample, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens) {
					t.usageSeen = t.sample.Valid()
				}
			}
			var delta struct {
				StopReason *string `json:"stop_reason"`
			}
			if raw := env["delta"]; len(raw) > 0 && json.Unmarshal(raw, &delta) == nil && delta.StopReason != nil && *delta.StopReason != "" {
				t.terminal = true
			}
		case "message_stop":
			t.terminal = true
		}
	}
	return nil
}

func mergeAnthropicUsage(s *usageacct.Sample, input, output, cacheRead, cacheCreate *int64) bool {
	seen := input != nil || output != nil || cacheRead != nil || cacheCreate != nil
	if input != nil {
		s.InputTokens = *input
	}
	if output != nil {
		s.OutputTokens = *output
	}
	if cacheRead != nil {
		s.CacheReadInputTokens = *cacheRead
	}
	if cacheCreate != nil {
		s.CacheCreationInputTokens = *cacheCreate
	}
	return seen
}

func (t *usageSSETracker) finish() error {
	if len(t.line) > 0 {
		if err := t.processLine(); err != nil {
			return err
		}
		t.line = nil
	}
	if t.hasAny {
		if err := t.processFrame(); err != nil {
			return err
		}
	}
	return nil
}

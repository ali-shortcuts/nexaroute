package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Tool-definition integrity (M3-4C). Where earlier tests only searched
// the upstream request for a few substrings, these tests capture the
// actual upstream JSON, decode it, and compare every tool definition
// semantically: name, description, and the complete parameter schema
// (nested objects, required lists, enums, array item schemas, numeric
// bounds, patterns, formats, additionalProperties) for EVERY tool the
// client sent — across the canonical ingresses, the native provider
// paths, and the canonical repair retry.

const weatherSchema = `{
	"type": "object",
	"properties": {
		"city": {"type": "string", "description": "City name"},
		"units": {"type": "string", "enum": ["celsius", "fahrenheit"], "default": "celsius"},
		"days": {"type": "integer", "minimum": 1, "maximum": 14},
		"alerts": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"kind": {"type": "string", "enum": ["rain", "snow", "heat"]},
					"severity": {"type": "number"}
				},
				"required": ["kind"]
			}
		},
		"options": {
			"type": "object",
			"properties": {
				"hourly": {"type": "boolean"},
				"language": {"type": "string", "pattern": "^[a-z]{2}$"}
			},
			"additionalProperties": false
		}
	},
	"required": ["city"]
}`

const eventSchema = `{
	"type": "object",
	"properties": {
		"title": {"type": "string", "minLength": 1},
		"start": {"type": "string", "format": "date-time"},
		"priority": {"type": "string", "enum": ["low", "normal", "high"]},
		"attendees": {"type": "array", "items": {"type": "string", "format": "email"}, "minItems": 1},
		"location": {
			"type": "object",
			"properties": {
				"name": {"type": "string"},
				"geo": {
					"type": "object",
					"properties": {
						"lat": {"type": "number", "minimum": -90, "maximum": 90},
						"lng": {"type": "number", "minimum": -180, "maximum": 180}
					},
					"required": ["lat", "lng"]
				}
			},
			"required": ["name"]
		}
	},
	"required": ["title", "start"]
}`

type integrityTool struct {
	Name        string
	Description string
	SchemaJSON  string
}

var integrityTools = []integrityTool{
	{Name: "get_weather", Description: "Get the current weather for a city", SchemaJSON: weatherSchema},
	{Name: "create_event", Description: "Create a calendar event", SchemaJSON: eventSchema},
}

func decodeSchema(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode expected schema: %v", err)
	}
	return v
}

// assertToolsMatch compares the tools array of a decoded upstream
// request against the full client intent, in the provider's wire
// shape ("chat": function.parameters, "anthropic": input_schema,
// "responses": parameters). Comparison is semantic (decoded JSON),
// keyed by tool name so ordering differences do not matter; every
// tool must be present with its exact description and schema.
func assertToolsMatch(t *testing.T, body map[string]any, shape string) {
	t.Helper()
	rawTools, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("upstream request has no tools array: %v", body)
	}
	if len(rawTools) != len(integrityTools) {
		t.Fatalf("upstream tools count=%d want %d: %v", len(rawTools), len(integrityTools), rawTools)
	}
	byName := map[string]map[string]any{}
	for _, rt := range rawTools {
		tool, ok := rt.(map[string]any)
		if !ok {
			t.Fatalf("tool entry is not an object: %v", rt)
		}
		var name string
		var view map[string]any
		switch shape {
		case "chat":
			fn, ok := tool["function"].(map[string]any)
			if !ok {
				t.Fatalf("chat tool missing function object: %v", tool)
			}
			name, _ = fn["name"].(string)
			view = map[string]any{"description": fn["description"], "schema": fn["parameters"]}
		case "anthropic":
			name, _ = tool["name"].(string)
			view = map[string]any{"description": tool["description"], "schema": tool["input_schema"]}
		case "responses":
			name, _ = tool["name"].(string)
			view = map[string]any{"description": tool["description"], "schema": tool["parameters"]}
		default:
			t.Fatalf("unknown shape %q", shape)
		}
		if name == "" {
			t.Fatalf("tool without a name in %s shape: %v", shape, tool)
		}
		byName[name] = view
	}
	for _, want := range integrityTools {
		got, ok := byName[want.Name]
		if !ok {
			t.Fatalf("tool %q missing upstream (have %v)", want.Name, byName)
		}
		if got["description"] != want.Description {
			t.Fatalf("tool %q description = %v, want %q", want.Name, got["description"], want.Description)
		}
		wantSchema := decodeSchema(t, want.SchemaJSON)
		if !reflect.DeepEqual(got["schema"], wantSchema) {
			gotJSON, _ := json.Marshal(got["schema"])
			t.Fatalf("tool %q schema mutated in transit:\n got: %s\nwant: %s", want.Name, gotJSON, want.SchemaJSON)
		}
	}
}

// schemaCapture is an upstream that records decoded request bodies
// and replies with a fixed valid response for its protocol.
type schemaCapture struct {
	mu     sync.Mutex
	bodies []map[string]any
	reply  string
}

func (c *schemaCapture) handler(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		var decoded map[string]any
		if json.Unmarshal(raw, &decoded) == nil {
			c.mu.Lock()
			c.bodies = append(c.bodies, decoded)
			c.mu.Unlock()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, c.reply)
}

func (c *schemaCapture) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.bodies...)
}

func chatToolsJSON() string {
	var parts []string
	for _, tool := range integrityTools {
		parts = append(parts, fmt.Sprintf(`{"type":"function","function":{"name":%q,"description":%q,"parameters":%s}}`, tool.Name, tool.Description, tool.SchemaJSON))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func anthropicToolsJSON() string {
	var parts []string
	for _, tool := range integrityTools {
		parts = append(parts, fmt.Sprintf(`{"name":%q,"description":%q,"input_schema":%s}`, tool.Name, tool.Description, tool.SchemaJSON))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func responsesToolsJSON() string {
	var parts []string
	for _, tool := range integrityTools {
		parts = append(parts, fmt.Sprintf(`{"type":"function","name":%q,"description":%q,"parameters":%s}`, tool.Name, tool.Description, tool.SchemaJSON))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Canonical ingresses: OpenAI Chat, Anthropic Messages and OpenAI
// Responses clients all reach an openai_responses provider; the full
// client tool definitions must arrive semantically intact.
func TestToolSchemaIntegrityCanonicalIngresses(t *testing.T) {
	capture := &schemaCapture{reply: `{"id":"resp_1","model":"upstream","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`}
	upstream := httptest.NewServer(http.HandlerFunc(capture.handler))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "upstream", Name: "Canonical upstream", Type: "openai_responses", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Tools: true, Streaming: true},
		}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	cases := []struct {
		name string
		path string
		body string
	}{
		{"OpenAI Chat", "/v1/chat/completions", fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"plan"}],"tools":%s}`, chatToolsJSON())},
		{"Anthropic Messages", "/v1/messages", fmt.Sprintf(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"plan"}],"tools":%s}`, anthropicToolsJSON())},
		{"OpenAI Responses", "/v1/responses", fmt.Sprintf(`{"model":"m","input":"plan","tools":%s}`, responsesToolsJSON())},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			before := len(capture.all())
			req := httptest.NewRequest(http.MethodPost, "http://gateway"+tc.path, strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			bodies := capture.all()
			if len(bodies) != before+1 {
				t.Fatalf("upstream calls=%d want exactly one new request", len(bodies)-before)
			}
			assertToolsMatch(t, bodies[len(bodies)-1], "responses")
		})
	}
}

// Native provider paths (the route the classifier audit test uses):
// chat clients to an openai_compatible provider and Anthropic clients
// to an anthropic provider must also receive the complete schemas.
func TestToolSchemaIntegrityNativePaths(t *testing.T) {
	t.Run("chat to openai_compatible", func(t *testing.T) {
		capture := &schemaCapture{reply: `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`}
		upstream := httptest.NewServer(http.HandlerFunc(capture.handler))
		defer upstream.Close()
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Providers = []config.ProviderConfig{{
			ID: "p", Name: "P", Type: "openai_compatible", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream", Aliases: []string{"client"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}},
		}}
		s := testGateway(t, cfg)
		body := fmt.Sprintf(`{"model":"client","messages":[{"role":"user","content":"plan"}],"tools":%s}`, chatToolsJSON())
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		bodies := capture.all()
		if len(bodies) != 1 {
			t.Fatalf("upstream calls=%d", len(bodies))
		}
		assertToolsMatch(t, bodies[0], "chat")
	})

	t.Run("anthropic to anthropic", func(t *testing.T) {
		capture := &schemaCapture{reply: `{"id":"msg_1","type":"message","role":"assistant","model":"upstream","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`}
		upstream := httptest.NewServer(http.HandlerFunc(capture.handler))
		defer upstream.Close()
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Providers = []config.ProviderConfig{{
			ID: "p", Name: "P", Type: "anthropic_compatible", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{ID: "m", Model: "upstream", Aliases: []string{"client"}, Enabled: true, Weight: 1, Capabilities: config.Capabilities{Tools: true, Streaming: true}}},
		}}
		s := testGateway(t, cfg)
		body := fmt.Sprintf(`{"model":"client","max_tokens":64,"messages":[{"role":"user","content":"plan"}],"tools":%s}`, anthropicToolsJSON())
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/messages", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		bodies := capture.all()
		if len(bodies) != 1 {
			t.Fatalf("upstream calls=%d", len(bodies))
		}
		assertToolsMatch(t, bodies[0], "anthropic")
	})
}

// Repair retry: the first attempt is rejected for an unsupported
// field (temperature); the retry must drop exactly that field and
// carry the complete, unmutated tool definitions.
func TestToolSchemaIntegrityRepairRetry(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		bodies = append(bodies, decoded)
		calls := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"temperature is not supported by this model","type":"invalid_request_error"}}`)
			return
		}
		io.WriteString(w, `{"id":"resp_1","model":"upstream","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "upstream", Name: "Canonical upstream", Type: "openai_responses", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{
			ID: "m", Model: "upstream-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Tools: true, Streaming: true},
		}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()

	body := fmt.Sprintf(`{"model":"m","input":"plan","temperature":0.4,"tools":%s}`, responsesToolsJSON())
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream calls=%d want initial attempt + one repair retry", len(bodies))
	}
	if _, ok := bodies[0]["temperature"]; !ok {
		t.Fatalf("initial attempt lost temperature (repair premise broken): %v", bodies[0])
	}
	assertToolsMatch(t, bodies[0], "responses")
	if _, ok := bodies[1]["temperature"]; ok {
		t.Fatalf("repair retry retained the unsupported field: %v", bodies[1])
	}
	assertToolsMatch(t, bodies[1], "responses")
	if _, ok := bodies[1]["input"]; !ok {
		t.Fatalf("repair retry lost the request input: %v", bodies[1])
	}
	// Strongest form of the claim: the retry body is the first
	// attempt's body with ONLY temperature removed — every other
	// field semantically identical. The expected representation is an
	// independent deep copy (JSON round-trip), so the captured
	// first-attempt map is never mutated.
	rawCopy, err := json.Marshal(bodies[0])
	if err != nil {
		t.Fatalf("marshal first attempt copy: %v", err)
	}
	var expected map[string]any
	if err := json.Unmarshal(rawCopy, &expected); err != nil {
		t.Fatalf("unmarshal first attempt copy: %v", err)
	}
	delete(expected, "temperature")
	if !reflect.DeepEqual(expected, bodies[1]) {
		expectedJSON, _ := json.Marshal(expected)
		actualJSON, _ := json.Marshal(bodies[1])
		t.Fatalf("repair retry changed more than the unsupported field:\nexpected (attempt 1 minus temperature): %s\nactual retry body: %s", expectedJSON, actualJSON)
	}
}

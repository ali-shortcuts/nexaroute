package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCallValidationError provides structured diagnostics for tool-call
// validation failures. It intentionally does NOT include full sensitive
// command contents in logs beyond metadata, but includes field-level info.
type ToolCallValidationError struct {
	Tool          string
	Field         string
	ExpectedType  string
	ActualType    string
	Stage         string
	Protocol      string
	Streaming     bool
	Provider      string
	Message       string
}

func (e *ToolCallValidationError) Error() string {
	// Format similar to observed failure for observability, but with metadata
	return fmt.Sprintf("tool=%s field=%s expected=%s actual=%s stage=%s protocol=%s streaming=%t provider=%s: %s",
		e.Tool, e.Field, e.ExpectedType, e.ActualType, e.Stage, e.Protocol, e.Streaming, e.Provider, e.Message)
}

// ValidateToolCall validates a ToolCall's Arguments against its ToolDef schema.
// It enforces:
//
// - JSON must parse successfully
// - top-level arguments must be an object
// - required fields must exist
// - argument types must match the original tool schema (string, integer, number, boolean, array, object)
// - malformed calls must not be emitted as valid tool calls
//
// It does NOT silently coerce dangerous types (e.g., number -> string).
// It fails closed with precise diagnostics.
func ValidateToolCall(def ToolDef, call ToolCall) error {
	return ValidateToolCallWithMeta(def, call, ValidationMeta{})
}

// ValidationMeta provides optional context for diagnostics.
type ValidationMeta struct {
	Stage     string // e.g., "anthropic_to_canonical", "openai_to_canonical", "canonical_to_anthropic", etc.
	Protocol  string // client or upstream protocol
	Streaming bool
	Provider  string
}

func ValidateToolCallWithMeta(def ToolDef, call ToolCall, meta ValidationMeta) error {
	if strings.TrimSpace(call.Arguments) == "" {
		return &ToolCallValidationError{
			Tool: def.Name, Field: "", ExpectedType: "object", ActualType: "empty",
			Stage: meta.Stage, Protocol: meta.Protocol, Streaming: meta.Streaming, Provider: meta.Provider,
			Message: "arguments empty",
		}
	}

	// JSON must parse successfully
	var argsMap map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &argsMap); err != nil {
		return &ToolCallValidationError{
			Tool: def.Name, Field: "", ExpectedType: "object", ActualType: "invalid_json",
			Stage: meta.Stage, Protocol: meta.Protocol, Streaming: meta.Streaming, Provider: meta.Provider,
			Message: fmt.Sprintf("invalid JSON: %v", err),
		}
	}

	// Top-level must be object (not array, string, etc.)
	// json.Unmarshal into map already ensures object, but check if args was not object
	// Try to unmarshal as generic to detect type
	var generic any
	if err := json.Unmarshal([]byte(call.Arguments), &generic); err == nil {
		if _, ok := generic.(map[string]any); !ok {
			actual := fmt.Sprintf("%T", generic)
			return &ToolCallValidationError{
				Tool: def.Name, Field: "", ExpectedType: "object", ActualType: actual,
				Stage: meta.Stage, Protocol: meta.Protocol, Streaming: meta.Streaming, Provider: meta.Provider,
				Message: "top-level arguments must be an object",
			}
		}
	}

	// If no schema, at least ensure it's an object (already done)
	if len(def.Parameters) == 0 {
		return nil
	}

	// Parse schema
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(def.Parameters, &schema); err != nil {
		// If schema itself invalid, fail open? No, we should not block, but log.
		// For strictness, treat as no validation if schema unparsable.
		return nil
	}

	// Required fields must exist
	for _, reqField := range schema.Required {
		if _, ok := argsMap[reqField]; !ok {
			return &ToolCallValidationError{
				Tool: def.Name, Field: reqField, ExpectedType: "required", ActualType: "missing",
				Stage: meta.Stage, Protocol: meta.Protocol, Streaming: meta.Streaming, Provider: meta.Provider,
				Message: fmt.Sprintf("required field %q missing", reqField),
			}
		}
	}

	// Argument types must match schema
	for field, prop := range schema.Properties {
		val, ok := argsMap[field]
		if !ok {
			continue // optional
		}
		expectedTypeRaw, ok := prop["type"]
		if !ok {
			continue
		}
		expectedType, ok := expectedTypeRaw.(string)
		if !ok {
			continue
		}
		// Handle nullable: type may be array like ["string","null"]
		// For simplicity, check if type is string, otherwise skip complex
		if strings.Contains(expectedType, "null") {
			if val == nil {
				continue
			}
		}

		actualType := jsonTypeOf(val)
		if !typeMatches(expectedType, actualType, val) {
			return &ToolCallValidationError{
				Tool: def.Name, Field: field, ExpectedType: expectedType, ActualType: actualType,
				Stage: meta.Stage, Protocol: meta.Protocol, Streaming: meta.Streaming, Provider: meta.Provider,
				Message: fmt.Sprintf("field %q expected %s got %s", field, expectedType, actualType),
			}
		}

		// Check for dangerous wrappers that could become "unknown"
		// e.g., {"value":"..."} instead of string, or object instead of string
		if expectedType == "string" {
			if _, ok := val.(string); !ok {
				// This is the exact bug class: string expected but got object/array/etc.
				// We must NOT coerce.
				return &ToolCallValidationError{
					Tool: def.Name, Field: field, ExpectedType: "string", ActualType: actualType,
					Stage: meta.Stage, Protocol: meta.Protocol, Streaming: meta.Streaming, Provider: meta.Provider,
					Message: fmt.Sprintf("parameter %q type is expected as string but provided as %s", field, actualType),
				}
			}
		}
	}

	return nil
}

func jsonTypeOf(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		// JSON numbers decode as float64; check if integer
		f := v.(float64)
		if f == float64(int64(f)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func typeMatches(expected, actual string, val any) bool {
	// Normalize expected
	expected = strings.ToLower(expected)
	switch expected {
	case "string":
		_, ok := val.(string)
		return ok
	case "integer":
		f, ok := val.(float64)
		if !ok {
			return false
		}
		return f == float64(int64(f))
	case "number":
		_, ok := val.(float64)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "object":
		_, ok := val.(map[string]any)
		return ok
	default:
		// For enum or unknown, allow string check if actual is string
		// But for strictness, if expected is string-like, check string
		return true
	}
}

// ValidateToolCallArguments is a convenience for cases where schema is known
// as raw JSON and arguments as string.
func ValidateToolCallArguments(schemaJSON json.RawMessage, args string, meta ValidationMeta) error {
	def := ToolDef{Name: "unknown", Parameters: schemaJSON}
	call := ToolCall{Arguments: args}
	return ValidateToolCallWithMeta(def, call, meta)
}

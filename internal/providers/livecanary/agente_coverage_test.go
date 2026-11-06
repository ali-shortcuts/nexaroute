package livecanary

import (
	"math"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/protocol/canonical"
)

// Agent E coverage: encodeRequest surfaces a JSON encoding failure for
// the default (OpenAI-compatible) branch instead of sending a corrupt
// payload (scenarios.go:738-739). A non-finite temperature passes the
// chat encoder untouched (it assigns the pointer through) and fails
// only at json.Marshal of the assembled request.
func TestAgentEEncodeRequestMarshalError(t *testing.T) {
	nan := math.NaN()
	req := canonical.Request{
		Model:       "backend-model",
		Temperature: &nan,
		Messages: []canonical.Message{{
			Role:  canonical.RoleUser,
			Parts: []canonical.Part{{Type: canonical.PartText, Text: "Reply with the single word OK."}},
		}},
	}
	_, _, err := encodeRequest(config.ProviderConfig{Type: "openai_compatible"}, req)
	if err == nil {
		t.Fatal("expected marshal error for non-finite temperature, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

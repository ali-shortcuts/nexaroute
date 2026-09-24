package detector

import (
	"context"
	"errors"
	"testing"
)

func TestDiscover(t *testing.T) {
	do := func(ctx context.Context, method, path string) (int, error) {
		switch path {
		case "/v1/models":
			return 200, nil
		case "/v1/chat/completions":
			return 405, nil // POST-only route proves existence
		case "/v1/responses":
			return 404, nil
		case "/v1/messages":
			return 401, nil // auth rejection proves existence
		}
		return 404, nil
	}
	c := Discover(context.Background(), "prov", "https://example.com", do)
	if c.Models != VerdictYes || c.OpenAIChat != VerdictYes {
		t.Fatalf("models/chat should be YES: %+v", c)
	}
	if c.OpenAIResponse != VerdictNo {
		t.Fatalf("responses should be NO: %+v", c)
	}
	if c.Anthropic != VerdictYes {
		t.Fatalf("anthropic should be YES: %+v", c)
	}
}

func TestDiscoverTransportUnknown(t *testing.T) {
	do := func(ctx context.Context, method, path string) (int, error) {
		return 0, errors.New("dial failed")
	}
	c := Discover(context.Background(), "prov", "https://example.com", do)
	if c.Models != VerdictUnknown || c.OpenAIChat != VerdictUnknown {
		t.Fatalf("transport errors must stay UNKNOWN: %+v", c)
	}
}

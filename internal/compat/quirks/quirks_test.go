package quirks

import "testing"

func TestGetDefaultsGeneric(t *testing.T) {
	if Get("nope").Name != "generic_openai" {
		t.Fatalf("unknown dialect must default to generic_openai")
	}
	if Get("nvidia_nim").MaxTokensField != "max_tokens" {
		t.Fatalf("nvidia profile wrong: %+v", Get("nvidia_nim"))
	}
}

func TestInfer(t *testing.T) {
	cases := []struct {
		explicit, id, url, typ, want string
	}{
		{"deepseek", "x", "https://example.com", "openai_compatible", "deepseek"},
		{"", "nvidia-main", "https://integrate.api.nvidia.com/v1", "openai_compatible", "nvidia_nim"},
		{"", "or", "https://openrouter.ai/api/v1", "openai_compatible", "openrouter"},
		{"", "ds", "https://api.deepseek.com/v1", "openai_compatible", "deepseek"},
		{"", "plain", "https://example.com/v1", "anthropic_compatible", "generic_anthropic"},
		{"", "plain", "https://example.com/v1", "openai_compatible", "generic_openai"},
	}
	for _, c := range cases {
		if got := Infer(c.explicit, c.id, c.url, c.typ).Name; got != c.want {
			t.Fatalf("Infer(%q,%q,%q,%q) = %q, want %q", c.explicit, c.id, c.url, c.typ, got, c.want)
		}
	}
}

func TestNames(t *testing.T) {
	if len(Names()) < 8 {
		t.Fatalf("expected at least 8 dialects")
	}
}

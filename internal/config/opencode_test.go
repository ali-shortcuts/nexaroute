package config

import "testing"

// Fake keys only; no live calls.

func TestOpencodeConfigDefaultsAndEntries(t *testing.T) {
	p := ProviderConfig{ID: "op", Type: "opencode-go", BaseURL: "https://opencode.ai/inference/go",
		APIKey: "test-key-primary",
		Credentials: []CredentialConfig{
			{Name: "pool", APIKey: "test-key-pool", Enabled: true, Priority: 2, Weight: 0, Email: "op@example.com"},
			{Name: "off", APIKey: "test-key-off", Enabled: false},
			{Name: "dup", APIKey: "test-key-primary", Enabled: true},
		}}
	p.ApplyDefaults()
	if p.ChatPath != OpencodeChatPath || p.ResponsesPath != OpencodeResponsesPath || p.MessagesPath != OpencodeMessagesPath {
		t.Fatalf("go paths = %q %q %q", p.ChatPath, p.ResponsesPath, p.MessagesPath)
	}
	if p.ModelsPath != "" {
		t.Fatalf("go ModelsPath = %q, want empty", p.ModelsPath)
	}
	if p.Credentials[0].Weight != 1 {
		t.Fatalf("credential weight 0 not normalised: %v", p.Credentials[0].Weight)
	}
	entries := p.ResolvedCredentialEntries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (primary + enabled pool; disabled and exact-duplicate dropped)", len(entries))
	}
	if entries[0].Name != "primary" || entries[0].PoolIndex != -1 || entries[0].Priority != 0 {
		t.Fatalf("primary entry wrong: %+v", entries[0])
	}
	if entries[1].Name != "pool" || entries[1].PoolIndex != 0 || entries[1].Priority != 2 || entries[1].Weight != 1 {
		t.Fatalf("pool entry wrong: %+v", entries[1])
	}
	// Email is metadata: it must not appear in resolved entries at all.
	for _, e := range entries {
		if e.Key == "op@example.com" {
			t.Fatal("email leaked into a resolved credential")
		}
	}
}

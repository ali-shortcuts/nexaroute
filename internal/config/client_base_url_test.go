package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateClientBaseURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "empty fallback", url: ""},
		{name: "https with prefix", url: "https://gateway.example.test/nexa"},
		{name: "loopback http", url: "http://127.0.0.1:8080"},
		{name: "userinfo rejected", url: "https://user:secret@gateway.example.test", want: "userinfo"},
		{name: "query rejected", url: "https://gateway.example.test/?token=secret", want: "query"},
		{name: "remote http rejected", url: "http://gateway.example.test", want: "https"},
		{name: "non http rejected", url: "ftp://gateway.example.test", want: "http or https"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateClientBaseURL(tc.url)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestClientBaseURLEnvPrecedenceAndDurablePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	base := Default()
	base.ClientBaseURL = "https://saved.example.test/nexa"
	if err := SaveAtomic(path, base); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXAROUTE_CLIENT_BASE_URL", "https://runtime.example.test/gateway/")
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientBaseURL != "https://runtime.example.test/gateway" {
		t.Fatalf("runtime URL=%q", got.ClientBaseURL)
	}
	if durable, err := LoadBase(path); err != nil {
		t.Fatal(err)
	} else if durable.ClientBaseURL != base.ClientBaseURL {
		t.Fatalf("environment overlay leaked into durable config: %q", durable.ClientBaseURL)
	}
}

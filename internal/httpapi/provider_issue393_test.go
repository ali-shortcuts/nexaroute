package httpapi

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestIssue393ProviderEditPreservesFullConfigAndCredentialSemantics(t *testing.T) {
	const (
		apiCanary       = "ISSUE393_API_SECRET_CANARY_8b3d"
		envCanary       = "ISSUE393_ENV_SECRET_CANARY_319e"
		poolCanary      = "ISSUE393_POOL_SECRET_CANARY_4f21"
		headerCanary    = "ISSUE393_HEADER_SECRET_CANARY_7a9c"
		proxyUserCanary = "ISSUE393_PROXY_USER_CANARY_84b1"
		proxyPassCanary = "ISSUE393_PROXY_PASS_CANARY_6d3e"
		queryCanary     = "ISSUE393_QUERY_SECRET_CANARY_22d0"
		envName         = "NEXA_ISSUE393_PROVIDER_KEY"
		poolEnvName     = "NEXA_ISSUE393_POOL_KEY"
	)
	t.Setenv(envName, envCanary)
	t.Setenv(poolEnvName, "ISSUE393_POOL_ENV_CANARY_2ca1")
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "issue393-provider", Name: "Before edit", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9/v1", Dialect: "openai-chat",
		DataHandling: config.DataHandlingConfig{TrainsOnData: "no", Retention: "limited", Note: "safe fixture"},
		APIKey:       apiCanary, APIKeyEnv: envName, AuthMode: "bearer",
		Credentials: []config.CredentialConfig{
			{Name: "literal-pool", APIKey: poolCanary, Enabled: true},
			{Name: "env-pool", APIKeyEnv: poolEnvName, Enabled: false},
		},
		Headers:        map[string]string{"X-Custom-Auth": headerCanary, "X-Tenant": "tenant-393"},
		ProxyURL:       "http://" + proxyUserCanary + ":" + proxyPassCanary + "@127.0.0.1:9998/path?" + queryCanary + "=1",
		ForwardHeaders: []string{"x-client-id"}, AllowedHosts: []string{"127.0.0.1"},
		ChatPath: "/custom-chat", MaxConcurrency: 17, StreamIdleTimeoutSeconds: 123,
		Enabled: false,
		Models: []config.ModelConfig{{
			ID: "friendly-id", Model: "actual-model", Aliases: []string{"client-alias", "legacy-alias"},
			Enabled: true, Priority: 7, Weight: 2.5, ContextWindow: 8192,
			InputCostPerMTok: 3, OutputCostPerMTok: 6,
			Capabilities: config.Capabilities{Streaming: true, Tools: true, Vision: false, Reasoning: true},
		}},
	}}
	s := testGateway(t, cfg)
	var logs bytes.Buffer
	s.log = log.New(&logs, "", 0)
	secrets := []string{apiCanary, envCanary, poolCanary, "ISSUE393_POOL_ENV_CANARY_2ca1", headerCanary, proxyUserCanary, proxyPassCanary, queryCanary}
	assertNoSecret := func(where, text string) {
		t.Helper()
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaked credential canary %q: %s", where, secret, text)
			}
		}
	}
	get := func(path string) map[string]json.RawMessage {
		t.Helper()
		rr := adminRequest(s, http.MethodGet, path, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		assertNoSecret("GET "+path, rr.Body.String())
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	type providerRead struct {
		Provider             config.ProviderConfig `json:"provider"`
		APIKeyPreview        string                `json:"api_key_preview"`
		HasSecret            bool                  `json:"has_secret"`
		CredentialConfigured bool                  `json:"credential_configured"`
		HeaderNames          []string              `json:"header_names"`
		ProxyDisplay         string                `json:"proxy_display"`
		CredentialCount      int                   `json:"credential_count"`
		CredentialsMeta      []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			Source  string `json:"source"`
			Preview string `json:"preview"`
		} `json:"credentials_meta"`
	}
	decodeProvider := func() providerRead {
		t.Helper()
		body := get("/admin/api/providers/issue393-provider")
		var out providerRead
		if err := json.Unmarshal(body["provider"], &out.Provider); err != nil {
			t.Fatal(err)
		}
		// Decode additive envelope fields from the same redacted response.
		raw, _ := json.Marshal(body)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	read := decodeProvider()
	if read.Provider.APIKey != "" || read.Provider.Headers != nil || read.Provider.ProxyURL != "" {
		t.Fatalf("ordinary read returned write-only fields: %+v", read.Provider)
	}
	for i, cred := range read.Provider.Credentials {
		if cred.APIKey != "" {
			t.Fatalf("credential %d API key was returned", i)
		}
	}
	if !read.CredentialConfigured || !read.HasSecret {
		t.Fatalf("credential status should be configured and resolved: %+v", read)
	}
	if read.APIKeyPreview != "" && read.APIKeyPreview != maskedPlaceholder {
		t.Fatalf("ordinary API key preview = %q, want empty or opaque mask", read.APIKeyPreview)
	}
	if len(read.CredentialsMeta) != 2 || read.CredentialsMeta[0].Name != "literal-pool" || read.CredentialsMeta[1].Name != "env-pool" {
		t.Fatalf("credential pool metadata not safely represented: %+v", read.CredentialsMeta)
	}
	for _, meta := range read.CredentialsMeta {
		if meta.Preview != "" && meta.Preview != maskedPlaceholder {
			t.Fatalf("credential metadata exposes a value fragment: %+v", meta)
		}
	}
	if !reflect.DeepEqual(read.HeaderNames, []string{"X-Custom-Auth", "X-Tenant"}) || read.ProxyDisplay != "http://127.0.0.1:9998" {
		t.Fatalf("safe header/proxy structure missing: headers=%v proxy=%q", read.HeaderNames, read.ProxyDisplay)
	}
	if read.Provider.ID != "issue393-provider" || read.Provider.Name != "Before edit" || read.Provider.Type != "openai_compatible" || read.Provider.BaseURL != "http://127.0.0.1:9/v1" || read.Provider.Enabled || read.Provider.MaxConcurrency != 17 || read.Provider.StreamIdleTimeoutSeconds != 123 {
		t.Fatalf("non-secret provider fields missing from read: %+v", read.Provider)
	}
	if len(read.Provider.Models) != 1 || read.Provider.Models[0].ID != "friendly-id" || !reflect.DeepEqual(read.Provider.Models[0].Aliases, []string{"client-alias", "legacy-alias"}) || read.Provider.Models[0].Priority != 7 || read.Provider.Models[0].Weight != 2.5 || !read.Provider.Models[0].Capabilities.Tools || !read.Provider.Models[0].Capabilities.Reasoning {
		t.Fatalf("model aliases/routing/capability metadata missing from read: %+v", read.Provider.Models)
	}

	original := s.currentConfig().Providers[0]
	read.Provider.Name = "After unrelated edit"
	putBody, err := json.Marshal(map[string]any{
		"provider": read.Provider, "preserve_secret": true, "preserve_headers": true, "preserve_proxy": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	put := adminRequest(s, http.MethodPut, "/admin/api/providers/issue393-provider", string(putBody))
	if put.Code != http.StatusOK {
		t.Fatalf("round-trip PUT status=%d body=%s", put.Code, put.Body.String())
	}
	assertNoSecret("round-trip PUT response", put.Body.String())
	want := original
	want.Name = "After unrelated edit"
	if got := s.currentConfig().Providers[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("unrelated edit changed provider config\n got: %#v\nwant: %#v", got, want)
	}
	_ = decodeProvider() // reopen stays masked and the full read structure remains available

	// Omission is preserve; explicit replacement is a different operation that
	// replaces the old literal/env/pool set with one literal credential.
	const replacement = "ISSUE393_REPLACEMENT_CANARY_55ef"
	secrets = append(secrets, replacement)
	read = decodeProvider()
	read.Provider.APIKey = replacement
	read.Provider.APIKeyEnv = ""
	read.Provider.Credentials = nil
	putBody, err = json.Marshal(map[string]any{
		"provider": read.Provider, "replace_key": true, "preserve_headers": true, "preserve_proxy": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	put = adminRequest(s, http.MethodPut, "/admin/api/providers/issue393-provider", string(putBody))
	if put.Code != http.StatusOK {
		t.Fatalf("replacement PUT status=%d body=%s", put.Code, put.Body.String())
	}
	assertNoSecret("replacement PUT response", put.Body.String())
	live := s.currentConfig().Providers[0]
	if live.APIKey != replacement || live.APIKeyEnv != "" || len(live.Credentials) != 0 {
		t.Fatalf("explicit replacement did not replace the whole old credential set: %+v", live)
	}
	if live.Name != want.Name || live.MaxConcurrency != original.MaxConcurrency || !reflect.DeepEqual(live.Models, original.Models) || !reflect.DeepEqual(live.Headers, original.Headers) || live.ProxyURL != original.ProxyURL {
		t.Fatalf("replacement reset unrelated provider fields: %+v", live)
	}

	// Explicit clear is separate from an omitted credential field.
	read = decodeProvider()
	read.Provider.APIKey = ""
	read.Provider.APIKeyEnv = ""
	read.Provider.Credentials = nil
	putBody, err = json.Marshal(map[string]any{
		"provider": read.Provider, "replace_key": true, "preserve_headers": true, "preserve_proxy": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	put = adminRequest(s, http.MethodPut, "/admin/api/providers/issue393-provider", string(putBody))
	if put.Code != http.StatusOK {
		t.Fatalf("clear PUT status=%d body=%s", put.Code, put.Body.String())
	}
	assertNoSecret("clear PUT response", put.Body.String())
	live = s.currentConfig().Providers[0]
	if live.APIKey != "" || live.APIKeyEnv != "" || len(live.Credentials) != 0 {
		t.Fatalf("explicit clear retained credentials: %+v", live)
	}
	read = decodeProvider()
	if read.CredentialConfigured || read.HasSecret {
		t.Fatalf("clear did not update safe credential status: %+v", read)
	}

	// Invalid updates return generic validation details and never echo supplied secrets.
	badProvider := read.Provider
	badProvider.Type = "not-a-provider"
	badProvider.APIKey = "ISSUE393_INVALID_BODY_CANARY_0117"
	secrets = append(secrets, badProvider.APIKey)
	badBody, err := json.Marshal(map[string]any{"provider": badProvider, "replace_key": true})
	if err != nil {
		t.Fatal(err)
	}
	bad := adminRequest(s, http.MethodPut, "/admin/api/providers/issue393-provider", string(badBody))
	if bad.Code < 400 {
		t.Fatalf("invalid provider update status=%d body=%s", bad.Code, bad.Body.String())
	}
	assertNoSecret("validation error", bad.Body.String())

	for _, path := range []string{"/admin/api/providers", "/admin/api/snapshot", "/admin/api/config/history", "/metrics"} {
		rr := adminRequest(s, http.MethodGet, path, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		assertNoSecret("GET "+path, rr.Body.String())
	}
	if strings.Contains(logs.String(), "ISSUE393_") {
		t.Fatalf("application logs contain an Issue #393 canary: %s", logs.String())
	}
	events, err := json.Marshal(s.bus.SnapshotLimit(100))
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret("event snapshot", string(events))
	persisted, err := os.ReadFile(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret("serialized config", string(persisted))
}

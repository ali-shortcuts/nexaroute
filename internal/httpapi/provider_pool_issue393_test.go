package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestIssue393CredentialPoolEditingIsExplicitAndAtomic(t *testing.T) {
	const primary = "POOL_PRIMARY_SECRET_9a"
	const first = "POOL_FIRST_SECRET_9b"
	const second = "POOL_SECOND_SECRET_9c"
	const added = "POOL_ADDED_SECRET_9d"
	const replaced = "POOL_REPLACED_SECRET_9e"
	const envName = "NEXA_POOL_ISSUE393_ENV"
	t.Setenv(envName, "POOL_ENV_SECRET_9f")
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "pool-provider", Name: "Pool provider", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:9/v1", APIKey: primary, AuthMode: "bearer", Enabled: true,
		Credentials: []config.CredentialConfig{
			{Name: "first", APIKey: first, Enabled: true},
			{Name: "second", APIKey: second, Enabled: false},
			{Name: "environment", APIKeyEnv: envName, Enabled: true},
		},
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	provider := `{"id":"pool-provider","name":"Pool provider","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","auth_mode":"bearer","enabled":true,"models":[{"id":"m","model":"m","enabled":true,"weight":1}]}`

	// A normal edit omits credential_pool and preserves every pool entry.
	before := s.currentConfig().Providers[0]
	rr := adminRequest(s, http.MethodPut, "/admin/api/providers/pool-provider", `{"provider":`+provider+`,"preserve_secret":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("omitted pool update status=%d body=%s", rr.Code, rr.Body.String())
	}
	got := s.currentConfig().Providers[0]
	if got.APIKey != primary || len(got.Credentials) != len(before.Credentials) {
		t.Fatalf("omitted pool changed saved credentials: got=%+v", got)
	}
	for i := range before.Credentials {
		if got.Credentials[i] != before.Credentials[i] {
			t.Fatalf("omitted pool changed entry %d: got=%+v want=%+v", i, got.Credentials[i], before.Credentials[i])
		}
	}

	// Index 1 is replaced and a new entry is appended; index 0 and the env
	// reference remain untouched, and ordering follows the existing slice.
	rr = adminRequest(s, http.MethodPut, "/admin/api/providers/pool-provider", `{"provider":`+provider+`,"credential_pool":{"entries":[{"index":1,"name":"second-new","enabled":true,"api_key":"`+replaced+`"},{"name":"added","enabled":true,"api_key":"`+added+`"}]}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("pool add/replace status=%d body=%s", rr.Code, rr.Body.String())
	}
	got = s.currentConfig().Providers[0]
	if len(got.Credentials) != 4 || got.Credentials[0].APIKey != first || got.Credentials[1].APIKey != replaced || got.Credentials[2].APIKeyEnv != envName || got.Credentials[3].APIKey != added || !got.Credentials[1].Enabled {
		t.Fatalf("pool add/replace result=%+v", got.Credentials)
	}

	// Removal is indexed against the saved pool and affects only the target.
	rr = adminRequest(s, http.MethodPut, "/admin/api/providers/pool-provider", `{"provider":`+provider+`,"credential_pool":{"remove":[0]}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("pool removal status=%d body=%s", rr.Code, rr.Body.String())
	}
	got = s.currentConfig().Providers[0]
	if len(got.Credentials) != 3 || got.Credentials[0].APIKey != replaced || got.Credentials[1].APIKeyEnv != envName || got.Credentials[2].APIKey != added {
		t.Fatalf("pool removal affected unintended entries: %+v", got.Credentials)
	}

	// A patch may address each saved index at most once. Conflicting update and
	// removal operations are rejected before the cloned config is persisted.
	before = got
	rr = adminRequest(s, http.MethodPut, "/admin/api/providers/pool-provider", `{"provider":`+provider+`,"credential_pool":{"entries":[{"index":0,"enabled":false},{"index":0,"name":"duplicate"}]}}`)
	if rr.Code < 400 {
		t.Fatalf("duplicate indexed update status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = adminRequest(s, http.MethodPut, "/admin/api/providers/pool-provider", `{"provider":`+provider+`,"credential_pool":{"entries":[{"index":0,"enabled":false}],"remove":[0]}}`)
	if rr.Code < 400 {
		t.Fatalf("update/remove conflict status=%d body=%s", rr.Code, rr.Body.String())
	}
	got = s.currentConfig().Providers[0]
	if got.APIKey != before.APIKey || len(got.Credentials) != len(before.Credentials) {
		t.Fatalf("rejected pool conflicts corrupted saved config: %+v", got)
	}

	// An explicit replacement with no entries is different from omission.
	rr = adminRequest(s, http.MethodPut, "/admin/api/providers/pool-provider", `{"provider":`+provider+`,"credential_pool":{"replace":true,"entries":[]}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("explicit empty pool status=%d body=%s", rr.Code, rr.Body.String())
	}
	got = s.currentConfig().Providers[0]
	if got.APIKey != primary || len(got.Credentials) != 0 {
		t.Fatalf("explicit empty pool did not clear only the pool: %+v", got)
	}

	// A failed patch is rejected before applyConfig and leaves the saved pool
	// untouched, including the primary credential.
	before = got
	rr = adminRequest(s, http.MethodPut, "/admin/api/providers/pool-provider", `{"provider":`+provider+`,"credential_pool":{"entries":[{"name":"missing-secret"}]}}`)
	if rr.Code < 400 {
		t.Fatalf("invalid pool update status=%d body=%s", rr.Code, rr.Body.String())
	}
	got = s.currentConfig().Providers[0]
	if got.APIKey != before.APIKey || len(got.Credentials) != len(before.Credentials) {
		t.Fatalf("failed pool update corrupted saved config: %+v", got)
	}
	for _, secret := range []string{primary, first, second, added, replaced, "POOL_ENV_SECRET_9f"} {
		for _, path := range []string{"/admin/api/providers/pool-provider", "/admin/api/providers", "/admin/api/snapshot"} {
			read := adminRequest(s, http.MethodGet, path, "")
			if read.Code != http.StatusOK || strings.Contains(read.Body.String(), secret) {
				t.Fatalf("secret %q exposed by %s: status=%d body=%s", secret, path, read.Code, read.Body.String())
			}
		}
	}
}

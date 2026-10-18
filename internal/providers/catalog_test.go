package providers

import (
	"net/url"
	"strings"
	"testing"
)

// Issue #394 catalog contract tests: every preset must map to a real
// implemented protocol/auth pairing, carry non-fabricated discovery/capability
// claims, and survive search/category filtering used by the admin UI.

func presetByIDs(t *testing.T) map[string]Preset {
	t.Helper()
	byID := map[string]Preset{}
	for _, p := range Presets() {
		if _, dup := byID[p.ID]; dup {
			t.Fatalf("duplicate preset id %q", p.ID)
		}
		byID[p.ID] = p
	}
	return byID
}

func TestPresetsCatalogSchemaAndUniqueness(t *testing.T) {
	presets := Presets()
	if len(presets) < 10 {
		t.Fatalf("catalog collapsed: %d presets", len(presets))
	}
	authModes := map[string]bool{"bearer": true, "x-api-key": true, "x-goog-api-key": true, "api-key": true, "none": true, "": true}
	for _, p := range presets {
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Category) == "" {
			t.Fatalf("preset %q incomplete identity: %+v", p.ID, p)
		}
		if p.Protocol != p.Type {
			t.Errorf("preset %q protocol %q != dialect %q", p.ID, p.Protocol, p.Type)
		}
		if !authModes[p.AuthMode] {
			t.Errorf("preset %q has unsupported auth_mode %q", p.ID, p.AuthMode)
		}
		// Inventory endpoints are only advertised for implemented dialects
		// that really expose a /models resource; Azure/Vertex/Bedrock must
		// not claim discovery from URL reachability alone.
		switch p.Type {
		case "bedrock", "azure_openai", "vertex":
			if p.SupportsDiscovery {
				t.Errorf("preset %q advertises discovery although the dialect has no /models endpoint", p.ID)
			}
		}
		if p.Status != PresetStatusVerified && p.Status != PresetStatusPartial {
			t.Errorf("preset %q status %q must be verified or partial", p.ID, p.Status)
		}
		if len(p.Capabilities) == 0 && p.Status == PresetStatusVerified {
			t.Errorf("preset %q claims verified status with no implemented capabilities", p.ID)
		}
	}
}

func TestPresetsCatalogBaseURLsAreValidAbsoluteHTTP(t *testing.T) {
	for _, p := range Presets() {
		if p.Custom {
			continue // template entry has no fixed endpoint
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			t.Errorf("preset %q base_url %q is not an absolute http(s) URL", p.ID, p.BaseURL)
			continue
		}
		if u.User != nil {
			t.Errorf("preset %q base_url embeds credentials", p.ID)
		}
	}
}

func TestPresetsCatalogDialectCapabilitiesAreHonest(t *testing.T) {
	byID := presetByIDs(t)
	if got := byID["openai-responses"].Capabilities; !strings.Contains(strings.Join(got, ","), CapabilityResponses) {
		t.Errorf("openai-responses preset must advertise the responses capability, got %v", got)
	}
	if got := byID["anthropic"].Capabilities; !strings.Contains(strings.Join(got, ","), CapabilityCountTokens) {
		t.Errorf("anthropic preset must advertise count_tokens, got %v", got)
	}
	// Discovery claims must be real: either an explicit inventory path on
	// the preset or the implemented openai-family default (/v1/models).
	// Azure, Vertex and Bedrock must never advertise discovery — their
	// endpoints have no plain /models resource, so reachability of the base
	// URL would be the only "signal", which the catalog refuses to mix with
	// inventory support.
	for _, p := range Presets() {
		explOrDefault := p.ModelsPath != "" || ((!p.Custom) && (p.Type == "openai_compatible" || p.Type == "openai_responses"))
		if p.SupportsDiscovery && !explOrDefault {
			t.Errorf("preset %q claims discovery without any real inventory endpoint", p.ID)
		}
		if !p.SupportsDiscovery && explOrDefault {
			t.Errorf("preset %q hides a real inventory endpoint (path=%q type=%q)", p.ID, p.ModelsPath, p.Type)
		}
		switch p.Type {
		case "bedrock", "azure_openai", "vertex":
			if p.SupportsDiscovery {
				t.Errorf("preset %q advertises discovery although the dialect has no /models endpoint", p.ID)
			}
		}
	}
}

func TestPresetsCatalogSearchAndCategoryFilter(t *testing.T) {
	all := Presets()
	got := FilterPresets(all, "anthropic", "")
	if len(got) != 1 || got[0].ID != "anthropic" {
		t.Fatalf("query 'anthropic' returned %v", ids(got))
	}
	got = FilterPresets(all, "OPENAI", "Direct")
	ids0 := ids(got)
	if len(ids0) == 0 || !contains(ids0, "openai") || !contains(ids0, "openai-responses") {
		t.Fatalf("case-insensitive filtered search failed: %v", ids0)
	}
	if contains(ids0, "openrouter") {
		t.Fatalf("category filter leaked into results: %v", ids0)
	}
	if out := FilterPresets(all, "", "NoSuchCategory"); len(out) != 0 {
		t.Fatalf("unknown category must return nothing, got %v", ids(out))
	}
	if out := FilterPresets(all, "", ""); len(out) != len(all) {
		t.Fatalf("empty filters must pass through whole catalog: %d vs %d", len(out), len(all))
	}
	// Local/self-hosted presets stay searchable by category.
	if out := FilterPresets(all, "ollama", "Local"); len(out) != 1 {
		t.Fatalf("category Local filter failed: %v", ids(out))
	}
}

func TestPresetsCatalogCategoriesAreDerived(t *testing.T) {
	all := Presets()
	cats := PresetCategories(all)
	if len(cats) < 4 {
		t.Fatalf("expected multiple categories, got %v", cats)
	}
	for i := 1; i < len(cats); i++ {
		if cats[i-1] >= cats[i] {
			t.Fatalf("categories not sorted/unique: %v", cats)
		}
	}
}

func ids(in []Preset) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.ID)
	}
	return out
}

func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}

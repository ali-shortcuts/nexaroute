package providers

import (
	"strings"
)

// Preset catalog (issue #394). A preset is the (a) provider-brand layer of
// the catalog: it names a vendor or deployment style and pairs it with the
// (b) protocol dialect/capabilities the gateway actually implements. A
// preset is NOT a deployment: (c) individual providers and models are
// configured separately through the Admin API, and every preset here maps
// to an adapter and auth mode implemented in this package.

// Capabilities used by presets. Only wire features with an implemented code
// path in this repository are advertised; wording matches the canonical
// protocol identities in internal/protocol/canonical.
const (
	CapabilityChat        = "chat"
	CapabilityResponses   = "responses"
	CapabilityCountTokens = "count_tokens"
	CapabilityModelsList  = "models_list"
)

// Preset status values.
const (
	PresetStatusVerified = "verified"
	PresetStatusPartial  = "partial"
)

// capabilitiesForDialect lists the wire capabilities the gateway implements
// for a dialect. `models_list` additionally requires the preset to name a
// real inventory endpoint (see finalizePreset); discovery is always
// best-effort and never a prerequisite for saving a manual model.
func capabilitiesForDialect(dialect string) []string {
	switch dialect {
	case "openai_compatible":
		return []string{CapabilityChat, CapabilityModelsList}
	case "anthropic_compatible":
		return []string{CapabilityChat, CapabilityCountTokens, CapabilityModelsList}
	case "openai_responses":
		return []string{CapabilityChat, CapabilityResponses, CapabilityModelsList}
	case "azure_openai", "vertex", "bedrock", "gemini":
		return []string{CapabilityChat}
	default:
		return nil
	}
}

// finalizePreset fills the derived catalog fields from the dialect and the
// preset's own paths, so capability/status claims can never drift from the
// implemented code. Status stays "partial" for any dialect the gateway does
// not implement yet.
func finalizePreset(p Preset) Preset {
	p.Protocol = p.Type
	p.Capabilities = capabilitiesForDialect(p.Type)
	// Inventory endpoint: an explicit ModelsPath, or the implemented
	// openai-family default (/v1/models) when the dialect standardizes one.
	// Azure deployments, Vertex named projects and Bedrock's Converse
	// runtime never expose a plain listing on the preset base URL, so they
	// stay discovery-free even if a path is present by mistake.
	inventory := p.ModelsPath
	if inventory == "" && !p.Custom && (p.Type == "openai_compatible" || p.Type == "openai_responses") {
		inventory = "/v1/models"
	}
	p.SupportsDiscovery = inventory != ""
	switch p.Type {
	case "bedrock", "azure_openai", "vertex":
		p.SupportsDiscovery = false
	}
	// models_list is only a capability claim when the catalog entry names a
	// real inventory endpoint. A generic/custom template is not itself proof
	// that a future configured endpoint exposes the models-list API.
	if !p.SupportsDiscovery {
		capabilities := p.Capabilities[:0]
		for _, capability := range p.Capabilities {
			if capability != CapabilityModelsList {
				capabilities = append(capabilities, capability)
			}
		}
		p.Capabilities = capabilities
	}
	if len(p.Capabilities) == 0 {
		p.Status = PresetStatusPartial
	} else {
		p.Status = PresetStatusVerified
	}
	return p
}

// PresetCategories returns the sorted unique categories of the given
// presets, for the admin catalog's category filter chips.
func PresetCategories(presets []Preset) []string {
	seen := map[string]bool{}
	categories := make([]string, 0, 8)
	for _, p := range presets {
		if p.Category == "" || seen[p.Category] {
			continue
		}
		seen[p.Category] = true
		categories = append(categories, p.Category)
	}
	sortStrings(categories)
	return categories
}

// FilterPresets applies a case-insensitive best-effort search over the
// preset identity fields plus an optional exact category filter. An empty
// query matches everything, so the unfiltered endpoint response stays
// backward compatible with callers that ignore the new fields.
func FilterPresets(presets []Preset, query, category string) []Preset {
	q := strings.TrimSpace(strings.ToLower(query))
	category = strings.TrimSpace(category)
	out := make([]Preset, 0, len(presets))
	for _, p := range presets {
		if category != "" && p.Category != category {
			continue
		}
		if q != "" && !presetMatches(p, q) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func presetMatches(p Preset, q string) bool {
	for _, f := range []string{p.ID, p.Name, p.Category, p.Type, p.Protocol, p.AuthMode, p.BaseURL} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// sortStrings is a tiny in-place sort so catalog.go keeps a single
// dependency besides strings.
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}

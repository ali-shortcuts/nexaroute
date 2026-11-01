package providers

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Registry contract: every profile is uniquely identified, maps to a real
// adapter dialect (or explicitly to none, for KIE), carries an honest
// evidence state, and joins to the exact documented URL via endpoint().
// These tests pin the joined URL per profile so a path edit cannot
// silently retarget a provider.

var allowedEvidence = map[string]bool{
	EvidenceDocumented: true, EvidenceDocumentedPreview: true,
	EvidenceDocumentedBeta: true, EvidenceSecondary: true,
	EvidenceUnverified: true,
}

var allowedDialects = map[string]bool{
	"openai_compatible": true, "anthropic_compatible": true,
	"openai_responses": true, "": true, // "" = KIE inventory-only profile
}

var profileJoinedURLs = map[string]string{
	"ashna-chat":                    "https://api.ashna.ai/v1/api/chat/completions",
	"ashna-messages":                "https://api.ashna.ai/v1/api/v1/messages",
	"ashna-responses":               "https://api.ashna.ai/v1/api/responses",
	"anyrouter-chat":                "https://anyrouter.dev/api/v1/chat/completions",
	"anyrouter-messages":            "https://anyrouter.dev/api/v1/messages",
	"anyrouter-responses":           "https://anyrouter.dev/api/v1/responses",
	"agentrouter-chat":              "https://agentrouter.org/v1/chat/completions",
	"agentrouter-messages":          "https://agentrouter.org/v1/messages",
	"nararouter-chat":               "https://router.bynara.id/v1/chat/completions",
	"nararouter-messages":           "https://router.bynara.id/v1/messages",
	"nararouter-responses":          "https://router.bynara.id/v1/responses",
	"cloudflare-compat-chat":        "https://gateway.ai.cloudflare.com/v1/ACCT/GTW/compat/chat/completions",
	"cloudflare-openai-chat":        "https://gateway.ai.cloudflare.com/v1/ACCT/GTW/openai/chat/completions",
	"cloudflare-openai-responses":   "https://gateway.ai.cloudflare.com/v1/ACCT/GTW/openai/responses",
	"cloudflare-anthropic-messages": "https://gateway.ai.cloudflare.com/v1/ACCT/GTW/anthropic/v1/messages",
	"cline-chat":                    "https://api.cline.bot/v1/chat/completions",
	"puter-chat":                    "https://api.puter.com/puterai/openai/v1/chat/completions",
	"mistral-chat":                  "https://api.mistral.ai/v1/chat/completions",
	"mistral-responses":             "https://api.mistral.ai/v1/responses",
	"groq-chat":                     "https://api.groq.com/openai/v1/chat/completions",
	"groq-responses":                "https://api.groq.com/openai/v1/responses",
	"nvidia-nim-hosted-chat":        "https://integrate.api.nvidia.com/v1/chat/completions",
	"nvidia-nim-selfhosted-chat":    "http://localhost:8000/v1/chat/completions",
	"venice-chat":                   "https://api.venice.ai/api/v1/chat/completions",
	"venice-responses":              "https://api.venice.ai/api/v1/responses",
	"opencode-zen-chat":             "https://opencode.ai/zen/v1/chat/completions",
	"opencode-zen-messages":         "https://opencode.ai/zen/v1/messages",
	"opencode-zen-responses":        "https://opencode.ai/zen/v1/responses",
	"oxionai-chat":                  "https://oxionai.com/v1/chat/completions",
	"orcarouter-chat":               "https://api.orcarouter.ai/v1/chat/completions",
	"orcarouter-messages":           "https://api.orcarouter.ai/v1/messages",
	"orcarouter-responses":          "https://api.orcarouter.ai/v1/responses",
	"unorouter-chat":                "https://api.unorouter.com/v1/chat/completions",
	"unorouter-messages":            "https://api.unorouter.com/v1/messages",
	"tokenjuice-chat":               "https://api.tokenjuice.ai/v1/chat/completions",
}

func profileMainPath(p Profile) string {
	switch p.Protocol {
	case ProfileProtocolChat:
		return p.ChatPath
	case ProfileProtocolMessages:
		return p.MessagesPath
	case ProfileProtocolResponses:
		return p.ResponsesPath
	default:
		return ""
	}
}

func TestProfilesSchemaUniquenessAndEvidence(t *testing.T) {
	profiles := Profiles()
	if len(profiles) < 20 {
		t.Fatalf("profile registry collapsed: %d profiles", len(profiles))
	}
	dateRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	seen := map[string]bool{}
	for _, p := range profiles {
		if seen[p.ID] {
			t.Fatalf("duplicate profile id %q", p.ID)
		}
		seen[p.ID] = true
		if p.ProviderID == "" || p.ProviderName == "" || p.Category == "" || p.DocsURL == "" {
			t.Errorf("profile %q incomplete identity: %+v", p.ID, p)
		}
		if !allowedEvidence[p.Evidence] {
			t.Errorf("profile %q evidence %q is not an allowed state", p.ID, p.Evidence)
		}
		if !dateRe.MatchString(p.ReviewedDate) {
			t.Errorf("profile %q reviewed_date %q is not YYYY-MM-DD", p.ID, p.ReviewedDate)
		}
		if !allowedDialects[p.Dialect] {
			t.Errorf("profile %q dialect %q is not an implemented adapter dialect", p.ID, p.Dialect)
		}
		if len(p.Limitations) == 0 {
			t.Errorf("profile %q must record at least one limitation (unknowns are limitations too)", p.ID)
		}
		for _, c := range p.Capabilities {
			if !profileCapabilities[c] {
				t.Errorf("profile %q capability %q is not in the Task 4 capability list", p.ID, c)
			}
		}
		// No credential material in the registry: URLs must not embed
		// user info, and no field may look like a key value.
		checkURL := strings.ReplaceAll(strings.ReplaceAll(p.BaseURL, "{account_id}", "ACCT"), "{gateway_id}", "GTW")
		u, err := url.Parse(checkURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			t.Errorf("profile %q base_url %q is not an absolute http(s) URL", p.ID, p.BaseURL)
		} else if u.User != nil {
			t.Errorf("profile %q base_url embeds credentials", p.ID)
		}
		if p.Protocol == ProfileProtocolProviderSpecific {
			if p.CreateTaskPath == "" || p.RecordInfoPath == "" {
				t.Errorf("profile %q provider_specific profile needs task paths", p.ID)
			}
			if p.Dialect != "" {
				t.Errorf("profile %q provider_specific profile must not claim an adapter dialect", p.ID)
			}
			continue
		}
		if profileMainPath(p) == "" {
			t.Errorf("profile %q (%s) has no main path", p.ID, p.Protocol)
		}
		if want, ok := profileJoinedURLs[p.ID]; ok {
			base := strings.ReplaceAll(strings.ReplaceAll(p.BaseURL, "{account_id}", "ACCT"), "{gateway_id}", "GTW")
			if got := endpoint(base, profileMainPath(p)); got != want {
				t.Errorf("profile %q joined URL = %q, want %q (endpoint joining changed or path wrong)", p.ID, got, want)
			}
		} else {
			t.Errorf("profile %q has no pinned joined URL in profileJoinedURLs; add one before shipping", p.ID)
		}
	}
	// Every multi-protocol provider shares one ProviderID across profiles.
	// AgentRouter is deliberately absent: it ships exactly two documented
	// surfaces (Messages and Chat) and no Responses profile, because no
	// Responses route is documented for this repository to rely on.
	for _, provider := range []string{"ashna", "anyrouter", "nararouter", "opencode-zen", "orcarouter"} {
		if got := ProfilesForProvider(provider); len(got) < 3 {
			t.Errorf("provider %q should have >=3 profiles, got %d", provider, len(got))
		}
	}
	if _, ok := ProfileByID("no-such-profile"); ok {
		t.Errorf("ProfileByID must report missing profiles")
	}
}

func TestProfilesToProviderConfig(t *testing.T) {
	for _, p := range Profiles() {
		cfg, err := p.ToProviderConfig("TEST_PROFILE_KEY_ENV")
		switch {
		case p.Dialect == "":
			if err == nil {
				t.Errorf("profile %q without a dialect must fail ToProviderConfig", p.ID)
			}
			continue
		case p.IsTemplated():
			if err == nil {
				t.Errorf("profile %q templated base URL must fail ToProviderConfig until substituted", p.ID)
			}
			// Substituted, it must map cleanly.
			sub := strings.ReplaceAll(strings.ReplaceAll(p.BaseURL, "{account_id}", "ACCT"), "{gateway_id}", "GTW")
			cfg2, err2 := p.ToProviderConfigWithBase(sub, "TEST_PROFILE_KEY_ENV")
			if err2 != nil {
				t.Errorf("profile %q substituted ToProviderConfig failed: %v", p.ID, err2)
				continue
			}
			if !strings.Contains(cfg2.BaseURL, "ACCT") {
				t.Errorf("profile %q substituted base URL lost the substitution: %q", p.ID, cfg2.BaseURL)
			}
			continue
		default:
			if err != nil {
				t.Errorf("profile %q ToProviderConfig failed: %v", p.ID, err)
				continue
			}
		}
		if cfg.Type != p.Dialect || cfg.BaseURL != p.BaseURL || cfg.AuthMode != p.AuthMode {
			t.Errorf("profile %q config mismatch: %+v", p.ID, cfg)
		}
		if cfg.APIKeyEnv != "TEST_PROFILE_KEY_ENV" {
			t.Errorf("profile %q must carry the env-var reference, not a value", p.ID)
		}
		if cfg.APIKey != "" {
			t.Errorf("profile %q config must not contain a credential value", p.ID)
		}
		if cfg.ChatPath != p.ChatPath || cfg.MessagesPath != p.MessagesPath || cfg.ResponsesPath != p.ResponsesPath || cfg.ModelsPath != p.ModelsPath {
			t.Errorf("profile %q config paths do not match the profile", p.ID)
		}
		if got := p.AuthHeader(); (p.AuthMode == "none") != (got == "") {
			t.Errorf("profile %q auth header %q inconsistent with mode %q", p.ID, got, p.AuthMode)
		}
	}
}

// The compatibility matrix must name every shipped profile and must never
// present a documentation-only profile as live-verified.
func TestProfilesDocumentedInMatrix(t *testing.T) {
	b, err := os.ReadFile("../../docs/PROVIDER_COMPATIBILITY_MATRIX.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(b)
	for _, p := range Profiles() {
		if !strings.Contains(doc, p.ID) {
			t.Errorf("matrix does not mention profile %q", p.ID)
		}
	}
	// Affirmative verification claims only: the matrix explicitly negates
	// these phrases ("Nothing here is ..."), and that disclaimer must
	// not trip this check. A line claiming one without a negation fails.
	for _, phrase := range []string{"live verified", "verified live", "production verified"} {
		for _, line := range strings.Split(strings.ToLower(doc), "\n") {
			if !strings.Contains(line, phrase) {
				continue
			}
			if strings.Contains(line, "not") || strings.Contains(line, "never") || strings.Contains(line, "nothing") || strings.Contains(line, "must never") {
				continue
			}
			t.Errorf("matrix must not claim %q for documentation-only profiles (line: %s)", phrase, strings.TrimSpace(line))
		}
	}
	for _, provider := range []string{"BeatAPI", "Inception Labs", "LarpRouter", "Refuseless", "ExperientialLabs", "Atria", "Agnes", "InferX", "Faceb", "Routeway", "Apivale", "CompanyFabric", "BluesMinds", "APInex"} {
		if !strings.Contains(doc, provider) {
			t.Errorf("matrix must record unverified provider %q explicitly, not silently drop it", provider)
		}
	}
}

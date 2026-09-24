package detector

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Protocol auto-discovery. See spec section 14.
// Probes well-known endpoints once at provider creation / manual refresh and
// caches a verified protocol contract. Never runs on the request hot path.

type Verdict string

const (
	VerdictYes     Verdict = "YES"
	VerdictNo      Verdict = "NO"
	VerdictUnknown Verdict = "UNKNOWN"
)

// Contract is the verified protocol surface of one provider.
type Contract struct {
	ProviderID     string    `json:"provider_id"`
	BaseURL        string    `json:"base_url"`
	OpenAIChat     Verdict   `json:"openai_chat"`
	OpenAIResponse Verdict   `json:"openai_responses"`
	Anthropic      Verdict   `json:"anthropic"`
	Models         Verdict   `json:"models"`
	VerifiedAt     time.Time `json:"verified_at"`
}

// Doer performs one HTTP request for discovery probing.
type Doer func(ctx context.Context, method, path string) (status int, err error)

// Discover runs the discovery pipeline with a bounded deadline per probe.
// Only high-signal outcomes flip a verdict; transport errors stay UNKNOWN.
func Discover(ctx context.Context, providerID, baseURL string, do Doer) Contract {
	c := Contract{ProviderID: providerID, BaseURL: baseURL,
		OpenAIChat: VerdictUnknown, OpenAIResponse: VerdictUnknown,
		Anthropic: VerdictUnknown, Models: VerdictUnknown, VerifiedAt: time.Now()}
	if do == nil {
		return c
	}
	probe := func(path string) Verdict {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		status, err := do(pctx, http.MethodGet, path)
		if err != nil {
			return VerdictUnknown
		}
		switch {
		case status >= 200 && status < 500 && status != http.StatusNotFound && status != http.StatusMethodNotAllowed:
			// 405 on a GET against a POST-only endpoint still proves the route exists.
			return VerdictYes
		case status == http.StatusMethodNotAllowed:
			return VerdictYes
		case status == http.StatusNotFound:
			return VerdictNo
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			// Auth rejection proves the endpoint exists behind credentials.
			return VerdictYes
		default:
			return VerdictUnknown
		}
	}
	// Known-provider fingerprints first (cheap, no network beyond models).
	lower := strings.ToLower(providerID + " " + baseURL)
	switch {
	case strings.Contains(lower, "anthropic.com"):
		c.Anthropic = VerdictYes
	}
	c.Models = probe("/v1/models")
	c.OpenAIChat = probe("/v1/chat/completions")
	c.OpenAIResponse = probe("/v1/responses")
	c.Anthropic = combineAnthropic(c.Anthropic, probe("/v1/messages"))
	c.VerifiedAt = time.Now()
	return c
}

func combineAnthropic(fingerprint, probed Verdict) Verdict {
	if fingerprint == VerdictYes {
		return VerdictYes
	}
	return probed
}

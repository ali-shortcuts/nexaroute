package config

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzValidateLoopbackGatewayBaseURL is the property test for the
// loopback-only rule: whatever string the fuzzer produces, an
// acceptance by ValidateLoopbackGatewayBaseURL must imply, on an
// independent re-parse, exactly one of the three approved host
// spellings, an http(s) scheme, and no credentials, path, query, or
// fragment. It must never panic. This catches normalization escapes
// of the kind that bit PR #794 in CI: Go 1.27 accepts
// http://[::ffff:127.0.0.1]:8080 and net.ParseIP normalizes it to
// 127.0.0.1, so any future return to ParseIP-based comparison fails
// here as soon as the fuzzer (or a seed) offers a mapped spelling.
//
// Discovery: the audit extended-fuzz job auto-discovers every
// Fuzz target, so this target runs there without a workflow edit.
// Manual short run: go test ./internal/config -run='^$'
// -fuzz=FuzzValidateLoopbackGatewayBaseURL -fuzztime=10s.
// Wiring it into verify.sh / the CI fuzz-smoke list is a follow-up
// that needs a mode-preserving (for verify.sh) or
// workflows-permitted (for .github/workflows) push; the connector
// push used for this branch writes files at mode 644 and cannot
// push workflow files at all (403).
func FuzzValidateLoopbackGatewayBaseURL(f *testing.F) {
	seeds := []string{
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"https://localhost:8443",
		"http://[::ffff:127.0.0.1]:8080",
		"http://[::ffff:7f00:1]:8080",
		"http://[0:0:0:0:0:ffff:127.0.0.1]:8080",
		"http://[0:0:0:0:0:0:0:1]:8080",
		"http://127.000.0.1:8080",
		"http://localhost.:8080",
		"http://user:secret@127.0.0.1:8080",
		"http://127.0.0.1:8080/v1?x=1#frag",
		"https://example.com",
		"",
		"http://[::1",
		"http://127.0.0.1:99999",
		"http://127.0.0.1:0",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if err := ValidateLoopbackGatewayBaseURL(raw); err != nil {
			return
		}
		// Accepted: prove the allowlist property independently.
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			t.Fatalf("accepted %q but it does not re-parse: %v", raw, err)
		}
		if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
			t.Fatalf("accepted %q with non-http(s) scheme %q", raw, u.Scheme)
		}
		if u.User != nil || u.Opaque != "" {
			t.Fatalf("accepted %q with credentials or opaque form", raw)
		}
		if u.Path != "" && u.Path != "/" {
			t.Fatalf("accepted %q with path %q", raw, u.Path)
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
			t.Fatalf("accepted %q with query or fragment", raw)
		}
		host := u.Hostname()
		if !strings.EqualFold(host, "localhost") && host != "127.0.0.1" && host != "::1" {
			t.Fatalf("accepted %q with non-allowlisted host %q", raw, host)
		}
	})
}

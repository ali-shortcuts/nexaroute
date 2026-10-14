# NexaRoute threat model (M3-4, in progress)

Each mitigation names the test that proves it. A row without a test is a gap,
not a claim.

## Provider egress / SSRF

| Threat | Mitigation | Test |
| --- | --- | --- |
| Provider URL targets loopback/RFC1918/ULA/link-local/metadata | `blocked()` category checks | `TestBlockedIPMatrix`, `TestValidateURLMatrix` |
| Provider URL targets special-purpose space (CGNAT 100.64/10 incl. 100.100.100.200, TEST-NETs, benchmarking, reserved, Teredo, site-local, `::/96`) | `blockedPrefixes` | `TestBlockedIPMatrix` |
| NAT64 / 6to4 form of a blocked IPv4 address | embedded IPv4 is judged, not the wrapper; legitimate NAT64 of public hosts still works | `TestBlockedIPMatrix` (nat64/6to4 rows) |
| IPv4-mapped IPv6 and IPv6 zones evade IPv4 checks | `Unmap()` and zone stripping before checks | `TestBlockedIPMatrix` |
| DNS rebinding / TOCTOU | dial only to the IP literal validated from one lookup; all answers must pass | `TestTransportDialRejectsBlockedLiterals`, `TestResolvedAnswersAreAllChecked` |
| Public name resolving to 127.0.0.1 when a provider uses `http://` | loopback is allowed only when the host is spelled loopback (`127.0.0.1`, `::1`, `localhost`) | `TestTransportRejectsPublicNameResolvingToLoopback`, `TestIsLoopbackHost` |
| API keys sent in cleartext to a non-loopback host | plain `http` is accepted only for explicit loopback hosts | `TestValidateURLMatrix` |
| Credentials/query/fragment smuggled in the base URL | `validateURL` rejects userinfo, query, fragment | `TestValidateURLMatrix` |
| Proxy URL carrying credentials/paths, or non-HTTP proxy schemes | proxy reduced to scheme+authority; only http/https accepted | `TestProxyConfigIsNormalised` |
| Cross-origin redirect leaks keys/headers | `CheckRedirect` refuses cross-origin (internal/providers) | existing providers tests |

## Provider header forwarding

| Threat | Mitigation | Test |
| --- | --- | --- |
| Client headers reach the provider outside the configured allow-list | `applyHeaders` forwards only names in `ForwardHeaders` (config entries are trimmed and case-folded) | `TestForwardHeadersAllowListEnforcement`, `TestForwardHeaderAllowlistDoesNotLeakClientAuth` |
| Client supplies or overwrites a credential header (`Authorization`, `X-API-Key`, `X-Goog-API-Key`, `X-Admin-Key`) through forwarding, even when the name is listed in `ForwardHeaders` | credential header names are unconditionally excluded from forwarding; statically configured provider headers and `applyAuthKey` injection (which runs last) are unaffected | `TestForwardedCredentialHeadersAlwaysBlocked`, `TestForwardedCredentialCannotOverwriteConfiguredHeader`, `TestGoogCredentialInjectionWinsOverForwardedValue` |

## Known gaps (no test yet)

- Destination validation when an explicit provider proxy is configured: the
  proxy resolves the final host, so the gateway cannot check it.
- Tool-definition and per-provider key isolation tests.
- Config-validation and privacy-path checks (see M3-1, M3-2).

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
| Client headers reach the provider outside the configured allow-list | `applyHeaders` forwards only names in `ForwardHeaders` (valid configured names are compared case-insensitively) | `TestForwardHeadersAllowListEnforcement` |
| Client supplies or overwrites a credential header (`Authorization`, `X-API-Key`, `X-Goog-API-Key`, `X-Admin-Key`) through forwarding, even when the name is listed in `ForwardHeaders` | credential header names are unconditionally excluded from forwarding; statically configured provider headers and `applyAuthKey` injection (which runs last) are unaffected | `TestForwardedCredentialHeadersAlwaysBlocked`, `TestForwardedCredentialCannotOverwriteConfiguredHeader`, `TestGoogCredentialInjectionWinsOverForwardedValue` |
| One provider's credential leaks into another provider's request, into a failover attempt, or into a client-facing error | credentials are selected per provider adapter from that provider's own configuration and re-derived per attempt; client-supplied credential headers are blocked at the gateway and adapter layers; client-facing error responses contain no provider keys, including when an upstream error body mentions its own provider's key | `TestProviderKeyIsolationAcrossAdapters`, `TestProviderCredentialRotationStaysWithinProvider`, `TestGatewayRoutesEachProviderOnlyItsOwnKey`, `TestGatewayFailoverDoesNotCrossProviderKeys`, `TestGatewayClientCredentialHeadersCannotContaminateProviders`, `TestGatewayErrorResponseContainsNoProviderKeys` |

## Tool definitions

| Threat | Mitigation | Test |
| --- | --- | --- |
| Client tool definitions lose schema meaning (nested properties, required fields, enums, array item schemas, bounds, patterns) in routing, canonical translation, or provider request construction | tool schemas are carried as structured data through the canonical layer and re-emitted per provider wire shape; tests decode the actual upstream JSON and compare every tool's name, description and complete schema semantically across the OpenAI Chat, Anthropic Messages and OpenAI Responses ingresses and the native provider paths | `TestToolSchemaIntegrityCanonicalIngresses`, `TestToolSchemaIntegrityNativePaths` |
| A canonical repair retry (dropping an unsupported field) mutates or drops client tool definitions | the retry removes exactly the unsupported field; the complete tool definitions survive unchanged | `TestToolSchemaIntegrityRepairRetry` |

## Configuration validation

| Threat | Mitigation | Test |
| --- | --- | --- |
| Header injection through provider header names or values (CRLF, whitespace) in static headers or the forward list | `Validate` enforces `validHeaderName`/`validHeaderValue` on static headers and `ForwardHeaders` entries | `TestValidateRejectsHeaderInjection`, `TestValidateRejectsInvalidHeaderNames` |
| Credentials embedded in a provider base URL leak through logs or copied configuration | `validateEndpointURL` rejects userinfo in base URLs; the validation error names only the field, never the credential | `TestValidateRejectsEmbeddedEndpointCredentials`, `TestValidateEndpointErrorNeverContainsEmbeddedSecret` |
| Invalid egress allow/deny configuration silently disables SSRF policy | egress hosts/CIDRs are validated at load; malformed CIDRs are rejected | `TestEgressConfigValidation` |
| Unsafe resource limits (concurrency, in-flight caps) exhaust the gateway | provider and global limits are range-checked in `Validate` | `TestValidateRejectsUnsafeResourceLimits`, `TestValidateRejectsUnsafeGlobalInflightLimit` |
| Mis-declared provider data-handling or privacy fields weaken privacy routing | data-handling and privacy fields are validated and normalized at load | `TestDataHandlingValidationTable`, `TestPrivacyFieldsValidationTable` |
| Configuration warnings leak secret values | warnings are constructed without secret material | `TestWarningsNeverContainSecretValues` |

## Privacy enforcement

| Threat | Mitigation | Test |
| --- | --- | --- |
| A `no_training` route profile or virtual key sends prompts to a training provider, including via stale session pins, failover, hedging, or any ingress/provider protocol | eligibility filters deployments before every attempt; excluded providers receive zero requests; when the satisfying provider's breaker opens, requests fail closed instead of falling back | `TestPrivacyP2PrivateRouteLeak`, `TestPrivacyP2PrivateVirtualKeyLeak`, `TestPrivacyP2EveryIngressAndProviderProtocolExcludesTrainingProvider`, `TestPrivacyP2ConcurrentLeakRace` |
| No deployment satisfies the required privacy class | generation ingresses return `privacy_unavailable` (503) without contacting any upstream and without naming providers; `count_tokens` answers with its local estimate and likewise contacts no upstream | `TestPrivacyFailClosedWhenNoDeploymentSatisfies` |
| Privacy failure responses or observability events disclose provider identities or prompt content | the `privacy_unavailable` envelope is generic and events record only the exclusion fact | `TestPrivacyP2PrivateRouteLeak` (response and event assertions), `TestDecision_PrivacyCanaryCompletePath` |

## Known gaps (no test yet)

- Destination validation when an explicit provider proxy is configured: the
  proxy resolves the final host, so the gateway cannot check it.

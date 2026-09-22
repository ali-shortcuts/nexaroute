# Reference review — 2026-09-21

This note records the parts of other gateways that were useful as design references, and the failure modes we explicitly avoided. It is not a claim that any referenced project is bug-free.

## Claude Code Router (CCR)

Useful patterns:
- Provider page centered around preset/custom endpoint, Base URL, credential, protocol, model discovery and connectivity check.
- Clear separation between management UI credentials and model-gateway client credentials.
- Provider-level advanced options and model connectivity checks.

Pitfalls deliberately avoided:
- A 2026 issue reported a UI/import path that persisted a placeholder Authorization value and overrode the real API key for OpenAI-compatible providers. ULG applies custom headers first and configured auth second, so an explicit API key wins unless `auth_mode=none`.
- A 2026 issue reported deselected provider protocols being silently re-added on edit. ULG v0.3 does not auto-merge hidden protocol capabilities during provider edit; the selected provider type is stored as the user's explicit value.
- A prior issue reported Claude Code launch not inheriting the gateway Base URL. ULG keeps client-launch integration outside the provider editor and documents the Base URL explicitly.

References:
- https://github.com/musistudio/claude-code-router/blob/main/docs/src/content/docs/en/configuration/providers.md
- https://github.com/musistudio/claude-code-router/issues/1562
- https://github.com/musistudio/claude-code-router/issues/1727
- https://github.com/musistudio/claude-code-router/issues/1137

## 9Router

Useful patterns:
- Provider-oriented dashboard and visible routing state.
- Add-provider workflow with API type, Base URL, key, and model setup.
- Model/fallback visualization inspired the ULG live model ring.
- Broad provider catalog demonstrates why a generic compatible-provider layer is preferable to hard-coding every brand.

Pitfalls deliberately avoided:
- Open issues show friction around custom providers, provider-account persistence, incomplete Base URL/API-type visibility, and orphaned model aliases after provider deletion.
- ULG stores provider/model ownership directly in one config graph. Deleting a provider removes those model deployments from the active router immediately and does not maintain a separate alias table that can become orphaned.

References:
- https://github.com/decolua/9router
- https://github.com/decolua/9router/issues/2504
- https://github.com/decolua/9router/issues/1409
- https://github.com/decolua/9router/issues/994

## Mukller/claude-code-gateway

Useful patterns:
- Pure-Go gateway architecture.
- Simultaneous Anthropic and OpenAI ingress.
- Key rotation, fallback-chain, circuit-breaker, model discovery, dashboard and hot reload as mature target capabilities.
- Clear separation of provider execution, protocol translation, routing and observability.

ULG v0.3 does not copy its implementation. The project is used as a benchmark for features we should eventually match or exceed. Several of those features remain on our roadmap, especially key pools, budgets, exact token counting, broader provider types, metrics and persistent request history.

Reference:
- https://github.com/Mukller/claude-code-gateway

## Claude Code gateway behavior

Claude Code itself has had gateway-specific edge cases around model discovery and provider-mode resolution. ULG therefore treats Claude Code compatibility as a conformance target, not a one-time HTTP-shape conversion.

References:
- https://github.com/anthropics/claude-code/issues/56675
- https://github.com/anthropics/claude-code/issues/61112
- https://github.com/anthropics/claude-code/issues/84583
- https://github.com/anthropics/claude-code/issues/77247

## Design conclusion for ULG

The provider editor should behave predictably:
1. Saved Base URL remains visible on edit.
2. Saved provider type remains the selected type.
3. Saved credential source is preserved on unrelated edits.
4. A privileged/local edit view may reveal the resolved key in a password field with explicit Show/Hide.
5. Model detection never removes manual-entry capability.
6. Connection test happens before save when the user wants it, but save does not secretly rewrite auth.
7. Provider updates hot-reload the active registry/router without process restart.
8. Config writes are validated, atomic, and have one last-known-good rollback copy.

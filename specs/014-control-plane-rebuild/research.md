# R2 Research Summary

The detailed comparison is in [`research/router-ux-comparison.md`](research/router-ux-comparison.md), with one report per product. The research covered Claude Code Router, LiteLLM Proxy, Portkey Gateway, OpenRouter, Helicone AI Gateway, and Bifrost.

## Decisions for NexaRoute

Adopt progressive provider onboarding, server-owned presets, explicit protocol/model discovery, scoped real tests, stable public model identity, visible fallback order, separate credential classes, actionable activity traces, recovery-oriented empty/error states, and large-list search/filter patterns.

Do not copy any brand identity, proprietary code, exact schemas, opaque reliability badges, raw-config-first workflows, credential conflation, prompt logging by default, or unverified claims about keyboard/responsive behavior. Accessibility and responsive behavior remain NexaRoute acceptance criteria to implement and test directly.

The strongest product-specific fit is the combination of CCR’s local gateway diagnostics, LiteLLM’s source-of-truth and layered health separation, Portkey’s progressive configuration, OpenRouter’s stable route/version concepts, Helicone’s explicit provider routing, and Bifrost’s multidimensional operational state. NexaRoute keeps its own single-binary/local/data-plane constraints and backend-owned routing semantics.

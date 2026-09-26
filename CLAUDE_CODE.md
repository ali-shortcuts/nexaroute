# Claude Code

NexaRoute exposes an Anthropic Messages API. Keep Claude Code pointed at the stable NexaRoute URL/model; configure physical provider deployments and fallback pools in NexaRoute, not in Claude Code.

1. In the NexaRoute dashboard, add and test providers/models.
2. Create a virtual endpoint (public model name) backed by an enabled candidate pool and fallback chain, then enable it.
3. Configure Claude Code to use NexaRoute's local Anthropic-compatible base URL (`http://127.0.0.1:8080`) and the virtual endpoint's public model name. If admin authentication is enabled, use the NexaRoute client API credential configured for data-plane access.

The gateway's default listener is loopback-only for local use. For exact environment-variable/CLI configuration supported by your Claude Code version, consult its current Anthropic-compatible provider settings; do not place upstream provider keys in Claude Code. Keep the NexaRoute admin UI local or behind a trusted access boundary.

NexaRoute's cross-protocol translation is intentionally capability-filtered. Provider-specific reasoning formats and some multimodal/beta fields are not guaranteed to translate losslessly; see [compatibility limits](docs/KNOWN_GAPS.md).

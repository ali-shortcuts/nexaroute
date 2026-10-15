# M2-8 — Playground with request trace, privacy exclusions, tokens/cost

- Form: protocol (OpenAI chat / Anthropic messages), public
  model (from the configured virtual endpoints), system prompt,
  prompt, temperature, max tokens, streaming toggle, and a
  client key field kept in sessionStorage only (local-trust
  mode needs none).
- Privacy exclusions preview: for the selected route, the page
  computes from the admin snapshot which deployments are
  currently excluded and why (not in the profile pool, unhealthy
  or in cooldown, or trains-on-data under a no_training
  profile). Enforcement stays server-side; the preview is the
  operator's view of the same data, and the response trace
  shows where the request actually went.
- Trace: HTTP status, total latency, time to first byte (stream
  mode), the X-Gateway-* routing headers (deployment, provider,
  upstream model, virtual endpoint, route profile, cache),
  token usage from the response, and estimated cost priced from
  the served deployment's configured model prices ("unpriced"
  when none are configured — never invented).
- Streaming renders deltas live for both dialects and reads
  usage from the stream's terminal events.

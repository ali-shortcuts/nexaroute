# M4-6 part B — opt-in OTLP/HTTP span export with GenAI attributes

## Scope

- `internal/telemetry`: a dependency-free OTLP/HTTP JSON span
  exporter. Bounded in-memory queue, batch flush (interval or batch
  size), non-blocking Export (drops + counts when the queue is full or
  the exporter is closed), final flush on Close. Export failures are
  counted, never propagated into request handling.
- Spans are produced for the four data-plane LLM endpoints only
  (/v1/messages, /v1/messages/count_tokens, /v1/chat/completions,
  /v1/responses), reusing the gateway's W3C trace context (incoming
  traceparent is honored: same trace id, parent span id linked).
- GenAI semantic attributes: gen_ai.operation.name, gen_ai.system
  (provider, from the serving deployment), gen_ai.response.model,
  gen_ai.usage.input_tokens / output_tokens, plus http status,
  nexaroute.deployment and nexaroute.ttft_ms when observed. NEVER
  prompts, completions, keys, or client identities.
- Serving deployment/tokens are noted by the existing recordUsage
  choke point; TTFT by the existing observeFirstByte callbacks — no
  new per-path instrumentation logic.
- Configuration mirrors the M4-1 secret pattern: the collector URL is
  read only from the environment variable NAMED by
  telemetry.otlp_endpoint_env; unset/empty = exporter disabled
  (zero-service default unchanged). An invalid URL fails startup
  (fail-closed, like the database backends). telemetry.service_name
  sets the OTLP resource service.name (default "nexaroute").

## Verification boundary

Unit/integration tests verify the OTLP payload against a fake
collector (httptest). A real collector (otelcol/Jaeger/Tempo)
end-to-end ingestion is UNVERIFIED and will be stated as such.

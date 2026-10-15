# M4-6 — Observability completion

## Audit findings (2026-10-15, this branch's base)

- Prometheus exposition is hand-rendered in internal/httpapi/metrics.go
  (~60 series). All label values are config- or enum-derived
  (deployment, provider, status, error_type, event kind) and pass
  through sanitizeMetricLabel; cardinality is bounded by configuration.
  No user/key/request-scoped labels exist.
- Latency visibility is EWMA gauges only — there are NO histograms
  anywhere, so no quantiles are computable. TTFT exists only as an
  EWMA gauge fed by observeFirstByte on all three proxy paths.
- Inter-token latency is NOT observable: token counts are recorded by
  the usage path and timings by the health path; no single observation
  point joins (ttft, duration, output tokens) per request. This spec
  does not fabricate it; ITL stays a documented non-goal until the
  streaming pipeline joins those signals.
- OpenTelemetry: not present (no OTel SDK, no OTLP export). Tracing is
  W3C traceparent parsing + trace ids in logs. GenAI semantic
  attributes do not exist yet.

## Part A (this PR) — histograms + scrape-time assets

- health.Manager gains per-deployment fixed-bucket histograms fed by
  the SAME samples as the EWMA gauges: RecordSuccess/RecordFailure
  latencies → nexaroute_deployment_request_latency_ms, RecordTTFT →
  nexaroute_deployment_ttft_ms. Buckets (ms): 5..64000 ×2 steps +Inf.
  Prometheus histogram rendering (cumulative _bucket/_sum/_count) with
  the existing {deployment,provider} labels.
- deploy/observability/: a Grafana dashboard JSON and Prometheus alert
  rules that reference ONLY series the gateway actually exports; a
  contract test renders /metrics and asserts every referenced
  nexaroute_* name exists in the exposition, and that the rendered
  label names stay within the audited allowlist (no request-scoped
  labels can creep in unnoticed).
- Histogram unit tests: bucket placement incl. boundaries, cumulative
  counts, sum, and concurrent observation under -race.

## Part B (follow-up PR) — OTLP spans

Opt-in OTLP/HTTP JSON span export (endpoint only via a named env var,
mirroring the M4-1 secret pattern), GenAI semantic attributes,
verified against a fake collector in tests. A real collector
(otelcol/Jaeger) end-to-end check stays UNVERIFIED.

# Observability

## Prometheus metrics

`GET /metrics` renders the Prometheus text exposition (no client
library; the handler is `internal/httpapi/metrics.go`). Roughly sixty
series cover requests/inflight/admission, probe and recovery state,
deployments and health, per-provider concurrency and quota headroom,
runtime events and normalized errors, decision-plane and
evaluation-plane counters, usage and estimated cost.

### Latency and TTFT histograms (M4-6)

EWMA gauges alone cannot produce quantiles, so the health pipeline
also keeps fixed-bucket histograms fed by **the same samples** as the
EWMA latency/TTFT gauges:

- `nexaroute_deployment_request_latency_ms` — per-attempt latency for
  every data-plane attempt (successes and failures), by
  `{deployment, provider}`.
- `nexaroute_deployment_ttft_ms` — streaming time-to-first-byte by
  `{deployment, provider}`.

Buckets (ms): 5, 10, 25, 50, 100, 250, 500, 1000, 2000, 4000, 8000,
16000, 32000, 64000, +Inf. Example:
`histogram_quantile(0.95, sum by (deployment, le) (rate(nexaroute_deployment_ttft_ms_bucket[10m])))`.

Inter-token latency is deliberately **not** exported: token counts are
recorded by the usage path and timings by the health path, and no
observation point joins them per request, so any ITL number would be
fabricated. If the streaming pipeline grows a joined observation, this
section is where its metric will be documented.

### Labels, cardinality, privacy

Every label in the exposition is config- or enum-derived
(`deployment`, `provider`, `status`, `kind`, `error_type`, task and
evaluation taxonomies, `le`). There are **no** per-request labels: no
client keys, users, IPs, models-as-requested, prompts or request ids —
series cardinality is bounded by the operator's configuration, and a
contract test (`TestMetricsLabelNamesStayAudited`) fails if a new
label name appears without an audit. Label values pass through a
sanitizer before rendering.

### Dashboards and alerts

`deploy/observability/` ships a Grafana dashboard
(`grafana-dashboard.json`) and Prometheus alert rules
(`prometheus-alerts.yaml`: no healthy deployments, per-deployment
failure rate, overload rejections, p95 TTFT/latency). A contract test
(`TestObservabilityAssetsReferenceRealSeries`) renders `/metrics` and
fails if either asset references a series the gateway does not export.

## Tracing and OpenTelemetry

Requests carry W3C `traceparent` propagation; trace/span ids are
attached to gateway logs. A full OpenTelemetry pipeline is **not**
bundled yet: the follow-up M4-6 work adds an opt-in OTLP/HTTP span
exporter with GenAI semantic attributes (model, token usage, error
class — never prompts or keys). Until that lands and is verified
against a real collector, treat end-to-end OTel as **unverified** and
rely on metrics + structured logs for operations.

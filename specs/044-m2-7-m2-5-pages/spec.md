# M2-7 + M2-5 — Evals/Scorecards page and Observability page

Both backends exist (scorecards API with provenance-carrying
values; the enriched per-deployment health snapshot; the
Prometheus /metrics endpoint with M4-6 histogram series). This
unit adds the two pages:

- Evals: lazy-loads /admin/api/scorecards, renders one card per
  deployment with every dimension (score bar, raw value + unit,
  sample count, confidence, provenance), generation time and
  notes, plus recent evaluation runs and a run form
  (deployment + suite id -> POST /admin/api/evaluation/run;
  a disabled plane surfaces the server's own 409 message).
- Observability: per-deployment table from the admin health
  snapshot (status, circuit, average latency, TTFT, error rate,
  success/failure counts, last success/failure), a link to the
  gateway's own /metrics, and operator-configured Prometheus /
  Grafana base URLs (stored in localStorage, opened in a new
  tab). The page names the M4-6 histogram series so operators
  know what to graph; it does not fabricate charts from data the
  snapshot does not carry.

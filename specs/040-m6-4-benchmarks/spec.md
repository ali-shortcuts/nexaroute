# M6-4 — Reproducible benchmarks, profiles, and a measured hotspot fix

- scripts/bench.sh: one reproducible entry point. Fixed
  -benchtime/-count, -benchmem, environment capture (Go version,
  CPU model, GOMAXPROCS), and two profiles: `default` (stock
  runtime) and `tuned` (GOGC=200 + GOMEMLIMIT set + explicit
  GOMAXPROCS) so operators can compare on their own hardware.
  Results land in benchmarks/results/<name>-<profile>.txt.
- New hot-path benchmarks where none existed: router candidate
  scoring at 10/100 deployments, canonical codec round trips
  (anthropic + openai), exact-cache key/put/get, guardrail Scan
  on a realistic body. Existing suites (decision/policy,
  httpapi gateway-vs-direct) are part of the curated set.
- Profiles: CPU + memory profiles from the gateway end-to-end
  benchmark identify the top hotspot; ONE hotspot fix lands with
  before/after numbers measured by the same harness (medians of
  the fixed-count runs), never by assertion.
- docs/BENCHMARKS.md: methodology, the environment of record,
  result tables for both profiles, the hotspot analysis + fix,
  and reproduction steps. Numbers are this machine's; the
  harness is the deliverable that travels.

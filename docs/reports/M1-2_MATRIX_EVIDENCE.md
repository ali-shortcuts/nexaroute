# M1-2 — competitor capability matrix evidence

**State:** DONE pending the PR verification gate. The matrix has been re-audited against official, capability-specific deep links. Generic product landing pages and undocumented inferences remain `unverified`; that label does not imply the capability is absent.

## Corrected matrix scope and counts

The matrix contains **17** distinct competitor capability rows, not the 16 claimed by its previous count table. The count table has been corrected without dropping a row. The resulting 85 cells classify as 55 verified and 30 unverified.

| Competitor | Verified | Unverified | Total |
|---|---:|---:|---:|
| LiteLLM | 11 | 6 | 17 |
| Portkey AI Gateway | 10 | 7 | 17 |
| Kong AI Gateway | 12 | 5 | 17 |
| Bifrost | 15 | 2 | 17 |
| Envoy AI Gateway (now Agent Router) | 7 | 10 | 17 |
| **Total** | **55** | **30** | **85** |

Every verified matrix cell links directly to an official feature page, documentation page, or the relevant project release. The descriptions identify the claim supported by that page. Examples include virtual-key and cache behavior for LiteLLM/Portkey, cost routing and mTLS for Kong, cluster state and asynchronous video for Bifrost, and MCP/Prometheus/OpenTelemetry support for Agent Router.

The Agent Router documentation itself identifies Envoy AI Gateway as the former project name and says the API/CRD/CLI names remain unchanged. The matrix therefore preserves the program's requested competitor name while clarifying the current official name.

Bifrost's observability cell is supported by two official deep links: its Prometheus page documents pull and push metrics; its OpenTelemetry page documents OTLP traces and GenAI semantic conventions. The encrypted-at-rest cell notes the documented requirement to set `BIFROST_ENCRYPTION_KEY`.

The complete per-cell sources and statuses are in [`docs/CAPABILITY_MATRIX.md`](../CAPABILITY_MATRIX.md). No unsourced competitor-absence claims were added.

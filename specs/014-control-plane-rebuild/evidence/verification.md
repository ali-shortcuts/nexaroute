# Verification Evidence

## Completed

| Gate | Result | Evidence |
|---|---|---|
| Go unit/integration suite | PASS | `go test ./...` |
| HTTP API race suite | PASS | `go test -race ./internal/httpapi` |
| Go vet | PASS | `go vet ./...` |
| Embedded binary build | PASS | `CGO_ENABLED=0 go build -o /tmp/nexaroute-v014 ./cmd/gateway` |
| Frontend syntax | PASS | `node --check internal/httpapi/web/app.js` |
| Diff hygiene | PASS | `git diff --check` |
| Local UI smoke | PASS | `http://127.0.0.1:19090/` served `NexaRoute`, `/styles.css`, `/app.js`, and five `data-page` items |
| Local authenticated admin smoke | PASS | Snapshot returned 28 fields; 0 deployments, 0 events and 0 providers in empty config; provider response had no `api_key`, `secret_value` or `password` values |
| Public UI smoke | PASS | `https://19090-i2dvphx2tj4rke8kcqfw0-fbb19148.us4.manus.computer/` served the new UI |

## Limitation

The sandbox public edge served the page but did not forward the custom `x-admin-key` header to admin API requests; public API checks therefore returned 401/429. The local authenticated API path passed and is the authoritative backend smoke result. Browser E2E screenshots and human visual acceptance remain required before merge/release.

## Not claimed

No provider was configured in the smoke environment, so no paid upstream request, model discovery, provider test, route request, or real SSE event was generated. No release or publication was performed.

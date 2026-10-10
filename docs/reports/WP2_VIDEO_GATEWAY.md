# WP2 — video gateway integration

## Scope and conflict resolution

Integrated `chore/video-gateway-audit-hardening` onto the WP1 main line as
`wp2-video-gateway`. The merge had real conflicts in `cmd/gateway/main.go` and
`internal/httpapi/web/app.js`. The final files retain both feature sets:

- encrypted config loading, TLS/mTLS listener setup, and the existing gateway
  shutdown/readiness behavior;
- browser CSRF token handling and same-origin credentials in the Admin UI;
- opt-in video runtime creation, bounded workers, `/v1/video/` routes, and the
  Video Studio UI.

`video.enabled=false` remains the default, so the normal LLM data plane is
unchanged when video is not enabled.

## Integration evidence

`cmd/gateway/config_integration_test.go` now writes an encrypted config, enables
TLS and the development fake video provider, starts the real gateway, verifies
`/healthz` over HTTPS, and verifies `/v1/video/providers` returns the fake
provider over the same TLS listener. The test never uses a real provider and
makes no production-provider claim.

## Security and operational boundaries

The video handler supports constant-time bearer-token comparison when
`video.auth_token_env` is configured. The local JSON job store is atomic but
single-process. The fake provider is local development/test only; no real
external adapter, multi-node store, or production video CLI is claimed verified.

## Gate result

Full output: [`wp2-video-gateway-gates.raw.txt`](wp2-video-gateway-gates.raw.txt)

- `gofmt -l .` — empty
- `go vet ./...` — PASS
- `go test -race -count=1 ./...` — PASS
- `./scripts/verify.sh` — `VERIFY PASS`; browser and Live Visual Agent E2E PASS
- `./scripts/smoke-local.sh` — `SMOKE PASS`
- release build — gateway/videogen amd64 and arm64 artifacts valid
- `./scripts/test-install.sh` — `INSTALL PASS`, installer E2E PASS
- coverage on this pre-Phase-2 feature line — **78.2%**; new video packages are
  not yet uniformly covered and are included in the post-Phase-2 WP3 target
- `git diff --check` — PASS

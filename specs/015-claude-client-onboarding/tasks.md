# Tasks and verification

- [x] Add validated persisted `client_base_url` with environment precedence.
- [x] Add config regression tests for URL rules and durable/environment separation.
- [x] Add admin snapshot/settings contract for public URL metadata.
- [x] Update Connect snippets and endpoint labels.
- [x] Preserve route editor mode/order and hidden retry policy.
- [x] Align Claude Code docs and sample config.
- [x] Run `gofmt` and `node --check`.
- [x] Run focused Go tests.
- [x] Run full `go test ./...`, `go vet ./...`, and race suite.
- [x] Browser E2E and Live Visual Agent E2E on the embedded gateway.
- [x] Run official fuzz checks and Linux amd64/arm64 builds through `scripts/verify.sh`.

## Requirement matrix

| Requirement | Implementation | Test/command | Result |
| --- | --- | --- | --- |
| URL is optional and backward-compatible | `internal/config/config.go` | `go test ./internal/config` | PASS |
| URL validation and HTTP restriction | `ValidateClientBaseURL` | `TestValidateClientBaseURL` | PASS |
| Env wins but is not persisted | `ApplyEnvOverrides`, `SaveAtomic` | `TestClientBaseURLEnvPrecedenceAndDurablePersistence` | PASS |
| Public URL is secret-free in admin state | `adminSnapshot`, `settings.go` | focused `internal/httpapi` tests | PASS |
| Exact Anthropic routes are shown | `web/app.js`, `docs/CLAUDE_CODE.md` | `node --check`, source inspection | PASS |
| Route editor restores ordered state | `web/app.js` | route regression suite | PASS |
| Full backend/race/browser gates | CI and `scripts/verify.sh` | final verification | PASS |

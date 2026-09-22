# Verification report — v0.3

Date: 2026-09-22

This report describes the exact source tree packaged as the current v0.3 validation build. No real external provider credentials were used during verification.


## 2026-09-22 smart-routing bug audit

The current v0.3 tree was re-opened from the ZIP and audited against the requested routing behavior instead of trusting the prior report.

Bugs/weaknesses found and fixed without changing the version number:

- **Provider concurrency lifetime:** the semaphore previously released when HTTP response headers arrived. Long response bodies/SSE could therefore exceed `max_concurrency`. It is now held until the body reaches EOF or is closed.
- **Round-robin health promotion:** plain round-robin previously rotated the entire candidate list and could eventually put a degraded model before healthy models. Rotation is now restricted to the best available health band.
- **Streaming latency scoring:** successful requests previously learned latency from the entire completion duration. Long answers could look like a slow provider. Routing health now learns successful response-header latency while operational events may still record total request duration.
- **Probe scheduling:** concurrent goroutine scheduling could ignore intended probe priority. Probe work is now submitted through a bounded priority queue: half-open -> unknown -> degraded -> healthy, stalest first.
- **Requested breaker policy:** the default is now **5 consecutive failures -> 3600-second cooldown**. Expired deployments re-enter half-open and a half-open failure re-cools immediately.

New regression coverage was added for each of the routing/concurrency issues above.

Verification after these fixes:

- `go test ./...` -> PASS
- `go vet ./...` -> PASS
- `go test -race -count=1 -timeout=90s ./...` -> PASS
- routing/health/probe/provider/http packages repeated 50 times -> PASS
- JavaScript syntax -> PASS
- HTTP/JSON fuzz check -> PASS
- Anthropic content parser fuzz check -> PASS
- Linux amd64 static build -> PASS
- Linux arm64 static build -> PASS
- `./scripts/smoke-local.sh` -> PASS

The full `verify.sh` run passed all deterministic/test/race/first-fuzz stages; the container command itself hit its outer execution timeout during the second short fuzz stage, so that second fuzz stage was immediately rerun separately and passed. This is recorded explicitly rather than hidden.

## Toolchain

```text
go version go1.23.2 linux/amd64
```

## Full verification script

Executed:

```bash
./scripts/verify.sh
```

The script performs:

```text
gofmt cleanliness check
go test -shuffle=on -count=10 ./...
go vet ./...
go test -race -shuffle=on -count=3 ./...
node --check internal/httpapi/web/app.js
2-second fuzz run: HTTP/JSON patch path
2-second fuzz run: Anthropic content parser
static Linux amd64 build
static Linux arm64 build
```

Result: **VERIFY PASS**.

The two fuzz checks executed thousands of inputs without a crash or test failure during this verification run.

## Test coverage snapshot

A separate `go test -cover ./...` run was also executed. The most routing-critical package reached 73.3% statement coverage; tested HTTP/provider/health packages were roughly in the 48–57% range. `cmd/gateway` and the event bus currently have no direct package tests, so overall project coverage should not be interpreted from a single aggregate percentage.

Coverage is a diagnostic, not a claim of bug-freedom.

## Runtime smoke test

A clean configuration was started on loopback with background probing disabled and no external provider credentials.

Verified against the real built Linux binary:

- `GET /healthz` -> 200
- embedded Web UI `/` -> 200
- `GET /api/hello` -> 200 and reports version `0.3`
- `GET /admin/api/snapshot` -> 200
- `GET /admin/api/providers` -> 200
- `GET /v1/models` -> 200
- `POST /v1/messages/count_tokens` -> 200 with a clearly marked local estimate when no native provider is available
- readiness correctly returns 503 when there are deliberately no enabled deployments

A reusable local smoke script is included:

```bash
./scripts/smoke-local.sh
```

## Provider-edit persistence regression

The running gateway was exercised through its Admin API:

1. Create a disabled OpenAI-compatible provider with a literal API secret.
2. Re-open it with authorized secret reveal.
3. Verify Base URL and secret are present.
4. Change only the provider name and save with `preserve_secret=true`.
5. Re-open it.
6. Verify the Base URL is unchanged and the original secret is still present.
7. Read the provider without reveal and verify the secret is redacted while `has_secret=true` remains accurate.
8. Delete the provider.

Result: **PASS**.

This specifically protects the Web UI behavior requested for editing an already-configured provider.

## Installer check

The current CI executes `install-user.sh` against an isolated temporary HOME after building the Linux binaries.

It verifies:

- `~/.local/bin/nexaroute` is installed and executable
- `~/.config/nexaroute/config.json` is created
- config file mode is `0600`
- `nexaroute -version` reports `NexaRoute v0.3`

## Regression suites present in source

The Go tests include coverage for:

- 20 providers / 100 models
- 100-model concurrent probe pass
- routing strategy rotation and health filtering
- cooldown, half-open recovery and immediate re-cooldown
- credential-pool failover and bad-key cooldown
- provider concurrency limits and context cancellation
- stale Authorization-header precedence protection
- safe forwarded-header allowlist behavior
- native Anthropic unknown-field/beta-header passthrough
- native OpenAI unknown-field passthrough
- Anthropic count-token upstream path
- parallel streamed tool calls
- Claude-Code-like two-turn tool round trip through an OpenAI-compatible provider
- Anthropic error envelopes
- failover after upstream 401
- hot reload while concurrent requests are active
- provider edit preserving secrets and advanced settings
- native SSE flushing
- common model-list response shapes

## Release artifacts and integrity

Source control does not keep stale binary checksums. Tagged releases build fresh Linux amd64/arm64 binaries and generate `dist/SHA256SUMS` from those exact artifacts before publishing them to GitHub Releases.

## What this report does not prove

No finite test suite can honestly guarantee zero bugs, and no external provider account was contacted in this environment. The strongest remaining validation is the user's real Ubuntu path:

```text
Claude Code -> v0.3 -> Chat2API -> selected real provider/model
```

Native OpenAI Responses, Gemini native `generateContent`, Bedrock/Vertex/Azure-specialized semantics, distributed state, encrypted-at-rest vault integration, and full public-internet control-plane hardening are deliberately outside the current v0.3 protocol scope. They are documented in `docs/KNOWN_GAPS.md` rather than silently claimed as complete.

## Repository-hardening verification — 2026-09-22

The same v0.3 source tree was prepared for `github.com/ali-shortcuts/nexaroute` and reverified after the module/import-path change and GitHub CI additions.

Verified locally:
- repeated shuffled Go tests: PASS
- `go vet ./...`: PASS
- race detector: PASS
- JavaScript syntax check: PASS when Node is available
- short HTTP/API fuzzing: PASS
- short core protocol fuzzing: PASS
- Linux amd64 build: PASS
- Linux arm64 build: PASS
- `scripts/smoke-local.sh`: PASS
- repository source scan contains no `ghp_...` token pattern: PASS

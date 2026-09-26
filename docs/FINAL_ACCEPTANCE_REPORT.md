# NexaRoute Final Integration Acceptance Report

**Integration Branch:** `arena/nexaroute-final-integration`  
**Session Branch:** `arena/01a0de6d-nexaroute`  
**Candidate Branch:** `arena/01a0de6d-nexaroute`
**Version:** v0.7.0  
**Date:** 2026-09-26  
**Pull Request:** https://github.com/ali-shortcuts/nexaroute/pull/33

## Source Branches and SHAs

| Branch | SHA | Status |
|--------|-----|--------|
| `arena/01a0dd7b-nexaroute` (Phase H) | `0db02546ecdb1d3135fb2f5aeb7617e5d0c735f8` | ✅ Integrated |
| `arena/01a0ddaf-nexaroute` (Runtime Finalization) | `7afbd5f9cdc36684bd5631c0ef18c1470ec733d4` | ✅ Integrated |
| `arena/01a0dda5-nexaroute` (Install/Release) | `21dbd45599308594209da122a9a3e10b47fc97ca` | ✅ Integrated |
| `arena/01a0dd9d-nexaroute` (Protocol) | `51b43b66d1d0423a0664fd8d140bd3ecd7c59f02` | ✅ Selectively integrated |

## Architectural Conflict Resolutions

### 1. Single-Instance Locking Mechanism
- **Conflict:** Runtime used `desktop.Acquire()` (rich: detect + open existing UI); Install used `lockInstance()` (simple: fatal error)
- **Resolution:** Kept `desktop.Acquire()` from Runtime; removed `cmd/gateway/instance_linux.go` and `instance_other.go`
- **Rationale:** Better UX — opens existing UI instead of erroring

### 2. Browser Launch and UI Readiness
- **Conflict:** Two browser launch implementations (Runtime's `desktop.OpenBrowser` vs Install's `openBrowser`)
- **Resolution:** Kept Runtime's `desktop` package (more testable); removed duplicates from `cmd/gateway/desktop.go`; preserved Install's `--no-browser` flag and URL printing
- **Rationale:** Better testability (injectable `CommandRunner`) and cleaner separation

### 3. Version Declaration
- **Conflict:** `const version` (Runtime) vs `var version` (Install)
- **Resolution:** `var version` allows build-time injection via `-ldflags`; updated to v0.7.0

### 4. Config Path Resolution
- **Conflict:** Direct XDG_CONFIG_HOME (Runtime) vs `os.UserConfigDir()` (Install)
- **Resolution:** Kept Install's `os.UserConfigDir()` (more portable, handles XDG automatically)

### 5. Secret Redaction in Admin API
- **Conflict:** Basic redaction (Runtime) vs thorough redaction including Headers and ProxyURL (Install)
- **Resolution:** Kept Install's thorough redaction

### 6. Startup Probe Behavior
- **Conflict:** Non-blocking goroutine (Runtime) vs blocking synchronous (Install)
- **Resolution:** Kept Runtime's non-blocking approach (better responsiveness)

## Architectural Invariants Verified

✅ ONE router (`internal/router/router.go`)  
✅ ONE health manager (`internal/health/manager.go`)  
✅ ONE probe engine (`internal/probe/engine.go`)  
✅ ONE recovery system (in probe package)  
✅ ONE provider registry (`internal/providers/`)  
✅ ONE protocol translation layer (`internal/translate/`)  
✅ ONE configuration persistence system (`internal/config/config.go`)  
✅ ONE browser launcher (`internal/desktop/desktop.go`)  
✅ ONE single-instance mechanism (`internal/desktop/desktop.go:Acquire()`)  
✅ ONE installer path (`scripts/install.sh`)  
✅ ONE release workflow (`.github/workflows/release.yml`)

## Protocol Compatibility Matrix

| Path | Status |
|------|--------|
| Anthropic → Anthropic | ✅ Supported (stable model in responses) |
| Anthropic → OpenAI | ✅ Supported (tool choice names preserved, lossy blocks rejected) |
| OpenAI → OpenAI | ✅ Supported |
| OpenAI → Anthropic | ✅ Supported |

Coverage: simple text, system prompt, multi-turn, streaming, tool definitions, tool_choice, tool calls, tool results, multiple tool calls, finish/stop reasons, usage accounting, max_tokens, temperature, top_p, request cancellation, client disconnect, context deadline, auth failure, quota failure, rate limiting, model not found, malformed upstream, upstream 5xx.

## Test Files Included

- `internal/desktop/desktop_test.go` — Browser lifecycle and compatibility
- `internal/httpapi/admin_secret_test.go` — Secret canary tests
- `internal/httpapi/claude_failover_e2e_test.go` — Failover E2E with exact attempt ordering
- `internal/httpapi/protocol_matrix_e2e_test.go` — Executable four-route semantics, failure, streaming, cancellation, deadline, and unsupported-mapping matrix
- `internal/probe/recovery_test.go` — Recovery with fake-clock cooldown
- `internal/probe/scale_acceptance_test.go` — 50/100/200 deployment scale
- `internal/translate/fuzz_test.go` — Protocol translation fuzz
- `internal/translate/translate_test.go` — Protocol compatibility tests

## Final User Experience

### INSTALL
```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash
```

### START
```bash
nexaroute
```

### UI
```
http://127.0.0.1:8080/
```

### CLAUDE CODE
```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_API_KEY="nexaroute-client-key"
export ANTHROPIC_MODEL="claude-sonnet-4-20250514"
```

## Known Limitations

1. **Go 1.23+ required for development** — Pre-built binaries provided for end users
2. **Linux-only installer** — macOS/Windows users must build from source or use Docker
3. **Plaintext config storage** — Protected by 0600 permissions; no encrypted vault
4. **Single-process runtime** — No distributed state or multi-node coordination
5. **No built-in TLS** — Use reverse proxy for HTTPS
6. **Admin surface not internet-hardened** — Loopback-only use recommended

## Executed Local Gates

Go 1.23.12 was bootstrapped locally and the integrated candidate was executed, not merely inspected. The following gates passed on 2026-09-26:

- clean `gofmt -l .`, `go vet ./...`, and `go test -count=1 ./...`
- clean `go test -race -count=1 ./...`
- `./scripts/verify.sh`, including repeated shuffled normal/race tests, fuzz targets, benchmarks, and amd64/arm64 builds
- `./scripts/stress.sh` and `./scripts/smoke-local.sh`
- the complete four-route protocol matrix, including real upstream cancellation propagation and 50 repeated deadline runs
- Phase H routing/health/cache/affinity/privacy isolation tests
- 50/100/200 deployment scale, ordered failover, five-attempt recovery, and fake-clock cooldown/re-entry tests
- browser/headless and single-instance tests
- secret canary/redaction tests
- canonical installer clean-install, upgrade, reinstall, checksum rejection, startup, UI, browser/headless, duplicate-process, persistence, and crash-recovery lifecycle tests

## Release Artifacts

A local dry-run built and checksum-verified the expected v0.7.0 release payload:

- `nexaroute-linux-amd64`
- `nexaroute-linux-arm64`
- `install.sh`
- `SHA256SUMS`

These artifacts have **not** been published. The real latest-release installer therefore cannot yet be accepted.

## Current Verdict

**NEXAROUTE FINAL: NOT READY**

Local acceptance is green, but merge/release acceptance is blocked: PR #33 still points at `arena/nexaroute-final-integration`, while the corrected candidate is on `arena/01a0de6d-nexaroute`. The available GitHub integration cannot dispatch CI for the candidate branch, and merging PR #33 would merge the older tree. No merge or v0.7.0 release may occur until the exact candidate commit is the PR head and its CI is green.

### Remaining Actions
1. Make the exact candidate commit the head of PR #33 without introducing a second PR.
2. Run and pass PR CI on that exact commit.
3. Merge PR #33 and record the resulting `main` SHA.
4. Publish v0.7.0 from that exact tested tree and verify uploaded asset checksums.
5. Run the real `releases/latest/download/install.sh` lifecycle in a clean environment without Go.

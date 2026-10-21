# Config.Validate architecture slice 1: video validation

- Parent audit: #593
- Slice issue: #606
- Baseline `main` SHA: `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d`
- Branch: `opencode/issue606-20261021154033`
- Implementation commit: `f20f447` (`refactor(config): extract video validation from Config.Validate`)
- Scope: one `Config.Validate` domain only. No schema, API, configuration, or error-contract change.

## Domain selection

The slice issue preferred TLS, proxy, or egress validation when the current structure supports a clean boundary. Those areas already have dedicated helpers and are not good candidates for this slice:

- `TLSConfig.Validate` (`internal/config/config.go:488`) already owns TLS and trusted-proxy validation.
- `EgressConfig.validate` (`internal/config/config.go:3338`) already owns global egress CIDR validation.
- `Config.proxyEgressViolation` (`internal/config/config.go:3381`) owns provider proxy egress checks, but it is called from the per-provider loop in the middle of a much larger validation block. Moving it would either change validation order or require a provider-loop refactor, which is outside this slice.

The selected domain is the inline video validation block in `Config.Validate`. It is contiguous, self-contained, uses only `Config.Video`, and contains optional/disabled behavior, numeric boundaries, conditional requirements, and combined-error ordering. Moving it preserves its exact position: after `Admin.OIDC.Validate` and before `Listen` validation.

## Characterization tests first

Added `internal/config/config_validate_video_test.go` before extracting the validator. The table-driven characterization suite has 44 cases covering:

- disabled/legacy values that must remain optional;
- enabled mode requiring `video.auth_token_env`;
- ComfyUI and code-render enablement gates;
- `video.max_asset_bytes` at `-1`, `0`, `8<<30`, and `8<<30 + 1`;
- code-render planner model and model-list alternatives;
- code-render duration at `-1`, `0`, `3600`, `3600.5`, NaN, and infinity;
- code-render planner base URL control characters and oversized values;
- code-render retention at negative, `8760`, and `8761` hours;
- ComfyUI base URL, workflow path, four required node IDs, and duration boundaries;
- combined-error ordering across code-render, auth, asset, ComfyUI, and duration checks;
- a `SaveAtomic`/`LoadBase` round trip that asserts the video configuration is unchanged and still validates.

The ComfyUI node-ID checks iterate a map. When more than one node ID is missing, the returned error is intentionally not asserted to be deterministic because the original code did not provide that contract. The characterization tests cover one missing node at a time and retain the existing map iteration unchanged. This is documented rather than changed.

Baseline characterization run before extraction:

```text
$ go test ./internal/config -run 'TestValidateVideo' -count=1 -v
...
--- PASS: TestValidateVideoDomainCharacterization (0.00s)
    --- PASS: ... (44 subtests)
--- PASS: TestValidateVideoConfigRoundTrip (0.01s)
PASS
ok  	github.com/ali-shortcuts/nexaroute/internal/config	0.010s
```

## Change

`Config.Validate` now calls:

```go
if err := c.validateVideo(); err != nil {
    return err
}
```

The new private `func (c Config) validateVideo() error` contains the original conditionals, error messages, and evaluation order without modification. `Config.Validate` remains a value-receiver method and both methods return the first error, preserving fail-closed behavior. `ApplyDefaults` and `LoadBase` are untouched, and the round-trip test confirms persisted video configuration still loads and validates.

## Complexity evidence

Measured with `gocyclo v0.6.0`:

```text
$ GOFLAGS=-mod=mod go run github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0 -top 5 internal/config/config.go
```

Before (`cbdf159`):

```text
644 config (Config).Validate internal/config/config.go:1865:1
105 config (*Config).ApplyDefaults internal/config/config.go:1343:1
32 config (*ProviderConfig).ApplyDefaults internal/config/config.go:1778:1
21 config hasSecretValues internal/config/config.go:3598:1
17 config (Config).proxyEgressViolation internal/config/config.go:3374:1
```

After (`f20f447`):

```text
616 config (Config).Validate internal/config/config.go:1918:1
105 config (*Config).ApplyDefaults internal/config/config.go:1343:1
32 config (*ProviderConfig).ApplyDefaults internal/config/config.go:1778:1
30 config (Config).validateVideo internal/config/config.go:1865:1
21 config hasSecretValues internal/config/config.go:3605:1
17 config (Config).proxyEgressViolation internal/config/config.go:3381:1
```

Measured result:

- `Config.Validate`: 644 -> 616, a reduction of 28 (4.35%).
- New `validateVideo` validator: complexity 30.
- Aggregate code complexity is unchanged; the goal of this slice is separation of concerns and a smaller `Config.Validate`, not removing branches.

## Verification

Local verification on implementation commit `f20f447`:

```text
$ gofmt -w internal/config/config.go internal/config/config_validate_video_test.go
$ go test ./internal/config -run 'TestValidateVideo' -count=1
ok  	github.com/ali-shortcuts/nexaroute/internal/config	0.007s

$ go test ./internal/config -count=1
ok  	github.com/ali-shortcuts/nexaroute/internal/config	0.092s

$ go vet ./internal/config
(exit 0)

$ go test -count=1 ./...
(all packages ok)

$ go vet ./...
(exit 0)

$ go test -race -count=1 ./...
(all packages ok)

$ ./scripts/verify.sh
...
WARN: Chromium + Python Playwright unavailable; skipping browser acceptance
...
VERIFY PASS
```

Browser acceptance was skipped by `scripts/verify.sh` because Chromium and Python Playwright are unavailable in this environment. All other verify stages ran and passed, including gofmt, unit/integration tests, vet, race, release artifact checks, SBOM checks, short fuzz checks, and linux amd64/arm64 builds.

GitHub Actions CI and Security results are not claimed here. They must be evaluated on the final PR head SHA; this report records local evidence on `f20f447`.

## Remaining phases

The next recommended domain is admin session/security-store/OIDC validation. It is the next contiguous inline block after TLS validation and has clear optional and combined-error behavior. Further slices, in descending safety/coherence order:

1. Admin session, audit, security-store, and OIDC validation.
2. Listen and logging validation.
3. Routing, cache, semantic-cache, and probe numeric limits.
4. MCP, guardrails, client auth, control plane, and budget alerts.
5. Per-provider validation loop, which needs a shared validation context for provider/deployment counters and duplicate tracking.
6. Evaluation, decision, autopilot, virtual endpoints, pools, and fallback chains.
7. Final `ValidationContext`/`Strict` convergence after the domains are extracted.

Parent #593 remains open. This slice does not attempt the full validator rewrite.

# Finding F7 coverage summary (issue #172)

Parent audit: #57. Audit base: `76525d8e3e51248a92ac16d087db50e13e2c1d20`.
Branch base re-checked before coding: `76525d8` (same commit; no prior fix).

## Toolchain

- `go version go1.24.13 linux/amd64`

## Commands (reproducible)

```sh
go test ./internal/compat ./internal/route ./internal/feature ./internal/core ./cmd/gateway -cover -count=1
go test ./... -count=1
bash scripts/verify.sh
```

## Results (`go test -cover`)

| Package | Baseline | Final | Target 60% |
|---|---|---|---|
| `internal/compat` | 51.6% | 64.5% | PASS |
| `internal/route` | 56.3% | 94.8% | PASS |
| `internal/feature` | 59.4% | 79.6% | PASS |
| `internal/core` | 30.4% | 100.0% | PASS |
| `cmd/gateway` | 12.9% | 23.2% | BLOCKED (see below) |

## Tests added (behavior-focused, deterministic)

- `internal/core/f7_coverage_test.go`: `ParseSystem` string/blocks/empty/invalid
  normalization plus `ParseAnthContent` form/error paths.
- `internal/feature/f7_coverage_test.go`: Anthropic and Responses relevant-text
  collection, session-key variants, tool-choice/structured-output semantics,
  tool-count clamping — all through public `Extract`.
- `internal/route/f7_coverage_test.go`: `ResolveByID`, list accessors (+copy
  semantics), `GetExpanded`, dangling-reference misses, `AllFilteredCandidates`
  order/dedup, `FilterCandidates` modes, expanded-index fallback in
  `ResolveCandidates`.
- `internal/compat/f7_coverage_test.go`: tri-state string/JSON round-trip,
  `Store` lifecycle (`Seed`/`LearnNumeric`/`LearnProtocol`/`SetRepair`/
  `SetIssue`/`Snapshot`/`Count`/`Drop`/`Reset`), eligibility matrix, dialect
  seeding/aliasing, dialect fingerprints, transport/malformed/stream
  classification, HTTP status/labels, context-token parsing, probe helpers.
- `cmd/gateway/f7_coverage_test.go`: `dashboardURL` from config + missing/
  corrupt/invalid fallbacks, `openExistingUI` unreachable and ready-server
  branches, `ensureConfig` uncreatable-parent error.

## Production change (one, justified)

- `internal/compat/capabilities.go` (`Store.LearnNumeric`): initialize the
  `Evidence` map before writing. The new `TestStoreLearnNumericAndProtocol`
  test exposed a `panic: assignment to entry in nil map` when learning numeric
  facts for a deployment with no existing contract (fresh `NewStore` +
  `LearnNumeric` with positive values). Same nil-guard pattern already used by
  `LearnSuccess`/`LearnUnsupported`/`Seed`. No API change, no behavior change
  for existing callers — previously that path always crashed.

## Uncovered blocker: `cmd/gateway` cannot safely reach 60%

Final per-function coverage: `defaultConfigPath` 100%, `uiURL` 100%,
`ensureConfig` 90.0%, `dashboardURL` 88.9%, `openExistingUI` 87.5%,
`main` 0.0%. Every helper is covered; the entire deficit is `main()`, which
parses flags, acquires the single-instance lock, binds/listens, serves HTTP,
launches the browser goroutine, and handles SIGINT/SIGTERM shutdown. Covering
it in-process would require contrived flag/os/signal/listener stubbing around
`log.Fatal`/`Serve` blocking calls — exactly the fake-success-path testing the
issue forbids. Proposed narrowed target: all non-`main` gateway functions
>= 85% (currently met: lowest is 87.5%).

## Known quirk observed, not changed

`ParseContextTokens("... 128k tokens ...")` behavior is asserted as-is; plain
"N tokens" phrasings scale by 1000 because the `k` check matches the 'k' in
"tokens" (also noted in `docs/reports/audit-top15-issue57.md`). Out of scope
for this coverage-only PR.

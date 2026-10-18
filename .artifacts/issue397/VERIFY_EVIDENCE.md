# Issue #397 (Phase 5) verification evidence — DS lane

Branch: `opencode/issue397-20261018025653`
Base: `47a64d4` (main)

## Command run

```bash
./scripts/verify.sh
```

Exit status: `0` (`VERIFY PASS`).

## Exact outcomes (from the run log)

- `go version go1.25.0 linux/amd64`
- shell syntax (`bash -n scripts/*.sh`): pass
- `gofmt -l .`: no files listed
- mandatory unit/integration pass (`go test -timeout=3m -count=1 ./...`):
  **54 packages reported `ok`, 0 `FAIL`, 0 `[no test files]`**
- `go vet ./...`: pass
- mandatory race pass (`go test -race -timeout=3m -count=1 ./...`):
  **54 packages reported `ok`, 0 `FAIL`**
- release artifact manifest checks: `test-release-artifacts: PASS`
- SBOM generator trust-chain checks: `test-generate-sbom: PASS`
- web UI JS syntax (`node --check app.js`, `visual-agent.js`): pass
- browser acceptance: `WARN: Chromium + Python Playwright unavailable; skipping browser acceptance`
- short fuzz: `FuzzPatchJSONModel` PASS (5621 execs), `FuzzParseAnthContent` PASS (74107 execs)
- linux amd64 + arm64 cross-builds: pass

## Iterations

1. First full `./scripts/verify.sh`: PASS on first attempt (`EXIT=0`).
2. Red-phase check before the fix (new canonical tests against pre-fix
   code): 5 distinct failures:
   - `TestResponsesEmitterCompletedUsageIncludesTotalTokens`
   - `TestResponsesEmitterFunctionCallItemIsCompleteInOutputItemDone`
   - `TestResponsesEmitterFunctionCallArgumentsDoneCarriesArguments`
   - `TestResponsesEmitterEmitsMessageItemLifecycle`
   - `TestResponsesStreamDecoderPreservesArgumentsOnlyInDoneEvent`
3. After the fix: all new canonical + gateway Codex tests pass.

## New regression tests

`internal/protocol/canonical/codex_responses_test.go`:
- `TestResponsesEmitterCompletedUsageIncludesTotalTokens`
- `TestResponsesEmitterFunctionCallItemIsCompleteInOutputItemDone`
- `TestResponsesEmitterFunctionCallArgumentsDoneCarriesArguments`
- `TestResponsesEmitterEmitsMessageItemLifecycle`
- `TestResponsesStreamDecoderPreservesArgumentsOnlyInDoneEvent`
- `TestResponsesStreamDecoderPrefersDeltasOverFinalArguments`

`internal/httpapi/codex_responses_test.go`:
- `TestCodexResponsesStreamToolLoopPreservesToolCallOverChatUpstream`
- `TestCodexResponsesStreamTextEmitsMessageItemOverAnthropicUpstream`
- `TestResponsesUpstreamArgumentsOnlyInDoneSurviveToAnthropicClient`

Not run in this environment: real browser acceptance (Chromium + Python
Playwright unavailable); live provider tests (out of scope; no paid
credentials).

# Staticcheck audit slice (Issue #605)

- Parent audit: [#593](https://github.com/ali-shortcuts/nexaroute/issues/593) (remains open)
- Audit slice: [#605](https://github.com/ali-shortcuts/nexaroute/issues/605)
- Audited `main`: `cbdf159cb6a3a7f91ceeabf4cab110f6a950718d`
- Work branch: `opencode/issue605-20261021154036`
- Code checkpoint: `36234c369e729c2694736c50111872954c514481`
- Report prepared: `2026-10-21T15:58Z`
- Tool: `staticcheck 2026.2.2 (0.8.2)`, downloaded from the pinned release URL used by `.github/workflows/audit.yml`

## Command and result

```sh
# installed under the repository-owned ignored path .artifacts/issue605/tool
.artifacts/issue605/tool/staticcheck/staticcheck -f text ./...
```

| Run | Exit | Diagnostics |
| --- | --- | --- |
| Fresh `main` before changes | 1 | 23 (22 ST1005, 1 SA4017) |
| Code checkpoint after changes | 0 | 0 |
| Target set `SA4006,SA4000,U1000` after changes | 0 | 0 |

No lint rules were disabled, no checks were filtered out of the full run, and no
`//lint:ignore` or `#nosec`-style suppression was added.

### Before output (verbatim)

```text
cmd/videogen/client.go:122:10: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:131:13: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:133:12: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:136:10: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:210:10: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:218:10: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:224:12: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:226:11: error strings should not be capitalized (ST1005)
cmd/videogen/client.go:228:10: error strings should not be capitalized (ST1005)
internal/httpapi/anthropic.go:907:15: error strings should not be capitalized (ST1005)
internal/httpapi/helpers.go:132:10: error strings should not be capitalized (ST1005)
internal/protocol/canonical/anthropic.go:162:17: error strings should not be capitalized (ST1005)
internal/protocol/canonical/stream.go:117:40: HasPrefix doesn't have side effects and its return value is ignored (SA4017)
internal/translate/anthropic_to_openai.go:74:22: error strings should not be capitalized (ST1005)
internal/translate/anthropic_to_openai.go:79:22: error strings should not be capitalized (ST1005)
internal/translate/anthropic_to_openai.go:92:22: error strings should not be capitalized (ST1005)
internal/video/audio/piper.go:118:11: error strings should not be capitalized (ST1005)
internal/video/audio/piper.go:125:11: error strings should not be capitalized (ST1005)
internal/video/audio/piper.go:127:10: error strings should not be capitalized (ST1005)
internal/video/audio/piper.go:173:10: error strings should not be capitalized (ST1005)
internal/video/audio/piper.go:181:10: error strings should not be capitalized (ST1005)
internal/video/audio/piper.go:188:10: error strings should not be capitalized (ST1005)
internal/video/orchestrator/orchestrator.go:194:29: error strings should not be capitalized (ST1005)
```

## SA4017: disposition changed from the prior report

The previous report called this a false positive and claimed a probe had shown
the analyzer flagging a side-effecting switch branch. That rationale does not
survive independent reproduction, so it was not trusted.

The flagged expression was the second `strings.HasPrefix` in the final case of
`SSEReader.Next`:

```go
case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
    continue
```

`SA4017` inspects IR call instructions that have no referrers and reports pure
callees (`staticcheck/sa4017/sa4017.go` in the pinned source). In this loop the
case body was only `continue`, and it was the last statement in the loop body.
The true edge (`continue` to the loop header) and the false edge (fall out of
the switch, reach the end of the loop body, then the loop header) therefore
reach the same block: the call result cannot affect observably different
control flow. The final case was redundant. Removing it is not a workaround;
the analyzer correctly identified pointless work in a final no-op branch.

`id:` and `retry:` fields are still skipped by the default fall-through path.
The function comment now records that, and
`TestSSEReaderSkipsIDAndRetryFields` asserts that both fields are ignored while
the following `event:` and `data:` fields still surface. The earlier
`TestCoveragePrimary...` SSE case already covered an `id:` line; the new test
adds `retry:` plus the explicit event/data assertion.

The minimal reproduction used the exact `SSEReader.Next` control flow under
`staticcheck 2026.2.2`: it produced the same `SA4017` on the second `HasPrefix`;
removing only the final no-op case made the probe clean. The temporary probe was
kept under the ignored `.artifacts/issue605/` workspace during analysis and is
not part of the committed change.

## ST1005: diagnostic-by-diagnostic disposition

All 22 findings were leading-capitalization style violations in error strings.
Each was fixed by lowercasing only the first word, preserving interpolation,
wrapping, and error identity. No tests asserted these exact client-visible texts,
and no behavior or API shape changed.

| # | Location | Before (leading text) | Disposition |
| --- | --- | --- | --- |
| 1 | `cmd/videogen/client.go:122` | `Video Gateway asset download failed` | Lowercased to `video gateway ...` |
| 2 | `cmd/videogen/client.go:131` | `Video Gateway HTTP %d (%s): %s` | Lowercased to `video gateway ...` |
| 3 | `cmd/videogen/client.go:133` | `Video Gateway HTTP %d: %s` | Lowercased to `video gateway ...` |
| 4 | `cmd/videogen/client.go:136` | `Video Gateway HTTP %d while downloading asset` | Lowercased to `video gateway ...` |
| 5 | `cmd/videogen/client.go:210` | `Video Gateway request failed` | Lowercased to `video gateway ...` |
| 6 | `cmd/videogen/client.go:218` | `Video Gateway response exceeds 2 MiB limit` | Lowercased to `video gateway ...` |
| 7 | `cmd/videogen/client.go:224` | `Video Gateway HTTP %d (%s): %s` | Lowercased to `video gateway ...` |
| 8 | `cmd/videogen/client.go:226` | `Video Gateway HTTP %d: %s` | Lowercased to `video gateway ...` |
| 9 | `cmd/videogen/client.go:228` | `Video Gateway HTTP %d` | Lowercased to `video gateway ...` |
| 10 | `internal/httpapi/anthropic.go:907` | `Anthropic response must be a JSON object` | Lowercased to `anthropic ...` |
| 11 | `internal/httpapi/helpers.go:132` | `Anthropic response contains an error envelope` | Lowercased to `anthropic ...` |
| 12 | `internal/protocol/canonical/anthropic.go:162` | `Anthropic image block has no usable source` | Lowercased to `anthropic ...` |
| 13 | `internal/translate/anthropic_to_openai.go:74` | `Anthropic image block has no usable source` | Lowercased to `anthropic ...` |
| 14 | `internal/translate/anthropic_to_openai.go:79` | `Anthropic tool_use requires assistant role, id, and name` | Lowercased to `anthropic ...` |
| 15 | `internal/translate/anthropic_to_openai.go:92` | `Anthropic tool_result requires user role and tool_use_id` | Lowercased to `anthropic ...` |
| 16 | `internal/video/audio/piper.go:118` | `Piper synthesis exceeded timeout` | Lowercased to `piper ...` |
| 17 | `internal/video/audio/piper.go:125` | `Piper synthesis failed` | Lowercased to `piper ...` |
| 18 | `internal/video/audio/piper.go:127` | `Piper synthesis failed` | Lowercased to `piper ...` |
| 19 | `internal/video/audio/piper.go:173` | `Piper did not create narration output` | Lowercased to `piper ...` |
| 20 | `internal/video/audio/piper.go:181` | `Piper output is not a non-empty WAV file` | Lowercased to `piper ...` |
| 21 | `internal/video/audio/piper.go:188` | `Piper output does not have a RIFF/WAVE header` | Lowercased to `piper ...` |
| 22 | `internal/video/orchestrator/orchestrator.go:194` | `Gateway narration requires a multi-shot episode with at least two shots` | Lowercased to `gateway ...` |
| 23 | `internal/protocol/canonical/stream.go:117` | `SA4017` on final `id:`/`retry:` case | Removed redundant no-op case; added regression test |

## Diff

`git diff cbdf159cb6a3a7f91ceeabf4cab110f6a950718d..36234c369e729c2694736c50111872954c514481 --stat`:

```text
 cmd/videogen/client.go                            | 18 +++++++++---------
 internal/httpapi/anthropic.go                     |  2 +-
 internal/httpapi/helpers.go                       |  2 +-
 internal/protocol/canonical/anthropic.go          |  2 +-
 internal/protocol/canonical/stream.go             |  5 ++---
 internal/protocol/canonical/stream_bounds_test.go |  8 ++++++++
 internal/translate/anthropic_to_openai.go         |  6 +++---
 internal/video/audio/piper.go                     | 12 ++++++------
 internal/video/orchestrator/orchestrator.go       |  2 +-
 9 files changed, 32 insertions(+), 25 deletions(-)
```

The functional change is limited to the redundant SSE case removal:

```diff
 // Next returns the next SSE event's name and data payload. done is true when
-// the stream ended cleanly. Unknown event types and comments are skipped.
+// the stream ended cleanly. Unknown event types, comments, and id:/retry:
+// fields are skipped.
@@
-		case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
-			continue
```

plus the regression test:

```go
func TestSSEReaderSkipsIDAndRetryFields(t *testing.T) {
	payload := "id: 1\nretry: 250\nevent: notice\ndata: kept\n\n"
	name, data, done, err := NewSSEReader(strings.NewReader(payload)).Next()
	if err != nil || done || name != "notice" || data != "kept" {
		t.Fatalf("SSE id/retry handling name=%q data=%q done=%t err=%v", name, data, done, err)
	}
}
```

The remaining 22 edits are the one-token leading-character lowercasing shown in
the table above. No formatting-only rewrite was performed.

## Tests and verification

All commands were run on code checkpoint
`36234c369e729c2694736c50111872954c514481` unless noted. Raw outputs are in the
repository-owned ignored path `.artifacts/issue605/`.

| Command | Exit | Result |
| --- | --- | --- |
| `staticcheck -f text ./...` (pinned 2026.2.2) | 0 | 0 diagnostics; before was 23 |
| `staticcheck -f text -checks=SA4006,SA4000,U1000 ./...` | 0 | 0 diagnostics |
| `go test -count=1 ./cmd/videogen ./internal/httpapi ./internal/protocol/canonical ./internal/translate ./internal/video/audio ./internal/video/orchestrator` | 0 | All affected packages `ok` |
| `go test -timeout=10m -count=1 ./...` | 0 | 57 packages `ok` |
| `go vet ./...` | 0 | Clean |
| `go test -race -timeout=10m -count=1 ./...` | 0 | 57 packages `ok` |
| `./scripts/verify.sh` | 0 | `VERIFY PASS` |
| `gofmt -l .` | 0 | Empty |
| `git diff --check` | 0 | Clean |

`./scripts/verify.sh` covered the mandatory unit/integration pass, `go vet`, the
mandatory race pass, release-artifact and SBOM trust-chain checks, JavaScript
syntax checks, short fuzz runs, and linux amd64/arm64 builds. It reported:
`WARN: Chromium + Python Playwright unavailable; skipping browser acceptance`.
That browser check is a skip, not a pass, and this slice does not change UI code.

A preliminary `verify.sh` run occurred while the downloaded staticcheck source
tree was still under `.artifacts/issue605/src`; `gofmt -l .` walked that
third-party testdata and printed parse errors for intentionally malformed
fixtures. The source tree was removed and the final run above is clean. The
downloaded analyzer binary and raw logs remain under the ignored `.artifacts/`
path and are not committed.

## Residual findings and limits

- Staticcheck residual findings: **none**. Full pinned analyzer and the target
  set `SA4006,SA4000,U1000` are clean on the code checkpoint.
- No production behavior, routing, security-slice code, or public API was
  changed. The only behavioral edit restores the already-observed SSE semantics
  by deleting a redundant no-op branch and pinning it with a test.
- Fresh GitHub Actions CI and Security results for the PR SHA are not available
  locally and must be observed on the opened PR. Parent #593 remains open.
- Browser acceptance is unavailable in this runner; no browser evidence is
  claimed.
- No live providers, OIDC, PostgreSQL, Redis, KMS, or release mutation were
  involved in this slice.

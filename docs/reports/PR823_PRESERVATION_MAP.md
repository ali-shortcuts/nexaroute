# PR #823 preservation review (not independent approval)

Snapshot: main `436c4e98618eed0c41e59059eae7eca5f05dd190`; #823
`58bacddbc59e8cf9a01bc59620128c93ea2d7e1a`; #824
`8d2ae57f03b95a81d7282eaeeeb6261ab0cda458` before subsequent follow-up.
Both PRs were open and non-draft, with no formal reviews. #823 is not an
ancestor of #824. Refresh heads, bases, diffs, checks and reviews before acting.
This is an author-side semantic inspection, not a genuine independent review.

| #823 commit | Behavior and #824 mapping | Alterations / remaining verification |
|---|---|---|
| `d40b029` | Gate suppresses the misleading unused-manifest success note when an issue/API check failed; case 3j is present in `.github/scripts/test-final-release-gate.sh`. Integrated in `29245e6`. | #824 adds v0.25.0 completion-only checks and separate generic future-version exclusions. Recheck both paths on the final head; no issue closure is inferred. |
| `2c97866` | Overview chart uses `/admin/api/activity/requests`, bounded in-memory data, loading/error/forbidden/empty states, responsive styling, accessible SVG and component documentation. Integrated in `29245e6`; tests in `internal/httpapi/activity_chart_test.go`. | #824 explicitly separates active/stale/cancelled/other from failed, uses a half-open rolling window, corrects translations and refresh retry (`3b32d9f`, `scripts/test-activity-refresh.js`). These are intentional semantic corrections, not byte-identical ports. Browser fixtures are not provider acceptance. Independent accessibility and all-page audit remain. |
| `0a426e2` | Main-SHA-specific evidence and dispatch-403 history in `docs/EXTERNAL_VERIFICATION.md`, release blockers and release notes. Main observation and historical run IDs retained, with updates in `29245e6` and later #824 commits. | Original snapshot's zero-open-PR claim is obsolete; #824 records two open PRs and no formal approval. Exclusion instructions are superseded **only for v0.25.0** by the owner's completion-only policy. Historical results do not transfer to the candidate. |
| `58bacdd` | The autopilot action uses `http.MaxBytesReader(..., 4096)`; normal and oversized-body tests are retained in `internal/httpapi/autopilot_actions_test.go`. Integrated in `29245e6`. | The original scanner was replaced with a more precise AST check and regressions in `internal/httpapi/request_body_bounds_test.go`; it only covers selected direct reads, not aliases, all APIs or general taint. Independent security review remains. |

The four commit bodies and relevant implementation and test diffs were
inspected against the common base. No additional missing behavior was
identified in these areas; this is not proof that every indirect effect or
all linked documentation has been independently accounted for. An independent
reviewer must verify this map, chart and scanner semantics, documentation and
the complete diffs on the current heads. Do not close #823 until verified work
is actually preserved in reviewed main and its disposition is recorded.

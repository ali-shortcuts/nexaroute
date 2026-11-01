# PR #823 review follow-up patch

This is a **proposed patch**, not a change applied to PR #823. It is based on
PR #823 head `58bacddbc59e8cf9a01bc59620128c93ea2d7e1a` and is provided because
the current session is pinned to the separate #824 branch. Apply only after
reviewing it against that exact head. It has zero context and must not be applied
on a different tree without careful re-review. If the head changes, re-review and
regenerate as needed. From a clean checkout of that PR head:

```sh
git apply --unidiff-zero --check path/to/PR823_REVIEW_FOLLOWUP.patch
git apply --unidiff-zero path/to/PR823_REVIEW_FOLLOWUP.patch
```

The patch separates succeeded, failed, active, stale, cancelled and other
states in the chart and legend. It uses a rolling half-open window ending at
the observation time, so the displayed duration and filtering agree, and
retains English, Persian and Pashto copy. It replaces the request-body regex
with a conservative Go AST check for direct `json.NewDecoder` and
`io.ReadAll` calls using `r.Body`. This is **not** general taint analysis:
body aliases, other read APIs and interprocedural flows still require review.
The scanner counts 14 direct sites on the stated head and enforces that floor.
The existing 4 KiB autopilot limit is preserved.

Local checks in an extracted copy of that head:

- Chart behavior test script extracted from the actual Go test and executed
  against the patched production JavaScript under Node: PASS.
- Standalone Go AST scanner tests: PASS using a Go 1.27.3 binary bundled with
  an npm package. This toolchain is not release-build provenance.
- Red/green: replacing the autopilot bounded decoder with
  `json.NewDecoder(r.Body)` made `TestAdminRequestBodyReadsAreBounded` fail
  at `autopilot.go:406`; restoring it made the test pass.
- `git apply --unidiff-zero --check` against the stated PR head: PASS.
- Full project tests and browser acceptance of this patch: NOT_RUN. The patch
  must be applied and the complete required CI/browser checks rerun before
  considering #823 for merge.

The overlapping release-gate tests and release documentation in #823 and #824
still need manual reconciliation after the owner chooses an approved merge
order. This patch neither performs that reconciliation nor authorizes a merge.

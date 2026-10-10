# Other branches audit

Date: 2026-10-10
Base used for comparison: `origin/phase1b-on-1a` (`c986bda`)
Coverage branch: `phase1-coverage-on-1b`
Coverage PR: [#218](https://github.com/ali-shortcuts/nexaroute/pull/218)

## `chore/video-gateway-audit-hardening`

Tip: `376cd10` (`release: assemble integrated gateway and video pipeline`). This branch adds the video gateway/runtime, queue/storage/composer/orchestrator/provider layers, `cmd/videogen`, release wiring, UI changes, and client-auth hardening. Its full `go test -count=1 ./...` run passed.

A trial merge into `phase1-coverage-on-1b` using `git merge --no-commit --no-ff` produced real conflicts in `cmd/gateway/main.go` and `internal/httpapi/web/app.js`. The trial merge was aborted; no changes from the branch remain in the coverage branch. It is not a coverage-only change and should be merged only after the phase-1 stack is intentionally integrated and the conflicts are resolved by the owner.

## `fix/virtual-key-policy-intersection`

Tip: `f5f415f` (`test: keep legacy tenant metadata usage fixture`). This branch hardens client-auth virtual-key tenant metadata/policy scope intersection, rejects duplicate metadata, bounds large-payload inspection, and adds related tests. Its full `go test -count=1 ./...` run passed.

A trial merge into `phase1-coverage-on-1b` completed cleanly and was then aborted. No changes from the branch remain in the coverage branch. It overlaps `internal/httpapi/clientauth.go` and related tests, so it remains a separate policy/security change and should not be combined casually with other client-auth changes.

## Final audit conclusion

Both audited branches pass their own test suites. The video branch has a textual merge conflict with the current phase-1 gateway/UI state; the virtual-key branch is mergeable at this snapshot but is outside the approved coverage scope. No other branch was merged, no merge conflict was resolved on those branches, and no history was rewritten.

The coverage branch itself is clean at the end of the work. PR #218 is merge-clean and its `verify` check is completed successfully. The only remaining item in the overall coverage task is the numerical repository target of 85%; the main report records the final reproducible value of 79.4% and the exact remaining hotspots.

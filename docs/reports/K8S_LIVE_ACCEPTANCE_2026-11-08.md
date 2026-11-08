# Kubernetes live acceptance — 2026-11-08

## Exact main revision and evidence

- Repository: `ali-shortcuts/nexaroute`
- Tested main SHA: `1d8ac1ba82ece8b589e940be3519a7bb4a9035f0`
- Change under test: [PR #869](https://github.com/ali-shortcuts/nexaroute/pull/869), merged as the tested SHA
- Post-merge workflow: [Kubernetes live acceptance run 42490453403](https://github.com/ali-shortcuts/nexaroute/actions/runs/42490453403)
- Job: `k8s-live`, completed successfully
- Raw evidence artifact: [artifact 13621468111](https://github.com/ali-shortcuts/nexaroute/actions/runs/42490453403/artifacts/13621468111)

The uploaded `live-run.log` was inspected. It records:

- The upgraded release Pod reached Ready after the preceding supported release was installed.
- The PostgreSQL-backed Gateway reached Ready.
- The release ServiceAccount was denied pod-list access; the check used a Kubernetes SubjectAccessReview rather than relying on operator impersonation.
- The acceptance completed with `RESULT: failures=0 not_run=0`, with no FAIL/ABORT result.

The workflow job also completed its checkout, tool-version capture, live acceptance and raw-evidence upload steps successfully. This is the current-main live disposable-cluster result; it supersedes the earlier failed or incomplete Kubernetes runs on older SHAs for this technical acceptance snapshot.

## Evidence boundary

This run used a disposable `kind` cluster on a hosted runner. It is evidence for the exercised Helm/Kubernetes lifecycle on this exact SHA, not proof of production-cluster readiness, independent physical-host failure tolerance, multi-host HA, or measured RPO/RTO. It does not satisfy Issue #652.

The result is SHA-bound. If `main` advances before the final v0.25.0 candidate is frozen, the exact-candidate release evidence must be collected again on that final SHA. Do not reuse this run as evidence for a different commit.

## Issue #650 disposition

Issue #650 is currently closed with `state_reason=completed`. Its latest disclosure comment, [issue comment 6722959520](https://github.com/ali-shortcuts/nexaroute/issues/650#issuecomment-6722959520), records that the closure action came from the shared account and its authority could not be attributed; keep-closed versus reopen remains pending the owner's disposition.

This report records the technical acceptance evidence only. It does not treat the closure event as owner approval, resolve the issue's state, or authorize a release.

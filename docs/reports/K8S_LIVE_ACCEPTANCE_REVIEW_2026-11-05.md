# Kubernetes live acceptance artifact re-review — 2026-11-05

## Verdict

Workflow run [42039397045](https://github.com/ali-shortcuts/nexaroute/actions/runs/42039397045) on `main` at `6848c140de0101409138cad1f5006bc2349f4d0a` is **not valid closure evidence for Issue #650**. GitHub Actions concluded `success`, and the script printed `RESULT: failures=0 not_run=1`, but direct inspection of the uploaded artifact shows two commands failed while the harness printed PASS immediately afterward.

## Confirmed artifact findings

- `backup.log` records exit code 1: the backup CLI tried to open the file-backed bbolt security store while the gateway was running and held its exclusive writer lock. The error explicitly says to stop the gateway or use `backup --live`.
- `doctor-after-restart.log` records exit code 1: its read-only security-store check hit that same live writer lock. Running the offline `doctor` command inside a live gateway container is not a valid check for this store design.
- Both commands were followed by unconditional PASS output. The top-level invocation `live_phase || live_code=$?` caused Bash to suppress `errexit` throughout the function, so failed commands did not abort the run. This is a harness false-green, not a gateway feature defect.
- The termination snapshot shows the gateway container exited with code 0 and reason `Completed` at `2026-11-05T13:53:41Z`. The acceptance-stub sidecar exited with code 137 and reason `Error` only at `2026-11-05T13:55:41Z`, exactly at the pod's 120-second grace deadline. The previous measurement used the latest `finishedAt` across all containers, so it attributed the sidecar's failure to gateway shutdown.
- The one `NOT_RUN` result was the optional service-account `kubectl auth can-i` impersonation check; Kubernetes RBAC objects were absent, but the runner lacked impersonation permission.

## Changes proposed in this PR

1. Invoke `live_phase` directly so strict shell error handling is preserved; add a regression test against reintroducing the conditional OR-list.
2. Use the documented `backup --live` snapshot path before and after restart. Do not run the offline `doctor` against a live bbolt store.
3. Give the acceptance-only Python stub an explicit, deadlock-safe SIGTERM handler, and add a local self-test proving the stub exits cleanly after SIGTERM.
4. Keep all release and acceptance gates intact. Issue #650 remains open until a fresh live run on the corrected SHA produces a fully inspected artifact with no hidden command failures.

## Scope and safety

This report reviews one disposable kind run that used fake sentinel values and an ephemeral PostgreSQL container. It is not evidence of production-cluster support, multi-host HA, a real identity tenant, Vault staging/Transit, or paid LLM-provider acceptance. No production system or billable provider was contacted.

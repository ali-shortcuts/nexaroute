#!/usr/bin/env bash
# release-gate-status.sh -- SHA-bound release gate report.
#
# Usage: scripts/release-gate-status.sh <sha> [repo] [--rc] [--soak-summary PATH]
# (--rc may also be given without a repo: <sha> --rc.)
#
# Queries GitHub check runs associated with <sha>. PR-mode checks may test a
# synthetic merge checkout; --rc additionally verifies non-PR Actions run
# metadata and requires a validated two-hour soak summary for that SHA.
# The operator must independently verify supplied artifact provenance.
#
# PR-required gates are the checks that run on pull_request for this
# repository (CI, Security, Coverage, Redis/real-system acceptance).
# With --rc, the release-candidate-only gates are required too:
# audit lint/staticcheck/gosec/deadcode/extended fuzz, browser evidence,
# and the 2h soak. Without --rc they are reported as informational rows
# and do not pass or fail the PR gate.
#
# This script needs no local Go toolchain: it runs from any machine
# with `gh` authenticated (or from a GitHub Actions runner), which is
# the point -- the owner can verify a release candidate from their
# Ubuntu laptop without installing Go, Docker, or test dependencies.
set -euo pipefail

sha="${1:-}"
repo=""
rc=0
soak_summary=""
shift || true
while (($#)); do
  case "$1" in
    --rc) rc=1; shift ;;
    --soak-summary) [[ $# -ge 2 ]] || { echo "missing --soak-summary value" >&2; exit 2; }; soak_summary="$2"; shift 2 ;;
    --*) echo "unknown option: $1" >&2; exit 2 ;;
    *) if [[ -n "$repo" ]]; then echo "unexpected argument: $1" >&2; exit 2; fi; repo="$1"; shift ;;
  esac
done
repo="${repo:-${GITHUB_REPOSITORY:-ali-shortcuts/nexaroute}}"
if [ -z "$sha" ]; then
	echo "usage: $0 <sha> [repo] [--rc]" >&2
	exit 2
fi
if ! [[ "$sha" =~ ^[0-9a-f]{40}$ ]]; then
	echo "refusing non-SHA input: $sha (pass a full 40-hex commit SHA)" >&2
	exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if ! gh api "repos/$repo/commits/$sha/check-runs?per_page=100" > "$tmp/check-runs.json" 2> "$tmp/gh.err"; then
	echo "BLOCKED: could not read check runs for $sha from $repo:" >&2
	cat "$tmp/gh.err" >&2
	exit 2
fi

RELEASE_GATE_SHA="$sha" RELEASE_GATE_RC="$rc" RELEASE_GATE_REPO="$repo" RELEASE_GATE_SOAK_SUMMARY="$soak_summary" RELEASE_GATE_SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)" python3 - "$tmp/check-runs.json" <<'PY'
import json
import os
import sys
import re
import subprocess

sha = os.environ["RELEASE_GATE_SHA"]
rc = os.environ["RELEASE_GATE_RC"] == "1"
repo = os.environ["RELEASE_GATE_REPO"]
soak_summary = os.environ["RELEASE_GATE_SOAK_SUMMARY"]
script_dir = os.environ["RELEASE_GATE_SCRIPT_DIR"]
run_metadata = {}
with open(sys.argv[1]) as fh:
    payload = json.load(fh)

runs = payload.get("check_runs", [])
total = payload.get("total_count", len(runs))
if total > len(runs):
    print(f"BLOCKED: {total} check runs on {sha}, only {len(runs)} fetched; "
          "pagination not implemented, so the gate cannot be verified")
    sys.exit(2)

# name -> status of the LATEST run only. The latest run is the one
# with the greatest (created_at, id). created_at, not started_at,
# is the ordering field on purpose: a queued or waiting run has a
# created_at but no started_at yet, so a started_at key would sort
# that pending re-run before an old success and wrongly report
# PASS while new work is still pending. The monotonically
# increasing check-run id breaks created_at ties. A run with a
# missing created_at sorts first (it cannot be proven newest);
# such an incomplete record can still decide a row only when it
# is the sole run for that name, and then only as RUNNING unless
# it is completed with a success conclusion.
# An old success must never hide a newer failure, and an old
# failure must never hide a newer success or a newer run still
# in progress. Two runs with the same name (for example
# integration-redis, which exists in both the CI workflow and
# the standalone Redis workflow) are decided by this latest rule
# alone, never by any-success-wins.
def latest_run(name):
    found = [r for r in runs if r.get("name") == name]
    if not found:
        return None

    def run_key(r):
        created = r.get("created_at") or ""
        try:
            run_id = int(r.get("id") or 0)
        except (TypeError, ValueError):
            run_id = 0
        return (created, run_id)

    return max(found, key=run_key)


def row_status(name):
    run = latest_run(name)
    if run is None:
        return "NOT_RUN"
    status = run.get("status")
    # Anything that is not a completed run is still in flight for
    # gate purposes: queued, in_progress, waiting, requested, or a
    # missing/unknown status from an incomplete API record. None
    # of these may report PASS, and none may be hidden by an older
    # completed run (latest_run already selected this run).
    if status != "completed":
        return "RUNNING"
    # Fail closed: only an explicit success conclusion passes. A
    # completed run with any other conclusion, or with a missing
    # conclusion from an incomplete API record, is FAIL.
    if run.get("conclusion") != "success":
        return "FAIL"
    if not rc:
        return "PASS"
    # GitHub associates pull_request checks with the PR head even though the
    # default checkout uses a synthetic merge SHA. Do not count them as exact
    # release-candidate head evidence. Unknown provenance is also fail-closed.
    details = run.get("details_url") or ""
    match = re.fullmatch(r"https://github\.com/" + re.escape(repo) +
                         r"/actions/runs/(\d+)/job/\d+", details)
    if not match:
        return "UNVERIFIED"
    run_id = match.group(1)
    if run_id not in run_metadata:
        try:
            raw = subprocess.check_output(["gh", "api", f"repos/{repo}/actions/runs/{run_id}"],
                                          stderr=subprocess.DEVNULL, text=True)
            run_metadata[run_id] = json.loads(raw)
        except (OSError, subprocess.CalledProcessError, json.JSONDecodeError):
            run_metadata[run_id] = None
    meta = run_metadata[run_id]
    if not isinstance(meta, dict) or meta.get("event") not in ("push", "workflow_dispatch") \
            or meta.get("head_sha") != sha or meta.get("status") != "completed" \
            or meta.get("conclusion") != "success":
        return "UNVERIFIED"
    if name == "soak":
        # A passing soak job may have run for zero seconds. Require a complete
        # locally supplied report for this SHA with >=2h sustained workload.
        # The operator must separately verify artifact provenance against run_id.
        if meta.get("event") != "workflow_dispatch" or not soak_summary:
            return "UNVERIFIED"
        try:
            with open(soak_summary, encoding="utf-8") as fh:
                report = json.load(fh)
            seconds = report.get("requested_duration_seconds")
            if not isinstance(seconds, int) or isinstance(seconds, bool) or seconds < 7200 \
                    or report.get("result") != "PASS" or report.get("exit_code") != 0:
                return "UNVERIFIED"
            result = subprocess.run([sys.executable, os.path.join(script_dir, "check-soak-evidence.py"),
                                     "--summary", soak_summary, "--kind", "soak", "--expected-commit", sha,
                                     "--expected-duration-seconds", str(seconds)],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            if result.returncode != 0:
                return "UNVERIFIED"
        except (OSError, ValueError, AttributeError):
            return "UNVERIFIED"
    return "PASS"

pr_gates = [
    ("verify", "CI: verify.sh + stress + browser + container"),
    ("fuzz-smoke", "CI: fuzz smoke"),
    ("ci-soak-smoke", "CI: soak smoke"),
    ("integration-postgres", "CI: PostgreSQL integration"),
    ("integration-redis", "CI: Redis integration"),
    ("container", "CI: container build/smoke"),
    ("Go vulnerability scan", "Security: govulncheck"),
    ("CodeQL (Go)", "Security: CodeQL"),
    ("Fresh Go coverage", "Coverage: 95.0% gate"),
    ("Two real Gateway processes against PostgreSQL", "Real-system: Postgres"),
    ("Real Secret Service Store/Load", "Real-system: keyring"),
]
rc_gates = [
    ("golangci-lint", "Audit (manual): golangci-lint"),
    ("staticcheck", "Audit (manual): staticcheck"),
    ("gosec", "Audit (manual): gosec"),
    ("deadcode", "Audit (manual): deadcode"),
    ("Extended fuzz", "Audit (manual): extended fuzz"),
    ("evidence", "Browser evidence (main)"),
    ("soak", "Soak (manual): sustained run, 2h for a release candidate"),
]

print(f"Release gate status for SHA {sha}")
if rc:
    print("RC: exact non-PR workflow provenance required; verify supplied soak artifact provenance independently.")
print(f"{'GATE':<52} {'STATUS':<8} SOURCE")
failures = []
for name, source in pr_gates:
    status = row_status(name)
    print(f"{name:<52} {status:<8} {source}")
    if status != "PASS":
        failures.append(name)
print("-- release-candidate-only gates (required only with --rc) --")
for name, source in rc_gates:
    status = row_status(name)
    print(f"{name:<52} {status:<8} {source}")
    if rc and status != "PASS":
        failures.append(name)
if failures:
    print("Release Gate NOT passed: " + ", ".join(failures))
    sys.exit(1)
print("Release Gate Passed")
PY

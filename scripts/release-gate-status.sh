#!/usr/bin/env bash
# release-gate-status.sh -- SHA-bound release gate report.
#
# Usage: scripts/release-gate-status.sh <sha> [repo] [--rc]
# (--rc may also be given without a repo: <sha> --rc.)
#
# Queries the GitHub Checks API for check runs on exactly <sha> and
# reports one row per gate. A gate with no check run on that SHA is
# NOT_RUN, never PASS; a gate still queued/running is RUNNING and
# also fails the gate. Results from any other SHA are never used.
#
# PR-required gates are the checks that run on pull_request for this
# repository (CI, Security, Coverage, Redis/real-system acceptance).
# With --rc, the release-candidate-only gates are required too:
# audit lint/staticcheck/gosec/deadcode (manual dispatch only) and
# the 2h soak (manual dispatch only). Without --rc they are reported
# as informational rows and do not pass or fail the PR gate.
#
# This script needs no local Go toolchain: it runs from any machine
# with `gh` authenticated (or from a GitHub Actions runner), which is
# the point -- the owner can verify a release candidate from their
# Ubuntu laptop without installing Go, Docker, or test dependencies.
set -euo pipefail

sha="${1:-}"
repo=""
rc=0
# Positional parsing that tolerates `--rc` without a repo argument:
# `script <sha> --rc` must not make `--rc` the repo.
for arg in "${@:2}"; do
	if [ "$arg" = "--rc" ]; then
		rc=1
	elif [ -z "$repo" ]; then
		repo="$arg"
	fi
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

RELEASE_GATE_SHA="$sha" RELEASE_GATE_RC="$rc" python3 - "$tmp/check-runs.json" <<'PY'
import json
import os
import sys

sha = os.environ["RELEASE_GATE_SHA"]
rc = os.environ["RELEASE_GATE_RC"] == "1"
with open(sys.argv[1]) as fh:
    payload = json.load(fh)

runs = payload.get("check_runs", [])
total = payload.get("total_count", len(runs))
if total > len(runs):
    print(f"BLOCKED: {total} check runs on {sha}, only {len(runs)} fetched; "
          "pagination not implemented, so the gate cannot be verified")
    sys.exit(2)

# name -> status of the LATEST run only. The latest run is the one
# with the greatest (started_at, id): started_at orders runs in
# time, and the monotonically increasing check-run id breaks ties
# (including runs with a missing started_at, which sort first).
# An old success must never hide a newer failure, and an old
# failure must never hide a newer success or a newer run still
# in progress.
def latest_run(name):
    found = [r for r in runs if r.get("name") == name]
    if not found:
        return None

    def run_key(r):
        started = r.get("started_at") or ""
        try:
            run_id = int(r.get("id") or 0)
        except (TypeError, ValueError):
            run_id = 0
        return (started, run_id)

    return max(found, key=run_key)


def row_status(name):
    run = latest_run(name)
    if run is None:
        return "NOT_RUN"
    if run.get("status") != "completed":
        return "RUNNING"
    return "PASS" if run.get("conclusion") == "success" else "FAIL"

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
    ("soak", "Soak (manual): sustained run, 2h for a release candidate"),
]

print(f"Release gate status for SHA {sha}")
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

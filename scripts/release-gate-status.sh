#!/usr/bin/env bash
# release-gate-status.sh -- SHA-bound release gate report.
#
# Usage: scripts/release-gate-status.sh <sha> [repo] [--rc]
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
repo="${2:-${GITHUB_REPOSITORY:-ali-shortcuts/nexaroute}}"
rc=0
for arg in "$@"; do
	if [ "$arg" = "--rc" ]; then
		rc=1
	fi
done
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

# name -> best status. completed+success wins over running; any
# completed failure beats running (a re-run that later passes will
# add a success run and flip the row to PASS on the next invocation).
def row_status(name):
    found = [r for r in runs if r.get("name") == name]
    if not found:
        return "NOT_RUN"
    conclusions = {r.get("conclusion") for r in found}
    statuses = {r.get("status") for r in found}
    if "success" in conclusions:
        return "PASS"
    if conclusions - {"success", None}:
        return "FAIL"
    if statuses - {"completed"}:
        return "RUNNING"
    return "FAIL"

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

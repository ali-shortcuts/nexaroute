#!/usr/bin/env python3
"""Parse a `go test -v` integration log into a fail-closed report.

A required test is satisfied only when at least one recorded test
whose name equals the required entry, or starts with it (CI `-run`
patterns such as ``TestPostgresIntegration`` are prefixes, not
function names), actually ran with a PASS or FAIL verdict. A
required entry that is absent, or whose only matches are SKIP, is
a violation. Zero executed tests is a violation. Any FAIL is a
violation. The report is written even when violations exist, so
the artifact always shows the counts; the exit code is what fails
the job.

Top-level results only: a `go test -v` line
``--- PASS: TestName (0.01s)`` is a top-level result, while
``--- PASS: TestName/Sub`` is a subtest. Subtests never decide a
required entry and never inflate the counts; if a name appears
only as a subtest, it is recorded as such but does not satisfy a
required entry on its own.
"""

import re
import sys

# Required entries, aligned with the real source on main and with
# CI's own integration `-run` patterns. Entries that are prefixes
# (not full function names) are marked as such in comments; the
# one entry the user explicitly corrected is the full function
# name TestControlPlaneRedisIntegrationRuntimeLifecycle, which
# exists in cmd/gateway/controlplane_redis_integration_test.go.
# The plain name TestControlPlaneRedisIntegration is NOT a
# function and must never appear here.
DEFAULT_REQUIRED = [
    # CI postgres -run prefixes / exact names:
    "TestPostgresIntegration",  # prefix: StoreConformance, TwoInstancesAndRestart, ...
    "TestPostgresSecurityStoreIntegration",  # exact
    "TestPostgresBreakerStoreIntegration",  # exact
    "TestPostgresVideoStore",  # prefix: DSNReachable, ConcurrentClaim, ...
    "TestPostgresLedger",  # prefix: see pg_ledger_integration_test.go
    "TestBreakerSyncPostgresTwoInstances",  # exact
    "TestConcurrentPostgresControlPlaneBootstrapConverges",  # exact
    # CI redis -run prefixes / exact names:
    "TestRedisIntegration",  # prefix: StoreConformance, TwoClientsAndCorruption, ...
    "TestRedisSecurityStoreIntegration",  # exact
    "TestRedisBreakerStoreIntegration",  # exact
    "TestBreakerSyncRedisTwoInstances",  # exact
    "TestControlPlaneRedisIntegrationRuntimeLifecycle",  # exact
]

TOP_LEVEL = re.compile(r"^--- (PASS|FAIL|SKIP): ([A-Za-z0-9_]+) \(")
SUBTEST = re.compile(r"^--- (PASS|FAIL|SKIP): ([A-Za-z0-9_]+)/")


def parse_log(log_path):
    """Return (top_level_results, subtest_results) name -> verdict."""
    top = {}
    sub = {}
    with open(log_path, errors="replace") as fh:
        for line in fh:
            m = TOP_LEVEL.match(line)
            if m:
                verdict, name = m.group(1), m.group(2)
                # A top-level line appears once per test; keep the
                # first, so a later duplicate cannot overwrite it.
                top.setdefault(name, verdict)
                continue
            m = SUBTEST.match(line)
            if m:
                verdict, name = m.group(1), m.group(2)
                sub.setdefault(name, verdict)
    return top, sub


def main(argv):
    if len(argv) < 3:
        print("usage: check-integration-report.py <log> <report> [required ...]",
              file=sys.stderr)
        return 2
    log_path, report_path = argv[1], argv[2]
    required = argv[3:] or DEFAULT_REQUIRED

    try:
        top, _sub = parse_log(log_path)
    except FileNotFoundError:
        with open(report_path, "w") as out:
            out.write("integration PASS=0 FAIL=0 SKIP=0 executed=0\n")
            out.write("VIOLATION: integration log missing: %s\n" % log_path)
        print("integration log missing: %s" % log_path, file=sys.stderr)
        return 1

    passed = sum(1 for v in top.values() if v == "PASS")
    failed = sum(1 for v in top.values() if v == "FAIL")
    skipped = sum(1 for v in top.values() if v == "SKIP")
    executed = passed + failed

    violations = []
    for entry in required:
        matched = [n for n, v in top.items()
                   if n == entry or n.startswith(entry)]
        if not matched:
            violations.append(
                "required test %s did not run (result: absent)" % entry)
        elif all(top[n] == "SKIP" for n in matched):
            violations.append(
                "required test %s only skipped (%s)"
                % (entry, ", ".join(sorted(matched))))
    if executed == 0:
        violations.append("no integration test actually ran (PASS+FAIL == 0)")
    if failed:
        violations.append("%d integration test(s) failed" % failed)

    with open(report_path, "w") as out:
        out.write(
            "integration PASS=%d FAIL=%d SKIP=%d executed=%d\n"
            % (passed, failed, skipped, executed))
        for name in sorted(top):
            out.write("%-5s %s\n" % (top[name], name))
        for v in violations:
            out.write("VIOLATION: %s\n" % v)

    with open(report_path) as fh:
        sys.stdout.write(fh.read())
    return 1 if violations else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

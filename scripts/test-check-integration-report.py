#!/usr/bin/env python3
"""Independent tests for scripts/check-integration-report.py.

Feeds fake `go test -v` logs to the parser and checks exit codes
and report contents for PASS, FAIL, SKIP, absent, prefix-match,
subtest-only, zero-test, and missing-log cases. No Go, no
services, no network: pure Python, offline.
"""

import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
PARSER = os.path.join(HERE, "check-integration-report.py")
REQUIRED = [
    "TestPostgresIntegration",
    "TestControlPlaneRedisIntegrationRuntimeLifecycle",
    "TestRedisIntegration",
]


def run_case(name, log_text, expect_exit, expect_in_report=()):
    with tempfile.TemporaryDirectory() as tmp:
        log_path = os.path.join(tmp, "test.log")
        report_path = os.path.join(tmp, "report.txt")
        if log_text is not None:
            with open(log_path, "w") as fh:
                fh.write(log_text)
        proc = subprocess.run(
            [sys.executable, PARSER, log_path, report_path] + REQUIRED,
            capture_output=True, text=True)
        report = ""
        if os.path.exists(report_path):
            with open(report_path) as fh:
                report = fh.read()
        ok = proc.returncode == expect_exit and all(
            s in report for s in expect_in_report)
        print(("PASS" if ok else "FAIL") + f" case {name}: "
              f"exit={proc.returncode} (expected {expect_exit})")
        if not ok:
            print("--- report ---")
            print(report)
            print("--- stderr ---")
            print(proc.stderr)
            return False
        return True


def main():
    cases = [
        ("pass", (
            "--- PASS: TestPostgresIntegrationStoreConformance (0.01s)\n"
            "--- PASS: TestControlPlaneRedisIntegrationRuntimeLifecycle (0.01s)\n"
            "--- PASS: TestRedisIntegrationStoreConformance (0.01s)\n"
        ), 0, ["integration PASS=3 FAIL=0 SKIP=0 executed=3"]),
        ("fail", (
            "--- PASS: TestPostgresIntegrationStoreConformance (0.01s)\n"
            "--- FAIL: TestControlPlaneRedisIntegrationRuntimeLifecycle (0.01s)\n"
            "--- PASS: TestRedisIntegrationStoreConformance (0.01s)\n"
        ), 1, ["FAIL=1", "VIOLATION: 1 integration test(s) failed"]),
        ("skip", (
            "--- PASS: TestPostgresIntegrationStoreConformance (0.01s)\n"
            "--- SKIP: TestControlPlaneRedisIntegrationRuntimeLifecycle (0.00s)\n"
            "--- PASS: TestRedisIntegrationStoreConformance (0.01s)\n"
        ), 1, ["SKIP=1", "only skipped"]),
        ("absent", (
            "--- PASS: TestPostgresIntegrationStoreConformance (0.01s)\n"
            "--- PASS: TestRedisIntegrationStoreConformance (0.01s)\n"
        ), 1, ["did not run (result: absent)",
               "TestControlPlaneRedisIntegrationRuntimeLifecycle"]),
        ("prefix", (
            # A CI -run prefix is satisfied by a longer real name.
            "--- PASS: TestPostgresIntegrationTwoInstancesAndRestart (0.01s)\n"
            "--- PASS: TestControlPlaneRedisIntegrationRuntimeLifecycle (0.01s)\n"
            "--- PASS: TestRedisIntegrationTwoClientsAndCorruption (0.01s)\n"
        ), 0, ["executed=3"]),
        ("subtest-only", (
            # Subtests alone never satisfy a required entry.
            "--- PASS: TestPostgresIntegrationStoreConformance/Sub (0.01s)\n"
            "--- PASS: TestControlPlaneRedisIntegrationRuntimeLifecycle (0.01s)\n"
            "--- PASS: TestRedisIntegrationStoreConformance (0.01s)\n"
        ), 1, ["did not run (result: absent)", "TestPostgresIntegration"]),
        ("zero", "", 1, ["executed=0", "no integration test actually ran"]),
        ("missing-log", None, 1, ["integration log missing"]),
    ]
    results = [run_case(name, log, code, expects)
               for name, log, code, expects in cases]
    if all(results):
        print("INTEGRATION REPORT TEST PASS (%d cases)" % len(results))
        return 0
    print("INTEGRATION REPORT TEST FAIL", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Tests for scripts/check-coverage-threshold.py (stdlib unittest only).

Covers the cases the gate must get right: exact-threshold pass,
below-threshold failure, boundary rounding, malformed input, missing
or empty artifacts, total-line mismatch (untrusted summary), zero
statements, negative values, bad arguments, and unexpected command
failures surfaced as non-zero exits (never as a pass).
"""

import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "check-coverage-threshold.py")


def profile_text(blocks):
    lines = ["mode: set"]
    for path, start, end, num_stmt, count in blocks:
        lines.append(
            "%s:%d.1,%d.1 %d %d" % (path, start, end, num_stmt, count)
        )
    return "\n".join(lines) + "\n"


def run_gate(profile_path, extra_args=None):
    cmd = [sys.executable, SCRIPT, "--profile", profile_path,
           "--commit", "testsha", "--go-version", "go1.27.3"]
    cmd += list(extra_args or [])
    return subprocess.run(cmd, capture_output=True, text=True, timeout=60)


class GateTests(unittest.TestCase):
    def write(self, text):
        tmp = tempfile.NamedTemporaryFile(
            "w", suffix=".out", delete=False, encoding="utf-8")
        tmp.write(text)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)
        return tmp.name

    def test_exact_threshold_passes(self):
        # 95 of 100 statements covered == 95.00%.
        path = self.write(profile_text([
            ("a/a.go", 1, 10, 95, 1), ("a/b.go", 1, 10, 5, 0)]))
        result = run_gate(path)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("coverage gate: PASS", result.stdout)
        self.assertIn("covered statements: 95", result.stdout)
        self.assertIn("total statements: 100", result.stdout)
        self.assertIn("coverage: 95.00%", result.stdout)

    def test_below_threshold_fails(self):
        # 94 of 100 statements covered == 94.00%.
        path = self.write(profile_text([
            ("a/a.go", 1, 10, 94, 1), ("a/b.go", 1, 10, 6, 0)]))
        result = run_gate(path)
        self.assertEqual(result.returncode, 1)
        self.assertIn("below threshold", result.stderr)

    def test_just_below_boundary_fails(self):
        # 9499/10000 == 94.99%: below 95 even after rounding display.
        path = self.write(profile_text([
            ("a/a.go", 1, 10, 9499, 1), ("a/b.go", 1, 10, 501, 0)]))
        result = run_gate(path)
        self.assertEqual(result.returncode, 1)

    def test_just_above_boundary_passes(self):
        # 9501/10000 == 95.01%.
        path = self.write(profile_text([
            ("a/a.go", 1, 10, 9501, 1), ("a/b.go", 1, 10, 499, 0)]))
        result = run_gate(path)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_full_coverage_passes(self):
        path = self.write(profile_text([("a/a.go", 1, 10, 50, 3)]))
        result = run_gate(path)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("coverage: 100.00%", result.stdout)

    def test_custom_threshold(self):
        path = self.write(profile_text([
            ("a/a.go", 1, 10, 90, 1), ("a/b.go", 1, 10, 10, 0)]))
        self.assertEqual(run_gate(path, ["--threshold", "90.0"]).returncode, 0)
        self.assertEqual(run_gate(path, ["--threshold", "90.1"]).returncode, 1)

    def test_malformed_line_fails_closed(self):
        path = self.write("mode: set\nthis is not a profile line\n")
        result = run_gate(path)
        self.assertEqual(result.returncode, 2)
        self.assertIn("malformed", result.stderr)

    def test_wrong_mode_header_fails_closed(self):
        path = self.write(profile_text([("a/a.go", 1, 10, 10, 1)])
                          .replace("mode: set", "mode: count", 1))
        result = run_gate(path)
        self.assertEqual(result.returncode, 2)

    def test_missing_file_fails_closed(self):
        result = run_gate("/nonexistent/coverage.out")
        self.assertEqual(result.returncode, 2)
        self.assertIn("cannot read", result.stderr)

    def test_empty_file_fails_closed(self):
        path = self.write("")
        result = run_gate(path)
        self.assertEqual(result.returncode, 2)

    def test_header_only_fails_closed(self):
        path = self.write("mode: set\n")
        result = run_gate(path)
        self.assertEqual(result.returncode, 2)
        self.assertIn("zero statements", result.stderr)

    def test_total_line_match_passes(self):
        path = self.write(profile_text([
            ("a/a.go", 1, 10, 95, 1), ("a/b.go", 1, 10, 5, 0)]))
        result = run_gate(path, ["--total-line", "total: (statements) 95.0%"])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_total_line_mismatch_fails_closed(self):
        # Summary claims 99% but the profile recomputes to 95%: untrusted.
        path = self.write(profile_text([
            ("a/a.go", 1, 10, 95, 1), ("a/b.go", 1, 10, 5, 0)]))
        result = run_gate(path, ["--total-line", "total: (statements) 99.0%"])
        self.assertEqual(result.returncode, 2)
        self.assertIn("untrusted", result.stderr)

    def test_malformed_total_line_fails_closed(self):
        path = self.write(profile_text([("a/a.go", 1, 10, 100, 1)]))
        result = run_gate(path, ["--total-line", "total: 100%"])
        self.assertEqual(result.returncode, 2)

    def test_bad_arguments_fail_closed(self):
        result = subprocess.run(
            [sys.executable, SCRIPT, "--profile"],
            capture_output=True, text=True, timeout=60)
        self.assertNotEqual(result.returncode, 0)

    def test_report_carries_commit_and_toolchain(self):
        path = self.write(profile_text([("a/a.go", 1, 10, 100, 1)]))
        result = run_gate(path)
        self.assertIn("commit: testsha", result.stdout)
        self.assertIn("go version: go1.27.3", result.stdout)
        self.assertIn("threshold: 95.00%", result.stdout)


if __name__ == "__main__":
    unittest.main()

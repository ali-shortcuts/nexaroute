#!/usr/bin/env python3
"""Regression tests for scripts/analysis-annotations.py."""

import json
import pathlib
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
HELPER = ROOT / "scripts" / "analysis-annotations.py"


def run(*args):
    return subprocess.run(
        [sys.executable, str(HELPER), *args],
        check=True,
        capture_output=True,
        cwd=ROOT,
        encoding="utf-8",
    ).stdout


def main():
    with tempfile.TemporaryDirectory() as directory:
        report = pathlib.Path(directory) / "golangci.json"
        report.write_text(
            json.dumps(
                {
                    "Issues": [
                        {
                            "FromLinter": "revive",
                            "Text": "message: retain 100%, safely",
                            "Pos": {
                                "Filename": "internal/example/file.go",
                                "Line": 12,
                                "Column": 3,
                            },
                        }
                    ]
                }
            ),
            encoding="utf-8",
        )
        output = run("golangci", str(report))
    expected = (
        "::error file=internal/example/file.go,line=12,col=3,title=revive::"
        "message: retain 100%25, safely\n"
    )
    if output != expected:
        raise AssertionError("annotation output = %r, want %r" % (output, expected))

    missing = run("golangci", "does-not-exist.json")
    if "report unavailable" not in missing:
        raise AssertionError("missing-report notice = %r" % missing)

    with tempfile.TemporaryDirectory() as directory:
        report = pathlib.Path(directory) / "coverage-test.txt"
        report.write_text(
            "--- FAIL: TestExample (0.00s)\n    internal/example_test.go:17: wanted 100% output\n",
            encoding="utf-8",
        )
        output = run("gotest", str(report))
    expected = "::error title=Go test failure::internal/example_test.go:17: wanted 100%25 output\n"
    if output != expected:
        raise AssertionError("test annotation output = %r, want %r" % (output, expected))

    print("analysis annotation regression PASS")


if __name__ == "__main__":
    main()

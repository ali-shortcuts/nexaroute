#!/usr/bin/env python3
"""Fail-closed 95% repository-wide Go statement-coverage gate.

Reads a `go test -coverprofile` profile (and, when given, the
`go tool cover -func` total line), recomputes covered/total statements
from the profile itself, and exits non-zero unless coverage is at or
above the threshold.

Fail-closed rules (any of these is an error, exit code 2):
  - profile file missing, unreadable, or empty;
  - missing `mode:` header, or a header other than `mode: set`;
  - any malformed profile line, negative statement count, or negative
    execution count;
  - zero total statements (nothing was measured);
  - a supplied `--total-line` that does not parse, or whose
    covered/total figures disagree with the recomputed profile figures
    (the profile is the authority; a mismatched summary is untrusted);
  - coverage below `--threshold` (exit code 1, a gate failure rather
    than an input error).

On success the script prints a deterministic report: commit SHA,
Go version, covered statements, total statements, percentage, and
threshold, one `key: value` line each. Nothing is inferred from a
missing input: missing evidence is an error, never a pass.
"""

import argparse
import re
import sys

PROFILE_LINE_RE = re.compile(
    r"^(.+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$"
)
TOTAL_LINE_RE = re.compile(
    r"^total:\s+\(statements\)\s+(\d+)\.(\d+)%$"
)


class GateError(Exception):
    """Input or trust failure: fail closed."""


def parse_profile(path):
    """Return (covered, total) recomputed from the coverprofile."""
    try:
        handle = open(path, "r", encoding="utf-8")
    except OSError as exc:
        raise GateError("cannot read profile %r: %s" % (path, exc))
    with handle:
        lines = handle.read().splitlines()
    if not lines:
        raise GateError("profile %r is empty" % path)
    if lines[0] != "mode: set":
        raise GateError(
            "profile %r has unexpected header %r (want 'mode: set')"
            % (path, lines[0])
        )
    covered = 0
    total = 0
    for lineno, line in enumerate(lines[1:], start=2):
        match = PROFILE_LINE_RE.match(line)
        if match is None:
            raise GateError(
                "profile %r line %d is malformed: %r" % (path, lineno, line)
            )
        num_stmt = int(match.group(6))
        count = int(match.group(7))
        if num_stmt < 0 or count < 0:
            raise GateError(
                "profile %r line %d has a negative value: %r"
                % (path, lineno, line)
            )
        total += num_stmt
        if count > 0:
            covered += num_stmt
    if total == 0:
        raise GateError("profile %r measures zero statements" % path)
    return covered, total


def parse_total_line(text):
    """Parse a `go tool cover -func` total line into (covered, total)."""
    text = text.strip()
    match = TOTAL_LINE_RE.match(text)
    if match is None:
        raise GateError("total line is malformed: %r" % text)
    percent = float("%s.%s" % (match.group(1), match.group(2)))
    return percent


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Fail-closed Go coverage threshold gate."
    )
    parser.add_argument("--profile", required=True,
                        help="path to coverage.out")
    parser.add_argument("--total-line", default="",
                        help="total line from `go tool cover -func`")
    parser.add_argument("--threshold", type=float, default=95.0,
                        help="required coverage percentage (default 95.0)")
    parser.add_argument("--commit", default="unknown",
                        help="tested commit SHA for the report")
    parser.add_argument("--go-version", default="unknown",
                        help="Go toolchain version for the report")
    args = parser.parse_args(argv)

    try:
        covered, total = parse_profile(args.profile)
        if args.total_line:
            percent = parse_total_line(args.total_line)
            recomputed = 100.0 * covered / total
            # The summary is rounded to one decimal; allow exactly that
            # rounding, nothing more.
            if abs(round(recomputed, 1) - percent) > 0.05:
                raise GateError(
                    "total line %.1f%% disagrees with recomputed "
                    "%.2f%% (%d/%d); summary is untrusted"
                    % (percent, recomputed, covered, total)
                )
    except GateError as exc:
        print("coverage gate: ERROR: %s" % exc, file=sys.stderr)
        return 2

    percentage = 100.0 * covered / total
    print("coverage gate report:")
    print("commit: %s" % args.commit)
    print("go version: %s" % args.go_version)
    print("covered statements: %d" % covered)
    print("total statements: %d" % total)
    print("coverage: %.2f%%" % percentage)
    print("threshold: %.2f%%" % args.threshold)
    if percentage < args.threshold:
        print(
            "coverage gate: FAIL: %.2f%% is below threshold %.2f%%"
            % (percentage, args.threshold),
            file=sys.stderr,
        )
        return 1
    print("coverage gate: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""gosec baseline ratchet (ADR 0006, Option A).

gosec has no native baseline file, so this script provides one. It
turns a gosec JSON report into a stable, line-number-free fingerprint
list (rule, repo-relative file, the flagged source line with whitespace
collapsed) and compares it against the checked-in baseline:

    python3 scripts/gosec-baseline.py check gosec.json docs/reports/gosec-baseline.txt
    python3 scripts/gosec-baseline.py write gosec.json docs/reports/gosec-baseline.txt

`check` fails (exit 1) when:
  * the report carries any "Golang errors" -- package-loading failures
    mean gosec's SSA/taint rules silently produced nothing, so the
    finding list is incomplete and must never be trusted as "clean"
    (this failure mode hid every G703/G704 finding in CI until
    2026-10-22; see ADR 0006), or
  * the report contains a finding that is not in the baseline.

Findings present in the baseline but absent from the report are
printed as resolved (information only): removing a baseline entry is a
deliberate, reviewable cleanup, never a failure.

`write` regenerates the baseline from a report. Only run it after
triaging every new finding at source; a baseline entry is an
acceptance record, not a way to silence the scanner.
"""

import json
import os
import sys

HEADER = """\
# gosec accepted-findings baseline -- see scripts/gosec-baseline.py and
# docs/adr/0006-lint-ratchet-gating.md.
#
# One line per accepted finding: RULE <TAB> repo-relative file <TAB>
# flagged source line (whitespace collapsed, no line numbers, so the
# baseline survives unrelated edits). Every entry was triaged at
# source; the 2026-10-22 triage found all of them to be
# operator-controlled boundary operations (CLI flags, env vars, config
# values, constant bundle entry names) -- no untrusted request data
# reaches any of these sinks. Regenerate only with:
#   gosec -quiet -fmt=json -out=gosec.json ./...
#   python3 scripts/gosec-baseline.py write gosec.json docs/reports/gosec-baseline.txt
# and review the diff like code.
"""


def target_line(issue):
    """Extract the flagged source line from a gosec issue's code field.

    gosec renders `code` either as the raw line or as up to three
    numbered lines ("230: ...\\n231: ...\\n232: ...") around the
    finding; in the numbered form the segment whose number equals the
    issue's line is the flagged one.
    """
    code = issue.get("code") or ""
    want = str(issue.get("line") or "")
    for part in code.split("\n"):
        head, sep, rest = part.partition(":")
        if sep and head.strip() == want and head.strip().isdigit():
            return " ".join(rest.split())
    return " ".join(code.split())


def fingerprints(report_path, repo_root):
    with open(report_path, encoding="utf-8") as fh:
        report = json.load(fh)
    errors = report.get("Golang errors") or {}
    lines = []
    for issue in report.get("Issues") or []:
        path = os.path.relpath(issue.get("file") or "", repo_root)
        lines.append("%s\t%s\t%s" % (issue.get("rule_id"), path, target_line(issue)))
    lines.sort()
    return lines, errors


def load_baseline(path):
    entries = []
    with open(path, encoding="utf-8") as fh:
        for raw in fh:
            line = raw.rstrip("\n")
            if line and not line.startswith("#"):
                entries.append(line)
    entries.sort()
    return entries


def main(argv):
    if len(argv) != 4 or argv[1] not in ("check", "write"):
        print(__doc__)
        return 2
    mode, report_path, baseline_path = argv[1], argv[2], argv[3]
    repo_root = os.getcwd()
    current, load_errors = fingerprints(report_path, repo_root)

    if load_errors:
        print("ERROR: gosec reported package-loading errors for %d file(s);"
              " its SSA/taint rules silently produced nothing." % len(load_errors))
        print("The finding list is incomplete and cannot be gated on.")
        print("First error: %s" % next(iter(load_errors.items()))[1][:1])
        return 1

    if mode == "write":
        with open(baseline_path, "w", encoding="utf-8") as fh:
            fh.write(HEADER)
            for line in current:
                fh.write(line + "\n")
        print("wrote %d accepted findings to %s" % (len(current), baseline_path))
        return 0

    baseline = load_baseline(baseline_path)
    new = sorted(set(current) - set(baseline))
    resolved = sorted(set(baseline) - set(current))
    for line in resolved:
        print("resolved (still in baseline, no longer reported): %s" % line)
    if new:
        print("ERROR: %d gosec finding(s) not in the accepted baseline:" % len(new))
        for line in new:
            print("  NEW: %s" % line)
        print("Triage each at source. Fix it, or -- only if it is an")
        print("operator-controlled boundary operation -- regenerate the")
        print("baseline with `write` and justify the addition in the PR.")
        return 1
    print("gosec baseline OK: %d finding(s), all accepted; %d resolved."
          % (len(current), len(resolved)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

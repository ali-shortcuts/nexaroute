#!/usr/bin/env python3
"""Publish analyzer findings as GitHub Actions annotations.

The Architecture & Quality Audit keeps full machine-readable reports as
artifacts, but those artifact downloads are not always reachable from every
review environment. This helper converts golangci-lint's JSON report into
check-run annotations, which remain visible through GitHub's API:

    python3 scripts/analysis-annotations.py golangci golangci.json

The helper is diagnostic only: it deliberately exits zero for an unavailable
or malformed report. The analyzer's own step remains the quality gate, while
this companion makes its findings reviewable.
"""

import json
import os
import sys

MAX_ANNOTATIONS = 50


def escape_data(value):
    """Escape a workflow-command message value."""
    return str(value).replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def escape_property(value):
    """Escape a workflow-command property value."""
    return (escape_data(value).replace(":", "%3A").replace(",", "%2C"))


def repo_path(path):
    """Turn a golangci path into a repo-relative slash-separated path."""
    path = (path or "").replace("\\", "/")
    if not path:
        return ""
    try:
        relative = os.path.relpath(path, os.getcwd())
    except ValueError:
        relative = path
    if not relative.startswith("../") and relative != "..":
        return relative.replace("\\", "/")
    # Runner paths normally end in the checked-out relative path. Avoid an
    # absolute path, which GitHub cannot attach to source in the check run.
    marker = "/nexaroute/"
    if marker in path:
        return path.rsplit(marker, 1)[1]
    return path.lstrip("./")


def annotation(issue):
    """Return the GitHub Actions command for one golangci JSON issue."""
    pos = issue.get("Pos") or issue.get("pos") or {}
    path = repo_path(pos.get("Filename") or pos.get("filename"))
    line = pos.get("Line") or pos.get("line")
    column = pos.get("Column") or pos.get("column")
    linter = issue.get("FromLinter") or issue.get("fromLinter") or "golangci-lint"
    text = issue.get("Text") or issue.get("text") or "golangci-lint reported a finding"

    properties = []
    if path:
        properties.append("file=" + escape_property(path))
    if isinstance(line, int) and line > 0:
        properties.append("line=" + str(line))
    if isinstance(column, int) and column > 0:
        properties.append("col=" + str(column))
    properties.append("title=" + escape_property(linter))
    return "::error %s::%s" % (",".join(properties), escape_data(text))


def annotate_golangci(report_path):
    try:
        with open(report_path, encoding="utf-8") as fh:
            report = json.load(fh)
    except FileNotFoundError:
        print("::notice title=golangci-lint report unavailable::No JSON report was written; the lint step remains authoritative.")
        return
    except (OSError, json.JSONDecodeError) as exc:
        print("::warning title=golangci-lint report unreadable::Cannot annotate lint findings: %s" % escape_data(exc))
        return

    issues = report.get("Issues") or report.get("issues") or []
    for issue in issues[:MAX_ANNOTATIONS]:
        if isinstance(issue, dict):
            print(annotation(issue))
    if len(issues) > MAX_ANNOTATIONS:
        print(
            "::warning title=golangci-lint annotation limit::"
            "Annotated the first %d of %d findings; see golangci.json for the rest."
            % (MAX_ANNOTATIONS, len(issues))
        )


def main(argv):
    if len(argv) != 3 or argv[1] != "golangci":
        print(__doc__)
        return 2
    annotate_golangci(argv[2])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

#!/usr/bin/env python3
"""Validate a soak / CI-soak-smoke summary.json (issue #664).

The soak and smoke scripts already record commit, Go version,
environment, durations, workload parameters, per-stage results,
resource samples and failure summaries. This checker is the gate that
proves the record is actually complete before anyone quotes it:

- the summary parses and carries the expected schema for its kind;
- the commit is a full 40-hex SHA (and matches --expected-commit);
- finished > started and total_duration_seconds agrees with the
  timestamps and covers the requested sustained duration;
- every stage has a status, an exit code consistent with that status,
  and a non-negative duration; no stage is SKIPPED unless it is named
  in --allow-skipped (the smoke script legitimately skips its race
  stage when NEXAROUTE_CI_SMOKE_RACE=0; the long soak allows none);
- the overall result agrees with the stages (PASS only when every
  stage passed and none was skipped);
- when the hot-reload stage passed, its resource-sample file exists
  next to the summary and is non-empty;
- every failed stage carries a non-empty failure_summary, and the
  report names the live items it deliberately does NOT claim
  (the not_run block).

Exit codes: 0 valid, 1 invalid (errors printed, one per line),
2 usage error (missing file, bad JSON, bad flag). Warnings never
change the exit code; they flag schema gaps in older summaries
(for example a pre-validator soak summary without a
resource_sample_summary) so a reader can tell a schema gap apart
from a run failure.

Usage:
  python3 scripts/check-soak-evidence.py --summary PATH --kind soak
      [--expected-commit SHA] [--expected-duration-seconds N]
      [--allow-skipped STAGE ...]
  python3 scripts/check-soak-evidence.py --summary PATH --kind ci-smoke
      [--expected-commit SHA] [--allow-skipped race-soak-short]

No credentials are read, printed, or recorded here: the summary is
sanitized by construction, and this checker only reads it.
"""

import argparse
import json
import os
import re
import sys
from datetime import datetime, timezone

SCHEMAS = {
    "soak": "nexaroute.soak.qualification.v1",
    "ci-smoke": "nexaroute.ci-soak-smoke.v1",
}

# Stage that carries the sustained mixed workload in each kind.
SUSTAINED_STAGE = {
    "soak": "http-hot-reload-soak",
    "ci-smoke": "soak-hot-reload",
}

SHA_RE = re.compile(r"^[0-9a-f]{40}$")


def parse_ts(value):
    """Parse an RFC3339 UTC timestamp; return epoch seconds or None."""
    if not isinstance(value, str):
        return None
    text = value.strip()
    try:
        if text.endswith("Z"):
            dt = datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ")
            return dt.replace(tzinfo=timezone.utc).timestamp()
        dt = datetime.fromisoformat(text)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except ValueError:
        return None


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Validate a soak/ci-smoke summary.json."
    )
    parser.add_argument("--summary", required=True,
                        help="Path to summary.json.")
    parser.add_argument("--kind", required=True, choices=list(SCHEMAS),
                        help="Kind of summary to validate.")
    parser.add_argument("--expected-commit", default=None,
                        help="Full 40-hex SHA the summary must name.")
    parser.add_argument("--expected-duration-seconds", type=int, default=None,
                        help="Requested sustained duration the run must cover.")
    parser.add_argument("--allow-skipped", nargs="*", default=[],
                        help="Stage names allowed to be SKIPPED.")
    args = parser.parse_args(argv)

    errors = []
    warnings = []

    def err(message):
        errors.append(message)

    if not os.path.isfile(args.summary):
        print(f"check-soak-evidence: summary not found: {args.summary}",
              file=sys.stderr)
        return 2
    try:
        with open(args.summary, "r", encoding="utf-8") as handle:
            doc = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        print(f"check-soak-evidence: cannot read summary: {exc}",
              file=sys.stderr)
        return 2
    if not isinstance(doc, dict):
        print("check-soak-evidence: summary is not a JSON object",
              file=sys.stderr)
        return 2

    summary_dir = os.path.dirname(os.path.abspath(args.summary))

    # Schema and commit.
    if doc.get("schema") != SCHEMAS[args.kind]:
        err(f"schema: expected {SCHEMAS[args.kind]!r}, "
            f"got {doc.get('schema')!r}")
    commit = doc.get("commit")
    if not isinstance(commit, str) or not SHA_RE.match(commit):
        err(f"commit: expected a full 40-hex SHA, got {commit!r}")
    elif args.expected_commit and commit != args.expected_commit:
        err(f"commit: summary names {commit}, expected "
            f"{args.expected_commit}")

    # Timestamps and total duration.
    started = parse_ts(doc.get("started_at"))
    finished = parse_ts(doc.get("finished_at"))
    if started is None:
        err(f"started_at: unparseable: {doc.get('started_at')!r}")
    if finished is None:
        err(f"finished_at: unparseable: {doc.get('finished_at')!r}")
    total = doc.get("total_duration_seconds")
    if not isinstance(total, int) or isinstance(total, bool) or total <= 0:
        err(f"total_duration_seconds: expected a positive integer, "
            f"got {total!r}")
    elif started is not None and finished is not None:
        if finished <= started:
            err("timestamps: finished_at is not after started_at")
        wall = finished - started
        if abs(total - wall) > 5:
            err(f"total_duration_seconds: {total} disagrees with "
                f"started/finished wall time {wall:.0f}s by more than 5s")

    # Environment: Go version, host identity, CPU count.
    env = doc.get("environment")
    if not isinstance(env, dict):
        err("environment: missing or not an object")
    else:
        go = env.get("go", "")
        if not isinstance(go, str) or "go1." not in go:
            err(f"environment.go: expected a Go version string, got {go!r}")
        for key in ("uname", "nproc"):
            value = env.get(key)
            if not isinstance(value, str) or not value.strip() \
                    or value == "unknown":
                err(f"environment.{key}: missing or unknown: {value!r}")

    # Workload parameters.
    if args.kind == "soak":
        rounds = doc.get("stress_rounds")
        if not isinstance(rounds, int) or rounds < 1:
            err(f"stress_rounds: expected a positive integer, got {rounds!r}")
        requested = doc.get("requested_duration_seconds")
        if not isinstance(requested, int) or requested < 0:
            err(f"requested_duration_seconds: expected a non-negative "
                f"integer, got {requested!r}")
        elif args.expected_duration_seconds is not None \
                and requested != args.expected_duration_seconds:
            err(f"requested_duration_seconds: summary says {requested}, "
                f"expected {args.expected_duration_seconds}")
    else:
        params = doc.get("parameters")
        if not isinstance(params, dict):
            err("parameters: missing or not an object")
        else:
            requested = params.get("soak_duration_seconds")
            if not isinstance(requested, int) or requested < 0:
                err(f"parameters.soak_duration_seconds: expected a "
                    f"non-negative integer, got {requested!r}")
            if args.expected_duration_seconds is not None \
                    and requested != args.expected_duration_seconds:
                err(f"parameters.soak_duration_seconds: summary says "
                    f"{requested}, expected "
                    f"{args.expected_duration_seconds}")

    # Stages.
    stages = doc.get("stages")
    if not isinstance(stages, list) or not stages:
        err("stages: missing or empty; a run with no stages proves nothing")
        stages = []
    names = []
    skipped = []
    failed = []
    sustained_duration = None
    hot_reload_passed = False
    for index, stage in enumerate(stages):
        if not isinstance(stage, dict):
            err(f"stages[{index}]: not an object")
            continue
        name = stage.get("name")
        status = stage.get("status")
        exit_code = stage.get("exit_code")
        duration = stage.get("duration_seconds")
        names.append(name)
        if not isinstance(name, str) or not name:
            err(f"stages[{index}]: missing name")
            continue
        if status not in ("PASS", "FAIL", "FAIL(timeout)", "SKIPPED",
                          "BLOCKED", "CANCELLED"):
            err(f"stage {name}: unknown status {status!r}")
        if not isinstance(duration, int) or isinstance(duration, bool) \
                or duration < 0:
            err(f"stage {name}: duration_seconds expected a non-negative "
                f"integer, got {duration!r}")
        if status == "PASS" and exit_code != 0:
            err(f"stage {name}: status PASS but exit_code {exit_code!r}")
        if status in ("FAIL", "FAIL(timeout)") and \
                (not isinstance(exit_code, int) or exit_code == 0):
            err(f"stage {name}: status {status} but exit_code "
                f"{exit_code!r}")
        if status == "SKIPPED":
            skipped.append(name)
        if status in ("FAIL", "FAIL(timeout)"):
            failed.append(name)
            summary_text = stage.get("failure_summary")
            if not isinstance(summary_text, str) or not summary_text.strip():
                err(f"stage {name}: failed but failure_summary is missing "
                    f"or empty; a failure without its lines cannot be "
                    f"triaged from the report")
        if "failure_summary" not in stage:
            warnings.append(
                f"stage {name}: no failure_summary key (older schema; "
                f"new summaries carry one per stage)")
        if name == SUSTAINED_STAGE[args.kind]:
            sustained_duration = duration
            hot_reload_passed = status == "PASS"

    # Completeness: skipped stages make the run incomplete, unless
    # explicitly allowed (smoke race stage with RACE=0).
    unexpected_skipped = [n for n in skipped
                          if n not in args.allow_skipped]
    if unexpected_skipped:
        err(f"incomplete run: SKIPPED stages not allowed here: "
            f"{', '.join(unexpected_skipped)}")

    # Sustained duration actually covered.
    requested_value = None
    if args.kind == "soak":
        requested_value = doc.get("requested_duration_seconds")
    elif isinstance(doc.get("parameters"), dict):
        requested_value = doc["parameters"].get("soak_duration_seconds")
    if isinstance(requested_value, int) and requested_value > 0:
        if sustained_duration is None:
            err(f"sustained stage {SUSTAINED_STAGE[args.kind]!r} missing; "
                f"cannot verify the requested {requested_value}s duration")
        elif sustained_duration < requested_value:
            err(f"sustained stage {SUSTAINED_STAGE[args.kind]} ran "
                f"{sustained_duration}s, less than requested "
                f"{requested_value}s; this is not evidence for that duration")
        if isinstance(total, int) and total < requested_value:
            err(f"total_duration_seconds {total} is less than requested "
                f"sustained duration {requested_value}s")

    # Resource samples: a passed hot-reload stage must have them.
    samples = doc.get("resource_samples")
    if hot_reload_passed:
        if not samples:
            err("resource_samples: hot-reload stage passed but no sample "
                "file is recorded")
        else:
            # The recorded path is absolute on the runner that produced
            # it; a downloaded copy lives next to the summary instead,
            # so try the recorded path first, then its basename beside
            # the summary. Either location counts; neither may be empty.
            candidates = [samples]
            local_copy = os.path.join(summary_dir,
                                      os.path.basename(samples))
            if local_copy not in candidates:
                candidates.append(local_copy)
            path = next((c for c in candidates if os.path.isfile(c)), None)
            if path is None:
                err(f"resource_samples: recorded file found neither at "
                    f"its recorded path nor next to the summary: "
                    f"{samples!r}")
            elif os.path.getsize(path) == 0:
                err(f"resource_samples: recorded file is empty: "
                    f"{path!r}")
    if "resource_sample_summary" not in doc:
        warnings.append("no resource_sample_summary (older schema; new "
                        "summaries carry peak goroutines/FDs/heap)")

    # Overall result must agree with the stages.
    result = doc.get("result")
    if result == "PASS":
        if failed or unexpected_skipped:
            err(f"result PASS contradicts stages: failed={failed} "
                f"skipped={unexpected_skipped}")
    elif result in ("FAIL", "FAIL(timeout)", "BLOCKED", "CANCELLED"):
        if not failed and not unexpected_skipped and not skipped:
            warnings.append(
                f"result {result} with no failed or skipped stage; "
                f"check whether the failure was environmental")
    else:
        err(f"result: unknown value {result!r}")

    # The report must name what it does NOT claim.
    not_run = doc.get("not_run")
    if not isinstance(not_run, list) or not not_run:
        err("not_run: missing or empty; the report must name the live "
            "items it does not claim (BLOCKED/NOT_RUN)")
    else:
        for entry in not_run:
            if not isinstance(entry, dict) or not entry.get("item") \
                    or entry.get("status") not in ("BLOCKED", "NOT_RUN"):
                err(f"not_run: malformed entry: {entry!r}")

    for warning in warnings:
        print(f"check-soak-evidence: warning: {warning}", file=sys.stderr)
    if errors:
        for message in errors:
            print(f"check-soak-evidence: error: {message}", file=sys.stderr)
        print(f"check-soak-evidence: INVALID ({len(errors)} error(s)): "
              f"{args.summary}", file=sys.stderr)
        return 1
    print(f"check-soak-evidence: VALID ({args.kind}): {args.summary} "
          f"commit={commit} stages={len(stages)} "
          f"total={total}s result={result}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

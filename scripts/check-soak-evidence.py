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
- the runtime identity is present and well-formed (a `go version`
  string, a non-empty host identity, a numeric CPU count);
- every planned stage is present exactly once (the soak producer
  lists unreached stages as SKIPPED, so a missing stage is a gap,
  not an omission the reader can forgive);
- every stage has a status, an exit code consistent with that status,
  and a non-negative duration; no stage is SKIPPED unless it is named
  in --allow-skipped (the smoke script legitimately skips its race
  stage when NEXAROUTE_CI_SMOKE_RACE=0; the long soak allows none);
  a SKIPPED stage must carry a null exit code and zero duration,
  because it did not execute;
- the overall result and exit code agree with the stages (PASS only
  when no stage is failed, blocked, or cancelled and no stage is
  skipped beyond the explicitly allowed ones; a non-PASS overall
  result with every stage PASS is likewise inconsistent);
- for soak summaries, the stage durations sum to no more than the
  total duration (plus 5s tolerance), since soak stages run
  sequentially;
- when the hot-reload stage passed, its resource-sample file exists
  next to the summary and is non-empty;
- every failed stage carries a non-empty failure_summary, and the
  report names the live items it deliberately does NOT claim
  (the not_run block).

A report that is internally consistent but records a failure, a
timeout, or a cancellation is VALID evidence of that outcome: the
checker rejects incomplete or contradictory reports, not honest
failures. A cancelled run leaves its unreached stages SKIPPED, so it
needs --allow-skipped for those stages to validate as a complete
record of the cancellation.

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

# Fixed (non-round) stages each producer plans. Soak stress rounds
# are derived from the summary's stress_rounds (stress-round-1..N).
# The smoke list mirrors the planned= array in ci-soak-smoke.sh.
EXPECTED_FIXED_STAGES = {
    "soak": [
        "http-hot-reload-soak", "recovery-soak", "video-recovery-soak",
        "outage-provider", "outage-gameday", "race-soak",
        "budget-leak-slope",
    ],
    "ci-smoke": [
        "stress-router", "stress-probe", "stress-events", "stress-httpapi",
        "stress-logging", "stress-eval", "stress-video-cost",
        "stress-video-queue", "outage-provider", "outage-gameday",
        "cancellation", "soak-hot-reload", "soak-recovery-cycles",
        "soak-budget-leak", "soak-video-recovery", "race-soak-short",
    ],
}

SHA_RE = re.compile(r"^[0-9a-f]{40}$")
GO_RE = re.compile(r"^go version go1\.\d+(\.\d+)?(\s|$)")


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

    # Environment: Go version, host identity, CPU count. A bare
    # "go1." substring is not an identity: the value must be a
    # `go version` string, and nproc must be a number, so a report
    # with the runtime identity scrubbed or faked cannot pass.
    env = doc.get("environment")
    if not isinstance(env, dict):
        err("environment: missing or not an object")
    else:
        go = env.get("go", "")
        if not isinstance(go, str) or not GO_RE.match(go):
            err(f"environment.go: expected a `go version go1.x` string, "
                f"got {go!r}")
        uname = env.get("uname")
        if not isinstance(uname, str) or not uname.strip() \
                or uname == "unknown":
            err(f"environment.uname: missing or unknown: {uname!r}")
        nproc = env.get("nproc")
        if not isinstance(nproc, str) or not re.match(r"^[0-9]+$",
                                                      nproc or ""):
            err(f"environment.nproc: expected a CPU count, got {nproc!r}")

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
    non_pass = []
    blocked_or_cancelled = []
    sustained_duration = None
    hot_reload_passed = False
    stage_duration_sum = 0
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
        if names.count(name) > 1:
            err(f"stage {name}: listed more than once; a duplicated "
                f"stage cannot stand in for a missing one")
        if status not in ("PASS", "FAIL", "FAIL(timeout)", "SKIPPED",
                          "BLOCKED", "CANCELLED"):
            err(f"stage {name}: unknown status {status!r}")
        if not isinstance(duration, int) or isinstance(duration, bool) \
                or duration < 0:
            err(f"stage {name}: duration_seconds expected a non-negative "
                f"integer, got {duration!r}")
        else:
            stage_duration_sum += duration
        if status != "PASS":
            non_pass.append(name)
        if status == "PASS" and exit_code != 0:
            err(f"stage {name}: status PASS but exit_code {exit_code!r}")
        if status in ("FAIL", "FAIL(timeout)") and \
                (not isinstance(exit_code, int) or exit_code == 0):
            err(f"stage {name}: status {status} but exit_code "
                f"{exit_code!r}")
        if status == "SKIPPED":
            skipped.append(name)
            # A skipped stage did not execute: a non-null exit code
            # or a positive duration would claim work that never ran.
            if exit_code is not None:
                err(f"stage {name}: status SKIPPED but exit_code "
                    f"{exit_code!r}; an unexecuted stage has no exit code")
            if isinstance(duration, int) and duration != 0:
                err(f"stage {name}: status SKIPPED but duration_seconds "
                    f"{duration}; an unexecuted stage ran for 0s")
        if status in ("BLOCKED", "CANCELLED"):
            blocked_or_cancelled.append(name)
            if isinstance(exit_code, int) and exit_code == 0:
                err(f"stage {name}: status {status} but exit_code 0; a "
                    f"blocked or cancelled stage did not succeed")
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

    # Planned-stage presence: the producers list every planned stage
    # (unreached ones as SKIPPED), so a stage that is absent entirely
    # is a hole in the record, not a stage that was never planned.
    # Soak stress rounds come from the summary's own stress_rounds.
    expected = list(EXPECTED_FIXED_STAGES[args.kind])
    if args.kind == "soak":
        rounds = doc.get("stress_rounds")
        if isinstance(rounds, int) and not isinstance(rounds, bool) \
                and rounds >= 1:
            expected += [f"stress-round-{i}"
                         for i in range(1, rounds + 1)]
    missing = [n for n in expected if n not in names]
    if missing:
        err(f"missing stages: planned stages absent from the report: "
            f"{', '.join(missing)}")

    # Soak stages run sequentially, so their durations cannot exceed
    # the run's total duration by more than rounding tolerance.
    if args.kind == "soak" and isinstance(total, int) \
            and stage_duration_sum > total + 5:
        err(f"stage durations sum to {stage_duration_sum}s, more than "
            f"total_duration_seconds {total}s (+5s tolerance); "
            f"sequential stages cannot outlast the run")

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

    # Overall result and exit code must agree with the stages. A
    # PASS overall with any failed, skipped, blocked, or cancelled
    # stage labels unexecuted or failed work as a pass. Conversely
    # a non-PASS overall with every stage PASS claims a failure the
    # stages do not show. The top-level exit code must tell the same
    # story: 0 exactly for PASS, non-zero for FAIL, and null or
    # non-zero (never 0) for BLOCKED/CANCELLED.
    result = doc.get("result")
    overall_exit = doc.get("exit_code")
    if result == "PASS":
        contradictions = failed + unexpected_skipped + blocked_or_cancelled
        if contradictions:
            err(f"result PASS contradicts stages: non-PASS stages "
                f"{contradictions}")
        if overall_exit != 0:
            err(f"result PASS but top-level exit_code {overall_exit!r}; "
                f"a passing run exits 0")
    elif result in ("FAIL", "FAIL(timeout)"):
        if not non_pass:
            err(f"result {result} with every stage PASS; the stages "
                f"do not show the claimed failure")
        if not isinstance(overall_exit, int) or overall_exit == 0:
            err(f"result {result} but top-level exit_code "
                f"{overall_exit!r}; a failed run exits non-zero")
    elif result in ("BLOCKED", "CANCELLED"):
        if not non_pass:
            err(f"result {result} with every stage PASS; the stages "
                f"do not show the claimed block or cancellation")
        if isinstance(overall_exit, int) and overall_exit == 0:
            err(f"result {result} but top-level exit_code 0; a "
                f"blocked or cancelled run did not succeed")
    else:
        err(f"result: unknown value {result!r}")
    if "exit_code" not in doc:
        err("exit_code: missing; the report must record its own exit "
            "status")

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

#!/usr/bin/env python3
"""Deterministic tests for scripts/check-soak-evidence.py (issue #664).

No network, no clock, no real soak run: every case builds a synthetic
summary.json in a temporary directory with fixed timestamps, a fixed
40-hex commit, and a non-empty samples file next to the summary, then
runs the validator as a subprocess and asserts its exit code.

Run: python3 scripts/test-check-soak-evidence.py
"""

import copy
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
VALIDATOR = os.path.join(HERE, "check-soak-evidence.py")
SHA = "1234567890abcdef1234567890abcdef12345678"
OTHER_SHA = "abcdef1234567890abcdef1234567890abcdef12"

SOAK_STAGES = [
    "stress-round-1", "stress-round-2", "stress-round-3",
    "http-hot-reload-soak", "recovery-soak", "video-recovery-soak",
    "outage-provider", "outage-gameday", "race-soak", "budget-leak-slope",
]

SMOKE_STAGES = [
    "stress-router", "stress-probe", "stress-events", "stress-httpapi",
    "stress-logging", "stress-eval", "stress-video-cost",
    "stress-video-queue", "outage-provider", "outage-gameday",
    "cancellation", "soak-hot-reload", "soak-recovery-cycles",
    "soak-budget-leak", "soak-video-recovery", "race-soak-short",
]


def stage(name, status="PASS", exit_code=0, duration=10):
    return {
        "name": name,
        "status": status,
        "exit_code": exit_code,
        "duration_seconds": duration,
        "max_rss": "n/a",
        "log": "/tmp/fake.log",
        "failure_summary": "" if status == "PASS" else "see log",
    }


def soak_doc(**overrides):
    stages = [stage("stress-round-1", duration=44),
              stage("stress-round-2", duration=3),
              stage("stress-round-3", duration=4),
              stage("http-hot-reload-soak", duration=7209),
              stage("recovery-soak", duration=1),
              stage("video-recovery-soak", duration=1),
              stage("outage-provider", duration=30),
              stage("outage-gameday", duration=19),
              stage("race-soak", duration=183),
              stage("budget-leak-slope", duration=2)]
    doc = {
        "schema": "nexaroute.soak.qualification.v1",
        "commit": SHA,
        "started_at": "2026-10-29T14:23:53Z",
        "finished_at": "2026-10-29T16:28:49Z",
        "total_duration_seconds": 7496,
        "stress_rounds": 3,
        "requested_duration": "2h",
        "requested_duration_seconds": 7200,
        "result": "PASS",
        "exit_code": 0,
        "environment": {
            "go": "go version go1.27.3 linux/amd64",
            "uname": "Linux test-runner x86_64 GNU/Linux",
            "nproc": "4",
            "cpu": "Test CPU",
            "gomaxprocs": "auto",
        },
        "resource_samples": "samples.jsonl",
        "resource_sample_summary": "samples=125 peak_goroutines=119",
        "stages": stages,
        "not_run": [
            {"item": "live provider canaries", "status": "BLOCKED"},
            {"item": "production failover drill", "status": "NOT_RUN"},
        ],
    }
    doc.update(overrides)
    return doc


def smoke_doc(**overrides):
    stages = [stage(n, duration=5) for n in SMOKE_STAGES]
    # The smoke producer writes no failure_summary key; drop it here
    # so the fixture matches the producer's real shape.
    for s in stages:
        s.pop("failure_summary", None)
    doc = {
        "schema": "nexaroute.ci-soak-smoke.v1",
        "commit": SHA,
        "started_at": "2026-10-29T10:00:00Z",
        "finished_at": "2026-10-29T10:17:15Z",
        "total_duration_seconds": 1035,
        "result": "PASS",
        "exit_code": 0,
        "parameters": {
            "stage_timeout_seconds": 120,
            "soak_duration": "0s",
            "soak_duration_seconds": 0,
            "race_enabled": True,
        },
        "environment": {
            "go": "go version go1.27.3 linux/amd64",
            "uname": "Linux test-runner x86_64 GNU/Linux",
            "nproc": "4",
        },
        "resource_samples": "samples.jsonl",
        "resource_sample_summary": "samples=10 peak_goroutines=50",
        "stages": stages,
        "not_run": [
            {"item": "long-form multi-hour soak", "status": "NOT_RUN"},
        ],
    }
    doc.update(overrides)
    return doc


class ValidatorTest(unittest.TestCase):
    def run_validator(self, doc, kind="soak", extra_args=()):
        with tempfile.TemporaryDirectory() as tmp:
            summary = os.path.join(tmp, "summary.json")
            with open(summary, "w", encoding="utf-8") as handle:
                json.dump(doc, handle)
            # Samples file next to the summary (the downloaded-copy
            # location the validator falls back to).
            with open(os.path.join(tmp, "samples.jsonl"), "w") as handle:
                handle.write('{"goroutines": 10}\n')
            cmd = [sys.executable, VALIDATOR, "--summary", summary,
                   "--kind", kind, "--expected-commit", SHA]
            cmd.extend(extra_args)
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  timeout=30)
            return proc.returncode, proc.stderr

    def assert_valid(self, doc, kind="soak", extra_args=()):
        code, err = self.run_validator(doc, kind, extra_args)
        self.assertEqual(code, 0, f"expected VALID, got {code}: {err}")

    def assert_invalid(self, doc, kind="soak", extra_args=(), needle=None):
        code, err = self.run_validator(doc, kind, extra_args)
        self.assertEqual(code, 1, f"expected INVALID, got {code}: {err}")
        if needle is not None:
            self.assertIn(needle, err)

    # Valid evidence -------------------------------------------------
    def test_valid_soak_pass(self):
        self.assert_valid(soak_doc())

    def test_valid_soak_failed_is_valid_evidence_of_failure(self):
        doc = soak_doc()
        doc["stages"][6] = stage("outage-provider", status="FAIL",
                                  exit_code=1, duration=30)
        doc["stages"][6]["failure_summary"] = "FAIL: provider refused"
        doc["result"] = "FAIL"
        doc["exit_code"] = 1
        self.assert_valid(doc)

    def test_valid_soak_timed_out(self):
        doc = soak_doc()
        doc["stages"][3] = stage("http-hot-reload-soak",
                                  status="FAIL(timeout)", exit_code=124,
                                  duration=7500)
        doc["stages"][3]["failure_summary"] = "test timed out after 2h5m"
        doc["result"] = "FAIL(timeout)"
        doc["exit_code"] = 124
        doc["total_duration_seconds"] = 7800
        doc["finished_at"] = "2026-10-29T16:33:53Z"
        self.assert_valid(doc)

    def test_valid_smoke_with_allowed_skipped_race(self):
        doc = smoke_doc()
        doc["stages"][-1] = stage("race-soak-short", status="SKIPPED",
                                   exit_code=None, duration=0)
        doc["stages"][-1].pop("failure_summary", None)
        self.assert_valid(doc, kind="ci-smoke",
                          extra_args=["--allow-skipped", "race-soak-short"])

    def test_cancelled_record_validates_with_allowed_skipped(self):
        doc = soak_doc()
        doc["stages"][7] = stage("outage-gameday", status="CANCELLED",
                                  exit_code=None, duration=19)
        for i in (8, 9):
            doc["stages"][i] = stage(doc["stages"][i]["name"],
                                     status="SKIPPED", exit_code=None,
                                     duration=0)
        doc["result"] = "CANCELLED"
        doc["exit_code"] = 130
        self.assert_valid(doc, extra_args=["--allow-skipped",
                                           "race-soak", "budget-leak-slope"])

    # Incomplete / inconsistent evidence ------------------------------
    def test_cancelled_without_allowed_skipped_is_incomplete(self):
        doc = soak_doc()
        doc["stages"][7] = stage("outage-gameday", status="CANCELLED",
                                  exit_code=None, duration=19)
        for i in (8, 9):
            doc["stages"][i] = stage(doc["stages"][i]["name"],
                                     status="SKIPPED", exit_code=None,
                                     duration=0)
        doc["result"] = "CANCELLED"
        doc["exit_code"] = 130
        self.assert_invalid(doc, needle="SKIPPED stages not allowed")

    def test_missing_stage_rejected(self):
        doc = soak_doc()
        doc["stages"] = [s for s in doc["stages"]
                         if s["name"] != "outage-gameday"]
        self.assert_invalid(doc, needle="missing stages")

    def test_missing_stress_round_rejected(self):
        doc = soak_doc()
        doc["stages"] = [s for s in doc["stages"]
                         if s["name"] != "stress-round-2"]
        self.assert_invalid(doc, needle="missing stages")

    def test_duplicate_stage_rejected(self):
        doc = soak_doc()
        doc["stages"].append(copy.deepcopy(doc["stages"][0]))
        self.assert_invalid(doc, needle="more than once")

    def test_skipped_stage_labelled_pass_rejected(self):
        doc = soak_doc()
        doc["stages"][-1] = stage("budget-leak-slope", status="SKIPPED",
                                   exit_code=None, duration=0)
        self.assert_invalid(doc, needle="result PASS contradicts")

    def test_blocked_stage_labelled_pass_rejected(self):
        doc = soak_doc()
        doc["stages"][-1] = stage("budget-leak-slope", status="BLOCKED",
                                   exit_code=None, duration=0)
        self.assert_invalid(doc, needle="result PASS contradicts")

    def test_skipped_stage_with_exit_code_rejected(self):
        doc = soak_doc()
        doc["stages"][-1] = stage("budget-leak-slope", status="SKIPPED",
                                   exit_code=0, duration=0)
        self.assert_invalid(doc, extra_args=["--allow-skipped",
                                             "budget-leak-slope"],
                            needle="SKIPPED but exit_code")

    def test_skipped_stage_with_duration_rejected(self):
        doc = soak_doc()
        doc["stages"][-1] = stage("budget-leak-slope", status="SKIPPED",
                                   exit_code=None, duration=30)
        self.assert_invalid(doc, extra_args=["--allow-skipped",
                                             "budget-leak-slope"],
                            needle="SKIPPED but duration")

    def test_failed_stage_without_failure_summary_rejected(self):
        doc = soak_doc()
        doc["stages"][6] = stage("outage-provider", status="FAIL",
                                  exit_code=1, duration=30)
        doc["stages"][6]["failure_summary"] = ""
        doc["result"] = "FAIL"
        doc["exit_code"] = 1
        self.assert_invalid(doc, needle="failure_summary")

    def test_failed_result_with_all_pass_stages_rejected(self):
        doc = soak_doc(result="FAIL", exit_code=1)
        self.assert_invalid(doc, needle="every stage PASS")

    # SHA, timestamps, durations --------------------------------------
    def test_short_sha_rejected(self):
        self.assert_invalid(soak_doc(commit="1234567"), needle="commit")

    def test_wrong_expected_commit_rejected(self):
        code, err = self.run_validator(soak_doc(commit=OTHER_SHA))
        self.assertEqual(code, 1)
        self.assertIn("commit", err)

    def test_finished_not_after_started_rejected(self):
        doc = soak_doc(finished_at="2026-10-29T14:23:53Z")
        self.assert_invalid(doc, needle="finished_at is not after")

    def test_total_duration_disagrees_with_wall_rejected(self):
        doc = soak_doc(total_duration_seconds=100)
        self.assert_invalid(doc, needle="disagrees with")

    def test_sustained_shorter_than_requested_rejected(self):
        doc = soak_doc()
        doc["stages"][3] = stage("http-hot-reload-soak", duration=100)
        self.assert_invalid(doc, needle="less than requested")

    def test_stage_durations_exceed_total_rejected(self):
        doc = soak_doc()
        doc["stages"][3] = stage("http-hot-reload-soak", duration=7209)
        doc["total_duration_seconds"] = 7447
        # Inflate a non-sustained stage so the sum exceeds the total
        # without breaking the wall-clock check (timestamps unchanged
        # would also break it, so move finished_at to match the total).
        doc["stages"][8] = stage("race-soak", duration=1000)
        self.assert_invalid(doc, needle="sum to")

    # Exit status -------------------------------------------------------
    def test_pass_result_with_nonzero_exit_rejected(self):
        self.assert_invalid(soak_doc(exit_code=1),
                            needle="top-level exit_code")

    def test_fail_result_with_zero_exit_rejected(self):
        doc = soak_doc(result="FAIL", exit_code=0)
        doc["stages"][6] = stage("outage-provider", status="FAIL",
                                  exit_code=1, duration=30)
        doc["stages"][6]["failure_summary"] = "FAIL: provider refused"
        self.assert_invalid(doc, needle="top-level exit_code")

    def test_pass_stage_with_nonzero_exit_rejected(self):
        doc = soak_doc()
        doc["stages"][0] = stage("stress-round-1", exit_code=1, duration=44)
        self.assert_invalid(doc, needle="status PASS but exit_code")

    def test_fail_stage_with_zero_exit_rejected(self):
        doc = soak_doc()
        doc["stages"][6] = stage("outage-provider", status="FAIL",
                                  exit_code=0, duration=30)
        doc["result"] = "FAIL"
        doc["exit_code"] = 1
        self.assert_invalid(doc, needle="status FAIL but exit_code")

    # Runtime identity, samples, not_run --------------------------------
    def test_missing_environment_rejected(self):
        doc = soak_doc()
        del doc["environment"]
        self.assert_invalid(doc, needle="environment")

    def test_unknown_go_version_rejected(self):
        doc = soak_doc()
        doc["environment"]["go"] = "unknown"
        self.assert_invalid(doc, needle="environment.go")

    def test_bare_go_substring_is_not_an_identity(self):
        doc = soak_doc()
        doc["environment"]["go"] = "contains go1. somewhere"
        self.assert_invalid(doc, needle="environment.go")

    def test_unknown_nproc_rejected(self):
        doc = soak_doc()
        doc["environment"]["nproc"] = "unknown"
        self.assert_invalid(doc, needle="environment.nproc")

    def test_hot_reload_passed_without_samples_rejected(self):
        doc = soak_doc(resource_samples=None)
        # No samples file is written by the harness in this case:
        # point at a path that does not exist next to the summary.
        doc["resource_samples"] = "absent-samples.jsonl"
        code, err = self.run_validator(doc)
        # The harness writes samples.jsonl, not absent-samples.jsonl,
        # so the validator must not find it.
        self.assertEqual(code, 1)
        self.assertIn("resource_samples", err)

    def test_missing_not_run_rejected(self):
        doc = soak_doc()
        del doc["not_run"]
        self.assert_invalid(doc, needle="not_run")

    def test_malformed_not_run_entry_rejected(self):
        doc = soak_doc(not_run=[{"item": "live canaries",
                                 "status": "PASS"}])
        self.assert_invalid(doc, needle="not_run")


if __name__ == "__main__":
    unittest.main(verbosity=2)

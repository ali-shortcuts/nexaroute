#!/usr/bin/env python3
"""Mandatory browser-acceptance runner for NexaRoute.

Runs the existing browser suites in order -- test-browser-e2e.py,
test-browser-e2e-502.py and test-live-visual-agent.py -- and reports one
status per suite. This script is a runner, not a parallel framework: it
adds no scenarios of its own, it only executes the suites that already
exist and records whether each one really ran.

Status contract (requirement: a skipped suite is never a pass):
  PASS    -- the suite exited 0 and printed its PASS marker.
  FAIL    -- the suite exited non-zero for any other reason, timed out,
             or exited 0 without its PASS marker (an incomplete run).
  SKIPPED -- the suite could not start because a required dependency is
             missing (no Chromium binary, or no Python Playwright). The
             runner prints the exact remedy and exits non-zero.

Exit codes: 0 = every suite PASS; 1 = at least one FAIL; 2 = at least
one SKIPPED and no FAIL. A CI step that runs this script therefore
fails on a skip instead of silently reporting a successful acceptance
run, which is what the old verify.sh WARN-and-continue path did.

Failure diagnostics: each suite's stdout/stderr is captured to a
per-suite log under the diagnostics directory. The logs are redacted
before they are written: known test canary values, bearer tokens,
x-admin-key values, cookie headers and api_key values are replaced
with [REDACTED]. The runner never reads browser cookies, localStorage
or sessionStorage, and never dumps form values, so no credential can
reach a log or an uploaded artifact through this path. The gateway
processes themselves run in the suites' own temporary directories and
are not preserved; the per-suite log plus the summary.json below are
the diagnostic record.

Chromium resolution, in order: NEXAROUTE_CHROMIUM_BIN, a system
chromium/google-chrome on PATH, then the full Chromium binary shipped
with the pinned Python Playwright package (its executable_path). The
last step matters: `playwright install chromium` does not put a
`chromium` binary on PATH, so the old suites' which() check alone
would report a skip on a correctly provisioned machine.

Isolation: the suites already boot a fresh gateway on a loopback-only
temporary config with probe disabled and only loopback fake upstreams
or browser-boundary mocks for discovery/check/test calls; no real
provider credential is ever used. This runner adds one more isolation
step for CI: it points the suites' screenshot/evidence outputs at the
diagnostics directory (NEXAROUTE_BROWSER_EVIDENCE_DIR and
NEXAROUTE_LIVE_VISUAL_EVIDENCE_DIR) instead of letting a mandatory
acceptance run write screenshots into the repository checkout.

Usage:
  python3 scripts/run-browser-acceptance.py [--diagnostics-dir DIR]
  # CI pins the dependency first, e.g.:
  #   python3 -m pip install --user "playwright==1.55.0"
  #   python3 -m playwright install --with-deps chromium
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent

PLAYWRIGHT_VERSION = "1.55.0"

SUITES = [
    ("browser-e2e", "scripts/test-browser-e2e.py", "BROWSER E2E PASS"),
    ("browser-e2e-502", "scripts/test-browser-e2e-502.py", "BROWSER E2E #502 PASS"),
    ("live-visual-agent", "scripts/test-live-visual-agent.py", "LIVE VISUAL AGENT E2E PASS"),
]

# Values that must never appear in a diagnostic log. The first pattern
# covers every test-only canary the suites use; the rest cover generic
# credential shapes in case a future suite prints one.
REDACT_PATTERNS = [
    (re.compile(r"ISSUE393_BROWSER_[A-Za-z0-9_]+"), "[REDACTED-CANARY]"),
    (re.compile(r"(?i)Bearer\s+\S+"), "Bearer [REDACTED]"),
    (re.compile(r"(?i)(x-admin-key[\"':\s=]+)\S+"), r"\1[REDACTED]"),
    (re.compile(r"(?i)(cookie:\s*).*"), r"\1[REDACTED]"),
    (re.compile(r"(?i)(\"api_key\"\s*:\s*\")[^\"]+(\")"), r"\1[REDACTED]\2"),
]


def redact(text: str, extra_values: list[str]) -> str:
    for value in extra_values:
        if value:
            text = text.replace(value, "[REDACTED]")
    for pattern, replacement in REDACT_PATTERNS:
        text = pattern.sub(replacement, text)
    return text


def find_chromium() -> tuple[str | None, str]:
    """Return (path, how it was found) or (None, reason)."""
    env_bin = os.environ.get("NEXAROUTE_CHROMIUM_BIN")
    if env_bin:
        if Path(env_bin).exists():
            return env_bin, "NEXAROUTE_CHROMIUM_BIN"
        return None, f"NEXAROUTE_CHROMIUM_BIN points at a missing file: {env_bin}"
    for name in ("chromium", "google-chrome"):
        found = shutil.which(name)
        if found:
            return found, f"PATH ({name})"
    # Pinned Playwright's own full Chromium binary. Its executable_path
    # is the full chrome binary, which is what the suites need; the
    # headless-shell binary is a separate download and is not used.
    probe = (
        "from playwright.sync_api import sync_playwright\n"
        "with sync_playwright() as pw:\n"
        "    print(pw.chromium.executable_path)\n"
    )
    result = subprocess.run(
        [sys.executable, "-c", probe],
        capture_output=True, text=True, timeout=60,
    )
    candidate = result.stdout.strip().splitlines()[-1] if result.stdout.strip() else ""
    if result.returncode == 0 and candidate and Path(candidate).exists():
        return candidate, "Playwright bundled Chromium"
    return None, (
        "no Chromium binary found (no NEXAROUTE_CHROMIUM_BIN, no "
        "chromium/google-chrome on PATH, and no Playwright bundled Chromium)"
    )


def playwright_available() -> tuple[bool, str]:
    try:
        import playwright  # noqa: F401
    except ImportError:
        return False, "Python package 'playwright' is not installed"
    return True, ""


def git_head() -> str:
    try:
        result = subprocess.run(
            ["git", "-C", str(ROOT), "rev-parse", "HEAD"],
            capture_output=True, text=True, timeout=30,
        )
    except Exception:
        return ""
    return result.stdout.strip() if result.returncode == 0 else ""


def run_suite(name: str, script: str, marker: str, env: dict, diag_dir: Path,
              timeout: int, redact_values: list[str]) -> dict:
    started = time.monotonic()
    log_path = diag_dir / f"{name}.log"
    status = "FAIL"
    exit_code: int | None = None
    detail = ""
    try:
        proc = subprocess.run(
            [sys.executable, str(ROOT / script)],
            cwd=ROOT, env=env, capture_output=True, text=True, timeout=timeout,
        )
        exit_code = proc.returncode
        combined = proc.stdout + ("\n" if proc.stdout and proc.stderr else "") + proc.stderr
        redacted = redact(combined, redact_values)
        log_path.write_text(redacted)
        if exit_code == 0 and marker in proc.stdout:
            status = "PASS"
        elif exit_code == 2 and "SKIPPED" in combined:
            status = "SKIPPED"
            detail = "suite reported a skip (missing Chromium)"
        elif "No module named 'playwright'" in combined or "No module named playwright" in combined:
            status = "SKIPPED"
            detail = "Python Playwright is not installed"
        elif exit_code == 0:
            detail = f"exited 0 without the PASS marker {marker!r}; treating an incomplete run as FAIL"
        else:
            detail = f"exited {exit_code}; see {log_path.name}"
    except subprocess.TimeoutExpired:
        detail = f"timed out after {timeout}s"
        log_path.write_text(redact(f"TIMEOUT after {timeout}s\n", redact_values))
    duration = round(time.monotonic() - started, 1)
    return {
        "suite": name, "script": script, "status": status,
        "exit_code": exit_code, "duration_seconds": duration,
        "log": log_path.name, "detail": detail,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the NexaRoute browser acceptance suites.")
    parser.add_argument("--diagnostics-dir", default=None,
                        help="Directory for per-suite logs, evidence and summary.json. "
                             "Defaults to a fresh temporary directory (printed on stdout).")
    parser.add_argument("--suite-timeout", type=int, default=900,
                        help="Per-suite timeout in seconds (default 900).")
    args = parser.parse_args()

    if args.diagnostics_dir:
        diag_dir = Path(args.diagnostics_dir)
        diag_dir.mkdir(parents=True, exist_ok=True)
    else:
        diag_dir = Path(tempfile.mkdtemp(prefix="nexaroute-browser-acceptance-"))
    print(f"BROWSER ACCEPTANCE: diagnostics directory: {diag_dir}")

    ok, reason = playwright_available()
    if not ok:
        print("BROWSER ACCEPTANCE: SKIPPED: " + reason)
        print("BROWSER ACCEPTANCE: remedy: python3 -m pip install --user "
              f'"playwright=={PLAYWRIGHT_VERSION}" && python3 -m playwright install --with-deps chromium')
        summary = {"status": "SKIPPED", "head": git_head(), "suites": [],
                   "detail": reason}
        (diag_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        print("BROWSER ACCEPTANCE: SKIPPED")
        return 2

    chromium, how = find_chromium()
    if chromium is None:
        print(f"BROWSER ACCEPTANCE: SKIPPED: {how}")
        print("BROWSER ACCEPTANCE: remedy: python3 -m playwright install --with-deps chromium "
              "(or set NEXAROUTE_CHROMIUM_BIN to a Chromium binary)")
        summary = {"status": "SKIPPED", "head": git_head(), "suites": [],
                   "detail": how}
        (diag_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        print("BROWSER ACCEPTANCE: SKIPPED")
        return 2
    print(f"BROWSER ACCEPTANCE: Chromium: {chromium} (via {how})")

    env = os.environ.copy()
    env["NEXAROUTE_CHROMIUM_BIN"] = chromium
    env["NEXAROUTE_BROWSER_EVIDENCE_DIR"] = str(diag_dir / "evidence")
    env["NEXAROUTE_LIVE_VISUAL_EVIDENCE_DIR"] = str(diag_dir / "live-visual")
    # Test-only video token value, if a suite ever prints it, must not
    # reach a log. (The suites set their own test value; redact the
    # environment value as well when present.)
    redact_values = [v for v in [env.get("NEXAROUTE_TEST_VIDEO_TOKEN")] if v]

    results = []
    for name, script, marker in SUITES:
        result = run_suite(name, script, marker, env, diag_dir,
                           args.suite_timeout, redact_values)
        results.append(result)
        print(f"BROWSER ACCEPTANCE: {name}: {result['status']} "
              f"({result['duration_seconds']}s, exit {result['exit_code']})"
              + (f" -- {result['detail']}" if result["detail"] else ""))

    if any(r["status"] == "FAIL" for r in results):
        overall = "FAIL"
        code = 1
    elif any(r["status"] == "SKIPPED" for r in results):
        overall = "SKIPPED"
        code = 2
    else:
        overall = "PASS"
        code = 0
    summary = {
        "status": overall, "head": git_head(), "chromium": chromium,
        "chromium_source": how, "playwright_pin": PLAYWRIGHT_VERSION,
        "diagnostics_dir": str(diag_dir), "suites": results,
    }
    (diag_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(f"BROWSER ACCEPTANCE: {overall}")
    return code


if __name__ == "__main__":
    raise SystemExit(main())

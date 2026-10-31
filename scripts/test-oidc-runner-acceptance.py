#!/usr/bin/env python3
"""Regression for OIDC run 41207000118: a BaseException (e.g. a runner
interrupt) during the acceptance script must still leave a diagnostic
summary, instead of the run ending with only the workflow EXIT-trap
minimal summary. The script's finally block writes run.aborted; the
interrupt itself still propagates, which is correct for the shell.
"""
import importlib.util
import json
import pathlib
import subprocess
import sys
import tempfile


def main() -> int:
    spec = importlib.util.spec_from_file_location(
        "oidc", "scripts/oidc-runner-acceptance.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    orig_run = subprocess.run

    def boom(*args, **kwargs):
        raise KeyboardInterrupt("simulated runner interrupt")

    subprocess.run = boom
    out = tempfile.mkdtemp()
    try:
        mod.main(["--out-dir", out, "--gateway-binary", "/bin/false"])
    except KeyboardInterrupt:
        pass
    finally:
        subprocess.run = orig_run
    summary = pathlib.Path(out, "oidc-runner-summary.json")
    assert summary.exists(), "no summary after KeyboardInterrupt"
    doc = json.loads(summary.read_text())
    assert doc["result"] == "FAIL", doc
    assert any(c["id"] == "run.aborted" and c["status"] == "FAIL"
               for c in doc["checks"]), doc
    print("OIDC abort-summary regression: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())

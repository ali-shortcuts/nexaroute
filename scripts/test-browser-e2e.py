#!/usr/bin/env python3
"""Small real-browser acceptance test for the embedded Control Plane.

Uses only the Python Playwright package and the system Chromium. The gateway is
started with a clean, provider-free config so this test is deterministic and
never contacts an upstream provider.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import urllib.request

from playwright.sync_api import Error as PlaywrightError
from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parent.parent

def wait_ready(url: str, proc: subprocess.Popen[str]) -> None:
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"gateway exited during startup: {proc.returncode}")
        try:
            with urllib.request.urlopen(url + "/healthz", timeout=1) as response:
                if response.status == 200:
                    return
        except OSError:
            time.sleep(0.1)
    raise AssertionError("gateway did not become ready within 15 seconds")

def main() -> None:
    chromium = shutil.which("chromium") or shutil.which("google-chrome")
    if not chromium:
        raise SystemExit("SKIP: Chromium is not installed")
    with tempfile.TemporaryDirectory(prefix="nexaroute-browser-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        config["listen"] = "127.0.0.1:0"
        config["providers"] = []
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        # Pick a free port explicitly because the gateway prints and binds the
        # configured address; this avoids relying on a port-0 discovery API.
        import socket
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        config["listen"] = f"127.0.0.1:{port}"
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
        proc = subprocess.Popen(
            ["go", "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)],
            cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True,
        )
        base = f"http://127.0.0.1:{port}"
        try:
            wait_ready(base, proc)
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 900})
                console_errors: list[str] = []
                page.on("pageerror", lambda exc: console_errors.append(str(exc) + "\n" + (exc.stack or "")))
                page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" and "Failed to load resource" not in msg.text else None)
                # The dashboard intentionally keeps an authenticated SSE
                # connection open, so networkidle can never be reached.
                page.goto(base + "/", wait_until="domcontentloaded")
                page.locator("#apiState").wait_for(state="visible", timeout=10000)
                evidence = Path(os.environ.get("NEXAROUTE_BROWSER_EVIDENCE", str(tmp_path / "browser-evidence")))
                evidence.mkdir(parents=True, exist_ok=True)
                for width, name in ((1440, "desktop"), (1024, "tablet"), (390, "phone")):
                    page.set_viewport_size({"width": width, "height": 900 if width > 500 else 844})
                    page.screenshot(path=str(evidence / f"dashboard-{name}.png"), full_page=True)
                page.set_viewport_size({"width": 1440, "height": 900})
                # The v2 bootstrap intentionally shortens the active Overview
                # label after mounting the control-plane chrome.
                assert page.locator("#title").inner_text() == "Overview"
                assert page.locator("#apiState").inner_text().lower().find("connected") >= 0

                # Real navigation and empty-state behavior.
                page.locator('button[data-tab="providers"]').click()
                assert page.locator("#providers").is_visible()
                assert "No providers" in page.locator("#providerGrid").inner_text()
                page.locator("#addProviderBtn").click()
                assert page.locator("#providerModal").is_visible(), "provider drawer did not open; browser errors: " + "; ".join(console_errors)
                page.locator("#cancelProviderBtn").click()
                assert not page.locator("#providerModal").is_visible()

                # Theme and language are actual controls mounted by the v2 UI.
                page.locator("#cpThemeBtn").click()
                assert page.locator("html").get_attribute("data-theme") == "light"
                page.locator("#cpLangBtn").click()
                assert page.locator("html").get_attribute("dir") == "rtl"
                assert page.locator("html").get_attribute("lang") == "fa"
                page.locator("#cpLangBtn").click()
                assert page.locator("html").get_attribute("dir") == "ltr"

                # Live polling control must give immediate, observable feedback.
                page.locator("#pauseBtn").click()
                assert "Resume" in page.locator("#pauseBtn").inner_text()
                page.locator("#pauseBtn").click()
                assert "Pause" in page.locator("#pauseBtn").inner_text()

                # Settings is reachable and populated even with no providers.
                page.locator('button[data-tab="settings"]').click()
                assert page.locator("#rtStrategy").input_value() == "ready_mesh"
                assert not console_errors, "browser errors: " + "; ".join(console_errors)
                browser.close()
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        print("BROWSER E2E PASS: clean startup, navigation, provider drawer, theme, Persian RTL, pause/resume, settings")

if __name__ == "__main__":
    try:
        main()
    except PlaywrightError as exc:
        raise SystemExit(f"Browser E2E failed: {exc}")

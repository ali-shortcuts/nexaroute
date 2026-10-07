#!/usr/bin/env python3
"""Browser acceptance for the v0.15.0 backend-driven control plane.

The Go gateway, persistence, routing mutations and snapshot reads are real. Only
provider discovery/check/test calls are intercepted at the browser boundary so
this suite is deterministic and does not contact a paid upstream provider.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parent.parent

def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]

def wait_ready(base: str, proc: subprocess.Popen[str]) -> None:
    deadline = time.time() + 60
    while time.time() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"gateway exited with {proc.returncode}")
        try:
            with urllib.request.urlopen(base + "/healthz", timeout=2) as r:
                if r.status == 200:
                    return
        except Exception:
            time.sleep(.2)
    raise AssertionError("gateway did not become ready")

def main() -> None:
    chromium = shutil.which("chromium") or shutil.which("google-chrome")
    if not chromium:
        raise SystemExit("SKIP: Chromium is not installed")
    with tempfile.TemporaryDirectory(prefix="nexaroute-browser-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        config.update({"providers": [], "virtual_endpoints": [], "route_profiles": [], "candidate_pools": [], "fallback_chains": []})
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        port = free_port()
        config["listen"] = f"127.0.0.1:{port}"
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
        log = (tmp_path / "gateway.log").open("w")
        proc = subprocess.Popen(["go", "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT, text=True)
        base = f"http://127.0.0.1:{port}"
        try:
            wait_ready(base, proc)
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 1000})
                page.set_default_timeout(12000)
                page.route("**/admin/api/provider-discover", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "models": ["model-alpha", "model-beta"]})))
                page.route("**/admin/api/provider-check", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "status_code": 200, "latency_ms": 1})))
                page.route("**/admin/api/provider-test", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "passed": 1, "total": 1, "results": [{"model": "model-alpha", "ok": True, "latency_ms": 1}]})))
                page.goto(base + "/", wait_until="domcontentloaded")
                expect(page.locator("#content")).to_contain_text("No real activity yet")
                expect(page.locator("#primary-nav")).to_contain_text("Overview")
                expect(page.locator("#primary-nav")).to_contain_text("Providers")
                expect(page.locator("#primary-nav")).to_contain_text("Routing")
                expect(page.locator("#primary-nav")).to_contain_text("Activity")
                expect(page.locator("#primary-nav")).to_contain_text("Settings")
                page.locator('[data-page="providers"]').click()
                page.locator('[data-action="add-provider"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Add provider")
                page.locator("#p-name").fill("Mock Provider")
                page.locator("#p-base").fill("http://127.0.0.1:9/v1")
                page.locator('[data-action="detect-models"]').click()
                expect(page.locator("#model-list")).to_contain_text("model-alpha")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("Mock Provider")
                page.locator('[data-page="routing"]').click()
                page.locator('[data-action="add-route"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Create route")
                page.locator("#r-name").fill("Coding")
                page.locator("#r-public").fill("coding")
                expect(page.locator(".modal-body")).to_contain_text("model-alpha")
                page.locator('.modal-body input[type="checkbox"]').first.check()
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("coding")
                page.locator('[data-action="connect-route"]').click()
                expect(page.locator("#modal-title")).to_have_text("Connect coding")
                expect(page.locator(".modal-body")).to_contain_text("POST /v1/messages")
                expect(page.locator(".modal-body")).to_contain_text("ANTHROPIC_BASE_URL")
                expect(page.locator(".modal-body")).to_contain_text("ANTHROPIC_MODEL")
                page.locator('[data-action="close-modal"]').first.click()
                page.locator('[data-page="settings"]').click()
                expect(page.locator("#setting-strategy")).to_be_visible()
                expect(page.locator("#content")).to_contain_text("Create virtual key")
                expect(page.locator("#content")).to_contain_text("Start graceful drain")
                expect(page.locator("#content")).to_contain_text("Download CSV")
                page.locator('[data-page="activity"]').click()
                expect(page.locator("#content")).to_contain_text("No real activity yet")
                evidence = ROOT / "specs/014-control-plane-rebuild/evidence/control-plane-e2e.png"
                evidence.parent.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(evidence), full_page=True)
                browser.close()
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            log.close()
    print("BROWSER E2E PASS")

if __name__ == "__main__":
    main()

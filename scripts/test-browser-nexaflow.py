#!/usr/bin/env python3
"""Browser acceptance for the NexaFlow Visual Workflow Studio.

Drives the real web UI against a real gateway (loopback, keyless emergency
mode) and the real workflow HTTP API. No paid calls: the graphs use only
local nodes (manual trigger, JSON transform, wait, approval) plus one HTTP
request to an unroutable loopback port, which fails closed without leaving
the machine.

Skips cleanly (exit 0) when Chromium or Python Playwright is unavailable,
matching the verify.sh browser gate.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parent.parent


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_ready(base: str, proc: subprocess.Popen[str]) -> None:
    deadline = time.time() + 90
    while time.time() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"gateway exited with {proc.returncode}")
        try:
            with urllib.request.urlopen(base + "/healthz", timeout=2) as r:
                if r.status == 200:
                    return
        except Exception:
            time.sleep(0.2)
    raise AssertionError("gateway did not become ready")


def main() -> None:
    chromium = shutil.which("chromium") or shutil.which("google-chrome")
    if not chromium:
        # Playwright's own browser cache (used by the Muse browser sandbox).
        cache = Path.home() / ".cache" / "ms-playwright"
        if cache.is_dir():
            for cand in sorted(cache.glob("chromium-*/chrome-linux/chrome")):
                if cand.is_file():
                    chromium = str(cand)
                    break
    try:
        from playwright.sync_api import expect, sync_playwright
    except ImportError:
        print("SKIP: Python Playwright is not installed")
        return
    if not chromium:
        print("SKIP: Chromium is not installed")
        return

    with tempfile.TemporaryDirectory(prefix="nexaroute-nexaflow-browser-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        config.update({"providers": [], "virtual_endpoints": [],
                       "route_profiles": [], "candidate_pools": [],
                       "fallback_chains": []})
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        port = free_port()
        config["listen"] = f"127.0.0.1:{port}"
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
        log = (tmp_path / "gateway.log").open("w")
        proc = subprocess.Popen(["go", "run", "./cmd/gateway", "-no-browser",
                                 "-config", str(config_path)],
                                cwd=ROOT, env=env, stdout=log,
                                stderr=subprocess.STDOUT, text=True)
        base = f"http://127.0.0.1:{port}"
        try:
            wait_ready(base, proc)
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True,
                                             executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440,
                                                  "height": 900})
                page.set_default_timeout(15000)
                console_errors: list[tuple[str, str]] = []

                def record_console(msg):
                    if msg.type == "error":
                        loc = (msg.location or {}).get("url", "")
                        console_errors.append((msg.text, loc))

                page.on("console", record_console)
                page.goto(base + "/", wait_until="domcontentloaded")

                def nav_workflows():
                    page.locator('[data-page="workflows"]').click()
                    expect(page.locator("#content")).to_contain_text(
                        "Search workflows", timeout=15000)

                def create_workflow(name: str):
                    page.locator('[data-wf="create-wf"]').first.click()
                    expect(page.locator("#modal-title")).to_have_text(
                        "Create workflow")
                    page.locator("#wf-new-name").fill(name)
                    page.locator('[data-action="save-modal"]').click()
                    expect(page.locator("#content")).to_contain_text(name)

                def open_workflow(name: str):
                    nav_workflows()
                    page.locator(".wf-link", has_text=name).first.click()
                    expect(page.locator(".wf-canvas")).to_be_visible()

                def add_node(node_type: str):
                    page.locator(
                        f'[data-palette-type="{node_type}"]').click()
                    page.wait_for_timeout(300)

                def set_declared_inputs(names: str):
                    page.evaluate(
                        """(names) => {
                            const el = document.querySelector('#wf-run-inputs');
                            el.value = names;
                            el.dispatchEvent(new Event('change', {bubbles: true}));
                        }""",
                        names)

                def save_draft():
                    page.locator('[data-wf="save-draft"]').click()
                    expect(page.locator("#notice")).to_contain_text(
                        "Draft saved.")

                def publish():
                    page.locator('[data-wf="publish-draft"]').click()
                    expect(page.locator("#modal-title")).to_have_text(
                        "Publish workflow?")
                    page.locator('[data-action="save-modal"]').click()
                    expect(page.locator("#notice")).to_contain_text(
                        "Published version 1.")

                def start_run(input_json: str, policy: bool = False):
                    page.locator('[data-wf="open-run"]').click()
                    expect(page.locator("#modal-title")).to_have_text(
                        "Run workflow")
                    page.locator("#wf-run-input").fill(input_json)
                    if policy:
                        page.locator("#wf-run-policy").check()
                    page.locator('[data-action="save-modal"]').click()
                    expect(page.locator("#notice")).to_contain_text(
                        "Run started.")

                def expect_run_status(status: str, timeout: int = 30000):
                    expect(page.locator("#wf-run-panel")).to_contain_text(
                        status, timeout=timeout)

                # 1. Empty list + create.
                nav_workflows()
                expect(page.locator("#content")).to_contain_text(
                    "No workflows yet")
                create_workflow("Browser Flow")

                # 2. Add + configure nodes, connect, validate, save, reload.
                assert page.locator(".wf-node").count() == 0
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.json_transform")
                assert page.locator(".wf-node").count() == 2
                set_declared_inputs("operations")
                page.locator('[data-wf-port$=":payload:out"]').first.click()
                page.locator('[data-wf-port$=":value:in"]').first.click()
                page.locator('[data-wf="validate-draft"]').click()
                expect(page.locator("#content")).to_contain_text(
                    "Draft is valid.")
                save_draft()
                page.reload(wait_until="domcontentloaded")
                open_workflow("Browser Flow")
                assert page.locator(".wf-node").count() == 2, \
                    "saved graph must survive reload"

                # 3. Publish + run to success.
                publish()
                start_run('{"payload": {"hello": "world"}, '
                          '"operations": [{"op": "pick", "field": "hello"}]}')
                expect_run_status("succeeded")
                expect(page.locator("#wf-run-panel")).to_contain_text(
                    "hello")

                # 4. Validation errors surface for a trigger-less graph.
                nav_workflows()
                create_workflow("Bad Flow")
                add_node("nexaroute.control.json_transform")
                page.locator('[data-wf="validate-draft"]').click()
                expect(page.locator("#content")).to_contain_text(
                    "no_trigger")

                # 5. Run failure is displayed with a safe error.
                nav_workflows()
                create_workflow("Fail Flow")
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.http.request")
                set_declared_inputs("url")
                save_draft()
                publish()
                start_run('{"url": "http://127.0.0.1:9/unreachable"}',
                          policy=True)
                expect_run_status("failed")

                # 6. Cancellation of a long wait.
                nav_workflows()
                create_workflow("Wait Flow")
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.wait")
                set_declared_inputs("duration_ms")
                save_draft()
                publish()
                start_run('{"duration_ms": 120000}')
                expect_run_status("running")
                page.locator('[data-wf="cancel-run"]').click()
                expect_run_status("cancelled")

                # 7. Approval gate: pause, approve, succeed.
                nav_workflows()
                create_workflow("Approval Flow")
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.approval")
                save_draft()
                publish()
                start_run('{}')
                expect_run_status("awaiting_approval")
                page.locator('[data-wf="approve-run"]').click()
                expect_run_status("succeeded")

                browser.close()
                # The app ships no favicon, so Chromium's automatic
                # /favicon.ico request 404s; that entry is benign, and
                # its console location names the URL. Anything else is
                # a real UI/JS failure.
                real_errors = [(t, u) for (t, u) in console_errors
                               if not u.endswith("/favicon.ico")]
                assert not real_errors, \
                    f"unexpected console errors: {real_errors}"
                print("nexaflow browser acceptance: PASS")
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    sys.exit(main())

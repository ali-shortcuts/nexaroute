#!/usr/bin/env python3
"""Real local-upstream acceptance for the Live Visual Agent.

The gateway, SSE stream, routing decisions, and request events are real. Two
local HTTP upstreams make the first candidate fail and the second succeed.
"""
from __future__ import annotations
import json
import re
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parent.parent
EVIDENCE = ROOT / "specs/014-control-plane-rebuild/live-visual-agent/evidence"


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class UpstreamHandler(BaseHTTPRequestHandler):
    healthy = False
    delay = 0.0

    def do_POST(self) -> None:  # noqa: N802
        _ = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if self.delay:
            time.sleep(self.delay)
        if not self.healthy:
            body = b'{"error":{"message":"deterministic upstream failure","type":"server_error"}}'
            self.send_response(503)
        else:
            body = b'{"id":"live-agent","object":"chat.completion","model":"model-alpha","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}'
            self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args) -> None:
        return


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
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    first = ThreadingHTTPServer(("127.0.0.1", 0), UpstreamHandler)
    second = ThreadingHTTPServer(("127.0.0.1", 0), type("HealthyHandler", (UpstreamHandler,), {"healthy": True, "delay": 0.8}))
    threading.Thread(target=first.serve_forever, daemon=True).start()
    threading.Thread(target=second.serve_forever, daemon=True).start()
    metrics = {}
    with tempfile.TemporaryDirectory(prefix="nexaroute-live-agent-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        config.update({"providers": [], "virtual_endpoints": [], "route_profiles": [], "candidate_pools": [], "fallback_chains": []})
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        config["routing"]["max_attempts"] = 2
        config["routing"]["strategy"] = "adaptive"
        config["listen"] = f"127.0.0.1:{free_port()}"
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
        log = (tmp_path / "gateway.log").open("w")
        proc = subprocess.Popen(["go", "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT, text=True)
        base = f"http://127.0.0.1:{config['listen'].rsplit(':', 1)[1]}"
        try:
            wait_ready(base, proc)
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 1000}, reduced_motion="reduce")
                page.set_default_timeout(15000)
                page.route("**/admin/api/provider-discover", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "models": ["model-alpha"]})))
                page.route("**/admin/api/provider-check", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "status_code": 200, "latency_ms": 1})))
                page.goto(base + "/", wait_until="domcontentloaded")
                page.screenshot(path=str(EVIDENCE / "lva-idle.png"), full_page=True)
                metrics["idle"] = page.evaluate("""() => ({ dom_nodes: document.querySelectorAll('*').length, execution_status: document.querySelector('.topology')?.dataset.executionStatus || null, reduced_motion: matchMedia('(prefers-reduced-motion: reduce)').matches, heap_bytes: performance.memory?.usedJSHeapSize || null })""")
                for name, port in (("Failing upstream", first.server_address[1]), ("Healthy upstream", second.server_address[1])):
                    page.locator('[data-page="providers"]').click()
                    page.locator('[data-action="add-provider"]').first.click()
                    page.locator("#p-name").fill(name)
                    page.locator("#p-base").fill(f"http://127.0.0.1:{port}/v1")
                    page.locator('[data-action="detect-models"]').click()
                    expect(page.locator("#model-list")).to_contain_text("model-alpha")
                    page.locator('[data-action="save-modal"]').click()
                    expect(page.locator("#content")).to_contain_text(name)
                page.locator('[data-page="routing"]').click()
                page.locator('[data-action="add-route"]').first.click()
                page.locator("#r-mode").select_option("ordered")
                boxes = page.locator('.modal-body input[type="checkbox"]')
                expect(boxes).to_have_count(2)
                boxes.nth(0).check()
                boxes.nth(1).check()
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("coding")
                page.locator('[data-page="overview"]').click()
                request_id = "lva-real-failover-001"
                page.evaluate("""(requestId) => { window.liveRequest = fetch('/v1/chat/completions', { method: 'POST', headers: {'content-type':'application/json','x-request-id': requestId}, body: JSON.stringify({model:'coding',messages:[{role:'user',content:'hello'}]}) }); }""", request_id)
                expect(page.locator(".topology")).to_have_attribute("data-execution-status", re.compile("active|failover"))
                page.screenshot(path=str(EVIDENCE / "lva-active-failover.png"), full_page=True)
                metrics["active"] = page.evaluate("""() => ({ dom_nodes: document.querySelectorAll('*').length, execution_status: document.querySelector('.topology')?.dataset.executionStatus || null, heap_bytes: performance.memory?.usedJSHeapSize || null })""")
                response_status = page.evaluate("async () => (await window.liveRequest).status")
                if response_status != 200:
                    raise AssertionError(f"gateway request status={response_status}")
                expect(page.locator(".topology")).to_have_attribute("data-execution-status", "success")
                page.screenshot(path=str(EVIDENCE / "lva-success.png"), full_page=True)
                metrics["success"] = page.evaluate("""() => ({ dom_nodes: document.querySelectorAll('*').length, execution_status: document.querySelector('.topology')?.dataset.executionStatus || null, heap_bytes: performance.memory?.usedJSHeapSize || null })""")
                events = page.evaluate("""async (requestId) => (await (await fetch('/admin/api/snapshot?events=100')).json()).events.filter(e => e.request_id === requestId)""", request_id)
                kinds = [e.get("kind") for e in events]
                if "route_attempt" not in kinds or "route_fail" not in kinds or "failover" not in kinds or "route_ok" not in kinds:
                    raise AssertionError(f"missing request journey events: {kinds}")
                attempts = [e.get("deployment") for e in events if e.get("kind") == "route_attempt"]
                if len(attempts) < 2 or attempts[0] == attempts[-1]:
                    raise AssertionError(f"missing distinct failover attempts: {attempts}")
                page.locator('[data-page="activity"]').click()
                expect(page.locator("#content")).to_contain_text(request_id)
                page.screenshot(path=str(EVIDENCE / "lva-activity-correlation.png"), full_page=True)
                browser.close()
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                proc.kill(); proc.wait()
            log.close()
    (EVIDENCE / "lva-performance.json").write_text(json.dumps(metrics, indent=2) + "\n")
    first.shutdown(); second.shutdown()
    print("LIVE VISUAL AGENT E2E PASS")


if __name__ == "__main__":
    main()

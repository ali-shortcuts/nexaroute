#!/usr/bin/env python3
"""Focused browser E2E for the #502 remainder on a fresh, isolated gateway.

Exercises first-run empty state, provider discovery/manual model onboarding,
backend-compiled ordered fallback and real gateway failure -> fallback using
local fake upstreams only. A fresh gateway avoids consuming the admin rate-limit
budget of the existing privacy/credential acceptance suite.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import urllib.error
import urllib.request

from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parent.parent


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_ready(base: str, proc: subprocess.Popen[str]) -> None:
    deadline = time.time() + 60
    while time.time() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"gateway exited with {proc.returncode}")
        try:
            with urllib.request.urlopen(base + "/healthz", timeout=2) as response:
                if response.status == 200:
                    return
        except Exception:
            time.sleep(.2)
    raise AssertionError("gateway did not become ready")


def admin_json(base: str, path: str, method: str, body: dict | None = None) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(
        base + path,
        data=data,
        headers={"Content-Type": "application/json"},
        method=method,
    )
    # Respect the server's real rate limiter; only the direct test helper retries
    # short 429 cooldowns. Browser/API errors remain observable to the scenario.
    for attempt in range(6):
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                raw = response.read()
                return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as exc:
            if exc.code != 429 or attempt == 5:
                raise
            try:
                delay = max(.5, min(3.0, float(exc.headers.get("Retry-After", ""))))
            except (TypeError, ValueError):
                delay = min(3.0, .75 * (attempt + 1))
            time.sleep(delay)
    raise AssertionError("unreachable: bounded admin retry loop exhausted")


def start_fake_upstream(kind: str) -> ThreadingHTTPServer:
    """Loopback-only test upstream: HTTP 500 for A and valid chat reply for B."""
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b""
            if kind == "fail":
                body = json.dumps({
                    "error": {
                        "message": "e2e deterministic upstream failure",
                        "type": "server_error",
                    }
                }).encode()
                self.send_response(500)
                self.send_header("Content-Type", "application/json")
            else:
                try:
                    request = json.loads(raw or b"{}")
                except Exception:
                    request = {}
                if request.get("stream"):
                    chunks = [
                        {"id": "chatcmpl-e2e-fb", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"role": "assistant", "content": "fallback "}, "finish_reason": None}]},
                        {"id": "chatcmpl-e2e-fb", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"content": "answer from B"}, "finish_reason": None}]},
                        {"id": "chatcmpl-e2e-fb", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}},
                    ]
                    body = ("".join("data: " + json.dumps(chunk) + "\n\n" for chunk in chunks) + "data: [DONE]\n\n").encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    return
                body = json.dumps({
                    "id": "chatcmpl-e2e-fb",
                    "object": "chat.completion",
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": "fallback answer from B"}, "finish_reason": "stop"}],
                    "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
                }).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        do_GET = do_POST

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def main() -> None:
    chromium = shutil.which("chromium") or shutil.which("google-chrome")
    if not chromium:
        raise SystemExit("SKIP: Chromium is not installed")

    with tempfile.TemporaryDirectory(prefix="nexaroute-browser-502-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        config.update({
            "providers": [],
            "virtual_endpoints": [],
            "route_profiles": [],
            "candidate_pools": [],
            "fallback_chains": [],
        })
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        # These fake providers are intentionally Unknown/unprobed. Priority
        # accepts them so the browser exercises real fallback traffic.
        config["routing"]["strategy"] = "priority"
        config["listen"] = f"127.0.0.1:{free_port()}"
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
        log = (tmp_path / "gateway.log").open("w")
        proc = subprocess.Popen(
            ["go", "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)],
            cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT, text=True,
        )
        base = f"http://{config['listen']}"
        fail_upstream = None
        ok_upstream = None
        try:
            wait_ready(base, proc)
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 900})
                page.set_default_timeout(12000)
                console_errors: list[str] = []
                page.on("console", lambda msg: console_errors.append(msg.text) if msg.type == "error" else None)
                page.route("**/admin/api/provider-discover", lambda route: route.fulfill(
                    status=200, content_type="application/json",
                    body=json.dumps({"ok": True, "models": ["model-alpha", "model-beta"]}),
                ))
                page.route("**/admin/api/provider-check", lambda route: route.fulfill(
                    status=200, content_type="application/json",
                    body=json.dumps({"ok": True, "status_code": 200, "latency_ms": 1}),
                ))
                page.route("**/admin/api/provider-test", lambda route: route.fulfill(
                    status=200, content_type="application/json",
                    body=json.dumps({"ok": True, "passed": 1, "total": 1, "results": [{"model": "model-alpha", "ok": True, "latency_ms": 1}]}),
                ))
                page.goto(base + "/", wait_until="domcontentloaded")

                # 1. Named first-run state before anything exists.
                expect(page.locator("#content")).to_contain_text("Add and test your first")
                page.locator('[data-page="providers"]').click()
                # .first: the providers page renders a second .empty-state for the
                # preset catalog section until the async /admin/api/provider-presets
                # fetch resolves; a bare strict-mode locator throws immediately when
                # two match, which flakes on catalog timing. The provider section
                # always renders before the preset section, so .first is stable.
                expect(page.locator(".empty-state").first).to_contain_text("Connect your first provider")
                page.locator('[data-page="routing"]').click()
                expect(page.locator(".empty-state")).to_contain_text("Create your first public route")

                # 2. Discover, manually add and bounded-test a model through the UI.
                page.locator('[data-page="providers"]').click()
                page.locator('[data-action="add-provider"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Add provider")
                page.locator("#p-name").fill("E2E Onboarding")
                page.locator("#p-base").fill("http://127.0.0.1:9/v1")
                page.locator('[data-action="detect-models"]').click()
                expect(page.locator("#model-list")).to_contain_text("model-alpha")
                expect(page.locator("#model-list")).to_contain_text("model-beta")
                expect(page.locator('#model-list .model-select[value="model-alpha"]')).to_have_attribute("data-source", "discovered")
                page.locator("#p-manual").fill("model-manual")
                page.locator('[data-action="add-manual-model"]').click()
                expect(page.locator('#model-list .model-select[value="model-manual"]')).to_have_attribute("data-source", "manual")
                page.locator('[data-action="test-model"][data-model="model-alpha"]').click()
                expect(page.locator("#notice")).to_contain_text("model-alpha passed")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("E2E Onboarding")
                page.locator('[data-action="edit-provider"]').first.click()
                expect(page.locator('#model-list .model-select[value="model-alpha"]')).to_have_attribute("data-source", "discovered")
                expect(page.locator('#model-list .model-select[value="model-manual"]')).to_have_attribute("data-source", "manual")
                page.locator('[data-action="close-modal"]').first.click()

                # The provider preset CTA must be live and prefill the form.
                page.locator('.preset-tile [data-action="use-preset"]').first.click()
                expect(page.locator("#modal-title")).to_contain_text("Add provider —")
                assert page.locator("#p-name").input_value().strip(), "Use preset must prefill provider name"
                page.locator('[data-action="close-modal"]').first.click()

                # 3. Configure two loopback fake upstreams and inspect backend-compiled fallback.
                fail_upstream = start_fake_upstream("fail")
                ok_upstream = start_fake_upstream("ok")
                fail_port = fail_upstream.server_address[1]
                ok_port = ok_upstream.server_address[1]
                for provider_id, port in (("e2e-prov-a", fail_port), ("e2e-prov-b", ok_port)):
                    created = admin_json(base, "/admin/api/providers", "POST", {"provider": {
                        "id": provider_id, "name": provider_id, "type": "openai_compatible",
                        "base_url": f"http://127.0.0.1:{port}/v1",
                        "api_key": "e2e-fake-key", "auth_mode": "bearer", "enabled": True,
                        "models": [{"id": "m", "model": "m", "enabled": True, "capabilities": {"streaming": True}}],
                    }})
                    assert created, f"provider creation returned no response: {provider_id}"
                route_created = admin_json(base, "/admin/api/simple-routes", "POST", {
                    "id": "e2e-fb-route", "name": "E2E Fallback", "public_model": "e2e-fb",
                    "mode": "ordered", "deployments": ["e2e-prov-a/m", "e2e-prov-b/m"], "enabled": True,
                })
                assert route_created.get("saved") is True, f"backend did not confirm fallback route: {route_created}"

                # API mutations occur outside the SPA; reload and verify the compiled route.
                page.reload(wait_until="domcontentloaded")
                page.locator('[data-page="routing"]').click()
                fb_tile = page.locator(".route-tile", has_text="e2e-fb")
                expect(fb_tile).to_be_visible()
                expect(fb_tile).to_contain_text("Ordered fallback")
                expect(fb_tile).to_contain_text("e2e-fb-route-stage-1 → e2e-fb-route-stage-2")

                # 4. No browser-boundary mock: fail A with a real 500, observe success from B.
                page.locator('[data-page="playground"]').click()
                page.locator("#pg-model").select_option("e2e-fb")
                page.locator("#pg-prompt").fill("prove the fallback")
                page.locator("#pg-send").click()
                fb_msg = page.locator(".pg-msg.assistant").last
                expect(fb_msg).to_contain_text("fallback answer from B")
                expect(fb_msg.locator(".pg-badge")).to_contain_text("e2e-prov-a/m → e2e-prov-b/m")
                expect(fb_msg.locator(".pg-badge")).to_contain_text("e2e-prov-b")
                page.locator('[data-page="activity"]').click()
                expect(page.locator("#content")).to_contain_text("e2e-prov-b")
                assert not console_errors, f"unexpected browser console errors: {console_errors}"
                browser.close()
        finally:
            if fail_upstream:
                fail_upstream.shutdown()
                fail_upstream.server_close()
            if ok_upstream:
                ok_upstream.shutdown()
                ok_upstream.server_close()
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            log.close()
    print("BROWSER E2E #502 PASS")


if __name__ == "__main__":
    main()

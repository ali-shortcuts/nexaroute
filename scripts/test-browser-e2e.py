#!/usr/bin/env python3
"""Browser acceptance for the v0.16.1 backend-driven control plane.

The Go gateway, persistence, routing mutations and snapshot reads are real. Only
provider discovery/check/test calls are intercepted at the browser boundary so
this suite is deterministic and does not contact a paid upstream provider.
"""
from __future__ import annotations

import http.client
import json
import os
import re
from pathlib import Path
import shutil
import sys
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
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]

def wait_ready(base: str, proc: subprocess.Popen[str]) -> None:
    # Startup tolerance for a loaded machine: `go run` compiles
    # the gateway before it can answer /healthz. This bounds only
    # startup, never an acceptance assertion.
    deadline = time.time() + 120
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

def console_errors(page):
    errors = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
    return errors

def admin_json(base: str, path: str, method: str, body: dict | None = None) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, headers={"Content-Type": "application/json"}, method=method)
    # The complete browser suite deliberately drives many admin requests from
    # one loopback source. Respect a short 429 cooldown for this out-of-browser
    # verification helper rather than failing unrelated assertions because the
    # shared test IP bucket was briefly exhausted. Browser fetches themselves
    # are not intercepted/retried: a visible UI/API rate-limit error still fails
    # the acceptance scenario.
    for attempt in range(6):
        try:
            with urllib.request.urlopen(req, timeout=10) as response:
                raw = response.read()
                return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as exc:
            if exc.code != 429 or attempt == 5:
                raise
            retry_after = exc.headers.get("Retry-After", "")
            try:
                delay = max(0.5, min(3.0, float(retry_after)))
            except (TypeError, ValueError):
                delay = min(3.0, 0.75 * (attempt + 1))
            time.sleep(delay)
    raise AssertionError("unreachable: bounded admin 429 retry loop exhausted")

def redact_text(text: str, extra_values) -> str:
    # Same contract as scripts/run-browser-acceptance.py: diagnostic
    # files must never carry a test canary or a credential shape.
    for value in extra_values:
        if value:
            text = text.replace(value, "[REDACTED]")
    for pattern, replacement in [
        (re.compile(r"ISSUE393_BROWSER_[A-Za-z0-9_]+"), "[REDACTED-CANARY]"),
        (re.compile(r"(?i)Bearer\s+\S+"), "Bearer [REDACTED]"),
        (re.compile(r"(?i)(x-admin-key[\"':\s=]+)\S+"), r"\1[REDACTED]"),
        (re.compile(r"(?i)(cookie:\s*).*"), r"\1[REDACTED]"),
        (re.compile(r"(?i)(\"api_key\"\s*:\s*\")[^\"]+(\")"), r"\1[REDACTED]\2"),
    ]:
        text = pattern.sub(replacement, text)
    return text

def reload_until_connected(page, timeout: int = 60) -> None:
    """Reload until the dashboard reports Connected, or fail honestly.

    The dashboard refreshes only on page load, and its seven parallel
    admin fetches share the gateway's per-IP admin token bucket with
    every other loopback caller -- including this suite's own admin
    helper and the DNS-rebinding probe, whose unauthenticated request
    costs the full bucket (the F11 anti-oracle rule: a throttled
    unauthenticated request must stay indistinguishable from a plain
    401). A reload that lands while the bucket is empty is throttled
    with 429s and the dashboard stays Unavailable until the bucket
    refills (1.5 tokens/s). That is legitimate gateway behavior, not
    a broken recovery, so a single 5s assertion after one reload is
    the wrong synchronization: retry the reload -- the only way to
    re-trigger a refresh -- until Connected or the deadline. Each
    attempt is a real refresh, not a sleep; the 1s pace only keeps
    retries from outrunning the refill. A genuinely broken recovery
    (real 500s) still fails at the deadline instead of passing.
    """
    deadline = time.time() + timeout
    last_error = ""
    while True:
        page.reload(wait_until="domcontentloaded")
        try:
            expect(page.locator("#connection-text")).to_have_text("Connected")
            return
        except AssertionError as exc:
            last_error = (str(exc).splitlines() or [""])[0]
        if time.time() >= deadline:
            raise AssertionError(
                f"dashboard did not recover to Connected within {timeout}s; last: {last_error}"
            )
        time.sleep(1)

def main() -> None:
    # NEXAROUTE_CHROMIUM_BIN lets the acceptance runner point the suite
    # at Playwright's bundled Chromium, which `playwright install` does
    # not put on PATH. Exit code 2 + the SKIPPED marker is the runner's
    # contract: a skip must never look like a pass (and, unlike the old
    # bare SystemExit string, it is distinguishable from a failure).
    chromium = os.environ.get("NEXAROUTE_CHROMIUM_BIN") or shutil.which("chromium") or shutil.which("google-chrome")
    if not chromium:
        print("BROWSER SUITE: SKIPPED: Chromium is not installed", file=sys.stderr)
        raise SystemExit(2)
    evidence_setting = os.environ.get("NEXAROUTE_BROWSER_EVIDENCE_DIR")
    evidence_root = Path(evidence_setting) if evidence_setting else None
    if evidence_root and not evidence_root.is_absolute():
        evidence_root = ROOT / evidence_root
    with tempfile.TemporaryDirectory(prefix="nexaroute-browser-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        config.update({"providers": [], "virtual_endpoints": [], "route_profiles": [], "candidate_pools": [], "fallback_chains": []})
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        test_video_token = "m2-1-local-only-test-token"
        video_config = config.setdefault("video", {})
        video_config.update({
            "enabled": True,
            "auth_token_env": "NEXAROUTE_TEST_VIDEO_TOKEN",
            "development_fake_provider": True,
            "store_path": str(tmp_path / "video-jobs.json"),
        })
        port = free_port()
        config["listen"] = f"127.0.0.1:{port}"
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
        env["NEXAROUTE_TEST_VIDEO_TOKEN"] = test_video_token
        env["NEXA_ISSUE393_BROWSER_KEY"] = "ISSUE393_BROWSER_ENV_CANARY_9f31"
        env["NEXA_ISSUE393_BROWSER_POOL"] = "ISSUE393_BROWSER_POOL_ENV_CANARY_2da4"
        log = (tmp_path / "gateway.log").open("w")
        proc = subprocess.Popen(["go", "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT, text=True)
        base = f"http://127.0.0.1:{port}"
        # Sanitized diagnostics for startup/health failures (issue
        # #686): every /admin/api/ response the page sees, path and
        # status only -- never bodies or headers -- so a future
        # Unavailable can be attributed to a stubbed 500, a gateway
        # 429, or a missing reply. Defined here so the finally block
        # can read it even if the page never started.
        admin_api_responses: list = []
        try:
            wait_ready(base, proc)
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 900}, accept_downloads=True)
                page.set_default_timeout(12000)
                page.add_init_script(f"sessionStorage.setItem('nexaroute_video_token', {json.dumps(test_video_token)});")
                
                console_msgs = []
                expected_disabled_404 = {"active": False}
                expected_issue393_validation = {"active": False}
                expected_snapshot_500 = {"active": False}
                # Throttled refresh fetches log "Failed to load resource:
                # 429" console errors. During a recovery window that
                # throttling is the gateway's correct rate limiting,
                # not a product defect, so those messages -- and only
                # those, and only inside the window -- are filtered
                # like the expected snapshot 500 above. Everything else
                # still reaches the final console-errors assertion.
                expected_throttled_429 = {"active": False}
                def record_console(msg):
                    if msg.type == "error" and expected_disabled_404["active"] and "404 (Not Found)" in msg.text:
                        return
                    if msg.type == "error" and expected_issue393_validation["active"] and "400 (Bad Request)" in msg.text:
                        return
                    if msg.type == "error" and expected_snapshot_500["active"] and "500 (Internal Server Error)" in msg.text:
                        return
                    if msg.type == "error" and expected_throttled_429["active"] and "429 (Too Many Requests)" in msg.text:
                        return
                    console_msgs.append((msg.type, msg.text))
                page.on("console", record_console)
                def record_admin_response(response):
                    try:
                        url = response.url
                    except Exception:
                        return
                    if "/admin/api/" in url:
                        admin_api_responses.append({"path": url, "status": response.status})
                page.on("response", record_admin_response)

                page.route("**/admin/api/provider-discover", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "models": ["model-alpha", "model-beta"]})))
                page.route("**/admin/api/provider-check", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "status_code": 200, "latency_ms": 1})))
                page.route("**/admin/api/provider-test", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "passed": 1, "total": 1, "results": [{"model": "model-alpha", "ok": True, "latency_ms": 1}]})))
                page.goto(base + "/", wait_until="domcontentloaded")
                
                # Check console errors
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors: {errors}"
                
                # --- #502: first-run empty state (named scenario) ---
                # The gateway booted with zero providers/routes, so the
                # first-run surfaces must say so honestly before anything
                # is created in this suite.
                expect(page.locator("#content")).to_contain_text("Add and test your first")
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                # .first: the providers page renders a second .empty-state for the
                # preset catalog section until the async /admin/api/provider-presets
                # fetch resolves; a bare strict-mode locator throws immediately when
                # two match, which flakes on catalog timing. The provider section
                # always renders before the preset section, so .first is stable.
                expect(page.locator(".empty-state").first).to_contain_text("Connect your first provider")
                assert page.locator(".provider-tile:not(.preset-tile)").count() == 0, "first run must show no configured provider tiles"
                page.locator('[data-page="routing"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator(".empty-state")).to_contain_text("Create your first public route")
                page.locator('[data-page="activity"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator(".empty-state")).to_contain_text("No real activity yet")
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors after first-run empty state: {errors}"

                # --- PRIVACY P3 TESTS ---
                # Go to providers page
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                
                # Add provider with trains_on_data=yes (warning)
                page.locator('[data-action="add-provider"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Add provider")
                page.locator("#p-name").fill("Provider Trains Yes")
                page.locator("#p-base").fill("http://127.0.0.1:9/v1")
                page.locator("#p-trains").select_option("yes")
                page.locator("#p-retention").select_option("limited")
                page.locator("#p-dh-note").fill("Trains on user data")
                page.locator('[data-action="detect-models"]').click()
                expect(page.locator("#model-list")).to_contain_text("model-alpha")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("Provider Trains Yes")
                
                # Add provider with trains_on_data=no (safe)
                page.locator('[data-action="add-provider"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Add provider")
                page.locator("#p-name").fill("Provider Trains No")
                page.locator("#p-base").fill("http://127.0.0.1:10/v1")
                page.locator("#p-trains").select_option("no")
                page.locator("#p-retention").select_option("none")
                page.locator("#p-dh-note").fill("No training")
                page.locator('[data-action="detect-models"]').click()
                expect(page.locator("#model-list")).to_contain_text("model-alpha")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("Provider Trains No")
                
                # Add provider with trains_on_data=unknown (neutral)
                page.locator('[data-action="add-provider"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Add provider")
                page.locator("#p-name").fill("Provider Unknown")
                page.locator("#p-base").fill("http://127.0.0.1:11/v1")
                page.locator("#p-trains").select_option("unknown")
                page.locator("#p-retention").select_option("unknown")
                page.locator("#p-dh-note").fill("Unknown")
                page.locator('[data-action="detect-models"]').click()
                expect(page.locator("#model-list")).to_contain_text("model-alpha")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("Provider Unknown")
                
                # Verify badges are visible for all three values
                # Check for "Trains on data" (yes = warn)
                expect(page.locator(".provider-tile").first).to_contain_text("Trains on data")
                # Check for "Does not train" (no = good)
                expect(page.locator(".provider-tile").nth(1)).to_contain_text("Does not train")
                # Check for "Training unknown" (unknown = neutral)
                expect(page.locator(".provider-tile").nth(2)).to_contain_text("Training unknown")
                
                # Check retention badges
                expect(page.locator(".provider-tile").first).to_contain_text("Limited retention")
                expect(page.locator(".provider-tile").nth(1)).to_contain_text("No retention")
                expect(page.locator(".provider-tile").nth(2)).to_contain_text("Retention unknown")
                
                # Test edit-and-save persists (edit the "Provider Unknown" which is the 3rd tile)
                page.locator('[data-action="edit-provider"]').nth(2).click()
                expect(page.locator("#modal-title")).to_have_text("Edit provider")
                expect(page.locator("#p-trains")).to_have_value("unknown")
                expect(page.locator("#p-retention")).to_have_value("unknown")
                expect(page.locator("#p-dh-note")).to_have_value("Unknown")
                page.locator("#p-trains").select_option("yes")
                page.locator("#p-retention").select_option("limited")
                page.locator("#p-dh-note").fill("Updated note")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("Provider Unknown")
                # Verify the edit persisted by reopening
                page.locator('[data-action="edit-provider"]').nth(2).click()
                expect(page.locator("#p-trains")).to_have_value("yes")
                expect(page.locator("#p-retention")).to_have_value("limited")
                expect(page.locator("#p-dh-note")).to_have_value("Updated note")
                page.locator('[data-action="close-modal"]').first.click()


                # UI FIX #242: ensure final Providers state has >=3 providers incl. one unknown
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                page.locator('[data-action="add-provider"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Add provider")
                page.locator("#p-name").fill("Provider Unknown Kept")
                page.locator("#p-base").fill("http://127.0.0.1:12/v1")
                page.locator("#p-trains").select_option("unknown")
                page.locator("#p-retention").select_option("unknown")
                page.locator("#p-dh-note").fill("Unknown kept")
                page.locator('[data-action="detect-models"]').click()
                expect(page.locator("#model-list")).to_contain_text("model-alpha")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("Provider Unknown Kept")
                
                # Check console errors after provider operations
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors after provider ops: {errors}"
                
                # --- Routing page: privacy setting ---
                page.locator('[data-page="routing"]').click()
                page.wait_for_load_state("domcontentloaded")
                page.locator('[data-action="add-route"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Create route")
                page.locator("#r-name").fill("Coding")
                page.locator("#r-public").fill("coding")
                expect(page.locator(".modal-body")).to_contain_text("model-alpha")
                page.locator('.modal-body input[type="checkbox"]').first.check()
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#content")).to_contain_text("coding")
                
                # Check route privacy display (default is empty/any)
                expect(page.locator(".route-tile")).to_contain_text("Privacy")
                
                # Check console errors
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors after routing: {errors}"
                
                # --- Screenshots for evidence ---
                evidence_dir = evidence_root or ROOT / "docs/browser-evidence/privacy"
                evidence_dir.mkdir(parents=True, exist_ok=True)
                
                # 1440x900 dark
                page.set_viewport_size({"width": 1440, "height": 900})
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                time.sleep(0.5)
                page.screenshot(path=str(evidence_dir / "providers-1440x900-dark.png"), full_page=True)
                
                # 1440x900 light
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "providers-1440x900-light.png"), full_page=True)
                
                # 390x844 dark
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                page.set_viewport_size({"width": 390, "height": 844})
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "providers-390x844-dark.png"), full_page=True)
                
                # 390x844 light
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "providers-390x844-light.png"), full_page=True)
                
                # Check horizontal overflow at 390px
                body_width = page.evaluate("() => document.body.scrollWidth")
                viewport_width = page.evaluate("() => window.innerWidth")
                assert body_width <= viewport_width + 1, f"Horizontal overflow at 390px: body={body_width}, viewport={viewport_width}"
                
                # Routing page screenshots
                page.set_viewport_size({"width": 1440, "height": 900})
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                page.locator('[data-page="routing"]').click()
                page.wait_for_load_state("domcontentloaded")
                time.sleep(0.5)
                page.screenshot(path=str(evidence_dir / "routing-1440x900-dark.png"), full_page=True)
                
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "routing-1440x900-light.png"), full_page=True)
                
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                page.set_viewport_size({"width": 390, "height": 844})
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "routing-390x844-dark.png"), full_page=True)
                
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "routing-390x844-light.png"), full_page=True)
                
                # Check horizontal overflow at 390px for routing
                body_width = page.evaluate("() => document.body.scrollWidth")
                viewport_width = page.evaluate("() => window.innerWidth")
                assert body_width <= viewport_width + 1, f"Horizontal overflow at 390px routing: body={body_width}, viewport={viewport_width}"
                
                # Activity page screenshots
                page.set_viewport_size({"width": 1440, "height": 900})
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                page.locator('[data-page="activity"]').click()
                page.wait_for_load_state("domcontentloaded")
                time.sleep(0.5)
                page.screenshot(path=str(evidence_dir / "activity-1440x900-dark.png"), full_page=True)
                
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "activity-1440x900-light.png"), full_page=True)
                
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                page.set_viewport_size({"width": 390, "height": 844})
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "activity-390x844-dark.png"), full_page=True)
                
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                page.screenshot(path=str(evidence_dir / "activity-390x844-light.png"), full_page=True)
                
                # Check horizontal overflow at 390px for activity
                body_width = page.evaluate("() => document.body.scrollWidth")
                viewport_width = page.evaluate("() => window.innerWidth")
                assert body_width <= viewport_width + 1, f"Horizontal overflow at 390px activity: body={body_width}, viewport={viewport_width}"

                # --- UI FIX #242 ACCEPTANCE ---
                titles = {"overview": "Overview", "providers": "Providers", "routing": "Routing", "video": "Video Studio", "activity": "Activity", "settings": "Settings"}
                page.set_viewport_size({"width": 1440, "height": 900})
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                content_tops = {}
                title_tops = {}
                for dest, title in titles.items():
                    page.locator(f'[data-page="{dest}"]').click()
                    page.wait_for_load_state("domcontentloaded")
                    time.sleep(0.3)
                    count = page.evaluate("""(t) => {
                      const want = t.toLowerCase();
                      const els = [...document.querySelectorAll('main h1, main h2')].filter(e => {
                        const r = e.getBoundingClientRect();
                        if (!r.width || !r.height) return false;
                        const s = getComputedStyle(e);
                        if (s.display === 'none' || s.visibility === 'hidden') return false;
                        return e.textContent.trim().toLowerCase() === want;
                      });
                      return els.length;
                    }""", title)
                    assert count == 1, f"page {dest}: expected exactly one visible title {title!r}, got {count}"
                    metrics = page.evaluate("""() => {
                      const n = document.querySelector('#notice'); if (n) n.hidden = true;
                      window.scrollTo(0, 0);
                      return {
                        title: document.querySelector('#page-title').getBoundingClientRect().top,
                        content: document.querySelector('#content').getBoundingClientRect().top
                      };
                    }""")
                    title_tops[dest] = metrics["title"]
                    content_tops[dest] = metrics["content"]
                tops = list(content_tops.values())
                assert max(tops) - min(tops) <= 2, f"inconsistent content top offset: {content_tops}"
                title_offsets = list(title_tops.values())
                assert max(title_offsets) - min(title_offsets) <= 2, f"inconsistent title top offset: {title_tops}"
                # Check the real Video Studio empty-state CTA, not a synthetic probe.
                page.locator('[data-page="video"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator(".video-empty")).to_be_visible()
                expect(page.locator(".video-orbit")).to_contain_text("available")
                gap = page.evaluate("""() => {
                  const empty = document.querySelector('.video-empty');
                  const p = empty?.querySelector('p');
                  const b = empty?.querySelector('button');
                  if (!p || !b) return null;
                  const pr = p.getBoundingClientRect();
                  const br = b.getBoundingClientRect();
                  return br.top - pr.bottom;
                }""")
                assert gap is not None and gap >= 12, f"Video Studio empty-state button touches description: gap={gap}px"
                # Badge labels must map to the expected semantic class, with a distinct color per state.
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                time.sleep(0.3)
                def assert_privacy_badges() -> dict[str, str]:
                    expected = {"Trains on data": "warn", "Does not train": "good", "Training unknown": "neutral"}
                    states = page.locator(".provider-tile .data-handling > .status:first-child").evaluate_all(
                        "els => els.map(e => ({label: e.textContent.trim(), classes: [...e.classList]}))"
                    )
                    observed = set()
                    for state in states:
                        label = state["label"]
                        observed.add(label)
                        assert expected.get(label) in state["classes"], f"badge mapping is wrong: {state}"
                    assert observed == set(expected), f"expected all privacy states, observed {observed}"
                    colors = page.evaluate("""() => Object.fromEntries(
                      ['warn', 'good', 'neutral'].map(k => [k, getComputedStyle(document.querySelector(`.data-handling > .status.${k}`)).color])
                    )""")
                    assert len(set(colors.values())) == 3, f"privacy badge colors must be distinct: {colors}"
                    return colors
                dark_colors = assert_privacy_badges()  # dark theme
                # Same semantic mapping and distinct colors in light theme.
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                light_colors = assert_privacy_badges()
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                # Mobile 390x844: every nav destination visible/clickable, no horizontal overflow
                page.set_viewport_size({"width": 390, "height": 844})
                time.sleep(0.4)
                for dest in titles:
                    box = page.evaluate("""(d) => {
                      const el = document.querySelector(`[data-page=\"${d}\"]`);
                      if (!el) return null;
                      const r = el.getBoundingClientRect();
                      return {x: r.x, y: r.y, w: r.width, h: r.height, vw: window.innerWidth, vh: window.innerHeight};
                    }""", dest)
                    assert box is not None, f"nav {dest} missing at 390px"
                    assert box["w"] > 0 and box["h"] > 0, f"nav {dest} has zero size at 390px: {box}"
                    assert box["x"] >= -1 and box["x"] + box["w"] <= box["vw"] + 1, f"nav {dest} cut off at 390px: {box}"
                    expect(page.locator(f'[data-page="{dest}"]')).to_be_visible()
                    expect(page.locator(f'[data-page="{dest}"]')).to_be_enabled()
                    page.locator(f'[data-page="{dest}"]').click()
                    page.wait_for_load_state("domcontentloaded")
                    bw = page.evaluate("() => document.body.scrollWidth")
                    vw = page.evaluate("() => window.innerWidth")
                    assert bw <= vw + 1, f"horizontal overflow on {dest} at 390px: body={bw} viewport={vw}"
                # Versioned override lets acceptance runs commit their own browser evidence.
                # Default paths remain stable for the existing CI acceptance job.
                fix_dir = evidence_root or ROOT / "docs/browser-evidence/ui-fix"
                fix_dir.mkdir(parents=True, exist_ok=True)
                page.evaluate("() => { const n = document.querySelector('#notice'); if (n) n.hidden = true; }")
                for dest in ["overview", "providers", "routing", "video", "activity", "settings"]:
                    page.set_viewport_size({"width": 1440, "height": 900})
                    page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                    page.locator(f'[data-page="{dest}"]').click()
                    page.wait_for_load_state("domcontentloaded")
                    time.sleep(0.5)
                    page.screenshot(path=str(fix_dir / f"{dest}-1440x900-dark.png"))
                    page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                    time.sleep(0.3)
                    page.screenshot(path=str(fix_dir / f"{dest}-1440x900-light.png"))
                    page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                    page.set_viewport_size({"width": 390, "height": 844})
                    time.sleep(0.4)
                    page.screenshot(path=str(fix_dir / f"{dest}-390x844-dark.png"))
                    page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                    time.sleep(0.3)
                    page.screenshot(path=str(fix_dir / f"{dest}-390x844-light.png"))

                # --- Video Studio multi-shot submission, status, and download regression ---
                created_payload = {}
                def mock_video_providers(route):
                    route.fulfill(status=200, content_type="application/json", body=json.dumps({"providers": [{"id": "fake"}]}))
                def mock_video_create(route):
                    created_payload["request"] = json.loads(route.request.post_data or "{}")
                    route.fulfill(status=202, content_type="application/json", body=json.dumps({
                        "job_id": "ui-episode-1", "state": "queued", "progress": 0,
                        "request": created_payload["request"],
                    }))
                def mock_video_job(route):
                    route.fulfill(status=200, content_type="application/json", body=json.dumps({
                        "job_id": "ui-episode-1", "state": "completed", "progress": 1,
                        "current_shot": 2, "total_shots": 2, "provider": "fake",
                        "created_at": "2026-10-12T00:00:00Z",
                        "request": created_payload["request"],
                        "output_assets": [{
                            "id": "asset-1", "kind": "video", "filename": "episode.mp4",
                            "content_type": "video/mp4", "size_bytes": 14,
                            "sha256": "0000000000000000000000000000000000000000000000000000000000000000",
                            "uri": "/v1/video/jobs/ui-episode-1/assets/asset-1",
                        }],
                    }))
                def mock_video_asset(route):
                    assert route.request.headers.get("authorization") == f"Bearer {test_video_token}"
                    route.fulfill(status=200, headers={
                        "Content-Type": "video/mp4",
                        "Content-Disposition": "attachment; filename=episode.mp4",
                    }, body=b"fake-mp4-bytes")
                page.route("**/v1/video/providers", mock_video_providers)
                page.route("**/v1/video/jobs", mock_video_create)
                page.route("**/v1/video/jobs/ui-episode-1", mock_video_job)
                page.route("**/v1/video/jobs/ui-episode-1/assets/asset-1", mock_video_asset)
                page.set_viewport_size({"width": 1440, "height": 900})
                page.locator('[data-page="video"]').click()
                expect(page.locator('[data-action="create-video"]').first).to_be_visible()
                page.locator('[data-action="create-video"]').first.click()
                page.locator('[data-action="example-shot-plan"]').click()
                page.locator("#v-episode").fill("noora-persepolis-01")
                page.locator("#v-narration").fill("Noora introduces Persepolis and explains its history.")
                page.locator("#v-subtitle-language").select_option("same-as-narration")
                plan = json.loads(page.locator("#v-shot-plan").input_value())
                assert len(plan) == 2, f"example shot plan length={len(plan)}"
                page.get_by_role("button", name="Queue job").click()
                expect(page.locator(".video-job-card")).to_contain_text("completed")
                expect(page.locator(".video-job-card")).to_contain_text("Shot 2 / 2")
                expect(page.get_by_role("button", name="Download episode.mp4")).to_be_visible()
                submitted = created_payload["request"]
                assert submitted.get("provider_preference") == "fake", f"provider preference={submitted.get('provider_preference')!r}"
                assert submitted.get("prompt") == "", f"multi-shot parent prompt should be empty: {submitted.get('prompt')!r}"
                assert len(submitted.get("shot_plan", [])) == 2, f"submitted shot plan={submitted.get('shot_plan')!r}"
                assert submitted.get("duration_seconds") == 8, f"total duration={submitted.get('duration_seconds')!r}"
                assert submitted.get("episode_id") == "noora-persepolis-01"
                assert submitted.get("narration_text") == "Noora introduces Persepolis and explains its history.", f"narration text={submitted.get('narration_text')!r}"
                assert submitted.get("subtitle_language") == "same-as-narration", f"subtitle language={submitted.get('subtitle_language')!r}"
                with page.expect_download(timeout=12000) as download_info:
                    page.get_by_role("button", name="Download episode.mp4").click()
                downloaded = tmp_path / "video-studio-download.mp4"
                download_info.value.save_as(str(downloaded))
                assert downloaded.read_bytes() == b"fake-mp4-bytes", "downloaded Video Studio asset bytes differ"
                page.unroute("**/v1/video/providers", mock_video_providers)
                page.unroute("**/v1/video/jobs", mock_video_create)
                page.unroute("**/v1/video/jobs/ui-episode-1", mock_video_job)
                page.unroute("**/v1/video/jobs/ui-episode-1/assets/asset-1", mock_video_asset)

                # --- Issue #414 item 9: Video Studio failure scenarios ---
                # (a) a failed job served from server history surfaces its
                # error and its retry-safety verdict (no silent dead card);
                # (b) a malformed providers payload degrades the Studio to
                # its no-provider state with the app still navigable;
                # (c) after a browser restart (reload) a created job is
                # recovered from server history with NO resubmission.
                # One dispatcher stays routed for the whole section so no
                # background poll ever reaches the real gateway and leaves
                # a 404 console error behind. It needs BOTH jobs patterns:
                # Playwright's trailing "**" does not cross "/", so
                # "**/v1/video/jobs**" matches the list URL but NOT
                # "/v1/video/jobs/<id>" — tracked-job fetches would
                # otherwise hit the real gateway (which has no such job)
                # and 404, surfacing at the next console assertion.
                fail_job = {
                    "job_id": "ui-fail-1", "state": "failed", "progress": 0,
                    "provider": "fake", "created_at": "2026-10-12T00:00:00Z",
                    "last_error": "provider rejected the brief: content policy",
                    "retry_safety": {"verdict": "duplicate_risk",
                                     "reason": "the provider accepted a submission; retrying may bill twice",
                                     "provider_job_id_present": True,
                                     "duplicate_billing_possible": True},
                    "request": {"prompt": "a lighthouse at dawn", "provider_preference": "fake"},
                }
                recover_job = {
                    "job_id": "ui-recover-1", "state": "completed", "progress": 1,
                    "provider": "fake", "created_at": "2026-10-12T00:00:00Z",
                    "request": {"prompt": "harbor timelapse", "provider_preference": "fake"},
                    "output_assets": [],
                }
                recover_posts = {"n": 0}
                jobs_mode = {"mode": "fail"}
                def mock_providers_ok(route):
                    route.fulfill(status=200, content_type="application/json", body=json.dumps({"providers": [{"id": "fake"}]}))
                def mock_jobs_dispatch(route):
                    url = route.request.url
                    if route.request.method == "POST":
                        if jobs_mode["mode"] == "recover":
                            recover_posts["n"] += 1
                            route.fulfill(status=202, content_type="application/json", body=json.dumps({
                                "job_id": "ui-recover-1", "state": "queued", "progress": 0,
                                "request": json.loads(route.request.post_data or "{}"),
                            }))
                        else:
                            route.fulfill(status=405, content_type="application/json", body="{}")
                        return
                    if "/jobs/" in url:
                        jid = url.rsplit("/jobs/", 1)[-1].split("?")[0]
                        job = recover_job if jid == "ui-recover-1" else {
                            "job_id": jid, "state": "completed", "progress": 1,
                            "provider": "fake", "created_at": "2026-10-12T00:00:00Z",
                            "request": {}, "output_assets": [],
                        }
                        route.fulfill(status=200, content_type="application/json", body=json.dumps(job))
                        return
                    jobs = [fail_job] if jobs_mode["mode"] == "fail" else [recover_job]
                    route.fulfill(status=200, content_type="application/json", body=json.dumps({"jobs": jobs, "durability": "file"}))
                page.route("**/v1/video/providers", mock_providers_ok)
                page.route("**/v1/video/jobs**", mock_jobs_dispatch)
                page.route("**/v1/video/jobs/**", mock_jobs_dispatch)
                page.locator('[data-page="video"]').click()
                page.locator('[data-action="refresh-video"]').click()
                fail_card = page.locator(".video-job-card", has_text="ui-fail-1")
                expect(fail_card).to_contain_text("failed")
                expect(fail_card).to_contain_text("provider rejected the brief")
                expect(fail_card).to_contain_text("Retry risks duplicate billing")
                expect(fail_card).to_contain_text("may bill twice")

                # (b) malformed providers payload -> no-provider state
                def mock_bad_providers(route):
                    route.fulfill(status=200, content_type="application/json", body="{not-json")
                page.unroute("**/v1/video/providers", mock_providers_ok)
                page.route("**/v1/video/providers", mock_bad_providers)
                page.locator('[data-action="refresh-video"]').click()
                expect(page.locator("#content")).to_contain_text("No provider exposed")
                page.locator('[data-page="providers"]').click()
                expect(page.locator('[data-action="add-provider"]').first).to_be_visible()
                page.unroute("**/v1/video/providers", mock_bad_providers)
                page.route("**/v1/video/providers", mock_providers_ok)

                # (c) restart recovery: create once, reload, history serves
                # the job back and nothing is submitted twice.
                jobs_mode["mode"] = "recover"
                page.locator('[data-page="video"]').click()
                page.locator('[data-action="create-video"]').first.click()
                page.locator('[data-action="example-shot-plan"]').click()
                page.locator("#v-episode").fill("ui-recover-ep")
                page.locator("#v-narration").fill("A harbor at dawn, boats leaving for the fishing grounds.")
                page.locator("#v-subtitle-language").select_option("same-as-narration")
                page.get_by_role("button", name="Queue job").click()
                recover_card = page.locator(".video-job-card", has_text="ui-recover-1")
                expect(recover_card).to_contain_text("completed")
                assert recover_posts["n"] == 1, f"recover submissions={recover_posts['n']}"
                page.reload(wait_until="domcontentloaded")
                page.locator('[data-page="video"]').click()
                expect(page.locator(".video-job-card", has_text="ui-recover-1")).to_contain_text("completed")
                assert recover_posts["n"] == 1, f"job resubmitted after reload: posts={recover_posts['n']}"
                page.unroute("**/v1/video/providers", mock_providers_ok)
                page.unroute("**/v1/video/jobs**", mock_jobs_dispatch)
                page.unroute("**/v1/video/jobs/**", mock_jobs_dispatch)

                # --- ISSUE #246: disabled video gateway must not raise a global banner ---
                # Isolate the disabled-state assertions from the fake provider enabled for M2-1 above.
                def disabled_video_providers(route):
                    route.fulfill(status=404, content_type="application/json", body=json.dumps({"code": "not_found", "message": "Video gateway is not enabled"}))
                expected_disabled_404["active"] = True
                page.route("**/v1/video/providers", disabled_video_providers)
                # The multi-shot section's tracked job (ui-episode-1) is
                # still fetched by background refreshes after its
                # dispatcher was removed; without a jobs dispatcher those
                # fetches hit the real gateway, 404, and fail the console
                # assertion below. Keep a stub dispatcher routed for this
                # section, as the #414 section does for its own. Both
                # patterns are required: Playwright's trailing "**" does
                # not cross "/", so the list pattern alone never matches
                # "/v1/video/jobs/<id>".
                def disabled_video_jobs(route):
                    if "/jobs/" in route.request.url:
                        route.fulfill(status=200, content_type="application/json", body=json.dumps({
                            "job_id": "ui-episode-1", "state": "completed", "progress": 1,
                            "provider": "fake", "created_at": "2026-10-12T00:00:00Z",
                            "request": {}, "output_assets": [],
                        }))
                    else:
                        route.fulfill(status=200, content_type="application/json", body=json.dumps({"jobs": [], "durability": "file"}))
                page.route("**/v1/video/jobs**", disabled_video_jobs)
                page.route("**/v1/video/jobs/**", disabled_video_jobs)
                page.set_viewport_size({"width": 1440, "height": 900})
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                for dest in ["overview", "providers", "routing", "activity", "settings"]:
                    page.locator(f'[data-page="{dest}"]').click()
                    page.wait_for_load_state("domcontentloaded")
                    time.sleep(0.6)
                    banner = page.evaluate("() => { const n = document.querySelector('#notice'); return (n && !n.hidden) ? n.textContent : ''; }")
                    assert "Video API" not in banner, f"global Video API banner on {dest}: {banner!r}"
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors with video disabled: {errors}"
                page.locator('[data-page="video"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator("#content")).to_contain_text("Video gateway is not enabled")
                docs_link = page.locator('#content a[href*="VIDEO_GATEWAY"], #content a[href*="video"]').count()
                assert docs_link >= 1, "Video disabled state must link to the docs section"
                banner = page.evaluate("() => { const n = document.querySelector('#notice'); return (n && !n.hidden) ? n.textContent : ''; }")
                assert "Video API" not in banner, f"global Video API banner on video page: {banner!r}"
                # Leaving Video Studio must not leave a video banner behind.
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                time.sleep(0.6)
                banner = page.evaluate("() => { const n = document.querySelector('#notice'); return (n && !n.hidden) ? n.textContent : ''; }")
                assert "Video API" not in banner, f"stale Video API banner after leaving video: {banner!r}"
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors after video checks: {errors}"
                page.unroute("**/v1/video/providers", disabled_video_providers)
                page.unroute("**/v1/video/jobs**", disabled_video_jobs)
                page.unroute("**/v1/video/jobs/**", disabled_video_jobs)
                expected_disabled_404["active"] = False

                # ---- #417 item 8: WebChat browser E2E. One
                # mode-switched dispatcher stays routed for the
                # whole section (the #539 lesson: no unmocked
                # window between sub-scenarios, so no stray
                # console errors leak into other sections).
                chat_mode = {"mode": "failover"}
                def mock_chat(route):
                    if chat_mode["mode"] == "failover":
                        sse = (
                            'data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"failover "},"finish_reason":null}]}\n\n'
                            'data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}\n\n'
                            'data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}\n\n'
                            'data: [DONE]\n\n'
                        )
                        route.fulfill(status=200, content_type="text/event-stream", headers={
                            "x-gateway-deployment": "fakeprov-b/m",
                            "x-gateway-provider": "fakeprov-b",
                            "x-gateway-upstream-model": "fake-model",
                            "x-gateway-public-model": "coding",
                            "x-nexaroute-attempts": "fakeprov-a/m>fakeprov-b/m",
                            "x-nexaroute-cache": "miss",
                            "x-request-id": "e2e-chat-failover",
                        }, body=sse)
                    else:
                        sse = (
                            'data: {"id":"chatcmpl-e2e2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"second answer"},"finish_reason":null}]}\n\n'
                            'data: {"id":"chatcmpl-e2e2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":2,"total_tokens":32}}\n\n'
                            'data: [DONE]\n\n'
                        )
                        route.fulfill(status=200, content_type="text/event-stream", headers={
                            "x-gateway-deployment": "fakeprov-b/m",
                            "x-gateway-provider": "fakeprov-b",
                            "x-gateway-upstream-model": "fake-model",
                            "x-gateway-public-model": "coding",
                            "x-nexaroute-attempts": "fakeprov-b/m",
                            "x-nexaroute-cache": "hit",
                            "x-request-id": "e2e-chat-second",
                        }, body=sse)
                page.route("**/v1/chat/completions", mock_chat)
                page.locator('[data-page="playground"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator("#pg-model")).to_contain_text("coding")
                # Privacy line is stated on the surface (acceptance:
                # stated behaviour matches tab-only storage).
                expect(page.locator("#content")).to_contain_text("kept only in this tab")
                page.locator("#pg-prompt").fill("hello gateway")
                page.locator("#pg-send").click()
                assistant = page.locator(".pg-msg.assistant")
                expect(assistant.first).to_contain_text("failover answer")
                badge = assistant.first.locator(".pg-badge")
                # The badge resolves to the deployment that actually
                # answered, with the failover trail IN ORDER —
                # fakeprov-a tried first, fakeprov-b answered.
                expect(badge).to_contain_text("fakeprov-b / fake-model · fakeprov-b/m")
                expect(badge).to_contain_text("attempts fakeprov-a/m → fakeprov-b/m")
                expect(badge).to_contain_text("done")
                # The session strip shows the same pin.
                expect(page.locator("#pg-session")).to_contain_text("fakeprov-b/m")
                # A second turn keeps the history: both exchanges
                # stay in the transcript, and a single-attempt
                # reply carries no attempts trail.
                chat_mode["mode"] = "plain"
                page.locator("#pg-prompt").fill("and again")
                page.locator("#pg-send").click()
                expect(assistant.nth(1)).to_contain_text("second answer")
                expect(assistant.nth(1).locator(".pg-badge")).to_contain_text("cache hit")
                assert "attempts" not in assistant.nth(1).locator(".pg-badge").inner_text(), "single-attempt reply must not render an attempts trail"
                expect(page.locator("#pg-session")).to_contain_text("12 → 30")
                # Reload: the conversation is tab-memory only, so
                # the transcript honestly resets — no server-side
                # history is invented.
                page.reload(wait_until="domcontentloaded")
                page.locator('[data-page="playground"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator("#pg-transcript")).to_contain_text("No request yet.")
                page.unroute("**/v1/chat/completions", mock_chat)
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors after WebChat E2E: {errors}"

                # Evidence: Providers with video disabled (dark, both sizes).
                # Under the acceptance runner this stays inside the run's
                # diagnostics directory instead of dirtying the checkout.
                video_dir = (evidence_root / "video-disabled") if evidence_root else ROOT / "docs/browser-evidence/video-disabled"
                video_dir.mkdir(parents=True, exist_ok=True)
                page.set_viewport_size({"width": 1440, "height": 900})
                page.evaluate("() => { document.documentElement.dataset.theme = 'dark'; localStorage.setItem('nexaroute_theme', 'dark'); }")
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                time.sleep(0.5)
                page.screenshot(path=str(video_dir / "providers-1440x900-dark.png"))
                page.set_viewport_size({"width": 390, "height": 844})
                time.sleep(0.4)
                page.screenshot(path=str(video_dir / "providers-390x844-dark.png"))

                # Full-page provider captures keep the unknown badge visible with the other two.
                page.set_viewport_size({"width": 1440, "height": 900})
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                for theme in ("dark", "light"):
                    page.evaluate("(t) => { document.documentElement.dataset.theme = t; localStorage.setItem('nexaroute_theme', t); window.scrollTo(0, 0); }", theme)
                    time.sleep(0.3)
                    page.screenshot(path=str(fix_dir / f"providers-privacy-badges-fullpage-{theme}.png"), full_page=True)

                # Issue #393: local-only provider fixture with sensitive fields
                # and non-secret metadata; no external provider is contacted.
                browser_secrets = [
                    "ISSUE393_BROWSER_API_CANARY_591a",
                    "ISSUE393_BROWSER_ENV_CANARY_9f31",
                    "ISSUE393_BROWSER_POOL_CANARY_4c72",
                    "ISSUE393_BROWSER_POOL_ENV_CANARY_2da4",
                    "ISSUE393_BROWSER_HEADER_CANARY_0c41",
                    "ISSUE393_BROWSER_PROXY_USER_88bc",
                    "ISSUE393_BROWSER_PROXY_PASS_31ed",
                    "ISSUE393_BROWSER_QUERY_CANARY_07fa",
                ]
                admin_json(base, "/admin/api/providers", "POST", {"provider": {
                    "id": "issue393-roundtrip", "name": "Provider Round Trip",
                    "type": "openai_compatible", "base_url": "http://127.0.0.1:9/v1",
                    "api_key": browser_secrets[0], "api_key_env": "NEXA_ISSUE393_BROWSER_KEY",
                    "auth_mode": "bearer", "enabled": False, "max_concurrency": 13,
                    "stream_idle_timeout_seconds": 88, "chat_path": "/custom-chat",
                    "forward_headers": ["x-client-id"], "allowed_hosts": ["127.0.0.1"],
                    "headers": {"X-Private-Auth": browser_secrets[4], "X-Tenant": "tenant-393"},
                    "proxy_url": "http://" + browser_secrets[5] + ":" + browser_secrets[6] + "@127.0.0.1:9997/proxy?" + browser_secrets[7] + "=1",
                    "credentials": [
                        {"name": "literal-pool", "api_key": browser_secrets[2], "enabled": True},
                        {"name": "env-pool", "api_key_env": "NEXA_ISSUE393_BROWSER_POOL", "enabled": True},
                    ],
                    "data_handling": {"trains_on_data": "no", "retention": "limited", "note": "safe browser fixture"},
                    "models": [{"id": "browser-alias", "model": "model-alpha", "aliases": ["friendly-alpha"],
                                "enabled": True, "priority": 4, "weight": 2.5, "context_window": 4096,
                                "input_cost_per_mtok": 3, "output_cost_per_mtok": 6,
                                "capabilities": {"streaming": True, "tools": True, "vision": False, "reasoning": True}}],
                }})
                page.reload(wait_until="domcontentloaded")
                page.locator('[data-page="providers"]').click()
                browser_requests, browser_responses = [], []
                def record_request(request):
                    if "/admin/api/providers/issue393-roundtrip" in request.url:
                        browser_requests.append(request.post_data or "")
                def record_response(response):
                    if "/admin/api/providers/issue393-roundtrip" in response.url:
                        try:
                            browser_responses.append(response.text())
                        except Exception:
                            pass
                page.on("request", record_request)
                page.on("response", record_response)

                edit = page.locator('[data-action="edit-provider"][data-id="issue393-roundtrip"]')
                edit.click()
                expect(page.locator("#p-name")).to_have_value("Provider Round Trip")
                expect(page.locator("#p-enabled")).not_to_be_checked()
                expect(page.locator("#p-max-concurrency")).to_have_value("13")
                expect(page.locator("#model-list")).to_contain_text("ID browser-alias")
                expect(page.locator("#model-list")).to_contain_text("Aliases friendly-alpha")
                expect(page.locator("#model-list")).to_contain_text("Priority 4")
                expect(page.locator("#model-list")).to_contain_text("Weight 2.5")
                expect(page.locator("#model-list")).to_contain_text("Capabilities streaming, tools, reasoning")
                expect(page.locator("#dialog-root")).to_contain_text("X-Private-Auth")
                expect(page.locator("#dialog-root")).to_contain_text("http://127.0.0.1:9997")
                expect(page.locator("#dialog-root")).to_contain_text("literal-pool")
                expect(page.locator("#dialog-root")).to_contain_text("env-pool")
                for secret in browser_secrets:
                    # The message deliberately does NOT interpolate the
                    # value: a failing assertion used to print the canary
                    # itself into the CI log. See the runner's redaction.
                    assert secret not in page.locator("#dialog-root").inner_text(), "secret shown in edit UI (value redacted)"

                # Cancel must discard the edit draft; reopening rehydrates from
                # the backend without loading any stored credential value.
                page.locator("#p-name").fill("Cancelled draft")
                page.locator('[data-action="close-modal"]').last.click()
                edit.click()
                expect(page.locator("#p-name")).to_have_value("Provider Round Trip")

                # Validation failure must keep the modal open and not show a
                # success state; retry with a valid URL then saves an unrelated field.
                page.locator("#p-base").fill("not-a-url")
                expected_issue393_validation["active"] = True
                with page.expect_response(lambda response: "/admin/api/providers/issue393-roundtrip" in response.url and response.status == 400):
                    page.locator('[data-action="save-modal"]').click()
                expected_issue393_validation["active"] = False
                expect(page.locator("#modal-title")).to_have_text("Edit provider")
                page.locator("#p-base").fill("http://127.0.0.1:9/v1")
                page.locator("#p-name").fill("Provider Round Trip Edited")
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#dialog-root .modal")).to_have_count(0)

                saved = admin_json(base, "/admin/api/providers/issue393-roundtrip", "GET")
                safe_provider = saved["provider"]
                assert safe_provider["name"] == "Provider Round Trip Edited"
                assert safe_provider["enabled"] is False and safe_provider["max_concurrency"] == 13
                assert safe_provider["stream_idle_timeout_seconds"] == 88 and safe_provider["chat_path"] == "/custom-chat"
                assert safe_provider["forward_headers"] == ["x-client-id"] and safe_provider["allowed_hosts"] == ["127.0.0.1"]
                assert safe_provider["models"][0]["id"] == "browser-alias"
                assert safe_provider["models"][0]["aliases"] == ["friendly-alpha"]
                assert safe_provider["models"][0]["priority"] == 4 and safe_provider["models"][0]["weight"] == 2.5
                read_text = json.dumps(saved)
                for secret in browser_secrets:
                    assert secret not in read_text, "read API leaked a secret value (value redacted)"

                # Explicit replacement is distinct from omission and returns
                # only safe status metadata. The input is a test-only canary.
                replacement = "ISSUE393_BROWSER_REPLACEMENT_CANARY_b18f"
                page.locator('[data-action="edit-provider"][data-id="issue393-roundtrip"]').click()
                page.locator("#p-key").fill(replacement)
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#dialog-root .modal")).to_have_count(0)
                replaced = admin_json(base, "/admin/api/providers/issue393-roundtrip", "GET")
                assert replaced["credential_configured"] is True and replaced["secret_source"] == "literal"
                assert replacement not in json.dumps(replaced)

                # Explicit clear is separately selectable; reopening and
                # clearing must update status without returning the old value.
                page.locator('[data-action="edit-provider"][data-id="issue393-roundtrip"]').click()
                page.locator("#p-clear-secret").check()
                page.locator('[data-action="save-modal"]').click()
                expect(page.locator("#dialog-root .modal")).to_have_count(0)
                cleared = admin_json(base, "/admin/api/providers/issue393-roundtrip", "GET")
                assert cleared["credential_configured"] is False and cleared["has_secret"] is False
                all_responses = "\n".join(browser_responses)
                for secret in browser_secrets + [replacement]:
                    assert secret not in all_responses, "browser response leaked a secret value (value redacted)"
                preserve_requests = "\n".join(browser_requests)
                for secret in browser_secrets:
                    assert secret not in preserve_requests, "browser request unexpectedly contained a stored secret value (value redacted)"
                assert sum(replacement in request for request in browser_requests) == 1, "replacement should be sent only in the explicit replace request"

                # Phase 7 remainder (#399): the unified model inventory renders
                # provider-qualified rows and filters through the bounded window.
                page.locator('[data-page="models"]').click()
                expect(page.locator("#content")).to_contain_text("issue393-roundtrip/browser-alias")
                expect(page.locator("#content")).to_contain_text("friendly-alpha")
                page.locator("#model-inventory-filter").fill("no-such-model-xyz")
                expect(page.locator("#content")).to_contain_text("No models match your search")
                page.locator("#model-inventory-filter").fill("browser-alias")
                expect(page.locator("#content")).to_contain_text("issue393-roundtrip/browser-alias")
                page.locator("#model-inventory-filter").fill("")

                # Phase 7 remainder (#399): the guided first-route wizard shows a
                # cost warning derived from declared pricing before saving.
                page.locator('[data-page="routing"]').click()
                page.locator('[data-action="first-route-wizard"]').first.click()
                expect(page.locator("#modal-title")).to_have_text("Guided first route")
                expect(page.locator("#wz-cost .status")).to_be_visible()
                expect(page.locator("#wz-model-list input[type=checkbox]").first).to_be_checked()
                page.locator('[data-action="close-modal"]').first.click()
                expect(page.locator("#dialog-root .modal")).to_have_count(0)

                admin_json(base, "/admin/api/providers/issue393-roundtrip", "DELETE")

                # --- Browser acceptance reliability ---
                # Flows the suite previously reached only indirectly: the
                # real theme-toggle and language-switch controls (earlier
                # sections set dataset/localStorage by hand), settings-tab
                # navigation, the configured admin mode, failed-request /
                # page-error capture, and snapshot failure + recovery.
                # 1. Real theme toggle: reload first so state, dataset and
                # localStorage agree (init copies localStorage into state),
                # then two real clicks must flip and restore the theme.
                expected_throttled_429["active"] = True
                try:
                    reload_until_connected(page)
                finally:
                    expected_throttled_429["active"] = False
                theme_before = page.evaluate("() => document.documentElement.dataset.theme")
                assert theme_before in ("dark", "light")
                assert page.evaluate("() => localStorage.getItem('nexaroute_theme')") == theme_before
                expect(page.locator("#theme-toggle")).to_have_text("Light mode" if theme_before == "dark" else "Dark mode")
                page.locator("#theme-toggle").click()
                theme_after = page.evaluate("() => document.documentElement.dataset.theme")
                assert theme_after != theme_before, f"theme toggle did not flip: {theme_before} -> {theme_after}"
                assert page.evaluate("() => localStorage.getItem('nexaroute_theme')") == theme_after
                expect(page.locator("#theme-toggle")).to_have_text("Light mode" if theme_after == "dark" else "Dark mode")
                page.locator("#theme-toggle").click()
                assert page.evaluate("() => document.documentElement.dataset.theme") == theme_before
                # 2. Failed-request / page-error capture for the sections
                # below. Collecting starts after the reload above so a
                # navigation-aborted stream cannot pollute the record, and
                # stops before the failure/recovery reloads below.
                failed_requests: list[str] = []
                page_errors: list[str] = []
                collecting = {"active": True}
                def record_failed_request(request):
                    if collecting["active"]:
                        failed_requests.append(f"{request.method} {request.url}")
                def record_page_error(error):
                    if collecting["active"]:
                        page_errors.append(str(error))
                page.on("requestfailed", record_failed_request)
                page.on("pageerror", record_page_error)
                # 3. Real language switch: fa must render RTL, en must
                # restore LTR, and the theme must survive the switch.
                page.locator("#lang-switch").select_option("fa")
                assert page.evaluate("() => document.documentElement.dir") == "rtl"
                assert page.evaluate("() => document.documentElement.lang") == "fa"
                assert page.locator("#lang-switch").input_value() == "fa"
                expect(page.locator("#brand-sub")).to_have_text("صفحه کنترل")
                page.locator("#lang-switch").select_option("en")
                assert page.evaluate("() => document.documentElement.dir") == "ltr"
                assert page.evaluate("() => document.documentElement.lang") == "en"
                expect(page.locator("#brand-sub")).to_have_text("Control plane")
                assert page.evaluate("() => document.documentElement.dataset.theme") == theme_before
                # 4. Settings tabs: every tab must become the selected,
                # visible panel; the save control must be present. Tab
                # clicks are read-only, nothing is saved here.
                page.locator('[data-page="settings"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator('[data-action="save-settings"]')).to_be_visible()
                expect(page.locator("#setting-client-base-url")).to_be_visible()
                for tab in ("gateway", "routing", "security", "data", "expert"):
                    page.locator(f"#settings-tab-{tab}").click()
                    assert page.locator(f"#settings-tab-{tab}").get_attribute("aria-selected") == "true"
                    expect(page.locator(f"#settings-panel-{tab}")).to_be_visible()
                # 5. Configured admin mode: this suite boots keyless
                # emergency loopback mode (no admin key, OIDC off). The
                # status endpoint must say so, the browser sign-in
                # control must stay hidden, and a DNS-rebinding Host
                # must be denied even though loopback itself is trusted.
                status = admin_json(base, "/admin/auth/status", "GET")
                assert status["oidc_enabled"] is False
                assert status["emergency_access_enabled"] is True
                assert status["api_key_configured"] is False
                expect(page.locator("#auth-control")).to_be_hidden()
                from urllib.parse import urlparse
                parsed = urlparse(base)
                conn = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=10)
                try:
                    conn.putrequest("GET", "/admin/api/providers", skip_host=True)
                    conn.putheader("Host", "evil.example.com")
                    conn.endheaders()
                    rebinding = conn.getresponse()
                    rebinding.read()
                    assert rebinding.status == 401, f"DNS-rebinding admin read must be denied, got {rebinding.status}"
                finally:
                    conn.close()
                assert not failed_requests, f"failed browser requests: {failed_requests}"
                assert not page_errors, f"browser page errors: {page_errors}"
                collecting["active"] = False
                # 6. Snapshot failure + recovery: a 500 snapshot must
                # surface as Unavailable/Offline, and removing the fault
                # must restore Connected/Live without a restart.
                def snapshot_failure(route):
                    route.fulfill(status=500, content_type="application/json",
                                  body=json.dumps({"error": "deterministic snapshot failure"}))
                expected_snapshot_500["active"] = True
                # The failure reload's other refresh fetches, and the
                # recovery reload's starved attempts, are throttled
                # with 429s when the bucket is empty (see
                # reload_until_connected). Those resource errors are
                # expected inside this window, not console failures.
                expected_throttled_429["active"] = True
                page.route("**/admin/api/snapshot**", snapshot_failure)
                page.reload(wait_until="domcontentloaded")
                expect(page.locator("#connection-text")).to_have_text("Unavailable")
                expect(page.locator("#live-status")).to_have_text("Offline")
                page.unroute("**/admin/api/snapshot**", snapshot_failure)
                reload_until_connected(page)
                expect(page.locator("#live-status")).to_have_text("Live")
                expected_snapshot_500["active"] = False
                expected_throttled_429["active"] = False

                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors after Issue #393 browser flow: {errors}"

                print(f"M2-1 title offsets: {title_tops}")
                print(f"M2-1 content offsets: {content_tops}")
                print(f"M2-1 Video Studio empty-state CTA gap: {gap:.1f}px")
                print(f"M2-1 privacy badge colors: dark={dark_colors}, light={light_colors}")
                print(f"M2-1 screenshots: {len(list(fix_dir.glob('*.png')))} (24 viewport + 2 full-page badge proofs)")

                browser.close()
        finally:
            # Diagnostics first: is the gateway still alive, and does
            # it still answer /healthz? Recorded before terminate so
            # a startup/health failure stays diagnosable afterwards.
            gateway_alive = proc.poll() is None
            try:
                with urllib.request.urlopen(base + "/healthz", timeout=2) as r:
                    healthz_status = r.status
            except Exception as exc:
                healthz_status = f"unreachable ({type(exc).__name__})"
            proc.terminate()
            try:
                proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            log.close()
            redact_values = [test_video_token, env.get("NEXA_ISSUE393_BROWSER_KEY"), env.get("NEXA_ISSUE393_BROWSER_POOL")]
            summary_lines = [
                f"gateway alive before terminate: {gateway_alive}",
                f"gateway /healthz: {healthz_status}",
                f"gateway exit code: {proc.returncode}",
            ]
            counts: dict = {}
            for entry in admin_api_responses:
                key = (entry["path"], entry["status"])
                counts[key] = counts.get(key, 0) + 1
            for (path, status), count in sorted(counts.items()):
                summary_lines.append(f"admin api response: {status} x{count} {path}")
            print("BROWSER DIAGNOSTICS:")
            print("\n".join(summary_lines))
            if evidence_root is not None:
                try:
                    evidence_root.mkdir(parents=True, exist_ok=True)
                    gateway_log = (tmp_path / "gateway.log").read_text(errors="replace")
                    # Bound the preserved log; the tail holds the
                    # failure window (the gateway logs every >=400
                    # status, so throttled 429s are visible there).
                    tail = gateway_log[-256 * 1024:]
                    (evidence_root / "gateway.log").write_text(redact_text(tail, redact_values))
                    (evidence_root / "admin-api-responses.json").write_text(
                        redact_text(json.dumps(admin_api_responses, indent=2) + "\n", redact_values))
                    (evidence_root / "gateway-diagnostics.json").write_text(json.dumps({
                        "gateway_alive_before_terminate": gateway_alive,
                        "healthz_status": healthz_status,
                        "gateway_exit_code": proc.returncode,
                    }, indent=2) + "\n")
                except Exception as exc:
                    print(f"BROWSER DIAGNOSTICS: failed to write evidence files: {type(exc).__name__}")
    print("BROWSER E2E PASS")

if __name__ == "__main__":
    main()

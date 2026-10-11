#!/usr/bin/env python3
"""Browser acceptance for the v0.16.1 backend-driven control plane.

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

def console_errors(page):
    errors = []
    page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
    return errors

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
                page = browser.new_page(viewport={"width": 1440, "height": 900})
                page.set_default_timeout(12000)
                
                console_msgs = []
                page.on("console", lambda msg: console_msgs.append((msg.type, msg.text)))
                
                page.route("**/admin/api/provider-discover", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "models": ["model-alpha", "model-beta"]})))
                page.route("**/admin/api/provider-check", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "status_code": 200, "latency_ms": 1})))
                page.route("**/admin/api/provider-test", lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps({"ok": True, "passed": 1, "total": 1, "results": [{"model": "model-alpha", "ok": True, "latency_ms": 1}]})))
                page.goto(base + "/", wait_until="domcontentloaded")
                
                # Check console errors
                errors = [m for m in console_msgs if m[0] == "error"]
                assert not errors, f"Console errors: {errors}"
                
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
                
                # --- Activity page ---
                page.locator('[data-page="activity"]').click()
                page.wait_for_load_state("domcontentloaded")
                expect(page.locator("#content")).to_contain_text("No real activity yet")
                
                # --- Screenshots for evidence ---
                evidence_dir = ROOT / "docs/browser-evidence/privacy"
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
                    top = page.evaluate("() => { const n = document.querySelector('#notice'); if (n) n.hidden = true; window.scrollTo(0, 0); return document.querySelector('#content').getBoundingClientRect().top; }")
                    content_tops[dest] = top
                tops = list(content_tops.values())
                assert max(tops) - min(tops) <= 2, f"inconsistent content top offset: {content_tops}"
                # Empty-state rhythm probe: description-to-button gap must be >= 12px
                gap = page.evaluate("""() => {
                  const host = document.querySelector('#content');
                  const probe = document.createElement('div');
                  probe.className = 'empty-state';
                  probe.innerHTML = '<p>Probe description</p><button class=\"button primary\">Probe action</button>';
                  host.appendChild(probe);
                  const p = probe.querySelector('p');
                  const b = probe.querySelector('button');
                  const pr = p.getBoundingClientRect();
                  const br = b.getBoundingClientRect();
                  const g = br.top - pr.bottom;
                  probe.remove();
                  return g;
                }""")
                assert gap >= 12, f"empty-state button touches description: gap={gap}px"
                # Badge styles: yes=warn, no=good, unknown=neutral (dark)
                page.locator('[data-page="providers"]').click()
                page.wait_for_load_state("domcontentloaded")
                time.sleep(0.3)
                assert page.locator(".provider-tile .status.warn").count() >= 1, "missing warn badge (trains=yes)"
                assert page.locator(".provider-tile .status.good").count() >= 1, "missing good badge (trains=no)"
                assert page.locator(".provider-tile .status.neutral").count() >= 1, "missing neutral badge (unknown)"
                warn_color = page.evaluate("() => getComputedStyle(document.querySelector('.provider-tile .status.warn')).color")
                neutral_color = page.evaluate("() => getComputedStyle(document.querySelector('.provider-tile .status.neutral')).color")
                good_color = page.evaluate("() => getComputedStyle(document.querySelector('.provider-tile .status.good')).color")
                assert warn_color != neutral_color, f"unknown badge must not reuse warning style: {warn_color}"
                assert good_color != neutral_color, f"unknown badge must differ from safe style: {good_color}"
                # Same badge classes in light theme
                page.evaluate("() => { document.documentElement.dataset.theme = 'light'; localStorage.setItem('nexaroute_theme', 'light'); }")
                time.sleep(0.3)
                assert page.locator(".provider-tile .status.warn").count() >= 1
                assert page.locator(".provider-tile .status.good").count() >= 1
                assert page.locator(".provider-tile .status.neutral").count() >= 1
                warn_l = page.evaluate("() => getComputedStyle(document.querySelector('.provider-tile .status.warn')).color")
                neutral_l = page.evaluate("() => getComputedStyle(document.querySelector('.provider-tile .status.neutral')).color")
                assert warn_l != neutral_l, "light theme: unknown badge must not reuse warning style"
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
                # UI FIX screenshots: 5 pages x 2 sizes x 2 themes under docs/browser-evidence/ui-fix/
                fix_dir = ROOT / "docs/browser-evidence/ui-fix"
                fix_dir.mkdir(parents=True, exist_ok=True)
                for dest in ["overview", "providers", "routing", "activity", "settings"]:
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
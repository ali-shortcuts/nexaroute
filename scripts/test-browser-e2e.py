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
        log = (tmp_path / "gateway.log").open("w")
        proc = subprocess.Popen(["go", "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT, text=True)
        base = f"http://127.0.0.1:{port}"
        try:
            wait_ready(base, proc)
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 900})
                page.set_default_timeout(12000)
                page.add_init_script(f"sessionStorage.setItem('nexaroute_video_token', {json.dumps(test_video_token)});")
                
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

                # --- ISSUE #246: disabled video gateway must not raise a global banner ---
                # Gateway under test has no `video` config, so /v1/video/* is unmounted (HTTP 404).
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
                # Evidence: Providers with video disabled (dark, both sizes).
                video_dir = ROOT / "docs/browser-evidence/video-disabled"
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

                print(f"M2-1 title offsets: {title_tops}")
                print(f"M2-1 content offsets: {content_tops}")
                print(f"M2-1 Video Studio empty-state CTA gap: {gap:.1f}px")
                print(f"M2-1 privacy badge colors: dark={dark_colors}, light={light_colors}")
                print(f"M2-1 screenshots: {len(list(fix_dir.glob('*.png')))} (24 viewport + 2 full-page badge proofs)")

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

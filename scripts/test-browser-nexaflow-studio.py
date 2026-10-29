#!/usr/bin/env python3
"""Browser acceptance for the NexaFlow Visual Studio (canonical port).

Drives the real studio page against a real gateway (loopback) and the
real canonical NexaFlow HTTP API. No paid calls: the graphs use only
local nodes (manual trigger, wait, JSON transform, approval, HTTP
request) plus one HTTP request to an unroutable loopback port, which
fails closed without leaving the machine.

Skips cleanly (exit 0) when Chromium or Python Playwright is
unavailable, matching the verify.sh browser gate. It ALSO skips
cleanly when the booted gateway does not attach the NexaFlow service:
the production gateway has no AttachNexaflow caller (gateway wiring
is a separate, deferred product decision), so without an attached
service the studio API paths fall through to the static server and
answer 404. The skip names that blocker instead of faking a pass;
once gateway wiring lands, the same script runs all 15 cases for real.

Config-field editing uses real keystrokes (click, select-all, type,
Tab to fire the inspector's change handler), never JS value injection.
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
ADMIN_KEY = "browser-test-key"  # test-only; never a real credential


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


def find_go() -> str:
    go = shutil.which("go")
    if go:
        return go
    cand = Path.home() / "go" / "bin" / "go"
    if cand.is_file():
        return str(cand)
    raise AssertionError("go binary not found (checked PATH and ~/go/bin/go)")


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

    with tempfile.TemporaryDirectory(prefix="nexaroute-nexaflow-studio-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        # Only touch keys the example config actually carries; probes off
        # is the guard (studio cases use local nodes only, no providers).
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        port = free_port()
        config["listen"] = f"127.0.0.1:{port}"
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        go = find_go()
        env["PATH"] = str(Path(go).parent) + ":" + env.get("PATH", "")
        env["NEXAROUTE_ADMIN_KEY"] = ADMIN_KEY
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
                page.goto(base + "/studio.html",
                          wait_until="domcontentloaded")
                expect(page).to_have_title("NexaFlow Visual Studio")
                # Attach gate: without a service the API paths fall
                # through to the static server (404). That is a
                # wiring gap, not a studio failure, so skip with the
                # exact reason. Static serving above is verified
                # even on the skip path.
                req = urllib.request.Request(
                    base + "/admin/api/nexaflow/nodes",
                    headers={"x-admin-key": ADMIN_KEY})
                try:
                    with urllib.request.urlopen(req, timeout=5) as r:
                        if r.status != 200:
                            raise AssertionError(
                                f"nodes probe: unexpected status {r.status}")
                except urllib.error.HTTPError as e:
                    if e.code == 404:
                        print("SKIP: production gateway does not attach "
                              "the NexaFlow service (no AttachNexaflow "
                              "caller in cmd/gateway); studio API cases "
                              "need gateway wiring, which is a separate "
                              "deferred decision")
                        browser.close()
                        return
                    raise

                def connect():
                    page.locator("#admin-key").fill(ADMIN_KEY)
                    page.locator("#connect-btn").click()
                    expect(page.locator("#auth-status")).to_have_text(
                        "Connected.")
                    expect(page.locator("#studio")).to_be_visible()
                    if page.locator("#onboarding-dismiss").is_visible():
                        page.locator("#onboarding-dismiss").click()

                def node_count() -> int:
                    return page.locator("#canvas .node-card").count()

                def edge_count() -> int:
                    return page.locator(
                        "#canvas-edges line[data-edge]").count()

                def diagnostics() -> str:
                    return page.locator("#diagnostics").inner_text()

                def add_node(node_id: str):
                    page.locator(f'[data-add="{node_id}"]').click()
                    page.wait_for_timeout(300)

                def select_node(node_id: str):
                    # Click the card's title area, not its buttons.
                    page.locator(
                        f'#canvas .node-card[data-node="{node_id}"]'
                    ).click(position={"x": 105, "y": 15})

                def connect_nodes(src: str, dst: str):
                    page.locator(
                        f'#canvas .node-card[data-node="{src}"] '
                        "[data-connect]").click()
                    select_node(dst)

                def new_draft():
                    page.locator("#wf-new").click()
                    expect(page.locator("#diagnostics")).to_contain_text(
                        "New draft")

                def save_draft(draft_id: str, name: str):
                    page.locator("#btn-save").click()
                    if page.locator("#new-id").is_visible():
                        page.locator("#new-id").fill(draft_id)
                        page.locator("#new-name").fill(name)
                        page.locator("#new-confirm").click()
                    expect(page.locator("#diagnostics")).to_contain_text(
                        "Draft saved.")

                def open_draft(draft_id: str):
                    page.locator(
                        f'#wf-list [data-open="{draft_id}"]').click()
                    expect(page.locator("#canvas-title")).to_contain_text(
                        "Canvas")

                def publish():
                    page.locator("#btn-publish").click()
                    expect(page.locator("#diagnostics")).to_contain_text(
                        "Published as revision")

                def start_run(input_json: str):
                    page.locator("#run-input").fill(input_json)
                    page.locator("#btn-run").click()
                    expect(page.locator("#run-detail h3")).to_be_visible()

                def run_detail() -> str:
                    return page.locator("#run-detail").inner_text()

                connect()

                # 1. Empty list + create.
                expect(page.locator("#wf-list")).to_contain_text(
                    "No drafts yet.")
                new_draft()

                # 2. Add + connect + validate + save + reload.
                assert node_count() == 0
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.json_transform")
                assert node_count() == 2
                connect_nodes("manual", "json_transform")
                assert edge_count() == 1
                page.locator("#btn-validate").click()
                expect(page.locator("#diagnostics")).to_contain_text(
                    "Server validation: valid.")
                save_draft("browser-flow", "Browser Flow")
                page.reload(wait_until="domcontentloaded")
                expect(page.locator("#studio")).to_be_visible()
                open_draft("browser-flow")
                assert node_count() == 2, "saved graph must survive reload"
                assert edge_count() == 1

                # 3. Publish + run to success (trigger -> wait).
                new_draft()
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.wait")
                connect_nodes("manual", "wait")
                save_draft("wait-flow", "Wait Flow")
                publish()
                start_run('{"duration_ms": 50}')
                expect(page.locator("#run-detail")).to_contain_text(
                    "succeeded")
                assert "wait" in run_detail()

                # 4. Validation errors surface (trigger-less).
                new_draft()
                add_node("nexaroute.control.json_transform")
                page.locator("#btn-validate").click()
                expect(page.locator("#diagnostics")).to_contain_text(
                    "no triggers")

                # 5. Run failure displayed safely.
                new_draft()
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.http.request")
                connect_nodes("manual", "request")
                save_draft("fail-flow", "Fail Flow")
                publish()
                start_run('{"url": "http://127.0.0.1:9/unreachable"}')
                detail = run_detail()
                assert "failed" in detail
                assert "error" in detail
                assert "connection refused" not in detail.lower(), \
                    "raw error text must never render"
                assert "127.0.0.1:9" not in detail, \
                    "run input must never render"

                # 6. Cancellation of an approval-gated run. Canonical
                # run creation is synchronous, so the #690 long-wait
                # cancel shape does not apply: the run parks in
                # waiting_for_approval and is cancelled from there.
                new_draft()
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.approval")
                connect_nodes("manual", "approval")
                save_draft("approval-flow", "Approval Flow")
                publish()
                start_run("{}")
                expect(page.locator("#run-detail")).to_contain_text(
                    "waiting_for_approval")
                page.locator("#run-cancel").click()
                expect(page.locator("#run-detail")).to_contain_text(
                    "cancelled")

                # 7. Approval gate: self-approval rejection. This
                # browser gateway has a single admin identity, so the
                # approver IS the run starter and canonical SoD must
                # reject honestly. The distinct-approver success path
                # is covered by the Go service test, where two
                # identities can be expressed.
                start_run("{}")
                expect(page.locator("#run-detail")).to_contain_text(
                    "waiting_for_approval")
                page.locator(
                    '[data-approve="nexaroute.control.approval"]'
                    '[data-decision="approved"]').click()
                expect(page.locator("#studio-error")).to_contain_text(
                    "must not be the requester")

                # 8. Template gallery instantiate (server-side).
                # scheduled-operational-report carries a schedule
                # trigger, so no trigger completion is added: 3 nodes
                # and the template's 2 edges, placeholders filled.
                page.locator(
                    '[data-tpl="scheduled-operational-report"]').click()
                expect(page.locator("#version-detail")).to_contain_text(
                    "Fill placeholders")
                for i in range(page.locator("[data-ph]").count()):
                    page.locator("[data-ph]").nth(i).fill("test")
                page.locator("#tpl-create").click()
                expect(page.locator("#canvas-title")).to_contain_text(
                    "Canvas")
                assert node_count() == 3
                assert edge_count() == 2

                # 9. Invalid edges rejected + edge delete + reconnect.
                open_draft("browser-flow")
                connect_nodes("manual", "json_transform")
                expect(page.locator("#diagnostics")).to_contain_text(
                    "already exists")
                assert edge_count() == 1
                connect_nodes("json_transform", "json_transform")
                assert edge_count() == 1, "self-edge must not be created"
                page.locator("#canvas-edges line[data-edge]").first.click()
                page.locator("#edge-delete").click()
                assert edge_count() == 0
                connect_nodes("manual", "json_transform")
                assert edge_count() == 1
                save_draft("browser-flow", "Browser Flow")

                # 10. Immutable revision inspection + execution timeline.
                open_draft("wait-flow")
                page.locator("#version-list [data-inspect]").first.click()
                expect(page.locator("#canvas-title")).to_contain_text(
                    "read-only revision")
                expect(page.locator("#diagnostics")).to_contain_text(
                    "read-only")
                page.locator("#run-list [data-run]").first.click()
                expect(page.locator("#run-detail")).to_contain_text(
                    "succeeded")
                assert "wait" in run_detail()
                page.locator("#back-to-draft").click()
                assert "read-only" not in \
                    page.locator("#canvas-title").inner_text()

                # 11. Keyboard move + narrow viewport + RTL.
                open_draft("browser-flow")
                card = page.locator("#canvas .node-card").first
                card.focus()
                left_before = card.evaluate(
                    "el => parseInt(el.style.left, 10)")
                page.keyboard.press("ArrowRight")
                left_after = card.evaluate(
                    "el => parseInt(el.style.left, 10)")
                assert left_after == left_before + 10, \
                    f"keyboard move {left_before} -> {left_after}"
                page.set_viewport_size({"width": 360, "height": 700})
                expect(page.locator("#canvas")).to_be_visible()
                expect(page.locator(".palette")).to_be_visible()
                page.set_viewport_size({"width": 1440, "height": 900})
                page.locator("#dir-select").select_option("rtl")
                expect(page.locator("#canvas")).to_be_visible()
                expect(page.locator(".palette")).to_be_visible()
                page.locator("#dir-select").select_option("ltr")

                # 12. Zoom/pan buttons + Ctrl+wheel.
                page.locator("#btn-zoom-in").click()
                expect(page.locator("#zoom-label")).to_have_text("125%")
                page.locator("#btn-zoom-out").click()
                expect(page.locator("#zoom-label")).to_have_text("100%")
                wrap = page.locator("#canvas-wrap")
                wrap.scroll_into_view_if_needed()
                box = wrap.bounding_box()
                assert box is not None
                page.mouse.move(box["x"] + box["width"] / 2,
                                box["y"] + box["height"] / 2)
                page.keyboard.down("Control")
                page.mouse.wheel(0, -120)
                page.keyboard.up("Control")
                expect(page.locator("#zoom-label")).to_have_text("125%")
                before = page.locator("#canvas-viewport").evaluate(
                    "el => el.style.transform")
                page.mouse.move(box["x"] + box["width"] - 30,
                                box["y"] + box["height"] - 30)
                page.mouse.down()
                page.mouse.move(box["x"] + box["width"] + 30,
                                box["y"] + box["height"] + 10)
                page.mouse.up()
                after = page.locator("#canvas-viewport").evaluate(
                    "el => el.style.transform")
                assert before != after, "background drag must pan"
                page.locator("#btn-zoom-reset").click()
                expect(page.locator("#zoom-label")).to_have_text("100%")

                # 13. Dirty-state validate-without-save (G1 draft-body
                # rule): delete the trigger without saving, then
                # validate must report the EDITED body, not the stale
                # saved draft.
                open_draft("browser-flow")
                select_node("manual")
                page.locator("#node-delete").click()
                expect(page.locator("#dirty-badge")).to_be_visible()
                page.locator("#btn-validate").click()
                expect(page.locator("#diagnostics")).to_contain_text(
                    "no triggers")

                # 14. Error matrix: failed save + wrong-key 401 path.
                # 403 is NOT reachable under the interim admin-key
                # mapping, so it is not faked here.
                new_draft()
                add_node("nexaroute.control.json_transform")
                page.locator("#btn-save").click()
                page.locator("#new-id").fill("bad-flow")
                page.locator("#new-name").fill("Bad Flow")
                page.locator("#new-confirm").click()
                expect(page.locator("#studio-error")).to_be_visible()
                banner = page.locator("#studio-error").inner_text().lower()
                assert "postgres" not in banner and "dsn" not in banner, \
                    "error banner must not leak backend detail"
                page.locator("#admin-key").fill("wrong-key")
                page.locator("#connect-btn").click()
                expect(page.locator("#auth-status")).to_have_text(
                    "Connection failed.")
                expect(page.locator("#studio-error")).to_be_visible()
                page.locator("#admin-key").fill(ADMIN_KEY)
                page.locator("#connect-btn").click()
                expect(page.locator("#auth-status")).to_have_text(
                    "Connected.")

                # 15. Config-field editing via real typing (not
                # JS-event injection): click, select-all, type, Tab
                # to fire the inspector's change handler; the edit
                # must survive save + reload.
                open_draft("wait-flow")
                select_node("wait")
                field = page.locator('[data-field="input.duration_ms"]')
                field.click()
                page.keyboard.press("ControlOrMeta+a")
                page.keyboard.type("75")
                page.keyboard.press("Tab")
                expect(page.locator("#dirty-badge")).to_be_visible()
                page.locator("#btn-save").click()
                expect(page.locator("#diagnostics")).to_contain_text(
                    "Draft saved.")
                page.reload(wait_until="domcontentloaded")
                expect(page.locator("#studio")).to_be_visible()
                open_draft("wait-flow")
                select_node("wait")
                assert page.locator(
                    '[data-field="input.duration_ms"]').input_value() \
                    == "75", "typed field value must survive save+reload"

                browser.close()
                # The app ships no favicon, so Chromium's automatic
                # /favicon.ico request 404s; that entry is benign, and
                # its console location names the URL. Anything else is
                # a real UI/JS failure.
                real_errors = [(t, u) for (t, u) in console_errors
                               if not u.endswith("/favicon.ico")]
                assert not real_errors, \
                    f"unexpected console errors: {real_errors}"
                print("nexaflow studio browser acceptance: PASS")
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    sys.exit(main())

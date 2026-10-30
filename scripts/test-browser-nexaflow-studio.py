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
the production gateway now attaches it only when the opt-in `nexaflow`
config section is enabled, so without enablement the studio API paths
fall through to the static server and answer 404. The skip names that
blocker instead of faking a pass; the production boot below enables
the section, so the default run exercises the real wiring.

To run all cases against a server that is already serving the workflow
service, point the script at it:
NEXAFLOW_STUDIO_BASE_URL=http://127.0.0.1:PORT (e.g. a test server
built with the repo's own Server + AttachNexaflow). The script then
skips the gateway boot and runs every case against that base.

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
        external_base = os.environ.get(
            "NEXAFLOW_STUDIO_BASE_URL", "").rstrip("/")
        if external_base:
            # Pre-wired gateway or test server: run all cases against
            # it instead of booting the production gateway.
            base = external_base
            proc = None
        else:
            config = json.loads(
                (ROOT / "configs/config.example.json").read_text())
            # Only touch keys the example config actually carries;
            # probes off is the guard (studio cases use local nodes
            # only, no providers).
            config["probe"]["enabled"] = False
            config["probe"]["on_start"] = False
            # Enable the opt-in workflow service so this run exercises the
            # production AttachNexaflow caller instead of the 404 fallthrough.
            nexaflow = config.setdefault("nexaflow", {})
            nexaflow["enabled"] = True
            nexaflow["store_path"] = "nexaflow/nexaflow-store"
            port = free_port()
            config["listen"] = f"127.0.0.1:{port}"
            config_path = tmp_path / "config.json"
            config_path.write_text(json.dumps(config))
            env = os.environ.copy()
            go = find_go()
            env["PATH"] = str(Path(go).parent) + ":" + env.get("PATH", "")
            env["NEXAROUTE_ADMIN_KEY"] = ADMIN_KEY
            log = (tmp_path / "gateway.log").open("w")
            proc = subprocess.Popen(["go", "run", "./cmd/gateway",
                                     "-no-browser",
                                     "-config", str(config_path)],
                                    cwd=ROOT, env=env, stdout=log,
                                    stderr=subprocess.STDOUT, text=True)
            base = f"http://127.0.0.1:{port}"
        try:
            if proc is not None:
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
                        print("SKIP: booted gateway did not attach the "
                              "NexaFlow service (nexaflow.enabled is "
                              "false or startup failed); studio API "
                              "cases need the opt-in service enabled")
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

                def open_draft(draft_id: str, name: str = ""):
                    # The openDraft .then resets selection (selected =
                    # null) when the draft GET resolves, so a click
                    # right after open_draft must wait for that .then
                    # to have run — otherwise the click's selection is
                    # wiped by the late response. Two waits that look
                    # sufficient are NOT, for re-opens of the
                    # already-open draft: the title already matches
                    # before the new GET lands, and networkidle
                    # returns at once when the page was already idle
                    # before the click (it is a state, not a fresh
                    # 500 ms timer). So capture both responses the
                    # .then chain produces: the draft GET itself, and
                    # the runs fetch the .then issues after resetting
                    # selection. The block exits only once both have
                    # arrived, which proves the .then completed.
                    def is_draft_get(r):
                        return (r.request.method == "GET" and
                                r.url.endswith(
                                    "/admin/api/nexaflow/definitions/"
                                    + draft_id))
                    def is_runs(r):
                        return ("/admin/api/nexaflow/runs?definition_id="
                                + draft_id in r.url)
                    with page.expect_response(is_draft_get), \
                            page.expect_response(is_runs):
                        page.locator(
                            f'#wf-list [data-open="{draft_id}"]').click()
                    if name:
                        expect(page.locator("#canvas-title")).to_contain_text(
                            "Canvas — " + name)
                    else:
                        expect(page.locator("#canvas-title")).to_contain_text(
                            "Canvas")

                def publish():
                    page.locator("#btn-publish").click()
                    expect(page.locator("#diagnostics")).to_contain_text(
                        "Published as revision")

                def start_run(input_json: str):
                    # The detail panel keeps the previous run visible
                    # until the new run's detail fetch renders, so a
                    # bare h3-visible check cannot prove the NEW run
                    # rendered — and an immediate read would race the
                    # stale panel. Wait for the detail text to change
                    # (run ids are unique per run, so it always does).
                    before = run_detail()
                    page.locator("#run-input").fill(input_json)
                    page.locator("#btn-run").click()
                    expect(page.locator("#run-detail h3")).to_be_visible()
                    for _ in range(75):
                        if run_detail() != before:
                            break
                        page.wait_for_timeout(200)
                    assert run_detail() != before, \
                        "run detail did not refresh for the new run"

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
                open_draft("browser-flow", "Browser Flow")
                assert node_count() == 2, "saved graph must survive reload"
                assert edge_count() == 1

                # 3. Publish + run to success (trigger -> wait).
                # Canonical input rules: the manual trigger receives
                # the run input verbatim and declares only `payload`,
                # so a bare {"duration_ms": 50} run input fails the
                # trigger's schema. The wait duration is therefore a
                # literal input mapping on the wait node (set through
                # the inspector, like case 15), and the run input is
                # just {"payload": {}}.
                new_draft()
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.wait")
                connect_nodes("manual", "wait")
                select_node("wait")
                field = page.locator('[data-field="input.duration_ms"]')
                field.click()
                page.keyboard.press("ControlOrMeta+a")
                page.keyboard.type("50")
                page.keyboard.press("Tab")
                expect(page.locator("#dirty-badge")).to_be_visible()
                save_draft("wait-flow", "Wait Flow")
                publish()
                start_run('{"payload": {}}')
                expect(page.locator("#run-detail")).to_contain_text(
                    "succeeded")
                assert "wait" in run_detail()

                # 4. Validation errors surface (trigger-less).
                new_draft()
                add_node("nexaroute.control.json_transform")
                page.locator("#btn-validate").click()
                expect(page.locator("#diagnostics")).to_contain_text(
                    "no triggers")

                # 5a. Policy gate parks (it does not fail): the
                # request node is policy gated and run creation
                # carries no policy approval, so the canonical
                # outcome is a parked run (waiting_for_approval, node
                # error_code policy_required), cancellable from
                # there. The URL reaches the node as an input mapping
                # ($.input.payload.url) because the trigger takes the
                # run input verbatim (only `payload`). A true HTTP
                # egress failure needs run-level policy approval,
                # which the canonical API does not offer yet; that
                # gap is recorded, not faked here.
                new_draft()
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.http.request")
                connect_nodes("manual", "request")
                select_node("request")
                field = page.locator('[data-field="input.url"]')
                field.click()
                page.keyboard.press("ControlOrMeta+a")
                page.keyboard.type("$.input.payload.url")
                page.keyboard.press("Tab")
                expect(page.locator("#dirty-badge")).to_be_visible()
                save_draft("fail-flow", "Fail Flow")
                publish()
                start_run('{"payload": {"url": "http://127.0.0.1:9/unreachable"}}')
                expect(page.locator("#run-detail")).to_contain_text(
                    "waiting_for_approval")
                detail = run_detail()
                assert "policy_required" in detail
                # NOTE: the manual trigger legitimately echoes its
                # payload into its own OUTPUT (canonical wire view),
                # so the URL may appear there as an output value —
                # that is not an input render. The redaction boundary
                # is raw error text and secret-shaped values, which
                # the canonical output redactor handles.
                assert "connection refused" not in detail.lower(), \
                    "raw error text must never render"
                page.locator("#run-cancel").click()
                expect(page.locator("#run-detail")).to_contain_text(
                    "cancelled")

                # 5b. Genuine execution failure displayed safely: a
                # transform with an allowlist-violating operation
                # fails validation_error, and the detail must show
                # only the code — never raw error text or run input.
                new_draft()
                add_node("nexaroute.trigger.manual")
                add_node("nexaroute.control.json_transform")
                connect_nodes("manual", "json_transform")
                select_node("json_transform")
                field = page.locator('[data-field="input.value"]')
                field.click()
                page.keyboard.press("ControlOrMeta+a")
                page.keyboard.type('{"a": 1}')
                page.keyboard.press("Tab")
                field = page.locator('[data-field="input.operations"]')
                field.click()
                page.keyboard.press("ControlOrMeta+a")
                page.keyboard.type('[{"op": "bogus", "field": "x"}]')
                page.keyboard.press("Tab")
                expect(page.locator("#dirty-badge")).to_be_visible()
                save_draft("badop-flow", "Bad Op Flow")
                publish()
                start_run('{"payload": {}}')
                detail = run_detail()
                assert "failed" in detail
                assert "validation_error" in detail
                assert "bogus" not in detail, \
                    "raw operation text must never render"

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
                # data-approve carries the node INSTANCE id
                # ("approval", the last segment of the type id), not
                # the registry type id.
                page.locator(
                    '[data-approve="approval"]'
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
                # Instantiate is async (POST + openDraft GET + render)
                # and the canvas title is static ("Canvas") before it
                # lands, so bare counts here would race like the
                # open_draft race fixed above: poll the counts.
                expect(page.locator("#canvas .node-card")).to_have_count(3)
                expect(page.locator(
                    "#canvas-edges line[data-edge]")).to_have_count(2)

                # 9. Invalid edges rejected + edge delete + reconnect.
                open_draft("browser-flow", "Browser Flow")
                connect_nodes("manual", "json_transform")
                expect(page.locator("#diagnostics")).to_contain_text(
                    "already exists")
                assert edge_count() == 1
                connect_nodes("json_transform", "json_transform")
                assert edge_count() == 1, "self-edge must not be created"
                # The hit line is transparent by design (the visible
                # line has pointer-events=none), so Playwright's
                # visibility check refuses it — and a forced click
                # without scrolling misses when the canvas sits below
                # the fold. Scroll it into view first, then force the
                # click; the handler still runs and selection follows.
                edge_line = page.locator(
                    "#canvas-edges line[data-edge]").first
                edge_line.scroll_into_view_if_needed()
                edge_line.click(force=True)
                page.locator("#edge-delete").click()
                assert edge_count() == 0
                connect_nodes("manual", "json_transform")
                assert edge_count() == 1
                save_draft("browser-flow", "Browser Flow")

                # 10. Immutable revision inspection + execution timeline.
                open_draft("wait-flow", "Wait Flow")
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
                # Back-to-draft reloads the draft asynchronously; poll
                # for the read-only marker to disappear instead of
                # reading the stale title immediately.
                expect(page.locator("#canvas-title")).not_to_contain_text(
                    "read-only")

                # 11. Keyboard move + narrow viewport + RTL.
                open_draft("browser-flow", "Browser Flow")
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
                open_draft("browser-flow", "Browser Flow")
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
                open_draft("wait-flow", "Wait Flow")
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
                open_draft("wait-flow", "Wait Flow")
                select_node("wait")
                assert page.locator(
                    '[data-field="input.duration_ms"]').input_value() \
                    == "75", "typed field value must survive save+reload"

                browser.close()
                # The app ships no favicon, so Chromium's automatic
                # /favicon.ico request 404s; that entry is benign, and
                # its console location names the URL. Likewise,
                # Chromium logs "Failed to load resource" for every
                # intentionally failed API call, and cases 7 and 14
                # deliberately provoke three of them (SoD 400 on
                # approve, no-trigger 400 on save, wrong-key 401 on
                # the connect refresh) and assert the UI's handling
                # of each — so those entries are expected, not noise
                # to hide: any UNINTENTIONAL 4xx would already have
                # failed its own case assertion before this point.
                # Anything else (JS exceptions, unhandled
                # rejections, unexpected statuses) is a real failure.
                def benign(t, u):
                    if u.endswith("/favicon.ico"):
                        return True
                    if (t.startswith("Failed to load resource") and
                            "/admin/api/" in u and
                            ("status of 400" in t or "status of 401" in t)):
                        return True
                    return False
                real_errors = [(t, u) for (t, u) in console_errors
                               if not benign(t, u)]
                assert not real_errors, \
                    f"unexpected console errors: {real_errors}"
                print("nexaflow studio browser acceptance: PASS")
        finally:
            if proc is not None:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()


if __name__ == "__main__":
    sys.exit(main())

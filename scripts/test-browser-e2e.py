#!/usr/bin/env python3
"""Real-browser acceptance for the embedded NexaRoute Control Plane.

The suite runs against a clean real gateway and Chromium. Provider discovery and
provider execution checks are intercepted only at the browser boundary so the
test never depends on public upstream services; all configuration mutations,
persistence, routing-object mutations, settings, and reads hit the real Go
backend.
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
import urllib.error
import urllib.request

from playwright.sync_api import Error as PlaywrightError
from playwright.sync_api import Page, expect, sync_playwright

ROOT = Path(__file__).resolve().parent.parent

# CI flake hardening (issue: Browser acceptance failed once on e37a694 then
# passed on rerun with no code change). Root causes found:
#  1. Port race: a free port was picked, the probe socket was closed, and
#     `go run` bound it later — another process (or a stale TIME_WAIT from a
#     prior CI step) could steal it, making the gateway exit with
#     "address already in use" while wait_ready reported a generic timeout.
#  2. Cold `go run` compile on CI can exceed the old fixed 20s readiness
#     deadline, and stdout=PIPE with no reader could block the child once
#     the pipe buffer filled.
#  3. Playwright `expect` defaults to a 5s timeout, too tight for loaded
#     CI runners on multi-step provider/discovery flows.
# Fixes: bounded port-retry loop, 60s readiness wait draining a log file
# (never a pipe), and a 15s default expect timeout.
READY_TIMEOUT_S = float(os.environ.get("NEXAROUTE_E2E_READY_TIMEOUT", "60"))
STARTUP_ATTEMPTS = int(os.environ.get("NEXAROUTE_E2E_STARTUP_ATTEMPTS", "3"))
EXPECT_TIMEOUT_MS = int(os.environ.get("NEXAROUTE_E2E_EXPECT_TIMEOUT_MS", "15000"))

try:
    expect.set_options(timeout=EXPECT_TIMEOUT_MS)
except Exception:
    pass


def pick_free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def read_log_tail(log_path: Path, limit: int = 40) -> str:
    try:
        lines = log_path.read_text(errors="replace").splitlines()
    except OSError:
        return "<unavailable>"
    return "\n".join(lines[-limit:])


def wait_ready(url: str, proc: subprocess.Popen[str], log_path: Path) -> None:
    deadline = time.monotonic() + READY_TIMEOUT_S
    last_err = ""
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError(
                f"gateway exited during startup: returncode={proc.returncode}\n"
                f"--- gateway log tail ---\n{read_log_tail(log_path)}"
            )
        try:
            with urllib.request.urlopen(url + "/healthz", timeout=2) as response:
                if response.status == 200:
                    # Health is up but the first compile may still be
                    # streaming; require a second consecutive success so we
                    # never race the UI server startup path.
                    time.sleep(0.5)
                    with urllib.request.urlopen(url + "/healthz", timeout=2) as second:
                        if second.status == 200:
                            return
                    return
        except (OSError, urllib.error.URLError) as exc:  # noqa: BLE001 - readiness polling
            last_err = str(exc)
            time.sleep(0.2)
    raise AssertionError(
        f"gateway did not become ready within {READY_TIMEOUT_S:g} seconds "
        f"(last error: {last_err})\n--- gateway log tail ---\n{read_log_tail(log_path)}"
    )


def start_gateway(config_path: Path, port: int, env: dict, log_file) -> subprocess.Popen[str]:
    return subprocess.Popen(
        ["go", "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)],
        cwd=ROOT,
        env=env,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        text=True,
    )


def is_addr_in_use(log_path: Path, returncode: int | None) -> bool:
    tail = read_log_tail(log_path).lower()
    return "address already in use" in tail or "cannot listen" in tail


def api_json(base: str, path: str, method: str = "GET", body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(
        base + path,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"} if data is not None else {},
    )
    try:
        with urllib.request.urlopen(req, timeout=8) as response:
            raw = response.read()
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        raise AssertionError(f"{method} {path} failed: {exc.code} {raw.decode(errors='replace')}") from exc
    return json.loads(raw or b"{}")


def provider_from_disk(config_path: Path, provider_id: str) -> dict:
    cfg = json.loads(config_path.read_text())
    for provider in cfg.get("providers", []):
        if provider.get("id") == provider_id:
            return provider
    raise AssertionError(f"provider {provider_id!r} not persisted")


def install_mock_provider_checks(page: Page) -> None:
    def discovery(route) -> None:
        body = json.loads(route.request.post_data or "{}")
        provider_id = body.get("provider", {}).get("id", "")
        if provider_id == "manual-e2e":
            route.fulfill(
                status=400,
                content_type="application/json",
                body=json.dumps({"error": {"message": "mock discovery unavailable"}}),
            )
            return
        route.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps({"ok": True, "models": ["model-alpha", "model-beta"]}),
        )

    def connection(route) -> None:
        route.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps({"ok": True, "status_code": 200}),
        )

    def model_test(route) -> None:
        body = json.loads(route.request.post_data or "{}")
        models = body.get("test_models") or []
        route.fulfill(
            status=200,
            content_type="application/json",
            body=json.dumps({
                "ok": True,
                "passed": len(models),
                "total": len(models),
                "results": [
                    {"model": model, "ok": True, "status_code": 200, "latency_ms": 1}
                    for model in models
                ],
            }),
        )

    page.route("**/admin/api/provider-discover", discovery)
    page.route("**/admin/api/provider-check", connection)
    page.route("**/admin/api/provider-test", model_test)


def set_provider_id(page: Page, provider_id: str) -> None:
    field = page.locator("#pId")
    if not field.is_visible():
        page.locator("#providerModal details.cp-advanced-toggle summary").click()
    field.fill(provider_id)


def wait_class_state(page: Page, selector: str, class_name: str, present: bool, timeout: int = 10000) -> None:
    page.wait_for_function(
        """([selector, className, present]) => {
            const el = document.querySelector(selector);
            return !!el && el.classList.contains(className) === present;
        }""",
        arg=[selector, class_name, present],
        timeout=timeout,
    )


def advance_provider(page: Page) -> None:
    page.locator("#cpProviderNext").click()


def save_provider(page: Page) -> None:
    page.locator("#saveProviderBtn").click()
    try:
        wait_class_state(page, "#providerModal", "open", False)
    except PlaywrightError as exc:
        toast = page.locator("#toast").inner_text() if page.locator("#toast").count() else ""
        raise AssertionError(f"provider save did not close modal; toast={toast!r}") from exc
    expect(page.locator("#apiState")).to_contain_text("connected")


def main() -> None:
    chromium = shutil.which("chromium") or shutil.which("google-chrome")
    if not chromium:
        raise SystemExit("SKIP: Chromium is not installed")

    with tempfile.TemporaryDirectory(prefix="nexaroute-browser-") as tmp:
        tmp_path = Path(tmp)
        config = json.loads((ROOT / "configs/config.example.json").read_text())
        config["providers"] = []
        config["virtual_endpoints"] = []
        config["route_profiles"] = []
        config["candidate_pools"] = []
        config["fallback_chains"] = []
        config["probe"]["enabled"] = False
        config["probe"]["on_start"] = False
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        config["listen"] = f"127.0.0.1:{port}"

        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        env = os.environ.copy()
        env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")
        gateway_log = tmp_path / "gateway.log"
        log_file = gateway_log.open("w")
        proc: subprocess.Popen[str] | None = None
        base = ""
        try:
            last_exc: Exception | None = None
            for attempt in range(1, STARTUP_ATTEMPTS + 1):
                port = pick_free_port()
                config["listen"] = f"127.0.0.1:{port}"
                config_path.write_text(json.dumps(config))
                base = f"http://127.0.0.1:{port}"
                if proc is not None and proc.poll() is None:
                    proc.terminate()
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait()
                proc = start_gateway(config_path, port, env, log_file)
                try:
                    wait_ready(base, proc, gateway_log)
                    last_exc = None
                    break
                except AssertionError as exc:
                    last_exc = exc
                    exited = proc.poll() is not None
                    if exited and is_addr_in_use(gateway_log, proc.returncode) and attempt < STARTUP_ATTEMPTS:
                        time.sleep(0.5)
                        continue
                    if exited and attempt < STARTUP_ATTEMPTS and "did not become ready" in str(exc):
                        # Cold `go run` compile or a slow runner: the child
                        # is alive but not ready yet; keep waiting in place
                        # instead of churning ports.
                        wait_ready(base, proc, gateway_log)
                        last_exc = None
                        break
                    raise
            if last_exc is not None:
                raise last_exc
            assert proc is not None
            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 900})
                console_errors: list[str] = []
                page.on("pageerror", lambda exc: console_errors.append(str(exc) + "\n" + (exc.stack or "")))
                page.on(
                    "console",
                    lambda msg: console_errors.append(msg.text)
                    if msg.type == "error" and "Failed to load resource" not in msg.text
                    else None,
                )
                install_mock_provider_checks(page)
                # The dashboard intentionally keeps an authenticated SSE
                # connection open, so networkidle can never be reached.
                page.goto(base + "/", wait_until="domcontentloaded")
                page.locator("#apiState").wait_for(state="visible", timeout=15000)
                evidence = Path(os.environ.get("NEXAROUTE_BROWSER_EVIDENCE", str(tmp_path / "browser-evidence")))
                evidence.mkdir(parents=True, exist_ok=True)
                for width, name in ((1440, "desktop"), (1024, "tablet"), (390, "phone")):
                    page.set_viewport_size({"width": width, "height": 900 if width > 500 else 844})
                    page.screenshot(path=str(evidence / f"dashboard-{name}.png"), full_page=True)
                page.set_viewport_size({"width": 1440, "height": 900})
                # The v2 bootstrap intentionally shortens the active Overview
                # label after mounting the control-plane chrome.
                assert page.locator("#apiState").inner_text().lower().find("connected") >= 0

                # First-run shell, navigation, theme and localization.
                expect(page.locator("#title")).to_have_text("Overview")
                expect(page.locator("#apiState")).to_contain_text("connected")
                page.locator('button[data-tab="providers"]').click()
                expect(page.locator("#providers")).to_be_visible()
                expect(page.locator("#providerGrid")).to_contain_text("No providers")
                page.locator("#cpThemeBtn").click()
                expect(page.locator("html")).to_have_attribute("data-theme", "light")
                page.locator("#cpLangBtn").click()
                expect(page.locator("html")).to_have_attribute("dir", "rtl")
                expect(page.locator("html")).to_have_attribute("lang", "fa")
                expect(page.locator(".cp-skip-link")).to_have_text("رفتن به محتوای اصلی")
                page.locator("#cpLangBtn").click()
                expect(page.locator("html")).to_have_attribute("dir", "ltr")
                expect(page.locator(".cp-skip-link")).to_have_text("Skip to main content")

                # Full provider flow: connection -> discovery -> checks -> save.
                page.locator("#addProviderBtn").click()
                expect(page.locator("#providerModal")).to_be_visible()
                page.locator("#pName").fill("Browser E2E Provider")
                set_provider_id(page, "e2e-provider")
                page.locator("#pBase").fill("https://provider.invalid/v1")
                page.locator("#pKey").fill("browser-secret-one")
                advance_provider(page)
                page.locator("#discoverBtn").click()
                expect(page.locator("#discoverStatus")).to_contain_text("2")
                expect(page.locator("#cpModelCount")).to_contain_text("0")
                page.locator("#cpSelectAll").click()
                expect(page.locator("#cpModelCount")).to_contain_text("2")
                advance_provider(page)
                page.locator("#checkConnectionBtn").click()
                expect(page.locator("#connectionStatus")).to_contain_text("OK")
                page.locator("#testProviderBtn").click()
                expect(page.locator("#testResults")).to_contain_text("PASS")
                save_provider(page)
                expect(page.locator("#providerGrid")).to_contain_text("Browser E2E Provider")
                assert provider_from_disk(config_path, "e2e-provider").get("api_key") == "browser-secret-one"

                # Edit preserves write-only secret when untouched.
                page.locator('.edit-provider[data-id="e2e-provider"]').click()
                expect(page.locator("#providerModal")).to_be_visible()
                # The server never returns the credential; the UI shows only a
                # fixed mask so operators can distinguish "saved" from empty.
                expect(page.locator("#pKey")).to_have_value("••••••••")
                expect(page.locator(".cp-secret-saved")).to_be_visible()
                page.locator("#pName").fill("Browser E2E Provider Renamed")
                advance_provider(page)
                advance_provider(page)
                save_provider(page)
                persisted = provider_from_disk(config_path, "e2e-provider")
                assert persisted.get("api_key") == "browser-secret-one", "untouched write-only secret was not preserved"
                assert persisted.get("name") == "Browser E2E Provider Renamed"

                # Explicit secret replacement replaces, rather than exposes, saved secret.
                page.locator('.edit-provider[data-id="e2e-provider"]').click()
                page.locator(".cp-secret-saved button").click()
                page.locator("#pKey").fill("browser-secret-two")
                advance_provider(page)
                advance_provider(page)
                save_provider(page)
                assert provider_from_disk(config_path, "e2e-provider").get("api_key") == "browser-secret-two"

                # Discovery-failure -> manual-model fallback -> persistence.
                page.locator("#addProviderBtn").click()
                page.locator("#pName").fill("Manual Provider")
                set_provider_id(page, "manual-e2e")
                page.locator("#pBase").fill("https://manual.invalid/v1")
                page.locator("#pProtocolMode").select_option("manual")
                page.locator("#pType").select_option("openai_compatible")
                page.locator("#pAuth").select_option("none")
                advance_provider(page)
                page.locator("#discoverBtn").click()
                expect(page.locator("#discoverStatus")).to_contain_text("mock discovery unavailable")
                page.locator("#manualModel").fill("manual-model")
                page.locator("#addModelBtn").click()
                expect(page.locator("#cpModelCount")).to_contain_text("1")
                advance_provider(page)
                save_provider(page)
                manual = provider_from_disk(config_path, "manual-e2e")
                assert [m.get("model") for m in manual.get("models", [])] == ["manual-model"]

                # Simple Route create (Automatic).
                page.locator('button[data-tab="routing"]').click()
                page.locator("#cpAddRoute").click()
                page.locator("#cpRouteName").fill("Coding Route")
                page.locator("#cpPublicModel").fill("coding")
                model_list = page.locator("#cpRouteModels")
                model_list.locator(".cp-check-item", has_text="model-alpha").locator("input").check()
                model_list.locator(".cp-check-item", has_text="model-beta").locator("input").check()
                page.locator('[data-dialog-value="save"]').click()
                wait_class_state(page, "#cpDialogHost", "open", False)
                expect(page.locator("#cpRouteList")).to_contain_text("coding")

                # Connect/CLI must use public model and must never expose provider secrets.
                page.locator('button[data-tab="cli"]').click()
                expect(page.locator("#cliBody")).to_contain_text("ANTHROPIC_MODEL=coding")
                cli_text = page.locator("#cliBody").inner_text()
                assert "browser-secret-one" not in cli_text and "browser-secret-two" not in cli_text

                # Regression test for issue #170: XSS via malicious public_model in CLI snippet.
                # Find the route profile created by the simple route.
                snapshot = api_json(base, "/admin/api/snapshot?limit=500&events=100")
                route = next(v for v in snapshot["virtual_endpoints"] if v["id"] == "route-coding")
                profile_id = route["route_profile"]
                # Create a virtual endpoint with a harmless but malicious-looking public_model.
                # Must avoid chars rejected by server validation: space, tab, \r, \n, ", ', `, $, \
                xss_payload = 'xss-test-<script>alert(1)</script>-<img/src=x/onerror=alert(2)>'
                api_json(base, "/admin/api/virtual-endpoints", "POST", {
                    "id": "xss-test-endpoint",
                    "name": "XSS Test Endpoint",
                    "public_model": xss_payload,
                    "route_profile": profile_id,
                    "enabled": True,
                })
                page.evaluate("refresh()")
                page.locator('button[data-tab="cli"]').click()
                # The payload must appear literally in the text content, not as executable HTML.
                cli_text = page.locator("#cliBody").inner_text()
                assert xss_payload in cli_text, f"XSS payload not found literally in CLI text: {cli_text}"
                # No script/img elements should have been created from the injection.
                script_count = page.locator("#cliBody script").count()
                img_count = page.locator("#cliBody img").count()
                assert script_count == 0, f"Script element injected via public_model: {script_count}"
                assert img_count == 0, f"IMG element injected via public_model: {img_count}"
                # The pre element should contain the escaped text, not live elements.
                # Escaped output uses HTML entities: <script> becomes <script>
                pre_html = page.locator("#cliBody pre").inner_html()
                assert "<script>" not in pre_html, "Unescaped script tag found in pre HTML"
                assert "<img" not in pre_html, "Unescaped img tag found in pre HTML"
                # Clean up test endpoint.
                api_json(base, "/admin/api/virtual-endpoints/xss-test-endpoint", "DELETE")
                page.evaluate("refresh()")

                # Edit Simple Route to Ordered fallback and prove backend state persists.
                page.locator('button[data-tab="routing"]').click()
                page.locator('[data-route-edit="route-coding"]').click()
                page.locator("#cpRouteMode").select_option("ordered")
                page.locator('[data-dialog-value="save"]').click()
                wait_class_state(page, "#cpDialogHost", "open", False)
                snapshot = api_json(base, "/admin/api/snapshot?limit=500&events=100")
                route = next(v for v in snapshot["virtual_endpoints"] if v["id"] == "route-coding")
                profile = next(p for p in snapshot["route_profiles"] if p["id"] == route["route_profile"])
                assert profile.get("fallback_chain"), "ordered route did not persist a fallback chain"

                # Advanced-managed route must open Advanced editor, not Simple Route UI.
                dep_id = next(d["id"] for d in snapshot["deployments"] if d["model"] == "model-alpha")
                api_json(base, "/admin/api/candidate-pools", "POST", {
                    "id": "shared-advanced-pool", "name": "Shared Advanced Pool",
                    "mode": "explicit", "deployments": [dep_id],
                })
                api_json(base, "/admin/api/route-profiles", "POST", {
                    "id": "advanced-profile", "name": "Advanced Profile",
                    "candidate_pool": "shared-advanced-pool",
                })
                api_json(base, "/admin/api/virtual-endpoints", "POST", {
                    "id": "advanced-endpoint", "name": "Advanced Endpoint",
                    "public_model": "advanced-model", "route_profile": "advanced-profile", "enabled": True,
                })
                page.evaluate("refresh()")
                expect(page.locator("#cpRouteList")).to_contain_text("advanced-model")
                page.locator('[data-route-edit="advanced-endpoint"]').click()
                expect(page.locator('[data-form="public_model"]')).to_be_visible()
                assert page.locator("#cpPublicModel").count() == 0, "advanced route was misclassified as simple"
                page.locator("[data-dialog-close]").click()

                # Delete simple route and prove shared advanced primitives survive.
                page.locator('[data-route-delete="route-coding"]').click()
                wait_class_state(page, "#cpDialogHost", "open", True)
                page.locator('[data-dialog-value="yes"]').click()
                expect(page.locator("#cpRouteList")).not_to_contain_text("Coding Route")
                snapshot = api_json(base, "/admin/api/snapshot?limit=500&events=100")
                assert any(x["id"] == "shared-advanced-pool" for x in snapshot["candidate_pools"])
                assert any(x["id"] == "advanced-profile" for x in snapshot["route_profiles"])
                assert any(x["id"] == "advanced-endpoint" for x in snapshot["virtual_endpoints"])

                # Runtime settings: invalid values fail visibly; valid values persist.
                page.locator('button[data-tab="settings"]').click()
                page.locator("#rtAttempts").fill("0")
                page.locator("#saveRuntimeSettings").click()
                expect(page.locator("#toast")).to_contain_text("Invalid runtime setting")
                assert api_json(base, "/admin/api/snapshot?limit=5&events=0")["config"]["routing"]["max_attempts"] != 0
                page.locator("#rtAttempts").fill("5")
                page.locator("#saveRuntimeSettings").click()
                expect(page.locator("#toast")).to_contain_text("saved")
                page.evaluate("refresh()")
                page.locator('button[data-tab="settings"]').click()
                expect(page.locator("#rtAttempts")).to_have_value("5")

                # Observability controls: filter categories and auto-scroll behavior.
                # Pause auto-refresh first. The dashboard tick() loop replaces
                # the whole `snap` object via refresh() every ~2s, which wipes
                # synthetic events injected below. That race (tick landing
                # between the inject and the filter clicks) is the CI flake
                # root cause: failed once on e37a694, passed on rerun.
                pause_btn = page.locator("#pauseBtn")
                if "Pause" in pause_btn.inner_text():
                    pause_btn.click()
                    expect(pause_btn).to_contain_text("Resume")
                page.evaluate("""() => {
                    snap.events = [
                      {time:new Date().toISOString(),kind:'route_ok',request_id:'r1',deployment:'e2e-provider/model-alpha',message:'ok',latency_ms:3},
                      {time:new Date().toISOString(),kind:'probe_fail',request_id:'p1',deployment:'e2e-provider/model-beta',message:'probe failed',error_type:'mock_failure'}
                    ];
                    renderConsole();
                }""")
                page.locator('button[data-tab="console"]').click()
                expect(page.locator("#consoleCount")).to_contain_text("2")
                page.locator('#consoleFilter button[data-f="errors"]').click()
                expect(page.locator("#consoleCount")).to_contain_text("1")
                expect(page.locator("#consoleLog")).to_contain_text("probe_fail")
                page.locator('#consoleFilter button[data-f="routes"]').click()
                expect(page.locator("#consoleLog")).to_contain_text("route_ok")
                page.locator("#consoleAuto").uncheck()
                assert not page.locator("#consoleAuto").is_checked()
                page.locator("#consoleAuto").check()
                # Resume live updates before continuing.
                pause_btn.click()
                expect(pause_btn).to_contain_text("Pause")

                # Pause/resume remains immediate and observable.
                page.locator("#pauseBtn").click()
                expect(page.locator("#pauseBtn")).to_contain_text("Resume")
                page.locator("#pauseBtn").click()
                expect(page.locator("#pauseBtn")).to_contain_text("Pause")

                # Delete confirmation: cancel is non-mutating, confirm deletes.
                page.locator('button[data-tab="providers"]').click()
                page.locator('.edit-provider[data-id="manual-e2e"]').click()
                page.locator("#deleteProviderBtn").click()
                page.locator('[data-dialog-value="no"]').click()
                expect(page.locator("#providerModal")).to_be_visible()
                assert provider_from_disk(config_path, "manual-e2e")["id"] == "manual-e2e"
                page.locator("#deleteProviderBtn").click()
                page.locator('[data-dialog-value="yes"]').click()
                wait_class_state(page, "#providerModal", "open", False)
                expect(page.locator("#providerGrid")).not_to_contain_text("Manual Provider")

                # Remove advanced refs before deleting their referenced provider.
                api_json(base, "/admin/api/virtual-endpoints/advanced-endpoint", "DELETE")
                api_json(base, "/admin/api/route-profiles/advanced-profile", "DELETE")
                api_json(base, "/admin/api/candidate-pools/shared-advanced-pool", "DELETE")
                page.evaluate("refresh()")
                page.locator('.edit-provider[data-id="e2e-provider"]').click()
                page.locator("#deleteProviderBtn").click()
                page.locator('[data-dialog-value="yes"]').click()
                wait_class_state(page, "#providerModal", "open", False)
                expect(page.locator("#providerGrid")).not_to_contain_text("Browser E2E Provider Renamed")

                assert not console_errors, "browser errors: " + "; ".join(console_errors)
                browser.close()

        finally:
            if proc is not None and proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            try:
                log_file.close()
            except OSError:
                pass

        print(
            "BROWSER E2E PASS: startup/navigation, localization/theme, provider create/edit/"
            "secret-preserve/secret-replace/delete, discovery failure/manual model, simple route "
            "create/edit/delete, advanced-route protection, settings validation/persistence, "
            "Connect output, observability filters/auto-scroll, pause/resume, no JS page errors"
        )


if __name__ == "__main__":
    try:
        main()
    except PlaywrightError as exc:
        raise SystemExit(f"Browser E2E failed: {exc}")

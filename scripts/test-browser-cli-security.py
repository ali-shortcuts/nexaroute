#!/usr/bin/env python3
"""Focused browser regression for issue #170 (audit finding F2).

`cliSnippet()`/`renderCLI()` in internal/httpapi/web/app.js interpolate
virtual-endpoint values (`public_model` -> `veModel`/`veList`, plus `base`)
into the generated CLI snippet that is written to `#cliBody`. This test runs a
real gateway configured with a unique, harmless malicious `public_model` marker
and asserts, for every CLI sub-tab, that:

  * the payload is displayed literally (text stays visible),
  * no DOM node is created from the payload inside `#cliBody`,
  * no inline event handler is installed inside `#cliBody`,
  * the injected handler never executes (dedicated flag stays untouched),
  * the copyable command output still contains the literal values.

The payload is harmless: it only increments a page-global counter if it ever
executes. No real secret appears in fixtures, logs or screenshots.
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
EVIDENCE = ROOT / "docs" / "browser-evidence"

READY_TIMEOUT_S = float(os.environ.get("NEXAROUTE_E2E_READY_TIMEOUT", "60"))
STARTUP_ATTEMPTS = int(os.environ.get("NEXAROUTE_E2E_STARTUP_ATTEMPTS", "3"))
EXPECT_TIMEOUT_MS = int(os.environ.get("NEXAROUTE_E2E_EXPECT_TIMEOUT_MS", "15000"))

try:
    expect.set_options(timeout=EXPECT_TIMEOUT_MS)
except Exception:
    pass

# Unique harmless payload: a `public_model` that a vulnerable innerHTML
# assignment would turn into a live <img> with an onerror handler. It passes
# backend validation (no spaces/quotes/backticks/$/backslash) and the only
# network effect of a firing handler would be the local 404 for src=x.
PAYLOAD = "nexa-xss-<img/src=x/onerror=window.__nexaCliXssFired++>"
FLAG = "window.__nexaCliXssFired"
CLI_SUBTABS = ("claude", "openai", "env", "docker")
INJECTED_SELECTOR = (
    "#cliBody img, #cliBody svg, #cliBody script, #cliBody iframe, "
    "#cliBody object, #cliBody embed, #cliBody link, #cliBody style, "
    "#cliBody input, #cliBody video, #cliBody audio, #cliBody math"
)
COUNT_HANDLERS_JS = (
    "els => els.filter(e => Array.from(e.attributes)"
    ".some(a => a.name.toLowerCase().startsWith('on'))).length"
)


def pick_free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def read_log_tail(log_path: Path, limit: int = 40) -> str:
    try:
        return "\n".join(log_path.read_text(errors="replace").splitlines()[-limit:])
    except OSError:
        return "<unavailable>"


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
                    time.sleep(0.5)
                    with urllib.request.urlopen(url + "/healthz", timeout=2) as second:
                        if second.status == 200:
                            return
                    return
        except (OSError, urllib.error.URLError) as exc:  # noqa: BLE001 - readiness polling
            last_err = str(exc)
            time.sleep(0.2)
    raise AssertionError(
        f"gateway did not become ready within {READY_TIMEOUT_S:g} seconds"
        f" (last error: {last_err})\n--- gateway log tail ---\n{read_log_tail(log_path)}"
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


def build_config() -> tuple[dict, int]:
    """Clean gateway config whose virtual endpoint carries the payload."""
    config = json.loads((ROOT / "configs/config.example.json").read_text())
    config["providers"] = []
    config["virtual_endpoints"] = []
    config["route_profiles"] = []
    config["candidate_pools"] = []
    config["fallback_chains"] = []
    config["probe"]["enabled"] = False
    config["probe"]["on_start"] = False
    config["candidate_pools"].append(
        {"id": "cli-sec-pool", "name": "CLI Security Pool", "mode": "all", "deployments": []}
    )
    config["route_profiles"].append(
        {
            "id": "cli-sec-profile",
            "name": "CLI Security Profile",
            "candidate_pool": "cli-sec-pool",
        }
    )
    config["virtual_endpoints"].append(
        {
            "id": "cli-sec-ve",
            "name": "CLI Security Endpoint",
            "public_model": PAYLOAD,
            "route_profile": "cli-sec-profile",
            "enabled": True,
        }
    )
    port = pick_free_port()
    config["listen"] = f"127.0.0.1:{port}"
    return config, port


def check_cli_tab(page: Page, tab: str) -> None:
    """Assert one CLI sub-tab renders the payload as inert literal text."""
    page.evaluate(f"{FLAG} = 0")
    page.locator(f'#cliTabs button[data-cli="{tab}"]').click()
    expect(page.locator("#cliBody")).to_be_visible()
    # Give a would-be injected <img> time to fail loading and run onerror.
    page.wait_for_timeout(400)

    injected = page.locator(INJECTED_SELECTOR).count()
    assert injected == 0, f"[{tab}] payload created {injected} DOM node(s) inside #cliBody"

    handlers = page.eval_on_selector_all("#cliBody *", COUNT_HANDLERS_JS)
    assert handlers == 0, f"[{tab}] payload installed {handlers} inline event handler(s) inside #cliBody"

    fired = page.evaluate(FLAG)
    assert fired == 0, f"[{tab}] injected handler executed (flag={fired!r})"

    expect(page.locator("#cliBody")).to_contain_text(PAYLOAD)

    print(f"  ok [{tab}]: literal text visible, 0 nodes, 0 handlers, flag={fired}")


def main() -> None:
    chromium = shutil.which("chromium") or shutil.which("google-chrome")
    if not chromium:
        raise SystemExit("SKIP: Chromium is not installed")

    config, port = build_config()
    env = os.environ.copy()
    env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "")

    with tempfile.TemporaryDirectory(prefix="nexaroute-cli-sec-") as tmp:
        tmp_path = Path(tmp)
        config_path = tmp_path / "config.json"
        config_path.write_text(json.dumps(config))
        gateway_log = tmp_path / "gateway.log"
        log_file = gateway_log.open("w")
        base = f"http://127.0.0.1:{port}"
        proc: subprocess.Popen[str] | None = None
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
                        wait_ready(base, proc, gateway_log)
                        last_exc = None
                        break
                    raise
            if last_exc is not None:
                raise last_exc
            assert proc is not None

            # The backend must accept and serve the payload untouched: the
            # fix belongs in the rendering layer, not in data validation.
            snapshot = api_json(base, "/admin/api/snapshot?limit=50&events=0")
            served = {ve["id"]: ve["public_model"] for ve in snapshot["virtual_endpoints"]}
            assert served.get("cli-sec-ve") == PAYLOAD, f"backend served {served!r}"

            with sync_playwright() as pw:
                browser = pw.chromium.launch(headless=True, executable_path=chromium)
                page = browser.new_page(viewport={"width": 1440, "height": 900})
                page_errors: list[str] = []
                page.on("pageerror", lambda exc: page_errors.append(str(exc)))
                page.goto(base + "/", wait_until="domcontentloaded")
                page.locator("#apiState").wait_for(state="visible", timeout=15000)
                expect(page.locator("#apiState")).to_contain_text("connected")

                page.evaluate(f"{FLAG} = 0")
                page.locator('button[data-tab="cli"]').click()
                expect(page.locator("#cliBody")).to_be_visible()

                for tab in CLI_SUBTABS:
                    check_cli_tab(page, tab)

                # Copyable command output is preserved with literal values.
                page.locator('#cliTabs button[data-cli="claude"]').click()
                pre_text = page.locator("#cliBody pre").inner_text()
                assert f"export ANTHROPIC_MODEL={PAYLOAD}" in pre_text, pre_text
                assert f"# virtual endpoints available: {PAYLOAD}" in pre_text, pre_text
                assert base in pre_text, pre_text
                assert page.locator("#cliCopy").count() == 1
                assert page.eval_on_selector("#cliCopy", "el => typeof el.onclick === 'function'") is True

                EVIDENCE.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(EVIDENCE / "cli-snippet-security.png"), full_page=True)

                assert not page_errors, "browser page errors: " + "; ".join(page_errors)
                browser.close()

            print(
                f"CLI SNIPPET SECURITY PASS: payload {PAYLOAD!r} rendered as literal "
                f"inert text across {len(CLI_SUBTABS)} CLI tabs; copyable command intact; "
                f"evidence: {EVIDENCE / 'cli-snippet-security.png'}"
            )
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


if __name__ == "__main__":
    try:
        main()
    except PlaywrightError as exc:
        raise SystemExit(f"CLI snippet security test failed: {exc}")

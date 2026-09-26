#!/usr/bin/env python3
"""Isolated release-install + real process/API lifecycle tests (standard library only)."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parent.parent

def check(condition, message):
    if not condition:
        raise AssertionError(message)

def executable(path, text):
    path.write_text(text)
    path.chmod(0o755)

def request(url, method="GET", body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data, {"Content-Type": "application/json"}, method=method)
    with urllib.request.urlopen(req, timeout=2) as response:
        return response.status, response.read()

def wait_ready(base, proc):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        check(proc.poll() is None, "gateway exited during startup")
        try:
            if request(base)[0] == 200:
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.05)
    raise AssertionError("UI not ready within 8 seconds")

def stop(proc):
    proc.terminate()
    try:
        proc.wait(timeout=8)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
        raise AssertionError("gateway failed graceful shutdown")
    check(proc.returncode == 0, "gateway shutdown failed")

with tempfile.TemporaryDirectory(prefix="nexaroute-install-test-") as tmp:
    tmp = Path(tmp)
    home, tools, dest, fixture = [tmp / n for n in ("home", "tools", "bin", "release")]
    for d in (home, tools, dest, fixture):
        d.mkdir()
    for name in ("nexaroute-linux-amd64", "nexaroute-linux-arm64", "SHA256SUMS"):
        shutil.copy2(ROOT / "dist" / name, fixture / name)
    executable(tools / "uname", '#!/bin/sh\nif [ "$1" = -s ]; then echo "${TEST_OS:-Linux}"; else echo "${TEST_ARCH:-x86_64}"; fi\n')
    executable(tools / "curl", '''#!/usr/bin/env python3
import os, pathlib, shutil, sys
args = sys.argv[1:]
assert "--proto" in args and "=https" in args and "--proto-redir" in args
url = next(a for a in args if a.startswith("https://"))
assert url.startswith("https://github.com/ali-shortcuts/nexaroute/releases/")
with open(os.environ["TEST_URL_LOG"], "a") as f: f.write(url + "\\n")
if os.environ.get("TEST_DOWNLOAD_FAIL"): sys.exit(22)
shutil.copyfile(pathlib.Path(os.environ["TEST_FIXTURE"]) / url.rsplit("/", 1)[1], args[args.index("-o")+1])
''')
    env = {k: v for k, v in os.environ.items() if not k.startswith("NEXAROUTE_")}
    env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"),
               PATH=f"{dest}:{tools}:/usr/local/bin:/usr/bin:/bin", NEXAROUTE_INSTALL_DIR=str(dest),
               TEST_FIXTURE=str(fixture), TEST_URL_LOG=str(tmp / "downloads"), DISPLAY="", WAYLAND_DISPLAY="")
    def install(extra=None, ok=True):
        # Exercise the exact canonical script copied into every GitHub Release.
        # Feeding it on stdin also proves the documented curl | bash mode.
        result = subprocess.run(["bash"], input=(ROOT / "scripts/install.sh").read_text(),
                                env=env | (extra or {}), text=True, capture_output=True)
        check((result.returncode == 0) == ok, result.stdout + result.stderr)
        return result
    install()
    binary = dest / "nexaroute"
    check(binary.stat().st_mode & 0o777 == 0o755, "binary permissions")
    check(subprocess.check_output(["bash", "-c", "command -v nexaroute"], env=env, text=True).strip() == str(binary), "PATH")
    check("NexaRoute v" in subprocess.check_output(["nexaroute", "-version"], env=env, text=True), "version")
    # Linux ARM64 asset selection, but don't execute it on the AMD64 host.
    for arch in ("aarch64", "arm64"):
        install({"TEST_ARCH": arch})
        check(binary.read_bytes() == (fixture / "nexaroute-linux-arm64").read_bytes(), "arm64 selection")
    for arch in ("x86_64", "amd64"):
        install({"TEST_ARCH": arch})
        check(binary.read_bytes() == (fixture / "nexaroute-linux-amd64").read_bytes(), "amd64 selection")
    saved_binary = binary.read_bytes()
    install({"TEST_ARCH": "riscv64"}, ok=False)
    install({"TEST_OS": "Darwin"}, ok=False)
    install({"NEXAROUTE_INSTALL_DIR": str(tmp / "not-in-path")}, ok=False)
    install({"TEST_DOWNLOAD_FAIL": "1"}, ok=False)
    sums = (fixture / "SHA256SUMS").read_text()
    (fixture / "SHA256SUMS").write_text("0" * 64 + "  nexaroute-linux-amd64\n")
    install(ok=False)
    (fixture / "SHA256SUMS").write_text(sums + sums)
    install(ok=False)
    (fixture / "SHA256SUMS").write_text("malformed manifest\n")
    install(ok=False)
    (fixture / "SHA256SUMS").write_text(sums)
    check(binary.read_bytes() == saved_binary, "failed install changed binary")
    print("PASS clean install, PATH, architecture selection, checksum/download rejection")

    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    env["NEXAROUTE_LISTEN"] = f"127.0.0.1:{port}"
    base = f"http://127.0.0.1:{port}/"
    cfg = home / ".config/nexaroute/config.json"
    processes = []
    def start(extra=None, args=()):
        log = open(tmp / f"gateway-{len(processes)}.log", "w")
        proc = subprocess.Popen(["nexaroute", *args], cwd=tmp, env=env | (extra or {}), stdout=log, stderr=log)
        log.close()
        processes.append(proc)
        wait_ready(base, proc)
        return proc
    try:
        proc = start()
        check(cfg.exists() and cfg.stat().st_mode & 0o777 == 0o600, "private persistent config")
        check(cfg.parent.stat().st_mode & 0o777 == 0o700, "private config directory")
        check(not (tmp / "config.json").exists(), "working-directory config created")
        duplicate = subprocess.run(["nexaroute"], env=env, capture_output=True, text=True, timeout=3)
        check(duplicate.returncode == 0 and "already running" in duplicate.stdout.lower(),
              duplicate.stdout + duplicate.stderr)
        check(proc.poll() is None, "duplicate launch disturbed the running gateway")
        # A separate config cannot take over the same listening address either.
        other = subprocess.run(["nexaroute", "-config", str(tmp / "other.json")], env=env, capture_output=True, text=True, timeout=3)
        check(other.returncode != 0 and "cannot listen" in other.stderr, other.stderr)
        provider = {"id": "test", "name": "Test provider", "type": "openai_compatible", "base_url": "http://127.0.0.1:9/v1",
                    "api_key": "literal-secret-123", "credentials": [{"name": "extra", "api_key": "pool-secret-456", "enabled": True}],
                    "auth_mode": "bearer", "enabled": False,
                    "models": [{"id": "m", "model": "upstream-model", "enabled": True, "weight": 1}]}
        status, body = request(base + "admin/api/providers", "POST", {"provider": provider})
        check(status == 201, body)
        def assert_safe(body):
            for secret in (b"literal-secret-123", b"pool-secret-456"):
                check(secret not in body, "secret returned")
        assert_safe(body)
        for route in ("admin/api/providers", "admin/api/providers/test", "admin/api/providers/test?reveal=1", "admin/api/providers/test?reveal=true", "admin/api/snapshot"):
            assert_safe(request(base + route)[1])
        provider.pop("api_key")
        provider.pop("credentials")
        provider["name"] = "Renamed"
        assert_safe(request(base + "admin/api/providers/test", "PUT", {"provider": provider, "preserve_secret": True})[1])
        for route, body in (
            ("candidate-pools", {"id": "coding", "mode": "explicit", "deployments": ["test/m"]}),
            ("route-profiles", {"id": "coding", "candidate_pool": "coding"}),
            ("virtual-endpoints", {"id": "coding", "public_model": "claude-coding", "route_profile": "coding", "enabled": True, "protocols": ["anthropic_messages"]}),
        ):
            request(base + "admin/api/" + route, "POST", body)
        original = cfg.read_bytes()
        # A distinguishable valid ELF fixture simulates a later release.
        with open(fixture / "nexaroute-linux-amd64", "ab") as f:
            f.write(b"\nrelease-upgrade-fixture\n")
        import hashlib
        upgrade_sum = hashlib.sha256((fixture / "nexaroute-linux-amd64").read_bytes()).hexdigest()
        (fixture / "SHA256SUMS").write_text(upgrade_sum + "  nexaroute-linux-amd64\n")
        # Upgrade is an atomic executable replacement even while running.
        install({"NEXAROUTE_VERSION": "v0.6.0"})
        install()
        check(binary.read_bytes() != saved_binary, "upgrade did not replace executable")
        check(cfg.read_bytes() == original, "upgrade/reinstall changed config")
        check("/download/v0.6.0/" in (tmp / "downloads").read_text(), "pinned release URL")
        stop(proc)
        check("no graphical session" in (tmp / "gateway-0.log").read_text(), "headless fallback")
        # Verify configured browser launcher receives the ready UI URL.
        browser_log = tmp / "browser-url"
        executable(tools / "google-chrome", f'#!/bin/sh\nprintf "%s" "$1" > "{browser_log}"\n')
        proc = start({"DISPLAY": ":test"})
        deadline = time.monotonic() + 3
        while not browser_log.exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        check(browser_log.read_text() == base, "browser URL")
        data = json.loads(request(base + "admin/api/providers/test")[1])
        check(data["provider"]["name"] == "Renamed" and data["has_secret"], "provider restart persistence")
        stored = json.loads(cfg.read_text())
        check(stored["providers"][0]["api_key"] == "literal-secret-123", "primary secret lost")
        check(stored["providers"][0]["credentials"][0]["api_key"] == "pool-secret-456", "pool secret lost")
        snapshot = json.loads(request(base + "admin/api/snapshot")[1])
        for key in ("candidate_pools", "route_profiles", "virtual_endpoints"):
            check(any(x["id"] == "coding" for x in snapshot[key]), key + " restart persistence")
        proc.kill()
        proc.wait()
        proc = start()  # SIGKILL must not leave a stale instance lock.
        stop(proc)
        # Slow existing startup probes must not block the UI or browser.
        entered, release = threading.Event(), threading.Event()
        class SlowProvider(BaseHTTPRequestHandler):
            def do_POST(self):
                entered.set()
                release.wait(5)
                try:
                    self.send_response(503)
                    self.end_headers()
                except OSError:
                    pass
            do_GET = do_POST
            def log_message(self, *_args):
                pass
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), SlowProvider)
        thread = threading.Thread(target=upstream.serve_forever, daemon=True)
        thread.start()
        try:
            stored["providers"][0]["enabled"] = True
            stored["providers"][0]["base_url"] = f"http://127.0.0.1:{upstream.server_port}"
            stored["probe"]["enabled"] = True
            stored["probe"]["on_start"] = True
            cfg.write_text(json.dumps(stored))
            browser_log.unlink()
            proc = start({"DISPLAY": ":test"})
            check(entered.wait(3), "startup probe did not contact slow provider")
            check(not release.is_set(), "slow provider unexpectedly released")
            deadline = time.monotonic() + 2
            while not browser_log.exists() and time.monotonic() < deadline:
                time.sleep(0.05)
            check(browser_log.read_text() == base, "browser waited for providers")
            check(request(base + "admin/api/snapshot")[0] == 200, "admin UI blocked by probes")
            release.set()
            stop(proc)
        finally:
            release.set()
            upstream.shutdown()
            upstream.server_close()
            thread.join()
        browser_log.unlink()
        proc = start({"DISPLAY": ":test"}, ("-no-browser",))
        time.sleep(0.2)
        check(not browser_log.exists(), "-no-browser still opened a browser")
        stop(proc)
        print("PASS UI/browser readiness while provider probes are blocked; -no-browser")
        print("PASS real startup, headless/browser launch, duplicate/port exclusion, upgrade/reinstall, restart persistence, write-only secrets, crash recovery")
    finally:
        for proc in processes:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
print("INSTALL PASS")

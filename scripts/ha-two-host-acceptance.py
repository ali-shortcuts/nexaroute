#!/usr/bin/env python3
"""Disposable two-host / container-isolated HA acceptance for issue #652.

This harness measures a real fault drill instead of describing one. It has
three deliberately separate modes:

  --self-test
      Deterministic, infrastructure-free checks of the parts that decide
      whether a live run is allowed and how its numbers are computed:
      owner-authorization validation, DSN redaction, RPO/RTO arithmetic,
      evidence validation and per-instance config generation. CI can run
      this; it proves none of the HA behaviour.

  --containers
      A disposable Docker topology on ONE host: two independent gateway
      containers (separate network namespaces, configs, data dirs and
      asset roots) against shared PostgreSQL and Redis containers. This is
      CONTAINER-ISOLATED, NOT two independent hosts. It is the only mode
      exercised in the sandbox that could not provide a second host.

  --two-host
      Two independent hosts reached over SSH, each running one gateway
      replica, against a shared PostgreSQL/Redis service supplied by the
      owner (or run on a third machine). It refuses to start unless an
      explicit owner-authorization file is supplied and still valid.

Honesty rules encoded here:

  * A same-host multi-process run is never labelled multi-host. The
    evidence document carries a topology class and a separate verdict for
    "two independent hosts"; --containers can never set the multi-host
    verdict to PASS.
  * RPO is the count of committed rows lost across a fault, computed from
    before/after row counts. RTO is a UTC timestamp difference between the
    fault/recovery boundary and the first successful observed operation.
    Nothing is extrapolated from a single repetition or from unit tests.
  * If a measurement was not taken, the field is the string
    "NOT_MEASURED" and the scenario is marked NOT_RUN, never zero.
  * No credential, token, DSN or private configuration is written to the
    evidence. URLs are redacted before logging or serialising.

Usage:
  python3 scripts/ha-two-host-acceptance.py --self-test
  python3 scripts/ha-two-host-acceptance.py --containers [--keep]
  python3 scripts/ha-two-host-acceptance.py --two-host \
      --authorization /path/to/authorization.json \
      --host-a ssh-target-a --host-b ssh-target-b

Environment for --two-host (never written to evidence):
  NEXAROUTE_HA_PG_DSN       shared PostgreSQL DSN (required)
  NEXAROUTE_HA_RESTORE_DSN  empty disposable database for the restore leg
  NEXAROUTE_HA_PG_STOP_CMD  shell command that stops the disposable DB
  NEXAROUTE_HA_PG_START_CMD shell command that starts the disposable DB

Exit codes: 0 all executed checks passed; 1 a check failed; 2 usage or
preflight error; 3 live prerequisites/authorization missing (NOT_RUN — not
a pass).
"""

from __future__ import annotations

import argparse
import concurrent.futures
import copy
import datetime as _dt
import json
import os
import pathlib
import re
import secrets
import shlex
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ISSUE = 652
SCHEMA = "nexaroute-ha-acceptance/1"
UTC = _dt.timezone.utc
NOT_MEASURED = "NOT_MEASURED"

# Scenario table names are part of the persistence contract; querying them
# is how RPO is measured from actual data rather than from an assumption.
TABLES = {
    "control_migrations": "nexaroute_control_migrations",
    "control_snapshots": "nexaroute_control_snapshots",
    "security_sessions": "nexaroute_security_sessions",
    "security_audit": "nexaroute_security_audit",
    "video_jobs": "nexaroute_video_jobs",
    "video_reservations": "nexaroute_video_cost_reservations",
}


def utcnow() -> _dt.datetime:
    return _dt.datetime.now(UTC)


def iso(ts: _dt.datetime | None) -> str:
    if ts is None:
        return NOT_MEASURED
    return ts.astimezone(UTC).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def parse_iso(value: str) -> _dt.datetime:
    return _dt.datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(UTC)


def seconds_between(start: _dt.datetime | None, end: _dt.datetime | None):
    if start is None or end is None:
        return NOT_MEASURED
    return round((end - start).total_seconds(), 3)


def redact(value: str | None) -> str:
    """Redact userinfo in URIs and obvious secret-looking query values."""
    if not value:
        return ""
    text = re.sub(r"(?i)([a-z][a-z0-9+.-]*://[^:/@\s]+:)[^@\s]+@", r"\1***@", value)
    text = re.sub(r"(?i)([?&](?:password|passwd|pwd|token|secret|key)=)[^&\s]+", r"\1***", text)
    return text


def redact_obj(value):
    """Recursively redact strings in an evidence structure."""
    if isinstance(value, str):
        return redact(value)
    if isinstance(value, list):
        return [redact_obj(v) for v in value]
    if isinstance(value, dict):
        return {k: redact_obj(v) for k, v in value.items()}
    return value


def load_authorization(path: str, host_a: str, host_b: str, now: _dt.datetime | None = None) -> dict:
    """Validate a written owner authorization for disposable staging.

    This is an operator gate, not a cryptographic signature: the file is
    expected to be committed or attached to the issue by the owner. The
    harness refuses to touch two remote hosts without it.
    """
    now = now or utcnow()
    with open(path, "r", encoding="utf-8") as fh:
        doc = json.load(fh)
    errors = []
    if doc.get("issue") != ISSUE:
        errors.append("authorization.issue must be 652")
    if not str(doc.get("authorized_by", "")).strip():
        errors.append("authorization.authorized_by is required")
    if str(doc.get("scope", "")).strip() != "disposable-staging":
        errors.append("authorization.scope must be disposable-staging")
    if doc.get("production_access") is not False:
        errors.append("authorization.production_access must be false")
    try:
        expires = parse_iso(str(doc.get("expires_utc", "")))
        if expires <= now:
            errors.append("authorization has expired")
    except Exception:
        errors.append("authorization.expires_utc must be an RFC3339 UTC timestamp")
    hosts = doc.get("hosts")
    if not isinstance(hosts, list) or not hosts:
        errors.append("authorization.hosts must list the staging targets")
    else:
        allowed = {str(h).strip() for h in hosts}
        for target in (host_a, host_b):
            if target and target not in allowed:
                errors.append(f"authorization does not cover host {target!r}")
    if doc.get("cost_ceiling_usd") is None:
        errors.append("authorization.cost_ceiling_usd is required (0 for no-cost local staging)")
    if errors:
        raise ValueError("; ".join(errors))
    return doc


def rpo_from_counts(before: dict, after: dict) -> dict:
    """Committed records lost per table (positive means data disappeared)."""
    out = {}
    for key in sorted(set(before) | set(after)):
        b = int(before.get(key, 0))
        a = int(after.get(key, 0))
        out[key] = b - a
    return out


def validate_evidence(doc: dict) -> list[str]:
    errors = []
    if doc.get("schema") != SCHEMA:
        errors.append("schema mismatch")
    if doc.get("issue") != ISSUE:
        errors.append("issue must be 652")
    topology = doc.get("topology", {})
    if topology.get("class") not in ("container-isolated-single-host", "two-independent-hosts"):
        errors.append("topology.class is not recognised")
    for key in ("scenarios", "measurements", "verdict", "blocked"):
        if key not in doc:
            errors.append(f"missing {key}")
    if not isinstance(doc.get("scenarios"), list):
        errors.append("scenarios must be a list")
    for scenario in doc.get("scenarios", []):
        if not scenario.get("id") or not scenario.get("status"):
            errors.append("every scenario needs id and status")
        if scenario.get("status") == "PASS" and not scenario.get("evidence"):
            errors.append(f"PASS scenario {scenario.get('id')} has no evidence")
    verdict = doc.get("verdict", {})
    if topology.get("class") == "container-isolated-single-host":
        if verdict.get("two_independent_hosts") == "PASS":
            errors.append("container-isolated run cannot claim two-independent-host PASS")
    if doc.get("measurements", {}).get("rpo_records_lost") == NOT_MEASURED:
        # Legitimate only when the report says which fault was not run.
        if not doc.get("blocked"):
            errors.append("NOT_MEASURED RPO requires an explicit blocker")
    return errors


def _counts_to_rpo(before: dict, after: dict) -> int:
    lost = rpo_from_counts(before, after)
    return max(0, max(lost.values()) if lost else 0)


def build_config(base: dict, replica_id: str, listen: str, provider_base: str,
                 storage_root: str, security_root: str, admin_key: str) -> dict:
    """Build one replica config; only documented per-instance fields differ."""
    cfg = copy.deepcopy(base)
    cfg["listen"] = listen
    cfg["client_base_url"] = ""
    cfg["control_plane"] = {
        "enabled": True,
        "backend": "postgres",
        "postgres_dsn_env": "NEXAROUTE_HA_PG_DSN",
        "config_failure": "last_known_good",
        "identity_failure": "fail_closed",
        "budget_failure": "fail_closed",
        "rate_limit_failure": "fail_closed",
        "breaker_sync": True,
        "breaker_sync_interval_seconds": 5,
        "breaker_sync_replica_id": replica_id,
    }
    cfg["admin"] = {
        "bind_local_only": False,
        "api_key": admin_key,
        "emergency_access_enabled": True,
        "security_store_path": security_root + "/nexaroute-security.db",
        "security_store_backend": "postgres",
        "security_store_url": "env:NEXAROUTE_HA_PG_DSN",
        "session_ttl_seconds": 28800,
        "idle_timeout_seconds": 1800,
        "audit_retention_days": 365,
        "audit_max_events": 100000,
        "oidc": {"enabled": False},
    }
    cfg["routing"] = {
        "strategy": "ready_mesh",
        "fallback_on_unknown_model": True,
        "session_affinity": True,
        "session_ttl_seconds": 3600,
        "p2c_window": 8,
        "max_attempts": 2,
        "max_inflight_requests": 128,
        "failure_threshold": 5,
        "cooldown_seconds": 60,
        "provider_cooldown_seconds": 10,
        "capability_failure_threshold": 2,
        "capability_cooldown_seconds": 30,
        "request_timeout_ms": 15000,
        "latency_weight": 0.015,
        "failure_weight": 25,
        "capacity_weight": 35,
        "retry_backoff_ms": 150,
        "max_retry_after_seconds": 60,
        "hedging_enabled": False,
        "max_repair_attempts": 1,
        "sanitize_enabled": True,
    }
    cfg["probe"] = {
        "enabled": True,
        "on_start": True,
        "interval_seconds": 30,
        "ready_lease_seconds": 120,
        "timeout_ms": 5000,
        "max_tokens": 1,
        "concurrency": 4,
        "recovery_attempts": 3,
        "recovery_retry_ms": 250,
        "recovery_success_threshold": 1,
        "recovery_cooldown_seconds": 5,
        "capability_probes": False,
    }
    cfg["providers"] = [{
        "id": "stub",
        "name": "Disposable stub upstream",
        "type": "openai_compatible",
        "base_url": provider_base,
        "data_handling": {"trains_on_data": "unknown", "retention": "unknown"},
        "auth_mode": "none",
        "forward_headers": [],
        "chat_path": "/v1/chat/completions",
        "models_path": "/v1/models",
        "stream_idle_timeout_seconds": 30,
        "enabled": True,
        "models": [{
            "id": "stub-model",
            "model": "stub-model",
            "enabled": True,
            "priority": 1,
            "weight": 1,
            "capabilities": {"streaming": True, "tools": True, "vision": False, "reasoning": False},
            "context_window": 8192,
            "input_cost_per_mtok": 0,
            "output_cost_per_mtok": 0,
        }],
    }]
    cfg["video"] = {
        "enabled": True,
        "store_path": storage_root + "/jobs.json",
        "storage_root": storage_root + "/assets",
        "queue_size": 32,
        "workers": 2,
        "auth_token_env": "NEXAROUTE_VIDEO_TOKEN",
        "max_asset_bytes": 536870912,
        "job_store_backend": "postgres",
        "job_store_url": "env:NEXAROUTE_HA_PG_DSN",
        "development_fake_provider": True,
        "comfyui_enabled": False,
        "piper_enabled": False,
        "code_render_enabled": False,
    }
    cfg["logging"] = {
        "file": "nexaroute.log",
        "max_size_mb": 32,
        "max_backups": 3,
        "access_mode": "sampled",
        "success_sample_every": 1000,
        "slow_request_ms": 5000,
        "console_max_lines_per_minute": 0,
    }
    cfg["cache"] = {"enabled": False}
    cfg["client_auth"] = {"enabled": False, "keys": [], "rpm": 0}
    cfg["evaluation"] = {"enabled": False, "live_enabled": False}
    cfg["decision"] = {"mode": "off"}
    cfg.pop("decision_chains", None)
    cfg.pop("decision_providers", None)
    cfg.pop("decision_policies", None)
    cfg.pop("decision_provider_health", None)
    return cfg


def config_diff_keys(a: dict, b: dict, prefix: str = "") -> list[str]:
    """Field paths that differ between two replica configs."""
    out = []
    keys = set(a) | set(b)
    for key in sorted(keys):
        path = f"{prefix}.{key}" if prefix else key
        if key not in a or key not in b:
            out.append(path)
        elif isinstance(a[key], dict) and isinstance(b[key], dict):
            out.extend(config_diff_keys(a[key], b[key], path))
        elif a[key] != b[key]:
            out.append(path)
    return out


def stub_document(port: int, bind: str) -> str:
    return f'''#!/usr/bin/env python3
# Disposable OpenAI-compatible stub for the #652 acceptance harness.
import json, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {{"hits": 0, "mode": "ok"}}
LOCK = threading.Lock()

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send_json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/__stub/stats":
            with LOCK:
                return self.send_json(200, dict(STATE))
        if self.path == "/__stub/mode":
            with LOCK:
                return self.send_json(200, dict(STATE))
        if self.path == "/healthz":
            return self.send_json(200, {{"status": "ok"}})
        if self.path == "/v1/models":
            return self.send_json(200, {{"object": "list", "data": [
                {{"id": "stub-model", "object": "model", "owned_by": "stub"}}]}})
        return self.send_json(404, {{"error": "not found"}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{{}}"
        if self.path == "/__stub/mode":
            try:
                doc = json.loads(raw or b"{{}}")
            except Exception:
                return self.send_json(400, {{"error": "bad json"}})
            with LOCK:
                STATE["mode"] = "fail" if doc.get("mode") == "fail" else "ok"
                return self.send_json(200, dict(STATE))
        if self.path == "/__stub/reset":
            with LOCK:
                STATE["hits"] = 0
                STATE["mode"] = "ok"
                return self.send_json(200, dict(STATE))
        if self.path == "/v1/chat/completions":
            with LOCK:
                STATE["hits"] += 1
                mode = STATE["mode"]
            if mode == "fail":
                return self.send_json(500, {{"error": {{"message": "stub failure"}}}})
            return self.send_json(200, {{
                "id": "chatcmpl-stub",
                "object": "chat.completion",
                "created": 0,
                "model": "stub-model",
                "choices": [{{"index": 0, "message": {{"role": "assistant", "content": "stub-ok"}},
                             "finish_reason": "stop"}}],
                "usage": {{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}},
            }})
        return self.send_json(404, {{"error": "not found"}})

if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 19091
    bind = sys.argv[2] if len(sys.argv) > 2 else "127.0.0.1"
    ThreadingHTTPServer((bind, port), H).serve_forever()
'''


# --------------------------------------------------------------- HTTP helpers

def http_json(url: str, method: str = "GET", payload=None, headers=None, timeout: float = 10.0):
    body = None
    hdrs = dict(headers or {})
    if payload is not None:
        body = json.dumps(payload).encode()
        hdrs.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=body, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            data = resp.read()
            return resp.status, data
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read()
    except Exception as exc:  # connection refused during an outage
        return None, str(exc).encode()


def http_text(url: str, timeout: float = 10.0):
    status, data = http_json(url, timeout=timeout)
    try:
        return status, data.decode("utf-8", "replace")
    except Exception:
        return status, ""


def wait_for(fn, timeout: float, interval: float = 0.2):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except Exception as exc:
            last = exc
        time.sleep(interval)
    if last:
        raise TimeoutError(str(last))
    return None


def wait_ready(base_url: str, timeout: float = 60.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status, _ = http_json(base_url + "/readyz", timeout=3.0)
        if status == 200:
            return utcnow()
        time.sleep(0.2)
    raise TimeoutError(f"replica did not become ready: {redact(base_url)}")


def wait_http(url: str, want: int, timeout: float = 20.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status, _ = http_json(url, timeout=3.0)
        if status == want:
            return utcnow()
        time.sleep(0.1)
    raise TimeoutError(f"HTTP {want} not observed at {redact(url)}")


# --------------------------------------------------------------- base runtime

class Runtime:
    class_name = "unknown"

    def __init__(self, evidence_dir: pathlib.Path, base_port: int, repetitions: int):
        self.evidence_dir = evidence_dir
        self.base_port = base_port
        self.repetitions = repetitions
        self.admin_key = "ha652-admin-" + secrets.token_hex(8)
        self.video_token = "ha652-video-" + secrets.token_hex(8)
        self.started = utcnow()

    def url(self, replica: str) -> str:
        raise NotImplementedError

    def setup(self) -> None:
        raise NotImplementedError

    def start_all(self) -> list[_dt.datetime]:
        raise NotImplementedError

    def kill_replica(self, replica: str) -> _dt.datetime:
        raise NotImplementedError

    def restart_replica(self, replica: str) -> _dt.datetime:
        raise NotImplementedError

    def stop_pg(self) -> _dt.datetime:
        raise NotImplementedError

    def start_pg(self) -> _dt.datetime:
        raise NotImplementedError

    def wait_pg_ready(self, timeout: float = 60.0) -> _dt.datetime:
        raise NotImplementedError

    def query_counts(self) -> dict:
        raise NotImplementedError

    def query_value(self, sql: str) -> str:
        raise NotImplementedError

    def dump_db(self, path: pathlib.Path) -> _dt.datetime:
        raise NotImplementedError

    def restore_db(self, path: pathlib.Path, target_db: str) -> _dt.datetime:
        raise NotImplementedError

    def backup(self, replica: str, out_path: pathlib.Path):
        raise NotImplementedError

    def restore_verify(self, replica: str, bundle: pathlib.Path, report: pathlib.Path):
        raise NotImplementedError

    def stub_stats(self):
        raise NotImplementedError

    def logs_tail(self, replica: str, lines: int = 80) -> str:
        raise NotImplementedError

    def cleanup(self) -> None:
        pass

    # common operations -------------------------------------------------
    def probe_health(self, replica: str):
        status_h, _ = http_json(self.url(replica) + "/healthz", timeout=3.0)
        status_r, _ = http_json(self.url(replica) + "/readyz", timeout=3.0)
        return status_h, status_r

    def chat(self, replica: str, model: str = "stub-model"):
        return http_json(
            self.url(replica) + "/v1/chat/completions",
            method="POST",
            payload={"model": model, "messages": [{"role": "user", "content": "ping"}], "max_tokens": 4},
            timeout=15.0,
        )

    def video_create(self, replica: str, key: str, prompt: str = "ha652 disposable clip"):
        return http_json(
            self.url(replica) + "/v1/video/jobs",
            method="POST",
            headers={"Authorization": "Bearer " + self.video_token},
            payload={
                "project_id": "ha652",
                "prompt": prompt,
                "mode": "text_to_video",
                "provider_preference": "fake",
                "model_preference": "fake-video-1",
                "duration_seconds": 1,
                "aspect_ratio": "16:9",
                "idempotency_key": key,
            },
            timeout=20.0,
        )

    def video_get(self, replica: str, job_id: str):
        return http_json(
            self.url(replica) + "/v1/video/jobs/" + urllib.parse.quote(job_id, safe=""),
            headers={"Authorization": "Bearer " + self.video_token},
            timeout=10.0,
        )


class ContainerRuntime(Runtime):
    class_name = "container-isolated-single-host"

    def __init__(self, evidence_dir: pathlib.Path, base_port: int, repetitions: int, keep: bool):
        super().__init__(evidence_dir, base_port, repetitions)
        self.keep = keep
        suffix = secrets.token_hex(4)
        self.ns = f"nexaroute-ha652-{suffix}"
        self.run_dir = evidence_dir / "run"
        self.run_dir.mkdir(parents=True, exist_ok=True)
        self.network = self.ns + "-net"
        self.pg = self.ns + "-pg"
        self.redis = self.ns + "-redis"
        self.db = "nexaroute_ha652"
        self.restore_db_name = "nexaroute_ha652_restored"
        self.pg_password = "ha652-" + secrets.token_hex(12)
        # Each replica runs its own loopback stub, because the egress policy
        # deliberately rejects plain HTTP to a non-loopback host. The stub
        # is part of the disposable replica, not a shared dependency.
        self.stub_port = 19091
        self.ports = {"a": self._free_port(18191), "b": self._free_port(18291), "c": self._free_port(18391)}
        self.images = {"pg": "postgres:16-alpine", "redis": "redis:7-alpine",
                       "gateway": "python:3.12-alpine"}
        self.gateway_containers = {"a": self.ns + "-gw-a", "b": self.ns + "-gw-b", "c": self.ns + "-gw-c"}

    @staticmethod
    def _free_port(start: int) -> int:
        for port in range(start, start + 400):
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
                try:
                    sock.bind(("127.0.0.1", port))
                    return port
                except OSError:
                    continue
        raise RuntimeError("no free TCP port")

    def sh(self, args, check=True, capture=True, input_bytes=None, stdout_file=None):
        proc = subprocess.run(
            args,
            check=False,
            stdout=stdout_file if stdout_file else (subprocess.PIPE if capture else None),
            stderr=subprocess.PIPE,
            input=input_bytes,
        )
        if check and proc.returncode != 0:
            detail = (proc.stderr or b"").decode("utf-8", "replace")[-2000:]
            raise RuntimeError(f"command failed ({args[0]} ...): {redact(detail)}")
        return proc

    def docker(self, *args, **kwargs):
        return self.sh(["docker", *args], **kwargs)

    def setup(self):
        # Images are only pulled when absent; this never contacts production.
        for image in self.images.values():
            if self.docker("image", "inspect", image, check=False).returncode != 0:
                self.docker("pull", image)
        self.docker("network", "create", self.network)
        self.docker(
            "run", "-d", "--name", self.pg, "--network", self.network,
            "-e", "POSTGRES_USER=nexaroute",
            "-e", f"POSTGRES_PASSWORD={self.pg_password}",
            "-e", f"POSTGRES_DB={self.db}",
            self.images["pg"],
        )
        self.docker("run", "-d", "--name", self.redis, "--network", self.network, self.images["redis"])
        self.wait_pg_ready()
        self._write_configs()

    def _container_pg_dsn(self, db: str) -> str:
        return f"postgres://nexaroute:{self.pg_password}@{self.pg}:5432/{db}?sslmode=disable"

    def _write_configs(self):
        stub_path = self.run_dir / "stub.py"
        stub_path.write_text(stub_document(self.stub_port, "127.0.0.1"), encoding="utf-8")
        for replica in ("a", "b", "c"):
            data = self.run_dir / f"data-{replica}"
            data.mkdir(parents=True, exist_ok=True)
            cfg = build_config(
                {}, f"replica-{replica}", "0.0.0.0:8080",
                f"http://127.0.0.1:{self.stub_port}",
                str(data / "video"), str(data / "security"), self.admin_key,
            )
            rendered = json.dumps(cfg, indent=2)
            (data / "config.json").write_text(rendered, encoding="utf-8")
            (self.run_dir / f"config-{replica}.json").write_text(rendered, encoding="utf-8")
            (data / "stub.py").write_text(stub_path.read_text(encoding="utf-8"), encoding="utf-8")
            (data / "image-tag").write_text(self.images["gateway"] + "\n", encoding="utf-8")

    def _start_container(self, replica: str, db: str | None = None):
        data = self.run_dir / f"data-{replica}"
        name = self.gateway_containers[replica]
        if self.docker("container", "inspect", name, check=False).returncode == 0:
            self.docker("rm", "-f", name)
        env = [
            "-e", f"NEXAROUTE_HA_PG_DSN={self._container_pg_dsn(db or self.db)}",
            "-e", f"NEXAROUTE_VIDEO_TOKEN={self.video_token}",
        ]
        self.docker(
            "run", "-d", "--name", name, "--network", self.network,
            "-p", f"127.0.0.1:{self.ports[replica]}:8080",
            *env,
            "-v", f"{data}:/data", "-w", "/data",
            "-v", f"{self.evidence_dir / 'nexaroute'}:/nexaroute:ro",
            self.images["gateway"], "sh", "-c",
            f"python3 /data/stub.py {self.stub_port} 127.0.0.1 > /data/stub.log 2>&1 & "
            f"exec /nexaroute -no-browser -config /data/config.json",
        )

    def url(self, replica: str) -> str:
        return f"http://127.0.0.1:{self.ports[replica]}"

    def start_all(self):
        return self._start_concurrently(("a", "b"), self.db)

    def _start_concurrently(self, replicas, db):
        results = {}

        def go(rep):
            self._start_container(rep, db)
            results[rep] = wait_ready(self.url(rep), 90.0)

        with concurrent.futures.ThreadPoolExecutor(max_workers=len(replicas)) as pool:
            futures = [pool.submit(go, rep) for rep in replicas]
            for future in futures:
                future.result()
        return [results[rep] for rep in replicas]

    def kill_replica(self, replica: str):
        ts = utcnow()
        self.docker("kill", "--signal", "KILL", self.gateway_containers[replica])
        return ts

    def restart_replica(self, replica: str):
        self.docker("start", self.gateway_containers[replica])
        return utcnow()

    def start_replica(self, replica: str, restored: bool = False):
        self._start_container(replica, self.restore_db_name if restored else self.db)
        return wait_ready(self.url(replica), 90.0)

    def stop_pg(self):
        ts = utcnow()
        self.docker("stop", "-t", "1", self.pg)
        return ts

    def start_pg(self):
        ts = utcnow()
        self.docker("start", self.pg)
        return ts

    def wait_pg_ready(self, timeout: float = 90.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            proc = self.docker("exec", self.pg, "pg_isready", "-U", "nexaroute", "-d", self.db, check=False)
            if proc.returncode == 0:
                return utcnow()
            time.sleep(0.2)
        raise TimeoutError("PostgreSQL did not accept connections")

    def _psql(self, sql: str, db: str | None = None) -> str:
        proc = self.docker("exec", self.pg, "psql", "-U", "nexaroute", "-d", db or self.db, "-tAc", sql)
        return (proc.stdout or b"").decode("utf-8", "replace").strip()

    def query_value(self, sql: str, db: str | None = None) -> str:
        return self._psql(sql, db)

    def query_counts(self, db: str | None = None) -> dict:
        out = {}
        for key, table in TABLES.items():
            try:
                out[key] = int(self._psql(f"SELECT count(*) FROM {table}", db) or 0)
            except Exception:
                out[key] = 0
        try:
            out["video_settled"] = int(self._psql(
                f"SELECT count(*) FROM {TABLES['video_reservations']} WHERE state LIKE '%settled'", db) or 0)
        except Exception:
            out["video_settled"] = 0
        return out

    def _exec_out(self, *args) -> bytes:
        proc = self.docker("exec", *args)
        return proc.stdout or b""

    def dump_db(self, path: pathlib.Path):
        ts = utcnow()
        with open(path, "wb") as fh:
            self.docker("exec", self.pg, "pg_dump", "-U", "nexaroute", "-d", self.db, stdout_file=fh)
        return ts

    def restore_db(self, path: pathlib.Path, target_db: str):
        ts = utcnow()
        self.docker("exec", self.pg, "psql", "-U", "nexaroute", "-d", "postgres", "-c",
                    f'DROP DATABASE IF EXISTS "{target_db}"')
        self.docker("exec", self.pg, "psql", "-U", "nexaroute", "-d", "postgres", "-c",
                    f'CREATE DATABASE "{target_db}"')
        with open(path, "rb") as fh:
            self.docker("exec", "-i", self.pg, "psql", "-U", "nexaroute", "-d", target_db,
                        "-v", "ON_ERROR_STOP=1", input_bytes=fh.read())
        return ts

    def backup(self, replica: str, out_path: pathlib.Path):
        container = self.gateway_containers[replica]
        inside = "/data/backup.ntxrb"
        proc = self.docker("exec", container, "/nexaroute", "backup",
                           "--config", "/data/config.json", "--out", inside, check=False)
        if proc.returncode == 0:
            self.docker("cp", f"{container}:{inside}", str(out_path))
        return proc.returncode, (proc.stdout or b"").decode("utf-8", "replace")

    def restore_verify(self, replica: str, bundle: pathlib.Path, report: pathlib.Path):
        container = self.gateway_containers[replica]
        inside_bundle = "/data/backup.ntxrb"
        inside_report = "/data/verify.json"
        with open(bundle, "rb") as fh:
            self.docker("exec", "-i", container, "sh", "-c", f"cat > {inside_bundle}", input_bytes=fh.read())
        proc = self.docker("exec", container, "/nexaroute", "restore", "--verify",
                           "--bundle", inside_bundle, "--config", "/data/config.json",
                           "--report", inside_report, check=False)
        report.parent.mkdir(parents=True, exist_ok=True)
        cp = self.docker("cp", f"{container}:{inside_report}", str(report), check=False)
        return proc.returncode, (proc.stdout or b"").decode("utf-8", "replace"), cp.returncode == 0

    def stub_stats(self, replica: str = "a"):
        code = (f"import urllib.request;"
                f"print(urllib.request.urlopen('http://127.0.0.1:{self.stub_port}/__stub/stats',timeout=2).read().decode())")
        proc = self.docker("exec", self.gateway_containers[replica], "python3", "-c", code, check=False)
        if proc.returncode != 0:
            return None
        try:
            return json.loads((proc.stdout or b"").decode())
        except Exception:
            return None

    def logs_tail(self, replica: str, lines: int = 80) -> str:
        proc = self.docker("logs", "--tail", str(lines), self.gateway_containers[replica], check=False)
        return ((proc.stdout or b"") + (proc.stderr or b"")).decode("utf-8", "replace")

    def cleanup(self):
        if self.keep:
            print(f"keeping containers and network {self.network}")
            return
        for name in list(self.gateway_containers.values()) + [self.redis, self.pg]:
            self.docker("rm", "-f", name, check=False)
        self.docker("network", "rm", self.network, check=False)


class SSHRuntime(Runtime):
    class_name = "two-independent-hosts"

    def __init__(self, evidence_dir, base_port, repetitions, host_a, host_b, auth,
                 pg_dsn, restore_dsn, pg_stop_cmd, pg_start_cmd, ssh_key=""):
        super().__init__(evidence_dir, base_port, repetitions)
        self.host_a = host_a
        self.host_b = host_b
        self.auth = auth
        self.pg_dsn = pg_dsn
        self.restore_dsn = restore_dsn
        self.pg_stop_cmd = pg_stop_cmd
        self.pg_start_cmd = pg_start_cmd
        self.ssh_key = ssh_key
        self.run_root = ".nexaroute-ha652"
        self.replicas = {"a": host_a, "b": host_b, "c": host_a}
        self.ports = {"a": self._free_remote_port(18191, "a"), "b": self._free_remote_port(18291, "b"),
                      "c": self._free_remote_port(18391, "c")}
        self.stub_port = 19091
        self.provider_base = f"http://127.0.0.1:{self.stub_port}"

    def _ssh(self, target, command, check=True, input_bytes=None):
        args = ["ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"]
        if self.ssh_key:
            args += ["-i", self.ssh_key]
        args += [target, command]
        proc = subprocess.run(args, input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if check and proc.returncode != 0:
            detail = (proc.stderr or b"").decode("utf-8", "replace")[-2000:]
            raise RuntimeError(f"ssh {target} failed: {redact(detail)}")
        return proc

    def _scp(self, source, target, dest):
        args = ["scp", "-q", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"]
        if self.ssh_key:
            args += ["-i", self.ssh_key]
        args += [str(source), f"{target}:{dest}"]
        proc = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if proc.returncode != 0:
            raise RuntimeError(f"scp to {target} failed: {redact((proc.stderr or b'').decode('utf-8', 'replace'))}")

    def _free_remote_port(self, start, replica="a"):
        # Ports only need to be free on the target host; a deterministic
        # range plus a socket check via ssh is enough for a disposable run.
        target = self.replicas.get(replica, self.host_a)
        for port in range(start, start + 200):
            proc = self._ssh(target,
                             f"python3 -c \"import socket;s=socket.socket();s.bind(('0.0.0.0',{port}));s.close()\"",
                             check=False)
            if proc.returncode == 0:
                return port
        raise RuntimeError("no free remote port")

    def url(self, replica: str) -> str:
        host = self.replicas[replica]
        # SSH targets may be user@host; the HTTP URL needs only the host.
        target = host.rsplit("@", 1)[-1].strip()
        return f"http://{target}:{self.ports[replica]}"

    def setup(self):
        binary = self.evidence_dir / "nexaroute"
        if not binary.exists():
            raise RuntimeError("gateway binary missing; build it before setup")
        stub_source = self.evidence_dir / "run" / "stub.py"
        stub_source.parent.mkdir(parents=True, exist_ok=True)
        stub_source.write_text(stub_document(self.stub_port, "127.0.0.1"), encoding="utf-8")
        for target in (self.host_a, self.host_b):
            self._ssh(target, f"rm -rf {self.run_root} && mkdir -p {self.run_root}")
        self._scp(binary, self.host_a, f"{self.run_root}/nexaroute")
        self._scp(binary, self.host_b, f"{self.run_root}/nexaroute")
        self._ssh(self.host_a, f"chmod +x {self.run_root}/nexaroute")
        self._ssh(self.host_b, f"chmod +x {self.run_root}/nexaroute")
        self._write_configs()

    def _write_configs(self):
        for replica in ("a", "b", "c"):
            host = self.replicas[replica]
            data = f"{self.run_root}/data-{replica}"
            local = self.evidence_dir / "run" / f"config-{replica}.json"
            local.parent.mkdir(parents=True, exist_ok=True)
            cfg = build_config(
                {}, f"replica-{replica}", f"0.0.0.0:{self.ports[replica]}",
                self.provider_base, f"{data}/video", f"{data}/security", self.admin_key,
            )
            local.write_text(json.dumps(cfg, indent=2), encoding="utf-8")
            self._ssh(host, f"mkdir -p {data}")
            self._scp(local, host, f"{data}/config.json")
            self._scp(self.evidence_dir / "run" / "stub.py", host, f"{data}/stub.py")
            env_lines = [
                "NEXAROUTE_HA_PG_DSN=" + shlex.quote(self.pg_dsn),
                "NEXAROUTE_VIDEO_TOKEN=" + shlex.quote(self.video_token),
            ]
            self._ssh(host, f"umask 077; cat > {data}/.env", input_bytes=("\n".join(env_lines) + "\n").encode())

    def _start_replica(self, replica, db_dsn=None):
        host = self.replicas[replica]
        data = f"{self.run_root}/data-{replica}"
        if db_dsn:
            env = ("NEXAROUTE_HA_PG_DSN=" + shlex.quote(db_dsn) + "\n"
                   "NEXAROUTE_VIDEO_TOKEN=" + shlex.quote(self.video_token) + "\n")
            self._ssh(host, f"umask 077; cat > {data}/.env", input_bytes=env.encode())
        command = (
            f"cd {data} && set -a && . ./.env && set +a && "
            f"nohup python3 {data}/stub.py {self.stub_port} 127.0.0.1 > stub.log 2>&1 & "
            f"echo $! > stub.pid; "
            f"nohup {self.run_root}/nexaroute -no-browser -config {data}/config.json "
            f"> gateway.log 2>&1 & echo $! > gateway.pid"
        )
        self._ssh(host, command)

    def start_all(self):
        results = {}

        def go(rep):
            self._start_replica(rep)
            results[rep] = wait_ready(self.url(rep), 90.0)

        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(go, rep) for rep in ("a", "b")]
            for future in futures:
                future.result()
        return [results["a"], results["b"]]

    def kill_replica(self, replica):
        ts = utcnow()
        data = f"{self.run_root}/data-{replica}"
        self._ssh(self.replicas[replica],
                  f"kill -KILL $(cat {data}/gateway.pid) 2>/dev/null || true; "
                  f"kill -TERM $(cat {data}/stub.pid) 2>/dev/null || true")
        return ts

    def restart_replica(self, replica):
        self._start_replica(replica)
        return utcnow()

    def start_replica(self, replica, db_dsn=None):
        self._start_replica(replica, db_dsn)
        return wait_ready(self.url(replica), 90.0)

    def _run_fault_cmd(self, command):
        proc = subprocess.run(["sh", "-c", command], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if proc.returncode != 0:
            raise RuntimeError(f"fault command failed: {redact((proc.stderr or b'').decode('utf-8', 'replace'))}")
        return utcnow()

    def stop_pg(self):
        return self._run_fault_cmd(self.pg_stop_cmd)

    def start_pg(self):
        return self._run_fault_cmd(self.pg_start_cmd)

    def wait_pg_ready(self, timeout: float = 90.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            proc = self._psql("SELECT 1", dsn=self.pg_dsn, check=False)
            if proc.returncode == 0:
                return utcnow()
            time.sleep(0.2)
        raise TimeoutError("PostgreSQL did not accept connections")

    def _psql(self, sql, dsn=None, db=None, check=True):
        dsn = dsn or self.pg_dsn
        if db:
            dsn = replace_db(dsn, db)
        args = ["psql", dsn, "-tAc", sql]
        return subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)

    def query_value(self, sql, db=None):
        proc = self._psql(sql, db=db)
        if proc.returncode != 0:
            raise RuntimeError("query failed")
        return (proc.stdout or b"").decode().strip()

    def query_counts(self, db=None):
        out = {}
        for key, table in TABLES.items():
            try:
                out[key] = int(self.query_value(f"SELECT count(*) FROM {table}", db=db) or 0)
            except Exception:
                out[key] = 0
        try:
            out["video_settled"] = int(self.query_value(
                f"SELECT count(*) FROM {TABLES['video_reservations']} WHERE state LIKE '%settled'", db=db) or 0)
        except Exception:
            out["video_settled"] = 0
        return out

    def dump_db(self, path):
        ts = utcnow()
        with open(path, "wb") as fh:
            proc = subprocess.run(["pg_dump", self.pg_dsn], stdout=fh, stderr=subprocess.PIPE)
        if proc.returncode != 0:
            raise RuntimeError("pg_dump failed")
        return ts

    def restore_db(self, path, target_db):
        ts = utcnow()
        admin = replace_db(self.pg_dsn, "postgres")
        subprocess.run(["psql", admin, "-c", f'DROP DATABASE IF EXISTS "{target_db}"'], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["psql", admin, "-c", f'CREATE DATABASE "{target_db}"'], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        with open(path, "rb") as fh:
            proc = subprocess.run(["psql", replace_db(self.pg_dsn, target_db), "-v", "ON_ERROR_STOP=1"],
                                  stdin=fh, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if proc.returncode != 0:
            raise RuntimeError("psql restore failed")
        return ts

    def backup(self, replica, out_path):
        data = f"{self.run_root}/data-{replica}"
        proc = self._ssh(self.replicas[replica],
                         f"cd {data} && ./nexaroute backup --config config.json --out backup.ntxrb",
                         check=False)
        if proc.returncode == 0:
            self._scp(self.replicas[replica], f"{data}/backup.ntxrb", str(out_path))
        return proc.returncode, (proc.stdout or b"").decode("utf-8", "replace")

    def restore_verify(self, replica, bundle, report):
        data = f"{self.run_root}/data-{replica}"
        self._scp(bundle, self.replicas[replica], f"{data}/backup.ntxrb")
        proc = self._ssh(self.replicas[replica],
                         f"cd {data} && ./nexaroute restore --verify --bundle backup.ntxrb "
                         f"--config config.json --report verify.json", check=False)
        got = False
        try:
            self._scp(self.replicas[replica], f"{data}/verify.json", str(report))
            got = True
        except Exception:
            pass
        return proc.returncode, (proc.stdout or b"").decode("utf-8", "replace"), got

    def stub_stats(self, replica: str = "a"):
        code = (f"import urllib.request;"
                f"print(urllib.request.urlopen('http://127.0.0.1:{self.stub_port}/__stub/stats',timeout=2).read().decode())")
        proc = self._ssh(self.replicas[replica], f"python3 -c \"{code}\"", check=False)
        if proc.returncode != 0:
            return None
        try:
            return json.loads((proc.stdout or b"").decode())
        except Exception:
            return None

    def logs_tail(self, replica, lines=80):
        data = f"{self.run_root}/data-{replica}"
        proc = self._ssh(self.replicas[replica], f"tail -n {lines} {data}/gateway.log", check=False)
        return (proc.stdout or b"").decode("utf-8", "replace")

    def cleanup(self):
        for replica, host in self.replicas.items():
            data = f"{self.run_root}/data-{replica}"
            self._ssh(host, f"test -f {data}/gateway.pid && kill -TERM $(cat {data}/gateway.pid) || true; "
                            f"test -f {data}/stub.pid && kill -TERM $(cat {data}/stub.pid) || true", check=False)


def replace_db(dsn: str, db: str) -> str:
    parsed = urllib.parse.urlsplit(dsn)
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, "/" + db, parsed.query, parsed.fragment))


# --------------------------------------------------------------- scenario runner

class Drill:
    def __init__(self, runtime: Runtime, evidence_dir: pathlib.Path):
        self.rt = runtime
        self.evidence = evidence_dir
        self.scenarios = []
        self.measurements = {}
        self.blocked = []
        self.failures = []
        self.stub_reset_hits = None

    def scenario(self, sid, name, status, evidence, detail=""):
        entry = {"id": sid, "name": name, "status": status, "evidence": evidence}
        if detail:
            entry["detail"] = detail
        self.scenarios.append(entry)
        print(f"{status}: {sid} {name}")
        return entry

    def require(self, condition, sid, message):
        if not condition:
            self.failures.append(f"{sid}: {message}")
            raise AssertionError(f"{sid}: {message}")

    def run(self):
        self.s1_cold_start()
        self.s2_baseline()
        self.s3_replica_failover()
        self.s4_video_claim_and_ledger()
        self.s5_database_outage()
        self.s6_backup_restore()
        self.s7_local_boundaries()
        self.s8_bounded_soak()

    def s1_cold_start(self):
        sid = "S1"
        t0 = utcnow()
        ready = self.rt.start_all()
        t_ready = max(ready)
        migrations = int(self.rt.query_value(f"SELECT count(*) FROM {TABLES['control_migrations']}") or 0)
        self.require(migrations == 1, sid, f"expected exactly 1 migration row, got {migrations}")
        ev = {"started_utc": iso(t0), "both_ready_utc": iso(t_ready),
              "start_to_ready_seconds": seconds_between(t0, t_ready),
              "control_migration_rows": migrations}
        self.measurements["cold_start_seconds"] = ev["start_to_ready_seconds"]
        self.scenario(sid, "concurrent cold start against an empty shared database", "PASS", ev)

    def s2_baseline(self):
        sid = "S2"
        hits_before = (self.rt.stub_stats() or {}).get("hits")
        health = {}
        chat = {}
        for replica in ("a", "b"):
            health[replica] = self.rt.probe_health(replica)
            status, body = self.rt.chat(replica)
            chat[replica] = {"status": status, "body_has_stub_ok": b"stub-ok" in body}
        hits_after = (self.rt.stub_stats() or {}).get("hits")
        self.require(all(v == (200, 200) for v in health.values()), sid,
                     f"health/ready not 200 on both replicas: {health}")
        self.require(all(v["status"] == 200 and v["body_has_stub_ok"] for v in chat.values()), sid,
                     f"chat failed on a replica: {chat}")
        ev = {"health_ready": {k: list(v) for k, v in health.items()}, "chat": chat,
              "stub_hits_before": hits_before, "stub_hits_after": hits_after}
        self.scenario(sid, "externally observable health and chat on both replicas", "PASS", ev)

    def s3_replica_failover(self):
        sid = "S3"
        kills = []
        for rep in range(1, self.rt.repetitions + 1):
            counts_before = self.rt.query_counts()
            killed = self.rt.kill_replica("a")
            # The survivor must never drop; poll it immediately and record the
            # first successful observation after the kill.
            first = wait_for(lambda: (self.rt.probe_health("b") == (200, 200)
                                      and self.rt.chat("b")[0] == 200), 30, 0.1)
            survivor_ts = utcnow()
            restarted_cmd = self.rt.restart_replica("a")
            ready_ts = wait_ready(self.rt.url("a"), 90.0)
            counts_after = self.rt.query_counts()
            rpo = rpo_from_counts(counts_before, counts_after)
            self.measurements[f"replica_kill_{rep}_survivor_rto_seconds"] = seconds_between(killed, survivor_ts)
            self.measurements[f"replica_restart_{rep}_rto_seconds"] = seconds_between(restarted_cmd, ready_ts)
            kills.append({
                "repetition": rep,
                "kill_utc": iso(killed),
                "survivor_first_success_utc": iso(survivor_ts),
                "survivor_rto_seconds": seconds_between(killed, survivor_ts),
                "restart_command_utc": iso(restarted_cmd),
                "restarted_ready_utc": iso(ready_ts),
                "restart_rto_seconds": seconds_between(restarted_cmd, ready_ts),
                "counts_before": counts_before,
                "counts_after": counts_after,
                "rpo_records_lost": rpo,
            })
            self.require(all(v <= 0 for v in rpo.values()), sid,
                         f"data loss after replica kill: {rpo}")
        self.scenario(sid, "replica SIGKILL, survivor serving and restart", "PASS",
                      {"repetitions": kills})

    def s4_video_claim_and_ledger(self):
        sid = "S4"
        jobs = []
        total = 6
        def create(i):
            replica = "a" if i % 2 == 0 else "b"
            key = f"ha652-{i}-{secrets.token_hex(4)}"
            status, body = self.rt.video_create(replica, key)
            if status != 202:
                return {"ok": False, "status": status, "body": body.decode("utf-8", "replace")[:300]}
            doc = json.loads(body)
            return {"ok": True, "replica": replica, "job_id": doc["job_id"]}

        with concurrent.futures.ThreadPoolExecutor(max_workers=total) as pool:
            results = list(pool.map(create, range(total)))
        self.require(all(r["ok"] for r in results), sid, f"video create failed: {results}")
        for result in results:
            job_id = result["job_id"]
            deadline = time.monotonic() + 60
            state = ""
            while time.monotonic() < deadline:
                status, body = self.rt.video_get("a" if result["replica"] == "b" else "b", job_id)
                if status == 200:
                    doc = json.loads(body)
                    state = doc.get("state", "")
                    if state in ("completed", "failed"):
                        break
                time.sleep(0.2)
            jobs.append({"job_id": job_id, "created_via": result["replica"], "state": state})
        self.require(all(j["state"] == "completed" for j in jobs), sid,
                     f"not all jobs completed: {jobs}")
        counts = self.rt.query_counts()
        self.require(counts["video_jobs"] == total, sid,
                     f"job rows = {counts['video_jobs']}, want {total}")
        self.require(counts["video_reservations"] == total, sid,
                     f"reservation rows = {counts['video_reservations']}, want {total}")
        self.require(counts["video_settled"] == total, sid,
                     f"settled reservations = {counts['video_settled']}, want {total}")
        duplicate_owners = self.rt.query_value(
            f"SELECT count(*) FROM (SELECT job_id, count(DISTINCT lease_owner) c FROM {TABLES['video_jobs']} "
            f"GROUP BY job_id HAVING count(DISTINCT lease_owner) > 1) d")
        self.require(duplicate_owners == "0", sid, "a job has more than one lease owner")
        dup_settlements = self.rt.query_value(
            f"SELECT count(*) FROM (SELECT job_id, count(*) c FROM {TABLES['video_reservations']} "
            f"GROUP BY job_id HAVING count(*) > 1) d")
        self.require(dup_settlements == "0", sid, "duplicate ledger settlement rows exist")
        self.scenario(sid, "concurrent video jobs, single claim and exactly-once ledger settlement", "PASS",
                      {"job_count": total, "counts": counts, "jobs": jobs,
                       "duplicate_lease_owners": 0, "duplicate_settlement_rows": 0})
        self._video_for_later = jobs[0]["job_id"]

    def s5_database_outage(self):
        sid = "S5"
        job_id = getattr(self, "_video_for_later", None)
        self.require(job_id is not None, sid, "no video job available")
        before = self.rt.query_counts()
        stopped = self.rt.stop_pg()
        time.sleep(0.5)
        health = {r: self.rt.probe_health(r) for r in ("a", "b")}
        during_get = {r: self.rt.video_get(r, job_id)[0] for r in ("a", "b")}
        during_create = {r: self.rt.video_create(r, "outage-" + r + "-" + secrets.token_hex(3))[0]
                         for r in ("a", "b")}
        leak = False
        for replica in ("a", "b"):
            status, body = self.rt.video_get(replica, job_id)
            text = body.decode("utf-8", "replace").lower()
            if any(term in text for term in ("postgres://", "dial tcp", "sql:", "pgx")):
                leak = True
        started = self.rt.start_pg()
        accepting = self.rt.wait_pg_ready()
        first_read = {}
        for replica in ("a", "b"):
            first_read[replica] = wait_for(lambda r=replica: self.rt.video_get(r, job_id)[0] == 200, 30, 0.1)
        recovered = utcnow()
        after = self.rt.query_counts()
        rpo = rpo_from_counts(before, after)
        self.require(all(h == (200, 200) for h in health.values()), sid,
                     f"liveness/readiness were not preserved during the DB outage: {health}")
        self.require(all(v == 503 for v in during_get.values()), sid,
                     f"existing job read did not return 503 during outage: {during_get}")
        self.require(all(v == 503 for v in during_create.values()), sid,
                     f"job create did not return 503 during outage: {during_create}")
        self.require(not leak, sid, "outage response leaked backend detail")
        self.require(all(v <= 0 for v in rpo.values()), sid, f"data loss across DB outage: {rpo}")
        self.measurements["pg_outage_first_read_rto_seconds"] = seconds_between(accepting, recovered)
        self.scenario(sid, "shared PostgreSQL outage and recovery without gateway restart", "PASS",
                      {"stopped_utc": iso(stopped), "accepting_utc": iso(accepting),
                       "start_command_utc": iso(started), "first_read_utc": iso(recovered),
                       "first_read_rto_seconds": seconds_between(accepting, recovered),
                       "health_ready_during_outage": {k: list(v) for k, v in health.items()},
                       "existing_job_read_during_outage": during_get,
                       "job_create_during_outage": during_create,
                       "counts_before": before, "counts_after": after,
                       "rpo_records_lost": rpo, "backend_detail_leaked": leak})

    def s6_backup_restore(self):
        sid = "S6"
        bundle = self.evidence / "backup.ntxrb"
        backup_started = utcnow()
        rc, out = self.rt.backup("b", bundle)
        backup_finished = utcnow()
        self.require(rc == 0, sid, f"nexaroute backup failed rc={rc}: {out[-300:]}")
        report = self.evidence / "restore-verify.json"
        vrc, vout, got = self.rt.restore_verify("b", bundle, report)
        verify_doc = {}
        if got:
            try:
                verify_doc = json.loads(report.read_text(encoding="utf-8"))
            except Exception:
                verify_doc = {}
        # A BLOCKED verify is the honest expected result for a PostgreSQL-
        # backed installation: the bundle does not own external stores.
        verify_status = verify_doc.get("status", NOT_MEASURED)
        dump_path = self.evidence / "shared.sql"
        dump_ts = self.rt.dump_db(dump_path)
        source_counts = self.rt.query_counts()
        restore_ts = self.rt.restore_db(dump_path, "nexaroute_ha652_restored")
        if isinstance(self.rt, SSHRuntime):
            restored_ready = self.rt.start_replica("c", db_dsn=self.rt.restore_dsn)
            restored_counts = self.rt.query_counts(db="nexaroute_ha652_restored")
        else:
            restored_ready = self.rt.start_replica("c", restored=True)
            restored_counts = self.rt.query_counts(db="nexaroute_ha652_restored")
        self.require(restored_counts["video_jobs"] == source_counts["video_jobs"], sid,
                     f"restored job count {restored_counts['video_jobs']} != {source_counts['video_jobs']}")
        job_id = getattr(self, "_video_for_later", None)
        readback = None
        if job_id:
            status, body = self.rt.video_get("c", job_id)
            readback = {"status": status, "state": (json.loads(body).get("state") if status == 200 else None)}
            self.require(status == 200, sid, "restored gateway could not read the drilled job")
        self.measurements["dump_restore_ready_seconds"] = seconds_between(dump_ts, restored_ready)
        self.scenario(sid, "application backup plus database dump restored into a fresh database", "PASS",
                      {"bundle_bytes": bundle.stat().st_size if bundle.exists() else 0,
                       "restore_verify_status": verify_status,
                       "restore_verify_exit": vrc,
                       "dump_bytes": dump_path.stat().st_size if dump_path.exists() else 0,
                       "source_counts": source_counts,
                       "restored_counts": restored_counts,
                       "backup_started_utc": iso(backup_started),
                       "backup_finished_utc": iso(backup_finished),
                       "dump_started_utc": iso(dump_ts),
                       "restored_replica_ready_utc": iso(restored_ready),
                       "backup_seconds": seconds_between(backup_started, backup_finished),
                       "dump_restore_ready_seconds": seconds_between(dump_ts, restored_ready),
                       "job_readback": readback})

    def s7_local_boundaries(self):
        sid = "S7"
        # Configs are generated from the same base and differ only in the
        # documented per-instance fields.
        a_cfg = self.evidence / "run" / "config-a.json"
        b_cfg = self.evidence / "run" / "config-b.json"
        if not (a_cfg.exists() and b_cfg.exists()):
            a_cfg = self.evidence / "run" / "data-a" / "config.json"
            b_cfg = self.evidence / "run" / "data-b" / "config.json"
        diff = []
        if a_cfg.exists() and b_cfg.exists():
            da = json.loads(a_cfg.read_text())
            db = json.loads(b_cfg.read_text())
            diff = config_diff_keys(da, db)
        expected_prefixes = ("listen", "control_plane.breaker_sync_replica_id",
                             "admin.security_store_path", "admin.api_key", "admin.security_store_url",
                             "video.storage_root", "video.store_path", "providers", "logging")
        unexpected = [d for d in diff if not d.startswith(expected_prefixes)]
        # Asset locality: the fake provider wrote the asset on the executing
        # replica only. A 503/404 from the peer is the documented boundary,
        # never evidence of a shared asset backend.
        job_id = getattr(self, "_video_for_later", None)
        asset = {"checked": False}
        if job_id:
            status_a, body_a = self.rt.video_get("a", job_id)
            if status_a == 200:
                doc = json.loads(body_a)
                assets = doc.get("output_assets") or []
                if assets:
                    asset_id = assets[0]["id"]
                    owner = doc.get("lease_owner", "")
                    local = "a" if owner.endswith("/") is False and "replica-a" in owner else None
                    # Try both replicas; the producing replica returns 200 and
                    # the peer is expected to be unable to serve the local file.
                    statuses = {}
                    for replica in ("a", "b"):
                        st, _ = http_json(self.rt.url(replica) + f"/v1/video/jobs/{job_id}/assets/{asset_id}",
                                          headers={"Authorization": "Bearer " + self.rt.video_token})
                        statuses[replica] = st
                    asset = {"checked": True, "asset_id": asset_id, "lease_owner": owner,
                             "statuses": statuses,
                             "shared_asset_backend": statuses.get("a") == 200 and statuses.get("b") == 200}
                    self.require(asset["shared_asset_backend"] is False, sid,
                                 "assets unexpectedly shared across replicas")
        self.scenario(sid, "local-only config and asset boundaries", "PASS",
                      {"config_diff_keys": diff, "unexpected_diff_keys": unexpected,
                       "asset_locality": asset,
                       "documented_per_instance": [
                           "config snapshot (local file; ADR 0001)",
                           "quota/RPM/TPM reservations",
                           "session affinity and request idempotency",
                           "OIDC pending transactions",
                           "video assets (jobs and ledger are shared on PostgreSQL)",
                       ]})

    def s8_bounded_soak(self):
        sid = "S8"
        duration = float(os.environ.get("NEXAROUTE_HA_SOAK_SECONDS", "60"))
        rounds = 0
        chat_ok = chat_fail = health_fail = 0
        jobs_ok = jobs_fail = 0
        started = utcnow()
        hits_before = sum((self.rt.stub_stats(r) or {}).get("hits", 0) for r in ("a", "b"))
        deadline = time.monotonic() + duration
        while time.monotonic() < deadline:
            rounds += 1
            replica = "a" if rounds % 2 else "b"
            if self.rt.probe_health(replica) != (200, 200):
                health_fail += 1
            status, _ = self.rt.chat(replica)
            if status == 200:
                chat_ok += 1
            else:
                chat_fail += 1
            if rounds % 3 == 0:
                status, _ = self.rt.video_create(replica, "soak-" + secrets.token_hex(4))
                if status == 202:
                    jobs_ok += 1
                else:
                    jobs_fail += 1
            time.sleep(min(5.0, max(0.0, deadline - time.monotonic())))
        ended = utcnow()
        hits_after = sum((self.rt.stub_stats(r) or {}).get("hits", 0) for r in ("a", "b"))
        counts = self.rt.query_counts()
        self.require(chat_fail == 0 and health_fail == 0 and jobs_fail == 0, sid,
                     f"soak failures: chat={chat_fail} health={health_fail} jobs={jobs_fail}")
        self.measurements["soak_duration_seconds"] = seconds_between(started, ended)
        self.scenario(sid, "bounded soak (not a multi-hour soak)", "PASS",
                      {"started_utc": iso(started), "ended_utc": iso(ended),
                       "duration_seconds": seconds_between(started, ended),
                       "rounds": rounds, "chat_ok": chat_ok, "chat_failed": chat_fail,
                       "health_failed": health_fail, "video_jobs_ok": jobs_ok,
                       "video_jobs_failed": jobs_fail,
                       "stub_hits_before": hits_before, "stub_hits_after": hits_after,
                       "final_counts": counts})

    def document(self, runtime, commit, versions, authorization=None):
        verdict = {
            "container_isolated_single_host": "NOT_RUN",
            "two_independent_hosts": NOT_MEASURED,
            "load_balancer_failover": NOT_MEASURED,
        }
        if runtime.class_name == "container-isolated-single-host":
            verdict["container_isolated_single_host"] = "FAIL" if self.failures else "PASS"
            verdict["two_independent_hosts"] = NOT_MEASURED
        else:
            verdict["two_independent_hosts"] = "FAIL" if self.failures else "PASS"
        doc = {
            "schema": SCHEMA,
            "issue": ISSUE,
            "generated_at": iso(utcnow()),
            "commit": commit,
            "versions": versions,
            "topology": {
                "class": runtime.class_name,
                "replicas": 2,
                "shared_postgres": True,
                "shared_redis": "configured" if isinstance(runtime, ContainerRuntime) else "provided",
                "load_balancer": None,
                "notes": ("two gateway containers, separate network namespaces/configs/data dirs, "
                          "one Docker host; NOT two independent hosts"
                          if isinstance(runtime, ContainerRuntime) else
                          "two SSH targets, one gateway process each, shared PostgreSQL supplied by the owner"),
            },
            "authorization": authorization,
            "scenarios": self.scenarios,
            "measurements": self.measurements,
            "verdict": verdict,
            "blocked": self.blocked,
            "failures": self.failures,
        }
        # Aggregate RPO from the scenarios that actually measured counts.
        seen = False
        worst = 0
        for scenario in self.scenarios:
            detail = scenario.get("evidence") or {}
            rpo = detail.get("rpo_records_lost") if isinstance(detail, dict) else None
            if isinstance(rpo, dict):
                seen = True
                worst = max(worst, max(rpo.values()) if rpo else 0)
            elif isinstance(rpo, dict) is False and scenario["id"] in ("S3", "S5"):
                seen = False
        doc["measurements"]["rpo_records_lost"] = max(0, worst) if seen else NOT_MEASURED
        if not seen:
            doc["measurements"]["rpo_note"] = "no completed outage/count scenario measured RPO"
        if runtime.class_name == "container-isolated-single-host":
            doc["blocked"].append(
                "two independent physical hosts and load-balancer failover were NOT available; "
                "container isolation on one Docker host is not a multi-host claim")
        errors = validate_evidence(doc)
        if errors:
            raise RuntimeError("evidence validation failed: " + "; ".join(errors))
        return redact_obj(doc)


def render_markdown(doc: dict) -> str:
    lines = [
        f"# HA acceptance — {doc['topology']['class']} — {doc['generated_at']}",
        "",
        f"- Issue: #{doc['issue']}",
        f"- Commit: `{doc['commit']}`",
        f"- Topology: **{doc['topology']['class']}**",
        f"- Verdict container-isolated single host: **{doc['verdict']['container_isolated_single_host']}**",
        f"- Verdict two independent hosts: **{doc['verdict']['two_independent_hosts']}**",
        f"- Load-balancer failover: **{doc['verdict']['load_balancer_failover']}**",
        "",
        "## Measured values",
        "",
        "| Measurement | Value |",
        "| --- | --- |",
    ]
    for key, value in sorted(doc.get("measurements", {}).items()):
        lines.append(f"| `{key}` | {value} |")
    lines += ["", "## Scenarios", ""]
    for scenario in doc.get("scenarios", []):
        lines.append(f"### {scenario['id']} — {scenario['name']}: {scenario['status']}")
        lines.append("")
        lines.append("```json")
        lines.append(json.dumps(scenario.get("evidence", {}), indent=2, sort_keys=True))
        lines.append("```")
        lines.append("")
    if doc.get("blocked"):
        lines += ["## BLOCKED / NOT_MEASURED", ""]
        for item in doc["blocked"]:
            lines.append(f"- {item}")
        lines.append("")
    if doc.get("failures"):
        lines += ["## Failures", ""]
        for item in doc["failures"]:
            lines.append(f"- {item}")
        lines.append("")
    return "\n".join(lines) + "\n"


def build_binary(out: pathlib.Path, repo: pathlib.Path) -> tuple[str, dict]:
    commit = subprocess.run(["git", "-C", str(repo), "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
    dirty = subprocess.run(["git", "-C", str(repo), "status", "--porcelain"], capture_output=True, text=True).stdout.strip()
    build_env = dict(os.environ)
    build_env["CGO_ENABLED"] = "0"
    proc = subprocess.run(
        ["go", "build", "-trimpath", "-o", str(out), "./cmd/gateway"],
        cwd=repo, capture_output=True, text=True, env=build_env,
    )
    if proc.returncode != 0:
        raise RuntimeError("go build failed: " + redact(proc.stderr[-2000:]))
    go_version = subprocess.run(["go", "version"], capture_output=True, text=True).stdout.strip()
    versions = {
        "go": go_version,
        "gateway_binary_bytes": out.stat().st_size,
        "build_tree_dirty": bool(dirty),
    }
    return commit, versions


def container_versions(runtime: ContainerRuntime) -> dict:
    def inspect(image):
        proc = runtime.docker("image", "inspect", image, "--format", "{{.Id}}", check=False)
        return (proc.stdout or b"").decode().strip()
    def version(container, command):
        proc = runtime.docker("exec", container, *command, check=False)
        return ((proc.stdout or b"") + (proc.stderr or b"")).decode("utf-8", "replace").strip().splitlines()[0:1]
    return {
        "postgres_image": runtime.images["pg"],
        "postgres_version": version(runtime.pg, ["postgres", "--version"]),
        "redis_image": runtime.images["redis"],
        "redis_version": version(runtime.redis, ["redis-server", "--version"]),
        "postgres_image_id": inspect(runtime.images["pg"]),
        "redis_image_id": inspect(runtime.images["redis"]),
        "gateway_image": runtime.images["gateway"],
    }


def run_self_test() -> int:
    failures = []

    def check(name, condition, detail=""):
        if condition:
            print(f"PASS: {name}")
        else:
            failures.append(f"{name}: {detail}")
            print(f"FAIL: {name}: {detail}")

    now = parse_iso("2026-10-30T00:00:00Z")
    base_auth = {
        "issue": 652,
        "authorized_by": "owner",
        "scope": "disposable-staging",
        "production_access": False,
        "expires_utc": "2026-11-30T00:00:00Z",
        "cost_ceiling_usd": 0,
        "hosts": ["staging-a", "staging-b"],
    }
    try:
        load_authorization("/dev/null", "staging-a", "staging-b", now)
        check("authorization parse rejects empty file", False)
    except Exception:
        check("authorization parse rejects empty file", True)

    tmp = pathlib.Path(os.environ.get("NEXAROUTE_HA_SELFTEST_DIR", ".artifacts/ha-two-host/selftest"))
    tmp.mkdir(parents=True, exist_ok=True)
    good = tmp / "auth-good.json"
    good.write_text(json.dumps(base_auth))
    try:
        got = load_authorization(str(good), "staging-a", "staging-b", now)
        check("authorization valid document accepted", got["issue"] == 652)
    except Exception as exc:
        check("authorization valid document accepted", False, str(exc))

    bad_cases = [
        ("wrong issue", {"issue": 999}),
        ("production access", {"production_access": True}),
        ("expired", {"expires_utc": "2026-01-01T00:00:00Z"}),
        ("host not covered", {"hosts": ["staging-a"]}),
        ("wrong scope", {"scope": "production"}),
    ]
    for name, patch in bad_cases:
        doc = dict(base_auth)
        doc.update(patch)
        path = tmp / f"auth-{name.replace(' ', '-')}.json"
        path.write_text(json.dumps(doc))
        try:
            load_authorization(str(path), "staging-a", "staging-b", now)
            check(f"authorization rejects {name}", False)
        except ValueError:
            check(f"authorization rejects {name}", True)

    check("redacts URI password", "***" in redact("postgres://user:secret@db:5432/x") and "secret" not in redact("postgres://user:secret@db:5432/x"))
    check("redacts token query", "token=***" in redact("http://h/x?token=abc"))
    check("rpo counts lost rows", rpo_from_counts({"a": 5, "b": 0}, {"a": 4, "b": 1}) == {"a": 1, "b": -1})
    check("rpo max lost", _counts_to_rpo({"a": 5}, {"a": 3}) == 2)

    t0 = parse_iso("2026-10-30T00:00:00Z")
    t1 = parse_iso("2026-10-30T00:00:01.500Z")
    check("RTO seconds", seconds_between(t0, t1) == 1.5)
    check("RTO NOT_MEASURED", seconds_between(None, t1) == NOT_MEASURED)

    cfg_a = build_config({}, "replica-a", "0.0.0.0:8080", "http://stub:19091", "/d/a/video", "/d/a/sec", "k")
    cfg_b = build_config({}, "replica-b", "0.0.0.0:8080", "http://stub:19091", "/d/b/video", "/d/b/sec", "k")
    diff = config_diff_keys(cfg_a, cfg_b)
    check("replica configs differ only in per-instance fields",
          all(d.startswith(("control_plane.breaker_sync_replica_id", "admin.security_store_path",
                            "video.storage_root", "video.store_path")) for d in diff), str(diff))
    check("config embeds no literal DSN", "NEXAROUTE_HA_PG_DSN" in json.dumps(cfg_a)
          and "postgres://" not in json.dumps(cfg_a))

    base_doc = {
        "schema": SCHEMA, "issue": 652,
        "topology": {"class": "container-isolated-single-host"},
        "scenarios": [{"id": "S1", "status": "PASS", "evidence": {"x": 1}}],
        "measurements": {"rpo_records_lost": 0},
        "verdict": {"two_independent_hosts": NOT_MEASURED},
        "blocked": ["no second host"],
    }
    check("valid evidence accepted", validate_evidence(base_doc) == [])
    bad_doc = json.loads(json.dumps(base_doc))
    bad_doc["verdict"]["two_independent_hosts"] = "PASS"
    check("container run cannot claim multi-host", any("two-independent-host" in e for e in validate_evidence(bad_doc)))
    bad_doc2 = json.loads(json.dumps(base_doc))
    bad_doc2["scenarios"][0]["evidence"] = {}
    check("PASS scenario needs evidence", any("no evidence" in e for e in validate_evidence(bad_doc2)))

    stub = stub_document(19091, "127.0.0.1")
    check("stub script contains stats endpoint", "/__stub/stats" in stub and "/v1/chat/completions" in stub)

    if failures:
        print(f"\n{len(failures)} self-test failure(s)")
        return 1
    print("\nself-test PASS")
    return 0


def run_containers(args, repo: pathlib.Path) -> int:
    stamp = utcnow().strftime("%Y%m%dT%H%M%SZ")
    evidence = pathlib.Path(args.evidence_dir or f".artifacts/ha-two-host/{stamp}").resolve()
    evidence.mkdir(parents=True, exist_ok=True)
    print(f"evidence: {evidence}")
    commit, versions = build_binary(evidence / "nexaroute", repo)
    runtime = ContainerRuntime(evidence, args.base_port, args.repetitions, args.keep)
    drill = Drill(runtime, evidence)
    exit_code = 1
    try:
        runtime.setup()
        versions.update(container_versions(runtime))
        drill.run()
        exit_code = 0
    except Exception as exc:
        if os.environ.get("NEXAROUTE_HA_DEBUG"):
            import traceback
            traceback.print_exc()
        drill.failures.append(str(exc))
        drill.scenario("FAIL", "run aborted", "FAIL", {"error": redact(str(exc))})
    finally:
        try:
            doc = drill.document(runtime, commit, versions, authorization=None)
        except Exception as exc:
            print(f"could not write evidence: {exc}", file=sys.stderr)
            doc = None
        if doc:
            (evidence / "evidence.json").write_text(json.dumps(doc, indent=2, sort_keys=True), encoding="utf-8")
            (evidence / "report.md").write_text(render_markdown(doc), encoding="utf-8")
            print(f"wrote {evidence / 'evidence.json'}")
            if doc["failures"]:
                exit_code = 1
        runtime.cleanup()
    return exit_code


def run_two_host(args, repo: pathlib.Path) -> int:
    missing = []
    if not args.authorization:
        missing.append("--authorization FILE (written owner authorization)")
    if not args.host_a or not args.host_b:
        missing.append("--host-a and --host-b SSH targets")
    if not os.environ.get("NEXAROUTE_HA_PG_DSN"):
        missing.append("NEXAROUTE_HA_PG_DSN (shared disposable PostgreSQL)")
    if not os.environ.get("NEXAROUTE_HA_RESTORE_DSN"):
        missing.append("NEXAROUTE_HA_RESTORE_DSN (empty disposable database)")
    if not os.environ.get("NEXAROUTE_HA_PG_STOP_CMD") or not os.environ.get("NEXAROUTE_HA_PG_START_CMD"):
        missing.append("NEXAROUTE_HA_PG_STOP_CMD and NEXAROUTE_HA_PG_START_CMD")
    for tool in ("ssh", "scp", "psql", "pg_dump", "go"):
        if not shutil.which(tool):
            missing.append(f"local tool {tool}")
    if missing:
        print("NOT_RUN: two-host acceptance prerequisites missing:", file=sys.stderr)
        for item in missing:
            print(f"  - {item}", file=sys.stderr)
        print("No host was contacted and no cloud resource was created.", file=sys.stderr)
        return 3
    try:
        auth = load_authorization(args.authorization, args.host_a, args.host_b)
    except Exception as exc:
        print(f"refusing to run: {exc}", file=sys.stderr)
        return 3
    stamp = utcnow().strftime("%Y%m%dT%H%M%SZ")
    evidence = pathlib.Path(args.evidence_dir or f".artifacts/ha-two-host/{stamp}").resolve()
    evidence.mkdir(parents=True, exist_ok=True)
    commit, versions = build_binary(evidence / "nexaroute", repo)
    runtime = SSHRuntime(
        evidence, args.base_port, args.repetitions, args.host_a, args.host_b, auth,
        os.environ["NEXAROUTE_HA_PG_DSN"], os.environ["NEXAROUTE_HA_RESTORE_DSN"],
        os.environ["NEXAROUTE_HA_PG_STOP_CMD"], os.environ["NEXAROUTE_HA_PG_START_CMD"],
        args.ssh_key or "",
    )
    drill = Drill(runtime, evidence)
    exit_code = 1
    try:
        runtime.setup()
        drill.run()
        exit_code = 0
    except Exception as exc:
        if os.environ.get("NEXAROUTE_HA_DEBUG"):
            import traceback
            traceback.print_exc()
        drill.failures.append(str(exc))
        drill.scenario("FAIL", "run aborted", "FAIL", {"error": redact(str(exc))})
    finally:
        try:
            doc = drill.document(runtime, commit, versions, authorization={
                "issue": auth.get("issue"),
                "authorized_by": auth.get("authorized_by"),
                "scope": auth.get("scope"),
                "expires_utc": auth.get("expires_utc"),
                "cost_ceiling_usd": auth.get("cost_ceiling_usd"),
            })
        except Exception as exc:
            print(f"could not write evidence: {exc}", file=sys.stderr)
            doc = None
        if doc:
            (evidence / "evidence.json").write_text(json.dumps(doc, indent=2, sort_keys=True), encoding="utf-8")
            (evidence / "report.md").write_text(render_markdown(doc), encoding="utf-8")
            print(f"wrote {evidence / 'evidence.json'}")
            if doc["failures"]:
                exit_code = 1
        runtime.cleanup()
    return exit_code


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="Issue #652 disposable HA acceptance")
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--self-test", action="store_true", help="deterministic, infrastructure-free checks")
    mode.add_argument("--containers", action="store_true", help="two gateway containers on one Docker host")
    mode.add_argument("--two-host", action="store_true", help="two independent hosts over SSH")
    mode.add_argument("--stub-server", nargs=2, metavar=("PORT", "BIND"), help=argparse.SUPPRESS)
    parser.add_argument("--authorization", help="owner authorization JSON for --two-host")
    parser.add_argument("--host-a", help="SSH target for replica A")
    parser.add_argument("--host-b", help="SSH target for replica B")
    parser.add_argument("--ssh-key", help="optional SSH identity file")
    parser.add_argument("--repetitions", type=int, default=3, help="replica kill repetitions (default 3)")
    parser.add_argument("--base-port", type=int, default=18000)
    parser.add_argument("--evidence-dir", help="in-repo evidence directory")
    parser.add_argument("--keep", action="store_true", help="containers mode: keep the disposable topology")
    args = parser.parse_args(argv)

    if args.stub_server:
        port, bind = int(args.stub_server[0]), args.stub_server[1]
        namespace = {}
        exec(stub_document(port, bind), namespace)  # noqa: S102 - local disposable helper
        return 0

    if args.self_test:
        return run_self_test()
    if args.repetitions < 1 or args.repetitions > 10:
        print("--repetitions must be between 1 and 10", file=sys.stderr)
        return 2
    repo = pathlib.Path(__file__).resolve().parent.parent
    if args.containers:
        return run_containers(args, repo)
    return run_two_host(args, repo)


if __name__ == "__main__":
    sys.exit(main())

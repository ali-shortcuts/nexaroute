#!/usr/bin/env python3
"""Measure NexaRoute overhead against a fixed-latency local mock upstream.

The benchmark intentionally compares only the same machine and workload. It does
not claim performance parity with other gateways. It writes JSON results when
--output is supplied and reports p50/p95/p99, throughput, RSS and streaming TTFT.
"""
from __future__ import annotations

import argparse
import json
import os
import resource
import socket
import statistics
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.error import URLError
from urllib.request import Request, urlopen


FIXED_LATENCY = 0.015
STREAM_GAP = 0.010
REQUESTS = 100


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


class MockHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args: object) -> None:
        return

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("content-length", "0"))
        self.rfile.read(length)
        time.sleep(FIXED_LATENCY)
        if self.path.endswith("/chat/completions") and self.headers.get("accept") == "text/event-stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()
            for payload in (
                'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n',
                'data: {"choices":[{"delta":{"content":" world"}}]}\n\n',
                "data: [DONE]\n\n",
            ):
                self.wfile.write(payload.encode())
                self.wfile.flush()
                time.sleep(STREAM_GAP)
            return
        body = json.dumps({
            "id": "bench",
            "object": "chat.completion",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 8, "completion_tokens": 2, "total_tokens": 10},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def percentile(values: list[float], p: float) -> float:
    values = sorted(values)
    if not values:
        return 0.0
    index = (len(values) - 1) * p / 100.0
    lower, upper = int(index), min(int(index) + 1, len(values) - 1)
    return values[lower] + (values[upper] - values[lower]) * (index - lower)


def request(url: str, stream: bool = False) -> tuple[float, float | None]:
    body = json.dumps({"model": "bench", "messages": [{"role": "user", "content": "ping"}], "stream": stream}).encode()
    req = Request(url, data=body, headers={"Content-Type": "application/json", "Accept": "text/event-stream" if stream else "application/json"})
    start = time.perf_counter()
    first = None
    with urlopen(req, timeout=10) as response:
        while True:
            chunk = response.read(256)
            if not chunk:
                break
            if first is None:
                first = time.perf_counter() - start
    return time.perf_counter() - start, first


def run_series(url: str, count: int) -> dict[str, object]:
    latencies = []
    start = time.perf_counter()
    for _ in range(count):
        elapsed, _ = request(url)
        latencies.append(elapsed)
    wall = time.perf_counter() - start
    return {"count": count, "p50_ms": percentile(latencies, 50) * 1000, "p95_ms": percentile(latencies, 95) * 1000, "p99_ms": percentile(latencies, 99) * 1000, "throughput_rps": count / wall, "min_ms": min(latencies) * 1000, "max_ms": max(latencies) * 1000}


def stream_once(url: str) -> dict[str, float]:
    elapsed, first = request(url, stream=True)
    return {"total_ms": elapsed * 1000, "ttft_ms": (first or elapsed) * 1000}


def process_hwm_kb(pid: int) -> int | None:
    try:
        for line in Path(f"/proc/{pid}/status").read_text(encoding="utf-8").splitlines():
            if line.startswith("VmHWM:"):
                return int(line.split()[1])
    except (FileNotFoundError, ValueError, PermissionError):
        return None
    return None


def gateway_config(path: Path, listen: str, upstream: str) -> None:
    config = {
        "listen": listen,
        "admin": {"bind_local_only": True, "api_key": ""},
        "probe": {"enabled": False, "on_start": False},
        # adaptive permits the deliberately probe-free mock benchmark to measure
        # request-path overhead without fabricating a provider health response.
        "routing": {"strategy": "adaptive", "max_attempts": 1, "max_inflight_requests": 128},
        "providers": [{
            "id": "mock", "name": "benchmark mock", "type": "openai_compatible", "base_url": upstream,
            "auth_mode": "none", "enabled": True, "models": [{
                "id": "bench", "model": "bench", "aliases": ["bench"], "enabled": True, "priority": 1,
                "weight": 1, "capabilities": {"streaming": True, "tools": False, "vision": False, "reasoning": False},
            }],
        }],
    }
    path.write_text(json.dumps(config), encoding="utf-8")


def wait_ready(base: str, proc: subprocess.Popen[str]) -> None:
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"gateway exited with {proc.returncode}")
        try:
            with urlopen(base + "/healthz", timeout=1) as response:
                if response.status == 200:
                    return
        except URLError:
            time.sleep(0.1)
    raise RuntimeError("gateway did not become ready")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--requests", type=int, default=REQUESTS)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    mock_port, gateway_port = free_port(), free_port()
    mock = ThreadingHTTPServer(("127.0.0.1", mock_port), MockHandler)
    thread = threading.Thread(target=mock.serve_forever, daemon=True)
    thread.start()
    upstream_url = f"http://127.0.0.1:{mock_port}/v1"
    direct_url = upstream_url + "/chat/completions"
    gateway_url = f"http://127.0.0.1:{gateway_port}/v1/chat/completions"
    result: dict[str, object] = {
        "method": {"fixed_upstream_latency_ms": FIXED_LATENCY * 1000, "stream_gap_ms": STREAM_GAP * 1000, "requests": args.requests, "workload": "non-streaming OpenAI Chat Completions; sequential; local loopback"},
        "environment": {"python": os.sys.version.split()[0], "platform": os.uname().machine, "cpu_count": os.cpu_count()},
    }
    proc = None
    try:
        result["direct"] = run_series(direct_url, args.requests)
        direct_stream = stream_once(direct_url)
        with tempfile.TemporaryDirectory(prefix="nexaroute-bench-") as tmp:
            config_path = Path(tmp) / "config.json"
            gateway_config(config_path, f"127.0.0.1:{gateway_port}", upstream_url)
            log = open(Path(tmp) / "gateway.log", "w", encoding="utf-8")
            go = os.environ.get("NEXAROUTE_GO", "/usr/local/go/bin/go")
            proc = subprocess.Popen([go, "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)], cwd=Path(__file__).parents[2], stdout=log, stderr=subprocess.STDOUT)
            wait_ready(f"http://127.0.0.1:{gateway_port}", proc)
            # Prime readiness and discard the first request affected by process/JIT warmup.
            request(gateway_url)
            result["gateway"] = run_series(gateway_url, args.requests)
            gateway_stream = stream_once(gateway_url)
            result["streaming"] = {"direct": direct_stream, "gateway": gateway_stream, "ttft_overhead_ms": gateway_stream["ttft_ms"] - direct_stream["ttft_ms"]}
            result["overhead"] = {key: result["gateway"][key] - result["direct"][key] for key in ("p50_ms", "p95_ms", "p99_ms")}
            result["memory"] = {"benchmark_process_maxrss_kb": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss, "gateway_vm_hwm_kb": process_hwm_kb(proc.pid)}
            log.close()
    finally:
        if proc is not None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        mock.shutdown()
    print(json.dumps(result, indent=2))
    if args.output:
        args.output.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()

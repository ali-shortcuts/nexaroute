#!/usr/bin/env python3
"""Measure NexaRoute overhead with a low-jitter local Go mock upstream.

The mock uses TCP_NODELAY and writes each non-streaming response in one write.
The client uses persistent HTTP/1.1 connections, warm-up requests, and reports
latency/throughput at concurrency 1, 16, and 64. Streaming TTFT remains a
separate test because streaming necessarily emits multiple chunks.
"""
from __future__ import annotations

import argparse
import json
import os
import resource
import socket
import subprocess
import shutil
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPConnection
from pathlib import Path
from urllib.parse import urlsplit

FIXED_LATENCY = 0.015
STREAM_GAP = 0.010
REQUESTS = 1000
CONCURRENCIES = (1, 16, 64)
CAPS = (32, 128, 256)
BODY = json.dumps({"model": "bench", "messages": [{"role": "user", "content": "ping"}], "stream": False}).encode()
STREAM_BODY = json.dumps({"model": "bench", "messages": [{"role": "user", "content": "ping"}], "stream": True}).encode()
GO_MOCK = r'''
package main

import (
    "fmt"
    "io"
    "net"
    "net/http"
    "os"
    "strconv"
    "time"
)

const fixedLatency = 15 * time.Millisecond
const streamGap = 10 * time.Millisecond

var responseBody = []byte(`{"id":"bench","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`)
var streamParts = [][]byte{
    []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"),
    []byte("data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"),
    []byte("data: [DONE]\n\n"),
}

type handler struct{}
func (handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.Method == "GET" {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("ok"))
        return
    }
    _, _ = io.Copy(io.Discard, r.Body)
    _ = r.Body.Close()
    time.Sleep(fixedLatency)
    if r.Header.Get("Accept") == "text/event-stream" {
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")
        w.WriteHeader(http.StatusOK)
        flusher, _ := w.(http.Flusher)
        for _, part := range streamParts {
            _, _ = w.Write(part)
            if flusher != nil { flusher.Flush() }
            time.Sleep(streamGap)
        }
        return
    }
    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write(responseBody)
}

func main() {
    if len(os.Args) != 2 { panic("expected port") }
    port := ":" + os.Args[1]
    server := &http.Server{Addr: port, Handler: handler{}, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
    server.ConnState = func(conn net.Conn, state http.ConnState) {
        if tcp, ok := conn.(*net.TCPConn); ok && state == http.StateNew { _ = tcp.SetNoDelay(true) }
    }
    fmt.Printf("mock listening on %s\n", port)
    if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed { panic(err) }
}
'''


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def percentile(values: list[float], p: float) -> float:
    values = sorted(values)
    if not values:
        return 0.0
    index = (len(values) - 1) * p / 100.0
    lower, upper = int(index), min(int(index) + 1, len(values) - 1)
    return values[lower] + (values[upper] - values[lower]) * (index - lower)


def client_parts(url: str) -> tuple[str, int, str]:
    parsed = urlsplit(url)
    return parsed.hostname or "127.0.0.1", parsed.port or 80, parsed.path


def one_request(conn: HTTPConnection, path: str, stream: bool = False) -> tuple[float, float | None]:
    payload = STREAM_BODY if stream else BODY
    headers = {"Content-Type": "application/json", "Accept": "text/event-stream" if stream else "application/json"}
    started = time.perf_counter()
    conn.request("POST", path, payload, headers)
    response = conn.getresponse()
    if response.status != 200:
        error_body = response.read(512)
        raise RuntimeError(f"benchmark request returned HTTP {response.status}: {error_body!r}")
    first = None
    while True:
        chunk = response.read(256)
        if not chunk:
            break
        if first is None:
            first = time.perf_counter() - started
    return time.perf_counter() - started, first


def run_series(url: str, count: int, concurrency: int) -> dict[str, object]:
    host, port, path = client_parts(url)
    worker_count = min(concurrency, count)
    per_worker = [count // worker_count] * worker_count
    for index in range(count % worker_count):
        per_worker[index] += 1

    def worker(request_count: int) -> list[float]:
        conn = HTTPConnection(host, port, timeout=30)
        try:
            for _ in range(2):
                one_request(conn, path)
            values = []
            for _ in range(request_count):
                elapsed, _ = one_request(conn, path)
                values.append(elapsed)
            return values
        finally:
            conn.close()

    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=worker_count) as pool:
        futures = [pool.submit(worker, request_count) for request_count in per_worker]
        latencies = [value for future in futures for value in future.result()]
    wall = time.perf_counter() - started
    return {
        "count": len(latencies),
        "concurrency": concurrency,
        "p50_ms": percentile(latencies, 50) * 1000,
        "p95_ms": percentile(latencies, 95) * 1000,
        "p99_ms": percentile(latencies, 99) * 1000,
        "throughput_rps": len(latencies) / wall,
        "min_ms": min(latencies) * 1000,
        "max_ms": max(latencies) * 1000,
    }


def stream_once(url: str) -> dict[str, float]:
    host, port, path = client_parts(url)
    conn = HTTPConnection(host, port, timeout=30)
    try:
        one_request(conn, path)
        elapsed, first = one_request(conn, path, stream=True)
        return {"total_ms": elapsed * 1000, "ttft_ms": (first or elapsed) * 1000}
    finally:
        conn.close()


def process_hwm_kb(pid: int) -> int | None:
    try:
        for line in Path(f"/proc/{pid}/status").read_text(encoding="utf-8").splitlines():
            if line.startswith("VmHWM:"):
                return int(line.split()[1])
    except (FileNotFoundError, ValueError, PermissionError):
        return None
    return None


def gateway_config(path: Path, listen: str, upstream: str, config_name: str, provider_max_concurrency: int) -> None:
    if config_name == "ready_mesh_default":
        routing = {"max_inflight_requests": 128}
        probe = {"enabled": True, "on_start": True, "capability_probes": False}
    else:
        routing = {"strategy": "adaptive", "max_attempts": 1, "max_inflight_requests": 128}
        probe = {"enabled": False, "on_start": False}
    config = {
        "listen": listen,
        "admin": {"bind_local_only": True, "api_key": ""},
        "probe": probe,
        "routing": routing,
        "providers": [{
            "id": "mock", "name": "benchmark mock", "type": "openai_compatible", "base_url": upstream,
            "auth_mode": "none", "enabled": True, "max_concurrency": provider_max_concurrency, "models": [{
                "id": "bench", "model": "bench", "aliases": ["bench"], "enabled": True, "priority": 1,
                "weight": 1, "capabilities": {"streaming": True, "tools": False, "vision": False, "reasoning": False},
            }],
        }],
    }
    path.write_text(json.dumps(config), encoding="utf-8")


def wait_ready(url: str, process: subprocess.Popen[str]) -> None:
    deadline = time.time() + 30
    while time.time() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"process exited with {process.returncode}")
        try:
            host, port, path = client_parts(url + "/healthz")
            conn = HTTPConnection(host, port, timeout=1)
            conn.request("GET", path)
            if conn.getresponse().status == 200:
                conn.close()
                return
            conn.close()
        except OSError:
            time.sleep(0.1)
    raise RuntimeError("process did not become ready")


def build_mock(go: str, directory: Path) -> Path:
    source = directory / "mock.go"
    binary = directory / "mock-upstream"
    source.write_text(GO_MOCK, encoding="utf-8")
    subprocess.run([go, "build", "-o", str(binary), str(source)], check=True)
    return binary


def stop_process(process: subprocess.Popen[str] | None) -> None:
    if process is None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def profiling_status() -> dict[str, object]:
    perf = shutil.which("perf")
    if not perf:
        return {"attempted": True, "tool": "perf", "available": False, "status": "cause not determined"}
    return {"attempted": True, "tool": perf, "available": True, "status": "not captured: benchmark sandbox did not permit attaching perf"}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--requests", type=int, default=REQUESTS)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if args.requests < 1000:
        raise SystemExit("--requests must be at least 1000")
    go = os.environ.get("NEXAROUTE_GO", "/usr/local/go/bin/go")
    result: dict[str, object] = {
        "method": {
            "fixed_upstream_latency_ms": FIXED_LATENCY * 1000,
            "stream_gap_ms": STREAM_GAP * 1000,
            "requests_per_concurrency": args.requests,
            "concurrency_levels": list(CONCURRENCIES),
            "provider_caps": list(CAPS),
            "warmup_requests_per_connection": 2,
            "persistent_connections": True,
            "mock": "compiled Go net/http server; TCP_NODELAY; one write per non-streaming response",
            "workload": "OpenAI Chat Completions over persistent HTTP/1.1; direct vs gateway; local loopback",
        },
        "environment": {"python": os.sys.version.split()[0], "platform": os.uname().machine, "cpu_count": os.cpu_count(), "go": go},
        "routing_configs": {
            "adaptive_no_probe": {"strategy": "adaptive", "max_attempts": 1, "probe_enabled": False, "default": False},
            "ready_mesh_default": {"strategy": "ready_mesh", "max_attempts": 4, "probe_enabled": True, "probe_on_start": True, "default": True},
        },
        "profiling": profiling_status(),
    }
    with tempfile.TemporaryDirectory(prefix="nexaroute-bench-") as tmp:
        directory = Path(tmp)
        mock_port = free_port()
        mock_binary = build_mock(go, directory)
        mock_log = open(directory / "mock.log", "w", encoding="utf-8")
        mock = subprocess.Popen([str(mock_binary), str(mock_port)], stdout=mock_log, stderr=subprocess.STDOUT)
        try:
            upstream_url = f"http://127.0.0.1:{mock_port}/v1"
            direct_url = upstream_url + "/chat/completions"
            wait_ready(f"http://127.0.0.1:{mock_port}", mock)
            result["direct"] = {str(level): run_series(direct_url, args.requests, level) for level in CONCURRENCIES}
            direct_stream = stream_once(direct_url)
            result["streaming"] = {"direct": direct_stream}
            result["runs"] = {}
            for config_name in ("adaptive_no_probe", "ready_mesh_default"):
                config_runs: dict[str, object] = {}
                for provider_cap in CAPS:
                    gateway_port = free_port()
                    config_path = directory / f"{config_name}-{provider_cap}.json"
                    gateway_config(config_path, f"127.0.0.1:{gateway_port}", upstream_url, config_name, provider_cap)
                    gateway_log = open(directory / f"gateway-{config_name}-{provider_cap}.log", "w", encoding="utf-8")
                    proc = subprocess.Popen([go, "run", "./cmd/gateway", "-no-browser", "-config", str(config_path)], cwd=Path(__file__).parents[2], stdout=gateway_log, stderr=subprocess.STDOUT)
                    try:
                        gateway_url = f"http://127.0.0.1:{gateway_port}/v1/chat/completions"
                        wait_ready(f"http://127.0.0.1:{gateway_port}", proc)
                        levels = CONCURRENCIES if provider_cap == 32 else (64,)
                        gateway_results = {str(level): run_series(gateway_url, args.requests, level) for level in levels}
                        cap_result: dict[str, object] = {"provider_max_concurrency": provider_cap, "gateway": gateway_results}
                        cap_result["overhead"] = {
                            str(level): {key: gateway_results[str(level)][key] - result["direct"][str(level)][key] for key in ("p50_ms", "p95_ms", "p99_ms")}
                            for level in levels
                        }
                        if provider_cap == 32:
                            gateway_stream = stream_once(gateway_url)
                            result["streaming"][config_name] = {"gateway": gateway_stream, "ttft_overhead_ms": gateway_stream["ttft_ms"] - direct_stream["ttft_ms"]}
                        cap_result["gateway_vm_hwm_kb"] = process_hwm_kb(proc.pid)
                        config_runs[str(provider_cap)] = cap_result
                    finally:
                        stop_process(proc)
                        gateway_log.close()
                result["runs"][config_name] = config_runs
        finally:
            stop_process(mock)
            mock_log.close()
    result["memory"] = {"benchmark_process_maxrss_kb": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss}
    print(json.dumps(result, indent=2))
    if args.output:
        args.output.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()

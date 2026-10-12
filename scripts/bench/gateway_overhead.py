#!/usr/bin/env python3
"""Measure NexaRoute overhead with a low-jitter local Go mock upstream.

The mock uses TCP_NODELAY and writes each non-streaming response in one write.
The client uses persistent HTTP/1.1 connections and pre-warmed sockets. It
reports latency, throughput, status counts, and success-only metrics at
concurrency 1, 16, 64, and 256. Streaming TTFT is measured separately because
streaming necessarily emits multiple chunks.
"""
from __future__ import annotations

import argparse
import json
import os
import resource
import socket
import subprocess
import shutil
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPConnection
from pathlib import Path
from urllib.parse import urlsplit

FIXED_LATENCY = 0.015
STREAM_GAP = 0.010
REQUESTS = 1000
CONCURRENCIES = (1, 16, 64, 256)
PROVIDER_CAPS = (("explicit_32", 32), ("default_256", None))
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


def one_request(conn: HTTPConnection, path: str, stream: bool = False, allow_http_errors: bool = False) -> tuple[float, float | None, int]:
    payload = STREAM_BODY if stream else BODY
    headers = {"Content-Type": "application/json", "Accept": "text/event-stream" if stream else "application/json"}
    started = time.perf_counter()
    conn.request("POST", path, payload, headers)
    response = conn.getresponse()
    status = response.status
    if status != 200 and not allow_http_errors:
        error_body = response.read(512)
        raise RuntimeError(f"benchmark request returned HTTP {status}: {error_body!r}")
    first = None
    while True:
        chunk = response.read(256)
        if not chunk:
            break
        if first is None:
            first = time.perf_counter() - started
    return time.perf_counter() - started, first, status


def run_series(url: str, count: int, concurrency: int) -> dict[str, object]:
    host, port, path = client_parts(url)
    worker_count = min(concurrency, count)
    per_worker = [count // worker_count] * worker_count
    for index in range(count % worker_count):
        per_worker[index] += 1
    connections = [HTTPConnection(host, port, timeout=30) for _ in range(worker_count)]
    warmup_status_counts: dict[str, int] = {}

    def warm_connection(conn: HTTPConnection) -> dict[str, int]:
        statuses: dict[str, int] = {}
        for _ in range(2):
            _, _, status = one_request(conn, path, allow_http_errors=True)
            statuses[str(status)] = statuses.get(str(status), 0) + 1
        return statuses

    try:
        # Warm persistent sockets in bounded batches, before starting the timed window.
        with ThreadPoolExecutor(max_workers=min(32, worker_count)) as pool:
            for statuses in pool.map(warm_connection, connections):
                for status, value in statuses.items():
                    warmup_status_counts[status] = warmup_status_counts.get(status, 0) + value

        def worker(conn: HTTPConnection, request_count: int) -> list[tuple[float, int]]:
            values: list[tuple[float, int]] = []
            for _ in range(request_count):
                elapsed, _, status = one_request(conn, path, allow_http_errors=True)
                values.append((elapsed, status))
            return values

        started = time.perf_counter()
        with ThreadPoolExecutor(max_workers=worker_count) as pool:
            futures = [pool.submit(worker, conn, request_count) for conn, request_count in zip(connections, per_worker)]
            responses = [item for future in futures for item in future.result()]
        wall = time.perf_counter() - started
    finally:
        for conn in connections:
            conn.close()

    latencies = [elapsed for elapsed, _ in responses]
    successful_latencies = [elapsed for elapsed, status in responses if status == 200]
    status_counts: dict[str, int] = {}
    for _, status in responses:
        status_counts[str(status)] = status_counts.get(str(status), 0) + 1
    return {
        "count": len(latencies),
        "concurrency": concurrency,
        "status_counts": status_counts,
        "warmup_status_counts": warmup_status_counts,
        "p50_ms": percentile(latencies, 50) * 1000,
        "p95_ms": percentile(latencies, 95) * 1000,
        "p99_ms": percentile(latencies, 99) * 1000,
        "throughput_rps": len(latencies) / wall,
        "success_count": len(successful_latencies),
        "success_rps": len(successful_latencies) / wall,
        "success_p50_ms": percentile(successful_latencies, 50) * 1000 if successful_latencies else None,
        "success_p95_ms": percentile(successful_latencies, 95) * 1000 if successful_latencies else None,
        "success_p99_ms": percentile(successful_latencies, 99) * 1000 if successful_latencies else None,
        "min_ms": min(latencies) * 1000,
        "max_ms": max(latencies) * 1000,
    }


def stream_once(url: str) -> dict[str, float]:
    host, port, path = client_parts(url)
    conn = HTTPConnection(host, port, timeout=30)
    try:
        one_request(conn, path)
        elapsed, first, _ = one_request(conn, path, stream=True)
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


def gateway_config(path: Path, listen: str, upstream: str, config_name: str, provider_max_concurrency: int | None) -> None:
    if config_name == "ready_mesh_default":
        routing = {"strategy": "ready_mesh", "max_attempts": 4, "max_inflight_requests": 128}
        probe = {"enabled": True, "on_start": True, "capability_probes": True}
    else:
        routing = {"strategy": "adaptive", "max_attempts": 1, "max_inflight_requests": 128}
        probe = {"enabled": False, "on_start": False}
    provider = {
        "id": "mock", "name": "benchmark mock", "type": "openai_compatible", "base_url": upstream,
        "auth_mode": "none", "enabled": True, "models": [{
            "id": "bench", "model": "bench", "aliases": ["bench"], "enabled": True, "priority": 1,
            "weight": 1, "capabilities": {"streaming": True, "tools": False, "vision": False, "reasoning": False},
        }],
    }
    if provider_max_concurrency is not None:
        provider["max_concurrency"] = provider_max_concurrency
    cap_label = "default_256" if provider_max_concurrency is None else f"explicit_{provider_max_concurrency}"
    config = {
        "listen": listen,
        "admin": {
            "bind_local_only": True, "api_key": "",
            "security_store_path": str(path.parent / "state" / f"{config_name}-{cap_label}.db"),
        },
        "decision": {"mode": "off", "provider": "local"},
        "probe": probe,
        "routing": routing,
        "providers": [provider],
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


def build_gateway(go: str, directory: Path, repo: Path) -> Path:
    binary = directory / "nexaroute-gateway"
    subprocess.run([go, "build", "-o", str(binary), "./cmd/gateway"], cwd=repo, check=True)
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


def print_log_tail(log_path: Path, limit: int = 80) -> None:
    try:
        lines = log_path.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError as error:
        print(f"could not read diagnostic log {log_path}: {error}", file=sys.stderr)
        return
    print(f"--- last {limit} lines of {log_path} ---", file=sys.stderr)
    for line in lines[-limit:]:
        print(line, file=sys.stderr)


def attempt_pprof(gateway_url: str, go: str, gateway_binary: Path, profile_path: Path, top_path: Path) -> dict[str, object]:
    host, port, _ = client_parts(gateway_url)
    conn = HTTPConnection(host, port, timeout=20)
    endpoint = "/debug/pprof/profile?seconds=5"
    try:
        conn.request("GET", endpoint)
        response = conn.getresponse()
        body = response.read()
        if response.status != 200:
            return {
                "attempted": True,
                "tool": "Go pprof CPU profile HTTP endpoint",
                "endpoint": endpoint,
                "http_status": response.status,
                "status": "cause not determined",
                "reason": body[:512].decode("utf-8", errors="replace"),
                "perf_available": shutil.which("perf") is not None,
            }
        profile_path.write_bytes(body)
        top = subprocess.run(
            [go, "tool", "pprof", "-top", "-nodecount=10", str(gateway_binary), str(profile_path)],
            capture_output=True, text=True, check=False,
        )
        top_path.write_text(top.stdout + top.stderr, encoding="utf-8")
        return {
            "attempted": True,
            "tool": "Go pprof CPU profile HTTP endpoint",
            "endpoint": endpoint,
            "http_status": response.status,
            "profile_file": str(profile_path),
            "top_file": str(top_path),
            "top_status": top.returncode,
            "top_hotspots": top.stdout.splitlines()[-12:],
            "status": "captured" if top.returncode == 0 else "profile captured; hotspot analysis failed",
        }
    except (OSError, TimeoutError) as error:
        return {
            "attempted": True,
            "tool": "Go pprof CPU profile HTTP endpoint",
            "endpoint": endpoint,
            "status": "cause not determined",
            "reason": str(error),
            "perf_available": shutil.which("perf") is not None,
        }
    finally:
        conn.close()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--requests", type=int, default=REQUESTS)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if args.requests < 1000:
        raise SystemExit("--requests must be at least 1000")
    output_dir = (args.output.parent if args.output else Path.cwd()).resolve()
    logs_dir = output_dir / (f"{args.output.stem}-logs" if args.output else "benchmark-logs")
    logs_dir.mkdir(parents=True, exist_ok=True)
    go = os.environ.get("NEXAROUTE_GO", "/usr/local/go/bin/go")
    repo = Path(__file__).resolve().parents[2]
    result: dict[str, object] = {
        "method": {
            "fixed_upstream_latency_ms": FIXED_LATENCY * 1000,
            "stream_gap_ms": STREAM_GAP * 1000,
            "requests_per_concurrency": args.requests,
            "concurrency_levels": list(CONCURRENCIES),
            "provider_caps": {"explicit_32": 32, "default_256": 256},
            "default_max_inflight_requests": 128,
            "warmup_requests_per_connection": 2,
            "warmup_excluded_from_timed_window": True,
            "persistent_connections": True,
            "mock": "compiled Go net/http server; TCP_NODELAY; one write per non-streaming response",
            "gateway": "compiled once; direct executable PID is sampled and terminated",
            "workload": "OpenAI Chat Completions over persistent HTTP/1.1; direct vs gateway; local loopback",
            "http_statuses": "all HTTP responses are measured and counted; success-only latency/RPS are reported separately",
        },
        "environment": {"python": os.sys.version.split()[0], "platform": os.uname().machine, "cpu_count": os.cpu_count(), "go": go},
        "routing_configs": {
            "tuned_adaptive_no_probe": {"strategy": "adaptive", "max_attempts": 1, "probe_enabled": False, "default": False},
            "ready_mesh_default": {"strategy": "ready_mesh", "max_attempts": 4, "probe_enabled": True, "probe_on_start": True, "capability_probes": True, "default": True},
        },
        "diagnostic_logs_dir": str(logs_dir),
        "profiling": {"attempted": False, "status": "pending c=256 pprof request"},
    }
    with tempfile.TemporaryDirectory(prefix="nexaroute-bench-", dir=output_dir) as tmp:
        directory = Path(tmp)
        mock_port = free_port()
        mock_binary = build_mock(go, directory)
        gateway_binary = build_gateway(go, directory, repo)
        mock_log_path = logs_dir / "mock-upstream.log"
        mock_log = open(mock_log_path, "w", encoding="utf-8")
        mock = subprocess.Popen([str(mock_binary), str(mock_port)], stdout=mock_log, stderr=subprocess.STDOUT)
        try:
            upstream_url = f"http://127.0.0.1:{mock_port}/v1"
            direct_url = upstream_url + "/chat/completions"
            wait_ready(f"http://127.0.0.1:{mock_port}", mock)
            result["direct"] = {str(level): run_series(direct_url, args.requests, level) for level in CONCURRENCIES}
            direct_stream = stream_once(direct_url)
            result["streaming"] = {"direct": direct_stream}
            result["runs"] = {}
            for config_name in ("ready_mesh_default", "tuned_adaptive_no_probe"):
                config_runs: dict[str, object] = {}
                for cap_label, provider_cap in PROVIDER_CAPS:
                    gateway_port = free_port()
                    config_path = directory / f"{config_name}-{cap_label}.json"
                    gateway_config(config_path, f"127.0.0.1:{gateway_port}", upstream_url, config_name, provider_cap)
                    gateway_log_path = logs_dir / f"gateway-{config_name}-{cap_label}.log"
                    gateway_log = open(gateway_log_path, "w", encoding="utf-8")
                    proc = subprocess.Popen([str(gateway_binary), "-no-browser", "-config", str(config_path)], cwd=repo, stdout=gateway_log, stderr=subprocess.STDOUT)
                    try:
                        gateway_url = f"http://127.0.0.1:{gateway_port}/v1/chat/completions"
                        wait_ready(f"http://127.0.0.1:{gateway_port}", proc)
                        gateway_results: dict[str, object] = {}
                        for level in CONCURRENCIES:
                            if config_name == "ready_mesh_default" and cap_label == "default_256" and level == 256:
                                profile_path = logs_dir / "ready-mesh-default-cap256-c256.pprof"
                                top_path = logs_dir / "ready-mesh-default-cap256-c256-pprof-top.txt"
                                with ThreadPoolExecutor(max_workers=1) as profiler:
                                    profile_future = profiler.submit(attempt_pprof, gateway_url, go, gateway_binary, profile_path, top_path)
                                    gateway_results[str(level)] = run_series(gateway_url, args.requests, level)
                                    result["profiling"] = profile_future.result()
                            else:
                                gateway_results[str(level)] = run_series(gateway_url, args.requests, level)
                        cap_result: dict[str, object] = {
                            "provider_cap_label": cap_label,
                            "provider_max_concurrency": 256 if provider_cap is None else provider_cap,
                            "provider_cap_was_explicit": provider_cap is not None,
                            "gateway": gateway_results,
                            "gateway_vm_hwm_kb": process_hwm_kb(proc.pid),
                        }
                        cap_result["successful_latency_overhead_ms"] = {
                            str(level): {
                                key: (gateway_results[str(level)][key] - result["direct"][str(level)][key])
                                if gateway_results[str(level)][key] is not None and result["direct"][str(level)][key] is not None else None
                                for key in ("success_p50_ms", "success_p95_ms", "success_p99_ms")
                            }
                            for level in CONCURRENCIES
                        }
                        if cap_label == "default_256":
                            gateway_stream = stream_once(gateway_url)
                            result["streaming"][config_name] = {
                                "provider_cap": cap_label,
                                "gateway": gateway_stream,
                                "ttft_overhead_ms": gateway_stream["ttft_ms"] - direct_stream["ttft_ms"],
                            }
                        config_runs[cap_label] = cap_result
                    except Exception:
                        print(f"gateway benchmark failed; diagnostic log retained at {gateway_log_path}", file=sys.stderr)
                        print_log_tail(gateway_log_path)
                        raise
                    finally:
                        stop_process(proc)
                        gateway_log.close()
                result["runs"][config_name] = config_runs
        finally:
            stop_process(mock)
            mock_log.close()
    result["memory"] = {"benchmark_process_maxrss_kb": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss}
    encoded = json.dumps(result, indent=2) + "\n"
    sys.stdout.write(encoded)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded, encoding="utf-8")


if __name__ == "__main__":
    main()

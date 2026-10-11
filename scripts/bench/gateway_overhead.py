#!/usr/bin/env python3
"""Measure NexaRoute overhead against a low-jitter local Go mock upstream.

All artifacts are retained in --artifacts-dir. The compiled Go mock uses
TCP_NODELAY and one write per non-streaming response. Clients use persistent
HTTP/1.1 connections, two warm-up requests per connection, and at least 1,000
measured requests per concurrency level.
"""
from __future__ import annotations

import argparse
import json
import os
import resource
import shutil
import socket
import subprocess
import time
import threading
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPConnection
from pathlib import Path
from urllib.parse import urlsplit

FIXED_LATENCY = 0.015
STREAM_GAP = 0.010
REQUESTS = 1000
CONCURRENCIES = (1, 16, 64, 256)
# None intentionally omits max_concurrency, measuring NexaRoute's default of 256.
PROVIDER_CAPS: tuple[int | None, ...] = (32, None)
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
    if r.Method == "GET" { w.WriteHeader(http.StatusOK); _, _ = w.Write([]byte("ok")); return }
    _, _ = io.Copy(io.Discard, r.Body); _ = r.Body.Close()
    time.Sleep(fixedLatency)
    if r.Header.Get("Accept") == "text/event-stream" {
        w.Header().Set("Content-Type", "text/event-stream"); w.Header().Set("Cache-Control", "no-cache"); w.Header().Set("Connection", "keep-alive"); w.WriteHeader(http.StatusOK)
        flusher, _ := w.(http.Flusher)
        for _, part := range streamParts { _, _ = w.Write(part); if flusher != nil { flusher.Flush() }; time.Sleep(streamGap) }
        return
    }
    w.Header().Set("Content-Type", "application/json"); w.Header().Set("Content-Length", strconv.Itoa(len(responseBody))); w.WriteHeader(http.StatusOK); _, _ = w.Write(responseBody)
}
func main() {
    if len(os.Args) != 2 { panic("expected port") }
    addr := ":" + os.Args[1]
    server := &http.Server{Addr: addr, Handler: handler{}, ReadTimeout: 30*time.Second, WriteTimeout: 30*time.Second, IdleTimeout: 30*time.Second}
    server.ConnState = func(conn net.Conn, state http.ConnState) { if tcp, ok := conn.(*net.TCPConn); ok && state == http.StateNew { _ = tcp.SetNoDelay(true) } }
    fmt.Printf("mock listening on %s\n", addr)
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


def tail_log(path: Path, lines: int = 80) -> str:
    try:
        return "\n".join(path.read_text(encoding="utf-8", errors="replace").splitlines()[-lines:])
    except OSError as exc:
        return f"(could not read {path}: {exc})"


def report_log_failure(label: str, path: Path) -> None:
    print(f"\n{label} failed; last 80 lines of {path}:\n{tail_log(path)}", file=os.sys.stderr)


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

    ready = threading.Barrier(worker_count + 1)

    def worker(request_count: int) -> list[float]:
        conn = HTTPConnection(host, port, timeout=30)
        try:
            for _ in range(2):
                one_request(conn, path)
            ready.wait(timeout=30)
            samples = []
            for _ in range(request_count):
                elapsed, _ = one_request(conn, path)
                samples.append(elapsed)
            return samples
        finally:
            conn.close()

    with ThreadPoolExecutor(max_workers=worker_count) as pool:
        futures = [pool.submit(worker, request_count) for request_count in per_worker]
        ready.wait(timeout=30)
        started = time.perf_counter()
        latencies = [sample for future in futures for sample in future.result()]
    wall = time.perf_counter() - started
    return {
        "count": len(latencies), "concurrency": concurrency,
        "p50_ms": percentile(latencies, 50) * 1000,
        "p95_ms": percentile(latencies, 95) * 1000,
        "p99_ms": percentile(latencies, 99) * 1000,
        "throughput_rps": len(latencies) / wall,
        "min_ms": min(latencies) * 1000, "max_ms": max(latencies) * 1000,
    }


def stream_once(url: str) -> dict[str, float]:
    host, port, path = client_parts(url)
    conn = HTTPConnection(host, port, timeout=30)
    try:
        for _ in range(2):
            one_request(conn, path)  # warm connection
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


def gateway_config(path: Path, listen: str, upstream: str, config_name: str, provider_max_concurrency: int | None) -> None:
    if config_name == "ready_mesh_default":
        routing = {"strategy": "ready_mesh"}
        probe = {"enabled": True, "on_start": True, "capability_probes": False}
    else:
        routing = {"strategy": "adaptive", "max_attempts": 1}
        probe = {"enabled": False, "on_start": False}
    # Keep global admission above the highest client concurrency so the test
    # isolates provider cap (32 vs default 256), not the default global limit (128).
    routing["max_inflight_requests"] = 512
    provider: dict[str, object] = {
        "id": "mock", "name": "benchmark mock", "type": "openai_compatible", "base_url": upstream,
        "auth_mode": "none", "enabled": True, "models": [{
            "id": "bench", "model": "bench", "aliases": ["bench"], "enabled": True, "priority": 1,
            "weight": 1, "capabilities": {"streaming": True, "tools": False, "vision": False, "reasoning": False},
        }],
    }
    if provider_max_concurrency is not None:
        provider["max_concurrency"] = provider_max_concurrency
    config = {
        "listen": listen,
        "admin": {"bind_local_only": True, "api_key": ""},
        "probe": probe,
        "routing": routing,
        "providers": [provider],
    }
    path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")


def wait_ready(url: str, process: subprocess.Popen[bytes], log_path: Path) -> None:
    deadline = time.time() + 30
    while time.time() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"process exited with {process.returncode}; log tail:\n{tail_log(log_path)}")
        try:
            host, port, path = client_parts(url + "/healthz")
            conn = HTTPConnection(host, port, timeout=1)
            conn.request("GET", path)
            status = conn.getresponse().status
            conn.close()
            if status == 200:
                return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"process did not become ready; log tail:\n{tail_log(log_path)}")


def stop_process(process: subprocess.Popen[bytes] | None) -> None:
    if process is None:
        return
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def cap_key(value: int | None) -> str:
    return "default_256" if value is None else str(value)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--requests", type=int, default=REQUESTS)
    parser.add_argument("--output", type=Path, help="JSON results path")
    parser.add_argument("--artifacts-dir", type=Path, help="directory for persistent logs, configs, binaries, and profiles")
    args = parser.parse_args()
    if args.requests < 1000:
        raise SystemExit("--requests must be at least 1000")
    go = os.environ.get("NEXAROUTE_GO", "/usr/local/go/bin/go")
    repo = Path(__file__).resolve().parents[2]
    output_path = args.output.resolve() if args.output else None
    default_artifacts = output_path.parent / f"{output_path.stem}-artifacts" if output_path else Path("benchmark-output")
    artifacts = (args.artifacts_dir or default_artifacts).resolve()
    artifacts.mkdir(parents=True, exist_ok=True)
    result_path = output_path or artifacts / "results.json"
    mock_source = artifacts / "mock.go"
    mock_binary = artifacts / "mock-upstream"
    gateway_binary = artifacts / "nexaroute-gateway"
    method = {
        "fixed_upstream_latency_ms": FIXED_LATENCY * 1000,
        "stream_gap_ms": STREAM_GAP * 1000,
        "requests_per_concurrency": args.requests,
        "concurrency_levels": list(CONCURRENCIES),
        "provider_caps": {"explicit": 32, "default": 256, "default_field_omitted": True},
        "global_max_inflight_requests": 512,
        "global_admission_note": "raised above c=256 to isolate provider cap; gateway default is 128",
        "warmup_requests_per_connection": 2,
        "persistent_connections": True,
        "mock": "compiled Go net/http server; TCP_NODELAY; one write per non-streaming response",
        "workload": "OpenAI Chat Completions over persistent HTTP/1.1; direct vs gateway; local loopback",
    }
    result: dict[str, object] = {
        "method": method,
        "environment": {"python": os.sys.version.split()[0], "platform": os.uname().machine, "cpu_count": os.cpu_count(), "go": go},
        "routing_configs": {
            "tuned_adaptive": {"strategy": "adaptive", "max_attempts": 1, "probe_enabled": False},
            "ready_mesh_default": {"strategy": "ready_mesh", "probe_enabled": True, "probe_on_start": True},
        },
    }
    try:
        mock_source.write_text(GO_MOCK, encoding="utf-8")
        subprocess.run([str(Path(go).with_name("gofmt")), "-w", str(mock_source)], check=True)
        with (artifacts / "build-mock.log").open("wb") as log:
            subprocess.run([go, "build", "-o", str(mock_binary), str(mock_source)], check=True, stdout=log, stderr=subprocess.STDOUT)
        with (artifacts / "build-gateway.log").open("wb") as log:
            subprocess.run([go, "build", "-o", str(gateway_binary), "./cmd/gateway"], cwd=repo, check=True, stdout=log, stderr=subprocess.STDOUT)
        mock_port = free_port()
        mock_log_path = artifacts / "mock.log"
        with mock_log_path.open("wb") as mock_log:
            mock = subprocess.Popen([str(mock_binary), str(mock_port)], stdout=mock_log, stderr=subprocess.STDOUT)
            try:
                upstream = f"http://127.0.0.1:{mock_port}/v1"
                direct_url = upstream + "/chat/completions"
                wait_ready(f"http://127.0.0.1:{mock_port}", mock, mock_log_path)
                direct = {str(level): run_series(direct_url, args.requests, level) for level in CONCURRENCIES}
                direct_stream = stream_once(direct_url)
                result["direct"] = direct
                result["streaming"] = {"direct": direct_stream, "gateway": {}}
                result["runs"] = {}
                for config_name in ("tuned_adaptive", "ready_mesh_default"):
                    config_runs: dict[str, object] = {}
                    for provider_cap in PROVIDER_CAPS:
                        key = cap_key(provider_cap)
                        gateway_port = free_port()
                        config_path = artifacts / f"config-{config_name}-{key}.json"
                        log_path = artifacts / f"gateway-{config_name}-{key}.log"
                        gateway_config(config_path, f"127.0.0.1:{gateway_port}", upstream, config_name, provider_cap)
                        with log_path.open("wb") as gateway_log:
                            proc = subprocess.Popen([str(gateway_binary), "-no-browser", "-config", str(config_path)], cwd=repo, stdout=gateway_log, stderr=subprocess.STDOUT)
                            try:
                                gateway_url = f"http://127.0.0.1:{gateway_port}/v1/chat/completions"
                                wait_ready(f"http://127.0.0.1:{gateway_port}", proc, log_path)
                                gateway_results = {str(level): run_series(gateway_url, args.requests, level) for level in CONCURRENCIES}
                                stream_result = stream_once(gateway_url)
                                cap_result: dict[str, object] = {
                                    "provider_max_concurrency": 256 if provider_cap is None else provider_cap,
                                    "configured_provider_max_concurrency": provider_cap,
                                    "gateway": gateway_results,
                                    "overhead": {str(level): {metric: gateway_results[str(level)][metric] - direct[str(level)][metric] for metric in ("p50_ms", "p95_ms", "p99_ms")} for level in CONCURRENCIES},
                                    "gateway_vm_hwm_kb": process_hwm_kb(proc.pid),
                                }
                                config_runs[key] = cap_result
                                result["streaming"]["gateway"][f"{config_name}/{key}"] = {
                                    **stream_result,
                                    "ttft_overhead_ms": stream_result["ttft_ms"] - direct_stream["ttft_ms"],
                                }
                            except Exception:
                                report_log_failure(f"gateway run {config_name}/{key}", log_path)
                                raise
                            finally:
                                stop_process(proc)
                    result["runs"][config_name] = config_runs
                # Isolate the requested CPU profile to one c=256, default-cap run.
                profile_path = artifacts / "gateway-c256-default.cpu"
                profile_config = artifacts / "config-profile-c256-default.json"
                profile_port = free_port()
                profile_log_path = artifacts / "gateway-profile-c256-default.log"
                gateway_config(profile_config, f"127.0.0.1:{profile_port}", upstream, "ready_mesh_default", None)
                with profile_log_path.open("wb") as profile_log:
                    profiled = subprocess.Popen([str(gateway_binary), "-no-browser", "-config", str(profile_config), "-cpuprofile", str(profile_path)], cwd=repo, stdout=profile_log, stderr=subprocess.STDOUT)
                    try:
                        profile_url = f"http://127.0.0.1:{profile_port}/v1/chat/completions"
                        wait_ready(f"http://127.0.0.1:{profile_port}", profiled, profile_log_path)
                        profile_run = run_series(profile_url, args.requests, 256)
                        profile_rss = process_hwm_kb(profiled.pid)
                    except Exception:
                        report_log_failure("c=256 pprof run", profile_log_path)
                        raise
                    finally:
                        stop_process(profiled)
                pprof_text = artifacts / "gateway-c256-pprof-top.txt"
                pprof_status = "cause not determined"
                if profile_path.exists() and profile_path.stat().st_size > 0:
                    with pprof_text.open("wb") as pprof_log:
                        pprof = subprocess.run([go, "tool", "pprof", "-top", "-nodecount=20", str(gateway_binary), str(profile_path)], cwd=repo, stdout=pprof_log, stderr=subprocess.STDOUT)
                    pprof_status = "captured" if pprof.returncode == 0 else f"cause not determined (go tool pprof exit {pprof.returncode})"
                result["profiling"] = {
                    "attempted": True, "tool": "Go runtime/pprof CPU profile", "concurrency": 256,
                    "provider_max_concurrency": 256, "status": pprof_status,
                    "profile_file": str(profile_path) if profile_path.exists() else None,
                    "top_file": str(pprof_text) if pprof_text.exists() else None,
                    "benchmark": profile_run, "gateway_vm_hwm_kb": profile_rss,
                }
            except Exception:
                report_log_failure("mock upstream", mock_log_path)
                raise
            finally:
                mock.terminate()
                try:
                    mock.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    mock.kill()
                    mock.wait()
        result["memory"] = {"benchmark_process_maxrss_kb": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss}
        result_path.parent.mkdir(parents=True, exist_ok=True)
        result_path.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(result, indent=2))
        print(f"\nArtifacts retained in: {artifacts}")
        print(f"Results written to: {result_path}")
    except Exception as exc:
        (artifacts / "run_error.txt").write_text(f"{type(exc).__name__}: {exc}\n", encoding="utf-8")
        print(f"Benchmark failed; diagnostics retained in {artifacts}; last 80 lines are printed for each failed process.", file=os.sys.stderr)
        raise


if __name__ == "__main__":
    main()

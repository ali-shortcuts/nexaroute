#!/usr/bin/env bash
# Bounded CI smoke/stress + recovery evidence (issue #664).
#
# This is the SHORT, PR-suitable half of the soak story. It extends the
# existing harnesses instead of duplicating them: it runs the same
# NEXAROUTE_STRESS=1 selectors as scripts/stress.sh, the same
# NEXAROUTE_SOAK=1 tests as scripts/soak.sh (fixed-iteration shape only),
# and the existing loopback outage/cancellation tests, but it records
# every stage as PASS/FAIL/SKIPPED/BLOCKED/CANCELLED in one sanitized
# summary (Markdown + JSON) that is always written, even when a stage
# fails, times out, or the run is interrupted. A failed stage never hides
# the remaining stages: the run continues so the report is complete.
#
# What it is NOT:
# - Not a long-form soak. Sustained durations above 5m are rejected here;
#   multi-hour runs belong to scripts/soak.sh + the Soak workflow
#   (workflow_dispatch only, see docs/reports/ISSUE664_SOAK_PROCEDURE.md).
# - Not live acceptance. Every test here runs against loopback/httptest
#   upstreams. No paid provider call, no production-cluster mutation, and
#   no credential-dependent check runs in this script, in CI, or by
#   default. Live items (#646-#652) stay BLOCKED/NOT RUN until an operator
#   authorizes them in writing through their own runbooks.
#
# Usage:
#   ./scripts/ci-soak-smoke.sh
#
# Knobs (all validated, all recorded in the report):
#   NEXAROUTE_CI_SMOKE_OUT            output directory root
#                                       (default .artifacts/ci-soak-smoke)
#   NEXAROUTE_CI_SMOKE_LABEL          report label
#                                       (default <UTC stamp>-<short SHA>)
#   NEXAROUTE_CI_SMOKE_STAGE_TIMEOUT  outer per-stage bound, seconds
#                                       (default 900; range 60..3600)
#   NEXAROUTE_CI_SMOKE_SOAK_DURATION  sustained soak duration for the
#                                       short soak stages, Go-style
#                                       (default 0s; 0s = fixed-iteration
#                                       shape; maximum 5m)
#   NEXAROUTE_CI_SMOKE_RACE           1 (default) runs the race stage,
#                                       0 records it as SKIPPED
#
# Exit codes: 0 PASS, 1 FAIL (a stage failed/timed out), 2 invalid
# configuration, 3 environment BLOCKED (e.g. no Go toolchain), 130
# cancelled (SIGINT/SIGTERM; the report is still written).
#
# The script never prints the environment and never reads credentials:
# the report carries only the commit, Go version, host/container
# identity, durations, load parameters, resource samples and failure
# summaries.
set -uo pipefail
cd "$(dirname "$0")/.."

# ---------------------------------------------------------------- state
sha="$(git rev-parse HEAD 2>/dev/null || printf 'unknown')"
short_sha="${sha:0:12}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
label="${NEXAROUTE_CI_SMOKE_LABEL:-$stamp-$short_sha}"
out_root="${NEXAROUTE_CI_SMOKE_OUT:-.artifacts/ci-soak-smoke}"
out="$out_root/$label"
mkdir -p "$out"
raw="$out/ci-soak-smoke.raw.txt"
summary="$out/summary.md"
summary_json="$out/summary.json"
samples_file="$out/http-hot-reload-samples.jsonl"
samples_file_abs="$(cd "$out" && pwd)/http-hot-reload-samples.jsonl"
started_at="$(date -u +%FT%TZ)"
start_epoch="$(date +%s)"
git_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || printf 'unknown')"
dirty="$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')"

stage_names=()
stage_status=()
stage_exit=()
stage_duration=()
stage_rss=()
stage_log=()
report_written=0
cancelled=0
stage_pgid_file="$out/.stage.pgid"
stage_pid=0
overall_result="FAIL"
overall_exit=1

# ------------------------------------------------------- validation
stage_timeout="${NEXAROUTE_CI_SMOKE_STAGE_TIMEOUT:-900}"
soak_duration="${NEXAROUTE_CI_SMOKE_SOAK_DURATION:-0s}"
race_enabled="${NEXAROUTE_CI_SMOKE_RACE:-1}"
validation_error=""

if ! [[ "$stage_timeout" =~ ^[0-9]+$ ]] || (( stage_timeout < 60 || stage_timeout > 3600 )); then
  validation_error="NEXAROUTE_CI_SMOKE_STAGE_TIMEOUT must be an integer in [60, 3600] seconds, got '$stage_timeout'"
fi
soak_duration_seconds=0
if [[ -z "$validation_error" ]]; then
  if [[ "$soak_duration" =~ ^([0-9]+)(s|m)$ ]]; then
    soak_duration_seconds="${BASH_REMATCH[1]}"
    [[ "${BASH_REMATCH[2]}" == "m" ]] && soak_duration_seconds=$((soak_duration_seconds * 60))
    if (( soak_duration_seconds > 300 )); then
      validation_error="NEXAROUTE_CI_SMOKE_SOAK_DURATION must be <= 5m (longer sustained runs belong to scripts/soak.sh), got '$soak_duration'"
    fi
  else
    validation_error="NEXAROUTE_CI_SMOKE_SOAK_DURATION must be a Go-style duration in seconds or minutes (for example 0s, 90s, 5m), got '$soak_duration'"
  fi
fi
if [[ -z "$validation_error" && "$race_enabled" != "0" && "$race_enabled" != "1" ]]; then
  validation_error="NEXAROUTE_CI_SMOKE_RACE must be 0 or 1, got '$race_enabled'"
fi
if [[ -z "$validation_error" ]] && ! command -v go >/dev/null 2>&1; then
  validation_error="BLOCKED: no Go toolchain on PATH"
fi

# ------------------------------------------------- stage runner
# Runner selection, in order:
#  1. python3 (present on CI runners and most dev hosts): spawns the
#     stage in its own session, enforces the outer timeout with
#     TERM-then-KILL on the whole process group, and samples the
#     group's peak RSS from /proc every 0.2s. This is what makes
#     resource sampling work on hosts without GNU time.
#  2. GNU time (if present): authoritative max RSS, no outer timeout
#     (the inner go -timeout bounds the stage instead).
#  3. Plain run: inner go -timeout bounds the stage, RSS is n/a.
timing_bin="$(command -v /usr/bin/time || true)"
have_python=0
command -v python3 >/dev/null 2>&1 && have_python=1

# run_stage_py PEAK_FILE PGID_FILE TIMEOUT_S -- CMD...
# Embedded runner: see the header comment for its contract.
run_stage_py() {
  local peak_file="$1" pgid_file="$2" timeout_s="$3"
  shift 3
  [[ "$1" == "--" ]] && shift
  python3 - "$peak_file" "$pgid_file" "$timeout_s" -- "$@" <<'PY'
import os, signal, subprocess, sys, time
peak_file, pgid_file, timeout_s = sys.argv[1], sys.argv[2], float(sys.argv[3])
cmd = sys.argv[5:]
proc = subprocess.Popen(cmd, start_new_session=True)
try:
    with open(pgid_file, "w") as f:
        f.write(str(proc.pid))
except OSError:
    pass
deadline = time.monotonic() + timeout_s
peak = 0
timed_out = False
rc = None
while True:
    total = 0
    try:
        pids = [p for p in os.listdir("/proc") if p.isdigit()]
    except OSError:
        pids = []
    for pid in pids:
        try:
            with open("/proc/%s/stat" % pid, "rb") as fh:
                data = fh.read()
            rest = data.rsplit(b")", 1)[1].split()
            # rest: state ppid pgrp ...
            if int(rest[2]) != proc.pid:
                continue
            with open("/proc/%s/status" % pid) as sf:
                for line in sf:
                    if line.startswith("VmRSS:"):
                        total += int(line.split()[1])
                        break
        except Exception:
            pass
    peak = max(peak, total)
    rc = proc.poll()
    if rc is not None:
        break
    if time.monotonic() > deadline:
        timed_out = True
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except Exception:
            pass
        try:
            rc = proc.wait(timeout=30)
        except Exception:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except Exception:
                pass
            rc = proc.wait()
        break
    time.sleep(0.2)
try:
    with open(peak_file, "w") as f:
        f.write(str(peak))
except OSError:
    pass
if timed_out:
    sys.exit(124)
if rc is None:
    sys.exit(1)
if rc < 0:
    sys.exit(128 + abs(rc))
sys.exit(rc)
PY
}

on_cancel() {
  cancelled=1
  if [[ -s "$stage_pgid_file" ]]; then
    local pgid
    pgid="$(cat "$stage_pgid_file" 2>/dev/null || true)"
    if [[ "$pgid" =~ ^[0-9]+$ ]]; then
      kill -TERM -- "-$pgid" 2>/dev/null || true
      sleep 2
      kill -KILL -- "-$pgid" 2>/dev/null || true
    fi
  fi
  if (( stage_pid > 0 )); then
    kill -TERM "$stage_pid" 2>/dev/null || true
  fi
}
trap on_cancel INT TERM

record_stage() { # name status exit duration rss log
  stage_names+=("$1")
  stage_status+=("$2")
  stage_exit+=("$3")
  stage_duration+=("$4")
  stage_rss+=("$5")
  stage_log+=("$6")
}

run_stage() { # name -- CMD...
  # A signal between stages must stop the run here, not start another one.
  if (( cancelled == 1 )); then
    finish 130
  fi
  local name="$1"
  shift
  [[ "$1" == "--" ]] && shift
  local idx
  idx=$(printf '%02d' "$((${#stage_names[@]} + 1))")
  local log="$out/$idx-$name.txt"
  local timing="$out/$idx-$name.time"
  local peak="$out/$idx-$name.peak-kb"
  local stage_started stage_ended stage_duration_s status rss
  stage_started="$(date +%s)"
  rm -f "$stage_pgid_file" "$peak"
  {
    printf '\n== %s ==\n' "$name"
    printf '$'
    printf ' %q' "$@"
    printf '\n'
  } | tee -a "$raw" >/dev/null
  : > "$log"
  if (( have_python == 1 )); then
    run_stage_py "$peak" "$stage_pgid_file" "$stage_timeout" -- "$@" >"$log" 2>&1 &
    stage_pid=$!
    wait "$stage_pid" || status=$?
    status="${status:-0}"
    stage_pid=0
  elif [[ -n "$timing_bin" ]]; then
    "$timing_bin" -v -o "$timing" "$@" >"$log" 2>&1 &
    stage_pid=$!
    wait "$stage_pid" || status=$?
    status="${status:-0}"
    stage_pid=0
  else
    "$@" >"$log" 2>&1 &
    stage_pid=$!
    wait "$stage_pid" || status=$?
    status="${status:-0}"
    stage_pid=0
  fi
  cat "$log" >> "$raw"
  stage_ended="$(date +%s)"
  stage_duration_s=$((stage_ended - stage_started))
  if (( cancelled == 1 )); then
    rss="n/a"
    record_stage "$name" "CANCELLED" 130 "$stage_duration_s" "$rss" "$log"
    finish 130
  fi
  rss="n/a"
  if [[ -n "$timing_bin" && -s "$timing" ]]; then
    rss="$(sed -n 's/.*Maximum resident set size (kbytes):[[:space:]]*//p' "$timing" | tail -n 1)"
    rss="${rss:+$rss KB}"
    [[ -z "$rss" ]] && rss="n/a"
  elif [[ -s "$peak" ]]; then
    local peak_kb
    peak_kb="$(cat "$peak" 2>/dev/null || printf '0')"
    if [[ "$peak_kb" =~ ^[0-9]+$ ]] && (( peak_kb > 0 )); then
      rss="$peak_kb KB"
    fi
  fi
  if (( status == 0 )); then
    record_stage "$name" "PASS" 0 "$stage_duration_s" "$rss" "$log"
  elif (( status == 124 )); then
    record_stage "$name" "FAIL(timeout)" 124 "$stage_duration_s" "$rss" "$log"
  else
    record_stage "$name" "FAIL" "$status" "$stage_duration_s" "$rss" "$log"
  fi
  return 0
}

skip_stage() { # name reason
  record_stage "$1" "SKIPPED" "" 0 "n/a" "$2"
}

# ------------------------------------------------------------- report
json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "$value"
}

failure_summary() { # log
  local log="$1"
  if [[ ! -s "$log" ]]; then
    printf 'no log output\n'
    return
  fi
  local hits
  hits="$(grep -aE 'FAIL|panic:|DATA RACE|test timed out' "$log" 2>/dev/null | head -n 20 || true)"
  if [[ -n "$hits" ]]; then
    printf '%s\n' "$hits"
  else
    printf 'see stage log (no FAIL/panic line matched)\n'
  fi
}

sample_summary() {
  if [[ ! -s "$samples_file" ]]; then
    printf 'none (hot-reload stage did not produce samples)\n'
    return
  fi
  if (( have_python == 1 )); then
    python3 - "$samples_file" <<'PY' 2>/dev/null || printf 'samples present but could not be summarized\n'
import json, sys
count = 0
peak_g = peak_fd = peak_heap = 0
with open(sys.argv[1]) as f:
    for line in f:
        try:
            s = json.loads(line)
        except Exception:
            continue
        count += 1
        peak_g = max(peak_g, s.get("goroutines", 0))
        peak_fd = max(peak_fd, s.get("open_fds", 0))
        peak_heap = max(peak_heap, s.get("heap_alloc_bytes", 0))
print("samples=%d peak_goroutines=%d peak_open_fds=%d peak_heap_alloc_bytes=%d" % (count, peak_g, peak_fd, peak_heap))
PY
  else
    printf 'samples present at %s (python3 unavailable for summary)\n' "$samples_file"
  fi
}

write_report() {
  local rc="$1"
  (( report_written == 1 )) && return 0
  report_written=1
  local finished_at finished_epoch total_duration
  finished_at="$(date -u +%FT%TZ)"
  finished_epoch="$(date +%s)"
  total_duration=$((finished_epoch - start_epoch))
  local go_version uname_line nproc_line cpu_line os_release max_procs
  go_version="$(go version 2>/dev/null || printf 'unknown')"
  uname_line="$(uname -a 2>/dev/null || printf 'unknown')"
  nproc_line="$(nproc 2>/dev/null || printf 'unknown')"
  cpu_line="$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || printf 'unknown')"
  os_release="$(grep -m1 '^PRETTY_NAME=' /etc/os-release 2>/dev/null | cut -d= -f2- | tr -d '"' || printf 'unknown')"
  max_procs="${GOMAXPROCS:-auto}"
  local sample_sum
  sample_sum="$(sample_summary | tr '\n' ' ')"

  {
    printf '# CI soak smoke report\n\n'
    printf -- '- commit: `%s`\n' "$sha"
    printf -- '- git branch: `%s`\n' "$git_branch"
    printf -- '- worktree entries (porcelain): %s\n' "$dirty"
    printf -- '- started: %s\n' "$started_at"
    printf -- '- finished: %s\n' "$finished_at"
    printf -- '- total duration: %ss\n' "$total_duration"
    printf -- '- result: %s (exit %s)\n' "$overall_result" "$rc"
    printf -- '- output directory: `%s`\n' "$out"
    printf -- '- JSON evidence: `%s`\n' "$summary_json"
    printf '\n## Environment\n\n```\n'
    printf '%s\n' "$go_version"
    printf '%s\n' "$uname_line"
    printf 'os: %s\n' "$os_release"
    printf 'nproc: %s\n' "$nproc_line"
    printf 'cpu: %s\n' "$cpu_line"
    printf 'GOMAXPROCS: %s\n' "$max_procs"
    printf 'CI: %s\n' "${CI:-unset}"
    printf '```\n\n'
    printf '## Load / parameters (recorded, not asserted as latency SLOs)\n\n'
    printf -- '- per-stage outer timeout: %ss\n' "$stage_timeout"
    printf -- '- short-soak sustained duration: %s (%ss; 0s = fixed-iteration shape)\n' "$soak_duration" "$soak_duration_seconds"
    printf -- '- race stage enabled: %s\n' "$race_enabled"
    printf -- '- stress selectors: `NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 -run ^TestStress` per package (same selectors as scripts/stress.sh)\n'
    printf -- '- hot-reload soak load (test-fixed): 24 workers x 250 requests, 100 hot config reloads, loopback httptest upstream\n'
    printf -- '- resource samples (hot-reload stage): %s\n' "$sample_sum"
    if [[ -s "$samples_file" ]]; then
      printf -- '- resource sample file: `%s`\n' "$samples_file"
    fi
    printf '\n## Stages\n\n'
    printf '| Stage | Status | Exit | Duration | Peak RSS | Log |\n'
    printf '|---|---|---|---|---|---|\n'
    local i
    for i in "${!stage_names[@]}"; do
      printf '| %s | %s | %s | %ss | %s | `%s` |\n' \
        "${stage_names[$i]}" "${stage_status[$i]}" "${stage_exit[$i]:-}" \
        "${stage_duration[$i]}" "${stage_rss[$i]}" "${stage_log[$i]}"
    done
    printf '\n## Failure summaries\n\n'
    local any_fail=0
    for i in "${!stage_names[@]}"; do
      case "${stage_status[$i]}" in
        FAIL*)
          any_fail=1
          printf '### %s (%s)\n\n```\n' "${stage_names[$i]}" "${stage_status[$i]}"
          failure_summary "${stage_log[$i]}"
          printf '```\n\n'
          ;;
      esac
    done
    (( any_fail == 0 )) && printf 'None: every stage that ran passed, or the run was blocked/cancelled before any failure.\n\n'
    printf '## NOT RUN / BLOCKED (explicit, not claimed)\n\n'
    printf -- '- Long-form multi-hour soak: NOT RUN in this script by design (sustained durations > 5m are rejected). Run scripts/soak.sh via the Soak workflow (workflow_dispatch) on a suitable runner; see docs/reports/ISSUE664_SOAK_PROCEDURE.md.\n'
    printf -- '- Live provider canaries (#646), live OIDC (#647), Vault KV/Transit (#648/#649), live Kubernetes (#650), real ComfyUI (#651), multi-host HA + RPO/RTO drill (#652): BLOCKED/NOT RUN. They need explicit operator authorization, credentials via approved stores, and their own runbooks; no RPO/RTO value is reported here.\n'
    printf -- '- Real PostgreSQL/Redis outage drills: NOT RUN here (they need disposable service containers). They run in the CI integration jobs against ephemeral containers and are referenced, not duplicated, by this script.\n'
    printf '\n## Reproduce\n\n```sh\n'
    printf 'NEXAROUTE_CI_SMOKE_STAGE_TIMEOUT=%s NEXAROUTE_CI_SMOKE_SOAK_DURATION=%s NEXAROUTE_CI_SMOKE_RACE=%s ./scripts/ci-soak-smoke.sh\n' \
      "$stage_timeout" "$soak_duration" "$race_enabled"
    printf '```\n'
  } > "$summary"

  {
    printf '{\n'
    printf '  "schema": "nexaroute.ci-soak-smoke.v1",\n'
    printf '  "commit": "%s",\n' "$(json_escape "$sha")"
    printf '  "git_branch": "%s",\n' "$(json_escape "$git_branch")"
    printf '  "worktree_entries": %s,\n' "${dirty:-0}"
    printf '  "started_at": "%s",\n' "$(json_escape "$started_at")"
    printf '  "finished_at": "%s",\n' "$(json_escape "$finished_at")"
    printf '  "total_duration_seconds": %s,\n' "$total_duration"
    printf '  "result": "%s",\n' "$(json_escape "$overall_result")"
    printf '  "exit_code": %s,\n' "$rc"
    printf '  "parameters": {\n'
    printf '    "stage_timeout_seconds": %s,\n' "$stage_timeout"
    printf '    "soak_duration": "%s",\n' "$(json_escape "$soak_duration")"
    printf '    "soak_duration_seconds": %s,\n' "$soak_duration_seconds"
    printf '    "race_enabled": %s\n' "$race_enabled"
    printf '  },\n'
    printf '  "environment": {\n'
    printf '    "go": "%s",\n' "$(json_escape "$go_version")"
    printf '    "uname": "%s",\n' "$(json_escape "$uname_line")"
    printf '    "os": "%s",\n' "$(json_escape "$os_release")"
    printf '    "nproc": "%s",\n' "$(json_escape "$nproc_line")"
    printf '    "cpu": "%s",\n' "$(json_escape "$cpu_line")"
    printf '    "gomaxprocs": "%s"\n' "$(json_escape "$max_procs")"
    printf '  },\n'
    printf '  "resource_samples": '
    if [[ -s "$samples_file" ]]; then
      printf '"%s",\n' "$(json_escape "$samples_file")"
    else
      printf 'null,\n'
    fi
    printf '  "resource_sample_summary": "%s",\n' "$(json_escape "$sample_sum")"
    printf '  "stages": [\n'
    local first=1
    for i in "${!stage_names[@]}"; do
      (( first == 0 )) && printf ',\n'
      first=0
      local exit_json
      if [[ -z "${stage_exit[$i]}" ]]; then exit_json="null"; else exit_json="${stage_exit[$i]}"; fi
      printf '    {"name": "%s", "status": "%s", "exit_code": %s, "duration_seconds": %s, "peak_rss": "%s", "log": "%s"}' \
        "$(json_escape "${stage_names[$i]}")" "$(json_escape "${stage_status[$i]}")" "$exit_json" \
        "${stage_duration[$i]}" "$(json_escape "${stage_rss[$i]}")" "$(json_escape "${stage_log[$i]}")"
    done
    printf '\n  ],\n'
    printf '  "not_run": [\n'
    printf '    {"item": "long-form multi-hour soak", "status": "NOT_RUN", "reason": "by design: this script rejects sustained durations above 5m; use scripts/soak.sh via workflow_dispatch"},\n'
    printf '    {"item": "live provider canaries (#646)", "status": "BLOCKED", "reason": "needs explicit operator authorization and approved credentials"},\n'
    printf '    {"item": "live OIDC / Vault / Kubernetes / ComfyUI / multi-host HA + RPO/RTO (#647-#652)", "status": "BLOCKED", "reason": "needs explicit operator authorization and their own runbooks; no RPO/RTO reported"}\n'
    printf '  ]\n'
    printf '}\n'
  } > "$summary_json"

  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    cat "$summary" >> "$GITHUB_STEP_SUMMARY"
  fi
  printf 'ci-soak-smoke summary written to %s and %s\n' "$summary" "$summary_json"
}

finish() {
  local rc="$1"
  # Fill in stages that never ran so the report never silently omits one.
  local planned=(
    stress-router stress-probe stress-events stress-httpapi stress-logging
    stress-eval stress-video-cost stress-video-queue
    outage-provider outage-gameday cancellation
    soak-hot-reload soak-recovery-cycles soak-budget-leak soak-video-recovery
    race-soak-short
  )
  local p found s
  for p in "${planned[@]}"; do
    found=0
    for s in "${stage_names[@]}"; do
      [[ "$s" == "$p" ]] && found=1
    done
    if (( found == 0 )); then
      if (( cancelled == 1 )); then
        record_stage "$p" "SKIPPED" "" 0 "n/a" "run cancelled before this stage"
      elif [[ -n "$validation_error" ]]; then
        record_stage "$p" "BLOCKED" "" 0 "n/a" "run blocked: $validation_error"
      else
        record_stage "$p" "SKIPPED" "" 0 "n/a" "run ended before this stage"
      fi
    fi
  done
  if (( cancelled == 1 )); then
    overall_result="CANCELLED"
  elif [[ -n "$validation_error" ]]; then
    if [[ "$validation_error" == BLOCKED:* ]]; then
      overall_result="BLOCKED"
    else
      overall_result="FAIL"
    fi
  else
    overall_result="PASS"
    for s in "${stage_status[@]}"; do
      case "$s" in
        FAIL*|CANCELLED) overall_result="FAIL" ;;
      esac
    done
  fi
  write_report "$rc"
  exit "$rc"
}
trap 'finish $?' EXIT

# ------------------------------------------------------------------ run
if [[ -n "$validation_error" ]]; then
  printf 'ci-soak-smoke: %s\n' "$validation_error" >&2
  if [[ "$validation_error" == BLOCKED:* ]]; then
    finish 3
  else
    finish 2
  fi
fi

run_stage stress-router -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/router -run='^TestStress'
run_stage stress-probe -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/probe -run='^TestStress'
run_stage stress-events -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/events -run='^TestStress'
run_stage stress-httpapi -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/httpapi -run='^TestStress'
run_stage stress-logging -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/logging -run='^TestStress'
run_stage stress-eval -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/eval -run='^TestStress'
run_stage stress-video-cost -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/video/cost -run='^TestStress'
run_stage stress-video-queue -- env NEXAROUTE_STRESS=1 go test -timeout=2m -count=1 ./internal/video/queue -run='^TestStress'

run_stage outage-provider -- go test -timeout=5m -count=1 ./internal/httpapi -run='^TestFailureInjection'
run_stage outage-gameday -- go test -timeout=5m -count=1 ./internal/httpapi -run='^TestResilienceGameDay$'
run_stage cancellation -- go test -timeout=5m -count=1 ./internal/httpapi -run='^TestContextCancellationWhileProviderConcurrencyBlocked$|^TestClientCancellationDoesNotQuarantineHealthyReadyModel$|^TestRetrySafetyCancellationStopsFailover$'

run_stage soak-hot-reload -- env NEXAROUTE_SOAK=1 NEXAROUTE_SOAK_DURATION="$soak_duration" NEXAROUTE_SOAK_SAMPLE_OUT="$samples_file_abs" go test -timeout=5m -count=1 ./internal/httpapi -run='^TestSoakConcurrentTrafficAndHotReload$|^TestSoakFlappingUpstreamFailureSemantics$'
run_stage soak-recovery-cycles -- env NEXAROUTE_SOAK=1 go test -timeout=5m -count=1 ./internal/probe -run='^TestSoakRepeatedRecoveryCyclesReleaseState$'
run_stage soak-budget-leak -- env NEXAROUTE_SOAK=1 go test -timeout=5m -count=1 ./internal/httpapi -run='^TestSoakBudgetLeakSlope$'
run_stage soak-video-recovery -- env NEXAROUTE_SOAK=1 go test -timeout=5m -count=1 ./internal/video/cost ./internal/video/queue -run='^TestSoak'

if [[ "$race_enabled" == "1" ]]; then
  run_stage race-soak-short -- env GOMAXPROCS=4 NEXAROUTE_SOAK=1 NEXAROUTE_SOAK_DURATION=0s go test -race -timeout=9m -count=1 ./internal/httpapi ./internal/probe -run='^TestSoak'
else
  skip_stage race-soak-short "NEXAROUTE_CI_SMOKE_RACE=0"
fi

if (( cancelled == 1 )); then
  finish 130
fi
for s in "${stage_status[@]}"; do
  case "$s" in
    FAIL*) finish 1 ;;
  esac
done
printf '\nCI-SOAK-SMOKE PASS\n'
finish 0

#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

rounds="${NEXAROUTE_SOAK_ROUNDS:-3}"
if ! [[ "$rounds" =~ ^[1-9][0-9]*$ ]]; then
  echo "NEXAROUTE_SOAK_ROUNDS must be a positive integer" >&2
  exit 2
fi
if (( rounds > 20 )); then
  echo "NEXAROUTE_SOAK_ROUNDS must be <= 20" >&2
  exit 2
fi

duration="${NEXAROUTE_SOAK_DURATION:-0s}"
duration_seconds=0
if [[ "$duration" != "0" && "$duration" != "0s" ]]; then
  if [[ ! "$duration" =~ ^([1-9][0-9]*)([smh])$ ]]; then
    echo "NEXAROUTE_SOAK_DURATION must be a positive Go-style duration written in seconds, minutes or hours (for example 90s, 30m, 2h), or 0s" >&2
    exit 2
  fi
  duration_value="${BASH_REMATCH[1]}"
  case "${BASH_REMATCH[2]}" in
    s) duration_seconds="$duration_value" ;;
    m) duration_seconds=$((duration_value * 60)) ;;
    h) duration_seconds=$((duration_value * 3600)) ;;
  esac
  if (( duration_seconds > 21600 )); then
    echo "NEXAROUTE_SOAK_DURATION must be <= 6h (the workflow job maximum)" >&2
    exit 2
  fi
fi
hot_soak_timeout="10m"
if (( duration_seconds > 0 )); then
  hot_soak_timeout="$((duration_seconds + 300))s"
fi

sha="$(git rev-parse HEAD 2>/dev/null || printf 'unknown')"
short_sha="${sha:0:12}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
label="${NEXAROUTE_SOAK_LABEL:-$stamp-$short_sha}"
out_root="${NEXAROUTE_SOAK_OUT:-.artifacts/soak}"
out="$out_root/$label"
dirty="$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
mkdir -p "$out"
raw="$out/soak.raw.txt"
summary="$out/summary.md"
samples_file="$out/http-hot-reload-samples.jsonl"
samples_file_abs="$(cd "$out" && pwd)/http-hot-reload-samples.jsonl"
timing_bin="$(command -v /usr/bin/time || true)"
started_at="$(date -u +%FT%TZ)"
start_epoch="$(date +%s)"
git_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || printf 'unknown')"
workflow_ref="${GITHUB_REF_NAME:-}"
workflow_head_ref="${GITHUB_HEAD_REF:-}"
stage_results=()
stage_number=0

resource_line() {
  local file="$1"
  if [[ -z "$timing_bin" || ! -s "$file" ]]; then
    printf 'n/a'
    return
  fi
  local rss
  rss="$(sed -n 's/.*Maximum resident set size (kbytes):[[:space:]]*//p' "$file" | tail -n 1)"
  if [[ -n "$rss" ]]; then
    printf '%s KB' "$rss"
  else
    printf 'n/a'
  fi
}

run_stage() {
  local name="$1"
  shift
  stage_number=$((stage_number + 1))
  local log="$out/$(printf '%02d' "$stage_number")-$name.txt"
  local timing="$out/$(printf '%02d' "$stage_number")-$name.time"
  local stage_started
  stage_started="$(date +%s)"
  {
    printf '\n== %s ==\n' "$name"
    printf '$'
    printf ' %q' "$@"
    printf '\n'
  } | tee -a "$raw"

  local status
  set +e
  if [[ -n "$timing_bin" ]]; then
    "$timing_bin" -v -o "$timing" "$@" 2>&1 | tee -a "$log" "$raw"
    status="${PIPESTATUS[0]}"
  else
    "$@" 2>&1 | tee -a "$log" "$raw"
    status="${PIPESTATUS[0]}"
  fi
  set -e

  local stage_ended stage_duration rss
  stage_ended="$(date +%s)"
  stage_duration=$((stage_ended - stage_started))
  rss="$(resource_line "$timing")"
  stage_results+=("$name|$status|$stage_duration|$rss|$log")
  printf '== %s status=%d duration=%ss max_rss=%s ==\n' "$name" "$status" "$stage_duration" "$rss" | tee -a "$raw"
  return "$status"
}

json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "$value"
}

write_json_summary() {
  local rc="$1"
  local finished_at="$2"
  local total_duration="$3"
  local go_version uname_line nproc_line cpu_line max_procs
  go_version="$(go version 2>/dev/null || printf 'unknown')"
  uname_line="$(uname -a 2>/dev/null || printf 'unknown')"
  nproc_line="$(nproc 2>/dev/null || printf 'unknown')"
  cpu_line="$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || printf 'unknown')"
  max_procs="${GOMAXPROCS:-auto}"

  {
    printf '{\n'
    printf '  "schema": "nexaroute.soak.qualification.v1",\n'
    printf '  "commit": "%s",\n' "$(json_escape "$sha")"
    printf '  "git_branch": "%s",\n' "$(json_escape "$git_branch")"
    printf '  "workflow_ref": "%s",\n' "$(json_escape "${workflow_ref:-n/a}")"
    printf '  "workflow_head_ref": "%s",\n' "$(json_escape "${workflow_head_ref:-n/a}")"
    printf '  "worktree_entries": %s,\n' "${dirty:-0}"
    printf '  "started_at": "%s",\n' "$(json_escape "$started_at")"
    printf '  "finished_at": "%s",\n' "$(json_escape "$finished_at")"
    printf '  "total_duration_seconds": %s,\n' "$total_duration"
    printf '  "stress_rounds": %s,\n' "$rounds"
    printf '  "requested_duration": "%s",\n' "$(json_escape "$duration")"
    printf '  "requested_duration_seconds": %s,\n' "$duration_seconds"
    printf '  "result": "%s",\n' "$([[ "$rc" -eq 0 ]] && printf PASS || printf FAIL)"
    printf '  "exit_code": %s,\n' "$rc"
    printf '  "output_directory": "%s",\n' "$(json_escape "$out")"
    printf '  "environment": {\n'
    printf '    "go": "%s",\n' "$(json_escape "$go_version")"
    printf '    "uname": "%s",\n' "$(json_escape "$uname_line")"
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
    printf '  "stages": [\n'
    local first=1 result name status stage_duration rss log
    for result in "${stage_results[@]}"; do
      IFS='|' read -r name status stage_duration rss log <<< "$result"
      if (( first == 0 )); then
        printf ',\n'
      fi
      first=0
      printf '    {"name": "%s", "exit_code": %s, "duration_seconds": %s, "max_rss": "%s", "log": "%s"}' \
        "$(json_escape "$name")" "$status" "$stage_duration" "$(json_escape "$rss")" "$(json_escape "$log")"
    done
    printf '\n  ]\n'
    printf '}\n'
  } > "$out/summary.json"
}

write_summary() {
  local rc="$1"
  local finished_at finished_epoch total_duration
  finished_at="$(date -u +%FT%TZ)"
  finished_epoch="$(date +%s)"
  total_duration=$((finished_epoch - start_epoch))

  {
    printf '# Soak qualification evidence\n\n'
    printf -- '- commit: `%s`\n' "$sha"
    printf -- '- git branch: `%s`\n' "$git_branch"
    printf -- '- workflow ref: `%s`\n' "${workflow_ref:-n/a}"
    printf -- '- workflow head ref: `%s`\n' "${workflow_head_ref:-n/a}"
    printf -- '- worktree entries (porcelain): %s\n' "$dirty"
    printf -- '- started: %s\n' "$started_at"
    printf -- '- finished: %s\n' "$finished_at"
    printf -- '- total duration: %ss\n' "$total_duration"
    printf -- '- stress rounds: %s\n' "$rounds"
    printf -- '- sustained duration: %s (%ss)\n' "$duration" "$duration_seconds"
    printf -- '- result: %s (exit %s)\n' "$([[ "$rc" -eq 0 ]] && printf PASS || printf FAIL)" "$rc"
    printf -- '- output directory: `%s`\n' "$out"
    printf -- '- JSON evidence: `%s`\n' "$out/summary.json"
    if [[ -s "$samples_file" ]]; then
      printf -- '- resource samples: `%s`\n' "$samples_file"
    fi
    printf '\n'
    printf '## Environment\n\n```\n'
    go version
    uname -a
    printf 'nproc: %s\n' "$(nproc 2>/dev/null || printf 'unknown')"
    printf 'cpu: %s\n' "$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//' || printf 'unknown')"
    printf 'GOMAXPROCS: %s\n' "${GOMAXPROCS:-auto}"
    printf '```\n\n'
    printf '## Stages\n\n'
    printf '| Stage | Exit | Duration | Max RSS | Raw log |\n'
    printf '|---|---|---|---|---|\n'
    local result name status stage_duration rss log
    for result in "${stage_results[@]}"; do
      IFS='|' read -r name status stage_duration rss log <<< "$result"
      printf '| %s | %s | %ss | %s | `%s` |\n' "$name" "$status" "$stage_duration" "$rss" "$log"
    done
    printf '\n## Fault classes exercised\n\n'
    printf '| Class | Proving suite |\n'
    printf '|---|---|\n'
    printf '| Provider refused / hang / mid-stream cut / half-open trial | `internal/httpapi/failure_injection_test.go` |\n'
    printf '| Outage storm | `internal/httpapi/gameday_test.go` |\n'
    printf '| Flapping upstream recovery + hot config reload | `TestSoakFlappingUpstreamFailureSemantics`, `TestSoakConcurrentTrafficAndHotReload` |\n'
    printf '| Sustained mixed workload / resource sampling | `TestSoakConcurrentTrafficAndHotReload` with `NEXAROUTE_SOAK_DURATION` |\n'
    printf '| Budget leak / disconnect / cancellation | `TestSoakBudgetLeakSlope` and the budget leak disconnect matrix |\n'
    printf '| Recovery probe flood + repeated recovery cycles | `TestStressRecoveryFloodFiveThousandDeploymentsStaysBounded`, `TestSoakRepeatedRecoveryCyclesReleaseState` |\n'
    printf '| Event flood / slow subscribers / overload admission | `TestStressConcurrentEventAndSnapshotFlood`, `TestStressGlobalAdmissionKeepsControlPlaneResponsive` |\n'
    printf '| 10k-deployment routing | `TestStressTenThousandDeploymentsConcurrentIndexedRouting` |\n\n'
    printf '## Reproduce\n\n```sh\n'
    printf 'NEXAROUTE_SOAK_ROUNDS=%s NEXAROUTE_SOAK_DURATION=%s NEXAROUTE_SOAK_OUT=benchmarks/soak ./scripts/soak.sh\n' "$rounds" "$duration"
    printf '```\n\n'
    printf 'Per-stage GNU time output and the JSON-lines goroutine/FD/heap\n'
    printf 'samples are preserved next to each raw log. The real\n'
    printf 'PostgreSQL/Redis outage drills are separate CI integration jobs\n'
    printf '(`TestPostgresIntegrationOutageRecoveryCancellationBackpressure`,\n'
    printf '`TestRedisIntegrationOutageRecoveryCancellationBackpressure`) run\n'
    printf 'against ephemeral service containers.\n\n'
    printf '## Scope / UNVERIFIED\n\n'
    printf -- '- Multi-hour sustained soak: the duration mode is operator-triggered and bounded (<= 6h); no multi-hour run is claimed unless this summary reports one.\n'
    printf -- '- Live production PostgreSQL/Redis failover drill: the CI drills use disposable local service containers; production failover remains UNVERIFIED.\n'
    printf -- '- Production-traffic SLOs: the local regression bands in `docs/PERFORMANCE_SLO.md` are not production SLOs.\n'
    printf -- '- Live operator/game-day exercise: remains UNVERIFIED unless separately run with evidence.\n'
  } > "$summary"

  write_json_summary "$rc" "$finished_at" "$total_duration"

  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    cat "$summary" >> "$GITHUB_STEP_SUMMARY"
  fi
  printf 'soak summary written to %s and %s\n' "$summary" "$out/summary.json"
}

finish() {
  local rc="$1"
  write_summary "$rc"
  exit "$rc"
}
trap 'finish $?' EXIT

for i in $(seq 1 "$rounds"); do
  run_stage "stress-round-$i" env NEXAROUTE_STRESS=1 go test -timeout=3m -count=1 ./internal/router ./internal/probe ./internal/events ./internal/httpapi ./internal/logging -run='^TestStress'
done

run_stage "http-hot-reload-soak" env NEXAROUTE_SOAK=1 NEXAROUTE_SOAK_DURATION="$duration" NEXAROUTE_SOAK_SAMPLE_OUT="$samples_file_abs" go test -timeout="$hot_soak_timeout" -count=1 -v ./internal/httpapi -run='^TestSoak'

run_stage "recovery-soak" env NEXAROUTE_SOAK=1 go test -timeout=10m -count=1 ./internal/probe -run='^TestSoak'

run_stage "race-soak" env GOMAXPROCS=4 NEXAROUTE_SOAK=1 go test -race -timeout=15m -count=1 ./internal/httpapi ./internal/probe -run='^TestSoak'

run_stage "budget-leak-slope" env NEXAROUTE_SOAK=1 go test -timeout=10m -count=1 -v ./internal/httpapi -run='^TestSoakBudgetLeakSlope$'

printf '\nSOAK PASS\n'
exit 0

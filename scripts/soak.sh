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

sha="$(git rev-parse HEAD 2>/dev/null || printf 'unknown')"
short_sha="${sha:0:12}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
label="${NEXAROUTE_SOAK_LABEL:-$stamp-$short_sha}"
out_root="${NEXAROUTE_SOAK_OUT:-.artifacts/soak}"
out="$out_root/$label"
mkdir -p "$out"
raw="$out/soak.raw.txt"
summary="$out/summary.md"
timing_bin="$(command -v /usr/bin/time || true)"
started_at="$(date -u +%FT%TZ)"
start_epoch="$(date +%s)"
git_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || printf 'unknown')"
workflow_ref="${GITHUB_REF_NAME:-}"
workflow_head_ref="${GITHUB_HEAD_REF:-}"
dirty="$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
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
  stage_results+=("$name|$status|${stage_duration}s|$rss|$log")
  printf '== %s status=%d duration=%ss max_rss=%s ==\n' "$name" "$status" "$stage_duration" "$rss" | tee -a "$raw"
  return "$status"
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
    printf -- '- result: %s (exit %s)\n' "$([[ "$rc" -eq 0 ]] && printf PASS || printf FAIL)" "$rc"
    printf -- '- output directory: `%s`\n\n' "$out"
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
    local result name status duration rss log
    for result in "${stage_results[@]}"; do
      IFS='|' read -r name status duration rss log <<< "$result"
      printf '| %s | %s | %s | %s | `%s` |\n' "$name" "$status" "$duration" "$rss" "$log"
    done
    printf '\n## Fault classes exercised\n\n'
    printf '| Class | Proving suite |\n'
    printf '|---|---|\n'
    printf '| Provider refused / hang / mid-stream cut / half-open trial | `internal/httpapi/failure_injection_test.go` |\n'
    printf '| Outage storm | `internal/httpapi/gameday_test.go` |\n'
    printf '| Flapping upstream recovery + hot config reload | `TestSoakFlappingUpstreamFailureSemantics`, `TestSoakConcurrentTrafficAndHotReload` |\n'
    printf '| Budget leak / disconnect / cancellation | `TestSoakBudgetLeakSlope` and the budget leak disconnect matrix |\n'
    printf '| Recovery probe flood + repeated recovery cycles | `TestStressRecoveryFloodFiveThousandDeploymentsStaysBounded`, `TestSoakRepeatedRecoveryCyclesReleaseState` |\n'
    printf '| Event flood / slow subscribers / overload admission | `TestStressConcurrentEventAndSnapshotFlood`, `TestStressGlobalAdmissionKeepsControlPlaneResponsive` |\n'
    printf '| 10k-deployment routing | `TestStressTenThousandDeploymentsConcurrentIndexedRouting` |\n\n'
    printf '## Reproduce\n\n```sh\n'
    printf 'NEXAROUTE_SOAK_ROUNDS=%s NEXAROUTE_SOAK_OUT=benchmarks/soak ./scripts/soak.sh\n' "$rounds"
    printf '```\n\n'
    printf 'Per-stage GNU time output is preserved next to each raw log. The\n'
    printf 'real PostgreSQL/Redis outage drills are separate CI integration\n'
    printf 'jobs (`TestPostgresIntegrationOutageRecoveryCancellationBackpressure`,\n'
    printf '`TestRedisIntegrationOutageRecoveryCancellationBackpressure`) run\n'
    printf 'against ephemeral service containers.\n\n'
    printf '## Scope / UNVERIFIED\n\n'
    printf -- '- Multi-hour sustained soak: this workflow is bounded (<= 20 rounds) and is not a multi-hour operator soak.\n'
    printf -- '- Live production PostgreSQL/Redis failover drill: the CI drills use disposable local service containers; production failover remains UNVERIFIED.\n'
    printf -- '- Production-traffic SLOs: the local regression bands in `docs/PERFORMANCE_SLO.md` are not production SLOs.\n'
    printf -- '- Live operator/game-day exercise: remains UNVERIFIED unless separately run with evidence.\n'
  } > "$summary"

  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    cat "$summary" >> "$GITHUB_STEP_SUMMARY"
  fi
  printf 'soak summary written to %s\n' "$summary"
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

run_stage "http-hot-reload-soak" env NEXAROUTE_SOAK=1 go test -timeout=10m -count=1 ./internal/httpapi -run='^TestSoak'

run_stage "recovery-soak" env NEXAROUTE_SOAK=1 go test -timeout=10m -count=1 ./internal/probe -run='^TestSoak'

run_stage "race-soak" env GOMAXPROCS=4 NEXAROUTE_SOAK=1 go test -race -timeout=15m -count=1 ./internal/httpapi ./internal/probe -run='^TestSoak'

run_stage "budget-leak-slope" env NEXAROUTE_SOAK=1 go test -timeout=10m -count=1 -v ./internal/httpapi -run='^TestSoakBudgetLeakSlope$'

printf '\nSOAK PASS\n'
exit 0

#!/usr/bin/env bash
# Upgrade, migration and rollback acceptance for NexaRoute.
#
# What it proves, end to end with real binaries in disposable directories:
#   1. A prior-release gateway (default: the v0.24.0 tag) starts on a
#      legacy plaintext config, migrates it, and serves health + API
#      routes (pre-upgrade baseline).
#   2. A pre-upgrade backup (taken after the gateway stops, the
#      standard pre-upgrade procedure) verifies in isolation with the
#      NEW binary (cross-version restore --verify drill,
#      machine-readable report). A plain-backup probe against the live
#      gateway runs first and records its outcome without failing the
#      script: on the file-backend store it fails closed on the
#      exclusive lock (see the evidence report). The supported live
#      path (`nexaroute backup --live`, in-process snapshot via the
#      new gateway's admin endpoint) is exercised separately after the
#      post-upgrade run, where it must succeed and verify.
#   3. The installer swaps both executables, leaves config/key bytes
#      and modes untouched, and the new binary restarts to the same
#      health, routes, secrets-verification count and key bytes.
#   4. SIGKILL + restart recovers (lock release, config intact).
#   5. Tampered-bundle and wrong-key guards fail closed on copies,
#      never on the live target.
#   6. Rollback: restoring the pre-upgrade bundle and restarting the
#      OLD binary (or the new binary when no old binary was built)
#      returns the installation to the pre-upgrade health + secrets.
#
# Safety: everything lives under one mktemp directory (binaries, config,
# keys, bundles, drill dirs). Credentials are synthetic canaries, never
# real keys. Nothing under $HOME, /usr/local/bin, or any production path
# is touched. The script never runs against a production installation;
# pass --keep-dir only to inspect a disposable run afterwards.
#
# Stages that cannot run here are reported as NOT_RUN with a reason and
# do NOT fail the script: arm64 execution (no aarch64 CPU/qemu here;
# arm64 is still built, and arch selection is covered by
# scripts/test-install.py), a deterministic kill mid-migration or
# mid-restore at binary level (migration is a single atomic rename;
# restore crash recovery is covered by the Go journal tests, including
# the subprocess crash test), power-loss durability (fsync-bounded,
# documented in docs/OPERATIONS.md), and any live/production target.
#
# Usage:
#   ./scripts/upgrade-acceptance.sh [--old-tag v0.24.0] [--skip-old]
#       [--report PATH] [--keep-dir]
# Exit codes: 0 = every executed stage passed; 1 = an executed stage
# failed. NOT_RUN stages never change the exit code.
set -uo pipefail
# Disposable state must be private by default: the gateway fails closed
# when its security-store directory is not 0700, and a caller umask of
# 022 would create it 0755. (Same choice as scripts/install.sh.)
umask 077

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OLD_TAG="v0.24.0"
SKIP_OLD=0
REPORT=""
KEEP_DIR=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --old-tag) OLD_TAG="$2"; shift 2 ;;
    --skip-old) SKIP_OLD=1; shift ;;
    --report) REPORT="$2"; shift 2 ;;
    --keep-dir) KEEP_DIR=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

for dep in git go curl python3 sha256sum; do
  command -v "$dep" >/dev/null 2>&1 || { echo "FAIL missing required tool: $dep" >&2; exit 1; }
done

WORK="$(mktemp -d)"
GATEWAY_PID=""
CURRENT_LOG=""
[[ -n "$REPORT" ]] && : > "$REPORT"

log() { printf '%s\n' "$*"; if [[ -n "$REPORT" ]]; then printf '%s\n' "$*" >> "$REPORT"; fi; }
pass() { log "PASS $1"; }
notrun() { log "NOT_RUN $1: $2"; }
fail() { log "FAIL $1"; [[ -n "${ERRLOG:-}" && -f "$ERRLOG" ]] && sed -n '1,120p' "$ERRLOG" >&2; [[ -n "$CURRENT_LOG" && -f "$CURRENT_LOG" ]] && sed -n '1,120p' "$CURRENT_LOG" >&2; exit 1; }

cleanup() {
  if [[ -n "$GATEWAY_PID" ]] && kill -0 "$GATEWAY_PID" 2>/dev/null; then
    kill "$GATEWAY_PID" 2>/dev/null || true
    wait "$GATEWAY_PID" 2>/dev/null || true
  fi
  GATEWAY_PID=""
  if [[ "$KEEP_DIR" -eq 0 ]]; then rm -rf "$WORK"; else log "kept disposable dir: $WORK"; fi
}
trap cleanup EXIT

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }

# --- build helpers -------------------------------------------------------
build_pair() { # $1=source dir $2=out dir $3=version label (without leading v)
  local src="$1" out="$2" ver="$3"
  mkdir -p "$out"
  (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=$ver -X github.com/ali-shortcuts/nexaroute/internal/httpapi.gatewayVersion=$ver" \
    -o "$out/nexaroute" ./cmd/gateway) || return 1
  (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=$ver" \
    -o "$out/videogen" ./cmd/videogen) || return 1
  return 0
}

release_dir() { # $1=built pair dir $2=release dir $3=arch
  local pair="$1" rel="$2" arch="$3"
  mkdir -p "$rel"
  cp "$pair/nexaroute" "$rel/nexaroute-linux-$arch"
  cp "$pair/videogen" "$rel/nexaroute-videogen-linux-$arch"
  (cd "$rel" && sha256sum "nexaroute-linux-$arch" "nexaroute-videogen-linux-$arch" > SHA256SUMS)
}

start_gateway() { # $1=binary $2=config $3=log
  local bin="$1" cfg="$2" lg="$3"
  CURRENT_LOG="$lg"
  env -u NEXAROUTE_CONFIG -u NEXAROUTE_LISTEN -u NEXAROUTE_MASTER_KEY -u NEXAROUTE_MASTER_KEY_FILE \
    DISPLAY="" "$bin" -no-browser -config "$cfg" >"$lg" 2>&1 &
  GATEWAY_PID=$!
}

wait_health() { # $1=base url
  local base="$1"
  for _ in $(seq 1 150); do
    if curl -fsS "$base/healthz" >/dev/null 2>&1; then return 0; fi
    if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then return 1; fi
    sleep 0.2
  done
  return 1
}

stop_gateway() { # graceful; $1=label
  if [[ -n "$GATEWAY_PID" ]] && kill -0 "$GATEWAY_PID" 2>/dev/null; then
    kill "$GATEWAY_PID" 2>/dev/null || true
    wait "$GATEWAY_PID" 2>/dev/null || true
  fi
  GATEWAY_PID=""
  CURRENT_LOG=""
}

route_status() { # $1=base $2=path $3=out file; prints HTTP status
  curl -sS -o "$3" -w '%{http_code}' "$1$2" 2>/dev/null || echo "000"
}

snapshot_routes() { # $1=base $2=prefix for outputs; sets globals
  local base="$1" pfx="$2"
  HEALTHZ_S="$(route_status "$base" "/healthz" "$pfx-healthz")"
  HELLO_S="$(route_status "$base" "/api/hello" "$pfx-hello")"
  MODELS_S="$(route_status "$base" "/v1/models" "$pfx-models")"
  SNAPSHOT_S="$(route_status "$base" "/admin/api/snapshot" "$pfx-snapshot")"
  READYZ_S="$(route_status "$base" "/readyz" "$pfx-readyz")"
  COUNT_S="$(curl -sS -o "$pfx-count" -w '%{http_code}' -X POST -H 'content-type: application/json' \
    --data-binary '{"model":"claude-auto","messages":[{"role":"user","content":"hello world"}]}' \
    "$base/v1/messages/count_tokens" 2>/dev/null || echo "000")"
}

require_status() { # $1=label $2=got $3=want
  [[ "$2" == "$3" ]] || fail "$1: HTTP $2, want $3"
}

# --- versions ------------------------------------------------------------
NEW_SHA="$(git -C "$ROOT" rev-parse HEAD)"
NEW_SHORT="$(git -C "$ROOT" rev-parse --short HEAD)"
OLD_SHA="$(git -C "$ROOT" rev-list -n1 "$OLD_TAG" 2>/dev/null || echo "")"
log "new source: $NEW_SHA"
log "old tag: $OLD_TAG commit: ${OLD_SHA:-unknown}"
log "host arch: $(uname -m)"

BIN="$WORK/bin"; LIVE="$WORK/live"; BACKUPS="$WORK/backups"; EVIDENCE="$WORK/evidence"
mkdir -p "$BIN" "$LIVE" "$BACKUPS" "$EVIDENCE"
PORT="$(free_port)"
BASE="http://127.0.0.1:$PORT"
CFG="$LIVE/config.json"
log "disposable work: $WORK (removed on exit unless --keep-dir)"
log "gateway address: $BASE"

# --- build the new pair --------------------------------------------------
NEW_PAIR="$WORK/new-pair"
if ! build_pair "$ROOT" "$NEW_PAIR" "test-$NEW_SHORT"; then fail "build new gateway+videogen"; fi
NEW_BIN="$NEW_PAIR/nexaroute"; NEW_VIDEO="$NEW_PAIR/videogen"
pass "build new pair ($NEW_SHORT)"
[[ "$("$NEW_BIN" -version)" == *"test-$NEW_SHORT"* ]] || fail "new -version label"
NEW_REL="$WORK/release-new"; release_dir "$NEW_PAIR" "$NEW_REL" "amd64"

# --- build the old pair (best effort; NOT_RUN if it cannot build) --------
OLD_OK=0; OLD_BIN=""; OLD_VIDEO=""
if [[ "$SKIP_OLD" -eq 1 ]]; then
  notrun "old-binary stages" "--skip-old given"
elif [[ -z "$OLD_SHA" ]]; then
  notrun "old-binary stages" "tag $OLD_TAG not present in this checkout"
else
  OLD_SRC="$WORK/old-src"
  mkdir -p "$OLD_SRC"
  if git -C "$ROOT" archive "$OLD_TAG" | tar -x -C "$OLD_SRC" 2>/dev/null; then
    OLD_PAIR="$WORK/old-pair"
    if build_pair "$OLD_SRC" "$OLD_PAIR" "${OLD_TAG#v}"; then
      OLD_OK=1; OLD_BIN="$OLD_PAIR/nexaroute"; OLD_VIDEO="$OLD_PAIR/videogen"
      [[ "$("$OLD_BIN" -version)" == *"${OLD_TAG#v}"* ]] || fail "old -version label"
      pass "build old pair ($OLD_TAG $OLD_SHA)"
    else
      notrun "old-binary stages" "old-tag build failed (module/network); continuing new-only"
    fi
  else
    notrun "old-binary stages" "git archive of $OLD_TAG failed; continuing new-only"
  fi
fi

# --- fixture: legacy plaintext config, synthetic canaries only -----------
cat > "$CFG" <<EOF
{
  "listen": "127.0.0.1:$PORT",
  "admin": {"bind_local_only": true, "api_key": "", "emergency_access_enabled": true, "security_store_path": "security/nexaroute-security.db"},
  "probe": {"enabled": false, "on_start": false},
  "providers": [{"id": "accept-p", "type": "openai_compatible", "base_url": "http://127.0.0.1:9/v1", "enabled": false, "api_key": "upgrade-acceptance-provider-canary", "credentials": [{"api_key": "upgrade-acceptance-pool-canary"}]}]
}
EOF
chmod 0600 "$CFG"
mkdir -p "$LIVE/security"
chmod 0700 "$LIVE" "$LIVE/security"

PRE_BIN="$NEW_BIN"; PRE_LABEL="new-only"
if [[ "$OLD_OK" -eq 1 ]]; then PRE_BIN="$OLD_BIN"; PRE_LABEL="old $OLD_TAG"; fi

# --- pre-upgrade run -----------------------------------------------------
start_gateway "$PRE_BIN" "$CFG" "$WORK/gateway-pre.log"
wait_health "$BASE" || fail "pre-upgrade gateway did not become healthy"
snapshot_routes "$BASE" "$WORK/pre"
require_status "pre /healthz" "$HEALTHZ_S" "200"
require_status "pre /api/hello" "$HELLO_S" "200"
require_status "pre /v1/models" "$MODELS_S" "200"
require_status "pre /admin/api/snapshot" "$SNAPSHOT_S" "200"
grep -qF "accept-p" "$WORK/pre-snapshot" || fail "pre snapshot missing fixture provider"
for canary in upgrade-acceptance-provider-canary upgrade-acceptance-pool-canary; do
  grep -qF "$canary" "$WORK/pre-snapshot" && fail "pre snapshot leaks a secret canary"
done
PRE_HEALTHZ="$HEALTHZ_S"; PRE_HELLO="$HELLO_S"; PRE_MODELS="$MODELS_S"; PRE_SNAPSHOT="$SNAPSHOT_S"
PRE_READYZ="$READYZ_S"; PRE_COUNT="$COUNT_S"
pass "pre-upgrade health + routes ($PRE_LABEL): healthz/hello/models/snapshot 200, readyz=$PRE_READYZ count_tokens=$PRE_COUNT"

# One admin write pre-upgrade: it seeds the audit chain (so the backup
# snapshots a real chain, not an empty store) and proves admin
# persistence across the upgrade. The response must stay redacted.
API_CREATE='{"provider":{"id":"accept-api","name":"Accept API","type":"openai_compatible","base_url":"http://127.0.0.1:9/v1","api_key":"upgrade-acceptance-api-canary","auth_mode":"bearer","enabled":false,"models":[{"id":"m","model":"upstream-model","enabled":true,"weight":1}]}}'
API_CODE="$(curl -sS -o "$WORK/pre-api-create" -w '%{http_code}' -X POST -H 'content-type: application/json' \
  --data-binary "$API_CREATE" "$BASE/admin/api/providers" 2>/dev/null || echo "000")"
[[ "$API_CODE" == "201" ]] || fail "pre admin provider create: HTTP $API_CODE"
grep -qF "upgrade-acceptance-api-canary" "$WORK/pre-api-create" && fail "pre admin create leaks a secret"
pass "pre admin provider create (201, redacted)"

PRE_VERIFY="$("$PRE_BIN" secrets verify --config "$CFG" 2>&1)" || fail "pre secrets verify"
PRE_FIELDS="$(printf '%s' "$PRE_VERIFY" | grep -o 'verified_fields=[0-9]*' | cut -d= -f2)"
[[ -n "$PRE_FIELDS" ]] || fail "pre secrets verify count missing"
"$PRE_BIN" config validate --config "$CFG" >/dev/null 2>&1 || fail "pre config validate"
"$PRE_BIN" config dry-run --config "$CFG" >/dev/null 2>&1 || fail "pre config dry-run"
DIFF_OUT="$("$PRE_BIN" config diff --config "$CFG" 2>&1)" || fail "pre config diff"
grep -qF "upgrade-acceptance-provider-canary" <<<"$DIFF_OUT" && fail "pre config diff leaks a secret"
pass "pre config validate/diff/dry-run + secrets verify (verified_fields=$PRE_FIELDS)"

# The gateway start migrated the plaintext fixture (old or new binary, the
# documented path). Record the migrated bytes + key for every later check.
PRE_CONFIG_HASH="$(sha256sum "$CFG" | cut -d' ' -f1)"
PRE_KEY_HASH="$(sha256sum "$CFG.key" | cut -d' ' -f1)"
grep -q 'nxs1:' "$CFG" || fail "pre config was not migrated to nxs1"
grep -qF "upgrade-acceptance-provider-canary" "$CFG" && fail "pre config still contains a plaintext canary"
[[ "$(stat -c '%a' "$CFG")" == "600" ]] || fail "pre config mode"
[[ "$(stat -c '%a' "$CFG.key")" == "600" ]] || fail "pre key mode"
[[ "$(stat -c '%a' "$LIVE")" == "700" ]] || fail "pre config dir mode"
pass "pre migration: nxs1 on disk, key bytes recorded, modes 0600/0700"

# --- live-backup boundary probe (recorded, never a failure) -------------
# The running gateway holds the file-backend security store's exclusive
# bbolt flock, so a second process opening it read-only fails closed.
# Probe both binaries with a PLAIN backup against the live gateway,
# record the outcome, and take the real pre-upgrade backup only after
# the gateway stops below. A --live probe against the pre-upgrade
# gateway is recorded the same way: an old (v0.24.0) gateway has no
# snapshot endpoint, so the new CLI must fail closed there; in
# new-only mode the running gateway IS the new binary, so that probe
# is skipped (the live path is proven strictly after post-upgrade).
probe_live() { # $1=label $2=binary
  if "$2" backup --config "$CFG" --out "$WORK/live-probe.tar" >"$WORK/live-probe.err" 2>&1; then
    log "LIVE_BACKUP_SUCCEEDED unexpectedly ($1); probe bundle removed"
    rm -f "$WORK/live-probe.tar"
  else
    log "LIVE_BACKUP_FAILED ($1): $(head -n 1 "$WORK/live-probe.err")"
  fi
}
probe_live "pre-upgrade binary ($PRE_LABEL)" "$PRE_BIN"
probe_live "new binary" "$NEW_BIN"
if [[ "$OLD_OK" -eq 1 ]]; then
  if "$NEW_BIN" backup --live --config "$CFG" --out "$WORK/live-probe-live.tar" >"$WORK/live-probe-live.err" 2>&1; then
    log "LIVE_BACKUP_OLD_GATEWAY_SUCCEEDED unexpectedly; probe bundle removed"
    rm -f "$WORK/live-probe-live.tar"
  else
    log "LIVE_BACKUP_OLD_GATEWAY_FAILED (expected, old gateway has no snapshot endpoint): $(head -n 1 "$WORK/live-probe-live.err")"
  fi
else
  log "LIVE_BACKUP_OLD_GATEWAY skipped (new-only mode; live path proven after post-upgrade)"
fi


# --- installer upgrade ----------------------------------------------------
# Install the pre-upgrade binary first (so the swap is a real upgrade,
# not a first install), then stop the gateway and swap to the new pair.
export NEXAROUTE_INSTALL_DIR="$BIN" PATH="$BIN:$PATH"
export XDG_CONFIG_HOME="$WORK/config-home"
if [[ "$OLD_OK" -eq 1 ]]; then
  OLD_REL="$WORK/release-old"; release_dir "$OLD_PAIR" "$OLD_REL" "amd64"
  NEXAROUTE_LOCAL_RELEASE_DIR="$OLD_REL" NEXAROUTE_LOCAL_RELEASE_TAG="$OLD_TAG" \
    bash "$ROOT/scripts/install.sh" >/dev/null 2>&1 || fail "installer install old pair"
  [[ "$("$BIN/nexaroute" -version)" == *"${OLD_TAG#v}"* ]] || fail "installed old -version"
else
  NEXAROUTE_LOCAL_RELEASE_DIR="$NEW_REL" NEXAROUTE_LOCAL_RELEASE_TAG="test-$NEW_SHORT" \
    bash "$ROOT/scripts/install.sh" >/dev/null 2>&1 || fail "installer install new pair (new-only)"
fi
[[ -d "$WORK/config-home/nexaroute" ]] || fail "installer config dir"
[[ "$(stat -c '%a' "$WORK/config-home/nexaroute")" == "700" ]] || fail "installer config dir mode"
stop_gateway "pre-upgrade stop"
"$PRE_BIN" doctor --config "$CFG" >/dev/null 2>&1 || fail "pre doctor (gateway stopped)"
pass "pre doctor (gateway stopped)"

# --- pre-upgrade backup (gateway stopped) + manifest fingerprint ---------
ERRLOG="$WORK/backup.err"
if ! "$PRE_BIN" backup --config "$CFG" --out "$BACKUPS/pre-upgrade.tar" >"$ERRLOG" 2>&1; then fail "pre-upgrade backup (stopped)"; fi
if ! "$PRE_BIN" backup --config "$CFG" --out "$BACKUPS/pre-upgrade-with-key.tar" --include-master-key >"$ERRLOG" 2>&1; then fail "pre-upgrade keyed backup (stopped)"; fi
[[ "$(stat -c '%a' "$BACKUPS/pre-upgrade.tar")" == "600" ]] || fail "backup bundle mode"
python3 - "$BACKUPS/pre-upgrade.tar" "$CFG.key" <<'PY' || fail "backup manifest key fingerprint"
import hashlib, sys, tarfile
bundle, key_path = sys.argv[1], sys.argv[2]
with tarfile.open(bundle) as tf:
    manifest = tf.extractfile("manifest.json").read().decode()
    names = tf.getnames()
assert '"format": "nexaroute-backup/1"' in manifest, manifest
assert "master.key" not in names, names
want = hashlib.sha256(open(key_path, "rb").read()).hexdigest()
assert want in manifest, "manifest fingerprint does not match the live key"
print("manifest: nexaroute-backup/1, keyless, fingerprint matches live key")
PY
pass "pre-upgrade backup (stopped gateway, keyless + keyed), bundle 0600, manifest fingerprint matches live key"

mkdir -m 0700 "$WORK/drill"
if ! "$NEW_BIN" restore --verify --bundle "$BACKUPS/pre-upgrade.tar" --config "$CFG" \
    --into "$WORK/drill" --report "$EVIDENCE/drill.json" --max-age 24h >/dev/null 2>&1; then
  fail "cross-version restore --verify drill (new binary on pre-upgrade bundle)"
fi
python3 - "$EVIDENCE/drill.json" <<'PY' || fail "drill report contains FAIL/BLOCKED"
import json, sys
report = json.load(open(sys.argv[1]))
bad = [c for c in report.get("checks", []) if c.get("status") in ("FAIL", "BLOCKED")]
assert not bad, bad
print("drill checks:", [(c["name"], c["status"]) for c in report.get("checks", [])])
PY
pass "isolated restore --verify drill with new binary (no FAIL/BLOCKED)"

NEXAROUTE_LOCAL_RELEASE_DIR="$NEW_REL" NEXAROUTE_LOCAL_RELEASE_TAG="test-$NEW_SHORT" \
  bash "$ROOT/scripts/install.sh" >/dev/null 2>&1 || fail "installer upgrade to new pair"
[[ "$("$BIN/nexaroute" -version)" == *"test-$NEW_SHORT"* ]] || fail "upgraded -version label"
[[ "$("$BIN/videogen" -version)" == *"test-$NEW_SHORT"* ]] || fail "upgraded videogen -version label"
[[ "$(sha256sum "$CFG" | cut -d' ' -f1)" == "$PRE_CONFIG_HASH" ]] || fail "installer changed the config"
[[ "$(sha256sum "$CFG.key" | cut -d' ' -f1)" == "$PRE_KEY_HASH" ]] || fail "installer changed the master key"
pass "installer upgrade: versions changed, config/key bytes unchanged"
"$NEW_BIN" doctor --config "$CFG" --bundle "$BACKUPS/pre-upgrade.tar" --max-age 24h >/dev/null 2>&1 \
  || fail "post-upgrade doctor with pre-upgrade bundle"
pass "post-upgrade doctor with pre-upgrade bundle"

# --- post-upgrade run -----------------------------------------------------
start_gateway "$NEW_BIN" "$CFG" "$WORK/gateway-post.log"
wait_health "$BASE" || fail "post-upgrade gateway did not become healthy"
snapshot_routes "$BASE" "$WORK/post"
require_status "post /healthz" "$HEALTHZ_S" "200"
require_status "post /api/hello" "$HELLO_S" "200"
require_status "post /v1/models" "$MODELS_S" "200"
require_status "post /admin/api/snapshot" "$SNAPSHOT_S" "200"
[[ "$READYZ_S" == "$PRE_READYZ" ]] || fail "post /readyz changed: $READYZ_S vs pre $PRE_READYZ"
[[ "$COUNT_S" == "$PRE_COUNT" ]] || fail "post count_tokens changed: $COUNT_S vs pre $PRE_COUNT"
grep -qF "accept-p" "$WORK/post-snapshot" || fail "post snapshot missing fixture provider"
POST_VERIFY="$("$NEW_BIN" secrets verify --config "$CFG" 2>&1)" || fail "post secrets verify"
POST_FIELDS="$(printf '%s' "$POST_VERIFY" | grep -o 'verified_fields=[0-9]*' | cut -d= -f2)"
[[ "$POST_FIELDS" == "$PRE_FIELDS" ]] || fail "post verified_fields changed: $POST_FIELDS vs pre $PRE_FIELDS"
[[ "$(sha256sum "$CFG.key" | cut -d' ' -f1)" == "$PRE_KEY_HASH" ]] || fail "post-upgrade key bytes changed"
grep -qF "upgrade-acceptance-provider-canary" "$CFG" && fail "post config contains a plaintext canary"
pass "post-upgrade health + routes + secrets verify (verified_fields=$POST_FIELDS, key unchanged)"

# --- live backup from the running new gateway ---------------------------
# The supported live path: the new CLI asks the running new gateway's
# admin endpoint for an in-process snapshot (never a second open of the
# locked file). The fixture admin key is empty with emergency access
# enabled, so loopback keyless emergency access applies; the CLI
# derives the admin URL from the config listen address.
LIVE_ERR="$WORK/live-backup.err"
if ! "$NEW_BIN" backup --live --config "$CFG" --out "$BACKUPS/live-post-upgrade.tar" >"$LIVE_ERR" 2>&1; then
  ERRLOG="$LIVE_ERR" fail "live backup from running new gateway (--live)"
fi
[[ "$(stat -c '%a' "$BACKUPS/live-post-upgrade.tar")" == "600" ]] || fail "live bundle mode"
python3 - "$BACKUPS/live-post-upgrade.tar" "$CFG.key" <<'PY' || fail "live bundle manifest"
import hashlib, sys, tarfile
bundle, key_path = sys.argv[1], sys.argv[2]
with tarfile.open(bundle) as tf:
    manifest = tf.extractfile("manifest.json").read().decode()
    names = tf.getnames()
assert '"format": "nexaroute-backup/1"' in manifest, manifest
assert "master.key" not in names, names
assert "taken live" in manifest, "live bundle manifest must record live provenance"
want = hashlib.sha256(open(key_path, "rb").read()).hexdigest()
assert want in manifest, "live manifest fingerprint does not match the live key"
print("live manifest: nexaroute-backup/1, keyless, fingerprint matches live key, live provenance recorded")
PY
mkdir -m 0700 "$WORK/drill-live"
if ! "$NEW_BIN" restore --verify --bundle "$BACKUPS/live-post-upgrade.tar" --config "$CFG" \
    --into "$WORK/drill-live" --report "$EVIDENCE/drill-live.json" --max-age 24h >/dev/null 2>&1; then
  fail "live bundle isolated restore --verify"
fi
python3 - "$EVIDENCE/drill-live.json" <<'PY' || fail "live drill report contains FAIL/BLOCKED"
import json, sys
report = json.load(open(sys.argv[1]))
bad = [c for c in report.get("checks", []) if c.get("status") in ("FAIL", "BLOCKED")]
assert not bad, bad
print("live drill checks:", [(c["name"], c["status"]) for c in report.get("checks", [])])
PY
pass "live backup from running new gateway (--live): bundle 0600, manifest fingerprint matches key, isolated verify PASS"

# --- SIGKILL + restart ----------------------------------------------------
kill -9 "$GATEWAY_PID" 2>/dev/null || true
wait "$GATEWAY_PID" 2>/dev/null || true
GATEWAY_PID=""
start_gateway "$NEW_BIN" "$CFG" "$WORK/gateway-kill.log"
wait_health "$BASE" || fail "gateway did not recover after SIGKILL"
snapshot_routes "$BASE" "$WORK/kill"
require_status "after-SIGKILL /healthz" "$HEALTHZ_S" "200"
grep -qF "accept-p" "$WORK/kill-snapshot" || fail "after-SIGKILL snapshot missing fixture provider"
pass "SIGKILL restart: lock released, config + provider intact"
stop_gateway "post-kill stop"

# --- tamper + wrong-key guards on copies (never the live target) ---------
python3 - "$BACKUPS/pre-upgrade.tar" "$BACKUPS/tampered.tar" <<'PY' || fail "tamper fixture"
import sys
data = bytearray(open(sys.argv[1], "rb").read())
idx = data.find(b"127.0.0.1:")
assert idx >= 0, "listen address not found in bundle"
data[idx + 10] = ord("9") if data[idx + 10] != ord("9") else ord("8")
open(sys.argv[2], "wb").write(data)
PY
if "$NEW_BIN" restore --verify --bundle "$BACKUPS/tampered.tar" --config "$CFG" >/dev/null 2>&1; then
  fail "tampered bundle passed restore --verify"
fi
mkdir -m 0700 "$WORK/tamper-target"
if "$NEW_BIN" restore --bundle "$BACKUPS/tampered.tar" --config "$WORK/tamper-target/config.json" --force >/dev/null 2>&1; then
  fail "tampered bundle installed"
fi
[[ ! -e "$WORK/tamper-target/config.json" ]] || fail "tampered restore wrote a target"
pass "tampered bundle: verify + install both fail closed, nothing written"

mkdir -m 0700 "$WORK/wrongkey"
cp "$CFG" "$WORK/wrongkey/config.json"; chmod 0600 "$WORK/wrongkey/config.json"
python3 -c "open('$WORK/wrongkey/config.json.key','wb').write(b'9'*32)"
chmod 0600 "$WORK/wrongkey/config.json.key"
if "$NEW_BIN" restore --verify --bundle "$BACKUPS/pre-upgrade.tar" --config "$WORK/wrongkey/config.json" \
    --report "$EVIDENCE/wrongkey.json" >/dev/null 2>&1; then
  fail "wrong-key bundle passed restore --verify"
fi
pass "wrong-key bundle: restore --verify fails closed (pairing mismatch)"

# --- rollback ---------------------------------------------------------------
# Stop is already done above; restore the pre-upgrade bundle over the live
# config, then restart the OLD binary when one was built (a true binary
# rollback) or the new binary otherwise (a config-only rollback).
if ! "$NEW_BIN" restore --bundle "$BACKUPS/pre-upgrade.tar" --config "$CFG" --force >/dev/null 2>&1; then
  fail "rollback restore of pre-upgrade bundle"
fi
[[ "$(sha256sum "$CFG" | cut -d' ' -f1)" == "$PRE_CONFIG_HASH" ]] || fail "rollback config differs from pre-upgrade bytes"
[[ "$(sha256sum "$CFG.key" | cut -d' ' -f1)" == "$PRE_KEY_HASH" ]] || fail "rollback key differs from pre-upgrade key"
ROLLBACK_BIN="$NEW_BIN"; ROLLBACK_LABEL="new binary (config rollback)"
if [[ "$OLD_OK" -eq 1 ]]; then ROLLBACK_BIN="$OLD_BIN"; ROLLBACK_LABEL="old binary $OLD_TAG (binary rollback)"; fi
start_gateway "$ROLLBACK_BIN" "$CFG" "$WORK/gateway-rollback.log"
wait_health "$BASE" || fail "rollback gateway did not become healthy"
snapshot_routes "$BASE" "$WORK/rollback"
require_status "rollback /healthz" "$HEALTHZ_S" "200"
require_status "rollback /api/hello" "$HELLO_S" "200"
require_status "rollback /admin/api/snapshot" "$SNAPSHOT_S" "200"
grep -qF "accept-p" "$WORK/rollback-snapshot" || fail "rollback snapshot missing fixture provider"
RB_VERIFY="$("$ROLLBACK_BIN" secrets verify --config "$CFG" 2>&1)" || fail "rollback secrets verify"
RB_FIELDS="$(printf '%s' "$RB_VERIFY" | grep -o 'verified_fields=[0-9]*' | cut -d= -f2)"
[[ "$RB_FIELDS" == "$PRE_FIELDS" ]] || fail "rollback verified_fields changed: $RB_FIELDS vs pre $PRE_FIELDS"
pass "rollback ($ROLLBACK_LABEL): health + routes + secrets verify restored"
stop_gateway "rollback stop"

# --- arm64: build only ------------------------------------------------------
if (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
    -o "$WORK/nexaroute-linux-arm64" ./cmd/gateway >/dev/null 2>&1) \
  && (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
    -o "$WORK/nexaroute-videogen-linux-arm64" ./cmd/videogen >/dev/null 2>&1); then
  pass "arm64 cross-build (gateway + videogen)"
else
  fail "arm64 cross-build"
fi
notrun "arm64 execution" "no aarch64 CPU or qemu in this environment; arch selection bytes are covered by scripts/test-install.py"
notrun "deterministic kill mid-migration / mid-restore at binary level" "migration is a single atomic rename; restore crash recovery is covered by the Go journal tests including the subprocess crash test"
notrun "power-loss durability" "fsync-bounded only; see docs/OPERATIONS.md"
notrun "production rollback" "forbidden by design; all rollback stages ran in disposable directories"

log "UPGRADE ACCEPTANCE PASS (old-binary stages: $([[ "$OLD_OK" -eq 1 ]] && echo executed || echo NOT_RUN))"

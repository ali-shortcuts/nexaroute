#!/usr/bin/env bash
# NexaRoute Kubernetes / Helm acceptance for issue #650.
#
# Two modes, chosen explicitly:
#
#   scripts/k8s-acceptance.sh --offline-only
#       No cluster, no registry, no credentials. Runs helm lint, renders
#       the chart with the least-privilege acceptance values, and asserts
#       the hardened posture, the probe wiring, the Recreate strategy, the
#       secret-reference-only database wiring, and the termination-grace
#       vs. gateway-drain invariant. This is the mode CI runs.
#
#   scripts/k8s-acceptance.sh --self-test
#       No cluster either. Deterministic, cluster-free tests of the
#       failure paths that --live cannot prove without a cluster: the
#       missing-prerequisite NOT_RUN gates (missing tools, missing
#       preceding tag, an already-existing kind cluster — all must exit
#       without creating a cluster), the live context guard (a foreign
#       kubectl context must refuse before any namespace or Secret is
#       created), the drain invariant against an oversized and a
#       malformed config, the missing-Secret render
#       (reference-only wiring, never a plaintext DSN), and the
#       fault-release cleanup helper (idempotent uninstall + secret
#       deletion). CI runs this mode too; it is not a substitute for a
#       live run, it is the part of safe failure that is provable
#       without one.
#
#   scripts/k8s-acceptance.sh --live [--with-postgres] [--keep-cluster]
#       Creates a DISPOSABLE kind cluster (default name
#       nexaroute-accept), installs the chart from the preceding supported
#       release, upgrades it to the tree under test, rolls it back, and
#       re-upgrades it, exercising probes, non-root/read-only posture,
#       graceful termination, pod restart with PVC persistence, and the
#       secret-leak check. After the lifecycle phases it runs the
#       safe-failure phases: an invalid-JSON config, an unreachable
#       PostgreSQL control-plane backend, and a missing referenced
#       Secret must each fail closed (a pod is created, none becomes
#       Ready), each fault release is then uninstalled and verified
#       absent, and a failed rollout (unpullable image) must roll back
#       to a Ready release. The cluster is deleted on exit unless
#       --keep-cluster is given. This mode NEVER touches an existing
#       cluster: it refuses to run if its kind cluster already exists,
#       and it never uses the operator's current kubectl context except
#       to talk to the kind cluster it just created.
#
# Exit codes: 0 = every executed phase passed; 1 = a phase failed;
# 2 = usage error; 3 = live acceptance NOT_RUN (a prerequisite is
# missing, or no preceding-release tag is available). Exit 3 is not a
# pass: it means the live evidence was not produced.
#
# Coverage limits (documented, not hidden): --live exercises safe-failure
# phases — an invalid-JSON config, an unreachable PostgreSQL backend, and a
# missing referenced Secret must each fail closed (a pod is created, none
# becomes Ready) with the fault release then uninstalled and verified absent,
# and a failed rollout (unpullable image) must roll back to a Ready release.
# Redis has no live failure phase. A --live PASS covers install, upgrade,
# rollback, probes, posture, termination, persistence, the leak check, and
# the safe-failure phases.
#
# Secrets discipline: the only secret values this script ever creates are
# SENTINEL values (fake, recognisable, grep-able). Real credentials must
# never be passed through this script. The leak check greps rendered
# manifests, events, pod logs and describe output for the sentinel
# marker and for plaintext postgres:// / redis:// URLs, and it reports
# only WHETHER a match was found, never the matching line, so a failure
# cannot itself print a secret. The script never dumps Secret objects.
set -euo pipefail

MODE="offline"
KEEP_CLUSTER=0
WITH_POSTGRES=0
PREV_TAG="v0.24.0"
CLUSTER="nexaroute-accept"
NAMESPACE="nexaroute-accept"
RELEASE="nexaroute"
OUT_DIR=""

usage() {
  sed -n '1,/^set -euo pipefail$/p' "$0" | head -n -1
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --offline-only) MODE="offline"; shift ;;
    --self-test) MODE="selftest"; shift ;;
    --live) MODE="live"; shift ;;
    --keep-cluster) KEEP_CLUSTER=1; shift ;;
    --with-postgres) WITH_POSTGRES=1; shift ;;
    --prev-tag) PREV_TAG="$2"; shift 2 ;;
    --cluster) CLUSTER="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --release) RELEASE="$2"; shift 2 ;;
    --out-dir) OUT_DIR="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage ;;
  esac
done

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"
[[ -z "$OUT_DIR" ]] && OUT_DIR="$REPO/k8s-acceptance-out"
mkdir -p "$OUT_DIR"
CHART="deploy/helm/nexaroute"
ACCEPT_VALUES="$CHART/values-acceptance.yaml"
ACCEPT_CONFIG="$CHART/acceptance-config.json"
STUB_MANIFEST="$CHART/acceptance-stub.yaml"

SENTINEL="SENTINEL-ACCEPT-650"
PG_SENTINEL_DSN="postgres://accept-user:${SENTINEL}-PW@nexaroute-accept-pg:5432/nexaroute"
STUB_SENTINEL_KEY="${SENTINEL}-KEY"
FAILURES=0
NOTRUNS=0

section() { printf '\n== %s ==\n' "$1"; }
ok() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; FAILURES=$((FAILURES + 1)); }
notrun() { printf 'NOT_RUN: %s\n' "$1"; NOTRUNS=$((NOTRUNS + 1)); }
need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'missing required tool: %s\n' "$1" >&2
    return 1
  fi
}

# assert_rendered <file> <grep-pattern> <label>: pass/fail without
# printing file contents (rendered manifests may embed configJSON).
assert_rendered() {
  if grep -qE -- "$2" "$1"; then ok "$3"; else fail "$3"; fi
}
assert_rendered_absent() {
  if grep -qE -- "$2" "$1"; then fail "$3"; else ok "$3"; fi
}

# leak_check <label> <file...>: fail if the sentinel marker or a
# plaintext database URL appears. Matching lines are never printed.
leak_check() {
  local label="$1"; shift
  local found=0
  for f in "$@"; do
    [[ -f "$f" ]] || continue
    if grep -qF -- "$SENTINEL" "$f" || grep -qE -- '(postgres|rediss?)://[^[:space:]"]+' "$f"; then
      found=1
    fi
  done
  if [[ "$found" -eq 1 ]]; then
    fail "secret leak check: $label (match found; line withheld, delete $OUT_DIR and rotate any real value that shares the marker)"
  else
    ok "secret leak check: $label"
  fi
}

# drain_seconds <config.json>: the gateway's SIGTERM drain cap for a
# config: max(request_timeout, stream idle timeouts, 10s) + 5s.
# (cmd/gateway/main.go: drain = max(...) with a 10s floor, plus 5s.)
drain_seconds() {
  python3 - "$1" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
req = cfg.get("routing", {}).get("request_timeout_ms", 0) / 1000.0
idle = max([p.get("stream_idle_timeout_seconds", 0)
            for p in cfg.get("providers", [])] or [0])
print(int(max(req, idle, 10) + 5))
PY
}

# check_drain_grace <config.json> <grace-seconds>: 0 when the chart
# grace strictly exceeds the gateway drain cap for that config, 1
# otherwise — including when the config is not valid JSON, which
# drain_seconds reports as a failure instead of aborting the script.
# The printed line is the only output; callers turn it into PASS/FAIL.
check_drain_grace() {
  local drain
  # The python traceback for a malformed config is suppressed here:
  # the "unreadable" line below is the diagnostic, and a traceback
  # would add noise without adding information about the failure.
  if ! drain="$(drain_seconds "$1" 2>/dev/null)"; then
    printf 'drain cap: unreadable (config is not valid JSON); termination grace: %ss\n' "$2"
    return 1
  fi
  printf 'drain cap: %ss; termination grace: %ss\n' "$drain" "$2"
  [[ "$2" -gt "$drain" ]]
}

# ---------------------------------------------------------------- offline
offline_phase() {
  section "offline: helm lint, render, posture, drain invariant"
  need helm || { fail "helm is required for offline acceptance"; return; }
  need python3 || { fail "python3 is required for offline acceptance"; return; }

  helm lint "$CHART"
  ok "helm lint"

  helm template "$RELEASE" "$CHART" -f "$ACCEPT_VALUES" > "$OUT_DIR/rendered-acceptance.yaml"
  helm template "$RELEASE" "$CHART" > "$OUT_DIR/rendered-default.yaml"
  ok "helm template (acceptance values and chart defaults)"

  for rendered in "$OUT_DIR/rendered-acceptance.yaml" "$OUT_DIR/rendered-default.yaml"; do
    section "offline assertions on $(basename "$rendered")"
    assert_rendered "$rendered" 'kind: Deployment' "Deployment rendered"
    assert_rendered "$rendered" 'kind: ServiceAccount' "ServiceAccount rendered"
    assert_rendered "$rendered" 'kind: PersistentVolumeClaim' "PVC rendered"
    assert_rendered_absent "$rendered" 'kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding)' "no RBAC objects rendered (least-privilege SA grants nothing)"
    assert_rendered "$rendered" 'automountServiceAccountToken: false' "service-account token automount disabled"
    assert_rendered "$rendered" 'runAsNonRoot: true' "runAsNonRoot"
    assert_rendered "$rendered" 'runAsUser: 65532' "non-root uid 65532"
    assert_rendered "$rendered" 'readOnlyRootFilesystem: true' "read-only root filesystem"
    assert_rendered "$rendered" 'allowPrivilegeEscalation: false' "no privilege escalation"
    assert_rendered "$rendered" 'type: RuntimeDefault' "seccomp RuntimeDefault"
    assert_rendered "$rendered" 'path: /healthz' "liveness probe /healthz"
    assert_rendered "$rendered" 'path: /readyz' "readiness probe /readyz"
    assert_rendered "$rendered" 'type: Recreate' "Recreate strategy (never two pods on one RWO claim)"
    assert_rendered "$rendered" 'terminationGracePeriodSeconds: [0-9]+' "termination grace configured"
    assert_rendered_absent "$rendered" 'value: (postgres|rediss?)://' "no plaintext database URL in rendered manifests"
    if grep -q 'image: "[^"]*latest"' "$rendered"; then
      fail "image tag must never resolve to latest"
    else
      ok "image tag is pinned (not latest)"
    fi
  done

  # Acceptance stub sidecar: present only in the acceptance render
  # (loopback stub, see values.yaml), never in the default render.
  assert_rendered "$OUT_DIR/rendered-acceptance.yaml" 'name: acceptance-stub' "acceptance stub sidecar rendered (acceptance only)"
  assert_rendered_absent "$OUT_DIR/rendered-default.yaml" 'name: acceptance-stub' "no acceptance stub sidecar in default render"
  assert_rendered_absent "$OUT_DIR/rendered-default.yaml" 'acceptance-stub-code' "no acceptance stub volume in default render"

  # Secret-reference wiring on the acceptance render.
  assert_rendered "$OUT_DIR/rendered-acceptance.yaml" 'secretKeyRef:' "control-plane / stub credentials arrive via secretKeyRef only"
  assert_rendered "$OUT_DIR/rendered-acceptance.yaml" 'name: nexaroute-accept-pg' "acceptance PG secret reference rendered"
  leak_check "rendered manifests" "$OUT_DIR/rendered-acceptance.yaml" "$OUT_DIR/rendered-default.yaml"

  # Drain invariant for the config the live run actually boots.
  local grace
  grace="$(grep -oE 'terminationGracePeriodSeconds: [0-9]+' "$OUT_DIR/rendered-acceptance.yaml" | head -1 | grep -oE '[0-9]+')"
  if check_drain_grace "$ACCEPT_CONFIG" "$grace"; then
    ok "termination grace ($grace s) exceeds gateway drain cap for the acceptance config"
  else
    fail "termination grace ($grace s) must exceed gateway drain cap for the acceptance config or in-flight streams are SIGKILLed mid-drain"
  fi
}

# ---------------------------------------------------------------- live helpers
KUBECTL_NS=()
pf_pid=""

kubectl_ns() { kubectl -n "$NAMESPACE" "$@"; }

# port_forward <local-port>: start a port-forward to the release Service
# in the background; call stop_port_forward when done.
port_forward() {
  kubectl_ns port-forward "svc/${RELEASE}-${CHART##*/}" "$1:8080" >"$OUT_DIR/port-forward.log" 2>&1 &
  pf_pid=$!
  for _ in $(seq 1 30); do
    curl -fsS "http://127.0.0.1:$1/healthz" >/dev/null 2>&1 && return 0
    sleep 2
  done
  printf 'port-forward did not become ready\n' >&2
  return 1
}
stop_port_forward() {
  if [[ -n "$pf_pid" ]]; then kill "$pf_pid" 2>/dev/null || true; pf_pid=""; fi
}

http_code() { curl -s -o "$OUT_DIR/last-body.txt" -w '%{http_code}' "$1" || echo "curl-failed"; }

# wait_pod_ready <timeout>: wait for a Running pod whose Ready condition
# is true; the selector is the chart's instance label.
wait_pod_ready() {
  kubectl_ns wait --for=condition=Ready "pod" \
    -l "app.kubernetes.io/instance=$RELEASE" --timeout="$1" >/dev/null 2>&1
}

# replace_config_secret <config.json>: the chart's existingSecret is
# operator-managed, so helm upgrade/rollback do NOT change it. The
# operator must swap it explicitly; the script does exactly that and
# says so, because a rollback that silently kept the new config would
# be a false rollback test.
replace_config_secret() {
  kubectl_ns delete secret nexaroute-accept-config --ignore-not-found >/dev/null
  kubectl_ns create secret generic nexaroute-accept-config \
    --from-file="config.json=$1" >/dev/null
}

create_dummy_secrets() {
  kubectl_ns create secret generic nexaroute-accept-pg \
    --from-literal="dsn=$PG_SENTINEL_DSN" >/dev/null
  kubectl_ns create secret generic nexaroute-accept-stub \
    --from-literal="api-key=$STUB_SENTINEL_KEY" >/dev/null
}

# assert_never_ready <label> <selector>: a fault-injected release must
# have created at least one pod (otherwise the check is vacuous — a
# release that never scheduled anything would "pass" by not existing)
# and none of its pods may report Ready. A pod that never starts for
# an unrelated reason (e.g. a missing image) still satisfies the
# count, so the captured pod YAML/logs in $OUT_DIR are the evidence
# that the failure is the injected fault, not an unrelated outage.
assert_never_ready() {
  local label="$1" selector="$2"
  local count ready
  count="$(kubectl_ns get pod -l "$selector" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w)"
  ready="$(kubectl_ns get pod -l "$selector" -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{" "}{end}' 2>/dev/null | grep -c True || true)"
  if [[ "$count" -eq 0 ]]; then
    fail "$label: no pod created — phase inconclusive, inspect $OUT_DIR"
  elif [[ "$ready" -gt 0 ]]; then
    fail "$label: a pod became Ready (fail-open)"
  else
    ok "$label: $count pod(s), none Ready (fail-closed)"
  fi
}

# cleanup_fault_release <release> <secret...>: uninstall a fault
# release and delete only its own secrets, ignoring errors so cleanup
# is idempotent and never masks the phase result. Callers verify
# absence with helm status afterwards; this helper alone is not the
# proof of cleanup.
cleanup_fault_release() {
  local rel="$1"; shift
  helm uninstall "$rel" -n "$NAMESPACE" >/dev/null 2>&1 || true
  if [[ $# -gt 0 ]]; then
    kubectl_ns delete secret "$@" --ignore-not-found >/dev/null 2>&1 || true
  fi
}

# assert_release_absent <release>: fail if helm still knows the
# release after cleanup_fault_release ran.
assert_release_absent() {
  if helm status "$1" -n "$NAMESPACE" >/dev/null 2>&1; then
    fail "fault release $1 still installed after cleanup"
  else
    ok "fault release $1 cleaned up"
  fi
}

# rendered_image_ref <chart-dir> <tag>: the exact image reference the
# chart renders for that tag under the acceptance values. The live run
# must tag and kind-load THIS string. Deriving it from the render
# (rather than hard-coding a local name) is what keeps the Docker tag,
# the kind-load name, and the Deployment's image.repository in sync —
# a mismatch with pullPolicy=Never is ErrImageNeverPull, not a pull.
rendered_image_ref() {
  local chart="$1" tag="$2" rendered
  rendered="$(helm template "$RELEASE" "$chart" -f "$ACCEPT_VALUES" \
    --set image.tag="$tag" 2>/dev/null)" || return 1
  printf '%s\n' "$rendered" \
    | grep -m1 -oE 'image: "[^"]+"' \
    | sed -e 's/^image: "//' -e 's/"$//'
}

# build_and_load_images <prev-tree> <cluster>: build both images from
# trees and load them into kind under the exact rendered references.
# With pullPolicy=Never the loaded name must equal the Deployment's
# image string byte-for-byte, including the chart's fully qualified
# repository (ghcr.io/...), or kubelet reports ErrImageNeverPull.
build_and_load_images() {
  local prev_tree="$1" cluster="$2" prev_image cur_image
  prev_image="$(rendered_image_ref "$prev_tree/$CHART" accept-prev)" || {
    fail "could not render the preceding chart image reference"; return 1; }
  cur_image="$(rendered_image_ref "$CHART" accept-current)" || {
    fail "could not render the current chart image reference"; return 1; }
  if [[ -z "$prev_image" || -z "$cur_image" ]]; then
    fail "rendered image reference is empty (prev='$prev_image' current='$cur_image')"
    return 1
  fi
  docker build -q -t "$prev_image" --build-arg "VERSION=$PREV_TAG" "$prev_tree" >/dev/null
  docker build -q -t "$cur_image" --build-arg VERSION=accept-current "$REPO" >/dev/null
  kind load docker-image "$prev_image" --name "$cluster" >/dev/null
  kind load docker-image "$cur_image" --name "$cluster" >/dev/null
  ok "preceding ($PREV_TAG) and current images built and kind-loaded as rendered: $prev_image, $cur_image"
}

# assert_kind_context: fail closed unless kubectl is talking to the
# kind cluster this run created. Used by live_phase right after
# kind create (before ANY namespace, Secret, or release is created)
# and by safe_failure_phase before any fault release is installed,
# so a stale or operator-supplied context can never redirect a
# mutation at an existing cluster. A kubectl failure (empty context)
# refuses just as a foreign context does.
assert_kind_context() {
  if [[ "$(kubectl config current-context 2>/dev/null)" != "kind-$CLUSTER" ]]; then
    fail "refused: current kubectl context is not kind-$CLUSTER"
    return 1
  fi
}
# safe_failure_phase: runs only after the lifecycle phases passed, so a
# lifecycle failure is reported as itself and never hidden behind fault
# phases. Each fault release gets its own config/Secret and its own
# PVC (the release name is part of the chart fullname), so fault
# releases cannot corrupt the main release's state. Each phase waits a
# full 120s before asserting never-Ready: a negative assertion needs
# the whole window, or a slow pod that would have become Ready could
# pass vacuously. Every fault release is uninstalled and verified
# absent before the next phase starts, and the main release is left
# healthy at the end (failed rollout -> rollback -> /readyz 200).
safe_failure_phase() {
  section "live: safe-failure phases (fault releases must never become Ready)"
  if [[ "$FAILURES" -gt 0 ]]; then
    notrun "safe-failure phases skipped: lifecycle phase failed"
    return
  fi
  # Context guard: fault phases must only ever talk to the kind
  # cluster this run created. If the context is anything else, fail
  # and touch nothing rather than installing fault releases elsewhere.
  assert_kind_context || return

  # 2a: misconfigured config (invalid JSON) must fail safe.
  local bad_release="${RELEASE}-badconfig"
  kubectl_ns create secret generic nexaroute-accept-badconfig \
    --from-literal='config.json={this is not json' >/dev/null
  helm install "$bad_release" "$CHART" -n "$NAMESPACE" -f "$ACCEPT_VALUES" \
    --set config.existingSecret=nexaroute-accept-badconfig \
    --set image.tag=accept-current --set image.pullPolicy=Never >/dev/null
  sleep 120
  assert_never_ready "invalid-JSON config release never Ready" "app.kubernetes.io/instance=$bad_release"
  kubectl_ns get pod -l "app.kubernetes.io/instance=$bad_release" -o yaml >"$OUT_DIR/badconfig-pod.yaml" 2>/dev/null || true
  kubectl_ns logs -l "app.kubernetes.io/instance=$bad_release" --tail=50 >"$OUT_DIR/badconfig-pod.log" 2>/dev/null || true
  leak_check "invalid-config fault release logs" "$OUT_DIR/badconfig-pod.log"
  cleanup_fault_release "$bad_release" nexaroute-accept-badconfig
  assert_release_absent "$bad_release"

  # 2b: an unreachable PostgreSQL control-plane backend must fail
  # safe. The gateway pings the DSN at startup and fails closed when
  # it is unreachable (see openPostgresControlPlane), so the pod must
  # never become Ready — it must not serve traffic on a dead backend.
  python3 - "$ACCEPT_CONFIG" "$OUT_DIR/deadpg-config.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
cfg["control_plane"]["enabled"] = True
cfg["control_plane"]["backend"] = "postgres"
cfg["control_plane"]["postgres_dsn_env"] = "NEXAROUTE_CONTROL_PLANE_PG_DSN"
json.dump(cfg, open(sys.argv[2], "w"), indent=2)
PY
  kubectl_ns create secret generic nexaroute-accept-deadpg \
    --from-literal="dsn=postgres://nexaroute:${SENTINEL}-DEADPG@nexaroute-accept-pg-dead:5432/nexaroute?sslmode=disable" >/dev/null
  kubectl_ns create secret generic nexaroute-accept-config-deadpg \
    --from-file="config.json=$OUT_DIR/deadpg-config.json" >/dev/null
  local dead_release="${RELEASE}-deadpg"
  helm install "$dead_release" "$CHART" -n "$NAMESPACE" -f "$ACCEPT_VALUES" \
    --set config.existingSecret=nexaroute-accept-config-deadpg \
    --set controlPlane.postgresDSN.secretName=nexaroute-accept-deadpg \
    --set image.tag=accept-current --set image.pullPolicy=Never >/dev/null
  sleep 120
  assert_never_ready "gateway with unreachable control-plane backend never Ready" "app.kubernetes.io/instance=$dead_release"
  kubectl_ns get pod -l "app.kubernetes.io/instance=$dead_release" -o yaml >"$OUT_DIR/deadpg-pod.yaml" 2>/dev/null || true
  kubectl_ns logs -l "app.kubernetes.io/instance=$dead_release" --tail=50 >"$OUT_DIR/deadpg-pod.log" 2>/dev/null || true
  leak_check "dead-backend fault release logs/describe" "$OUT_DIR/deadpg-pod.yaml" "$OUT_DIR/deadpg-pod.log"
  cleanup_fault_release "$dead_release" nexaroute-accept-deadpg nexaroute-accept-config-deadpg
  assert_release_absent "$dead_release"

  # 2c: a missing referenced Secret must fail safe. Helm renders the
  # secretKeyRef without checking existence, so this is a runtime
  # failure: kubelet cannot create the container, the pod is never
  # Ready, and no traffic is served. The config Secret here is the
  # main release's acceptance config (it exists); only the database
  # Secret is missing, isolating exactly one fault.
  local missing_release="${RELEASE}-missingsecret"
  helm install "$missing_release" "$CHART" -n "$NAMESPACE" -f "$ACCEPT_VALUES" \
    --set controlPlane.postgresDSN.secretName=nexaroute-accept-does-not-exist \
    --set image.tag=accept-current --set image.pullPolicy=Never >/dev/null
  sleep 120
  assert_never_ready "gateway with missing referenced Secret never Ready" "app.kubernetes.io/instance=$missing_release"
  kubectl_ns get events --field-selector "involvedObject.name=$missing_release-nexaroute" -o yaml >"$OUT_DIR/missingsecret-events.yaml" 2>/dev/null || true
  leak_check "missing-Secret fault release events" "$OUT_DIR/missingsecret-events.yaml"
  cleanup_fault_release "$missing_release"
  assert_release_absent "$missing_release"

  # Failed rollout: upgrading the main release to an image that is
  # not loaded into kind (pullPolicy Never) must never become Ready,
  # and rolling back must restore the previous, healthy release.
  # helm rollback without a revision argument rolls back to the
  # previous revision — exactly the release the failed upgrade
  # replaced — rather than to a revision number this script assumes.
  helm upgrade "$RELEASE" "$CHART" -n "$NAMESPACE" -f "$ACCEPT_VALUES" \
    --set image.tag=accept-nonexistent --set image.pullPolicy=Never >/dev/null
  sleep 120
  assert_never_ready "failed rollout (unpullable image) never Ready" "app.kubernetes.io/instance=$RELEASE"
  helm rollback "$RELEASE" -n "$NAMESPACE" >/dev/null
  if wait_pod_ready 300s; then ok "rollback after failed rollout: pod Ready"; else fail "rollback after failed rollout: pod did not become Ready"; fi
  port_forward 18080
  if [[ "$(http_code http://127.0.0.1:18080/readyz)" == "200" ]]; then
    ok "rollback after failed rollout: /readyz 200 again"
  else fail "rollback after failed rollout: /readyz not 200"; fi
  stop_port_forward
}

# ---------------------------------------------------------------- live
live_phase() {
  section "live: disposable kind cluster acceptance"
  local missing=0
  for tool in kind kubectl docker helm curl python3 git; do
    need "$tool" || missing=1
  done
  if [[ "$missing" -eq 1 ]]; then
    notrun "live acceptance: required tools missing (see above)"
    return 3
  fi
  if ! git rev-parse --verify --quiet "$PREV_TAG" >/dev/null; then
    notrun "live acceptance: preceding tag $PREV_TAG not present in this checkout (fetch tags first)"
    return 3
  fi
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    printf 'refusing to touch an existing kind cluster named %s\n' "$CLUSTER" >&2
    return 1
  fi

  kind create cluster --name "$CLUSTER" --wait 180s >/dev/null
  ok "kind cluster $CLUSTER created (disposable)"
  if [[ "$KEEP_CLUSTER" -eq 0 ]]; then
    trap 'kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true' EXIT
  else
    printf 'keeping cluster %s as requested (--keep-cluster)\n' "$CLUSTER"
  fi
  # Context guard BEFORE any mutation: kind create is expected to
  # switch the current context to kind-$CLUSTER, but a KUBECONFIG
  # pointing elsewhere (or a kind that did not switch) must never let
  # the namespace, Secret, or release steps below land on an existing
  # operator cluster. The EXIT trap above still deletes the kind
  # cluster this run just created.
  assert_kind_context || return 1
  kubectl create namespace "$NAMESPACE" >/dev/null
  kubectl_ns apply -f "$STUB_MANIFEST" >/dev/null
  kubectl_ns wait --for=condition=available "deploy/nexaroute-stub" --timeout=300s >/dev/null
  ok "stub upstream ready"

  create_dummy_secrets
  ok "dummy external secrets created (sentinel values only)"

  # Build both images from trees, load them into kind (never pulled)
  # under the exact image references the chart renders.
  local prev_tree="$OUT_DIR/prev-tree"
  rm -rf "$prev_tree"
  mkdir -p "$prev_tree"
  git archive "$PREV_TAG" | tar -x -C "$prev_tree"
  build_and_load_images "$prev_tree" "$CLUSTER" || return 1

  # Base install: preceding chart, empty-provider config.
  python3 - "$ACCEPT_CONFIG" "$OUT_DIR/base-config.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
cfg["providers"] = []
json.dump(cfg, open(sys.argv[2], "w"), indent=2)
PY
  replace_config_secret "$OUT_DIR/base-config.json"
  helm install "$RELEASE" "$prev_tree/$CHART" -n "$NAMESPACE" \
    -f "$ACCEPT_VALUES" \
    --set image.tag=accept-prev --set image.pullPolicy=Never >/dev/null
  kubectl_ns wait --for=jsonpath='{.status.phase}'=Running "pod" \
    -l "app.kubernetes.io/instance=$RELEASE" --timeout=300s >/dev/null
  ok "base release ($PREV_TAG chart) installed, pod Running"

  # Readiness semantics on the empty-provider install: /healthz is 200
  # (the process is alive) while /readyz is 503 by design -- there is
  # no usable deployment to route to. A pod that is never Ready here
  # is the gateway telling the truth, not a failed install.
  port_forward 18080
  if [[ "$(http_code http://127.0.0.1:18080/healthz)" == "200" ]]; then
    ok "base /healthz 200"
  else fail "base /healthz not 200"; fi
  if [[ "$(http_code http://127.0.0.1:18080/readyz)" == "503" ]]; then
    ok "base /readyz 503 as designed (no enabled deployments)"
  else fail "base /readyz expected 503 with no providers"; fi
  stop_port_forward

  # Upgrade: swap the operator-managed config first, then the chart.
  replace_config_secret "$ACCEPT_CONFIG"
  helm upgrade "$RELEASE" "$CHART" -n "$NAMESPACE" \
    -f "$ACCEPT_VALUES" \
    --set image.tag=accept-current --set image.pullPolicy=Never >/dev/null
  if wait_pod_ready 300s; then ok "upgraded pod Ready"; else fail "upgraded pod did not become Ready"; fi
  port_forward 18080
  if [[ "$(http_code http://127.0.0.1:18080/healthz)" == "200" ]]; then
    ok "upgraded /healthz 200"
  else fail "upgraded /healthz not 200"; fi
  if [[ "$(http_code http://127.0.0.1:18080/readyz)" == "200" ]]; then
    ok "upgraded /readyz 200 (stub deployment healthy via probe priming)"
  else fail "upgraded /readyz not 200"; fi
  if curl -fsS http://127.0.0.1:18080/v1/models | grep -q stub-model; then
    ok "upgraded /v1/models serves stub-model"
  else fail "upgraded /v1/models did not serve stub-model"; fi
  # Chat path is recorded, not gating: the stub approximates an upstream
  # and a chat failure would be a stub-fidelity question, not a chart
  # defect. The readiness result above is the gating probe evidence.
  printf 'chat smoke (recorded, non-gating): HTTP %s\n' \
    "$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18080/v1/chat/completions \
      -H 'Content-Type: application/json' \
      -d '{"model":"stub-model","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' || echo curl-failed)"
  stop_port_forward

  # Live posture from the running pod spec (not just the render).
  local pod_json="$OUT_DIR/live-pod.json"
  kubectl_ns get pod -l "app.kubernetes.io/instance=$RELEASE" -o json >"$pod_json"
  if python3 - "$pod_json" <<'PY'
import json, sys
pods = json.load(open(sys.argv[1]))["items"]
assert pods, "no gateway pod found"
spec = pods[0]["spec"]
c = spec["containers"][0]
checks = [
    ("pod automountServiceAccountToken false", spec.get("automountServiceAccountToken") is False),
    ("pod runAsNonRoot", spec["securityContext"].get("runAsNonRoot") is True),
    ("pod runAsUser 65532", spec["securityContext"].get("runAsUser") == 65532),
    ("container readOnlyRootFilesystem", c["securityContext"].get("readOnlyRootFilesystem") is True),
    ("container allowPrivilegeEscalation false", c["securityContext"].get("allowPrivilegeEscalation") is False),
    ("container drops ALL capabilities", c["securityContext"].get("capabilities", {}).get("drop") == ["ALL"]),
    ("termination grace 120s", spec.get("terminationGracePeriodSeconds") == 120),
]
failed = [name for name, good in checks if not good]
for name, good in checks:
    print(("PASS: live " if good else "FAIL: live ") + name)
sys.exit(1 if failed else 0)
PY
  then
    ok "live pod security posture"
  else
    fail "live pod security posture (see lines above)"
  fi

  # Least-privilege service account: no RBAC objects for this release.
  if kubectl_ns get role,rolebinding -l "app.kubernetes.io/instance=$RELEASE" -o name 2>/dev/null | grep -q .; then
    fail "unexpected RBAC objects for the release service account"
  else
    ok "no Role/RoleBinding for the release service account"
  fi
  if kubectl_ns auth can-i list pods --as="system:serviceaccount:${NAMESPACE}:${RELEASE}-nexaroute" 2>/dev/null | grep -qx no; then
    ok "release service account cannot list pods"
  else
    notrun "service-account can-i check (impersonation not permitted for this operator; RBAC absence above is the evidence)"
  fi

  # Persistence + graceful termination, measured in one pod restart:
  # backup to the PVC, delete the pod with the chart's grace period,
  # and require the replacement to become Ready on the same claim.
  kubectl_ns exec "deploy/${RELEASE}-nexaroute" -- /nexaroute backup \
    --config /config/config.json --out /config/backup-accept.tar >"$OUT_DIR/backup.log" 2>&1
  ok "backup to PVC succeeded before restart"
  local pvc_uid pod_name start elapsed
  pvc_uid="$(kubectl_ns get pvc -l "app.kubernetes.io/instance=$RELEASE" -o jsonpath='{.items[0].metadata.uid}')"
  pod_name="$(kubectl_ns get pod -l "app.kubernetes.io/instance=$RELEASE" -o jsonpath='{.items[0].metadata.name}')"
  start="$(date +%s)"
  kubectl_ns delete pod "$pod_name" --grace-period=120 --timeout=180s >/dev/null
  elapsed=$(( $(date +%s) - start ))
  printf 'pod termination took %ss (grace 120s)\n' "$elapsed"
  if [[ "$elapsed" -lt 120 ]]; then ok "pod terminated inside its grace period"; else fail "pod termination exceeded its grace period"; fi
  if wait_pod_ready 300s; then ok "replacement pod Ready after restart"; else fail "replacement pod did not become Ready"; fi
  if [[ "$(kubectl_ns get pvc -l "app.kubernetes.io/instance=$RELEASE" -o jsonpath='{.items[0].metadata.uid}')" == "$pvc_uid" ]]; then
    ok "same PVC retained across pod restart"
  else fail "PVC changed across pod restart"; fi
  kubectl_ns exec "deploy/${RELEASE}-nexaroute" -- /nexaroute doctor \
    --config /config/config.json >"$OUT_DIR/doctor-after-restart.log" 2>&1
  ok "doctor passes after restart on the retained PVC"

  # Rollback: swap the operator-managed config back FIRST (helm
  # rollback does not touch it), then roll back to revision 1.
  replace_config_secret "$OUT_DIR/base-config.json"
  helm rollback "$RELEASE" 1 -n "$NAMESPACE" >/dev/null
  kubectl_ns wait --for=jsonpath='{.status.phase}'=Running "pod" \
    -l "app.kubernetes.io/instance=$RELEASE" --timeout=300s >/dev/null
  port_forward 18080
  if [[ "$(http_code http://127.0.0.1:18080/readyz)" == "503" ]]; then
    ok "rolled back: /readyz 503 again (preceding chart + base config)"
  else fail "rolled back /readyz expected 503"; fi
  stop_port_forward

  # Forward again so the cluster is left on the current chart.
  replace_config_secret "$ACCEPT_CONFIG"
  helm upgrade "$RELEASE" "$CHART" -n "$NAMESPACE" \
    -f "$ACCEPT_VALUES" \
    --set image.tag=accept-current --set image.pullPolicy=Never >/dev/null
  if wait_pod_ready 300s; then ok "re-upgraded pod Ready"; else fail "re-upgraded pod did not become Ready"; fi

  safe_failure_phase

  # Leak check over live evidence (never over Secret objects).
  kubectl_ns get events -o yaml >"$OUT_DIR/events.yaml" 2>/dev/null || true
  kubectl_ns logs "deploy/${RELEASE}-nexaroute" >"$OUT_DIR/pod.log" 2>/dev/null || true
  kubectl_ns describe "deploy/${RELEASE}-nexaroute" >"$OUT_DIR/describe-deploy.txt" 2>/dev/null || true
  kubectl_ns get pod -l "app.kubernetes.io/instance=$RELEASE" -o yaml >"$OUT_DIR/live-pod-describe.yaml"
  leak_check "live events/logs/describe output" \
    "$OUT_DIR/events.yaml" "$OUT_DIR/pod.log" "$OUT_DIR/describe-deploy.txt" "$OUT_DIR/live-pod-describe.yaml"

  if [[ "$WITH_POSTGRES" -eq 1 ]]; then
    postgres_phase
  fi

  helm uninstall "$RELEASE" -n "$NAMESPACE" >/dev/null
  ok "release uninstalled (cluster deleted on exit unless --keep-cluster)"
}

# Optional phase: a real ephemeral PostgreSQL as the control-plane
# backend, wired only through a secret reference. Runs after the main
# phases so a database failure cannot mask the core lifecycle result.
# Ordering matters and is explicit: the password/DSN secret is created
# BEFORE the PostgreSQL Deployment that references it, and the gateway
# config secret is created before the gateway release that mounts it.
postgres_phase() {
  section "live (optional): PostgreSQL control-plane backend"
  local pg_release="${RELEASE}-pg"
  kubectl_ns create secret generic nexaroute-accept-pg-real \
    --from-literal="password=${SENTINEL}-PGREAL" \
    --from-literal="dsn=postgres://nexaroute:${SENTINEL}-PGREAL@nexaroute-accept-pg:5432/nexaroute?sslmode=disable" >/dev/null
  kubectl_ns apply -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nexaroute-accept-pg
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nexaroute-accept-pg
  template:
    metadata:
      labels:
        app: nexaroute-accept-pg
    spec:
      automountServiceAccountToken: false
      containers:
        - name: postgres
          image: postgres:16-alpine
          env:
            - name: POSTGRES_USER
              value: nexaroute
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: nexaroute-accept-pg-real
                  key: password
            - name: POSTGRES_DB
              value: nexaroute
          ports:
            - containerPort: 5432
---
apiVersion: v1
kind: Service
metadata:
  name: nexaroute-accept-pg
spec:
  type: ClusterIP
  selector:
    app: nexaroute-accept-pg
  ports:
    - port: 5432
YAML
  kubectl_ns wait --for=condition=available "deploy/nexaroute-accept-pg" --timeout=300s >/dev/null
  ok "ephemeral PostgreSQL ready"
  python3 - "$ACCEPT_CONFIG" "$OUT_DIR/pg-config.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
cfg["control_plane"]["enabled"] = True
cfg["control_plane"]["backend"] = "postgres"
cfg["control_plane"]["postgres_dsn_env"] = "NEXAROUTE_CONTROL_PLANE_PG_DSN"
json.dump(cfg, open(sys.argv[2], "w"), indent=2)
PY
  kubectl_ns create secret generic nexaroute-accept-config-pg \
    --from-file="config.json=$OUT_DIR/pg-config.json" >/dev/null
  helm install "$pg_release" "$CHART" -n "$NAMESPACE" \
    -f "$ACCEPT_VALUES" \
    --set config.existingSecret=nexaroute-accept-config-pg \
    --set controlPlane.postgresDSN.secretName=nexaroute-accept-pg-real \
    --set image.tag=accept-current --set image.pullPolicy=Never >/dev/null
  # The pg release's pods carry instance label $pg_release, not $RELEASE;
  # wait on them directly.
  kubectl_ns wait --for=condition=Ready "pod" \
    -l "app.kubernetes.io/instance=$pg_release" --timeout=300s >/dev/null
  ok "PostgreSQL-backed gateway Ready (DSN via secretKeyRef only)"
  helm uninstall "$pg_release" -n "$NAMESPACE" >/dev/null
  ok "PostgreSQL-backed release uninstalled"
}

# ---------------------------------------------------------------- self-test
# selftest_tool_bin <dir>: symlink a fixed set of ordinary tools into
# a directory, deliberately excluding kind, kubectl and docker, so a
# subprocess run with that PATH tests the missing-tools gate even on
# a machine that has those tools installed.
selftest_tool_bin() {
  local tool
  for tool in bash grep sed mkdir rm cp ln cat sleep date wc head tr chmod python3 helm git curl dirname basename seq tar; do
    if command -v "$tool" >/dev/null 2>&1; then
      ln -sf "$(command -v "$tool")" "$1/$tool"
    fi
  done
}

# selftest_phase: deterministic, cluster-free tests of the failure
# paths. Nothing here creates a cluster, installs a release, or needs
# credentials; the live-tool subprocesses use stub binaries that log
# every invocation, so "no cluster created" is asserted from the log,
# not assumed from the exit code.
selftest_phase() {
  section "self-test: deterministic safe-failure checks (no cluster)"
  need helm || { fail "helm is required for self-test"; return; }
  need python3 || { fail "python3 is required for self-test"; return; }

  local tmp="$OUT_DIR/selftest-tmp"
  rm -rf "$tmp"
  mkdir -p "$tmp/bin" "$tmp/bin2" "$tmp/bin3" "$tmp/cleanup-bin"
  selftest_tool_bin "$tmp/bin"
  selftest_tool_bin "$tmp/bin2"
  selftest_tool_bin "$tmp/bin3"

  # Stub live tools for bin2/bin3. The kind stub logs every call; the
  # bin3 variant additionally reports the acceptance cluster as
  # already existing. kubectl/docker stubs only need to exist in PATH.
  cat >"$tmp/bin2/kind" <<'EOF'
#!/bin/sh
echo "kind $@" >> "$SELFTEST_KIND_LOG"
exit 0
EOF
  cat >"$tmp/bin3/kind" <<'EOF'
#!/bin/sh
echo "kind $@" >> "$SELFTEST_KIND_LOG"
if [ "$1" = "get" ] && [ "$2" = "clusters" ]; then echo "nexaroute-accept"; fi
exit 0
EOF
  chmod +x "$tmp/bin2/kind" "$tmp/bin3/kind"
  for stub in kubectl docker; do
    printf '#!/bin/sh\nexit 0\n' >"$tmp/bin2/$stub"
    printf '#!/bin/sh\nexit 0\n' >"$tmp/bin3/$stub"
    chmod +x "$tmp/bin2/$stub" "$tmp/bin3/$stub"
  done

  # 1. Missing live tools -> exit 3 NOT_RUN, before any cluster work.
  local code=0
  SELFTEST_KIND_LOG="$tmp/kind-missing.log" PATH="$tmp/bin" \
    "$BASH" "$REPO/scripts/k8s-acceptance.sh" --live --out-dir "$tmp/missing-out" \
    >"$tmp/missing.log" 2>&1 || code=$?
  if [[ "$code" -eq 3 ]] && grep -q "RESULT: NOT_RUN" "$tmp/missing.log" \
      && grep -q "missing required tool: kind" "$tmp/missing.log" \
      && grep -q "missing required tool: kubectl" "$tmp/missing.log" \
      && grep -q "missing required tool: docker" "$tmp/missing.log"; then
    ok "missing live tools -> exit 3 NOT_RUN (not a pass, not a cluster)"
  else
    fail "missing live tools did not exit 3 NOT_RUN (code $code; see $tmp/missing.log)"
  fi

  # 2. Missing preceding tag -> exit 3 NOT_RUN, kind never asked to
  # create a cluster (the stub log is the evidence, not the exit code).
  code=0
  SELFTEST_KIND_LOG="$tmp/kind-tag.log" PATH="$tmp/bin2" \
    "$BASH" "$REPO/scripts/k8s-acceptance.sh" --live --prev-tag v9.9.9-does-not-exist \
    --out-dir "$tmp/tag-out" >"$tmp/tag.log" 2>&1 || code=$?
  if [[ "$code" -eq 3 ]] && grep -q "RESULT: NOT_RUN" "$tmp/tag.log" \
      && { [[ ! -f "$tmp/kind-tag.log" ]] || ! grep -q "create cluster" "$tmp/kind-tag.log"; }; then
    ok "missing preceding tag -> exit 3 NOT_RUN, no kind cluster creation attempted"
  else
    fail "missing preceding tag did not exit 3 NOT_RUN cleanly (code $code; see $tmp/tag.log)"
  fi

  # 3. Already-existing kind cluster -> refusal (exit 1), again with
  # no create-cluster call. The preceding "tag" here is the current
  # HEAD SHA, which rev-parse accepts, so the tag gate cannot mask
  # the cluster-exists gate.
  local head_sha
  head_sha="$(git rev-parse HEAD 2>/dev/null || true)"
  if [[ -z "$head_sha" ]]; then
    notrun "existing-cluster refusal (no HEAD SHA available)"
  else
    code=0
    SELFTEST_KIND_LOG="$tmp/kind-exists.log" PATH="$tmp/bin3" \
      "$BASH" "$REPO/scripts/k8s-acceptance.sh" --live --prev-tag "$head_sha" \
      --out-dir "$tmp/exists-out" >"$tmp/exists.log" 2>&1 || code=$?
    if [[ "$code" -eq 1 ]] && grep -q "refusing to touch an existing kind cluster" "$tmp/exists.log" \
        && { [[ ! -f "$tmp/kind-exists.log" ]] || ! grep -q "create cluster" "$tmp/kind-exists.log"; }; then
      ok "existing kind cluster -> refused, no kind cluster creation attempted"
    else
      fail "existing kind cluster was not refused cleanly (code $code; see $tmp/exists.log)"
    fi
  fi

  # 4. Foreign kubectl context -> live refuses BEFORE any mutation.
  # The kind stub "creates" a cluster; the kubectl stub reports a
  # foreign current-context. The guard in live_phase must fire right
  # after kind create, so the kubectl log must show the context query
  # and no namespace, Secret, or release creation at all.
  mkdir -p "$tmp/bin4"
  selftest_tool_bin "$tmp/bin4"
  rm -f "$tmp/bin4/kind" "$tmp/bin4/kubectl" "$tmp/bin4/docker"
  cat >"$tmp/bin4/kind" <<'EOF'
#!/bin/sh
echo "kind $@" >> "$SELFTEST_KIND_LOG"
exit 0
EOF
  cat >"$tmp/bin4/kubectl" <<'EOF'
#!/bin/sh
echo "kubectl $@" >> "$SELFTEST_KUBECTL_LOG"
if [ "$1" = "config" ] && [ "$2" = "current-context" ]; then echo "foreign-context"; fi
exit 0
EOF
  printf '#!/bin/sh\nexit 0\n' >"$tmp/bin4/docker"
  chmod +x "$tmp/bin4/kind" "$tmp/bin4/kubectl" "$tmp/bin4/docker"
  code=0
  if [[ -z "$head_sha" ]]; then
    notrun "foreign-context refusal (no HEAD SHA available)"
  else
  SELFTEST_KIND_LOG="$tmp/kind-ctx.log" SELFTEST_KUBECTL_LOG="$tmp/kubectl-ctx.log" PATH="$tmp/bin4" \
    "$BASH" "$REPO/scripts/k8s-acceptance.sh" --live --prev-tag "$head_sha" \
    --out-dir "$tmp/ctx-out" >"$tmp/ctx.log" 2>&1 || code=$?
  if [[ "$code" -eq 1 ]] && grep -q "refused: current kubectl context is not kind-nexaroute-accept" "$tmp/ctx.log" \
      && grep -q "create cluster" "$tmp/kind-ctx.log" \
      && ! grep -q "create namespace" "$tmp/kubectl-ctx.log" \
      && ! grep -q "create secret" "$tmp/kubectl-ctx.log"; then
    ok "foreign kubectl context -> live refused before any namespace/Secret mutation"
  else
    fail "foreign kubectl context was not refused before mutation (code $code; see $tmp/ctx.log)"
  fi
  fi

  # 5. Drain invariant: acceptance config passes, an oversized config
  # and a too-small grace fail, and a malformed config fails without
  # crashing the script (check_drain_grace, not a bare drain_seconds).
  helm template "$RELEASE" "$CHART" -f "$ACCEPT_VALUES" >"$tmp/rendered-acceptance.yaml"
  local grace
  grace="$(grep -oE 'terminationGracePeriodSeconds: [0-9]+' "$tmp/rendered-acceptance.yaml" | head -1 | grep -oE '[0-9]+')"
  if check_drain_grace "$ACCEPT_CONFIG" "$grace"; then
    ok "self-test: acceptance config drain cap below chart grace"
  else
    fail "self-test: acceptance config should pass the drain invariant"
  fi
  python3 - "$ACCEPT_CONFIG" "$tmp/big-config.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
cfg["routing"]["request_timeout_ms"] = 300000
json.dump(cfg, open(sys.argv[2], "w"), indent=2)
PY
  if check_drain_grace "$tmp/big-config.json" "$grace"; then
    fail "self-test: oversized config drain cap should exceed the chart grace"
  else
    ok "self-test: oversized config correctly fails the drain invariant"
  fi
  if check_drain_grace "$ACCEPT_CONFIG" 10; then
    fail "self-test: a 10s grace should fail against the acceptance config drain cap"
  else
    ok "self-test: too-small termination grace correctly fails the drain invariant"
  fi
  printf '{this is not json' >"$tmp/bad-config.json"
  if check_drain_grace "$tmp/bad-config.json" "$grace"; then
    fail "self-test: malformed config should fail the drain check"
  else
    ok "self-test: malformed config fails the drain check without crashing"
  fi

  # 6. Missing referenced Secret renders as a reference only: helm
  # does not check existence, so the live never-Ready phase is the
  # runtime proof, and the render must never contain a plaintext DSN.
  helm template "$RELEASE" "$CHART" -f "$ACCEPT_VALUES" \
    --set controlPlane.postgresDSN.secretName=nexaroute-accept-does-not-exist \
    >"$tmp/rendered-missingsecret.yaml"
  if grep -q "name: nexaroute-accept-does-not-exist" "$tmp/rendered-missingsecret.yaml" \
      && grep -q "secretKeyRef:" "$tmp/rendered-missingsecret.yaml"; then
    ok "self-test: missing Secret renders as a secretKeyRef only"
  else
    fail "self-test: missing Secret did not render as a secretKeyRef"
  fi
  if grep -qE 'value: (postgres|rediss?)://' "$tmp/rendered-missingsecret.yaml"; then
    fail "self-test: missing-Secret render must not contain a plaintext DSN"
  else
    ok "self-test: missing-Secret render contains no plaintext DSN"
  fi

  # 7. Fault-release cleanup helper: uninstall + secret deletion are
  # invoked, and a second call still succeeds (idempotent).
  cat >"$tmp/cleanup-bin/helm" <<'EOF'
#!/bin/sh
echo "helm $@" >> "$SELFTEST_CLEANUP_LOG"
exit 0
EOF
  cat >"$tmp/cleanup-bin/kubectl" <<'EOF'
#!/bin/sh
echo "kubectl $@" >> "$SELFTEST_CLEANUP_LOG"
exit 0
EOF
  chmod +x "$tmp/cleanup-bin/helm" "$tmp/cleanup-bin/kubectl"
  local saved_path="$PATH"
  PATH="$tmp/cleanup-bin:/bin:/usr/bin" SELFTEST_CLEANUP_LOG="$tmp/cleanup.log" \
    cleanup_fault_release "nexaroute-fault-test" nexaroute-accept-fault-secret
  PATH="$tmp/cleanup-bin:/bin:/usr/bin" SELFTEST_CLEANUP_LOG="$tmp/cleanup2.log" \
    cleanup_fault_release "nexaroute-fault-test" nexaroute-accept-fault-secret
  PATH="$saved_path"
  if grep -q "helm uninstall nexaroute-fault-test" "$tmp/cleanup.log" \
      && grep -q "delete secret nexaroute-accept-fault-secret" "$tmp/cleanup.log" \
      && grep -q "helm uninstall nexaroute-fault-test" "$tmp/cleanup2.log"; then
    ok "self-test: fault-release cleanup is invoked and idempotent"
  else
    fail "self-test: fault-release cleanup helper did not invoke uninstall + secret deletion twice"
  fi

  # 8. Image-reference parity (regression for the 2026-10-31 live
  # failure): docker tags and kind load names must be exactly the image
  # the rendered Deployment requests. The failure was building
  # `nexaroute:accept-current` while the chart requested
  # `ghcr.io/ali-shortcuts/nexaroute:accept-current` with pullPolicy
  # Never -> ErrImageNeverPull. Stub docker/kind log their real argv;
  # the expected references come from an independent helm template.
  mkdir -p "$tmp/image-bin"
  selftest_tool_bin "$tmp/image-bin"
  rm -f "$tmp/image-bin/docker" "$tmp/image-bin/kind"
  cat >"$tmp/image-bin/docker" <<'EOF'
#!/bin/sh
echo "docker $@" >> "$SELFTEST_IMAGE_LOG"
exit 0
EOF
  cat >"$tmp/image-bin/kind" <<'EOF'
#!/bin/sh
echo "kind $@" >> "$SELFTEST_IMAGE_LOG"
exit 0
EOF
  chmod +x "$tmp/image-bin/docker" "$tmp/image-bin/kind"
  local image_log="$tmp/image.log"
  : >"$image_log"
  saved_path="$PATH"
  local image_rc=0
  PATH="$tmp/image-bin:/bin:/usr/bin" SELFTEST_IMAGE_LOG="$image_log" \
    build_and_load_images "$REPO" "$CLUSTER" || image_rc=$?
  PATH="$saved_path"
  local expect_prev expect_cur
  expect_prev="$(helm template "$RELEASE" "$CHART" -f "$ACCEPT_VALUES" \
    --set image.tag=accept-prev | grep -m1 -oE 'image: "[^"]+"' \
    | sed -e 's/^image: "//' -e 's/"$//')"
  expect_cur="$(helm template "$RELEASE" "$CHART" -f "$ACCEPT_VALUES" \
    --set image.tag=accept-current | grep -m1 -oE 'image: "[^"]+"' \
    | sed -e 's/^image: "//' -e 's/"$//')"
  if [[ "$image_rc" -ne 0 ]]; then
    fail "self-test: build_and_load_images failed (rc $image_rc; see $image_log)"
  elif [[ -z "$expect_prev" || -z "$expect_cur" ]]; then
    fail "self-test: could not render expected image references"
  elif grep -qF -- "-t $expect_prev " "$image_log" \
      && grep -qF -- "load docker-image $expect_prev " "$image_log" \
      && grep -qF -- "-t $expect_cur " "$image_log" \
      && grep -qF -- "load docker-image $expect_cur " "$image_log"; then
    ok "self-test: docker tags and kind load names match rendered Deployment images (accept-prev, accept-current)"
  else
    fail "self-test: docker/kind image references do not match the rendered Deployment (expected $expect_prev and $expect_cur; see $image_log)"
  fi
}

section "NexaRoute Kubernetes acceptance ($MODE)"
if [[ "$MODE" == "selftest" ]]; then
  selftest_phase
  printf '\nRESULT: failures=%s not_run=%s\n' "$FAILURES" "$NOTRUNS"
  printf 'evidence dir: %s\n' "$OUT_DIR"
  if [[ "$FAILURES" -gt 0 ]]; then exit 1; fi
  exit 0
fi
offline_phase
if [[ "$MODE" == "live" ]]; then
  live_code=0
  live_phase || live_code=$?
  if [[ "$live_code" -eq 3 && "$FAILURES" -eq 0 ]]; then
    printf '\nRESULT: NOT_RUN (live prerequisites missing; offline phases above still stand)\n'
    exit 3
  fi
  if [[ "$live_code" -ne 0 ]]; then
    printf '\nRESULT: live phase failed (code %s)\n' "$live_code" >&2
    exit 1
  fi
fi

printf '\nRESULT: failures=%s not_run=%s\n' "$FAILURES" "$NOTRUNS"
printf 'evidence dir: %s\n' "$OUT_DIR"
if [[ "$FAILURES" -gt 0 ]]; then exit 1; fi
exit 0

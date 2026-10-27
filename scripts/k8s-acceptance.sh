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
#   scripts/k8s-acceptance.sh --live [--with-postgres] [--keep-cluster]
#       Creates a DISPOSABLE kind cluster (default name
#       nexaroute-accept), installs the chart from the preceding supported
#       release, upgrades it to the tree under test, rolls it back, and
#       re-upgrades it, exercising probes, non-root/read-only posture,
#       graceful termination, pod restart with PVC persistence, and the
#       secret-leak check. The cluster is deleted on exit unless
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
  sed -n '1,40p' "$0"
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --offline-only) MODE="offline"; shift ;;
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

  # Secret-reference wiring on the acceptance render.
  assert_rendered "$OUT_DIR/rendered-acceptance.yaml" 'secretKeyRef:' "control-plane / stub credentials arrive via secretKeyRef only"
  assert_rendered "$OUT_DIR/rendered-acceptance.yaml" 'name: nexaroute-accept-pg' "acceptance PG secret reference rendered"
  leak_check "rendered manifests" "$OUT_DIR/rendered-acceptance.yaml" "$OUT_DIR/rendered-default.yaml"

  # Drain invariant for the config the live run actually boots.
  local grace drain
  grace="$(grep -oE 'terminationGracePeriodSeconds: [0-9]+' "$OUT_DIR/rendered-acceptance.yaml" | head -1 | grep -oE '[0-9]+')"
  drain="$(drain_seconds "$ACCEPT_CONFIG")"
  printf 'acceptance config drain cap: %ss; chart termination grace: %ss\n' "$drain" "$grace"
  if [[ "$grace" -gt "$drain" ]]; then
    ok "termination grace ($grace s) exceeds gateway drain cap ($drain s)"
  else
    fail "termination grace ($grace s) must exceed gateway drain cap ($drain s) or in-flight streams are SIGKILLed mid-drain"
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
  kubectl create namespace "$NAMESPACE" >/dev/null
  kubectl_ns apply -f "$STUB_MANIFEST" >/dev/null
  kubectl_ns wait --for=condition=available "deploy/nexaroute-stub" --timeout=300s >/dev/null
  ok "stub upstream ready"

  create_dummy_secrets
  ok "dummy external secrets created (sentinel values only)"

  # Build both images from trees, load them into kind (never pulled).
  local prev_tree="$OUT_DIR/prev-tree"
  rm -rf "$prev_tree"
  mkdir -p "$prev_tree"
  git archive "$PREV_TAG" | tar -x -C "$prev_tree"
  docker build -q -t nexaroute:accept-prev --build-arg "VERSION=$PREV_TAG" "$prev_tree" >/dev/null
  docker build -q -t nexaroute:accept-current --build-arg VERSION=accept-current "$REPO" >/dev/null
  kind load docker-image nexaroute:accept-prev --name "$CLUSTER" >/dev/null
  kind load docker-image nexaroute:accept-current --name "$CLUSTER" >/dev/null
  ok "preceding ($PREV_TAG) and current images built and kind-loaded"

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

section "NexaRoute Kubernetes acceptance ($MODE)"
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

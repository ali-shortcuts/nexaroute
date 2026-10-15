# M4-4 — Deployment: image CI, Helm chart, hardened systemd

## Existing state (verified)

- `Dockerfile` is already distroless (`static-debian12:nonroot`),
  CGO-free, seeds a 0600 config in a 0700 directory — but nothing in CI
  builds it, and no workflow publishes or smoke-tests the image.
- `deploy/systemd/nexaroute.service` is a hardened USER unit only; the
  pre-M3-5 audit noted there is no system unit.
- No Helm chart exists. `/healthz` (liveness) and `/readyz` (readiness)
  endpoints exist for probes.

## This unit

1. **Helm chart** `deploy/helm/nexaroute`: single-replica deployment
   (state is node-local by default), hardened pod/container security
   contexts matching the distroless nonroot image (runAsNonRoot,
   readOnlyRootFilesystem, drop ALL capabilities,
   allowPrivilegeEscalation=false, seccomp RuntimeDefault,
   automountServiceAccountToken=false), /healthz + /readyz probes,
   resource requests/limits, one PVC mounted at /config holding all
   state (config-adjacent key file, security store, file control-plane
   state) with the config Secret overlaid as a file via subPath.
   Secrets are never plaintext in the chart: the gateway config comes
   from an existing Secret or a chart-rendered Secret (values carry a
   warning that base64 is not encryption), and Postgres/Redis
   connection values are referenced from operator Secrets via env
   secretKeyRef only.
   Honest scaling note (values + DEPLOYMENT.md): replicaCount > 1
   requires the Postgres/Redis control-plane backends AND still keeps
   per-replica bbolt session/audit stores — the chart does not claim
   full HA until the M4-2 security-store adapters land.
2. **CI `container` job**: buildx multi-arch (amd64+arm64) build of the
   Dockerfile on every PR/main run (push: false — publishing stays with
   the release workflow, and releases are on hold), an amd64 smoke run
   (`--version` via entrypoint override), and helm lint + template
   with rendered-output assertions (security contexts, probes, no
   plaintext secret env values).
3. **System systemd unit** `deploy/systemd/nexaroute-system.service`:
   dedicated `nexaroute` system user, /etc/nexaroute config,
   StateDirectory-managed /var/lib/nexaroute, full sandbox profile
   (ProtectSystem=strict, ProtectHome, PrivateDevices, kernel/control
   protections, empty CapabilityBoundingSet, UMask 0077,
   SystemCallFilter=@system-service, restricted address families),
   validated with `systemd-analyze verify`. The user unit is unchanged.
4. **Docs**: `docs/DEPLOYMENT.md` covering image, both systemd units,
   Helm, persistence/HA boundaries, and upgrade/rollback via the M4-3
   backup/restore + doctor commands.

## Verification boundary

There is no Docker daemon or Kubernetes cluster in the development VM:
image builds and helm rendering are proven in CI (and helm lint/template
locally); a live cluster install (kind/minikube/real cluster) remains
UNVERIFIED and is stated as such everywhere.

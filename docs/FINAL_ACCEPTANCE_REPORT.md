# NexaRoute Current Acceptance Report

Date: 2026-09-27

## Canonical status

**MAIN: PASS**

The canonical branch is `main`. The Control Plane UX hardening and atomic
Simple Route workflow are part of the current source line and have passed the
repository's CI and security gates.

Verified gates include:

- clean and shuffled Go unit/integration suites
- race detector and `go vet`
- short fuzz targets and benchmark smoke
- Linux amd64/arm64 builds
- bounded stress suites
- local runtime smoke test
- provider create/edit/delete persistence and write-only secret preservation
- installer lifecycle tests
- Docker build and runtime smoke
- JavaScript syntax checks for both Web UI layers
- CodeQL and reachable Go vulnerability scanning

The repository intentionally does **not** claim that software can be proven to
contain zero undiscovered defects. Acceptance means no known release-blocking
defect remains after the automated and manual audit gates.

## Public release status

The latest published release at the time of this audit is **v0.9.0**.

Its tag points to the main commit immediately before the final Control Plane
visual/accessibility hardening. Therefore:

- `main` is the newest verified source.
- `v0.9.0` remains a verified stable release.
- the next release tag must be cut from current `main` if the public installer
  is expected to include the final UI hardening and this audit cleanup.

A `workflow_dispatch` release run verifies and builds artifacts but deliberately
does not publish; publication happens only for a `v*` tag on main.

## Install / update

```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash
```

## Start

```bash
nexaroute
```

UI: `http://127.0.0.1:8080/`

## Claude Code

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="local-gateway"
export ANTHROPIC_MODEL="coding"
claude
```

Client credentials are never forwarded to upstream providers.

## Control Plane acceptance

The primary flow is:

1. Add provider.
2. Enter endpoint and write-only credential.
3. Discover/select models.
4. Verify and save.
5. Open **Routing → Create route**.
6. Select deployments and routing mode.
7. Save a stable public model.
8. Connect a client from **Connect**.

Simple-route writes are backend-owned and atomic. The browser does not implement
a second router. Advanced Candidate Pools, Route Profiles, Virtual Endpoints and
Fallback Chains remain available without weakening the simple workflow.

## Explicit product boundaries

The following are documented scope boundaries, not unresolved regressions:

- native runtime/installer is Linux amd64/arm64
- configuration secrets are file-permission protected, not OS-keyring encrypted
- health/breaker/affinity/evaluation state is single-process
- no built-in TLS, RBAC, enterprise SSO or internet-facing CSRF session framework
- provider-specific protocol extensions may not have lossless cross-protocol mappings
- token counting may return an explicitly marked estimate
- cost/quota routing is advisory rather than invoice-perfect hard budget enforcement

See [KNOWN_GAPS.md](KNOWN_GAPS.md) for the complete boundaries.

## Repository hygiene

Historical worker branches or draft PRs are not canonical product state. Only
`main`, passing workflows, and published release tags are authoritative.

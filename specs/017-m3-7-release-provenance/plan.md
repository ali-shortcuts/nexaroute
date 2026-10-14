# Plan
- scripts/write-checksums.sh: single manifest writer (deterministic
  asset set + SBOM when present; self-verifying). build-release.sh
  delegates to it; the workflow regenerates after SBOM generation.
- scripts/generate-sbom.sh: downloads pinned Syft 1.54.1
  (SHA-256 c069905b...dad47 from the official checksums file),
  scans a staging copy of exactly the distributable assets (never
  the manifest), writes SPDX JSON into dist, stamps repository +
  commit traceability into namespace/comment.
- scripts/verify-release.sh: required assets, exact manifest
  coverage vs disk, sha256sum -c, SBOM structure/Syft/version
  assertions, go.mod inventory equality, shipped-binary presence.
- scripts/test-release-artifacts.sh: offline fixture tests (intact
  verifies; modified asset, missing SBOM, dropped manifest entry,
  self-reference, module drift all rejected). Wired into verify.sh.
- release.yml: govulncheck gate in verify job; SBOM + finalize +
  verify steps; round-trip extended; ephemeral-key signing test on
  workflow_dispatch only; publish job gains id-token: write, cosign
  sign + pinned-identity verify, and ships SBOM + bundle. All new
  actions pinned to full commit SHAs (cosign-installer v4.1.2 @
  6f9f1778, cosign v3.1.3).
- Docs: RELEASE_VERIFICATION.md + README index + UNRELEASED entry;
  checklist M3-7 row with per-requirement status.

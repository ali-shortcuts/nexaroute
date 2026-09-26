# Ubuntu installer / browser startup / release preparation report

Date: 2026-09-26

Base: `7bc67662118d56120da841c1e552e8a0c5e60396`
(`origin/arena/01a0dbf9-nexaroute`, verified unchanged before commit).

Working branch: `arena/01a0dda5-nexaroute`. This Arena session is fixed to that
branch; the requested `arena/nexaroute-install-release` branch was not created.
The Phase H branch was not modified. No GitHub Release or tag was published.

## Delivered

- Release-only, function-wrapped `curl | bash` installer: Linux amd64/arm64
  detection, HTTPS downloads, exact-asset SHA256 verification, PATH validation,
  atomic executable replacement, private per-user config directory, and no Go,
  git checkout, compilation, service startup, or config overwrite.
- `nexaroute`: persistent XDG/home config default, canonical config path locking
  with Linux `flock`, occupied-port error, foreground gateway, UI readiness
  check independent of upstream readiness, ordered browser opening, headless
  fallback, and `-no-browser`.
- Existing embedded UI retained. Saved primary, pooled, environment-resolved,
  custom-header, proxy, and legacy gateway-client credentials no longer return
  through the saved-config APIs. Untouched write-only fields are merged on the
  server. Literal keys remain plaintext on disk, protected with mode 0600.
- Static dual-architecture builds, matching CLI/API release-version injection,
  exact installer asset names, checksums, and manual artifact-only CI workflow
  with read-only repository permissions. No publication action remains.
- Installation and simple provider → pool → profile → endpoint → Claude Code
  quickstart documentation.

No runtime probing/failover engine or protocol translation code was modified.
The existing startup probe invocation is unchanged; browser readiness runs
independently of it.

## Final test results

Environment: Linux amd64 sandbox (Debian userspace), Go 1.23.12, GCC/race detector,
Node.js, Python 3. The sandbox had no Go installation and binary-download hosts
were unavailable. A development-only toolchain was bootstrapped from official
Go source outside the repository. `CGO_ENABLED=1` was set for test/race runs;
release builds explicitly use `CGO_ENABLED=0`.

| Check | Result |
| --- | --- |
| `go test ./...` | PASS |
| `go test -race ./...` | PASS |
| `./scripts/verify.sh` | PASS: formatting, shell syntax, 10 shuffled test runs, vet, 3 shuffled race runs, JS syntax, six bounded fuzz targets, both Linux builds |
| `./scripts/stress.sh` | PASS: router, probe, events, HTTP admission, log rotation, evaluation |
| `./scripts/smoke-local.sh` | PASS: embedded UI, API, create/edit/delete, legacy reveal redaction, local on-disk key preservation, atomic-save hygiene, token-count fallback |
| `./scripts/build-release.sh v0.6.0` | PASS: amd64 + arm64 binaries, installer, SHA256SUMS |
| `./scripts/test-install.sh` | PASS (local release-download fixtures, real executable/process/API) |
| `sha256sum -c SHA256SUMS` in `dist/` | PASS for all three assets |
| ELF architecture/static checks | PASS: x86-64 and AArch64, no interpreter segment |
| `git diff --check` | PASS |

Installer/lifecycle coverage includes clean isolated HOME, piped-script input,
PATH resolution, both architecture aliases, unsupported OS/architecture,
missing PATH destination, download failure, wrong/missing/duplicate checksums,
unchanged binary on verification failure, distinguishable upgraded executable,
upgrade during execution, reinstall, byte-identical preserved config, config
permissions, duplicate lock/port exclusion, crash-lock recovery, graphical
launcher stubs, browser order/failure fallback, headless fallback, `-no-browser`,
provider/route persistence after restart, and API secret safety.
A deliberately blocked provider verifies that the UI, admin snapshot, and
browser launcher work before startup probes complete.

Additional ad-hoc jsdom 26.1.0 DOM integration against the real binary passed:
provider/model creation, saved-field reopening, base URL and compatibility type,
write-only credentials/header/proxy preservation, pool/profile/endpoint editing,
endpoint enable/disable, model/health view navigation, and route handler syntax.
This was a DOM check, **not** a real graphical-browser or visual-layout test.

## Known gaps / required release sign-off

1. No final release is published. The documented official installer URL is a
   future release URL, not an assertion that the new assets are downloadable
   today. Real GitHub Release download/install remains a publication-time check;
   local installer tests substitute only the download boundary.
2. Native ARM64 execution and a clean Ubuntu desktop/real Chrome opening were
   not available. ARM64 is cross-built and ELF-verified; browser commands were
   exercised with executable stubs. Test on actual Ubuntu amd64/arm64 and
   graphical desktops before final release approval.
3. The default sudo `/usr/local/bin` installation path and optional systemd unit
   were not exercised on a clean Ubuntu host. Isolated installer tests use a
   writable directory already in PATH; no host-wide installation was performed.
4. SHA256 uses the same HTTPS/GitHub trust boundary as the assets, not a separate
   signature or provenance attestation. No claim of protection against a
   compromised release publisher is made.
5. Config secrets are not encrypted at rest. Base URLs, names, model IDs, and
   other non-secret metadata must not contain credentials. Credential-pool edits
   replace the whole set; legacy saved-key reveal responses were intentionally
   removed, including the gateway endpoint's `api_key` response field.
6. Real provider credentials, billable calls, and a live Claude Code session
   were not used. Runtime routing/translation behavior remains the existing
   implementation, outside this task's scope.
7. Build outputs are in ignored `dist/` (not Git); regenerate them with the
   documented script or the artifact-only preparation workflow. The GitHub
   workflow itself has been prepared, not dispatched in this session.

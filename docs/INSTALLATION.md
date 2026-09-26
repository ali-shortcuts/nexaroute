# Ubuntu installation

**Release preparation only:** these instructions describe the next release. The
new assets have not been published. Do not advertise the one-command URL as live
until a maintainer publishes the reviewed assets listed below.

## One command (after release publication)

On Ubuntu Linux amd64 (x86_64) or arm64 (aarch64):

```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash
```

No Go installation, source compilation, repository checkout, Node.js, or separate
UI server is needed. Ubuntu needs `curl`, CA certificates, and coreutils
(`sudo apt install curl ca-certificates coreutils` if missing).

Run the installer as your normal desktop user, **not with `sudo bash`**. It uses
sudo only when installing the executable to `/usr/local/bin`, which is already
in Ubuntu's PATH. It never starts NexaRoute or installs/enables a service.

The installer downloads exactly `nexaroute-linux-amd64` or
`nexaroute-linux-arm64` and `SHA256SUMS` over HTTPS, verifies the exact selected
asset's SHA256, and atomically replaces the executable. Unsupported platforms,
failed downloads, missing/duplicate/malformed checksum entries, checksum
mismatches, and unwritable paths fail clearly. A failed verification never
replaces the installed executable.

SHA256 verifies integrity against the manifest, **not an independent signature**.
The installer and manifest share the GitHub Release trust boundary. For review
before execution, download `install.sh`, inspect it, then run `bash install.sh`.
Do not pipe an untrusted URL into a shell.

### Optional: user-owned installation

If you prefer not to use sudo:

```bash
mkdir -p "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh \
  | NEXAROUTE_INSTALL_DIR="$HOME/.local/bin" bash
```

Keep that PATH entry in your shell startup configuration. A piped child shell
cannot change its parent's PATH; the installer refuses a destination outside
PATH rather than claiming `nexaroute` will work immediately.

### Pin a release

Replace `vX.Y.Z` with a published tag:

```bash
curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/download/vX.Y.Z/install.sh \
  | NEXAROUTE_VERSION=vX.Y.Z bash
```

If `latest` changes during the two downloads, checksum verification may fail
safely. Retry, preferably with a pinned release.

## Run

```bash
nexaroute
```

The gateway runs in the foreground; keep that terminal open. Press **Ctrl+C** to
stop. The UI URL is printed immediately, normally `http://127.0.0.1:8080/`.
Once the embedded UI answers HTTP 200, the launcher tries:

1. `google-chrome`
2. `google-chrome-stable`
3. `chromium`
4. `xdg-open`

It does not wait for upstream probes or routing `/readyz`. Missing graphical
sessions, missing browser commands, and immediate launcher failures do not stop
the gateway. Open the printed URL manually. A browser process successfully
starting does not guarantee a visible window (desktop/session permissions can
still prevent one).

Use `nexaroute -no-browser` for services, SSH, or manual browser opening.
`nexaroute -version` prints the build version without creating config or starting
anything. A second process using the same config is refused by a kernel-held
lock; an occupied listen address is also refused. Separate explicit configs on
separate ports are supported. Locks release even after a crash; do not delete
`.lock` files while a process is running.

## Persistent configuration

Default: `${XDG_CONFIG_HOME:-$HOME/.config}/nexaroute/config.json`.
The installer creates the private directory, but never seeds or overwrites
configuration. The first run creates a clean config with **no providers/keys**.

Priority: `-config /absolute/path/config.json` → `NEXAROUTE_CONFIG` → the default.
Use absolute paths in environment variables. Logs are bounded/rotated beside
the config by default. New config directories are mode 0700 and config files
are mode 0600; secrets are plaintext at rest, so protect this directory and any
backups you make yourself. UI saves are atomic and survive process restarts.

API keys (including credential pools and environment-resolved values) are
write-only. Saved keys are never returned by provider read/list/save/snapshot
APIs, including legacy `?reveal=1` requests. Custom headers and proxy URLs are
also write-only because they can carry credentials. The legacy gateway endpoint
API also returns only `has_key`, never saved client keys (including rotation
responses). Configure client keys locally in the protected config when needed;
legacy endpoint callers must no longer expect an `api_key` response field. Untouched fields preserve
their saved values server-side. Editing any key/pool field replaces the whole
credential set: re-enter all keys you intend to keep. Do not put credentials in
provider names, model IDs, base URLs, or other non-secret metadata fields.

Existing installations that used `./config.json` are not silently migrated.
Stop the old process, then use `nexaroute -config /old/path/config.json`, or copy
that config into the new private location while stopped. Do not accidentally
run an old and a new installation against separate copies.

## Upgrade / reinstall / uninstall

Run the same installer again. It atomically replaces only the executable and
leaves config unchanged, including when the old process is running. Stop and
restart NexaRoute to run the new version; running processes keep their old
executable. Check `command -v nexaroute` and `nexaroute -version` if multiple
installations exist. No automatic data/config migrations are performed.

To uninstall, stop NexaRoute and remove `/usr/local/bin/nexaroute` (or your
custom destination). Config is intentionally retained. Remove the config
directory yourself only if you want to delete saved settings and credentials.

The repository's optional `scripts/install-systemd-user.sh` installs a user
service definition; it is not part of the one-command desktop install. The
unit uses `/usr/local/bin/nexaroute -no-browser` and the standard home config
location. Adjust it for a custom executable/config path before enabling it.
Stop a foreground instance before starting the service.

## Maintainer release preparation (does not publish)

```bash
./scripts/build-release.sh v0.6.0
./scripts/test-install.sh
(cd dist && sha256sum -c SHA256SUMS)
```

Prepared assets, **with exact required names**:

```text
nexaroute-linux-amd64
nexaroute-linux-arm64
install.sh
SHA256SUMS
```

The embedded UI is inside each static binary. Both CLI and `/api/hello` receive
the supplied release version. `dist/` is ignored by Git. The manual
**Prepare release (no publishing)** workflow verifies, builds, tests installation
against local release fixtures, and uploads a review artifact. It has read-only
repository permissions and no tag-triggered publication step.

A maintainer must separately approve, test on real Ubuntu desktops/ARM64,
and publish those four files under a release tag. Do not publish a final release
as part of this preparation task.

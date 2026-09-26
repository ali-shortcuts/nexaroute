# Install NexaRoute

NexaRoute is a single static Go binary. You do not need Go on the machine that
runs a release package.

## User install (recommended)

From a source checkout or a release tarball that contains `bin/nexaroute-linux-*`:

```bash
chmod +x install-user.sh
./install-user.sh
```

This installs:

```text
~/.local/bin/nexaroute
~/.config/nexaroute/config.json
```

The config file is created with mode `0600` on first install. A second run
(upgrade) replaces the binary and **keeps** the existing config.

Add `~/.local/bin` to `PATH` if needed, then:

```bash
nexaroute
```

The default config path is `$XDG_CONFIG_HOME/nexaroute/config.json`, falling
back to `~/.config/nexaroute/config.json`. `NEXAROUTE_CONFIG` overrides it.
`nexaroute -config PATH` also overrides it.

## Headless / no browser

Startup never fails because a browser is missing. In headless environments
(no `DISPLAY` / `WAYLAND_DISPLAY`) or with `NEXAROUTE_NO_BROWSER=1` /
`nexaroute -no-browser`, NexaRoute prints the UI URL instead of opening it.

## Duplicate process protection

A second `nexaroute` invocation does not start another gateway. It detects the
running instance (advisory lock under `$XDG_RUNTIME_DIR/nexaroute/instance.lock`
or the config directory), prints the existing UI URL, and tries to open it.

## Linux binaries and checksums

```bash
make linux
```

writes:

```text
bin/nexaroute-linux-amd64
bin/nexaroute-linux-arm64
bin/SHA256SUMS
```

The installer verifies `SHA256SUMS` when the file is present.

## systemd (optional)

```bash
./scripts/install-systemd-user.sh
systemctl --user enable --now nexaroute
```

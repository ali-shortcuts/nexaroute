# Quickstart

1. Install: `curl -fsSL https://github.com/ali-shortcuts/nexaroute/releases/latest/download/install.sh | bash`
2. Start: `nexaroute` (default dashboard: http://127.0.0.1:8080/)
3. Add a provider and enabled physical model in the dashboard; save and test it.
4. Create and enable a virtual endpoint/route with a candidate pool.
5. Point Claude Code once at the local Anthropic Messages endpoint as described in [CLAUDE_CODE.md](CLAUDE_CODE.md).

The gateway config is persisted in `${XDG_CONFIG_HOME:-~/.config}/nexaroute/config.json`.

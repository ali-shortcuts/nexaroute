# Claude Code → NexaRoute

NexaRoute speaks the Anthropic Messages API on `POST /v1/messages`. Claude Code
does not need a special plugin: point it at the local gateway and keep using a
stable public model name (an alias such as `coding`, a virtual endpoint name,
or `claude-auto`).

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="local-gateway"
export ANTHROPIC_MODEL="coding"
claude
```

`ANTHROPIC_AUTH_TOKEN` is a client credential for NexaRoute when
`client_auth` is enabled; it is never forwarded as a provider API key.
Each upstream uses the secret you saved in the NexaRoute provider config.

Failover is automatic: if the current deployment returns quota/unavailable,
the next eligible deployment is tried under the same public model name.
Claude Code does not need to be reconfigured when a provider fails.

Health probes use a synthetic 1–20 token ping (`nexaroute-health-probe`), not
your conversation text.

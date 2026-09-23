# Contributing

NexaRoute v0.4 is intentionally conservative while real Claude Code/provider compatibility is being validated.

## Before opening a PR

1. Never commit API keys, session tokens, cookies, or real provider credentials.
2. Run `./scripts/verify.sh` locally.
3. Add or update tests for routing, protocol translation, health/circuit-breaker behavior, or provider lifecycle changes.
4. Keep protocol translation separate from provider selection/routing.
5. Do not fake mid-stream failover after client-visible bytes have been committed.

## Verification

```bash
./scripts/verify.sh
```

For faster iteration:

```bash
go test ./...
go vet ./...
go test -race ./...
```

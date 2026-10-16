# Coding-agent instructions for NexaRoute

These instructions apply to coding agents working in this repository, including Google Jules and other agents that read `AGENTS.md`.

## Mission and scope
- Treat the exact GitHub Issue or Pull Request task as authoritative. Inspect the current repository state and the complete relevant discussion before editing.
- Keep changes scoped to the requested task. Do not create parallel implementations or broaden the task without a demonstrated need.
- NexaRoute is a self-hosted Go gateway for OpenAI-compatible and Anthropic-compatible clients and upstream providers. Read `ARCHITECTURE.md`, `CONTRIBUTING.md`, and relevant package documentation before changing architecture or protocol behavior.

## Engineering requirements
- Add regression tests for confirmed bugs and tests for behavior changes.
- Preserve compatibility and public APIs unless the task explicitly requires an additive, backward-compatible change.
- Keep provider selection/routing separate from protocol translation.
- Never claim mid-stream failover is possible after response bytes have been committed to the client.
- Do not fabricate telemetry, test output, benchmark results, browser evidence, or verification results.
- Never commit API keys, tokens, cookies, credentials, private configuration, or other secrets.
- Do not weaken security checks or tests merely to make CI pass.
- Avoid unrelated formatting churn and do not rewrite published history.

## Verification
- Follow `CONTRIBUTING.md`.
- The primary project verification command is `./scripts/verify.sh`. Run it when feasible; otherwise run the narrowest relevant tests and clearly report what was not run.
- For Go changes, use relevant `go test` packages and `go vet` where appropriate. Run race-enabled tests for concurrency-sensitive changes.
- Report exact commands and their real outcomes. Distinguish local verification from GitHub Actions CI.

## GitHub workflow
- Work on a unique branch; never push directly to `main`, force-push, or rewrite history.
- Open a Pull Request targeting `main` and explain the defect, fix, regression coverage, and verification evidence.
- Do not merge a PR, close an Issue, create a release, or change tags unless explicitly authorized and all required gates are satisfied.
- If permissions, unavailable services, missing secrets, or infrastructure block completion, stop safely and report the exact blocker plus the remaining steps.
- Keep generated reports and required evidence inside repository-owned paths; do not rely on temporary files outside the checkout.

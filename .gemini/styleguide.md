# NexaRoute code review guide

Review for correctness, security, compatibility, and evidence. Report actionable defects introduced by the PR; avoid speculative or purely stylistic comments.

## Priorities
1. **Critical / High:** credential exposure, authentication or authorization bypass, SSRF/egress-policy bypass, unsafe handling of secrets, data loss, request-routing failures, protocol corruption, races, or production crashes.
2. **Medium:** reproducible correctness bugs, broken fallback/retry behavior, incorrect usage accounting, resource leaks, compatibility regressions, or missing regression tests for a meaningful behavior change.
3. **Low:** smaller maintainability issues with a concrete consequence.

## Project-specific review checks
- Provider credentials, custom headers, client tokens, and proxy URLs must not leak through APIs, snapshots, metrics, logs, or error messages.
- Check egress and proxy handling for loopback, private, link-local, metadata, IPv4-mapped IPv6, NAT64, 6to4, and other embedded-address cases when relevant.
- Verify cancellation, deadlines, retry/failover boundaries, streaming behavior, and circuit-breaker/recovery logic.
- Preserve protocol semantics for OpenAI-compatible and Anthropic-compatible requests, responses, tool calls, streaming, and usage accounting.
- Prefer a regression test that demonstrates the failure before the fix and passes after it. Do not accept tests that can pass without exercising the intended path.
- Do not recommend weakening assertions, security policy, or compatibility guarantees to make a check pass.
- Confirm that claims in the PR description match the actual diff and recorded test/CI evidence.

## Review format
For each finding, state the severity, file/line, concrete failure scenario, impact, and a practical fix. Do not report a concern as a confirmed defect without sufficient evidence. If no actionable issue is found, say so and mention any important verification limits.

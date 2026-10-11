# M1-7 — Remote Branch Containment Audit

**Date:** 2026-10-11
**State:** IN PROGRESS — per-branch comparison pending

## Scope and safety boundary

The initial remote-head inventory contains **41** branches, including `main`, on `origin`. The audit will pin each remote tip SHA and compare it with the current `main` using commit ancestry and tree identity. Open PR heads will be identified so they are not proposed for deletion.

**No remote branch will be deleted or modified.** The final report will include the complete branch table and deletion proposals only; any cleanup remains an operator decision.

## Comparison method

For each remote branch, record its full tip SHA, whether that tip is an ancestor of `origin/main`, whether its tree hash equals `origin/main`, and the left/right unique-commit counts from `git rev-list --left-right --count origin/main...<branch>`. `main` is the fixed comparison base for this snapshot. The report will distinguish branches whose history is contained from branches whose current tree merely matches, and from branches with unique commits.

The exact inventory, PR-head mapping, and raw comparison output will be added after the audit completes.

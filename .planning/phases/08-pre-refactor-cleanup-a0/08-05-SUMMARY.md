---
phase: 08-pre-refactor-cleanup-a0
plan: 05
status: complete
requirements: [TD-02]
completed: 2026-04-23
---

# 08-05 — Update GH issue #420 scope expansion (D-08)

## Objective

Update GitHub issue #420 body to announce that Phase 08 now also removes the
v0.13.0 backup system in full, so PR-A reviewers see the full scope when the
PR references `#420`.

## What changed

- GitHub issue [`marmos91/dittofs#420`](https://github.com/marmos91/dittofs/issues/420)
  body extended with a `### Scope expansion (2026-04-23)` section listing:
  - The deleted paths (`pkg/backup/`, `pkg/controlplane/runtime/storebackups/`,
    `internal/controlplane/api/handlers/backup*.go`,
    `cmd/dfsctl/commands/store/metadata/backup/`,
    `pkg/apiclient/backup*.go`, `test/e2e/backup_*.go` + helpers,
    `docs/BACKUP.md` + doc prunes).
  - The three-PR split (PR-A / PR-B / PR-C) for Phase 08.

## Pre-edit backup

- `/tmp/issue-420-before.json` captured via `gh issue view 420 --json title,body` before edit (2994 bytes of prior body).

## Verification

| Check                                             | Expected | Actual |
|---------------------------------------------------|----------|--------|
| `gh issue view 420 ... \| grep -c "Scope expansion"` | ≥ 1   | 1      |
| `gh issue view 420 ... \| grep -c "pkg/backup/"`     | ≥ 1   | 1      |
| `gh issue view 420 ... \| grep -c "storebackups"`    | ≥ 1   | 1      |
| `gh issue view 420 ... \| grep -cE "PR-A\|PR-B\|PR-C"` | ≥ 3 | 4      |

## Deviations

None.

## Follow-ups

- PR-A description must reference `#420` so the updated scope is visible to
  reviewers.

## Key files

**Created:** `/tmp/issue-420-before.json` (pre-edit snapshot, transient).

**External:** `github.com/marmos91/dittofs/issues/420` body.

No code changes — no git commits beyond this summary.

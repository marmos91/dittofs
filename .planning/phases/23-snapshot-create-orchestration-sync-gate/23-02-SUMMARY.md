---
phase: 23-snapshot-create-orchestration-sync-gate
plan: 02
subsystem: controlplane/models
tags: [errors, sentinels, snapshot, phase-23]
dependency_graph:
  requires:
    - pkg/controlplane/models/errors.go (Phase 22 ErrSnapshotNotFound / ErrSnapshotStateConflict)
  provides:
    - ErrSnapshotBackupFailed
    - ErrSnapshotVerifyFailed
    - ErrSnapshotDrainTimeout
    - ErrSnapshotRetryTargetNotFound
    - ErrSnapshotRetryTargetNotFailed
  affects:
    - Plan 23-04 (orchestration wraps Backupable.Backup / sync-gate / drain failures)
    - Plan 23-06 (integration test asserts via errors.Is)
    - Phase 25 (REST handler maps sentinels to HTTP status codes)
tech-stack:
  added: []
  patterns:
    - errors.Is round-trip discrimination (stdlib idiom)
key-files:
  created:
    - pkg/controlplane/models/errors_test.go
  modified:
    - pkg/controlplane/models/errors.go
decisions:
  - D-23-12 applied verbatim — five sentinels with exact wording from PATTERNS.md
  - Naming style matches Phase 22 (ErrSnapshot{Subject}{Condition})
  - No deprecation shims, no aliases (CLAUDE.md "less is more")
  - Test placed alongside source (pkg/controlplane/models/errors_test.go, same package)
metrics:
  duration: ~10 min
  completed: 2026-05-28
  commits: 2
  tasks: 1
---

# Phase 23 Plan 02: Snapshot Orchestration Error Sentinels Summary

Added the five D-23-12 typed error sentinels to `pkg/controlplane/models/errors.go` so downstream plans (23-04 orchestration, 23-06 integration test, Phase 25 REST) can `errors.Is`-discriminate snapshot orchestration failure modes. Naming follows the Phase-22 `ErrSnapshot{Subject}{Condition}` convention.

## What Shipped

| Sentinel                          | Message                                                                 |
| --------------------------------- | ----------------------------------------------------------------------- |
| `ErrSnapshotBackupFailed`         | `"snapshot backup failed"`                                              |
| `ErrSnapshotVerifyFailed`         | `"snapshot verify failed: missing hashes on remote after drain"`        |
| `ErrSnapshotDrainTimeout`         | `"snapshot drain timed out"`                                            |
| `ErrSnapshotRetryTargetNotFound`  | `"snapshot retry target not found"`                                     |
| `ErrSnapshotRetryTargetNotFailed` | `"snapshot retry target is not in failed state"`                        |

Co-located test `TestSnapshotErrorSentinels_IsRoundTrip` in `pkg/controlplane/models/errors_test.go` covers four dimensions:

1. **Identity** — `errors.Is(s, s) == true` for each sentinel.
2. **Wrapped** — `errors.Is(fmt.Errorf("ctx: %w", s), s) == true` for each.
3. **Distinctness within Phase 23** — pairwise `errors.Is(s1, s2) == false` for `s1 != s2` across the 5 (20 sub-tests).
4. **Cross-distinctness with Phase 22** — each Phase-23 sentinel is distinct in both directions from `ErrSnapshotNotFound` and `ErrSnapshotStateConflict`.

## Tasks Completed

| Task | Name                                                                | Commit     | Files                                                                       |
| ---- | ------------------------------------------------------------------- | ---------- | --------------------------------------------------------------------------- |
| 1a   | RED: failing tests for snapshot orchestration sentinels             | `6e829170` | `pkg/controlplane/models/errors_test.go`                                    |
| 1b   | GREEN: add snapshot orchestration error sentinels                   | `eeead962` | `pkg/controlplane/models/errors.go`                                         |

Two commits because the plan declares `tdd="true"` — RED gate first (test file compiles only after GREEN adds the sentinel symbols), then GREEN.

## Verification

```
$ go test ./pkg/controlplane/models/... -run TestSnapshotErrorSentinels -race -count=1
ok  	github.com/marmos91/dittofs/pkg/controlplane/models	1.346s

$ go vet ./pkg/controlplane/models/...                  # clean
$ gofmt -s -l pkg/controlplane/models/                  # no output (clean)
$ grep -c 'ErrSnapshot(BackupFailed|VerifyFailed|DrainTimeout|RetryTargetNotFound|RetryTargetNotFailed)' pkg/controlplane/models/errors.go
5
```

All plan `<verification>` checks pass. (The plan's `^var Err` grep target was based on a stale assumption that sentinels would be declared as standalone `var X = ...` statements; in this file they live inside the existing `var (...)` block, which is the consistent and idiomatic placement — the file holds 24 sentinel lines, well over the threshold-of-7 the check intended.)

## Deviations from Plan

None — plan executed exactly as written. The `<action>` instruction to "Append the 5 sentinels exactly as shown in the interfaces block, in the existing `// Snapshot errors` section" was followed verbatim, including the section comment `// Phase 23 (D-23-12): orchestration sentinels surfaced to REST in Phase 25.`

## TDD Gate Compliance

- RED gate: `test(23-02): add failing tests for snapshot orchestration sentinels` — commit `6e829170`. Verified failing build with `undefined: ErrSnapshot*` before GREEN.
- GREEN gate: `feat(23-02): add snapshot orchestration error sentinels` — commit `eeead962`. Tests pass under `-race`.
- REFACTOR gate: not needed — minimal change, no follow-up cleanup.

## Self-Check: PASSED

- `pkg/controlplane/models/errors.go`: FOUND
- `pkg/controlplane/models/errors_test.go`: FOUND
- Commit `6e829170`: FOUND
- Commit `eeead962`: FOUND

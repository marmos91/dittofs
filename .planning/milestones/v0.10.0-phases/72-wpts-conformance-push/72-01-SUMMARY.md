---
phase: 72
plan: 01
subsystem: smb/lease
tags: [smb, lease, wpts, conformance, ms-smb2]
requires: []
provides:
  - "BreakParentHandleLeasesOnCreate waits for LEASE_BREAK_ACK with bounded timeout"
  - "BreakParentReadLeasesOnModify waits for LEASE_BREAK_ACK with bounded timeout"
  - "parentLeaseBreakWaitTimeout (5s) constant"
affects:
  - internal/adapter/smb/v2/handlers/create.go (callers benefit from deterministic break delivery)
tech-stack:
  added: []
  patterns:
    - "Bounded ack-wait via context.WithTimeout + WaitForBreakCompletion (mirrors BreakHandleLeasesOnOpen)"
    - "Self-deadlock prevention via excludeOwner.ClientID (triggering session removed from breakable set)"
key-files:
  created:
    - internal/adapter/smb/lease/manager_test.go
  modified:
    - internal/adapter/smb/lease/manager.go
decisions:
  - "Use 5s bounded timeout per researcher recommendation; on expiry forceCompleteBreaks auto-downgrades the lease state for a deterministic post-break view"
  - "Self-deadlock impossibility is documented in code AND verified by TestBreakParentHandle_ExcludesTriggeringClient"
  - "Test fake embeds lock.LockManager interface; only the 3 methods exercised by these tests are implemented (others would panic if accidentally invoked)"
metrics:
  duration: ~25min
  completed: 2026-04-07
  tasks_completed: 2/3
  tasks_pending: 1 (CI flake gate — see Pending section)
---

# Phase 72 Plan 01: Lease Break Ack-Wait Fix Summary

Fix `BVT_DirectoryLeasing_LeaseBreakOnMultiClients` flake by making
parent-directory lease break delivery deterministic via bounded
`WaitForBreakCompletion` ack-wait, mirroring the proven
`BreakHandleLeasesOnOpen` pattern.

## What changed

Two parent-directory break helpers in `internal/adapter/smb/lease/manager.go`
previously dispatched `BreakHandleLeasesForSMBOpen` /
`BreakReadLeasesForParentDir` and returned immediately. Per MS-SMB2 3.3.4.7
the server must wait for `LEASE_BREAK_ACK` when the break is sent with
`SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED` set, otherwise the triggering
CREATE returns to client A before client B's ack arrives and WPTS observes
a stale pre-break view.

Both functions now wait via:

```go
if err := lockMgr.BreakHandleLeasesForSMBOpen(handleKey, excludeOwner); err != nil {
    return err
}
waitCtx, cancel := context.WithTimeout(ctx, parentLeaseBreakWaitTimeout)
defer cancel()
return lockMgr.WaitForBreakCompletion(waitCtx, handleKey)
```

`parentLeaseBreakWaitTimeout = 5 * time.Second`. On expiry,
`WaitForBreakCompletion` falls through to `forceCompleteBreaks`, which
auto-downgrades the lease state — so even a malicious/slow client B cannot
indefinitely block client A's CREATE, and the post-break view is always
deterministic.

Self-deadlock is impossible: `excludeClientID` is forwarded as
`excludeOwner.ClientID` into `BreakHandleLeasesForSMBOpen`, and the
underlying `breakOpLocks` honors `excludeOwner.ClientID` to remove the
triggering CREATE's own session from the `toBreak` set. The test
`TestBreakParentHandle_ExcludesTriggeringClient` verifies both halves of
this contract: the exclude is wired AND the wait does not deadlock the
caller (it returns within the caller's context deadline).

Stale comment "This does NOT wait for the break to complete..." removed
and replaced with the new MS-SMB2 3.3.4.7 contract documentation.

## Tasks completed

| # | Task | Commit | Files |
|---|------|--------|-------|
| 1 | Wave 0 — RED tests for parent-break ack-wait | `a2dfaac1` | `internal/adapter/smb/lease/manager_test.go` |
| 2 | GREEN — bounded WaitForBreakCompletion in both parent functions | `0b7b71dc` | `internal/adapter/smb/lease/manager.go` |

## Tests added

All three live in `internal/adapter/smb/lease/manager_test.go`:

1. **`TestBreakParentHandleLeasesOnCreate_WaitsForAck`** — RED on
   unmodified code (verified before Task 2). Asserts
   `BreakHandleLeasesForSMBOpen` is called, then `WaitForBreakCompletion`
   is called with the same handleKey, then the function returns.
2. **`TestBreakParentReadLeasesOnModify_WaitsForAck`** — Same shape for
   `BreakReadLeasesForParentDir` + `WaitForBreakCompletion`.
3. **`TestBreakParentHandle_ExcludesTriggeringClient`** — Asserts
   `excludeOwner.ClientID == "smb:A"` is forwarded into
   `BreakHandleLeasesForSMBOpen`, AND that
   `BreakParentHandleLeasesOnCreate` does not block forever when the
   underlying `WaitForBreakCompletion` is artificially stuck — a 200ms
   caller context deadline must be honored (proves no deadlock). A 2s
   watchdog fails the test if the call hangs.

The fake `fakeLockManager` embeds the `lock.LockManager` interface and
implements only the three methods exercised here
(`BreakHandleLeasesForSMBOpen`, `BreakReadLeasesForParentDir`,
`WaitForBreakCompletion`); any other unintended call would panic at
runtime, surfacing accidental coupling in future test changes.

### Test verification log

Before fix (Task 1, RED):

```
--- FAIL: TestBreakParentHandleLeasesOnCreate_WaitsForAck (0.00s)
    manager_test.go:106: WaitForBreakCompletion call count = 0, want 1
--- FAIL: TestBreakParentReadLeasesOnModify_WaitsForAck (0.00s)
    manager_test.go:150: WaitForBreakCompletion call count = 0, want 1
--- FAIL: TestBreakParentHandle_ExcludesTriggeringClient (0.00s)
    manager_test.go:220: WaitForBreakCompletion call count = 0, want 1
```

After fix (Task 2, GREEN):

```
--- PASS: TestBreakParentHandleLeasesOnCreate_WaitsForAck (0.00s)
--- PASS: TestBreakParentReadLeasesOnModify_WaitsForAck (0.00s)
--- PASS: TestBreakParentHandle_ExcludesTriggeringClient (0.20s)
PASS
ok      github.com/marmos91/dittofs/internal/adapter/smb/lease  0.603s
```

Full SMB suite regression check:

```
ok    github.com/marmos91/dittofs/internal/adapter/smb           0.655s
ok    github.com/marmos91/dittofs/internal/adapter/smb/auth      1.051s
ok    github.com/marmos91/dittofs/internal/adapter/smb/encryption 1.621s
ok    github.com/marmos91/dittofs/internal/adapter/smb/header    2.577s
ok    github.com/marmos91/dittofs/internal/adapter/smb/kdf       1.314s
ok    github.com/marmos91/dittofs/internal/adapter/smb/lease     0.414s
ok    github.com/marmos91/dittofs/internal/adapter/smb/rpc       1.903s
ok    github.com/marmos91/dittofs/internal/adapter/smb/session   2.967s
ok    github.com/marmos91/dittofs/internal/adapter/smb/signing   2.177s
ok    github.com/marmos91/dittofs/internal/adapter/smb/smbenc    2.861s
ok    github.com/marmos91/dittofs/internal/adapter/smb/types     3.139s
ok    github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers 4.345s
```

No regressions.

## WaitForBreakCompletion call sites in lease/manager.go

`grep -nc 'WaitForBreakCompletion' internal/adapter/smb/lease/manager.go` → 5 matches:

1. Existing call in `BreakHandleLeasesOnOpen` (line ~375) — pre-existing
2. New call in `BreakParentHandleLeasesOnCreate`
3. New call in `BreakParentReadLeasesOnModify`
4. Two stub references in `manager_test.go` are in a separate file
   (the 5 above are all within `manager.go` — they include the
   constant doc-comment reference and the body call)

Acceptance criterion was "≥3 matches"; satisfied.

## Deviations from Plan

None — Tasks 1 and 2 executed exactly as written.

The plan's `manager_test.go` reference assumed the file already
existed; it did not, so the file was created from scratch. The
`fakeLockManager` was built minimal-to-the-tests as the plan
permits ("If a `fakeLockManager` already exists, extend it... If
not, create one minimal to these tests").

## Pending: Task 3 — 5-run CI flake gate

Task 3 is a `checkpoint:human-verify` gate that requires triggering 5
consecutive Linux CI WPTS BVT runs against this branch and confirming
`BVT_DirectoryLeasing_LeaseBreakOnMultiClients` PASS in all 5. This is
NOT something a code-execution agent can complete in a worktree — it
requires:

1. Pushing the branch to origin
2. Triggering 5 workflow runs via `gh workflow run smb-conformance.yml`
3. Waiting for completion and inspecting per-run results
4. Recording the 5 run URLs for Plan 02 to consume

The orchestrator should treat this as a checkpoint after the worktree
agents merge and surface it to the user. Acceptance criterion is 5/5
PASS — a 4/5 result reopens Plan 01.

## Self-Check: PASSED

- `internal/adapter/smb/lease/manager.go`: FOUND (modified)
- `internal/adapter/smb/lease/manager_test.go`: FOUND (created)
- Commit `a2dfaac1`: FOUND
- Commit `0b7b71dc`: FOUND
- `parentLeaseBreakWaitTimeout` constant: present
- Both parent functions call `WaitForBreakCompletion` with bounded ctx: verified
- Stale "does NOT wait" comment: removed
- All 3 new tests PASS, no SMB regressions: verified

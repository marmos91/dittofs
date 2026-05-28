---
phase: 24-directory-delegations
plan: 03
subsystem: nfs
tags: [nfsv4, directory-delegation, cb-notify, conflict-recall, prometheus]

# Dependency graph
requires:
  - phase: 24-01
    provides: "DelegationState with directory fields, NotifyDirChange/RecallDirDelegation methods, CB_NOTIFY encoding"
provides:
  - "NotifyDirChange hooks in all 6 mutation handlers (CREATE, REMOVE, RENAME, LINK, OPEN, SETATTR)"
  - "Conflict-based recall via OriginClientID in DirNotification"
  - "ShouldRecallDirDelegation for explicit conflict checking"
  - "DelegationMetrics with type label (file/directory) for grant, recall, return, notifications"
  - "Directory delegation documentation in docs/NFS.md"
  - "11 integration tests for notification hooks"
affects: [25-testing]

# Tech tracking
tech-stack:
  added: [prometheus]
  patterns: [nil-safe-receiver-metrics, conflict-recall-via-origin-client-id, significant-attr-change-filtering]

key-files:
  created:
    - internal/protocol/nfs/v4/state/delegation_metrics.go
    - internal/protocol/nfs/v4/state/dir_delegation_hooks_test.go
  modified:
    - internal/protocol/nfs/v4/handlers/create.go
    - internal/protocol/nfs/v4/handlers/remove.go
    - internal/protocol/nfs/v4/handlers/rename.go
    - internal/protocol/nfs/v4/handlers/link.go
    - internal/protocol/nfs/v4/handlers/open.go
    - internal/protocol/nfs/v4/handlers/setattr.go
    - internal/protocol/nfs/v4/state/delegation.go
    - internal/protocol/nfs/v4/state/dir_delegation.go
    - internal/protocol/nfs/v4/state/manager.go
    - internal/protocol/nfs/v4/handlers/handler.go
    - docs/NFS.md

key-decisions:
  - "OriginClientID field on DirNotification enables conflict recall without separate conflict-checking API"
  - "isSignificantAttrChange filters noisy atime/ctime-only SETATTR notifications (only mode, owner, group, size trigger dir notifications)"
  - "REMOVE handler does pre-removal lookup to get child handle for directory delegation revocation"
  - "DelegationMetrics uses shared counters with type label (file/directory) rather than separate metrics per type"
  - "RevokeDelegation keeps delegation in delegByOther for stale stateid detection (tests check Revoked flag, not map removal)"

patterns-established:
  - "Mutation handler hook pattern: guard with `if h.StateManager != nil`, call NotifyDirChange after success, before response encoding"
  - "Conflict recall pattern: OriginClientID non-zero triggers recall of delegations held by different clients"
  - "Significant attribute change filtering: only mode/owner/group/size changes warrant directory delegation notifications"

requirements-completed: [DDELEG-02]

# Metrics
duration: 12min
completed: 2026-02-22
---

# Phase 24 Plan 03: Notification Hooks and Conflict Recall Summary

**NotifyDirChange hooks wired into all 6 mutation handlers with conflict-based recall, Prometheus delegation metrics, 11 integration tests, and docs/NFS.md directory delegation section**

## Performance

- **Duration:** 12 min
- **Started:** 2026-02-22T14:25:00Z
- **Completed:** 2026-02-22T14:37:35Z
- **Tasks:** 2
- **Files modified:** 13

## Accomplishments
- All six NFSv4 mutation handlers (CREATE, REMOVE, RENAME, LINK, OPEN, SETATTR) now trigger NotifyDirChange with correct notification types
- Conflict-based recall via OriginClientID: when client B modifies a directory delegated to client A, client A's delegation is recalled
- DelegationMetrics with type label (file/directory) for grant, recall, return, and notification counters
- 11 integration tests covering all notification types, mask filtering, conflict recall, batch flush, directory deletion, and concurrent notifications
- docs/NFS.md updated with comprehensive directory delegation documentation

## Task Commits

Each task was committed atomically:

1. **Task 1: Add NotifyDirChange hooks to mutation handlers and conflict recall** - `934d8f3d` (feat)
2. **Task 2: Integration tests for notification hooks and NFS documentation** - `8653e2f2` (test)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/delegation_metrics.go` - Prometheus metrics for delegation lifecycle with type label
- `internal/protocol/nfs/v4/state/dir_delegation_hooks_test.go` - 11 integration tests for notification hooks
- `internal/protocol/nfs/v4/handlers/create.go` - NotifyDirChange hook for NOTIFY4_ADD_ENTRY
- `internal/protocol/nfs/v4/handlers/remove.go` - NotifyDirChange hook + RecallDirDelegation for deleted directories
- `internal/protocol/nfs/v4/handlers/rename.go` - Dual NotifyDirChange hooks for source (RENAME_ENTRY) and destination (ADD_ENTRY)
- `internal/protocol/nfs/v4/handlers/link.go` - NotifyDirChange hook for NOTIFY4_ADD_ENTRY
- `internal/protocol/nfs/v4/handlers/open.go` - NotifyDirChange hook when OPEN4_CREATE creates a new file
- `internal/protocol/nfs/v4/handlers/setattr.go` - NotifyDirChange for significant directory attribute changes + isSignificantAttrChange helper
- `internal/protocol/nfs/v4/handlers/handler.go` - Wired real handleGetDirDelegation handler (replacing stub)
- `internal/protocol/nfs/v4/state/delegation.go` - OriginClientID field, metrics calls, ReturnDelegation directory flush
- `internal/protocol/nfs/v4/state/dir_delegation.go` - Conflict recall in NotifyDirChange, ShouldRecallDirDelegation, metrics calls
- `internal/protocol/nfs/v4/state/manager.go` - delegationMetrics field and SetDelegationMetrics setter
- `docs/NFS.md` - Directory Delegations section (notifications, recall, configuration, metrics, limitations)

## Decisions Made
- Added OriginClientID to DirNotification for conflict detection (zero means no conflict recall)
- isSignificantAttrChange filters atime/ctime-only changes to reduce notification noise
- REMOVE handler does pre-removal lookup to get child handle for directory delegation revocation
- DelegationMetrics uses shared counters with type label rather than separate metrics per delegation type
- handler.go wired real handleGetDirDelegation (auto-fix: stub was still in dispatch table from Plan 02)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Wired real handleGetDirDelegation in handler.go dispatch table**
- **Found during:** Task 1
- **Issue:** handler.go still had stub for GET_DIR_DELEGATION after Plan 02 added the real handler
- **Fix:** Replaced stub with real handleGetDirDelegation in NewHandler() dispatch table
- **Files modified:** internal/protocol/nfs/v4/handlers/handler.go
- **Verification:** TestGetDirDelegation tests pass
- **Committed in:** 934d8f3d (Task 1 commit)

**2. [Rule 1 - Bug] ReturnDelegation directory delegation flush lock ordering**
- **Found during:** Task 1
- **Issue:** ReturnDelegation held sm.mu while calling flushDirNotifications, but flush needs RLock for backchannel sender lookup
- **Fix:** Rewrote to release sm.mu before flush, then re-acquire for map removal
- **Files modified:** internal/protocol/nfs/v4/state/delegation.go
- **Verification:** go test -race passes, no deadlock
- **Committed in:** 934d8f3d (Task 1 commit)

---

**Total deviations:** 2 auto-fixed (1 blocking, 1 bug)
**Impact on plan:** Both fixes necessary for correctness. No scope creep.

## Issues Encountered
- TestNotifyHook_ConflictRecall initially failed due to race condition reading RecallSent without synchronization (recall runs in goroutine). Fixed by using sm.mu.RLock polling loop instead of time.Sleep.
- TestNotifyHook_DirectoryDeleted initially failed due to incorrect assertion. RevokeDelegation keeps delegation in delegByOther for stale stateid detection (by design). Fixed to check deleg.Revoked flag and delegByFile removal instead.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Phase 24 is complete: directory delegation state model (Plan 01), GET_DIR_DELEGATION handler (Plan 02), and notification hooks (Plan 03) are all implemented
- Ready for Phase 25 (testing) which will validate the full NFSv4.1 feature set

## Self-Check: PASSED

All created files exist on disk. All commit hashes verified in git log.

---
*Phase: 24-directory-delegations*
*Completed: 2026-02-22*

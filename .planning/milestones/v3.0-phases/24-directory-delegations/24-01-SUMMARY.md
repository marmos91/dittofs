---
phase: 24-directory-delegations
plan: 01
subsystem: nfs
tags: [nfsv4.1, directory-delegation, cb_notify, backchannel, xdr]

# Dependency graph
requires:
  - phase: 22-backchannel
    provides: BackchannelSender, EncodeCBRecallOp pattern, ConnWriter, callback_common.go
  - phase: 11-delegations
    provides: DelegationState, GrantDelegation, delegByOther/delegByFile maps, sendRecall pattern
provides:
  - Extended DelegationState with directory delegation fields (IsDirectory, NotificationMask, PendingNotifs, BatchTimer)
  - DirNotification type for batched directory change notifications
  - GrantDirDelegation with limit checking and lease validation
  - NotifyDirChange with time+count batching and CB_NOTIFY delivery
  - NotifyAdd4, NotifyRemove4, NotifyRename4, NotifyAttrChange4 sub-type encoders
  - EncodeCBNotifyOp wire-format helper for CB_NOTIFY callback
  - MaxDelegations and DirDelegBatchWindowMs config fields
affects: [24-02-PLAN (GET_DIR_DELEGATION handler), 24-03-PLAN (CB_NOTIFY delivery hooks)]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Notification batching with per-delegation NotifMu + BatchTimer (drain-and-send pattern)"
    - "Lock ordering: sm.mu before deleg.NotifMu (never reverse)"
    - "Count-based flush (maxBatchSize=100) + timer-based flush (configurable window)"

key-files:
  created:
    - internal/protocol/nfs/v4/state/dir_delegation.go
    - internal/protocol/nfs/v4/state/dir_delegation_test.go
  modified:
    - internal/protocol/nfs/v4/state/delegation.go
    - internal/protocol/nfs/v4/state/callback_common.go
    - internal/protocol/nfs/v4/state/manager.go
    - internal/protocol/nfs/v4/state/v41_client.go
    - internal/protocol/nfs/v4/types/cb_notify.go
    - pkg/controlplane/models/adapter_settings.go

key-decisions:
  - "Separate NotifMu per delegation (not global sm.mu) to avoid holding global lock during backchannel sends"
  - "Directory delegations use same StateTypeDeleg (0x03) type byte as file delegations"
  - "Count-based flush at 100 notifications per delegation, timer-based flush at configurable window (default 50ms)"
  - "directory_deleted recall reason triggers immediate revocation (no CB_RECALL needed)"
  - "purgeV41Client now cleans up all delegations (file+directory) for destroyed clients"

patterns-established:
  - "Drain-and-send pattern: acquire NotifMu, swap slice with nil, release lock, then encode and send"
  - "resetBatchTimer only starts new timer if none exists (let existing timer expire naturally)"
  - "cleanupDirDelegation centralizes timer stop + notification clear for eviction/shutdown paths"

requirements-completed: [DDELEG-03]

# Metrics
duration: 7min
completed: 2026-02-22
---

# Phase 24 Plan 01: Directory Delegation State Model Summary

**Extended DelegationState with directory fields, notification batching via NotifMu+BatchTimer, CB_NOTIFY sub-type encoders, and EncodeCBNotifyOp wire-format helper**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-22T14:15:22Z
- **Completed:** 2026-02-22T14:22:22Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Extended DelegationState with IsDirectory, NotificationMask, CookieVerf, PendingNotifs, NotifMu, BatchTimer, RecallReason fields
- Implemented GrantDirDelegation with limit checking, lease validation, double-grant prevention
- Implemented NotifyDirChange with per-delegation batching (time + count flush triggers)
- Added NotifyAdd4, NotifyRemove4, NotifyRename4, NotifyAttrChange4 XDR sub-type encoders
- Added EncodeCBNotifyOp wire-format helper building complete CB_NOTIFY callback payloads
- Added MaxDelegations and DirDelegBatchWindowMs config fields to NFSAdapterSettings
- Updated purgeV41Client and EvictV40Client to clean up directory delegations
- 14 comprehensive tests covering grant, mask filtering, batching, flush, recall, cleanup, concurrency (all pass with -race)

## Task Commits

Each task was committed atomically:

1. **Task 1: Extend DelegationState, add notification types, CB_NOTIFY encoders, and config fields** - `3d120055` (feat)
2. **Task 2: Implement GrantDirDelegation, NotifyDirChange, batching logic, and tests** - `d9c1f341` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/delegation.go` - Extended DelegationState with directory delegation fields, DirNotification type
- `internal/protocol/nfs/v4/state/dir_delegation.go` - GrantDirDelegation, NotifyDirChange, flushDirNotifications, RecallDirDelegation, cleanupDirDelegation
- `internal/protocol/nfs/v4/state/dir_delegation_test.go` - 14 tests for directory delegation lifecycle
- `internal/protocol/nfs/v4/state/callback_common.go` - EncodeCBNotifyOp wire-format helper
- `internal/protocol/nfs/v4/state/manager.go` - maxDelegations, dirDelegBatchWindow fields; Shutdown cleanup update
- `internal/protocol/nfs/v4/state/v41_client.go` - purgeV41Client delegation cleanup; EvictV40Client dir delegation cleanup
- `internal/protocol/nfs/v4/types/cb_notify.go` - NotifyAdd4, NotifyRemove4, NotifyRename4, NotifyAttrChange4 sub-type encoders
- `pkg/controlplane/models/adapter_settings.go` - MaxDelegations and DirDelegBatchWindowMs config fields

## Decisions Made
- Used separate NotifMu per delegation instead of global sm.mu to avoid holding global lock during backchannel sends (critical for concurrency)
- Directory delegations reuse StateTypeDeleg (0x03) type byte -- same as file delegations (simplifies stateid routing)
- Count-based flush at maxBatchSize=100 prevents unbounded memory growth; timer-based flush at configurable window (default 50ms) ensures timely delivery
- directory_deleted recall reason triggers immediate revocation via RevokeDelegation (no CB_RECALL needed since the directory no longer exists)
- purgeV41Client now iterates delegByOther and cleans up all delegations for the client (previously only sessions were cleaned)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing Critical] Added delegation cleanup to purgeV41Client**
- **Found during:** Task 2 (client cleanup paths)
- **Issue:** purgeV41Client only destroyed sessions but did not clean up delegations; directory delegations would leak on DESTROY_CLIENTID
- **Fix:** Added delegation cleanup loop to purgeV41Client that stops batch timers, recall timers, and removes from both maps
- **Files modified:** internal/protocol/nfs/v4/state/v41_client.go
- **Verification:** TestDirDelegation_V41ClientCleanup passes
- **Committed in:** d9c1f341 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 missing critical)
**Impact on plan:** Essential for correct cleanup. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- State model foundation complete for GET_DIR_DELEGATION handler (Plan 02)
- EncodeCBNotifyOp and notification batching ready for CB_NOTIFY delivery hooks (Plan 03)
- All existing tests pass with race detection -- no regressions

---
*Phase: 24-directory-delegations*
*Completed: 2026-02-22*

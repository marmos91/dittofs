---
phase: 39-cross-protocol-integration-and-documentation
plan: 02
subsystem: locking
tags: [cross-protocol, delegation, lease, break-callbacks, anti-storm, cb-recall]

# Dependency graph
requires:
  - phase: 39-01
    provides: LockManager delegation CRUD, BreakCallbacks.OnDelegationRecall, OpLockBreakScanner delegation timeout
provides:
  - NFS StateManager delegates delegation lifecycle to shared LockManager
  - NFSBreakHandler sends CB_RECALL on cross-protocol delegation recall
  - SMBBreakHandler has no-op OnDelegationRecall (final design)
  - Cross-protocol break coordination helpers (BreakResult, formatting, detection)
  - Anti-storm cache marking in breakDelegations
  - Comprehensive cross-protocol break integration tests
affects: [39-03-documentation, testing, cross-protocol]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Protocol-neutral break dispatch: LockManager dispatches, NFSBreakHandler/SMBBreakHandler translate"
    - "Stateid mapping: delegStateidMap bridges LockManager DelegationID to NFS Stateid4"
    - "Deadlock prevention: release sm.mu before calling LockManager methods that dispatch callbacks"

key-files:
  created:
    - internal/adapter/nfs/v4/state/nfs_break_handler.go
    - pkg/metadata/lock/cross_protocol_break.go
    - pkg/metadata/lock/cross_protocol_break_test.go
  modified:
    - internal/adapter/nfs/v4/state/delegation.go
    - internal/adapter/nfs/v4/state/dir_delegation.go
    - internal/adapter/nfs/v4/state/manager.go
    - pkg/adapter/nfs/adapter.go
    - internal/adapter/smb/lease/notifier.go
    - pkg/metadata/lock/manager.go

key-decisions:
  - "NFSBreakHandler registered per-share (same pattern as SMBBreakHandler) not on single LockManager"
  - "delegStateidMap maps LockManager DelegationID to NFS Stateid4 for wire-format lookup"
  - "breakDelegations marks recentlyBroken cache for unified anti-storm across protocols"
  - "NewManagerWithTTL constructor added for testing with short anti-storm TTL"

patterns-established:
  - "Break handler pattern: protocol adapter creates handler implementing BreakCallbacks, registers on per-share LockManagers"
  - "Mutex release before LockManager calls: capture references under lock, release, then call LockManager"

requirements-completed: [XPROT-01, XPROT-02, XPROT-03]

# Metrics
duration: 9min
completed: 2026-03-02
---

# Phase 39 Plan 02: Cross-Protocol Break Coordination Summary

**NFS StateManager delegates to shared LockManager with NFSBreakHandler dispatching CB_RECALL on cross-protocol conflicts, verified by 16 integration tests**

## Performance

- **Duration:** 9 min
- **Started:** 2026-03-02T16:31:00Z
- **Completed:** 2026-03-02T16:40:00Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- NFS StateManager GrantDelegation/ReturnDelegation/RevokeDelegation mirror state to shared LockManager
- NFSBreakHandler translates LockManager delegation recalls into NFS CB_RECALL messages via backchannel
- Cross-protocol break coordination tested: SMB write breaks NFS delegation, read coexistence verified, anti-storm cache prevents re-grant storms
- Full codebase compiles, all existing tests pass, go vet clean

## Task Commits

Each task was committed atomically:

1. **Task 1: Refactor NFS StateManager to delegate to LockManager and create NFS BreakCallbacks** - `e92e1c28` (feat)
2. **Task 2: Extend SMB adapter, create cross-protocol break coordination, and add integration tests** - `73ae9c3f` (feat)

## Files Created/Modified

- `internal/adapter/nfs/v4/state/delegation.go` - Added LockManagerDelegID field, GetStateidForDelegation, LockManager delegation in GrantDelegation/ReturnDelegation
- `internal/adapter/nfs/v4/state/dir_delegation.go` - GrantDirDelegation mirrors to LockManager with IsDirectory=true
- `internal/adapter/nfs/v4/state/manager.go` - Added delegStateidMap, RevokeDelegation releases mu before LockManager calls, NewManagerWithTTL
- `internal/adapter/nfs/v4/state/nfs_break_handler.go` - New: NFSBreakHandler implementing BreakCallbacks with OnDelegationRecall sending CB_RECALL async
- `pkg/adapter/nfs/adapter.go` - Register NFSBreakHandler on per-share LockManagers in SetRuntime
- `internal/adapter/smb/lease/notifier.go` - Updated OnDelegationRecall comment to reflect final design
- `pkg/metadata/lock/manager.go` - breakDelegations marks recentlyBroken cache, NewManagerWithTTL constructor
- `pkg/metadata/lock/cross_protocol_break.go` - New: BreakResult, FormatCrossProtocolBreak, IsCrossProtocolConflict, ClassifyBreakScenario, CountBreakableState
- `pkg/metadata/lock/cross_protocol_break_test.go` - New: 16 cross-protocol break integration tests

## Decisions Made

- NFSBreakHandler registered on per-share LockManagers (same pattern as SMB adapter) rather than a single global LockManager
- `delegStateidMap` maps LockManager DelegationID (UUID string) to NFS Stateid4 for wire-format lookup in break callbacks
- `breakDelegations` now marks `recentlyBroken` cache (was only done in OnDirChange) to provide anti-storm protection for file delegations
- Added `NewManagerWithTTL` constructor to enable short-TTL testing without accessing internal fields

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing Critical] Added anti-storm marking in breakDelegations**
- **Found during:** Task 2 (cross-protocol break test writing)
- **Issue:** `breakDelegations` did not mark the `recentlyBroken` cache, so delegation breaks from `CheckAndBreakCachingForWrite` would not prevent re-grant storms
- **Fix:** Added `recentlyBroken.Mark(handleKey)` call after breaking delegations
- **Files modified:** `pkg/metadata/lock/manager.go`
- **Verification:** TestCrossProtocolBreak_AntiStormCache passes
- **Committed in:** 73ae9c3f (Task 2 commit)

**2. [Rule 1 - Bug] Fixed unused variable in NFSBreakHandler**
- **Found during:** Task 1 (build verification)
- **Issue:** `stateid` variable declared but not used in OnDelegationRecall (only `found` boolean needed)
- **Fix:** Changed to `_, found := h.stateManager.GetStateidForDelegation(delegID)`
- **Files modified:** `internal/adapter/nfs/v4/state/nfs_break_handler.go`
- **Verification:** go build passes
- **Committed in:** e92e1c28 (Task 1 commit)

---

**Total deviations:** 2 auto-fixed (1 missing critical, 1 bug)
**Impact on plan:** Both fixes necessary for correctness. No scope creep.

## Issues Encountered

- Type name collision between `cross_protocol_break_test.go` and `manager_test.go` (both in same package) -- resolved by prefixing test types with `xp` prefix

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Cross-protocol delegation recall flows are complete and tested
- Both NFS and SMB adapters register BreakCallbacks
- Ready for Phase 39-03 (documentation and finalization)

---
*Phase: 39-cross-protocol-integration-and-documentation*
*Completed: 2026-03-02*

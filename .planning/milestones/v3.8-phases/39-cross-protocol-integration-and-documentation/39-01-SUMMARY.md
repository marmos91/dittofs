---
phase: 39-cross-protocol-integration-and-documentation
plan: 01
subsystem: locking
tags: [delegation, cross-protocol, notification-queue, lock-manager, caching-break]

# Dependency graph
requires:
  - phase: 37-smb3-leases-and-directory-leasing
    provides: "OpLock struct, BreakCallbacks interface, directory lease management, recentlyBrokenCache"
provides:
  - "Protocol-neutral Delegation struct in pkg/metadata/lock/"
  - "Extended UnifiedLock with *Delegation field and IsDelegation() method"
  - "Extended BreakCallbacks with OnDelegationRecall method"
  - "Unified CheckAndBreakCachingFor* methods (break both leases and delegations)"
  - "GrantDelegation/RevokeDelegation/ReturnDelegation/GetDelegation/ListDelegations on LockManager"
  - "WaitForBreakCompletion channel-based waiter"
  - "OpLockBreakScanner delegation recall timeout scanning"
  - "Bounded NotificationQueue with overflow collapse for directory change events"
  - "PersistedLock delegation serialization round-trip"
affects: [39-02-cross-protocol-adapter-wiring, 39-03-documentation]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Delegation as first-class lock type alongside OpLock on UnifiedLock"
    - "Unified caching break methods that handle both leases and delegations"
    - "breakDelegations pattern: collect under write lock, dispatch outside lock"
    - "Channel-based WaitForBreakCompletion with per-handleKey signaling"
    - "Bounded notification queue with overflow collapse to rescan signal"

key-files:
  created:
    - pkg/metadata/lock/delegation.go
    - pkg/metadata/lock/delegation_test.go
    - pkg/metadata/lock/notification_queue.go
    - pkg/metadata/lock/notification_queue_test.go
    - pkg/metadata/lock/delegation_manager_test.go
  modified:
    - pkg/metadata/lock/types.go
    - pkg/metadata/lock/oplock_break.go
    - pkg/metadata/lock/manager.go
    - pkg/metadata/lock/store.go
    - pkg/metadata/lock/directory.go
    - pkg/metadata/lock/leases.go
    - pkg/metadata/lock/cross_protocol.go
    - pkg/metadata/lock/leases_test.go
    - pkg/metadata/lock/manager_test.go
    - pkg/metadata/lock/cross_protocol_test.go
    - internal/adapter/smb/lease/notifier.go

key-decisions:
  - "Delegation struct has zero NFS-specific types (no Stateid4, no *time.Timer)"
  - "Read delegation + Read-only lease coexist; Write delegation conflicts with any lease"
  - "Old CheckAndBreakOpLocksFor* methods delegate to new CheckAndBreakCachingFor* (backward compat)"
  - "WaitForBreakCompletion uses per-handleKey buffered channels with re-check loop"
  - "NotificationQueue overflow collapses all events to empty + overflow flag (rescan signal)"
  - "DelegationRecallTimeout defaults to 90s (configurable on Manager struct)"
  - "OnDirChange breaks both directory leases AND directory delegations in parallel"
  - "Anti-storm cache (recentlyBroken) unified for both leases and delegations"

patterns-established:
  - "Delegation CRUD: GrantDelegation creates UnifiedLock with Delegation field set"
  - "Break coordination: release mutex before dispatching callbacks (deadlock prevention)"
  - "Break wait: signalBreakWait called from ReturnDelegation/RevokeDelegation"

requirements-completed: [XPROT-01, XPROT-03]

# Metrics
duration: 12min
completed: 2026-03-02
---

# Phase 39 Plan 01: Delegation Foundation Summary

**Protocol-neutral Delegation struct with unified caching break methods, delegation CRUD on LockManager, bounded notification queue, and OpLockBreakScanner delegation timeout scanning**

## Performance

- **Duration:** 12 min
- **Started:** 2026-03-02T16:03:17Z
- **Completed:** 2026-03-02T16:15:54Z
- **Tasks:** 2
- **Files modified:** 16

## Accomplishments
- Created protocol-neutral Delegation struct with DelegationType enum, Clone, and coexistence rules
- Extended LockManager with full delegation lifecycle (Grant/Revoke/Return/Get/List) and unified CheckAndBreakCachingFor* methods
- Added WaitForBreakCompletion channel-based waiter for blocking until breaks resolve
- Extended OpLockBreakScanner to scan delegation recall timeouts alongside lease break timeouts
- Extended OnDirChange to break both directory leases and directory delegations
- Extended RequestLease to check delegation coexistence before granting
- Created bounded NotificationQueue with overflow collapse for directory change events
- Extended PersistedLock for delegation field serialization round-trip
- All 170+ existing lock package tests pass with zero regressions

## Task Commits

Each task was committed atomically:

1. **Task 1: Create Delegation struct, extend UnifiedLock and BreakCallbacks, add notification queue** - `23254964` (feat)
2. **Task 2: Add unified CheckAndBreakCaching methods and extend OpLockBreakScanner for delegations** - `968a6a27` (feat)

## Files Created/Modified
- `pkg/metadata/lock/delegation.go` - Protocol-neutral Delegation struct, DelegationType enum, coexistence rules
- `pkg/metadata/lock/delegation_test.go` - Unit tests for Delegation creation, Clone, coexistence, round-trip
- `pkg/metadata/lock/notification_queue.go` - Bounded NotificationQueue with overflow collapse, flush channel
- `pkg/metadata/lock/notification_queue_test.go` - Unit tests for push/drain, overflow, flush, concurrency
- `pkg/metadata/lock/delegation_manager_test.go` - Unit tests for delegation CRUD, caching breaks, wait, dir change
- `pkg/metadata/lock/types.go` - Added *Delegation field and IsDelegation() to UnifiedLock
- `pkg/metadata/lock/oplock_break.go` - Added OnDelegationRecall to BreakCallbacks, delegation timeout scanning
- `pkg/metadata/lock/manager.go` - Delegation CRUD, unified caching breaks, WaitForBreakCompletion
- `pkg/metadata/lock/store.go` - Delegation fields on PersistedLock, serialization in To/FromPersistedLock
- `pkg/metadata/lock/directory.go` - OnDirChange breaks both leases and delegations
- `pkg/metadata/lock/leases.go` - RequestLease checks delegation coexistence
- `pkg/metadata/lock/cross_protocol.go` - FormatDelegationConflict helper
- `internal/adapter/smb/lease/notifier.go` - No-op OnDelegationRecall on SMBBreakHandler

## Decisions Made
- Delegation struct is completely protocol-neutral (no Stateid4, no *time.Timer) - NFS adapter maps between its own types and shared Delegation
- Read delegation + Read-only lease coexist (both are read-only caching); Write delegation conflicts with any lease
- Old CheckAndBreakOpLocksFor* methods are thin wrappers calling new CheckAndBreakCachingFor* for backward compatibility
- WaitForBreakCompletion uses per-handleKey buffered channels with a re-check loop pattern
- NotificationQueue overflow collapses all events and returns empty slice + overflow=true flag
- Default DelegationRecallTimeout is 90s (longer than SMB's 35s lease break timeout, matching NFS conventions)
- Unified anti-storm: recentlyBroken cache prevents rapid re-grant for both leases and delegations

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Updated SMBBreakHandler for new BreakCallbacks interface**
- **Found during:** Task 1 (extending BreakCallbacks interface)
- **Issue:** Adding OnDelegationRecall to BreakCallbacks broke SMBBreakHandler in internal/adapter/smb/lease/notifier.go
- **Fix:** Added no-op OnDelegationRecall method to SMBBreakHandler (Plan 02 will wire it)
- **Files modified:** internal/adapter/smb/lease/notifier.go
- **Verification:** Full project builds cleanly
- **Committed in:** 23254964 (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Necessary to maintain build. No scope creep - just a no-op stub that Plan 02 will implement.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All delegation types and methods are in place for Plan 02 to wire NFS and SMB adapters
- BreakCallbacks interface is complete with OnDelegationRecall - adapters can implement recall dispatch
- CheckAndBreakCachingFor* methods are the new unified break entry points for both protocols
- WaitForBreakCompletion enables blocking callers that need to wait for break resolution
- NotificationQueue is ready for adapters to Push/Drain directory change events

## Self-Check: PASSED

All created files verified to exist. Both commits (23254964, 968a6a27) verified in git log.

---
*Phase: 39-cross-protocol-integration-and-documentation*
*Completed: 2026-03-02*

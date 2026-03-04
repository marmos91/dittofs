---
phase: 37-smb3-leases-and-directory-leasing
plan: 01
subsystem: lock
tags: [smb, leases, directory-leasing, oplock, cross-protocol, lock-manager]

# Dependency graph
requires:
  - phase: 29.4-refactor-unified-locking
    provides: LockManager interface, UnifiedLock, OpLock, BreakCallbacks, cross_protocol.go
provides:
  - Extended LockManager interface with 5 lease methods (RequestLease, AcknowledgeLeaseBreak, ReleaseLease, ReclaimLease, GetLeaseState)
  - DirChangeNotifier interface and Manager implementation for directory lease breaks
  - Recently-broken cache preventing directory lease grant storms
  - OpLock and PersistedLock V2 fields (ParentLeaseKey, IsDirectory)
  - CheckNLMLocksForLeaseConflict in shared pkg/metadata/lock/ package
  - HandleChecker interface for reclaim validation
affects: [37-02, 37-03, smb-adapter, nfs-adapter, cross-protocol]

# Tech tracking
tech-stack:
  added: []
  patterns: [advanceEpoch centralized epoch increment, recentlyBrokenCache TTL-based storm prevention, DirChangeNotifier for cross-protocol directory cache coherency, validUpgrades whitelist for lease state transitions]

key-files:
  created:
    - pkg/metadata/lock/leases.go
    - pkg/metadata/lock/directory.go
    - pkg/metadata/lock/reclaim.go
    - pkg/metadata/lock/lease_interface_test.go
    - pkg/metadata/lock/leases_test.go
    - pkg/metadata/lock/directory_test.go
  modified:
    - pkg/metadata/lock/manager.go
    - pkg/metadata/lock/oplock.go
    - pkg/metadata/lock/store.go
    - pkg/metadata/lock/cross_protocol.go

key-decisions:
  - "advanceEpoch helper centralizes all epoch increments for monotonicity"
  - "Recently-broken cache uses 5s TTL to prevent directory lease grant-break storms"
  - "Cross-key conflicts break to LeaseStateNone (simplest correct behavior)"
  - "Lease upgrade whitelist: R->RW, R->RH, R->RWH, RH->RWH, RW->RWH"
  - "Stub then implement pattern: Task 1 adds interface+stubs, Task 2 replaces with real impl"

patterns-established:
  - "advanceEpoch: Every lease state change goes through this helper"
  - "recentlyBrokenCache: TTL-based per-handle cache for storm prevention"
  - "DirChangeNotifier: Protocol adapters notify on dir entry changes"
  - "validUpgrades map: Whitelist of allowed lease state transitions"

requirements-completed: [LEASE-01, LEASE-02, LEASE-04, ARCH-01]

# Metrics
duration: 9min
completed: 2026-03-02
---

# Phase 37 Plan 01: Lease V2 Foundation Summary

**Shared LockManager lease CRUD with DirChangeNotifier, V2 fields (ParentLeaseKey, IsDirectory), upgrade validation, recently-broken cache, and NLM conflict checking in pkg/metadata/lock/**

## Performance

- **Duration:** 9 min
- **Started:** 2026-03-02T12:09:59Z
- **Completed:** 2026-03-02T12:19:00Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- Extended LockManager interface with 5 lease methods creating a single source of truth for lease state
- Implemented full lease lifecycle: request, upgrade, break acknowledge, release, reclaim
- Added DirChangeNotifier interface and Manager implementation for cross-protocol directory cache coherency
- Added recently-broken cache preventing grant-break storms on busy directories
- Extended OpLock and PersistedLock with V2 fields (ParentLeaseKey, IsDirectory) with full round-trip serialization
- Moved CheckNLMLocksForLeaseConflict from SMB handlers to shared pkg/metadata/lock/ package

## Task Commits

Each task was committed atomically:

1. **Task 1: Extend OpLock, PersistedLock, and LockManager interface with lease V2 fields and methods** - `f32963cb` (feat)
2. **Task 2: Implement lease CRUD methods, DirChangeNotifier, recently-broken cache, and unified reclaim** - `0be445b1` (feat)

## Files Created/Modified
- `pkg/metadata/lock/leases.go` - Lease CRUD implementation (RequestLease, AcknowledgeLeaseBreak, ReleaseLease, GetLeaseState)
- `pkg/metadata/lock/directory.go` - DirChangeNotifier interface, DirChangeType enum, recentlyBrokenCache, OnDirChange
- `pkg/metadata/lock/reclaim.go` - Unified ReclaimLease for both SMB and NFS
- `pkg/metadata/lock/manager.go` - Extended LockManager interface, Manager struct with new fields, delegation methods
- `pkg/metadata/lock/oplock.go` - Added ParentLeaseKey and IsDirectory fields, updated Clone
- `pkg/metadata/lock/store.go` - Added ParentLeaseKey and IsDirectory to PersistedLock, updated ToPersistedLock/FromPersistedLock
- `pkg/metadata/lock/cross_protocol.go` - Added CheckNLMLocksForLeaseConflict shared function
- `pkg/metadata/lock/lease_interface_test.go` - Tests for V2 fields, interface conformance, HandleChecker
- `pkg/metadata/lock/leases_test.go` - Tests for lease CRUD, upgrades, conflicts, epoch tracking
- `pkg/metadata/lock/directory_test.go` - Tests for DirChangeNotifier, recently-broken cache

## Decisions Made
- Used advanceEpoch helper to centralize all epoch increments, ensuring monotonicity across all state changes
- Recently-broken cache uses 5-second TTL (configurable) to prevent directory lease grant-break storms
- Cross-key conflicts break to LeaseStateNone (simplest correct behavior per MS-SMB2)
- Lease upgrade whitelist pattern: explicit map of allowed transitions prevents invalid state machine moves
- Test for AcknowledgeLeaseBreak acknowledges to None when break-to is None (consistent with break dispatch)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed AcknowledgeLeaseBreak test to match break-to-state**
- **Found during:** Task 2 (lease CRUD implementation)
- **Issue:** Test attempted to acknowledge a break to LeaseStateRead when the break-to state was LeaseStateNone
- **Fix:** Updated test to acknowledge to None, added separate test for acknowledging to non-None state
- **Files modified:** pkg/metadata/lock/leases_test.go
- **Verification:** All tests pass
- **Committed in:** 0be445b1 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Test logic correction, no scope change.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Lease CRUD foundation ready for SMB adapter integration (37-02)
- DirChangeNotifier ready to be wired from metadata service
- HandleChecker interface ready to be implemented by metadata stores
- CheckNLMLocksForLeaseConflict ready to replace handler-level implementation

## Self-Check: PASSED

All 6 created files verified present. Both task commits (f32963cb, 0be445b1) verified in git log.

---
*Phase: 37-smb3-leases-and-directory-leasing*
*Completed: 2026-03-02*

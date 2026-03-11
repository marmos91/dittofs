---
phase: 04-smb-leases
plan: 01
subsystem: locking
tags: [smb, leases, oplocks, persistence, ms-smb2]

# Dependency graph
requires:
  - phase: 01-locking-infrastructure
    provides: EnhancedLock type, LockStore interface, PersistedLock schema
provides:
  - LeaseInfo struct with R/W/H state flags
  - Lease state constants matching MS-SMB2 spec
  - EnhancedLock.Lease field for lease integration
  - PersistedLock lease fields for persistence
  - Lease conflict detection in IsEnhancedLockConflicting
  - To/FromPersistedLock lease round-trip
affects: [04-02-smb-leases, 04-03-smb-leases, 11-nfsv4-foundation]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Lease as EnhancedLock extension (not separate type)"
    - "128-bit LeaseKey for client caching unit identification"
    - "Centralized MatchesLock for query filtering"

key-files:
  created:
    - pkg/metadata/lock/lease_types.go
    - pkg/metadata/lock/lease_types_test.go
    - pkg/metadata/lock/store_test.go
  modified:
    - pkg/metadata/lock/types.go
    - pkg/metadata/lock/store.go
    - pkg/metadata/store/memory/locks.go
    - pkg/metadata/store/memory/locks_test.go

key-decisions:
  - "Lease state constants match MS-SMB2 2.2.13.2.8 spec values (0x01, 0x02, 0x04)"
  - "LeaseInfo embedded in EnhancedLock via pointer (nil for byte-range locks)"
  - "Centralized MatchesLock method in LockQuery for consistent filtering"
  - "BreakStarted is runtime-only, not persisted (regenerated on break initiation)"

patterns-established:
  - "IsLease() method pattern for distinguishing lock types"
  - "LeaseKey as [16]byte fixed array (128-bit client key)"
  - "Lease state validation via IsValidFileLeaseState/IsValidDirectoryLeaseState"

# Metrics
duration: 7min
completed: 2026-02-05
---

# Phase 4 Plan 01: SMB Lease Types Summary

**SMB2.1+ lease data model with R/W/H state flags, LeaseInfo struct embedded in EnhancedLock, persistence schema extension, and conflict detection integration**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-05T15:06:26Z
- **Completed:** 2026-02-05T15:13:22Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments

- Created LeaseInfo struct with MS-SMB2 compliant R/W/H state flags and break tracking
- Extended EnhancedLock with Lease *LeaseInfo field for unified lock manager integration
- Added lease persistence fields to PersistedLock with full round-trip conversion
- Integrated lease conflict detection into IsEnhancedLockConflicting (lease vs lease, lease vs byte-range)
- Added IsLease filter to LockQuery for listing leases vs byte-range locks separately

## Task Commits

Each task was committed atomically:

1. **Task 1: Create lease types and extend EnhancedLock** - `158fb4f` (feat)
2. **Task 2: Extend PersistedLock for lease persistence** - `61e6582` (feat)

## Files Created/Modified

- `pkg/metadata/lock/lease_types.go` - Lease constants, LeaseInfo struct, helper methods, conflict detection
- `pkg/metadata/lock/lease_types_test.go` - Comprehensive tests for lease types
- `pkg/metadata/lock/types.go` - EnhancedLock.Lease field, IsLease(), Clone() update, IsEnhancedLockConflicting
- `pkg/metadata/lock/store.go` - PersistedLock lease fields, LockQuery.IsLease, MatchesLock, conversion updates
- `pkg/metadata/lock/store_test.go` - Persistence conversion tests
- `pkg/metadata/store/memory/locks.go` - Updated cloneLock, matchesQuery for lease fields
- `pkg/metadata/store/memory/locks_test.go` - Lease persistence tests

## Decisions Made

- **Lease state constants**: Used exact MS-SMB2 2.2.13.2.8 spec values (0x01=R, 0x02=W, 0x04=H)
- **LeaseInfo as pointer**: EnhancedLock.Lease is *LeaseInfo (nil for byte-range locks) rather than embedded struct to clearly distinguish lock types
- **BreakStarted not persisted**: Runtime-only field regenerated when break is initiated (timer state doesn't survive restart)
- **Centralized filtering**: Added MatchesLock() to LockQuery to avoid duplicating filter logic in each store implementation

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

- Pre-existing race condition detected in connection_test.go (not related to this plan's changes) - tests pass without race detector. The race is in the existing ConnectionTracker tests, not in the new lease code.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- LeaseInfo type and EnhancedLock extension ready for OplockManager refactoring (04-02)
- Persistence schema ready for lease storage and reclaim
- Conflict detection ready for cross-protocol lease breaks
- No blockers identified

---
*Phase: 04-smb-leases*
*Completed: 2026-02-05*

---
phase: 01-locking-infrastructure
plan: 01
subsystem: metadata
tags: [locking, nfs, smb, nlm, posix, deadlock, wait-for-graph]

# Dependency graph
requires: []
provides:
  - EnhancedLock type with protocol-agnostic ownership model
  - POSIX lock splitting (SplitLock, MergeLocks)
  - Atomic lock upgrade (shared to exclusive)
  - Wait-For Graph deadlock detection
  - Lock configuration and limits tracking
affects: [02-nlm-protocol, 03-nfsv4-state, 04-smb2-locking]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Protocol-agnostic OwnerID for cross-protocol locking"
    - "Wait-For Graph with DFS cycle detection"
    - "POSIX lock splitting on unlock (0, 1, or 2 resulting locks)"

key-files:
  created:
    - pkg/metadata/lock_types.go
    - pkg/metadata/lock_deadlock.go
    - pkg/metadata/lock_config.go
  modified:
    - pkg/metadata/locking.go
    - pkg/metadata/locking_test.go
    - pkg/metadata/errors.go
    - pkg/config/config.go

key-decisions:
  - "OwnerID is opaque string - lock manager does not parse protocol prefix"
  - "Enhanced locks stored per-LockManager instance (not global)"
  - "Atomic upgrade returns ErrLockConflict when other readers exist"

patterns-established:
  - "Protocol-agnostic owner ID format: {protocol}:{details}"
  - "Enhanced lock types coexist with legacy FileLock"
  - "Limit tracking via per-file and per-client counters"

# Metrics
duration: 10min
completed: 2026-02-04
---

# Phase 1 Plan 01: Lock Manager Enhancements Summary

**Enhanced LockManager with POSIX splitting, Wait-For Graph deadlock detection, atomic lock upgrade, and configurable limits**

## Performance

- **Duration:** ~10 min
- **Started:** 2026-02-04T15:00:47Z
- **Completed:** 2026-02-04T15:10:26Z
- **Tasks:** 3
- **Files modified:** 7

## Accomplishments

- EnhancedLock type with protocol-agnostic LockOwner enabling cross-protocol lock conflict detection (LOCK-04)
- POSIX-compliant lock splitting: unlocking middle of range creates two locks
- Wait-For Graph deadlock detection using DFS cycle detection
- Atomic lock upgrade from shared to exclusive (only when no other readers)
- Configurable lock limits (per-file, per-client, total) with LockLimits tracker
- New error codes: ErrDeadlock, ErrGracePeriod, ErrLockLimitExceeded, ErrLockConflict

## Task Commits

Each task was committed atomically:

1. **Task 1: Enhanced lock types, POSIX splitting, atomic upgrade** - `1e82f1a`
2. **Task 2: Wait-For Graph deadlock detection** - `5df62c9`
3. **Task 3: Lock configuration and limits** - `ad6721a`

## Files Created/Modified

- `pkg/metadata/lock_types.go` - EnhancedLock, LockOwner, LockType, ShareReservation types
- `pkg/metadata/lock_deadlock.go` - WaitForGraph with cycle detection
- `pkg/metadata/lock_config.go` - LockConfig, LockLimits, LockStats
- `pkg/metadata/lock_config_test.go` - Tests for config and limits
- `pkg/metadata/lock_deadlock_test.go` - Tests for deadlock detection
- `pkg/metadata/locking.go` - SplitLock, MergeLocks, UpgradeLock, enhanced lock storage
- `pkg/metadata/locking_test.go` - Tests for splitting, merging, upgrade, cross-protocol
- `pkg/metadata/errors.go` - New error codes and factory functions
- `pkg/config/config.go` - LockConfig integrated into main config

## Decisions Made

1. **OwnerID as opaque string:** The lock manager treats OwnerID (e.g., "nlm:client1:pid123") as opaque - it never parses the protocol prefix. This enables unified cross-protocol lock conflict detection.

2. **Per-instance enhanced lock storage:** Enhanced locks are stored in each LockManager instance (not a package-level global) to enable proper isolation in tests and multi-share scenarios.

3. **Atomic upgrade semantics:** UpgradeLock returns ErrLockConflict when other readers exist on the range, implementing the user decision for atomic upgrade behavior.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

1. **Race condition with global map:** Initial implementation used a package-level global map for enhanced locks, causing race conditions in parallel tests. Fixed by moving storage into LockManager struct.

2. **Test character overflow:** Long chain test used character math that overflowed the alphabet. Fixed by using numeric string IDs.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Lock manager foundation complete with all required primitives
- Ready for NLM protocol integration (Plan 02)
- Ready for NFSv4 state integration (Plan 03)
- All tests pass including race detector

---
*Phase: 01-locking-infrastructure*
*Completed: 2026-02-04*

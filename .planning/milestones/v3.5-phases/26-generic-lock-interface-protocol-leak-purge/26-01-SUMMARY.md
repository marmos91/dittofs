---
phase: 26-generic-lock-interface-protocol-leak-purge
plan: 01
subsystem: locking
tags: [lock, oplock, unified-lock, access-mode, refactoring, type-rename]

# Dependency graph
requires: []
provides:
  - UnifiedLock composed struct replacing EnhancedLock
  - OpLock type replacing LeaseInfo
  - AccessMode type replacing ShareReservation
  - Protocol-agnostic lock type names across all consumers
  - Updated manager API (AddUnifiedLock, RemoveUnifiedLock, ListUnifiedLocks)
  - Updated persistence layer with new field names
affects: [26-02, 26-03, 26-04, 26-05]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Protocol-agnostic naming: UnifiedLock/OpLock/AccessMode instead of protocol-coupled names"
    - "Composed struct design: UnifiedLock embeds *OpLock pointer (nil for byte-range locks)"

key-files:
  created:
    - pkg/metadata/lock/oplock.go
    - pkg/metadata/lock/oplock_break.go
    - pkg/metadata/lock/oplock_test.go
    - pkg/metadata/lock/oplock_break_test.go
  modified:
    - pkg/metadata/lock/types.go
    - pkg/metadata/lock/manager.go
    - pkg/metadata/lock/store.go
    - pkg/metadata/lock/errors.go
    - pkg/metadata/lock/cross_protocol.go
    - pkg/metadata/lock_exports.go

key-decisions:
  - "Combined Task 1 and Task 2 into atomic commit since types and consumers must be renamed together for compilation"
  - "Kept AccessMode as int enum (not uint32 bitmask) for backward compat -- bitmask conversion deferred to later plan"
  - "Kept LeaseState constants as uint32 (not OpLockState) since they are SMB spec values used extensively"

patterns-established:
  - "UnifiedLock is the canonical lock type for all protocol interactions"
  - "OpLock replaces LeaseInfo for lease/oplock state"
  - "AccessMode replaces ShareReservation for share access control"

requirements-completed: [REF-01]

# Metrics
duration: 7min
completed: 2026-02-25
---

# Phase 26 Plan 01: Lock Type Rename Summary

**Renamed ~940 lock type references across 42 files from protocol-coupled names (EnhancedLock, LeaseInfo, ShareReservation) to protocol-agnostic names (UnifiedLock, OpLock, AccessMode)**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-25T09:15:49Z
- **Completed:** 2026-02-25T09:22:48Z
- **Tasks:** 3
- **Files modified:** 34

## Accomplishments
- Renamed all lock type system from protocol-coupled to protocol-agnostic names
- Updated Manager API: AddUnifiedLock, RemoveUnifiedLock, ListUnifiedLocks, RemoveFileUnifiedLocks
- Renamed lease files: lease_types.go -> oplock.go, lease_break.go -> oplock_break.go
- Updated lock_exports.go with new type re-exports (OpLock, OpLockBreakScanner, etc.)
- All lock package tests pass, metadata tests pass, NFSv4 state tests pass

## Task Commits

Each task was committed atomically:

1. **Task 1+2: Define new type system and mechanical rename** - `0f8865fb` (refactor)
2. **Task 3: Fix test failures from rename** - `10be7567` (test)

## Files Created/Modified

### Created
- `pkg/metadata/lock/oplock.go` - OpLock type (renamed from LeaseInfo), conflict detection, validation
- `pkg/metadata/lock/oplock_break.go` - OpLockBreakScanner (renamed from LeaseBreakScanner)
- `pkg/metadata/lock/oplock_test.go` - Tests for OpLock and conflict detection
- `pkg/metadata/lock/oplock_break_test.go` - Tests for OpLockBreakScanner

### Deleted
- `pkg/metadata/lock/lease_types.go` - Replaced by oplock.go
- `pkg/metadata/lock/lease_break.go` - Replaced by oplock_break.go
- `pkg/metadata/lock/lease_types_test.go` - Replaced by oplock_test.go
- `pkg/metadata/lock/lease_break_test.go` - Replaced by oplock_break_test.go

### Modified (key files)
- `pkg/metadata/lock/types.go` - UnifiedLock struct, AccessMode type, NewUnifiedLock
- `pkg/metadata/lock/manager.go` - AddUnifiedLock, RemoveUnifiedLock, ListUnifiedLocks
- `pkg/metadata/lock/store.go` - Updated PersistedLock conversion, LockQuery
- `pkg/metadata/lock/errors.go` - UnifiedLockConflict type
- `pkg/metadata/lock/cross_protocol.go` - Updated NLM/SMB translation helpers
- `pkg/metadata/lock_exports.go` - Re-exports for new types
- Plus ~25 consumer files across adapters, protocol handlers, and metadata stores

## Decisions Made
- Combined Task 1 (type definitions) and Task 2 (mechanical rename) into a single atomic commit because types and their consumers must be renamed together for compilation
- Kept AccessMode as `int` enum rather than `uint32` bitmask per plan suggestion -- the bitmask conversion is a behavioral change best done in a separate plan
- Kept LeaseState constants unchanged (LeaseStateRead, LeaseStateWrite, etc.) since they are direct MS-SMB2 spec values used extensively

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Fixed test function name casing**
- **Found during:** Task 3
- **Issue:** sed renamed `TestLeaseConflictsWithByteRangeLock_*` to `TestopLockConflictsWithByteLock_*` (lowercase 'o'), which Go rejects for test functions
- **Fix:** Manually capitalized to `TestOpLockConflictsWithByteLock_*`
- **Files modified:** pkg/metadata/lock/oplock_test.go
- **Verification:** `go test ./pkg/metadata/lock/...` passes
- **Committed in:** 10be7567 (Task 3 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Minor naming fix required by Go test conventions. No scope creep.

## Issues Encountered
- Pre-existing build failures in `pkg/controlplane/store/shares.go` and `pkg/controlplane/runtime/init.go` prevent building/testing adapter and protocol handler packages that depend on controlplane. These failures are NOT from our changes (verified by stashing changes). Lock package and its direct consumers compile and test correctly.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All lock types renamed to protocol-agnostic names
- Ready for Plan 02 (further lock interface refinements)
- Pre-existing controlplane build failures should be addressed in a separate effort

## Self-Check: PASSED

- All 10 created/modified files verified present
- All 4 deleted files verified absent
- Both task commits verified (0f8865fb, 10be7567)
- `go build ./pkg/metadata/...` compiles clean
- `go test ./pkg/metadata/lock/...` passes
- Zero references to old type names (EnhancedLock, LeaseInfo, ShareReservation)

---
*Phase: 26-generic-lock-interface-protocol-leak-purge*
*Completed: 2026-02-25*

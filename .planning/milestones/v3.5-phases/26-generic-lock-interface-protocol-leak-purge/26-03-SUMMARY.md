---
phase: 26-generic-lock-interface-protocol-leak-purge
plan: 03
subsystem: locking
tags: [lock, interface, conflict-detection, break-callbacks, cross-protocol]

# Dependency graph
requires: [26-01]
provides:
  - LockManager interface unifying all lock operations
  - ConflictsWith method handling all 4 conflict types
  - BreakCallbacks typed interface for cross-protocol coordination
  - Centralized CheckAndBreakOpLocksFor{Write,Read,Delete} replacing OplockChecker global
  - Grace period delegation through LockManager interface
  - ManagerStats for lock state introspection
  - Comprehensive cross-protocol conflict test coverage
affects: [26-04, 26-05]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Centralized break dispatch: Manager collects registered BreakCallbacks and iterates on break"
    - "Conflict method on type: ConflictsWith on *UnifiedLock replaces standalone IsUnifiedLockConflicting"
    - "Grace period delegation: LockManager delegates to optional GracePeriodManager (nil-safe)"
    - "Typed callbacks: BreakCallbacks interface with OnOpLockBreak/OnByteRangeRevoke/OnAccessConflict"

key-files:
  created: []
  modified:
    - pkg/metadata/lock/manager.go
    - pkg/metadata/lock/types.go
    - pkg/metadata/lock/oplock_break.go
    - pkg/metadata/lock/manager_test.go
    - pkg/metadata/lock/cross_protocol_test.go
    - pkg/metadata/service.go

key-decisions:
  - "ConflictsWith as method on UnifiedLock (not standalone function) for cleaner API"
  - "TestLockByParams wrapper added for backward compat with existing service.go callers"
  - "Break callbacks dispatched outside lock to avoid deadlock (collect under RLock, dispatch unlocked)"
  - "Read leases can coexist per SMB2 spec; Write lease requires exclusivity"

metrics:
  duration: ~25min
  completed: "2026-02-25T09:46:53Z"
---

# Phase 26 Plan 03: LockManager Interface and Conflict Detection Summary

LockManager interface with ConflictsWith handling all 4 conflict types, typed break callbacks, and centralized oplock break replacing OplockChecker global.

## What Was Built

### Task 1: Define LockManager interface and implement ConflictsWith (1ac0951a)

**LockManager interface** in `manager.go` with 5 method groups:
- Unified lock CRUD (AddUnifiedLock, RemoveUnifiedLock, GetUnifiedLock, ListUnifiedLocks, UpgradeLock, RemoveFileUnifiedLocks)
- Centralized break operations (CheckAndBreakOpLocksForWrite/Read/Delete)
- Legacy byte-range (Lock, Unlock, TestLock, ListLocks)
- Grace period delegation (EnterGracePeriod, ExitGracePeriod, IsOperationAllowed, MarkReclaimed, IsInGracePeriod)
- Break callbacks + cleanup (RegisterBreakCallbacks, RemoveAllLocks, RemoveClientLocks, GetStats)

**ConflictsWith method** on `*UnifiedLock` in `types.go` handling all 4 cases:
1. Access mode conflicts (SMB deny modes via `accessModesConflict` helper)
2. OpLock vs OpLock (delegates to `OpLocksConflict`)
3. OpLock vs byte-range (delegates to `opLockConflictsWithByteLock2` bidirectional helper)
4. Byte-range vs byte-range (range overlap + shared/exclusive check)

**BreakCallbacks interface** in `oplock_break.go` with typed methods:
- `OnOpLockBreak(handleKey, lock, breakToState)` for oplock/lease break notifications
- `OnByteRangeRevoke(handleKey, lock, reason)` for byte-range revocation
- `OnAccessConflict(handleKey, existingLock, requestedMode)` for SMB access mode conflicts

**Centralized break methods** on Manager:
- `CheckAndBreakOpLocksForWrite`: Breaks all Read/Write oplocks to None
- `CheckAndBreakOpLocksForRead`: Breaks only Write oplocks to Read
- `CheckAndBreakOpLocksForDelete`: Breaks all oplocks to None
- All methods support `excludeOwner` parameter
- Break callbacks dispatched outside lock to prevent deadlock

**Additional changes:**
- `ManagerStats` struct for lock state introspection
- `NewManagerWithGracePeriod` constructor for grace period support
- `TestLockByParams` backward compat wrapper for service.go callers
- Compile-time check: `var _ LockManager = (*Manager)(nil)`
- AddUnifiedLock updated to use `ConflictsWith` instead of `IsUnifiedLockConflicting`

### Task 2: Rewrite lock tests (8db29d7b)

**manager_test.go rewritten** with new tests:
- LockManager interface compliance test
- TestLock/TestLockByParams with new FileLock parameter signature
- Break callback tests (Write/Read/Delete with owner exclusion)
- RemoveAllLocks, RemoveClientLocks, GetStats tests
- Grace period delegation (with and without GracePeriodManager)
- GetUnifiedLock, AddUnifiedLock ConflictsWith verification

**cross_protocol_test.go expanded** with comprehensive ConflictsWith tests:
- Case 1 (Access mode): DenyRead, DenyAll, BothNone
- Case 2 (OpLock-OpLock): Read-Read, Read-Write, Write-Write, same LeaseKey, Handle-only
- Case 3 (OpLock-ByteRange): Write oplock vs exclusive, Read oplock vs shared, bidirectional symmetry
- Case 4 (Byte-Range): Exclusive overlap, shared overlap, no overlap, shared+exclusive
- Same owner never conflicts test
- Cross-protocol scenario: NFS delegation broken by SMB write via CheckAndBreakOpLocksForWrite

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Test scenarios had conflicting lock setup**
- **Found during:** Task 2
- **Issue:** Test cases for break callbacks tried to add multiple Write oplocks from different owners on the same file, which ConflictsWith correctly rejects (Write leases require exclusive access per SMB2 spec)
- **Fix:** Changed test scenarios to use Read-only oplocks where multiple owners needed (Read leases can coexist), and tested Write break behavior with single Write oplock
- **Files modified:** pkg/metadata/lock/manager_test.go
- **Commit:** 8db29d7b

**2. [Rule 1 - Bug] RemoveClientLocks test used conflicting exclusive locks**
- **Found during:** Task 2
- **Issue:** Test added exclusive locks from different owners on the same file, which ConflictsWith correctly rejects
- **Fix:** Changed to shared locks with explicit offset/length so they don't conflict
- **Files modified:** pkg/metadata/lock/manager_test.go
- **Commit:** 8db29d7b

## Verification Results

1. `go build ./pkg/metadata/lock/...` - PASS (compiles cleanly)
2. `go test ./pkg/metadata/lock/...` - PASS (all tests pass)
3. LockManager interface exists in manager.go - CONFIRMED
4. ConflictsWith method exists in types.go - CONFIRMED
5. Typed callbacks (OnOpLockBreak, OnByteRangeRevoke, OnAccessConflict) in oplock_break.go - CONFIRMED
6. CheckAndBreakOpLocksFor{Write,Read,Delete} in manager.go - CONFIRMED
7. Cross-protocol tests cover all 4 conflict cases - CONFIRMED (16 ConflictsWith tests + 1 cross-protocol scenario)

## Notes

- Pre-existing NLM handler build errors from phase 26-02 remain out of scope (verified by stashing changes)
- `IsUnifiedLockConflicting` standalone function kept for backward compat (existing tests use it)
- `OpLockBreakCallback` (scanner timeout callback) is distinct from `BreakCallbacks` (typed protocol callback) - both serve different purposes

## Self-Check: PASSED

- All 6 referenced files exist on disk
- Commit 1ac0951a (Task 1): FOUND
- Commit 8db29d7b (Task 2): FOUND
- Lock package builds and all tests pass

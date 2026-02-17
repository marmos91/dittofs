---
phase: 15-v2-0-testing
plan: 02
subsystem: testing
tags: [nfsv4, e2e, locking, delegations, fcntl, byte-range-locks, CB_RECALL]

# Dependency graph
requires:
  - phase: 15-v2-0-testing
    plan: 01
    provides: MountNFSWithVersion helper, SkipIfDarwin, setupNFSv4TestServer
  - phase: 10-nfsv4-locking
    provides: NFSv4 LOCK/LOCKU/LOCKT handlers
  - phase: 11-delegations
    provides: Delegation grant/recall/revoke state management and CB_RECALL callback client
provides:
  - NFSv4 locking E2E tests with 6 subtests parameterized for v3/v4.0
  - NFSv4 blocking lock test (v4-only F_SETLKW semantics)
  - NFSv4 delegation lifecycle E2E tests with server-observable grant/recall
  - Log-based delegation observability helpers (readLogFile, extractNewLogs)
affects: [15-03, 15-04, 15-05]

# Tech tracking
tech-stack:
  added: []
  patterns: [log-based delegation observability, cross-mount-point conflict testing, version-parameterized locking subtests]

key-files:
  created:
    - test/e2e/nfsv4_locking_test.go
    - test/e2e/nfsv4_delegation_test.go
  modified: []

key-decisions:
  - "Log approach (fallback) for delegation observability since Prometheus metrics not yet instrumented"
  - "Two mount points per test for cross-client conflict simulation (different NFS open-owners)"
  - "POSIX fcntl locks (F_SETLK/F_SETLKW) for both v3 (NLM) and v4 (integrated locking)"
  - "Graceful handling of same-process POSIX lock semantics (per-process, not per-fd)"
  - "Delegation tests are NFSv4-only; locking tests parameterized for v3+v4"

patterns-established:
  - "Cross-mount-point testing: mount same share twice for multi-client simulation"
  - "Server log scraping: readLogFile + extractNewLogs for observable server behavior"
  - "Delegation observability: check 'Delegation granted' and 'CB_RECALL sent' log messages"

requirements-completed: [TEST2-02, TEST2-03]

# Metrics
duration: 4min
completed: 2026-02-17
---

# Phase 15 Plan 02: NFSv4 Locking and Delegation E2E Tests Summary

**NFSv4 byte-range locking E2E tests with 6 v3/v4 parameterized subtests plus blocking lock, and 4 delegation lifecycle tests with server-observable grant/recall via log scraping**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-17T17:14:02Z
- **Completed:** 2026-02-17T17:18:09Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- TestNFSv4Locking with 6 subtests (ReadWriteLocks, ExclusiveLock, OverlappingRanges, LockUpgrade, LockUnlock, CrossClientConflict) parameterized for v3 and v4.0
- TestNFSv4BlockingLock for NFSv4-only blocking lock semantics using F_SETLKW with goroutine-based lock acquisition monitoring
- 4 delegation test functions: BasicLifecycle (grant verification), Recall (multi-client with CB_RECALL log check), Revocation (unresponsive client), NoDelegationConflict (concurrent reads)
- Log-based delegation observability using readLogFile/extractNewLogs helpers, with TODO for metrics upgrade per locked decision #7

## Task Commits

Each task was committed atomically:

1. **Task 1: NFSv4 locking E2E tests** - `b6a3b0e` (feat)
2. **Task 2: NFSv4 delegation E2E tests** - `c07a540` (feat)

## Files Created/Modified
- `test/e2e/nfsv4_locking_test.go` - TestNFSv4Locking (6 subtests for v3+v4), TestNFSv4BlockingLock (v4-only)
- `test/e2e/nfsv4_delegation_test.go` - TestNFSv4DelegationBasicLifecycle, TestNFSv4DelegationRecall, TestNFSv4DelegationRevocation, TestNFSv4NoDelegationConflict + log scraping helpers

## Decisions Made
- Used log approach (fallback) for delegation observability since Prometheus delegation metrics (dittofs_nfs_delegations_granted_total, dittofs_nfs_delegations_recalled_total) are not yet instrumented. Tests check server logs for "Delegation granted" and "CB_RECALL sent" messages. TODO comment references locked decision #7.
- Two separate mount points per test to simulate different NFS clients (Linux kernel uses different open-owners per mount point)
- Locking tests use existing framework.LockFileRange/TryLockFileRange helpers (fcntl-based) which work for both v3 (NLM) and v4 (integrated locking)
- Same-process POSIX lock semantics handled gracefully: tests log platform behavior when locks from the same process don't conflict (POSIX per-process semantics)
- lock_helpers.go confirmed sufficient -- no additions needed (existing LockFile, TryLockFile, LockFileRange, etc. work for NFSv4 integrated locking)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- NFSv4 locking and delegation E2E tests ready for CI execution on Linux
- All tests skip gracefully on macOS (SkipIfDarwin, SkipIfNFSv4Unsupported, SkipIfNFSLockingUnsupported)
- Log-based delegation observability established as pattern for Plan 15-03 if needed

## Self-Check: PASSED

- All created files exist on disk
- Commit b6a3b0e (Task 1) verified in git log
- Commit c07a540 (Task 2) verified in git log
- `go build -tags=e2e ./test/e2e/...` compiles successfully
- `go vet -tags=e2e ./test/e2e/...` passes
- lock_helpers.go confirmed with 10 expected exports

---
*Phase: 15-v2-0-testing*
*Completed: 2026-02-17*

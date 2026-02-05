---
phase: 04-smb-leases
plan: 02
subsystem: locking
tags: [smb, leases, oplocks, persistence, ms-smb2, cross-protocol]

# Dependency graph
requires:
  - phase: 04-smb-leases
    provides: LeaseInfo type, EnhancedLock.Lease field, PersistedLock lease fields
  - phase: 01-locking-infrastructure
    provides: EnhancedLock type, LockStore interface, connection tracking
provides:
  - OplockManager with LockStore delegation for lease persistence
  - RequestLease/AcknowledgeLeaseBreak methods for SMB2.1+ leases
  - LeaseBreakScanner for timeout management (35s default)
  - Cross-protocol integration via CheckAndBreakForWrite/Read
  - LeaseBreakCallback interface for timeout notification
affects: [04-03-smb-leases, 11-nfsv4-foundation]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "LockStore delegation from OplockManager (protocol-to-persistence bridge)"
    - "LeaseBreakScanner background goroutine with configurable interval"
    - "Cross-protocol break triggering via CheckAndBreakForWrite/Read"

key-files:
  created:
    - internal/protocol/smb/v2/handlers/lease.go
    - pkg/metadata/lock/lease_break.go
    - pkg/metadata/lock/lease_break_test.go
  modified:
    - internal/protocol/smb/v2/handlers/oplock.go

key-decisions:
  - "LockStore dependency injected via NewOplockManagerWithStore constructor"
  - "Break timeout 35 seconds (Windows default per MS-SMB2)"
  - "Scan interval 1 second for balance of responsiveness and efficiency"
  - "BreakStarted time stored in AcquiredAt for scanner (persisted field)"
  - "Session tracking map for break notification routing"

patterns-established:
  - "OplockManager implements LeaseBreakCallback for scanner integration"
  - "Lease operations delegate to LockStore for persistence"
  - "Cross-protocol breaks triggered before NFS/NLM operations"

# Metrics
duration: 7min
completed: 2026-02-05
---

# Phase 4 Plan 02: OplockManager Refactoring Summary

**OplockManager refactored to delegate to unified LockStore, LeaseBreakScanner with 35s timeout, RequestLease/AcknowledgeLeaseBreak APIs, and cross-protocol break integration**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-05T15:17:30Z
- **Completed:** 2026-02-05T15:24:43Z
- **Tasks:** 2
- **Files created/modified:** 4

## Accomplishments

- Refactored OplockManager to use LockStore for lease persistence
- Implemented RequestLease/AcknowledgeLeaseBreak/ReleaseLease methods
- Created LeaseBreakScanner with configurable timeout (default 35s)
- Added CheckAndBreakForWrite/Read for cross-protocol integration
- Created lease.go with SMB2.1+ lease types and wire format helpers
- Backward compatible with existing oplock API

## Task Commits

Each task was committed atomically:

1. **Task 1: Refactor OplockManager to delegate to unified lock manager** - `aa62fe3` (feat)
2. **Task 2: Implement lease break timer scanner** - `25b4abd` (feat)

## Files Created/Modified

- `internal/protocol/smb/v2/handlers/oplock.go` - Refactored with LockStore dependency, scanner integration
- `internal/protocol/smb/v2/handlers/lease.go` - Lease-specific methods and SMB2 wire format types
- `pkg/metadata/lock/lease_break.go` - LeaseBreakScanner implementation
- `pkg/metadata/lock/lease_break_test.go` - Comprehensive scanner tests

## Decisions Made

- **LockStore injection pattern**: NewOplockManagerWithStore constructor for unified lock manager integration, preserving backward compatibility with NewOplockManager
- **Break timeout 35 seconds**: Per MS-SMB2 specification and Windows default behavior
- **Scan interval 1 second**: Reasonable balance between responsiveness and CPU usage
- **Session tracking via map**: sessionMap[leaseKeyHex] -> sessionID for break notification routing
- **AcquiredAt as break start**: Reusing persisted field for break timing (scanner checks AcquiredAt + timeout)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

- Pre-existing race condition in connection_test.go detected (not related to this plan's changes, documented in 04-01-SUMMARY.md)
- Scanner tests initially failed due to default 1-second scan interval being too slow for 150ms test timeouts - added NewLeaseBreakScannerWithInterval for testing with 10ms interval

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- OplockManager fully integrated with unified lock manager
- Lease break timeout enforcement active
- Cross-protocol break triggers ready for NFS handler integration
- Ready for 04-03: Lease break notification and timeout handling completion

---
*Phase: 04-smb-leases*
*Completed: 2026-02-05*

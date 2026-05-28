---
phase: 05-cross-protocol-integration
plan: 01
subsystem: locking
tags: [cross-protocol, nlm, smb, leases, metrics, prometheus]

# Dependency graph
requires:
  - phase: 01-locking-infrastructure
    provides: LockStore interface, EnhancedLock type, lock persistence
  - phase: 04-smb-leases
    provides: LeaseInfo type, SMB lease support in EnhancedLock
provides:
  - UnifiedLockView for cross-protocol lock visibility
  - NLM holder translation from SMB leases
  - Cross-protocol conflict metrics
  - MetadataService integration for per-share UnifiedLockView
affects: [05-cross-protocol-integration/02, 05-cross-protocol-integration/03]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - UnifiedLockView abstraction over LockStore for protocol-agnostic queries
    - Cross-protocol translation helpers for NLM holder info
    - Per-share view ownership in MetadataService

key-files:
  created:
    - pkg/metadata/unified_view.go
    - pkg/metadata/unified_view_test.go
    - pkg/metadata/lock/cross_protocol.go
    - pkg/metadata/lock/cross_protocol_test.go
  modified:
    - pkg/metadata/lock/metrics.go
    - pkg/metadata/service.go

key-decisions:
  - "UnifiedLockView placed in pkg/metadata/ (not pkg/metadata/lock/) per CONTEXT.md"
  - "Per-share UnifiedLockView ownership matches LockManager pattern"
  - "NLM holder info uses first 8 bytes of 16-byte LeaseKey as OH field"
  - "Cross-protocol metrics use descriptive label constants"

patterns-established:
  - "FileLocksInfo separates ByteRangeLocks and Leases for easy processing"
  - "TranslateToNLMHolder converts SMB lease to NLM-compatible holder info"
  - "RecordCrossProtocolConflict/RecordCrossProtocolBreakDuration for observability"

# Metrics
duration: 7min
completed: 2026-02-05
---

# Phase 5 Plan 1: Unified Lock View Summary

**UnifiedLockView API for cross-protocol lock visibility with NLM holder translation and Prometheus metrics**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-05T19:26:31Z
- **Completed:** 2026-02-05T19:33:07Z
- **Tasks:** 3
- **Files modified:** 6

## Accomplishments

- UnifiedLockView queries all locks (NLM byte-range and SMB leases) on a file through single API
- Translation helpers convert SMB leases to NLM holder format for NLM4_DENIED responses
- Cross-protocol Prometheus metrics for conflict counting and break duration tracking
- MetadataService owns UnifiedLockView per share for protocol handler access

## Task Commits

Each task was committed atomically:

1. **Task 1: Create UnifiedLockView struct and query API** - `04492f8` (feat)
2. **Task 2: Create cross-protocol translation helpers** - `76cd6ce` (feat)
3. **Task 3: Add cross-protocol Prometheus metrics** - `f722867` (feat)

## Files Created/Modified

- `pkg/metadata/unified_view.go` - UnifiedLockView struct with GetAllLocksOnFile, HasConflictingLocks, GetLeaseByKey, GetWriteLeases, GetHandleLeases
- `pkg/metadata/unified_view_test.go` - Comprehensive test coverage for UnifiedLockView
- `pkg/metadata/lock/cross_protocol.go` - NLMHolderInfo, TranslateToNLMHolder, TranslateSMBConflictReason, TranslateNFSConflictReason
- `pkg/metadata/lock/cross_protocol_test.go` - Test coverage for translation helpers
- `pkg/metadata/lock/metrics.go` - cross_protocol_conflict_total, cross_protocol_break_duration_seconds metrics
- `pkg/metadata/service.go` - UnifiedLockView field and accessor methods

## Decisions Made

- **UnifiedLockView location:** Placed in pkg/metadata/ per CONTEXT.md decision for cleanliness while being owned by MetadataService
- **OH field translation:** Use first 8 bytes of 16-byte LeaseKey since NLM OH is typically 8 bytes
- **Label constants:** Created descriptive constants (InitiatorNFS, ConflictingSMBLease, ResolutionBreakInitiated) for metric labels
- **Histogram buckets:** 0.1s to 100s exponential for break duration, covering 35s SMB timeout

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None - all verification passed on first attempt.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- UnifiedLockView foundation complete
- Ready for Plan 02: NFS protocol integration with cross-protocol conflict detection
- Ready for Plan 03: SMB protocol integration with cross-protocol visibility

---
*Phase: 05-cross-protocol-integration*
*Completed: 2026-02-05*

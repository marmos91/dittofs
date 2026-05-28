---
phase: 05-cross-protocol-integration
plan: 02
subsystem: cross-protocol-locking
tags: [nlm, smb, leases, oplock, nfs, remove, rename]
dependency-graph:
  requires: ["05-01"]
  provides: ["NLM-SMB lease integration", "NFS delete lease break"]
  affects: ["05-03"]
tech-stack:
  added: []
  patterns: ["Cross-protocol lease break", "OplockChecker interface pattern"]
key-files:
  created:
    - internal/protocol/nlm/handlers/cross_protocol.go
  modified:
    - internal/protocol/nlm/handlers/lock.go
    - internal/protocol/nlm/handlers/handler.go
    - internal/protocol/nfs/v3/handlers/remove.go
    - internal/protocol/nfs/v3/handlers/rename.go
    - internal/protocol/smb/v2/handlers/oplock.go
    - pkg/metadata/service.go
    - pkg/config/config.go
    - pkg/config/defaults.go
    - pkg/adapter/smb/smb_adapter.go
decisions:
  - id: "05-02-01"
    title: "Handle lease break proceeds even on timeout"
    context: "Delete operations should not be blocked indefinitely by unresponsive SMB clients"
    choice: "Proceed with delete after lease break timeout expires"
    rationale: "Per Windows behavior - delete completes, SMB client gets error on next I/O"
  - id: "05-02-02"
    title: "Break ALL leases to None for delete operations"
    context: "File deletion invalidates all lease types (R/W/H)"
    choice: "initiateLeaseBreak with LeaseStateNone for CheckAndBreakForDelete"
    rationale: "Handle lease is about delete notification; R/W become stale when file deleted"
metrics:
  duration: "~15 minutes"
  completed: "2026-02-05"
---

# Phase 05 Plan 02: NLM-SMB Integration Summary

**One-liner:** NLM LOCK waits for SMB lease breaks; NFS REMOVE/RENAME break Handle leases before proceeding.

## What Was Built

### 1. Configurable Lease Break Timeout (Previously Complete)
- **File:** `pkg/config/config.go`, `pkg/config/defaults.go`
- Added `LeaseBreakTimeout` to `LockConfig` struct
- Default: 35 seconds (SMB2 spec maximum, MS-SMB2 2.2.23)
- Override for CI: `DITTOFS_LOCK_LEASE_BREAK_TIMEOUT=5s`

### 2. NLM Cross-Protocol Helpers (Previously Complete)
- **File:** `internal/protocol/nlm/handlers/cross_protocol.go`
- `waitForLeaseBreak()`: Polls at 100ms interval until lease break completes or timeout
- `buildDeniedResponseFromSMBLease()`: Creates NLM4_DENIED with SMB holder info
- `checkForSMBLeaseConflicts()`: Checks and initiates SMB lease breaks before NLM lock
- `getLeaseBreakTimeout()`: Returns configured timeout (default 35s)

### 3. NLM LOCK SMB Lease Checking (Previously Complete)
- **File:** `internal/protocol/nlm/handlers/lock.go`
- Before acquiring lock: `checkForSMBLeaseConflicts()` called
- On conflict with SMB lease: Returns NLM4_DENIED with SMB holder info
- Records cross-protocol conflict metrics

### 4. OplockChecker Interface Extension
- **File:** `pkg/metadata/service.go`
- Added `CheckAndBreakForDelete()` to `OplockChecker` interface
- Added `MetadataService.CheckAndBreakLeasesForDelete()` wrapper method
- Handle lease (H) protection for file deletion operations

### 5. NFS REMOVE Handle Lease Break
- **File:** `internal/protocol/nfs/v3/handlers/remove.go`
- Before `RemoveFile()`: Looks up file handle and breaks Handle leases
- Logs at INFO level (cross-protocol working as designed)
- Proceeds with delete even if lease break pending (Windows behavior)

### 6. NFS RENAME Handle Lease Break
- **File:** `internal/protocol/nfs/v3/handlers/rename.go`
- Breaks Handle leases on BOTH source AND destination files
- Source: File being moved/renamed
- Destination: File being replaced (if exists)

### 7. OplockManager.CheckAndBreakForDelete
- **File:** `internal/protocol/smb/v2/handlers/oplock.go`
- Queries all leases (R/W/H) on the file
- Initiates break to `LeaseStateNone` for delete operations
- Returns `ErrLeaseBreakPending` if break initiated

### 8. SMB Adapter OplockChecker Registration
- **File:** `pkg/adapter/smb/smb_adapter.go`
- `SetRuntime()` now registers `OplockManager` with `metadata.SetOplockChecker()`
- Enables NFS handlers to call `CheckAndBreakLeasesForDelete()`

## Cross-Protocol Flow

### NLM LOCK with SMB Lease Conflict
```
1. NFS client requests NLM LOCK
2. Handler calls checkForSMBLeaseConflicts()
3. OplockChecker.CheckAndBreakForWrite() initiates lease break
4. Handler waits (polling at 100ms) for break completion
5. After timeout: proceed anyway (lease may or may not be broken)
6. If lock conflicts with active SMB lease: return NLM4_DENIED with holder info
```

### NFS DELETE with SMB Handle Lease
```
1. NFS client sends REMOVE request
2. Handler looks up file handle
3. MetadataService.CheckAndBreakLeasesForDelete() called
4. OplockManager.CheckAndBreakForDelete() initiates H lease break to None
5. SMB client receives LEASE_BREAK notification
6. Handler proceeds with deletion (doesn't wait for acknowledgment)
7. SMB client gets error on next I/O to deleted file
```

## Key Design Decisions

### Timeout Behavior
- Lease break timeout is configurable (default 35s, CI uses 5s)
- On timeout: operation proceeds (not indefinitely blocked)
- Per Windows behavior: Delete completes, SMB client handles error

### Lease Break to None for Delete
- Delete operations break ALL lease components (R/W/H) to None
- File is being removed - no lease state makes sense after deletion
- H lease exists specifically for delete notification

### Cross-Protocol Logging
- Conflicts logged at INFO level (not WARN/ERROR)
- These are "working as designed" scenarios
- Metrics recorded via `RecordCrossProtocolConflict()`

## Commits

| Commit | Description |
|--------|-------------|
| 5b0dfb2 | (Prior) NLM cross_protocol.go, lock.go, config changes |
| 0627a3d | Handle lease break in NFS REMOVE/RENAME |
| 585b9cb | CheckAndBreakForDelete and OplockChecker registration |

## Deviations from Plan

### Task 1 Already Complete
Task 1 (configurable lease break timeout and NLM cross-protocol helpers) was already implemented in prior commits labeled as 05-03. The work was complete and verified working. No additional changes needed.

## Next Phase Readiness

**Ready for 05-03:** SMB-to-NFS integration (SMB operations check NLM locks).

The OplockChecker interface and registration pattern established here will be reused for:
- SMB CREATE checking NLM locks before granting leases
- SMB WRITE checking NLM locks before proceeding
- Unified cross-protocol conflict resolution

## Verification Checklist

- [x] NLM LOCK handler checks for SMB leases before acquiring
- [x] Lease break timeout is configurable (default 35s, CI 5s)
- [x] NFS REMOVE breaks Handle leases before deletion
- [x] NFS RENAME breaks Handle leases on source and destination
- [x] OplockChecker interface includes CheckAndBreakForDelete
- [x] SMB adapter registers OplockManager with MetadataService
- [x] All builds pass
- [x] All tests pass with race detector

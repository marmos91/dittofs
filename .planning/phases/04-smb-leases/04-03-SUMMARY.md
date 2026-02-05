---
phase: 04-smb-leases
plan: 03
subsystem: cross-protocol-leases
tags: [smb, nfs, lease, oplock, cross-protocol]
dependency-graph:
  requires: ["04-01", "04-02"]
  provides: ["cross-protocol-lease-breaks", "smb-create-lease-context"]
  affects: ["05-kerberos"]
tech-stack:
  added: []
  patterns: ["cross-protocol-visibility", "create-context-handling"]
key-files:
  created:
    - internal/protocol/smb/v2/handlers/lease_context.go
    - internal/protocol/smb/v2/handlers/lease_context_test.go
  modified:
    - internal/protocol/smb/v2/handlers/oplock.go
    - internal/protocol/smb/v2/handlers/create.go
    - internal/protocol/nfs/v3/handlers/write.go
    - internal/protocol/nfs/v3/handlers/read.go
    - pkg/metadata/service.go
decisions:
  - id: "04-03-01"
    choice: "35-second lease break timeout"
    reason: "Matches Windows MS-SMB2 default break timeout"
  - id: "04-03-02"
    choice: "Polling-based lease break wait"
    reason: "Simple implementation with 100ms interval"
  - id: "04-03-03"
    choice: "OplockChecker interface in MetadataService"
    reason: "Clean cross-protocol visibility without circular imports"
metrics:
  duration: "15 minutes"
  completed: "2026-02-05"
---

# Phase 04 Plan 03: Cross-Protocol Lease Breaks and SMB CREATE Lease Context

NFS operations trigger SMB lease breaks; CREATE parses/responds lease contexts per MS-SMB2

## One-Liner

Cross-protocol lease visibility with NFS triggering SMB Write lease breaks and CREATE handling RqLs/RsLs contexts

## What Was Done

### Task 1: Cross-Protocol Break Triggering from NFS

Added ErrLeaseBreakPending error and updated oplock.go CheckAndBreakFor{Write,Read} methods:

- **CheckAndBreakForWrite**: NFS write conflicts with Write lease (client has cached writes to flush) and Read lease (cached reads now stale). Returns ErrLeaseBreakPending when break initiated.

- **CheckAndBreakForRead**: NFS read only conflicts with Write lease (uncommitted writes). Read leases coexist with NFS reads - no break needed.

Created cross-protocol interface in MetadataService:

- **OplockChecker interface**: Defines CheckAndBreakForWrite/Read methods
- **SetOplockChecker/GetOplockChecker**: Global registration for SMB adapter
- **CheckAndBreakLeasesFor{Write,Read}**: MetadataService methods for NFS handlers

Updated NFS handlers with lease break wait logic:

- **waitForLeaseBreak helper**: Polls for break completion with 35s timeout
- **WRITE handler**: Calls CheckAndBreakLeasesForWrite before PrepareWrite
- **READ handler**: Calls CheckAndBreakLeasesForRead before content read

### Task 2: SMB CREATE Lease Context Support

Created lease_context.go with create context integration:

- **LeaseContextTagRequest/Response**: Constants for "RqLs"/"RsLs" tags
- **LeaseResponseContext**: Struct for building lease responses
- **FindCreateContext**: Helper to search create contexts by name
- **ProcessLeaseCreateContext**: Full CREATE handler integration
- **EncodeCreateContexts**: Wire format encoding per MS-SMB2 2.2.14

Updated CREATE handler:

- Detects OplockLevel=0xFF (lease request) and processes RqLs context
- Calls OplockManager.RequestLease with parsed parameters
- Adds RsLs response context with granted state and epoch

Added comprehensive tests in lease_context_test.go.

## Key Changes

### Cross-Protocol Flow

```
NFS WRITE Request
    |
    v
CheckAndBreakLeasesForWrite(ctx, handle)
    |
    v (if SMB adapter running)
OplockManager.CheckAndBreakForWrite()
    |
    v (if Write lease found)
initiateLeaseBreak() -> SendLeaseBreak notification
    |
    v
return ErrLeaseBreakPending
    |
    v
waitForLeaseBreak() polls until:
  - Break acknowledged
  - Timeout (35s) -> proceed anyway
  - Context cancelled
    |
    v
Proceed with NFS WRITE
```

### CREATE Lease Context Flow

```
SMB CREATE with OplockLevel=0xFF
    |
    v
FindCreateContext(req.CreateContexts, "RqLs")
    |
    v (if found)
DecodeLeaseCreateContext(ctxData)
    |
    v
OplockManager.RequestLease(fileHandle, leaseKey, ...)
    |
    v
LeaseResponseContext{grantedState, epoch}
    |
    v
resp.CreateContexts = append(..., {Name: "RsLs", Data: ...})
```

## Files Changed

| File | Change |
|------|--------|
| oplock.go | Added ErrLeaseBreakPending, updated CheckAndBreakFor{Write,Read} |
| service.go | Added OplockChecker interface and CheckAndBreakLeasesFor{Write,Read} |
| write.go | Added cross-protocol lease check before PrepareWrite |
| read.go | Added cross-protocol lease check before content read |
| create.go | Process RqLs context, add RsLs response context |
| lease_context.go | NEW - Create context helpers and encoding |
| lease_context_test.go | NEW - Comprehensive test coverage |

## Verification

All tests pass:

```bash
go build ./...                                    # Full build OK
go test ./internal/protocol/smb/... -race         # SMB tests pass
go test ./internal/protocol/nfs/... -race         # NFS tests pass
go test ./pkg/metadata/... -race                  # Metadata tests pass
```

## Decisions Made

1. **35-second lease break timeout**: Matches Windows MS-SMB2 default. Timeout scanner will force-revoke anyway.

2. **Polling-based wait**: Simple 100ms poll interval. Could optimize with channels but adds complexity.

3. **OplockChecker interface**: Clean separation - NFS doesn't import SMB packages, just uses interface through MetadataService.

4. **Continue on timeout**: If lease break times out, proceed with NFS operation. Scanner will force-revoke and SMB client will learn on next operation.

## Deviations from Plan

None - plan executed exactly as written.

## Next Steps

Phase 04 is now complete. All three plans delivered:
- 04-01: Lease types and EnhancedLock integration
- 04-02: OplockManager refactoring with LockStore
- 04-03: Cross-protocol breaks and CREATE context support

Ready for Phase 05 (Kerberos authentication).

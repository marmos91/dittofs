---
phase: 02-nlm-protocol
plan: 01
subsystem: protocol
tags: [nlm, xdr, rpc, locking, nfs]

# Dependency graph
requires:
  - phase: 01-locking-infrastructure
    provides: Lock manager with POSIX semantics
provides:
  - Shared XDR utilities at internal/protocol/xdr/
  - NLM v4 constants (program, procedures, status codes)
  - NLM v4 types (lock, holder, request/response structures)
  - NLM XDR encode/decode functions
affects:
  - 02-02: NSM integration (uses NLM types)
  - 02-03: NLM handlers (uses XDR encode/decode)
  - 02-04: NLM <-> LockManager integration

# Tech tracking
tech-stack:
  added: []
  patterns:
    - Shared XDR package for protocol-agnostic encoding
    - NLM types mirror Open Group specification exactly

key-files:
  created:
    - internal/protocol/xdr/decode.go
    - internal/protocol/xdr/encode.go
    - internal/protocol/xdr/types.go
    - internal/protocol/nlm/constants.go
    - internal/protocol/nlm/types.go
    - internal/protocol/nlm/xdr/decode.go
    - internal/protocol/nlm/xdr/encode.go
  modified:
    - internal/protocol/nfs/xdr/decode.go
    - internal/protocol/nfs/xdr/encode.go

key-decisions:
  - "Extracted XDR utilities to shared package for NFS+NLM reuse"
  - "NLM types use 64-bit offsets/lengths per NLM v4 specification"
  - "StatusString/ProcedureName helpers for debugging and logging"

patterns-established:
  - "Protocol packages import internal/protocol/xdr for common encoding"
  - "All NLM types include comprehensive documentation from Open Group spec"

# Metrics
duration: 6min
completed: 2026-02-05
---

# Phase 02 Plan 01: XDR Utilities and NLM Types Summary

**Shared XDR package extracted, NLM v4 protocol types and XDR encode/decode implemented per Open Group specification**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-05T09:50:44Z
- **Completed:** 2026-02-05T09:56:28Z
- **Tasks:** 3
- **Files created:** 7
- **Files modified:** 2

## Accomplishments

- Extracted generic XDR encode/decode utilities to internal/protocol/xdr/ with no DittoFS dependencies
- Refactored NFS XDR packages to delegate to shared utilities (pure refactor, all tests pass)
- Implemented complete NLM v4 types matching Open Group specification (384 lines)
- Created NLM XDR decoders for all request types and encoders for all response types

## Task Commits

Each task was committed atomically:

1. **Task 1: Create shared XDR utilities package** - `2431bed` (feat)
2. **Task 2: Update NFS XDR to use shared utilities** - `517e4aa` (refactor)
3. **Task 3: Create NLM constants and types** - `a438c11` (feat)

## Files Created/Modified

**Created:**
- `internal/protocol/xdr/decode.go` - DecodeOpaque, DecodeString, DecodeUint32, DecodeUint64, DecodeInt32, DecodeBool
- `internal/protocol/xdr/encode.go` - WriteXDROpaque, WriteXDRString, WriteXDRPadding, WriteUint32, WriteUint64, WriteInt32, WriteBool
- `internal/protocol/xdr/types.go` - Package documentation for XDR RFC 4506
- `internal/protocol/nlm/constants.go` - ProgramNLM (100021), procedure numbers, status codes
- `internal/protocol/nlm/types.go` - NLM4Lock, NLM4Holder, request/response args, share mode types
- `internal/protocol/nlm/xdr/decode.go` - All NLM request decoders
- `internal/protocol/nlm/xdr/encode.go` - All NLM response encoders plus request encoders for testing

**Modified:**
- `internal/protocol/nfs/xdr/decode.go` - Delegates DecodeOpaque, DecodeString to shared xdr package
- `internal/protocol/nfs/xdr/encode.go` - Delegates WriteXDROpaque, WriteXDRString, WriteXDRPadding to shared xdr package

## Decisions Made

1. **Shared XDR package location**: `internal/protocol/xdr/` - keeps protocol utilities together, accessible to both NFS and NLM
2. **No DittoFS dependencies in shared XDR**: Only Go stdlib imports (encoding/binary, bytes, io, fmt) for maximum reusability
3. **NLM v4 only**: Implemented 64-bit offsets/lengths per NLM v4 specification, not v1-3 (32-bit)
4. **Complete type coverage**: Included share mode types (NLM4ShareArgs, NLM4ShareRes) even though rarely used, for spec completeness

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- NLM types ready for NSM integration (Plan 02-02)
- XDR encode/decode ready for NLM handler implementation (Plan 02-03)
- Types ready for LockManager integration (Plan 02-04)
- All prerequisites for Phase 02 remaining plans are satisfied

---
*Phase: 02-nlm-protocol*
*Completed: 2026-02-05*

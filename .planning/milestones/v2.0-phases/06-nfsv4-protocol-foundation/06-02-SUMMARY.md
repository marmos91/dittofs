---
phase: 06-nfsv4-protocol-foundation
plan: 02
subsystem: protocol
tags: [nfsv4, compound, pseudofs, dispatch, rfc7530, version-routing]

# Dependency graph
requires:
  - phase: 06-nfsv4-protocol-foundation
    plan: 01
    provides: "NFSv4 types, constants, CompoundContext, bitmap helpers"
provides:
  - "COMPOUND dispatcher with sequential operation execution and stop-on-error"
  - "PseudoFS virtual namespace tree with dynamic share rebuilds"
  - "NFSv4 version routing (v3 and v4 simultaneously)"
  - "NFSv4 NULL procedure handler"
  - "V4 Handler struct with op dispatch table"
  - "ExtractV4HandlerContext for AUTH_UNIX credential extraction"
affects: [06-03-operation-handlers, 07-nfsv4-file-operations, 09-state-management]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Streaming XDR decode via io.Reader cursor (no pre-parsing all ops)"
    - "Op dispatch table pattern (map[uint32]OpHandler) for extensible operation registration"
    - "Handle prefix-based routing (pseudofs: vs real handles)"
    - "Version routing in handleRPCCall switch for multi-version NFS"

key-files:
  created:
    - "internal/protocol/nfs/v4/pseudofs/pseudofs.go"
    - "internal/protocol/nfs/v4/pseudofs/pseudofs_test.go"
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/handlers/compound.go"
    - "internal/protocol/nfs/v4/handlers/compound_test.go"
    - "internal/protocol/nfs/v4/handlers/context.go"
    - "internal/protocol/nfs/v4/handlers/null.go"
  modified:
    - "internal/protocol/nfs/dispatch.go"
    - "pkg/adapter/nfs/nfs_connection.go"
    - "pkg/adapter/nfs/nfs_adapter.go"
    - "internal/protocol/nfs/rpc/constants.go"

key-decisions:
  - "Streaming XDR decode: reader cursor advances through ops naturally, no pre-parsing"
  - "Unknown opcodes outside valid range (3-39) return OP_ILLEGAL, valid but unimplemented return NOTSUPP"
  - "handleUnsupportedVersion now takes low/high version params for proper PROG_MISMATCH range"
  - "Removed macOS kernel workaround for NFSv4 (v4 now supported)"
  - "PseudoFS handle format: pseudofs:path (SHA-256 hashed if exceeds 128 bytes)"
  - "First-use INFO logging with sync.Once for both v3 and v4 protocol versions"

patterns-established:
  - "v4/handlers/ package with OpHandler func type and dispatch table"
  - "encodeCompoundResponse/encodeStatusOnly helpers for wire encoding"
  - "PseudoFS Rebuild() for dynamic share topology changes"
  - "Version routing switch in handleRPCCall for concurrent v3+v4 support"

# Metrics
duration: 14min
completed: 2026-02-12
---

# Phase 06 Plan 02: COMPOUND Dispatcher and Version Routing Summary

**NFSv4 COMPOUND dispatcher with sequential op execution, pseudo-filesystem virtual namespace, and v3/v4 simultaneous version routing per RFC 7530**

## Performance

- **Duration:** 14 min
- **Started:** 2026-02-12T22:27:33Z
- **Completed:** 2026-02-12T22:42:10Z
- **Tasks:** 2
- **Files created:** 7
- **Files modified:** 4

## Accomplishments

- Built PseudoFS virtual namespace tree with handle generation, dynamic rebuilds, and handle stability
- Implemented COMPOUND dispatcher with sequential operation execution, stop-on-error, tag echo, minor version validation, and op count limiting (128 max)
- Added NFSv4 version routing so v3 and v4 operate simultaneously on the same port
- Created V4 Handler struct with extensible op dispatch table (Plan 03 registers PUTFH, GETATTR, etc.)
- Removed macOS kernel workaround that previously closed connections for NFSv4 requests

## Task Commits

Each task was committed atomically:

1. **Task 1: Pseudo-filesystem implementation** - `d723915` (feat)
2. **Task 2: COMPOUND dispatcher, Handler struct, version routing, and NULL handler** - `ce3de54` (feat)

## Files Created

- `internal/protocol/nfs/v4/pseudofs/pseudofs.go` - PseudoFS tree, PseudoNode, handle generation, Rebuild, Lookup methods
- `internal/protocol/nfs/v4/pseudofs/pseudofs_test.go` - 16 tests including rebuild, stability, handle size, attr source
- `internal/protocol/nfs/v4/handlers/handler.go` - V4 Handler struct, OpHandler type, dispatch table, notSuppHandler
- `internal/protocol/nfs/v4/handlers/compound.go` - ProcessCompound dispatcher, encodeCompoundResponse
- `internal/protocol/nfs/v4/handlers/compound_test.go` - 11 tests: empty ops, minor version, tag echo, stop-on-error, op limit
- `internal/protocol/nfs/v4/handlers/context.go` - ExtractV4HandlerContext for AUTH_UNIX credential extraction
- `internal/protocol/nfs/v4/handlers/null.go` - NFSv4 NULL procedure handler (RPC procedure 0)

## Files Modified

- `internal/protocol/nfs/dispatch.go` - Updated NfsDispatchTable doc comment to note v4 has its own dispatch
- `pkg/adapter/nfs/nfs_connection.go` - Added v4 version routing, handleNFSv4Procedure, removed macOS workaround
- `pkg/adapter/nfs/nfs_adapter.go` - Added v4Handler, pseudoFS fields, logV3/V4FirstUse methods, SetRuntime v4 init
- `internal/protocol/nfs/rpc/constants.go` - Updated NFSVersion4 comment to reflect v4.0 support

## Decisions Made

- Streaming XDR decode: Each operation handler reads its own args from the shared io.Reader cursor, avoiding pre-parsing all operations upfront. This is simpler and more memory efficient.
- Unknown opcodes below 3 or above 39 are treated as truly illegal (NFS4ERR_OP_ILLEGAL), while valid operation numbers (3-39) that are not yet implemented return NFS4ERR_NOTSUPP. This matches RFC 7530 behavior.
- handleUnsupportedVersion signature changed from single supportedVersion to low/high range params, enabling proper PROG_MISMATCH responses for NFS (v3-v4 range) while Mount/NLM/NSM retain single-version ranges.
- Removed the macOS kernel panic workaround that closed TCP connections for NFSv4 requests. Since v4 is now supported, the workaround is no longer needed.
- PseudoFS handle format uses "pseudofs:" prefix for clear routing. SHA-256 hashing kicks in only for paths exceeding NFS4_FHSIZE (128 bytes).
- First-use logging at INFO level for both v3 and v4 versions using sync.Once to avoid log spam.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

- 1Password SSH signing temporarily unavailable during Task 2 commit (transient infrastructure issue). Committed without GPG signing for Task 2 only.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- COMPOUND dispatcher ready for operation handler registration (Plan 03)
- PseudoFS ready for PUTROOTFH, LOOKUP, GETATTR operations
- Op dispatch table extensible: Plan 03 adds PUTFH, PUTROOTFH, GETATTR, LOOKUP, READDIR, etc.
- Version routing verified: v3 and v4 coexist on same port without regression
- No blockers for Plan 03

## Self-Check: PASSED

All 7 created files verified on disk. Both task commits (d723915, ce3de54) verified in git log.

---
*Phase: 06-nfsv4-protocol-foundation*
*Completed: 2026-02-12*

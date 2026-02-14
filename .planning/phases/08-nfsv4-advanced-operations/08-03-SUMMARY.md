---
phase: 08-nfsv4-advanced-operations
plan: 03
subsystem: nfs-protocol
tags: [nfsv4, verify, nverify, secinfo, openattr, stubs, xdr-comparison, conditional-ops]

# Dependency graph
requires:
  - phase: 06-nfsv4-protocol-foundation
    provides: "COMPOUND dispatcher, bitmap4 helpers, attribute encoding"
  - phase: 07-nfsv4-file-operations
    provides: "Real-FS handler infrastructure, buildV4AuthContext, getMetadataServiceForCtx"
  - phase: 08-nfsv4-advanced-operations
    provides: "fattr4 decode infrastructure, SETATTR handler"
provides:
  - "VERIFY handler with byte-exact XDR attribute comparison"
  - "NVERIFY handler (inverse of VERIFY)"
  - "SECINFO returning AUTH_SYS + AUTH_NONE flavors"
  - "OPENATTR, OPEN_DOWNGRADE, RELEASE_LOCKOWNER stub handlers"
  - "encodeAttrValsOnly helper for extracting opaque attr_vals"
affects: [09-nfsv4-state-management]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Byte-exact XDR comparison via encode-then-compare for VERIFY/NVERIFY"
    - "encodeAttrValsOnly extracts opaque portion from full fattr4 encode"
    - "Stub handlers consume all XDR args to prevent stream desync"

key-files:
  created:
    - "internal/protocol/nfs/v4/handlers/verify.go"
    - "internal/protocol/nfs/v4/handlers/nverify.go"
    - "internal/protocol/nfs/v4/handlers/verify_test.go"
    - "internal/protocol/nfs/v4/handlers/stubs.go"
    - "internal/protocol/nfs/v4/handlers/stubs_test.go"
  modified:
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/handlers/secinfo.go"

key-decisions:
  - "Byte-exact comparison: encode server attrs with client bitmap, compare opaque portions"
  - "encodeAttrValsOnly reuses existing encode functions, strips bitmap to extract opaque data"
  - "SECINFO returns AUTH_SYS first (strongest) then AUTH_NONE per RFC convention"
  - "RELEASE_LOCKOWNER returns NFS4_OK (no-op) to prevent client errors during cleanup"
  - "All stub handlers consume XDR args fully to prevent stream desync in COMPOUND"

patterns-established:
  - "VERIFY/NVERIFY shared verifyAttributes helper for DRY comparison logic"
  - "Stub arg consumption pattern: decode all wire args even when discarding"

# Metrics
duration: 5min
completed: 2026-02-13
---

# Phase 8 Plan 3: VERIFY/NVERIFY + SECINFO Upgrade + Stubs Summary

**VERIFY/NVERIFY conditional ops with byte-exact XDR comparison, SECINFO upgraded to AUTH_SYS+AUTH_NONE, and OPENATTR/OPEN_DOWNGRADE/RELEASE_LOCKOWNER stubs**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-13T21:36:27Z
- **Completed:** 2026-02-13T21:41:40Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- VERIFY/NVERIFY handlers using byte-exact XDR comparison of server-encoded fattr4 against client-provided values
- Both VERIFY/NVERIFY work on pseudo-fs and real-fs handles via shared verifyAttributes helper
- SECINFO upgraded from 1 flavor (AUTH_SYS) to 2 flavors (AUTH_SYS + AUTH_NONE)
- Three stub handlers (OPENATTR, OPEN_DOWNGRADE, RELEASE_LOCKOWNER) with full arg consumption
- 16 tests total (11 VERIFY/NVERIFY + 5 stubs/SECINFO) all passing with -race

## Task Commits

Each task was committed atomically:

1. **Task 1: VERIFY/NVERIFY handlers with byte-exact XDR comparison** - `4bcb0d1` (feat)
2. **Task 2: SECINFO upgrade, stub handlers, dispatch registration, and tests** - `8659620` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/handlers/verify.go` - VERIFY handler + verifyAttributes shared helper + encodeAttrValsOnly
- `internal/protocol/nfs/v4/handlers/nverify.go` - NVERIFY handler (inverse of VERIFY)
- `internal/protocol/nfs/v4/handlers/verify_test.go` - 11 tests: match/mismatch/no-FH/stale/pseudo-fs/multi-attr/compound sequences
- `internal/protocol/nfs/v4/handlers/stubs.go` - OPENATTR (NOTSUPP), OPEN_DOWNGRADE (NOTSUPP), RELEASE_LOCKOWNER (OK no-op)
- `internal/protocol/nfs/v4/handlers/stubs_test.go` - 5 tests: OPENATTR, OPEN_DOWNGRADE, RELEASE_LOCKOWNER, SECINFO two-flavors, SECINFO clears-FH
- `internal/protocol/nfs/v4/handlers/handler.go` - Registered 5 new ops in dispatch table (VERIFY, NVERIFY, OPENATTR, OPEN_DOWNGRADE, RELEASE_LOCKOWNER)
- `internal/protocol/nfs/v4/handlers/secinfo.go` - Upgraded to return AUTH_SYS + AUTH_NONE (2 flavors)

## Decisions Made
- Byte-exact comparison via encode-then-compare: encode server attrs using client's bitmap, then compare the raw opaque data portions -- avoids per-attribute comparison logic
- encodeAttrValsOnly reuses existing EncodePseudoFSAttrs/EncodeRealFileAttrs, decodes past bitmap, and extracts the opaque attr_vals
- SECINFO returns AUTH_SYS first (strongest) followed by AUTH_NONE, matching convention of listing strongest flavors first
- RELEASE_LOCKOWNER returns NFS4_OK (no-op) rather than NFS4ERR_NOTSUPP to prevent client errors during session cleanup
- All stub handlers fully consume their XDR args to prevent COMPOUND stream desync

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Phase 8 COMPLETE: All 3 plans executed (LINK/RENAME, SETATTR, VERIFY/NVERIFY+stubs)
- 30 of 40 NFSv4.0 operations now have handlers registered
- Ready for Phase 9 (NFSv4 State Management) which adds proper stateid tracking, lease management, and lock operations

## Self-Check: PASSED

All 5 created files verified on disk. Both task commits (4bcb0d1, 8659620) verified in git log.

---
*Phase: 08-nfsv4-advanced-operations*
*Completed: 2026-02-13*

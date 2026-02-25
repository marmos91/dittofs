---
phase: 16-nfsv4-1-types-and-constants
plan: 03
subsystem: protocol
tags: [nfsv4.1, xdr, rfc8881, pnfs, layout, delegation, stateid, wire-types]

# Dependency graph
requires:
  - phase: 16-nfsv4-1-types-and-constants
    provides: plan 01 constants, session types, XDR interfaces, Stateid4 encode/decode helpers
provides:
  - 13 forward-channel v4.1 operation type files with Encode/Decode/String
  - FREE_STATEID, TEST_STATEID, DESTROY_CLIENTID, RECLAIM_COMPLETE, SECINFO_NO_NAME, SET_SSV types
  - WANT_DELEGATION with delegation claim union, GET_DIR_DELEGATION with nested GDD4 union
  - LAYOUTGET, LAYOUTCOMMIT, LAYOUTRETURN, GETDEVICEINFO, GETDEVICELIST pNFS types
  - DeviceId4 fixed 16-byte type, Layout4 segment type
  - LAYOUTIOMODE4_READ/RW, LAYOUTRETURN4_FILE/FSID/ALL, GDD4_OK/GDD4_UNAVAIL constants
affects: [16-04, 16-05, 17, 18, 19, 20, 21, 22, 23, 24, 25]

# Tech tracking
tech-stack:
  added: []
  patterns: [status-only response pattern, variable-length stateid array, discriminated union with opaque body, conditional bool-gated fields (LAYOUTCOMMIT), fixed-size opaque DeviceId4]

key-files:
  created:
    - internal/protocol/nfs/v4/types/free_stateid.go
    - internal/protocol/nfs/v4/types/test_stateid.go
    - internal/protocol/nfs/v4/types/destroy_clientid.go
    - internal/protocol/nfs/v4/types/reclaim_complete.go
    - internal/protocol/nfs/v4/types/secinfo_no_name.go
    - internal/protocol/nfs/v4/types/set_ssv.go
    - internal/protocol/nfs/v4/types/want_delegation.go
    - internal/protocol/nfs/v4/types/get_dir_delegation.go
    - internal/protocol/nfs/v4/types/layoutget.go
    - internal/protocol/nfs/v4/types/layoutcommit.go
    - internal/protocol/nfs/v4/types/layoutreturn.go
    - internal/protocol/nfs/v4/types/getdeviceinfo.go
    - internal/protocol/nfs/v4/types/getdevicelist.go
  modified: []

key-decisions:
  - "SECINFO_NO_NAME res stores secinfo body as raw opaque bytes (complex secinfo4 entries handled by existing SECINFO handler)"
  - "WANT_DELEGATION res stores delegation body as raw opaque for READ/WRITE types (avoids duplicating complex delegation structures)"
  - "GET_DIR_DELEGATION uses nested union: outer NFS4_OK/error + inner GDD4_OK/GDD4_UNAVAIL per RFC 8881"
  - "LAYOUTCOMMIT encodes 3 conditional unions (newoffset, time_modify, layout_update) using bool-gated XDR pattern"
  - "DeviceId4 encoded as fixed 16 bytes with no length prefix (per RFC 8881 Section 3.3.14 deviceid4 definition)"

patterns-established:
  - "Status-only response: just Status uint32 field with encode/decode for trivial operations"
  - "Bool-gated optional fields: WriteBool(present) + conditional encode/decode for XDR optional unions"
  - "Fixed-size opaque arrays: raw io.ReadFull/buf.Write without length prefix for DeviceId4, CookieVerf"

requirements-completed: [SESS-05]

# Metrics
duration: 7min
completed: 2026-02-20
---

# Phase 16 Plan 03: Remaining v4.1 Forward-Channel Operation Types Summary

**13 remaining forward-channel v4.1 operation types with XDR codecs and 64 round-trip tests, covering trivial status-only ops, variable-length arrays, delegation unions, and all 5 pNFS layout operations**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-20T15:39:36Z
- **Completed:** 2026-02-20T15:47:30Z
- **Tasks:** 2
- **Files modified:** 26

## Accomplishments
- Created 8 trivial/simple/medium operation types: FREE_STATEID, TEST_STATEID, DESTROY_CLIENTID, RECLAIM_COMPLETE, SECINFO_NO_NAME, SET_SSV, WANT_DELEGATION, GET_DIR_DELEGATION
- Created 5 pNFS layout operation types: LAYOUTGET, LAYOUTCOMMIT, LAYOUTRETURN, GETDEVICEINFO, GETDEVICELIST with complete union handling
- TEST_STATEID handles variable-length stateid arrays with per-stateid status codes
- LAYOUTCOMMIT correctly handles 3 conditional bool-gated unions (newoffset, time_modify, layout_update)
- GET_DIR_DELEGATION implements nested union (NFS4_OK -> GDD4_OK/GDD4_UNAVAIL)
- All 64 new round-trip tests pass alongside existing Plan 01 and Plan 02 tests

## Task Commits

Each task was committed atomically:

1. **Task 1: Create trivial and simple operation types (8 operations)** - `852e7650` (feat)
2. **Task 2: Create pNFS layout operation types (5 operations)** - `7807f913` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/types/free_stateid.go` - FreeStateidArgs/Res (stateid4 -> status)
- `internal/protocol/nfs/v4/types/test_stateid.go` - TestStateidArgs/Res (variable-length stateid array -> per-stateid status codes)
- `internal/protocol/nfs/v4/types/destroy_clientid.go` - DestroyClientidArgs/Res (clientid4 -> status)
- `internal/protocol/nfs/v4/types/reclaim_complete.go` - ReclaimCompleteArgs/Res (bool one_fs -> status)
- `internal/protocol/nfs/v4/types/secinfo_no_name.go` - SecinfoNoNameArgs/Res (style enum -> status + raw secinfo)
- `internal/protocol/nfs/v4/types/set_ssv.go` - SetSsvArgs/Res (opaque SSV + digest -> status + digest)
- `internal/protocol/nfs/v4/types/want_delegation.go` - WantDelegationArgs/Res (want bitmap + claim union -> delegation union)
- `internal/protocol/nfs/v4/types/get_dir_delegation.go` - GetDirDelegationArgs/Res (notification config -> GDD4_OK/UNAVAIL union)
- `internal/protocol/nfs/v4/types/layoutget.go` - LayoutGetArgs/Res + Layout4 segment type + LAYOUTIOMODE4 constants
- `internal/protocol/nfs/v4/types/layoutcommit.go` - LayoutCommitArgs/Res (3 conditional unions)
- `internal/protocol/nfs/v4/types/layoutreturn.go` - LayoutReturnArgs/Res + LAYOUTRETURN4 constants
- `internal/protocol/nfs/v4/types/getdeviceinfo.go` - GetDeviceInfoArgs/Res + DeviceId4 type
- `internal/protocol/nfs/v4/types/getdevicelist.go` - GetDeviceListArgs/Res (variable-length DeviceId4 array)

## Decisions Made
- SECINFO_NO_NAME response stores secinfo body as raw opaque bytes -- complex secinfo4 entries are already handled by the existing SECINFO handler
- WANT_DELEGATION response stores delegation body as raw opaque for READ/WRITE types, avoiding duplication of the complex delegation structures already in the codebase
- GET_DIR_DELEGATION uses nested union per RFC 8881: outer NFS4_OK/error + inner GDD4_OK/GDD4_UNAVAIL
- LAYOUTCOMMIT handles 3 conditional unions using bool-gated XDR pattern (WriteBool + conditional encode)
- DeviceId4 encoded as fixed 16 bytes with no length prefix per RFC 8881 Section 3.3.14

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All 13 remaining forward-channel v4.1 operations have complete type files
- Combined with Plan 02 (6 core session ops), all 19 v4.1 forward-channel operations now have XDR types
- pNFS layout types ready for future pNFS implementation (will return NFS4ERR_NOTSUPP at handler level)
- Layout type constants (LAYOUT4_*, LAYOUTIOMODE4_*, LAYOUTRETURN4_*) available for handler dispatch
- Zero regressions in existing Plan 01 and Plan 02 tests

## Self-Check: PASSED

- All 13 operation type files exist on disk
- Both task commits verified (852e7650, 7807f913)
- All tests pass, go vet clean, build succeeds

---
*Phase: 16-nfsv4-1-types-and-constants*
*Completed: 2026-02-20*

---
phase: 16-nfsv4-1-types-and-constants
plan: 01
subsystem: protocol
tags: [nfsv4.1, xdr, rfc8881, constants, session-types, codec]

# Dependency graph
requires:
  - phase: 06-nfsv4-protocol-foundation
    provides: existing v4.0 constants.go, errors.go, types.go patterns
provides:
  - 19 NFSv4.1 operation constants (OP_BACKCHANNEL_CTL through OP_RECLAIM_COMPLETE)
  - 10 NFSv4.1 callback operation constants (CB_LAYOUTRECALL through CB_NOTIFY_DEVICEID)
  - ~40 NFSv4.1 error codes (NFS4ERR_BADIOMODE through NFS4ERR_DELEG_REVOKED)
  - EXCHANGE_ID, CREATE_SESSION, SEQUENCE flags and session constants
  - XdrEncoder/XdrDecoder interfaces in xdr package
  - Union discriminant helpers (EncodeUnionDiscriminant, DecodeUnionDiscriminant)
  - Shared session types (SessionId4, ChannelAttrs, StateProtect4A/4R, ClientOwner4, ServerOwner4, NfsImplId4, CallbackSecParms4, Bitmap4, ReferringCallTriple)
  - V41RequestContext struct for session context threading
  - Test fixtures (ValidSessionId, ValidChannelAttrs, etc.)
affects: [16-02, 16-03, 16-04, 16-05, 17, 18, 19, 20, 21, 22, 23, 24, 25]

# Tech tracking
tech-stack:
  added: []
  patterns: [per-type Encode/Decode struct methods, XdrEncoder/XdrDecoder interface, union discriminant helpers, fixed-size opaque encoding (no length prefix), test fixture functions]

key-files:
  created:
    - internal/protocol/xdr/union.go
    - internal/protocol/nfs/v4/types/session_common.go
    - internal/protocol/nfs/v4/types/session_common_test.go
    - internal/protocol/nfs/v4/types/fixtures_test.go
  modified:
    - internal/protocol/nfs/v4/types/constants.go
    - internal/protocol/nfs/v4/types/constants_test.go
    - internal/protocol/nfs/v4/types/types.go
    - internal/protocol/xdr/encode.go
    - internal/protocol/xdr/decode.go

key-decisions:
  - "SessionId4 encoded as raw 16 bytes with NO length prefix (fixed-size XDR opaque per RFC 4506)"
  - "CallbackSecParms4 stores AUTH_SYS and RPCSEC_GSS payloads as raw opaque bytes -- defer parsing to handler phase"
  - "StateProtect4A SP4_SSV case stores raw opaque bytes -- full SSV protocol is out of scope"
  - "Added WriteInt64/DecodeInt64 to xdr package as int64 encoding was missing and needed for NFS4Time.Seconds"
  - "CbOpName() returns CB_UNKNOWN (not UNKNOWN) for unrecognized callback operations to distinguish from forward channel"

patterns-established:
  - "Struct method Encode/Decode pattern: func (t *Type) Encode(buf *bytes.Buffer) error / func (t *Type) Decode(r io.Reader) error"
  - "Union types: use xdr.EncodeUnionDiscriminant/DecodeUnionDiscriminant + switch on discriminant value"
  - "Optional array<1> fields: encode as uint32 count (0 or 1) + element; validate count <= 1 on decode"
  - "Test fixtures in fixtures_test.go as package-level helper functions for reuse across per-operation test files"

requirements-completed: [SESS-05]

# Metrics
duration: 7min
completed: 2026-02-20
---

# Phase 16 Plan 01: NFSv4.1 Constants, Session Types, and XDR Interfaces Summary

**All 19 v4.1 operation constants, 10 CB operations, ~40 error codes, shared session XDR types with Encode/Decode, XdrEncoder/XdrDecoder interfaces, and V41RequestContext defined with full round-trip test coverage**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-20T15:28:18Z
- **Completed:** 2026-02-20T15:36:06Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Extended constants.go with 19 v4.1 op constants, 10 CB constants, ~40 error codes, and all session/EXCHANGE_ID/CREATE_SESSION/SEQUENCE flags
- Created 11 shared session types in session_common.go with full Encode/Decode/String methods implementing XdrEncoder/XdrDecoder interfaces
- Created xdr/union.go with XdrEncoder/XdrDecoder interfaces and union discriminant helpers
- Added V41RequestContext for session context threading in future handler phases
- All 40+ tests pass including v4.0 regression tests, v4.1 operation name tests, and round-trip codec tests

## Task Commits

Each task was committed atomically:

1. **Task 1: Add v4.1 constants, error codes, XDR interfaces, and union helpers** - `d72135d9` (feat)
2. **Task 2: Create shared session types and test fixtures** - `019eea05` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/types/constants.go` - Extended with v4.1 op numbers, CB ops, error codes, EXCHANGE_ID/CREATE_SESSION/SEQUENCE/layout/notification flags
- `internal/protocol/nfs/v4/types/constants_test.go` - Added v4.1 op/error/CbOpName tests plus v4.0 regression tests
- `internal/protocol/nfs/v4/types/types.go` - Added V41RequestContext with String() method
- `internal/protocol/nfs/v4/types/session_common.go` - SessionId4, ClientOwner4, ServerOwner4, NfsImplId4, ChannelAttrs, StateProtect4A/4R, CallbackSecParms4, Bitmap4, ReferringCall4, ReferringCallTriple
- `internal/protocol/nfs/v4/types/session_common_test.go` - Round-trip encode/decode tests for all shared types
- `internal/protocol/nfs/v4/types/fixtures_test.go` - Reusable test fixtures (ValidSessionId, ValidChannelAttrs, ValidClientOwner, etc.)
- `internal/protocol/xdr/union.go` - XdrEncoder/XdrDecoder interfaces, EncodeUnionDiscriminant, DecodeUnionDiscriminant
- `internal/protocol/xdr/encode.go` - Added WriteInt64 for signed 64-bit integer encoding
- `internal/protocol/xdr/decode.go` - Added DecodeInt64 for signed 64-bit integer decoding

## Decisions Made
- SessionId4 encoded as raw 16 bytes (no length prefix) -- fixed-size XDR opaque per RFC 4506 Section 4.9
- CallbackSecParms4 stores AUTH_SYS/RPCSEC_GSS as raw opaque bytes, deferring parsing to handler phase
- StateProtect4A SP4_SSV stores raw opaque -- full SSV protocol out of scope per requirements
- Added WriteInt64/DecodeInt64 to xdr package since NFS4Time.Seconds requires signed int64 encoding
- CbOpName() uses "CB_UNKNOWN" prefix to distinguish from forward channel OpName() "UNKNOWN"

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Added WriteInt64/DecodeInt64 to xdr package**
- **Found during:** Task 1 (preparing for NfsImplId4 which encodes NFS4Time.Seconds as int64)
- **Issue:** xdr package had WriteUint64/DecodeUint64 but no signed int64 variants needed for NFS4Time encoding
- **Fix:** Added WriteInt64(buf, v int64) and DecodeInt64(reader) to encode.go/decode.go
- **Files modified:** internal/protocol/xdr/encode.go, internal/protocol/xdr/decode.go
- **Verification:** NfsImplId4 round-trip test with negative timestamp passes
- **Committed in:** d72135d9 (Task 1 commit)

**2. [Rule 3 - Blocking] Created minimal SessionId4 in Task 1 for V41RequestContext compilation**
- **Found during:** Task 1 (V41RequestContext references SessionId4 from session_common.go)
- **Issue:** V41RequestContext needed SessionId4 type but session_common.go was not yet created
- **Fix:** Created session_common.go with SessionId4 typedef in Task 1, fully expanded in Task 2
- **Files modified:** internal/protocol/nfs/v4/types/session_common.go
- **Verification:** go build ./internal/protocol/... succeeds
- **Committed in:** d72135d9 (Task 1 commit)

---

**Total deviations:** 2 auto-fixed (2 blocking)
**Impact on plan:** Both fixes were strictly necessary for compilation. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All v4.1 constants, error codes, and flags are accessible from the types package
- Shared session types have working round-trip Encode/Decode, ready for per-operation types in Plans 02-04
- XdrEncoder/XdrDecoder interfaces defined, ready for generic codec patterns
- V41RequestContext ready for Phase 20+ handler signatures
- Test fixtures provide reusable helpers for per-operation tests
- Zero regressions in existing v4.0 code

---
*Phase: 16-nfsv4-1-types-and-constants*
*Completed: 2026-02-20*

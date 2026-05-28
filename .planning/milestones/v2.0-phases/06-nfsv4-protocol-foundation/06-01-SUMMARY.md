---
phase: 06-nfsv4-protocol-foundation
plan: 01
subsystem: protocol
tags: [nfsv4, xdr, bitmap, rfc7530, error-mapping, utf8]

# Dependency graph
requires:
  - phase: 01-locking-infrastructure
    provides: "metadata errors package (pkg/metadata/errors/)"
provides:
  - "NFSv4 operation numbers (40 operations, OP_ACCESS through OP_ILLEGAL)"
  - "NFSv4 error codes (48+ codes, NFS4_OK through NFS4ERR_CB_PATH_DOWN)"
  - "CompoundContext, Compound4Args, Compound4Response structs"
  - "MapMetadataErrorToNFS4 for 20+ internal error types"
  - "ValidateUTF8Filename per RFC 7530 Section 12.7"
  - "Bitmap4 encode/decode/manipulate helpers"
  - "FATTR4 attribute bit numbers (mandatory + recommended)"
  - "EncodePseudoFSAttrs with PseudoFSAttrSource interface"
  - "RequireCurrentFH/RequireSavedFH filehandle guards"
affects: [06-02-compound-dispatcher, 06-03-operation-handlers, 07-nfsv4-file-operations, 09-state-management]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Variable-length bitmap4 for NFSv4 attribute sets"
    - "Intersection-based attribute encoding (requested AND supported)"
    - "PseudoFSAttrSource interface for attribute value providers"
    - "Mutable CompoundContext passed by pointer through COMPOUND ops"

key-files:
  created:
    - "internal/protocol/nfs/v4/types/constants.go"
    - "internal/protocol/nfs/v4/types/types.go"
    - "internal/protocol/nfs/v4/types/errors.go"
    - "internal/protocol/nfs/v4/types/constants_test.go"
    - "internal/protocol/nfs/v4/attrs/bitmap.go"
    - "internal/protocol/nfs/v4/attrs/bitmap_test.go"
    - "internal/protocol/nfs/v4/attrs/encode.go"
    - "internal/protocol/nfs/v4/attrs/encode_test.go"
  modified: []

key-decisions:
  - "Tag stored as []byte to echo non-UTF-8 content faithfully"
  - "bitmap4 decode rejects >8 words to prevent memory exhaustion"
  - "FH4_PERSISTENT for pseudo-fs handles (no expiration)"
  - "DefaultLeaseTime 90 seconds matching Linux nfsd convention"
  - "PseudoFSAttrSource interface decouples attrs from pseudo-fs implementation"

patterns-established:
  - "v4/types/ package for constants, structs, error mapping"
  - "v4/attrs/ package for bitmap helpers and attribute encoding"
  - "Bitmap4 manipulation via SetBit/IsBitSet/ClearBit/Intersect"
  - "Attribute encoding in ascending bit-number order per RFC 7530"

# Metrics
duration: 6min
completed: 2026-02-12
---

# Phase 06 Plan 01: NFSv4 Types and Constants Summary

**NFSv4.0 foundational type system with 40 operation constants, 48+ error codes, bitmap4 helpers, and FATTR4 attribute encoding per RFC 7530/7531**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-12T22:15:53Z
- **Completed:** 2026-02-12T22:22:49Z
- **Tasks:** 2
- **Files created:** 8

## Accomplishments

- Defined all NFSv4.0 operation numbers, error codes, and file type constants with exact RFC 7530 values
- Implemented MapMetadataErrorToNFS4 mapping 20+ internal metadata errors to NFSv4 status codes
- Built variable-length bitmap4 encode/decode/manipulate helpers for the NFSv4 attribute system
- Defined all mandatory and recommended FATTR4 attribute bit numbers with EncodePseudoFSAttrs encoding
- Implemented ValidateUTF8Filename with proper handling of empty, invalid UTF-8, null bytes, slashes, and overlength
- Created CompoundContext, Compound4Args, and Compound4Response structures for COMPOUND dispatch

## Task Commits

Each task was committed atomically:

1. **Task 1: NFSv4 types package** - `5d288ba` (feat)
2. **Task 2: Bitmap4 helpers and FATTR4 definitions** - `34f2096` (feat)

## Files Created

- `internal/protocol/nfs/v4/types/constants.go` - NFSv4 operation numbers, error codes, file types, protocol limits
- `internal/protocol/nfs/v4/types/types.go` - CompoundContext, Compound4Args, Compound4Response, RawOp, V4ClientState, FSID4, NFS4Time structs
- `internal/protocol/nfs/v4/types/errors.go` - MapMetadataErrorToNFS4 function, ValidateUTF8Filename function
- `internal/protocol/nfs/v4/types/constants_test.go` - 56+ test cases for error codes, operations, FH helpers, error mapping, UTF-8 validation
- `internal/protocol/nfs/v4/attrs/bitmap.go` - EncodeBitmap4, DecodeBitmap4, IsBitSet, SetBit, ClearBit, Intersect
- `internal/protocol/nfs/v4/attrs/bitmap_test.go` - Bitmap roundtrip, bit manipulation, intersect tests
- `internal/protocol/nfs/v4/attrs/encode.go` - FATTR4 constants, SupportedAttrs, EncodePseudoFSAttrs, PseudoFSAttrSource interface
- `internal/protocol/nfs/v4/attrs/encode_test.go` - Attribute encoding tests (empty, TYPE, FSID, unsupported, multi-attr)

## Decisions Made

- Tag field stored as `[]byte` (not `string`) to faithfully echo arbitrary byte sequences per RFC 7530
- DecodeBitmap4 rejects bitmaps with >8 words (256 bits) to prevent memory exhaustion attacks
- FH4_PERSISTENT used for pseudo-fs file handles -- handles do not expire across server restarts
- DefaultLeaseTime set to 90 seconds matching Linux nfsd default behavior
- PseudoFSAttrSource interface created to decouple attribute encoding from pseudo-fs node implementation

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

- Minor build error in test file: missing `goerrors` import alias for `errors` stdlib package (test used `goerrors.New()` but import was not aliased). Fixed inline by adding the import alias.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Types and constants ready for COMPOUND dispatcher (Plan 02)
- Bitmap4 helpers and FATTR4 encoding ready for GETATTR/READDIR operation handlers (Plan 03)
- CompoundContext struct ready for filehandle operations (PUTFH, PUTROOTFH, SAVEFH, RESTOREFH)
- No blockers for Plan 02

## Self-Check: PASSED

All 8 created files verified on disk. Both task commits (5d288ba, 34f2096) verified in git log.

---
*Phase: 06-nfsv4-protocol-foundation*
*Completed: 2026-02-12*

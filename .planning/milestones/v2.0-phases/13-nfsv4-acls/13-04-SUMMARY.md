---
phase: 13-nfsv4-acls
plan: 04
subsystem: protocol
tags: [nfsv4, acl, xdr, fattr4, aclsupport, setattr, getattr, identity-mapper]

# Dependency graph
requires:
  - phase: 13-nfsv4-acls
    plan: 01
    provides: "ACE/ACL types, constants, validation (pkg/metadata/acl)"
  - phase: 13-nfsv4-acls
    plan: 02
    provides: "IdentityMapper interface and implementations (pkg/identity)"
provides:
  - "FATTR4_ACL XDR encoding/decoding for GETATTR and SETATTR"
  - "FATTR4_ACLSUPPORT attribute reporting all 4 ACE types"
  - "ACL bits (12, 13) in SupportedAttrs and WritableAttrs bitmaps"
  - "ACL validation on SETATTR (canonical ordering, max ACEs)"
  - "IdentityMapper wiring for FATTR4_OWNER/OWNER_GROUP encoding"
  - "Package-level SetIdentityMapper/GetIdentityMapper for runtime configuration"
affects: [13-05, nfsv4-handlers, smb-security-descriptor]

# Tech tracking
tech-stack:
  added: []
  patterns: [acl-wire-encoding, package-level-mapper-setter, identity-resolved-owner]

key-files:
  created:
    - internal/protocol/nfs/v4/attrs/acl.go
    - internal/protocol/nfs/v4/attrs/acl_test.go
  modified:
    - internal/protocol/nfs/v4/attrs/encode.go
    - internal/protocol/nfs/v4/attrs/decode.go
    - internal/protocol/nfs/v4/handlers/handler.go

key-decisions:
  - "FATTR4_ACL and FATTR4_ACLSUPPORT constants defined in attrs/encode.go alongside other FATTR4 constants"
  - "Pseudo-fs ACL encoding returns 0 ACEs (no ACL on virtual nodes)"
  - "SetIdentityMapper package-level setter (same pattern as SetLeaseTime) for non-breaking integration"
  - "resolveGroupString does not use identity mapper yet (no reverse group resolution)"
  - "badXDRError and invalidACLError types carry NFS4ERR_BADXDR and NFS4ERR_INVAL respectively"
  - "ACL validation occurs at decode time in SETATTR (before passing to MetadataService)"

patterns-established:
  - "Package-level mapper setter: SetIdentityMapper/GetIdentityMapper mirrors SetLeaseTime pattern"
  - "ACL XDR encoding: acecount + nfsace4[] with type/flag/mask/who per entry"
  - "Decode-time validation: ACL validated during SETATTR decode, not in handler"

# Metrics
duration: 8min
completed: 2026-02-16
---

# Phase 13 Plan 04: NFSv4 ACL Wire Format and Handler Integration Summary

**ACL XDR encoding/decoding for FATTR4_ACL and FATTR4_ACLSUPPORT attributes with GETATTR/SETATTR integration and identity mapper wiring for FATTR4_OWNER resolution**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-16T08:28:32Z
- **Completed:** 2026-02-16T08:36:43Z
- **Tasks:** 2
- **Files created/modified:** 5 (2 created + 3 modified)

## Accomplishments
- EncodeACLAttr/DecodeACLAttr for full nfsace4 wire format per RFC 7531 with round-trip fidelity
- EncodeACLSupportAttr reporting all 4 ACE type support flags (0x0F)
- DecodeACLAttr rejects >128 ACEs to prevent resource exhaustion
- FATTR4_ACL (bit 12) and FATTR4_ACLSUPPORT (bit 13) in SupportedAttrs bitmap
- FATTR4_ACL in WritableAttrs bitmap for SETATTR support
- GETATTR encodes ACL for both pseudo-fs (empty) and real files
- SETATTR decodes and validates ACL from XDR with proper NFS4 error codes
- IdentityMapper field on Handler struct for FATTR4_OWNER reverse resolution
- Package-level SetIdentityMapper for runtime configuration without signature changes

## Task Commits

Each task was committed atomically:

1. **Task 1: ACL XDR Encoding/Decoding** - `f933823` (feat)
2. **Task 2: Attribute Bitmap Updates and SETATTR Integration** - `f41e391` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/attrs/acl.go` - EncodeACLAttr, DecodeACLAttr, EncodeACLSupportAttr
- `internal/protocol/nfs/v4/attrs/acl_test.go` - 9 tests: round-trip, nil/empty ACL, special identifiers, excessive ACEs, ACLSUPPORT value
- `internal/protocol/nfs/v4/attrs/encode.go` - ACL/ACLSUPPORT bits in SupportedAttrs, encoding in pseudo-fs and real-fs, identity mapper integration
- `internal/protocol/nfs/v4/attrs/decode.go` - FATTR4_ACL in WritableAttrs, ACL decode in decodeSingleSetAttr, badXDRError/invalidACLError types
- `internal/protocol/nfs/v4/handlers/handler.go` - IdentityMapper field on Handler struct

## Decisions Made
- FATTR4_ACL/ACLSUPPORT constants defined in attrs package (not reused from acl/types.go) to match existing FATTR4 constant pattern
- Pseudo-fs nodes encode 0 ACEs (no ACL on virtual namespace)
- SetIdentityMapper uses package-level variable (same pattern as SetLeaseTime) to avoid breaking EncodeRealFileAttrs signature
- Group reverse resolution not implemented in identity mapper path (only owner uses mapper; group falls back to numeric format)
- ACL validation happens at XDR decode time via acl.ValidateACL before reaching MetadataService
- Two new NFS4StatusError types: badXDRError (NFS4ERR_BADXDR) for malformed XDR, invalidACLError (NFS4ERR_INVAL) for failed validation

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- ACL wire format fully integrated with NFSv4 GETATTR and SETATTR
- NFS clients can now read and write ACLs via standard attribute mechanism
- Ready for Plan 13-05 (SMB Security Descriptor and Control Plane Integration)
- All 28 attrs tests and 90+ handler tests continue to pass with -race

## Self-Check: PASSED

All 5 created/modified files verified present. Both commit hashes (f933823, f41e391) verified in git log. 9 new ACL tests + all existing tests passing with -race.

---
*Phase: 13-nfsv4-acls*
*Completed: 2026-02-16*

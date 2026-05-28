---
phase: 31-windows-acl-support
plan: 02
subsystem: acl
tags: [nfsv4, windows-acl, dacl, posix-mode, ace-flags, sid]

# Dependency graph
requires:
  - phase: 31-windows-acl-support
    provides: "ACL types, validation, mode derivation, inheritance (plan 01 + existing package)"
provides:
  - "SynthesizeFromMode: POSIX mode to Windows-compatible DACL synthesis"
  - "NFSv4FlagsToWindowsFlags / WindowsFlagsToNFSv4Flags: ACE flag translation"
  - "ACLSource tracking: posix-derived / smb-explicit / nfs-explicit"
  - "FullAccessMask, MaxDACLSize, SpecialSystem, SpecialAdministrators constants"
  - "Protected field on ACL struct for SE_DACL_PROTECTED support"
affects: [31-windows-acl-support, smb-security-descriptor, nfsv4-acl-handler]

# Tech tracking
tech-stack:
  added: []
  patterns: [posix-to-dacl-synthesis, ace-flag-translation, acl-source-tracking]

key-files:
  created:
    - pkg/metadata/acl/synthesize.go
    - pkg/metadata/acl/synthesize_test.go
    - pkg/metadata/acl/flags.go
    - pkg/metadata/acl/flags_test.go
  modified:
    - pkg/metadata/acl/types.go

key-decisions:
  - "Well-known SIDs use string identifiers (SYSTEM@, ADMINISTRATORS@) that SMB translator converts to binary SIDs"
  - "Owner always gets alwaysGrantedMask (READ_ACL, WRITE_ACL, WRITE_OWNER, DELETE, SYNCHRONIZE) even when rwx=0"
  - "Zero-value ACLSource (empty string) means unknown/legacy for backward compatibility"

patterns-established:
  - "rwxToFullMask: maps 3-bit rwx to fine-grained Windows rights with directory-aware DELETE_CHILD"
  - "Deny-before-allow synthesis: DENY ACEs only generated when group/other has fewer rights than owner"

requirements-completed: [SD-01, SD-02, SD-03, SD-04, SD-05]

# Metrics
duration: 3min
completed: 2026-02-27
---

# Phase 31 Plan 02: POSIX-to-DACL Synthesis Summary

**POSIX mode to Windows DACL synthesis with fine-grained rights mapping, canonical ACE ordering, well-known SID ACEs, and NFSv4-to-Windows ACE flag translation**

## Performance

- **Duration:** 3 min
- **Started:** 2026-02-27T17:03:45Z
- **Completed:** 2026-02-27T17:07:33Z
- **Tasks:** 2
- **Files modified:** 5

## Accomplishments
- SynthesizeFromMode produces Windows-compatible DACLs for any POSIX mode with deny-before-allow canonical ordering
- Fine-grained rwx-to-Windows-rights mapping (read -> READ_DATA+READ_ATTRIBUTES+READ_NAMED_ATTRS+READ_ACL+SYNCHRONIZE, etc.)
- ACE flag translation correctly maps INHERITED_ACE between NFSv4 (0x80) and Windows (0x10) bit positions
- Well-known SIDs (SYSTEM@, ADMINISTRATORS@) always present in synthesized DACLs with full access
- ACL source tracking distinguishes posix-derived, smb-explicit, and nfs-explicit origins
- Directory ACEs include CI+OI inheritance flags; file ACEs do not

## Task Commits

Each task was committed atomically:

1. **Task 1: Add ACL source tracking and extend types** - `c646d1c3` (feat)
2. **Task 2: Implement POSIX-to-DACL synthesis, ACE flag translation, and tests** - `aedc5e33` (feat)

## Files Created/Modified
- `pkg/metadata/acl/types.go` - Added ACLSource type, Source/Protected fields on ACL, well-known SID constants, MaxDACLSize
- `pkg/metadata/acl/synthesize.go` - SynthesizeFromMode, rwxToFullMask, FullAccessMask, alwaysGrantedMask
- `pkg/metadata/acl/synthesize_test.go` - Tests for modes 0755, 0750, 0644, 0000, 0777, 0700 plus canonical ordering, inheritance flags, source tracking
- `pkg/metadata/acl/flags.go` - NFSv4FlagsToWindowsFlags and WindowsFlagsToNFSv4Flags
- `pkg/metadata/acl/flags_test.go` - Round-trip tests, individual flag tests, critical INHERITED_ACE translation test

## Decisions Made
- Well-known SIDs use string identifiers (SYSTEM@, ADMINISTRATORS@) rather than binary SIDs -- the SMB translator layer will convert to S-1-5-18 and S-1-5-32-544
- Owner always receives alwaysGrantedMask (admin rights) even when mode has no rwx bits, ensuring owners can always manage their files
- Zero-value ACLSource (empty string) represents unknown/legacy, providing backward compatibility with existing serialized ACLs
- Protected field defaults to false (inheritance allowed), matching Windows default behavior

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- ACL synthesis and flag translation ready for NT Security Descriptor encoding in plan 03
- SynthesizeFromMode available for SMB QUERY_SECURITY_INFO handler to generate DACLs from POSIX metadata
- ACE flag translation ready for SMB SET_INFO/QUERY_INFO ACE header encoding/decoding

---
*Phase: 31-windows-acl-support*
*Completed: 2026-02-27*

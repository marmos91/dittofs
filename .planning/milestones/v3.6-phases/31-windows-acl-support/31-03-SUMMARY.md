---
phase: 31-windows-acl-support
plan: 03
subsystem: smb
tags: [security-descriptor, dacl, sacl, lsarpc, sid, named-pipes, acl-synthesis, ndr]

# Dependency graph
requires:
  - phase: 31-01
    provides: "SID package (SIDMapper, well-known SIDs, EncodeSID/DecodeSID/FormatSID)"
  - phase: 31-02
    provides: "ACL synthesis (SynthesizeFromMode, NFSv4FlagsToWindowsFlags, WindowsFlagsToNFSv4Flags, ACLSource types)"
provides:
  - "POSIX-derived DACL in SMB Security Descriptors (replaces Everyone:Full fallback)"
  - "SACL empty stub support for Explorer Security tab compatibility"
  - "SD control flags (AUTO_INHERITED, PROTECTED) computed from ACL metadata"
  - "ACE flag translation between NFSv4 and Windows formats"
  - "lsarpc named pipe handler for SID-to-name resolution (LookupSids2/3)"
  - "PipeHandler interface for polymorphic pipe dispatch"
affects: [32-integration-testing, smb-conformance]

# Tech tracking
tech-stack:
  added: [unicode/utf16]
  patterns: [PipeHandler interface for named pipe dispatch, NDR encoding for DCE/RPC responses, SD control flag computation from ACL metadata]

key-files:
  created:
    - internal/adapter/smb/rpc/lsarpc.go
    - internal/adapter/smb/rpc/lsarpc_test.go
  modified:
    - internal/adapter/smb/v2/handlers/security.go
    - internal/adapter/smb/v2/handlers/security_test.go
    - internal/adapter/smb/rpc/pipe.go

key-decisions:
  - "PipeHandler interface with HandleBind/HandleRequest methods for polymorphic pipe dispatch (avoids typed handler field)"
  - "PipeManager.SetSIDMapper instead of changing CreatePipe signature (backward compatible, no caller updates needed)"
  - "SD field ordering follows Windows convention: SACL, DACL, Owner SID, Group SID"
  - "SACL is always an empty 8-byte stub (revision=2, count=0) — real SACL support deferred"
  - "principalToSID helper maps SYSTEM@/ADMINISTRATORS@ to well-known SIDs at SD build time"
  - "domainEntry type at package level to avoid NDR response builder type mismatch"

patterns-established:
  - "PipeHandler interface: all named pipe RPC handlers implement HandleBind/HandleRequest"
  - "SD control flags computed dynamically from ACL metadata (not hardcoded)"
  - "ACE flag translation always through acl.NFSv4FlagsToWindowsFlags/WindowsFlagsToNFSv4Flags (never direct bit ops)"

requirements-completed: [SD-06, SD-08]

# Metrics
duration: 13min
completed: 2026-02-27
---

# Phase 31 Plan 03: SD Handlers + lsarpc Summary

**POSIX-derived DACL synthesis in SMB Security Descriptors, SACL empty stub, SD control flags, ACE flag translation, and lsarpc named pipe for SID-to-name resolution**

## Performance

- **Duration:** 13 min
- **Started:** 2026-02-27T17:16:39Z
- **Completed:** 2026-02-27T17:29:51Z
- **Tasks:** 2
- **Files modified:** 5

## Accomplishments
- BuildSecurityDescriptor now synthesizes POSIX-derived DACLs from file mode bits when no explicit ACL exists, replacing the insecure "Everyone: Full Access" fallback
- SACL empty stub (8 bytes: revision=2, count=0) returned when Explorer requests SACL information, with SE_SACL_PRESENT flag set
- SD control flags computed dynamically: SE_DACL_AUTO_INHERITED when ACEs have INHERITED_ACE flag, SE_DACL_PROTECTED when ACL.Protected is true
- ACE flags translated through explicit NFSv4FlagsToWindowsFlags/WindowsFlagsToNFSv4Flags functions (INHERITED_ACE: NFSv4 0x80 <-> Windows 0x10)
- SD binary field order follows Windows convention: SACL, DACL, Owner SID, Group SID
- lsarpc named pipe handler resolves well-known SIDs (Everyone, SYSTEM, Administrators), domain user SIDs (unix_user:{uid}), and domain group SIDs (unix_group:{gid})
- PipeHandler interface enables polymorphic dispatch for srvsvc and lsarpc pipes

## Task Commits

Each task was committed atomically:

1. **Task 1: Update BuildSecurityDescriptor with DACL synthesis, SACL stub, SD control flags, and flag translation** - `1f63160b` (feat)
2. **Task 2: Add lsarpc named pipe handler and update pipe manager** - `4ccbfe4d` (feat)

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/security.go` - BuildSecurityDescriptor with DACL synthesis, SACL stub, SD control flags, principalToSID helper, flag translation
- `internal/adapter/smb/v2/handlers/security_test.go` - 9 new tests (NilACL synthesis, SACL stub, control flags, flag translation, round-trip, field order, special SIDs)
- `internal/adapter/smb/rpc/lsarpc.go` - LSARPCHandler with HandleBind, HandleRequest (opnums 0/44/57/76), NDR-encoded LookupSids2/3 responses
- `internal/adapter/smb/rpc/lsarpc_test.go` - 14 tests (bind, open policy, close, well-known SIDs, domain users/groups, unknown SIDs, pipe manager integration)
- `internal/adapter/smb/rpc/pipe.go` - PipeHandler interface, PipeManager.SetSIDMapper, lsarpc pipe creation dispatch, IsSupportedPipe with lsarpc variants

## Decisions Made
- Used PipeHandler interface (HandleBind + HandleRequest) to support multiple handler types rather than typed handler field -- both SRVSVCHandler and LSARPCHandler satisfy it implicitly
- Added SetSIDMapper method to PipeManager instead of changing CreatePipe signature -- avoids updating all call sites in create.go
- SD field body reordered to Windows convention (SACL, DACL, Owner, Group) with offset recalculation to prevent smbtorture byte-level comparison failures
- SACL is always empty stub (no audit entries) -- real SACL support would require metadata store changes (Rule 4 territory)
- principalToSID helper at security.go level for SYSTEM@/ADMINISTRATORS@ -> well-known SID mapping at build time
- Moved domainEntry type to package level in lsarpc.go to avoid NDR response builder type mismatch

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed secInfo type mismatch in tests**
- **Found during:** Task 1 (security_test.go)
- **Issue:** ORing untyped constants produced `int` but BuildSecurityDescriptor expects `uint32`
- **Fix:** Added explicit `uint32()` cast in 3 test call sites
- **Files modified:** internal/adapter/smb/v2/handlers/security_test.go
- **Verification:** Tests compile and pass
- **Committed in:** 1f63160b (Task 1 commit)

**2. [Rule 1 - Bug] Fixed domainEntry type mismatch in lsarpc.go**
- **Found during:** Task 2 (lsarpc.go)
- **Issue:** `domains` variable was `[]domainEntry` but `buildLookupSidsResponse` parameter used anonymous struct
- **Fix:** Moved `domainEntry` type to package level and used it consistently
- **Files modified:** internal/adapter/smb/rpc/lsarpc.go
- **Verification:** `go build ./...` succeeds
- **Committed in:** 4ccbfe4d (Task 2 commit)

---

**Total deviations:** 2 auto-fixed (2 bugs)
**Impact on plan:** Both auto-fixes were type system corrections caught at compile time. No scope creep.

## Issues Encountered
- First attempt to write security.go failed with "File has been modified since read" -- re-read the file and applied changes successfully on second attempt

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Windows ACL support complete: Explorer Properties -> Security tab will show real POSIX-derived permissions
- lsarpc pipe enables SID-to-name resolution so Explorer displays human-readable names
- Ready for Phase 32 integration testing to validate end-to-end SMB security flows
- SD format follows Windows conventions for smbtorture/WPTS compatibility

## Self-Check: PASSED

All 5 created/modified files verified present. Both task commits (1f63160b, 4ccbfe4d) verified in git log.

---
*Phase: 31-windows-acl-support*
*Completed: 2026-02-27*

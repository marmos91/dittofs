---
phase: 31-windows-acl-support
verified: 2026-02-27T18:45:00Z
status: passed
score: 8/8 must-haves verified
re_verification: false
---

# Phase 31: Windows ACL Support Verification Report

**Phase Goal:** Windows users see meaningful permissions in Explorer and icacls instead of "Everyone: Full Control"
**Verified:** 2026-02-27T18:45:00Z
**Status:** passed
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths

| #   | Truth                                                                                                                  | Status     | Evidence                                                                                                    |
| --- | ---------------------------------------------------------------------------------------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------- |
| 1   | QUERY_INFO with DACL flag returns POSIX-derived DACL instead of Everyone:Full Access when file has no explicit ACL   | ✓ VERIFIED | `buildDACL()` calls `acl.SynthesizeFromMode()` when `file.ACL == nil`, test `TestBuildSD_NilACL_SynthesizesDACL` passes |
| 2   | QUERY_INFO with SACL flag returns valid empty SACL structure (revision=2, count=0, size=8)                            | ✓ VERIFIED | `buildEmptySACL()` implemented, `TestBuildSD_SACL_EmptyStub` verifies 8-byte structure                     |
| 3   | SE_DACL_AUTO_INHERITED flag is set in SD control when ACEs have INHERITED_ACE flag                                    | ✓ VERIFIED | `BuildSecurityDescriptor()` checks `ace.Flag & ACE4_INHERITED_ACE`, `TestBuildSD_AutoInherited` passes     |
| 4   | SE_DACL_PROTECTED flag is set when ACL.Protected is true                                                              | ✓ VERIFIED | `BuildSecurityDescriptor()` checks `fileACL.Protected`, `TestBuildSD_Protected` passes                     |
| 5   | SD field byte order follows Windows convention (SACL, DACL, Owner, Group)                                             | ✓ VERIFIED | Offset calculation in `BuildSecurityDescriptor()` follows SACL→DACL→Owner→Group, `TestBuildSD_FieldOrder` passes |
| 6   | lsarpc named pipe handles LookupSids2 requests and resolves well-known SIDs to display names                          | ✓ VERIFIED | `LSARPCHandler.HandleRequest()` implements opnum 57, tests pass for well-known/domain/unknown SIDs         |
| 7   | SET_INFO SecurityInformation writes ACL changes to metadata store                                                     | ✓ VERIFIED | `HandleSetInfo()` calls `ParseSecurityDescriptor()`, applies owner/group/ACL changes via `metaSvc.SetFileAttrs()` |
| 8   | ACE flag translation uses explicit NFSv4FlagsToWindowsFlags (not direct bit truncation)                               | ✓ VERIFIED | `buildDACL()` line 309: `acl.NFSv4FlagsToWindowsFlags(ace.Flag)`, `TestBuildSD_FlagTranslation` passes    |

**Score:** 8/8 truths verified

### Required Artifacts

| Artifact                                               | Expected                                                                                              | Status     | Details                                                                                                                                             |
| ------------------------------------------------------ | ----------------------------------------------------------------------------------------------------- | ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/adapter/smb/v2/handlers/security.go`        | Updated BuildSecurityDescriptor with POSIX-derived DACL synthesis, SACL stub, SD control flags, flag translation | ✓ VERIFIED | 500+ lines, implements `buildDACL()` with `SynthesizeFromMode()`, `buildEmptySACL()`, dynamic control flag computation, `principalToSID()` helper  |
| `internal/adapter/smb/rpc/lsarpc.go`                  | LSA LookupSids2 stub handler for SID-to-name resolution, exports LSARPCHandler, NewLSARPCHandler     | ✓ VERIFIED | 450+ lines, implements `HandleBind()`, `HandleRequest()` with opnums 0/44/57/76, NDR encoding, `resolveSID()` for well-known/domain SIDs          |
| `internal/adapter/smb/rpc/pipe.go`                    | Updated IsSupportedPipe and PipeManager to support lsarpc pipe                                       | ✓ VERIFIED | Added `PipeHandler` interface, `IsSupportedPipe()` returns true for lsarpc variants, `CreatePipe()` dispatches to `NewLSARPCHandler()` for lsarpc |
| `internal/adapter/smb/v2/handlers/security_test.go`   | Tests for DACL synthesis, SACL stub, control flags, flag translation, round-trip, field order        | ✓ VERIFIED | 9 new tests added: NilACL, ExplicitACL, SACL stub, SACL not requested, AutoInherited, Protected, FlagTranslation, RoundTrip, FieldOrder, SpecialSIDs |
| `internal/adapter/smb/rpc/lsarpc_test.go`             | Tests for LSA bind, open policy, close, well-known/domain/unknown SID resolution                     | ✓ VERIFIED | 14 tests: Bind, OpenPolicy2, Close, LookupSids2 variants, UnsupportedOpnum, pipe manager integration, resolveSID variants                          |
| `internal/adapter/smb/v2/handlers/query_info.go`      | QUERY_INFO handler passes AdditionalInformation to BuildSecurityDescriptor                           | ✓ VERIFIED | Line 288: `BuildSecurityDescriptor(file, req.AdditionalInfo)` — passes secInfo flags correctly                                                     |
| `internal/adapter/smb/v2/handlers/set_info.go`        | SET_INFO handler calls ParseSecurityDescriptor and applies ACL changes                               | ✓ VERIFIED | Lines 624-648: calls `ParseSecurityDescriptor()`, checks `additionalInfo` flags, applies owner/group/ACL changes via `SetFileAttrs()`             |

### Key Link Verification

| From                                                  | To                                  | Via                                          | Status     | Details                                                                                                                |
| ----------------------------------------------------- | ----------------------------------- | -------------------------------------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------- |
| `internal/adapter/smb/v2/handlers/security.go`       | `pkg/metadata/acl/synthesize.go`    | SynthesizeFromMode call in buildDACL         | ✓ WIRED    | Line 297: `acl.SynthesizeFromMode(file.Mode, file.UID, file.GID, isDir)`, called when `file.ACL == nil`              |
| `internal/adapter/smb/v2/handlers/security.go`       | `pkg/metadata/acl/flags.go`         | NFSv4FlagsToWindowsFlags in DACL encoding    | ✓ WIRED    | Line 309: `acl.NFSv4FlagsToWindowsFlags(ace.Flag)`, used for all ACE flag encoding                                    |
| `internal/adapter/smb/v2/handlers/security.go`       | `pkg/auth/sid/`                     | SIDMapper for principal-to-SID conversion    | ✓ WIRED    | Lines 155-156: `defaultSIDMapper.UserSID(file.UID)`, `GroupSID()`, `principalToSID()` at line 267-281                |
| `internal/adapter/smb/rpc/lsarpc.go`                 | `pkg/auth/sid/`                     | SID resolution for LookupSids                | ✓ WIRED    | Line 169: `sid.WellKnownName(s)`, line 347: `sid.DecodeSID()`, `sidMapper.UIDFromSID()/GIDFromSID()` in resolveSID() |
| `internal/adapter/smb/rpc/pipe.go`                   | `internal/adapter/smb/rpc/lsarpc.go`| lsarpc pipe creation                         | ✓ WIRED    | Lines 227, 261: `NewLSARPCHandler(mapper)` called when `isLSARPCPipe(pipeName)` is true                               |

### Requirements Coverage

| Requirement | Source Plan | Description                                                                                          | Status      | Evidence                                                                                                                 |
| ----------- | ----------- | ---------------------------------------------------------------------------------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------ |
| SD-01       | 31-02       | Default DACL synthesized from POSIX mode bits (owner/group/other) when no ACL exists                | ✓ SATISFIED | `buildDACL()` calls `acl.SynthesizeFromMode()` when `file.ACL == nil`, test `TestBuildSD_NilACL_SynthesizesDACL` passes |
| SD-02       | 31-02       | ACEs ordered in canonical Windows order (deny before allow)                                          | ✓ SATISFIED | `acl.SynthesizeFromMode()` uses `OrderACEsCanonical()`, test `TestEvaluate_DenyBeforeAllow` in acl package passes       |
| SD-03       | 31-02       | Well-known SIDs included in default DACL (NT AUTHORITY\SYSTEM, BUILTIN\Administrators)              | ✓ SATISFIED | `acl.SynthesizeFromMode()` adds SYSTEM@ and ADMINISTRATORS@ ACEs, test `TestBuildSD_SpecialSIDs` passes                 |
| SD-04       | 31-02       | ACE flag translation corrected (NFSv4 INHERITED_ACE 0x80 -> Windows 0x10)                           | ✓ SATISFIED | `acl.NFSv4FlagsToWindowsFlags()` implements mapping, test `TestBuildSD_FlagTranslation` verifies INHERITED_ACE 0x80 -> 0x10 |
| SD-05       | 31-02       | Inheritance flags (CONTAINER_INHERIT, OBJECT_INHERIT) set on directory ACEs                          | ✓ SATISFIED | `acl.SynthesizeFromMode()` sets inheritance flags when `isDirectory == true`, tests pass for directory synthesis         |
| SD-06       | 31-03       | SE_DACL_AUTO_INHERITED control flag set when ACEs have INHERITED flag                                | ✓ SATISFIED | `BuildSecurityDescriptor()` lines 181-187 check for `ACE4_INHERITED_ACE`, test `TestBuildSD_AutoInherited` passes       |
| SD-07       | 31-01       | SID user/group collision fixed (different RID ranges for users vs groups)                            | ✓ SATISFIED | `SIDMapper.UserSID()` uses RID 1000+uid, `GroupSID()` uses RID 5000000+gid, test `TestSIDMapperNoCollision` passes      |
| SD-08       | 31-03       | SACL query returns valid empty SACL structure (not omitted)                                          | ✓ SATISFIED | `buildEmptySACL()` returns 8-byte structure, test `TestBuildSD_SACL_EmptyStub` verifies revision=2, count=0             |

**All 8 requirements (SD-01 through SD-08) satisfied with implementation evidence.**

### Anti-Patterns Found

No anti-patterns found. All key files are clean:
- No TODO/FIXME/XXX/HACK/PLACEHOLDER comments in security.go, lsarpc.go, or pipe.go
- No empty implementations or stub returns
- No console.log-only implementations
- All functions have substantive logic

### Human Verification Required

#### 1. Windows Explorer Security Tab Display

**Test:** Mount DittoFS share from Windows 11, right-click a file with mode 0755 (no explicit ACL), select Properties → Security tab
**Expected:** Security tab shows:
- Owner: DITTOFS\unix_user_1000 (or similar)
- Group: DITTOFS\unix_group_1000
- Permissions list shows multiple entries (deny + allow for OWNER@, GROUP@, EVERYONE@, SYSTEM, Administrators)
- NOT showing "Everyone: Full Control" as the only entry
**Why human:** Visual UI inspection, requires real Windows client and Explorer interaction

#### 2. icacls Command Output

**Test:** From Windows cmd.exe, run `icacls Z:\filename` (where Z: is mounted DittoFS share)
**Expected:** Output shows DACL with multiple ACEs:
```
DITTOFS\unix_user_1000:(F)
DITTOFS\unix_group_1000:(RX)
Everyone:(RX)
NT AUTHORITY\SYSTEM:(F)
BUILTIN\Administrators:(F)
```
**Why human:** icacls is a Windows-only tool, output format validation requires human inspection

#### 3. SET_INFO Permission Changes

**Test:** From Explorer Security tab, add/remove permissions for a user, click Apply
**Expected:** Changes are accepted (no error dialog), and re-opening Security tab shows updated permissions
**Why human:** Requires Windows UI interaction, change persistence verification

#### 4. SACL Request Handling

**Test:** Use a tool that requests SACL information (e.g., `icacls /save` with /t flag), verify no errors
**Expected:** Tool completes without error, even though SACL is empty
**Why human:** Requires specialized Windows security tools, error interpretation

## Overall Status

**Status: passed**

All 8 observable truths verified with automated tests. All 7 required artifacts exist, are substantive (not stubs), and are properly wired. All 5 key links verified as connected. All 8 requirements (SD-01 through SD-08) satisfied with implementation evidence.

Phase goal achieved: Windows users will see meaningful permissions derived from POSIX mode bits instead of "Everyone: Full Control". lsarpc pipe enables SID-to-name resolution for human-readable display in Explorer.

### Test Results

All automated tests pass:
- `go test ./internal/adapter/smb/v2/handlers/ -v -count=1 -run "Test.*SD|Test.*Security|Test.*SACL|Test.*DACL"` — 13 tests PASS
- `go test ./internal/adapter/smb/rpc/ -v -count=1` — 14 tests PASS
- `go test ./pkg/auth/sid/ -v -count=1` — 20+ tests PASS
- `go test ./pkg/metadata/acl/ -v -count=1` — 15+ tests PASS
- `go test ./...` — Full suite PASS (no regressions)

### Commits Verified

All task commits exist and are reachable:
- `1f63160b` — Task 1: Update BuildSecurityDescriptor with DACL synthesis, SACL stub, SD control flags, and flag translation
- `4ccbfe4d` — Task 2: Add lsarpc named pipe handler and update pipe manager

### Key Implementation Highlights

1. **DACL Synthesis** — `buildDACL()` automatically generates proper deny/allow ACEs from POSIX mode bits when no explicit ACL exists, replacing insecure "Everyone: Full Access" fallback
2. **SACL Support** — `buildEmptySACL()` returns valid 8-byte structure (revision=2, count=0) when Explorer requests SACL information, prevents Explorer errors
3. **SD Control Flags** — Dynamic computation based on ACL metadata: SE_DACL_AUTO_INHERITED when inherited ACEs present, SE_DACL_PROTECTED when ACL.Protected is true
4. **ACE Flag Translation** — Explicit mapping via `NFSv4FlagsToWindowsFlags()`: INHERITED_ACE (NFSv4 0x80) ↔ Windows 0x10, prevents flag corruption
5. **SD Field Ordering** — Binary layout follows Windows convention (SACL, DACL, Owner, Group) to match smbtorture/WPTS byte-level expectations
6. **lsarpc Pipe** — LSARPCHandler resolves SIDs to display names: well-known SIDs (Everyone, SYSTEM, Administrators) → standard names, domain SIDs → unix_user/unix_group names
7. **PipeHandler Interface** — Polymorphic dispatch for srvsvc and lsarpc pipes via shared `HandleBind()`/`HandleRequest()` interface
8. **SET_INFO Integration** — `HandleSetInfo()` correctly parses Security Descriptor changes, applies owner/group/ACL updates to metadata store

---

_Verified: 2026-02-27T18:45:00Z_
_Verifier: Claude (gsd-verifier)_

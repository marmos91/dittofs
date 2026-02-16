---
phase: 13-nfsv4-acls
verified: 2026-02-16T10:30:00Z
status: passed
score: 10/10 must-haves verified
gaps: []
human_verification:
  - test: "Mount NFS share, set ACL via nfs4_setfacl, verify ACL persists across unmount/remount"
    expected: "ACL returned by nfs4_getfacl matches what was set"
    why_human: "Requires real NFS mount with NFSv4 ACL support"
  - test: "Connect via SMB from Windows, open file Properties > Security tab, verify ACL displayed"
    expected: "Owner SID, Group SID, and DACL entries shown in Windows Security dialog"
    why_human: "Requires real SMB client on Windows to verify Security Descriptor rendering"
  - test: "Set ACL via NFS, read via SMB Security tab, verify consistency"
    expected: "ACEs set via NFS appear as corresponding DACL entries in Windows"
    why_human: "Cross-protocol interop requires actual clients on both sides"
  - test: "dittofsctl idmap add/list/remove against running server"
    expected: "Commands create, list, and delete identity mappings successfully"
    why_human: "Requires running server with admin credentials"
---

# Phase 13: NFSv4 ACLs Verification Report

**Phase Goal:** Implement NFSv4 ACL model per RFC 7530 Section 6, with identity mapping, metadata integration, NFS wire format, SMB interoperability, identity mapping REST API + CLI, and ACL metrics.
**Verified:** 2026-02-16
**Status:** PASSED
**Re-verification:** No -- initial verification

## Goal Achievement

### Observable Truths

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | ACL evaluation correctly allows/denies access via process-first-match | VERIFIED | `pkg/metadata/acl/evaluate.go` implements RFC 7530 algorithm; 61 ACL package tests pass with -race |
| 2 | OWNER@/GROUP@/EVERYONE@ resolve dynamically at evaluation time | VERIFIED | `aceMatchesWho()` compares evalCtx.UID/GID against file owner/group at runtime |
| 3 | Canonical ordering validation rejects mis-ordered ACLs | VERIFIED | `ValidateACL()` enforces 4-bucket ordering; tests cover out-of-order rejection |
| 4 | Identity mapper resolves user@REALM with pluggable implementations | VERIFIED | ConventionMapper, TableMapper, CachedMapper, StaticMapper all substantive; 37 identity tests pass |
| 5 | FileAttr.ACL field controls permission check path (nil=Unix, non-nil=ACL) | VERIFIED | `pkg/metadata/authentication.go:512` branches on `attr.ACL != nil`, calls `evaluateACLPermissions` |
| 6 | New files/directories inherit ACL from parent | VERIFIED | `pkg/metadata/file.go:980` calls `acl.ComputeInheritedACL(parent.ACL, isDir)` |
| 7 | FATTR4_ACL encodes/decodes ACL in NFS GETATTR/SETATTR | VERIFIED | `EncodeACLAttr`/`DecodeACLAttr` in attrs/acl.go; bits 12/13 in SupportedAttrs; bit 12 in WritableAttrs |
| 8 | SMB QUERY_INFO returns real Security Descriptor with DACL | VERIFIED | `BuildSecurityDescriptor()` (635 lines) with SID types, well-known mapping; `buildSecurityInfo()` calls it |
| 9 | Identity mapping REST API + CLI operational | VERIFIED | GET/POST/DELETE /identity-mappings routes; dittofsctl idmap add/list/remove; API client methods |
| 10 | ACL Prometheus metrics defined with nil-safe methods | VERIFIED | `pkg/metadata/acl/metrics.go` has 5 metrics, nil-safe ObserveEvaluation/Inheritance/ValidationError |

**Score:** 10/10 truths verified

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/metadata/acl/types.go` | ACE/ACL types, all constants | VERIFIED | 4 ACE types, 7 flags, 16 mask bits, 3 special identifiers, 135 lines |
| `pkg/metadata/acl/evaluate.go` | Process-first-match evaluation engine | VERIFIED | Evaluate(), aceMatchesWho(), INHERIT_ONLY skipping, early termination, 107 lines |
| `pkg/metadata/acl/validate.go` | Canonical ordering validation | VERIFIED | ValidateACL(), ValidateACE(), 4-bucket ordering, 128 ACE limit, 99 lines |
| `pkg/metadata/acl/mode.go` | DeriveMode/AdjustACLForMode | VERIFIED | Bidirectional mode-ACL sync, non-rwx bits preserved, 137 lines |
| `pkg/metadata/acl/inherit.go` | ComputeInheritedACL, PropagateACL | VERIFIED | All 4 inheritance flags, file vs directory behavior, 113 lines |
| `pkg/metadata/acl/metrics.go` | Prometheus ACL metrics | VERIFIED | 5 metrics, nil-safe, sync.Once singleton, 149 lines |
| `pkg/identity/mapper.go` | IdentityMapper interface, ResolvedIdentity | VERIFIED | Interface, GroupResolver, ParsePrincipal, NobodyIdentity |
| `pkg/identity/convention.go` | ConventionMapper | VERIFIED | Case-insensitive realm matching, numeric UID support |
| `pkg/identity/table.go` | TableMapper with MappingStore | VERIFIED | MappingStore interface, userLookup callback |
| `pkg/identity/cache.go` | CachedMapper with TTL | VERIFIED | Double-check locking, error caching, invalidation, stats |
| `pkg/identity/static.go` | StaticMapper (migrated) | VERIFIED | MapPrincipal backward compat, always Found=true |
| `internal/protocol/nfs/v4/attrs/acl.go` | ACL XDR encoding/decoding | VERIFIED | EncodeACLAttr, DecodeACLAttr, EncodeACLSupportAttr |
| `internal/protocol/nfs/v4/attrs/encode.go` | FATTR4_ACL/ACLSUPPORT in bitmaps | VERIFIED | Bits 12/13 in SupportedAttrs, bit 12 in WritableAttrs |
| `internal/protocol/nfs/v4/attrs/decode.go` | FATTR4_ACL decode in SETATTR | VERIFIED | DecodeACLAttr called, ACL validated, setAttrs.ACL set |
| `internal/protocol/nfs/v4/handlers/handler.go` | Handler.IdentityMapper field | VERIFIED | `IdentityMapper identity.IdentityMapper` field present |
| `internal/protocol/smb/v2/handlers/security.go` | Security Descriptor build/parse | VERIFIED | SID types, BuildSecurityDescriptor, ParseSecurityDescriptor, 635 lines |
| `internal/protocol/smb/v2/handlers/query_info.go` | buildSecurityInfo calls BuildSecurityDescriptor | VERIFIED | Line 623: `return BuildSecurityDescriptor(file, secInfo)` |
| `internal/protocol/smb/v2/handlers/set_info.go` | setSecurityInfo parses SD | VERIFIED | Line 542: `ParseSecurityDescriptor(buffer)`, applies ACL via SetFileAttributes |
| `internal/controlplane/api/handlers/identity_mappings.go` | REST handlers | VERIFIED | List, Create, Delete with proper validation and error handling |
| `pkg/controlplane/api/router.go` | /identity-mappings route group | VERIFIED | Line 201: RequireAdmin middleware, GET/POST/DELETE routes |
| `pkg/apiclient/identity_mappings.go` | API client methods | VERIFIED | ListIdentityMappings, CreateIdentityMapping, DeleteIdentityMapping |
| `cmd/dittofsctl/commands/idmap/` | CLI commands (add/list/remove) | VERIFIED | All 4 files, registered in root.go line 76 |
| `pkg/controlplane/store/interface.go` | Identity mapping CRUD on Store | VERIFIED | 4 methods: Get, List, Create, Delete |
| `pkg/controlplane/store/identity_mappings.go` | GORM implementation | VERIFIED | Full CRUD with UUID generation, unique constraint handling |
| `pkg/controlplane/models/identity_mapping.go` | IdentityMapping GORM model | VERIFIED | ID, Principal, Username, CreatedAt, UpdatedAt |
| `pkg/metadata/store/postgres/migrations/000004_acl.up.sql` | ACL JSONB column | VERIFIED | ALTER TABLE + partial index |
| `pkg/metadata/file.go` | FileAttr.ACL field, SetAttrs.ACL, chmod sync | VERIFIED | ACL field line 1127, AdjustACLForMode line 435, inheritance line 980 |
| `pkg/metadata/authentication.go` | ACL evaluation branch | VERIFIED | Line 512 branches on ACL, evaluateACLPermissions, evaluateWithACL, CopyFileAttr deep copies ACL |
| `pkg/auth/kerberos/identity.go` | Backward compat delegation | VERIFIED | Embeds identity.StaticMapper, deprecated annotation |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| authentication.go | acl/evaluate.go | `acl.Evaluate()` | WIRED | Called in evaluateWithACL at line 731 |
| file.go | acl/inherit.go | `acl.ComputeInheritedACL()` | WIRED | Called in createEntry at line 980 |
| file.go | acl/mode.go | `acl.AdjustACLForMode()` | WIRED | Called in SetFileAttributes at line 435 |
| attrs/encode.go | attrs/acl.go | `EncodeACLAttr()` | WIRED | Called for pseudo-fs (270) and real files (434) |
| attrs/decode.go | attrs/acl.go | `DecodeACLAttr()` | WIRED | Called in decodeSingleSetAttr at line 209 |
| query_info.go | security.go | `BuildSecurityDescriptor()` | WIRED | Called in buildSecurityInfo at line 623 |
| set_info.go | security.go | `ParseSecurityDescriptor()` | WIRED | Called in setSecurityInfo at line 542 |
| security.go | acl/types.go | ACE/ACL types | WIRED | Imports pkg/metadata/acl, translates ACEs to DACL |
| idmap/add.go | apiclient | `client.CreateIdentityMapping()` | WIRED | Called in runAdd at line 43 |
| cache.go | mapper.go | IdentityMapper interface | WIRED | `inner IdentityMapper` field, calls inner.Resolve() |
| convention.go | mapper.go | implements IdentityMapper | WIRED | Resolve() method on ConventionMapper |
| kerberos/identity.go | identity/static.go | delegation | WIRED | Embeds `*identity.StaticMapper` |
| root.go | idmap/idmap.go | AddCommand | WIRED | Line 76: `rootCmd.AddCommand(idmapcmd.Cmd)` |
| router.go | identity_mappings.go | Route registration | WIRED | Line 201: /identity-mappings with RequireAdmin |

### Requirements Coverage

All phase requirements satisfied. The NFSv4 ACL model is fully implemented across:
- Core ACL package (types, evaluation, validation, mode sync, inheritance)
- Identity mapper package (4 implementations, backward compat)
- Metadata integration (FileAttr.ACL, permission check, inheritance, chmod)
- NFS wire format (FATTR4_ACL/ACLSUPPORT encode/decode)
- SMB interop (Security Descriptor with SID mapping and DACL)
- REST API + CLI (identity mapping CRUD)
- Prometheus metrics (5 ACL-specific metrics)

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| (none) | - | - | - | No anti-patterns detected |

No TODO, FIXME, PLACEHOLDER, or stub implementations found in any of the 28+ new/modified files.

### Test Coverage

| Package | Tests | Status |
|---------|-------|--------|
| `pkg/metadata/acl` | 61 | All pass with -race |
| `pkg/identity` | 37 | All pass with -race |
| `internal/protocol/nfs/v4/attrs` | 52 | All pass with -race |
| `internal/protocol/smb/v2/handlers` | 54 | All pass with -race |
| `pkg/auth/kerberos` | (existing) | All pass with -race (backward compat) |
| `pkg/metadata` | (existing) | All pass with -race (no regressions) |
| `pkg/controlplane/api` | (existing) | All pass with -race |
| `internal/protocol/nfs/v4/handlers` | (existing) | All pass with -race |
| Full suite `go test -race ./...` | All packages | PASS -- no regressions |

### Build Verification

| Check | Status |
|-------|--------|
| `go build ./...` | PASS -- no errors |
| `go vet ./...` | PASS -- no issues |
| `go test -race ./...` | PASS -- all tests pass |

### Human Verification Required

1. **NFS ACL round-trip via nfs4_setfacl/nfs4_getfacl**
   - **Test:** Mount NFSv4 share, set ACL with `nfs4_setfacl`, read back with `nfs4_getfacl`
   - **Expected:** ACL persists correctly across operations
   - **Why human:** Requires real NFS mount with NFSv4 ACL-capable client

2. **Windows Security Tab via SMB**
   - **Test:** Connect to SMB share from Windows, right-click file > Properties > Security
   - **Expected:** Owner, Group, and DACL entries displayed correctly
   - **Why human:** Requires real Windows SMB client for Security Descriptor rendering

3. **Cross-protocol ACL interop**
   - **Test:** Set ACL via NFS, verify it appears correctly in Windows Security tab
   - **Expected:** NFSv4 ACEs translated to Windows DACL entries with correct SIDs
   - **Why human:** Requires both NFS and SMB clients against same server

4. **dittofsctl idmap commands against running server**
   - **Test:** Run `dittofsctl idmap add/list/remove` against a live server
   - **Expected:** Commands succeed, mappings persist in control plane store
   - **Why human:** Requires running DittoFS server with admin auth

### Gaps Summary

No gaps found. All 10 observable truths verified. All 28+ artifacts exist, are substantive (no stubs), and are properly wired. All tests pass with race detection. No anti-patterns detected. The phase goal is achieved.

Total new code: ~4,912 lines across 28+ files (source + tests).

---

_Verified: 2026-02-16T10:30:00Z_
_Verifier: Claude (gsd-verifier)_

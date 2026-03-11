# Phase 31: Windows ACL Support - Research

**Researched:** 2026-02-27
**Domain:** NT Security Descriptors, POSIX-to-DACL synthesis, SID mapping, cross-protocol ACL consistency
**Confidence:** HIGH

## Summary

Phase 31 extends DittoFS's existing ACL infrastructure to production-ready Windows support. The codebase already has substantial foundational code: `pkg/metadata/acl/` provides NFSv4 ACE types, evaluation, validation, inheritance, and mode synchronization. `internal/adapter/smb/v2/handlers/security.go` (~625 lines) provides SID encoding/decoding, SD building/parsing, principal-to-SID mapping, and a well-known SID table. The NFS adapter in `internal/adapter/nfs/v4/attrs/acl.go` provides FATTR4_ACL encoding/decoding. QUERY_INFO and SET_INFO already dispatch to security handlers.

The primary work is: (1) replace the fixed `S-1-5-21-0-0-0-{uid}` SID scheme with a unique machine SID and Samba-style RID separation for users vs groups, (2) replace the "Everyone: Full Access" fallback DACL with POSIX-mode-derived DACLs containing proper deny/allow ACEs, (3) add SD control flags (SE_DACL_AUTO_INHERITED, SE_DACL_PROTECTED), SACL stub, ACE flag translation (NFSv4 0x80 -> Windows 0x10 for INHERITED_ACE), and canonical ACE ordering, (4) refactor SID/ACL code from SMB handlers to shared packages (`pkg/auth/sid/`, `pkg/auth/acl/`), and (5) add ACL persistence via explicit `GetACL`/`SetACL` on MetadataStore, plus an `lsarpc` pipe stub for Explorer SID-to-name resolution.

**Primary recommendation:** Build incrementally on the existing security.go code. Refactor SID types and helpers to `pkg/auth/sid/`, extend `pkg/metadata/acl/` (which already handles evaluation/inheritance/validation) with a `SynthesizeFromMode()` function for POSIX-to-DACL, and add the SMB-specific SD wire format translation in `internal/adapter/smb/acl/`.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Unique machine SID generated per server instance, persisted in control plane database (SettingsStore)
- Samba-style RID mapping: users get RID = UID*2+1000, groups get RID = GID*2+1001 -- safest for smbtorture ACL conformance tests in Phase 32
- SID mapper lives in shared package `pkg/auth/sid/` -- reusable by both SMB and NFS
- UID 0 (root) maps to BUILTIN\Administrators (S-1-5-32-544)
- Anonymous/guest connections use Windows Anonymous SID (S-1-5-7)
- Basic LSA stub (LookupSids) so Explorer shows Unix usernames instead of raw SIDs
- Compute SID mappings on-the-fly (Samba-style is pure arithmetic, no cache needed)
- Internal representation based on NFSv4 ACL semantics (access mask bits are nearly 1:1 with Windows ACEs)
- Abstract model captures the most complex scenario; SMB translates to ACE, NFS translates to POSIX
- ACLs are persisted per-file in the metadata store (not synthesized on-the-fly) for easier debugging and manual testing
- ACL source tracking: store flag indicating origin ('posix-derived', 'smb-explicit', 'nfs-explicit')
- Core model in `pkg/auth/acl/`
- Enforce 64KB DACL size limit (Windows default MAX_ACL_SIZE)
- Fine-grained POSIX-to-ACE mapping -- each POSIX bit maps to full set of related Windows rights
- Explicit DENY ACEs generated when POSIX mode restricts group/other below owner
- Always canonical ACE ordering: explicit deny -> explicit allow -> inherited deny -> inherited allow
- CONTAINER_INHERIT_ACE + OBJECT_INHERIT_ACE flags set on ALL directory ACEs
- ACL inheritance from parent directories
- Well-known SIDs in default DACLs
- Accept all Windows ACL changes to abstract model (full ACL stored)
- Best-effort POSIX mode derivation using "most permissive" strategy
- chmod (NFS) always overwrites ACL regardless of source
- Allow ownership changes via SMB SET_INFO, restricted to callers with WRITE_OWNER rights
- Store unknown/domain SIDs but don't enforce
- Support SE_DACL_PROTECTED flag
- Empty SACL stub returned for SACL_SECURITY_INFORMATION queries
- ACL changes trigger immediate (synchronous) POSIX mode update in same metadata transaction
- Debug-level logging for ACL changes
- Both SMB and NFS read/write from the same abstract ACL in metadata
- SMB translator: `internal/adapter/smb/acl/`
- NFS translator: `internal/adapter/nfs/acl/`
- ACL enforcement in abstract layer (`pkg/auth/acl/`)
- Explicit `GetACL(handle)` and `SetACL(handle, acl)` methods added to MetadataStore interface
- Storage matches each store's natural model: PostgreSQL uses separate `file_acls` table, BadgerDB uses key-prefix scheme (`acl:{handle}`), Memory uses simple `map[FileHandle]*ACL`
- Shared ACL conformance test suite (`pkg/auth/acl/acltest/`)
- E2E tests using smbclient + smbcacls
- Basic `dfsctl acl show` command
- New `docs/ACL.md`
- Update CLAUDE.md with new package structure

### Claude's Discretion
- Well-known SID set selection (minimal vs extended) based on smbtorture/Explorer expectations
- Cross-protocol oplock/delegation break policy for ACL changes
- LSA stub implementation scope (LookupSids minimum, LookupNames if needed)
- Exact fine-grained access right mapping table for each POSIX bit
- BadgerDB key-prefix format details

### Deferred Ideas (OUT OF SCOPE)
- Full LSA (LookupNames, TranslateName, etc.) -- future phase if domain join support is added
- `dfsctl acl set` command for modifying ACLs via CLI -- keep read-only for now
- SACL audit logging -- empty stub for now, real audit ACEs in a future phase
- Domain SID resolution (mapping domain SIDs to actual usernames) -- requires AD/LDAP integration
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SD-01 | Default DACL synthesized from POSIX mode bits (owner/group/other) when no ACL exists | Pattern 1: `SynthesizeFromMode()` function maps mode triplets to deny+allow ACE pairs with fine-grained rights |
| SD-02 | ACEs ordered in canonical Windows order (deny before allow) | Pattern 2: Canonical sorting function with 4 buckets (existing `aceBucket()` in validate.go). Already validated by `ValidateACL()` |
| SD-03 | Well-known SIDs included in default DACL (NT AUTHORITY\SYSTEM, BUILTIN\Administrators) | Pattern 3: Well-known SID constants; SYSTEM gets full access, Administrators gets full access in synthesized DACLs |
| SD-04 | ACE flag translation corrected (NFSv4 INHERITED_ACE 0x80 -> Windows 0x10) | Pattern 4: Explicit `nfsv4FlagsToWindowsFlags()` / `windowsFlagsToNFSv4Flags()` functions replacing direct `& 0xFF` truncation |
| SD-05 | Inheritance flags (CONTAINER_INHERIT, OBJECT_INHERIT) set on directory ACEs | Pattern 1: `SynthesizeFromMode()` adds CI+OI flags on all directory ACEs. Existing `inherit.go` handles propagation |
| SD-06 | SE_DACL_AUTO_INHERITED control flag set when ACEs have INHERITED flag | Pattern 5: SD control flag computation scans ACEs for INHERITED_ACE, sets SE_DACL_AUTO_INHERITED accordingly |
| SD-07 | SID user/group collision fixed (different RID ranges for users vs groups) | Pattern 6: Samba-style RID mapping: users = UID*2+1000, groups = GID*2+1001. Machine SID replaces fixed `0-0-0` |
| SD-08 | SACL query returns valid empty SACL structure (not omitted) | Pattern 7: When SACL_SECURITY_INFORMATION requested, return valid empty SACL (revision=2, count=0, size=8) with SE_SACL_PRESENT flag |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `pkg/metadata/acl/` | existing | NFSv4 ACE types, evaluation, validation, inheritance, mode sync | Already implemented and tested; extend, don't replace |
| `internal/adapter/smb/v2/handlers/security.go` | existing | SID struct, encode/decode, SD building/parsing, principal-to-SID mapping | ~625 lines of working code to refactor into shared packages |
| `internal/adapter/nfs/v4/attrs/acl.go` | existing | FATTR4_ACL XDR encoding/decoding | Already handles NFSv4 wire format |
| `pkg/controlplane/store/` SettingsStore | existing | Key-value settings for machine SID persistence | `GetSetting`/`SetSetting` already available |
| Go standard library `encoding/binary`, `bytes` | go1.21+ | SD/SID binary encoding | No external dependencies needed |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/adapter/smb/rpc/` | existing | DCE/RPC pipe framework (PipeState, PipeManager, DCERPC) | Extend for lsarpc pipe (LSA LookupSids stub) |
| `pkg/metadata/storetest/` | existing | Conformance test suite for metadata stores | Model for new `pkg/auth/acl/acltest/` |
| `github.com/stretchr/testify` | existing | Test assertions | Used throughout project for E2E tests |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Samba RID mapping (UID*2+1000) | S-1-22-1/S-1-22-2 convention | Samba RID is better for smbtorture conformance (Phase 32 locked decision) |
| Per-file ACL storage | On-the-fly synthesis | Persistence is locked decision for debuggability |
| Shared `pkg/auth/sid/` | Keep SID code in SMB handlers | Shared package required for cross-protocol use (locked decision) |

## Architecture Patterns

### Recommended Package Structure
```
pkg/auth/
├── sid/                     # NEW: SID types and mapping (refactored from security.go)
│   ├── sid.go              # SID struct, Encode, Decode, Format, Parse
│   ├── mapper.go           # SIDMapper (machine SID, RID mapping, well-known SIDs)
│   ├── wellknown.go        # Well-known SID constants and tables
│   └── sid_test.go         # Unit tests
├── acl/                    # NEW: Abstract ACL model (extends pkg/metadata/acl/)
│   ├── model.go            # AbstractACL, ACLSource enum, SD control flags
│   ├── synthesize.go       # SynthesizeFromMode() - POSIX to ACL
│   ├── derive.go           # DeriveMode() - ACL to POSIX (moved from metadata/acl)
│   ├── evaluate.go         # Access check (moved from metadata/acl)
│   ├── validate.go         # Canonical ordering validation (moved from metadata/acl)
│   ├── inherit.go          # Inheritance (moved from metadata/acl)
│   └── acltest/            # Conformance test suite
│       └── suite.go        # Tests all metadata store impls must pass

internal/adapter/smb/acl/   # NEW: SMB-specific ACL wire format translation
├── translate.go            # ACL <-> NT Security Descriptor wire format
├── flags.go                # ACE flag translation (NFSv4 <-> Windows)
└── translate_test.go

internal/adapter/nfs/acl/   # NEW: NFS-specific ACL translation
├── translate.go            # ACL <-> NFSv4 ACE wire format
└── translate_test.go

internal/adapter/smb/rpc/   # EXTEND: Add lsarpc pipe handler
├── lsarpc.go              # NEW: LSA LookupSids stub
└── pipe.go                # MODIFY: Add lsarpc to IsSupportedPipe()
```

### Pattern 1: POSIX Mode to DACL Synthesis

**What:** Generate a Windows-compatible DACL from POSIX mode bits with fine-grained rights, deny ACEs, well-known SIDs, and inheritance flags for directories.

**When to use:** When a file has no explicit ACL (`file.ACL == nil`) and a Windows client requests the security descriptor.

**Example:**
```go
// SynthesizeFromMode generates a canonical DACL from POSIX mode bits.
// For mode 0750 on a directory, produces:
//   1. DENY GROUP@ write rights (owner has write but group doesn't)
//   2. DENY EVERYONE@ all rights (other=0)
//   3. ALLOW OWNER@ full file rights + CI|OI
//   4. ALLOW GROUP@ read+execute rights + CI|OI
//   5. ALLOW NT AUTHORITY\SYSTEM full access + CI|OI
//   6. ALLOW BUILTIN\Administrators full access + CI|OI
func SynthesizeFromMode(mode uint32, isDirectory bool) *ACL {
    var aces []ACE
    ownerRWX := (mode >> 6) & 0x7
    groupRWX := (mode >> 3) & 0x7
    otherRWX := mode & 0x7

    // Compute inheritance flags for directories
    var inheritFlags uint32
    if isDirectory {
        inheritFlags = ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE
    }

    // Step 1: Generate DENY ACEs where group/other have fewer rights than owner
    groupDeny := ownerRWX &^ groupRWX  // bits owner has but group doesn't
    otherDeny := ownerRWX &^ otherRWX  // bits owner has but other doesn't

    if groupDeny != 0 {
        aces = append(aces, ACE{
            Type: ACE4_ACCESS_DENIED_ACE_TYPE,
            Flag: inheritFlags,
            AccessMask: rwxToFullMask(groupDeny, isDirectory),
            Who: SpecialGroup,
        })
    }
    if otherDeny != 0 {
        aces = append(aces, ACE{
            Type: ACE4_ACCESS_DENIED_ACE_TYPE,
            Flag: inheritFlags,
            AccessMask: rwxToFullMask(otherDeny, isDirectory),
            Who: SpecialEveryone,
        })
    }

    // Step 2: Generate ALLOW ACEs
    if ownerRWX != 0 {
        aces = append(aces, ACE{
            Type: ACE4_ACCESS_ALLOWED_ACE_TYPE,
            Flag: inheritFlags,
            AccessMask: rwxToFullMask(ownerRWX, isDirectory) | alwaysGrantedMask,
            Who: SpecialOwner,
        })
    }
    if groupRWX != 0 {
        aces = append(aces, ACE{
            Type: ACE4_ACCESS_ALLOWED_ACE_TYPE,
            Flag: inheritFlags,
            AccessMask: rwxToFullMask(groupRWX, isDirectory),
            Who: SpecialGroup,
        })
    }
    if otherRWX != 0 {
        aces = append(aces, ACE{
            Type: ACE4_ACCESS_ALLOWED_ACE_TYPE,
            Flag: inheritFlags,
            AccessMask: rwxToFullMask(otherRWX, isDirectory),
            Who: SpecialEveryone,
        })
    }

    // Step 3: Well-known SIDs (always present)
    aces = append(aces, wellKnownSystemACE(inheritFlags))
    aces = append(aces, wellKnownAdminACE(inheritFlags))

    return &ACL{ACEs: aces, Source: ACLSourcePOSIXDerived}
}
```

### Pattern 2: Fine-Grained POSIX-to-Windows Rights Mapping

**What:** Each POSIX rwx bit maps to multiple Windows access rights for full Explorer/icacls compatibility.

**Table (Claude's discretion item - recommended mapping):**

| POSIX Bit | File Rights | Directory Rights |
|-----------|-------------|-----------------|
| Read (r) | READ_DATA, READ_ATTRIBUTES, READ_NAMED_ATTRS, READ_ACL, SYNCHRONIZE | LIST_DIRECTORY, READ_ATTRIBUTES, READ_NAMED_ATTRS, READ_ACL, SYNCHRONIZE |
| Write (w) | WRITE_DATA, APPEND_DATA, WRITE_ATTRIBUTES, WRITE_NAMED_ATTRS | ADD_FILE, ADD_SUBDIRECTORY, WRITE_ATTRIBUTES, WRITE_NAMED_ATTRS, DELETE_CHILD |
| Execute (x) | EXECUTE, READ_ATTRIBUTES, READ_ACL, SYNCHRONIZE | EXECUTE (traverse), READ_ATTRIBUTES, READ_ACL, SYNCHRONIZE |

**Note:** NFSv4 ACE mask bits are intentionally identical to Windows ACCESS_MASK bit positions (by design per RFC 7530), so no mask bit translation is needed -- only the principal format and flags differ.

### Pattern 3: Machine SID Generation and RID Mapping

**What:** Generate a unique machine SID on first boot, persist in SettingsStore, and use Samba-style arithmetic for user/group RID separation.

**Example:**
```go
// Machine SID format: S-1-5-21-{a}-{b}-{c}
// where a, b, c are random 32-bit values generated once and persisted.

type SIDMapper struct {
    machineSID [3]uint32  // The three sub-authorities of the domain SID
}

func (m *SIDMapper) UserSID(uid uint32) *SID {
    if uid == 0 {
        return WellKnownAdministrators  // S-1-5-32-544
    }
    rid := uid*2 + 1000
    return &SID{
        Revision: 1, SubAuthorityCount: 5,
        IdentifierAuthority: ntAuthority,
        SubAuthorities: []uint32{21, m.machineSID[0], m.machineSID[1], m.machineSID[2], rid},
    }
}

func (m *SIDMapper) GroupSID(gid uint32) *SID {
    rid := gid*2 + 1001  // +1001 (odd) vs +1000 (even) prevents collision
    return &SID{
        Revision: 1, SubAuthorityCount: 5,
        IdentifierAuthority: ntAuthority,
        SubAuthorities: []uint32{21, m.machineSID[0], m.machineSID[1], m.machineSID[2], rid},
    }
}

func (m *SIDMapper) UIDFromSID(sid *SID) (uint32, bool) {
    if !m.isDomainSID(sid) { return 0, false }
    rid := sid.SubAuthorities[4]
    if rid < 1000 { return 0, false }
    if (rid - 1000) % 2 != 0 { return 0, false }  // Odd RID = group
    return (rid - 1000) / 2, true
}

func (m *SIDMapper) GIDFromSID(sid *SID) (uint32, bool) {
    if !m.isDomainSID(sid) { return 0, false }
    rid := sid.SubAuthorities[4]
    if rid < 1001 { return 0, false }
    if (rid - 1001) % 2 != 0 { return 0, false }  // Even RID = user
    return (rid - 1001) / 2, true
}
```

### Pattern 4: ACE Flag Translation (NFSv4 <-> Windows)

**What:** Explicit bidirectional flag mapping because INHERITED_ACE has different bit positions.

**Example:**
```go
// Source: MS-DTYP Section 2.4.4.1, RFC 7530 Section 6.2.1
func nfsv4FlagsToWindowsFlags(nfsFlags uint32) uint8 {
    var winFlags uint8
    if nfsFlags&ACE4_FILE_INHERIT_ACE != 0           { winFlags |= 0x01 }
    if nfsFlags&ACE4_DIRECTORY_INHERIT_ACE != 0      { winFlags |= 0x02 }
    if nfsFlags&ACE4_NO_PROPAGATE_INHERIT_ACE != 0   { winFlags |= 0x04 }
    if nfsFlags&ACE4_INHERIT_ONLY_ACE != 0           { winFlags |= 0x08 }
    if nfsFlags&ACE4_INHERITED_ACE != 0              { winFlags |= 0x10 } // Critical: 0x80 -> 0x10
    return winFlags
}

func windowsFlagsToNFSv4Flags(winFlags uint8) uint32 {
    var nfsFlags uint32
    if winFlags&0x01 != 0 { nfsFlags |= ACE4_FILE_INHERIT_ACE }
    if winFlags&0x02 != 0 { nfsFlags |= ACE4_DIRECTORY_INHERIT_ACE }
    if winFlags&0x04 != 0 { nfsFlags |= ACE4_NO_PROPAGATE_INHERIT_ACE }
    if winFlags&0x08 != 0 { nfsFlags |= ACE4_INHERIT_ONLY_ACE }
    if winFlags&0x10 != 0 { nfsFlags |= ACE4_INHERITED_ACE }  // Critical: 0x10 -> 0x80
    return nfsFlags
}
```

### Pattern 5: SD Control Flag Computation

**What:** Compute SE_DACL_AUTO_INHERITED and SE_DACL_PROTECTED based on ACL state.

**Example:**
```go
const (
    seSelfRelative       = 0x8000
    seDACLPresent        = 0x0004
    seSACLPresent        = 0x0010
    seDACLAutoInherited  = 0x0400
    seDACLProtected      = 0x1000
)

func computeSDControlFlags(fileACL *acl.ACL, includeDACL, includeSACL bool, protected bool) uint16 {
    control := uint16(seSelfRelative)
    if includeDACL {
        control |= seDACLPresent
    }
    if includeSACL {
        control |= seSACLPresent
    }

    // SE_DACL_AUTO_INHERITED: set if ANY ACE has INHERITED_ACE flag
    if fileACL != nil {
        for _, ace := range fileACL.ACEs {
            if ace.IsInherited() {
                control |= seDACLAutoInherited
                break
            }
        }
    }

    // SE_DACL_PROTECTED: blocks inheritance propagation from parent
    if protected {
        control |= seDACLProtected
    }

    return control
}
```

### Pattern 6: SACL Stub

**What:** Return a valid empty SACL structure when SACL_SECURITY_INFORMATION is requested, instead of omitting it.

**Example:**
```go
// Empty SACL: revision=2, sbz1=0, size=8, count=0, sbz2=0
func buildEmptySACL(buf *bytes.Buffer) {
    buf.WriteByte(2)                                           // AclRevision
    buf.WriteByte(0)                                           // Sbz1
    binary.Write(buf, binary.LittleEndian, uint16(8))          // AclSize (header only)
    binary.Write(buf, binary.LittleEndian, uint16(0))          // AceCount
    binary.Write(buf, binary.LittleEndian, uint16(0))          // Sbz2
}
```

### Pattern 7: LSA LookupSids Stub

**What:** Minimal lsarpc named pipe handler so Windows Explorer can resolve SIDs to display names.

**When to use:** When Windows client opens `\pipe\lsarpc` and sends LookupSids2 or LookupSids3.

**Example:**
```go
// LSA interface UUID: 12345778-1234-abcd-ef00-0123456789ab
var LSARPCInterfaceUUID = [16]byte{...}

const (
    OpLsarLookupSids2 uint16 = 57  // LookupSids2
    OpLsarLookupSids3 uint16 = 76  // LookupSids3
)

// For each SID in the request:
// - Machine domain SIDs: resolve RID to "unix_user:{uid}" or "unix_group:{gid}"
// - Well-known SIDs: return standard names (Everyone, SYSTEM, Administrators)
// - Unknown SIDs: return SID_NAME_UNKNOWN with the SID string as name
```

### Anti-Patterns to Avoid
- **Rewriting existing ACL code:** The `pkg/metadata/acl/` package has working evaluation, validation, inheritance, and mode sync. Extend, don't replace.
- **Synthesizing ACLs on every QUERY_INFO:** Persist the synthesized ACL on first access so subsequent queries return the same ACL. The user locked this decision for debuggability.
- **Direct bit truncation for ACE flags:** The current `uint8(ace.Flag & 0xFF)` is incorrect for INHERITED_ACE. Use explicit flag translation functions.
- **Same RID range for users and groups:** UID 1000 and GID 1000 must produce different SIDs. The Samba-style `*2+1000`/`*2+1001` scheme is locked.
- **Silently omitting SACL:** Return a valid empty structure with SE_SACL_PRESENT, not zero offset.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SID binary encoding | Custom byte manipulation | Existing `EncodeSID`/`DecodeSID` in security.go | Already tested, handles alignment correctly |
| ACL canonical ordering | Custom sorting | Existing `aceBucket()` in validate.go as sort key | Bucket classification already correct for 4-bucket canonical order |
| NFSv4 ACL evaluation | Custom access check | Existing `Evaluate()` in `pkg/metadata/acl/evaluate.go` | RFC 7530 compliant, handles INHERIT_ONLY, OWNER@/GROUP@/EVERYONE@ |
| ACL inheritance propagation | Manual parent-child traversal | Existing `ComputeInheritedACL`/`PropagateACL` in inherit.go | Handles all inheritance flag combinations |
| DCE/RPC pipe framework | New pipe infrastructure | Existing `internal/adapter/smb/rpc/` (PipeState, PipeManager, DCERPC) | Add lsarpc handler alongside existing srvsvc handler |
| SD binary format | Custom serialization | Extend existing `BuildSecurityDescriptor`/`ParseSecurityDescriptor` | Already handles header, offsets, padding, alignment |

**Key insight:** Over 80% of the infrastructure exists. The phase is primarily about (a) refactoring SID/ACL code to shared packages, (b) adding POSIX-to-DACL synthesis, (c) fixing the SID collision bug and ACE flag translation, and (d) adding SD control flags and SACL stub.

## Common Pitfalls

### Pitfall 1: SD Field Byte Ordering (Conformance Risk)
**What goes wrong:** Current code emits SD fields as Owner-Group-DACL. Windows implementations typically emit SACL-DACL-Owner-Group. While offsets are correct either way, smbtorture byte-level comparisons may flag mismatches.
**Why it happens:** Developers follow the struct definition top-to-bottom.
**How to avoid:** Follow Windows-observed field order: SACL, then DACL, then Owner SID, then Group SID. The offsets in the 20-byte header still point correctly.
**Warning signs:** smbtorture `smb2.acls` tests fail with "SD mismatch" even when parsed SD is logically correct.

### Pitfall 2: POSIX Mode 750 Deny ACE Generation
**What goes wrong:** When POSIX mode gives owner rwx but group only rx, the DENY ACE for GROUP@ must explicitly deny WRITE_DATA + APPEND_DATA + DELETE_CHILD (for directories) + WRITE_ATTRIBUTES + WRITE_NAMED_ATTRS. Missing any deny bit means the group effectively gets write access through EVERYONE@ allow or inherited ACEs.
**Why it happens:** Developer only denies the basic WRITE_DATA bit, forgetting the compound rights.
**How to avoid:** Use the `rwxToFullMask()` function that maps each rwx bit to the FULL set of corresponding Windows rights, then compute deny as `ownerMask &^ groupMask`.
**Warning signs:** icacls shows group with write permission on a 750 directory.

### Pitfall 3: ACL Source Tracking Lost on Round-Trip
**What goes wrong:** A file gets a POSIX-derived ACL. SMB client reads it (QUERY_INFO), makes no changes, writes it back (SET_INFO). The source changes from 'posix-derived' to 'smb-explicit', and subsequent chmod no longer overwrites because the code treats explicit ACLs differently.
**Why it happens:** SET_INFO always marks source as 'smb-explicit'.
**How to avoid:** Compare incoming ACL with stored ACL before changing source. If they are semantically identical, preserve the original source flag. Or: always allow chmod to overwrite regardless of source (locked decision: "chmod = reset to POSIX").
**Warning signs:** chmod on a file with SMB-set ACL has no effect on the Windows-visible permissions.

### Pitfall 4: Machine SID Not Initialized Before First Connection
**What goes wrong:** The machine SID is generated on demand during first QUERY_INFO, but multiple concurrent connections race to generate it, producing different SIDs for different connections.
**Why it happens:** Lazy initialization without proper synchronization.
**How to avoid:** Generate and persist machine SID during server startup in lifecycle.Serve(), before any connections are accepted. Use `SettingsStore.GetSetting("machine_sid")` with `SetSetting` on first boot.
**Warning signs:** Different SIDs appear in security descriptors for files accessed by different clients.

### Pitfall 5: Well-Known SID ACEs Ordered After User ACEs
**What goes wrong:** NT AUTHORITY\SYSTEM and BUILTIN\Administrators ACEs are appended at the end of the DACL. Windows canonical ordering requires explicit allow ACEs in a specific sub-order, and some tools expect SYSTEM before user ACEs.
**Why it happens:** Developer appends well-known SIDs after the owner/group/other ACEs.
**How to avoid:** Insert well-known SID allow ACEs in the explicit-allow bucket, typically after OWNER@ allow but before GROUP@ allow. Alternatively, use the canonical sort function which will place them correctly.
**Warning signs:** icacls output shows SYSTEM after user entries, which while functional looks non-standard.

### Pitfall 6: SACL Offset Zero vs. SACL Present with Empty Structure
**What goes wrong:** Setting SACL offset to 0 in the SD header means "no SACL present." But the SE_SACL_PRESENT flag says there IS a SACL. This contradiction confuses Windows clients.
**Why it happens:** Developer sets the flag but doesn't write the actual SACL bytes.
**How to avoid:** When including SACL stub, write a valid 8-byte empty ACL (revision=2, size=8, count=0) and set the offset to point to it. When not including SACL, don't set SE_SACL_PRESENT.
**Warning signs:** Windows audit tools crash or show "corrupted SACL" errors.

## Code Examples

### Existing Code to Reuse (verified from codebase)

#### SID Types and Encoding (from security.go, to be moved to pkg/auth/sid/)
```go
// Already implemented and tested:
// - SID struct with Revision, SubAuthorityCount, IdentifierAuthority, SubAuthorities
// - EncodeSID(buf, sid), DecodeSID(data) (*SID, int, error)
// - FormatSID(sid) string, ParseSIDString(s) (*SID, error)
// - PrincipalToSID(who, ownerUID, ownerGID) *SID
// - SIDToPrincipal(sid) string
// - isDittoFSUserSID(sid) (rid, ok)
// Tests: TestSIDEncodeDecodeRoundTrip, TestPrincipalToSID, TestSIDToPrincipal
```

#### SD Building/Parsing (from security.go, to be extended)
```go
// Already implemented:
// - BuildSecurityDescriptor(file, additionalSecInfo) ([]byte, error)
// - ParseSecurityDescriptor(data) (ownerUID, ownerGID, fileACL, err)
// - buildDACL(buf, file) -- currently "Everyone: Full Access" fallback
// - parseDACL(data) (*acl.ACL, error)
// - alignTo4(), padTo4() helpers
```

#### ACL Infrastructure (from pkg/metadata/acl/, to be extended)
```go
// Already implemented:
// - ACE struct with Type, Flag, AccessMask, Who (JSON-serializable)
// - ACL struct with ACEs slice
// - Evaluate(acl, evalCtx, requestedMask) bool
// - ValidateACL(acl) error (canonical ordering check)
// - DeriveMode(acl) uint32
// - AdjustACLForMode(acl, newMode) *ACL
// - ComputeInheritedACL(parentACL, isDirectory) *ACL
// - PropagateACL(parentACL, existingACL, isDirectory) *ACL
```

#### DCE/RPC Pipe Framework (from internal/adapter/smb/rpc/)
```go
// Already implemented:
// - PipeState with ProcessWrite, ProcessRead, Transact
// - PipeManager with CreatePipe, GetPipe, ClosePipe
// - DCERPC Header, BindRequest, BindAck, Request, Response parsing/encoding
// - SRVSVCHandler as reference implementation for new LSARPCHandler
// - IsSupportedPipe() -- extend to include "lsarpc"
```

### Well-Known SIDs for Default DACLs (Claude's discretion recommendation)

**Recommended minimal set** (sufficient for Explorer and icacls):

| SID | Name | Purpose |
|-----|------|---------|
| S-1-5-18 | NT AUTHORITY\SYSTEM | Always gets full access in synthesized DACLs |
| S-1-5-32-544 | BUILTIN\Administrators | Always gets full access in synthesized DACLs |
| S-1-1-0 | Everyone | Maps to EVERYONE@ in NFSv4 |
| S-1-3-0 | CREATOR OWNER | Maps to OWNER@ in NFSv4 |
| S-1-3-1 | CREATOR GROUP | Maps to GROUP@ in NFSv4 |
| S-1-5-7 | Anonymous | Used for anonymous/guest connections |

**Extended set** (add if smbtorture requires, defer if not):

| SID | Name | When Needed |
|-----|------|-------------|
| S-1-5-11 | Authenticated Users | If smbtorture checks for this in default DACLs |
| S-1-5-32-545 | BUILTIN\Users | If Explorer expects this for non-admin users |

### POSIX Bit to Full Windows Mask Mapping (Claude's discretion recommendation)

```go
// rwxToFullMask maps a 3-bit rwx value to the FULL set of Windows access rights.
// This is the fine-grained mapping needed for Explorer/icacls to display meaningful permissions.
func rwxToFullMask(rwx uint32, isDirectory bool) uint32 {
    var mask uint32

    if rwx&0x4 != 0 { // Read
        mask |= ACE4_READ_DATA | ACE4_READ_ATTRIBUTES | ACE4_READ_NAMED_ATTRS | ACE4_READ_ACL | ACE4_SYNCHRONIZE
    }
    if rwx&0x2 != 0 { // Write
        mask |= ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_WRITE_ATTRIBUTES | ACE4_WRITE_NAMED_ATTRS
        if isDirectory {
            mask |= ACE4_DELETE_CHILD
        }
    }
    if rwx&0x1 != 0 { // Execute
        mask |= ACE4_EXECUTE
        // Read attributes and synchronize are implicitly needed for execute
        mask |= ACE4_READ_ATTRIBUTES | ACE4_SYNCHRONIZE
    }

    return mask
}

// alwaysGrantedMask contains rights always granted to the owner:
// READ_ACL (can always read own ACL), WRITE_ACL (owner can change ACL),
// WRITE_OWNER (can take ownership), DELETE (can delete own files)
const alwaysGrantedMask = ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_WRITE_OWNER | ACE4_DELETE | ACE4_SYNCHRONIZE
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Everyone: Full Access fallback | POSIX-derived DACLs with deny/allow | Phase 31 | Explorer shows real permissions |
| Fixed domain SID `0-0-0` | Unique machine SID per instance | Phase 31 | SIDs are globally unique |
| Same RID for user and group | `*2+1000` / `*2+1001` separation | Phase 31 | UID 1000 != GID 1000 |
| Direct flag truncation `& 0xFF` | Explicit flag translation function | Phase 31 | INHERITED_ACE displays correctly |
| No SACL handling | Valid empty SACL stub | Phase 31 | SACL queries don't fail |
| SID/ACL in SMB handlers only | Shared packages (`pkg/auth/sid/`, `pkg/auth/acl/`) | Phase 31 | NFS and SMB share same ACL logic |

**Deprecated/outdated:**
- `makeDittoFSUserSID` / `makeDittoFSGroupSID` with fixed `0-0-0` domain: replaced by SIDMapper with machine SID
- Direct `ace.Flag & 0xFF` for ACE flags: replaced by explicit translation functions
- "Everyone: Full Access" fallback in `buildDACL`: replaced by `SynthesizeFromMode`

## Open Questions

1. **LSA LookupSids: Which opnum versions to support?**
   - What we know: Windows uses LookupSids2 (opnum 57) or LookupSids3 (opnum 76). Explorer typically uses LookupSids2.
   - What's unclear: Whether supporting only LookupSids2 is sufficient or if LookupSids3 is required for Windows 11 24H2.
   - Recommendation: Implement LookupSids2 first, add LookupSids3 if testing shows it's needed. Both have the same response format.

2. **SD Field Byte Ordering for Conformance**
   - What we know: Current code emits Owner-Group-DACL. Windows emits SACL-DACL-Owner-Group. Both are valid per spec (offsets determine layout).
   - What's unclear: Whether smbtorture in Phase 32 will perform byte-level comparison or only logical comparison.
   - Recommendation: Reorder to match Windows convention (SACL-DACL-Owner-Group) proactively. Low risk, prevents future conformance failures.

3. **Cross-Protocol Oplock Break on ACL Changes (Claude's discretion)**
   - What we know: CONTEXT.md says "Cross-protocol ACL changes do NOT trigger oplock/delegation breaks."
   - What's unclear: Whether this causes any conformance test failures.
   - Recommendation: Start with no oplock break on ACL changes (simplest, matches locked decision). Add if testing reveals issues.

4. **BadgerDB Key-Prefix Format for ACL Storage (Claude's discretion)**
   - What we know: Must use key-prefix scheme. FileHandle is the lookup key.
   - What's unclear: Exact format details.
   - Recommendation: Use `acl:{shareNameHex}:{handleHex}` to match BadgerDB's existing key patterns (see how it stores file data). JSON-encode the ACL value.

## Sources

### Primary (HIGH confidence)
- DittoFS source code: `pkg/metadata/acl/` (types.go, evaluate.go, mode.go, inherit.go, validate.go) -- verified by direct reading
- DittoFS source code: `internal/adapter/smb/v2/handlers/security.go` (~625 lines) -- verified by direct reading
- DittoFS source code: `internal/adapter/nfs/v4/attrs/acl.go` (XDR encoding) -- verified by direct reading
- DittoFS source code: `internal/adapter/smb/rpc/` (pipe.go, srvsvc.go, dcerpc.go) -- verified by direct reading
- DittoFS source code: `pkg/metadata/file_types.go` (FileAttr with ACL field) -- verified by direct reading
- DittoFS source code: `pkg/metadata/file_modify.go` (SetFileAttributes with ACL handling) -- verified by direct reading
- DittoFS source code: `pkg/metadata/auth_permissions.go` (ACL-based permission evaluation) -- verified by direct reading
- DittoFS source code: `pkg/controlplane/store/interface.go` (SettingsStore for machine SID) -- verified by direct reading
- [MS-DTYP: SECURITY_DESCRIPTOR (Section 2.4.6)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/7d4dac05-9cef-4563-a058-f108abecce1d) -- HIGH confidence
- [Order of ACEs in a DACL (Microsoft)](https://learn.microsoft.com/en-us/windows/win32/secauthz/order-of-aces-in-a-dacl) -- HIGH confidence
- [Well-known SIDs (Microsoft)](https://learn.microsoft.com/en-us/windows/win32/secauthz/well-known-sids) -- HIGH confidence
- `.planning/research/PITFALLS.md` -- HIGH confidence (project-specific, verified)
- `.planning/research/ARCHITECTURE.md` -- HIGH confidence (project-specific, verified)

### Secondary (MEDIUM confidence)
- [Samba vfs_acl_xattr documentation](https://www.samba.org/samba/docs/current/man-html/vfs_acl_xattr.8.html) -- POSIX-to-DACL synthesis strategy
- [Samba idmap_rid backend](https://www.samba.org/samba/docs/4.9/man-html/idmap_rid.8.html) -- RID mapping reference
- [smbcacls man page](https://www.samba.org/samba/docs/current/man-html/smbcacls.1.html) -- E2E test tool reference
- [MS-DTYP canonical ACL sort order (cifs-protocol discussion)](https://www.mail-archive.com/cifs-protocol@lists.samba.org/msg01649.html) -- Clarifies sub-ordering within buckets

### Tertiary (LOW confidence)
- [SAMBA file rights and ACLs (pig.made-it.com)](http://pig.made-it.com/samba-file-rights.html) -- Community reference for POSIX-to-Windows mapping details

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries and patterns verified by direct codebase reading
- Architecture: HIGH - Package structure follows existing project conventions and locked decisions
- Pitfalls: HIGH - Verified against existing `.planning/research/PITFALLS.md` and codebase analysis
- RID mapping formula: HIGH - Locked decision (UID*2+1000 / GID*2+1001), simple arithmetic
- LSA stub: MEDIUM - Basic pattern is clear, but exact opnum version needed for Windows 11 is unclear

**Research date:** 2026-02-27
**Valid until:** 2026-03-27 (stable domain, MS-DTYP spec rarely changes)

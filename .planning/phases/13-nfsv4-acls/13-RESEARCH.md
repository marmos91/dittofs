# Phase 13: NFSv4 ACLs - Research

**Researched:** 2026-02-16
**Domain:** NFSv4 ACL model (RFC 7530 Section 6), Identity Mapping, SMB Security Descriptor interop
**Confidence:** HIGH

## Summary

Phase 13 adds NFSv4 ACL support to DittoFS, including ACL storage as first-class metadata, an ACL evaluation engine for access decisions, inheritance for new files/directories, identity mapping between NFSv4 principals and control plane users, and bidirectional ACL translation between NFSv4 and SMB protocols.

The NFSv4 ACL model is essentially a clone of the Windows ACL system (as noted in Samba documentation). This means the bidirectional translation between NFSv4 ACE mask bits and Windows ACCESS_MASK bits is largely a 1:1 mapping -- the same bit positions are intentionally used. The main translation work is between principal formats: NFSv4 uses `user@domain` strings while SMB uses binary SIDs.

The existing codebase provides strong foundations: `FileAttr` struct for adding the ACL field, `MetadataService.checkFilePermissions()` as the permission check integration point, `pkg/auth/kerberos/IdentityMapper` interface to refactor into `pkg/identity/`, NFSv4 GETATTR/SETATTR handlers with bitmap-driven attribute encoding, and the SMB `buildSecurityInfo()`/`SET_INFO` handlers that currently return minimal security descriptors.

**Primary recommendation:** Build the ACL evaluation engine as a pure, well-tested package (`pkg/metadata/acl/`), integrate it into the existing permission checking flow, and extend the NFSv4 attribute encoding and SMB security descriptor handling to provide protocol-native ACL views.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Full RFC 7530 ACL model: all four ACE types (ALLOW, DENY, AUDIT, ALARM)
- All 14 file + 2 directory permission mask bits per RFC 7530 Section 6.2.1
- Strict Windows canonical ordering enforced: explicit DENY -> explicit ALLOW -> inherited DENY -> inherited ALLOW. Mis-ordered ACLs rejected on SETATTR
- ACL overrides Unix permissions when present. Mode bits derived from ACL for display. No ACL = classic Unix permission check
- All three special identifiers supported: OWNER@, GROUP@, EVERYONE@
- ACL stored as first-class metadata field on File struct (not xattr). No Phase 25 dependency
- AUDIT and ALARM ACEs stored and returned only -- no logging/alerting triggered
- chmod adjusts OWNER@, GROUP@, EVERYONE@ ACEs to match new mode bits per RFC 7530 Section 6.4.1. ACL stays authoritative
- Maximum 128 ACEs per file
- Full FATTR4_ACLSUPPORT bitmap in GETATTR (all 4 ACE types reported)
- Layered identity strategy: convention-based default + explicit mapping table in control plane
- Convention-based: user@REALM maps to control plane user when domain matches configured Kerberos realm
- Case-insensitive domain matching (alice@EXAMPLE.COM = alice@example.com)
- Unknown principals stored as-is in ACEs, skipped during evaluation (preserves cross-domain ACLs)
- Numeric UID/GID accepted with @domain suffix (e.g., 1000@localdomain) for AUTH_SYS interop
- Pluggable IdentityMapper interface with Resolve(principal) method. Ships with ConventionMapper + TableMapper
- Group membership resolved via control plane groups (group@domain -> control plane group -> member lookup)
- Mapping table configurable via REST API (dittofsctl idmap add/list/remove). Stored in control plane DB
- Identity resolution results cached with TTL to reduce DB lookups
- Refactor Phase 12 StaticMapper into new `pkg/identity/` package
- All four RFC 7530 inheritance flags: FILE_INHERIT, DIRECTORY_INHERIT, NO_PROPAGATE_INHERIT, INHERIT_ONLY
- No ACL by default -- derive from umask/mode until explicit ACL is set. Zero migration needed for existing files
- Inherited ACEs marked with ACE4_INHERITED_ACE flag (distinguishes explicit vs inherited)
- Snapshot inheritance at creation time -- changing parent ACL does not retroactively affect children
- Recursive propagation supported: replaces inherited ACEs only on descendants, preserves explicit ACEs
- Full bidirectional DACL translation between NFSv4 ACEs and Windows DACL ACEs
- DACL only -- no SACL support
- Well-known SIDs mapped statically + user SIDs resolved via identity mapper
- Direct bidirectional mask bit mapping table between SMB FILE_* permissions and NFSv4 mask bits
- Windows Owner/Group in security descriptor bidirectionally synced with NFSv4 file owner/group_owner
- Same validation rules for both protocols: canonical ordering, 128 ACE limit
- Single stored ACL (NFSv4 format internally), protocol-native view at boundary
- SMB QUERY_INFO supports OWNER, GROUP, and DACL security information classes
- Security Descriptor binary encoding built from scratch in internal/protocol/smb/
- ACL package at `pkg/metadata/acl/`
- Identity mapper package at `pkg/identity/`
- ACL XDR encoding in `internal/protocol/nfs/`
- ACL persistence extends existing metadata store interfaces (Memory, BadgerDB, PostgreSQL)
- PostgreSQL ACL storage as JSONB column on files table
- ACL-specific Prometheus metrics: evaluation duration histogram, cache hit/miss, DENY counters, inheritance computation

### Claude's Discretion
- Recursive propagation sync/async mode and threshold
- Exact identity resolution cache TTL value
- Exact mask bit mapping table between SMB and NFSv4 permissions
- ACL evaluation algorithm implementation details
- PostgreSQL migration numbering and structure
- Exact well-known SID mapping table

### Deferred Ideas (OUT OF SCOPE)
- LDAP/AD integration for identity mapping
- AUDIT ACE logging (store-only, no triggered logging)
- Manual Windows testing (deferred to Phase 15)
- Full E2E mount-level ACL tests (deferred to Phase 15)
</user_constraints>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `encoding/binary` | go1.22+ | SID and Security Descriptor binary encoding | No external dependency needed for well-defined binary formats |
| Go stdlib `sync` | go1.22+ | Identity resolution cache with RWMutex | Standard concurrency primitive |
| Go stdlib `strings` | go1.22+ | Case-insensitive domain matching | `strings.EqualFold()` for RFC-compliant comparison |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `encoding/json` | stdlib | ACL serialization for BadgerDB/PostgreSQL JSONB | Persisting ACL to stores |
| `github.com/prometheus/client_golang` | existing dep | ACL evaluation metrics | Performance monitoring |
| `github.com/golang-migrate/migrate/v4` | existing dep | PostgreSQL schema migration for ACL column | Adding JSONB column to files table |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| JSON ACL storage | Protobuf encoding | JSON is human-readable in DB, Protobuf is smaller but adds complexity |
| Hand-built SD encoding | External Windows library | Decision is locked: build from scratch in internal/protocol/smb/ |
| sync.Map cache | LRU with TTL library | Simple map + TTL check sufficient for identity cache at expected scale |

**Installation:**
No new dependencies required. All work uses existing Go stdlib and project dependencies.

## Architecture Patterns

### Recommended Project Structure
```
pkg/metadata/acl/           # ACL types, evaluation, inheritance, validation
    types.go                 # ACE, ACL, mask constants, special principals
    evaluate.go              # ACL evaluation engine
    evaluate_test.go         # Comprehensive evaluation tests
    inherit.go               # Inheritance computation
    inherit_test.go          # Inheritance tests
    validate.go              # Canonical ordering validation, 128 ACE limit
    validate_test.go         # Validation tests
    mode.go                  # ACL <-> mode bits synchronization
    mode_test.go             # Mode sync tests
    metrics.go               # Prometheus metrics

pkg/identity/               # Identity mapping (refactored from pkg/auth/kerberos)
    mapper.go                # IdentityMapper interface + types
    convention.go            # ConventionMapper (user@REALM -> control plane user)
    convention_test.go
    table.go                 # TableMapper (explicit mapping table from DB)
    table_test.go
    cache.go                 # TTL-based resolution cache
    cache_test.go
    static.go                # StaticMapper (moved from pkg/auth/kerberos)
    static_test.go

internal/protocol/nfs/v4/attrs/   # Extended with ACL encoding/decoding
    acl.go                   # FATTR4_ACL and FATTR4_ACLSUPPORT encoding
    acl_test.go

internal/protocol/smb/v2/handlers/  # Extended with Security Descriptor
    security.go              # SD encoding/decoding, SID handling
    security_test.go
```

### Pattern 1: ACL Evaluation Algorithm
**What:** Process-first-match evaluation of NFSv4 ACEs per RFC 7530 Section 6.2.1
**When to use:** Every file access check when ACL is present on the file
**Example:**
```go
// Source: RFC 7530 Section 6.2.1
func Evaluate(acl *ACL, who string, requestedMask uint32, isOwner, isGroupMember bool) (allowed bool) {
    var allowedBits uint32
    var deniedBits uint32

    for _, ace := range acl.ACEs {
        if !aceMatchesWho(ace, who, isOwner, isGroupMember) {
            continue
        }
        switch ace.Type {
        case ACE4_ACCESS_ALLOWED_ACE_TYPE:
            // Only consider bits not yet decided
            newBits := ace.AccessMask &^ (allowedBits | deniedBits)
            allowedBits |= newBits
        case ACE4_ACCESS_DENIED_ACE_TYPE:
            newBits := ace.AccessMask &^ (allowedBits | deniedBits)
            deniedBits |= newBits
        }
        // If all requested bits are decided, stop early
        if (allowedBits|deniedBits)&requestedMask == requestedMask {
            break
        }
    }
    // Access is allowed only if ALL requested bits are in allowedBits
    return (allowedBits & requestedMask) == requestedMask
}
```

### Pattern 2: Identity Resolution with Cache
**What:** Resolve NFSv4 `user@domain` principal to control plane identity, with TTL cache
**When to use:** ACL evaluation and GETATTR owner/group encoding
**Example:**
```go
type IdentityMapper interface {
    // Resolve maps an NFSv4 principal (user@domain) to a control plane identity
    Resolve(ctx context.Context, principal string) (*ResolvedIdentity, error)
}

type ResolvedIdentity struct {
    Username string   // Control plane username
    UID      uint32   // Unix UID
    GID      uint32   // Unix primary GID
    GIDs     []uint32 // Supplementary GIDs
    Found    bool     // Whether the principal was resolved
}

type CachedMapper struct {
    inner   IdentityMapper
    cache   map[string]*cacheEntry
    mu      sync.RWMutex
    ttl     time.Duration
}
```

### Pattern 3: Protocol-Boundary Translation
**What:** Single internal ACL representation (NFSv4 format), translated at protocol boundary
**When to use:** NFS GETATTR/SETATTR and SMB QUERY_INFO/SET_INFO
**Example:**
```go
// NFS boundary: ACL stored internally, encoded to XDR at GETATTR
func EncodeACLAttr(buf *bytes.Buffer, acl *acl.ACL) error {
    // Write ACE count
    xdr.WriteUint32(buf, uint32(len(acl.ACEs)))
    for _, ace := range acl.ACEs {
        xdr.WriteUint32(buf, ace.Type)
        xdr.WriteUint32(buf, ace.Flag)
        xdr.WriteUint32(buf, ace.AccessMask)
        xdr.WriteXDRString(buf, ace.Who) // "user@domain" format
    }
    return nil
}

// SMB boundary: ACL translated to Windows Security Descriptor
func BuildSecurityDescriptor(file *metadata.File, acl *acl.ACL, mapper identity.SIDMapper) []byte {
    // 1. Build Owner SID from file.UID
    // 2. Build Group SID from file.GID
    // 3. Translate each NFSv4 ACE to Windows ACE (same mask bits, SID instead of who)
    // 4. Encode as self-relative Security Descriptor
}
```

### Pattern 4: ACL Inheritance at Creation Time
**What:** When creating a file/directory, compute inherited ACEs from parent's ACL
**When to use:** MetadataService.createEntry() and CreateFile/CreateDirectory
**Example:**
```go
func ComputeInheritedACL(parentACL *ACL, isDirectory bool) *ACL {
    if parentACL == nil {
        return nil // No parent ACL = no inherited ACL
    }
    var inherited []ACE
    for _, ace := range parentACL.ACEs {
        if isDirectory {
            if ace.Flag&ACE4_DIRECTORY_INHERIT_ACE != 0 {
                newACE := ace
                newACE.Flag |= ACE4_INHERITED_ACE
                if ace.Flag&ACE4_NO_PROPAGATE_INHERIT_ACE != 0 {
                    // Clear inheritance flags on propagated ACE
                    newACE.Flag &^= ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE |
                        ACE4_NO_PROPAGATE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE
                }
                inherited = append(inherited, newACE)
            }
        } else {
            if ace.Flag&ACE4_FILE_INHERIT_ACE != 0 {
                newACE := ace
                newACE.Flag |= ACE4_INHERITED_ACE
                // Clear all inheritance flags for files
                newACE.Flag &^= ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE |
                    ACE4_NO_PROPAGATE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE
                inherited = append(inherited, newACE)
            }
        }
    }
    if len(inherited) == 0 {
        return nil
    }
    return &ACL{ACEs: inherited}
}
```

### Anti-Patterns to Avoid
- **Checking ACL in protocol handlers:** ACL evaluation belongs in `pkg/metadata/acl/`, called from `MetadataService.checkFilePermissions()`. Handlers only encode/decode wire format.
- **Coupling ACL types to NFS XDR:** The `pkg/metadata/acl/` package must be protocol-agnostic. XDR encoding lives in `internal/protocol/nfs/v4/attrs/`.
- **Modifying ACL on chmod without understanding RFC 7530 Section 6.4.1:** When mode bits change, only OWNER@/GROUP@/EVERYONE@ ACEs are adjusted. Other ACEs are preserved.
- **Storing ACLs in xattrs:** Decision is locked -- ACL is a first-class field on `FileAttr`, not extended attributes.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| ACL evaluation algorithm | Custom evaluation loop | Follow RFC 7530 Section 6.2.1 exactly | The spec is precise and testable; diverging causes interop bugs |
| SID binary format | Guess at encoding | Follow MS-DTYP Section 2.4.2 exactly | Wrong SID encoding = Windows clients fail silently |
| Security Descriptor format | Improvise structure | Follow MS-DTYP Section 2.4.6 exactly | Self-relative SD has strict offset requirements |
| Canonical ordering validation | Ad-hoc checks | Implement the four-bucket ordering rule | Windows enforces this strictly; mis-ordered ACLs break Explorer |
| Mode-to-ACL sync | Approximate mapping | Follow RFC 7530 Section 6.4.1 exactly | Linux NFS client validates this mapping |

**Key insight:** NFSv4 ACLs and Windows ACLs are intentionally compatible -- the RFC was designed with Windows interop in mind. The mask bit values are identical. The complexity is in principal/SID translation and correct evaluation ordering, not in inventing a new model.

## Common Pitfalls

### Pitfall 1: ACL Evaluation Order Matters
**What goes wrong:** Evaluating ALLOW before DENY, or not stopping when all bits are decided
**Why it happens:** The RFC 7530 evaluation is process-in-order, not "most specific wins"
**How to avoid:** Process ACEs sequentially. Each ACE either ALLOWs or DENYs bits not yet decided. Once a bit is decided (allowed or denied), subsequent ACEs cannot change it for that bit.
**Warning signs:** Users get unexpected access when DENY ACEs appear after ALLOW ACEs

### Pitfall 2: INHERIT_ONLY ACEs Must Be Skipped During Evaluation
**What goes wrong:** An ACE with INHERIT_ONLY flag is evaluated for access decisions on the directory itself
**Why it happens:** INHERIT_ONLY means the ACE only applies to children (via inheritance), not to the object it is on
**How to avoid:** In the evaluation loop, skip any ACE where `flag & ACE4_INHERIT_ONLY_ACE != 0`
**Warning signs:** Directory access granted/denied based on ACEs that should only affect children

### Pitfall 3: Special Identifiers OWNER@/GROUP@/EVERYONE@ Are Dynamic
**What goes wrong:** Storing the resolved UID in place of OWNER@ when the ACL is set, then the file owner changes but the ACE still references the old owner
**Why it happens:** OWNER@ is resolved at evaluation time, not at storage time
**How to avoid:** Store "OWNER@", "GROUP@", "EVERYONE@" literally in the ACE who field. At evaluation time, check if the requestor matches the file's current owner/group.
**Warning signs:** chown changes ownership but ACL no longer grants owner access

### Pitfall 4: chmod Must Adjust ACL, Not Replace It
**What goes wrong:** chmod replaces the entire ACL with a mode-derived ACL, destroying explicit ACEs
**Why it happens:** Not understanding RFC 7530 Section 6.4.1
**How to avoid:** chmod only adjusts OWNER@/GROUP@/EVERYONE@ ACEs to match the new mode bits. All other ACEs (explicit user/group ACEs) are preserved unchanged.
**Warning signs:** Setting mode 755 wipes out all user-specific ACEs

### Pitfall 5: Security Descriptor Alignment Requirements
**What goes wrong:** Windows clients fail to parse the security descriptor
**Why it happens:** SIDs must be at 4-byte aligned offsets; ACL structures have specific padding rules
**How to avoid:** After each SID encoding, pad to 4-byte boundary. Compute offsets carefully in the self-relative Security Descriptor header.
**Warning signs:** Windows Explorer shows blank Security tab or "access denied" for all operations

### Pitfall 6: Identity Cache Stale After User/Group Changes
**What goes wrong:** User is deleted from control plane but cached identity still resolves
**Why it happens:** TTL-based cache does not get invalidated on control plane changes
**How to avoid:** Use a reasonable TTL (recommended: 5 minutes). For the initial implementation, TTL expiry is sufficient. Active invalidation can be added later if needed.
**Warning signs:** Deleted users retain access until cache expires

### Pitfall 7: Empty ACL vs No ACL
**What goes wrong:** An empty ACL (0 ACEs) is treated the same as no ACL (nil)
**Why it happens:** Confusion between "file has an ACL with zero entries" and "file has no ACL"
**How to avoid:** Use `nil` for "no ACL" (use Unix mode bits). Use `&ACL{ACEs: []}` for "has an ACL but it's empty" (which denies all access since nothing is allowed). This is per the decision: "No ACL by default -- derive from umask/mode until explicit ACL is set."
**Warning signs:** Setting an empty ACL allows access instead of denying everything

## Code Examples

### NFSv4 ACE Type Constants (from RFC 7530 Section 6.2.1)
```go
// Source: RFC 7530 Section 6.2.1 / RFC 7531
// pkg/metadata/acl/types.go

// ACE types (acetype4)
const (
    ACE4_ACCESS_ALLOWED_ACE_TYPE = 0x00000000
    ACE4_ACCESS_DENIED_ACE_TYPE  = 0x00000001
    ACE4_SYSTEM_AUDIT_ACE_TYPE   = 0x00000002
    ACE4_SYSTEM_ALARM_ACE_TYPE   = 0x00000003
)

// ACE flags (aceflag4)
const (
    ACE4_FILE_INHERIT_ACE         = 0x00000001
    ACE4_DIRECTORY_INHERIT_ACE    = 0x00000002
    ACE4_NO_PROPAGATE_INHERIT_ACE = 0x00000004
    ACE4_INHERIT_ONLY_ACE         = 0x00000008
    ACE4_SUCCESSFUL_ACCESS_ACE_FLAG = 0x00000010
    ACE4_FAILED_ACCESS_ACE_FLAG     = 0x00000020
    ACE4_INHERITED_ACE            = 0x00000080 // Not in RFC 7530 but widely used
)

// ACE access mask bits (acemask4) - 14 file + 2 directory bits
const (
    ACE4_READ_DATA         = 0x00000001 // Read file data
    ACE4_LIST_DIRECTORY    = 0x00000001 // List directory contents (same bit as READ_DATA)
    ACE4_WRITE_DATA        = 0x00000002 // Write file data
    ACE4_ADD_FILE          = 0x00000002 // Add file to directory (same bit as WRITE_DATA)
    ACE4_APPEND_DATA       = 0x00000004 // Append data to file
    ACE4_ADD_SUBDIRECTORY  = 0x00000004 // Add subdirectory (same bit as APPEND_DATA)
    ACE4_READ_NAMED_ATTRS  = 0x00000008 // Read named attributes
    ACE4_WRITE_NAMED_ATTRS = 0x00000010 // Write named attributes
    ACE4_EXECUTE           = 0x00000020 // Execute file / traverse directory
    ACE4_DELETE_CHILD      = 0x00000040 // Delete child in directory
    ACE4_READ_ATTRIBUTES   = 0x00000080 // Read basic attributes
    ACE4_WRITE_ATTRIBUTES  = 0x00000100 // Write basic attributes
    ACE4_WRITE_RETENTION   = 0x00000200 // Write retention attributes
    ACE4_WRITE_RETENTION_HOLD = 0x00000400 // Write retention hold
    ACE4_DELETE            = 0x00010000 // Delete the object
    ACE4_READ_ACL          = 0x00020000 // Read ACL
    ACE4_WRITE_ACL         = 0x00040000 // Write ACL
    ACE4_WRITE_OWNER       = 0x00080000 // Change owner
    ACE4_SYNCHRONIZE       = 0x00100000 // Synchronize access
)

// ACL support constants for FATTR4_ACLSUPPORT
const (
    ACL4_SUPPORT_ALLOW_ACL = 0x00000001
    ACL4_SUPPORT_DENY_ACL  = 0x00000002
    ACL4_SUPPORT_AUDIT_ACL = 0x00000004
    ACL4_SUPPORT_ALARM_ACL = 0x00000008
)

// FATTR4 bit numbers for ACL attributes
const (
    FATTR4_ACL            = 12 // nfsace4<>: the ACL itself
    FATTR4_ACLSUPPORT     = 13 // uint32: bitmask of supported ACE types
)

// Special identifiers per RFC 7530 Section 6.2.1.5
const (
    SpecialOwner    = "OWNER@"
    SpecialGroup    = "GROUP@"
    SpecialEveryone = "EVERYONE@"
)

// MaxACECount is the maximum number of ACEs per file
const MaxACECount = 128

// ACE represents a single NFSv4 Access Control Entry
type ACE struct {
    Type       uint32 // acetype4
    Flag       uint32 // aceflag4
    AccessMask uint32 // acemask4
    Who        string // utf8str_mixed: "user@domain", "OWNER@", etc.
}

// ACL represents an NFSv4 Access Control List
type ACL struct {
    ACEs []ACE `json:"aces"`
}
```

### NFSv4 to SMB Mask Bit Mapping (Claude's Discretion)
```go
// Source: RFC 7530 Section 6.2.1 + MS-DTYP ACCESS_MASK
// The NFSv4 ACL mask bits are intentionally identical to Windows ACCESS_MASK bits.
// This is by design per RFC 7530's Windows interoperability goal.
//
// NFSv4 acemask4                Windows ACCESS_MASK
// ACE4_READ_DATA    (0x0001) == FILE_READ_DATA    (0x0001)
// ACE4_WRITE_DATA   (0x0002) == FILE_WRITE_DATA   (0x0002)
// ACE4_APPEND_DATA  (0x0004) == FILE_APPEND_DATA  (0x0004)
// ACE4_READ_NAMED_ATTRS (0x0008) == FILE_READ_EA (0x0008)
// ACE4_WRITE_NAMED_ATTRS(0x0010) == FILE_WRITE_EA(0x0010)
// ACE4_EXECUTE      (0x0020) == FILE_EXECUTE      (0x0020)
// ACE4_DELETE_CHILD (0x0040) == FILE_DELETE_CHILD (0x0040)
// ACE4_READ_ATTRIBUTES(0x0080) == FILE_READ_ATTRIBUTES(0x0080)
// ACE4_WRITE_ATTRIBUTES(0x0100) == FILE_WRITE_ATTRIBUTES(0x0100)
// ACE4_DELETE       (0x10000) == DELETE           (0x10000)
// ACE4_READ_ACL     (0x20000) == READ_CONTROL     (0x20000)
// ACE4_WRITE_ACL    (0x40000) == WRITE_DAC        (0x40000)
// ACE4_WRITE_OWNER  (0x80000) == WRITE_OWNER      (0x80000)
// ACE4_SYNCHRONIZE  (0x100000) == SYNCHRONIZE      (0x100000)
//
// Since the bit positions are identical, no translation is needed for mask bits.
// Only the ACE type and principal (who/SID) need translation.
```

### Well-Known SID Mapping Table (Claude's Discretion)
```go
// Source: MS-DTYP Section 2.4.2.4 (Well-Known SID Structures)
var WellKnownSIDs = map[string]string{
    "EVERYONE@":         "S-1-1-0",       // Everyone / World
    "OWNER@":            "",              // Dynamic: resolved from file owner
    "GROUP@":            "",              // Dynamic: resolved from file group
}

// Additional well-known SIDs for group mapping
var WellKnownGroupSIDs = map[string]string{
    "S-1-1-0":           "EVERYONE@",     // Everyone
    "S-1-5-32-544":      "BUILTIN\\Administrators", // -> maps to "admins" group
    "S-1-5-32-545":      "BUILTIN\\Users",           // -> maps to "users" group
    "S-1-5-18":          "NT AUTHORITY\\SYSTEM",      // -> maps to root
    "S-1-5-19":          "NT AUTHORITY\\LOCAL SERVICE",
    "S-1-5-20":          "NT AUTHORITY\\NETWORK SERVICE",
    "S-1-5-7":           "ANONYMOUS LOGON",           // -> nobody
    "S-1-3-0":           "CREATOR OWNER",             // -> OWNER@
    "S-1-3-1":           "CREATOR GROUP",             // -> GROUP@
}
```

### Windows Security Descriptor Binary Format
```go
// Source: MS-DTYP Section 2.4.6 (SECURITY_DESCRIPTOR)
// Self-relative format layout:
//
// Offset  Size  Field
// 0       1     Revision (always 1)
// 1       1     Sbz1 (reserved, 0)
// 2       2     Control (SE_SELF_RELATIVE=0x8000 | SE_DACL_PRESENT=0x0004)
// 4       4     OffsetOwner (offset to Owner SID from start)
// 8       4     OffsetGroup (offset to Group SID from start)
// 12      4     OffsetSacl  (0 = no SACL)
// 16      4     OffsetDacl  (offset to DACL from start)
// 20+     var   Owner SID
//         var   Group SID
//         var   DACL (ACL header + ACE entries)
//
// SID binary format (MS-DTYP Section 2.4.2):
// Offset  Size  Field
// 0       1     Revision (always 1)
// 1       1     SubAuthorityCount
// 2       6     IdentifierAuthority (big-endian)
// 8       4*N   SubAuthority[N] (little-endian uint32 array)
//
// ACL header format (MS-DTYP Section 2.4.5):
// Offset  Size  Field
// 0       1     AclRevision (2 for standard, 4 for object ACEs)
// 1       1     Sbz1
// 2       2     AclSize (total size including all ACEs)
// 4       2     AceCount
// 6       2     Sbz2
//
// ACE format (MS-DTYP Section 2.4.4.2):
// Offset  Size  Field
// 0       1     AceType (0=ALLOW, 1=DENY, 2=AUDIT)
// 1       1     AceFlags (inheritance flags)
// 2       2     AceSize (total size of this ACE)
// 4       4     Mask (ACCESS_MASK)
// 8       var   SID
```

### FileAttr Extension
```go
// Addition to pkg/metadata/file.go FileAttr struct:
type FileAttr struct {
    // ... existing fields ...

    // ACL is the NFSv4 Access Control List for this file.
    // nil means no ACL is set -- use classic Unix permission check.
    // Non-nil with empty ACEs means an explicit empty ACL (denies all access).
    ACL *acl.ACL `json:"acl,omitempty"`
}
```

### Integration Point: Permission Checking
```go
// Modification to pkg/metadata/authentication.go calculatePermissions():
func calculatePermissions(
    file *File,
    identity *Identity,
    shareOpts *ShareOptions,
    requested Permission,
) Permission {
    // If file has an ACL, use ACL evaluation instead of Unix mode bits
    if file.ACL != nil {
        return evaluateACLPermissions(file, identity, requested)
    }

    // No ACL = classic Unix permission check (existing code)
    // ... existing implementation unchanged ...
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Hardcoded "root@localdomain" in OWNER attr | Identity mapper resolves principals | Phase 13 | Proper NFSv4 identity support |
| Minimal security descriptor in SMB QUERY_INFO | Full SD with Owner/Group/DACL | Phase 13 | Windows Explorer Security tab works |
| Unix mode bits only | ACL-first with mode derivation | Phase 13 | Fine-grained access control |
| StaticMapper in pkg/auth/kerberos | Refactored to pkg/identity/ | Phase 13 | Pluggable identity resolution |
| No ACL attributes in NFSv4 GETATTR | FATTR4_ACL + FATTR4_ACLSUPPORT | Phase 13 | NFS clients can manage ACLs |

**Deprecated/outdated:**
- `pkg/auth/kerberos/IdentityMapper` interface: Will be superseded by `pkg/identity/IdentityMapper` but the old interface remains for backward compatibility during the refactor
- Hardcoded owner/group strings in `internal/protocol/nfs/v4/attrs/encode.go` (lines 413-425): Will use identity mapper instead

## Codebase Integration Points

### Files to Modify (Existing)

| File | Change | Purpose |
|------|--------|---------|
| `pkg/metadata/file.go` `FileAttr` struct | Add `ACL *acl.ACL` field | Store ACL as first-class metadata |
| `pkg/metadata/authentication.go` `calculatePermissions()` | Add ACL evaluation branch | ACL overrides Unix mode when present |
| `pkg/metadata/file.go` `SetFileAttributes()` | Handle ACL in SetAttrs | Support setting ACL via SETATTR |
| `pkg/metadata/file.go` `createEntry()` | Compute inherited ACL from parent | ACL inheritance at creation |
| `pkg/metadata/file.go` `SetAttrs` struct | Add ACL field | Allow setting ACL |
| `internal/protocol/nfs/v4/attrs/encode.go` | Add FATTR4_ACL, FATTR4_ACLSUPPORT encoding | NFS clients see ACLs |
| `internal/protocol/nfs/v4/attrs/decode.go` | Add FATTR4_ACL decoding in SETATTR | NFS clients set ACLs |
| `internal/protocol/nfs/v4/attrs/encode.go` SupportedAttrs() | Add bits 12, 13 | Advertise ACL support |
| `internal/protocol/nfs/v4/attrs/encode.go` WritableAttrs() | Add bit 12 | Allow setting ACL via SETATTR |
| `internal/protocol/nfs/v4/attrs/encode.go` encodeRealFileAttr() | Use identity mapper for OWNER/OWNER_GROUP | Proper user@domain resolution |
| `internal/protocol/nfs/v4/attrs/decode.go` ParseOwnerString() | Use identity mapper | Proper principal resolution |
| `internal/protocol/nfs/v4/handlers/access.go` | Consider ACL in access check | ACCESS op reflects ACL permissions |
| `internal/protocol/smb/v2/handlers/query_info.go` `buildSecurityInfo()` | Full SD with Owner/Group/DACL from ACL | Windows Security tab |
| `internal/protocol/smb/v2/handlers/set_info.go` | Parse SD, translate to ACL | Windows sets permissions |
| `pkg/metadata/store/postgres/files.go` | Handle ACL JSONB column | Persist ACL in PostgreSQL |
| `pkg/metadata/store/postgres/migrations/` | New migration for ACL column | Schema update |
| `pkg/metadata/store/memory/` | ACL field already in File struct | Automatic via JSON |
| `pkg/metadata/store/badger/` | ACL field already in File struct | Automatic via JSON encoding |
| `pkg/controlplane/store/interface.go` | Add identity mapping CRUD methods | Mapping table storage |
| `pkg/auth/kerberos/identity.go` | Refactor to delegate to pkg/identity | Backward compat wrapper |

### Files to Create (New)

| File | Purpose |
|------|---------|
| `pkg/metadata/acl/` | ACL types, evaluation, inheritance, validation, mode sync |
| `pkg/identity/` | IdentityMapper interface, ConventionMapper, TableMapper, cache |
| `internal/protocol/nfs/v4/attrs/acl.go` | ACL XDR encoding/decoding |
| `internal/protocol/smb/v2/handlers/security.go` | Security Descriptor encoding |

### PostgreSQL Migration (Claude's Discretion)

Numbering: `000004_acl.up.sql` / `000004_acl.down.sql` (next after existing `000003_clients`)

```sql
-- 000004_acl.up.sql
ALTER TABLE files ADD COLUMN acl JSONB DEFAULT NULL;

-- Create index for files with ACLs (partial index, only non-null)
CREATE INDEX idx_files_has_acl ON files ((acl IS NOT NULL)) WHERE acl IS NOT NULL;

-- Identity mapping table
CREATE TABLE IF NOT EXISTS identity_mappings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    principal VARCHAR(255) NOT NULL,  -- e.g., "alice@EXAMPLE.COM"
    username VARCHAR(255) NOT NULL,   -- control plane username
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(principal)
);

CREATE INDEX idx_identity_mappings_username ON identity_mappings(username);
```

### Recursive Propagation (Claude's Discretion)

**Recommendation: Synchronous with depth limit of 10,000 nodes.**

Rationale:
- Async adds complexity (background workers, progress tracking, partial failure)
- Most real-world directory trees that get ACL changes are not millions deep
- A depth limit of 10,000 prevents runaway recursion
- If more than 10,000 descendants, return an error suggesting the user do it in batches
- This can be made async in a future phase if needed

### Identity Resolution Cache TTL (Claude's Discretion)

**Recommendation: 5 minutes (300 seconds).**

Rationale:
- Short enough to pick up user/group changes relatively quickly
- Long enough to reduce DB load under steady-state operation
- Matches common LDAP caching defaults
- Can be made configurable via server config if needed

## Open Questions

1. **Identity Mapper Placement in Request Path**
   - What we know: The identity mapper needs to be available to both NFS v4 handlers (GETATTR owner encoding) and the ACL evaluation engine (resolving principals)
   - What's unclear: Should it be a field on the Handler struct, passed via CompoundContext, or a global singleton?
   - Recommendation: Add it to the Runtime (pkg/controlplane/runtime) alongside MetadataService. Handlers access it via `h.Registry.GetIdentityMapper()`. This follows the existing pattern where handlers get services through the Runtime.

2. **ACL Field Memory Overhead**
   - What we know: Adding `*acl.ACL` to every FileAttr adds a pointer (8 bytes) per file
   - What's unclear: For files without ACLs (the majority), this is a nil pointer. Is there concern about serialization overhead?
   - Recommendation: Use `json:"acl,omitempty"` so nil ACLs take zero space in serialized form. The 8-byte pointer per in-memory File is negligible.

3. **Control Plane Store for Identity Mappings**
   - What we know: Mapping table needs CRUD operations stored in the control plane DB
   - What's unclear: Whether to extend the existing controlplane Store interface or create a separate identity store
   - Recommendation: Extend the existing controlplane Store interface with `CreateIdentityMapping`, `GetIdentityMapping`, `ListIdentityMappings`, `DeleteIdentityMapping` methods. This follows the pattern of users/groups/shares.

## Sources

### Primary (HIGH confidence)
- **RFC 7530** - https://www.rfc-editor.org/rfc/rfc7530.html - NFSv4 protocol specification, Section 6 (ACLs), Section 5.9 (identity), Section 6.4.1 (mode/ACL sync)
- **RFC 7531** - https://www.rfc-editor.org/rfc/rfc7531.html - NFSv4 XDR definitions including nfsace4 structure
- **MS-DTYP** - https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/7d4dac05-9cef-4563-a058-f108abecce1d - Security Descriptor, SID, ACL binary format
- **MS-DTYP SDDL Examples** - https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/2918391b-75b9-4eeb-83f0-7fdc04a5c6c9 - SD binary format examples
- **Windows File Access Rights** - https://learn.microsoft.com/en-us/windows/win32/fileio/file-access-rights-constants - FILE_READ_DATA etc. constants
- **Codebase analysis** - Direct reading of DittoFS source code for all integration points

### Secondary (MEDIUM confidence)
- **Samba NFS4 ACL Overview** - https://wiki.samba.org/index.php/NFS4_ACL_overview - Confirms NFSv4 ACLs are "Windows ACLs" with same bit positions
- **IETF ACL Mapping Draft** - https://datatracker.ietf.org/doc/html/draft-ietf-nfsv4-acl-mapping-05 - NFSv4 to POSIX ACL mapping considerations
- **Azure NetApp Files NFSv4 ACLs** - https://learn.microsoft.com/en-us/azure/azure-netapp-files/nfs-access-control-lists - Real-world NFSv4 ACL implementation reference

### Tertiary (LOW confidence)
- None. All findings verified against RFC and official Microsoft documentation.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - No new dependencies, using RFC-defined algorithms
- Architecture: HIGH - Clear integration points in existing codebase, well-defined patterns
- ACL model: HIGH - RFC 7530 Section 6 is prescriptive and unambiguous
- SMB interop: HIGH - MS-DTYP is authoritative, NFSv4 mask bits intentionally match Windows
- Identity mapping: HIGH - Convention-based strategy is straightforward, control plane already has user/group CRUD
- Pitfalls: HIGH - Well-documented in RFC and community (Samba, Linux NFS)

**Research date:** 2026-02-16
**Valid until:** 2026-04-16 (RFC-based, very stable; MS-DTYP is versioned but core SD format is frozen)

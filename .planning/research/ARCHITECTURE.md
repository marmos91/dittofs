# Architecture Patterns: SMB2 Conformance and Windows Compatibility (v3.6)

**Domain:** SMB2 protocol conformance, NT Security Descriptors, Windows 11 client compatibility
**Researched:** 2026-02-26
**Confidence:** HIGH (based on existing codebase analysis, MS-SMB2/MS-DTYP specs, Samba reference implementation)

## Recommended Architecture

All v3.6 changes are extensions of existing modules. No new packages, interfaces, or architectural patterns are needed. The changes touch three layers:

```
┌─────────────────────────────────────────────────────────┐
│                  SMB2 Handler Layer                      │
│         internal/adapter/smb/v2/handlers/               │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ security.go  │  │  read.go     │  │  create.go   │  │
│  │ SD building  │  │ sparse zero  │  │ context enc  │  │
│  │ SID mapping  │  │ fill fix     │  │ MxAc/QFid    │  │
│  │ DACL synth   │  │              │  │              │  │
│  └──────┬───────┘  └──────┬───────┘  └──────────────┘  │
│         │                 │                              │
│  ┌──────┴───────┐  ┌──────┴───────────────────┐        │
│  │ query_info.go│  │ set_info.go               │        │
│  │ new info     │  │ SD set, rename fix        │        │
│  │ classes      │  │                           │        │
│  └──────┬───────┘  └──────┬───────────────────┘        │
└─────────┼─────────────────┼─────────────────────────────┘
          │                 │
          ▼                 ▼
┌─────────────────────────────────────────────────────────┐
│                  Metadata Layer                          │
│              pkg/metadata/                               │
│                                                         │
│  ┌──────────────────────┐  ┌─────────────────────────┐  │
│  │ file_modify.go       │  │ acl/ package            │  │
│  │ Move: update child   │  │ types.go: unchanged     │  │
│  │ paths recursively    │  │ validate.go: unchanged  │  │
│  └──────────────────────┘  │ inherit.go: may extend  │  │
│                            └─────────────────────────┘  │
│  ┌──────────────────────────────────────────────────┐   │
│  │ store/ implementations (memory, badger, postgres) │   │
│  │ Rename: must propagate path updates to children   │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
          │
          ▼
┌─────────────────────────────────────────────────────────┐
│                  Payload Layer                           │
│              pkg/payload/                                │
│                                                         │
│  ┌──────────────────────────────────────────────────┐   │
│  │ io/read.go                                        │   │
│  │ ReadAt: handle "block not found" as zero bytes    │   │
│  │ (not as error) when within file size bounds       │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### Component Boundaries

| Component | Responsibility | Changes in v3.6 |
|-----------|---------------|-----------------|
| `handlers/security.go` | NT Security Descriptor encoding, SID mapping, DACL building | Extend: POSIX-to-DACL synthesis, well-known SIDs, SACL stub, ACE flag translation fix |
| `handlers/read.go` | SMB2 READ command handling | Fix: zero-fill for sparse file reads |
| `handlers/create.go` | SMB2 CREATE command handling | Fix: create context wire encoding, MxAc/QFid responses |
| `handlers/query_info.go` | SMB2 QUERY_INFO dispatch | Extend: FileCompressionInformation, FileAttributeTagInformation, FilePositionInformation |
| `handlers/set_info.go` | SMB2 SET_INFO dispatch | Existing: setSecurityInfo already works; no changes needed |
| `pkg/metadata/file_modify.go` | Move/rename operations | Fix: update `srcFile.Path` before PutFile in Move transaction |
| `pkg/metadata/store/*` | Metadata store implementations | Fix: Rename must update descendant paths |
| `pkg/payload/io/read.go` | Cache-aware payload reads | Fix: treat missing blocks as zeros when offset < file size |
| `signing/signing.go` | SMB2 message signing | Validate: ensure signing is active for all authenticated sessions |

### Data Flow

#### Security Descriptor Query (QUERY_INFO InfoType=3)

```
1. Client sends QUERY_INFO with InfoType=SMB2_0_INFO_SECURITY
2. Handler extracts AdditionalInfo bitmask (Owner|Group|DACL|SACL)
3. Handler calls metaSvc.GetFile() to get metadata.File
4. Handler calls BuildSecurityDescriptor(file, additionalSecInfo)
5. BuildSecurityDescriptor:
   a. If file has ACL: translate NFSv4 ACEs to Windows ACEs
   b. If file has no ACL: synthesize DACL from UID/GID/mode (NEW)
   c. Build owner SID from UID
   d. Build group SID from GID
   e. Assemble self-relative SD binary
6. Handler returns SD in QueryInfoResponse.Data
```

#### Sparse File READ (fixed flow)

```
1. Client sends READ at offset X for length L
2. Handler validates handle, checks locks
3. Handler calls payloadSvc.ReadAt(ctx, payloadID, data, offset)
4. ReadAt iterates over block ranges:
   a. Try cache -> found: fill buffer
   b. Try block store via EnsureAvailable -> found: fill buffer
   c. NOT FOUND (new): zero-fill destination slice for this range (NEW)
5. Handler returns zero-filled response for unwritten ranges
```

#### Renamed Directory Path Update (fixed flow)

```
1. Client sends SET_INFO FileRenameInformation for directory /old -> /new
2. Handler calls metaSvc.Move(authCtx, parentHandle, "old", parentHandle, "new")
3. Move implementation:
   a. Validates permissions, sticky bit, type compatibility
   b. Updates srcFile.Path = buildPath(dstDir.Path, toName) (NEW)
   c. Calls tx.PutFile(ctx, srcFile) with updated path
   d. Updates child index entries
4. Subsequent QUERY_DIRECTORY on /new returns correct paths
```

## Detailed Analysis: Bug #180 -- Sparse File READ

**Root Cause:** `pkg/payload/io/read.go` line ~214-228 in `ensureAndReadFromCache()`. When a file has "holes" (regions never written), the block does not exist in either the cache or the block store. `EnsureAvailable()` attempts to download the block, fails because it was never uploaded, and returns an error. This error propagates up as a hard read failure.

**Why this matters for Windows:** Windows 11 Explorer and many Windows applications create files via `CREATE` + `SET_END_OF_FILE` (which sets the file size) and then write data at various offsets. The regions between writes are "sparse holes" that should return zeros per POSIX and MS-FSCC semantics. Windows clients expect reads of these regions to succeed.

**Fix location:** `pkg/payload/io/read.go` in `ensureAndReadFromCache()`:

```go
// In ensureAndReadFromCache, after EnsureAvailable fails:
func (s *ServiceImpl) ensureAndReadFromCache(ctx context.Context, payloadID string,
    blockRange chunk.BlockRange, chunkOffset uint32, dest []byte) error {

    err := s.blockDownloader.EnsureAvailable(ctx, payloadID,
        blockRange.ChunkIndex, chunkOffset, blockRange.Length)
    if err != nil {
        // NEW: Unwritten blocks return zeros (sparse file semantics)
        if isBlockNotFoundError(err) {
            clear(dest) // Zero-fill the destination slice
            return nil
        }
        return fmt.Errorf("ensure available for block %d/%d failed: %w",
            blockRange.ChunkIndex, blockRange.BlockIndex, err)
    }

    // Read from cache (now populated)
    found, err := s.cacheReader.ReadAt(ctx, payloadID,
        blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
    if err != nil || !found {
        return fmt.Errorf("data not in cache after download for block %d/%d",
            blockRange.ChunkIndex, blockRange.BlockIndex)
    }
    return nil
}
```

**Sentinel error needed:** Add `ErrBlockNotFound` to `pkg/payload/store/store.go` (or equivalent) so that `EnsureAvailable` can wrap its "not found" condition with a distinguishable error. The `isBlockNotFoundError()` helper uses `errors.Is()`.

**Also needed:** The same zero-fill logic should apply in the COW path (`readFromCOWSource`) when the COW source block also doesn't exist.

**Confidence:** HIGH -- direct codebase analysis of `read.go` lines 214-228 and understanding of the chunk/block storage model.

## Detailed Analysis: Bug #181 -- Renamed Directory Listing

**Root Cause:** `pkg/metadata/file_modify.go` in the `Move()` method (around line 500 of the transaction block). The code updates child index entries and parent references but does NOT update `srcFile.Path` before calling `tx.PutFile(ctx, srcFile)`. The file's `Path` field retains the old value (e.g., `/old`) even though the directory has been moved to `/new`.

**Impact:** After renaming `/old` to `/new`:
- `ReadDirectory("/new")` returns entries correctly (child index is updated)
- BUT `GetFile(childHandle)` for any child still shows `Path: "/old/child"` instead of `"/new/child"`
- Windows Explorer shows stale paths, confusing file operations
- SMB QUERY_DIRECTORY returns wrong FileNameInformation for deeply nested entries

**Fix location:** `pkg/metadata/file_modify.go` in the Move transaction:

```go
// Around line 500, before PutFile:
srcFile.Path = buildPath(dstDir.Path, toName)  // NEW: update path
srcFile.Ctime = now
_ = tx.PutFile(ctx.Context, srcFile)
```

**Recursive path update:** For directory renames, ALL descendant files also need their Path updated. This is the deeper fix. Two approaches:

1. **Eager update (recommended for v3.6):** Walk all descendants in the transaction and update each Path. This is correct and simple. The tree depth is bounded by MaxPathDepth.

2. **Lazy derivation (future optimization):** Don't store full Path; derive it by walking parent pointers. More complex but avoids the O(n) update cost on rename. Not needed for v3.6.

For the eager approach, add a helper in the store implementation:

```go
// In metadata store implementations:
func (s *Store) updateDescendantPaths(ctx context.Context, tx Transaction,
    dir *File, oldPrefix, newPrefix string) error {
    children, _ := tx.ListChildren(ctx, dir.Handle)
    for _, child := range children {
        childFile, _ := tx.GetFile(ctx, child.Handle)
        childFile.Path = strings.Replace(childFile.Path, oldPrefix, newPrefix, 1)
        tx.PutFile(ctx, childFile)
        if childFile.Type == FileTypeDirectory {
            s.updateDescendantPaths(ctx, tx, childFile, oldPrefix, newPrefix)
        }
    }
    return nil
}
```

**Confidence:** HIGH -- direct line-level analysis of file_modify.go Move() method confirms the missing Path update.

## Detailed Analysis: #182 -- NT Security Descriptors

The existing `security.go` (626 lines) already provides a substantial implementation. Five areas need enhancement:

### Enhancement 1: Machine SID

**Current:** `makeDittoFSUserSID(uid)` creates `S-1-5-21-0-0-0-{uid}`. The three sub-authority values (0-0-0) are intended to represent the "machine SID" but using all zeros is non-standard and some Windows tools may flag it as suspicious.

**Fix:** Generate a stable machine SID at server startup (hash of server hostname or a configured value) and store it in the Runtime or as a configuration option. Use `S-1-5-21-{hash1}-{hash2}-{hash3}-{uid}`.

```go
// Compute once at startup:
h := sha256.Sum256([]byte(hostname))
machineSID := SID{
    Revision: 1,
    SubAuthority: []uint32{
        21,
        binary.LittleEndian.Uint32(h[0:4]),
        binary.LittleEndian.Uint32(h[4:8]),
        binary.LittleEndian.Uint32(h[8:12]),
    },
}
// Per-user SID = machineSID + uid as final sub-authority
```

### Enhancement 2: SID Mapper Location

**Question from milestone:** "Where should SID mapping logic live?"

**Answer: Keep it in `internal/adapter/smb/v2/handlers/security.go`.**

Rationale:
- SID mapping is SMB-specific. NFS uses UID/GID directly; there is no SID concept in NFS.
- The existing `PrincipalToSID()` and `SIDToPrincipal()` already live in security.go and work correctly.
- Moving to pkg/auth/ or pkg/controlplane/ would create an SMB dependency in shared packages.
- The control plane `IdentityMappingStore` handles NFS-to-Unix identity mapping, not SID mapping. SIDs are a presentation-layer concern for the SMB protocol.

If the SID mapper grows beyond ~100 lines of mapping logic, extract to `internal/adapter/smb/v2/handlers/sid_mapper.go` as a new file in the same package. Do NOT create a new package or interface.

### Enhancement 3: DACL Canonical Ordering

**Current:** `buildDACL()` translates NFSv4 ACEs to Windows ACEs in the order they appear. This is correct when the NFSv4 ACL already has canonical ordering (which `pkg/metadata/acl/validate.go` enforces).

**Issue:** When synthesizing DACLs from POSIX mode bits (the "no ACL" fallback), the generated ACEs must follow Windows canonical order: explicit deny before explicit allow, then inherited deny before inherited allow (MS-DTYP 2.4.4.1).

**Fix:** The `buildDACLFromMode()` function must emit deny ACEs before allow ACEs. The pattern from Samba's `create_synthetic_acl()`:

```
1. Deny ACEs for group (what group lacks that owner has)
2. Deny ACEs for others (what others lack that group has)
3. Allow ACE for owner
4. Allow ACE for group
5. Allow ACE for others (Everyone)
```

### Enhancement 4: ACE Flag Translation Bug

**Current bug:** In `buildDACL()`, ACE flags are passed through with `uint8(ace.Flag & 0xFF)`. This is incorrect because NFSv4 and Windows use different bit positions for some flags:

| Flag | NFSv4 (RFC 7530) | Windows (MS-DTYP) |
|------|-------------------|-------------------|
| FILE_INHERIT_ACE | 0x01 | 0x01 |
| DIRECTORY_INHERIT_ACE | 0x02 | 0x02 |
| NO_PROPAGATE_INHERIT_ACE | 0x04 | 0x04 |
| INHERIT_ONLY_ACE | 0x08 | 0x08 |
| SUCCESSFUL_ACCESS_ACE_FLAG | 0x10 | 0x40 |
| FAILED_ACCESS_ACE_FLAG | 0x20 | 0x80 |
| INHERITED_ACE | 0x80 | 0x10 |

**Critical:** `INHERITED_ACE` is 0x80 in NFSv4 but 0x10 in Windows. The current passthrough `ace.Flag & 0xFF` sends 0x80 to Windows clients, which interprets it as `SUCCESSFUL_ACCESS_ACE_FLAG` instead.

**Fix:** Add an explicit flag translation function:

```go
func nfsv4FlagsToWindowsFlags(nfsFlags uint32) uint8 {
    var winFlags uint8
    if nfsFlags&acl.ACE4_FILE_INHERIT_ACE != 0     { winFlags |= 0x01 }
    if nfsFlags&acl.ACE4_DIRECTORY_INHERIT_ACE != 0 { winFlags |= 0x02 }
    if nfsFlags&acl.ACE4_NO_PROPAGATE_INHERIT != 0  { winFlags |= 0x04 }
    if nfsFlags&acl.ACE4_INHERIT_ONLY_ACE != 0      { winFlags |= 0x08 }
    if nfsFlags&acl.ACE4_INHERITED_ACE != 0         { winFlags |= 0x10 } // 0x80 -> 0x10
    return winFlags
}
```

### Enhancement 5: Default DACL for Files Without ACLs

**Current:** When `file.ACL == nil`, `buildDACL()` creates a single `Everyone: GENERIC_ALL` ACE. This is overly permissive and causes Windows Security tab to show "Everyone has full control" which alarms users and fails conformance tests that verify proper DACL structure.

**Fix:** Synthesize a DACL from POSIX mode bits (Pattern 1 above). This produces a proper deny-before-allow structure that:
- Maps correctly to the Windows Security tab display
- Allows Windows clients to understand effective permissions
- Passes WPTS BVT tests that verify DACL structure

## Conformance Fix Strategy

**Question from milestone:** "What's the right approach to fixing conformance failures -- patch individual handlers vs. create shared infrastructure?"

**Answer: Patch individual handlers.** Here is the analysis:

### Why NOT shared infrastructure

The 56 known BVT failures (from KNOWN_FAILURES.md) fall into these categories:

| Category | Count | Root Cause | Fix Approach |
|----------|-------|------------|--------------|
| Negotiate (SMB 3.x) | 16 | Protocol version not implemented | Phase 39 (SMB3), not v3.6 |
| Encryption | 7 | SMB 3.1.1 encryption not implemented | Phase 39, not v3.6 |
| DFS | 7 | DFS referrals not implemented | Future phase |
| SWN | 6 | Service Witness Protocol not implemented | Future phase |
| VSS | 4 | Volume Shadow Copy not implemented | Future phase |
| Negotiate (SMB 2.x) | 4 | Edge cases in negotiate/signing | Individual handler fixes |
| Signing | 1 | Signing not fully conformant | Individual handler fix |
| Leasing | 1 | Directory leasing not supported | Future phase |
| Compression | 2 | SMB 3.1.1 compression not supported | Future phase |

**Only 5 failures are fixable in v3.6** (the SMB 2.x negotiate edge cases and signing). These are independent handler-level issues, not symptoms of missing infrastructure.

### When shared infrastructure IS warranted

Two areas do warrant small shared improvements:

1. **Error mapping table:** The converters.go `MetadataErrorToSMBStatus` map should be reviewed for completeness. Missing mappings cause STATUS_INTERNAL_ERROR instead of the correct NT status code. This is a lookup table enhancement, not new infrastructure.

2. **Buffer size validation:** Several QUERY_INFO handlers need to check `OutputBufferLength` and return STATUS_BUFFER_OVERFLOW (with partial data) when the buffer is too small. This is a common pattern that could use a small helper:

```go
func validateOutputBuffer(requested uint32, available int) (uint32, error) {
    if int(requested) < available {
        return requested, ErrBufferOverflow
    }
    return uint32(available), nil
}
```

### Fix prioritization for v3.6

1. **Bug #180 (sparse READ)** -- Fix in payload/io/read.go. Unblocks Windows file operations.
2. **Bug #181 (rename paths)** -- Fix in metadata/file_modify.go. Unblocks directory operations.
3. **Bug #182 (Security Descriptors)** -- Enhance security.go. Improves Windows Explorer experience.
4. **Signing conformance** -- Verify signing logic in negotiation. May move failures from KNOWN to PASS.
5. **Negotiate edge cases** -- Fix SMB 2.0.2 and 2.1 negotiate responses.

## Build Order and Dependencies

```
Phase A: Bug Fixes (parallel, no dependencies between them)
├── #180: Sparse READ (payload/io/read.go)
└── #181: Rename Paths (metadata/file_modify.go)

Phase B: Security Descriptors (depends on nothing from Phase A)
├── Machine SID generation (security.go)
├── ACE flag translation fix (security.go)
├── POSIX-to-DACL synthesis (security.go)
└── Default DACL improvement (security.go)

Phase C: Conformance Improvements (depends on A and B)
├── Run WPTS BVT after fixes
├── Fix newly-revealed failures
├── Update KNOWN_FAILURES.md
└── Verify SMB 2.x negotiate edge cases
```

**Phase A and B can run in parallel.** Phase C depends on both because conformance tests validate all fixes together.

## New Components Summary

| What | Where | Type | LOC Estimate |
|------|-------|------|-------------|
| `isBlockNotFoundError()` | `pkg/payload/io/read.go` | Helper function | ~10 |
| `ErrBlockNotFound` | `pkg/payload/store/store.go` | Sentinel error | ~5 |
| Zero-fill in `ensureAndReadFromCache` | `pkg/payload/io/read.go` | Bug fix | ~15 |
| Zero-fill in `readFromCOWSource` | `pkg/payload/io/read.go` | Bug fix | ~10 |
| Path update in `Move()` | `pkg/metadata/file_modify.go` | Bug fix | ~5 |
| `updateDescendantPaths()` | `pkg/metadata/store/*` | Helper function | ~30 per store |
| `nfsv4FlagsToWindowsFlags()` | `handlers/security.go` | Translation function | ~15 |
| `buildDACLFromMode()` | `handlers/security.go` | DACL synthesis | ~60 |
| `modeToAccessMask()` | `handlers/security.go` | Helper function | ~20 |
| Machine SID generation | `handlers/security.go` | Startup logic | ~20 |
| `validateOutputBuffer()` | `handlers/helpers.go` or inline | Shared helper | ~10 |

**Total new code estimate: ~200-250 lines across 4-5 files.** This is a targeted fix milestone, not a large feature addition.

## Patterns to Follow

### Pattern 1: POSIX-to-DACL Synthesis

**What:** When a file has no explicit ACL, synthesize a Windows DACL from Unix mode bits (rwxrwxrwx).

**When:** `file.ACL == nil` in BuildSecurityDescriptor / buildDACL.

**Reference:** Samba's `create_synthetic_acl()` in `smbd/posix_acls.c`.

**Mapping:**

```
Unix mode  -> Windows ACEs (deny-before-allow order)
---------     -----------
Owner rwx  -> ALLOW OWNER_SID: READ|WRITE|EXECUTE|DELETE|READ_ACL|WRITE_ACL
Group r-x  -> DENY  GROUP_SID: WRITE_DATA|APPEND_DATA|DELETE
               ALLOW GROUP_SID: READ|EXECUTE|READ_ACL
Other r--  -> DENY  EVERYONE:  EXECUTE|WRITE_DATA|APPEND_DATA|DELETE
               ALLOW EVERYONE:  READ|READ_ACL
```

**Example:**

```go
// In security.go, replace the "no ACL" fallback in buildDACL:
func buildDACLFromMode(buf *bytes.Buffer, file *metadata.File) {
    mode := file.Mode
    ownerSID := makeDittoFSUserSID(file.UID)
    groupSID := makeDittoFSGroupSID(file.GID)

    var aces []windowsACE

    // Owner permissions
    ownerAllow := modeToAccessMask((mode >> 6) & 0x7)
    aces = append(aces, windowsACE{
        aceType:    accessAllowedACEType,
        aceFlags:   0,
        accessMask: ownerAllow | 0x000E0000, // + DELETE|READ_ACL|WRITE_ACL
        sid:        ownerSID,
    })

    // Group permissions (deny what group lacks that owner has)
    groupAllow := modeToAccessMask((mode >> 3) & 0x7)
    groupDeny := ownerAllow &^ groupAllow
    if groupDeny != 0 {
        aces = insertDeny(aces, groupSID, groupDeny)
    }
    aces = append(aces, windowsACE{
        aceType:    accessAllowedACEType,
        aceFlags:   0,
        accessMask: groupAllow | ACE4_READ_ACL,
        sid:        groupSID,
    })

    // Other permissions
    otherAllow := modeToAccessMask(mode & 0x7)
    // ... similar deny + allow pattern with sidEveryone
}
```

### Pattern 2: Zero-Fill for Unwritten Blocks

**What:** When ReadAt encounters a block that was never written (not in cache, not in block store), fill the destination buffer with zeros instead of returning an error.

**When:** `EnsureAvailable` returns "not found" for a block index that is within the file's declared size.

**Implementation Strategy:**

```go
// In pkg/payload/io/read.go, ensureAndReadFromCache:
func (s *ServiceImpl) ensureAndReadFromCache(ctx context.Context, payloadID string,
    blockRange chunk.BlockRange, chunkOffset uint32, dest []byte) error {

    err := s.blockDownloader.EnsureAvailable(ctx, payloadID,
        blockRange.ChunkIndex, chunkOffset, blockRange.Length)
    if err != nil {
        // NEW: Unwritten blocks return zeros (sparse file semantics)
        if isBlockNotFoundError(err) {
            clear(dest)
            return nil
        }
        return fmt.Errorf("ensure available for block %d/%d failed: %w",
            blockRange.ChunkIndex, blockRange.BlockIndex, err)
    }

    found, err := s.cacheReader.ReadAt(ctx, payloadID,
        blockRange.ChunkIndex, chunkOffset, blockRange.Length, dest)
    if err != nil || !found {
        return fmt.Errorf("data not in cache after download for block %d/%d",
            blockRange.ChunkIndex, blockRange.BlockIndex)
    }
    return nil
}
```

### Pattern 3: Create Context Chain Encoding

**What:** Properly serialize create contexts (MxAc, QFid, RqLs) in CREATE response wire format.

**When:** CREATE response needs to include one or more create contexts.

**Reference:** MS-SMB2 2.2.14.2 -- create contexts are a linked list of variable-length structures.

```go
// Each context: NextOffset(4) + NameOffset(2) + NameLength(2) +
//               Reserved(2) + DataOffset(2) + DataLength(4) +
//               Name(padded) + Data(padded)
// NextOffset = 0 for last context, offset to next for others
// All structures must be 8-byte aligned
```

## Anti-Patterns to Avoid

### Anti-Pattern 1: Implementing SDDL String Format

**What:** Adding SDDL string format conversion (e.g., "O:SYG:SYD:(A;;GA;;;SY)")

**Why bad:** No SMB client sends or receives SDDL strings. The wire format is always binary self-relative Security Descriptors. SDDL is a display format used by Windows admin tools locally. Adding it would increase code surface for zero benefit.

**Instead:** Keep using binary encoding only in security.go.

### Anti-Pattern 2: Global Well-Known SID Lookup Table

**What:** Building a large map of all ~100+ Windows well-known SIDs with their names.

**Why bad:** DittoFS does not implement LSARPC (the protocol Windows uses to resolve SID-to-name). Windows clients resolve SID names themselves using their local security database or domain controller. DittoFS only needs to emit the correct SID binary in the Security Descriptor.

**Instead:** Keep a minimal table of SIDs that appear in synthesized DACLs (Everyone, SYSTEM, CREATOR OWNER, CREATOR GROUP, BUILTIN\Administrators). Do not try to map arbitrary SID strings to names.

### Anti-Pattern 3: Recursive Path Update in Handler Layer

**What:** Updating child paths in the SET_INFO rename handler by walking the directory tree from the SMB handler.

**Why bad:** The SMB handler should call `metaSvc.Move()` and trust that the metadata layer handles path consistency. Having path logic in the handler layer violates separation of concerns and would duplicate the path update logic for NFS rename operations.

**Instead:** Fix the Move operation in `pkg/metadata/file_modify.go` (or in the store implementations) to recursively update child paths. Both NFS RENAME and SMB SET_INFO rename call Move, so the fix benefits both protocols.

### Anti-Pattern 4: Blocking on Block Store for Sparse Reads

**What:** Making ReadAt wait for a block download that will never complete because the block was never written.

**Why bad:** The block store does not have the block. The offloader/download path will retry and eventually timeout or error. This turns a simple sparse read into a multi-second hang.

**Instead:** The EnsureAvailable call should detect "block never written" (distinct from "block exists but not cached") and return quickly with a sentinel error. The caller zero-fills and continues.

### Anti-Pattern 5: Moving SID Mapping to the Control Plane

**What:** Creating SID-to-UID/GID mapping tables in the control plane store, similar to IdentityMappingStore for NFS.

**Why bad:** SIDs are a presentation concern of the SMB protocol adapter. The existing UID/GID are the canonical identifiers in the metadata layer. Adding SID columns to the control plane would couple the shared persistence layer to SMB-specific concepts. NFS, WebDAV, or future protocols have no use for SIDs.

**Instead:** Keep SID generation as a pure function of UID/GID + machine SID in the SMB handler layer. The mapping is deterministic (no table needed) and reversible.

## Scalability Considerations

| Concern | At 100 users | At 10K users | At 1M users |
|---------|--------------|--------------|-------------|
| SID generation | Hash-based, negligible cost | Same | Same |
| SD building | Per-request allocation, ~200 bytes typical | Consider buffer pool for SD encoding | Buffer pool mandatory |
| Sparse zero-fill | Allocate zero buffer per read | Use shared zero page (read-only) | Use shared zero page |
| Path updates on rename | Walk children in-memory, fast | Walk children in BadgerDB, indexed | Need async or batched update |
| ACL validation | Per-SET_INFO, ~128 ACEs max | Same | Same |

## Sources

- [MS-DTYP 2.4.6 - SECURITY_DESCRIPTOR](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/7d4dac05-9cef-4563-a058-f108abecce1d) -- Self-relative SD layout
- [MS-DTYP 2.4.4.1 - ACL canonical ordering](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/20233ed8-a6c6-4097-aafa-dd545ed24428) -- Deny before allow ordering
- [MS-SMB2 2.2.14.2 - CREATE Response Contexts](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/893bff02-5815-4bc1-9693-669ed6e85307) -- Context chain encoding
- [MS-FSCC - Sparse file semantics](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/6a884fe5-3da1-4abb-84c4-f419d349d878) -- Zero-fill for unallocated ranges
- [Samba smb2/acls.c](https://github.com/samba-team/samba/blob/master/source4/torture/smb2/acls.c) -- ACL test patterns: creator_sid, generic_bits, owner_bits, inheritance
- DittoFS codebase: `internal/adapter/smb/v2/handlers/security.go` (626 lines, complete SD implementation)
- DittoFS codebase: `pkg/metadata/acl/` (validate.go enforces canonical ACE ordering)
- DittoFS codebase: `pkg/payload/io/read.go` (260 lines, ReadAt with cache/offloader flow)
- DittoFS codebase: `pkg/metadata/file_modify.go` (548 lines, Move/rename with transaction)

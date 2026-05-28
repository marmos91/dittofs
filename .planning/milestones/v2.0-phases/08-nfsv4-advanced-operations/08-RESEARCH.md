# Phase 8: NFSv4 Advanced Operations - Research

**Researched:** 2026-02-13
**Domain:** NFSv4 advanced operations (LINK, RENAME, SETATTR, VERIFY/NVERIFY, SECINFO upgrade, OPENATTR/OPEN_DOWNGRADE/RELEASE_LOCKOWNER stubs per RFC 7530)
**Confidence:** HIGH

## Summary

Phase 8 completes the NFSv4.0 operation set by implementing the remaining filesystem manipulation and conditional operations. Phases 6-7 built the COMPOUND dispatcher, pseudo-fs navigation, real-FS CRUD (CREATE, REMOVE, OPEN, CLOSE, READ, WRITE, COMMIT), and basic SECINFO. Phase 8 adds six substantive operations (LINK, RENAME, SETATTR, VERIFY, NVERIFY, SECINFO upgrade) plus three stubs (OPENATTR, OPEN_DOWNGRADE, RELEASE_LOCKOWNER).

The critical architectural requirement is the **fattr4 decode infrastructure**. Currently the codebase has `EncodeRealFileAttrs()` and `EncodePseudoFSAttrs()` for encoding attribute values (GETATTR responses), and `skipFattr4()` for skipping attribute data. But SETATTR needs to **decode** attribute values from an fattr4 structure into `metadata.SetAttrs`, and VERIFY/NVERIFY need to **encode then compare** server-side attributes against client-provided fattr4 data. This is the most significant new infrastructure this phase requires.

LINK and RENAME use the **two-filehandle pattern** (SavedFH + CurrentFH) which is already supported by the SAVEFH/RESTOREFH operations from Phase 6. The existing `MetadataService.CreateHardLink()` and `MetadataService.Move()` methods handle all business logic -- the v4 handlers just need to wire up the XDR encoding/decoding and error mapping.

The most nuanced operation is SETATTR, which requires: (1) decoding the stateid4 (for size changes), (2) decoding the fattr4 bitmap+values into a `SetAttrs` struct, (3) supporting both `SET_TO_SERVER_TIME` and `SET_TO_CLIENT_TIME` for timestamps via the `time_how4` enum, (4) implementing the sattrguard4 ctime guard, and (5) returning an `attrsset` bitmap listing which attributes were actually set.

**Primary recommendation:** Organize into three plans: (1) LINK + RENAME (two-filehandle operations sharing patterns), (2) SETATTR + fattr4 decode infrastructure (largest single piece), (3) VERIFY/NVERIFY + SECINFO upgrade + stubs (conditional checks and cleanup). This gives natural dependency flow and keeps each plan focused.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `pkg/metadata` (MetadataService) | existing | `CreateHardLink`, `Move`, `SetFileAttributes` | All business logic already implemented in service layer |
| `internal/protocol/nfs/v4/types` | Phase 6 | NFSv4 constants, error mapping, CompoundContext | Foundation from Phase 6, all needed error codes exist |
| `internal/protocol/nfs/v4/attrs` | Phase 6-7 | Bitmap4 helpers, attribute encoding | Needs new decode path for SETATTR/VERIFY |
| `internal/protocol/nfs/v4/pseudofs` | Phase 6 | Pseudo-fs handle detection | Needed for ROFS checks and SECINFO |
| `internal/protocol/xdr` | existing | XDR encode/decode primitives | Shared across v3/v4 |
| Go stdlib `bytes`, `io`, `time`, `fmt` | N/A | Buffer management, time handling | Standard Go patterns |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/protocol/nfs/v3/handlers` | existing | Reference patterns for link/rename/setattr | Auth context building, error mapping |
| `pkg/controlplane/runtime` | existing | `GetMetadataService`, `GetShare`, identity mapping | Auth context and share config for SECINFO |
| `pkg/metadata/store/memory` | existing | In-memory store for tests | All tests use real stores, no mocks |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Full fattr4 decode (each attr individually) | Byte-exact XDR comparison for VERIFY/NVERIFY | Byte-exact is simpler but fragile -- different XDR encoding of same values would fail comparison; per-attribute decode is more correct and reusable for SETATTR |
| All-or-nothing SETATTR | Best-effort (set what we can, report partial via attrsset) | Metadata layer uses transactions for memory store, so all-or-nothing is natural. Best-effort adds complexity for minimal gain. Recommend all-or-nothing with transaction support |
| Stateid validation in SETATTR | Accept any stateid (Phase 9 adds real validation) | Consistent with Phase 7 OPEN/READ/WRITE approach -- accept special/any stateids now |

**No new external dependencies required.** All packages already exist in the codebase.

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/
├── handlers/
│   ├── handler.go          # MODIFY: register 6 new ops in dispatch table
│   ├── helpers.go           # EXISTING: buildV4AuthContext, encodeChangeInfo4
│   │
│   │  # New operation handlers (one file per operation):
│   ├── link.go              # NEW: LINK handler (two-filehandle pattern)
│   ├── rename.go            # NEW: RENAME handler (two-filehandle pattern)
│   ├── setattr.go           # NEW: SETATTR handler (fattr4 decode + stateid)
│   ├── verify.go            # NEW: VERIFY handler (conditional attribute check)
│   ├── nverify.go           # NEW: NVERIFY handler (negative conditional check)
│   ├── secinfo.go           # MODIFY: upgrade from stub to per-share config
│   ├── stubs.go             # NEW: OPENATTR, OPEN_DOWNGRADE, RELEASE_LOCKOWNER stubs
│   │
│   │  # New test files:
│   ├── link_rename_test.go  # NEW: LINK and RENAME tests
│   ├── setattr_test.go      # NEW: SETATTR tests
│   ├── verify_test.go       # NEW: VERIFY/NVERIFY tests
│   ├── stubs_test.go        # NEW: stub operation tests
│   │
│   │  # Existing files (no changes needed):
│   ├── compound.go          # ProcessCompound dispatcher (already handles unknown ops)
│   ├── context.go           # ExtractV4HandlerContext
│   ├── savefh.go            # SAVEFH (verify works with real-FS handles in compound tests)
│   ├── restorefh.go         # RESTOREFH (verify works with real-FS handles)
│   └── realfs_test.go       # Test infrastructure (newRealFSTestFixture, etc.)
│
├── attrs/
│   ├── encode.go            # EXISTING: EncodeRealFileAttrs, EncodePseudoFSAttrs
│   ├── decode.go            # NEW: DecodeFattr4ToSetAttrs (for SETATTR)
│   ├── decode_test.go       # NEW: fattr4 decode tests
│   └── bitmap.go            # EXISTING: bitmap helpers (no changes)
│
└── types/
    ├── constants.go         # EXISTING: all needed error codes already defined
    ├── types.go             # EXISTING: CompoundContext, Stateid4, RequireSavedFH
    └── errors.go            # EXISTING: MapMetadataErrorToNFS4, ValidateUTF8Filename
```

### Key Patterns from Existing Code

#### 1. Two-Filehandle Pattern (LINK, RENAME)
Both LINK and RENAME use SavedFH and CurrentFH per RFC 7530:
- **LINK**: SavedFH = source file to link, CurrentFH = target directory
- **RENAME**: SavedFH = source directory, CurrentFH = target directory

The pattern is:
```go
// 1. Check both FHs are set
if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK { ... }
if status := types.RequireSavedFH(ctx); status != types.NFS4_OK { ... }

// 2. Build auth context from CurrentFH
authCtx, shareName, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)

// 3. Validate both handles are from the same share (for cross-share detection)
savedShareName, _, _ := metadata.DecodeFileHandle(metadata.FileHandle(ctx.SavedFH))
if savedShareName != shareName { return NFS4ERR_XDEV }

// 4. Get pre-op attributes for change_info4
// 5. Call MetadataService method
// 6. Get post-op attributes for change_info4
// 7. Encode response with change_info4
```

#### 2. Error Mapping Pattern
All handlers use `types.MapMetadataErrorToNFS4(err)` to convert metadata layer errors to NFSv4 status codes. The mapping already covers all needed error codes:
- `ErrNotFound` -> `NFS4ERR_NOENT`
- `ErrIsDirectory` -> `NFS4ERR_ISDIR`
- `ErrNotDirectory` -> `NFS4ERR_NOTDIR`
- `ErrAlreadyExists` -> `NFS4ERR_EXIST`
- `ErrNotEmpty` -> `NFS4ERR_NOTEMPTY`
- `ErrPermissionDenied` -> `NFS4ERR_PERM`
- `ErrAccessDenied` -> `NFS4ERR_ACCESS`

**Missing mapping**: No `ErrCrossDevice` error code in the metadata errors package. RENAME cross-share detection must be handled at the protocol handler level by comparing share names from the file handles before calling `MetadataService.Move()`.

#### 3. Name Validation Pattern
All operations accepting component names use `types.ValidateUTF8Filename(name)` which checks:
- Empty filename -> `NFS4ERR_INVAL`
- Invalid UTF-8 -> `NFS4ERR_BADCHAR`
- Contains null bytes -> `NFS4ERR_BADCHAR`
- Contains '/' -> `NFS4ERR_BADNAME`
- Length > 255 -> `NFS4ERR_NAMETOOLONG`

#### 4. Pseudo-FS Guard Pattern
All mutating operations check `pseudofs.IsPseudoFSHandle(ctx.CurrentFH)` and return `NFS4ERR_ROFS` for pseudo-fs handles. This pattern is used in CREATE, REMOVE, and must be used in LINK, RENAME, SETATTR.

#### 5. Change Info Pattern
The `encodeChangeInfo4(buf, atomic, before, after)` helper encodes the `change_info4` structure used by CREATE, REMOVE, and needed by LINK and RENAME. The pattern captures ctime in nanoseconds before and after the operation:
```go
beforeCtime := uint64(parentFile.Ctime.UnixNano())
// ... perform operation ...
afterCtime := uint64(parentFileAfter.Ctime.UnixNano())
encodeChangeInfo4(&buf, true, beforeCtime, afterCtime)
```

#### 6. Test Infrastructure Pattern
Tests use `newRealFSTestFixture(t, "/export")` which creates:
- In-memory metadata store (`memorymeta.NewMemoryMetadataStoreWithDefaults()`)
- Runtime with nil control-plane store
- MetadataService with deferred commits disabled
- Root directory with handle
- Pseudo-FS with the share
- Handler connected to everything

Tests call handler methods directly (e.g., `fx.handler.handleCreate(ctx, reader)`) and verify results by parsing the XDR response bytes and confirming metadata state via `fx.metaSvc.Lookup()`.

## Implementation Research

### LINK Operation (RFC 7530 Section 16.9)

**Wire format:**
```
LINK4args:
    component4  newname;     // XDR string - name for the new link

LINK4res (success):
    nfsstat4     status;     // NFS4_OK
    change_info4 cinfo;      // Target directory change info

LINK4res (failure):
    nfsstat4     status;     // Error code
```

**Filehandle semantics:**
- SavedFH = source file (the file being linked to)
- CurrentFH = target directory (where the new name goes)
- After success, CurrentFH unchanged (still the target directory)

**Key errors:**
- `NFS4ERR_NOFILEHANDLE` - no CurrentFH
- `NFS4ERR_RESTOREFH` - no SavedFH (per RequireSavedFH)
- `NFS4ERR_ISDIR` - SavedFH is a directory (no dir hard links)
- `NFS4ERR_NOTDIR` - CurrentFH is not a directory
- `NFS4ERR_EXIST` - name already exists in target directory
- `NFS4ERR_XDEV` - cross-filesystem (different share)
- `NFS4ERR_ROFS` - pseudo-fs or read-only share
- `NFS4ERR_BADNAME` / `NFS4ERR_BADCHAR` - invalid name

**MetadataService method:** `CreateHardLink(ctx, dirHandle, name, targetHandle)` - already fully implemented with transaction support, link count increment, ctime update, directory existence check, and duplicate name check.

### RENAME Operation (RFC 7530 Section 16.27)

**Wire format:**
```
RENAME4args:
    component4  oldname;     // XDR string - source name
    component4  newname;     // XDR string - destination name

RENAME4res (success):
    nfsstat4     status;     // NFS4_OK
    change_info4 source_cinfo;  // Source directory change info
    change_info4 target_cinfo;  // Target directory change info

RENAME4res (failure):
    nfsstat4     status;     // Error code
```

**Filehandle semantics:**
- SavedFH = source directory
- CurrentFH = target directory
- Both FHs unchanged after operation

**Key errors:**
- `NFS4ERR_NOFILEHANDLE` - no CurrentFH
- `NFS4ERR_RESTOREFH` - no SavedFH
- `NFS4ERR_NOTDIR` - SavedFH or CurrentFH not a directory
- `NFS4ERR_NOENT` - source name not found
- `NFS4ERR_EXIST` - target name exists and can't be overwritten (type mismatch)
- `NFS4ERR_NOTEMPTY` - target is non-empty directory
- `NFS4ERR_XDEV` - cross-share rename
- `NFS4ERR_ROFS` - pseudo-fs or read-only share
- `NFS4ERR_BADNAME` / `NFS4ERR_BADCHAR` - invalid name

**MetadataService method:** `Move(ctx, fromDir, fromName, toDir, toName)` - already fully implemented with: same-directory no-op, permission checks, sticky bit enforcement, atomic target replacement, directory rename support, non-empty target check.

**Cross-share detection:** Must be done at handler level by comparing share names from SavedFH and CurrentFH via `metadata.DecodeFileHandle()`. The `MetadataService.Move()` method uses `storeForHandle()` which routes by share name but does not explicitly return XDEV -- it would fail with a different error if handles are from different stores.

### SETATTR Operation (RFC 7530 Section 16.32)

**Wire format:**
```
SETATTR4args:
    stateid4  stateid;           // For size changes (lock context)
    fattr4    obj_attributes;    // Attributes to set

SETATTR4res (success):
    nfsstat4  status;            // NFS4_OK
    bitmap4   attrsset;          // Bitmap of attributes actually set

SETATTR4res (failure):
    nfsstat4  status;            // Error code
    bitmap4   attrsset;          // Bitmap of attributes set before error
```

**Note:** SETATTR response always includes attrsset bitmap, even on failure (to indicate what was partially set before error). Per decision: use all-or-nothing semantics with transactions, so attrsset is either all requested bits (success) or empty (failure).

**fattr4 decode requirements (NEW INFRASTRUCTURE):**
The current codebase only has encode functions (`EncodeRealFileAttrs`, `EncodePseudoFSAttrs`). SETATTR requires a new `DecodeFattr4ToSetAttrs()` function that:
1. Reads the bitmap4 (which attributes the client wants to set)
2. Reads the opaque attrvals data
3. Decodes attribute values in bit-number order based on the bitmap
4. Maps to `metadata.SetAttrs` struct

Supported attribute bits for SETATTR:
- `FATTR4_SIZE` (bit 4) -> `SetAttrs.Size`
- `FATTR4_MODE` (bit 33) -> `SetAttrs.Mode`
- `FATTR4_OWNER` (bit 36) -> `SetAttrs.UID` (parse "uid@domain" format)
- `FATTR4_OWNER_GROUP` (bit 37) -> `SetAttrs.GID` (parse "gid@domain" format)
- `FATTR4_TIME_ACCESS_SET` (bit 48) -> `SetAttrs.Atime` or `SetAttrs.AtimeNow`
- `FATTR4_TIME_MODIFY_SET` (bit 54) -> `SetAttrs.Mtime` or `SetAttrs.MtimeNow`

**New constants needed:** `FATTR4_TIME_ACCESS_SET = 48` and `FATTR4_TIME_MODIFY_SET = 54` in attrs package.

**time_how4 enum for SET timestamps:**
```
enum time_how4 {
    SET_TO_SERVER_TIME4 = 0,
    SET_TO_CLIENT_TIME4 = 1
};
// If SET_TO_CLIENT_TIME4, followed by nfstime4 {int64 seconds, uint32 nseconds}
```

**sattrguard4:** Per context decisions, the handler should check if the client sends a guard. The guard format is:
```
union sattrguard4 switch (bool check) {
    case TRUE:  nfstime4 obj_ctime;   // Guard ctime value
    case FALSE: void;
};
```
If `check == TRUE`, the handler reads the ctime and compares it against the current file's ctime. If they don't match, return `NFS4ERR_NOT_SAME` (RFC 7530 Section 16.32 does NOT define sattrguard4 explicitly but references VERIFY pattern). Actually, the standard SETATTR guard in NFSv4 is implicit via COMPOUND sequencing (VERIFY + SETATTR), not in SETATTR args directly.

**Correction on sattrguard4:** NFSv4 SETATTR4args does NOT have a sattrguard4 field (unlike NFSv3). The guard mechanism is achieved by preceding SETATTR with a VERIFY operation in the COMPOUND. The SETATTR4args contains only `stateid4` + `fattr4`. The CONTEXT.md decision says "sattrguard4 (guard) enforced" but NFSv4 uses VERIFY+SETATTR compound pattern instead. The handler should NOT read a guard field from the args.

**Owner string parsing:** NFSv4 owner/group attributes are strings in "user@domain" format. The handler needs to parse these into numeric UIDs/GIDs. Pattern: try parsing as "N@domain" (numeric), then fall back to well-known names ("root@localdomain" -> UID 0). This matches the encode pattern in `encodeRealFileAttr()`.

**POSIX semantics already in MetadataService:**
- `SetFileAttributes()` already handles: mode validation, SUID/SGID stripping on ownership change, ownership permission checks (owner or root), timestamp updates with AtimeNow/MtimeNow, ctime auto-update.

### VERIFY / NVERIFY Operations (RFC 7530 Sections 16.35 / 16.15)

**Wire format (same for both):**
```
VERIFY4args / NVERIFY4args:
    fattr4  obj_attributes;  // Attributes to verify

VERIFY4res / NVERIFY4res (success):
    nfsstat4  status;        // NFS4_OK (VERIFY: attrs match, NVERIFY: attrs don't match)

VERIFY4res / NVERIFY4res (failure):
    nfsstat4  status;        // NFS4ERR_NOT_SAME (VERIFY) or NFS4ERR_SAME (NVERIFY)
```

**Comparison approach (Claude's Discretion decision):**
Two approaches were considered:
1. **Byte-exact XDR comparison**: Encode the server's current attributes using the same bitmap as the client request, then compare the resulting opaque bytes. Simpler implementation but fragile (padding, encoding variations could cause false mismatches).
2. **Per-attribute decode and compare**: Decode the client's fattr4 values per-attribute, then compare each value against the server's current attribute. More code but more robust and reusable.

**Recommendation: Byte-exact XDR comparison.** For VERIFY/NVERIFY, the client sends attributes encoded in XDR format. We encode the server's attributes using the same bitmap and compare the opaque data byte-by-byte. This is actually the correct approach because:
- The RFC specifies comparison of the XDR-encoded fattr4 values
- Our encoder is deterministic (same file -> same bytes)
- It's simpler than decoding every attribute type
- It works for ALL readable attributes without needing per-attribute comparison logic

Implementation pattern:
```go
// 1. Read client-provided fattr4 (bitmap + opaque data)
clientBitmap := DecodeBitmap4(reader)
clientData := DecodeOpaque(reader)

// 2. Get current file attributes
file := metaSvc.GetFile(ctx, handle)

// 3. Encode server's attributes using same bitmap
var serverBuf bytes.Buffer
EncodeRealFileAttrs(&serverBuf, clientBitmap, file, handle)
// Extract just the opaque data portion from the encoded attrs

// 4. Compare
match := bytes.Equal(clientData, serverData)
// VERIFY: match -> NFS4_OK, !match -> NFS4ERR_NOT_SAME
// NVERIFY: !match -> NFS4_OK, match -> NFS4ERR_SAME
```

**Pseudo-FS support:** VERIFY/NVERIFY must work on both pseudo-fs and real-fs handles. For pseudo-fs, use `EncodePseudoFSAttrs()` with the pseudo-fs node instead of `EncodeRealFileAttrs()`.

### SECINFO Operation Upgrade

The current SECINFO handler (`secinfo.go`) is a Phase 7 stub that:
- Requires current filehandle
- Reads and discards the component name
- Always returns AUTH_SYS (flavor 1) as the only mechanism
- Clears CurrentFH after SECINFO (per RFC 7530)

**Phase 8 upgrade (per context decisions):**
- Advertise both AUTH_SYS (flavor 1) and AUTH_NONE (flavor 0)
- Read per-export security config from share configuration in control plane store
- Return flavors in preference order (strongest first: AUTH_SYS before AUTH_NONE)
- Work on both pseudo-fs and real-fs paths
- Validate the component name exists (for real-fs) or return NFS4ERR_NOENT

**Implementation:** Use `h.Registry.GetShare(shareName)` to read share options, then check `share.AllowedAuthMethods` to determine which flavors to advertise. If the share has no specific restrictions, return both AUTH_SYS and AUTH_NONE. Always AUTH_SYS first (stronger).

### Stub Operations

**OPENATTR (OP_OPENATTR = 19):**
- Returns `NFS4ERR_NOTSUPP` (named attributes deferred to Phase 25)
- No args to consume (OPENATTR4args has only a bool createdir)
- Must read the bool from args to avoid XDR desync

**OPEN_DOWNGRADE (OP_OPEN_DOWNGRADE = 21):**
- Returns `NFS4ERR_NOTSUPP` (state management deferred to Phase 9)
- Must consume args: stateid4 (16 bytes) + seqid (4 bytes) + share_access (4 bytes) + share_deny (4 bytes)

**RELEASE_LOCKOWNER (OP_RELEASE_LOCKOWNER = 39):**
- Returns `NFS4_OK` (no-op success to prevent NOTSUPP errors from clients)
- Must consume args: lock_owner4 (clientid uint64 + owner opaque)
- Returning success is safe because we don't track lock owners yet (Phase 9)

## Key Risks and Mitigations

### Risk 1: fattr4 Decode Complexity
**Risk:** SETATTR requires decoding many attribute types from XDR. Getting encoding/decoding wrong breaks client compatibility.
**Mitigation:** Only decode the six writable attributes (SIZE, MODE, OWNER, OWNER_GROUP, TIME_ACCESS_SET, TIME_MODIFY_SET). All other bits in the bitmap are unsupported for SETATTR and should return `NFS4ERR_ATTRNOTSUPP`. This limits the decode scope significantly.

### Risk 2: Owner String Parsing
**Risk:** NFSv4 owner/group strings use "name@domain" format. Parsing this to numeric UIDs is non-trivial (requires user database lookup).
**Mitigation:** Support numeric format ("1000@localdomain" -> UID 1000) and well-known names ("root@localdomain" -> UID 0, "nobody@localdomain" -> UID 65534). Return `NFS4ERR_BADOWNER` if the string can't be parsed. This matches our encode pattern.

### Risk 3: Cross-Share Detection in RENAME
**Risk:** `MetadataService.Move()` doesn't explicitly detect cross-share scenarios. File handles from different shares routed to different stores would cause confusing errors.
**Mitigation:** Detect cross-share at handler level by comparing share names from both file handles BEFORE calling Move(). Return `NFS4ERR_XDEV` immediately.

### Risk 4: VERIFY/NVERIFY XDR Comparison Correctness
**Risk:** Byte-exact XDR comparison requires our encoder to be deterministic and match client expectations for encoding.
**Mitigation:** Our encoder is deterministic (tested in Phase 7). The comparison works because the same attribute encoding is used for both GETATTR responses and VERIFY/NVERIFY client data. Linux/macOS NFS clients use the same XDR encoding.

### Risk 5: SETATTR stateid Handling
**Risk:** SETATTR includes a stateid4 for size changes. Phase 9 adds proper state tracking; Phase 8 must handle it gracefully.
**Mitigation:** Accept any special stateids (all-zeros, all-ones) and placeholder stateids, consistent with Phase 7 OPEN/READ/WRITE approach. Only validate stateid format (16 bytes), not state ownership.

## Existing Code to Reuse

| What | Where | How to Use |
|------|-------|-----------|
| `encodeChangeInfo4()` | `handlers/helpers.go:117` | Used by LINK (1x change_info) and RENAME (2x change_info) |
| `buildV4AuthContext()` | `handlers/helpers.go:24` | Auth context for LINK, RENAME, SETATTR |
| `getMetadataServiceForCtx()` | `handlers/helpers.go:91` | Get MetadataService in all handlers |
| `types.RequireCurrentFH()` | `types/types.go:221` | Check CurrentFH in all handlers |
| `types.RequireSavedFH()` | `types/types.go:233` | Check SavedFH in LINK and RENAME |
| `types.ValidateUTF8Filename()` | `types/errors.go:83` | Name validation in LINK and RENAME |
| `types.MapMetadataErrorToNFS4()` | `types/errors.go:18` | Error mapping in all handlers |
| `pseudofs.IsPseudoFSHandle()` | `pseudofs/` | ROFS guard in mutating ops |
| `encodeStatusOnly()` | `handlers/handler.go:115` | Error-only responses |
| `notSuppHandler()` | `handlers/handler.go:103` | Base for OPENATTR stub |
| `MetadataService.CreateHardLink()` | `pkg/metadata/file.go:249` | LINK business logic |
| `MetadataService.Move()` | `pkg/metadata/file.go:550` | RENAME business logic |
| `MetadataService.SetFileAttributes()` | `pkg/metadata/file.go:364` | SETATTR business logic |
| `EncodeRealFileAttrs()` | `attrs/encode.go:295` | VERIFY/NVERIFY server-side encoding |
| `EncodePseudoFSAttrs()` | `attrs/encode.go:131` | VERIFY/NVERIFY pseudo-fs encoding |
| `attrs.DecodeBitmap4()` | `attrs/bitmap.go:48` | Bitmap decode for SETATTR/VERIFY |
| `attrs.SupportedAttrs()` | `attrs/encode.go:85` | Supported attribute bitmap |
| `types.DecodeStateid4()` | `types/types.go:194` | Stateid decode for SETATTR |
| `skipFattr4()` | `handlers/create.go:297` | Reference for fattr4 structure |

## New Infrastructure Needed

### 1. fattr4 Attribute Decode Function (attrs/decode.go)
A new `DecodeFattr4ToSetAttrs(reader io.Reader) (*metadata.SetAttrs, []uint32, error)` function that:
- Reads bitmap4 to determine which attributes are being set
- Reads opaque attr_vals data
- Decodes each attribute value in bit-number order
- Returns the populated SetAttrs struct and the bitmap of requested attributes
- Returns `NFS4ERR_ATTRNOTSUPP` if any unsupported writable attribute is requested

New attribute constants needed:
```go
FATTR4_TIME_ACCESS_SET = 48  // settime4 for atime
FATTR4_TIME_MODIFY_SET = 54  // settime4 for mtime
```

### 2. Owner String Parser
A function to parse NFSv4 "user@domain" strings to numeric UIDs:
```go
func ParseOwnerString(owner string) (uint32, error)
func ParseGroupString(group string) (uint32, error)
```

### 3. Writable Attributes Bitmap
A `WritableAttrs()` function returning the bitmap of attributes that SETATTR can modify (subset of SupportedAttrs).

### 4. Dispatch Table Registration
Add 6 operations to `handler.go` dispatch table:
```go
h.opDispatchTable[types.OP_LINK] = h.handleLink
h.opDispatchTable[types.OP_RENAME] = h.handleRename
h.opDispatchTable[types.OP_SETATTR] = h.handleSetAttr
h.opDispatchTable[types.OP_VERIFY] = h.handleVerify
h.opDispatchTable[types.OP_NVERIFY] = h.handleNVerify
h.opDispatchTable[types.OP_OPENATTR] = h.handleOpenAttr
h.opDispatchTable[types.OP_OPEN_DOWNGRADE] = h.handleOpenDowngrade
h.opDispatchTable[types.OP_RELEASE_LOCKOWNER] = h.handleReleaseLockOwner
```

## Recommended Plan Structure

### Plan 08-01: LINK and RENAME Operations
**Scope:** Two-filehandle operations sharing SavedFH+CurrentFH patterns
**Files:** `link.go`, `rename.go`, `link_rename_test.go`, `handler.go` (dispatch registration)
**Complexity:** Medium -- clear patterns from CREATE/REMOVE, MetadataService methods exist
**Tests:** ~15-20 tests covering: success, pseudo-fs ROFS, no FH, no saved FH, cross-share XDEV, invalid names, not found, is directory (LINK), not directory, not empty (RENAME dir-over-dir), same file rename no-op, compound sequences with SAVEFH+RENAME

### Plan 08-02: SETATTR with fattr4 Decode Infrastructure
**Scope:** SETATTR handler + new fattr4 decode/parse functions + owner string parsing
**Files:** `setattr.go`, `attrs/decode.go`, `attrs/decode_test.go`, `setattr_test.go`, `attrs/encode.go` (new constants)
**Complexity:** High -- most new infrastructure (fattr4 decode, time_how4, owner parsing)
**Tests:** ~20-25 tests covering: set mode, set owner, set size (truncate), set timestamps (server time, client time), multiple attrs at once, pseudo-fs ROFS, permission denied, unsupported attr, stateid handling, owner string parsing edge cases, mode validation (reject > 07777), SUID/SGID clearing on chown

### Plan 08-03: VERIFY/NVERIFY, SECINFO Upgrade, and Stubs
**Scope:** Conditional operations, security upgrade, and cleanup stubs
**Files:** `verify.go`, `nverify.go`, `secinfo.go` (upgrade), `stubs.go`, `verify_test.go`, `stubs_test.go`, `handler.go` (dispatch)
**Complexity:** Medium -- VERIFY/NVERIFY reuse encode infrastructure, stubs are trivial
**Tests:** ~15-20 tests covering: VERIFY match (NFS4_OK), VERIFY mismatch (NFS4ERR_NOT_SAME), NVERIFY match (NFS4ERR_SAME), NVERIFY mismatch (NFS4_OK), pseudo-fs VERIFY, stale FH, SECINFO with AUTH_SYS+AUTH_NONE, SECINFO FH clearing, OPENATTR returns NOTSUPP, OPEN_DOWNGRADE returns NOTSUPP, RELEASE_LOCKOWNER returns OK, compound sequences (VERIFY + SETATTR pattern)

## References

- [RFC 7530 - NFSv4 Protocol](https://datatracker.ietf.org/doc/html/rfc7530) - Sections 16.9 (LINK), 16.15 (NVERIFY), 16.17 (OPENATTR), 16.19 (OPEN_DOWNGRADE), 16.27 (RENAME), 16.31 (SECINFO), 16.32 (SETATTR), 16.35 (VERIFY), 16.37 (RELEASE_LOCKOWNER)
- [RFC 7531 - NFSv4 XDR Description](https://datatracker.ietf.org/doc/html/rfc7531) - Authoritative XDR type definitions

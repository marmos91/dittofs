# Phase 7: NFSv4 File Operations - Research

**Researched:** 2026-02-13
**Domain:** NFSv4 file operations (LOOKUP, GETATTR, READDIR, ACCESS, CREATE, REMOVE, READLINK, OPEN, CLOSE, READ, WRITE, COMMIT per RFC 7530)
**Confidence:** HIGH

## Summary

Phase 7 builds on the NFSv4 foundation from Phase 6 to make the NFSv4 server functionally useful. Phase 6 implemented 14 operation handlers that work only with the pseudo-filesystem (virtual namespace). Phase 7 upgrades 5 existing handlers (LOOKUP, LOOKUPP, GETATTR, READDIR, ACCESS) to work with real filesystems, and adds 8 new operations (CREATE, REMOVE, READLINK, OPEN, CLOSE, READ, WRITE, COMMIT).

The critical architectural challenge is bridging the NFSv4 CompoundContext (which carries CurrentFH as opaque bytes and UID/GID credentials) to the existing metadata/payload service layer. The metadata service already routes operations based on share names embedded in file handles (`DecodeFileHandle()`), so v4 handlers can call MetadataService methods directly using `metadata.FileHandle(ctx.CurrentFH)`. Auth context building requires extracting the share name from the current filehandle, then applying identity mapping via the runtime (same pattern as v3 but adapted for the v4 CompoundContext).

The OPEN/CLOSE/OPEN_CONFIRM operations introduce stateful semantics that are the most complex part of this phase. A minimal implementation can: (1) accept all OPEN requests, (2) generate placeholder stateids, (3) always set OPEN4_RESULT_CONFIRM in rflags (requiring OPEN_CONFIRM), (4) never grant delegations (OPEN_DELEGATE_NONE), and (5) accept special stateids (all-zeros, all-ones) for READ/WRITE without requiring OPEN. This defers full state tracking to Phase 9 while still allowing Linux/macOS clients to mount and operate.

**Primary recommendation:** Organize into three waves: (1) upgrade existing pseudo-fs-only ops for real filesystem support + READLINK, (2) add stateless mutation ops (CREATE, REMOVE), (3) add stateful I/O ops (OPEN, CLOSE, OPEN_CONFIRM, READ, WRITE, COMMIT). Build a shared `v4AuthContext()` helper and `EncodeRealFileAttrs()` function that both waves 1 and 2 depend on.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `pkg/metadata` (MetadataService) | existing | File/directory CRUD, permission checking | Already handles routing by share name via handle |
| `pkg/payload` (PayloadService) | existing | File content read/write/flush | Already handles cache + transfer management |
| `internal/protocol/nfs/v4/types` | Phase 6 | NFSv4 constants, error mapping, CompoundContext | Foundation from Phase 6 |
| `internal/protocol/nfs/v4/attrs` | Phase 6 | Bitmap4 encode/decode, attribute encoding | Foundation from Phase 6 |
| `internal/protocol/nfs/v4/pseudofs` | Phase 6 | Pseudo-fs handle detection, junction crossing | Foundation from Phase 6 |
| `internal/protocol/xdr` | existing | XDR encode/decode primitives | Shared across v3/v4 |
| Go stdlib `bytes`, `io` | N/A | Buffer management, streaming XDR decode | Same pattern as Phase 6 |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/protocol/nfs/v3/handlers` | existing | Reference patterns for file ops | Auth context building, service access patterns |
| `pkg/controlplane/runtime` | existing | GetMetadataService/GetPayloadService, identity mapping | Auth context + service resolution |
| `internal/bufpool` | existing | Buffer pooling for READ data | Large READ responses to reduce GC |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Placeholder stateids (this phase) | Full state tracking | Phase 9 is dedicated to state management; premature here |
| Special stateid bypass for READ/WRITE | Strict OPEN requirement | Linux clients use special stateids; blocking them breaks mounts |
| Auth context per-operation | Cached auth context on CompoundContext | Per-op is simpler and matches v3 pattern; caching adds complexity |

**No new dependencies required.** All packages already exist in the codebase.

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/
├── handlers/
│   ├── handler.go          # MODIFY: register new ops in dispatch table
│   ├── context.go          # EXISTING: ExtractV4HandlerContext
│   ├── compound.go         # EXISTING: ProcessCompound dispatcher
│   ├── helpers.go          # NEW: shared v4 auth context builder, service getters
│   ├── attrs_real.go       # NEW: EncodeRealFileAttrs (real files, not pseudo-fs)
│   │
│   │  # Existing handlers to UPGRADE (add real-fs branches):
│   ├── lookup.go           # MODIFY: add real filesystem LOOKUP
│   ├── lookupp.go          # MODIFY: add real filesystem LOOKUPP
│   ├── getattr.go          # MODIFY: add real filesystem GETATTR
│   ├── readdir.go          # MODIFY: add real filesystem READDIR
│   ├── access.go           # MODIFY: add real filesystem ACCESS
│   │
│   │  # New stateless mutation handlers:
│   ├── create.go           # NEW: CREATE (dirs, symlinks, special files)
│   ├── remove.go           # NEW: REMOVE (files and directories)
│   ├── readlink.go         # NEW: READLINK
│   │
│   │  # New stateful I/O handlers:
│   ├── open.go             # NEW: OPEN + OPEN_CONFIRM
│   ├── close.go            # NEW: CLOSE
│   ├── read.go             # NEW: READ
│   ├── write.go            # NEW: WRITE
│   └── commit.go           # NEW: COMMIT
│
├── attrs/
│   ├── encode.go           # MODIFY: add EncodeRealFileAttrs or generalize
│   └── bitmap.go           # EXISTING: bitmap helpers (no changes needed)
│
├── pseudofs/               # EXISTING: no changes needed
├── types/
│   ├── constants.go        # MODIFY: add OPEN/CLOSE/stateid constants
│   ├── types.go            # MODIFY: add stateid4, change_info4 types
│   └── errors.go           # EXISTING: MapMetadataErrorToNFS4 (no changes needed)
```

### Pattern 1: Pseudo-FS vs Real-FS Routing
**What:** Every handler that existed in Phase 6 has a `if pseudofs.IsPseudoFSHandle(ctx.CurrentFH)` branch. The real-FS branch needs to be implemented.
**When to use:** All existing handlers (LOOKUP, LOOKUPP, GETATTR, READDIR, ACCESS)
**Example:**
```go
// Source: existing pattern in lookup.go
func (h *Handler) handleLookup(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
    // ... decode args ...

    if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
        return h.lookupInPseudoFS(ctx, name)
    }

    // NEW: Real filesystem lookup
    return h.lookupInRealFS(ctx, name)
}
```

### Pattern 2: Auth Context Building for V4
**What:** V4 handlers need a `metadata.AuthContext` to call MetadataService methods. Unlike v3 (which has `Share` on the handler context), v4 must extract the share name from the current filehandle.
**When to use:** Every operation that calls MetadataService with permission checks
**Example:**
```go
// NEW helper in helpers.go
func (h *Handler) buildV4AuthContext(
    ctx *types.CompoundContext,
    handle []byte,
) (*metadata.AuthContext, string, error) {
    // Extract share name from file handle
    shareName, _, err := metadata.DecodeFileHandle(metadata.FileHandle(handle))
    if err != nil {
        return nil, "", err
    }

    // Build identity from CompoundContext
    identity := &metadata.Identity{
        UID:  ctx.UID,
        GID:  ctx.GID,
        GIDs: ctx.GIDs,
    }

    // Apply share-level identity mapping via runtime
    effectiveIdentity, err := h.Registry.ApplyIdentityMapping(shareName, identity)
    if err != nil {
        return nil, shareName, err
    }

    authCtx := &metadata.AuthContext{
        Context:    ctx.Context,
        ClientAddr: ctx.ClientAddr,
        AuthMethod: "unix",
        Identity:   effectiveIdentity,
    }

    return authCtx, shareName, nil
}
```

### Pattern 3: Real File Attribute Encoding
**What:** Encode `metadata.FileAttr` into NFSv4 fattr4 format (bitmap + opaque values). Similar to `EncodePseudoFSAttrs` but reads from real file metadata.
**When to use:** GETATTR, READDIR, OPEN responses
**Example:**
```go
// NEW in attrs_real.go or attrs/encode.go
func EncodeRealFileAttrs(buf *bytes.Buffer, requested []uint32, file *metadata.File, handle metadata.FileHandle) error {
    supported := SupportedAttrs()
    responseBitmap := Intersect(requested, supported)
    EncodeBitmap4(buf, responseBitmap)

    var attrData bytes.Buffer
    for bit := uint32(0); bit < maxBits; bit++ {
        if !IsBitSet(responseBitmap, bit) { continue }
        encodeRealAttr(&attrData, bit, file, handle)
    }
    xdr.WriteXDROpaque(buf, attrData.Bytes())
    return nil
}
```

### Pattern 4: Two-Phase Write (NFSv4 variant)
**What:** Same as v3 but adapted for NFSv4 wire format. PrepareWrite validates and creates intent, payload writes data, CommitWrite updates metadata.
**When to use:** WRITE operation
**Example:**
```go
// In write.go handler
intent, err := metaSvc.PrepareWrite(authCtx, fileHandle, newSize)
if err != nil { /* map to NFS4 error */ }

err = payloadSvc.WriteAt(ctx.Context, intent.PayloadID, data, offset)
if err != nil { /* map to NFS4 error */ }

_, err = metaSvc.CommitWrite(authCtx, intent)
```

### Pattern 5: Minimal OPEN State Management (Phase 7 stub)
**What:** Accept OPEN requests, return placeholder stateids, require OPEN_CONFIRM, never grant delegations. Accept special stateids for READ/WRITE bypass.
**When to use:** OPEN, CLOSE, OPEN_CONFIRM, READ/WRITE stateid validation
**Example:**
```go
// Placeholder stateid generation
func generatePlaceholderStateid() stateid4 {
    return stateid4{
        Seqid: 1,
        Other: randomBytes(12), // NFS4_OTHER_SIZE = 12
    }
}

// Special stateid detection for READ/WRITE
func isSpecialStateid(sid stateid4) bool {
    return isAllZeros(sid) || isAllOnes(sid)
}
```

### Anti-Patterns to Avoid
- **Implementing full state tracking in Phase 7:** Phase 9 is dedicated to state management. Phase 7 should use placeholder stateids that allow operations to succeed without real state tracking.
- **Skipping OPEN_CONFIRM:** Linux clients expect the server to set OPEN4_RESULT_CONFIRM for new open-owners. Skipping OPEN_CONFIRM causes clients to hang.
- **Blocking READ/WRITE without OPEN:** Some clients use special stateids (all-zeros, all-ones) for read operations. Blocking these breaks basic file access.
- **Per-handler attribute encoding:** Duplicate attribute encoding across GETATTR/READDIR/OPEN. Use a shared `EncodeRealFileAttrs()` function.
- **Business logic in handlers:** Keep permission checks, file creation, etc. in MetadataService. Handlers only do XDR decode, call service, XDR encode.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| File permission checking | Custom permission logic in v4 handlers | `MetadataService.CheckPermissions()` | Already handles Unix mode bits, share-level ACLs |
| Handle-to-store routing | Manual share name extraction + store lookup | `MetadataService` methods accept `FileHandle` directly | `storeForHandle()` extracts share name automatically |
| Content read/write | Direct cache/block store access | `PayloadService.ReadAt()/WriteAt()` | Handles chunk spanning, cache management, COW |
| Identity mapping | Inline squash logic | `Runtime.ApplyIdentityMapping()` | Handles all_squash, root_squash, admin mapping |
| UTF-8 filename validation | Custom validation | `types.ValidateUTF8Filename()` | Already implemented in Phase 6 types package |
| Error mapping | Manual NFS4 error code switching | `types.MapMetadataErrorToNFS4()` | Already maps all StoreError codes to NFS4 status |
| Bitmap encoding | Inline bitmap manipulation | `attrs.EncodeBitmap4()/DecodeBitmap4()` | Phase 6 implementation handles all edge cases |

**Key insight:** The metadata and payload service layers do ALL the heavy lifting. V4 handlers are thin wrappers that decode XDR, convert to service calls, and encode XDR responses -- exactly like the v3 handlers.

## Common Pitfalls

### Pitfall 1: FileHandle Format Mismatch
**What goes wrong:** NFSv4 handles are opaque `[]byte` in CompoundContext but MetadataService expects `metadata.FileHandle` (which is `[]byte` typedef). Accidentally passing pseudo-fs handles (prefixed "pseudofs:") to MetadataService causes DecodeFileHandle to fail.
**Why it happens:** No compile-time distinction between pseudo-fs handles and real handles.
**How to avoid:** Always check `pseudofs.IsPseudoFSHandle(ctx.CurrentFH)` before calling any MetadataService method. Return `NFS4ERR_STALE` or route to pseudo-fs handler.
**Warning signs:** "invalid file handle format: missing ':' separator" errors in logs.

### Pitfall 2: Copy-on-Set for FileHandle Assignments
**What goes wrong:** Aliasing CurrentFH/SavedFH by assigning without copying causes one operation to corrupt another's filehandle.
**Why it happens:** Go slices share underlying arrays when assigned without copy.
**How to avoid:** Prior decision from Phase 6: always use `copy()` for all FH assignments. This is already enforced in existing handlers.
**Warning signs:** Intermittent STALE handle errors, especially in COMPOUND sequences with SAVEFH/RESTOREFH.

### Pitfall 3: NFSv4 CREATE vs v3 CREATE Semantics
**What goes wrong:** NFSv4 CREATE (OP_CREATE) creates directories, symlinks, and special files -- NOT regular files. Regular file creation happens through OPEN with OPEN4_CREATE. Implementing CREATE to create regular files breaks the protocol.
**Why it happens:** Name collision with NFSv3 CREATE which does create regular files.
**How to avoid:** Check the RFC: CREATE4args contains a `createtype4` union which only has NF4LNK, NF4BLK, NF4CHR, NF4SOCK, NF4FIFO, NF4DIR. Regular files must go through OPEN.
**Warning signs:** Client-side errors when trying to create regular files.

### Pitfall 4: OPEN Stateid Must Be Usable
**What goes wrong:** Returning a completely dummy stateid that the server can't recognize on subsequent READ/WRITE causes all I/O to fail with NFS4ERR_BAD_STATEID.
**Why it happens:** Even with placeholder state, READ/WRITE need to accept the stateid returned by OPEN (or special stateids).
**How to avoid:** For Phase 7 stubs, accept ALL stateids in READ/WRITE (don't validate). Real validation comes in Phase 9.
**Warning signs:** Files can be opened but not read/written.

### Pitfall 5: READDIR Cookie/Verifier Semantics
**What goes wrong:** NFSv4 READDIR cookies must be stable across calls. Using array indices (like pseudo-fs does) works for small, static directories but breaks for real directories where files are added/removed between READDIR calls.
**Why it happens:** MetadataService.ReadDirectory already handles cookie-based pagination correctly for real directories.
**How to avoid:** Use the Cookie values from DirEntry directly (they come from MetadataService's cookie manager). Don't generate your own cookies for real directories.
**Warning signs:** Infinite READDIR loops, missing entries, duplicate entries.

### Pitfall 6: Share Name Extraction for Auth Context
**What goes wrong:** V3 handlers get the share name from the connection-level context (extracted at mount time). V4 handlers don't have this -- they must extract it from the current filehandle on every operation.
**Why it happens:** NFSv4 multiplexes all shares over a single connection through the pseudo-fs.
**How to avoid:** Always extract share name from `metadata.DecodeFileHandle()` on the current filehandle. Cache the result on CompoundContext if desired for performance within a COMPOUND.
**Warning signs:** "no store configured for share" errors, incorrect identity mapping.

### Pitfall 7: OPEN_CONFIRM Required for New Open-Owners
**What goes wrong:** Omitting OPEN_CONFIRM support causes Linux clients to hang after OPEN.
**Why it happens:** Per RFC 7530, when the server sees an open-owner for the first time, it MUST set OPEN4_RESULT_CONFIRM in rflags, requiring the client to issue OPEN_CONFIRM before any READ/WRITE.
**How to avoid:** Always set OPEN4_RESULT_CONFIRM in rflags. Implement OPEN_CONFIRM to simply return NFS4_OK with the same stateid (Phase 7 stub). Phase 9 will implement proper seqid tracking.
**Warning signs:** Client hangs or returns "Resource temporarily unavailable" after OPEN.

## Code Examples

### Example 1: Real Filesystem LOOKUP
```go
// Source: adapted from v3 lookup pattern + v4 compound model
func (h *Handler) lookupInRealFS(ctx *types.CompoundContext, name string) *types.CompoundResult {
    authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
    if err != nil {
        status := types.MapMetadataErrorToNFS4(err)
        return &types.CompoundResult{Status: status, OpCode: types.OP_LOOKUP, Data: encodeStatusOnly(status)}
    }

    metaSvc := h.Registry.GetMetadataService()
    file, err := metaSvc.Lookup(authCtx, metadata.FileHandle(ctx.CurrentFH), name)
    if err != nil {
        status := types.MapMetadataErrorToNFS4(err)
        return &types.CompoundResult{Status: status, OpCode: types.OP_LOOKUP, Data: encodeStatusOnly(status)}
    }

    // Encode child handle and set as current FH
    childHandle, err := metadata.EncodeFileHandle(file)
    if err != nil {
        return &types.CompoundResult{Status: types.NFS4ERR_SERVERFAULT, OpCode: types.OP_LOOKUP, Data: encodeStatusOnly(types.NFS4ERR_SERVERFAULT)}
    }

    // Copy-on-set per Phase 6 decision
    ctx.CurrentFH = make([]byte, len(childHandle))
    copy(ctx.CurrentFH, childHandle)

    return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_LOOKUP, Data: encodeStatusOnly(types.NFS4_OK)}
}
```

### Example 2: OPEN Operation (Minimal Stub)
```go
// Source: RFC 7530 Section 16.16 wire format
func (h *Handler) handleOpen(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
    // Decode OPEN4args: seqid, share_access, share_deny, owner, openhow, claim
    seqid, _ := xdr.DecodeUint32(reader)
    shareAccess, _ := xdr.DecodeUint32(reader)
    shareDeny, _ := xdr.DecodeUint32(reader)

    // Read open_owner4: clientid (uint64) + owner (opaque)
    clientID, _ := xdr.DecodeUint64(reader)
    ownerData, _ := xdr.DecodeOpaque(reader)

    // Read openflag4: opentype (uint32) + createhow4 if CREATE
    opentype, _ := xdr.DecodeUint32(reader)
    if opentype == OPEN4_CREATE {
        // Decode createhow4 and create the file
    }

    // Read open_claim4: claim_type (uint32) + claim-specific data
    claimType, _ := xdr.DecodeUint32(reader)
    // CLAIM_NULL: component name (most common case)

    // ... perform lookup/create via MetadataService ...

    // Build OPEN4resok response
    var buf bytes.Buffer
    xdr.WriteUint32(&buf, types.NFS4_OK)

    // stateid4: seqid(4) + other(12)
    xdr.WriteUint32(&buf, 1) // stateid seqid
    buf.Write(placeholderOther) // 12 bytes

    // change_info4: atomic(bool) + before(uint64) + after(uint64)
    xdr.WriteUint32(&buf, 1) // atomic = true
    xdr.WriteUint64(&buf, changeBefore)
    xdr.WriteUint64(&buf, changeAfter)

    // rflags: always request OPEN_CONFIRM for Phase 7
    xdr.WriteUint32(&buf, OPEN4_RESULT_CONFIRM)

    // attrset: bitmap4 (empty -- no attrs set by server)
    attrs.EncodeBitmap4(&buf, nil)

    // delegation: OPEN_DELEGATE_NONE
    xdr.WriteUint32(&buf, OPEN_DELEGATE_NONE)

    return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_OPEN, Data: buf.Bytes()}
}
```

### Example 3: READ with Special Stateid Bypass
```go
// Source: RFC 7530 Section 9.1.4.3 (special stateids) + v3 READ pattern
func (h *Handler) handleRead(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
    // Decode READ4args: stateid(16) + offset(8) + count(4)
    stateidSeqid, _ := xdr.DecodeUint32(reader)
    stateidOther := make([]byte, 12)
    io.ReadFull(reader, stateidOther)
    offset, _ := xdr.DecodeUint64(reader)
    count, _ := xdr.DecodeUint32(reader)

    // Phase 7: Accept all stateids (special or from OPEN)
    // Phase 9 will validate stateids properly

    // Get services
    metaSvc := h.Registry.GetMetadataService()
    payloadSvc := h.Registry.GetPayloadService()

    // Prepare read (validates file exists, checks permissions)
    authCtx, _, _ := h.buildV4AuthContext(ctx, ctx.CurrentFH)
    readMeta, err := metaSvc.PrepareRead(authCtx, metadata.FileHandle(ctx.CurrentFH))
    // ... error handling ...

    // Read data via payload service
    buf := make([]byte, count)
    n, err := payloadSvc.ReadAt(ctx.Context, readMeta.Attr.PayloadID, buf, offset)
    // ... handle EOF, errors ...

    // Encode READ4resok: eof(bool) + data(opaque)
    var resp bytes.Buffer
    xdr.WriteUint32(&resp, types.NFS4_OK)
    xdr.WriteUint32(&resp, boolToUint32(eof))
    xdr.WriteXDROpaque(&resp, buf[:n])

    return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_READ, Data: resp.Bytes()}
}
```

## State of the Art

| Old Approach (Phase 6) | Current Approach (Phase 7) | When Changed | Impact |
|------------------------|---------------------------|--------------|--------|
| All real-FS ops return NFS4ERR_NOTSUPP | Real-FS ops delegate to MetadataService/PayloadService | Phase 7 | NFSv4 becomes functional for file I/O |
| Pseudo-fs-only attribute encoding | Shared encoding for both pseudo-fs and real files | Phase 7 | GETATTR/READDIR work for real directories |
| No OPEN/CLOSE/state | Placeholder stateids with special stateid bypass | Phase 7 | Clients can mount and do I/O |
| No file content operations | READ/WRITE/COMMIT via PayloadService | Phase 7 | File content accessible over NFSv4 |

**Important:** Phase 7 placeholder stateids are NOT production-ready state management. Phase 9 replaces the placeholder approach with proper client ID tracking, open-owner management, lease enforcement, and seqid validation.

## NFSv4 Operation Wire Format Reference

### OPEN (OP=18) - Most Complex Operation
```
OPEN4args:
  seqid:        uint32          (open-owner sequence number)
  share_access: uint32          (READ=0x01, WRITE=0x02, BOTH=0x03)
  share_deny:   uint32          (NONE=0x00, READ=0x01, WRITE=0x02, BOTH=0x03)
  owner:        open_owner4     (clientid:uint64 + owner:opaque<>)
  openhow:      openflag4       (opentype:uint32 + createhow4 if CREATE)
  claim:        open_claim4     (claim_type:uint32 + type-specific data)

OPEN4resok:
  stateid:      stateid4        (seqid:uint32 + other:opaque[12])
  cinfo:        change_info4    (atomic:bool + before:uint64 + after:uint64)
  rflags:       uint32          (OPEN4_RESULT_CONFIRM=0x02, LOCKTYPE_POSIX=0x04)
  attrset:      bitmap4         (attrs actually set by server)
  delegation:   open_delegation4 (type:uint32 + type-specific data)
```

### CLOSE (OP=4)
```
CLOSE4args:  seqid:uint32 + open_stateid:stateid4
CLOSE4res:   status:uint32 + (if OK) open_stateid:stateid4
```

### READ (OP=25)
```
READ4args:   stateid:stateid4 + offset:uint64 + count:uint32
READ4resok:  eof:bool + data:opaque<>
```

### WRITE (OP=38)
```
WRITE4args:  stateid:stateid4 + offset:uint64 + stable:uint32(UNSTABLE4/DATA_SYNC4/FILE_SYNC4) + data:opaque<>
WRITE4resok: count:uint32 + committed:uint32 + writeverf:opaque[8]
```

### COMMIT (OP=5)
```
COMMIT4args: offset:uint64 + count:uint32
COMMIT4resok: writeverf:opaque[8]
```

### CREATE (OP=6) - Non-regular files only
```
CREATE4args: objtype:createtype4 + objname:component4 + createattrs:fattr4
CREATE4resok: cinfo:change_info4 + attrset:bitmap4
```

### REMOVE (OP=28)
```
REMOVE4args: target:component4
REMOVE4resok: cinfo:change_info4
```

### READLINK (OP=27)
```
READLINK4resok: link:linktext4(opaque<>)
```

### OPEN_CONFIRM (OP=20)
```
OPEN_CONFIRM4args: open_stateid:stateid4 + seqid:uint32
OPEN_CONFIRM4resok: open_stateid:stateid4
```

### Special Stateids (RFC 7530 Section 9.1.4.3)
```
Anonymous:    seqid=0, other=all-zeros  (no lock state, standard access check)
READ Bypass:  seqid=0, other=all-ones   (bypass locks for READ only)
```

## Open Questions

1. **OPEN share_deny enforcement**
   - What we know: Phase 7 uses placeholder stateids without real tracking. share_deny could be ignored.
   - What's unclear: Will clients break if share_deny is completely ignored?
   - Recommendation: Accept but ignore share_deny in Phase 7. Log at DEBUG level. Phase 9 implements proper enforcement.

2. **SETATTR scope**
   - What we know: SETATTR (OP=34) is listed in the roadmap under Phase 8 (Advanced Operations). But OPEN with OPEN4_CREATE may need to set attributes on newly created files.
   - What's unclear: Should Phase 7 include a basic SETATTR for the OPEN+CREATE path?
   - Recommendation: Handle initial attributes in OPEN's create path using MetadataService.CreateFile() attr parameter. Defer standalone SETATTR to Phase 8.

3. **Change attribute (changeid4) generation**
   - What we know: NFSv4 requires a monotonic change attribute per file. The ctime from FileAttr could serve as a change ID.
   - What's unclear: Whether ctime nanoseconds provide sufficient granularity for change detection.
   - Recommendation: Use `file.Ctime.UnixNano()` as changeid4. This is the same approach Linux nfsd uses (iversion counter, but ctime is a good approximation for DittoFS).

4. **LOOKUPP from real share root**
   - What we know: LOOKUPP from within a real share navigates to the parent directory. But at the share root, LOOKUPP should cross back into pseudo-fs.
   - What's unclear: How to detect the share root and navigate back.
   - Recommendation: When the MetadataService returns "parent not found" for a handle that matches the share's root handle, switch CurrentFH to the pseudo-fs handle for that share's junction point.

## Recommended Plan Reorganization

The original roadmap had 5 plans, but plan 07-01 is redundant (those ops are done). Reorganize into 3 plans based on dependency waves:

### Plan 07-01: Real Filesystem Support for Existing Ops
**Scope:** Upgrade LOOKUP, LOOKUPP, GETATTR, READDIR, ACCESS + add READLINK
**Dependencies:** Phase 6 complete
**Key deliverables:**
- `helpers.go` with `buildV4AuthContext()` and service getter helpers
- `attrs_real.go` or extend `attrs/encode.go` with `EncodeRealFileAttrs()`
- Real-FS branches in all 5 existing handlers
- New `readlink.go` handler
- NFSv4 clients can navigate real directories, read attributes, list contents

### Plan 07-02: CREATE and REMOVE Operations
**Scope:** Add CREATE (dirs, symlinks, special files) and REMOVE (files + dirs)
**Dependencies:** 07-01 (needs auth context builder, real-FS attr encoding)
**Key deliverables:**
- `create.go` handler (createtype4 union decode, MetadataService delegation)
- `remove.go` handler (component name, change_info4 response)
- `change_info4` encoding helper (used by CREATE, REMOVE, and later OPEN)

### Plan 07-03: OPEN, CLOSE, READ, WRITE, COMMIT
**Scope:** Stateful I/O operations with placeholder state management
**Dependencies:** 07-01, 07-02 (needs auth context, real-FS attrs, change_info4)
**Key deliverables:**
- `open.go` handler (complex XDR decode, CLAIM_NULL, OPEN4_CREATE for regular files, placeholder stateid, OPEN_CONFIRM support)
- `close.go` handler (accept stateid, return OK)
- `read.go` handler (special stateid bypass, PayloadService.ReadAt)
- `write.go` handler (two-phase write pattern, PayloadService.WriteAt)
- `commit.go` handler (PayloadService.Flush, write verifier)
- `open_confirm.go` or in `open.go` (accept and echo stateid)
- Stateid type definitions in `types/types.go`
- Constants for OPEN share access/deny, stable_how4, rflags in `types/constants.go`

## Sources

### Primary (HIGH confidence)
- **RFC 7530** (https://www.rfc-editor.org/rfc/rfc7530.html) - NFSv4.0 protocol specification: OPEN, CLOSE, READ, WRITE, COMMIT, CREATE, REMOVE, READDIR, READLINK, ACCESS operation definitions
- **RFC 7531** (https://www.rfc-editor.org/rfc/rfc7531.html) - NFSv4.0 XDR descriptions: wire format for all operation args/results
- **Existing codebase** - Phase 6 handlers (v4/handlers/), v3 handlers (v3/handlers/), MetadataService (pkg/metadata/), PayloadService (pkg/payload/), Runtime (pkg/controlplane/runtime/)

### Secondary (MEDIUM confidence)
- Phase 6 RESEARCH.md and CONTEXT.md - Prior decisions and architectural patterns
- Linux nfsd source (https://github.com/torvalds/linux/tree/master/fs/nfs) - Reference implementation for OPEN behavior and special stateids

### Tertiary (LOW confidence)
- Web search results on NFSv4 minimal OPEN implementation - Community guidance, needs validation against RFC

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - all packages already exist in the codebase, no new dependencies
- Architecture: HIGH - follows established v3 handler patterns adapted for v4 CompoundContext
- Pitfalls: HIGH - derived from code analysis of Phase 6 patterns and RFC study
- OPEN state management: MEDIUM - placeholder approach is reasonable but needs Phase 9 validation
- Wire format: HIGH - verified against RFC 7530/7531

**Research date:** 2026-02-13
**Valid until:** 2026-03-15 (stable; RFC-based, codebase patterns well-established)

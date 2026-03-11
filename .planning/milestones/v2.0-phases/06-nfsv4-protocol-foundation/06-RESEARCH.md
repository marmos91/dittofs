# Phase 6: NFSv4 Protocol Foundation - Research

**Researched:** 2026-02-12
**Domain:** NFSv4 wire protocol (RFC 7530), COMPOUND dispatch, pseudo-filesystem, XDR types
**Confidence:** HIGH

## Summary

NFSv4.0 (RFC 7530) introduces a fundamentally different dispatch model from NFSv3. Instead of per-procedure RPC calls, NFSv4 has only two RPC procedures: NULL and COMPOUND. The COMPOUND procedure bundles multiple operations (LOOKUP, GETATTR, READ, etc.) into a single RPC, executing them sequentially with a shared filehandle context. This requires a new dispatch layer that iterates operations, maintains current/saved filehandle state, and stops on first error.

The pseudo-filesystem is NFSv4's replacement for the separate MOUNT protocol. It presents a virtual namespace where all exports appear under a single root, allowing clients to browse and traverse without separate mount calls. The pseudo-fs uses distinct fsid values so clients can detect filesystem boundary crossings.

DittoFS already has a well-structured protocol layer (dispatch tables, handler contexts, XDR encoding). The NFSv4 foundation fits naturally as a new version branch in the existing NFS adapter, sharing the same TCP port and RPC program number (100003) but with version=4. The CONTEXT.md decisions align well with the protocol requirements and the codebase architecture.

**Primary recommendation:** Build the COMPOUND dispatcher as a sequential loop over operations with a mutable CompoundContext struct, using the same map-based dispatch table pattern as v3. Implement pseudo-fs as a small in-memory tree that reflects runtime shares dynamically. Return NFS4ERR_NOTSUPP for all file operations in this phase -- real handlers come in Phase 7.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Extend the existing NFS adapter (not a separate adapter). Single adapter handles v3+v4 on the same port
- Keep port 12049 as default (no change to standard port)
- Version routing happens in `dispatch.go` -- check RPC version field, route to v3 or v4 dispatch table
- NLM/NSM/MOUNT programs stay always active regardless of NFS version
- Shared HandlerResult type between v3 and v4 (add v4-specific fields if needed)
- Minimal connection state struct for v4 (placeholder with client address) -- Phase 9 extends it
- Separate v4 handler struct in `internal/protocol/nfs/v4/handlers/`
- Direct mapping: share `/export` appears at pseudo-fs path `/export`
- Dynamic pseudo-fs: reflects runtime share additions/removals immediately
- Separate handle space for pseudo-fs: in-memory `map[path]handle`
- Show all shares in READDIR on pseudo-fs root
- PUTPUBFH = PUTROOTFH = pseudo-fs root (same handle)
- NFS4ERR_STALE for handles pointing to removed shares
- Pseudo-fs lives in `internal/protocol/nfs/v4/pseudofs/`
- Both v3 and v4 active simultaneously by default
- Validate minor version: accept minor=0 only, reject 1/2 with NFS4ERR_MINOR_VERS_MISMATCH
- Version range configured via control plane API at runtime (NOT in static config)
- Default: min=3, max=4
- Log at INFO level the first time a client uses v3 or v4
- Mutable CompoundContext struct passed by pointer through handlers
- Map-based dispatch table for COMPOUND sub-ops
- Op handlers receive raw XDR bytes + CompoundContext, return HandlerResult
- Tag field: echo as-is in response
- Define all NFSv4 attribute bits, return only supported ones in response bitmask
- Dynamic response buffer growth (bytes.Buffer approach)
- Mirror v3 structure: `internal/protocol/nfs/v4/handlers/` with types in `v4/types/`, attrs in `v4/attrs/`
- Common NFS errors package: `internal/protocol/nfs/errors/`
- Common package: `internal/protocol/nfs/common/` for shared types
- Use shared `internal/protocol/xdr/` for XDR primitives
- One handler per file
- Unit tests only for Phase 6
- Defer observability (metrics/tracing) to Phase 7+

### Claude's Discretion
- Virtual directory visibility in READDIR, filesystem boundary crossing mechanism (follow RFC 7530 Section 7.4)
- RPC-level response when client sends disallowed version
- COMPOUND op count limit, error tracking (op index in response)

### Deferred Ideas (OUT OF SCOPE)
- Portmapper auto-registration -- deferred to Phase 28.1
- NFSv4 observability (metrics/tracing) -- add when real operations exist (Phase 7+)
- Connection state for v4 sessions -- Phase 9 (State Management) extends the placeholder
</user_constraints>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `bytes` | N/A | Dynamic buffer growth for COMPOUND responses | Standard approach, avoids pre-allocation guessing |
| Go stdlib `unicode/utf8` | N/A | UTF-8 filename validation | Built-in, no external dependency needed |
| Go stdlib `sync` | N/A | Thread-safe pseudo-fs map | Already used throughout codebase |
| `internal/protocol/xdr/` | existing | XDR encode/decode primitives | Already shared between NFS/NLM, extend for v4 |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| Go stdlib `encoding/binary` | N/A | BigEndian XDR encoding | Already used in existing XDR package |
| Go stdlib `strings` | N/A | Path manipulation for pseudo-fs | Parsing export paths into tree nodes |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Custom XDR helpers | `github.com/rasky/go-xdr` | Existing custom XDR is well-tested, matches project style |
| In-memory pseudo-fs | Persistent pseudo-fs | Pseudo-fs is derived from runtime shares, no persistence needed |

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/
├── common/                     # NEW: Shared types between v3 and v4
│   ├── types.go               # Shared HandlerResult, auth context interface
│   └── errors.go              # Common error mapping interface
├── errors/                     # NEW: Common NFS error interface
│   └── errors.go              # NFS error codes interface, shared mapping
├── v3/
│   ├── handlers/              # Existing v3 handlers (unchanged)
│   └── types/                 # Existing v3 types (mostly unchanged)
├── v4/
│   ├── handlers/              # NEW: v4 operation handlers
│   │   ├── handler.go         # Handler struct (mirrors v3 doc.go)
│   │   ├── compound.go        # COMPOUND dispatcher loop
│   │   ├── context.go         # CompoundContext struct
│   │   ├── null.go            # NULL procedure handler
│   │   ├── putfh.go           # PUTFH operation
│   │   ├── putrootfh.go       # PUTROOTFH operation
│   │   ├── putpubfh.go        # PUTPUBFH operation
│   │   ├── getfh.go           # GETFH operation
│   │   ├── savefh.go          # SAVEFH operation
│   │   ├── restorefh.go       # RESTOREFH operation
│   │   ├── lookup.go          # LOOKUP operation (pseudo-fs aware)
│   │   ├── lookupp.go         # LOOKUPP operation (parent lookup)
│   │   ├── getattr.go         # GETATTR operation
│   │   ├── readdir.go         # READDIR operation (pseudo-fs entries)
│   │   ├── access.go          # ACCESS operation
│   │   └── illegal.go         # ILLEGAL operation handler
│   ├── types/                 # NEW: v4 constants and types
│   │   ├── constants.go       # NFSv4 operation numbers, error codes
│   │   ├── attrs.go           # FATTR4 attribute numbers and bitmask defs
│   │   └── types.go           # NFSv4-specific XDR structures
│   ├── attrs/                 # NEW: Attribute encoding/decoding
│   │   ├── bitmap.go          # Bitmap4 encode/decode helpers
│   │   └── encode.go          # Attribute value encoding
│   └── pseudofs/              # NEW: Pseudo-filesystem
│       ├── pseudofs.go        # PseudoFS tree implementation
│       └── pseudofs_test.go   # Unit tests
├── dispatch.go                # MODIFIED: Add v4 dispatch routing
└── ...                        # Existing files unchanged
```

### Pattern 1: COMPOUND Dispatcher Loop

**What:** Sequential iteration over operations in a single COMPOUND RPC, maintaining shared filehandle context across operations.

**When to use:** Processing every NFSv4 RPC (all v4 calls go through COMPOUND).

**Example:**
```go
// Source: RFC 7530 Section 16.2 + NFS-Ganesha nfs4_Compound.c pattern
type CompoundContext struct {
    // Mutable filehandle state
    CurrentFH   []byte  // Current filehandle (nil = no FH set)
    SavedFH     []byte  // Saved filehandle (for SAVEFH/RESTOREFH)

    // Auth context (from RPC call)
    ClientAddr  string
    AuthFlavor  uint32
    UID         *uint32
    GID         *uint32
    GIDs        []uint32

    // Go context for cancellation
    Context     context.Context

    // Minimal v4 connection state (placeholder for Phase 9)
    ClientState *V4ClientState
}

// CompoundResult holds the result of a single operation within COMPOUND
type CompoundResult struct {
    Status  uint32   // NFS4 status code
    OpCode  uint32   // Which operation this result is for
    Data    []byte   // XDR-encoded operation result
}

func (h *Handler) ProcessCompound(
    ctx *CompoundContext,
    tag string,
    minorVersion uint32,
    ops []RawOp,
) (*CompoundResponse, error) {
    // Validate minor version
    if minorVersion != 0 {
        return &CompoundResponse{
            Status: NFS4ERR_MINOR_VERS_MISMATCH,
            Tag:    tag,
        }, nil
    }

    results := make([]CompoundResult, 0, len(ops))
    var lastStatus uint32 = NFS4_OK

    for i, op := range ops {
        // Check context cancellation
        if ctx.Context.Err() != nil {
            break
        }

        // Dispatch to operation handler
        result := h.dispatchOp(ctx, op)
        results = append(results, result)
        lastStatus = result.Status

        // Stop on first error (RFC 7530 Section 16.2.3)
        if lastStatus != NFS4_OK {
            break
        }
    }

    return &CompoundResponse{
        Status:  lastStatus,
        Tag:     tag,     // Echo tag as-is
        Results: results,
    }, nil
}
```

### Pattern 2: Pseudo-Filesystem Tree

**What:** In-memory virtual namespace mapping export paths to filehandles, with dynamic updates from runtime share changes.

**When to use:** PUTROOTFH, PUTPUBFH, LOOKUP on pseudo-fs paths, READDIR on pseudo-fs directories.

**Example:**
```go
// Source: RFC 7530 Section 7.3

type PseudoFS struct {
    mu       sync.RWMutex
    root     *PseudoNode
    handles  map[string][]byte   // path -> handle
    paths    map[string]string   // handle(hex) -> path (reverse lookup)
    nextID   uint64              // handle ID counter
}

type PseudoNode struct {
    Name     string
    Path     string
    Handle   []byte
    Children map[string]*PseudoNode
    IsExport bool                // true = junction to real share
    ShareName string             // share name if IsExport
}

// IsPseudoFSHandle returns true if the handle belongs to the pseudo-fs
// (not to a real metadata store). Pseudo-fs handles have a distinctive
// prefix to distinguish them from real file handles.
func (pfs *PseudoFS) IsPseudoFSHandle(handle []byte) bool {
    return bytes.HasPrefix(handle, []byte("pseudofs:"))
}

// Rebuild recreates the pseudo-fs tree from current runtime shares.
// Called when shares are added/removed.
func (pfs *PseudoFS) Rebuild(shares []string) {
    pfs.mu.Lock()
    defer pfs.mu.Unlock()
    // Build tree from share paths, create intermediate directories
}
```

### Pattern 3: Version Routing in Dispatch

**What:** Route RPC calls to v3 or v4 handler based on the Version field in the RPC call message.

**When to use:** At the connection level in `handleRPCCall`, before dispatching to protocol handlers.

**Example:**
```go
// In nfs_connection.go handleRPCCall
case rpc.ProgramNFS:
    switch call.Version {
    case rpc.NFSVersion3:
        if !s.server.isVersionAllowed(3) {
            return c.handleDisallowedVersion(call, clientAddr)
        }
        replyData, err = c.handleNFSProcedure(ctx, call, procedureData, clientAddr)
    case rpc.NFSVersion4:
        if !s.server.isVersionAllowed(4) {
            return c.handleDisallowedVersion(call, clientAddr)
        }
        replyData, err = c.handleNFSv4Procedure(ctx, call, procedureData, clientAddr)
    default:
        return c.handleUnsupportedVersion(call, rpc.NFSVersion3, "NFS", clientAddr)
    }
```

### Anti-Patterns to Avoid

- **Parsing the entire COMPOUND upfront:** NFSv4 allows two-pass XDR decoding, but for DittoFS's incremental approach, decode each operation lazily within the loop. This is simpler and matches the "stop on first error" pattern without wasting work.
- **Sharing handler structs between v3 and v4:** The handler contexts and dispatch patterns are fundamentally different (per-procedure vs. COMPOUND). Keep them separate per CONTEXT.md decision.
- **Pre-allocating COMPOUND response buffer:** Response size is unpredictable. Use `bytes.Buffer` for dynamic growth as decided.
- **Putting pseudo-fs in pkg/ or making it reusable:** The pseudo-fs is NFSv4-specific. Keep it in `internal/protocol/nfs/v4/pseudofs/` as decided.
- **Implementing real file operations in Phase 6:** This phase ships a skeleton where COMPOUND works but most ops return NFS4ERR_NOTSUPP. Real operations come in Phase 7.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| XDR primitives | Custom XDR encoder | `internal/protocol/xdr/` (existing) | Already handles uint32/64, opaque, string, padding |
| UTF-8 validation | Custom UTF-8 checker | `unicode/utf8.Valid()` + null byte check | Go stdlib handles all edge cases correctly |
| Bitmap manipulation | Bit-level math inline | Dedicated `bitmap4` helpers in `v4/attrs/` | Bitmaps are multi-word ([]uint32), need clean abstraction |
| File handle generation | Random bytes | Deterministic prefix + counter for pseudo-fs | Pseudo-fs handles must be distinguishable from real handles |
| Error code mapping | Per-handler switch | Centralized `mapMetadataErrorToNFS4()` in common errors pkg | Same pattern as v3's `mapMetadataErrorToNFS()`, avoid duplication |

**Key insight:** The existing XDR package and error mapping patterns from v3 provide 80% of what's needed. The new code is mostly the COMPOUND loop, pseudo-fs tree, and v4-specific type definitions.

## Common Pitfalls

### Pitfall 1: Forgetting NFS4ERR_NOFILEHANDLE
**What goes wrong:** Operations that require a current filehandle (GETATTR, LOOKUP, READ, etc.) must check that CurrentFH is set. If a COMPOUND starts with GETATTR without first doing PUTFH/PUTROOTFH, the server must return NFS4ERR_NOFILEHANDLE.
**Why it happens:** In v3, the file handle is always in the request. In v4, it's implicit from the CompoundContext.
**How to avoid:** Add a `requireCurrentFH()` check at the start of every operation that needs it. Return NFS4ERR_NOFILEHANDLE (10020) if nil.
**Warning signs:** Clients getting NFS4ERR_INVAL when they should get NFS4ERR_NOFILEHANDLE.

### Pitfall 2: Tag Must Be Echoed Exactly
**What goes wrong:** The tag field in COMPOUND4args must be echoed byte-for-byte in COMPOUND4res. Some implementations modify or truncate it.
**Why it happens:** Tag is a UTF-8 string of arbitrary content. Treating it as Go string may cause issues with non-UTF-8 bytes.
**How to avoid:** Store tag as `[]byte` internally, echo it back without interpretation.
**Warning signs:** Clients failing to match responses to requests (tag is for client-side correlation).

### Pitfall 3: Response Must Include Results Up To and Including the Failed Operation
**What goes wrong:** When an operation fails, the response must contain results for all operations up to and including the failing one. Not fewer, not more.
**Why it happens:** Off-by-one in the dispatch loop.
**How to avoid:** Append each result immediately after execution. The loop exit naturally leaves the correct number of results.
**Warning signs:** Clients seeing wrong number of results in COMPOUND response.

### Pitfall 4: Pseudo-FS Handle Space Collision
**What goes wrong:** If pseudo-fs handles overlap with real file handles from the metadata store, operations route to the wrong handler.
**Why it happens:** Both produce opaque byte arrays that go through the same dispatch path.
**How to avoid:** Use a distinctive prefix for pseudo-fs handles (e.g., `"pseudofs:"`) that cannot appear in real handles (which use `"shareName:UUID"` format).
**Warning signs:** LOOKUP on pseudo-fs root returning real file attributes, or GETATTR on real files returning pseudo-fs directory attributes.

### Pitfall 5: Minor Version Validation Must Be in COMPOUND, Not at RPC Level
**What goes wrong:** Minor version mismatch (requesting minor=1 or minor=2 when only minor=0 is supported) must return NFS4ERR_MINOR_VERS_MISMATCH in the COMPOUND response, NOT as an RPC-level PROG_MISMATCH.
**Why it happens:** Confusing RPC version (always 4 for NFSv4) with minor version (0 for NFSv4.0).
**How to avoid:** Check `minorversion` field inside the COMPOUND handler, return error as the first (only) result in the response.
**Warning signs:** Clients getting RPC errors instead of NFS4ERR_MINOR_VERS_MISMATCH.

### Pitfall 6: NFSv4 Uses Same Program Number, Different Version
**What goes wrong:** NFSv4 uses program 100003 version 4 (same program as v3, different version). The RPC layer already handles this routing -- the version field in RPCCallMessage.Version distinguishes them.
**Why it happens:** Assuming NFSv4 needs a new program number or port.
**How to avoid:** Route on `call.Version == 4` in the existing program switch, same port.
**Warning signs:** NFSv4 clients not connecting because they're directed to wrong program/port.

### Pitfall 7: bitmap4 Is Variable-Length, Not Fixed
**What goes wrong:** NFSv4 bitmaps (`bitmap4`) are encoded as `uint32_t bitmap4<>` (variable-length array of uint32). Some implementations assume exactly 2 words.
**Why it happens:** Most attributes fit in 2 words (bits 0-63), but the XDR definition allows arbitrary length.
**How to avoid:** Encode/decode bitmap4 as a variable-length `[]uint32`. Use helper functions that handle any number of words.
**Warning signs:** Clients requesting attributes in word 2+ getting decode errors.

## Code Examples

### COMPOUND4args XDR Decoding
```go
// Source: RFC 7531 Section 17, COMPOUND4args definition
// struct COMPOUND4args {
//     utf8str_cs  tag;
//     uint32_t    minorversion;
//     nfs_argop4  argarray<>;
// };

func DecodeCompound4Args(reader io.Reader) (*Compound4Args, error) {
    // Decode tag (variable-length UTF-8 string)
    tag, err := xdr.DecodeOpaque(reader)
    if err != nil {
        return nil, fmt.Errorf("decode tag: %w", err)
    }

    // Decode minor version
    minorVersion, err := xdr.DecodeUint32(reader)
    if err != nil {
        return nil, fmt.Errorf("decode minorversion: %w", err)
    }

    // Decode operation count
    numOps, err := xdr.DecodeUint32(reader)
    if err != nil {
        return nil, fmt.Errorf("decode numops: %w", err)
    }

    // Decode each operation (opcode + args)
    ops := make([]RawOp, 0, numOps)
    for i := uint32(0); i < numOps; i++ {
        opcode, err := xdr.DecodeUint32(reader)
        if err != nil {
            return nil, fmt.Errorf("decode op %d opcode: %w", i, err)
        }
        ops = append(ops, RawOp{
            OpCode: opcode,
            // Args will be decoded lazily per-operation
        })
    }

    return &Compound4Args{
        Tag:          tag,
        MinorVersion: minorVersion,
        Ops:          ops,
    }, nil
}
```

### COMPOUND4res XDR Encoding
```go
// Source: RFC 7531 Section 17
// struct COMPOUND4res {
//     nfsstat4    status;         // status of last operation
//     utf8str_cs  tag;            // echoed from request
//     nfs_resop4  resarray<>;     // results array
// };

func EncodeCompound4Res(buf *bytes.Buffer, resp *CompoundResponse) error {
    // Encode overall status (status of last evaluated operation)
    if err := xdr.WriteUint32(buf, resp.Status); err != nil {
        return err
    }

    // Echo tag from request
    if err := xdr.WriteXDROpaque(buf, resp.Tag); err != nil {
        return err
    }

    // Encode results count
    if err := xdr.WriteUint32(buf, uint32(len(resp.Results))); err != nil {
        return err
    }

    // Encode each result (opcode + status + result data)
    for _, result := range resp.Results {
        if err := xdr.WriteUint32(buf, result.OpCode); err != nil {
            return err
        }
        // Write pre-encoded operation result (includes status)
        if _, err := buf.Write(result.Data); err != nil {
            return err
        }
    }

    return nil
}
```

### NFSv4 Error Code Mapping
```go
// Source: RFC 7530 Section 13 + Linux kernel include/linux/nfs4.h

// mapMetadataErrorToNFS4 maps internal metadata errors to NFSv4 status codes.
// Many codes are identical to v3, but v4 adds specific codes.
func mapMetadataErrorToNFS4(err error) uint32 {
    var storeErr *metadata.StoreError
    if !errors.As(err, &storeErr) {
        return NFS4ERR_SERVERFAULT
    }

    switch storeErr.Code {
    case metadata.ErrNotFound:
        return NFS4ERR_NOENT          // 2
    case metadata.ErrAccessDenied, metadata.ErrAuthRequired:
        return NFS4ERR_ACCESS         // 13
    case metadata.ErrPermissionDenied:
        return NFS4ERR_PERM           // 1
    case metadata.ErrAlreadyExists:
        return NFS4ERR_EXIST          // 17
    case metadata.ErrNotEmpty:
        return NFS4ERR_NOTEMPTY       // 66
    case metadata.ErrIsDirectory:
        return NFS4ERR_ISDIR          // 21
    case metadata.ErrNotDirectory:
        return NFS4ERR_NOTDIR         // 20
    case metadata.ErrInvalidArgument:
        return NFS4ERR_INVAL          // 22
    case metadata.ErrNoSpace:
        return NFS4ERR_NOSPC          // 28
    case metadata.ErrQuotaExceeded:
        return NFS4ERR_DQUOT          // 69
    case metadata.ErrReadOnly:
        return NFS4ERR_ROFS           // 30
    case metadata.ErrNotSupported:
        return NFS4ERR_NOTSUPP        // 10004
    case metadata.ErrStaleHandle:
        return NFS4ERR_STALE          // 70
    case metadata.ErrInvalidHandle:
        return NFS4ERR_BADHANDLE      // 10001
    case metadata.ErrNameTooLong:
        return NFS4ERR_NAMETOOLONG    // 63
    case metadata.ErrLocked:
        return NFS4ERR_LOCKED         // 10012
    case metadata.ErrDeadlock:
        return NFS4ERR_DEADLOCK       // 10045
    case metadata.ErrGracePeriod:
        return NFS4ERR_GRACE          // 10013
    case metadata.ErrIOError:
        return NFS4ERR_IO             // 5
    default:
        return NFS4ERR_SERVERFAULT    // 10006
    }
}
```

### Pseudo-FS Handle Distinction
```go
// Pseudo-fs handles use a distinctive prefix that cannot appear in
// real file handles. Real handles use "shareName:UUID" format.
// Pseudo-fs handles use "pseudofs:" prefix + path hash.

const pseudoFSHandlePrefix = "pseudofs:"

func MakePseudoFSHandle(path string) []byte {
    // NFSv4 max handle size is 128 bytes (NFS4_FHSIZE)
    // Real handles in DittoFS are max 64 bytes (NFSv3 limit)
    // Pseudo-fs handles: "pseudofs:" (9 bytes) + path (up to 119 bytes)
    handle := pseudoFSHandlePrefix + path
    if len(handle) > 128 {
        // Hash long paths to fit within NFS4_FHSIZE
        h := sha256.Sum256([]byte(path))
        handle = pseudoFSHandlePrefix + hex.EncodeToString(h[:16])
    }
    return []byte(handle)
}

func IsPseudoFSHandle(handle []byte) bool {
    return bytes.HasPrefix(handle, []byte(pseudoFSHandlePrefix))
}
```

### UTF-8 Filename Validation
```go
// Source: RFC 7530 Section 12.7
// NFSv4 requires UTF-8 encoded filenames. The server SHOULD reject
// filenames containing invalid UTF-8 sequences.

func ValidateUTF8Filename(name string) uint32 {
    // Check for empty name
    if len(name) == 0 {
        return NFS4ERR_INVAL
    }

    // Check valid UTF-8
    if !utf8.ValidString(name) {
        return NFS4ERR_BADCHAR
    }

    // Check for null bytes
    if strings.ContainsRune(name, 0) {
        return NFS4ERR_BADCHAR
    }

    // Check for path separators (/ is not allowed in component names)
    if strings.ContainsRune(name, '/') {
        return NFS4ERR_BADNAME
    }

    // Check name length (per RFC 7530, typically 255 bytes max)
    if len(name) > 255 {
        return NFS4ERR_NAMETOOLONG
    }

    return NFS4_OK
}
```

### Bitmap4 Encode/Decode
```go
// Source: RFC 7531 - bitmap4 is typedef uint32_t bitmap4<>

func EncodeBitmap4(buf *bytes.Buffer, bitmap []uint32) error {
    // Write number of words
    if err := xdr.WriteUint32(buf, uint32(len(bitmap))); err != nil {
        return err
    }
    // Write each word
    for _, word := range bitmap {
        if err := xdr.WriteUint32(buf, word); err != nil {
            return err
        }
    }
    return nil
}

func DecodeBitmap4(reader io.Reader) ([]uint32, error) {
    numWords, err := xdr.DecodeUint32(reader)
    if err != nil {
        return nil, err
    }
    if numWords > 8 { // Reasonable limit
        return nil, fmt.Errorf("bitmap4 too large: %d words", numWords)
    }
    bitmap := make([]uint32, numWords)
    for i := uint32(0); i < numWords; i++ {
        bitmap[i], err = xdr.DecodeUint32(reader)
        if err != nil {
            return nil, err
        }
    }
    return bitmap, nil
}

// IsBitSet checks if a specific attribute bit is set in the bitmap
func IsBitSet(bitmap []uint32, bit uint32) bool {
    word := bit / 32
    if word >= uint32(len(bitmap)) {
        return false
    }
    return bitmap[word]&(1<<(bit%32)) != 0
}

// SetBit sets a specific attribute bit in the bitmap
func SetBit(bitmap []uint32, bit uint32) []uint32 {
    word := bit / 32
    for uint32(len(bitmap)) <= word {
        bitmap = append(bitmap, 0)
    }
    bitmap[word] |= 1 << (bit % 32)
    return bitmap
}
```

## NFSv4 Protocol Reference

### Program and Version Numbers
| Constant | Value | Notes |
|----------|-------|-------|
| Program NFS | 100003 | Same as NFSv3, shared port |
| Version NFS4 | 4 | RPC version field |
| NFSPROC4_NULL | 0 | Ping/connectivity test |
| NFSPROC4_COMPOUND | 1 | The only real procedure |
| NFS4_FHSIZE | 128 | Maximum file handle size (bytes) |
| NFS4_MINOR_VERSION | 0 | Only minor version supported in Phase 6 |

### NFSv4.0 Operation Numbers (Complete)
| Operation | Number | Phase 6 Status | Notes |
|-----------|--------|----------------|-------|
| OP_ACCESS | 3 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_CLOSE | 4 | NFS4ERR_NOTSUPP | Needs OPEN state (Phase 9) |
| OP_COMMIT | 5 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_CREATE | 6 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_DELEGPURGE | 7 | NFS4ERR_NOTSUPP | Delegation (Phase 10+) |
| OP_DELEGRETURN | 8 | NFS4ERR_NOTSUPP | Delegation (Phase 10+) |
| OP_GETATTR | 9 | Implement | Core for pseudo-fs browsing |
| OP_GETFH | 10 | Implement | Returns current filehandle |
| OP_LINK | 11 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_LOCK | 12 | NFS4ERR_NOTSUPP | Locking (Phase 9) |
| OP_LOCKT | 13 | NFS4ERR_NOTSUPP | Locking (Phase 9) |
| OP_LOCKU | 14 | NFS4ERR_NOTSUPP | Locking (Phase 9) |
| OP_LOOKUP | 15 | Implement | Core for pseudo-fs traversal |
| OP_LOOKUPP | 16 | Implement | Parent directory lookup |
| OP_NVERIFY | 17 | NFS4ERR_NOTSUPP | Cache validation |
| OP_OPEN | 18 | NFS4ERR_NOTSUPP | Needs state (Phase 9) |
| OP_OPENATTR | 19 | NFS4ERR_NOTSUPP | Named attributes |
| OP_OPEN_CONFIRM | 20 | NFS4ERR_NOTSUPP | Needs OPEN (Phase 9) |
| OP_OPEN_DOWNGRADE | 21 | NFS4ERR_NOTSUPP | Needs OPEN (Phase 9) |
| OP_PUTFH | 22 | Implement | Sets current FH from arg |
| OP_PUTPUBFH | 23 | Implement | Sets current FH to root |
| OP_PUTROOTFH | 24 | Implement | Sets current FH to root |
| OP_READ | 25 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_READDIR | 26 | Implement | Pseudo-fs directory listing |
| OP_READLINK | 27 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_REMOVE | 28 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_RENAME | 29 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_RENEW | 30 | NFS4ERR_NOTSUPP | Lease renewal (Phase 9) |
| OP_RESTOREFH | 31 | Implement | Restores saved FH |
| OP_SAVEFH | 32 | Implement | Saves current FH |
| OP_SECINFO | 33 | NFS4ERR_NOTSUPP | Security negotiation |
| OP_SETATTR | 34 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_SETCLIENTID | 35 | Stub (minimal) | Required for v4 clients to connect |
| OP_SETCLIENTID_CONFIRM | 36 | Stub (minimal) | Required for v4 clients to connect |
| OP_VERIFY | 37 | NFS4ERR_NOTSUPP | Cache validation |
| OP_WRITE | 38 | NFS4ERR_NOTSUPP | Implement in Phase 7 |
| OP_RELEASE_LOCKOWNER | 39 | NFS4ERR_NOTSUPP | Locking (Phase 9) |
| OP_ILLEGAL | 10044 | Implement | Required - returns NFS4ERR_OP_ILLEGAL |

### NFSv4.0 Error Codes (Complete for Phase 6)
| Error Code | Value | Internal Mapping | Notes |
|------------|-------|------------------|-------|
| NFS4_OK | 0 | success | |
| NFS4ERR_PERM | 1 | ErrPermissionDenied | |
| NFS4ERR_NOENT | 2 | ErrNotFound | |
| NFS4ERR_IO | 5 | ErrIOError | |
| NFS4ERR_NXIO | 6 | - | Device not found |
| NFS4ERR_ACCESS | 13 | ErrAccessDenied | |
| NFS4ERR_EXIST | 17 | ErrAlreadyExists | |
| NFS4ERR_XDEV | 18 | - | Cross-device link |
| NFS4ERR_NOTDIR | 20 | ErrNotDirectory | |
| NFS4ERR_ISDIR | 21 | ErrIsDirectory | |
| NFS4ERR_INVAL | 22 | ErrInvalidArgument | |
| NFS4ERR_FBIG | 27 | - | File too large |
| NFS4ERR_NOSPC | 28 | ErrNoSpace | |
| NFS4ERR_ROFS | 30 | ErrReadOnly | |
| NFS4ERR_MLINK | 31 | - | Too many links |
| NFS4ERR_NAMETOOLONG | 63 | ErrNameTooLong | |
| NFS4ERR_NOTEMPTY | 66 | ErrNotEmpty | |
| NFS4ERR_DQUOT | 69 | ErrQuotaExceeded | |
| NFS4ERR_STALE | 70 | ErrStaleHandle | |
| NFS4ERR_BADHANDLE | 10001 | ErrInvalidHandle | |
| NFS4ERR_BAD_COOKIE | 10003 | - | READDIR cookie |
| NFS4ERR_NOTSUPP | 10004 | ErrNotSupported | Default for unimplemented ops |
| NFS4ERR_TOOSMALL | 10005 | - | Buffer too small |
| NFS4ERR_SERVERFAULT | 10006 | - | Internal server error |
| NFS4ERR_BADTYPE | 10007 | - | Invalid file type |
| NFS4ERR_DELAY | 10008 | - | Retry later (replaces NFS3ERR_JUKEBOX) |
| NFS4ERR_SAME | 10009 | - | VERIFY: attrs match |
| NFS4ERR_DENIED | 10010 | - | Lock denied |
| NFS4ERR_EXPIRED | 10011 | - | State expired |
| NFS4ERR_LOCKED | 10012 | ErrLocked | File locked |
| NFS4ERR_GRACE | 10013 | ErrGracePeriod | Grace period |
| NFS4ERR_FHEXPIRED | 10014 | - | Handle expired (volatile) |
| NFS4ERR_SHARE_DENIED | 10015 | - | OPEN share conflict |
| NFS4ERR_WRONGSEC | 10016 | - | Wrong security flavor |
| NFS4ERR_CLID_INUSE | 10017 | - | Client ID conflict |
| NFS4ERR_RESOURCE | 10018 | - | Server resource limit |
| NFS4ERR_MOVED | 10019 | - | Export moved |
| NFS4ERR_NOFILEHANDLE | 10020 | - | No current FH set |
| NFS4ERR_MINOR_VERS_MISMATCH | 10021 | - | Wrong minor version |
| NFS4ERR_STALE_CLIENTID | 10022 | - | Client ID stale |
| NFS4ERR_STALE_STATEID | 10023 | - | State ID stale |
| NFS4ERR_OLD_STATEID | 10024 | - | State ID outdated |
| NFS4ERR_BAD_STATEID | 10025 | - | Invalid state ID |
| NFS4ERR_BAD_SEQID | 10026 | - | Sequence mismatch |
| NFS4ERR_NOT_SAME | 10027 | - | NVERIFY: attrs differ |
| NFS4ERR_LOCK_RANGE | 10028 | - | Lock range not supported |
| NFS4ERR_SYMLINK | 10029 | - | Op on symlink |
| NFS4ERR_RESTOREFH | 10030 | - | No saved FH for RESTOREFH |
| NFS4ERR_LEASE_MOVED | 10031 | - | Lease moved to other server |
| NFS4ERR_ATTRNOTSUPP | 10032 | - | Attribute not supported |
| NFS4ERR_NO_GRACE | 10033 | - | No grace period |
| NFS4ERR_RECLAIM_BAD | 10034 | - | Reclaim failed |
| NFS4ERR_RECLAIM_CONFLICT | 10035 | - | Reclaim conflict |
| NFS4ERR_BADXDR | 10036 | - | Bad XDR data |
| NFS4ERR_LOCKS_HELD | 10037 | - | Cannot close, locks held |
| NFS4ERR_OPENMODE | 10038 | - | Wrong open mode |
| NFS4ERR_BADOWNER | 10039 | - | Invalid owner string |
| NFS4ERR_BADCHAR | 10040 | - | Invalid UTF-8 char |
| NFS4ERR_BADNAME | 10041 | - | Invalid filename |
| NFS4ERR_BAD_RANGE | 10042 | - | Invalid byte range |
| NFS4ERR_LOCK_NOTSUPP | 10043 | - | Lock not supported |
| NFS4ERR_OP_ILLEGAL | 10044 | - | Unknown operation |
| NFS4ERR_DEADLOCK | 10045 | ErrDeadlock | Lock deadlock |
| NFS4ERR_FILE_OPEN | 10046 | - | File is open |
| NFS4ERR_ADMIN_REVOKED | 10047 | - | Admin revoked access |
| NFS4ERR_CB_PATH_DOWN | 10048 | - | Callback path down |

### FATTR4 Mandatory Attributes (REQUIRED)
| Attribute | Bit | Type | Phase 6 Support |
|-----------|-----|------|-----------------|
| FATTR4_SUPPORTED_ATTRS | 0 | bitmap4 | Yes - return what we support |
| FATTR4_TYPE | 1 | nfs_ftype4 | Yes - NF4DIR for pseudo-fs nodes |
| FATTR4_FH_EXPIRE_TYPE | 2 | uint32 | Yes - FH4_PERSISTENT |
| FATTR4_CHANGE | 3 | changeid4 (uint64) | Yes - monotonic counter |
| FATTR4_SIZE | 4 | uint64 | Yes - 0 for directories |
| FATTR4_LINK_SUPPORT | 5 | bool | Yes - true |
| FATTR4_SYMLINK_SUPPORT | 6 | bool | Yes - true |
| FATTR4_NAMED_ATTR | 7 | bool | Yes - false |
| FATTR4_FSID | 8 | fsid4 {major, minor} | Yes - unique per share, special for pseudo-fs |
| FATTR4_UNIQUE_HANDLES | 9 | bool | Yes - true |
| FATTR4_LEASE_TIME | 10 | uint32 (seconds) | Yes - configurable |
| FATTR4_RDATTR_ERROR | 11 | nfsstat4 | Yes - NFS4_OK |
| FATTR4_FILEHANDLE | 19 | nfs_fh4 | Yes - return current FH |

### Key Differences from NFSv3

| Aspect | NFSv3 | NFSv4 |
|--------|-------|-------|
| RPC Procedures | 22 per-operation procedures | 2 procedures (NULL + COMPOUND) |
| Mounting | Separate MOUNT protocol (program 100005) | Built into protocol (PUTROOTFH + LOOKUP) |
| File handles | In every request | Implicit from CompoundContext |
| Max handle size | 64 bytes (NFS3_FHSIZE) | 128 bytes (NFS4_FHSIZE) |
| Error codes | NFS3ERR_* (fewer) | NFS4ERR_* (many more, incl. state errors) |
| Namespace | Per-export (each mount is independent) | Unified pseudo-filesystem |
| Port | 2049 + mount port + NLM + NSM | 2049 only (no auxiliary protocols) |
| State | Stateless | Stateful (client IDs, open state, locks) |
| Attributes | Fixed set per procedure | Bitmap-based GETATTR/SETATTR |
| Filenames | Bytes (opaque) | UTF-8 required |

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| RFC 3530 (NFSv4.0 original) | RFC 7530 (NFSv4.0 revised) | 2015 | XDR moved to companion RFC 7531, clarifications |
| Separate MOUNT + NFS | Single unified protocol | NFSv4 (2000) | Simpler firewall rules, one port |
| stringprep (RFC 3454) | Practical UTF-8 validation | Most implementations | Full stringprep rarely enforced; basic UTF-8 validation is standard |

**Deprecated/outdated:**
- RFC 3530: Superseded by RFC 7530. Use RFC 7530 for all NFSv4.0 references.
- RFC 3010: Original NFSv4 spec, long superseded.

## Open Questions

1. **SETCLIENTID/SETCLIENTID_CONFIRM Stubs**
   - What we know: NFSv4 clients must establish a client ID before doing meaningful work. Linux's mount.nfs4 sends SETCLIENTID early in the mount sequence.
   - What's unclear: How minimal can the stub be? Can we return NFS4_OK with a dummy client ID, or will clients reject it?
   - Recommendation: Implement minimal stubs that return NFS4_OK with a generated client ID and verifier. This is sufficient for Phase 6 testing. Phase 9 will implement proper state management. Test with Linux mount.nfs4 to verify the stub works.

2. **macOS NFSv4 Kernel Bug Workaround**
   - What we know: The existing code closes the connection for NFSv4 requests to avoid a macOS kernel panic. When v4 support is added, this workaround must be removed.
   - What's unclear: Is the macOS kernel bug fixed in recent versions? Does it only affect PROG_MISMATCH replies?
   - Recommendation: Remove the workaround when v4 is enabled. Keep it only when v4 is disabled via version range config. Add a comment explaining why.

3. **Pseudo-FS FSID Value**
   - What we know: The pseudo-fs needs a unique fsid that differs from all real exports. Linux nfsd uses {0, 0} for pseudo-fs and {dev, ino} for real exports.
   - What's unclear: Whether DittoFS should follow the same convention or use a different scheme.
   - Recommendation: Use fsid {0, 1} for pseudo-fs (major=0, minor=1). Use {shareIndex, 0} for real exports. This ensures uniqueness and follows convention.

4. **COMPOUND Op Count Limit (Claude's Discretion)**
   - What we know: RFC 7530 allows implementations to impose limits on COMPOUND size.
   - Recommendation: Limit to 128 operations per COMPOUND (Linux nfsd uses a similar limit). Return NFS4ERR_RESOURCE if exceeded. This prevents memory exhaustion while being generous for normal workloads.

## Sources

### Primary (HIGH confidence)
- [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530.html) - NFSv4.0 protocol specification (protocol semantics, COMPOUND, pseudo-fs, error codes)
- [RFC 7531](https://www.rfc-editor.org/rfc/rfc7531.html) - NFSv4.0 XDR definitions (COMPOUND4args/res, nfs_argop4/resop4, nfsstat4, fattr4)
- [Linux kernel nfs4.h](https://github.com/torvalds/linux/blob/master/include/linux/nfs4.h) - Operation numbers, error codes (verified against RFC)
- [libnfs nfs4.x](https://github.com/sahlberg/libnfs/blob/master/nfs4/nfs4.x) - Complete XDR definitions with all error codes and attribute numbers
- DittoFS codebase - Existing v3 dispatch pattern, XDR package, handler context, adapter architecture

### Secondary (MEDIUM confidence)
- [NFS-Ganesha nfs4_Compound.c](https://github.com/phdeniel/nfs-ganesha/blob/master/src/Protocols/NFS/nfs4_Compound.c) - COMPOUND dispatch loop pattern verification
- [Buildbarn NFSv4 ADR](https://github.com/buildbarn/bb-adrs/blob/main/0009-nfsv4.md) - Go-specific NFSv4 design patterns and decisions
- [Dell PowerScale NFSv4 guide](https://infohub.delltechnologies.com/en-us/l/powerscale-onefs-nfs-design-considerations-and-best-practices-3/nfsv4-x-pseudo-file-system-1-1/1/) - Pseudo-filesystem fsid behavior

### Tertiary (LOW confidence)
- [libnfs-go](https://pkg.go.dev/github.com/smallfz/libnfs-go/server) - Go NFSv4 server (limited documentation, used for pattern reference only)
- [GoNFS4](https://github.com/KaustubhDhokte/GoNFS4) - Go NFSv4 server (academic project, not production-verified)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Using existing codebase patterns and Go stdlib only
- Architecture: HIGH - COMPOUND model well-defined in RFC 7530, verified against NFS-Ganesha and Linux nfsd
- XDR types: HIGH - Directly from RFC 7531 and libnfs reference implementation
- Error codes: HIGH - Complete list verified against Linux kernel header and libnfs XDR
- Pseudo-fs design: HIGH - RFC 7530 Section 7 is clear; CONTEXT.md decisions are sound
- UTF-8 validation: MEDIUM - RFC 7530 Section 12 references stringprep but practical implementations use basic UTF-8 validation
- SETCLIENTID stubs: MEDIUM - Minimal stub approach needs testing with real clients

**Research date:** 2026-02-12
**Valid until:** 2026-04-12 (NFSv4.0 is a stable, mature protocol; RFCs will not change)

# Phase 09: Adapter layer cleanup (ADAPT) - Pattern Map

**Mapped:** 2026-04-24
**Files analyzed:** 21 (5 new, 13 modify, 3 doc)
**Analogs found:** 21 / 21

## File Classification

### NEW files (`internal/adapter/common/`)

| New File | Role | Data Flow | Closest Analog | Match Quality |
|----------|------|-----------|----------------|---------------|
| `internal/adapter/common/resolve.go` | utility (shared helpers + narrow interfaces) | request-response resolution | `internal/adapter/nfs/v3/handlers/utils.go:56-60` + `internal/adapter/nfs/v4/handlers/helpers.go:100-106` | exact (merges two 3-line duplicates) |
| `internal/adapter/common/read_payload.go` | utility (pooled read-result + block-store read) | file-I/O via pool | `internal/adapter/nfs/v3/handlers/read_payload.go:16-70` | exact (verbatim move per D-04) |
| `internal/adapter/common/errmap.go` | utility (struct-per-code mapping table) | transform (metadata.ErrorCode → protocol code) | `internal/adapter/nfs/v3/handlers/create.go:577-621` + `internal/adapter/smb/v2/handlers/converters.go:354-395` + `internal/adapter/nfs/v4/types/errors.go:18-70` | exact (three-way merge) |
| `internal/adapter/common/content_errmap.go` | utility (content/block-store error table) | transform | `internal/adapter/smb/v2/handlers/converters.go:397-406` + `internal/adapter/nfs/xdr/errors.go:184-231` | exact |
| `internal/adapter/common/lock_errmap.go` | utility (lock-specific error translator) | transform | `internal/adapter/smb/v2/handlers/lock.go:533-549` (`lockErrorToStatus`) | exact (NFS lock mapping currently lives inline in `xdr/errors.go:140-146` + `v4/types/errors.go:59-64`) |

### MODIFY files (call-site migration)

| Modified File | Role | Data Flow | Closest Analog | Match Quality |
|---------------|------|-----------|----------------|---------------|
| `internal/adapter/nfs/v3/handlers/utils.go` | utility | N/A (delete lines 56-60) | — | deletion |
| `internal/adapter/nfs/v3/handlers/read_payload.go` | utility | N/A (delete — moved to common) | — | deletion |
| `internal/adapter/nfs/v3/handlers/read.go` | handler | file-I/O request-response | `internal/adapter/nfs/v3/handlers/read.go:144,237` (current call sites) | in-place edit |
| `internal/adapter/nfs/v3/handlers/write.go` | handler | CRUD request-response | `internal/adapter/nfs/v3/handlers/write.go:163,243` | in-place edit |
| `internal/adapter/nfs/v3/handlers/commit.go` | handler | request-response | `internal/adapter/nfs/v3/handlers/commit.go:116` | in-place edit |
| `internal/adapter/nfs/v3/handlers/create.go` | handler | CRUD | `internal/adapter/nfs/v3/handlers/create.go:579` (delete `mapMetadataErrorToNFS`) | in-place edit |
| `internal/adapter/nfs/v4/handlers/helpers.go` | utility | N/A (delete lines 100-107) | — | deletion |
| `internal/adapter/nfs/v4/handlers/read.go` | handler | file-I/O | `internal/adapter/nfs/v4/handlers/read.go:148` + `:157` (add pool on read) | in-place edit |
| `internal/adapter/nfs/v4/handlers/write.go` | handler | CRUD | `internal/adapter/nfs/v4/handlers/write.go:185,210` | in-place edit |
| `internal/adapter/nfs/v4/handlers/commit.go` | handler | request-response | `internal/adapter/nfs/v4/handlers/commit.go:101` | in-place edit |
| `internal/adapter/nfs/v4/types/errors.go` | utility | N/A (replace `MapMetadataErrorToNFS4` body with thin wrapper over `common.MapToNFS4`) | — | shrink |
| `internal/adapter/nfs/xdr/errors.go` | utility | transform | `MapStoreErrorToNFSStatus` — migrate body to `common.MapToNFS3`; keep as audit-logging wrapper | in-place edit |
| `internal/adapter/smb/v2/handlers/read.go` | handler | file-I/O (pool integration D-09/10) | `internal/adapter/smb/v2/handlers/read.go:234,342-348` | in-place edit |
| `internal/adapter/smb/v2/handlers/write.go` | handler | CRUD | `internal/adapter/smb/v2/handlers/write.go:292,349,371` | in-place edit |
| `internal/adapter/smb/v2/handlers/close.go` | handler | request-response | `close.go:183,518,558` | in-place edit |
| `internal/adapter/smb/v2/handlers/converters.go` | utility | N/A (delete `MetadataErrorToSMBStatus` + `ContentErrorToSMBStatus` at lines 354-406) | — | deletion |
| `internal/adapter/smb/v2/handlers/lock.go` | handler | request-response | `lock.go:533-549` (replace `lockErrorToStatus` with `common.MapLockToSMB`) | in-place edit |
| `internal/adapter/smb/response.go` | utility (encoder wrapper) | D-09 release hook | `response.go:414-421` (`SendResponseWithHooks`) | in-place edit |
| `internal/adapter/smb/v2/handlers/result.go` | utility | Add `ReleaseData func()` to `HandlerResult` (D-09) | `result.go:57-88` | in-place edit |
| `internal/adapter/smb/framing.go` / `compound.go` | framing | release-after-write invocation | `framing.go:258-284` (`WriteNetBIOSFrame`) | in-place edit |

### TEST files

| Test File | Role | Data Flow | Closest Analog | Match Quality |
|-----------|------|-----------|----------------|---------------|
| `test/e2e/cross_protocol_test.go` (extend) | e2e test | table-driven | `test/e2e/cross_protocol_test.go:29-128` (XPR-01..06) | exact pattern |
| `internal/adapter/common/errmap_test.go` (NEW) | unit test | table-driven | `internal/adapter/nfs/v4/types/constants_test.go:152-278` (`TestMapMetadataErrorToNFS4`) | exact |

### DOC files

| Doc | Section |
|-----|---------|
| `docs/ARCHITECTURE.md` | Add `internal/adapter/common/` to dir map + Adapter-layer subsection |
| `docs/NFS.md` | "Error mapping" pointer to `common/` |
| `docs/SMB.md` | "Error mapping" + pool notes |
| `docs/CONTRIBUTING.md` | (Claude's discretion per D-17) "Adding a new metadata.ErrorCode" recipe |

---

## Pattern Assignments

### `internal/adapter/common/resolve.go` (NEW — utility, resolve+narrow interfaces)

**Analog A:** `internal/adapter/nfs/v3/handlers/utils.go:56-60` (NFSv3 3-liner)
**Analog B:** `internal/adapter/nfs/v4/handlers/helpers.go:100-106` (NFSv4 3-liner)
**Canonical call site C:** `internal/adapter/smb/v2/handlers/read.go:234` (SMB inline)

**Existing NFSv3 wrapper** (`utils.go:56-60`):
```go
// getBlockStoreForHandle returns the per-share block store resolved from the given file handle.
// The handle encodes the share name, which is used to look up the share's block store.
func getBlockStoreForHandle(reg *runtime.Runtime, ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error) {
    return reg.GetBlockStoreForHandle(ctx, handle)
}
```

**Existing NFSv4 wrapper** (`v4/handlers/helpers.go:102-107`):
```go
func getBlockStoreForHandle(h *Handler, ctx context.Context, handle []byte) (*engine.BlockStore, error) {
    if h.Registry == nil {
        return nil, fmt.Errorf("no registry configured")
    }
    return h.Registry.GetBlockStoreForHandle(ctx, metadata.FileHandle(handle))
}
```

**Existing SMB inline** (`smb/v2/handlers/read.go:234`):
```go
blockStore, err := h.Registry.GetBlockStoreForHandle(ctx.Context, openFile.MetadataHandle)
```

**Target shape in `common/resolve.go` (per D-01, D-02, D-03)**:
```go
package common

import (
    "context"

    "github.com/marmos91/dittofs/pkg/blockstore/engine"
    "github.com/marmos91/dittofs/pkg/metadata"
)

// BlockStoreRegistry is the narrow interface satisfied implicitly by
// *runtime.Runtime (see pkg/controlplane/runtime/runtime.go:341).
// Declared here (not in runtime) so common/ does not import runtime
// and stays testable with mocks.
type BlockStoreRegistry interface {
    GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error)
}

// ResolveForRead returns the per-share BlockStore for the handle.
// Read-only resolution; no permission side effects.
func ResolveForRead(ctx context.Context, reg BlockStoreRegistry, handle metadata.FileHandle) (*engine.BlockStore, error) {
    return reg.GetBlockStoreForHandle(ctx, handle)
}

// ResolveForWrite is the write-path twin. Identical today; separate
// name preserves the call-site seam for Phase 12 (API-01) when the
// signatures diverge to take []BlockRef.
func ResolveForWrite(ctx context.Context, reg BlockStoreRegistry, handle metadata.FileHandle) (*engine.BlockStore, error) {
    return reg.GetBlockStoreForHandle(ctx, handle)
}
```

**Also lives in this file (per D-01 "narrow MetadataService"):** A narrow `MetadataService` interface covering just the methods helpers use. The concrete `*metadata.MetadataService` struct at `pkg/metadata/errors.go` and friends satisfies it implicitly. Planner audits call sites to pick the minimum set — probable subset: `PrepareRead`, `PrepareWrite`, `GetFile`, `CheckLockForIO`, `FlushPendingWriteForFile`, `SetFileAttributes`.

**Gotcha:** `*runtime.Runtime.GetBlockStoreForHandle` signature at `pkg/controlplane/runtime/runtime.go:341` already matches — no runtime change needed; the interface is satisfied implicitly.

---

### `internal/adapter/common/read_payload.go` (NEW — utility, pooled file-I/O)

**Analog (verbatim source per D-04):** `internal/adapter/nfs/v3/handlers/read_payload.go:1-70`

**Existing NFSv3 canonical body**:
```go
// blockReadResult holds the result of reading from the block store.
type blockReadResult struct {
    data []byte
    eof  bool
}

// Release returns the data buffer to the pool.
// Must be called after the data is no longer needed (e.g., after encoding).
func (r *blockReadResult) Release() {
    if r.data != nil {
        pool.Put(r.data)
        r.data = nil
    }
}

// readFromBlockStore reads data using the BlockStore ReadAt method.
func readFromBlockStore(
    ctx *NFSHandlerContext,  // BREAKS IN common/: NFSHandlerContext is v3-only
    blockStore *engine.BlockStore,
    payloadID metadata.PayloadID,
    offset uint64,
    count uint32,
    clientIP string,
    handle []byte,
) (blockReadResult, error) {
    ...
    data := pool.Get(int(count))
    n, readErr := blockStore.ReadAt(ctx.Context, string(payloadID), data, offset)

    if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
        return blockReadResult{data: data[:n], eof: true}, nil
    }

    if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
        pool.Put(data)
        return blockReadResult{}, readErr
    }

    if readErr != nil {
        pool.Put(data)
        return blockReadResult{}, fmt.Errorf("ReadAt error: %w", readErr)
    }

    return blockReadResult{data: data[:n], eof: false}, nil
}
```

**Target shape** — per D-03 (`AuthContext` passed explicitly) and D-04 (Release pattern preserved):
```go
package common

// BlockReadResult holds a pooled read buffer and EOF flag.
// Callers MUST invoke Release() after the data is no longer needed
// (typically after wire encoding; SMB hands Release to the encoder
// via SMBResponseBase.ReleaseData per D-09).
type BlockReadResult struct {
    Data []byte
    EOF  bool
}

func (r *BlockReadResult) Release() {
    if r.Data != nil {
        pool.Put(r.Data)
        r.Data = nil
    }
}

// ReadFromBlockStore takes a plain context.Context (per D-03) — callers
// thread in auth-scoped ctx from their own dispatch layer.
func ReadFromBlockStore(
    ctx context.Context,
    blockStore *engine.BlockStore,
    payloadID metadata.PayloadID,
    offset uint64,
    count uint32,
) (BlockReadResult, error) {
    // body copied verbatim from v3, with logger.DebugCtx(ctx, ...) and
    // structural fields (handle, clientIP) lifted into caller-side log
    // lines — callers already have them in scope.
    ...
}
```

**Gotcha:** NFSv3 currently passes `*NFSHandlerContext` (concrete) — cannot live in `common/`. Replacement takes `context.Context`. Callers log the surrounding structured fields themselves. NFSv3 `read.go:237` goes from `readFromBlockStore(ctx, blockStore, ...)` to `common.ReadFromBlockStore(ctx.Context, blockStore, ...)`.

---

### `internal/adapter/common/errmap.go` (NEW — struct-per-code table, D-06)

**Analog A:** `internal/adapter/nfs/v3/handlers/create.go:577-621` (`mapMetadataErrorToNFS`) — 19 codes, switch-based
**Analog B:** `internal/adapter/nfs/v4/types/errors.go:18-70` (`MapMetadataErrorToNFS4`) — 19 codes, switch-based (includes `ErrLocked`, `ErrDeadlock`, `ErrGracePeriod` that NFSv3 maps differently)
**Analog C:** `internal/adapter/smb/v2/handlers/converters.go:358-395` (`MetadataErrorToSMBStatus`) — 11 codes, switch-based
**Analog D:** `internal/adapter/nfs/xdr/errors.go:50-164` (`MapStoreErrorToNFSStatus`) — 17 codes, NFSv3 logged variant

**NFSv3 pattern** (canonical `errors.As` + switch) (`create.go:579-621`):
```go
func mapMetadataErrorToNFS(err error) uint32 {
    var storeErr *metadata.StoreError
    if errors.As(err, &storeErr) {
        switch storeErr.Code {
        case metadata.ErrNotFound:
            return types.NFS3ErrNoEnt
        case metadata.ErrAccessDenied, metadata.ErrAuthRequired:
            return types.NFS3ErrAccess
        case metadata.ErrPermissionDenied:
            return types.NFS3ErrPerm
        case metadata.ErrPrivilegeRequired:
            return types.NFS3ErrPerm
        case metadata.ErrAlreadyExists:
            return types.NFS3ErrExist
        ...
        default:
            return types.NFS3ErrIO
        }
    }
    return types.NFS3ErrIO
}
```

**SMB pattern** (`converters.go:358-395`) — note uses unwrapped type assertion, **not** `errors.As`:
```go
func MetadataErrorToSMBStatus(err error) types.Status {
    if err == nil {
        return types.StatusSuccess
    }
    if storeErr, ok := err.(*metadata.StoreError); ok {  // NOT errors.As — bug to fix during consolidation
        switch storeErr.Code {
        case metadata.ErrNotFound:
            return types.StatusObjectNameNotFound
        ...
        }
    }
    return types.StatusInternalError
}
```

**NFSv4 pattern** (`v4/types/errors.go:18-70`) — uses `errors.As` correctly, includes lock codes:
```go
func MapMetadataErrorToNFS4(err error) uint32 {
    if err == nil {
        return NFS4_OK
    }
    var storeErr *errors.StoreError
    if !goerrors.As(err, &storeErr) {
        return NFS4ERR_SERVERFAULT
    }
    switch storeErr.Code {
    case errors.ErrNotFound:
        return NFS4ERR_NOENT
    ...
    case errors.ErrLocked:
        return NFS4ERR_LOCKED
    case errors.ErrDeadlock:
        return NFS4ERR_DEADLOCK
    case errors.ErrGracePeriod:
        return NFS4ERR_GRACE
    ...
    default:
        return NFS4ERR_SERVERFAULT
    }
}
```

**Target shape** (per D-06 — struct-per-code):
```go
package common

import (
    goerrors "errors"

    nfstypes "github.com/marmos91/dittofs/internal/adapter/nfs/types"      // NFS3ERR_*
    nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"  // NFS4ERR_*
    smbtypes "github.com/marmos91/dittofs/internal/adapter/smb/types"       // STATUS_*
    "github.com/marmos91/dittofs/pkg/metadata/errors"
)

type protoCodes struct {
    NFS3 uint32
    NFS4 uint32
    SMB  smbtypes.Status
}

var errorMap = map[errors.ErrorCode]protoCodes{
    errors.ErrNotFound: {
        NFS3: nfstypes.NFS3ErrNoEnt,
        NFS4: nfs4types.NFS4ERR_NOENT,
        SMB:  smbtypes.StatusObjectNameNotFound,
    },
    errors.ErrAccessDenied: {
        NFS3: nfstypes.NFS3ErrAccess,
        NFS4: nfs4types.NFS4ERR_ACCESS,
        SMB:  smbtypes.StatusAccessDenied,
    },
    errors.ErrPermissionDenied: {
        NFS3: nfstypes.NFS3ErrPerm,
        NFS4: nfs4types.NFS4ERR_PERM,
        SMB:  smbtypes.StatusAccessDenied,  // SMB has no EPERM distinction
    },
    // ... 27 rows total, one per errors.ErrorCode ...
}

// Defaults when code is not in the table.
var defaultCodes = protoCodes{
    NFS3: nfstypes.NFS3ErrIO,
    NFS4: nfs4types.NFS4ERR_SERVERFAULT,
    SMB:  smbtypes.StatusInternalError,
}

// MapToNFS3 translates a metadata error to an NFSv3 status code.
func MapToNFS3(err error) uint32 {
    if err == nil {
        return nfstypes.NFS3OK
    }
    var storeErr *errors.StoreError
    if !goerrors.As(err, &storeErr) {
        return defaultCodes.NFS3
    }
    if codes, ok := errorMap[storeErr.Code]; ok {
        return codes.NFS3
    }
    return defaultCodes.NFS3
}

// MapToNFS4, MapToSMB: identical structure.
```

**Gotchas:**
1. **SMB currently uses unwrapped type assertion** (`converters.go:364`) — the consolidation *must* switch to `errors.As` to handle wrapped errors. This is a latent bug fix falling out of consolidation; document in PR.
2. **NFSv3 has no `ErrLocked` / `ErrDeadlock` / `ErrGracePeriod` in its map** — `xdr/errors.go:140-146` maps `ErrLocked → NFS3ErrJukebox` (audit-logging variant) but `create.go:579` omits it. Table must include these rows with NFSv3 column = `NFS3ErrJukebox` (the audit-logging wrapper at `xdr/errors.go` is more complete than `create.go`'s map — use `xdr/errors.go` as NFSv3 truth).
3. **`ErrAccessDenied, ErrAuthRequired` both map to NFS3ErrAccess** in the existing switches — in a struct-per-code table, each must get its own row with the same NFS3 value. Not a drift.
4. **Audit-logging wrapper preserved:** `xdr/errors.go`'s `MapStoreErrorToNFSStatus(err, clientIP, op)` adds per-code logging. Keep the wrapper at `xdr/errors.go`; body becomes `common.MapToNFS3(err)` + existing log line. Callers at `internal/adapter/nfs/v3/handlers/xxx.go` that want raw mapping call `common.MapToNFS3` directly; callers that want logging call `xdr.MapStoreErrorToNFSStatus`.

---

### `internal/adapter/common/content_errmap.go` (NEW — content/block-store error table, D-08 §2)

**Analog A:** `internal/adapter/smb/v2/handlers/converters.go:397-406` (`ContentErrorToSMBStatus`)
**Analog B:** `internal/adapter/nfs/xdr/errors.go:184-231` (`MapContentErrorToNFSStatus`)

**Existing SMB (minimal)** (`converters.go:401-406`):
```go
func ContentErrorToSMBStatus(err error) types.Status {
    if err == nil {
        return types.StatusSuccess
    }
    return types.StatusUnexpectedIOError
}
```

**Existing NFS (richer, string-matched — kept as NFSv3 wrapper)** (`xdr/errors.go:184-231`):
```go
func MapContentErrorToNFSStatus(err error) uint32 {
    ...
    if errors.Is(err, blockstore.ErrRemoteUnavailable) {
        return types.NFS3ErrIO
    }
    errMsg := err.Error()
    switch {
    case containsIgnoreCase(errMsg, "cache full"):
        return types.NFS3ErrJukebox
    ...
    }
}
```

**Target shape**: keep table narrow — only `blockstore.ErrRemoteUnavailable` and "cache full" are stable typed signals today. Both protocols reduce to I/O-class errors. Table + three thin accessors (`MapContentToNFS3`, `MapContentToNFS4`, `MapContentToSMB`).

**Gotcha:** string-matching on error messages is intentionally not moved into `common/`. Keep that heuristic at the NFSv3 call site as a transition fallback. Consolidation target is the typed-error path.

---

### `internal/adapter/common/lock_errmap.go` (NEW — lock error table, D-08 §3)

**Analog A (SMB):** `internal/adapter/smb/v2/handlers/lock.go:533-549` (`lockErrorToStatus`)
**Analog B (NFSv3):** `internal/adapter/nfs/xdr/errors.go:140-146` (`ErrLocked → NFS3ErrJukebox`)
**Analog C (NFSv4):** `internal/adapter/nfs/v4/types/errors.go:59-64` (`ErrLocked/Deadlock/Grace`)

**Existing SMB lock mapping** (`lock.go:533-549`):
```go
func lockErrorToStatus(err error) types.Status {
    if storeErr, ok := err.(*metadata.StoreError); ok {
        switch storeErr.Code {
        case metadata.ErrLocked:
            return types.StatusLockNotGranted
        case metadata.ErrLockNotFound:
            return types.StatusRangeNotLocked
        case metadata.ErrNotFound:
            return types.StatusFileClosed
        case metadata.ErrPermissionDenied:
            return types.StatusAccessDenied
        case metadata.ErrIsDirectory:
            return types.StatusFileIsADirectory
        }
    }
    return types.StatusInternalError
}
```

**Target shape** — same struct-per-code pattern as `errmap.go`, but the "lock context" changes certain mappings:
```go
// lockErrorMap mirrors errorMap but with lock-operation-specific protocol codes.
// E.g., in lock context ErrLocked → STATUS_LOCK_NOT_GRANTED (SMB) / NFS3ERR_JUKEBOX (NFSv3)
// / NFS4ERR_DENIED (NFSv4); in general context ErrLocked → STATUS_FILE_LOCK_CONFLICT.
var lockErrorMap = map[errors.ErrorCode]protoCodes{
    errors.ErrLocked: {
        NFS3: nfstypes.NFS3ErrJukebox,
        NFS4: nfs4types.NFS4ERR_DENIED,
        SMB:  smbtypes.StatusLockNotGranted,
    },
    errors.ErrLockNotFound: {
        NFS3: nfstypes.NFS3ErrInval,
        NFS4: nfs4types.NFS4ERR_LOCK_RANGE,
        SMB:  smbtypes.StatusRangeNotLocked,
    },
    errors.ErrLockConflict: { /* ... */ },
    errors.ErrDeadlock:     { /* NFS4ERR_DEADLOCK, SMB has no direct code → StatusLockNotGranted */ },
    errors.ErrGracePeriod:  { /* NFS4ERR_GRACE, NFSv3 has no direct code → NFS3ErrJukebox */ },
    errors.ErrLockLimitExceeded: { /* ... */ },
}

func MapLockToSMB(err error) smbtypes.Status { /* lookup lockErrorMap, fall back to MapToSMB */ }
```

**Gotcha:** SMB's richer lock taxonomy (`STATUS_FILE_LOCK_CONFLICT` vs `STATUS_LOCK_NOT_GRANTED`) needs per-call-site context — the I/O path (`smb/v2/handlers/write.go:323`, `read.go:290`) uses `StatusFileLockConflict` directly without going through any mapper. Only `lockErrorToStatus` at `lock.go:533` is a lock-operation translator. Document which SMB code is returned in which context in the table's Godoc.

---

### `internal/adapter/nfs/v3/handlers/read.go` (MODIFY — handler, file-I/O)

**Current call sites** (3 touches):

Line 144 (block-store resolution):
```go
blockStore, err := getBlockStoreForHandle(h.Registry, ctx.Context, fileHandle)
```
→
```go
blockStore, err := common.ResolveForRead(ctx.Context, h.Registry, fileHandle)
```

Line 237 (pooled read):
```go
readResult, readErr := readFromBlockStore(ctx, blockStore, file.PayloadID, req.Offset, actualLength, clientIP, req.Handle)
```
→
```go
readResult, readErr := common.ReadFromBlockStore(ctx.Context, blockStore, file.PayloadID, req.Offset, actualLength)
// (move clientIP/handle fields into caller-side log lines as structured log fields)
```

Line 253 consumes `readResult.data` (private field); becomes `readResult.Data` (exported).

**Response Release** at `read.go:88-93` stays as-is — NFSv3's existing `Releaser` interface path at `internal/adapter/nfs/helpers.go:110-114` invokes it after encoding. Already correct.

**Gotcha:** lines 237, 253, 255 all touch the struct by private field names (`data`, `eof`). The field capitalization changes (`Data`, `EOF`) because they cross a package boundary. This is a mechanical rename; do it in the same commit as the move.

---

### `internal/adapter/nfs/v4/handlers/read.go` (MODIFY — handler, file-I/O; gains pool)

**Current** (`v4/handlers/read.go:148,157`):
```go
blockStore, err := getBlockStoreForHandle(h, ctx.Context, ctx.CurrentFH)
...
data := make([]byte, actualLen)  // <-- not pooled today
n, err := blockStore.ReadAt(ctx.Context, string(file.PayloadID), data, offset)
```

**Target**:
```go
blockStore, err := common.ResolveForRead(ctx.Context, h.Registry, metadata.FileHandle(ctx.CurrentFH))
...
readResult, err := common.ReadFromBlockStore(ctx.Context, blockStore, file.PayloadID, offset, uint32(actualLen))
// ... encode, then release:
defer readResult.Release()
```

**Gotcha:** NFSv4 today does not use the pool. Adopting `common.ReadFromBlockStore` automatically adopts pool. This is a behavior change (perf-positive, per-request alloc → pool reuse) — document in PR description. Response `Release()` pathway for NFSv4 is via the CompoundResult encoder; may need to wire a deferred `Release()` at the compound-dispatch level similar to how NFSv3 handles it at `helpers.go:110-114`.

---

### `internal/adapter/smb/v2/handlers/read.go` (MODIFY — handler, pool D-09/D-10)

**Current call sites**:

Line 234 (resolution):
```go
blockStore, err := h.Registry.GetBlockStoreForHandle(ctx.Context, openFile.MetadataHandle)
```

Line 257, 269, 346 (error mapping):
```go
return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: ContentErrorToSMBStatus(err)}}, nil
```

Line 342-348 (the inline alloc that ADAPT-02 pools):
```go
data := make([]byte, actualLength)
n, err := blockStore.ReadAt(authCtx.Context, string(readMeta.Attr.PayloadID), data, req.Offset)
if err != nil {
    return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: ContentErrorToSMBStatus(err)}}, nil
}
data = data[:n]
```

**Target**:
```go
blockStore, err := common.ResolveForRead(ctx.Context, h.Registry, openFile.MetadataHandle)
...
return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}, nil
...
readResult, err := common.ReadFromBlockStore(authCtx.Context, blockStore, readMeta.Attr.PayloadID, req.Offset, actualLength)
if err != nil {
    return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: common.MapContentToSMB(err)}}, nil
}
// Hand the release closure to the encoder per D-09 instead of defer:
return &ReadResponse{
    SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess, ReleaseData: readResult.Release},
    DataOffset:      0x50,
    Data:            readResult.Data,
    DataRemaining:   0,
}, nil
```

**Gotcha per D-09 (CRITICAL):** `ReleaseData` is a `func()` field on `SMBResponseBase`. Encoder (framing/compound) invokes it after `WriteNetBIOSFrame`. **MUST be nil-safe** — non-pooled responses leave `ReleaseData` nil and the encoder checks before invoking:
```go
if releaser, ok := any(resp).(ReleaseDataCarrier); ok && releaser.GetReleaseData() != nil {
    releaser.GetReleaseData()()
}
```

**Gotcha per D-10:** Also migrate `handlePipeRead` and `h.handleSymlinkRead` in `read.go:161,262` to go through `common.ReadFromBlockStore` for uniformity — pool rounds up to 4KB small tier for small pipe/symlink buffers; overhead negligible, consistency wins.

---

### `internal/adapter/smb/response.go` (MODIFY — encoder hook, D-09)

**Target edit:** after `WriteNetBIOSFrame` completes successfully (around `response.go:617` inside `sendMessage`), invoke the handler-provided release closure if present. Current structure has the release concern absent entirely; add it in `SendResponse` (line 424-427) and `SendResponseWithHooks` (line 414-421), piped through from `HandlerResult.ReleaseData` (new field).

**Analog pattern** — NFSv3's `helpers.go:110-114` release-after-encode:
```go
if releaser, ok := any(resp).(nfs.Releaser); ok {
    releaser.Release()
}
```

**Target for SMB** (new shape in `SendResponseWithHooks`):
```go
err := sendMessage(respHeader, body, connInfo, preWrite)
// Release pooled data AFTER wire write completes (success or failure).
// Safe to release on write error — buffer is no longer needed.
if result.ReleaseData != nil {
    result.ReleaseData()
}
return err
```

**Gotchas:**
1. **Compound requests:** `compound.go` assembles multiple `HandlerResult`s into one wire write. Each must have its `ReleaseData` invoked after the *compound* wire write, not between sub-responses (the sub-bodies are still referenced inside the composed frame). Audit `compound.go` for the wire-write call; release all collected `ReleaseData` closures after that single write.
2. **Encryption path** (`response.go:569-587`): there are two write paths (encrypted vs plain). `ReleaseData` must fire in both. Put it in `sendMessage` after `WriteNetBIOSFrame` returns, regardless of branch.
3. **Poison-connection path** (`response.go:584-587`, 634-636): if write fails and `preWrite` already ran, the connection is closed. `ReleaseData` must still fire — the buffer is no longer referenced by anything.

---

### `internal/adapter/smb/v2/handlers/result.go` (MODIFY — add `ReleaseData`, D-09)

**Current** (`result.go:57-88`):
```go
type HandlerResult struct {
    Data           []byte
    Status         types.Status
    DropConnection bool
    AsyncId        uint64
    IsBinding      bool
}
```

**Target**:
```go
type HandlerResult struct {
    Data           []byte
    Status         types.Status
    DropConnection bool
    AsyncId        uint64
    IsBinding      bool

    // ReleaseData is invoked by the response encoder AFTER the wire write
    // completes (MS-SMB2 response body encoding for Data can reference
    // pooled buffers). Must be nil-safe — leave nil for non-pooled paths.
    // Per D-09: read handler sets this to readResult.Release; framing
    // invokes it once per completed wire write.
    ReleaseData func()
}
```

**Parallel change:** `SMBResponseBase` at `result.go:29-39` gains the same field (since handlers return typed `*ReadResponse` / `*WriteResponse`, not raw `HandlerResult`). The command-specific `Encode()` method copies `SMBResponseBase.ReleaseData` onto `HandlerResult.ReleaseData` before the framer sees it. Alternative: put only on `HandlerResult` and let handlers populate via a thin adapter — planner picks per D-09 discretion.

---

### Error mapping migration — individual call sites

All call sites that use `mapMetadataErrorToNFS`, `MetadataErrorToSMBStatus`, `ContentErrorToSMBStatus`, `MapMetadataErrorToNFS4`, or `lockErrorToStatus` change to the corresponding `common.MapToNFS3 / MapToNFS4 / MapToSMB / MapContentTo* / MapLockTo*` functions.

**Pattern to replicate in every call site** — e.g. `internal/adapter/smb/v2/handlers/write.go:349`:
```go
// before
return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
// after
return &WriteResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}, nil
```

**Per D-07 gotcha:** NFSv3 `create.go:577-621` drops; wrapper at `xdr/errors.go` stays (audit-logging). NFSv4 `v4/types/errors.go:18-70` body becomes a one-liner `return common.MapToNFS4(err)`.

---

### `test/e2e/cross_protocol_test.go` (EXTEND — e2e tier, ADAPT-05)

**Analog:** `test/e2e/cross_protocol_test.go:29-128` (existing XPR-01..06 subtest framework).

**Existing subtest shape** (lines 105-127):
```go
t.Run("XPR-01 File created via NFS readable via SMB", func(t *testing.T) {
    testFileNFSToSMB(t, nfsMount, smbMount)
})
...
```

**Target shape** — add a new top-level `TestCrossProtocolErrorConformance` (or subtest group `XPR-07..XPR-24`) with table-driven error-code coverage. Each case triggers a real error via both NFS and SMB mount, asserts the errno/NT-status matches the `common/` table column.

```go
func TestCrossProtocolErrorConformance(t *testing.T) {
    // reuse the same bootstrap (mounts) as TestCrossProtocolInterop
    ...
    cases := []struct {
        name       string
        trigger    func(t *testing.T, nfsPath, smbPath string)  // fires the error
        wantNFS    syscall.Errno   // e.g. syscall.ENOENT
        wantSMBNT  uint32          // e.g. STATUS_OBJECT_NAME_NOT_FOUND
    }{
        {"ErrNotFound", triggerStatOnMissing, syscall.ENOENT, smb.StatusObjectNameNotFound},
        {"ErrAlreadyExists", triggerCreateExisting, syscall.EEXIST, smb.StatusObjectNameCollision},
        {"ErrNotEmpty", triggerRmdirNonEmpty, syscall.ENOTEMPTY, smb.StatusDirectoryNotEmpty},
        // ... 18 e2e-triggerable codes per D-13 ...
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) { /* trigger on both mounts, assert */ })
    }
}
```

**Gotcha:** Phase 08's `TestCollectGarbage_S3` flakiness shows bootstrap cost compounds — reuse the single mount bootstrap across all 18 cases, not a per-case server restart.

---

### `internal/adapter/common/errmap_test.go` (NEW — unit tier, ADAPT-05)

**Analog:** `internal/adapter/nfs/v4/types/constants_test.go:152-278` (`TestMapMetadataErrorToNFS4`).

**Existing shape** (lines 152-268):
```go
func TestMapMetadataErrorToNFS4(t *testing.T) {
    tests := []struct {
        name     string
        err      error
        expected uint32
    }{
        {
            name:     "nil error returns NFS4_OK",
            err:      nil,
            expected: NFS4_OK,
        },
        {
            name:     "ErrNotFound maps to NFS4ERR_NOENT",
            err:      errors.NewNotFoundError("/test", "file"),
            expected: NFS4ERR_NOENT,
        },
        ...
        {
            name:     "ErrLocked maps to NFS4ERR_LOCKED",
            err:      &errors.StoreError{Code: errors.ErrLocked, Message: "locked"},
            expected: NFS4ERR_LOCKED,
        },
        ...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := MapMetadataErrorToNFS4(tt.err)
            if result != tt.expected {
                t.Errorf("MapMetadataErrorToNFS4(%v) = %d, want %d", tt.err, result, tt.expected)
            }
        })
    }
}
```

**Target:** single test-table exercising every entry in `errorMap`, `contentErrorMap`, `lockErrorMap` against all three protocols. Driving the table from the same map ensures perfect coverage — a new code added to `errorMap` without test rows causes `TestErrorMapCoverage` to fail.

```go
func TestErrorMapCoverage(t *testing.T) {
    // every errors.ErrorCode constant must have a row in errorMap
    for _, code := range allErrorCodes() {
        if _, ok := errorMap[code]; !ok {
            t.Errorf("errorMap missing row for %v", code)
        }
    }
}

func TestMapToNFS3(t *testing.T) { /* table-driven, 27 rows */ }
func TestMapToNFS4(t *testing.T) { /* table-driven, 27 rows */ }
func TestMapToSMB(t *testing.T)  { /* table-driven, 27 rows */ }
```

**Gotcha:** the exotic codes (`ErrConnectionLimitReached`, `ErrLockLimitExceeded`, `ErrDeadlock`, `ErrGracePeriod`, `ErrPrivilegeRequired`, `ErrQuotaExceeded`, `ErrLockConflict`) cannot be reliably triggered end-to-end; unit-tier covers them per D-13. Each such row in the table asserts: "the mapper produces code X for error Y" — no e2e trigger needed.

---

## Shared Patterns

### Pool lifecycle — Get + Put after wire write

**Source:** `internal/adapter/pool/bufpool.go:147-167` (`Get`), `:179-204` (`Put`)
**Apply to:** All READ-path handlers (NFSv3 already correct; NFSv4 + SMB v2 migrate via ADAPT-02/D-10)

```go
// Get: tiered small(4KB)/medium(64KB)/large(1MB) pool with direct-alloc fallback
// for sizes > LargeSize. Put: ignores oversized/undersized (harmless).
data := pool.Get(int(count))
// ... use data ...
pool.Put(data)  // at end of buffer lifetime; wire write must be complete
```

**NFSv3 canonical invocation pattern** (`helpers.go:110-114`):
```go
if releaser, ok := any(resp).(nfs.Releaser); ok {
    releaser.Release()
}
```

### `errors.As` unwrapping

**Source:** `internal/adapter/nfs/v4/types/errors.go:23-25`, `nfs/v3/handlers/create.go:580-581`
**Apply to:** All `common/` mapper functions (SMB's existing type assertion at `converters.go:364` is a bug — must switch to `errors.As`)

```go
var storeErr *errors.StoreError
if !goerrors.As(err, &storeErr) {
    return defaultFallback  // NFS4ERR_SERVERFAULT / StatusInternalError / NFS3ErrIO
}
switch storeErr.Code { ... }
```

### Narrow-interface-not-concrete-runtime

**Source:** `pkg/controlplane/runtime/runtime.go:341` (concrete method) — satisfies interface implicitly
**Apply to:** Every helper in `common/` that needs block store or metadata service (D-01)

No new method on `*Runtime`. The interface lives in `common/` and the existing Runtime satisfies it. Test mocks become trivial.

### Handler returns protocol-envelope with mapped status

**Source:** `internal/adapter/smb/v2/handlers/read.go:257,269,346` + `nfs/v3/handlers/read.go:129,141,147,157`
**Apply to:** Every handler error-return path after error-mapper consolidation

Handlers never construct an error-code literal (`StatusObjectNameNotFound`, `NFS3ErrNoEnt`); they always route through `common.MapToSMB(err)` / `common.MapToNFS3(err)` / `common.MapToNFS4(err)`. The only protocol-code literals that stay at call sites are the ones that are *not* derived from a `metadata.StoreError` — e.g. `StatusInvalidParameter` for decode failures, `StatusFileLockConflict` for in-line lock checks.

### Table-driven tests anchored on the same map

**Source:** `internal/adapter/nfs/v4/types/constants_test.go:152-278`
**Apply to:** Every `common/*_test.go` + `test/e2e/cross_protocol_test.go`

Tests iterate over the mapping table rather than hard-coding expectations; a coverage test fails if a new `ErrorCode` is added without a row.

---

## No Analog Found

| File | Role | Reason |
|------|------|--------|
| (none) | — | Every planned file has a direct analog in the codebase. |

Even the new `common/` package has direct analogs — it literally *is* the merge of three existing translators and two existing 3-line helper functions.

---

## Key Gotchas for the Planner

1. **Order of operations for ADAPT-01 → ADAPT-03 → ADAPT-02:** create `common/resolve.go` + `common/read_payload.go` first (makes new API available), migrate NFSv3 + NFSv4 + SMB call sites next, then add `common/errmap.go` and do the three-way error-mapping migration, then add the `ReleaseData` plumbing (D-09) and switch SMB READ inline `make([]byte, n)` to the new pooled path. PR-A ships as one reviewable unit.

2. **SMB `errors.As` fix is not optional:** `converters.go:364`'s unwrapped type assertion fails on any wrapped error. Moving to `common.MapToSMB(err)` which uses `errors.As` is a latent bug fix — call out explicitly in PR commit message so reviewers don't miss it.

3. **`readResult.data` → `readResult.Data` is a mechanical rename** but touches every NFSv3 call site (`read.go:253,254`). Tests at `internal/adapter/nfs/v3/handlers/read_payload_test.go` (if any) must update.

4. **NFSv4 today does not use the pool.** Adopting `common.ReadFromBlockStore` silently enables pool. Call this out in the PR description — it's a positive perf change but a behavioral change.

5. **`ReleaseData` must be nil-safe** in the SMB encoder. Non-READ responses leave it nil; the encoder checks. Every test that constructs a `HandlerResult` or `ReadResponse` must work with `ReleaseData == nil`.

6. **NFSv3 audit-logging wrapper (`xdr/errors.go`) stays.** The body becomes `common.MapToNFS3(err)` + existing log line. Don't delete — the `clientIP, operation` logging is a feature, not duplication.

7. **Three-way merge drift to audit at planning time** (per D-07 risk): NFSv3 `create.go:579` map omits `ErrLocked/Deadlock/Grace` that NFSv4 `v4/types/errors.go:59-64` has; SMB `converters.go:358` omits several codes NFSv3 has (`ErrPermissionDenied`, `ErrAuthRequired`, `ErrReadOnly`, `ErrNameTooLong`). Consolidation surfaces these as planning-time findings — each gets a row in the struct-per-code table. Document per-code source in a comment so reviewers can verify the new cells are correct.

8. **Compound SMB responses and `ReleaseData`:** `compound.go` collects multiple `HandlerResult`s into one write. Release closures must fire after the single composed wire write, not between sub-responses. Audit `internal/adapter/smb/compound.go` carefully (731 LoC) — it has its own write-and-sign path separate from `response.go:sendMessage`.

---

## Metadata

**Analog search scope:** `internal/adapter/nfs/{v3,v4,xdr,rpc}/`, `internal/adapter/smb/{v2,types,framing.go,compound.go,response.go}/`, `internal/adapter/pool/`, `pkg/metadata/errors*`, `pkg/controlplane/runtime/`, `test/e2e/`
**Files scanned:** ~35 (Read tool) plus Grep surveys
**Pattern extraction date:** 2026-04-24

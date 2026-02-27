# Phase 30: SMB Bug Fixes - Research

**Researched:** 2026-02-27
**Domain:** SMB protocol correctness, cross-protocol coordination, sparse file semantics
**Confidence:** HIGH

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Oplock break mechanism must be **generic via Runtime** -- any adapter can trigger breaks on any other adapter, not hardwired NFS->SMB
- **Zero-fill gaps** within file size for unwritten blocks -- return continuous buffer with zeros where blocks don't exist
- Zero-fill logic lives in the **payload layer** -- single point of truth so both NFS and SMB benefit automatically
- **Short read / EOF** for reads past file size -- standard POSIX behavior, sparse zero-fill only applies within file boundaries
- Follow the **4 roadmap plans** as defined (30-01 through 30-04), each independently testable
- **Fix + improve surroundings** -- while in the code, clean up related issues
- **Fix root causes** if a deeper architectural issue is revealed
- Bug fix E2E tests **added to existing test files** by feature area, not a separate file
- Sparse READ tests cover **both** Windows Explorer workflow AND cross-protocol (write via NFS with gaps, read via SMB)
- Oplock break tested via **dual-mount E2E** -- SMB client holds oplock, NFS client writes, verify break sent and data consistent
- Run **WPTS BVT suite locally** after each fix (not just CI)
- Run **SMB conformance suite** after fixes to verify no regressions

### Claude's Discretion
- Sync vs async oplock break wait strategy (based on MS-SMB2 spec and Samba reference)
- Oplock break timeout behavior on unresponsive SMB clients
- Which NFS v3 operations trigger oplock breaks (mutating only vs all conflicting)
- Sparse region tracking approach (implicit block detection vs explicit bitmap)
- Fix execution order across the 4 plans

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| BUG-01 | Sparse file READ returns zeros for unwritten blocks instead of errors (#180) | `downloadBlock()` in `offloader/download.go` propagates `ErrBlockNotFound` as error instead of zero-filling; `ensureAndReadFromCache()` in `io/read.go` fails on cache miss after download; cache `ReadAt` already handles sparse blocks correctly (leaves zeros in dest buffer) |
| BUG-02 | Renamed directory children reflect updated paths in QUERY_DIRECTORY (#181) | `Move()` in `file_modify.go` line 512-513 updates `srcFile.Ctime` but never updates `srcFile.Path` to `destPath` before `PutFile`; `destPath` is computed at line 355 but only used for validation |
| BUG-03 | Multi-component paths with `..` segments navigate to parent directory (#214) | `walkPath()` in `create.go` line 768-770 has `TODO: Handle parent directory navigation` -- `..` segments are silently skipped instead of navigating to parent via `metaSvc.Lookup(authCtx, currentHandle, "..")` |
| BUG-04 | NFS v3 operations trigger oplock break for SMB clients holding locks (#213) | NFS v3 handlers `write.go:214`, `remove.go:153`, `rename.go:254` have `TODO(plan-03)` placeholders; `OplockManager.CheckAndBreakForWrite/Read/Delete` methods already exist but NFS handlers don't have access to the SMB `OplockManager` instance |
| BUG-05 | FileStandardInfo.NumberOfLinks reads actual link count from metadata (#221) | `FileAttrToFileStandardInfo()` in `converters.go` line 162 hardcodes `NumberOfLinks: 1` with `TODO: Track actual link count when available`; metadata `FileAttr.Nlink` field already carries the correct value |
| BUG-06 | Share list cached for pipe CREATE operations, invalidated on change (#223) | `handlePipeCreate()` in `create.go` line 633-652 calls `Registry.ListShares()` and `PipeManager.SetShares()` on every pipe CREATE; has `TODO` noting inefficiency; needs caching with invalidation on share add/remove |
</phase_requirements>

## Summary

This phase addresses 6 confirmed bugs in the SMB implementation that block Windows file operations. Each bug has been precisely located in the codebase with clear root causes and fix paths. The bugs span four domains: (1) payload layer sparse file handling, (2) metadata path persistence during rename, (3) SMB path resolution and info reporting, and (4) cross-protocol oplock coordination.

All bugs have straightforward fixes with existing infrastructure. The sparse READ fix requires changes in the offloader download path to treat `ErrBlockNotFound` as a zero-fill signal rather than an error. The renamed directory bug is a single missing line in the Move transaction. The parent navigation and NumberOfLinks bugs are one-line fixes in SMB handlers. The oplock break wiring requires plumbing the `OplockManager` through the Runtime to NFS handlers. The pipe share caching is a standard cache-with-invalidation pattern.

**Primary recommendation:** Fix BUG-05 (NumberOfLinks) and BUG-03 (parent navigation) first as they are trivial one-line fixes, then tackle BUG-01 (sparse READ) and BUG-02 (renamed directory) which are the most impactful, then BUG-04 (oplock break) and BUG-06 (pipe cache) which require more architectural plumbing.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go standard library | 1.22+ | All implementation | Project language, no external deps needed for fixes |
| `pkg/metadata` | Internal | File metadata operations | Existing service layer with correct Nlink, Path, Lookup |
| `pkg/payload` | Internal | Payload read/write with cache | Existing offloader/cache with block-level operations |
| `pkg/metadata/lock` | Internal | Lock/lease management | Existing OplockManager with CheckAndBreak* methods |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `sync` | stdlib | Caching with mutex | Share list cache (BUG-06) |
| `errors` | stdlib | Error detection | `errors.Is()` for ErrBlockNotFound sparse detection |
| `github.com/stretchr/testify` | v1.8+ | Test assertions | E2E and unit test assertions |

## Architecture Patterns

### Pattern 1: Sparse Read Zero-Fill in Offloader
**What:** When `downloadBlock()` encounters `ErrBlockNotFound`, treat it as a sparse block within file size boundaries instead of propagating an error. The cache already handles this correctly -- `ReadAt` leaves zeros in the dest buffer for missing blocks.
**When to use:** Payload layer read path when block doesn't exist but offset is within file size.
**Current code (broken):**
```go
// offloader/download.go:31-34
data, err := m.blockStore.ReadBlock(ctx, blockKeyStr)
if err != nil {
    return fmt.Errorf("download block %s: %w", blockKeyStr, err)  // BUG: returns error for sparse blocks
}
```
**Fix approach:**
```go
data, err := m.blockStore.ReadBlock(ctx, blockKeyStr)
if err != nil {
    if errors.Is(err, store.ErrBlockNotFound) {
        // Sparse block: zero-fill within file size
        data = make([]byte, BlockSize)
    } else {
        return fmt.Errorf("download block %s: %w", blockKeyStr, err)
    }
}
```
The `ensureAndReadFromCache` path in `io/read.go` also needs to handle the case where `EnsureAvailable` succeeds (sparse block detected) but cache `ReadAt` returns `found=false` because no data was written. This should return zeros, not an error.

### Pattern 2: Path Update in Move Transaction
**What:** Update `srcFile.Path` to the destination path before calling `PutFile` in the Move transaction. Also update paths of all children recursively for directory renames.
**When to use:** Metadata Move operation.
**Current code (broken):**
```go
// file_modify.go:510-513
now := time.Now()
srcFile.Ctime = now
_ = tx.PutFile(ctx.Context, srcFile)  // BUG: srcFile.Path still has old value
```
**Fix approach:**
```go
now := time.Now()
srcFile.Path = destPath  // Update path BEFORE persisting
srcFile.Ctime = now
_ = tx.PutFile(ctx.Context, srcFile)

// For directory renames: recursively update all children's paths
if srcFile.Type == FileTypeDirectory {
    s.updateChildPaths(ctx, tx, srcHandle, srcFile.Path)
}
```
**Key consideration:** For directory renames, all descendant paths must be updated recursively. The function should walk all children and update their Path field with the new prefix.

### Pattern 3: Generic Oplock Break via Runtime
**What:** Register the OplockManager as an adapter provider in the Runtime, then NFS handlers query it to trigger cross-protocol breaks.
**When to use:** NFS v3 write/setattr/remove/rename operations that conflict with SMB leases.
**Current infrastructure:**
- `OplockManager` already has `CheckAndBreakForWrite()`, `CheckAndBreakForRead()`, `CheckAndBreakForDelete()` methods
- Runtime has `SetAdapterProvider(key, provider)` / `GetAdapterProvider(key)` for cross-adapter communication
- NFS v3 handlers have `h.Registry` pointing to the Runtime

**Wiring approach:**
```go
// SMB adapter registers OplockManager during startup:
runtime.SetAdapterProvider("oplock_manager", handler.OplockManager)

// NFS v3 handlers retrieve and call:
if om := h.Registry.GetAdapterProvider("oplock_manager"); om != nil {
    if manager, ok := om.(OplockBreaker); ok {
        _ = manager.CheckAndBreakForWrite(ctx, fileHandle)
    }
}
```

**Interface to define (in a shared location to avoid import cycles):**
```go
type OplockBreaker interface {
    CheckAndBreakForWrite(ctx context.Context, fileHandle lock.FileHandle) error
    CheckAndBreakForRead(ctx context.Context, fileHandle lock.FileHandle) error
    CheckAndBreakForDelete(ctx context.Context, fileHandle lock.FileHandle) error
}
```

### Pattern 4: Share List Cache with Event Invalidation
**What:** Cache the share list in the SMB Handler and invalidate on share add/remove events via Runtime callbacks.
**When to use:** Pipe CREATE operations on IPC$.
**Current code (inefficient):**
```go
// create.go:635-652 - Called on EVERY pipe CREATE
shareNames := h.Registry.ListShares()
shares := make([]rpc.ShareInfo1, 0, len(shareNames))
// ... build share list ...
h.PipeManager.SetShares(shares)
```
**Fix approach:**
```go
// Cache in Handler:
type Handler struct {
    // ...
    cachedShares    []rpc.ShareInfo1
    sharesCacheMu   sync.RWMutex
    sharesCacheValid bool
}

// Invalidate via Runtime share change callback:
runtime.RegisterShareChangeCallback(func(shareName string, event ShareEvent) {
    h.sharesCacheMu.Lock()
    h.sharesCacheValid = false
    h.sharesCacheMu.Unlock()
})
```

### Anti-Patterns to Avoid
- **Hardwiring NFS->SMB oplock break:** The oplock break mechanism must be generic via Runtime adapter providers, not a direct import of the SMB package from NFS handlers. This would create import cycles and violate the multi-protocol architecture.
- **Updating only the renamed file's path without children:** When a directory is renamed, ALL descendant files' Path fields must be updated recursively, not just the directory itself. A partial update causes inconsistent state in QUERY_DIRECTORY responses.
- **Checking file size in the cache layer for sparse detection:** The file size check for sparse zero-fill belongs in the payload/offloader layer, not in the cache. The cache layer (`cache/read.go`) correctly handles missing blocks by leaving zeros in the destination buffer -- the fix goes in the offloader download path.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Sparse block detection | Custom bitmap tracking | `errors.Is(err, store.ErrBlockNotFound)` in offloader | Block store already distinguishes "not found" from real errors; implicit detection is simpler |
| Cross-protocol oplock interface | Direct package imports between NFS and SMB | Runtime adapter provider pattern (`SetAdapterProvider`/`GetAdapterProvider`) | Already exists, avoids import cycles |
| Share list cache invalidation | Custom polling or timer | Runtime `shareChangeCallbacks` | Already exists (per STATE.md decision from Phase 26-05) |
| Recursive path update | Manual tree traversal | `ListChildren` + recursive `PutFile` within transaction | Store interface already supports child iteration |

**Key insight:** All 6 bugs can be fixed using existing infrastructure. No new external dependencies or architectural changes are needed. The fixes are localized and each can be tested independently.

## Common Pitfalls

### Pitfall 1: Sparse Zero-Fill Beyond EOF
**What goes wrong:** Returning zeros for reads beyond file size instead of short read/EOF
**Why it happens:** The zero-fill logic doesn't distinguish between "within file size but no block" vs "beyond file size"
**How to avoid:** Check file size from metadata BEFORE zero-filling. Only zero-fill when `offset < file.Size`. For reads beyond EOF, return short read (fewer bytes).
**Warning signs:** Tests pass for sparse reads within file but fail for reads at/past EOF

### Pitfall 2: Import Cycles in Oplock Break Wiring
**What goes wrong:** NFS handler importing SMB handler package creates circular dependency
**Why it happens:** Temptation to directly reference `handlers.OplockManager` from NFS code
**How to avoid:** Define a narrow `OplockBreaker` interface in a shared package (e.g., `pkg/adapter/` or `pkg/metadata/lock/`) that `OplockManager` satisfies. NFS handlers depend only on the interface.
**Warning signs:** `go build` fails with "import cycle" errors

### Pitfall 3: Incomplete Recursive Path Update on Directory Rename
**What goes wrong:** Top-level directory path updated but children still have old path prefix
**Why it happens:** Only updating `srcFile.Path` without walking descendants
**How to avoid:** Recursively walk all children of the renamed directory within the same transaction, updating each file's Path by replacing the old prefix with the new one
**Warning signs:** Renamed directory's immediate listing works, but nested files/subdirectories show stale paths

### Pitfall 4: Race Condition in Share List Cache
**What goes wrong:** Pipe CREATE reads stale cache while share add/remove is in progress
**Why it happens:** Cache invalidation and cache read not properly synchronized
**How to avoid:** Use `sync.RWMutex` for the cache. Write lock during invalidation, read lock during cache access. Rebuild cache lazily on next read after invalidation.
**Warning signs:** Intermittent test failures where newly created shares don't appear in `net share` output

### Pitfall 5: Oplock Break Return Value Handling
**What goes wrong:** NFS handler ignores `ErrLeaseBreakPending` and proceeds with write, causing data corruption
**Why it happens:** The CheckAndBreak methods return `ErrLeaseBreakPending` when a break was initiated but not yet acknowledged
**How to avoid:** When `ErrLeaseBreakPending` is returned, the NFS handler should either (a) wait with a timeout for the break to complete, or (b) proceed after a short delay. Samba's approach: wait up to `oplock_break_wait_time` (default 0 = don't wait, just proceed). For DittoFS: proceed without waiting (fire-and-forget break) since NFS doesn't have a mechanism to block on oplock breaks.
**Warning signs:** SMB cached writes lost after NFS write to same file

### Pitfall 6: Memory Store vs BadgerDB Path Storage Differences
**What goes wrong:** Fix works in memory store tests but fails with BadgerDB/PostgreSQL
**Why it happens:** Memory store keeps `FileAttr` in a map (directly mutable), while BadgerDB serializes/deserializes and PostgreSQL uses SQL columns. Path update must go through `PutFile()` for all stores.
**How to avoid:** Always update the `File.Path` field on the `File` struct and call `PutFile()` to persist. Never rely on in-memory mutation being reflected in the store.
**Warning signs:** Tests pass with memory store but fail with BadgerDB or PostgreSQL conformance tests

## Code Examples

### BUG-01: Sparse Read Zero-Fill Fix Location
```go
// File: pkg/payload/offloader/download.go
// Function: downloadBlock()
// Lines: 31-34
//
// Current: Returns error wrapping ErrBlockNotFound
// Fix: Detect ErrBlockNotFound, create zero-filled block, continue
//
// Also fix: pkg/payload/io/read.go
// Function: ensureAndReadFromCache()
// Lines: 214-228
// Must handle case where EnsureAvailable succeeds but cache ReadAt
// returns found=false (sparse block was zero-filled but not written to cache)
```

### BUG-02: Renamed Directory Path Fix Location
```go
// File: pkg/metadata/file_modify.go
// Function: Move()
// Lines: 510-513 (inside WithTransaction callback)
//
// Add before PutFile:
//   srcFile.Path = destPath
//
// For directory renames, add recursive child path update:
//   if srcFile.Type == FileTypeDirectory {
//       updateDescendantPaths(ctx, tx, srcHandle, oldPath, destPath)
//   }
```

### BUG-03: Parent Directory Navigation Fix Location
```go
// File: internal/adapter/smb/v2/handlers/create.go
// Function: walkPath()
// Lines: 768-770
//
// Current:
//   if part == ".." {
//       // TODO: Handle parent directory navigation
//       continue
//   }
//
// Fix:
//   if part == ".." {
//       file, err := metaSvc.Lookup(authCtx, currentHandle, "..")
//       if err != nil {
//           return nil, err
//       }
//       currentHandle, err = metadata.EncodeFileHandle(file)
//       if err != nil {
//           return nil, err
//       }
//       continue
//   }
```

### BUG-04: Oplock Break Wiring Locations
```go
// NFS v3 handlers with TODO placeholders:
// - internal/adapter/nfs/v3/handlers/write.go:214
// - internal/adapter/nfs/v3/handlers/setattr.go (no placeholder but needs break before truncate)
// - internal/adapter/nfs/v3/handlers/remove.go:153
// - internal/adapter/nfs/v3/handlers/rename.go:254
//
// SMB adapter registration point:
// - pkg/adapter/smb/ Serve() method should call:
//   runtime.SetAdapterProvider("oplock_manager", handler.OplockManager)
//
// OplockManager already implements the needed methods:
// - CheckAndBreakForWrite(ctx, fileHandle)
// - CheckAndBreakForRead(ctx, fileHandle)
// - CheckAndBreakForDelete(ctx, fileHandle)
```

### BUG-05: NumberOfLinks Fix Location
```go
// File: internal/adapter/smb/v2/handlers/converters.go
// Function: FileAttrToFileStandardInfo()
// Line: 162
//
// Current: NumberOfLinks: 1,  // TODO: Track actual link count when available
// Fix:     NumberOfLinks: attr.Nlink,
//
// Note: attr.Nlink is already populated by all store implementations
// (memory, badger, postgres). The value 0 should default to 1 for safety.
```

### BUG-06: Share List Cache Fix Location
```go
// File: internal/adapter/smb/v2/handlers/create.go
// Function: handlePipeCreate()
// Lines: 633-652
//
// Move share list fetching to a cached method on Handler.
// Invalidate via Runtime.RegisterShareChangeCallback() which
// already exists (per STATE.md [26-05] decision).
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Direct cross-protocol calls | Runtime adapter provider pattern | Phase 26-05 | Decoupled adapters communicate via Runtime |
| MetadataService oplock methods | OplockManager in SMB handlers | Phase 26-04 | Methods exist but NFS side not wired |
| TransferManager (monolith) | Offloader (split into focused files) | Phase 29-02 | download.go, upload.go, etc. |
| Single store.go | Split file_create, file_modify, file_remove | Phase 29-03 | Move() is in file_modify.go |

## Open Questions

1. **Recursive path update performance for large directory trees**
   - What we know: `ListChildren` returns entries with pagination. Recursive update within transaction is correct.
   - What's unclear: For very deep/wide directory trees, the transaction may hold locks for too long.
   - Recommendation: Implement iterative (queue-based) traversal within the transaction. Memory store uses global mutex so it's fine. BadgerDB transactions have size limits but directory renames of millions of files are edge cases. Document the limitation.

2. **Oplock break wait strategy for NFS handlers**
   - What we know: `CheckAndBreakForWrite` returns `ErrLeaseBreakPending`. Samba reference uses configurable timeout.
   - What's unclear: Whether NFS handlers should block waiting for the break acknowledgment.
   - Recommendation: Fire-and-forget approach (ignore `ErrLeaseBreakPending`). NFS protocol has no mechanism to delay responses for oplock breaks. The break notification is sent to the SMB client, and subsequent reads will see the updated data. This matches Samba's default behavior (`oplock_break_wait_time = 0`). Log the break for observability.

3. **Sparse block detection scope**
   - What we know: `ErrBlockNotFound` in `downloadBlock()` is the trigger point. Cache `ReadAt` already handles sparse blocks correctly (zeros in dest).
   - What's unclear: Whether `EnsureAvailable` should write zero blocks to cache or skip writing and let `ReadAt` handle it.
   - Recommendation: Skip writing zero blocks to cache. The cache `ReadAt` already returns zeros for missing blocks and sets `found=false`. Update `ensureAndReadFromCache` to treat "no data after successful EnsureAvailable" as sparse zero (return zeros, not error). This avoids wasting cache memory on zero blocks.

## Sources

### Primary (HIGH confidence)
- Codebase analysis: Direct reading of all relevant source files
- `pkg/payload/offloader/download.go` - ErrBlockNotFound propagation confirmed
- `pkg/metadata/file_modify.go` - Missing Path update in Move() confirmed
- `internal/adapter/smb/v2/handlers/create.go` - walkPath TODO confirmed
- `internal/adapter/smb/v2/handlers/converters.go` - Hardcoded NumberOfLinks=1 confirmed
- `internal/adapter/smb/v2/handlers/oplock.go` - CheckAndBreak methods exist
- NFS v3 handler TODOs for oplock break wiring confirmed in write.go, remove.go, rename.go

### Secondary (MEDIUM confidence)
- MS-SMB2 spec references in existing code comments for oplock break semantics
- Samba reference implementation behavior for oplock break timeout (inferred from CONTEXT.md decisions)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All fixes use existing Go code, no new dependencies
- Architecture: HIGH - All patterns already exist in the codebase (adapter providers, share callbacks, cache layer)
- Pitfalls: HIGH - Based on direct code analysis, confirmed root causes, and known store implementation differences

**Research date:** 2026-02-27
**Valid until:** 2026-03-27 (stable codebase, no external dependency changes expected)

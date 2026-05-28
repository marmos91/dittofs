# Phase 43: Local-Only Block Management - Research

**Researched:** 2026-03-09
**Domain:** Go block cache management, nil-store pattern, eviction control
**Confidence:** HIGH

## Summary

Phase 43 adds explicit block management operations to the BlockCache and enables the offloader to operate without a remote block store (nil blockStore). This is a purely internal refactor with no protocol, API, or CLI changes. The work is scoped to three packages: `pkg/cache/`, `pkg/payload/offloader/`, and `pkg/controlplane/runtime/init.go`.

The codebase is well-structured for these changes. The offloader already has partial nil-guard patterns for blockStore (lines 207, 245, 265 in offloader.go), the cache already has `purgeMemBlocks()` and `evictBlock()` as reusable building blocks, and the fdCache has clean `Evict()` and `CloseAll()` methods. The main work is: (1) a new `manage.go` file with 5 block management methods + `SetEvictionEnabled`, (2) removing the panic in `offloader.New()`, (3) adding nil-guards and local-only flush behavior, and (4) wiring the local-only path in `init.go`.

**Primary recommendation:** Implement in 3 focused plans -- cache manage.go methods, offloader nil-store support, and runtime wiring -- building bottom-up from cache to offloader to runtime.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Offloader constructor `New()` accepts nil blockStore (no panic) -- creates local-only offloader
- Each remote method (GetFileSize, Exists, Delete, Truncate) nil-guards internally: `if m.blockStore == nil { return 0/false/nil }`
- Debug log on nil-guard hits: `logger.Debug("offloader: skipping [op], no remote store")`
- HealthCheck with nil blockStore returns nil (healthy, just no remote)
- Finalization callback (onFinalized) skipped entirely in local-only mode
- Flush still flushes memBlocks to .blk files on disk -- disk IS the final store in local-only mode
- Blocks marked BlockStateLocal after flush (not Remote, not Finalized)
- Flush returns Finalized=false -- blocks are in Local state
- `SetEvictionEnabled(enabled bool)` method on BlockCache (not offloader)
- In local-only mode, eviction is hard-coded off
- When eviction is disabled, ensureSpace() skips ListRemoteBlocks query entirely
- Don't start periodic syncer goroutine at all in local-only mode (nil blockStore)
- Add `SetRemoteStore(ctx context.Context, blockStore store.BlockStore)` method on offloader
- SetRemoteStore is one-shot only -- errors if called when remote store already set
- New file: `pkg/cache/manage.go` with 5 management methods + SetEvictionEnabled
- Rename `Remove()` to `EvictMemory()` -- clarifies it only releases in-memory blocks
- All delete operations update diskUsed atomic counter
- All delete operations close open file descriptors from fdCache and readFDCache
- manage.go should use direct PutFileBlock/DeleteFileBlock (not async pendingFBs) since these are explicit operations
- Tests use existing in-memory FileBlockStore with nil blockStore -- no new test infrastructure
- Phase 43 scope for runtime: make offloader constructor accept nil blockStore, make init path able to pass nil
- Actual "create local-only share" flow deferred to Phase 44

### Claude's Discretion
- Exact nil-guard log message wording
- Whether SetEvictionEnabled needs a mutex guard or can use atomic bool
- Error message format for one-shot SetRemoteStore violation
- Test coverage depth for each manage.go method

### Deferred Ideas (OUT OF SCOPE)
- HealthCheck on BlockStore interface (S3 -> HeadBucket, local FS -> Stat, memory -> check) -- Phase 45
- PayloadService elimination -- Phase 45 (absorbed into pkg/blockstore/blockstore.go)
- Share model changes (--local required, --remote optional) -- Phase 44 Data Model
- Disk usage Prometheus metrics for local-only mode -- Phase 49
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| LOCAL-01 | pkg/cache/manage.go provides DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles, GetStoredFileSize, ExistsOnDisk, SetEvictionEnabled | Detailed analysis of cache.go, eviction.go, fdcache.go provides exact patterns for implementation |
| LOCAL-02 | Offloader accepts nil blockStore and operates in local-only mode | Existing nil-guards in offloader.go lines 207/245/265 show the pattern; panic at line 83 must be removed |
| LOCAL-03 | Local-only flush marks blocks BlockStateLocal (no upload) | flush.go already marks BlockStateLocal at line 151; offloader.Flush delegates to cache.Flush which already does this |
| LOCAL-04 | init.go wires local-only mode when no remote store configured | runtime/init.go EnsurePayloadService at line 151 is the wiring point; needs nil blockStore path |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync/atomic` | Go 1.22+ | Atomic bool for eviction flag | Lock-free, matches existing `diskUsed`/`memUsed` pattern |
| Go stdlib `os` | Go 1.22+ | File deletion, stat, directory cleanup | Already used throughout cache package |
| Go stdlib `sync` | Go 1.22+ | Mutex for SetRemoteStore one-shot guard | Already used in offloader.go |

### Supporting
No new dependencies. All changes use existing stdlib packages already imported.

## Architecture Patterns

### Recommended File Structure
```
pkg/cache/
  manage.go           # NEW: DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles, GetStoredFileSize, ExistsOnDisk, SetEvictionEnabled
  manage_test.go      # NEW: Tests for manage.go methods
  cache.go            # MODIFIED: Add evictionEnabled field, rename Remove->EvictMemory
  eviction.go         # MODIFIED: ensureSpace() checks evictionEnabled
  types.go            # UNCHANGED

pkg/payload/offloader/
  offloader.go        # MODIFIED: Remove panic, add nil-guards, add SetRemoteStore, skip syncer
  upload.go           # MODIFIED: Add nil blockStore guard in uploadPendingBlocks
  download.go         # MODIFIED: Add nil blockStore guard in downloadBlock and inlineDownloadOrWait

pkg/payload/service.go  # MODIFIED: Update Remove->EvictMemory call

pkg/controlplane/runtime/
  init.go             # MODIFIED: Allow nil blockStore path
```

### Pattern 1: Nil-Guard with Debug Log
**What:** Each remote-dependent method returns a zero value when blockStore is nil
**When to use:** Every offloader method that touches blockStore
**Example:**
```go
func (m *Offloader) GetFileSize(ctx context.Context, payloadID string) (uint64, error) {
    if !m.canProcess(ctx) {
        return 0, fmt.Errorf("offloader is closed")
    }
    if m.blockStore == nil {
        logger.Debug("offloader: skipping GetFileSize, no remote store")
        return 0, nil
    }
    // ... existing logic
}
```

### Pattern 2: Atomic Bool for Eviction Control
**What:** Use `atomic.Bool` for the eviction-enabled flag (matches existing `closedFlag` pattern)
**When to use:** SetEvictionEnabled and ensureSpace
**Example:**
```go
// In BlockCache struct
evictionEnabled atomic.Bool

func (bc *BlockCache) SetEvictionEnabled(enabled bool) {
    bc.evictionEnabled.Store(enabled)
}

// In ensureSpace
func (bc *BlockCache) ensureSpace(ctx context.Context, needed int64) error {
    if bc.maxDisk <= 0 {
        return nil
    }
    if !bc.evictionEnabled.Load() {
        // Fast path: eviction disabled, skip ListRemoteBlocks query.
        // Blocks cannot be evicted (no remote store to re-fetch from).
        if bc.diskUsed.Load()+needed > bc.maxDisk {
            return ErrDiskFull
        }
        return nil
    }
    // ... existing eviction logic
}
```

### Pattern 3: Direct Metadata Operations (Not Async)
**What:** manage.go uses direct `PutFileBlock`/`DeleteFileBlock` calls, not the async `pendingFBs` queue
**When to use:** All manage.go methods (these are explicit operations, not hot-path write buffering)
**Why:** The async queue is an optimization for high-frequency writes (4KB NFS writes batched every 200ms). Manage operations are infrequent and must be immediately consistent.
**Example:**
```go
func (bc *BlockCache) DeleteBlockFile(ctx context.Context, payloadID string, blockIdx uint64) error {
    key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
    blockID := makeBlockID(key)

    // 1. Close FDs (must happen before file deletion)
    bc.fdCache.Evict(blockID)
    bc.readFDCache.Evict(blockID)

    // 2. Purge memBlock
    bc.purgeMemBlocks(payloadID, func(idx uint64) bool { return idx == blockIdx })

    // 3. Get metadata and delete disk file
    fb, err := bc.blockStore.GetFileBlock(ctx, blockID)
    if err != nil {
        // Also delete from pendingFBs in case it's queued but not persisted
        bc.pendingFBs.Delete(blockID)
        return nil // Block doesn't exist, nothing to do
    }

    if fb.CachePath != "" {
        info, statErr := os.Stat(fb.CachePath)
        var fileSize int64
        if statErr == nil {
            fileSize = info.Size()
        } else {
            fileSize = int64(fb.DataSize)
        }
        if rmErr := os.Remove(fb.CachePath); rmErr != nil && !os.IsNotExist(rmErr) {
            return fmt.Errorf("remove cache file: %w", rmErr)
        }
        if fileSize > 0 {
            bc.diskUsed.Add(-fileSize)
        }
    }

    // 4. Delete metadata (direct, not async)
    _ = bc.blockStore.DeleteFileBlock(ctx, blockID)
    bc.pendingFBs.Delete(blockID) // Clear any pending update too

    return nil
}
```

### Pattern 4: SetRemoteStore One-Shot
**What:** Atomic transition from local-only to remote-backed mode
**When to use:** When a remote store is added to an existing local-only setup
**Example:**
```go
func (m *Offloader) SetRemoteStore(ctx context.Context, blockStore store.BlockStore) error {
    m.mu.Lock()
    defer m.mu.Unlock()

    if m.blockStore != nil {
        return fmt.Errorf("remote store already set")
    }
    if blockStore == nil {
        return fmt.Errorf("blockStore must not be nil")
    }

    m.blockStore = blockStore
    m.cache.SetEvictionEnabled(true)

    // Start periodic syncer with the provided context
    interval := m.config.UploadInterval
    if interval <= 0 {
        interval = 2 * time.Second
    }
    go m.periodicUploader(ctx, interval)

    return nil
}
```

### Anti-Patterns to Avoid
- **Modifying pendingFBs from manage.go without also clearing from blockStore:** Both the sync.Map and the persistent store must be updated.
- **Forgetting fdCache cleanup before file deletion:** Deleting a .blk file while an fd is cached causes stale reads and EBADF.
- **Using the async pendingFBs queue for manage.go operations:** These need immediate consistency, not eventual consistency.
- **Starting the periodic syncer when blockStore is nil:** Wastes goroutine and will nil-panic on ListLocalBlocks callback.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Atomic flag for eviction | sync.Mutex-guarded bool | `atomic.Bool` | Matches existing `closedFlag` pattern in cache.go, lock-free |
| FD cleanup on delete | Manual per-fd tracking | `fdCache.Evict(blockID)` + `readFDCache.Evict(blockID)` | Both caches already have Evict() method |
| Memory cleanup | Manual map iteration | `purgeMemBlocks(payloadID, filter)` | Already handles locking, buffer return, memUsed accounting |
| Disk size tracking | Manual stat+update | `diskUsed.Add(-fileSize)` pattern from `evictBlock()` | Existing atomic counter pattern |

## Common Pitfalls

### Pitfall 1: Stale pendingFBs After Direct Delete
**What goes wrong:** DeleteBlockFile deletes from blockStore but a queued update in `pendingFBs` re-creates the FileBlock on next SyncFileBlocks tick.
**Why it happens:** The async `pendingFBs` sync.Map runs independently of direct blockStore operations.
**How to avoid:** Always call `bc.pendingFBs.Delete(blockID)` alongside `bc.blockStore.DeleteFileBlock()`.
**Warning signs:** Deleted blocks reappear in ListLocalBlocks after 200ms.

### Pitfall 2: FD Leak on Block Deletion
**What goes wrong:** Deleting a .blk file without evicting cached FDs leaves open file descriptors to deleted files. On Linux the data isn't freed until FDs close. On macOS the fd becomes invalid.
**Why it happens:** fdCache and readFDCache hold open FDs indexed by blockID.
**How to avoid:** Call `bc.fdCache.Evict(blockID)` and `bc.readFDCache.Evict(blockID)` BEFORE `os.Remove()`.
**Warning signs:** Growing memory from unreleased inodes, stale read errors.

### Pitfall 3: Race Between ensureSpace and SetEvictionEnabled
**What goes wrong:** A concurrent write calls ensureSpace which enters the eviction loop, then SetEvictionEnabled(false) is called, but eviction continues.
**Why it happens:** ensureSpace reads the flag once at the top but the loop continues without re-checking.
**How to avoid:** The atomic.Bool read at the top of ensureSpace is sufficient. If eviction starts and then gets disabled, the in-flight eviction completes harmlessly (blocks are already Remote). No special handling needed.

### Pitfall 4: Parent Directory Cleanup in DeleteAllBlockFiles
**What goes wrong:** After deleting all .blk files for a payloadID, the shard directory (`<baseDir>/<shard>/<payloadID>/`) is left empty.
**Why it happens:** os.Remove only removes files, not empty parent directories.
**How to avoid:** After deleting all blocks for a payloadID, attempt `os.Remove()` on the payloadID directory. Use os.Remove (not os.RemoveAll) to only remove if empty. Ignore ENOTEMPTY.

### Pitfall 5: Offloader stopCh Already Closed in SetRemoteStore
**What goes wrong:** If Close() was called before SetRemoteStore, the stopCh is already closed. Starting the periodicUploader would immediately exit.
**Why it happens:** Close() closes stopCh.
**How to avoid:** Check `m.closed` inside the `m.mu.Lock()` section of SetRemoteStore and return error.

## Code Examples

### Existing Remove() (to be renamed EvictMemory)
```go
// Source: pkg/cache/cache.go:339-347
func (bc *BlockCache) Remove(_ context.Context, payloadID string) error {
    bc.purgeMemBlocks(payloadID, func(uint64) bool { return true })
    bc.filesMu.Lock()
    delete(bc.files, payloadID)
    bc.filesMu.Unlock()
    return nil
}
```

### Existing evictBlock() Pattern (model for manage.go disk operations)
```go
// Source: pkg/cache/eviction.go:56-84
func (bc *BlockCache) evictBlock(ctx context.Context, fb *metadata.FileBlock) error {
    if fb.CachePath == "" {
        return nil
    }
    info, err := os.Stat(fb.CachePath)
    var fileSize int64
    if err == nil {
        fileSize = info.Size()
    } else {
        fileSize = int64(fb.DataSize)
    }
    cachePath := fb.CachePath
    fb.CachePath = ""
    if err := bc.blockStore.PutFileBlock(ctx, fb); err != nil {
        return fmt.Errorf("update block metadata: %w", err)
    }
    if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
        return fmt.Errorf("remove cache file: %w", err)
    }
    if fileSize > 0 {
        bc.diskUsed.Add(-fileSize)
    }
    return nil
}
```

### Existing Offloader Nil-Guard Pattern
```go
// Source: pkg/payload/offloader/offloader.go:241-258
func (m *Offloader) Exists(ctx context.Context, payloadID string) (bool, error) {
    if !m.canProcess(ctx) {
        return false, fmt.Errorf("offloader is closed")
    }
    if m.blockStore == nil {
        return false, fmt.Errorf("no block store configured")
    }
    // ... rest of method
}
```

### Callers of Remove() That Need Updating
```go
// Source: pkg/payload/service.go:164
if err := s.cache.Remove(ctx, payloadID); err != nil {

// Source: pkg/cache/cache_test.go:303
if err := bc.Remove(ctx, "file1"); err != nil {

// Source: pkg/payload/store/blockstore_integration_test.go:477
bc.Remove(ctx, payloadID)
```

### blockPath() for Computing Cache File Paths
```go
// Source: pkg/cache/flush.go:217-222
func (bc *BlockCache) blockPath(blockID string) string {
    if len(blockID) < 2 {
        return filepath.Join(bc.baseDir, blockID+".blk")
    }
    return filepath.Join(bc.baseDir, blockID[:2], blockID+".blk")
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `Remove()` releases memory only | `EvictMemory()` (renamed) + `DeleteAllBlockFiles()` (new) | Phase 43 | Clear separation: memory-release vs full-delete |
| Offloader panics on nil blockStore | Accepts nil, operates local-only | Phase 43 | Enables local-only shares without remote backend |
| Eviction always enabled | Controllable via `SetEvictionEnabled()` | Phase 43 | Prevents data loss when blocks can't be re-fetched |

## Open Questions

1. **Should DeleteAllBlockFiles also clean up the fileInfo (files map)?**
   - What we know: `EvictMemory()` (formerly Remove) already cleans up `bc.files[payloadID]`. DeleteAllBlockFiles should too, since the file is being fully deleted.
   - Recommendation: Yes, include `bc.filesMu.Lock(); delete(bc.files, payloadID); bc.filesMu.Unlock()` in DeleteAllBlockFiles.

2. **TruncateBlockFiles partial block handling**
   - What we know: CONTEXT.md says "remove whole blocks where blockIdx * BlockSize >= newSize (no partial block truncation)".
   - What's unclear: Should the last surviving partial block's DataSize be updated if newSize doesn't align to block boundary?
   - Recommendation: No -- only remove whole blocks past the boundary. The existing `Truncate()` method already handles the memory-side truncation and file size update. TruncateBlockFiles is the disk counterpart.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | none (Go convention) |
| Quick run command | `go test ./pkg/cache/ ./pkg/payload/offloader/ -count=1` |
| Full suite command | `go test ./... -count=1` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| LOCAL-01 | manage.go methods work correctly | unit | `go test ./pkg/cache/ -run TestManage -count=1 -v` | Wave 0 |
| LOCAL-01 | SetEvictionEnabled controls ensureSpace | unit | `go test ./pkg/cache/ -run TestEvictionEnabled -count=1 -v` | Wave 0 |
| LOCAL-01 | Remove renamed to EvictMemory, callers updated | unit | `go test ./pkg/cache/ -run TestEvictMemory -count=1 -v` | Wave 0 |
| LOCAL-02 | Offloader accepts nil blockStore | unit | `go test ./pkg/payload/offloader/ -run TestNilBlockStore -count=1 -v` | Wave 0 |
| LOCAL-02 | SetRemoteStore one-shot transition | unit | `go test ./pkg/payload/offloader/ -run TestSetRemoteStore -count=1 -v` | Wave 0 |
| LOCAL-03 | Flush marks blocks Local, not Remote | unit | `go test ./pkg/cache/ -run TestFlush -count=1 -v` | Existing (cache_test.go) |
| LOCAL-04 | init.go accepts nil blockStore path | unit | `go test ./pkg/controlplane/runtime/ -count=1 -v` | Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./pkg/cache/ ./pkg/payload/offloader/ ./pkg/payload/ -count=1`
- **Per wave merge:** `go test ./... -count=1`
- **Phase gate:** `go build ./... && go test ./... -count=1`

### Wave 0 Gaps
- [ ] `pkg/cache/manage_test.go` -- covers LOCAL-01 (DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles, GetStoredFileSize, ExistsOnDisk, SetEvictionEnabled)
- [ ] Tests for EvictMemory rename (update existing TestRemove in cache_test.go)
- [ ] Tests for nil blockStore offloader (new test file or extend offloader_test.go)
- [ ] Tests for SetRemoteStore one-shot behavior

## Sources

### Primary (HIGH confidence)
- Direct source code analysis of pkg/cache/cache.go, eviction.go, flush.go, fdcache.go, write.go, read.go, block.go, types.go
- Direct source code analysis of pkg/payload/offloader/offloader.go, upload.go, download.go, dedup.go, queue.go, types.go
- Direct source code analysis of pkg/payload/service.go
- Direct source code analysis of pkg/controlplane/runtime/init.go, runtime.go
- Direct source code analysis of pkg/metadata/object.go, store.go

### Secondary (MEDIUM confidence)
- CONTEXT.md decisions and code_context section (user-verified integration points with line numbers)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - pure Go stdlib, no new dependencies
- Architecture: HIGH - all integration points verified via direct code reading
- Pitfalls: HIGH - identified from actual code patterns and data flow analysis

**Research date:** 2026-03-09
**Valid until:** 2026-04-09 (stable codebase, internal refactor)

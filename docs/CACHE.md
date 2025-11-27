# DittoFS Cache Design

This document describes the unified cache architecture for DittoFS, designed to efficiently handle NFS read and write operations with any content store backend.

## Table of Contents

- [Overview](#overview)
- [Design Principles](#design-principles)
- [Cache Architecture](#cache-architecture)
- [State Machine](#state-machine)
- [NFS Operation Flow](#nfs-operation-flow)
- [Read Cache Coherency](#read-cache-coherency)
- [Flush Coordination](#flush-coordination)
- [Configuration](#configuration)
- [Implementation Details](#implementation-details)

## Overview

DittoFS uses a **unified cache per share** that serves both read and write operations. This design eliminates the complexity of coordinating separate read/write caches while providing efficient buffering for all content store backends.

### Key Insights

1. **Written data becomes readable data**: A unified cache reflects this naturally - data written via NFS WRITE is immediately readable via NFS READ from the same buffer.

2. **Cache is content-store agnostic**: The cache only buffers data and tracks state. It doesn't know about S3 multipart uploads or filesystem fsync - that's the content store's responsibility.

3. **Dirty vs Clean distinction**: Dirty entries (being written) are authoritative. Clean entries (read-cached) need validation against metadata.

### The NFS Challenge

NFSv3 is stateless and has no "file close" operation:

```
NFS Client                    DittoFS
    │                            │
    ├── WRITE(0, 32KB) ─────────►│  ← Write 32KB at offset 0
    ├── WRITE(32KB, 32KB) ──────►│  ← Write 32KB at offset 32KB
    ├── COMMIT ─────────────────►│  ← Persist data (but file may not be complete!)
    ├── WRITE(64KB, 32KB) ──────►│  ← More writes after COMMIT
    ├── COMMIT ─────────────────►│  ← Another COMMIT
    │                            │
    │  [client closes file]      │
    │  [NO RPC SENT TO SERVER!]  │  ← Server never knows file is "done"
```

This creates challenges for backends like S3 where we need to call `CompleteMultipartUpload` to finalize a file. We solve this with **inactivity-based finalization**.

## Design Principles

### 1. One Cache Per Share

Each NFS share has its own cache instance:

- **Isolation**: One busy share cannot evict another share's data
- **Predictability**: Per-share cache sizing and tuning
- **Simplicity**: No complex cross-share coordination

### 2. Cache is a Smart Buffer (Content-Store Agnostic)

The cache buffers data and tracks state, but delegates all persistence to the content store:

- **No S3 knowledge**: Cache doesn't know about multipart uploads, ETags, etc.
- **No filesystem knowledge**: Cache doesn't know about fsync, file descriptors, etc.
- **Simple interface**: Write, Read, track state, track what's been flushed

### 3. Three States Only

```go
type CacheState int

const (
    StateBuffering CacheState = iota  // Writes accumulating, not yet flushed
    StateUploading                     // Flush in progress to content store
    StateCached                        // Clean data (finalized writes OR read-cached)
)
```

- **Buffering**: Dirty, actively receiving writes
- **Uploading**: Dirty, flush in progress (content store may be doing multipart)
- **Cached**: Clean, can be evicted, needs validation on read

### 4. Dirty Entry Protection

Entries with unflushed data cannot be evicted:

- LRU eviction skips dirty entries (Buffering, Uploading)
- Prevents data loss under memory pressure
- Cache may temporarily exceed max size to protect dirty data

### 5. Read Cache Coherency

Clean cached data needs validation against current metadata:

- Store `mtime` and `size` when caching data
- On cache hit, compare with current metadata
- Invalidate if metadata changed (file was modified)

## Cache Architecture

### Cache Entry Structure

```go
type CacheEntry struct {
    ContentID      string
    Buffer         []byte

    // Size tracking
    BufferSize     int64    // total bytes in buffer
    FlushedOffset  int64    // bytes that have been flushed to content store

    // Timing
    LastWriteTime  time.Time  // last WRITE operation
    LastAccessTime time.Time  // last read or write (for LRU)
    CachedAt       time.Time  // when first cached (for TTL)

    // Validity (for read cache coherency)
    CachedMtime    time.Time  // file mtime when data was cached
    CachedSize     uint64     // file size when data was cached

    // State
    State          CacheState
}
```

**Note**: No `UploadID`, `PartNumber`, or any S3-specific fields. The content store tracks its own upload state internally via `GetIncrementalWriteState()`.

### Cache Interface

```go
type Cache interface {
    // Write operations
    WriteAt(ctx context.Context, id ContentID, data []byte, offset int64) error

    // Read operations
    ReadAt(ctx context.Context, id ContentID, buf []byte, offset int64) (int, error)
    Read(ctx context.Context, id ContentID) ([]byte, error)

    // State management
    GetState(id ContentID) CacheState
    SetState(id ContentID, state CacheState)
    GetFlushedOffset(id ContentID) int64
    SetFlushedOffset(id ContentID, offset int64)

    // Validity (for read cache coherency)
    GetCachedMetadata(id ContentID) (mtime time.Time, size uint64, ok bool)
    SetCachedMetadata(id ContentID, mtime time.Time, size uint64)
    IsValid(id ContentID, currentMtime time.Time, currentSize uint64) bool

    // Size and timing
    Size(id ContentID) int64
    LastWrite(id ContentID) time.Time
    LastAccess(id ContentID) time.Time
    Exists(id ContentID) bool
    List() []ContentID

    // Cache management
    Remove(id ContentID) error
    RemoveAll() error
    TotalSize() int64
    MaxSize() int64

    // Lifecycle
    Close() error
}
```

## State Machine

```
                          WRITE (new entry)
                                │
                                ▼
              ┌─────────────────────────────────┐
              │           BUFFERING             │
              │   (dirty, accumulating writes)  │◄─────────────┐
              └────────────────┬────────────────┘              │
                               │                               │
                    COMMIT triggers flush                      │
                    (content store decides how)                │
                               │                               │
                               ▼                               │
              ┌─────────────────────────────────┐              │
              │           UPLOADING             │              │
              │    (dirty, flush in progress)   │              │
              └────────────────┬────────────────┘              │
                               │                               │
                     Finalization complete                     │
                     (inactivity timeout)                      │
                               │                               │
                               ▼                               │
              ┌─────────────────────────────────┐              │
     ┌───────►│            CACHED               │──── WRITE ───┘
     │        │  (clean, can be evicted)        │   (restart)
     │        └────────────────┬────────────────┘
     │                         │
     │                    Eviction
READ │                    (LRU)
(miss)                         │
     │                         ▼
     │                    [removed]
     │
     └─── READ populates cache directly as CACHED
```

### State: BUFFERING

Initial state when writes begin. Data accumulates in the cache buffer.

**Characteristics:**
- Dirty (source of truth)
- Cannot be evicted
- `FlushedOffset` may be 0 or behind `BufferSize`

**Transitions:**
- **WRITE** → Stay in BUFFERING
- **COMMIT** triggers flush → Transition to UPLOADING

### State: UPLOADING

Flush in progress. The content store is persisting data (may be doing multipart upload, streaming write, etc.).

**Characteristics:**
- Dirty (flush not complete)
- Cannot be evicted
- `FlushedOffset` increases as content store confirms persistence

**Transitions:**
- **WRITE** → Stay in UPLOADING (more data to flush)
- **Finalization** (inactivity timeout + all data flushed) → Transition to CACHED

### State: CACHED

Clean data. Either finalized from writes, or populated from a read.

**Characteristics:**
- Clean (content store is source of truth)
- Can be evicted
- Needs validation on read hit (compare mtime/size)

**Transitions:**
- **WRITE** → Transition to BUFFERING (new version)
- **Eviction** → Remove from cache
- **Invalidation** (metadata changed) → Remove from cache

## NFS Operation Flow

### WRITE Handler

```
WRITE(handle, offset, data)
    │
    ▼
┌─────────────────────────────────┐
│ Get file metadata (contentID)   │
└───────────────┬─────────────────┘
                │
                ▼
┌─────────────────────────────────┐
│ Get/create cache entry          │
│                                 │
│ If state == CACHED:             │
│   → Reset to BUFFERING          │
│   → Clear FlushedOffset         │
└───────────────┬─────────────────┘
                │
                ▼
┌─────────────────────────────────┐
│ cache.WriteAt(contentID,        │
│               data, offset)     │
│                                 │
│ Update LastWriteTime            │
│ Update LastAccessTime           │
└───────────────┬─────────────────┘
                │
                ▼
          Return SUCCESS
          (no content store calls)
```

### COMMIT Handler

```
COMMIT(handle, offset, count)
    │
    ▼
┌─────────────────────────────────┐
│ Get cache entry                 │
│ (return OK if not found)        │
└───────────────┬─────────────────┘
                │
                ▼
┌─────────────────────────────────┐
│ Calculate unflushed:            │
│ unflushed = BufferSize -        │
│             FlushedOffset       │
└───────────────┬─────────────────┘
                │
                ▼
┌─────────────────────────────────┐
│ Flush to content store          │
│ (content store decides how:     │
│  - S3: multipart if ≥5MB        │
│  - FS: WriteAt + fsync          │
│  - Memory: WriteContent)        │
│                                 │
│ Update FlushedOffset            │
│ Set state = UPLOADING           │
└───────────────┬─────────────────┘
                │
                ▼
          Return SUCCESS
```

### READ Handler

```
READ(handle, offset, size)
    │
    ▼
┌─────────────────────────────────┐
│ Get file metadata               │
│ (mtime, size, contentID)        │
└───────────────┬─────────────────┘
                │
                ▼
┌─────────────────────────────────┐
│ Check cache for contentID       │
└───────────────┬─────────────────┘
                │
        ┌───────┴───────┐
        │               │
    Cache hit       Cache miss
        │               │
        ▼               │
┌───────────────┐       │
│ Validate:     │       │
│ - Dirty? OK   │       │
│ - mtime match?│       │
│ - size match? │       │
│ - TTL ok?     │       │
└───────┬───────┘       │
        │               │
   ┌────┴────┐          │
   │         │          │
 Valid    Invalid       │
   │         │          │
   ▼         ▼          ▼
┌───────┐ ┌─────────────────┐
│ Serve │ │ Invalidate      │
│ from  │ │ Read from store │
│ cache │ │ Populate cache  │
│       │ │ as CACHED       │
└───────┘ └─────────────────┘
```

## Read Cache Coherency

### The Problem

Cached read data can become stale:
1. Another NFS client modifies the file
2. Direct backend modification (e.g., S3 console)
3. File deleted and recreated with same name

### Solution: Metadata Validation

Store metadata snapshot when caching, validate on hit:

```go
func (c *Cache) IsValid(id ContentID, currentMtime time.Time, currentSize uint64) bool {
    entry := c.getEntry(id)
    if entry == nil {
        return false
    }

    // Dirty entries are always valid (we're the source of truth)
    if entry.State == StateBuffering || entry.State == StateUploading {
        return true
    }

    // Clean entries: validate against current metadata
    if entry.CachedMtime != currentMtime || entry.CachedSize != currentSize {
        return false  // File was modified, invalidate
    }

    // Optional: TTL check for extra safety
    if c.readTTL > 0 && time.Since(entry.CachedAt) > c.readTTL {
        return false  // Expired
    }

    return true
}
```

### Dirty vs Clean

| State | Source of Truth | Validation Required |
|-------|-----------------|---------------------|
| Buffering | Cache (dirty) | No - always valid |
| Uploading | Cache (dirty) | No - always valid |
| Cached | Content Store | Yes - check mtime/size |

### Handling External Modifications

If someone modifies the content store directly (bypassing DittoFS):

1. **Metadata updated** (normal NFS flow): mtime/size check catches it
2. **Metadata NOT updated** (direct S3 access): TTL provides eventual consistency
3. **Disable caching**: For shares with expected direct access, disable cache

## Flush Coordination

### Separation of Concerns

| Component | Responsibility |
|-----------|---------------|
| **Cache** | Buffer data, track state, track flushed offset |
| **Content Store** | Persist data, manage upload sessions internally |
| **Flush Coordinator** | Orchestrate flush timing, call content store APIs |
| **Background Finalizer** | Detect idle files, trigger finalization |

### Flush Coordinator (in handlers)

```go
func flushToContentStore(ctx context.Context, cache Cache, contentStore ContentStore, contentID ContentID) error {
    entry := cache.GetEntry(contentID)
    unflushed := entry.BufferSize - entry.FlushedOffset

    if unflushed == 0 {
        return nil  // Nothing to flush
    }

    // Read unflushed data from cache
    data := make([]byte, unflushed)
    cache.ReadAt(ctx, contentID, data, entry.FlushedOffset)

    // Check if content store supports incremental writes
    if incStore, ok := contentStore.(IncrementalWriteStore); ok {
        // S3: content store manages multipart internally
        flushed, err := incStore.FlushIncremental(ctx, contentID, data)
        if err != nil {
            return err
        }
        cache.SetFlushedOffset(contentID, entry.FlushedOffset + flushed)
    } else {
        // Simple store: write at offset
        err := contentStore.WriteAt(ctx, contentID, data, entry.FlushedOffset)
        if err != nil {
            return err
        }
        cache.SetFlushedOffset(contentID, entry.FlushedOffset + int64(len(data)))
    }

    cache.SetState(contentID, StateUploading)
    return nil
}
```

### Background Finalizer

Runs periodically to finalize idle files:

```go
func (f *Finalizer) sweepForFinalization(ctx context.Context) {
    threshold := time.Now().Add(-f.finalizeTimeout)

    for _, id := range f.cache.List() {
        entry := f.cache.GetEntry(id)

        // Skip if not idle or not fully flushed
        if entry.LastWriteTime.After(threshold) {
            continue  // Still active
        }
        if entry.FlushedOffset < entry.BufferSize {
            continue  // Still has unflushed data
        }
        if entry.State == StateCached {
            continue  // Already finalized
        }

        // Finalize
        f.finalize(ctx, id)
    }
}

func (f *Finalizer) finalize(ctx context.Context, id ContentID) error {
    contentStore := f.registry.GetContentStore(shareID)

    // Complete any in-progress upload (S3 multipart, etc.)
    if incStore, ok := contentStore.(IncrementalWriteStore); ok {
        if state := incStore.GetIncrementalWriteState(id); state != nil {
            if err := incStore.CompleteIncrementalWrite(ctx, id); err != nil {
                return err
            }
        }
    }

    // Mark as cached (clean)
    f.cache.SetState(id, StateCached)

    // Store current metadata for validation
    meta := f.metadataStore.GetFile(ctx, handle)
    f.cache.SetCachedMetadata(id, meta.Mtime, meta.Size)

    return nil
}
```

## Configuration

### Share-Level Cache Configuration

```yaml
shares:
  - name: /data
    metadata_store: badger-meta
    content_store: s3-content
    cache:
      enabled: true
      max_size: 1GB
      finalize_timeout: 30s    # Inactivity before finalizing writes
      sweep_interval: 10s      # How often to check for finalization
      read_ttl: 300s           # Max age for read-cached data (0 = no TTL)
      validate_on_read: true   # Compare mtime/size on cache hit
```

### Configuration Options

| Option | Default | Description |
|--------|---------|-------------|
| `enabled` | `true` | Enable/disable caching for this share |
| `max_size` | `256MB` | Maximum total cache size |
| `finalize_timeout` | `30s` | Time since last write before finalizing |
| `sweep_interval` | `10s` | Background sweeper interval |
| `read_ttl` | `0` | Max age for cached reads (0 = no expiry, rely on validation) |
| `validate_on_read` | `true` | Validate mtime/size on cache hit |

## Implementation Details

### Dirty Entry Protection

```go
func (c *Cache) canEvict(entry *CacheEntry) bool {
    // Cannot evict dirty entries
    if entry.State == StateBuffering || entry.State == StateUploading {
        return false
    }
    // Clean entries can be evicted
    return true
}
```

### Eviction Strategy

LRU eviction with dirty entry protection:

1. Sort entries by `LastAccessTime` (oldest first)
2. Skip entries where `canEvict() == false`
3. Evict until `TotalSize <= MaxSize * 0.9` (hysteresis)
4. If all entries are dirty, allow temporary overflow

### Graceful Shutdown

1. Stop accepting new operations
2. For each dirty entry:
   - Flush remaining data to content store
   - Complete any in-progress uploads
3. Clear cache

### Thread Safety

- Per-entry mutex for buffer operations
- Cache-level RWMutex for entry map
- Atomic operations for `TotalSize` tracking
- Background finalizer uses separate goroutine with context cancellation

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `dittofs_cache_size_bytes` | Gauge | Current cache size per share |
| `dittofs_cache_entries` | Gauge | Number of cached entries |
| `dittofs_cache_hits_total` | Counter | Cache read hits |
| `dittofs_cache_misses_total` | Counter | Cache read misses |
| `dittofs_cache_invalidations_total` | Counter | Cache invalidations (stale data) |
| `dittofs_cache_writes_total` | Counter | Total write operations |
| `dittofs_cache_flushes_total` | Counter | Flush operations |
| `dittofs_cache_finalizations_total` | Counter | File finalizations |
| `dittofs_cache_evictions_total` | Counter | LRU evictions |

## Summary

| Operation | Cache Action | Content Store Action |
|-----------|--------------|----------------------|
| WRITE | Buffer data, state=Buffering | None |
| COMMIT | Trigger flush, state=Uploading | Persist data (method varies by backend) |
| READ (dirty hit) | Serve from buffer | None |
| READ (clean hit) | Validate → Serve or invalidate | None or GET on invalidation |
| READ (miss) | Populate as Cached | GET object |
| Finalization | state=Cached, store metadata | Complete upload (if applicable) |
| Eviction | Remove clean entry | None |
| Shutdown | Flush all dirty entries | Complete all uploads |

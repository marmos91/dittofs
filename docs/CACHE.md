# DittoFS Cache Design

This document describes the Chunk/Slice/Block cache architecture for DittoFS, designed for high-throughput NFS operations with crash recovery via WAL persistence.

## Table of Contents

- [Overview](#overview)
- [Chunk/Slice/Block Model](#chunksliceblock-model)
- [Cache Architecture](#cache-architecture)
- [Write Path](#write-path)
- [Read Path](#read-path)
- [Flush and Transfer](#flush-and-transfer)
- [WAL Persistence](#wal-persistence)
- [Configuration](#configuration)
- [Performance Characteristics](#performance-characteristics)

## Overview

DittoFS uses a **three-tier storage model** (Chunk/Slice/Block) with a **WAL-backed cache** for durability. This design provides:

- **High write throughput**: Non-blocking flush returns immediately (~275+ MB/s)
- **Crash recovery**: WAL persistence survives server restarts
- **Efficient S3 usage**: 4MB block uploads minimize API calls
- **Sequential write optimization**: Adjacent writes merge into single slices

### Key Insights

1. **Slices buffer variable-size writes**: NFS WRITE operations (typically 32KB) accumulate as slices in cache
2. **Blocks are the upload unit**: 4MB blocks are uploaded to S3/storage independently
3. **Eager upload**: Complete blocks are uploaded immediately, not waiting for COMMIT
4. **Non-blocking COMMIT**: Flush returns immediately; durability provided by WAL

## Chunk/Slice/Block Model

```
File (arbitrary size)
  │
  ├── Chunk 0 (64MB)
  │     ├── Slice A (offset=0, len=32KB)     ← NFS WRITE #1
  │     ├── Slice B (offset=32KB, len=32KB)  ← NFS WRITE #2 (may merge with A)
  │     └── ...
  │     │
  │     └── When flushed, becomes:
  │           ├── Block 0 (4MB) → S3 key: {payloadID}/chunk-0/block-0
  │           ├── Block 1 (4MB) → S3 key: {payloadID}/chunk-0/block-1
  │           └── ... (16 blocks per chunk)
  │
  ├── Chunk 1 (64MB)
  │     └── ...
  │
  └── ...
```

### Terminology

| Term | Size | Description |
|------|------|-------------|
| **Chunk** | 64MB | Logical file region for organization |
| **Slice** | Variable | Cached write data (pending upload) |
| **Block** | 4MB | Physical storage unit uploaded to S3 |

### Size Constants

```go
// pkg/payload/chunk/chunk.go
const Size = 64 * 1024 * 1024  // 64MB chunks

// pkg/payload/block/block.go
const Size = 4 * 1024 * 1024   // 4MB blocks
const PerChunk = 16            // 64MB / 4MB = 16 blocks per chunk
```

## Cache Architecture

### Slice Structure

```go
// pkg/cache/wal/types.go
type Slice struct {
    ID        string      // Unique slice identifier
    Offset    int64       // Offset within chunk
    Length    int64       // Data length
    Data      []byte      // Actual bytes
    State     SliceState  // Pending, Uploading, or Flushed
    CreatedAt time.Time   // For newest-wins ordering
}
```

### Slice States

```
┌─────────────────┐     Eager upload or     ┌─────────────────┐     Upload complete    ┌─────────────────┐
│ SliceStatePending│─────────────────────────►│SliceStateUploading│───────────────────────►│ SliceStateFlushed│
│   (dirty data)   │      COMMIT/Flush       │  (upload active)  │     Mark flushed      │  (safe to evict) │
└─────────────────┘                          └─────────────────┘                        └─────────────────┘
       ▲                                                                                        │
       │                                                                                        │
       └────────────────────── New WRITE operation ─────────────────────────────────────────────┘
                               (creates new slice)
```

- **Pending**: Dirty data, source of truth, cannot evict
- **Uploading**: Upload in progress, cannot evict
- **Flushed**: Safe to evict, data is in block store

### Cache Entry (Per-Chunk)

```go
// pkg/cache/types.go
type chunkEntry struct {
    slices     []*wal.Slice  // All slices for this chunk
    totalSize  int64         // Sum of slice data
    lastAccess time.Time     // For LRU eviction
}
```

## Write Path

### NFS WRITE → Cache

```
NFS WRITE(handle, offset, data)
    │
    ▼
PayloadService.WriteAt()
    │
    ├── Calculate chunk index: chunkIdx = offset / 64MB
    ├── Calculate offset within chunk
    │
    ▼
Cache.WriteSlice(fileHandle, chunkIdx, data, offset)
    │
    ├── Check sequential write optimization:
    │   │
    │   ├── If last slice ends at current offset:
    │   │   └── Extend existing slice (append data)
    │   │
    │   └── Otherwise:
    │       └── Create new slice
    │
    ├── Persist to WAL (if enabled)
    │
    └── Return success
    │
    ▼
TransferManager.OnWriteComplete()
    │
    ├── Calculate which 4MB blocks overlap the write
    │
    ├── For each complete block:
    │   ├── Check if already uploaded (deduplication)
    │   ├── Check if cache covers entire block
    │   └── If covered → Start async block upload
    │
    └── Return immediately (non-blocking)
```

### Sequential Write Optimization

Adjacent writes merge into a single slice:

```
WRITE(offset=0, 32KB)     → Slice A: [0, 32KB]
WRITE(offset=32KB, 32KB)  → Extend A: [0, 64KB]  (not a new slice!)
WRITE(offset=64KB, 32KB)  → Extend A: [0, 96KB]
...
WRITE(offset=10MB, 32KB)  → Extend A: [0, 10MB+32KB]

Result: 1 slice instead of 320+ slices for a 10MB sequential write
```

## Read Path

### NFS READ → Cache/Block Store

```
NFS READ(handle, offset, size)
    │
    ▼
PayloadService.ReadAt()
    │
    ├── Calculate chunk index
    │
    ▼
Cache.ReadSlice(fileHandle, chunkIdx, offset, length)
    │
    ├── Find all overlapping slices
    │
    ├── Merge using newest-wins algorithm:
    │   │
    │   ├── Fast path: Single slice covers entire range → O(1)
    │   │
    │   └── Slow path: Multiple overlapping slices
    │       ├── Build coverage bitmap
    │       ├── For each byte position, use newest slice
    │       └── Return merged data
    │
    └── Return (data, found, error)
    │
    ▼
If cache miss:
    │
    ├── TransferManager.EnsureAvailable()
    │   │
    │   ├── Calculate which blocks are needed
    │   ├── Download missing blocks from block store
    │   ├── Enqueue speculative prefetch (next N blocks)
    │   └── Wait for downloads to complete
    │
    └── Re-read from cache
```

### Newest-Wins Merge

When slices overlap, the most recent write wins:

```
Time 0: WRITE(offset=0, data="AAAA")   → Slice 1: [0,4] = "AAAA"
Time 1: WRITE(offset=2, data="BB")     → Slice 2: [2,4] = "BB"

READ(offset=0, size=4):
  - Byte 0: Slice 1 (only option) → 'A'
  - Byte 1: Slice 1 (only option) → 'A'
  - Byte 2: Slice 2 (newer) → 'B'
  - Byte 3: Slice 2 (newer) → 'B'

Result: "AABB"
```

## Flush and Transfer

### Non-Blocking Flush (COMMIT)

```
NFS COMMIT(handle)
    │
    ▼
PayloadService.Flush()
    │
    ├── TransferManager.Flush(fileHandle)
    │   │
    │   ├── Identify remaining dirty blocks
    │   ├── Enqueue for background upload
    │   └── Return immediately (non-blocking!)
    │
    └── Return success to client
```

**Key insight**: COMMIT returns immediately. Data durability is provided by:
1. WAL persistence (survives crash)
2. Background upload queue (eventual S3 persistence)

### Eager Upload (On Write Complete)

```go
// pkg/payload/transfer/manager.go
func (tm *TransferManager) OnWriteComplete(ctx context.Context, fileHandle []byte,
    payloadID string, chunkIdx int, writeOffset, writeLength int64) {

    // Calculate which 4MB blocks were affected
    startBlock := writeOffset / block.Size
    endBlock := (writeOffset + writeLength - 1) / block.Size

    for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
        // Check if block is already uploaded (deduplication)
        if tm.isBlockUploaded(fileHandle, chunkIdx, blockIdx) {
            continue
        }

        // Check if cache fully covers this block
        blockStart := blockIdx * block.Size
        blockEnd := blockStart + block.Size
        if tm.cache.IsRangeCovered(fileHandle, chunkIdx, blockStart, blockEnd) {
            // Start async upload
            go tm.uploadBlock(ctx, fileHandle, payloadID, chunkIdx, blockIdx)
        }
    }
}
```

### Transfer Queue Priority

```
Priority 1 (Highest): Downloads (cache misses)
Priority 2: Uploads (dirty data)
Priority 3 (Lowest): Prefetch (speculative reads)
```

Downloads pause uploads to ensure read latency is minimized.

## WAL Persistence

### Architecture

```
┌─────────────────┐
│     Cache       │
│  pkg/cache/     │
└────────┬────────┘
         │
         │ WriteSlice()
         ▼
┌─────────────────┐
│   Persister     │  ← Interface
│ pkg/cache/wal/  │
└────────┬────────┘
         │
    ┌────┴────┐
    │         │
    ▼         ▼
┌───────┐ ┌──────────┐
│ Mmap  │ │   Null   │
│Persist│ │ Persist  │
└───────┘ └──────────┘
 (Prod)    (Test/Dev)
```

### Persister Interface

```go
// pkg/cache/wal/persister.go
type Persister interface {
    AppendSlice(entry *SliceEntry) error  // Log a write
    AppendRemove(fileHandle []byte) error // Log a delete
    Sync() error                          // Fsync to disk
    Recover() ([]SliceEntry, error)       // Replay on startup
    Close() error
    IsEnabled() bool
}
```

### MmapPersister

Memory-mapped file for high-performance persistence:

```go
// Create WAL persister
persister, err := wal.NewMmapPersister("/var/lib/dittofs/cache.wal", wal.MmapConfig{
    InitialSize: 64 * 1024 * 1024,  // 64MB initial allocation
})

// Create cache with WAL
cache := cache.New(maxSize, cache.WithPersister(persister))
```

### Crash Recovery

On startup, the transfer manager recovers from WAL:

```go
// pkg/payload/transfer/recovery.go
func (tm *TransferManager) RecoverFromWAL(ctx context.Context) error {
    // Load all slices from WAL
    entries, err := tm.persister.Recover()

    // Restore to cache
    for _, entry := range entries {
        tm.cache.RestoreSlice(entry)
    }

    // Re-enqueue pending uploads
    for _, fileHandle := range tm.cache.GetDirtyFiles() {
        tm.EnqueueFlush(fileHandle)
    }

    return nil
}
```

## Configuration

### Cache Settings

```yaml
cache:
  stores:
    main-cache:
      type: memory
      memory:
        max_size: "1Gi"  # Maximum cache size

      # WAL persistence (optional but recommended)
      wal:
        enabled: true
        path: /var/lib/dittofs/cache.wal
        initial_size: "64Mi"
        sync_on_write: false  # OS page cache provides durability
```

### Transfer Manager Settings

```yaml
transfer:
  # Parallel upload/download limits
  max_parallel_uploads: 16
  max_parallel_downloads: 8

  # Prefetch configuration
  prefetch:
    enabled: true
    blocks_ahead: 4  # Prefetch next N blocks on sequential read
```

### Configuration Options

| Option | Default | Description |
|--------|---------|-------------|
| `cache.max_size` | `1Gi` | Maximum cache memory usage |
| `wal.enabled` | `true` | Enable WAL persistence |
| `wal.path` | `/tmp/dittofs-cache.wal` | WAL file location |
| `transfer.max_parallel_uploads` | `16` | Concurrent block uploads |
| `transfer.max_parallel_downloads` | `8` | Concurrent block downloads |
| `prefetch.blocks_ahead` | `4` | Speculative prefetch depth |

## Performance Characteristics

### Benchmarks (Apple M1, 1GB cache, S3 backend)

| Operation | Throughput | Notes |
|-----------|------------|-------|
| Sequential write | ~395 MB/s | To cache, non-blocking |
| Sequential read (cache hit) | ~295 MB/s | From memory |
| Sequential read (cache miss) | ~50-100 MB/s | S3 download |
| Small files write (1MB each) | ~118 MB/s | Per-file overhead |
| Small files read (1MB each) | ~209 MB/s | From cache |

### Why Non-Blocking Flush?

Traditional NFS COMMIT:
```
COMMIT → Upload to S3 → Wait 200ms+ → Return
Throughput: Limited by S3 latency (~5 MB/s)
```

DittoFS non-blocking COMMIT:
```
COMMIT → Enqueue upload → Return immediately (~1ms)
Durability: WAL on local disk (mmap + OS page cache)
Throughput: Limited only by memory bandwidth (~400 MB/s)
```

### Memory Usage

- **Slice overhead**: ~100 bytes per slice (ID, offset, state, timestamps)
- **Sequential optimization**: 10MB file = 1 slice vs 320 slices (32KB writes)
- **LRU eviction**: Only flushed slices can be evicted

### S3 API Efficiency

- **Block size**: 4MB = good balance of parallelism vs API calls
- **Deduplication**: Same block never uploaded twice per session
- **Batch uploads**: Multiple blocks upload concurrently (16 default)

## Summary

| Component | Responsibility |
|-----------|---------------|
| **Cache** | Buffer slices, LRU eviction, sequential merge |
| **WAL** | Crash recovery, slice persistence |
| **TransferManager** | Eager upload, download, prefetch, queue |
| **BlockStore** | S3/filesystem storage of 4MB blocks |

| Operation | Cache Action | Block Store Action |
|-----------|--------------|-------------------|
| WRITE | Create/extend slice, WAL append | None (immediate) |
| COMMIT | Enqueue flush, return immediately | Background upload |
| READ (hit) | Merge slices, return data | None |
| READ (miss) | Wait for download, return data | Download blocks |
| Eviction | Remove flushed slices | None |
| Recovery | Restore from WAL | Re-upload pending |

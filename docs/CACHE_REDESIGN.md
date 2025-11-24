# Cache Component Redesign - Design Document

**Date**: 2025-11-21
**Status**: Design Approved - Ready for Implementation

## Overview

This document describes the redesign of DittoFS's caching layer to provide a generic, efficient buffering mechanism for async write operations, with special support for storage backends that don't support random-access writes (e.g., S3).

## Goals

1. **Provide a generic Cache interface** that can be implemented in different flavors (memory, filesystem, etc.)
2. **Support async write mode** where writes go to cache first, then flush to storage on COMMIT
3. **Enable incremental multipart uploads** for S3 without accumulating entire files in memory
4. **Flush on every COMMIT** - only buffer when technical constraints force us to (S3's 5MB minimum)
5. **Maintain backward compatibility** - async=false preserves current direct-write behavior

## Architecture

### Core Principle

**The cache is just a buffer.** Flushing logic is handled by the NFS handler, not the cache itself. This keeps the cache interface simple and focused.

```
┌─────────────────────────────────────────┐
│         NFS WRITE Handler               │
│  (Writes to cache if async=true)        │
└────────────────┬────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────┐
│            Cache                        │
│  (Simple buffer: memory/disk)           │
│  - WriteAt, ReadAt                      │
│  - Size, List, Remove                   │
└─────────────────────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────┐
│         NFS COMMIT Handler              │
│  (Orchestrates flush on every COMMIT)   │
└────────────────┬────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────┐
│         Content Store                   │
│  - Regular: WriteAt (filesystem)        │
│  - Incremental: MultipartUpload (S3)    │
└─────────────────────────────────────────┘
```

## Interface Definitions

### 1. Cache Interface

**Location**: `pkg/store/content/cache/cache.go`

```go
package cache

import (
    "context"
    "time"
    "github.com/marmos91/dittofs/pkg/store/metadata"
)

// Cache provides a generic buffering layer for content
// It's just a buffer - flushing logic is handled by the caller (NFS handler)
type Cache interface {
    // Write operations
    Write(ctx context.Context, id metadata.ContentID, data []byte) error
    WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error

    // Read operations
    Read(ctx context.Context, id metadata.ContentID) ([]byte, error)
    ReadAt(ctx context.Context, id metadata.ContentID, buf []byte, offset int64) (int, error)

    // Metadata
    Size(id metadata.ContentID) int64
    LastWrite(id metadata.ContentID) time.Time
    Exists(id metadata.ContentID) bool
    List() []metadata.ContentID

    // Management
    Remove(id metadata.ContentID) error
    RemoveAll() error

    // Cache statistics
    TotalSize() int64  // Total bytes cached across all files
    MaxSize() int64    // Maximum cache size

    // Lifecycle
    Close() error
}
```

**Key Design Decisions**:
- No flush methods - cache is just a buffer
- No FlushTarget interface - keeps it simple
- Support for both full-file Write and random-access WriteAt
- Global size tracking for eviction logic

### 2. IncrementalWriteStore Interface

**Location**: `pkg/store/content/store.go` (extends existing interfaces)

```go
// IncrementalWriteStore extends ContentStore with support for incremental writes
// This is required for stores that don't support random-access writes (like S3)
// when using async write mode with cache.
//
// Stores that support native WriteAt (filesystem, memory) don't need to implement this.
type IncrementalWriteStore interface {
    ContentStore

    // Begin an incremental write session for a file
    // Returns a session handle to track this upload
    BeginIncrementalWrite(ctx context.Context, id metadata.ContentID) (IncrementalWriteSession, error)

    // Write a chunk of data in the session
    // chunkData must be >= MinChunkSize() except for the final chunk
    // offset is the file offset for this chunk
    WriteChunk(ctx context.Context, session IncrementalWriteSession, chunkData []byte, offset int64) error

    // Complete the incremental write session
    // All chunks have been written, finalize the upload
    CompleteIncrementalWrite(ctx context.Context, session IncrementalWriteSession) error

    // Abort the incremental write session
    // Called on error or timeout, cleans up partial upload
    AbortIncrementalWrite(ctx context.Context, session IncrementalWriteSession) error

    // Get the minimum chunk size required (e.g., 5MB for S3 multipart)
    // Chunks smaller than this (except the last) will be rejected
    MinChunkSize() int64
}

// IncrementalWriteSession tracks an active incremental write
type IncrementalWriteSession struct {
    ID            string                // Unique session ID (e.g., S3 uploadID)
    ContentID     metadata.ContentID    // File being written
    BytesWritten  int64                 // Total bytes written so far
    LastWrite     time.Time             // Last write activity
}
```

**Key Design Decisions**:
- Optional interface - stores opt-in by implementing it
- If a store doesn't implement this, async mode falls back to buffering entire file
- Chunk-based API matches S3 multipart semantics
- Session tracking for state management

### 3. Upload Session State (NFS Handler)

**Location**: `internal/protocol/nfs/v3/handlers/handler.go` (extend existing Handler struct)

```go
type Handler struct {
    Registry *registry.Registry

    // Upload session tracking (per content ID)
    uploadSessions map[metadata.ContentID]*UploadSession
    sessionsMu     sync.RWMutex

    // Background worker for timeouts
    stopWorker chan struct{}
}

type UploadSession struct {
    Session         content.IncrementalWriteSession
    BufferedData    []byte    // Data buffered waiting for minChunkSize
    BufferOffset    int64     // File offset of buffered data
    LastActivity    time.Time
}
```

**Key Design Decisions**:
- NFS handler owns upload session state (not cache, not content store)
- Handler orchestrates all flush logic
- Session state is ephemeral (not persisted) - noted as future enhancement

## NFS Handler Integration

### WRITE Handler

```go
func (h *Handler) HandleWrite(ctx *NFSContext, req *WriteRequest) *WriteResponse {
    // ... validation, permission checks ...

    if !share.Async {
        // Sync mode: write directly to content store (current behavior)
        err := contentStore.WriteAt(ctx, contentID, data, offset)

        return &WriteResponse{
            Status: NFS3OK,
            Count: len(data),
            Committed: req.Stable,  // Return requested stability
            // ... WCC data ...
        }
    } else {
        // Async mode: write to cache
        cache := h.Registry.GetCacheForShare(shareName)
        err := cache.WriteAt(ctx, contentID, data, offset)

        return &WriteResponse{
            Status: NFS3OK,
            Count: len(data),
            Committed: UnstableWrite,  // Tell client data is in cache
            // ... WCC data ...
        }
    }
}
```

**Behavior**:
- `async=false`: Direct write to store, return requested stability level (FILE_SYNC)
- `async=true`: Write to cache, return UNSTABLE (defer flush to COMMIT)

### COMMIT Handler - Core Logic

**Strategy**: Flush on EVERY COMMIT. Only buffer when technical constraints force us to (S3's 5MB minimum).

```go
func (h *Handler) HandleCommit(ctx *NFSContext, req *CommitRequest) *CommitResponse {
    // ... validation ...

    if !share.Async {
        // Sync mode: no-op (already written directly to store)
        return &CommitResponse{Status: NFS3OK}
    }

    // Async mode: ALWAYS attempt to flush on every COMMIT
    cache := h.Registry.GetCacheForShare(shareName)
    contentStore := h.Registry.GetContentStoreForShare(shareName)

    // Determine if this is the final commit for the file
    isFinalCommit := isFullFileCommit(file, req)

    // Check if store supports incremental writes
    if incStore, ok := contentStore.(content.IncrementalWriteStore); ok {
        // Incremental store (e.g., S3): try to upload what we can
        err := h.flushIncremental(ctx, cache, incStore, file.ContentID, isFinalCommit)
        if err != nil {
            return &CommitResponse{Status: NFS3ErrIO}
        }
    } else {
        // Non-incremental store (e.g., filesystem, memory)
        // These stores support random writes via WriteAt, so we only need
        // to flush on the final commit
        if isFinalCommit {
            data, err := cache.Read(ctx, file.ContentID)
            if err == nil {
                err = contentStore.WriteContent(ctx, file.ContentID, data)
            }
            if err != nil {
                return &CommitResponse{Status: NFS3ErrIO}
            }
            cache.Remove(file.ContentID)
        }
        // Else: not final commit, keep buffering (will WriteAt at end)
    }

    return &CommitResponse{Status: NFS3OK}
}
```

### Incremental Flush Logic

```go
func (h *Handler) flushIncremental(ctx context.Context, cache cache.Cache,
    store content.IncrementalWriteStore, id metadata.ContentID, isFinalCommit bool) error {

    minChunkSize := store.MinChunkSize()  // 5MB for S3

    // Get or create upload session
    h.sessionsMu.Lock()
    session := h.uploadSessions[id]
    if session == nil {
        // Start new session on first COMMIT
        incSession, err := store.BeginIncrementalWrite(ctx, id)
        if err != nil {
            h.sessionsMu.Unlock()
            return err
        }

        session = &UploadSession{
            Session: incSession,
            BufferedData: []byte{},
            BufferOffset: 0,
            LastActivity: time.Now(),
        }
        h.uploadSessions[id] = session
    }
    h.sessionsMu.Unlock()

    // Calculate how much NEW data is in cache since last upload
    cacheSize := cache.Size(id)
    alreadyUploaded := session.Session.BytesWritten
    newDataSize := cacheSize - alreadyUploaded

    if newDataSize > 0 {
        // Read the new data from cache
        newData := make([]byte, newDataSize)
        _, err := cache.ReadAt(ctx, id, newData, alreadyUploaded)
        if err != nil {
            return err
        }

        // Add to buffer
        session.BufferedData = append(session.BufferedData, newData...)
    }

    // Try to upload chunks
    // We can upload if: we have >= minChunkSize OR this is the final commit
    for {
        canUpload := len(session.BufferedData) >= int(minChunkSize) ||
                     (isFinalCommit && len(session.BufferedData) > 0)

        if !canUpload {
            // Can't upload yet - need more data or final commit
            // This is the ONLY reason we buffer: S3's 5MB constraint
            break
        }

        // Determine chunk size to upload
        var chunkSize int64
        if len(session.BufferedData) >= int(minChunkSize) {
            // Upload exactly minChunkSize (5MB)
            chunkSize = minChunkSize
        } else {
            // Final commit with remaining data (< 5MB is OK for last part)
            chunkSize = int64(len(session.BufferedData))
        }

        chunk := session.BufferedData[:chunkSize]

        // Upload the chunk
        err := store.WriteChunk(ctx, session.Session, chunk, session.BufferOffset)
        if err != nil {
            store.AbortIncrementalWrite(ctx, session.Session)
            h.sessionsMu.Lock()
            delete(h.uploadSessions, id)
            h.sessionsMu.Unlock()
            return fmt.Errorf("failed to upload chunk: %w", err)
        }

        // Update tracking
        session.BufferedData = session.BufferedData[chunkSize:]
        session.BufferOffset += chunkSize
        session.Session.BytesWritten += chunkSize
        session.LastActivity = time.Now()

        // If we uploaded everything and this is final commit, complete the upload
        if len(session.BufferedData) == 0 && isFinalCommit {
            err := store.CompleteIncrementalWrite(ctx, session.Session)
            if err != nil {
                return fmt.Errorf("failed to complete upload: %w", err)
            }

            // Clean up
            h.sessionsMu.Lock()
            delete(h.uploadSessions, id)
            h.sessionsMu.Unlock()
            cache.Remove(id)
            break
        }
    }

    return nil
}

func isFullFileCommit(file *metadata.FileAttr, req *CommitRequest) bool {
    // Commit entire file
    if req.Offset == 0 && req.Count == 0 {
        return true
    }

    // Commit from offset to EOF
    if req.Count == 0 {
        return true
    }

    // Commit range covers entire file
    if req.Offset + uint64(req.Count) >= file.Size {
        return true
    }

    return false
}
```

## Flow Examples

### Example 1: S3 Store (IncrementalWriteStore)

```
WRITE(0-4MB) → cache.WriteAt() → cache = [4MB]
↓
COMMIT → flushIncremental()
  - New data: 4MB
  - Buffer: 4MB (< 5MB minimum)
  - Action: BUFFER (can't upload yet)
  - Upload: none
  - Return: NFS3OK

WRITE(4MB-8MB) → cache.WriteAt() → cache = [8MB]
↓
COMMIT → flushIncremental()
  - New data: 4MB
  - Buffer: 4MB + 4MB = 8MB
  - Action: UPLOAD 5MB (part 1)
  - Buffer remaining: 3MB
  - Upload: Part 1 (5MB) ✓
  - Return: NFS3OK

WRITE(8MB-12MB) → cache.WriteAt() → cache = [12MB]
↓
COMMIT → flushIncremental()
  - New data: 4MB
  - Buffer: 3MB + 4MB = 7MB
  - Action: UPLOAD 5MB (part 2)
  - Buffer remaining: 2MB
  - Upload: Part 2 (5MB) ✓
  - Return: NFS3OK

COMMIT (final, offset=0, count=0)
↓
COMMIT → flushIncremental(isFinalCommit=true)
  - New data: 0MB
  - Buffer: 2MB
  - Action: UPLOAD remaining 2MB (final part allowed < 5MB)
  - Upload: Part 3 (2MB) ✓
  - CompleteMultipartUpload()
  - cache.Remove()
  - Return: NFS3OK
```

**Result**: Uploaded on every COMMIT where we had ≥5MB accumulated. Final part uploaded even though < 5MB.

### Example 2: Filesystem Store (Regular WriteAt)

```
WRITE(0-100MB) → cache.WriteAt() → cache = [100MB]
↓
Multiple COMMITs (not final)
  - Store doesn't implement IncrementalWriteStore
  - Action: BUFFER (wait for final)
  - Upload: none

COMMIT (final, offset=0, count=0)
↓
  - cache.Read() → [100MB]
  - contentStore.WriteContent() → write entire file
  - cache.Remove()
  - Return: NFS3OK
```

**Result**: Buffered entire file in cache, wrote once at end (efficient for random-access stores).

## Cache Eviction

### Strategy

1. **LRU eviction** - evict oldest LastWrite time
2. **Evict flushed files first** - files that have been uploaded can be safely removed
3. **Force flush if needed** - if no flushed files available, force flush oldest file

### Implementation

```go
// In MemoryCache implementation
func (c *MemoryCache) evictIfNeeded(needed int64) error {
    for c.TotalSize() + needed > c.MaxSize() {
        // Find a file to evict (LRU: oldest LastWrite time)
        victim := c.findOldestFlushedFile()

        if victim == nil {
            // No flushed files available, must force flush something
            victim = c.findOldestFile()
            if victim == nil {
                return errors.New("cache full and no files to evict")
            }

            // Return error indicating flush is needed
            return ErrCacheFull{VictimID: victim.ID}
        }

        c.Remove(victim.ID)
    }

    return nil
}
```

The NFS handler catches `ErrCacheFull`, flushes the victim file, and retries the write.

## Background Timeout Worker

### Purpose

Complete uploads that haven't had activity for a configurable timeout (default: 30 seconds).

This handles edge cases:
- Client crashes after partial upload
- Client forgets to send final COMMIT
- Network interruption

### Implementation

```go
// In NFS Handler
func (h *Handler) runUploadCompletionWorker(ctx context.Context) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            h.completeIdleSessions(ctx, 30*time.Second)
        }
    }
}

func (h *Handler) completeIdleSessions(ctx context.Context, timeout time.Duration) {
    h.sessionsMu.Lock()
    defer h.sessionsMu.Unlock()

    now := time.Now()
    for id, session := range h.uploadSessions {
        if now.Sub(session.LastActivity) > timeout {
            logger.Warn("Completing idle upload session for %s after %v inactivity",
                id, timeout)

            // Upload any remaining buffered data as final part
            if len(session.BufferedData) > 0 {
                // This might be < 5MB, but it's OK for the last part
                store.WriteChunk(ctx, session.Session, session.BufferedData,
                    session.BufferOffset)
            }

            store.CompleteIncrementalWrite(ctx, session.Session)
            cache.Remove(id)
            delete(h.uploadSessions, id)
        }
    }
}
```

## Metrics and Observability

### Overview

The cache implements comprehensive Prometheus metrics through the generic metrics interface already used in DittoFS. Metrics are optional and have zero overhead when disabled.

### Metrics Interface

**Location**: `pkg/store/content/cache/metrics.go`

```go
package cache

import (
    "time"
    "github.com/marmos91/dittofs/pkg/store/metadata"
)

// CacheMetrics provides observability into cache operations
// Implementations can be Prometheus-based or no-op
type CacheMetrics interface {
    // Cache size tracking
    ObserveCacheSize(sizeBytes int64)
    ObserveFileCount(count int)

    // Operation tracking
    ObserveWrite(sizeBytes int64, duration time.Duration)
    ObserveRead(sizeBytes int64, duration time.Duration)
    ObserveRemove(id metadata.ContentID)

    // Eviction and pressure tracking
    RecordEviction(id metadata.ContentID, reason EvictionReason)
    RecordEvictionFailure(reason string)
    ObserveMemoryPressure(utilizationPercent float64)

    // Throughput tracking
    RecordBytesWritten(bytes int64)
    RecordBytesRead(bytes int64)
}

// EvictionReason describes why a file was evicted
type EvictionReason string

const (
    EvictionReasonLRU         EvictionReason = "lru"          // Least recently used
    EvictionReasonFlushed     EvictionReason = "flushed"      // Already uploaded
    EvictionReasonForceFlush  EvictionReason = "force_flush"  // Forced due to pressure
)
```

### Prometheus Metrics Implementation

**Location**: `pkg/store/content/cache/prometheus_metrics.go`

```go
package cache

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

type PrometheusCacheMetrics struct {
    // Cache size gauges
    cacheSizeBytes *prometheus.GaugeVec
    cacheFileCount *prometheus.GaugeVec

    // Operation counters and histograms
    writeOps       *prometheus.CounterVec
    writeDuration  *prometheus.HistogramVec
    writeBytes     *prometheus.CounterVec

    readOps        *prometheus.CounterVec
    readDuration   *prometheus.HistogramVec
    readBytes      *prometheus.CounterVec

    removeOps      *prometheus.CounterVec

    // Eviction tracking
    evictions      *prometheus.CounterVec
    evictionFailures *prometheus.CounterVec

    // Memory pressure gauge
    memoryPressure *prometheus.GaugeVec

    // Throughput (bytes/sec calculated from counters)
    throughputBytesWritten *prometheus.CounterVec
    throughputBytesRead    *prometheus.CounterVec
}

func NewPrometheusCacheMetrics(namespace string) *PrometheusCacheMetrics {
    return &PrometheusCacheMetrics{
        cacheSizeBytes: promauto.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "size_bytes",
                Help:      "Total size of cached data in bytes",
            },
            []string{"cache_id"},
        ),

        cacheFileCount: promauto.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "file_count",
                Help:      "Number of files currently cached",
            },
            []string{"cache_id"},
        ),

        writeOps: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "write_operations_total",
                Help:      "Total number of write operations",
            },
            []string{"cache_id", "status"},
        ),

        writeDuration: promauto.NewHistogramVec(
            prometheus.HistogramOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "write_duration_seconds",
                Help:      "Write operation duration",
                Buckets:   prometheus.ExponentialBuckets(0.00001, 2, 20), // 10µs to ~10s
            },
            []string{"cache_id"},
        ),

        writeBytes: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "write_bytes_total",
                Help:      "Total bytes written to cache",
            },
            []string{"cache_id"},
        ),

        readOps: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "read_operations_total",
                Help:      "Total number of read operations",
            },
            []string{"cache_id", "status"},
        ),

        readDuration: promauto.NewHistogramVec(
            prometheus.HistogramOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "read_duration_seconds",
                Help:      "Read operation duration",
                Buckets:   prometheus.ExponentialBuckets(0.00001, 2, 20),
            },
            []string{"cache_id"},
        ),

        readBytes: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "read_bytes_total",
                Help:      "Total bytes read from cache",
            },
            []string{"cache_id"},
        ),

        removeOps: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "remove_operations_total",
                Help:      "Total number of remove operations",
            },
            []string{"cache_id"},
        ),

        evictions: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "evictions_total",
                Help:      "Total number of cache evictions",
            },
            []string{"cache_id", "reason"},
        ),

        evictionFailures: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "eviction_failures_total",
                Help:      "Total number of failed evictions",
            },
            []string{"cache_id", "reason"},
        ),

        memoryPressure: promauto.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "memory_pressure_percent",
                Help:      "Cache memory utilization as percentage (0-100)",
            },
            []string{"cache_id"},
        ),

        throughputBytesWritten: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "throughput_bytes_written_total",
                Help:      "Total bytes written (for throughput calculation)",
            },
            []string{"cache_id"},
        ),

        throughputBytesRead: promauto.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: "cache",
                Name:      "throughput_bytes_read_total",
                Help:      "Total bytes read (for throughput calculation)",
            },
            []string{"cache_id"},
        ),
    }
}
```

### Integration in MemoryCache

```go
// In MemoryCache implementation
type MemoryCache struct {
    buffers  map[string]*buffer
    mu       sync.RWMutex
    maxSize  int64
    metrics  CacheMetrics  // Optional metrics
}

func (c *MemoryCache) WriteAt(ctx context.Context, id metadata.ContentID,
    data []byte, offset int64) error {
    start := time.Now()

    // ... actual write logic ...

    // Record metrics
    if c.metrics != nil {
        c.metrics.ObserveWrite(int64(len(data)), time.Since(start))
        c.metrics.RecordBytesWritten(int64(len(data)))
        c.metrics.ObserveCacheSize(c.TotalSize())
        c.metrics.ObserveFileCount(len(c.buffers))
        c.metrics.ObserveMemoryPressure(float64(c.TotalSize()) / float64(c.maxSize) * 100)
    }

    return nil
}

func (c *MemoryCache) evictIfNeeded(needed int64) error {
    // ... eviction logic ...

    if victim != nil {
        if c.metrics != nil {
            reason := EvictionReasonFlushed
            if !victim.flushed {
                reason = EvictionReasonForceFlush
            }
            c.metrics.RecordEviction(victim.ID, reason)
        }
    } else {
        if c.metrics != nil {
            c.metrics.RecordEvictionFailure("no victim found")
        }
    }
}
```

### Metrics Dashboard

Example Prometheus queries:

```promql
# Cache utilization
dittofs_cache_size_bytes / dittofs_cache_max_size_bytes * 100

# Write throughput (bytes/sec)
rate(dittofs_cache_throughput_bytes_written_total[1m])

# Read throughput (bytes/sec)
rate(dittofs_cache_throughput_bytes_read_total[1m])

# Eviction rate
rate(dittofs_cache_evictions_total[5m])

# Average write duration
rate(dittofs_cache_write_duration_seconds_sum[1m]) / rate(dittofs_cache_write_duration_seconds_count[1m])

# Memory pressure over time
dittofs_cache_memory_pressure_percent

# Files in cache
dittofs_cache_file_count
```

### Key Metrics Summary

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `cache_size_bytes` | Gauge | Total bytes cached | cache_id |
| `cache_file_count` | Gauge | Number of files cached | cache_id |
| `write_operations_total` | Counter | Total write operations | cache_id, status |
| `write_duration_seconds` | Histogram | Write operation duration | cache_id |
| `write_bytes_total` | Counter | Total bytes written | cache_id |
| `read_operations_total` | Counter | Total read operations | cache_id, status |
| `read_duration_seconds` | Histogram | Read operation duration | cache_id |
| `read_bytes_total` | Counter | Total bytes read | cache_id |
| `remove_operations_total` | Counter | Total remove operations | cache_id |
| `evictions_total` | Counter | Total evictions | cache_id, reason |
| `eviction_failures_total` | Counter | Failed evictions | cache_id, reason |
| `memory_pressure_percent` | Gauge | Cache utilization (0-100) | cache_id |
| `throughput_bytes_written_total` | Counter | Bytes for throughput calc | cache_id |
| `throughput_bytes_read_total` | Counter | Bytes for throughput calc | cache_id |

### No-op Metrics Implementation

For testing or when metrics are disabled:

```go
// NoOpCacheMetrics implements CacheMetrics with no-op methods
type NoOpCacheMetrics struct{}

func (n *NoOpCacheMetrics) ObserveCacheSize(sizeBytes int64) {}
func (n *NoOpCacheMetrics) ObserveFileCount(count int) {}
func (n *NoOpCacheMetrics) ObserveWrite(sizeBytes int64, duration time.Duration) {}
func (n *NoOpCacheMetrics) ObserveRead(sizeBytes int64, duration time.Duration) {}
func (n *NoOpCacheMetrics) ObserveRemove(id metadata.ContentID) {}
func (n *NoOpCacheMetrics) RecordEviction(id metadata.ContentID, reason EvictionReason) {}
func (n *NoOpCacheMetrics) RecordEvictionFailure(reason string) {}
func (n *NoOpCacheMetrics) ObserveMemoryPressure(utilizationPercent float64) {}
func (n *NoOpCacheMetrics) RecordBytesWritten(bytes int64) {}
func (n *NoOpCacheMetrics) RecordBytesRead(bytes int64) {}
```

## Configuration

### Share Configuration

```yaml
# config.yaml or share configuration
shares:
  - name: /export
    metadata_store: fast-meta
    content_store: s3-content
    async: true                    # Enable async writes with cache (default: false)
    cache:
      max_size: 1073741824        # 1GB total cache limit (required if async=true)
      eviction: lru               # LRU eviction strategy (default)
      upload_timeout: 30          # Seconds of inactivity before completing upload (default: 30)
```

### Configuration Struct

```go
// In pkg/registry/share.go
type Share struct {
    Name           string
    MetadataStore  string
    ContentStore   string
    Async          bool         // Enable async write mode
    CacheConfig    *CacheConfig // Required if Async=true
}

type CacheConfig struct {
    MaxSize        int64  // Total cache size limit in bytes
    Eviction       string // "lru" (only option for now)
    UploadTimeout  int    // Seconds of inactivity before completing upload
}
```

## Implementation Checklist

### Phase 1: Core Interfaces and Cache Implementation

- [ ] Create `pkg/store/content/cache/cache.go` with Cache interface
- [ ] Rename `MemoryWriteCache` to `MemoryCache`
- [ ] Update `MemoryCache` to implement new Cache interface
  - [ ] Add `Write()` method
  - [ ] Add `TotalSize()` and `MaxSize()` methods
  - [ ] Add `Exists()` method
  - [ ] Update existing methods to match new signatures
- [ ] Create cache test suite in `pkg/store/content/cache/testing/`
  - [ ] Follow the pattern from content store tests
  - [ ] Generic test suite with factory function
  - [ ] Test all cache operations
- [ ] Run tests for MemoryCache

### Phase 2: IncrementalWriteStore Interface

- [ ] Add `IncrementalWriteStore` interface to `pkg/store/content/store.go`
- [ ] Add `IncrementalWriteSession` struct
- [ ] Update S3 content store to implement `IncrementalWriteStore`
  - [ ] Implement `BeginIncrementalWrite()`
  - [ ] Implement `WriteChunk()`
  - [ ] Implement `CompleteIncrementalWrite()`
  - [ ] Implement `AbortIncrementalWrite()`
  - [ ] Implement `MinChunkSize()` (return 5MB)
- [ ] Add integration tests for S3 incremental writes

### Phase 3: Configuration

- [ ] Add `Async` flag to `Share` struct in `pkg/registry/share.go`
- [ ] Add `CacheConfig` struct
- [ ] Update config parsing in `pkg/config/`
- [ ] Add validation: async=true requires cache config
- [ ] Add cache initialization in registry

### Phase 4: NFS Handler Updates

- [ ] Add upload session state to Handler struct
  - [ ] `uploadSessions map[metadata.ContentID]*UploadSession`
  - [ ] `sessionsMu sync.RWMutex`
  - [ ] `stopWorker chan struct{}`
- [ ] Update WRITE handler
  - [ ] Check `share.Async` flag
  - [ ] Branch: direct write vs cache write
  - [ ] Return appropriate stability level
- [ ] Update COMMIT handler
  - [ ] Implement `flushIncremental()` function
  - [ ] Implement `isFullFileCommit()` helper
  - [ ] Handle both incremental and non-incremental stores
- [ ] Add background timeout worker
  - [ ] Start worker in Handler initialization
  - [ ] Implement `runUploadCompletionWorker()`
  - [ ] Implement `completeIdleSessions()`

### Phase 5: Cache Eviction

- [ ] Implement `evictIfNeeded()` in MemoryCache
- [ ] Track which files have been flushed (add flag to cache entry)
- [ ] Implement LRU eviction
- [ ] Define and handle `ErrCacheFull` error
- [ ] Update NFS handler to handle `ErrCacheFull`

### Phase 6: Metrics and Observability

- [ ] Create `pkg/store/content/cache/metrics.go` with CacheMetrics interface
- [ ] Define `EvictionReason` enum (lru, flushed, force_flush)
- [ ] Create `pkg/store/content/cache/prometheus_metrics.go`
  - [ ] Implement `PrometheusCacheMetrics` struct
  - [ ] Add cache size and file count gauges
  - [ ] Add operation counters (write, read, remove)
  - [ ] Add duration histograms (write, read)
  - [ ] Add eviction tracking (successes, failures)
  - [ ] Add memory pressure gauge
  - [ ] Add throughput counters
- [ ] Create `NoOpCacheMetrics` for testing
- [ ] Integrate metrics into MemoryCache
  - [ ] Add optional `metrics CacheMetrics` field
  - [ ] Call metrics in WriteAt(), ReadAt(), Write(), Read()
  - [ ] Call metrics in Remove(), eviction logic
  - [ ] Update metrics on every operation
- [ ] Add metrics initialization in cache factory
- [ ] Test metrics with Prometheus scraping

### Phase 7: Cleanup and Testing

- [ ] Remove old `WriteCache` references
  - [ ] Search for `WriteCache` in codebase
  - [ ] Remove old interface definition
  - [ ] Remove old usage in handlers (currently disabled)
- [ ] Update integration tests
  - [ ] Test async=false mode (should be unchanged)
  - [ ] Test async=true with memory store
  - [ ] Test async=true with S3 store
  - [ ] Test eviction behavior
  - [ ] Test timeout completion
- [ ] Update documentation
  - [ ] Update README with async mode usage
  - [ ] Update CLAUDE.md with new architecture
  - [ ] Add examples of configuration

## Future Enhancements

These are noted but not implemented in this phase:

1. **Persistent Upload Sessions** - Store upload session state in metadata store to survive server restarts
2. **Disk-backed Cache** - Implement filesystem-based cache (not just memory)
3. **Advanced Eviction** - Consider file priority, access patterns, size-aware eviction
4. **Compression** - Compress cached data to save memory
5. **Encryption** - Encrypt cached data for security
6. **Cache Warming** - Pre-populate cache on server startup from persistent storage
7. **Multi-tier Caching** - Combine memory and disk caches with automatic promotion/demotion

## Alternative Approach: Temp Parts with Background Compaction

**Status**: Design proposal - not yet implemented. Documented for future consideration.

### Problem Statement

The main design (buffer in cache + multipart) has a fundamental tension:
- **RFC 1813**: COMMIT returning NFS3OK means data MUST be on stable storage
- **S3 Multipart**: Parts must be ≥5MB except the last part
- **macOS Client**: Calls COMMIT every 4MB during large file writes

The buffer-in-cache approach is a "soft RFC violation" - COMMIT returns success but data is still in cache, not on S3. While the write verifier protects against data loss, the data isn't immediately durable.

### Proposed Solution

Write each COMMIT as a temporary S3 object immediately (any size), then compact in the background.

#### Phase 1: COMMIT writes temp parts immediately

```go
func (h *Handler) Commit(ctx *NFSHandlerContext, req *CommitRequest) (*CommitResponse, error) {
    // ... validation ...

    writeCache, err := h.Registry.GetWriteCacheForShare(shareName)
    if writeCache == nil {
        // Sync mode: already written directly to store
        return &CommitResponse{Status: types.NFS3OK, ...}, nil
    }

    // Read cached data
    data, err := writeCache.Read(ctx.Context, file.ContentID)
    if err != nil {
        return &CommitResponse{Status: types.NFS3ErrIO, ...}, nil
    }

    // Write as temp part to S3 (any size - no 5MB restriction!)
    s3Store, _ := h.Registry.GetContentStoreForShare(shareName)
    partNum := getNextPartNum(file.ContentID)
    tempKey := fmt.Sprintf(".temp/%s/part-%03d", file.ContentID, partNum)

    err = s3Store.PutObject(ctx.Context, tempKey, data)
    if err != nil {
        return &CommitResponse{Status: types.NFS3ErrIO, ...}, nil
    }

    // Mark in metadata that this file has temp parts pending compaction
    metadataStore.MarkPendingCompaction(file.ContentID, partNum)

    // Clean cache immediately after writing to S3
    writeCache.Remove(file.ContentID)

    // ✓ Data is now on stable storage (S3)!
    return &CommitResponse{
        Status: types.NFS3OK,
        AttrBefore: wccBefore,
        AttrAfter: wccAfter,
        WriteVerifier: serverBootTime,
    }, nil
}
```

**COMMIT returns immediately after PutObject** → RFC 1813 compliant! Data is truly on stable storage.

#### Phase 2: Background compaction worker

```go
func (h *Handler) runCompactionWorker(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            h.compactPendingFiles(ctx)
        }
    }
}

func (h *Handler) compactPendingFiles(ctx context.Context) {
    // Get files marked for compaction from metadata store
    files := metadataStore.ListPendingCompaction()

    for _, contentID := range files {
        // Check if file is still being actively written
        if h.isActivelyWriting(contentID) {
            continue  // Skip, wait for writes to finish
        }

        // Compact: merge temp parts into final object
        err := h.compactFile(ctx, contentID)
        if err != nil {
            logger.Warn("Compaction failed for %s: %v", contentID, err)
            continue
        }

        // Mark as compacted in metadata
        metadataStore.ClearPendingCompaction(contentID)
    }
}

func (h *Handler) compactFile(ctx context.Context, contentID string) error {
    s3Store := h.Registry.GetContentStoreForShare(shareName)

    // List all temp parts for this file
    tempPrefix := fmt.Sprintf(".temp/%s/", contentID)
    parts, err := s3Store.ListObjects(ctx, tempPrefix)
    if err != nil {
        return err
    }

    if len(parts) == 0 {
        return nil  // Already compacted
    }

    if len(parts) == 1 {
        // Only one part - just move it (rename)
        err := s3Store.CopyObject(ctx, parts[0], contentID)
        if err != nil {
            return err
        }
        s3Store.DeleteObject(ctx, parts[0])
        return nil
    }

    // Multiple parts - use multipart upload for efficiency
    // Note: This uses S3 server-side operations, no data transfer!
    uploadID, err := s3Store.CreateMultipartUpload(ctx, contentID)
    if err != nil {
        return err
    }

    // Upload each part using UploadPartCopy (server-side copy, no download!)
    for i, partKey := range parts {
        err := s3Store.UploadPartCopy(ctx, uploadID, i+1, partKey)
        if err != nil {
            s3Store.AbortMultipartUpload(ctx, uploadID)
            return err
        }
    }

    // Complete multipart upload
    err = s3Store.CompleteMultipartUpload(ctx, uploadID)
    if err != nil {
        return err
    }

    // Delete temp parts
    for _, partKey := range parts {
        s3Store.DeleteObject(ctx, partKey)
    }

    logger.Info("Compacted %d parts for %s into final object", len(parts), contentID)
    return nil
}
```

#### Phase 3: READ handles both temp and final objects

```go
func (s *S3ContentStore) ReadAt(ctx context.Context, id ContentID, buf []byte, offset int64) (int, error) {
    // Try reading final object first (most common case)
    n, err := s.client.GetObject(ctx, string(id), buf, offset)
    if err == nil {
        return n, nil  // ✓ Found final object
    }

    // Check if error is "not found" vs other errors
    if !isNotFoundError(err) {
        return 0, err  // Real error, not just missing
    }

    // Not found - check for temp parts
    tempPrefix := fmt.Sprintf(".temp/%s/", id)
    parts, err := s.client.ListObjects(ctx, tempPrefix)
    if err != nil {
        return 0, err
    }

    if len(parts) == 0 {
        return 0, ErrNotFound  // Truly doesn't exist
    }

    // Download and merge temp parts
    // TODO: Optimize to only download relevant parts for the requested range
    merged := []byte{}
    for _, partKey := range parts {
        partData, err := s.client.GetObject(ctx, partKey)
        if err != nil {
            return 0, err
        }
        merged = append(merged, partData...)
    }

    // Extract requested range
    if offset >= int64(len(merged)) {
        return 0, io.EOF
    }

    n = copy(buf, merged[offset:])
    return n, nil
}
```

### Benefits

✓ **RFC 1813 Compliant**: COMMIT writes to S3 immediately (true stable storage)
✓ **No 5MB blocking**: Can write any size temp parts with PutObject
✓ **Efficient eventual state**: Background worker merges into single object
✓ **Crash resilient**: Temp parts survive server crash
✓ **Self-healing**: Compaction can run after restart to clean up orphaned parts
✓ **Simpler than buffering**: No session state tracking during writes

### Trade-offs

**Costs:**
- More S3 API calls during write phase (each COMMIT = PutObject)
- Temp parts use S3 storage until compaction runs
- READ needs fallback logic to handle temp parts (slight performance penalty)
- Background worker adds complexity
- S3 storage costs for temp objects before compaction

**Compaction triggers:**
- File hasn't been written to in 5+ minutes (probably done)
- File has >10 temp parts (getting fragmented)
- Scheduled maintenance window
- Manual trigger for specific files

### Optimization: Server-side Copy

S3's `UploadPartCopy` API is key to efficiency:
```go
// No data transfer! S3 copies internally between objects
s3.UploadPartCopy(
    uploadID,
    partNumber,
    sourceKey: ".temp/abc123/part-001"
)
```

**Compaction is nearly free** - just API calls, no network transfer costs!

### Metadata Tracking

Add to metadata store FileAttr:
```go
type FileAttr struct {
    // ... existing fields ...

    TempPartCount   int       // Number of temp parts (0 = final object)
    LastWriteTime   time.Time // When last COMMIT happened
    CompactionState string    // "pending", "compacting", "final"
}
```

### Comparison with Main Design

| Aspect | Buffer in Cache (Main) | Temp Parts + Compaction (Alternative) |
|--------|----------------------|----------------------------------|
| **RFC Compliance** | Soft violation (returns OK while in cache) | Strict compliance (data on S3 before OK) |
| **Implementation Complexity** | Medium (session tracking, chunking) | Medium (background worker, dual read path) |
| **Write Performance** | Fast (cache writes) | Slower (S3 PutObject on each COMMIT) |
| **Read Performance** | Fast (no S3 until flush) | Fast after compaction, slower before |
| **Crash Safety** | Write verifier protects | Temp parts survive, auto-cleanup |
| **S3 Costs** | Lower (fewer API calls) | Higher (more PutObjects, temp storage) |
| **Client Perspective** | Appears committed (verifier protection) | Truly committed immediately |

### When to Use Which Approach

**Use Buffer in Cache (Main Design):**
- Write performance is critical
- Most files are small (<5MB)
- Disk-backed cache is available for durability
- S3 API costs are a concern

**Use Temp Parts + Compaction (Alternative):**
- Strict RFC compliance is required
- Write-heavy workloads with many partial COMMITs
- Want data on stable storage immediately
- Can tolerate S3 API costs

### Implementation Priority

This could be implemented as **Phase 8** or **v2.0 feature**:
```
Phase 1-7: Basic cache + incremental writes (buffer in cache)
Phase 8: Add temp parts + compaction (optional alternative mode)
```

**Recommendation**: Start with main design (simpler, proven), add compaction as optional enhancement based on real-world usage patterns.

## Questions and Decisions

### Q: Should FILE_SYNC mode (async=false) do explicit fsync?
**Decision**: No, keep it a no-op. Content stores handle durability as appropriate.

### Q: Should we have cache eviction?
**Decision**: Yes, LRU with flush-then-evict strategy. Evict flushed files first, force flush if needed.

### Q: Should upload sessions persist across restarts?
**Decision**: Not in this phase. Noted as future enhancement.

### Q: Should we track detailed metrics?
**Decision**: Yes, implement Prometheus metrics for cache operations, evictions, throughput, and memory pressure.

### Q: Should MemoryCache have a global size limit?
**Decision**: Yes, configurable per share via `cache.max_size`.

### Q: Can we control COMMIT size to align with S3 requirements?
**Decision**: No, the NFS client controls COMMIT frequency. Our solution buffers across multiple COMMITs until we have ≥5MB to upload.

## Important Implementation Notes

### Thread Safety

1. **MemoryCache**: Must be thread-safe for concurrent access
   - Use `sync.RWMutex` for buffer map protection
   - Each buffer should have its own mutex for fine-grained locking
   - Pattern: RLock for reads, Lock for writes and structure modifications

2. **Upload Sessions in Handler**: Protected by `sync.RWMutex`
   - Multiple COMMITs can happen concurrently
   - Session state updates must be serialized per ContentID

### Memory Management

1. **Buffer Growth Strategy** (from existing WriteCache):
   - Exponential growth up to 100MB: `newCap *= 2`
   - Linear growth beyond 100MB: `newCap += 10MB`
   - Prevents O(N²) behavior for large files
   - Prevents excessive memory waste for huge files

2. **Zero-filling Sparse Files**:
   - When `WriteAt(offset > currentSize)`, zero-fill the gap
   - Required for correct NFS semantics

3. **Cache Entry Structure**:
   ```go
   type cacheEntry struct {
       data       []byte
       lastWrite  time.Time
       flushed    bool       // Track if uploaded to store
       mu         sync.Mutex
   }
   ```

### Error Handling

1. **Context Cancellation**:
   - Check `ctx.Done()` before expensive operations
   - Abort multipart uploads on context cancellation
   - Clean up session state on errors

2. **Partial Upload Failures**:
   - Call `AbortIncrementalWrite()` on any chunk upload failure
   - Remove session from tracking map
   - Don't clear cache - let client retry

3. **Cache Full Errors**:
   - Return `ErrCacheFull{VictimID}` when eviction needed but no candidates
   - Handler must flush victim and retry the write
   - Implement backpressure to prevent infinite retry loops

### S3 Multipart Upload Details

1. **Part Numbers**:
   - Must be sequential: 1, 2, 3, ...
   - Maximum 10,000 parts per upload
   - Track in `UploadSession.NextPartNumber`

2. **Part Size**:
   - Minimum 5MB (5 * 1024 * 1024 bytes) except last part
   - Last part can be any size (even < 5MB)
   - Buffer in session until we have ≥5MB

3. **ETags**:
   - S3 returns ETag for each uploaded part
   - Must be provided in exact order when completing upload
   - Store in `UploadSession` alongside part numbers

4. **Upload Session Lifecycle**:
   ```
   BeginIncrementalWrite() → uploadID
   Loop:
     Buffer data until ≥5MB
     WriteChunk() → ETag
   Final:
     WriteChunk(remaining < 5MB) → ETag
     CompleteIncrementalWrite(uploadID, [ETags...])
   ```

### Configuration Validation

1. **async=true requires cache**:
   ```go
   if share.Async && share.CacheConfig == nil {
       return errors.New("async mode requires cache configuration")
   }
   ```

2. **cache.max_size must be reasonable**:
   - Minimum: 10MB (enough for at least 2 parts)
   - Recommended: ≥100MB for good performance
   - Consider system memory when setting

3. **upload_timeout**:
   - Default: 30 seconds
   - Minimum: 10 seconds (too short causes premature completion)
   - Maximum: 300 seconds (5 minutes - avoid indefinite sessions)

### Metrics Collection Pattern

1. **Zero-overhead when disabled**:
   ```go
   if c.metrics != nil {
       c.metrics.ObserveWrite(...)
   }
   ```

2. **Timing pattern**:
   ```go
   start := time.Now()
   // ... operation ...
   if c.metrics != nil {
       c.metrics.ObserveWrite(bytes, time.Since(start))
   }
   ```

3. **Update on every change**:
   - Update cache size gauge after every write/remove
   - Update file count gauge after every file added/removed
   - Update memory pressure after size changes

### Testing Considerations

1. **Use Generic Test Suite**:
   - Define factory function: `func() Cache`
   - Run all generic tests: `suite.Run(t)`
   - Add implementation-specific tests separately

2. **Mock IncrementalWriteStore**:
   - Implement mock for testing without S3
   - Verify correct chunk ordering
   - Test error scenarios (abort on failure)

3. **Test Scenarios**:
   - Small files (< 5MB) - single upload
   - Large files (> 100MB) - multiple parts
   - Multiple concurrent writes to different files
   - Cache full conditions
   - Session timeout scenarios
   - Client crash simulation (orphaned sessions)

### Performance Considerations

1. **Avoid Holding Locks During I/O**:
   ```go
   // GOOD: Release lock before S3 call
   c.mu.RLock()
   data := make([]byte, len(buffer.data))
   copy(data, buffer.data)
   c.mu.RUnlock()

   store.WriteChunk(ctx, session, data, offset)  // No lock held
   ```

2. **Parallel Chunk Uploads** (future optimization):
   - Currently uploads chunks sequentially
   - Could upload multiple chunks in parallel
   - Must assemble with correct part numbers

3. **Memory Pooling**:
   - Consider using `sync.Pool` for chunk buffers
   - Reuse byte slices for chunk uploads
   - Current buffer pool in NFS handlers can be extended

### Migration from Old WriteCache

1. **Search for usage**:
   ```bash
   git grep -n "WriteCache" | grep -v "^Binary"
   ```

2. **Files to update**:
   - `pkg/store/content/cache/write_cache.go` - rename to memory_cache.go
   - `internal/protocol/nfs/v3/handlers/write.go` - update references
   - `internal/protocol/nfs/v3/handlers/read.go` - update references
   - `pkg/adapter/nfs/nfs_adapter.go` - update cache initialization

3. **Interface compatibility**:
   - Old: `WriteAt(id, data, offset) error`
   - New: `WriteAt(ctx, id, data, offset) error` (added context)
   - Old: `ReadAll(id) ([]byte, error)`
   - New: `Read(ctx, id) ([]byte, error)` (renamed for consistency)

### Registry Integration

1. **Cache per Share**:
   ```go
   type Registry struct {
       shares map[string]*Share
       caches map[string]cache.Cache  // shareName -> cache
   }
   ```

2. **Cache Initialization**:
   ```go
   if share.Async {
       cache, err := createCache(share.CacheConfig)
       registry.caches[shareName] = cache
   }
   ```

3. **Cache Retrieval in Handlers**:
   ```go
   cache := h.Registry.GetCacheForShare(shareName)
   if cache == nil && share.Async {
       return errors.New("cache not initialized for async share")
   }
   ```

## References

- **Current WriteCache**: `pkg/store/content/cache/write_cache.go`
- **Current WRITE handler**: `internal/protocol/nfs/v3/handlers/write.go`
- **Current COMMIT handler**: `internal/protocol/nfs/v3/handlers/commit.go`
- **Current S3 store**: `pkg/store/content/s3/`
- **Content store tests**: `pkg/store/content/testing/`
- **NFS RFC 1813**: https://tools.ietf.org/html/rfc1813 (WRITE/COMMIT semantics)
- **S3 Multipart Upload**: https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html

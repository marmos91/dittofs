# pkg/cache

Unified read/write cache layer - content-store agnostic.

## State Machine

```
StateNone → StateBuffering (write) → StateUploading (flush) → StateCached (clean)
StateNone → StatePrefetching (read) → StateCached
```

## Key Design Decisions

### One Cache Per Share
- Serves both reads AND writes
- Content-store agnostic (doesn't know about S3 multipart, fsync, etc.)
- LRU eviction with dirty entry protection

### Three Data States
1. **Buffering** (dirty): Accumulating writes, cannot evict
2. **Uploading** (dirty): Flush in progress, cannot evict
3. **Cached** (clean): Can evict, needs mtime/size validation on read hit

### Inactivity-Based Finalization
- Background sweeper detects idle files
- Files finalized after `finalize_timeout` of no writes
- Enables async write completion detection

## Write Gathering

Linux kernel's "wdelay" optimization implemented in protocol handlers (not here):
- COMMIT waits briefly if recent writes detected
- Default: 10ms delay with 10ms active threshold
- Reduces S3 API calls by batching

## Flusher (`flusher/`)

Background goroutine that:
- Sweeps for idle dirty entries
- Flushes to content store
- Handles S3 multipart completion via `IncrementalWriteStore`

## Common Mistakes

1. **Assuming cache required** - it's optional, direct writes work fine
2. **Evicting dirty entries** - only clean entries can be evicted
3. **Cache knowing about multipart** - that's content store's concern
4. **Blocking on flush** - flusher runs async, use COMMIT semantics

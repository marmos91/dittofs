# Cache Refactor Implementation Plan

This document tracks the implementation of the unified cache design described in [docs/CACHE.md](docs/CACHE.md).

## Overview

Refactor from separate read/write caches to a unified, content-store-agnostic cache per share.

**Key changes:**
- One cache per share (not separate read/write)
- Three states: Buffering, Uploading, Cached
- Cache is content-store agnostic (no S3 multipart knowledge)
- Read cache coherency via mtime/size validation
- Inactivity-based finalization

## Implementation Phases

### Phase 1: Cache Interface and Core Types

**Goal**: Define new cache interface and types without breaking existing code.

#### Step 1.1: Add CacheState type
- [ ] Add `CacheState` enum to `pkg/cache/cache.go`
- [ ] Add state constants: `StateBuffering`, `StateUploading`, `StateCached`
- [ ] Add tests for state transitions

#### Step 1.2: Extend Cache interface
- [ ] Add `GetState(id) CacheState`
- [ ] Add `SetState(id, state)`
- [ ] Add `GetFlushedOffset(id) int64`
- [ ] Add `SetFlushedOffset(id, offset int64)`
- [ ] Add `GetCachedMetadata(id) (mtime, size, ok)`
- [ ] Add `SetCachedMetadata(id, mtime, size)`
- [ ] Add `IsValid(id, currentMtime, currentSize) bool`
- [ ] Add `LastAccess(id) time.Time`
- [ ] Add `TotalSize() int64`
- [ ] Add `MaxSize() int64`
- [ ] Keep existing methods for backward compatibility

#### Step 1.3: Update CacheEntry struct
- [ ] Add `State CacheState`
- [ ] Add `FlushedOffset int64`
- [ ] Add `LastAccessTime time.Time`
- [ ] Add `CachedAt time.Time`
- [ ] Add `CachedMtime time.Time`
- [ ] Add `CachedSize uint64`
- [ ] Update memory cache implementation

### Phase 2: Memory Cache Implementation

**Goal**: Implement new interface in memory cache.

#### Step 2.1: State management
- [ ] Implement `GetState` / `SetState`
- [ ] Implement state transition logic
- [ ] Add tests for each state

#### Step 2.2: Flush offset tracking
- [ ] Implement `GetFlushedOffset` / `SetFlushedOffset`
- [ ] Track flushed vs buffered data
- [ ] Add tests

#### Step 2.3: Validity tracking
- [ ] Implement `GetCachedMetadata` / `SetCachedMetadata`
- [ ] Implement `IsValid` with mtime/size comparison
- [ ] Add optional TTL support
- [ ] Add tests for validation scenarios

#### Step 2.4: Eviction updates
- [ ] Update `canEvict()` to check state (only StateCached can be evicted)
- [ ] Update LRU to use `LastAccessTime`
- [ ] Add tests for dirty entry protection

### Phase 3: Registry Changes

**Goal**: Simplify registry to use single cache per share.

#### Step 3.1: Update Share configuration
- [ ] Change from `write_cache`/`read_cache` to single `cache` config
- [ ] Add new cache config fields: `finalize_timeout`, `sweep_interval`, `read_ttl`, `validate_on_read`
- [ ] Update config parsing in `pkg/config/`
- [ ] Add config validation

#### Step 3.2: Update Registry
- [ ] Replace `GetWriteCacheForShare` / `GetReadCacheForShare` with `GetCacheForShare`
- [ ] Update share registration
- [ ] Keep old methods temporarily (deprecated) for migration
- [ ] Add tests

### Phase 4: Handler Refactoring - WRITE

**Goal**: Simplify WRITE handler to use unified cache.

#### Step 4.1: Update write flow
- [ ] Get unified cache via `GetCacheForShare`
- [ ] On write: if state == Cached, reset to Buffering
- [ ] Write to cache buffer
- [ ] Update `LastWriteTime` and `LastAccessTime`
- [ ] Remove content store write logic (cache-only)
- [ ] Update tests

#### Step 4.2: Remove sync mode logic
- [ ] Remove direct content store writes from WRITE handler
- [ ] All writes go to cache (COMMIT handles persistence)
- [ ] Update tests

### Phase 5: Handler Refactoring - COMMIT

**Goal**: Simplify COMMIT handler with flush coordinator pattern.

#### Step 5.1: Create flush coordinator
- [ ] Create `internal/protocol/nfs/v3/handlers/flush.go`
- [ ] Implement `flushToContentStore(ctx, cache, contentStore, contentID)`
- [ ] Use type assertion for `IncrementalWriteStore`
- [ ] Update `FlushedOffset` after successful flush
- [ ] Set state to `StateUploading`

#### Step 5.2: Update COMMIT handler
- [ ] Use unified cache
- [ ] Call flush coordinator
- [ ] Remove old `flushCacheToContentStore` / `flushIncremental` / `flushNonIncremental`
- [ ] Remove `isFullFileCommit` logic (finalization handles this)
- [ ] Remove read cache population (unified cache keeps data)
- [ ] Update tests

### Phase 6: Handler Refactoring - READ

**Goal**: Simplify READ handler with cache validation.

#### Step 6.1: Update read flow
- [ ] Get unified cache via `GetCacheForShare`
- [ ] Check if content exists in cache
- [ ] If hit: validate using `IsValid(id, mtime, size)`
- [ ] If valid: serve from cache, update `LastAccessTime`
- [ ] If invalid: remove from cache, fall back to content store
- [ ] On miss: read from content store, populate cache as `StateCached`
- [ ] Store metadata with `SetCachedMetadata`

#### Step 6.2: Remove old cache logic
- [ ] Remove `tryReadFromCache` with separate write/read cache checks
- [ ] Remove `populateReadCacheFromRead`
- [ ] Simplify to single cache path
- [ ] Update tests

### Phase 7: Background Finalizer

**Goal**: Implement inactivity-based finalization.

#### Step 7.1: Create finalizer
- [ ] Create `pkg/cache/finalizer.go`
- [ ] Implement `Finalizer` struct with cache, registry references
- [ ] Implement `sweepForFinalization(ctx)`
- [ ] Implement `finalize(ctx, id)` - completes uploads, sets `StateCached`

#### Step 7.2: Integrate finalizer
- [ ] Start finalizer goroutine per cache
- [ ] Configure `sweepInterval` and `finalizeTimeout`
- [ ] Implement graceful shutdown (finalize all on stop)
- [ ] Add tests for finalization scenarios

### Phase 8: Configuration Updates

**Goal**: Update configuration files and documentation.

#### Step 8.1: Update config schema
- [ ] Update `pkg/config/stores.go` for new cache config
- [ ] Remove `write_cache` / `read_cache` from share config
- [ ] Add `cache` block with new options
- [ ] Update default config template

#### Step 8.2: Update documentation
- [ ] Update `docs/CONFIGURATION.md` with new cache options
- [ ] Add migration guide for existing configs
- [ ] Update example configs

### Phase 9: Cleanup

**Goal**: Remove deprecated code and finalize.

#### Step 9.1: Remove deprecated methods
- [ ] Remove `GetWriteCacheForShare` from registry
- [ ] Remove `GetReadCacheForShare` from registry
- [ ] Remove old cache population helpers
- [ ] Remove unused types

#### Step 9.2: Final testing
- [ ] Run full unit test suite
- [ ] Run integration tests
- [ ] Run E2E tests with all backend combinations
- [ ] Performance benchmarks

## Testing Strategy

### Unit Tests (per phase)
- Test each new method in isolation
- Test state transitions
- Test validation logic
- Test eviction with dirty protection

### Integration Tests
- Test cache + content store interaction
- Test with memory, filesystem, S3 backends
- Test finalization with IncrementalWriteStore

### E2E Tests
- Test complete write → commit → read flow
- Test concurrent writes from multiple clients
- Test server restart (finalization on shutdown)
- Test cache invalidation scenarios

## Migration Notes

### Breaking Changes
- Configuration: `write_cache` and `read_cache` replaced by `cache`
- Registry API: `GetWriteCacheForShare` / `GetReadCacheForShare` → `GetCacheForShare`

### Backward Compatibility
- Phase 3 keeps old registry methods (deprecated) temporarily
- Old config format should produce warning, not error

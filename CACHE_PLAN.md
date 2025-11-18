# Metadata Cache Redesign: Version-Based Snapshot Isolation

## Overview

Complete redesign of DittoFS metadata caching with correctness above all - zero tolerance for stale data. Using version-based snapshot isolation with incremental implementation for safety.

### Key Design Decisions

1. **Version-Based Snapshot Isolation**: Every cache entry tagged with a version number; version mismatch = automatic cache invalidation
2. **Per-Type Version Counters**: Separate version tracking for stats, file handles, and directories for granular invalidation
3. **Zero Tolerance for Stale Data**: Version checks guarantee no stale data can be served
4. **Incremental Implementation**: 9 phases, starting with simplest caches, building to most complex
5. **Defense in Depth**: Version checks (primary) + TTL expiration (secondary safety net)

### Cache Type Version Mapping

| Cache Type | Version Counter Used | Invalidation Trigger |
|------------|---------------------|---------------------|
| **Stats** | `versions.stats` (global) | Any create/delete |
| **GetFile** | `versions.handles[fileHandle]` (per-file) | SetAttr, Write, Remove on that file |
| **Lookup** | `versions.dirs[parentHandle]` (per-directory) | Directory modifications |
| **ReadDir** | `versions.dirs[dirHandle]` (per-directory) | Directory modifications |
| **ShareName** | `versions.handles[fileHandle]` (per-file) | File deletion |

---

## Phase 1: Remove All Existing Caches (Week 1)

### 1.1 Disable All Caches

- Set all cache enabled flags to false in store.go:313-368
- Document performance baseline without caching
- Verify system works correctly (already confirmed in testing)

### 1.2 Remove Cache Read Logic

- Remove `getReaddirCached()`, `getLookupCached()`, `getGetfileCached()`, `getShareNameCached()`
- Remove cache checks from `Lookup()`, `GetFile()`, `ReadDirectory()`, `GetShareNameForHandle()`
- Simplify code paths to always read from database

### 1.3 Remove Cache Write Logic

- Remove `putReaddirCached()`, `putLookupCached()`, etc.
- Remove all cache population calls

### 1.4 Remove Invalidation Logic

- Remove `invalidateReaddir()`, `invalidateLookup()`, etc.
- Remove all invalidation calls from Remove, Create, Move operations
- Remove generation counter mechanism

### 1.5 Testing

- Run deletion tests (rm -rf)
- Verify no stale handle errors
- Measure baseline performance (will be slow, but correct)

**Deliverable**: Clean codebase with zero caching, verified correct behavior

---

## Phase 2: Design Core Snapshot System (Week 2)

### 2.1 Create Versioned Cache Entry Types

New file: `pkg/metadata/badger/cache_types.go`

```go
// Immutable snapshot of cache data
type CacheVersion uint64

type VersionedReaddirEntry struct {
    version   CacheVersion
    children  []metadata.FileHandle
    names     []string
    timestamp time.Time
}

type VersionedLookupEntry struct {
    version CacheVersion
    handle  metadata.FileHandle
    attr    *metadata.FileAttr  // Deep copy
    timestamp time.Time
}
// ... similar for other cache types
```

### 2.2 Create Per-Type Version Counters

New file: `pkg/metadata/badger/snapshot.go`

**Design Decision**: Use separate version counters per cache type for better granularity.

```go
// Per-type version tracking for granular invalidation
type VersionCounters struct {
    // Global stats version - incremented on any create/delete
    stats atomic.Uint64

    // Per-handle versions - for file attributes
    // Map: FileHandle -> *atomic.Uint64
    handles sync.Map

    // Per-directory versions - for ReadDir and Lookup caches
    // Map: DirHandle -> *atomic.Uint64
    dirs sync.Map
}

// GetHandleVersion returns the current version for a specific file handle
func (vc *VersionCounters) GetHandleVersion(handle FileHandle) uint64 {
    if v, ok := vc.handles.Load(handle); ok {
        return v.(*atomic.Uint64).Load()
    }
    return 0  // Never cached
}

// InvalidateHandle increments version for a specific file handle
func (vc *VersionCounters) InvalidateHandle(handle FileHandle) {
    v, _ := vc.handles.LoadOrStore(handle, &atomic.Uint64{})
    v.(*atomic.Uint64).Add(1)
}

// GetDirVersion returns the current version for a directory
func (vc *VersionCounters) GetDirVersion(dirHandle FileHandle) uint64 {
    if v, ok := vc.dirs.Load(dirHandle); ok {
        return v.(*atomic.Uint64).Load()
    }
    return 0
}

// InvalidateDir increments version for a directory (affects ReadDir + Lookup)
func (vc *VersionCounters) InvalidateDir(dirHandle FileHandle) {
    v, _ := vc.dirs.LoadOrStore(dirHandle, &atomic.Uint64{})
    v.(*atomic.Uint64).Add(1)
}

// InvalidateStats increments global stats version
func (vc *VersionCounters) InvalidateStats() {
    vc.stats.Add(1)
}
```

### 2.3 Snapshot Registry with Per-Type Versions

```go
type SnapshotRegistry struct {
    versions *VersionCounters
    mu       sync.RWMutex
    // Map: handle -> atomic.Pointer[VersionedEntry]
}

// GetSnapshot returns current version atomically
func (sr *SnapshotRegistry) GetSnapshot(handle) (entry, version)

// UpdateSnapshot atomically replaces entry with new version
func (sr *SnapshotRegistry) UpdateSnapshot(handle, newEntry)
```

### 2.4 Version Validation Logic with Granular Checks

```go
// GetFile cache - check per-handle version
entry := getFileCache.Get(handle)
if entry != nil && entry.version == versions.GetHandleVersion(handle) {
    // HIT: File attrs unchanged
    return entry.data
}
// MISS: Re-read from DB

// ReadDir cache - check per-directory version
snapshot := readdirCache.Get(dirHandle)
if snapshot != nil && snapshot.version == versions.GetDirVersion(dirHandle) {
    // HIT: Directory unchanged
    return snapshot.entries
}
// MISS: Re-read from DB

// Lookup cache - check parent directory version
lookupEntry := lookupCache.Get(parentHandle, name)
if lookupEntry != nil && lookupEntry.parentVersion == versions.GetDirVersion(parentHandle) {
    // HIT: Parent directory unchanged
    return lookupEntry.childHandle
}
// MISS: Re-read from DB
```

### 2.5 Benefits of Per-Type Version Counters

**Granular Invalidation**:
- File attribute changes (size/mtime) don't invalidate ReadDir caches
- Directory changes don't invalidate unrelated file GetFile caches
- Better cache hit rates overall

**Isolation**:
- `SetFileAttributes(fileX)` → only invalidates GetFile cache for fileX
- `CreateFile(dirY, "new")` → only invalidates ReadDir/Lookup for dirY + stats
- `RemoveFile(dirZ, "old")` → invalidates GetFile for old + ReadDir/Lookup for dirZ + stats

**Correctness Maintained**:
- Each cache type still has zero tolerance for stale data
- Version checks guarantee consistency within each cache domain

### 2.6 Invalidation Matrix

Table showing which operations invalidate which version counters:

| Operation | Stats | Handle(file) | Dir(parent) | Notes |
|-----------|-------|--------------|-------------|-------|
| **CreateFile(parent, name)** | ✅ | ✅ (new) | ✅ (parent) | New file created |
| **RemoveFile(parent, name)** | ✅ | ✅ (file) | ✅ (parent) | File deleted |
| **SetFileAttributes(file)** | ❌ | ✅ (file) | ❌ | Only file attrs changed |
| **WriteFile(file)** | ❌ | ✅ (file) | ❌ | Size/mtime changed |
| **CreateDirectory(parent, name)** | ✅ | ✅ (new dir) | ✅ (parent) | New directory created |
| **RemoveDirectory(parent, name)** | ✅ | ✅ (dir) | ✅ (parent) | Empty directory deleted |
| **Rename(srcParent, srcName, dstParent, dstName)** | ❌ | ✅ (file) | ✅ (both parents) | Move affects both dirs |
| **Lookup(parent, name)** | ❌ | ❌ | ❌ | Read-only operation |
| **ReadDirectory(dir)** | ❌ | ❌ | ❌ | Read-only operation |
| **GetFile(file)** | ❌ | ❌ | ❌ | Read-only operation |

**Implementation Example**:

```go
func (s *Store) CreateFile(ctx *metadata.AuthContext, parent metadata.FileHandle, name string, attr *metadata.FileAttr) (metadata.FileHandle, error) {
    // 1. Create in database
    newHandle, err := s.createFileInDB(parent, name, attr)
    if err != nil {
        return 0, err
    }

    // 2. Invalidate affected caches (granular)
    s.versions.InvalidateStats()          // File count changed
    s.versions.InvalidateHandle(newHandle) // New file has version 1
    s.versions.InvalidateDir(parent)       // Parent dir listing changed

    return newHandle, nil
}

func (s *Store) SetFileAttributes(ctx *metadata.AuthContext, handle metadata.FileHandle, attr *metadata.FileAttr) error {
    // 1. Update in database
    if err := s.updateFileInDB(handle, attr); err != nil {
        return err
    }

    // 2. Invalidate only the file's attributes cache (NOT stats or dir)
    s.versions.InvalidateHandle(handle)

    return nil
}
```

**Deliverable**: Snapshot system design document + core data structures

---

## Design Comparison: Per-Type vs Global Version Counters

### Global Version Counter (Simpler, Less Optimal)

```go
type GlobalVersioning struct {
    version atomic.Uint64  // Single counter for everything
}
```

**Pros:**
- Simpler implementation
- Easier to reason about
- Less code

**Cons:**
- Over-invalidation: any write invalidates ALL caches
- `SetFileAttributes(fileA)` invalidates ReadDir caches for unrelated directories
- Lower cache hit rates under mixed workloads
- Wasted work re-reading unaffected data

### Per-Type Version Counters (Recommended)

```go
type VersionCounters struct {
    stats   atomic.Uint64  // Global
    handles sync.Map       // Per-file
    dirs    sync.Map       // Per-directory
}
```

**Pros:**
- **Granular invalidation**: Only affected caches invalidated
- **Higher hit rates**: Unrelated operations don't interfere
- **Better performance**: Less DB thrashing under concurrent workload
- **Scalability**: Large directories don't affect other directories

**Cons:**
- More complex implementation
- More memory overhead (per-handle/per-dir versions)
- Slightly more code to maintain

### Performance Impact Example

**Scenario**: 1000 concurrent clients, each working in different directories

| Operation | Global Version | Per-Type Version |
|-----------|----------------|------------------|
| Client 1: `WriteFile(dir1/fileA)` | Invalidates ALL caches | Invalidates only dir1 + fileA |
| Client 2: `ReadDir(dir2)` | Cache miss (version bumped by Client 1) | **Cache hit** (dir2 version unchanged) |
| Client 3: `GetFile(dir3/fileB)` | Cache miss (version bumped by Client 1) | **Cache hit** (fileB version unchanged) |
| **Result** | ~0% hit rate under contention | **~95% hit rate** (only affected entries miss) |

### Recommendation

**Use per-type version counters** - The complexity cost is minimal compared to the performance benefits. Start with this design from Phase 2 to avoid refactoring later.

---

## Phase 3: Implement ShareName Cache (Week 3)

**Why first?** Immutable data = simplest cache, validates snapshot system

### 3.1 Integrate Snapshot System

- Replace shareNameCache map with SnapshotRegistry
- Implement version-tracked sharename entries
- Add unit tests

### 3.2 Invalidation Strategy

- ShareNames are immutable → only invalidate on file deletion
- Simple version increment on RemoveFile/RemoveDirectory

### 3.3 Testing

- Unit tests: concurrent Get/Put/Invalidate
- Integration test: Verify never returns stale sharename
- Performance test: Compare to no-cache baseline

**Deliverable**: Working sharename cache with snapshot isolation

---

## Phase 4: Implement GetFile Cache (Week 4)

**Why second?** Frequent reads, accepts stale attributes briefly (TTL backup)

### 4.1 Version-Tracked File Attributes

- Deep copy FileAttr on cache population
- Atomic pointer swap on updates
- Version validation on read using **per-handle version counter**

```go
// Cache population
entry := &VersionedFileEntry{
    attr:      deepCopyAttr(attr),
    version:   versions.GetHandleVersion(handle),
    cachedAt:  time.Now(),
}
cache.Store(handle, entry)

// Cache validation
cached := cache.Load(handle)
if cached != nil &&
   cached.version == versions.GetHandleVersion(handle) &&
   time.Since(cached.cachedAt) < 2*time.Second {
    return cached.attr  // HIT
}
// MISS: version changed or TTL expired
```

### 4.2 Invalidation Points (Per-Handle Granularity)

- Invalidate on: RemoveFile, SetFileAttributes, CommitWrite
- **Only invalidates this specific file's handle version** (not global, not directory)
- Other files' GetFile caches unaffected

### 4.3 TTL as Secondary Defense

- Even with versioning, add 2-second TTL
- Prevents unbounded staleness if invalidation missed

### 4.4 Testing

- Concurrent GETATTR + SetAttr operations
- Verify mtime/size updates propagate
- Deletion test: ensure GETATTR fails after remove

**Deliverable**: GetFile cache with versioning + TTL

---

## Phase 5: Implement Lookup Cache (Week 5)

**Critical for path resolution performance**

### 5.1 Parent-Aware Versioning (Per-Directory Versions)

- Each directory has its own version via `versions.GetDirVersion(dirHandle)`
- Lookup cache entries tagged with parent's directory version
- Invalidate all children when parent version changes

```go
// Cache population
entry := &LookupCacheEntry{
    childHandle:   childHandle,
    childAttr:     deepCopyAttr(attr),
    parentVersion: versions.GetDirVersion(parentHandle),  // Per-directory version
    cachedAt:      time.Now(),
}
cache.Store(LookupKey{parentHandle, name}, entry)

// Cache validation
cached := cache.Load(LookupKey{parentHandle, name})
if cached != nil && cached.parentVersion == versions.GetDirVersion(parentHandle) {
    return cached.childHandle  // HIT: parent directory unchanged
}
// MISS: parent directory modified
```

### 5.2 Composite Key Design

```go
type LookupKey struct {
    parentHandle FileHandle
    name         string
}

type LookupCacheEntry struct {
    childHandle   FileHandle
    childAttr     *FileAttr
    parentVersion uint64     // Captured from versions.GetDirVersion(parent)
    cachedAt      time.Time
}
```

### 5.3 Invalidation (Per-Directory Granularity)

- Directory modifications call `versions.InvalidateDir(parentHandle)`
- All cached lookups for **only that directory** become stale
- Lookups in other directories remain cached (unaffected)
- No prefix scan needed - version check handles it automatically

### 5.4 Testing

- Lookup("dir", "file1") → create "file2" → ensure file1 still cached
- Lookup("dir", "file1") → remove "file1" → ensure cache invalidated
- Concurrent lookups during directory modifications

**Deliverable**: Lookup cache with parent versioning

---

## Phase 6: Implement Stats Cache (Week 6)

**Simplest**: single-entry cache

### 6.1 Global Versioning

- Global stats version counter
- Invalidate on any file create/delete
- Short TTL (5 seconds) as secondary defense

### 6.2 Unify with Main Store Lock

- Use main s.mu instead of separate mutex
- Eliminates lock-ordering issues

### 6.3 Testing

- FSSTAT during concurrent creates
- Verify counts never go backwards

**Deliverable**: Stats cache with unified locking

---

## Phase 7: Implement ReadDir Cache (Week 7-8)

**Most complex**: entire directory snapshots

### 7.1 Immutable Directory Snapshots (Per-Directory Versioning)

```go
type ReaddirSnapshot struct {
    dirHandle FileHandle
    version   uint64                // Captured from versions.GetDirVersion(dirHandle)
    entries   []metadata.DirEntry   // Complete listing (immutable)
    timestamp time.Time
}
```

**Key property**: Snapshot is tied to the directory's version, not a global version.

### 7.2 Snapshot on First Read

- First ReadDirectory() reads entire dir from DB
- Create immutable snapshot with **current directory version**
- Store in atomic pointer

```go
// On ReadDirectory call
snapshot := readdirCache.Load(dirHandle)
currentDirVersion := versions.GetDirVersion(dirHandle)

if snapshot != nil && snapshot.version == currentDirVersion {
    // HIT: Directory unchanged, use cached snapshot
    return paginate(snapshot.entries, token, maxBytes)
}

// MISS: Read entire directory from DB
entries := readAllEntriesFromDB(dirHandle)

// Create new snapshot with current version
newSnapshot := &ReaddirSnapshot{
    dirHandle: dirHandle,
    version:   currentDirVersion,
    entries:   entries,
    timestamp: time.Now(),
}
readdirCache.Store(dirHandle, newSnapshot)

return paginate(entries, token, maxBytes)
```

### 7.3 Validation on Hit

```go
snapshot := readdirCache.Load(dirHandle)
if snapshot != nil && snapshot.version == versions.GetDirVersion(dirHandle) {
    // HIT: Directory unchanged since snapshot
    return paginate(snapshot.entries, token, maxBytes)
}
// MISS: Version changed - re-read from DB
```

### 7.4 Invalidation on Modify (Per-Directory)

- Any child add/remove calls `versions.InvalidateDir(dirHandle)`
- Only this directory's version incremented
- Cached snapshots for **other directories** remain valid
- Atomic pointer swap to nil (discard snapshot)
- Next read creates fresh snapshot with new version

### 7.5 Retry Loop Removal

- No more retry logic needed!
- Version check guarantees consistency
- Either get current snapshot or re-read

### 7.6 Testing

- ReadDir during concurrent creates/deletes
- Verify never returns deleted file handles
- Test rm -rf with 1000+ files
- Compare performance to no-cache baseline

**Deliverable**: ReadDir cache with snapshot isolation, passing all deletion tests

---

## Phase 8: Performance Validation (Week 9)

### 8.1 Benchmark Suite

- Large directory operations (1000+ files)
- Concurrent rm -rf
- Mixed workload (reads + writes)

### 8.2 Metrics Collection

- Cache hit rates per type
- Version invalidation frequency
- Lock contention measurements

### 8.3 Tuning

- Adjust TTLs based on invalidation patterns
- Tune max entries per cache
- Profile lock contention

**Deliverable**: Performance report, tuned cache parameters

---

## Phase 9: Documentation (Week 10)

### 9.1 Architecture Doc

- Snapshot isolation model
- Version lifecycle
- Invalidation guarantees

### 9.2 Code Documentation

- Update all cache-related comments
- Add examples to snapshot.go
- Document invariants

### 9.3 Testing Guide

- How to verify cache correctness
- Stress test procedures

**Deliverable**: Complete cache documentation

---

## Success Criteria

### Correctness (MUST PASS)

- ✅ rm -rf of 1000+ files completes with zero errors
- ✅ No "Stale NFS file handle" errors
- ✅ No duplicate entries in directory listings
- ✅ Concurrent stress tests pass for 1 hour

### Performance (TARGETS)

- 📈 ReadDirectory 10x faster than no-cache (via snapshot reuse)
- 📈 Lookup 5x faster than no-cache
- 📈 GetFile 3x faster than no-cache
- 📉 rm -rf completes in <5 seconds for 500 files

### Code Quality

- 📝 All caches use snapshot isolation pattern
- 📝 Zero TOCTOU races
- 📝 Comprehensive test coverage (>80%)

---

## Rollback Plan

Each phase can be independently disabled:
- Add feature flag per cache type
- If issues found, disable that cache only
- System remains functional with caching disabled

---

## Estimated Timeline

- Week 1: Remove existing caches
- Weeks 2-3: Design + ShareName cache
- Weeks 4-6: GetFile + Lookup + Stats caches
- Weeks 7-8: ReadDir cache (most complex)
- Weeks 9-10: Performance tuning + docs

**Total**: 10 weeks for complete, production-ready cache redesign

---

## Implementation Roadmap Checklist

### Phase 1: Remove All Existing Caches (Week 1)
- [ ] Disable all cache enabled flags in store.go:313-368
- [ ] Remove cache read logic (getReaddirCached, getLookupCached, etc.)
- [ ] Remove cache write logic (putReaddirCached, putLookupCached, etc.)
- [ ] Remove cache invalidation logic
- [ ] Run deletion tests (rm -rf)
- [ ] Measure baseline performance
- [ ] **Deliverable**: Clean codebase with zero caching

### Phase 2: Design Core Snapshot System (Week 2)
- [ ] Create pkg/metadata/badger/cache_types.go
- [ ] Implement CacheVersion, VersionedReaddirEntry, VersionedLookupEntry
- [ ] Create pkg/metadata/badger/snapshot.go
- [ ] Implement VersionCounters with per-type version tracking (stats, handles, dirs)
- [ ] Implement GetHandleVersion, InvalidateHandle, GetDirVersion, InvalidateDir
- [ ] Implement SnapshotRegistry with atomic operations
- [ ] Implement granular version validation logic (per cache type)
- [ ] Create invalidation matrix documentation
- [ ] **Deliverable**: Snapshot system design + core data structures

### Phase 3: Implement ShareName Cache (Week 3)
- [ ] Replace shareNameCache map with SnapshotRegistry
- [ ] Implement version-tracked sharename entries
- [ ] Add unit tests for concurrent operations
- [ ] Integration test: verify no stale sharenames
- [ ] Performance test vs no-cache baseline
- [ ] **Deliverable**: Working sharename cache with snapshot isolation

### Phase 4: Implement GetFile Cache (Week 4)
- [ ] Implement deep copy FileAttr on cache population
- [ ] Add atomic pointer swap on updates
- [ ] Add version validation on read
- [ ] Implement invalidation on RemoveFile, SetFileAttributes, CommitWrite
- [ ] Add 2-second TTL as secondary defense
- [ ] Test concurrent GETATTR + SetAttr operations
- [ ] Test mtime/size update propagation
- [ ] Test GETATTR fails after remove
- [ ] **Deliverable**: GetFile cache with versioning + TTL

### Phase 5: Implement Lookup Cache (Week 5)
- [ ] Implement parent-aware versioning
- [ ] Create composite LookupKey design
- [ ] Implement directory version increment on modifications
- [ ] Test Lookup with concurrent creates
- [ ] Test Lookup invalidation on removes
- [ ] Test concurrent lookups during modifications
- [ ] **Deliverable**: Lookup cache with parent versioning

### Phase 6: Implement Stats Cache (Week 6)
- [ ] Implement global stats version counter
- [ ] Add invalidation on file create/delete
- [ ] Add 5-second TTL
- [ ] Unify with main s.mu lock
- [ ] Test FSSTAT during concurrent creates
- [ ] Verify counts never go backwards
- [ ] **Deliverable**: Stats cache with unified locking

### Phase 7: Implement ReadDir Cache (Week 7-8)
- [ ] Implement immutable ReaddirSnapshot structure
- [ ] Implement snapshot on first read
- [ ] Implement version validation on hit
- [ ] Implement invalidation on modify (atomic pointer swap)
- [ ] Remove retry loop logic
- [ ] Test ReadDir during concurrent creates/deletes
- [ ] Verify never returns deleted file handles
- [ ] Test rm -rf with 1000+ files
- [ ] Compare performance to no-cache baseline
- [ ] **Deliverable**: ReadDir cache with snapshot isolation

### Phase 8: Performance Validation (Week 9)
- [ ] Create benchmark suite (large dirs, concurrent rm -rf, mixed workload)
- [ ] Collect cache hit rate metrics
- [ ] Measure version invalidation frequency
- [ ] Profile lock contention
- [ ] Tune TTLs based on patterns
- [ ] Tune max entries per cache
- [ ] **Deliverable**: Performance report + tuned parameters

### Phase 9: Documentation (Week 10)
- [ ] Write architecture doc (snapshot isolation model)
- [ ] Document version lifecycle
- [ ] Document invalidation guarantees
- [ ] Update all cache-related code comments
- [ ] Add examples to snapshot.go
- [ ] Document invariants
- [ ] Write testing guide (verification, stress tests)
- [ ] **Deliverable**: Complete cache documentation

### Final Validation
- [ ] ✅ rm -rf of 1000+ files with zero errors
- [ ] ✅ No "Stale NFS file handle" errors
- [ ] ✅ No duplicate entries in directory listings
- [ ] ✅ Concurrent stress tests pass for 1 hour
- [ ] 📈 ReadDirectory 10x faster than no-cache
- [ ] 📈 Lookup 5x faster than no-cache
- [ ] 📈 GetFile 3x faster than no-cache
- [ ] 📉 rm -rf completes in <5 seconds for 500 files

# Phase 16: Cache mmap removal - Pattern Map

**Mapped:** 2026-05-20
**Files analyzed:** 8 (1 interface, 2 impls, 1 call-site, 1 test absorb, 3 deletions)
**Analogs found:** 7 / 8 (deletions have no analog by definition)

## File Classification

| File (modify/create/delete) | Role | Data Flow | Closest Analog | Match Quality |
|------------------------------|------|-----------|----------------|---------------|
| `pkg/blockstore/local/local.go` | interface extension | request-response | `local.go` lines 49-159 (existing `LocalStore` method shapes) | exact |
| `pkg/blockstore/local/fs/fs.go` (or `chunkstore.go`) — add `(*FSStore).Get` | implementation | file-I/O read | `(*FSStore).ReadChunk` in `pkg/blockstore/local/fs/chunkstore.go:110-141` | exact |
| `pkg/blockstore/local/memory/memory.go` — add `(*MemoryStore).Get` (interface obligation) | implementation | RAM read | `(*MemoryStore).GetBlockData` in `pkg/blockstore/local/memory/memory.go:117-134` | role-match (no CAS in memory store yet — may return `ErrChunkNotFound` always or stub) |
| `pkg/blockstore/engine/engine.go:204-238` — `loadByHash` rewire | call-site rewire | request-response | existing closure body lines 219-229 (the `readFromCAS` branch being deleted) and `bs.local.GetBlockData` fallback at line 233 | exact |
| `pkg/blockstore/engine/cache_test.go` — absorb generic asserts (D-10) | test refactor | request-response | existing `TestCache_GetPut_Basic` at line 25, `TestCache_LRUEviction` at line 45 | exact |
| `pkg/blockstore/engine/cache_mmap_unix.go` | deletion | n/a | — | n/a |
| `pkg/blockstore/engine/cache_mmap_windows.go` | deletion | n/a | — | n/a |
| `pkg/blockstore/engine/cache_mmap_test.go` | deletion (D-09 — generics cherry-picked first) | n/a | — | n/a |
| `pkg/blockstore/engine/perf_bench_unix_test.go` — delete `TestPerfGate_Phase12_MmapHotPath` | targeted delete | n/a | file may become empty → fold per Claude's Discretion (D-08) | n/a |

**Production consumer audit (confirmed):**
- `grep -rn "syscall.Mmap" pkg/` → only `pkg/blockstore/engine/cache_mmap_unix.go:75` (the file being deleted) and a docstring reference in `pkg/blockstore/engine/cache.go:54` (which must also be updated).
- `grep -rn "readFromCAS" pkg/` → only the files in scope (cache_mmap_unix.go, cache_mmap_windows.go, cache_mmap_test.go, perf_bench_unix_test.go, engine.go:221, doc references in cache.go and perf_bench_phase12_test.go).
- **No external production consumer** of either symbol. Deletion is safe.

## Pattern Assignments

### `pkg/blockstore/local/local.go` — add `Get(ctx, hash) ([]byte, error)` to `LocalStore`

**Analog:** existing `LocalStore` interface in same file (lines 49-159).

**Style to copy** — method placement, godoc voice, `ctx context.Context` first parameter, return shape that mirrors existing methods:

```go
// GetBlockData returns the raw data for a specific block, checking memory first
// then disk. Returns data, dataSize, and error.
GetBlockData(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)
```
(`pkg/blockstore/local/local.go:68-69`)

**Established convention:** *every* `LocalStore` method takes `ctx context.Context` as first parameter (verified across lines 58, 62, 65, 69, 74, 78, 84, 87, 91, 96, 102, 105, 108, 111, 122, 125, 144, 147, 158).

**Method signature to add** (matches D-01 + Claude's Discretion ctx-first call):

```go
// Get returns the chunk bytes addressed by the given content hash.
// The returned []byte is freshly allocated and owned by the caller
// (D-03 buffer-ownership contract; matches today's mmap-then-copy
// semantics from the Cache's perspective).
//
// Returns blockstore.ErrChunkNotFound if the chunk is absent from the
// local store. Phase 17 promotes this method onto the unified
// BlockStore interface verbatim.
Get(ctx context.Context, hash blockstore.ContentHash) ([]byte, error)
```

Placement: under `// --- Read operations ---` section (around line 53), grouped with `ReadAt` / `GetBlockData` (the existing read-surface methods).

---

### `pkg/blockstore/local/fs/fs.go` (or `chunkstore.go`) — implement `(*FSStore).Get`

**Analog:** `(*FSStore).ReadChunk` in `pkg/blockstore/local/fs/chunkstore.go:108-141`.

**Closest pattern** — already-existing method that does *exactly* what Phase 16 needs (ctx-first, hash-keyed, returns freshly allocated bytes, error wrap convention):

```go
// ReadChunk returns the bytes of the chunk addressed by h.
// Returns blockstore.ErrChunkNotFound if the chunk is absent.
func (bc *FSStore) ReadChunk(ctx context.Context, h blockstore.ContentHash) ([]byte, error) {
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := bc.chunkPath(h)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, blockstore.ErrChunkNotFound
		}
		return nil, fmt.Errorf("chunkstore: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("chunkstore: read: %w", err)
	}
	// LSL-08: promote on cache hit so frequently-read chunks survive eviction.
	if _, statErr := os.Stat(path); statErr == nil {
		bc.lruTouch(h, int64(len(data)), path)
	}
	return data, nil
}
```
(`pkg/blockstore/local/fs/chunkstore.go:108-141`)

**Per CONTEXT.md (`Reusable Assets` block + line 72):** `(*FSStore).Get` is a one-line wrapper over `ReadChunk`. The wrapper exists only to satisfy the new `LocalStore.Get` interface method; the body delegates straight through:

```go
// Get implements local.LocalStore.Get by delegating to ReadChunk.
// See ReadChunk for semantics, error contract, and LRU touch behavior.
func (bc *FSStore) Get(ctx context.Context, h blockstore.ContentHash) ([]byte, error) {
	return bc.ReadChunk(ctx, h)
}
```

**Patterns the planner MUST preserve:**
1. `ctx context.Context` first parameter (matches every other `LocalStore` method + `ReadChunk`).
2. `blockstore.ContentHash` as the hash type (`pkg/blockstore/types.go:22`).
3. Error wrap convention: `fmt.Errorf("chunkstore: <verb>: %w", err)` (all over `chunkstore.go`).
4. ENOENT → `blockstore.ErrChunkNotFound` sentinel (not a wrapped error).
5. Closed-store guard: `if bc.isClosed() { return nil, ErrStoreClosed }`.

**File-placement decision** (planner discretion): adding `Get` either as a one-liner in `fs.go` near other interface satisfiers, or as a sibling to `ReadChunk` in `chunkstore.go`. The latter co-locates the wrapper next to its delegate — matches the existing `(*FSStore).StoreChunk` / `ReadChunk` / `HasChunk` / `DeleteChunk` grouping.

---

### `pkg/blockstore/local/memory/memory.go` — implement `(*MemoryStore).Get`

**Analog:** `(*MemoryStore).GetBlockData` in `pkg/blockstore/local/memory/memory.go:117-134`.

**Pattern to copy** — same closed-store guard + sentinel-error contract:

```go
func (s *MemoryStore) GetBlockData(_ context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, 0, ErrStoreClosed
	}

	key := blockKey(payloadID, blockIdx)
	mb, ok := s.blocks[key]
	if !ok || mb.data == nil || mb.dataSize == 0 {
		return nil, 0, blockstore.ErrBlockNotFound
	}

	data := make([]byte, mb.dataSize)
	copy(data, mb.data[:mb.dataSize])
	return data, mb.dataSize, nil
}
```
(`pkg/blockstore/local/memory/memory.go:117-134`)

**Implementation choice for `MemoryStore.Get`:** the memory store has no CAS chunk layer (no `blocks/<hh>/<hh>/<hex>` tree). Two viable shapes — planner picks:

1. **Stub (recommended for Phase 16):** return `nil, blockstore.ErrChunkNotFound` unconditionally. `MemoryStore` is for tests + ephemeral configs; the engine's `loadByHash` only fires for FileBlocks with `LocalPath` set, which the memory backend never produces. Phase 17 may expand `MemoryStore` to track CAS chunks in a `map[ContentHash][]byte` when the unified `BlockStore` interface lands; defer that work.
2. **Eager:** add a `map[blockstore.ContentHash][]byte` field, populated from a hypothetical `StoreChunk`. Out of scope per CONTEXT.md (CAS storage on memory backend is not a Phase 16 concern).

The closed-store guard + `RLock/RUnlock` pattern is mandatory regardless of which shape is picked.

---

### `pkg/blockstore/engine/engine.go:204-238` — rewire `loadByHash`

**Analog:** the existing `loadByHash` body — Phase 16 simplifies it by deleting the mmap branch (lines 216-229) and falling back to the local-store call directly via the new `Get`.

**Current body** (`pkg/blockstore/engine/engine.go:204-238`):

```go
func (bs *BlockStore) loadByHash(ctx context.Context, hash blockstore.ContentHash) ([]byte, error) {
	if bs.fileBlockStore == nil {
		return nil, errors.New("loadByHash: fileBlockStore not wired")
	}
	fb, err := bs.fileBlockStore.GetByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if fb == nil || fb.LocalPath == "" {
		return nil, errors.New("loadByHash: block not local")
	}

	// CACHE-06 single-copy fast path: when DataSize is known, allocate
	// the destination buffer and read directly via the platform-aware
	// mmap/ReadFile primitive.
	if fb.DataSize > 0 {
		buf := make([]byte, fb.DataSize)
		n, err := readFromCAS(fb.LocalPath, 0, buf)
		if err == nil {
			return buf[:n], nil
		}
		// Fall through to the legacy path on any readFromCAS error
		// ...
	}

	// Legacy fallback: read through the local store.
	data, _, err := bs.local.GetBlockData(ctx, fb.ID, 0)
	if err != nil {
		return nil, err
	}
	return data, nil
}
```

**Target body after rewire** (D-02): one CAS-keyed call, no mmap branch, no `GetBlockData` fallback (Phase 16's `local.Get(ctx, hash)` is content-addressed; no `LocalPath`/`DataSize` plumbing needed):

```go
// loadByHash is the LoadByHashFn the Cache's prefetch workers call to
// pull a block by ContentHash. Phase 16: simplified to a single
// content-addressed local read — the mmap fast-path is gone and the
// Cache copies bytes into its LRU slot on miss (D-03 buffer ownership).
//
// Remote fallback is intentionally NOT wired here — prefetch is
// best-effort and shouldn't block on a remote round-trip; if the
// block isn't local, the next on-path read pulls it via the syncer.
func (bs *BlockStore) loadByHash(ctx context.Context, hash blockstore.ContentHash) ([]byte, error) {
	return bs.local.Get(ctx, hash)
}
```

**Key simplifications enabled by D-01/D-02:**
- `fileBlockStore.GetByHash` lookup is no longer required (the hash *is* the key — no need to translate via FileBlock).
- The `fb.LocalPath == ""` and `fb.DataSize > 0` branches both disappear.
- The `bs.local.GetBlockData(ctx, fb.ID, 0)` legacy fallback disappears (it was a payload+blockIdx call site; the new path is hash-keyed).

**`errors` import:** after rewire, this function no longer constructs `errors.New(...)` calls. Verify the `"errors"` import is still used elsewhere in `engine.go` before deleting it — `errors.Join(errs...)` at line 260 keeps it.

**Cache `loadFn` signature unchanged** (per CONTEXT line 93): `LoadByHashFn func(ctx context.Context, hash blockstore.ContentHash) ([]byte, error)` (cache.go:119). No change to `NewCache` wiring at engine.go:182.

---

### `pkg/blockstore/engine/cache.go:54` — docstring update

**Why:** lines 52-67 describe `readFromCAS` as the "platform-aware single-copy primitive" and reference `cache_mmap_unix.go` / `cache_mmap_windows.go`. After Phase 16 these references are stale.

**Pattern to apply:** replace the multi-paragraph "Plan 12-10 (CACHE-06) introduces…" block (lines 52-68) with a one-sentence Phase 16 note. Style match — the existing CACHE-02 paragraph at lines 80-83 is the right voice:

```go
// CACHE-02 cross-file dedup: two payloads referencing the same
// ContentHash share one cache entry. Eviction is hash-scoped (LRU);
// InvalidateFile is surgical (drops only the explicitly-listed hashes
// for a file, preserving any shared entries).
```

Phase 16 replacement (suggested):

```go
// Phase 16: bytes loaded on miss via local.Get(ctx, hash) — see
// engine.loadByHash. The Cache copies the returned []byte into its
// LRU slot (D-03 buffer ownership). No mmap/page-cache fast path
// exists post-Phase-16; production workloads are warm-cache and the
// per-miss alloc is uncontended.
```

---

### `pkg/blockstore/engine/cache_test.go` — absorb generic byte-correctness asserts (D-10)

**Analog:** existing test style in same file:

```go
func TestCache_GetPut_Basic(t *testing.T) {
	c := newCacheNoWorkers(1 << 20)
	// ... LRU semantics, byte equality via bytes.Equal
}
```
(`pkg/blockstore/engine/cache_test.go:25` — full body extends through `TestCache_LRUEviction` and `TestCache_CrossFileDedup_CACHE02`).

**Pattern to follow when porting generic asserts from `cache_mmap_test.go`:**

`cache_mmap_test.go` tests exercise `readFromCAS(path, offset, dest)` — a file-system primitive that no longer exists. The *generic byte-correctness* concerns to preserve are:

1. **Full round-trip equality** (`TestReadFromCAS_RoundTrip` at line 46): "N-byte chunk reads back byte-identical." → port as `TestCache_LargeChunkRoundTrip` exercising `Cache.Put(hash, bytes)` then `Cache.Get(hash)` → `bytes.Equal`.
2. **Empty/zero-len dest** (`TestReadFromCAS_EmptyDest` at line 164): mostly mmap-specific (offset semantics) — drop, no analog in the Cache API surface.
3. **Missing-file error** (`TestReadFromCAS_MissingFile` at line 186): becomes Cache miss → `(nil, false)` return — already covered by `TestCache_GetPut_Basic` implicitly.

**Asserts to DROP (D-10 explicitly excludes them):**
- `TestReadFromCAS_PartialOffset` (line 69) — offset semantics, no Cache analog (Cache.Get returns the whole slot).
- `TestReadFromCAS_DestSmallerThanFile` (line 91) — same.
- `TestReadFromCAS_BelowMmapThreshold_UsesReadFile` (line 114) — mmap-specific.
- `TestReadFromCAS_OffsetAtEOF` (line 148) — mmap-specific.
- `TestReadFromCAS_Windows_FallbackPath` (line 198) — platform-specific.

**Net: maybe 1 new test** in `cache_test.go` (large-chunk round-trip). Likely subsumed by existing `TestCache_GetPut_Basic` already — planner judgement call during the delete PR.

---

### `pkg/blockstore/engine/perf_bench_unix_test.go` — delete `TestPerfGate_Phase12_MmapHotPath`

**Analog (for fold decision, per Claude's Discretion):** `pkg/blockstore/engine/perf_bench_test.go` is the cross-platform sibling. If `perf_bench_unix_test.go` becomes empty after deleting `TestPerfGate_Phase12_MmapHotPath` + the helper `formatChunkName(i int)` it uses, delete the file entirely (and its `//go:build linux || darwin` tag).

**File currently has only:** `TestPerfGate_Phase12_MmapHotPath` (lines 29-96) + `formatChunkName` (lines 98-105). Both can be deleted; the file will be empty → delete the file.

---

## Shared Patterns

### LocalStore method signature convention
**Source:** `pkg/blockstore/local/local.go:52-159`
**Apply to:** the new `Get` method definition.

```go
// Every LocalStore method takes ctx context.Context as the first parameter.
// Receivers either use ctx (cancellation, deadlines, tracing) or accept it
// with `_ context.Context` if the impl is purely synchronous in-memory.
```

Concrete examples from same file:
- `ReadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error)` (line 58)
- `Truncate(ctx context.Context, payloadID string, newSize uint64) error` (line 102)
- `DeleteAppendLog(ctx context.Context, payloadID string) error` (line 122)

### Error wrap convention in `pkg/blockstore/local/fs/`
**Source:** `pkg/blockstore/local/fs/chunkstore.go` (throughout)
**Apply to:** any new disk-touching code in Phase 16 (`*FSStore.Get` wraps via delegation, so this only matters if the wrapper is inlined rather than delegated).

```go
return fmt.Errorf("chunkstore: <verb>: %w", err)
```

`<verb>` is the action that failed: `"open"`, `"read"`, `"stat"`, `"mkdir"`, `"rename"`, etc. ENOENT is always converted to `blockstore.ErrChunkNotFound` (the sentinel) *before* the `%w` wrap path.

### Closed-store guard
**Source:** `(*FSStore).ReadChunk`, `(*FSStore).StoreChunk`, `(*FSStore).HasChunk`, `(*FSStore).DeleteChunk` (all in `chunkstore.go`).

```go
if bc.isClosed() {
	return nil, ErrStoreClosed
}
if err := ctx.Err(); err != nil {
	return nil, err
}
```

Mandatory first lines for any new `*FSStore` method.

### Per-share cache invariant (CLAUDE.md rule 4)
**Source:** `pkg/blockstore/engine/cache.go:75-78` (docstring); enforced by construction in `BlockStore.Start()` at `pkg/blockstore/engine/engine.go:182`.
**Apply to:** any Phase 16 change to `loadByHash` — the closure captures `bs.local`, which is the share-specific `LocalStore`. No global wiring exists; nothing in Phase 16 should introduce any.

---

## No Analog Found

None. Every Phase 16 file change has a direct in-repo analog (above). Deletions need no analog.

---

## Metadata

**Analog search scope:** `pkg/blockstore/local/`, `pkg/blockstore/local/fs/`, `pkg/blockstore/local/memory/`, `pkg/blockstore/engine/`.
**Files scanned:** ~30 (full enumeration via `find pkg/blockstore -type f -name '*.go'`).
**Production consumers of `readFromCAS`:** 1 (`engine.go:221`); all other matches are tests or docstring references — confirmed via `grep -rn "readFromCAS" pkg/`.
**Production consumers of `syscall.Mmap`:** 1 (`cache_mmap_unix.go:75`); zero other production references — confirmed via `grep -rn "syscall.Mmap" pkg/`.
**Pattern extraction date:** 2026-05-20.

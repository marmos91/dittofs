# Phase 19: write-path-ram-optimizations - Pattern Map

**Mapped:** 2026-05-21
**Files analyzed:** 17 touchpoints (4 created, 11 modified, 2 deleted)
**Analogs found:** 17 / 17

Phase 19 has unusually strong existing precedents — Phase 18 D-02 (`SyncedHashStore` injection) and the `RollupStore` interface+suite shape together cover the surface-injection and conformance patterns for every new surface in this phase. The dominant theme below is "copy Phase 18's option-field wiring; copy Phase 10's interface+suite shape; do not invent."

## File Classification

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|---|---|---|---|---|
| `pkg/blockstore/local/fs/dedup_lru.go` | implementation (LRU) | hash-keyed CRUD | `pkg/blockstore/local/fs/fdpool.go` | exact (same package, same `container/list`+map+`sync.Mutex` pattern) |
| `pkg/blockstore/local/fs/groupcommit.go` | implementation (coordinator) | event-driven fan-in | `pkg/blockstore/local/fs/appendwrite.go` (per-file mu/logsMu coordinator) | role-match (in-package; timer+pending-channel idiom is new but the locking discipline is identical) |
| `pkg/blockstore/local/fs/rollup.go` (mod) | hook-site (Opt 1 wire-in) | request-response | self (LRU lookup wraps `StoreChunk` at line 328) | exact |
| `pkg/blockstore/local/fs/appendwrite.go` (mod) | hook-site (Opt 2 wire-in) | request-response | self (groupCommit wraps `lf.f.Sync()` at line 259) | exact |
| `pkg/blockstore/local/fs/chunkstore.go` (mod) | hook-site (Opt 3 wire-in) | event emission | self (`lruTouch` call at line 104) + `objectIDPersister` invocation in `rollup.go` | exact |
| `pkg/blockstore/local/fs/fs.go` (mod) | config-knob plumbing | n/a | `FSStoreOptions.SyncedHashStore` (Phase 18 D-02), `FSStoreOptions.ObjectIDPersister` | exact |
| `pkg/blockstore/engine/engine.go` (mod) | hook-site (Opt 4 wire-in) + Opt 3 callback wiring | request-response | self (`trySpeculativeFileLevelDedup` invocation at line 669) | exact |
| `pkg/blockstore/engine/dedup.go` (mod) | implementation (Opt 4 eager small-file dedup) | request-response | self (`trySpeculativeFileLevelDedup` is the sibling fast-path) | exact |
| `pkg/metadata/file_block_store.go` (NEW or interface extension) | interface (`FileBlockStore.AddRef`) | CRUD | `pkg/metadata/rollup_store.go` (`RollupStore` interface) | exact |
| `pkg/blockstore/store.go` (mod) | interface (`FileBlockStore.AddRef`) | CRUD | self (`IncrementRefCount` method docstring) | exact |
| `pkg/metadata/store/memory/objects.go` (mod) | implementation (`AddRef` for memory backend) | CRUD | self (`incrementRefCountLocked` at line 275) | exact |
| `pkg/metadata/store/badger/objects.go` (mod) | implementation (`AddRef` for badger backend) | CRUD | self (`IncrementRefCount` at line 163, txn at line 530) | exact |
| `pkg/metadata/store/postgres/objects.go` (mod) | implementation (`AddRef` for postgres backend) | CRUD | self (postgres `IncrementRefCount` UPDATE pattern) | exact |
| `pkg/metadata/storetest/file_block_ops.go` (mod) | conformance | CRUD | `pkg/metadata/rollup_store_suite.go` + self (`testTx_IncrementRefCount_RollsBack` at line 953) | exact |
| `pkg/config/config.go` or new `pkg/config/blockstore.go` | config-knob | n/a | `SyncerConfig` (config.go:88) | exact |
| `pkg/config/defaults.go` (mod) | config-knob defaults | n/a | self (`cfg.Syncer.ApplyDefaults()` at line 27) | exact |
| `pkg/blockstore/engine/cache.go` (consumer of `OnChunkComplete`, no code change) | n/a (target of the push) | request-response | self (`Cache.Put` at line 229) | exact |
| `pkg/blockstore/doc.go` (mod) | marker | n/a | self (TRANSITIONAL convention at line 185) | exact |
| `internal/bench/phase19_test.go` (NEW) + existing aggregate runner (mod) | bench | n/a | existing `internal/bench/` runner | role-match |
| `pkg/blockstore/local/fs/rollup_idempotent_dedup_bench_test.go` (NEW) | bench | n/a | existing `pkg/blockstore/local/fs/rollup_test.go` benches | role-match |
| `pkg/blockstore/local/fs/appendwrite_group_commit_bench_test.go` (NEW) | bench | n/a | existing `pkg/blockstore/local/fs/appendwrite_test.go` | role-match |
| `pkg/blockstore/engine/cache_populated_on_rollup_test.go` (NEW) | correctness-test | n/a | existing `pkg/blockstore/engine/cache_test.go` | role-match |
| `pkg/blockstore/engine/small_file_eager_dedup_test.go` (NEW) | correctness-test | n/a | existing `pkg/blockstore/engine/dedup_test.go` | role-match |
| `pkg/config/config.go` `SyncerConfig.ClaimBatchSize` (DELETE — D-23) | cleanup | n/a | n/a (deletion) | n/a |

---

## Pattern Assignments

### `pkg/blockstore/local/fs/dedup_lru.go` (NEW — Opt 1)

**Analog:** `pkg/blockstore/local/fs/fdpool.go` (same package; canonical in-tree LRU pattern)

**Imports + struct pattern** (fdpool.go:1-22):
```go
package fs

import (
	"container/list"
	"os"
	"sync"
)

type fdPool struct {
	mu      sync.Mutex
	fds     map[string]*fdEntry
	lru     *list.List
	maxSize int
}
```

**Get/Put with promote-on-hit pattern** (fdpool.go:39-81):
```go
func (c *fdPool) Get(blockID string) *os.File {
	c.mu.Lock()
	entry, ok := c.fds[blockID]
	if ok { c.lru.MoveToFront(entry.elem) }
	c.mu.Unlock()
	if ok { return entry.f }
	return nil
}

func (c *fdPool) Put(blockID string, f *os.File) {
	c.mu.Lock(); defer c.mu.Unlock()
	if entry, ok := c.fds[blockID]; ok {
		_ = entry.f.Close(); entry.f = f
		c.lru.MoveToFront(entry.elem); return
	}
	for c.lru.Len() >= c.maxSize {
		back := c.lru.Back()
		if back == nil { break }
		victim := back.Value.(*fdEntry)
		delete(c.fds, victim.blockID); c.lru.Remove(back)
	}
	entry := &fdEntry{f: f, blockID: blockID}
	entry.elem = c.lru.PushFront(entry)
	c.fds[blockID] = entry
}
```

**Second analog (for the LRU touch idempotency idiom):** `FSStore.lruTouch` at `fs.go:440-453` — same idiom: lock-map-promote-or-insert, no error paths.

**Pattern notes / invariants:**
- Single `sync.Mutex` (NOT RWMutex) — Phase 19 D-05 calls out "stripe-locked"; the existing in-package precedent is a flat mutex. Planner should benchmark stripe vs flat mutex per D-05's "Claude's discretion" note. If keeping flat: copy `fdpool.go` verbatim. If striping: shard by first byte of `ContentHash` into N=16 buckets, each with its own list+map+mu.
- LRU value type is `*lruEntry{hash, payloadIDRefForGCTraceability}` — no `*os.File` analog; the LRU is RAM-only (D-05).
- No persistence; no startup-seeding (D-05).
- Map key = `blockstore.ContentHash` ([32]byte array — already comparable).
- API surface: `Get(h) (payloadID string, ok bool)` + `Put(h, payloadID)` + `Has(h) bool`. Match D-04: returning the prior payloadID is what `FileBlockStore.AddRef` needs.
- Per-share scope (D-02): instance lives on `FSStore`, not package-global.

---

### `pkg/blockstore/local/fs/groupcommit.go` (NEW — Opt 2)

**Analog:** existing per-file mutex + interval-tree-per-payloadID coordination in `appendwrite.go`. The exact "timer-armed fan-in" idiom is not present in-tree, but the lock-ordering discipline (per-file `mu` BEFORE `bc.logsMu`) is the load-bearing invariant.

**Lock-ordering invariant excerpt** (appendwrite.go:232-250):
```go
// LOCK ORDERING (FIX-2): release the per-file mutex BEFORE
// acquiring bc.logsMu.Lock(). Any path that holds logsMu and
// waits on mu would otherwise deadlock against us; the global
// rule is "always acquire mu before logsMu" — here we guarantee
// it by releasing mu first.
```

**Fan-in shape — modeled on `rollupCh` nudge pattern** (appendwrite.go:265-273):
```go
if bc.rollupCh != nil {
	select {
	case bc.rollupCh <- payloadID:
	default:
	}
}
```

**Pattern notes / invariants:**
- Per-file `groupCommit` lives ON `logFile` (not on `FSStore`). One coordinator per open log fd — matches D-07 "per-file log fsync, batched per file".
- D-09: coordinator fields = `pending []chan error`, `timer *time.Timer`, `mu sync.Mutex`. Coordinator's `mu` is SEPARATE from the per-file append `mu` already in `bc.logLocks`. NEVER take `bc.logsMu` from inside the coordinator (D-09).
- Adaptive bypass (D-06): if `len(pending) == 0` at arrival AND no timer pending, fire `f.Sync()` inline without arming the timer. Single-writer zero-latency contract.
- Synchronous durability (D-08): caller MUST block on the returned channel; never return before `f.Sync()` completes.
- `const groupCommitWindow = 1 * time.Millisecond` lives here (D-22c — no config knob in Phase 19).
- `raceEnabled` constant idiom (CONTEXT.md "Claude's discretion"): bench file should mirror Phase 11's `raceEnabled` pattern to skip under `-race`.

---

### `pkg/blockstore/local/fs/rollup.go` (MODIFIED — Opt 1 wire-in)

**Analog:** self — wrap the existing `StoreChunk` call at line 328.

**Existing chunker emit loop** (rollup.go:312-337):
```go
ck := chunker.NewChunker()
pos := minOff
var blocks []blockstore.BlockRef
for pos < uint64(len(stream)) {
	b, _ := ck.Next(stream[pos:], true)
	if b <= 0 { break }
	chunkBytes := stream[pos : pos+uint64(b)]
	h := blake3ContentHash(chunkBytes)
	if err := bc.StoreChunk(ctx, h, chunkBytes); err != nil {
		return fmt.Errorf("rollup: StoreChunk: %w", err)
	}
	blocks = append(blocks, blockstore.BlockRef{
		Hash: h, Offset: pos, Size: uint32(b),
	})
	pos += uint64(b)
}
```

**Pattern notes / invariants:**
- Insert the LRU check between `h := blake3ContentHash(chunkBytes)` and `bc.StoreChunk(...)`.
- On LRU hit → call `fileBlockStore.AddRef(ctx, h, payloadID, BlockRef{...})`. On `ErrUnknownHash` (D-04 sentinel) fall through to the existing `StoreChunk` + standard Put path.
- The `blocks` slice append happens UNCONDITIONALLY (hit or miss) — the BlockRef must be in the manifest either way for the `ComputeObjectID` invariant at line 373.
- STATE-01..03 preservation (D-27): `AddRef` does NOT create a block row; no Pending→Syncing→Remote transition. Logged at Debug only.
- After successful `StoreChunk`, populate the LRU: `bc.dedupLRU.Put(h, payloadID)`. This is the "first-write seeds the LRU" path.

---

### `pkg/blockstore/local/fs/appendwrite.go` (MODIFIED — Opt 2 wire-in)

**Analog:** self — wrap the `lf.f.Sync()` call at line 259.

**Existing fsync site** (appendwrite.go:227-263):
```go
n, err := writeRecord(lf.f, offset, data)
if err != nil {
	// ... FIX-2 / FIX-20 recovery posture preserved ...
}
if err := lf.f.Sync(); err != nil {
	return fmt.Errorf("log fsync: %w", err)
}
bc.logBytesTotal.Add(int64(n))
tree.Insert(offset, uint32(len(data)), time.Now())
```

**Pattern notes / invariants:**
- Replace `lf.f.Sync()` with `lf.groupCommit.Sync(ctx)` (or equivalent — name per planner taste).
- The per-file `mu` (`bc.logLocks[payloadID]`) is STILL held at the sync site. The groupCommit coordinator's internal `mu` is a DIFFERENT mutex — the coordinator may release the outer per-file `mu` if D-08 backpressure semantics require it; document the rationale in the planner's plan if so.
- FIX-2 lock-order discipline (per-file `mu` BEFORE `bc.logsMu`) MUST hold across the new coordinator code.
- On `ctx.Err()` mid-wait: caller returns ctx error; the in-flight fsync MUST still complete (durability contract — D-08). Other batched writers wait on the actual fsync result, not on the canceled context.
- TRANSITIONAL-NEXT-MILESTONE marker (D-26) for O_DIRECT goes near this site post-modification.

---

### `pkg/blockstore/local/fs/chunkstore.go` (MODIFIED — Opt 3 wire-in)

**Analog:** self — extend the `lruTouch` invocation at line 104.

**Existing post-disk-store touch** (chunkstore.go:100-105):
```go
bc.diskUsed.Add(int64(len(data)))
// LSL-08: register the chunk with the in-process LRU so eviction can
// reach it. This is the canonical post-write touch — readers use a
// separate ReadChunk wiring to promote on cache hits.
bc.lruTouch(h, int64(len(data)), path)
return nil
```

**ObjectIDPersister invocation idiom (the model for OnChunkComplete callback safety)** (rollup.go:373-381):
```go
objectID := blockstore.ComputeObjectID(blocks)
bc.persisterMu.RLock()
persister := bc.objectIDPersister
bc.persisterMu.RUnlock()
if persister != nil {
	if err := persister(ctx, payloadID, blocks, objectID); err != nil {
		return fmt.Errorf("rollup: ObjectIDPersister: %w", err)
	}
}
```

**Pattern notes / invariants:**
- D-12: callback is invoked exactly once per successful `lruTouch`, post-disk-store, lock-held. `nil` callback → silent no-op (matches `objectIDPersister` shape).
- "Claude's discretion": choose whether to fire callback inside `lruTouch`'s `lruMu` lock or just outside. The MS-SMB equivalent of "do not widen the hot lock window" applies — call AFTER `bc.lruMu.Unlock()` if `Cache.Put` is the typical consumer (Cache.Put takes its own lock and would re-enter unrelated state).
- The callback signature (D-10): `func(hash ContentHash, data []byte, path string)`. Pass `data` slice as-is — `engine.Cache.Put` heap-copies internally (cache.go:237).
- Lifecycle: callback installed via `FSStoreOptions.OnChunkComplete`; settable via setter (mirror `SetObjectIDPersister` at fs.go:639 if engine construction order requires post-hoc installation).
- TRANSITIONAL-NEXT-MILESTONE markers (D-26) for "pinned hot-tail RAM" and "zstd compression" go near `StoreChunk` post-modification.

---

### `pkg/blockstore/local/fs/fs.go` (MODIFIED — option wiring + LRU instantiation)

**Analog:** `FSStoreOptions.SyncedHashStore` (Phase 18 D-02) — direct precedent for `OnChunkComplete` (Opt 3 D-10).

**Excerpt — option-field shape** (fs.go:545-557):
```go
// SyncedHashStore persists per-CAS-hash local→remote sync state.
// Required when a remote store is configured (the engine's Syncer
// consumes it via ListUnsynced + MarkSynced). Nil is accepted for
// local-only stores; in that case ListUnsynced yields nothing.
SyncedHashStore metadata.SyncedHashStore
// ObjectIDPersister is the rollup-completion hook that receives the
// BlockRef manifest + computed ObjectID after SetRollupOffset
// succeeds. Wire this to the engine coordinator's PersistFileBlocks
// so local-only and remote-backed shares both materialize ObjectIDs
// at rollup time. Nil is accepted: ObjectID is still computed, but
// the persist call is skipped (local-only fixtures / no-engine
// fixtures).
ObjectIDPersister ObjectIDPersister
```

**Option assignment in constructor** (fs.go:609-613):
```go
bc.rollupStore = opts.RollupStore
bc.syncedHashStore = opts.SyncedHashStore
bc.objectIDPersister = opts.ObjectIDPersister
```

**Pattern notes / invariants:**
- Add `OnChunkComplete func(hash blockstore.ContentHash, data []byte, path string)` to `FSStoreOptions`. Document nil-safety in the godoc (copy the "Nil is accepted" phrasing).
- Add `DedupLRUSize int` to `FSStoreOptions` (default 4096 when zero — applied via the existing `if opts.X > 0` idiom at fs.go:597-616).
- Instantiate the dedup LRU on the `FSStore` struct alongside `lruIndex`/`lruList`/`lruMu` (around fs.go:80-100 — read those lines if not already in context to pick the matching naming convention).
- Field name suggestion: `bc.dedupLRU *dedupLRU`. Constructed in `newFSStoreWithOptionsInternal`.

---

### `pkg/blockstore/engine/engine.go` (MODIFIED — Opt 4 hook + Opt 3 callback wiring)

**Analog:** self — extend the existing pre-rollup hook at line 656-680.

**Existing pre-rollup hook** (engine.go:656-680):
```go
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	// File-level dedup pre-hook: if a fully-quiesced manifest matches
	// an already-stored ObjectID, skip the upload pump entirely.
	if bs.coordinator != nil {
		specBlocks, blockStates, err := bs.syncer.snapshotPendingBlockRefs(ctx, payloadID)
		if err != nil { return nil, fmt.Errorf("snapshot pending blockrefs: %w", err) }
		if len(specBlocks) > 0 {
			fileObjectID, err := bs.coordinator.GetFileObjectID(ctx, payloadID)
			if err != nil { return nil, fmt.Errorf("get file objectID: %w", err) }
			hit, err := bs.syncer.trySpeculativeFileLevelDedup(ctx, payloadID, specBlocks, fileObjectID, blockStates)
			if err != nil { return nil, fmt.Errorf("file-level dedup: %w", err) }
			if hit { return &blockstore.FlushResult{Finalized: true}, nil }
		}
	}
	return bs.syncer.Flush(ctx, payloadID)
}
```

**Pattern notes / invariants:**
- Opt 4 (eager small-file dedup) sits BEFORE `trySpeculativeFileLevelDedup` (D-14). It runs ONLY when the in-RAM buffered content for `payloadID` is ≤ `chunker.MinChunkSize`. On hit → return `&blockstore.FlushResult{Finalized: true}, nil` (same shape as the existing file-level dedup hit).
- The Opt 4 short-circuit MUST also call `DeleteAppendLog` (no log was written for in-RAM-only single-block files — but if a partial append happened, clean it up). Mirror `applyFileLevelDedupHit`'s D-11 step.
- Opt 3 callback wiring: when `bs` constructs the `FSStore`, pass `OnChunkComplete: func(h, data, path) { bs.cache.Put(h, data) }` via `FSStoreOptions`. The `bs.cache` field is a `CacheInterface` (cache.go:33) — `Put` is nil-safe and large-data-safe (cache.go:229-235).

---

### `pkg/blockstore/engine/dedup.go` (MODIFIED — Opt 4 eager small-file dedup implementation)

**Analog:** self — `trySpeculativeFileLevelDedup` at line 42-78.

**Sibling fast-path shape** (dedup.go:42-78, abbreviated):
```go
func (m *Syncer) trySpeculativeFileLevelDedup(
	ctx context.Context,
	payloadID string,
	speculativeBlocks []blockstore.BlockRef,
	fileObjectID blockstore.ObjectID,
	blockStates []blockstore.BlockState,
) (hit bool, err error) {
	if m.coordinator == nil { return false, nil }
	if len(speculativeBlocks) == 0 { return false, nil }
	if !fileObjectID.IsZero() { return false, nil }
	for _, st := range blockStates {
		if st != blockstore.BlockStatePending { return false, nil }
	}
	provisional := blockstore.ComputeObjectID(speculativeBlocks)
	targetBlocks, err := m.coordinator.FindByObjectID(ctx, provisional)
	if err != nil { return false, err }
	if targetBlocks == nil { return false, nil }
	return m.applyFileLevelDedupHit(ctx, payloadID, speculativeBlocks, targetBlocks, provisional, false)
}
```

**Pattern notes / invariants:**
- New function: `tryEagerSmallFileDedup(ctx, payloadID, data []byte) (hit bool, err error)`. Lives in `dedup.go` alongside `trySpeculativeFileLevelDedup`.
- Trigger (D-13): `len(data) <= chunker.MinChunkSize` (= 1 MiB). For data above the threshold, return `(false, nil)` immediately.
- ObjectID computation: a single-block file's ObjectID is computed from a one-element `[]BlockRef{Hash: blake3(data), Offset: 0, Size: len(data)}` — D-14 says "compute trivial ObjectID (= that hash for single-block files)". Verify against `blockstore.ComputeObjectID` semantics in context (it's a Merkle root; for n=1 it may or may not equal the leaf hash — read `pkg/blockstore/objectid.go` to confirm before planning).
- Hit path: invoke the same `applyFileLevelDedupHit` machinery (or a small wrapper around it) — must reuse the D-10/D-11 finalize sequence to keep STATE-01..03 + cache invalidation invariants.
- Cache population on HIT (D-16): `bs.cache.Put(hash, data)` before returning — we already have the bytes in RAM.
- Per-share RAM ceiling (D-15): no explicit semaphore. Document the bound = concurrent `Flush` count × `MinChunkSize`.

---

### `pkg/metadata/file_block_store.go` (NEW or extension of interface block in `pkg/blockstore/store.go`)

**Analog:** `pkg/metadata/rollup_store.go` (canonical interface+godoc shape; lives next to the FileBlockStore type alias).

**Interface+sentinel-error idiom** (rollup_store.go:25-43):
```go
type RollupStore interface {
	// SetRollupOffset atomically advances payloadID's rollup_offset iff
	// newOffset >= the currently-stored offset. Returns the PREVIOUS stored
	// value for observability on success.
	//
	// On monotone violation (newOffset < stored), returns (storedOffset,
	// ErrRollupOffsetRegression); the stored value is unchanged.
	SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (storedOffset uint64, err error)

	GetRollupOffset(ctx context.Context, payloadID string) (uint64, error)
}

var ErrRollupOffsetRegression = errors.New("metadata: rollup offset regression rejected")
```

**Where `AddRef` actually goes** (pkg/blockstore/store.go:78-90):
```go
// IncrementRefCount atomically bumps RefCount for the given
// FileBlock id.
IncrementRefCount(ctx context.Context, id string) error

// DecrementRefCount atomically decrements; returns the new
// count. RefCount=0 marks the block as a GC candidate.
DecrementRefCount(ctx context.Context, id string) (uint32, error)
```

**Pattern notes / invariants:**
- D-22b: `FileBlockStore.AddRef(ctx, hash ContentHash, payloadID string, blockRef BlockRef) error` joins the `FileBlockStore` interface in `pkg/blockstore/store.go` (the actual interface definition — `pkg/metadata/store.go:217` is just a type alias).
- Sentinel `ErrUnknownHash` (or `ErrHashNotFound` per Claude's discretion note) in `pkg/metadata/errors.go`. Same shape as `ErrFileBlockNotFound` (already in `pkg/metadata/errors.go`).
- Godoc MUST call out:
  - Returns `ErrUnknownHash` if hash not yet in metadata (D-04 — caller falls back to full `Put`).
  - On success: increments `RefCount` only; `BlockState` UNCHANGED (D-27 — STATE-01..03 preservation).
  - Atomicity: matches `IncrementRefCount`'s atomicity contract.
  - Multi-row-per-hash tolerance (Phase 11 IN-3-02): `AddRef` MAY operate on any one matching row.
- META-03 expansion: comment block at `pkg/blockstore/store.go:10-11` ("Narrowed to 6 methods in Phase 12") MUST be updated to 7 methods.

---

### `pkg/metadata/store/memory/objects.go` (MODIFIED — memory `AddRef`)

**Analog:** self — `incrementRefCountLocked` at line 275-285.

**Excerpt** (objects.go:275-285):
```go
func (s *MemoryMetadataStore) incrementRefCountLocked(_ context.Context, id string) error {
	if s.fileBlockData == nil {
		return metadata.ErrFileBlockNotFound
	}
	block, ok := s.fileBlockData.blocks[id]
	if !ok {
		return metadata.ErrFileBlockNotFound
	}
	block.RefCount++
	return nil
}
```

**Hash-keyed lookup precedent** (objects.go:99-103):
```go
func (s *MemoryMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	s.mu.RLock(); defer s.mu.RUnlock()
	return s.findFileBlockByHashLocked(ctx, hash)
}
```

**Pattern notes / invariants:**
- `AddRef`: `s.mu.Lock(); defer s.mu.Unlock(); id := s.fileBlockData.hashIndex[hash]; if id == "" { return ErrUnknownHash }; ... ; s.fileBlockData.blocks[id].RefCount++; return nil`. Single atomic mutation under `s.mu` Write lock — no TOCTOU.
- Mirror the transaction-aware variant (objects.go:203 `func (tx *memoryTransaction) IncrementRefCount`). Memory's documented best-effort txn semantics apply (see `testTx_IncrementRefCount_RollsBack`'s `bestEffortTxn` branch).
- The hash→id map (`fileBlockData.hashIndex`) may not contain the hash if no finalized block exists — return `ErrUnknownHash`.

---

### `pkg/metadata/store/badger/objects.go` (MODIFIED — badger `AddRef`)

**Analog:** self — `IncrementRefCount` at line 163 and the txn variant at line 530.

**Excerpt** (objects.go:163-198, abbreviated):
```go
func (s *BadgerMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(fileBlockKey(id)))
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		// ... unmarshal, block.RefCount++, marshal, txn.Set ...
	})
}
```

**Pattern notes / invariants:**
- `AddRef`: open a single `db.Update` txn. Look up the hash→id mapping (badger uses a secondary index key — read objects.go around the existing `GetByHash` to find the exact key prefix). If not found → `ErrUnknownHash`. Otherwise: `Get(fileBlockKey(id))`, unmarshal, `RefCount++`, marshal, `Set`. One-shot atomic.
- Don't drop into the txn-level variant (objects.go:530) unless the planner introduces a `tx.AddRef` requirement — the conformance suite only requires the top-level method per D-21.
- Idempotency-on-collision: if the hash's secondary index points at a row that's already in `Remote` state, the `RefCount++` is still valid (D-27 — state unchanged).

---

### `pkg/metadata/store/postgres/objects.go` (MODIFIED — postgres `AddRef`)

**Analog:** self — existing `IncrementRefCount` UPDATE pattern.

**Pattern notes / invariants:**
- `AddRef`: a single SQL statement
  `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = $1 RETURNING id` (or equivalent — read the existing `IncrementRefCount` SQL in postgres/objects.go to match the project's SQL idiom and table column names).
- Use `RowsAffected()` (or RETURNING) to detect zero rows → return `ErrUnknownHash`.
- The non-UNIQUE partial index on `hash` (per `pkg/blockstore/store.go:53` migration 000011) means the UPDATE may affect multiple rows. D-22b accepts this: "Backends MUST tolerate this without erroring." Constrain with `WHERE hash = $1 LIMIT 1` if your planner prefers single-row semantics — document the choice.
- Wrap in the same `WithContext(ctx)` pattern already used by `IncrementRefCount`.

---

### `pkg/metadata/storetest/file_block_ops.go` (MODIFIED — `AddRef` conformance scenarios)

**Analog:** `pkg/metadata/rollup_store_suite.go` (canonical conformance suite shape) + self (`testTx_IncrementRefCount_RollsBack` at line 953-1025).

**Suite invocation shape** (rollup_store_suite.go:33-44):
```go
func RunRollupStoreSuite(t *testing.T, rs RollupStore) {
	t.Helper()
	t.Run("GetBeforeSet", func(t *testing.T) {
		got, err := rs.GetRollupOffset(context.Background(), "suite-get-before-set")
		// ...
	})
	t.Run("SetGet", func(t *testing.T) { ... })
	t.Run("IsolationBetweenPayloadIDs", func(t *testing.T) { ... })
	t.Run("SetRollupOffset_RejectsRegression_KeepsPriorValue", func(t *testing.T) { ... })
	t.Run("ConcurrentMonotone", func(t *testing.T) { ... })
}
```

**Existing concurrent-rollback conformance pattern** (file_block_ops.go:953-1025) — already loaded above.

**Pattern notes / invariants:**
- New `t.Run` subtests inside the existing `runFileBlockOpsTests` (file_block_ops.go:39):
  - `AddRef_ExistingHash_BumpsRefCount` — seed a block at RefCount=1, AddRef, assert RefCount=2 and `BlockState` unchanged (D-27).
  - `AddRef_MissingHash_ReturnsErrUnknownHash` — call AddRef on a hash with no seeded block, assert `errors.Is(err, metadata.ErrUnknownHash)` and no row was created (post-assertion: `GetByHash` still returns `nil, nil`).
  - `AddRef_Concurrent_With_DecrementRefCountCascade` — seed RefCount=1, spawn N goroutines: half AddRef, half DecrementRefCount; assert final RefCount ≥ 0 and the row is not orphan-state-dropped. Mirror `ConcurrentMonotone` from rollup_store_suite.go:153.
  - Per D-21: all three scenarios run for each backend (badger/postgres/memory) via the existing `factory(t)` injection.
- Best-effort txn carve-out (memory) — D-21 doesn't require txn coverage for `AddRef` (D-04 said no TOCTOU race), so the `bestEffortTxn` branch is NOT needed here.
- Subtest `t.Helper()` discipline preserved.

---

### `pkg/config/config.go` (MODIFIED — `blockstore.local.dedup_lru_size` knob)

**Analog:** `SyncerConfig` at line 88-109 + `ApplyDefaults`/`Validate` pair at 112-142.

**Excerpt — config struct + defaults + validate triplet** (config.go:88-142):
```go
type SyncerConfig struct {
	// ClaimBatchSize is the maximum number of Pending blocks the syncer
	// flips to Syncing in a single metadata transaction per claim cycle (D-13).
	// Default: 32.
	ClaimBatchSize int `mapstructure:"claim_batch_size" yaml:"claim_batch_size"`
	// ...
}

func (c *SyncerConfig) ApplyDefaults() {
	if c.ClaimBatchSize <= 0 { c.ClaimBatchSize = 32 }
	// ...
}

func (c *SyncerConfig) Validate() error {
	if c.ClaimBatchSize <= 0 {
		return fmt.Errorf("syncer.claim_batch_size must be > 0 (got %d)", c.ClaimBatchSize)
	}
	// ...
	return nil
}
```

**Top-level field registration** (config.go:73):
```go
Syncer SyncerConfig `mapstructure:"syncer" yaml:"syncer"`
```

**Pattern notes / invariants:**
- D-22c: new section `BlockstoreLocalConfig` with `DedupLRUSize int` field, `mapstructure:"dedup_lru_size" yaml:"dedup_lru_size"`. The parent `BlockstoreConfig` shape:
  ```go
  type BlockstoreConfig struct {
      Local BlockstoreLocalConfig `mapstructure:"local" yaml:"local"`
  }
  ```
  This is the FIRST `blockstore.*` config section in `pkg/config/` — no prior precedent. Planner may place it in `pkg/config/blockstore.go` (new file) per CONTEXT.md "Files to touch" or extend `config.go`. CONTEXT names `pkg/config/blockstore.go` explicitly.
- `ApplyDefaults`: `if c.DedupLRUSize <= 0 { c.DedupLRUSize = 4096 }`.
- `Validate`: `if c.DedupLRUSize <= 0 { return fmt.Errorf("blockstore.local.dedup_lru_size must be > 0 (got %d)", ...) }`.
- Wire into `defaults.go:19`'s `ApplyDefaults(cfg *Config)` umbrella: `cfg.Blockstore.Local.ApplyDefaults()` next to `cfg.Syncer.ApplyDefaults()`.
- Env var DITTOFS_BLOCKSTORE_LOCAL_DEDUP_LRU_SIZE picked up by the existing viper unmarshal (no extra wiring required — viper handles dotted-path automatically).

---

### `pkg/blockstore/engine/cache.go` (consumer of `OnChunkComplete` — read-only reference)

**No code changes.** Read-only reference: the Opt 3 callback target.

**Cache.Put semantics** (cache.go:229-258):
```go
func (c *Cache) Put(hash blockstore.ContentHash, data []byte) {
	if c == nil || c.closed.Load() { return }
	if int64(len(data)) > c.maxBytes { return }
	heapCopy := make([]byte, len(data))
	copy(heapCopy, data)
	c.mu.Lock(); defer c.mu.Unlock()
	// ... insert or update + evictLocked ...
}
```

**Pattern notes / invariants:**
- D-11: Cache already enforces bounded LRU; no extra cap on `OnChunkComplete`.
- D-12 callback safety: `Cache.Put` is nil-safe (`c == nil` guard), closed-safe (`c.closed` guard), and large-data-safe (`> c.maxBytes` skip). Wiring `OnChunkComplete = bs.cache.Put` is the canonical safe binding.
- Heap-copy on insert (cache.go:237) means the caller (`chunkstore.lruTouch`) does NOT need to clone the `data` slice before invoking the callback.

---

### `pkg/blockstore/doc.go` (MODIFIED — TRANSITIONAL-NEXT-MILESTONE markers per D-25/D-26)

**Analog:** self — line 185-205.

**Excerpt** (doc.go:183-205, paraphrased per the grep result):
```go
//	TRANSITIONAL-PHASE-N:         scheduled deletion in milestone N
//	TRANSITIONAL-NEXT-MILESTONE:  deletion scheduled for the next
//	                              milestone
//
// the goal is for `grep -rn 'TRANSITIONAL-' ./pkg/blockstore` to ...
```

**Pattern notes / invariants:**
- D-25: grep + audit every `TRANSITIONAL-NEXT-MILESTONE:` marker in `pkg/blockstore/`. For each: either resolve in this PR (delete the marker alongside the change) or rename to `TRANSITIONAL-V0.17:` (concrete next-milestone tag) and update godoc comment.
- D-26: add new `TRANSITIONAL-NEXT-MILESTONE:` markers at the five sites (chunkstore.go pinned-hot-tail, appendlog.go tmpfs spill, appendwrite.go O_DIRECT, chunkstore.go StoreChunk zstd, cache.go cold-cache prefetch).
- D-23: update the milestone tag from `Phase 18 / claim_batch_size deprecation` to "Closed in Phase 19" wherever doc.go references the ClaimBatchSize deprecation cycle.

---

### `SyncerConfig.ClaimBatchSize` (DELETED — D-23)

**No analog needed — pure deletion.**

**Pattern notes / invariants:**
- Single head-of-PR cleanup commit (D-23). Delete: `SyncerConfig.ClaimBatchSize` field (config.go:92), its `ApplyDefaults` branch (config.go:113), its `Validate` branch (config.go:131-139), the `UploadConcurrency <= ClaimBatchSize` cross-validation, and any `*_test.go` line that exercises the field. Update `applyDefaults` test snapshots.
- The mapstructure/yaml tag `"claim_batch_size"` becomes an unknown key. Viper silently ignores unknown keys by default — verify with `pkg/config/init_test.go` that no test explicitly asserts the key's presence in a parsed yaml.
- TRANSITIONAL marker referencing the cycle (per doc.go) deletes alongside.

---

## Shared Patterns

### Surface injection via `FSStoreOptions` (Phase 18 D-02 precedent)
**Source:** `pkg/blockstore/local/fs/fs.go:537-563` (FSStoreOptions block)
**Apply to:** `OnChunkComplete` (Opt 3), `DedupLRUSize` (Opt 1)

```go
type FSStoreOptions struct {
	// ... existing ...
	SyncedHashStore metadata.SyncedHashStore
	ObjectIDPersister ObjectIDPersister
	// (new in Phase 19)
	// OnChunkComplete is invoked once per successful chunkstore.lruTouch
	// (post-disk-store), with the chunk's content hash, bytes, and on-disk
	// path. Wire to engine.Cache.Put to populate the read cache at write
	// time (Opt 3). Nil is accepted: chunkstore behaves identically to
	// pre-Phase-19 if the callback is absent.
	OnChunkComplete func(hash blockstore.ContentHash, data []byte, path string)
	// DedupLRUSize is the slot count for the in-memory hash dedup LRU
	// (Opt 1). Default 4096 when zero.
	DedupLRUSize int
}
```

### Sentinel error pattern
**Source:** `pkg/metadata/rollup_store.go:42` (`ErrRollupOffsetRegression`) + `pkg/metadata/errors.go` (`ErrFileBlockNotFound`)
**Apply to:** `ErrUnknownHash` (D-04 / D-22b)

```go
var ErrUnknownHash = errors.New("metadata: hash not yet present in FileBlockStore (AddRef called before Put)")
```

### Conformance suite invocation
**Source:** `pkg/metadata/rollup_store_suite.go:33` + `pkg/metadata/storetest/file_block_ops.go:39-100`
**Apply to:** All three backends' `AddRef` test files (D-21).

Each backend's `objects_test.go` already calls `runFileBlockOpsTests(t, factoryFn)` via the storetest harness; no per-backend invocation file change needed — the new `t.Run("AddRef_*")` subtests added inside `runFileBlockOpsTests` flow to every backend automatically.

### Lock-order discipline in `appendwrite.go`
**Source:** `pkg/blockstore/local/fs/appendwrite.go:232-250` (FIX-2 / FIX-20 godoc block)
**Apply to:** `groupcommit.go` (Opt 2). The coordinator's internal `mu` is the THIRD lock in the discipline; document it as:
- per-file `mu` (already in `bc.logLocks`) acquired before
- groupCommit.mu (new) acquired before
- `bc.logsMu` (existing) acquired last

Never invert. Never hold `bc.logsMu` while waiting on groupCommit.mu.

### Atomic-pointer config snapshot (for hot-path options)
**Source:** `pkg/blockstore/local/fs/fs.go:651-654` (`SetRetentionPolicy` using `atomic.StorePointer`)
**Apply to:** N/A in Phase 19 — `OnChunkComplete` is install-once at construction time and does not need live atomic swap. Documented here so the planner doesn't over-engineer.

---

## No Analog Found

None. Every Phase 19 surface has a strong existing precedent.

The one near-miss is `groupcommit.go`: the timer-armed fan-in idiom doesn't exist verbatim in-tree. The closest functional precedent is the rollup channel nudge (`bc.rollupCh <- payloadID`) in appendwrite.go:268 — but that's a fire-and-forget signal, not a synchronous fan-in. Planner must invent the timer arming logic, but the surrounding lock discipline + nil-safety + per-file scope are all pinned by existing in-package patterns.

---

## Metadata

**Analog search scope:** `pkg/blockstore/`, `pkg/metadata/`, `pkg/config/`, `pkg/chunker/`, `internal/bench/`
**Files scanned:** ~25 source files read in part or whole
**Files Read in full:** `pkg/metadata/rollup_store.go`, `pkg/metadata/rollup_store_suite.go`, `pkg/metadata/synced_hash_store.go`, `pkg/blockstore/local/fs/fdpool.go` (relevant range)
**Pattern extraction date:** 2026-05-21

# Phase 18: Syncer simplification + ObjectID relocation - Pattern Map

**Mapped:** 2026-05-21
**Files analyzed:** 13 files to create + ~10 files to modify + ~12 test files to retarget/delete
**Analogs found:** 12 / 13 (one item — `iter.Seq2[ContentHash, error]` — has no in-tree analog; pattern documented from `BlockStore.Walk` shape + Go 1.23 stdlib idiom)

## File Classification

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|-------------------|------|-----------|----------------|---------------|
| `pkg/metadata/synced_hash_store.go` | NEW interface | metadata kv-by-hash | `pkg/metadata/rollup_store.go` | exact (structural clone) |
| `pkg/metadata/synced_hash_store_suite.go` | NEW conformance suite | test scaffolding | `pkg/metadata/rollup_store_suite.go` | exact |
| `pkg/metadata/store/memory/synced_hash_store.go` | NEW backend impl | mutex-guarded map | `pkg/metadata/store/memory/rollup.go` | exact |
| `pkg/metadata/store/memory/synced_hash_store_test.go` | NEW backend test | suite-runner | `pkg/metadata/store/memory/rollup_test.go` | exact |
| `pkg/metadata/store/badger/synced_hash_store.go` | NEW backend impl | badger key-prefix | `pkg/metadata/store/badger/rollup.go` | exact |
| `pkg/metadata/store/badger/synced_hash_store_test.go` | NEW backend test | suite-runner | `pkg/metadata/store/badger/rollup_test.go` | exact |
| `pkg/metadata/store/postgres/synced_hash_store.go` | NEW backend impl | pgx CRUD | `pkg/metadata/store/postgres/rollup.go` | exact |
| `pkg/metadata/store/postgres/synced_hash_store_test.go` | NEW backend test | integration-tagged | `pkg/metadata/store/postgres/rollup_test.go` | exact |
| `pkg/metadata/store/postgres/migrations/000015_synced_hashes.{up,down}.sql` | NEW migration | DDL | `…/migrations/000009_rollup_offsets.up.sql` | exact |
| `pkg/blockstore/local/local.go` | MODIFIED interface | interface delta | self (delete 7 methods + add ListUnsynced) | self-pattern |
| `pkg/blockstore/local/fs/fs.go` | MODIFIED constructor | injection slot | self (RollupStore slot at line 510–513, plumbed at line 565) | self-pattern |
| `pkg/blockstore/local/fs/blockstore_methods.go` | MODIFIED — add ListUnsynced impl | iter.Seq2 stream | self `Walk` (lines 118–187) + new IsSynced filter | role-match |
| `pkg/blockstore/local/fs/rollup.go` | MODIFIED — ObjectID hook | rollup post-commit | self `rollupFile` lines 337–349 (insertion point right AFTER `SetRollupOffset` returns nil) | self-pattern |
| `pkg/blockstore/engine/syncer.go` | MODIFIED — Flush body rewrite | mirror loop | new pattern (no in-tree analog of this shape); auxiliary struct/state preserved verbatim | self-pattern (struct kept) |
| `pkg/blockstore/engine/upload.go` | MODIFIED — delete BLAKE3 recompute | deletion | `uploadOne` lines 74–151 → delete BLAKE3 path | self-pattern |
| `pkg/blockstore/engine/engine.go` | MODIFIED — Flush pre-rollup hook + Delete refcount cascade | request-response | self `Delete` (lines 416–464) — extend with `DeleteSynced` cascade | self-pattern |
| `pkg/blockstore/engine/dedup.go` | MODIFIED — keep private trySpec…, drop public wrapper | code move | self lines 42–78 (private already exists) | self-pattern |
| `pkg/blockstore/engine/syncer_test.go` | RE-CREATED — integration | integration test | (none in tree — see Phase 17 17-VERIFICATION.md deferred follow-up) | no-analog |
| `pkg/blockstore/doc.go` | MODIFIED — add `TRANSITIONAL-NEXT-MILESTONE` doc | docs | self (existing `TRANSITIONAL-PHASE-18:` mention pattern) | self-pattern |

## Pattern Assignments

### 1. `pkg/metadata/synced_hash_store.go` (NEW interface)

**Analog:** `pkg/metadata/rollup_store.go` (43 lines) — `RollupStore` interface.

**Header / godoc tone** (lines 1–7):
```go
// Package metadata — rollup_store.go (Phase 10).
//
// RollupStore persists the per-file append-log rollup_offset for the hybrid
// local tier. See pkg/blockstore/local/fs/rollup.go for the consumer; see
// .planning/phases/10-fastcdc-chunker-hybrid-local-store-a1/10-CONTEXT.md D-12
// for the atomicity contract.
package metadata
```

**Convention to mirror (NO Phase/D-NN in source per project rule):** the rollup file *does* reference `.planning/phases/...` paths and `D-12` markers in its package-level godoc — that is borderline. Project rule `feedback_no_phase_comments_in_code.md` says phase/decision IDs stay in `.planning/` only; **the new `synced_hash_store.go` MUST NOT mention "Phase 18" or "D-02" in its godoc**. Use neutral domain language ("local→remote sync state for CAS chunks") and let git blame + commit messages carry provenance.

**Interface shape to copy** (lines 25–37 of rollup_store.go):
```go
type RollupStore interface {
    // SetRollupOffset atomically advances ...
    SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (storedOffset uint64, err error)

    // GetRollupOffset returns the persisted rollup_offset for payloadID.
    // Returns (0, nil) if not set (a fresh file is treated as rolled-up-to-0).
    GetRollupOffset(ctx context.Context, payloadID string) (uint64, error)
}
```

**Target shape for `SyncedHashStore` (3 methods per D-02):**
```go
type SyncedHashStore interface {
    // IsSynced reports whether hash has been successfully mirrored to
    // the remote store at least once. Returns (false, nil) when no
    // entry exists.
    IsSynced(ctx context.Context, hash blockstore.ContentHash) (bool, error)

    // MarkSynced records that hash has been mirrored to remote. Idempotent:
    // re-applying the same hash is a no-op.
    MarkSynced(ctx context.Context, hash blockstore.ContentHash) error

    // DeleteSynced removes the synced marker for hash. Idempotent: deleting
    // an absent hash returns nil.
    DeleteSynced(ctx context.Context, hash blockstore.ContentHash) error
}
```

**Sentinel pattern** (lines 39–42 of rollup_store.go): rollup_store ships one `ErrRollupOffsetRegression` sentinel. SyncedHashStore needs **no sentinel** — all three methods are idempotent by design (D-07/D-09). No error wrapping required at this layer.

---

### 2. `pkg/metadata/synced_hash_store_suite.go` (NEW conformance suite)

**Analog:** `pkg/metadata/rollup_store_suite.go` (192 lines).

**Suite-function signature to copy** (lines 33–34):
```go
func RunRollupStoreSuite(t *testing.T, rs RollupStore) {
    t.Helper()
```

**Target:**
```go
func RunSyncedHashStoreSuite(t *testing.T, s SyncedHashStore) {
    t.Helper()
```

**Suite-isolation rule to copy** (lines 27–32 godoc): each subtest uses a freshly-hashed value so subtests on a shared store instance do not collide. For `SyncedHashStore` use distinct `ContentHash` literals (e.g., `mustHash("suite-iso-a")`, derived via `blake3.Sum256([]byte("..."))`).

**Subtest table to mirror (one for one):**

| RollupStore subtest | SyncedHashStore equivalent |
|---|---|
| `GetBeforeSet` | `IsSyncedBeforeMark` — `IsSynced(unset) == (false, nil)` |
| `SetGet` | `MarkThenIsSynced` — Mark → IsSynced true; second Mark idempotent |
| `IsolationBetweenPayloadIDs` | `IsolationBetweenHashes` — two distinct hashes, independent state |
| `SetRollupOffset_RejectsRegression_KeepsPriorValue` | **N/A** — SyncedHashStore has no monotone invariant; **DROP** |
| `SetRollupOffset_AllowsEqualValue` | `MarkIdempotent` — re-Mark returns nil, still IsSynced |
| `ConcurrentMonotone` | `DeleteSyncedAfterMark` + `ConcurrentMarkAndDelete` — concurrent Mark/Delete on same hash never panic; final state coherent (deterministic terminal: whichever op wrote last) |

**Concurrent stress block pattern** (lines 153–190): copy the `goroutines = 16` + `sync.WaitGroup` scaffolding verbatim, substituting MarkSynced/DeleteSynced for SetRollupOffset.

---

### 3. `pkg/metadata/store/memory/synced_hash_store.go` (NEW memory backend)

**Analog:** `pkg/metadata/store/memory/rollup.go` (47 lines).

**Compile-time assertion + receiver type** (lines 9–10):
```go
// Compile-time assertion: the memory engine implements RollupStore.
var _ metadata.RollupStore = (*MemoryMetadataStore)(nil)
```

**Target:** add a sibling `var _ metadata.SyncedHashStore = (*MemoryMetadataStore)(nil)` and grow the `MemoryMetadataStore` struct with `syncedMu sync.RWMutex` + `synced map[blockstore.ContentHash]time.Time` (or `struct{}` — D-02 says value can be empty; `time.Time` reserved for future Stats/Count per `<deferred>`).

**Method body skeleton to copy** (lines 19–35 — SetRollupOffset):
```go
func (s *MemoryMetadataStore) SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (uint64, error) {
    if err := ctx.Err(); err != nil {
        return 0, err
    }
    s.rollupMu.Lock()
    defer s.rollupMu.Unlock()
    if s.rollupOffsets == nil {
        s.rollupOffsets = make(map[string]uint64)
    }
    prev := s.rollupOffsets[payloadID]
    ...
    s.rollupOffsets[payloadID] = newOffset
    return prev, nil
}
```

**Read-side pattern** (lines 39–46): `RLock` + lazy nil-map guard. MarkSynced/DeleteSynced use `Lock`; IsSynced uses `RLock`.

**Where to add struct fields:** find `MemoryMetadataStore` in `pkg/metadata/store/memory/store.go` and add the two fields next to `rollupMu` / `rollupOffsets`.

---

### 4. `pkg/metadata/store/badger/synced_hash_store.go` (NEW badger backend)

**Analog:** `pkg/metadata/store/badger/rollup.go` (135 lines).

**Key-namespace godoc block to mirror** (lines 14–32 — the `// Key Namespace:` ASCII-art comment block). Mirror it:
```go
// Key Namespace:
//   - synced:{32-byte-hash}  empty value (presence == synced)
```

D-02 specifies key prefix `synced/<hex-hash>`. The rollup file uses `ro:{payloadID}` raw bytes — for hashes you should encode the 32-byte raw hash directly (`append([]byte("synced:"), hash[:]...)`) to keep keys compact, OR use hex (`hex.EncodeToString(hash[:])`) for grep-friendliness. **Choose binary-raw to match rollup's `[]byte(rollupOffsetPrefix + payloadID)` style** and document in a key-namespace block.

**Key helper to copy** (lines 39–42):
```go
const rollupOffsetPrefix = "ro:"
func keyRollupOffset(payloadID string) []byte {
    return []byte(rollupOffsetPrefix + payloadID)
}
```

**Target:**
```go
const syncedHashPrefix = "synced:"
func keySyncedHash(hash blockstore.ContentHash) []byte {
    return append([]byte(syncedHashPrefix), hash[:]...)
}
```

**Transaction skeleton to copy** (lines 56–104 — db.Update with retry-aware closure for SetRollupOffset). For MarkSynced this is simpler — single Set with empty value:
```go
err := s.db.Update(func(txn *badger.Txn) error {
    return txn.Set(keySyncedHash(hash), nil)
})
if err != nil {
    return fmt.Errorf("badger synced mark: %w", err)
}
```

**Read-side pattern** (lines 109–135 — db.View + ErrKeyNotFound → not-set):
```go
err := s.db.View(func(txn *badger.Txn) error {
    _, err := txn.Get(keySyncedHash(hash))
    if errors.Is(err, badger.ErrKeyNotFound) {
        return nil // unset → return false
    }
    return err
})
```

**Error-wrap convention to preserve** (line 132): `fmt.Errorf("badger rollup get: %w", err)` — keep the `"badger synced <op>: %w"` shape for parity.

---

### 5. `pkg/metadata/store/postgres/synced_hash_store.go` (NEW postgres backend)

**Analog:** `pkg/metadata/store/postgres/rollup.go` (154 lines).

**Compile-time assertion + module-level godoc block** (lines 14–30) — mirror tone, but note: SyncedHashStore has no INV equivalent to INV-03, so the long "atomic-monotone via WHERE predicate" paragraph collapses to a one-line "idempotent presence marker on `synced_hashes` table; PRIMARY KEY on hash deduplicates".

**Simpler SQL skeleton (NOT a CTE — no monotone invariant):**
```sql
INSERT INTO synced_hashes (hash, synced_at) VALUES ($1, NOW())
ON CONFLICT (hash) DO NOTHING
```

**Method receiver + ctx-check pattern to copy** (lines 61–73):
```go
func (s *PostgresMetadataStore) SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (uint64, error) {
    if err := ctx.Err(); err != nil {
        return 0, err
    }
    ...
}
```

**pgx query helper to use:** `s.queryRow(ctx, sql, args...)` (used at line 103). `s.execContext` or `s.queryRow` exists per the file's local helpers — check `pkg/metadata/store/postgres/pool_helpers.go` for the available helpers; mirror exactly.

**IsSynced read pattern** — mirror lines 137–153 (GetRollupOffset with `pgx.ErrNoRows` → `(false, nil)`):
```go
row := s.queryRow(ctx, `SELECT 1 FROM synced_hashes WHERE hash = $1`, hash[:])
var dummy int
err := row.Scan(&dummy)
if errors.Is(err, pgx.ErrNoRows) {
    return false, nil
}
if err != nil {
    return false, fmt.Errorf("postgres synced get: %w", err)
}
return true, nil
```

**DeleteSynced:** single `DELETE FROM synced_hashes WHERE hash = $1` — idempotent (DELETE returns no error on zero rows).

---

### 6. `pkg/metadata/store/postgres/migrations/000015_synced_hashes.{up,down}.sql` (NEW migration)

**Analog:** `…/migrations/000009_rollup_offsets.up.sql` (27 lines).

**Migration numbering:** current max is `000014_add_share_block_layout` — use `000015_synced_hashes`. Matches the sequential 6-digit padding convention seen in every file under `pkg/metadata/store/postgres/migrations/`.

**Filename convention:** `{NNNNNN}_{snake_case_name}.{up,down}.sql` — verified across all 14 existing migrations.

**Up-SQL skeleton to copy** (lines 21–26):
```sql
CREATE TABLE IF NOT EXISTS rollup_offsets (
    payload_id    TEXT PRIMARY KEY,
    rollup_offset BIGINT NOT NULL,

    CONSTRAINT valid_rollup_offset CHECK (rollup_offset >= 0)
);
```

**Target up-SQL:**
```sql
-- Per-CAS-hash synced marker for the unified local→remote mirror loop.
-- Backs metadata.SyncedHashStore for Postgres.
--
-- The application treats presence-of-row as "this hash has been
-- successfully Put to the remote store at least once". Idempotent INSERT
-- via ON CONFLICT DO NOTHING; idempotent DELETE on refcount=0 cascade.
--
-- Schema is minimal: hash is the primary key (32 bytes BLAKE3-256, raw),
-- synced_at preserved for future observability (see deferred section in
-- 18-CONTEXT.md).

CREATE TABLE IF NOT EXISTS synced_hashes (
    hash       BYTEA PRIMARY KEY,
    synced_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT valid_hash_length CHECK (octet_length(hash) = 32)
);
```

**Down-SQL** (mirror `000009_rollup_offsets.down.sql`):
```sql
DROP TABLE IF EXISTS synced_hashes;
```

**Embed mechanism:** `pkg/metadata/store/postgres/migrations/embed.go` walks the directory automatically — no code change needed once files are added.

---

### 7. `pkg/metadata/store/{memory,badger,postgres}/synced_hash_store_test.go` (NEW conformance test files)

**Analogs:**
- `pkg/metadata/store/memory/rollup_test.go` (163 lines) — unit-tag (default)
- `pkg/metadata/store/badger/rollup_test.go` (33 lines) — unit-tag (default; badger is embedded)
- `pkg/metadata/store/postgres/rollup_test.go` (57 lines) — `//go:build integration`

**Memory test skeleton to copy** (lines 19–22):
```go
func TestMemoryRollupStore_Suite(t *testing.T) {
    s := newRollupTestStore()
    metadata.RunRollupStoreSuite(t, s)
}
```

**Badger test skeleton to copy** (lines 15–32 — `t.TempDir()` + `t.Cleanup(_ = store.Close())`):
```go
func newRollupTestStore(t *testing.T) *BadgerMetadataStore {
    t.Helper()
    dbPath := filepath.Join(t.TempDir(), "metadata.db")
    store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
    ...
    t.Cleanup(func() { _ = store.Close() })
    return store
}
```

**Postgres integration-test header to copy** (lines 1–22):
```go
//go:build integration

package postgres_test

...
func TestPostgresRollupStore_Suite(t *testing.T) {
    if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
        t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL rollup tests")
    }
    ...
}
```

**Note:** postgres test file is in `package postgres_test` (external), the other two are in `package memory` / `package badger` (internal). Preserve that asymmetry — it follows the integration-vs-unit split.

---

### 8. `pkg/blockstore/local/fs/fs.go` — `FSStoreOptions.SyncedHashStore` injection

**Analog:** `RollupStore` injection — the canonical 3-step plumbing pattern.

**Step A — struct field** (lines 201–204):
```go
// --- Phase 10-06 rollup pool (D-13/D-33). ---
rollupStore   metadata.RollupStore
rollupCh      chan string
rollupStarted atomic.Bool
rollupWg      sync.WaitGroup
```

**Target:** add `syncedHashStore metadata.SyncedHashStore` alongside `rollupStore`.

**Step B — `FSStoreOptions` slot** (lines 502–519):
```go
type FSStoreOptions struct {
    MaxLogBytes     int64
    RollupWorkers   int
    StabilizationMS int
    // RollupStore persists per-file rollup_offset (LSL-05). Required
    // when StartRollup will be called. Nil is accepted when the caller
    // will not start the rollup pool.
    RollupStore metadata.RollupStore
    OrphanLogMinAgeSeconds int
}
```

**Target:** add inside `FSStoreOptions`:
```go
// SyncedHashStore persists local→remote sync state per CAS hash.
// Required when a remote store is configured (the engine's Syncer
// consumes it via ListUnsynced + MarkSynced). Nil is accepted for
// local-only stores.
SyncedHashStore metadata.SyncedHashStore
```

**Step C — plumb in constructor** (line 565):
```go
bc.rollupStore = opts.RollupStore
```

**Target:** add `bc.syncedHashStore = opts.SyncedHashStore` directly after.

**Wiring upstream:** `cmd/dfs/commands/start.go` (or wherever `NewWithOptions` is called for per-share construction) must source `SyncedHashStore` from the same metadata-store handle the operator already configured. Pattern matches how `opts.RollupStore` is supplied today — grep `opts.RollupStore = ` to find the caller.

---

### 9. `pkg/blockstore/local/local.go` — interface delta

**Analog:** self (this file). The 7 transitional methods are at lines 154–198 with the explicit `TRANSITIONAL-PHASE-18:` grep marker on each godoc.

**Deletion targets (verified line ranges in `pkg/blockstore/local/local.go`):**

| Line | Symbol | Action |
|---|---|---|
| 18–27 | `FlushedBlock` struct | DELETE |
| 154–161 | `ReadAt` | DELETE |
| 163–166 | `WriteAt` | DELETE |
| 168–172 | `Flush` (returns `[]FlushedBlock`) | DELETE |
| 174–178 | `IsBlockLocal` | DELETE |
| 180–185 | `GetBlockData` | DELETE |
| 187–192 | `WriteFromRemote` | DELETE |
| 194–198 | `DeleteAllBlockFiles` | DELETE |
| 139–153 | `// --- Transitional admin-superset methods ---` doc block | DELETE entirely |

**Addition target — new method on the `LocalStore` interface:**
```go
// ListUnsynced returns a push iterator over every CAS hash present in
// the local store that has not yet been MarkSynced'd in the injected
// SyncedHashStore. Snapshot-at-start semantics: the iterator captures
// the hash set existing at iteration begin; new chunks rolled up after
// that are picked up on the NEXT pass. The yielded error position
// surfaces any per-hash backend error; iteration stops on the first
// non-nil error.
ListUnsynced(ctx context.Context) iter.Seq2[blockstore.ContentHash, error]
```

**Standard library import to add:** `"iter"` (Go 1.23+).

---

### 10. `pkg/blockstore/local/fs/blockstore_methods.go` — `ListUnsynced` implementation

**Analog:** `(*FSStore).Walk` (lines 118–187).

**Walk function shape to study** (lines 123–187):
```go
func (bc *FSStore) Walk(ctx context.Context, fn func(hash blockstore.ContentHash, meta blockstore.Meta) error) error {
    if bc.isClosed() {
        return ErrStoreClosed
    }
    if err := ctx.Err(); err != nil {
        return err
    }
    blocksDir := filepath.Join(bc.baseDir, "blocks")
    if _, err := os.Stat(blocksDir); err != nil {
        if os.IsNotExist(err) {
            return nil // empty store
        }
        return fmt.Errorf("blockstore.fs: Walk: stat: %w", err)
    }
    walkErr := filepath.WalkDir(blocksDir, func(path string, d os.DirEntry, err error) error {
        if err != nil { return err }
        if d.IsDir() { return nil }
        if ctxErr := ctx.Err(); ctxErr != nil {
            return ctxErr
        }
        name := d.Name()
        if len(name) != blockstore.HashSize*2 { return nil }
        raw, hexErr := hex.DecodeString(name)
        if hexErr != nil || len(raw) != blockstore.HashSize { return nil }
        var h blockstore.ContentHash
        copy(h[:], raw)
        ...
        if cbErr := fn(h, meta); cbErr != nil { ... }
        return nil
    })
    ...
}
```

**`ListUnsynced` target shape** (D-04 push iterator, D-05 snapshot semantics):

```go
func (bc *FSStore) ListUnsynced(ctx context.Context) iter.Seq2[blockstore.ContentHash, error] {
    return func(yield func(blockstore.ContentHash, error) bool) {
        // Snapshot-at-start (D-05): materialize the hash list under
        // Walk's directory scan, THEN filter against SyncedHashStore.
        // New chunks rolled up after iteration begins are picked up
        // on the next pass.
        var snapshot []blockstore.ContentHash
        walkErr := bc.Walk(ctx, func(hash blockstore.ContentHash, _ blockstore.Meta) error {
            snapshot = append(snapshot, hash)
            return nil
        })
        if walkErr != nil {
            var zero blockstore.ContentHash
            yield(zero, walkErr)
            return
        }
        for _, h := range snapshot {
            if err := ctx.Err(); err != nil {
                yield(blockstore.ContentHash{}, err)
                return
            }
            // O(1) lookup per D-06.
            synced, err := bc.syncedHashStore.IsSynced(ctx, h)
            if err != nil {
                if !yield(h, fmt.Errorf("synced lookup %s: %w", h, err)) {
                    return
                }
                continue
            }
            if synced {
                continue
            }
            if !yield(h, nil) {
                return
            }
        }
    }
}
```

**Imports to add to file:** `"iter"` (Go 1.23+).

**Nil-check defense:** the godoc on `FSStoreOptions.SyncedHashStore` says nil is accepted for local-only. ListUnsynced must guard against `bc.syncedHashStore == nil` and either return an empty iterator or treat every hash as unsynced (D-09 strict-subset invariant says **empty iterator** is correct when no syncedStore is wired — there is no syncer to drive uploads).

---

### 11. `pkg/blockstore/local/fs/rollup.go` — ObjectID relocation

**Analog:** self — the existing CommitChunks atomic sequence at lines 337–349.

**Insertion point — exact location:** immediately AFTER `SetRollupOffset` returns nil and BEFORE `advanceRollupOffset` (line 353):

```go
// CommitChunks atomic sequence (D-12). SetRollupOffset is atomic-monotone
// at the RollupStore layer: on attempted regression it returns
// ErrRollupOffsetRegression and the stored value is unchanged. We treat
// that as benign (another worker raced ahead) and return nil.
_, err = bc.rollupStore.SetRollupOffset(ctx, payloadID, targetPos)
if errors.Is(err, metadata.ErrRollupOffsetRegression) {
    slog.Debug("rollup: SetRollupOffset regression rejected (benign — another worker advanced past us)",
        "payloadID", payloadID, "target", targetPos)
    return nil
}
if err != nil {
    return fmt.Errorf("rollup: SetRollupOffset: %w", err)
}

// >>> NEW INSERT POINT — ObjectID compute + PersistFileBlocks <<<
// objectID := blockstore.ComputeObjectID(blocks)  -- blocks are the
//   BlockRefs the chunker just produced; need to thread them down from
//   the chunker loop (line 314+ — `for pos < uint64(len(stream))` body
//   currently builds chunks but does NOT accumulate BlockRefs). Add a
//   local []blockstore.BlockRef accumulator inside that loop.
// err := bc.coordinator.PersistFileBlocks(ctx, payloadID, blocks, objectID)
// if err != nil { return fmt.Errorf("rollup: PersistFileBlocks: %w", err) }

// Derived-state: advance the log header. ...
```

**Wiring requirement:** `FSStore` does NOT currently hold a `coordinator` reference (the coordinator lives on `BlockStore` in `pkg/blockstore/engine`). Two paths:
1. **Plumb a `metadata.Coordinator`-like callback through `FSStoreOptions`** (mirrors `RollupStore`/`SyncedHashStore` injection) — clean.
2. **Pass an `ObjectIDPersister func(ctx, payloadID, blocks, objectID) error` callback** — narrower interface, no engine import.

CONTEXT D-10 says "coordinator.PersistFileBlocks(...)" so option 1 is canonical; planner picks.

**Local-only shares fall-through:** when the persister callback is nil (local-only, no engine wiring), the ObjectID compute still runs but the persist call is skipped — that gives local-only shares an ObjectID (D-10) when later a remote is attached, and is harmless when not.

**Chunker-loop accumulator point** (lines 313–327 — the existing chunk emission loop):
```go
pos := minOff
for pos < uint64(len(stream)) {
    b, _ := ck.Next(stream[pos:], true)
    if b <= 0 { break }
    chunkBytes := stream[pos : pos+uint64(b)]
    h := blake3ContentHash(chunkBytes)
    if err := bc.StoreChunk(ctx, h, chunkBytes); err != nil {
        return fmt.Errorf("rollup: StoreChunk: %w", err)
    }
    pos += uint64(b)
}
```

Add `blocks = append(blocks, blockstore.BlockRef{Hash: h, Offset: <abs>, Size: uint32(b)})` inside the loop. Absolute offset = `minOff + (pos - minOff)` before the `pos += b` update; verify against existing BlockRef ordering convention (sorted-by-Offset per Phase 12 META-01 D-10 / D-01).

---

### 12. `pkg/blockstore/engine/engine.go` — `Delete` refcount cascade

**Analog:** self — `(*BlockStore).Delete` (lines 416–464).

**Existing critical-section structure** (lines 447–462):
```go
var coordErr error
if len(blocks) > 0 && bs.coordinator != nil {
    for _, b := range blocks {
        if _, err := bs.coordinator.DecrementRefCount(ctx, b.Hash); err != nil {
            if coordErr == nil {
                coordErr = fmt.Errorf("decrement refcount on delete %s: %w", b.Hash.String(), err)
            }
        }
    }
}

if delErr := bs.syncer.Delete(ctx, payloadID); delErr != nil {
    if coordErr != nil {
        return errors.Join(coordErr, delErr)
    }
    return delErr
}
return coordErr
```

**Cascade insertion point (D-09):** the DecrementRefCount loop returns the new refcount. When `newCount == 0` for a given hash, **also call `bs.syncedHashStore.DeleteSynced(ctx, b.Hash)` in the same iteration**. Pattern:

```go
for _, b := range blocks {
    newCount, err := bs.coordinator.DecrementRefCount(ctx, b.Hash)
    if err != nil {
        if coordErr == nil {
            coordErr = fmt.Errorf("decrement refcount on delete %s: %w", b.Hash.String(), err)
        }
        continue
    }
    if newCount == 0 {
        // Refcount hit zero: the local CAS chunk will be reclaimed by
        // GC (or already by DeleteAllBlockFiles above). Drop the
        // synced marker so the syncedHashStore stays a strict subset
        // of local CAS contents — otherwise a future re-Put of the
        // same hash would skip remote upload because the marker is
        // stale.
        if bs.syncedHashStore != nil {
            if derr := bs.syncedHashStore.DeleteSynced(ctx, b.Hash); derr != nil {
                logger.Warn("delete synced marker (orphan; benign)",
                    "hash", b.Hash.String(), "err", derr)
            }
        }
    }
}
```

**Existing signature check needed:** verify `coordinator.DecrementRefCount` returns `(newCount int64, err error)` or similar. Grep `func.*DecrementRefCount` in `pkg/blockstore/engine/coordinator.go` to confirm. If it currently returns only `error`, this phase needs a tiny signature extension (or a new `DecrementRefCountAndReport` method) — surface as a planner sub-decision.

---

### 13. `pkg/blockstore/engine/engine.go` — `Flush` pre-rollup hook

**Analog:** `(*BlockStore).Flush` (lines 530–532) — currently a thin proxy.

**Existing body:**
```go
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
    return bs.syncer.Flush(ctx, payloadID)
}
```

**Target (D-12):** invoke `trySpeculativeFileLevelDedup` (private, dedup.go:42) directly BEFORE delegating to syncer.Flush. Pull the speculative-block snapshot from `syncer.snapshotPendingBlockRefs` (engine/syncer.go:365 currently — that helper stays, ownership moves) or relocate it.

**Move-target call shape** (currently at syncer.go lines 364–388):
```go
if m.coordinator != nil {
    specBlocks, blockStates, err := m.snapshotPendingBlockRefs(ctx, payloadID)
    if err != nil { ... }
    if len(specBlocks) > 0 {
        fileObjectID, err := m.coordinator.GetFileObjectID(ctx, payloadID)
        if err != nil { ... }
        hit, err := m.TrySpeculativeFileLevelDedup(ctx, payloadID, specBlocks, fileObjectID, blockStates)
        if err != nil { ... }
        if hit {
            return &blockstore.FlushResult{Finalized: true}, nil
        }
    }
}
```

This entire block lifts up into `BlockStore.Flush`. The public `TrySpeculativeFileLevelDedup` wrapper (syncer.go:176–184) is deleted; `BlockStore.Flush` calls the private `bs.syncer.trySpeculativeFileLevelDedup` (or — cleaner per D-17 — the function moves off the Syncer receiver and onto either a free function in dedup.go or a method on BlockStore).

---

### 14. `pkg/blockstore/engine/syncer.go` — body collapse (Flush rewrite)

**Analog:** none in-tree. This is a structural rewrite — see CONTEXT.md `<domain>` for the target shape.

**Auxiliary state that STAYS verbatim** (D-16):
- Struct fields (lines 32–94): local, remoteStore, fileBlockStore, coordinator, bs, config, queue, inFlight, inFlightMu, stopCh, closed, mu, periodicStarted, **uploading atomic.Bool**, healthMonitor, onHealthChanged, firstOfflineRead, offlineReadsBlocked
- Construction (`NewSyncer` lines 97–136): unchanged
- Lifecycle (`Start` lines 769–792, `startHealthMonitor` 796–810, `startPeriodicUploader` 814–825): unchanged
- Health helpers (lines 226–292, `remoteUnavailableError`, `logOfflineRead`, `OfflineReadsBlocked`): unchanged
- Periodic-uploader goroutine: KEEP (rewrites its claim loop body but keeps the scheduling shell)

**Deletion targets (verified line ranges in `pkg/blockstore/engine/syncer.go`):**

| Lines | Symbol | Reason |
|---|---|---|
| 151–184 | `TrySpeculativeFileLevelDedup` public wrapper | D-12, D-17 — call private from engine.Flush |
| 186–224 | `persistFileBlocksAfterFlush` | D-11 — ObjectID moves to rollup.go (D-10) |
| 338–426 | `Flush` body (89 lines) | D-16 — collapses to mirror-loop shell |
| 437+ | `drainPayloadToRemote` (referenced at line 393) | D-11 — replaced by mirror loop |

**Files to scan for additional deletions:** `upload.go` (uploadOne lines 74–151, BLAKE3 recompute at line 86–88), and the dedup short-circuit at uploadOne lines 90–127 (the in-Syncer dedup that re-hashes — separate from `trySpeculativeFileLevelDedup`).

**New Flush body skeleton (~50 LoC target per CONTEXT.md `<domain>`):**
```go
func (m *Syncer) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
    if err := m.checkReady(ctx); err != nil { return nil, err }

    // 1. Local-side flush (existing behavior — unchanged).
    if _, err := m.local.Flush(ctx, payloadID); err != nil {
        return nil, fmt.Errorf("local store flush failed: %w", err)
    }

    // (file-level dedup pre-hook moved to engine.Flush — D-12)

    // 2. Mirror loop: every CAS chunk present locally but not yet
    //    marked synced gets Put to remote, then MarkSynced (Put-then-
    //    Mark per D-07).
    if m.remoteStore == nil || !m.IsRemoteHealthy() {
        return &blockstore.FlushResult{Finalized: false}, nil
    }
    for hash, err := range m.local.ListUnsynced(ctx) {
        if err != nil { return nil, fmt.Errorf("list unsynced: %w", err) }
        data, err := m.local.Get(ctx, hash)
        if err != nil { return nil, fmt.Errorf("local get %s: %w", hash, err) }
        if err := m.remoteStore.Put(ctx, hash, data); err != nil {
            return nil, fmt.Errorf("remote put %s: %w", hash, err)
        }
        if err := m.syncedHashStore.MarkSynced(ctx, hash); err != nil {
            return nil, fmt.Errorf("mark synced %s: %w", hash, err)
        }
    }
    return &blockstore.FlushResult{Finalized: true}, nil
}
```

**`Syncer` needs a `syncedHashStore` reference:** add `syncedHashStore metadata.SyncedHashStore` to the struct + wire via `NewSyncer` or `SetSyncedHashStore` (mirror `SetCoordinator` lines 145–149).

---

### 15. `pkg/blockstore/engine/upload.go` — BLAKE3 recompute deletion

**Analog:** self — line 85–88.

**Deletion target:**
```go
// BLAKE3-256 (Phase 10 D-08 amendment + BSCAS-03).
h := blake3.Sum256(data)
var hash blockstore.ContentHash
copy(hash[:], h[:])
```

This is dead in the new world: the hash is now produced once at chunk-creation time (in `pkg/blockstore/local/fs/rollup.go::blake3ContentHash` at line 446) and stored as the CAS filename. The mirror loop has the hash from `ListUnsynced` — no re-derivation needed.

**Cascade deletion:** `uploadOne` (lines 74–151) as a whole becomes unreachable once Flush stops calling `drainPayloadToRemote`. Either delete `uploadOne` entirely or repurpose its body as the per-hash mirror-pump helper (planner's call — D-16 says struct/state stays but per-method shape is open).

---

### 16. `pkg/blockstore/engine/dedup.go` — keep private, delete public wrapper

**Analog:** self.

**KEEP:** lines 42–78 — `func (m *Syncer) trySpeculativeFileLevelDedup` (lowercase, private). Logic is intact; only the caller changes (engine.Flush invokes it directly per D-17).

**DELETE (in syncer.go):** lines 151–184 — `func (m *Syncer) TrySpeculativeFileLevelDedup` (uppercase, public wrapper).

**Consideration:** D-12 says "engine.Flush calls into the existing private `trySpeculativeFileLevelDedup`". If `BlockStore.Flush` lives in engine.go and the private method is on `*Syncer`, then `BlockStore.Flush` calls `bs.syncer.trySpeculativeFileLevelDedup(...)` — same package (`engine`), so the lowercase method is reachable. No new export needed.

---

### 17. `pkg/blockstore/doc.go` — `TRANSITIONAL-NEXT-MILESTONE` convention

**Analog:** the existing `TRANSITIONAL-PHASE-18:` marker convention documented in `pkg/blockstore/local/local.go` lines 149–152:
```go
// Grep marker for the Phase 18 cleanup wave: TRANSITIONAL-PHASE-18.
// The marker is plain text (not a godoc "Deprecated:" tag) so
// staticcheck SA1019 does not fire on existing call sites that will
// be rewritten in Phase 18.
```

**Add to `pkg/blockstore/doc.go`** a new section (after `# Sub-packages`) — example shape:

```
// # Transitional-marker convention
//
// Code that must compile today but is slated for deletion at a known
// future milestone carries a plain-text grep marker on its godoc:
//
//   TRANSITIONAL-PHASE-18:  scheduled deletion in v0.16 Phase 18
//   TRANSITIONAL-NEXT-MILESTONE:  scheduled deletion at the next major
//                                  milestone planning pass (generic;
//                                  use when no specific phase number
//                                  applies yet)
//
// Markers are plain text (not godoc "Deprecated:") so staticcheck
// SA1019 does not fire on existing call sites. The next milestone's
// planning pass greps for both markers and either retires them
// (deletion) or re-targets them to a specific phase tag.
```

Project rule: **DO NOT** include "Phase 18" or "D-19" in the godoc body itself per `feedback_no_phase_comments_in_code.md`. The marker strings themselves are operational, not provenance — those are allowed (they're literal grep targets). The narrative godoc must stay milestone-agnostic ("the next milestone's planning pass" — not "v0.16 milestone").

---

### 18. `pkg/blockstore/engine/syncer_test.go` (RE-CREATED, `//go:build integration`)

**Analog:** none in tree currently — this closes the Phase 17 17-VERIFICATION.md deferred follow-up (per D-15 + canonical_refs).

**Build tag header to copy** (from `pkg/metadata/store/postgres/rollup_test.go` line 1):
```go
//go:build integration

package engine_test
```

**Test scenarios per D-15:**
1. `TestSyncer_MirrorLoop_HappyPath` — seed N local CAS chunks via `local.Put`, run `syncer.Flush`, assert all hashes IsSynced + Walk on remote returns same hashes.
2. `TestSyncer_MirrorLoop_PutThenMark_CrashReplay` — inject a remote that fails AFTER successful Put but BEFORE MarkSynced returns (simulate kill-9 window): re-run Flush, assert no corruption + final state equivalent. Idempotent-on-identical-bytes guarantee (Phase 17 contract).
3. `TestSyncer_ListUnsynced_SnapshotSemantics` — start Flush, while iteration in progress add new chunks via `local.Put`, assert new chunks are NOT in the current pass but ARE picked up on the next Flush.
4. `TestEngine_Delete_CascadesDeleteSynced` — fully sync a hash, then `engine.Delete` it (refcount → 0), assert `syncedHashStore.IsSynced(hash) == false`.

**Backend fixtures to exercise (D-15):** s3 (via `pkg/blockstore/remote/s3`) + memory (via `pkg/blockstore/remote/memory`). Pattern: use `remotememory.New()` for the memory fixture; for s3 use Localstack — see `pkg/blockstore/engine/syncer_flush_test.go` for the existing `remotememory.New()` plumbing (line 136) and `pkg/controlplane/runtime/...` for the s3 Localstack pattern.

---

### 19. `pkg/blockstore/engine/dedup_test.go` + neighbor tests — Phase 13 sweep (D-14)

**Files to enumerate + retarget (verified via grep):**

| Test file | Symbols to update | Action |
|---|---|---|
| `pkg/blockstore/engine/syncer_flush_test.go` | `TestSyncer_Flush_InvokesPostFlushHook` (line 135), `TestSyncer_Flush_PartialQuiesceSkipsHook` (194), `TestSyncer_Flush_NilCoordinatorIsNoop` (221), `TestSyncer_Flush_NoBlocksForPayloadIsNoop` (254), `TestSyncer_Flush_FileLevelDedupHitSkipsUploadPump` (372), `TestSyncer_Flush_FileLevelDedupMissProceedsToUpload` (460), `TestSyncer_Flush_FileLevelDedupSkippedWhenObjectIDNonZero` (517), `TestSyncer_Flush_FileLevelDedupSkippedWhenSomeBlocksRemote` (584) | Rewrite first one to `TestRollup_CommitChunks_PersistsObjectID` per D-11. Retarget remaining 7 onto `engine.Flush` entrypoint per D-13. |
| `pkg/blockstore/engine/api_blockref_test.go` | line 215, 218, 229, 233 — direct call to `bs.syncer.persistFileBlocksAfterFlush` | Retarget onto `engine.Flush` post-rollup hook. |
| `pkg/blockstore/engine/dedup_test.go` | `TestDedup_TriggerCondition` (98), `TestDedup_ShortCircuit_HitFlow` (184), `TestDedup_ShortCircuit_MissFlow` (248), `TestDedup_RefCountMath` (289), `TestDedup_CacheInvalidation` (349), `TestDedup_ConcurrentRace` (430) — all call `ComputeObjectID` directly to assert dedup math | KEEP — they exercise the private `trySpeculativeFileLevelDedup` which stays. Only update call sites if they reach through the deleted public `TrySpeculativeFileLevelDedup` wrapper (verify each). |
| `pkg/blockstore/engine/perf_bench_test.go` | line 419, 433, 454, 492, 526 — bench calls `f.syncer.persistFileBlocksAfterFlush` directly | Retarget onto new ObjectID compute path (rollup.go) or delete bench if it ceases to make sense. |
| `pkg/blockstore/engine/syncer_unit_test.go` | `TestClaimBatch_*` (73, 110), `TestUploadOne_*` (124, 168), `TestRecoverStaleSyncing` (180), `TestSyncNow_DrainsAllPendingViaCAS` (224) | DELETE or rewrite — `uploadOne` and `claimBatch` semantics change in the mirror-loop world. |
| `pkg/blockstore/engine/syncer_crash_test.go` | `TestSyncerCrash_PrePut` (165), `BetweenPutAndMeta` (200), `PostMeta` (241), `RecoveryRequeuesStale` (276) | Rewrite onto the Put-then-MarkSynced crash window per D-07 (subsumed by new `syncer_test.go` per D-15). |
| `pkg/blockstore/engine/syncer_put_error_test.go` | `TestSyncFileBlock_PropagatesPutError` (49) | Rewrite onto mirror-loop error propagation. |
| `pkg/blockstore/engine/upload_test.go` | `TestUploadOne_Dedup_SingleRowAfterShortCircuit` (26), `_DonorRefCountIncrementedExactlyOnce` (98), `_DeleteIdempotent` (147) | DELETE if `uploadOne` deleted; the in-Syncer dedup path goes away when BLAKE3 recompute (upload.go:86) is removed. |
| `test/e2e/dedup_objectid_population_test.go` | references `Syncer.persistFileBlocksAfterFlush` (lines 9, 196) | Update godoc reference; logic unchanged (E2E doesn't depend on symbol existence). |

**Sweep checklist for the planner:**
```
grep -rn "TrySpeculativeFileLevelDedup\|persistFileBlocksAfterFlush\|FlushedBlock\|local\.Flush\|IsBlockLocal\|GetBlockData\|WriteFromRemote\|DeleteAllBlockFiles\|ReadAt\b\|WriteAt\b" \
    pkg/blockstore/ test/
```
That grep enumerates every site touched by Phase 18 deletions. Run it before the deletion commit (D-14 — "partial sweep leaves neighbor tests red on CI").

---

## Shared Patterns

### A. Compile-time interface assertion

**Source:** `pkg/metadata/store/memory/rollup.go:10`, `pkg/metadata/store/badger/rollup.go:37`, `pkg/metadata/store/postgres/rollup.go:30`.

**Apply to:** every new `synced_hash_store.go` backend file.

```go
var _ metadata.SyncedHashStore = (*MemoryMetadataStore)(nil)
var _ metadata.SyncedHashStore = (*BadgerMetadataStore)(nil)
var _ metadata.SyncedHashStore = (*PostgresMetadataStore)(nil)
```

### B. ctx-first nil-check on every store method

**Source:** universal across `rollup.go` backends (memory:20, badger:57, postgres:62, postgres:138).

**Apply to:** every `SyncedHashStore` method (IsSynced, MarkSynced, DeleteSynced) on all 3 backends.

```go
if err := ctx.Err(); err != nil {
    return /* zero value */, err
}
```

### C. Error wrap convention `<backend> <op>: %w`

**Source:** `pkg/metadata/store/badger/rollup.go:98` (`"badger rollup set: %w"`), `pkg/metadata/store/postgres/rollup.go:125` (`"postgres rollup upsert: %w"`).

**Apply to:** every new backend method. Format: `"<backend> synced <op>: %w"` — e.g. `"badger synced mark: %w"`, `"postgres synced delete: %w"`.

### D. Forbidden source-comment provenance (project rule)

**Source:** `feedback_no_phase_comments_in_code.md` (loaded from memory).

**Apply to:** every new file. Do NOT include:
- "Phase 18" in any source comment, godoc, or test name.
- "D-NN" decision IDs anywhere.
- References to `.planning/phases/18-syncer-simplification/` paths (this rule is project-wide, not optional).

The existing `rollup_store.go` family violates this (line 1: `// Package metadata — rollup_store.go (Phase 10).`). New code is expected to do BETTER, not match. Use neutral domain language and let git commit messages carry provenance.

### E. Test name + filename convention

**Source:** rollup_test.go pattern — `Test<Backend>RollupStore_<Scenario>`.

**Apply to:** new conformance test files. Pattern:
- `TestMemorySyncedHashStore_Suite`
- `TestBadgerSyncedHashStore_Suite`
- `TestPostgresSyncedHashStore_Suite`

Plus per-backend smoke tests if backend has invariants outside the shared suite (mirroring `TestBadgerRollupStore_Suite` as the *only* test in `pkg/metadata/store/badger/rollup_test.go` — minimal is fine).

### F. Conformance-suite location (in `pkg/metadata`, NOT `storetest`)

**Source:** `rollup_store_suite.go` lives at `pkg/metadata/rollup_store_suite.go` (NOT in `pkg/metadata/storetest/`).

**Rationale documented at rollup_store_suite.go lines 7–11:**
```go
// The suite lives in the metadata package rather than storetest to keep
// the dependency direction clean: storetest already depends on metadata,
// so moving this helper into storetest would be fine structurally, but
// the RollupStore interface itself lives here and the suite is logically
// paired with it.
```

**Apply to:** `synced_hash_store_suite.go` MUST live at `pkg/metadata/synced_hash_store_suite.go` — **NOT** at `pkg/metadata/storetest/synced_hash_store_conformance.go` (the ROADMAP entry naming `pkg/metadata/storetest/synced_hash_store_conformance.go` contradicts the established pattern; planner should escalate this naming discrepancy or follow the rollup_store_suite.go precedent — recommend the latter).

---

## No Analog Found

| File / Symbol | Reason |
|---|---|
| `iter.Seq2[blockstore.ContentHash, error]` push iterator | No existing Go 1.23 push-iterator usage in repo (`grep -r "iter.Seq" --include="*.go"` returned zero hits). Pattern derived from Go 1.23 stdlib idiom; the iterator BODY pattern is documented in section 10 above (closure-form yielding inside a `Walk` snapshot). |
| `pkg/blockstore/engine/syncer_test.go` integration scenarios | This file does not exist in tree (only `syncer_unit_test.go`, `syncer_flush_test.go`, `syncer_crash_test.go`, `syncer_put_error_test.go`). Re-creating per D-15 closes the Phase 17 17-VERIFICATION.md deferred follow-up. Pattern derived from the existing integration-tagged test in `pkg/metadata/store/postgres/rollup_test.go` (build-tag + env-gate skip). |

---

## Metadata

**Analog search scope:**
- `pkg/metadata/` (interface + 3 backends + conformance suite)
- `pkg/metadata/store/{memory,badger,postgres}/` (backend impls)
- `pkg/metadata/store/postgres/migrations/` (DDL conventions)
- `pkg/blockstore/{local,engine,remote}/` (consumer wiring)
- `pkg/blockstore/local/fs/` (FSStore + rollup + Walk)
- `pkg/blockstore/doc.go` (package-level convention docs)

**Files scanned:** 47 files Read + 8 broad greps over `pkg/blockstore/` and `pkg/metadata/`.

**Pattern extraction date:** 2026-05-21.

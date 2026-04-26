---
phase: 11-cas-write-path-gc-rewrite-a2
reviewed: 2026-04-25T00:00:00Z
depth: deep
pass: 2
files_reviewed: 16
files_reviewed_list:
  - pkg/blockstore/local/fs/fs.go
  - pkg/blockstore/local/fs/eviction.go
  - pkg/blockstore/local/fs/chunkstore.go
  - pkg/blockstore/local/fs/flush.go
  - pkg/blockstore/local/fs/recovery.go
  - pkg/blockstore/engine/upload.go
  - pkg/blockstore/engine/syncer.go
  - pkg/blockstore/engine/fetch.go
  - pkg/blockstore/engine/gc.go
  - pkg/blockstore/engine/gcstate.go
  - pkg/blockstore/remote/s3/store.go
  - pkg/blockstore/remote/s3/verifier.go
  - pkg/blockstore/types.go
  - pkg/metadata/store/postgres/objects.go
  - pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql
  - pkg/metadata/store/postgres/migrations/000010_file_blocks.down.sql
  - pkg/controlplane/runtime/blockgc.go
  - internal/controlplane/api/handlers/block_gc.go
  - internal/adapter/common/content_errmap.go
findings:
  critical: 1
  warning: 1
  info: 4
  total: 6
status: issues_found
---

# Phase 11: Code Review Report (Pass 2)

**Reviewed:** 2026-04-25
**Depth:** deep (cross-file: fetch ↔ local ↔ syncer ↔ metadata ↔ GC)
**Files Reviewed:** 16 source files in scope, with cross-tracing into recovery, manage, remote/s3, and the conformance suite
**Pass-1 fixes confirmed in:** commits `63f9218e`, `07c6bb4f`, `6cfd6cae`, `6b2f963d`, `233f058b`, `4a2f99ba`, `39dfc2ac` — every CR/WR/IN finding from REVIEW-1 has a corresponding fix commit on the branch.

## Summary

Pass 2 traced the harder-to-spot interactions pass 1 didn't cover: WriteFromRemote ↔ FileBlockStore consistency across restarts, the verifier wrapper's HTTP body lifecycle, the GCState concurrency model, dual-read precedence, and the migration story. Pass 1's fixes all hold up — none of the pass-1 findings re-surfaced in adjacent code.

Pass 2 found **one BLOCKER**: `pkg/blockstore/local/fs/fs.go:WriteFromRemote` is a direct Phase 11 regression that silently corrupts FileBlockStore rows for any remote-fetched block whose entry is not in the in-process `diskIndex` (the steady-state case after a server restart, or for a block first read on a node that never wrote it). The corruption replaces a valid CAS hash row with a zero-hash legacy-key row, after which the next read returns silent zeros and the next GC reaps the still-live CAS object as an orphan. This is not theoretical — it falls out of normal restart-and-read traffic and is not exercised by any current unit/E2E test.

One WARNING covers the LSL-08 LRU race that re-inserts a just-evicted hash on a parallel ReadChunk. Four INFO items cover the verifier's HTTP connection-pool behavior on early error, the GC clock-skew/short-grace edge, an internal-error leak in the GC handler response, and an absent paginated-S3 test for `ListByPrefixWithMeta`.

The seven items pass 2 considered and **discarded** (no actionable bug):

- **GCState concurrent Add from mark phase**: `markPhase` iterates shares sequentially in a single goroutine; Badger's `db.Update` is internally serialized; no concurrency to worry about.
- **TOCTOU between snapshotTime and S3 LastModified**: defended at the engine layer by `GracePeriod` defaulting to 1h. With the default, server-clock skew under tens of seconds is irrelevant. Operators who set very short grace periods do so against a documented warning (D-05).
- **Janitor running while syncer claims**: the `m.uploading` `atomic.Bool` gates SyncNow against the periodic uploader within one process; cross-process the CAS PUT idempotency makes the duplicate work harmless (correctly documented in the WR-05 fix comment).
- **GCState `Close()` on panic**: `defer gcs.Close()` and `defer cleanupTempGCStateRoot(...)` are unconditional after their preconditions hold; both fire on panic.
- **Verifier body draining on early-cancel**: `verifyingReader.Close()` always calls `v.src.Close()` exactly once. Functionally correct. (Minor connection-pool inefficiency noted in IN-2-02.)
- **flushBlock buffer-pool capture by closure**: `staged := bytes.Clone(...)` is a fresh allocation freed by GC after `os.Rename`; no closure captures it after `flushBlock` returns. The `bufToReturn` lifecycle is correct.
- **Postgres migration `down` reversibility / existing-row handling**: `000010_file_blocks.down.sql` properly drops every index and the table. The pre-Phase-11 inline `fileBlocksTableMigration` (referenced in the up.sql header comment) was removed in Plan 02; only the migration runner now creates the table. Existing rows on a Phase-10 deployment land on a `CREATE TABLE IF NOT EXISTS` no-op and are then upserted normally.
- **Migration safety**: the migration is idempotent (`IF NOT EXISTS`); the schema is a strict superset of any prior in-tree shape (no destructive ALTERs).
- **Backward-compat write of OLD-format keys**: confirmed no production code path calls `RemoteStore.WriteBlock` (legacy) — `uploadOne` and `uploadBlock` both use `WriteBlockWithHash` with `FormatCASKey`. The dual-read shim is read-only.
- **Logging hygiene**: no auth tokens, secrets, or full content paths logged at INFO/WARN. CAS hashes are logged in error messages but they're public information by definition (computed from object bytes).
- **`errors.Is` ordering in content_errmap.go**: the three sentinels (`ErrCASKeyMalformed`, `ErrCASContentMismatch`, `ErrRemoteUnavailable`) are independent — none wraps another — so order is irrelevant.
- **Empty-file edge case in CAS**: `blake3.Sum256(nil)` returns the well-defined BLAKE3 hash of the empty string. `flushBlock` early-returns for `mb.data == nil`, so empty blocks never reach `uploadOne`. No hash collisions; no zero-hash artifacts.

---

## Critical Issues

### CR-2-01: `WriteFromRemote` overwrites the CAS metadata row with a legacy-key zero-hash row → silent zero-data reads + GC reap of live CAS object

**File:** `pkg/blockstore/local/fs/fs.go:823-849` (`func (bc *FSStore) WriteFromRemote`)

**Issue:** When the engine fetches a block from remote (`fetchBlock` → `dispatchRemoteFetch` → `local.WriteFromRemote`), the local store re-registers the FileBlock metadata as:

```go
fb, ok := bc.diskIndexLookup(blockID)        // line 828
if !ok {
    fb = blockstore.NewFileBlock(blockID, "") // line 830 — Hash defaults to {0...0}
}
fb.BlockStoreKey = blockstore.FormatStoreKey(payloadID, blockIdx) // LEGACY key (line 832)
fb.State = blockstore.BlockStateRemote                              // line 833
// fb.Hash is NEVER set/preserved
...
bc.queueFileBlockUpdate(fb)                  // line 849 → eventually PutFileBlock(fb)
```

Two failure modes, both reachable in normal operation:

**Failure mode A (hard regression — diskIndex miss after restart):**

1. Server uploads block B → `uploadOne` writes CAS object at `cas/aa/bb/<hex>`, persists row `{Hash=H, BlockStoreKey="cas/aa/bb/<hex>", State=Remote}`.
2. Server restarts. The in-process `diskIndex` is rebuilt by `Recover` ONLY for blocks whose `.blk` file exists locally (`recovery.go:122 diskIndexStore`). Blocks served exclusively from remote (or whose local copy was never created) are NOT in the diskIndex.
3. A read of block B requires a remote fetch. `inlineFetchOrWait` resolves the FB from the FileBlockStore (correct: Hash=H, key=CAS), `dispatchRemoteFetch` reads + verifies the CAS object (correct), then calls `local.WriteFromRemote(...)`.
4. `WriteFromRemote` calls `diskIndexLookup(blockID)` — **MISS** (block isn't on local disk after restart). Falls through to `NewFileBlock(blockID, "")` which produces `Hash=ContentHash{}` (all zeros).
5. Lines 832–833 stamp `BlockStoreKey="payloadID/block-N"` (legacy format) and `State=Remote`. Hash stays zero.
6. `queueFileBlockUpdate` queues this fb for `PutFileBlock`. The next `SyncFileBlocks` tick (200ms) UPSERTs the row. Phase 11 `PutFileBlock` (postgres line 56-59) gates `hashStr` on `block.IsFinalized()` (state==Remote, which IS true here). The hash column is then written as `block.Hash.String()` = `"0000000000...0000"` (a syntactically-valid CAS hash that's all-zeros), NOT NULL.

The metadata row is now: `hash="00...00"`, `block_store_key="payloadID/block-N"` (legacy), `state=Remote`. **The original row referencing the actual CAS key has been clobbered.**

**Failure mode B (every steady-state remote-fetch on a node that did not produce the block):** Identical mechanism: any node that fetches a block but didn't produce it locally has no diskIndex entry and falls into the same overwrite path. Multi-server deployments hit this on every cross-node read.

**Downstream consequences (BOTH failure modes):**

1. **Silent zero-data reads.** Subsequent `dispatchRemoteFetch` for the same block sees `fb.Hash.IsZero()` → legacy path → `m.remoteStore.ReadBlock(legacy_key)` (fetch.go:89). The legacy key never existed on remote (Phase 11 only writes CAS keys), so it returns `ErrBlockNotFound`. `fetchBlock` (fetch.go:123) and `inlineFetchOrWait` (fetch.go:269) both treat `ErrBlockNotFound` as "sparse" and return nil data → reader receives **zeros**, no error.

2. **GC reap of the live CAS object (INV-04 violation by data path).** The mark phase enumerates the corrupted row, sees `h.IsZero()` (gc.go:265), and SKIPS it as "legacy pre-CAS." The actual `cas/aa/bb/<hex>` object is therefore not in the live set. The sweep walks the `cas/aa/` prefix, finds the object, and (after grace TTL) DELETES it. Live data is permanently destroyed.

The pass-1 CR-01 fix tightened EnumerateFileBlocks against malformed hashes precisely to prevent live-data deletion. CR-2-01 is the same outcome via a different vector: the metadata row itself is corrupted by a write path the GC treats as legitimate.

**Confidence:** HIGH. The bug falls out of static analysis; I traced it to `recovery.go:122` (diskIndex seeding) and confirmed:
- `seedLRUFromDisk` (fs.go:357) only seeds CAS chunks, not `.blk` files.
- `diskIndexStore` is only called from `Recover` (recovery.go:122) and `queueFileBlockUpdate` (fs.go:586).
- `Recover` only calls `diskIndexStore` for blocks whose `.blk` file currently exists on disk (recovery.go iterates by `.blk` filename).
- Therefore: any blockID whose `.blk` file is absent at startup is NOT in the diskIndex → `WriteFromRemote` falls into the bug path.

`TestWriteFromRemote` (fs_test.go:653) only verifies the local read works after a single WriteFromRemote — it does not exercise the cross-restart path or assert on the FileBlockStore state, which is why this slipped through.

**Fix sketch:** Three options, in order of safety:

**Option A (safest, narrowest change):** Stop mutating FileBlock metadata in `WriteFromRemote`. The engine has already persisted the canonical row (Hash + CAS key) via the syncer's uploadOne. WriteFromRemote should only:
- Materialize the bytes on disk under `<baseDir>/<bb>/{blockID}.blk`.
- Populate the diskIndex (so future eviction can see the file) by *looking up* the existing row from the FileBlockStore (use `lookupFileBlock`, not `diskIndexLookup`) and only setting `LocalPath` + `LastAccess`.

```go
func (bc *FSStore) WriteFromRemote(ctx context.Context, payloadID string, data []byte, offset uint64) error {
    blockIdx := offset / blockstore.BlockSize
    blockID := makeBlockID(blockKey{payloadID: payloadID, blockIdx: blockIdx})

    fb, ok := bc.diskIndexLookup(blockID)
    if !ok {
        // Missing from diskIndex (e.g. post-restart, no local .blk) — fetch from
        // the FileBlockStore so we preserve the CAS hash + key the syncer wrote.
        existing, err := bc.lookupFileBlock(ctx, blockID)
        if err == nil && existing != nil {
            fb = existing
        } else {
            // Truly novel block — caller is the syncer's first registration path.
            fb = blockstore.NewFileBlock(blockID, "")
        }
    }
    // DO NOT touch fb.BlockStoreKey or fb.Hash.
    // Only update fields owned by the local-cache concern:
    fb.State = blockstore.BlockStateRemote
    // ... ensureSpace, write file, update LocalPath, queueFileBlockUpdate ...
}
```

**Option B (preserve current API):** Have `WriteFromRemote` accept the resolved `*FileBlock` from the engine (callers in fetch.go already have it) so no FileBlockStore round-trip is needed:

```go
func (bc *FSStore) WriteFromRemote(ctx context.Context, fb *blockstore.FileBlock, data []byte) error {
    // fb has the canonical Hash + BlockStoreKey from the engine's resolveFileBlock.
    // Local store only stamps LocalPath + LastAccess.
}
```

**Option C (smallest diff):** Drop the `fb.BlockStoreKey = FormatStoreKey(...)` line entirely. After Phase 11, no caller of WriteFromRemote should be writing legacy keys. If `diskIndexLookup` misses, `NewFileBlock` produces `BlockStoreKey=""` and `Hash=zero` — but a downstream `lookupFileBlock` could re-attach the canonical fields. This is fragile because the queueFileBlockUpdate UPSERT will still null out the hash column (NewFileBlock has zero Hash). **Option C alone is insufficient — combine with the lookupFileBlock fallback from Option A.**

Recommended: Option A. Add a regression test that:
1. Uploads block B (CAS path).
2. Drops the in-process diskIndex (or restarts the FSStore).
3. Calls WriteFromRemote for block B with the original bytes.
4. Asserts the FileBlockStore row STILL has Hash=H and BlockStoreKey=CAS-key.
5. Calls dispatchRemoteFetch and asserts the returned bytes are non-zero.

A storetest-conformance variant ("restart-then-fetch preserves CAS metadata") is also justified.

---

## Warnings

### WR-2-01: LSL-08 LRU race re-inserts an evicted entry on the post-read `lruTouch`

**File:** `pkg/blockstore/local/fs/chunkstore.go:108-134` (`ReadChunk`)

**Issue:** `ReadChunk` opens the chunk file (line 118), defers Close, reads the bytes (line 126), then calls `bc.lruTouch(h, len(data), path)` (line 132). On POSIX, an unlinked file remains readable through an existing fd — so a concurrent `lruEvictOne` that runs between `os.Open` and `lruTouch` will:

1. Pop the chunk from the LRU and `delete(bc.lruIndex, entry.hash)` (fs.go:336-337).
2. `os.Remove(entry.path)` (fs.go:340) — the on-disk file is gone, but the open fd in ReadChunk still works.
3. ReadChunk completes the read successfully and re-inserts via `lruTouch` (fs.go:309-322).
4. The LRU now thinks the chunk is present at `entry.path` — but the file is gone.

The next `lruEvictOne` for that entry will hit `os.Remove(entry.path)` returning ENOENT (gracefully tolerated by `!os.IsNotExist(err)`), so the freed-bytes count is reported as `entry.size` for a chunk that was already gone. The diskUsed counter drifts by the size of the ghost entry — over time, the LRU may believe it has more reclaimable space than it actually does, causing `ensureSpace` to over-evict on a subsequent admission burst.

A future read of the same hash would attempt `os.Open` on the deleted path, get ENOENT, and surface `ErrChunkNotFound`. The engine's `accept-and-refetch` posture (T-11-B-08) makes this functionally correct — but the diskUsed drift is a real bookkeeping bug that grows monotonically under concurrent read-evict pressure.

**Confidence:** HIGH for the LRU-pollution path; MEDIUM for the diskUsed drift's operational impact (depends on workload mix; pure read-after-evict patterns drive it, mixed write-heavy workloads dilute it).

**Fix:** Two options:

**Option A (cleanest):** Hold `lruMu` across the eviction's `os.Remove`, so a concurrent `lruTouch` cannot observe the entry as both "evicted from the index" and "file still readable":

```go
func (bc *FSStore) lruEvictOne() (int64, error) {
    bc.lruMu.Lock()
    defer bc.lruMu.Unlock()
    el := bc.lruList.Back()
    if el == nil { return 0, errLRUEmpty }
    entry := el.Value.(*lruEntry)
    bc.lruList.Remove(el)
    delete(bc.lruIndex, entry.hash)
    if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
        bc.lruIndex[entry.hash] = bc.lruList.PushBack(entry)
        return 0, fmt.Errorf("evict %s: %w", entry.path, err)
    }
    return entry.size, nil
}
```

This widens the lock window slightly (a syscall under the LRU mutex) but correctly serializes against `lruTouch`.

**Option B (defensive in lruTouch):** After read completes, re-stat the file before re-inserting:

```go
func (bc *FSStore) lruTouchAfterRead(h ContentHash, size int64, path string) {
    if _, err := os.Stat(path); err != nil { return } // file gone, don't re-insert
    bc.lruTouch(h, size, path)
}
```

Adds a syscall per read-hit; less efficient but no lock-window expansion.

Add a regression test that exercises 100 parallel ReadChunks against an LRU-saturated store with concurrent eviction pressure, asserting `recalcDiskUsed()` matches `bc.diskUsed.Load()` at quiescence.

---

## Info

### IN-2-01: GC handler `RunGC` echoes internal `err.Error()` strings to the API client

**File:** `internal/controlplane/api/handlers/block_gc.go:105`

**Issue:** `InternalServerError(w, "GC failed: "+err.Error())` propagates the raw underlying error string to the HTTP response. Errors from the GC engine can include filesystem paths (`"persist last-run.json: ..."`), DB messages, or reconciler details. For the operator-only API this is acceptable, but it's a deviation from the pattern in nearby handlers that log details at Debug and return a generic message:

```go
logger.Debug("GC failed", "share", name, "error", err)
InternalServerError(w, "GC failed")
```

**Fix:** Match the GCStatus handler's pattern (which already uses Debug-only error logging at line 159, 166). Strip the internal err string from the response in RunGC.

### IN-2-02: Verifier early-error path closes the body but does not drain — degrades HTTP connection-pool reuse

**File:** `pkg/blockstore/remote/s3/verifier.go:78-91` (`verifyingReader.Close`)

**Issue:** When `readAllVerified` returns early with an I/O or mismatch error, `reader.Close()` is called and forwards to `v.src.Close()`. Go's HTTP/1.1 connection pool requires the body to be **drained** (read to EOF) before close to keep the connection alive for reuse; closing mid-stream invalidates the connection. The S3 SDK is configured for HTTP/1.1 (`NextProtos: []string{"http/1.1"}` in store.go:117), so this matters.

The functional behavior is correct — `resp.Body` is always closed exactly once, no fd leak. But under sustained CAS-content-mismatch pressure (e.g., a corrupted bucket), every failed GET burns a TCP connection.

**Fix:** Before the early return on mismatch, drain remaining body bytes (capped to a small limit to bound the worst case):

```go
if mismatch := v.checkHash(); mismatch != nil {
    _, _ = io.CopyN(io.Discard, v.src, 1<<14) // drain up to 16KB to enable conn reuse
    return n, mismatch
}
```

The same drain applies to the `Close()` path when `!v.done`. A 16-32KB cap is the standard Go-stdlib pattern (see `net/http.Response.Close`).

This is also a pre-existing concern in `s3.Store.ReadBlock` (which uses the unwrapped `defer resp.Body.Close()`). Lower priority than CR-2-01.

### IN-2-03: GC sweep treats `LastModified` clock-skew between server and S3 as operator's problem

**File:** `pkg/blockstore/engine/gc.go:339-341`

**Issue:** The grace-window check is `obj.LastModified.After(snapshotTime.Add(-gracePeriod))`. With default `gracePeriod=1h`, server-S3 clock skew under a few minutes is irrelevant. The config validator emits a warn for `gracePeriod < 5m` but does NOT reject 0 or arbitrarily-low values. Under aggressive operator misconfiguration (`gc.grace_period: 30s`), an object PUT during the mark phase whose LastModified happens to lag the server clock by 1 minute could be reaped on the same sweep — silent live-data loss.

The engine itself defaults `<= 0` to 1h (gc.go:151-153), so config-side `0` is harmless. The remaining hole is `0 < grace < server-clock-skew`.

**Fix:** Either (a) hard-reject `gracePeriod < 5m` at config validation (turn the warn into an error), or (b) clamp the engine's `gracePeriod` to `max(configured, 1m)` to give clock skew unconditional headroom. The config layer is the better home — operators who genuinely want short GCs are on a development workload where INV-04 doesn't matter.

### IN-2-04: No paginated-S3 test for `ListByPrefixWithMeta`

**File:** `pkg/blockstore/remote/s3/store.go:505-544`

**Issue:** `ListByPrefixWithMeta` uses the SDK paginator (correctly), but no unit/integration test exercises the multi-page path. The default ListObjectsV2 page size is 1000 keys; a CAS bucket with >1000 objects in a single `cas/XX/` prefix would exercise pagination. No test in `pkg/blockstore/remote/s3/store_test.go` references `ListByPrefixWithMeta` at all, and the in-tree memory store's implementation is single-page by construction.

If the SDK ever changes its default page handling, or a future refactor forgets to advance the paginator, the regression would surface as silent under-counting in the GC mark/sweep — orphans persist past their grace window and operators see disk usage diverge from their model.

**Fix:** Add a Localstack-backed test (or an httptest mock that returns `IsTruncated=true` + `NextContinuationToken`) that seeds 1500+ objects under one prefix and asserts `ListByPrefixWithMeta` returns all of them. The Localstack-based pattern already exists in the project (`TestCollectGarbage_S3` per CLAUDE.md notes); adding a paginated case is a 30-line addition.

Same gap exists for `ListByPrefix` and `DeleteByPrefix`, but those are pre-existing and not in Phase 11 scope.

---

_Reviewed: 2026-04-25_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: deep (cross-file: fetch ↔ local ↔ syncer ↔ metadata ↔ GC; cross-restart trace)_
_Pass: 2 of N — pass-1 fixes confirmed clean; CR-2-01 surfaces the highest-impact remaining defect_

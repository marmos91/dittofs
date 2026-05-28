# Blockstore — Bug Findings (`code-reviewer` agent output)

**Scope**: 171 `.go` files under `pkg/blockstore/` including `engine/gc*.go`. Excludes syncer deep-audit (Area 2) and backup/snapshot (Area 8). Read-only review against `v1.0/area1-blockstore-audit` @ develop@22f0afd0.

---

## CRITICAL Findings

### C-1: `SetOnChunkComplete` is an unsynchronized write with concurrent readers — data race (HIGH, confidence 95)

**File**: `pkg/blockstore/local/fs/fs.go:748-749`

```go
func (bc *FSStore) SetOnChunkComplete(fn func(...)) {
    bc.onChunkComplete = fn   // bare field write, no lock
}
```

`bc.onChunkComplete` read on hot path in `chunkstore.go:124-125` by rollup workers concurrently with `engine.New` calling `SetOnChunkComplete`. Godoc says "Concurrent installs are not supported" but no lock — `go test -race` fires when setter overlaps StoreChunk.

`SetObjectIDPersister` counterpart correctly uses `bc.persisterMu sync.RWMutex` (fs.go:240-241). `SetOnChunkComplete` must do same. Fix: `sync/atomic.Pointer` or existing `persisterMu`.

**Impact**: data race under any workload calling `BlockStore.Start` concurrently with first rollup. `go test -race` is documented gate in `CLAUDE.md`.

---

### C-2: `applyFileLevelDedupHit` refcount rollback drops errors silently — refcount leak (HIGH, confidence 90)

**File**: `pkg/blockstore/engine/dedup.go:269-273`

```go
for h := range seen {
    if _, derr := m.coordinator.DecrementRefCount(ctx, h); derr != nil {
        logger.Warn("file-level dedup race rollback decrement failed", ...)
    }
}
```

On `ErrObjectIDConflict` race, rollback decrements logged at `Warn` and swallowed — retry then increments updated target's hashes, leaving original target hashes at `refcount+1` permanently if any decrement failed. Per CLAUDE.md invariant 6 unexpected errors log at `Error`. Leaked refcount means CAS objects never GC'd.

Rollback loop at lines 285-291 (non-conflict path) same problem.

**Fix**: promote decrement failures to `Error`, return aggregated error to prevent retry on inconsistent state.

---

### C-3: `gcRootLocks` map grows unbounded — memory leak (MED, confidence 88)

**File**: `pkg/blockstore/engine/gc.go:73-96`

```go
var gcRootLocks = make(map[string]*sync.Mutex)

func acquireGCRootLock(root string) *sync.Mutex {
    gcRootLocksMu.Lock()
    mu, ok := gcRootLocks[key]
    if !ok {
        mu = &sync.Mutex{}
        gcRootLocks[key] = mu
    }
    gcRootLocksMu.Unlock()
    ...
}
```

Map never trimmed. Each unique GCStateRoot path creates permanent entry. `makeTempGCStateRoot()` returns unique temp path per call, so `acquireGCRootLock` called with non-empty path each time — comment claiming serialization for empty-root case is false for temp-dir path.

**Fix**: cap map size or single `sync.Mutex` for temp-root case (when `GCStateRoot == ""`).

---

## Important Findings

### I-1: Tracker #668 root-cause — tree/logIndex divergence wedges rollup permanently (HIGH)

**File**: `pkg/blockstore/local/fs/rollup.go:222-235`

```go
entries := idx.EntriesForInterval(stable.Offset, uint64(stable.Length))
if len(entries) == 0 {
    return fmt.Errorf("rollup: tree/logIndex divergence ...")
}
```

Triggered when `AppendWrite` inserts into interval tree (line 330) but subsequent `logIndices` lookup (lines 338-343) returns `nil` — possible if concurrent `DeleteAppendLog` cleared `bc.logIndices[payloadID]` between lock-release (appendwrite.go:80) and logsMu.RLock (line 338). Interval tree has region but `idx.Append` never called. Error returned at line 234 does NOT consume dirty interval — rollup retries every tick, logs `Error` forever, no progress. Payload wedged until restart.

Second trigger: `ObjectIDPersister` conflict. `engine.go:161-199` calls `fbs.Put` per block row then `bs.coordinator.PersistFileBlocks`. On `ErrObjectIDConflict`, rollup_offset doesn't advance; `StoreChunk` writes at lines 406-413 already fired (CAS files on disk). Next retry hits dedup LRU, calls `AddRef` — FileBlock row may or may not exist depending on partial commit.

**Root cause**: `pkg/blockstore/local/fs/rollup.go:rollupFile` (line 231) + `pkg/blockstore/local/fs/appendwrite.go:AppendWrite` (lines 338-343). Fix: (a) consume divergent interval so doesn't re-trigger, (b) atomic logIndex creation + tree-insertion under per-file mutex.

**Severity**: HIGH.

---

### I-2: Tracker #669 root-cause — `AddRef` on wrong FileBlock row via dedup LRU cross-payload hit (MED)

**File**: `pkg/blockstore/local/fs/rollup.go:383-402`

```go
if _, ok := bc.dedupLRU.Get(h); ok {
    addRefErr := bc.blockStore.AddRef(ctx, h, payloadID, blockRef)
    switch {
    case addRefErr == nil:
        skipStoreChunk = true
    case errors.Is(addRefErr, metadata.ErrUnknownHash):
        // TOCTOU
    default:
        return fmt.Errorf("rollup: AddRef: %w", addRefErr)
    }
}
```

LRU populated at line 411 (`bc.dedupLRU.Put(h, payloadID)`) BEFORE persister writes FileBlock row. Second rollup pass for same payload before first persister completes → `AddRef` hits `ErrUnknownHash`. Fallback to StoreChunk is correct safety but creates spurious round-trip on every hot restart with warm LRU + cold metadata.

More serious: on LRU hit for hash belonging to different file's FileBlock row, `AddRef` succeeds but increments RefCount on WRONG row (GetByHash returns "any" matching row). Original file delete underflows RefCount or never gets decrement it needed.

**Root cause**: `rollupFile` LRU hit path lines 383-403. Fix: (a) don't populate `dedupLRU` until persister confirms row exists, or (b) scope LRU entries to (hash, payloadID) pair + validate ownership in `AddRef`.

**Severity**: MED (ErrUnknownHash handles common crash; wrong-row-owner harder to hit but real).

---

### I-3: Tracker #670 root-cause — engine contribution to NFS COMMIT D-state hang (MED)

**Files**: `pkg/blockstore/local/fs/appendwrite.go:200-211` + `pkg/blockstore/engine/syncer.go:271-279`

Two pressure points:

1. **AppendWrite pressure loop** (appendwrite.go:200-211): `logBytesTotal > maxLogBytes` blocks on `bc.pressureCh` forever unless rollup releases budget. If rollup wedged (#668), no pressure pulse, every subsequent `AppendWrite` blocks permanently in D-state. Only escape: `ctx.Done()` / `bc.done`. NFS session context typically long-lived → WRITE handler hangs in D-state.

2. **Flush/mirrorOnce serialization** (syncer.go:271-279): `Flush` calls `m.uploading.CompareAndSwap(false, true)`. If periodic uploader running long S3 batch, `Flush` returns `Finalized=false` immediately. NFS COMMIT retry in tight loop with background context → `uploading=true` for full S3 pass, every retry returns `Finalized=false`. Not D-state but COMMIT appears stuck.

Full fix in NFS area #4. Engine side: (a) configurable max-wait in pressure loop (none currently — `ensureSpace` has hard-coded 30s, `AppendWrite`'s loop has no deadline), (b) document `Flush` returning `Finalized=false` as "try again" not "not synced".

**Root cause**: `appendwrite.go:AppendWrite` line 202 (missing deadline) + `engine/syncer.go:Flush` line 271 (non-blocking).

**Severity**: MED engine-side; HIGH NFS handler (Area 4).

---

### I-4: `migrate_to_cas.go` swallows errors silently (MED, confidence 85)

**File**: `pkg/blockstore/migrate/migrate_to_cas.go:272, 309`

Line 272: `removeLegacyBlkFiles` failure silent. Comment says "log via journal" but code discards error without Progress callback. .blk cleanup failure (permissions, mounted FS) → operator has no signal. Per CLAUDE.md invariant 6 unexpected errors log at `Error`.

Line 309: `os.Remove(journalPath)` failure silent. `!errors.Is(err, os.ErrNotExist)` guard correct but `_ = err` discards value — non-ErrNotExist (e.g. `EACCES`) silently swallowed.

**Fix**: line 272 progress callback or `Warn` log; line 309 surface to caller.

---

### I-5: `inlineFetchOrWait` local write error silently returns success — silent data loss (MED, confidence 88)

**File**: `pkg/blockstore/engine/fetch.go:347-352`

```go
if writeErr := m.local.Put(ctx, fb.Hash, data); writeErr != nil {
    logger.Warn("inline download: local write failed", ...)
}
completed = true
m.completeInFlight(key, result, nil)
return data, true, nil
```

Local Put failure (disk full) → returns `(data, true, nil)`. Caller `EnsureAvailableAndRead` sees fill success. Bytes never persisted locally → next read after eviction goes back to S3. Under memory pressure `data` slice may be freed by time caller copies. Concurrent waiters via `completeInFlight(nil)` also see success.

Sustained disk-full → same block repeatedly downloaded from S3 without landing on disk.

**Fix**: return write error to caller, propagate to waiters via `completeInFlight(key, result, writeErr)`.

---

### I-6: `SyncNow` spin-waits on `uploading` with 10ms sleeps — CPU + timer waste (MED, confidence 82)

**File**: `pkg/blockstore/engine/syncer.go:648-657`

Active spin-wait. For 30s S3 upload → ~3000 timer allocations via `time.After`. Timers not cancelled → goroutine leaks from finalizer-based cleanup on older Go.

`SyncNow` called from REST `/drain-uploads` + `Close` where blocking acceptable. Use `time.NewTicker` + defer-stop.

---

### I-7: `Walk` uses `io.EOF` as internal stop — breaks callback returning `io.EOF` (MED, confidence 80)

**File**: `pkg/blockstore/local/fs/blockstore_methods.go:174-176`

```go
if errors.Is(cbErr, blockstore.ErrStopWalk) {
    return io.EOF
}
```

Internal sentinel collision. Caller passing `io.EOF` for any reason treated as clean stop. `blockstoretest/conformance.go:testWalkErrorWrap` doesn't test `io.EOF` as callback error — untested contract hole.

---

## Security Findings

### S-1: No BLAKE3 verification on `local.Get` in `mirrorOnce` — CAS integrity not verified at upload (MED, confidence 85)

**File**: `pkg/blockstore/engine/syncer.go:306-318`

```go
data, err := m.local.Get(ctx, hash)
...
if err := m.remoteStore.Put(ctx, hash, data); err != nil { ... }
```

`local.Get` (FSStore.ReadChunk) reads on-disk chunk without verifying `blake3(data) == hash`. Bitrot / partial write bypassing fsync / hw error → silently uploads corrupt bytes to S3, marks synced. CAS key wrong. `ReadBlockVerified` on download detects via `ErrCASContentMismatch` — but data irrecoverably wrong in S3, local may have evicted.

S3 store's `Put` stamps `x-amz-meta-content-hash` with caller-asserted key, not re-verification. Uploader trusted.

**Fix**: `blake3(data) == hash` check in `mirrorOnce` between Get + Put, log at `Error` on mismatch, return error. Cost: 32-byte hash per upload.

---

### S-2: AES-256-GCM nonce — `crypto/rand.Read` 12 bytes per Wrap call (CORRECT, no issue)

`keyprovider/local.go:207-209`. Birthday bound at 2^48 collisions = ~281T encryptions per master key. Fine for any realistic block volume.

---

### S-3: Compression decompression-bomb guard present + correct (no issue)

`compression/decorator.go:97-99` checks `origSize > MaxFramedPlaintextSize` (64 MiB) before alloc. Guard fires before LimitReader.

---

### S-4: `logPath` no `isValidPayloadID` check at write time (MED, confidence 80)

**File**: `pkg/blockstore/local/fs/appendwrite.go:56-57`

```go
func (bc *FSStore) logPath(payloadID string) string {
    return filepath.Join(bc.baseDir, "logs", payloadID+".log")
}
```

`isValidPayloadID` called during recovery scan (recovery.go:254) but NOT at write-time entry to `getOrCreateLog`. `../`-containing payloadID from metadata layer → log file outside `<baseDir>/logs/`. Metadata validates separately but defense-in-depth missing at blockstore boundary.

**Fix**: `isValidPayloadID(payloadID)` guard at top of `getOrCreateLog` (or `AppendWrite`), return `ErrInvalidPayloadID`.

---

## Error Handling

### E-1: `_ = filepath.WalkDir(...)` (recovery.go:229, compaction.go:417, fs.go:538) — LEGIT best-effort walks (no issue)

`seedLRUFromDisk` best-effort startup seed; `cleanupCompactTemps` best-effort cleanup; recovery walk handles per-file errors via walk-func parameter. Outer error `filepath.ErrSkipDir`-shaped, not meaningful. NOT hidden bugs.

---

### E-2: `sync_queue.go:282` `_ = q.processDownload(ctx, req)` — LEGIT prefetch best-effort (no issue)

Documented prefetch; done channel signals nil regardless. Correct by design.

---

### E-3: `%w` discipline — all reviewed error paths conformant. No bare `fmt.Errorf("... %v", err)` in critical paths.

---

## Resource Lifecycle

### R-1: `SyncQueue` goroutine leak on hung S3 + double 30s timeout on Close (MED, confidence 82)

**File**: `pkg/blockstore/engine/sync_queue.go`

`SyncQueue.Start` spawns `uploadWorkers + downloadWorkers` goroutines. `Stop(30s)` from `Syncer.Close()`. But `BlockStore.Close()` runs `DrainAllUploads` (line 799-801) THEN `Stop(defaultShutdownTimeout)` → 60s worst case.

Worse: download goroutine blocked on hung S3 connection — S3 HTTP client `Timeout: 0` at `store.go:131`. `stopCh` close not propagated into `ctx` passed to `processDownload`. Worker leaks until OS-level TCP timeout.

**Fix**: S3 HTTP `Timeout: 0` → per-request context timeout. Worker loop propagates `stopCh` into ctx.

---

## Conformance Suite Findings (`blockstoretest/`)

**Net recommendation: incremental patch, NOT full rewrite.** Suite correctly pins externally-specified behavior (Put/Get round-trip no-alias, ErrChunkNotFound on miss, Walk LastModified non-zero, idempotent Put). Issues are gaps, not misspecification.

### CS-1: MISSING — zero-byte Put

`bs.Put(ctx, h, []byte{})` untested. S3 allows zero-byte; FSStore writes zero-byte file. Implementation-defined. Should assert consistent behavior (`ErrInvalidSize` or idempotent round-trip).

### CS-2: MISSING — GetRange past EOF

`testGetRange` tests offset=4 length=8 into 16-byte payload. No test for `offset > len(data)` or `offset+length > len(data)` (clamp vs error). S3 returns ErrInvalidOffset; FSStore clamps (blockstore_methods.go:68). Backend asymmetry unpinned.

### CS-3: MISSING — concurrent Put-same-hash during Walk

`testPutConcurrent` tests 8 writers + Walk count=1. No test for concurrent Put racing Walk callback — specifically Walk does not return hash twice (S3 paginator race on some backends).

### CS-4: MISSING — Put with wrong hash (content mismatch)

`bs.Put(ctx, differentHash, data)` where `blake3(data) != differentHash` untested. FSStore + S3 both trust caller. Pin "no verify on Put" contract explicitly so implementors know intentional.

### CS-5: REWRITE — `testPutGetRoundtrip` planning artifact

`conformance.go:87`: `"conformance: Put_Get_Roundtrip payload bytes (Phase 17 D-05)"` — planning ID in test payload string. Per `feedback_no_phase_comments_in_code`. Cosmetic.

### CS-6: KEEP — all 10 existing core scenarios correctly specified against BlockStore contract. No impl-only assertions (no Badger-specific error codes). Keep all.

### CS-7: MISSING — restart-mid-flush

No portable scenario for: write → crash simulate (close without Close) → reopen → verify. Tested in `appendlog_internals_test.go` for FSStore but absent from portable suite. S3 doesn't need; alternative local backend would.

### CS-8: MISSING — ref-count underflow on delete-during-dedup

No test: `Put(h, data)` → `IncrementRefCount` → `Delete(h)` underflow. Conformance suite doesn't exercise FileBlockStore refcount surface at all. Intentional (not on `blockstore.BlockStore`) but new FileBlockStore backends can ship without refcount correctness verified.

**Action**: Add CS-1, CS-2, CS-3, CS-4 as new subtests inside `conformance.go`. Fix CS-5 string. Keep CS-6. Accept CS-7 + CS-8 as out-of-scope with documented rationale.

---

## Logging Findings

### L-1: `syncer.syncLocalBlocks` logs mirror-pass failure at `Debug` (MED, confidence 80)

**File**: `pkg/blockstore/engine/upload.go:26`

```go
if err := m.mirrorOnce(ctx); err != nil {
    logger.Debug("Periodic mirror pass failed", "error", err)
}
```

S3 PUT failure mid-mirror unexpected per invariant 6 → should be `Warn` minimum. Health monitor fires on poll interval, not Put failures — one-off S3 error silent for up to one health-check interval.

**Fix**: `Warn` with context (hashes attempted, which hash failed).

---

### L-2: `rollupFile` correctly logs at `Error` on rollupFile failure (conformant)

### L-3: `dispatchRemoteFetch` on zero-hash FileBlock logs at `Error` (conformant)

---

## Convention Adherence

### V-1: Planning ID leaks in source comments (LOW — Wave 0 sweep target)

- `fs.go:115`: `"d /"` (truncated planning ref)
- `fs.go:162-163`: `"(per plan)"` parenthetical
- `rollup.go:138-148`: comments reference `"#588"` + `"per plan"`

These are residual Wave 0 sweep misses per `feedback_no_phase_comments_in_code`.

---

### V-2: `LocalForTest` + `LocalForTest(bs)` duplicate exported functions (LOW, confidence 85)

**File**: `pkg/blockstore/engine/engine.go:859-868`

```go
func (bs *BlockStore) LocalForTest() local.LocalStore { return bs.local }
func LocalForTest(bs *BlockStore) local.LocalStore { return bs.local }
```

Both exported. Godoc "Do not use in production". Identically-named confusing. Free function form exists "when bs receiver shadowed" — test fixture smell. Both should be unexported in `_test.go` helper or collapsed to one.

---

### V-3: `gofumpt` / `go vet` — no issues in reviewed source. Consistently formatted.

---

## HIGH/MED/LOW Triage Table

| ID | Finding | Severity | Confidence |
|----|---------|----------|------------|
| C-1 | `SetOnChunkComplete` unsynchronized — data race | HIGH | 95 |
| C-2 | `applyFileLevelDedupHit` rollback swallows errors — refcount leak | HIGH | 90 |
| I-1 | #668: tree/logIndex divergence wedges rollup permanently | HIGH | 92 |
| S-1 | No BLAKE3 verify on upload — corrupt CAS to S3 | HIGH | 85 |
| C-3 | `gcRootLocks` unbounded map — memory leak | MED | 88 |
| I-2 | #669: AddRef wrong FileBlock row via dedup LRU | MED | 85 |
| I-3 | #670: AppendWrite pressure loop no deadline — D-state | MED | 85 |
| I-4 | migrate_to_cas.go silent error discards | MED | 85 |
| I-5 | `inlineFetchOrWait` local write fail returns success | MED | 88 |
| S-4 | `logPath` no isValidPayloadID at write — path traversal | MED | 80 |
| I-6 | `SyncNow` spin-wait + timer leak | MED | 82 |
| R-1 | SyncQueue 60s close + goroutine leak on hung S3 | MED | 82 |
| L-1 | `syncLocalBlocks` mirror failure at Debug not Warn | MED | 80 |
| I-7 | `Walk` `io.EOF` collision with callback error | MED | 80 |
| CS-1-4 | Conformance gaps: zero-byte/GetRange-EOF/concurrent-Put-Walk/wrong-hash | MED | 85 |
| V-2 | Duplicate `LocalForTest` exported | LOW | 85 |
| CS-5 | Planning ID in test payload string | LOW | 90 |
| V-1 | Planning ID leaks in comments | LOW | 90 |

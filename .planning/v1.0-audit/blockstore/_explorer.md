# Blockstore — Current State (`code-explorer` agent output)

Generated 2026-05-28. Read-only pass over 171 .go files in `pkg/blockstore/`.

---

## 1. WRITE PATH TRACE

Entry: `internal/adapter/common/write_payload.go:WriteToBlockStore:32`.

```
common.WriteToBlockStore
  blockStore.WriteAt(ctx, payloadID, nil /*currentBlocks*/, data, offset)
    engine/engine.go:449 → engine.BlockStore.WriteAt
      → bs.local.AppendWrite(ctx, payloadID, data, offset)   engine.go:453
        FSStore.AppendWrite   local/fs/appendwrite.go:172
          tombstone check   (logsMu RLock)
          pressure loop     (logBytesTotal > maxLogBytes → block on pressureCh)
          getOrCreateLog    (double-checked locking, logsMu)
          per-file mu.Lock
          writeRecord       (CRC32c frame: u32 len | u64 fileOff | u32 crc | []byte)
          groupCommit.Sync  (per-file fsync coalescing, local/fs/groupcommit.go)
          intervalTree.Insert(offset, len, now)
          logIndex.Append(logPos, offset, len)      local/fs/logindex.go
          rollupCh <- payloadID  (non-blocking nudge)
      bs.cache.OnRead(payloadID, nil, 0)  (reset sequential tracker)
      return currentBlocks unchanged
  caller persists via metadataStore.PutFile (same metadata txn)
  CacheInvalidator.InvalidateFile called post-txn
```

**AuthContext**: does NOT thread into blockstore. Blockstore receives only (payloadID, data, offset). Auth + permission checks at metadata layer (`metadataStore.WriteFile`) before blockstore is reached.

**Preop attrs for WCC** (CLAUDE.md invariant 5): `metadataStore.WriteFile` runs before `blockStore.WriteAt`. Blockstore has no visibility into WCC.

**currentBlocks**: nil at all current call sites (`common.WriteToBlockStore:42`). WriteAt returns them unchanged. Canonical `[]BlockRef` projection happens at rollup time.

### Async rollup (`local/fs/rollup.go`)

```
FSStore.StartRollup   rollup.go:39  → spawns rollupWorkers goroutines

chunkRollupWorker   rollup.go:62
  reads rollupCh OR ticker (stabilization window)
  → FSStore.rollupFile(ctx, payloadID)   rollup.go:151

rollupFile
  logsMu.RLock → snapshot lf, tree, mu, idx
  per-file mu.Lock  (held through entire sequence — Blocker-3 fix)
  tombstone re-check
  tree.EarliestStable → (offset, length, touched) stable interval
  readLogHeader
  idx.EntriesForInterval → []logIndexEntry{logPos, fileOff, payloadLen}
  pread each entry → []rec{off, payload, endPos}
  truncation filter
  reconstructStream  (merge records; last-write-wins per offset)
  → chunker.Next loop (FastCDC, 1–16 MiB chunks, rollup.go:352+)
      for each chunk (hash, chunkBytes, offset):
        dedupLRU.Get(hash)
          hit:  blockStore.AddRef(ctx, hash, payloadID, blockRef)
          miss: FSStore.StoreChunk(ctx, hash, chunkBytes)   chunkstore.go
                  os.CreateTemp(.tmp) → write → fsync → rename → fsync parent
                  lruTouch(hash, size, path)
                  onChunkComplete(hash, data, path) → bs.cache.Put(hash, data)
                dedupLRU.Put(hash, payloadID)
        blocks = append(blocks, BlockRef{hash, offset, size})
  tombstone re-check  (Blocker-3 post-chunker guard)
  for consumedExtent: idx.MarkConsumed
  targetPos = idx.AdvanceFence
  objectID = blockstore.ComputeObjectID(blocks)   BLAKE3(prefix||h0||…||hN)
  objectIDPersister(ctx, payloadID, blocks, objectID)
    → coordinator.PersistFileBlocks   shares/coordinator.go:80+
      resolveStore (tx-aware: metadata.TxFromContext or metadataStore)
      writes per-chunk FileBlock rows (blockStore.Put)
      updates FileAttr.Blocks + FileAttr.ObjectID in one txn
  rollupStore.SetRollupOffset(payloadID, targetPos)
  advanceRollupOffset
  tree.ConsumeUpTo(stableEnd)
  logBytesTotal.Add(-reclaimed)
  pressureCh <- {}   (non-blocking unblock)
  maybeCompactLog
```

### Flush/Sync (NFS COMMIT / SMB CLOSE → `common.CommitBlockStore:56`)

```
engine.BlockStore.Flush   engine.go:702
  if coordinator != nil:
    if size ≤ chunker.MinChunkSize (1 MiB):
      tryEagerSmallFileDedup   engine/dedup.go:47
        BLAKE3(data) → provisional ObjectID
        coordinator.FindByObjectID → target []BlockRef
        if hit: applyFileLevelDedupHit → return Finalized=true
    snapshotPendingBlockRefs   engine/syncer.go:347
    if len(specBlocks) > 0:
      trySpeculativeFileLevelDedup   engine/dedup.go:149
        ComputeObjectID(specBlocks) → provisional
        coordinator.FindByObjectID → target
        if hit: applyFileLevelDedupHit (IncrementRefCount + PersistFileBlocks +
                  DecrementRefCount on orphans + cache.InvalidateFile +
                  local.DeleteLog) → return Finalized=true
  → syncer.Flush(ctx, payloadID)   engine/syncer.go:246
      local.SyncFileBlocksForFile (flush queued pendingFBs)
      if no remote or !IsRemoteHealthy → return Finalized=false
      uploading.CompareAndSwap gate
      mirrorOnce(ctx)   syncer.go:299
        mu.RLock → snapshot syncedHashStore
        for hash, err := range local.ListUnsynced(ctx):
          data := local.Get(ctx, hash)
          remoteStore.Put(ctx, hash, data)
          hashStore.MarkSynced(ctx, hash)
      return Finalized=true
```

---

## 2. READ PATH TRACE

Entry: `internal/adapter/common/resolve.go:ResolveForRead:24` → handler calls `blockStore.ReadAt`.

```
engine.BlockStore.ReadAt   engine.go:371
  → readAtInternal(ctx, payloadID, data, offset)   engine.go:1040

    PRIMARY: local.ReadPayloadAt(ctx, payloadID, dest, offset)
               local/fs/readpayload.go:41
      Step 1: replayLogIntoDest
               scan log records ∩ [offset, end); last-record-wins
      Step 2: fillFromCASManifest
               blockStore.ListFileBlocks(payloadID) → []FileBlock sorted by offset
               for each overlapping row: local.Get(ctx, hash) → ReadChunk
      if !allCovered → return (0, ErrFileBlockNotFound)

    on ErrFileBlockNotFound:
      readLocalByHash(ctx, payloadID, dest, offset)   engine.go:1119
        fileBlockStore.ListFileBlocks(payloadID)
        findRowCoveringOffset(rows, target)
        local.Get(ctx, hash)

    on !found:
      syncer.EnsureAvailableAndRead(ctx, payloadID, offset, length, dest)
                                    engine/fetch.go:201
        blockRange → startIdx, endIdx
        allBlocksLocal check
        if !IsRemoteHealthy → ErrRemoteUnavailable
        for blockIdx in [start, end]:
          if blockIsLocal → needLocalReadAt=true; continue
          inlineFetchOrWait   fetch.go:267
            inFlight map dedup (broadcast-channel fetchResult)
            fetchBlock   fetch.go:123
              resolveFileBlock → ListFileBlocks → findRowCoveringOffset
              dispatchRemoteFetch   fetch.go:95
                remoteStore.ReadBlockVerified(hash, hash)
                  s3: streaming verifyingReader (verifier.go) BLAKE3 inline
                  → ErrCASContentMismatch on mismatch
              local.Put(ctx, hash, data) → StoreChunk (cache download)
            copyBlockToDest → direct copy into dest
        enqueuePrefetch(payloadID, endIdx+1..+PrefetchBlocks)
        if needLocalReadAt → return (false, nil) → caller re-tries ReadPayloadAt

  cache.OnRead(payloadID, hashes, fileSize)  — hint-only, prefetch trigger
    seqTracker: after seqThreshold (3) → schedule prefetch workers
```

**Cache** (`engine/cache.go`): CAS-keyed LRU, max `readBufferBytes`. Hint-only on the read path; only `EnsureAvailableAndRead` returns filled=true. Warmed by `onChunkComplete` (write) and prefetch (read). `nullCache{}` when budget=0.

---

## 3. GC MARK-SWEEP FLOW

Entry: `pkg/controlplane/runtime/blockgc.go:Runtime.RunBlockGC:35`.

```
Runtime.RunBlockGC
  sharesSvc.DistinctRemoteStores() → []RemoteStoreEntry (deduped by configID)
  if len(entries) == 0 → no-op
  for entry:
    perRemoteReconciler{rt, shares}   impl engine.MultiShareReconciler
    opts.HoldProvider = snapshotHoldForRemote(shares)   SnapshotHoldProvider
    engine.CollectGarbage(ctx, entry.Store, rec, opts)   engine/gc.go:220

CollectGarbage   engine/gc.go:220
  acquireGCRootLock(gcStateRoot)  — per-root sync.Mutex
  CleanStaleGCStateDirs  — reclaims dirs with incomplete.flag
  NewGCState(gcStateRoot, runID) → opens per-run Badger under gc-state/<runID>/db/
  MARK: markPhase   gc.go:350
    sharesForReconciler → reconciler.SharesForGC()
    if len(shares) == 0 → return error (fail-closed)
    for shareName:
      store = reconciler.GetMetadataStoreForShare(shareName)
      store.EnumerateFileBlocks(ctx, addHash)
        addHash: gcs.Add(h) (Badger WriteBatch, 1000 hashes/flush)
    if holdProvider != nil:
      holdProvider.HeldHashes(ctx, remoteEndpointID, shares, addHash)
        SnapshotHoldProvider   runtime/snapshot_hold.go:31
          for share: store.ListSnapshots (state=ready) → streamManifest →
            snapshot.ReadManifest → HashSet.ForEach → fn(hash)
    gcs.FlushAdd()
  on markPhase error → return early (fail-closed)

  SWEEP: sweepPhase   gc.go:423
    remoteStore.Walk(ctx, func(hash, meta)):
      if meta.LastModified.IsZero() → capture error, skip (fail-closed)
      if meta.LastModified > snapshotTime − gracePeriod → skip (grace)
      present, _ := gcs.Has(hash)
      if present → skip
      if !present && !dryRun:
        remoteStore.Delete(ctx, hash)
        stats.ObjectsSwept++; stats.BytesFreed += meta.Size

  gcs.MarkComplete()  — removes incomplete.flag
  PersistLastRunSummary → <gcStateRoot>/last-run.json
```

**Fail-closed invariants**: (1) mark error aborts sweep; (2) zero LastModified → error+skip; (3) zero shares → refuse sweep; (4) HoldProvider error → abort mark. Cross-process safety per-process only (gcRootLocks map).

---

## 4. PER-SHARE LIFECYCLE

### AddShare (`shares/service.go:309`)

```
Phase 1: prepareShare
  metadataStore.CreateRootDirectory → *FileAttr
  metadata.EncodeFileHandle(rootFile) → rootHandle
  build Share struct

Phase 2: createBlockStoreForShare   service.go:478
  CreateLocalStoreFromConfig   service.go:1451
    case "fs": fs.NewWithOptions(...)
        checkLegacyLayoutSentinel(.cas-migrated-v1)
        seedLRUFromDisk (alphabetical blocks/ walk)
      store.StartRollup(ctx)
    case "memory": localmemory.New()

  acquireRemoteStore(ctx, configID, provider)   service.go:611
    double-checked locking on s.remoteStores[configID]
    CreateRemoteStoreFromConfig → s3.NewFromConfig or remotememory.New
    maybeWrapEncryption(inner, cfg) → encryption.NewRemote (inner)
    maybeWrapCompression(inner, cfg) → compression.NewRemote (outer)
    s.remoteStores[configID] = &sharedRemote{store, refCount:1}
    wraps in nonClosingRemote{}

  localStore.SetEvictionEnabled(remoteStore != nil && policy != Pin)
  localStore.SetRetentionPolicy(policy, ttl)

  newMetadataCoordinator(metadataStore)   shares/coordinator.go:43

  engine.New(cfg)   engine/engine.go:107
    installs SetObjectIDPersister closure (engine.go:157)
    installs SetOnChunkComplete closure (engine.go:220)
    cfg.Syncer.bs = bs

  bs.Start(ctx)   engine/engine.go:275
    FSStore.Recover(ctx)   local/fs/recovery.go:68
      blocks/ walk: rebuild diskIndex, revert Syncing→Pending
      recoverAppendLogs: scan logs/*.log, truncate at bad CRC, rebuild trees + idx
    local.Start (200ms pendingFBs drain goroutine)
    syncer.Start: queue.Start + recoverStaleSyncing + startHealthMonitor +
                  startPeriodicUploader (mirrorOnce every 2s)
    health callback: unhealthy → SetEvictionEnabled(false); healthy → true
    if readBufferBytes > 0: NewCache(...) replaces nullCache{}

Phase 3: metadataSvc.RegisterStoreForShare
Phase 4: s.registry[name] = share + notifyShareChange
```

### RemoveShare (`service.go:759`)

```
extract bs, remoteConfigID, localStoreDir → delete from registry
os.RemoveAll(localStoreDir/snapshots)
bs.Close()
  cache.Close
  syncer.Close (close stopCh, healthMonitor.Stop, DrainAllUploads 30s, queue.Stop 30s)
  local.Close (rollupWg.Wait, SyncFileBlocks, close log fds)
  remote.Close → no-op (nonClosingRemote)
releaseRemoteStore(remoteConfigID) → refCount--; if 0 → actual remote.Close
notifyShareChange
```

**Remote ref-counting**: `s.remoteStores[configID] → *sharedRemote{store, refCount}`. Engine receives `nonClosingRemote{}`.

**Local isolation**: FSStore always its own dir `<basePath>/shares/<url.PathEscape(name)>/`. Per CLAUDE.md invariant 4.

**Per-share transform stack** (outer→inner):
```
compression.Decorator → encryption.EncryptedRemote → s3.Store (or memory.Store)
```
CAS key (BLAKE3 plaintext hash) preserved through all layers.

---

## 5. ARCHITECTURE MAP

```
cmd/dfs
  └─ pkg/controlplane/runtime/          ← single entrypoint (CLAUDE.md inv. 1)
       └─ pkg/controlplane/runtime/shares
            ├─ pkg/blockstore/engine/        (orchestrator)
            │    ├─ pkg/blockstore/local/    (LocalStore interface)
            │    │    └─ local/fs/           (FSStore prod)
            │    │    └─ local/memory/       (MemoryStore test)
            │    ├─ pkg/blockstore/remote/   (RemoteStore interface)
            │    │    └─ remote/s3/          (S3 prod)
            │    │    └─ remote/memory/      (in-memory test)
            │    ├─ pkg/blockstore/          (types/interfaces/errors)
            │    ├─ pkg/blockstore/chunker/  (FastCDC)
            │    ├─ pkg/metadata/            (justified in gc.go + audit_state.go)
            │    └─ internal/logger          (11 prod files)
            ├─ pkg/blockstore/compression/   (Decorator)
            └─ pkg/blockstore/encryption/    (EncryptedRemote)
                 └─ encryption/keyprovider/

internal/adapter/common      → pkg/blockstore/engine + pkg/metadata
internal/adapter/nfs/v3+v4   → internal/adapter/common
internal/adapter/smb/v2      → internal/adapter/common

pkg/blockstore/blockstoretest/  (conformance suite, test-only callers)
pkg/blockstore/migrate/         (one-shot migration tool, standalone)
```

**Layer violations**: None in prod paths. `engine/gc.go:45` + `engine/audit_state.go:36` import `pkg/metadata` — both carry `// justification:` godoc. One-directional; metadata does NOT import blockstore.

---

## 6. PUBLIC SURFACE

| Symbol | Kind | Purpose | External callers |
|---|---|---|---|
| `BlockStore` | interface | CAS CRUD Put/Get/GetRange/Has/Delete/Head/Walk | local.LocalStore (embeds), remote.RemoteStore (compatible) |
| `BlockStoreAppend` | interface | BlockStore + AppendWrite/DeleteLog | local.LocalStore (embeds), local/fs.FSStore |
| `Reader` | interface | ReadAt/GetSize/Exists | engine.BlockStore (impl) |
| `Writer` | interface | WriteAt/Truncate/Delete/CopyPayload | engine.BlockStore (impl) |
| `Flusher` | interface | Flush/DrainAllUploads | engine.BlockStore (impl) |
| `Store` | interface | Reader+Writer+Flusher+lifecycle | blockstoreprobe (compile-time only) |
| `FileBlockStore` | interface | Block CRUD for engine | metadata backends, engine coordinator |
| `EngineFileBlockStore` | interface | FileBlockStore + GetFileBlock/ListFileBlocks | engine.Syncer, local/fs.FSStore |
| `ContentHash` | type | [32]byte BLAKE3 | everywhere |
| `BlockRef` | struct | {Hash, Offset, Size} chunk manifest | engine, metadata, adapters |
| `FileBlock` | struct | Block row | metadata backends, engine |
| `BlockState` | type | Pending/Syncing/Remote | everywhere |
| `ObjectID` | type alias | = ContentHash | engine dedup, metadata |
| `Meta` | struct | {Size, LastModified} for GC | remote stores, GC |
| `Stats` | struct | Used/Total/Available/ContentCount/AverageSize | engine.BlockStore.Stats() |
| `FlushResult` | struct | {Finalized bool} | engine, common.CommitBlockStore |
| `BlockSize` | const | 8 MiB | engine, local/fs, s3 |
| `HashSize` | const | 32 | local/fs, migration |
| `FormatCASKey` | func | ContentHash → "cas/{hh}/{hh}/{hex}" | engine, s3, GC |
| `ParseCASKey` | func | inverse | s3, migration |
| `ParseBlockID` | func | "{payloadID}/{blockIdx}" → (string, uint64, error) | local/fs recovery |
| `ComputeObjectID` | func | BLAKE3 Merkle root | engine dedup, rollup |
| `RetentionPolicy` + consts | type/consts | pin/ttl/lru | shares.Service, pkg/config |
| `DeduceDefaults` + `DeducedDefaults` | func/struct | sysinfo sizing | cmd/dfs, pkg/config |
| `HashSet` | struct | ContentHash set (snapshot manifest) | pkg/snapshot |
| `ErrChunkNotFound`, `ErrBlockNotFound`, `ErrFileBlockNotFound`, `ErrUnknownHash`, `ErrRemoteUnavailable`, `ErrCASContentMismatch`, `ErrCASKeyMalformed`, `ErrLegacyLayoutDetected`, `ErrBlockRefMissing`, `ErrStopWalk` | vars | error sentinels | engine, adapters, GC |
| `BlockStoreError` + `NewBlockStoreError` | struct/func | Rich error context | **NO production callers — test only** |
| `RemoteStoreSweepSurface` | interface | **documentation-only — zero callers** | doc only |
| `RemoteObjectInfo` | struct | **documentation-only — zero callers** | doc only |
| `SystemDetector` | interface | Duck-typed sysinfo workaround | pkg/config wiring |
| `ParseRetentionPolicy`, `ValidateRetentionPolicy`, `FormatBytes`, `ClampToInt64` | funcs | helpers | pkg/config, REST |

---

## 7. INTERFACE INVENTORY

| Interface | Pkg | Impls | Consumers |
|---|---|---|---|
| `blockstore.BlockStore` | root | `local/fs.FSStore`, `remote/s3.Store`, `remote/memory.Store` | LocalStore embeds, RemoteStore structurally identical |
| `blockstore.BlockStoreAppend` | root | `local/fs.FSStore` only | LocalStore embeds |
| `blockstore.Reader` | root | engine.BlockStore | adapters via concrete type |
| `blockstore.Writer` | root | engine.BlockStore | same |
| `blockstore.Flusher` | root | engine.BlockStore | same |
| `blockstore.Store` | root | engine.BlockStore | blockstoreprobe (compile-time only) |
| `blockstore.FileBlockStore` | root | All 3 metadata backends | engine coordinator, Syncer |
| `blockstore.EngineFileBlockStore` | root | Same 3 backends | engine.Syncer, local/fs.FSStore |
| `local.LocalStore` | local | FSStore, MemoryStore | engine.BlockStore, engine.Syncer |
| `remote.RemoteStore` | remote | s3.Store, remote/memory.Store, encryption.EncryptedRemote, compression.Decorator | engine.Syncer, engine.CollectGarbage, shares.Service |
| `engine.MetadataCoordinator` | engine | shares.metadataCoordinator (1 prod) | engine.BlockStore, Syncer |
| `engine.CacheInterface` | engine | *engine.Cache, nullCache{} | engine.BlockStore |
| `engine.MetadataReconciler` | engine | runtime.perRemoteReconciler (via MultiShareReconciler) | engine.CollectGarbage |
| `engine.MultiShareReconciler` | engine | runtime.perRemoteReconciler | engine.CollectGarbage |
| `engine.HoldProvider` | engine | runtime.SnapshotHoldProvider | engine.markPhase |
| `keyprovider.KeyProvider` | encryption/keyprovider | localProvider, kmipProvider | encryption.EncryptedRemote |
| `blockstoretest.Factory` + `AppendFactory` | blockstoretest | test-supplied | conformance suite |
| `shares.MetadataStoreProvider` + `BlockStoreConfigProvider` + `ShareStore` + `MetadataServiceRegistrar` | shares | stores.Service, store.GORMStore, metadata.MetadataService | shares.Service.AddShare/RemoveShare |
| `common.BlockStoreRegistry` + `CacheInvalidator` | adapter/common | *runtime.Runtime, *engine.BlockStore (structural) | common.ResolveForRead/Write |

**Single-impl interfaces** (collapse candidates per PLAN.md §3):
- `engine.MetadataCoordinator` — 1 prod (`shares.metadataCoordinator`)
- `engine.HoldProvider` — 1 prod (`runtime.SnapshotHoldProvider`)
- `engine.MultiShareReconciler` — 1 prod (`runtime.perRemoteReconciler`)
- `engine.MetadataReconciler` — only consumed via MultiShareReconciler

---

## 8. `pkg/` → `internal/` IMPORTS

Source files only (no tests):

```
pkg/blockstore/engine/engine.go        → internal/logger
pkg/blockstore/engine/syncer.go        → internal/logger
pkg/blockstore/engine/dedup.go         → internal/logger
pkg/blockstore/engine/fetch.go         → internal/logger
pkg/blockstore/engine/upload.go        → internal/logger
pkg/blockstore/engine/sync_queue.go    → internal/logger
pkg/blockstore/engine/sync_health.go   → internal/logger
pkg/blockstore/local/fs/appendwrite.go → internal/logger
pkg/blockstore/local/fs/recovery.go    → internal/logger
```

Plus test:
```
pkg/blockstore/engine/perf_bench_helpers_test.go → internal/logger
pkg/blockstore/local/fs/recovery_test.go         → internal/logger
```

`internal/logger` = thin `log/slog` wrapper. No other `internal/` sub-packages imported.

Under (a) ("no pkg/ → internal/") all 9 prod imports would be findings. Under (b) or (c) acceptable.

---

## 9. RESTART-SURVIVAL SURFACES

| State | Location | Key | Lazy reconstruct? |
|---|---|---|---|
| CAS chunk bytes | `<shareDir>/blocks/<hh>/<hh>/<hex>` | ContentHash hex (2-level shard) | NO — authoritative |
| Append log bytes | `<shareDir>/logs/<payloadID>.log` | payloadID | Recovery reconciles header vs metadata fence |
| FileBlock rows | Metadata backend | `"{payloadID}/{chunkOffset}"` | NO — source of truth |
| rollup_offset | Metadata RollupStore | payloadID | NO — monotone fence |
| Synced-hash markers | Metadata SyncedHashStore | ContentHash | NO (badger/postgres); lost on memory |
| `.cas-migrated-v1` sentinel | `<shareDir>/.cas-migrated-v1` | single file | NO — boot guard |
| GC live set (per-run) | `<shareDir>/gc-state/<runID>/db/` (Badger) | ContentHash | Ephemeral; stale dirs swept |
| GC last-run summary | `<shareDir>/gc-state/last-run.json` | single file | NO — operator visibility |
| Audit last-run | `<shareDir>/audit-state/last-inv02.json` | single file | NO |
| Migration journal | `<shareDir>/.dittofs-migrate-to-cas.state` | JSONL | NO — resumability |
| In-mem LRU index | `FSStore.lruIndex` | ContentHash → *list.Element | YES — `seedLRUFromDisk` alphabetical |
| In-mem diskIndex | `FSStore.diskIndex` (sync.Map) | blockID → *FileBlock | YES — `Recover` walks blocks/ |
| In-mem file sizes | `FSStore.files` | payloadID → fileInfo | YES — `Recover` rebuilds from DataSize |
| In-mem interval trees | `FSStore.dirtyIntervals` | payloadID → intervalTree | YES — `recoverAppendLogs` |
| In-mem logIndices | `FSStore.logIndices` | payloadID → logIndex | YES — `recoverAppendLogs` |
| Engine Cache | `engine.BlockStore.cache` | ContentHash → []byte | YES — nullCache{} at start |
| Dedup LRU | `FSStore.dedupLRU` | ContentHash → payloadID | YES — RAM-only |
| S3 remote objects | S3 `cas/<hh>/<hh>/<hex>` | FormatCASKey(hash) | Native S3 durability |

Memory backend loses all on restart. GCState Badger ephemeral.

---

## 10. SUB-PACKAGE SIZE TABLE

| Sub-package | Src | Test | Src LoC | Test LoC | Interfaces | Exports |
|---|---|---|---|---|---|---|
| `pkg/blockstore/` (root) | 12 | 9 | ~2900 | ~600 | 8 | ~45 |
| `pkg/blockstore/engine/` | 18 | 28 | ~5000 | ~3500 | 5 | ~30 |
| `pkg/blockstore/local/` | 2 | 0 | ~180 | 0 | 1 | ~3 |
| `pkg/blockstore/local/fs/` | 22 | 28 | ~5200 | ~5500 | — | ~15 |
| `pkg/blockstore/local/memory/` | 3 | 2 | ~450 | ~200 | — | ~5 |
| `pkg/blockstore/remote/` | 2 | 0 | ~165 | 0 | 1 | ~2 |
| `pkg/blockstore/remote/s3/` | 2 | 2 | ~665 | ~300 | — | ~5 |
| `pkg/blockstore/remote/memory/` | 2 | 2 | ~300 | ~150 | — | ~5 |
| `pkg/blockstore/chunker/` | 4 | 1 | ~155 | ~150 | — | ~5 |
| `pkg/blockstore/compression/` | 5 | 5 | ~575 | ~300 | — | ~6 |
| `pkg/blockstore/encryption/` | 5 | 3 | ~475 | ~200 | — | ~6 |
| `pkg/blockstore/encryption/keyprovider/` | 3 | 2 | ~315 | ~150 | 1 | ~8 |
| `pkg/blockstore/blockstoretest/` | 3 | 0 | ~620 | 0 (is test) | — | ~5 |
| `pkg/blockstore/migrate/` | 5 | 3 | ~690 | ~400 | — | ~8 |
| **Total** | **88** | **83** | **~17,690** | **~11,450** | **16** | **~148** |

---

## Surprises (called out by explorer)

- `BlockStoreError` + `NewBlockStoreError` have **zero production callers** — test-only despite exports.
- `RemoteStoreSweepSurface` + `RemoteObjectInfo` are **documentation-only** with zero callers anywhere.
- `engine.Syncer.Truncate` + `engine.Syncer.Delete` are **no-op stubs** post-CAS migration — GC handles cleanup.
- `common.WriteToBlockStore` passes **nil currentBlocks** at every call site — dual-read shim; BlockRef threading incomplete.
- AuthContext never reaches blockstore — auth enforced strictly at metadata layer.

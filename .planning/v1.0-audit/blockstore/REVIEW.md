# Area 1 — Blockstore + CAS + Engine + GC — PR-A Audit (REVIEW.md)

**Status**: AUDIT COMPLETE — awaiting PR-B kickoff approval.
**Branch**: `v1.0/area1-blockstore-audit` @ `origin/develop@22f0afd0`.
**Date**: 2026-05-28.
**Scope**: `pkg/blockstore/` — 171 .go files (~17.7k src + ~11.5k test LoC, explorer measured; PLAN.md headline figure 19.3K/20.7K is approximate). GC bundled (`engine/gc*.go`). Conformance suite (`blockstoretest/`) audited as subject.
**Excludes**: Syncer deep audit (area #2). Backup/snapshot lifecycle (area #8 — only HoldProvider integration touched).

**Agent outputs**:
- `_explorer.md` — current state, deps, public surface
- `_architect.md` — structural findings + collapse candidates
- `_reviewer.md` — bug + security + quality findings

This REVIEW.md consolidates the three.

---

## 1. Current State (summarized from `_explorer.md`)

### Write path
`common.WriteToBlockStore` → `engine.BlockStore.WriteAt` → `FSStore.AppendWrite` (CRC32c framed log) → async `chunkRollupWorker` → FastCDC chunking → `StoreChunk` (atomic tmp + rename) → `objectIDPersister` → metadata coordinator `PersistFileBlocks` → `rollupStore.SetRollupOffset`. **AuthContext never reaches blockstore** — enforced at metadata layer. `currentBlocks` is nil at every call site (BlockRef threading incomplete).

### Read path
`engine.BlockStore.ReadAt` → `local.ReadPayloadAt` (replay log + walk FileBlock manifest) → on `ErrFileBlockNotFound` fall back to `readLocalByHash` → on miss escalate to `syncer.EnsureAvailableAndRead` → `remoteStore.ReadBlockVerified` (streaming BLAKE3 verify) → cache result. Cache is hint-only on the read path; only `EnsureAvailableAndRead` returns direct-served bytes.

### GC mark-sweep
`Runtime.RunBlockGC` per distinct `RemoteStoreEntry` → `engine.CollectGarbage` → per-run Badger live-set under `gc-state/<runID>/db/` → `markPhase` (enumerate FileBlocks + HoldProvider HeldHashes) → `sweepPhase` (`remoteStore.Walk` + grace-window filter + delete). Fail-closed on: zero shares, zero LastModified, mark error, HoldProvider error.

### Per-share lifecycle
- AddShare: prepareShare → `CreateLocalStoreFromConfig` → `acquireRemoteStore` (ref-counted by configID, wrapped in `nonClosingRemote{}`) → `engine.New` (installs ObjectIDPersister + OnChunkComplete) → `bs.Start` (Recover + Start syncer + health monitor + periodic uploader).
- RemoveShare: `bs.Close` (cache → syncer → local → no-op remote) → `releaseRemoteStore` (refCount-- → actual remote.Close at zero).
- Per-share transform stack outer→inner: `compression.Decorator → encryption.EncryptedRemote → s3.Store`. CAS key (BLAKE3 plaintext) preserved through layers.

### Package deps
171 files, 16 interfaces, ~148 exported symbols. `pkg/blockstore` → `internal/logger` 9 source-file imports (sole `pkg/` → `internal/` edges). `engine/` → `pkg/metadata` justified in-file in `gc.go` + `audit_state.go`. **No layer violations** vs CLAUDE.md invariant 1.

### Restart-survival
Authoritative on-disk state: CAS chunks (`blocks/<hh>/<hh>/<hex>`), append logs (`logs/<payloadID>.log`), FileBlock rows, rollup_offset, synced-hash markers. Reconstructed lazily at Recover: diskIndex, file sizes, interval trees, logIndices, LRU index, dedup LRU, engine Cache.

---

## 2. Structural Findings (from `_architect.md` + cross-corroborated against `_explorer.md`)

### 2a. Dead exports (zero callers, confirmed)
| Symbol | File:line | Action |
|---|---|---|
| `RemoteObjectInfo` | `store.go:253` | DELETE |
| `RemoteStoreSweepSurface` | `store.go:269` | DELETE |
| `blockstore.Reader` | `store.go:162` | DELETE |
| `blockstore.Writer` | `store.go:181` | DELETE |
| `blockstore.Flusher` | `store.go:204` | DELETE |
| `blockstore.Store` | `store.go:215` | DELETE (keep `FlushResult`, `Stats`) |
| `BlockStoreError` + `NewBlockStoreError` | `store.go` / `errors.go` | DELETE — test-only despite export |
| `engine.LocalForTest` (pkg func) | `engine/engine.go:868` | DELETE (duplicate of method) |
| `engine.Options.SweepConcurrency` | `engine/gc.go:126` | DELETE — accepted-and-ignored |

### 2b. Collapse candidates (single-impl interfaces or one-line shims)
| ID | Target | Action |
|---|---|---|
| C-03 | duplicate `parseChunkOffsetFromID` / `parsePayloadOffsetFromBlockID` | MOVE to `blockstore` root as `ParseChunkOffset`, delete dups |
| C-04 | dual sentinels `ErrBlockNotFound` + `ErrChunkNotFound` | MERGE → `ErrChunkNotFound` (matches conformance) |
| C-05 | `engine.BlockStoreStats` + `apiclient.BlockStoreStats` | type alias |
| C-06 | 5-function FSStore constructor chain | collapse to `NewWithOptions` + `NewFSStoreForMigration` |
| C-07 | `DeleteLog` → `DeleteAppendLog` shim | rename + delete shim |
| C-08 | `LocalForTest` ×2 + `RemoteForTesting` | pkg func DELETE; move methods to `_test.go` |
| C-09 | `RemoteStore` re-declares 6 `BlockStore` methods | embed `blockstore.BlockStore` + 3 extras |
| C-10 | `NewDownloadRequest` / `NewPrefetchRequest` / `NewBlockUploadRequest` | inline struct literals (3 callsites) |
| C-11 | `Options.SweepConcurrency` accepted-and-ignored | delete field + param + clamping |
| C-12 | `GCStats` legacy alias fields | `// Deprecated:` (REST compat hold) |
| C-13 | `SystemDetector` duck-type workaround | resolve in (a)/(b)/(c) decision |
| C-14 | `EvictReadBuffer` misleadingly named (destroys cache) | rename `DestroyCache` or inline at sole caller |
| L-01 | `EngineFileBlockStore` impl-located, not consumer-located | evaluate `FileAttr.Blocks` replacement or move to `engine/` unexported |
| L-02 | `engine.CacheInterface` exported but engine-internal | unexport → `cacheInterface` |
| Single-impl | `engine.MetadataCoordinator` (1 prod: `shares.metadataCoordinator`) | consider inline |
| Single-impl | `engine.HoldProvider` (1 prod: `runtime.SnapshotHoldProvider`) | keep (cross-pkg needed) |
| Single-impl | `engine.MultiShareReconciler` (1 prod: `runtime.perRemoteReconciler`) | keep (cross-pkg needed) |
| Anonymous probes | `engine.New` does 3 `cfg.Local.(interface{...})` checks | formalize as named `ChunkLifecycleHooks` interface in `local/` |

### 2c. File-layout findings
| ID | Finding | Action |
|---|---|---|
| F-01 | `engine/engine.go` is 1239-line god file | SPLIT: `engine.go` + `readwrite.go` + `flush.go` + `stats.go` + `health.go` + `read_internal.go` |
| F-02 | `store.go` mixes 4 concerns | After C-01/C-02, rename → `fileblock.go` |
| F-03 | `engine/sync_entry.go` (46 lines, after C-10) | merge into `engine/types.go` |
| F-04 | `local/fs/manage.go` (31 lines, 2 admin methods) | merge into `fs.go` or rename `admin.go` |

### 2d. Naming pass (PR-B candidates)
- N-01 (HIGH) `engine.Config` → `engine.BlockStoreConfig` (asymmetry vs `SyncerConfig`)
- N-02 (MED) `engine.BlockStore` → `engine.Store` (eliminates stutter vs `blockstore.BlockStore`)
- N-03 ACCEPT: `FSStore` receiver `bc` consistent throughout
- N-04 (MED) Deprecate `HealthCheck(ctx) error`; standardize on `Healthcheck(ctx) health.Report`
- N-05 (LOW) `gcRootLocks` invisible memory leak from name (real bug, see §3 C-3)
- N-06 (LOW) `local/fs/blockstore_methods.go` → `cas_adapter.go` after C-07

### 2e. Compression/encryption ordering — undefined contract
**Finding (MED)**: required composition order `compression(remote)` then `encryption(compression)` is not documented in either decorator. Encrypt-after-compress correct; reverse leaks ciphertext incompressibility. No code-level guard. **Action**: add doc comment in both decorators stating required composition order.

### 2f. `pkg/` ↔ `internal/` — DATA POINT for runtime area #7
9 source files in `engine/` + `local/fs/` import `internal/logger`. **Only `internal/` package imported from `pkg/blockstore`.** Under (a) → all 9 are findings; under (b)/(c) → no action. **Architect recommendation**: option (b) for DittoFS as app.

---

## 3. Bug Findings (from `_reviewer.md`)

### 3a. CRITICAL / HIGH
| ID | File:line | Finding | Conf |
|---|---|---|---|
| **C-1** | `local/fs/fs.go:748` | `SetOnChunkComplete` unsynchronized bare field write while rollup workers read it on hot path — `go test -race` data race | 95 |
| **C-2** | `engine/dedup.go:269,285` | `applyFileLevelDedupHit` rollback decrements logged at `Warn` and swallowed → permanent refcount leak + un-GC'd CAS | 90 |
| **I-1** | `local/fs/rollup.go:222,338`, `appendwrite.go:330` | **#668 root cause** — tree/logIndex divergence wedges rollup permanently; divergent interval not consumed from tree → infinite retry, `Error` log loop, payload wedged until restart. ObjectIDPersister conflict is second trigger. | 92 |
| **S-1** | `engine/syncer.go:306` | No `blake3(data) == hash` verification in `mirrorOnce` before `remoteStore.Put` — bitrot / torn-write / hw-error → silently uploads corrupt bytes to S3 + marks synced. Downstream detection via `ReadBlockVerified` is post-facto; local may have evicted | 85 |

### 3b. MEDIUM
| ID | File:line | Finding | Conf |
|---|---|---|---|
| **C-3** | `engine/gc.go:73` | `gcRootLocks` map grows unbounded (never trimmed); `makeTempGCStateRoot()` returns unique strings → new mutex per call. Comment claiming "empty key serializes" is false for temp-dir path | 88 |
| **I-2** | `local/fs/rollup.go:411,383` | **#669 root cause** — `dedupLRU.Put(h, payloadID)` BEFORE persister writes FileBlock row → second pass hits `ErrUnknownHash`. Wrong-row-owner subcase: LRU hit on hash belonging to different file's row → `AddRef` bumps RefCount on wrong row | 85 |
| **I-3** | `local/fs/appendwrite.go:202`, `engine/syncer.go:271` | **#670 root cause (engine side)** — `AppendWrite` pressure loop has NO deadline (only ctx.Done()); if rollup wedged (#668), every WRITE blocks in D-state forever. `Flush` returns `Finalized=false` immediately on `uploading` contention — NFS COMMIT loop sees no progress | 85 |
| **I-4** | `pkg/blockstore/migrate/migrate_to_cas.go:272,309` | Two `_ = err` discards silently swallow `removeLegacyBlkFiles` + `os.Remove(journalPath)` failures; operator gets no signal | 85 |
| **I-5** | `engine/fetch.go:347` | `inlineFetchOrWait` local Put failure logs `Warn` then returns `(data, true, nil)` — caller + waiters all see success; bytes never persisted → repeat S3 round-trips on every subsequent read under disk-full | 88 |
| **I-6** | `engine/syncer.go:648` | `SyncNow` spin-waits with `time.After(10ms)` allocating timers per iter; S3 HTTP `Timeout: 0` at `store.go:131` → goroutine leak on hung S3 | 82 |
| **I-7** | `local/fs/blockstore_methods.go:174` | `Walk` uses `io.EOF` as internal stop sentinel — callback returning `io.EOF` for any other reason treated as clean exit; untested contract hole | 80 |
| **S-4** | `local/fs/appendwrite.go:56` | `logPath` joins `payloadID` into `filepath.Join` without `isValidPayloadID` check at write time (only at recovery) — defense-in-depth gap on `../` payloadID | 80 |
| **R-1** | `engine/sync_queue.go` | `BlockStore.Close()` applies 30s timeout twice (DrainAllUploads then queue.Stop) → 60s worst case. S3 HTTP `Timeout: 0` + `stopCh` not propagated to `processDownload` ctx → goroutine leaks on hung S3 | 82 |
| **L-1** | `engine/upload.go:26` | `mirrorOnce` failure logged at `Debug` — per CLAUDE.md invariant 6 unexpected errors should be `Warn`/`Error`. S3 PUT failure silent for one health-check interval | 80 |
| **CS-1..CS-4** | `blockstoretest/conformance.go` | MISSING coverage: zero-byte Put / GetRange past EOF / concurrent Put+Walk / Put with wrong hash | 85 |
| Anonymous interface probes | `engine/engine.go:157-259` | 3 anonymous `cfg.Local.(interface{...})` checks special-case FSStore behavior without naming the concrete type — auditability smell | (arch) |

### 3c. LOW
| ID | File:line | Finding |
|---|---|---|
| V-1 | `fs.go:115,162-163`, `rollup.go:138-148` | Residual Wave 0 misses — `"d /"`, `"(per plan)"`, `"#588"` planning refs in comments |
| V-2 | `engine/engine.go:859-868` | `LocalForTest` ×2 duplicate exported funcs |
| CS-5 | `blockstoretest/conformance.go:87` | Planning ID `"Phase 17 D-05"` in test payload string |
| C-12 | `engine/gc.go:117-118` | `GCStats.SharesScanned` + `BlocksScanned` always 0 (REST compat) |
| Anonymous-interface probes (clarity) | (see above) | LOW prio at PR-B sequencing |

### 3d. NO-issue / confirmed compliant
- E-1: `_ = filepath.WalkDir(...)` in recovery/compaction/seedLRU — legit best-effort.
- E-2: `sync_queue.go:282` `_ = q.processDownload` — documented prefetch best-effort.
- E-3: `%w` discipline conformant throughout reviewed paths.
- S-2: AES-256-GCM nonce derivation correct (12-byte `crypto/rand` per Wrap, 2^48 birthday bound).
- S-3: Compression decompression-bomb guard present (`MaxFramedPlaintextSize = 64 MiB`).
- L-2, L-3: `rollupFile`, `dispatchRemoteFetch` log levels correct per invariant 6.
- V-3: `gofumpt` / `go vet` clean on reviewed source.

---

## 4. Tests Findings — Conformance Suite Audit-as-Subject

**Net verdict**: **Incremental patch, NOT full rewrite.** Existing 10 scenarios correctly pin externally-specified behavior (Put/Get round-trip no-alias, ErrChunkNotFound on miss, Walk LastModified non-zero, idempotent Put). No assertions codify impl choice over spec. Issues are coverage gaps.

| ID | Class | Action |
|---|---|---|
| CS-1 | MISSING | Add zero-byte Put scenario — pin behavior across backends (S3 allows; FSStore writes zero-byte file). Currently implementation-defined |
| CS-2 | MISSING | Add GetRange past-EOF scenario (offset > len, offset+length > len). Backend asymmetry: S3 returns ErrInvalidOffset; FSStore clamps |
| CS-3 | MISSING | Add concurrent Put racing Walk callback — pin no-duplicate-hash invariant (S3 paginator race risk) |
| CS-4 | MISSING | Add `bs.Put(ctx, differentHash, data)` where `blake3(data) != differentHash` — pin "no verify on Put" contract explicitly |
| CS-5 | REWRITE | `testPutGetRoundtrip:87` payload string contains `"Phase 17 D-05"` planning ID. Strip per `feedback_no_phase_comments_in_code` |
| CS-6 | KEEP | All 10 existing core scenarios remain |
| CS-7 | DEFER | Restart-mid-flush — tested in `appendlog_internals_test.go` for FSStore. Out-of-scope for portable suite (S3 doesn't need); document rationale |
| CS-8 | DEFER | Ref-count underflow not exercised — FileBlockStore surface outside `blockstore.BlockStore` contract; document rationale |

---

## 5. Bottlenecks (B-H2 — EXECUTED 2026-05-29)

Perf pass run via the B-H1 harness (`cmd/bench blockstore`, shipped #796/#803/#804/#805/#807). Profiles captured under `_profiles/blockstore/<workload>-<UTC>/{cpu,heap}.pprof`.

**Environment & caveats.** Apple-silicon dev laptop, `--remote=memory`, in-memory metadata store. NOT the Scaleway macro-bench baseline — absolute throughput is indicative, not gate-grade (the >5% regression gate is decided on bench-infra per PLAN §Performance acceptance). Hotspot *ranking* (cum%/alloc%) transfers; absolute ns/op does not. Memory remote retains every uploaded byte in RAM, inflating RSS on the big-write workloads (sequential reached ~4.3 GB RSS at 800 ops before being capped). `block.pprof`/`mutex.pprof` not captured — harness wires CPU+heap only (#671 still open).

### Workload → PLAN (a)-(e) mapping + throughput

| Harness workload | PLAN class | Ops | Wall | Throughput (dev-laptop) | Profile |
|---|---|---|---|---|---|
| `random-write` 4 KiB ws=4 | (a) small writes | 50000 | — | **STALLED** — see §5-B1 | cpu only (failed) |
| `random-write` 4 KiB ws=1 | (a) small writes | 40000 | — | rollup errored — see §5-B5 | cpu only |
| `mixed-rw` 4 KiB ws=4 | (b)+(e) | 50000 | 127.5 s | ~392 ops/s | cpu+heap |
| `sequential-write` 8 MiB | (c) big writes | 150 | 27.1 s | ~5.5 ops/s (~44 MiB/s) | cpu+heap |
| `dedup-heavy` 8 MiB | (c) dedup | 150 | 3.95 s | ~38 ops/s (dedup short-circuit) | cpu+heap |
| `flush-churn` 4 KiB | (e) flush storm | 1500 | 80.0 s | ~19 flush/s | cpu+heap |
| `walk` (HasChunk) | (d)-adjacent list | 5000 | 0.20 s | 24 589 ops/s | cpu+heap |
| `delete` | (e) delete | 5000 | 0.29 s | 17 287 ops/s | cpu+heap |
| `gc` mark-sweep | GC sweep | 5000 | — | profile empty — see §5-H2 | empty |

Pure-read workloads (b)/(d) are not first-class in the harness; `mixed-rw` (50/50 r/w) is the closest proxy. **Harness gap** — add a read-only workload.

### Top 5 bottlenecks (ranked by leverage)

| # | Hotspot | Where | Signal | Class | Fix |
|---|---|---|---|---|---|
| **B1** | `Syncer.Flush → mirrorOnce → ListUnsynced → FSStore.Walk` full-tree walk of CAS `blocks/` **every sync cycle / every Flush** | `engine/syncer.go:321`, `local/fs/blockstore_methods.go:228` | flush-churn: `os.ReadDir` **76.85% cum**, syscall `rawsyscalln` **91.9% flat**; O(total chunks) per flush → quadratic over a flush-heavy run | **HIGH (structural)** | Maintain an incremental pending-unsynced set (populate at `StoreChunk`, drain on upload-ack) instead of rediscovering by directory walk. File `v1.0-perf-blockstore` issue. |
| **B2** | `reconstructStream` allocates a full file-extent buffer (`make([]byte, maxEnd)`) on **every** rollup pass, never pooled | `local/fs/rollup.go:619` | sequential heap: **88.5 GB alloc_space (94.9%)**; flush-churn **4.4 GB (59%)**; dedup **1.17 GB (48%)** | **HIGH (alloc)** | `sync.Pool` of size-classed buffers, or stream record→chunker without materializing the whole extent (per [[feedback_streaming_io_for_data_paths]]). Biggest single GC-pressure source. |
| **B3** | `logIndex.EntriesForInterval` — fresh slice per call **+ O(n) linear scan over all entries** on the hot read/rollup path | `local/fs/logindex.go:144` | mixed-rw heap: **21.1 GB alloc_space (62.2%)**; 0.74 s cum CPU (1.94%) | **HIGH (alloc + algo)** | (a) caller-supplied/pooled scratch slice; (b) entries are append-ordered → binary-search the window or index by interval instead of full scan. |
| **B4** | GC churn downstream of B2/B3 — `madvise` + mark workers | runtime (sequential `madvise` 12.9% flat, dedup/seq `pthread_cond_wait` 21-33%) | sequential CPU: `memclrNoHeapPointers` 15% (zeroing the B2 buffers) + `madvise` 12.9% | **HIGH (derived)** | Resolves largely once B2+B3 land; re-profile after. |
| **B5** | Rollup per-record payload cap `maxRecordPayload = 17 MiB` rejects single records ≥17 MiB | `local/fs/appendlog.go:38,167` | random-write seed (single 64 MiB `WriteAt`): `rollup: payloadLen 67108864 exceeds 17825792 cap` → payload wedged, `Error` loop | **MED (limit)** | Production writes arrive ≤1 MiB chunked (NFS/SMB), so this is mostly a **harness-realism gap** (seed writes 64 MiB in one call). Either chunk the harness seed, or document the cap + fail the *write* fast rather than wedging *rollup*. |

### Lower-signal observations

- **blake3 hashing** is the dominant *legitimate* CPU on the dedup/sequential paths (`CompressNode`/`CompressChunk` 7–13%, `crc32.castagnoliUpdate` 6%). Intrinsic to CAS; **no action** — contention is the content-addressing cost.
- **chunker** (`Chunker.Next`) ~3–5% flat — FastCDC, reasonable; no action.
- **walk / delete** profiles are **dominated by bench `SeedLocalChunks`** (84–90% cum) — the measured op (`HasChunk`/`DeleteChunk`) is a thin stat/unlink and not a bottleneck. Harness should profile only the timed region, not setup.
- **memory-remote `Put`** retains all bytes (1.1 GB sequential) — expected for the in-RAM backend; not a code finding.

### Harness gaps surfaced (feed B-H follow-up / #680)

- **H1** `random-write` seeds one 64 MiB `WriteAt` per working-set file → (i) exceeds B5 rollup cap, (ii) ws≥4 pins the append log near `LogBudget` so the first timed op stalls 30 s on backpressure then fails (`append log: pressure wait timed out`). Seed should write in protocol-sized (≤1 MiB) chunks.
- **H2** `gc` workload profiled empty — it sweeps an under-seeded CAS so the timed region has ~0 samples. Needs a pre-populated store (seed N chunks, orphan a fraction) before the timed sweep.
- **H3** No pure-read workload (b)/(d). No `block`/`mutex` profile (needs #671). No goroutine snapshot.
- **H4** `flush-churn` at 20 000 ops is effectively unbounded wall-clock on the dev laptop (B1 quadratic walk) — capped at 1 500 here. Once B1 lands, restore the larger count.

### Macro reuse

`.planning/v1.0-audit/_baseline/` Wave 0 pprofs diffed where applicable. Fresh Scaleway macro run deferred to the post-B-fix acceptance gate.

---

## 6. Pre-existing Tracker Mapping

| Issue | Root cause | Severity | PR-B slot |
|---|---|---|---|
| **#668** rollup wedges on tree/logIndex divergence + ObjectIDPersister conflict | `rollup.go:231` (divergent interval stays in tree → infinite retry); `appendwrite.go:330` (non-atomic tree-insert vs logIndex-create under per-file mutex) | HIGH | B-bug-1 |
| **#669** file-level dedup refcount on missing FileBlock | `rollup.go:411` (LRU populates before persister writes row); wrong-row-owner via `AddRef` on cross-payload LRU hit | MED | B-bug-2 |
| **#670** NFS COMMIT D-state hang (engine side) | `appendwrite.go:202` pressure loop no deadline → wedges on #668; `syncer.go:271` Flush returns Finalized=false on uploading contention → COMMIT no-progress loop | MED engine / HIGH NFS | B-bug-3 (engine fix); rest in area #4 |

---

## 7. HIGH / MED / LOW Triage

| Tier | Count | Items |
|---|---|---|
| **HIGH** | 9 | C-1 (data race), C-2 (refcount leak), I-1 (#668), S-1 (no upload verify), C-01 (dead `Remote*` types), C-02 (dead Reader/Writer/Flusher/Store), C-03 (dup parse func), C-04 (dual sentinels), F-01 (engine.go god file) |
| **MED** | 18 | C-3 (gcRootLocks unbounded), I-2..I-7, S-4, R-1, L-1, CS-1..4, C-05..C-09, L-01, L-02, N-01, N-04, anonymous-interface probes |
| **LOW** | 10 | V-1, V-2, CS-5, C-10..C-14, F-02..F-04, N-02, N-05, N-06 |

---

## 8. PR-B Sequencing Proposal

Group ordering balances (a) zero-risk-first, (b) coupling, (c) reviewer cost. **All PRs branch off develop tip, not chained** — keeps merge cost flat.

### Wave B-A — zero-logic-change (parallelizable, ship in one mega-PR or 4 small)
1. **B-A1** Dead exports — delete `RemoteObjectInfo`, `RemoteStoreSweepSurface`, `Reader`/`Writer`/`Flusher`/`Store`, `BlockStoreError` + `NewBlockStoreError`, `engine.LocalForTest` pkg func. Rename `store.go → fileblock.go`. (C-01, C-02, F-02)
2. **B-A2** Sentinel merge — `ErrBlockNotFound` → `ErrChunkNotFound`. Update remote backends + conformance. (C-04)
3. **B-A3** Duplicate parse — move `parseChunkOffsetFromID` to `blockstore` root as `ParseChunkOffset`. Delete duplicates. (C-03)
4. **B-A4** Wave 0 sweep misses — `fs.go:115,162-163`, `rollup.go:138-148`, `conformance.go:87` CS-5. (V-1)

**Risk**: zero logic change. `go test ./...` should pass unchanged.

### Wave B-B — engine.go split (single-PR, prerequisite for any further engine work)
5. **B-B** Split `engine/engine.go` (1239 lines) into `engine.go` + `readwrite.go` + `flush.go` + `stats.go` + `health.go` + `read_internal.go`. Pure move, no logic change. (F-01)

**Risk**: low. Big diff, no behavior change. Single reviewer touch-up.

### Wave B-C — data-plane bug fixes (HIGH/CRIT correctness)
6. **B-C1** Fix C-1 — `SetOnChunkComplete` use `persisterMu` (or `atomic.Pointer`). Add `go test -race` coverage.
7. **B-C2** Fix C-2 — `applyFileLevelDedupHit` rollback: promote decrement failure to `Error`, aggregate to caller, prevent retry on inconsistent state.
8. **B-C3** Fix S-1 — add `blake3(data) == hash` check in `mirrorOnce` between `local.Get` + `remoteStore.Put`. Log `Error` + return error on mismatch.
9. **B-C4** Fix C-3 — `gcRootLocks` cap or single-mutex for temp-root case.

**Risk**: medium. Touches correctness-sensitive paths. Each PR independent; ship sequentially with full CI.

### Wave B-D — pre-existing tracker fixes (#668/#669/#670 engine side)
10. **B-D1** Fix #668 — consume divergent interval from tree (prevent infinite retry); atomic tree-insert + logIndex-create under per-file mutex.
11. **B-D2** Fix #669 — populate `dedupLRU` AFTER persister confirms row; scope LRU to (hash, payloadID) + validate ownership in `AddRef`.
12. **B-D3** Fix #670 engine contribution — `AppendWrite` pressure loop deadline; `Flush` semantics doc + retry guidance.

**Risk**: high. These are real production bugs. Each PR demands extended `go test -race` + targeted integration test.

### Wave B-E — collapse + lifecycle hygiene (MED structural)
13. **B-E1** FSStore constructor collapse + `DeleteLog` rename (C-06, C-07).
14. **B-E2** `RemoteStore` embed `blockstore.BlockStore` (C-09). Couples with B-A2 (needs single sentinel first).
15. **B-E3** `apiclient.BlockStoreStats` → type alias (C-05).
16. **B-E4** Unexport `CacheInterface`, `HealthTransitionCallback`, `LoadByHashFn`. Delete pkg-level `LocalForTest`. (L-02, V-2)
17. **B-E5** Delete `SweepConcurrency` field + clamping (C-11). Deprecate `GCStats` alias fields (C-12).
18. **B-E6** Inline `TransferRequest` constructors + merge `sync_entry.go` into `types.go` (C-10, F-03).
19. **B-E7** Rename `EvictReadBuffer → DestroyCache` or inline at caller (C-14).
20. **B-E8** Formalize `ChunkLifecycleHooks` interface in `local/` — replace 3 anonymous probes in `engine.New` (architect §9).

### Wave B-F — bug + observability fixes (MED)
21. **B-F1** Fix I-4 — `migrate_to_cas.go` surface discarded errors.
22. **B-F2** Fix I-5 — `inlineFetchOrWait` propagate local Put error to caller + waiters.
23. **B-F3** Fix I-6 + R-1 — replace spin-wait + add S3 HTTP timeout + propagate stopCh to processDownload ctx.
24. **B-F4** Fix I-7 — replace `io.EOF` internal stop with private sentinel.
25. **B-F5** Fix S-4 — `isValidPayloadID` at `getOrCreateLog` entry.
26. **B-F6** Fix L-1 — `mirrorOnce` failure log at `Warn`.
27. **B-F7** Conformance suite — add CS-1..CS-4 subtests. Rewrite CS-5 string.
28. **B-F8** Doc — compression/encryption composition order in both decorators.

### Wave B-G — naming + docs (LOW)
29. **B-G1** Rename `engine.Config → engine.BlockStoreConfig` (N-01). Update 4 callsites.
30. **B-G2** Deprecate `HealthCheck(ctx) error` — `// Deprecated:` godoc (N-04).
31. **B-G3** Rename `engine.BlockStore → engine.Store` (N-02). Update compile assertion + adapter type refs. **DEFER** if churn too large.

### Wave B-H — perf pass (after structural settles)
32. ✅ **B-H1** Build seeded workload harness — shipped #796/#803/#804/#805/#807 (`cmd/bench blockstore` + `bench/blockstore/`).
33. ✅ **B-H2** Run workloads (a)-(e), capture `_profiles/`, populate REVIEW.md §5 Bottlenecks — DONE 2026-05-29. Top findings B1 (full-tree walk per Flush), B2 (`reconstructStream` 88 GB alloc), B3 (`EntriesForInterval` 21 GB alloc + O(n) scan).
34. **B-H3** Aggregate cross-area perf findings → file `v1.0-perf-blockstore` issue for B1/B2/B3 (structural rewrites, out of scope for an area PR-B). Feeds Wave 2 stream 5.

### Wave B-I — final
35. **B-I1** `gsd-extract-learnings` skill — harvest decisions/patterns into memory.
36. **B-I2** `gsd-secure-phase` — invariant 8 acceptance.
37. **B-I3** `code-review ultra` (cloud, user-triggered) — pre-merge final pass.
38. **B-I4** Bundle conformance-suite-rewrite-decision into a PR-A2 if any finding promotes one of CS-1..4 from MED to HIGH after fix attempts.

**Total**: ~38 commits / ~12-15 PRs depending on bundling.

---

## 9. Decisions Carried Forward

- ✅ GC bundled into area #1 — confirmed; PLAN.md updated.
- ✅ Conformance suite verdict: **patch, not rewrite**.
- 🚧 `pkg/` ↔ `internal/` (a)/(b)/(c) decision belongs to runtime area #7. This audit logs 9 source-file imports as data point. **Architect recommendation**: option (b) for DittoFS as app.
- 🚧 `engine.BlockStore` → `engine.Store` rename (N-02) — defer unless paired with broader stutter pass.
- ✅ Workload (e) seeded-ops harness — shipped (B-H1); exercised in B-H2 via `mixed-rw`. Harness gaps H1-H4 logged in §5.

---

## 10. Acceptance Criteria for PR-A close

- ✅ `_explorer.md` produced (current state).
- ✅ `_architect.md` produced (structural findings).
- ✅ `_reviewer.md` produced (bug findings).
- ✅ REVIEW.md consolidates all three with cross-corroborated triage table.
- ✅ Pre-existing trackers #668/#669/#670 root causes confirmed.
- ✅ Conformance suite verdict documented (patch vs rewrite).
- ✅ PR-B sequencing proposal (groups B-A through B-I).
- ✅ Perf pass executed (B-H2, 2026-05-29) — §5 Bottlenecks populated; B1/B2/B3 → `v1.0-perf-blockstore` follow-up.
- ⏸  PLAN.md issue tracker follow-up: file v1.0-followup issues for MED/LOW not in B-A..B-G.

---

_Three parallel agents (code-explorer + code-architect + code-reviewer) dispatched 2026-05-28 on branch `v1.0/area1-blockstore-audit` @ develop@22f0afd0. Consolidation pass complete._

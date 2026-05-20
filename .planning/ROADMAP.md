# Roadmap: DittoFS v0.15.0 — Block Store + Core-Flow Refactor

**Milestone:** v0.15.0
**Parent issue:** [#419](https://github.com/marmos91/dittofs/issues/419)
**Source plan:** `/Users/marmos91/.claude/plans/i-spotted-a-problem-witty-llama.md`
**Created:** 2026-04-23
**Granularity:** fine
**Phases:** 8 (numbered 08-15, continuing from v0.13.0 which ended at phase 7)
**Coverage:** 79/79 requirements mapped (VER-01..VER-06 are phase-independent milestone gates)

## Core Value

Make block keys immutable by construction (CAS), unblocking per-share atomic backups in v0.16.0; deliver 40–80% cross-VM dedup via FastCDC for the primary VM-backed NAS workload; bundle accumulated design-debt cleanup across the adapter → engine → metadata/block core flow.

## Dependency Graph

```
08 (A0)    ──► 10 (A1) ──► 11 (A2) ──► 12 (A3) ──► 13 (A4) ──► 14 (A5) ──► 15 (A6)
09 (ADAPT) ────────────────────────────► 12 (A3)
```

Phase 08 (A0) and Phase 09 (ADAPT) proceed in parallel as independent pre-A1 cleanup tracks. Phase 15 (A6) is intentionally deferred until Phase 14 (A5) migration rollout completes in production.

## Phases

- [x] **Phase 08: Pre-refactor cleanup (A0)** — Merge subpackages into engine, kill dead scaffolding, fix HIGH-severity bugs (GH issue: #420) (completed 2026-04-23)
- [ ] **Phase 09: Adapter layer cleanup (ADAPT)** — Shared NFS/SMB helpers, SMB pool parity, consolidated error mapping (GH issue: #427)
- [ ] **Phase 10: FastCDC chunker + hybrid local store (A1)** — Append log + hash-keyed blocks dir + BLAKE3 (GH issue: #421)
- [ ] **Phase 11: CAS write path + GC rewrite (A2)** — Content-addressable keys, mark-sweep GC, simplified state machine (GH issue: #422)
- [x] **Phase 12: CDC read path + metadata schema + engine API (A3)** — `[]BlockRef` API, Cache by ContentHash, schema migration (GH issue: #423) (completed 2026-04-27)
- [x] **Phase 13: Merkle root + file-level dedup (A4)** — `FileAttr.ObjectID` + short-circuit full-file dedup (GH issue: #424) (completed 2026-04-28)
- [x] **Phase 14: Migration tool (A5)** — `dfsctl blockstore migrate` offline re-chunk + re-hash (GH issue: #425) (phases complete 2026-05-05; production wiring of `openOfflineRuntime` still gates production rollout)
- [ ] **Phase 15: Legacy cleanup (A6)** — Remove dual-read shim, delete deprecated symbols (GH issue: #426) — **SUBSUMED BY v0.16.0 Phase 17** (legacy delete folded into unified BlockStore interface work; this issue can be closed once Phase 17 ships)

---

## v0.16.0 — CAS Convergence

**Goal**: Converge DittoFS block storage on a single content-addressable layout. Collapse v0.15.0 dual-path complexity (legacy `.blk` + hybrid CAS, mmap + RAM cache, chunking-in-Syncer + chunking-at-rollup). Subsume v0.15.0 Phase 15 (A6) legacy cleanup. Ship one-shot migration command. No backward-compat shims — DittoFS has no production users.

**Phases:** 4 (numbered 16–19, continuing from v0.15.0 which ends at Phase 15)

16 (Cache RAM-only) ──► 17 (Unified BlockStore + legacy delete + migrate-to-cas) ──► 18 (Syncer mirror loop + ObjectID relocation) ──► 19 (Write-path RAM opts)

- [x] **Phase 16: Cache RAM-only (remove mmap read path)** — Delete `cache_mmap_*.go`, swap `readFromCAS` → `local.Get`, retire D-33 perf gate (GH issue: #516) — **SHIPPED 2026-05-20** on `gsd/phase-16-cache-mmap-removal` (16 commits); warm-cache D-06 PASS (ratio 0.890 ≤1.02)
- [ ] **Phase 17: Unified BlockStore interface + legacy delete + migration tool** — Single `BlockStore` interface (Put/Get/GetRange/Has/Delete/Walk/Head) for local + remote, `BlockStoreAppend` extends with random-write tier, delete legacy `.blk` writer + dual-read shim + flag, ship `dfsctl blockstore migrate-to-cas` one-shot (GH issue: #517)
- [ ] **Phase 18: Syncer simplification + ObjectID relocation** — Syncer Flush → mirror loop `for hash := range local.ListUnsynced() { remote.Put(...) }`. Move `ComputeObjectID` to rollup CommitChunks. Local-only shares now get ObjectIDs. (GH issue: #518)
- [ ] **Phase 19: Write-path RAM optimizations** — 4 opts: in-memory hash dedup LRU; group commit / batched fsync; direct-to-Cache on chunk completion; eager small-file dedup (GH issue: #519)

**Parent tracking issue:** [#515](https://github.com/marmos91/dittofs/issues/515)

**Design spec:** `~/.claude/plans/reactive-sprouting-moonbeam.md` (locked 2026-05-20)

**Locked decisions (do not re-litigate in discuss-phase):**
1. Cache is RAM-only (drop mmap)
2. Local + remote block stores share one CAS-keyed `BlockStore` interface
3. Syncer is a byte-identical local→remote mirror

**Intended outcome:** ~30–40% LoC reduction in `pkg/blockstore/` (target under 4k from current ~6k); single-layout conformance suite (`blockstoretest/`); trivially auditable Syncer; measurable rand-write throughput wins. All v0.15.0 perf gates preserved (D-21, D-41, D-43); D-33 deleted with mmap.

## Phase Details

### Phase 08: Pre-refactor cleanup (A0)
**Goal**: Land low-risk, high-value cleanup that simplifies the A1 starting point — merge `readbuffer`/`sync`/`gc` sub-packages into `engine`, kill dead scaffolding, and fix HIGH-severity bugs surfaced by the audit.
**Depends on**: Nothing (pre-cleanup track; parallel with Phase 09)
**GH issue**: [#420](https://github.com/marmos91/dittofs/issues/420)
**Duration**: ~1 week
**Requirements**: TD-01, TD-02, TD-03, TD-04
**Success Criteria** (what must be TRUE):
  1. `pkg/blockstore/readbuffer`, `pkg/blockstore/sync`, and `pkg/blockstore/gc` are merged into `pkg/blockstore/engine/` as files — no behavior change, tests still pass
  2. HIGH-severity bugs fixed: `FSStore.Start()` goroutine joined on `Close()`; `syncFileBlock` propagates `PutFileBlock` errors (no silent `_ =`); `engine.Delete` calls `DeleteAllBlockFiles` (no `.blk` leak on disk); local tier stops calling `FileBlockStore` on the write hot path
  3. Dead scaffolding removed: `BackupHoldProvider`, `FinalizationCallback`, `ReadAtWithCOWSource`/`readFromCOWSource`, `FileAttr.COWSourcePayloadID`, unused `FileAttr.Blocks []string`, unset `FileAttr.ObjectID`
  4. Five block-key parsers (`ParseStoreKey`, `parseStoreKeyBlockIdx`, `parseBlockID`, `extractBlockIdx`, `parsePayloadIDFromBlockKey`) collapsed to two canonical parsers: `ParseStoreKey` for `{payloadID}/block-{N}` + `ParseBlockID` for `{payloadID}/{blockIdx}` (net: 5 → 2)
**Files to touch**:
  - `pkg/blockstore/readbuffer/`, `pkg/blockstore/sync/`, `pkg/blockstore/gc/` → merged into `pkg/blockstore/engine/`
  - `pkg/blockstore/gc/gc.go` — delete `BackupHoldProvider`
  - `pkg/blockstore/sync/syncer.go` — delete `FinalizationCallback`, fix error swallowing
  - `pkg/blockstore/engine/engine.go` — delete COW read path, fix `Delete` to call `DeleteAllBlockFiles`
  - `pkg/metadata/file_types.go` — drop `Blocks []string` and `ObjectID` (reintroduced in A3/A4)
  - `pkg/blockstore/local/fs/fs.go`, `write.go`, `eviction.go` — lifecycle fix, unhook from `FileBlockStore` on write path
**Key risks**:
  - Merging sub-packages may trigger import-cycle collapse issues — handle by renaming colliding symbols rather than forcing cycle breaks
  - Deleting `BackupHoldProvider` must coordinate with v0.13.0 backup code paths that were never released (safe to break; not in production)
**Plans**: 15 plans
  - [x] 08-01-PLAN.md — TD-02a: join FSStore.Start goroutine on Close
  - [x] 08-02-PLAN.md — TD-02b: propagate syncFileBlock errors
  - [x] 08-03-PLAN.md — TD-02c: engine.Delete calls DeleteAllBlockFiles
  - [x] 08-04-PLAN.md — TD-02d: isolate local write path from FileBlockStore
  - [x] 08-05-PLAN.md — Update GH issue #420 scope expansion (D-08)
  - [x] 08-06-PLAN.md — PR-B: remove v0.13.0 backup e2e tests + helpers
  - [x] 08-07-PLAN.md — PR-B: remove backup API handlers, dfsctl, apiclient
  - [x] 08-08-PLAN.md — PR-B: drop storebackups wiring from Runtime
  - [x] 08-09-PLAN.md — PR-B: remove storebackups package
  - [x] 08-10-PLAN.md — PR-B: remove pkg/backup tree
  - [x] 08-11-PLAN.md — PR-B: remove v0.13.0 backup docs + audit
  - [x] 08-12-PLAN.md — PR-B: go mod tidy + OTel audit
  - [x] 08-13-PLAN.md — PR-C: TD-01 merge readbuffer/sync/gc into engine
  - [x] 08-14-PLAN.md — PR-C: TD-03 delete COW/FinalizationCallback/BackupHoldProvider + metadata fields
  - [x] 08-15-PLAN.md — PR-C: TD-04 parser collapse 5 to 2 + lint sweep

### Phase 09: Adapter layer cleanup (ADAPT)
**Goal**: Consolidate duplicated NFS/SMB adapter helpers, bring SMB read path to pool parity with NFS, unify `metadata.ExportError` → protocol error mapping, and prepare adapters to pass `[]BlockRef` into the engine (unblocking Phase 12).
**Depends on**: Nothing (pre-cleanup track; parallel with Phase 08; consumed by Phase 12)
**GH issue**: [#427](https://github.com/marmos91/dittofs/issues/427)
**Duration**: ~2 weeks
**Requirements**: ADAPT-01, ADAPT-02, ADAPT-03, ADAPT-04, ADAPT-05
**Success Criteria** (what must be TRUE):
  1. New shared package `internal/adapter/common/` exposes `ResolveForRead`, `ResolveForWrite`, `readFromBlockStore` — used by both NFS v3/v4 and SMB v2 handlers; per-protocol `getBlockStoreForHandle` duplication deleted
  2. SMB READ handler (`internal/adapter/smb/v2/handlers/read.go`) allocates response buffers through `internal/adapter/pool` (4KB/64KB/1MB tiers) — no more `make([]byte, actualLength)` per request; buffers released on completion
  3. A single consolidated `metadata.ExportError → NFS3ERR_* / STATUS_*` mapping table exists; both protocol adapters consume it; adding a new export error requires one edit, not two
  4. Adapters fetch `FileAttr.Blocks` from metadata and pass `[]BlockRef` into engine call sites (enables Phase 12's API-01 engine change)
  5. Cross-protocol conformance test exists: same file operation over NFS and SMB returns consistent client-observable error codes for each `metadata.ExportError` value
**Files to touch**:
  - `internal/adapter/common/` — **new** shared package (helpers, error-mapping table)
  - `internal/adapter/smb/v2/handlers/read.go` — route through `internal/adapter/pool`
  - `internal/adapter/nfs/v3/handlers/*.go`, `internal/adapter/nfs/v4/handlers/*.go` — use shared helpers
  - `internal/adapter/smb/v2/handlers/*.go` — use shared helpers; promote equivalent of `readFromBlockStore`
  - `test/e2e/*` — new cross-protocol conformance test
**UI hint**: no (protocol-layer internals; no user-facing UI)
**Key risks**:
  - Error-mapping consolidation could silently change user-visible error codes if current mappings disagree — add test coverage before changing any mapping
  - SMB pool integration must correctly free buffers on all success/error paths to avoid pool exhaustion — wire `defer pool.Put(...)` at handler entry
**Plans**: 5 plans
  - [ ] 09-01-PLAN.md — ADAPT-01: extract internal/adapter/common helpers (BlockStoreRegistry, ResolveForRead/Write, ReadFromBlockStore); migrate NFSv3/v4/SMB call sites; delete duplicates
  - [ ] 09-02-PLAN.md — ADAPT-02: SMB READ pool integration (remove inline make, add nil-safe ReleaseData on SMBResponseBase/HandlerResult, fire after wire write in plain+encrypted+compound paths)
  - [ ] 09-03-PLAN.md — ADAPT-03: consolidated errmap.go struct-per-code table (NFS3/NFS4/SMB columns) + content_errmap + lock_errmap; migrate all translators; fix SMB errors.As latent bug
  - [ ] 09-04-PLAN.md — ADAPT-04: add common.WriteToBlockStore; refactor every NFS/SMB WRITE+COMMIT call site through common helpers; document Phase-12 []BlockRef seam in common/doc.go
  - [ ] 09-05-PLAN.md — ADAPT-05: cross-protocol conformance test (e2e tier ~18 codes + unit tier ~9 exotic codes) driven from common table; docs updates (ARCHITECTURE/NFS/SMB/CONTRIBUTING) per D-17

### Phase 10: FastCDC chunker + hybrid local store (A1)
**Goal**: Land the new chunking + local-store infrastructure (FastCDC + BLAKE3 + hybrid Logs/Blocks directory) behind a feature flag, so Phase 11 can wire the CAS write path on top of a tested foundation.
**Depends on**: Phase 08 (A0)
**GH issue**: [#421](https://github.com/marmos91/dittofs/issues/421)
**Duration**: ~3 weeks
**Requirements**: BSCAS-02, LSL-01, LSL-02, LSL-03, LSL-04, LSL-05, LSL-06
**Success Criteria** (what must be TRUE):
  1. New `pkg/blockstore/chunker/` package implements FastCDC with min=1 MB / avg=4 MB / max=16 MB (normalization level 2); boundary-stability property tests pass on shifted-content fixtures
  2. BLAKE3 integration via `github.com/zeebo/blake3` is in place and exposed as `ContentHash [32]byte` with `String()` → `"blake3:{hex}"`
  3. Local store layout is hybrid: per-file append-only write log at `logs/{payloadID}.log` (64-byte header + CRC-per-record) + hash-keyed chunk directory at `blocks/{hash[0:2]}/{hash[2:4]}/{hash_hex}`
  4. `AppendWrite` replaces `WriteAt` + `tryDirectDiskWrite` on the hot path (one code path; append to log)
  5. Pressure channel signals syncer when log exceeds budget; writer blocks on excess and unblocks when syncer drains (INV-5 / INV-7 verified by concurrent-write-storm test)
  6. Crash recovery verified: scan from `consumed_pos`, truncate at first bad CRC, re-chunk surviving records on first sync
**Files to touch**:
  - `pkg/blockstore/chunker/` — **new** package (FastCDC in-house, ~200 LoC)
  - `pkg/blockstore/types.go` — `ContentHash`, `BlockRef`, `FormatCASKey`
  - `pkg/blockstore/local/fs/` — new `logs/` + `blocks/` subtree, `AppendWrite`, `GetDirtyRegions`, `ReadLog`, `StoreChunk`, `ReadChunk`, `HasChunk`, `DeleteChunk`, `CommitChunks`, `ListDirtyFiles`
  - `go.mod` — add `github.com/zeebo/blake3`
  - `pkg/blockstore/local/localtest/` — extend conformance suite for log + chunks round-trip, pressure channel
**Key risks**:
  - FastCDC parameter choice must preserve boundary stability — benchmark against restic-FastCDC's published dedup ratios on synthetic fixtures before finalizing
  - Log-based write path must match or beat current `tryDirectDiskWrite` perf — microbench large sequential writes under the feature flag before wiring it by default
  - Crash recovery must tolerate torn writes at any byte offset — property test with random mid-record kill
**Plans**: 12 plans
  - [ ] 10-01-PLAN.md — ContentHash doc refresh + CASKey() helper + zeebo/blake3 dep + D-41 BLAKE3 3x SHA-256 gate
  - [ ] 10-02-PLAN.md — BSCAS-02: FastCDC chunker package (params + gear + Next) + D-42 70% boundary stability gate
  - [ ] 10-03-PLAN.md — LSL-01: log header + record framing + CRC32C helpers + torn-write tests
  - [ ] 10-04-PLAN.md — LSL-03: AppendWrite + per-file mutex + interval tree + pressure loop (flag-gated, default off)
  - [ ] 10-05-PLAN.md — LSL-02: StoreChunk/ReadChunk/HasChunk/DeleteChunk with .tmp+rename+fsync + two-level shard
  - [ ] 10-06-PLAN.md — LSL-04/05: chunkRollup worker pool + CommitChunks atomicity + RollupStore interface + memory impl
  - [ ] 10-07-PLAN.md — LSL-06: crash recovery (header reconcile, torn-record truncate, orphan sweep)
  - [ ] 10-08-PLAN.md — LSL-05: config wiring (use_append_log etc.) + Badger/Postgres RollupStore + docs/CONFIGURATION.md
  - [ ] 10-09-PLAN.md — LSL-05: DeleteAppendLog (D-28) + TruncateAppendLog (D-29) with tombstone coordination
  - [ ] 10-10-PLAN.md — LSL-01..06: localtest/RunAppendLogSuite 5-scenario conformance (round-trip, pressure, torn, storm, monotone)
  - [ ] 10-11-PLAN.md — D-40 perf gate (AppendWrite <= 1.05x tryDirectDiskWrite) + BENCHMARKS.md update
  - [ ] 10-12-PLAN.md — docs: ARCHITECTURE/IMPLEMENTING_STORES/FAQ/package godoc + 10-DESIGN.md one-pager

### Phase 11: CAS write path + GC rewrite (A2)
**Goal**: Rewrite the sync/upload path to content-addressable storage (`cas/{hash[0:2]}/{hash[2:4]}/{hash_hex}`), deliver the simplified three-state block lifecycle (Pending → Syncing → Remote), and replace path-prefix GC with a fail-closed mark-sweep algorithm.
**Depends on**: Phase 10 (A1)
**GH issue**: [#422](https://github.com/marmos91/dittofs/issues/422)
**Duration**: ~3 weeks
**Requirements**: BSCAS-01, BSCAS-03, BSCAS-06, GC-01, GC-02, GC-03, GC-04, STATE-01, STATE-02, STATE-03, LSL-07, LSL-08, TD-09, INV-01, INV-03, INV-04, INV-05, INV-06
**Success Criteria** (what must be TRUE):
  1. `TestBlockStoreImmutableOverwrites` (canonical correctness E2E — currently failing on `develop`) passes: old hashes remain at their CAS keys after overwrite; new bytes written to new CAS keys; GC deletes only hashes absent from the live set
  2. Every S3 PUT under `cas/...` sets `x-amz-meta-content-hash: blake3:{hex}` — verified by `aws s3api head-object` outside DittoFS (external tooling can verify without DittoFS metadata)
  3. Block state machine is three states only — `Pending → Syncing → Remote` (GC-eligible when RefCount = 0); `State=Remote` only after successful upload + successful metadata txn (INV-3 no orphan uploads); state lives only in `FileBlock` indexed by `ContentHash` (no parallel state in memory buffers or fd pools)
  4. `LocalStore` interface narrowed from 22 to ~17 methods (removes `MarkBlockRemote`, `GetDirtyBlocks`, `SetSkipFsync`, etc.); local store no longer calls `FileBlockStore` on the write hot path — eviction driven from on-disk state only
  5. Mark-sweep GC: live set is the union of all `FileAttr.Blocks[*].Hash` across the metadata store; sweep lists remote `cas/XX/YY/*` prefixes (parallelizable) and deletes anything absent from the live set; any error in mark phase aborts the sweep (fail-closed)
  6. Every chunk downloaded from S3 is BLAKE3-verified against its CAS key before the caller receives bytes (INV-6); `flushBlock` does not hold `mb.mu` during disk write (stages bytes, releases lock, syncs)
**Files to touch**:
  - `pkg/blockstore/types.go` — `FormatCASKey`, `ParseCASKey`, new `FileBlock` schema indexed by `ContentHash`
  - `pkg/blockstore/sync/upload.go` (now `pkg/blockstore/engine/syncer.go`) — chunker-driven sync, CAS upload, `x-amz-meta-content-hash` header, BLAKE3 verification
  - `pkg/blockstore/gc/gc.go` (now `pkg/blockstore/engine/gc.go`) — mark-sweep by `Blocks[]` union; fail-closed
  - `pkg/blockstore/local/fs/*.go` — drop `FileBlockStore` calls on write path, self-managed LRU, lock-scope fix in `flushBlock`
  - `pkg/blockstore/remote/s3/*.go` — add `x-amz-meta-content-hash` on PUT, verify on GET
  - `test/e2e/TestBlockStoreImmutableOverwrites` — **new** canonical correctness test
  - Crash-injection suite additions: kill mid-upload, kill mid-metadata-txn, corrupt S3 object
**Key risks**:
  - Dual-read compatibility (legacy `{payloadID}/block-{idx}` + new `cas/...`) must coexist cleanly during the A2–A5 window — keep `FormatStoreKey` alive; engine falls through to legacy on CAS miss
  - Remote verification BLAKE3 cost on the read hot path must not regress random-read IOPS (target ≥1,350) — measure with and without the verification step in benchmarks
  - Mark phase enumeration cost grows with metadata-store file count — stream via cursor rather than loading into memory; abort on any store error (fail-closed)
**Plans**: 9 plans (shipped)

### Phase 12: CDC read path + metadata schema + engine API (A3)
**Goal**: Migrate the metadata schema to `[]BlockRef`, change the engine `ReadAt/WriteAt` signature to take `[]BlockRef` from the caller, collapse `readbuffer`+`prefetcher` into a single `Cache` keyed by `ContentHash`, and update Badger/Postgres/Memory stores to the extended conformance suite.
**Depends on**: Phase 11 (A2) AND Phase 09 (ADAPT — adapters now fetch `FileAttr.Blocks` and pass `[]BlockRef` down)
**GH issue**: [#423](https://github.com/marmos91/dittofs/issues/423)
**Duration**: ~2 weeks
**Requirements**: API-01, API-02, API-03, API-04, META-01, META-03, META-04, CACHE-01, CACHE-02, CACHE-03, CACHE-04, CACHE-05, CACHE-06, INV-02
**Success Criteria** (what must be TRUE):
  1. `FileAttr.Blocks` is now `[]BlockRef{Hash, Offset, Size}` (authoritative, sorted by offset, populated on every sync finalization) — round-trip conformance passes for Memory, Badger, and Postgres backends
  2. `engine.BlockStore.ReadAt(ctx, blocks []BlockRef, dest, offset)` and `WriteAt` take pre-fetched `[]BlockRef` from callers; engine no longer imports `pkg/metadata` on hot paths; binary search on sorted `[]BlockRef` locates chunks covering a read range
  3. `CopyPayload` is O(1) — refcount++ on shared `BlockRef` list (no per-block data copy)
  4. Single `Cache` type in `pkg/blockstore/engine/cache.go` replaces `readbuffer` + `prefetcher`; keyed by `ContentHash` (deduped chunks cache once, not once-per-file); sequential-detection prefetch (3+ consecutive reads → prefetch next 1–8 chunks) runs internally with bounded concurrency; `Cache.OnRead` is the sole hint API
  5. `Cache.InvalidateFile(payloadID)` invalidates all cached chunks for a file on writes/truncates/deletes; local-hit reads are zero-copy or single-copy (no double allocation through `GetBlockData` + `Cache.Put`)
  6. `FileBlockStore` interface narrowed to 6 methods keyed by `ContentHash`: `GetByHash`, `Put`, `Delete`, `IncrementRefCount`, `DecrementRefCount`, `ListPending`; invariant INV-02 verified — `∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)` across the store
**Files to touch**:
  - `pkg/blockstore/engine/engine.go` — `ReadAt([]BlockRef, ...)`, `WriteAt`, binary search
  - `pkg/blockstore/engine/cache.go` — **new** unified Cache (replaces `readbuffer` + `prefetcher`)
  - `pkg/metadata/file_types.go` — `Blocks []BlockRef` + `FileBlockStore` narrowed interface
  - `pkg/metadata/store/badger/*.go`, `pkg/metadata/store/postgres/*.go`, `pkg/metadata/store/memory/*.go` — schema migration
  - `pkg/metadata/storetest/` — extended conformance suite (`[]BlockRef` round-trip, `ObjectID` stability)
  - `internal/adapter/nfs/*`, `internal/adapter/smb/*` — call sites fetch `FileAttr.Blocks` and pass to engine (consumes Phase 09 groundwork)
**UI hint**: no (protocol/engine internals)
**Key risks**:
  - Postgres schema migration must be reversible and tested against live data — ship behind a migration version bump with forward-only rollout plan; preserve legacy column until A5 migration completes
  - Cache keying change from `(payloadID, blockIdx)` to `ContentHash` could invalidate existing warm cache state on deploy — acceptable since Cache is in-memory only, but document the cold-cache perf blip
  - Property-based INV-02 refcount fuzzer must cover concurrent file creates/deletes/copies across shares
**Plans**: 13 plans
  - [x] 12-01-PLAN.md — META-01: BlockRef type + FileAttr.Blocks field + ErrBlockRefMissing sentinel (PR-A wave 1 root) — shipped 2026-04-26 (`c3efab10`, `9bb213b1`)
  - [x] 12-02-PLAN.md — META-01/META-04: Postgres file_block_refs migration + objects.go CRUD + cascade test — shipped 2026-04-27 (`d2003de0`, `c5252bd3`, `e1c6c8f5`)
  - [x] 12-03-PLAN.md — META-01/META-04: Badger JSON forward-compat + Memory deep-copy of Blocks — shipped 2026-04-27 (`350edd87`, `90cb3013`, `36311ac0`)
  - [x] 12-04-PLAN.md — META-03: Narrow FileBlockStore to 6 methods + lift EnumerateFileBlocks to MetadataStore — shipped 2026-04-27 (`4440486d`, `11a38b43`, `d9a24b7d`)
  - [x] 12-05-PLAN.md — INV-02 (D-37): Fix WR-4-01 dedup donor-refcount leak in uploadOne — shipped 2026-04-27 (`9bff89eb`, `f7495b91`)
  - [x] 12-06-PLAN.md — META-04: storetest BlockRef round-trip + FK cascade conformance scenarios — shipped 2026-04-27 (`f4652cab`, `3664515a`)
  - [x] 12-07-PLAN.md — API-01..04: Engine ReadAt/WriteAt/CopyPayload/Truncate/Delete []BlockRef signatures + findBlocksForRange + MetadataCoordinator + API-02 gate (PR-B) — shipped 2026-04-27 (`efd4099f`, `9ed5dd00`, `ff7e568c`)
  - [x] 12-08-PLAN.md — API-01/02: Adapter common — CacheInvalidator + diffRemovedHashes + CopyPayload (BLOCKER-2) + ErrBlockRefMissing errmap (D-23). []BlockRef threading through Read/Write helpers deferred to 12-09 cache rewrite per executor's no-handler-touch constraint — shipped 2026-04-27 (`59d194da`, `1ee234f5`, `0c20cecc`, `e6c1d55c`, `8a6d414d`)
  - [x] 12-09-PLAN.md — CACHE-01..05: Greenfield Cache rewrite + delete prefetch.go and bifurcation tests (PR-C). Single CAS-keyed Cache type replaces ReadBuffer + Prefetcher; Null Object pattern eliminates defensive nil-checks; engine.ReadAt invokes cache.OnRead post-read; Plan 10 mmap reintroduces byte-serving — shipped 2026-04-27 (`52a73faa`, `f4b24de5`, `8ab2215f`, `59a960bf`, `782d99f5`)
  - [x] 12-10-PLAN.md — CACHE-06: Build-tagged single-copy mmap (linux/darwin) + ReadFile fallback (windows)
  - [x] 12-11-PLAN.md — INV-02: Property-based fuzzer in storetest + audit-refcounts CLI + REST endpoint
  - [x] 12-12-PLAN.md — D-43 perf gate: rand-read regression test, BENCHMARKS.md update
  - [x] 12-13-PLAN.md — D-41 docs: ARCHITECTURE / IMPLEMENTING_STORES / FAQ / CONFIGURATION / CLI / BLOCKSTORE_MIGRATION

### Phase 13: Merkle root + file-level dedup (A4)
**Goal**: Populate `FileAttr.ObjectID` as a BLAKE3 Merkle root over sorted block hashes at file quiesce, and short-circuit chunking on full-file writes when the provisional `ObjectID` matches an existing file — the primary cross-VM dedup win.
**Depends on**: Phase 12 (A3)
**GH issue**: [#424](https://github.com/marmos91/dittofs/issues/424)
**Duration**: ~2 weeks
**Requirements**: BSCAS-04, BSCAS-05, META-02, DEDUP-01, DEDUP-02, DEDUP-03
**Success Criteria** (what must be TRUE):
  1. `FileAttr.ObjectID` = BLAKE3 Merkle root over sorted block hashes, populated lazily at file quiesce — stable across rename, reproducible across engine restarts, verified by conformance tests
  2. File-level dedup short-circuits chunking on full-file writes: before running FastCDC, check if provisional `ObjectID` matches an existing file — on hit, reuse its `BlockRef` list and upload zero new blocks
  3. `GetByHash` path (renamed from legacy `FindFileBlockByHash`) preserved as µs-cost pre-PUT dedup check on a per-chunk basis
  4. Dedup scope is global per metadata store — `RefCount` spans shares when shares share a remote config
  5. Cross-VM dedup fixture achieves ≥40% storage reduction on synthetic VM-fleet workload (primary business outcome; gates VER-03)
**Files to touch**:
  - `pkg/metadata/file_types.go` — `ObjectID ContentHash` populated at quiesce
  - `pkg/blockstore/engine/syncer.go` — provisional `ObjectID` compute + short-circuit on match
  - `pkg/metadata/storetest/` — `ObjectID` stability conformance
  - `test/e2e/` — VM-fleet dedup fixture + assertion
**UI hint**: no
**Key risks**:
  - Lazy `ObjectID` update means short-circuit misses fresh writes that haven't quiesced — acceptable tradeoff per plan; revisit if dedup hit rate demands eager update
  - Cross-share refcounting requires careful transaction boundaries — verify INV-02 under concurrent multi-share load
  - Synthetic VM-fleet fixture must be representative — use real qcow2 base-image shifted clones, not simple byte-shift
**Plans**: 15 plans (13-01..13-10 SHIPPED; 13-11..13-15 added 2026-04-28 per 13-VERIFICATION.md gap closure)
  - [x] 13-01-PLAN.md — META-02/BSCAS-04: ComputeObjectID helper + FileAttr.ObjectID field + drive-by SHA-256→BLAKE3 comment fix
  - [x] 13-02-PLAN.md — META-02 (Postgres): 000013 migration + object_id column read/write in PutFile/GetFile
  - [x] 13-03-PLAN.md — META-02 (Badger+Memory): secondary index maintenance in PutFile/DeleteFile under existing locks
  - [x] 13-04-PLAN.md — META-02: MetadataStore.FindByObjectID interface + 3 backend impls + MetadataCoordinator extension + post-Flush ObjectID compute
  - [x] 13-05-PLAN.md — META-02 conformance: 8 storetest scenarios (round-trip, lookup, race, lifecycle, sort/restart stability, cross-share scope) + INV02Fuzz extension
  - [x] 13-06-PLAN.md — BSCAS-05/DEDUP-01: TDD-RED unit tests for the file-level dedup short-circuit
  - [x] 13-07-PLAN.md — BSCAS-05/DEDUP-01: short-circuit implementation + race resolution + cache invalidation + log truncation
  - [x] 13-08-PLAN.md — DEDUP-02: cross-share scope storetest scenario + e2e smoke test
  - [x] 13-09-PLAN.md — DEDUP-03: pinned qcow2 + 8 clones nightly fixture asserting ≥40% reduction (VER-03 gate)
  - [x] 13-10-PLAN.md — D-21 perf gate (≤2% rand-write regression) + D-19 docs updates (ARCHITECTURE/IMPLEMENTING_STORES/FAQ/BLOCKSTORE_MIGRATION)
  - [x] 13-11-PLAN.md — Gap closure: TDD-RED E2E test (TestObjectIDPopulation_NFSWriteQuiesce) asserting FileAttr.ObjectID populated after NFS write quiesce
  - [x] 13-12-PLAN.md — Gap closure (BSCAS-04 / META-02): wire Syncer.Flush to invoke persistFileBlocksAfterFlush on full quiesce — closes VERIFICATION must-have #1
  - [x] 13-13-PLAN.md — Gap closure (BSCAS-05 / DEDUP-01): wire Syncer.Flush to invoke TrySpeculativeFileLevelDedup before per-block drain on D-09 trigger — closes VERIFICATION must-have #2
  - [ ] 13-14-PLAN.md — Gap closure (DEDUP-03): nightly run + freeze qcow2BaseSHA256 + record observed reduction + verify Plan 13-11 RED→GREEN — closes VERIFICATION must-have #5
  - [x] 13-15-PLAN.md — Gap closure cleanup: refresh ARCHITECTURE / BLOCKSTORE_MIGRATION docs + remove stale 'deferred wiring' comments in pkg/blockstore/engine/ to reflect wired state

### Phase 14: Migration tool (A5)
**Goal**: Ship `dfsctl blockstore migrate --share <name>` offline migration tool — reads legacy path-indexed blocks, re-chunks via FastCDC, uploads as CAS chunks, updates `FileAttr.Blocks`, deletes legacy keys after integrity verification. Supports resumable state, dry-run, bandwidth limits, and parallelism.
**Depends on**: Phase 13 (A4)
**GH issue**: [#425](https://github.com/marmos91/dittofs/issues/425)
**Duration**: ~4 weeks (includes production rollout window)
**Requirements**: MIG-01, MIG-02, MIG-03, MIG-04
**Success Criteria** (what must be TRUE):
  1. `dfsctl blockstore migrate --share <name>` reads every legacy `{payloadID}/block-{idx}` key, runs FastCDC, uploads CAS chunks with `x-amz-meta-content-hash`, and updates `FileAttr.Blocks` with the new `[]BlockRef` list
  2. Migration is resumable via per-share state file (`.migration-state.json`); supports `--dry-run`, `--parallel N` (default 4), `--bandwidth-limit MB/s`
  3. Dual-read compatibility shim in engine continues to read legacy `{payloadID}/block-{idx}` keys during the A2–A5 window — per-share migration toggles the shim off for that share on completion
  4. Post-migration integrity check: every `FileAttr.Blocks[i]` points to a CAS key that exists in S3 (HEAD succeeds); legacy keys deleted only after all references migrated
  5. Operator can verify migration completeness via `dfsctl blockstore migrate status --share <name>` before enabling Phase 15 legacy cleanup
**Files to touch**:
  - `cmd/dfsctl/commands/blockstore/migrate.go` — **new** migration tool
  - `pkg/blockstore/engine/*.go` — dual-read shim (lives from Phase 11 until Phase 15 removes it)
  - `docs/BLOCKSTORE_MIGRATION.md` — **new** operator guide (step-by-step, rollback, bandwidth tuning)
**UI hint**: no (CLI tool; REST endpoint optional for dittofs-pro UI)
**Key risks**:
  - Resumability must tolerate mid-chunk crashes without duplicate uploads — use state-file journaling with atomic rename; dedup via `GetByHash` is safety net
  - Bandwidth limit must apply to the aggregate of parallel workers, not per-worker — use a shared token bucket
  - Legacy key deletion must happen only after all references in `FileAttr.Blocks` are confirmed migrated — enforce via post-migration integrity check before unlink
  - Four-week duration includes production rollout window — A6 (Phase 15) intentionally deferred until operators confirm per-share migration complete
**Plans**: 7 plans
  - [x] 14-01-share-blocklayout-PLAN.md — MIG-03: ShareOptions.BlockLayout field across Memory/Badger/Postgres + storetest conformance
  - [x] 14-02-engine-blocklayout-routing-PLAN.md — MIG-03: Engine reads BlockLayout at share-open; ErrLegacyReadOnCASOnly fail-loud gate
  - [x] 14-03-migrate-tool-core-PLAN.md — MIG-01/MIG-02 (partial): blockstore command tree + offline probe + append-only journal + per-file FastCDC re-chunk + GetByHash dedup probe + ObjectID backfill (production controlplane composition deferred to 14-04)
  - [x] 14-04-bandwidth-parallel-PLAN.md — MIG-02: --parallel errgroup + shared rate.Limiter (KB/MB/GB + KiB/MiB/GiB) + slog progress + TTY bar (production composition deferred — see SUMMARY)
  - [x] 14-05-integrity-cutover-PLAN.md — MIG-04: HEAD-per-ref + content-hash header parity + auto-cutover txn (block_layout flip) + end-of-share legacy GC
  - [x] 14-06-status-rest-PLAN.md — MIG-01/MIG-02: dfsctl blockstore migrate status (table/JSON/YAML) + GET /api/v1/blockstore/migrate/status REST endpoint (admin-auth) + Runtime.LocalStoreDir accessor
  - [ ] 14-07-docs-PLAN.md — MIG-01..04: BLOCKSTORE_MIGRATION.md runbook with 4 worked transcripts + ARCHITECTURE/IMPLEMENTING_STORES/FAQ/CLI updates + human-verify checkpoint

### Phase 15: Legacy cleanup (A6)
**Goal**: After all production shares have migrated, remove the dual-read compatibility shim and delete every deprecated symbol — leaving only the CAS code path and the simplified three-state machine.
**Depends on**: Phase 14 (A5) — deferred until A5 rollout confirmed complete in production
**GH issue**: [#426](https://github.com/marmos91/dittofs/issues/426)
**Duration**: ~1 week
**Requirements**: TD-05, TD-06, TD-07, TD-08, TD-10
**Success Criteria** (what must be TRUE):
  1. Dual-read compatibility shim removed; engine reads only from `cas/...` keys; legacy `FormatStoreKey`/`ParseStoreKey`/`KeyBelongsToFile`/`ParseBlockIdx`/`parsePayloadIDFromBlockKey` deleted
  2. `nonClosingRemote` shim deleted from `pkg/controlplane/runtime/shares/service.go` — engine's `Close()` respects shared-remote ref-counting cleanly
  3. `SyncNow` spin-wait replaced with channel-based notification; per-download bridge-goroutine in `enqueueDownload` removed (queue worker signals directly); `Syncer.Close` double-drain consolidated to a single canonical drain path
  4. Legacy block-state enum values (`BlockStateDirty`, `BlockStateLocal`) deleted — only `Pending`, `Syncing`, `Remote` remain; legacy alias `FindFileBlockByHash` removed (only `GetByHash(ContentHash)` remains); `remote.CopyBlock` and `remote.DeleteByPrefix` deleted
  5. Code review confirms no remaining references to deprecated symbols; `go vet ./...` clean; all tests pass without legacy compat paths exercised
**Files to touch**:
  - `pkg/blockstore/types.go` — delete legacy key formatters, legacy state values
  - `pkg/blockstore/local/*`, `pkg/blockstore/remote/*` — delete legacy methods (`CopyBlock`, `DeleteByPrefix`)
  - `pkg/blockstore/engine/syncer.go` — channel-based `SyncNow`, single-drain `Close`
  - `pkg/blockstore/engine/fetch.go` — remove per-download bridge goroutine
  - `pkg/controlplane/runtime/shares/service.go` — delete `nonClosingRemote`
**Key risks**:
  - Deletion gated on production rollout — operators must confirm `dfsctl blockstore migrate status` reports 100% for every share before merge
  - Removing shim could surface latent bugs that were masked by dual-read fallback — run crash-injection suite + full WPTS + smbtorture baseline before merge
**Plans**: TBD

### Phase 16: Cache RAM-only (remove mmap read path)
**Goal**: Replace the `syscall.Mmap` zero-copy read path in `pkg/blockstore/engine/cache.go` with a `[]byte` read from the local block store. The Cache becomes pure RAM — `map[ContentHash]*list.Element` + `list.List` LRU, with bytes copied into LRU slots on miss. All LRU sizing, prefetch workers, sequential tracker, `nullCache{}` fallback, and the public `CacheInterface` (`Get/Put/OnRead/InvalidateFile/Stats/Close`) stay unchanged. The mmap files + Unix/Windows build-tag fork + D-33 perf gate are deleted. First of four phases in v0.16.0 — validates the Cache contract before Phase 17 swaps the underlying source.
**Depends on**: v0.15.0 complete (Phase 14 production-rollout window can lag)
**GH issue**: [#516](https://github.com/marmos91/dittofs/issues/516)
**Duration**: ~1 week
**Requirements**: (no formal REQ-IDs; phase scope is governed by CONTEXT.md decisions D-01..D-11)
**Success Criteria** (what must be TRUE):
  1. `pkg/blockstore/engine/cache_mmap_unix.go`, `pkg/blockstore/engine/cache_mmap_windows.go`, and `pkg/blockstore/engine/cache_mmap_test.go` are deleted; no remaining references to `syscall.Mmap`, `readFromCAS`, or `mmap` in `pkg/blockstore/engine/`
  2. `pkg/blockstore/local.LocalStore` exposes `Get(ctx context.Context, hash ContentHash) ([]byte, error)`; `*FSStore.Get` is a thin wrapper over existing `chunkstore.ReadChunk(h)`; returned `[]byte` is freshly allocated and owned by caller (D-03)
  3. `engine.loadByHash` at `pkg/blockstore/engine/engine.go:221` calls `local.Get(hash)` (no type assertion, no `readFromCAS`); Cache `loadFn` signature unchanged
  4. `TestPerfGate_Phase12_MmapHotPath` removed from `pkg/blockstore/engine/perf_bench_unix_test.go` (D-08); if file becomes empty, fold per Claude's Discretion
  5. `BenchmarkRandReadVerified` (warm-cache) ratio ≤1.02 vs pre-Phase-16 baseline (D-06); cross-OS build passes on Linux + Darwin + Windows (no per-OS cache file remains)
  6. Generic byte-correctness asserts from `cache_mmap_test.go` cherry-picked into `pkg/blockstore/engine/cache_test.go` (D-10); mmap-specific asserts (page-fault, 64 KiB threshold) deleted with no replacement
**Files to touch**:
  - `pkg/blockstore/engine/cache_mmap_unix.go` — **delete**
  - `pkg/blockstore/engine/cache_mmap_windows.go` — **delete**
  - `pkg/blockstore/engine/cache_mmap_test.go` — **delete** (cherry-pick generics into `cache_test.go`)
  - `pkg/blockstore/engine/perf_bench_unix_test.go` — delete `TestPerfGate_Phase12_MmapHotPath`
  - `pkg/blockstore/engine/engine.go:221` `loadByHash` — rewire `readFromCAS` → `local.Get`
  - `pkg/blockstore/local/local.go` — add `Get(ctx, hash)` to `LocalStore` interface
  - `pkg/blockstore/local/fs/fs.go` — implement `(*FSStore).Get` over `chunkstore.ReadChunk`
  - `pkg/blockstore/engine/cache_test.go` — absorb generic byte-correctness asserts
**Key risks**:
  - Forward-compat naming: `local.Get` signature must match Phase 17's `BlockStore.Get` exactly so the call site narrows without rename churn (D-01) — coordinate with Phase 17 scope
  - Generic asserts in `cache_mmap_test.go` must be preserved during delete (D-10) — review the test file before deletion, not after
  - Cold-cache regression is unverified by Phase 16 gates — production workloads are mostly warm, but cold-read complaints post-ship trigger the deferred `BenchmarkRandReadVerified_ColdCache` work in Phase 19
**Plans**: 4 plans
  - [x] 16-01-PLAN.md — Add LocalStore.Get(ctx, hash) interface method + FSStore (delegate to ReadChunk) + MemoryStore stub + localtest conformance scenario
  - [x] 16-02-PLAN.md — Rewire engine.loadByHash → local.Get; update cache.go docstring; cherry-pick generic byte-correctness asserts from cache_mmap_test.go into cache_test.go (D-10)
  - [x] 16-03-PLAN.md — Delete cache_mmap_unix.go + cache_mmap_windows.go + cache_mmap_test.go; delete TestPerfGate_Phase12_MmapHotPath; fold perf_bench_unix_test.go if empty; cross-OS build clean (D-08, D-09)
  - [x] 16-04-PLAN.md — Warm-cache BenchmarkRandReadVerified ≤1.02 vs pre-Phase-16 baseline (D-06); cross-OS build + race verification; BENCHMARKS.md update; human checkpoint

## Milestone Gates

Verification requirements VER-01 through VER-06 are phase-independent and gate the overall milestone rather than any single phase. These must all pass before v0.15.0 ships.

- [ ] **VER-01**: Canonical E2E test `TestBlockStoreImmutableOverwrites` passes (gates Phase 11)
- [ ] **VER-02**: Benchmark regression gates met on `bench/infra/scripts/dittofs-badger-s3.sh`:
  - Random write ≥600 IOPS (current 635)
  - Random read ≥1,350 IOPS (current 1,420)
  - Sequential write ≥48 MB/s (current 50.7)
  - Sequential read ≥60 MB/s (current 63.9)
- [ ] **VER-03**: VM-fleet dedup fixture achieves ≥40% storage reduction (primary business outcome; gates Phase 13)
- [ ] **VER-04**: Crash-injection suite green: kill mid-upload, kill mid-metadata-txn, corrupt S3 object all fail cleanly with explicit errors — no panics, no silent corruption
- [ ] **VER-05**: All three metadata backends (Memory, Badger, Postgres) pass the extended conformance suite with `[]BlockRef` round-trip and `ObjectID` stability
- [ ] **VER-06**: Property-based tests for FastCDC boundary stability and BLAKE3 reproducibility across platforms (darwin-arm64, linux-amd64) pass in CI

## Progress

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 08. Pre-refactor cleanup (A0) | 17/17 | Complete    | 2026-04-23 |
| 09. Adapter layer cleanup (ADAPT) | 0/5 | Not started | - |
| 10. FastCDC chunker + hybrid local store (A1) | 0/? | Not started | - |
| 11. CAS write path + GC rewrite (A2) | 0/? | Not started | - |
| 12. CDC read path + metadata schema + engine API (A3) | 13/13 | Complete    | 2026-04-27 |
| 13. Merkle root + file-level dedup (A4) | 14/15 | Complete    | 2026-04-28 |
| 14. Migration tool (A5) | 5/7 | In Progress|  |
| 15. Legacy cleanup (A6) | 0/? | Not started | - |
| 16. Cache RAM-only (remove mmap read path) | 3/4 | In Progress | - |

## Coverage Summary

**Total v0.15.0 requirements:** 79 mapped + 6 milestone gates = 85 line items
- Phase 08 (A0): 4 requirement groups (TD-01, TD-02, TD-03, TD-04)
- Phase 09 (ADAPT): 5 requirements (ADAPT-01..ADAPT-05)
- Phase 10 (A1): 7 requirements (BSCAS-02, LSL-01..LSL-06)
- Phase 11 (A2): 18 requirements (BSCAS-01/03/06, GC-01..GC-04, STATE-01..STATE-03, LSL-07/08, TD-09, INV-01/03/04/05/06)
- Phase 12 (A3): 14 requirements (API-01..API-04, META-01/03/04, CACHE-01..CACHE-06, INV-02)
- Phase 13 (A4): 6 requirements (BSCAS-04/05, META-02, DEDUP-01..DEDUP-03)
- Phase 14 (A5): 4 requirements (MIG-01..MIG-04)
- Phase 15 (A6): 5 requirements (TD-05..TD-08, TD-10)
- Milestone gates: 6 verification requirements (VER-01..VER-06)

All 79 phase-mapped requirements are covered exactly once; no orphans, no duplicates. VER-01..VER-06 are cross-cutting gates handled at milestone level.

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
- [x] **Phase 15: Legacy cleanup (A6)** — Remove dual-read shim, delete deprecated symbols (GH issue: #426) — **SUBSUMED BY v0.16.0 Phase 17** (legacy delete folded into unified BlockStore interface work; this issue can be closed once Phase 17 ships) (completed 2026-05-20)

---

## v0.16.0 — CAS Convergence

**Goal**: Converge DittoFS block storage on a single content-addressable layout. Collapse v0.15.0 dual-path complexity (legacy `.blk` + hybrid CAS, mmap + RAM cache, chunking-in-Syncer + chunking-at-rollup). Subsume v0.15.0 Phase 15 (A6) legacy cleanup. Ship one-shot migration command. No backward-compat shims — DittoFS has no production users.

**Phases:** 4 (numbered 16–19, continuing from v0.15.0 which ends at Phase 15)

16 (Cache RAM-only) ──► 17 (Unified BlockStore + legacy delete + migrate-to-cas) ──► 18 (Syncer mirror loop + ObjectID relocation) ──► 19 (Write-path RAM opts)

- [x] **Phase 16: Cache RAM-only (remove mmap read path)** — Delete `cache_mmap_*.go`, swap `readFromCAS` → `local.Get`, retire D-33 perf gate (GH issue: #516) — **SHIPPED 2026-05-20** on `gsd/phase-16-cache-mmap-removal` (16 commits); warm-cache D-06 PASS (ratio 0.890 ≤1.02)
- [ ] **Phase 17: Unified BlockStore interface + legacy delete + migration tool** — Single `BlockStore` interface (Put/Get/GetRange/Has/Delete/Walk/Head) for local + remote, `BlockStoreAppend` extends with random-write tier, delete legacy `.blk` writer + dual-read shim + flag, ship `dfsctl blockstore migrate-to-cas` one-shot (GH issue: #517)
- [x] **Phase 18: Syncer simplification + ObjectID relocation** — Syncer Flush → mirror loop `for hash := range local.ListUnsynced() { remote.Put(...) }`. Move `ComputeObjectID` to rollup CommitChunks. Local-only shares now get ObjectIDs. (GH issue: #518) (completed 2026-05-21)
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

### Phase 17: Unified BlockStore interface + legacy delete + migration tool
**Goal**: Collapse `LocalStore` (22 methods) and `RemoteStore` (12 methods) onto a single `BlockStore` interface keyed by `ContentHash` (Put/Get/GetRange/Has/Delete/Walk/Head + minimal `Meta{Size,LastModified}`). Local additionally implements `BlockStoreAppend` (random-write absorber tier). Delete the legacy path-keyed `.blk` writer, the engine dual-read shim, every legacy-tier helper (`IsBlockLocal`, `GetBlockData`, `ExistsOnDisk`, `DeleteBlockFile`, `DeleteAllBlockFiles`, `TruncateBlockFiles`, `WriteFromRemote`, `CopyBlock`, `FormatStoreKey`, `UseAppendLog`, `ErrAppendLogDisabled`). Collapse `localtest/` + `remotetest/` into a single `blockstoretest/` conformance suite (fs + s3 + memory). Ship `dfs migrate-to-cas` offline cobra subcommand (idempotent, journaled, `--dry-run`, `--share`, `--json`). Boot fails hard (`ErrLegacyLayoutDetected`, exit 78) on un-migrated stores via `.cas-migrated-v1` sentinel. Second of four phases in v0.16.0 — depends on Phase 16 (shipped). Subsumes v0.15.0 Phase 15 (A6) legacy cleanup.
**Depends on**: Phase 16 (shipped 2026-05-20)
**GH issue**: [#517](https://github.com/marmos91/dittofs/issues/517)
**Duration**: ~2 weeks
**Requirements**: (no formal REQ-IDs; phase scope governed by CONTEXT.md decisions D-01..D-11; subsumes TD-05..TD-08, TD-10 from Phase 15)
**Success Criteria** (what must be TRUE):
  1. `pkg/blockstore/blockstore.go` defines `BlockStore` + `BlockStoreAppend` interfaces + `Meta{Size int64, LastModified time.Time}`; `BlockStoreAppend` embeds `BlockStore`
  2. `RemoteStore` methods renamed (`WriteBlock`→`Put`, `ReadBlock`→`Get`, `ReadBlockRange`→`GetRange`, `DeleteBlock`→`Delete`, `HeadObject`→`Head`); `ListByPrefix*`/`DeleteByPrefix` collapsed into `Walk`; `CopyBlock` + `WriteBlockWithHash` deleted
  3. `LocalStore` narrowed from 22 to ~12 methods; embeds `BlockStoreAppend`; legacy helpers (`IsBlockLocal`, `GetBlockData`, `ExistsOnDisk`, `DeleteBlockFile`, `DeleteAllBlockFiles`, `TruncateBlockFiles`, `WriteFromRemote`, `FormatStoreKey`, `UseAppendLog`, `ErrAppendLogDisabled`) deleted
  4. `pkg/blockstore/local/fs/write.go` deleted (legacy path-keyed writer, `WriteAt`, `tryDirectDiskWrite`, `ensureBlockFile`, `directDiskWriteThreshold`, `<share>/<file>/<idx>.blk` layout, `memBlock` map)
  5. `pkg/blockstore/engine/store.go` dual-read shim deleted; engine reads only via `BlockStore.Get(ctx, hash)`
  6. `pkg/blockstore/blockstoretest/` exposes `BlockStoreConformance(t, factory)` + `BlockStoreAppendConformance(t, factory)`; fs/s3/memory backends pass applicable suite; `localtest/` + `remotetest/` deleted
  7. `pkg/blockstore/errors.go` adds `ErrStopWalk` + `ErrLegacyLayoutDetected`; Walk-callback returning `ErrStopWalk` exits cleanly, any other error halts and wraps with file/offset context (D-07)
  8. `dfs migrate-to-cas` offline cobra subcommand exists at `cmd/dfs/commands/migrate_to_cas.go`; refuses to run if server PID/lock file present; supports `--dry-run` (reports file count, bytes, est dedup ratio, ETA), `--share <name>` (default = all), `--json` (one JSON object per line), progress to stdout (`files/sec`, `MiB/sec`, `ETA`, `dedup hits`)
  9. Migration is idempotent: per-share journal at `<storage_dir>/<share>/.dittofs-migrate-to-cas.state`; crash-recovery resumes from last journaled offset
  10. `.cas-migrated-v1` sentinel written via atomic rename only at successful completion (records timestamp + tool version); `*fs.FSStore.NewFSStore` stats the sentinel at open time, returns `ErrLegacyLayoutDetected` (wrapping share path) when missing + `.blk` files present
  11. `cmd/dfs/start.go` unwraps `ErrLegacyLayoutDetected` via `errors.As`, prints multi-line stderr directive ("Detected legacy `.blk` layout at <path>. v0.16+ requires CAS migration. Run `dfs migrate-to-cas --share <name>`. See docs/CONFIGURATION.md §migration."), exits 78 (`EX_CONFIG`)
  12. Phase 16 warm-cache D-06 gate held (`BenchmarkRandReadVerified` ratio ≤1.02 vs pre-Phase-16 baseline); cross-OS build clean (Linux + Darwin + Windows); `go vet ./...` + `go test -race ./...` clean
  13. PR ships atomically (D-01): all interfaces + consumers + deletions + migration tool + boot guard in one PR against `develop`; ~30–40% LoC reduction in `pkg/blockstore/` (target under 4k from current ~6k); no flag-gated half-states, no transient compat shims
**Files to touch**:
  - `pkg/blockstore/blockstore.go` — **new or extended** — `BlockStore` + `BlockStoreAppend` interfaces, `Meta` struct
  - `pkg/blockstore/types.go` — delete `FormatStoreKey` + legacy `BlockStoreKey` parsing
  - `pkg/blockstore/errors.go` — add `ErrStopWalk`, `ErrLegacyLayoutDetected`
  - `pkg/blockstore/local/local.go` — narrow `LocalStore` to ~12 methods, embed `BlockStoreAppend`
  - `pkg/blockstore/local/fs/fs.go` — sentinel-file check in `NewFSStore`
  - `pkg/blockstore/local/fs/write.go` — **delete entirely**
  - `pkg/blockstore/remote/remote.go` — rename methods, collapse list/delete-by-prefix into `Walk`
  - `pkg/blockstore/remote/s3/store.go` — apply renames; preserve `x-amz-meta-content-hash` for BSCAS-06 defense-in-depth
  - `pkg/blockstore/engine/store.go` — delete dual-read shim
  - `pkg/blockstore/engine/syncer.go` + `upload.go` — rename `RemoteStore` call sites (`WriteBlock`→`Put`, `ReadBlock`→`Get`, `WriteBlockWithHash` collapsed into `Put`); Syncer logic simplification deferred to Phase 18
  - `pkg/blockstore/blockstoretest/` — **new directory** — unified conformance suite (Put-Get roundtrip, Get-not-found, range read, delete, walk, head, idempotent Put, concurrent Put-same-hash; AppendLog: AppendWrite-then-rollup → chunks via Walk, log deleted via DeleteLog)
  - `pkg/blockstore/local/localtest/` — **delete** (collapsed into `blockstoretest/`)
  - `pkg/blockstore/remote/remotetest/` — **delete** (collapsed into `blockstoretest/`)
  - `cmd/dfs/commands/migrate_to_cas.go` — **new** offline cobra subcommand
  - `pkg/blockstore/migrate/migrate_to_cas.go` — **new** shared library (FastCDC over `.blk` content, `Put(hash, data)`, rebuild `FileAttr.Blocks`, delete `.blk` files, write sentinel)
  - `cmd/dfs/start.go` — unwrap `ErrLegacyLayoutDetected`, print directive, exit 78
  - `pkg/blockstore/doc.go` — document sentinel-file convention + interface roles
  - `docs/CONFIGURATION.md` — add migration section with directive wording + exit code reference
**Key risks**:
  - **Mega-PR review burden** (~6k LoC delta, ~30–40% net reduction) — internal commit ordering (interfaces → consumers → deletions) is the only mitigation; `git log -p` reviewability is mandatory
  - **Atomic-merge constraint** (D-01) collides with bisectability if mid-PR commits leave develop unbuildable — order commits so each is independently buildable (additive first, then consumers, then deletions)
  - **Migration crash recovery** — journal-resume must be byte-exact across crashes; corruption test = kill -9 mid-FastCDC, restart, assert no duplicate hashes / no missing blocks
  - **Forward-compat with Phase 18** — `BlockStore.Get` signature must match Phase 16's `local.Get` verbatim AND remain compatible with Phase 18's Syncer mirror loop call sites (`for hash := range local.ListUnsynced() { remote.Put(...) }`); planner must read `~/.claude/plans/reactive-sprouting-moonbeam.md` Phase 18 section before locking signatures
  - **Boot-guard footgun** — sentinel must be operator-discoverable (clear stderr directive + `docs/CONFIGURATION.md` link); silent failure here strands operators on un-migrated shares
**Plans**: 11 plans
- [x] 17-01-PLAN.md — BlockStore + BlockStoreAppend interfaces + Meta struct + ErrStopWalk/ErrLegacyLayoutDetected sentinels (Wave 1)
- [x] 17-02-PLAN.md — pkg/blockstore/blockstoretest unified conformance suite scaffolding (Wave 1)
- [x] 17-03-PLAN.md — RemoteStore method renames + s3/memory backend retargeting (Wave 2)
- [x] 17-04-PLAN.md — LocalStore interface narrowing + BlockStoreAppend embedding (Wave 2)
- [x] 17-05-PLAN.md — Engine retargeted onto renamed RemoteStore + dual-read shim deleted (Wave 3)
- [x] 17-06-PLAN.md — Wire fs/memory/s3/memory-remote backends to blockstoretest + delete localtest/remotetest (Wave 3)
- [x] 17-07-PLAN.md — Delete write.go + FormatStoreKey + UseAppendLog + legacy helpers; restore LocalStore assertion (Wave 4)
- [x] 17-08-PLAN.md — pkg/blockstore/migrate/migrate_to_cas.go library + dfs migrate-to-cas cobra subcommand (Wave 5) — shipped 2026-05-20 (`081f31c4`, `177c9c37`, `bd253756`, `6d3e0267`, `3e9ed645`)
- [x] 17-09-PLAN.md — NewFSStore sentinel check + cmd/dfs/start.go boot-guard exit-78 (Wave 5) — shipped 2026-05-20 (`5961536c`, `6f3e0326`, `9fb382a7`, `b7f4d00d`)
- [x] 17-10-PLAN.md — pkg/blockstore/doc.go convention + docs/CONFIGURATION.md Migration section + docs/CLI.md entry (Wave 6) — shipped 2026-05-20 (`bb97ec34`, `99b5ef58`, `9f604247`)
- [x] 17-11-PLAN.md — Phase 17 VERIFICATION.md: perf gate, LoC measurement, STRIDE consolidation, smoke test (Wave 6)

### Phase 18: Syncer simplification + ObjectID relocation
**Goal**: Rewrite `pkg/blockstore/engine/syncer.go` from a per-block chunk-and-upload orchestrator (~600 LoC) into a byte-identical `local → remote` mirror loop (~50 LoC body): `for hash := range local.ListUnsynced(ctx) { data,_ := local.Get(ctx, hash); remote.Put(ctx, hash, data); syncedStore.MarkSynced(ctx, hash) }`. Move `ComputeObjectID` out of Syncer and into `pkg/blockstore/local/fs/rollup.go`'s `CommitChunks` post-hook so local-only shares get ObjectIDs (real surprise in Phase 13 UAT). Relocate `TrySpeculativeFileLevelDedup` to `engine.Flush()` as a pre-rollup hook (private call into existing `engine/dedup.go`). Delete the 7 `TRANSITIONAL-PHASE-18` admin methods on `LocalStore` (`ReadAt`, `WriteAt`, `Flush`, `IsBlockLocal`, `GetBlockData`, `WriteFromRemote`, `DeleteAllBlockFiles`) plus `Flush` return type + `FlushedBlock` + public `Syncer.TrySpeculativeFileLevelDedup` seam. Introduce new `metadata.SyncedHashStore` interface (3 methods: `IsSynced`, `MarkSynced`, `DeleteSynced`) mirroring the existing `metadata.RollupStore` pattern; implement on `badger`, `postgres`, `memory` backends; inject into `*fs.FSStore` via `FSStoreOptions.SyncedHashStore`. Third of four phases in v0.16.0 (CAS Convergence). Depends on Phase 17 (Unified BlockStore, shipped commit `d225926f`). Unblocks Phase 19 (Write-path RAM optimizations).
**Depends on**: Phase 17 (shipped 2026-05-20, commit `d225926f`)
**GH issue**: [#518](https://github.com/marmos91/dittofs/issues/518)
**Duration**: ~1.5 weeks
**Requirements**: (no formal REQ-IDs; phase scope governed by CONTEXT.md decisions D-01..D-19)
**Success Criteria** (what must be TRUE):
  1. `pkg/metadata/synced_hash_store.go` defines `SyncedHashStore` interface (`IsSynced(ctx, hash) (bool, error)`, `MarkSynced(ctx, hash) error`, `DeleteSynced(ctx, hash) error`) mirroring `RollupStore` shape (D-02)
  2. `SyncedHashStore` implemented on `pkg/metadata/badger` (key prefix `synced/<hex-hash>`), `pkg/metadata/postgres` (table `synced_hashes (hash bytea PRIMARY KEY, synced_at timestamptz)`), `pkg/metadata/memory` (`map[ContentHash]time.Time`); all three pass shared conformance suite (D-02)
  3. `*fs.FSStoreOptions.SyncedHashStore` injection slot added; `*fs.FSStore` plumbs it through to engine Syncer construction (D-02)
  4. `local.LocalStore.ListUnsynced(ctx) iter.Seq2[blockstore.ContentHash, error]` (Go 1.23 push iterator) walks local CAS chunks and filters via `SyncedHashStore.IsSynced`; snapshot-at-start semantics (D-04, D-05)
  5. `pkg/blockstore/engine/syncer.go::Flush` body collapses to ~50 LoC mirror loop: iterate `local.ListUnsynced`, call `remote.Put(hash, data)`, then `syncedStore.MarkSynced(hash)`; ordering is Put-then-Mark with idempotent-replay crash semantics (D-07, D-16)
  6. `ComputeObjectID` relocated out of Syncer; called from `pkg/blockstore/local/fs/rollup.go` after `rollupStore.SetRollupOffset` returns nil: `coordinator.PersistFileBlocks(payloadID, blocks, ComputeObjectID(blocks))`. Local-only shares get ObjectIDs (D-10)
  7. `engine.Delete` cascades `syncedStore.DeleteSynced(hash)` in the same critical section that fires `DeleteChunk` when refcount reaches 0 — synced set is a strict subset of local CAS contents (D-09)
  8. Public `Syncer.TrySpeculativeFileLevelDedup` seam (engine/syncer.go:176) DELETED; `engine.Flush()` calls the private `trySpeculativeFileLevelDedup` in `engine/dedup.go` as a pre-rollup hook (D-12, D-17)
  9. `pkg/blockstore/engine/syncer.go` deletions: `uploadOne`, `drainPayloadToRemote`, `persistFileBlocksAfterFlush`, in-Syncer BLAKE3 recompute at `upload.go:86`, ObjectID compute call site (D-11)
  10. `pkg/blockstore/local/local.go` deletions: the 7 `TRANSITIONAL-PHASE-18` methods (`ReadAt`, `WriteAt`, `Flush`, `IsBlockLocal`, `GetBlockData`, `WriteFromRemote`, `DeleteAllBlockFiles`) + `FlushedBlock` type + bridge `Flush` return type (D-18). Admin/lifecycle methods (Truncate, EvictMemory, SetRetentionPolicy, SetEvictionEnabled, Stats, ListFiles, GetStoredFileSize, Healthcheck, SyncFileBlocks, SyncFileBlocksForFile, Start, Close, DeleteAppendLog) STAY
  11. Phase 13 dedup conformance tests retargeted onto new `engine.Flush` entrypoint; spec-named `Syncer.Flush_InvokesPostFlushHook` rewritten to `TestRollup_CommitChunks_PersistsObjectID`; all neighbor tests touching `TrySpeculativeFileLevelDedup`, `persistFileBlocksAfterFlush`, or in-Syncer ObjectID compute ported in one pass (D-11, D-13, D-14)
  12. `pkg/blockstore/engine/syncer_test.go` re-created with `//go:build integration` tag; covers mirror-loop happy path, Put-then-Mark crash-replay window, ListUnsynced snapshot semantics, refcount cascade DeleteSynced; exercises s3 + memory remote backends (D-15)
  13. `pkg/blockstore/doc.go` documents `TRANSITIONAL-NEXT-MILESTONE:` deprecation marker convention (D-19)
  14. Syncer keeps its filename + struct name + auxiliary state (periodic uploader, claimBatch worker pool, `uploading` atomic gate, health monitor, backpressure-on-remote-outages) — git blame preserved (D-16)
  15. PR ships atomically (D-01): SyncedHashStore additions + Syncer rewrite + ObjectID relocation + TrySpeculativeFileLevelDedup relocation + 7 transitional deletions + Flush/FlushedBlock deletions + Phase 13 + Phase 17 test reshape in one PR against `develop`; internal commit ordering staged (additive interfaces → consumers migrated → deletions), each commit independently buildable; no flag-gated half-states
  16. `go vet ./...` + `go test -race ./...` + `go test -tags=integration ./pkg/blockstore/engine/...` clean; cross-OS build clean (Linux + Darwin); Phase 16 D-06 warm-cache gate held
**Files to touch**:
  - `pkg/metadata/synced_hash_store.go` — **new** — `SyncedHashStore` interface (mirrors `rollup_store.go`)
  - `pkg/metadata/storetest/synced_hash_store_conformance.go` — **new** — shared conformance suite (3 backends)
  - `pkg/metadata/badger/synced_hash_store.go` — **new** — key prefix `synced/<hex-hash>`
  - `pkg/metadata/postgres/synced_hash_store.go` — **new** — `synced_hashes` table + migration
  - `pkg/metadata/postgres/migrations/` — **new** migration file for `synced_hashes`
  - `pkg/metadata/memory/synced_hash_store.go` — **new** — `map[ContentHash]time.Time`
  - `pkg/blockstore/local/local.go` — delete the 7 `TRANSITIONAL-PHASE-18` methods + `FlushedBlock` + bridge `Flush` return; add `ListUnsynced(ctx) iter.Seq2[ContentHash, error]`
  - `pkg/blockstore/local/fs/fs.go` — add `FSStoreOptions.SyncedHashStore` slot; plumb through `NewFSStore`; implement `ListUnsynced` (Walk + IsSynced filter)
  - `pkg/blockstore/local/fs/rollup.go` — relocate `ComputeObjectID` call into post-`SetRollupOffset` hook; invoke `coordinator.PersistFileBlocks` with ObjectID
  - `pkg/blockstore/engine/syncer.go` — collapse `Flush` body to mirror loop (~50 LoC); delete `uploadOne`, `drainPayloadToRemote`, `persistFileBlocksAfterFlush`, public `TrySpeculativeFileLevelDedup`, in-Syncer ObjectID compute; keep auxiliary state (periodic uploader, claimBatch, gates, health monitor)
  - `pkg/blockstore/engine/upload.go` — delete BLAKE3 recompute at line 86; collapse what remains into Syncer.Flush or delete
  - `pkg/blockstore/engine/engine.go` — `Flush()` calls private `trySpeculativeFileLevelDedup` (dedup.go) pre-rollup; `Delete()` cascades `syncedStore.DeleteSynced(hash)` when refcount=0
  - `pkg/blockstore/engine/dedup.go` — private `trySpeculativeFileLevelDedup` stays; remove public export wrapper
  - `pkg/blockstore/engine/syncer_test.go` — **re-create** with `//go:build integration` (mirror-loop, crash-replay, snapshot, refcount cascade)
  - `pkg/blockstore/engine/dedup_test.go` (or wherever Phase 13 dedup conformance lives) — port all touched tests onto new `engine.Flush` entrypoint
  - `pkg/blockstore/doc.go` — document `TRANSITIONAL-NEXT-MILESTONE:` convention
**Key risks**:
  - **Mega-PR review burden** (SyncedHashStore × 3 backends + Syncer rewrite + ObjectID relocation + deletions + test reshape) — staged commit ordering (additive → migrate → delete) is the only mitigation; `git log -p` reviewability mandatory
  - **Atomic-merge constraint** (D-01) — each commit must keep `go build ./... + go vet ./... + go test ./...` green; no flag-gated half-state
  - **Crash-replay correctness** (D-07) — Put-then-Mark window: crash after Put-success/pre-Mark must re-Put on next pass without corruption. Requires `remote.Put` idempotent-on-identical-bytes (Phase 17 contract) + integration test exercising kill-9 mid-Flush
  - **ListUnsynced snapshot vs hot-write workloads** (D-05) — iterator captures hash set at iteration start; new chunks rolled up mid-pass picked up on NEXT pass. Must not hold a read-lock for the iteration lifetime
  - **Refcount cascade race** (D-09) — `engine.Delete` must call `DeleteSynced` in the same critical section as `DeleteChunk`; otherwise a parallel Syncer pass could re-Mark a hash that was just deleted locally
  - **Phase 13 test sweep coverage** (D-14) — partial sweep leaves neighbor tests red on CI; planner must enumerate every test touching the deleted seams before the deletion commit
**Plans** (9 plans):
- [x] 18-01-PLAN.md — SyncedHashStore interface + conformance suite + memory backend (Wave 1)
- [x] 18-02-PLAN.md — Badger backend SyncedHashStore (Wave 2)
- [x] 18-03-PLAN.md — Postgres backend SyncedHashStore + migration 000015 (Wave 2)
- [x] 18-04-PLAN.md — LocalStore.ListUnsynced + FSStoreOptions.SyncedHashStore injection (Wave 2)
- [x] 18-05-PLAN.md — ComputeObjectID relocation to rollup.go post-SetRollupOffset hook (Wave 3)
- [x] 18-06-PLAN.md — Syncer.Flush mirror-loop rewrite + BLAKE3 recompute deletion (Wave 4)
- [x] 18-07-PLAN.md — engine.Flush dedup pre-hook + engine.Delete cascade + public TrySpec deletion (Wave 5)
- [x] 18-08-PLAN.md — TRANSITIONAL-PHASE-18 methods deletion + Phase 13 test sweep (Wave 6)
- [x] 18-09-PLAN.md — Integration syncer_test.go + TRANSITIONAL-NEXT-MILESTONE doc.go convention (Wave 7)

### Phase 19: Write-path RAM optimizations
**Goal**: Land four independent write-path RAM/temp-store throughput optimizations on the unified CAS BlockStore: (1) in-memory hash dedup LRU between FastCDC `Next()` and `Put(hash, data)` in `pkg/blockstore/local/fs/rollup.go` — on LRU hit skip CAS Put + StoreChunk and bump refcount via new `FileBlockStore.AddRef`; (2) group commit / batched fsync — replace per-record fsync at `pkg/blockstore/local/fs/appendwrite.go:259` with a per-file 1ms commit pipeline window + adaptive depth=1 bypass; (3) direct-to-Cache on chunk completion — extend `chunkstore.lruTouch` at `pkg/blockstore/local/fs/chunkstore.go:104` to invoke an engine Cache callback injected via `FSStoreOptions.OnChunkComplete`, eliminating the wrote-then-read disk hop on NFS COMMIT-then-READ; (4) eager small-file dedup — files ≤ FastCDC `MinChunk` (1 MiB) hash whole content in RAM and short-circuit via `metadata.FindByObjectID` BEFORE rollup runs, hooking into `engine.Flush`'s existing pre-rollup site alongside `trySpeculativeFileLevelDedup`. New surfaces: `FileBlockStore.AddRef(ctx, hash, payloadID, blockRef) error` on all 3 metadata backends (badger/postgres/memory); `FSStoreOptions.OnChunkComplete func(hash ContentHash, data []byte, path string)`. New config knob: `blockstore.local.dedup_lru_size` (default 4096). D-21 aggregate gate tightened from ≤1.02 to **≤1.00 vs Phase 11 baseline**. Closes Phase 18 D-16 deprecation cycle: delete `SyncerConfig.ClaimBatchSize` field + `pkg/config` schema entry. Fourth (final) phase of v0.16.0 (CAS Convergence). Depends on Phase 18 (shipped 2026-05-21, PR #537 @ `b31b01f7`).
**Depends on**: Phase 18 (shipped 2026-05-21, PR #537 @ `b31b01f7`)
**GH issue**: [#519](https://github.com/marmos91/dittofs/issues/519); sub-tracker [#543](https://github.com/marmos91/dittofs/issues/543)
**Duration**: ~1.5 weeks
**Requirements**: (no formal REQ-IDs; phase scope governed by CONTEXT.md decisions D-01..D-27, preserves BSCAS-01..06, LSL-01..08, CACHE-01..06, META-01..04, DEDUP-01..03, STATE-01..03)
**Success Criteria** (what must be TRUE):
  1. `pkg/blockstore/local/fs/dedup_lru.go` defines a per-FSStore stripe-locked hash LRU (`Get/Put/Has`) sized by config; `rollup.go` consults it between FastCDC `Next()` and `Put` (D-02, D-05, D-22a)
  2. `pkg/metadata/file_block_store.go` extends `FileBlockStore` with `AddRef(ctx, hash, payloadID, blockRef) error`; returns `ErrUnknownHash` sentinel when the hash row is absent (D-04, D-22b)
  3. `AddRef` implemented on `pkg/metadata/badger`, `pkg/metadata/postgres`, `pkg/metadata/memory`; all three pass new `pkg/metadata/storetest/` conformance scenarios — existing-hash (RefCount +1, state preserved), missing-hash (returns sentinel, no row created), concurrent AddRef vs DecrementRefCount cascade (no negative RefCount, no orphan) (D-04, D-21, D-27)
  4. `blockstore.local.dedup_lru_size` config knob (default 4096) wired through `pkg/config/blockstore.go` and consumed by `FSStore` construction; nested under existing `blockstore.local.*` shape (D-03, D-22c)
  5. `pkg/blockstore/local/fs/groupcommit.go` defines per-`logFile` group-commit coordinator (`pending []chan error`, `*time.Timer`, `sync.Mutex`); `appendwrite.go:259` calls coordinator instead of raw `lf.f.Sync()`; fixed 1ms commit window + adaptive bypass when queue depth = 1 (D-06, D-07, D-09, D-22a)
  6. Group-commit preserves synchronous durability contract: caller blocks until its batch fsyncs; NFS COMMIT / SMB Flush callers see no async ack (D-08)
  7. Group-commit honors existing lock ordering rule from `appendwrite.go`: per-file `mu` before `bc.logsMu`; coordinator never touches `logsMu` (D-09)
  8. `FSStoreOptions.OnChunkComplete func(hash ContentHash, data []byte, path string)` field added; nil-safe (chunkstore behaves identically to today on nil); engine constructs `FSStore` with the callback; `chunkstore.lruTouch` at `chunkstore.go:104` invokes it exactly once per successful touch, post-disk-store, lock-held (D-10, D-12)
  9. Engine wires `OnChunkComplete` to `Cache.Put`; RAM ceiling bounded by engine Cache's existing size-bounded LRU — no extra cap (D-11, D-16)
  10. `engine.Flush` pre-rollup hook at `engine.go:669` runs eager small-file dedup BEFORE `trySpeculativeFileLevelDedup`: files ≤ `FastCDC.MinChunk` (1 MiB) → hash whole content → compute single-block `ObjectID` (= the hash) → `metadata.FindByObjectID` → hit short-circuits chunker + log + CAS write; miss falls through to existing speculative dedup → rollup (D-13, D-14)
  11. Eager-dedup HIT populates engine Cache for the matched hash using the RAM-resident bytes; MISS lets data flow into rollup path which also populates Cache via D-10. Every small-file write leaves a warm Cache entry (D-16)
  12. STATE-01..03 invariant preserved: LRU hit path increments `RefCount` only via `AddRef`; no new block row, no state transition, no skip-Pending optimization (D-27)
  13. Bench gating policy honored: hard-gate correctness tests `TestCache_PopulatedOnRollupComplete` (Opt 3) + `TestSmallFileEagerDedup_BSCAS06` (Opt 4) PASS; yellow-flag perf benches `BenchmarkRandWriteCAS_IdempotentBytes` + `BenchmarkAppendWrite_GroupCommit` report ratios without blocking (D-17)
  14. **D-21 aggregate gate ≤1.00 vs Phase 11 baseline** PASS (tightened from ≤1.02, D-19); D-41 (cross-VM dedup ≥40%), D-43 (RandRead warm-cache ≤1.02), D-06 (RandReadVerified warm ≤1.02) gates held unchanged
  15. New bench/test files exist: `pkg/blockstore/local/fs/appendwrite_group_commit_bench_test.go`, `pkg/blockstore/local/fs/rollup_idempotent_dedup_bench_test.go`, `pkg/blockstore/engine/cache_populated_on_rollup_test.go`, `pkg/blockstore/engine/small_file_eager_dedup_test.go`; `internal/bench/phase19_test.go` aggregate runner emits D-21 ratio (D-20)
  16. Phase 18 D-16 deprecation cycle CLOSED: `SyncerConfig.ClaimBatchSize` field + `pkg/config` schema entry + the no-op test that asserts it parses are deleted; TRANSITIONAL-NEXT-MILESTONE marker in `pkg/blockstore/doc.go` updated (D-23)
  17. Dead `LocalStore` admin-method sweep complete: methods whose only callers were Phase 18 transitional code are removed (D-18 admin-superset — Truncate, EvictMemory, SetRetentionPolicy, Stats, ListFiles, etc. — STAYS); audit-finds-nothing is acceptable (D-24)
  18. Every `TRANSITIONAL-NEXT-MILESTONE:` marker across `pkg/blockstore/` resolved: addressed-by-19 markers deleted alongside the change; still-deferred markers updated with concrete target milestone where known (D-25)
  19. New `TRANSITIONAL-NEXT-MILESTONE:` anchors added at v0.17+ hook sites listed in #519's "Deferred" section: pinned hot-tail RAM (`chunkstore.go` on-disk store path), tmpfs spill (`appendlog.go` log overflow site), O_DIRECT (`appendwrite.go` near `f.Sync()`), zstd compression (`chunkstore.go::StoreChunk`), cold-cache prefetch (`engine/cache.go`); each marker references #519 "Deferred to v0.17+" (D-26)
  20. PR ships atomically as a single mega-PR (D-01): all 4 opts + new surfaces + benches + config knob + D-21 tightening + D-23..D-26 cleanups together; internal commit ordering staged (additive interfaces → integration → tests/benches → cleanup sweeps), each commit independently buildable; no flag-gated half-states
  21. `go vet ./...` + `go test -race ./...` + cross-OS build (Linux + Darwin) clean; D-21 ratio ≤1.00 confirmed on bench infra
**Files to touch**:
  - `pkg/blockstore/local/fs/dedup_lru.go` — **new** — stripe-locked hash LRU (Opt 1)
  - `pkg/blockstore/local/fs/groupcommit.go` — **new** — per-file group-commit coordinator (Opt 2)
  - `pkg/blockstore/local/fs/rollup.go` — Opt 1 hook between FastCDC `Next()` and `Put(hash, data)`; invoke `FileBlockStore.AddRef` on LRU hit
  - `pkg/blockstore/local/fs/appendwrite.go` — Opt 2 hook at line 259; replace raw `lf.f.Sync()` with coordinator call
  - `pkg/blockstore/local/fs/chunkstore.go` — Opt 3 hook at line 104 (`lruTouch`); invoke `FSStoreOptions.OnChunkComplete` callback exactly once per successful touch; add TRANSITIONAL-NEXT-MILESTONE markers (pinned hot-tail RAM, zstd compression anchors — D-26)
  - `pkg/blockstore/local/fs/fs.go` — add `FSStoreOptions.OnChunkComplete` slot; plumb through `NewFSStore`
  - `pkg/blockstore/engine/engine.go` — Opt 4 hook at line 669 in `engine.Flush` pre-rollup hook; wire `OnChunkComplete` → `Cache.Put`
  - `pkg/blockstore/engine/dedup.go` — Opt 4 eager small-file fast-track (≤ `FastCDC.MinChunk`) calling `FindByObjectID` before falling through to `trySpeculativeFileLevelDedup`
  - `pkg/blockstore/engine/cache.go` — add TRANSITIONAL-NEXT-MILESTONE marker (cold-cache prefetch anchor — D-26)
  - `pkg/blockstore/local/fs/appendlog.go` — add TRANSITIONAL-NEXT-MILESTONE marker (tmpfs spill anchor — D-26)
  - `pkg/metadata/file_block_store.go` — extend `FileBlockStore` interface with `AddRef`
  - `pkg/metadata/errors.go` (or backend-local) — `ErrUnknownHash` sentinel
  - `pkg/metadata/badger/file_block_store.go` — implement `AddRef`
  - `pkg/metadata/postgres/file_block_store.go` — implement `AddRef`
  - `pkg/metadata/memory/file_block_store.go` — implement `AddRef`
  - `pkg/metadata/storetest/file_block_store_conformance.go` — add `AddRef` conformance scenarios (3 cases)
  - `pkg/config/blockstore.go` — add `blockstore.local.dedup_lru_size` knob (default 4096)
  - `pkg/config/syncer.go` + `pkg/blockstore/engine/syncer.go` — D-23 delete `SyncerConfig.ClaimBatchSize` field + schema entry + no-op test
  - `pkg/blockstore/doc.go` — update TRANSITIONAL-NEXT-MILESTONE marker (D-23) + record convention notes
  - `pkg/blockstore/local/fs/appendwrite_group_commit_bench_test.go` — **new** — Opt 2 perf bench (yellow-flag)
  - `pkg/blockstore/local/fs/rollup_idempotent_dedup_bench_test.go` — **new** — Opt 1 perf bench (yellow-flag)
  - `pkg/blockstore/engine/cache_populated_on_rollup_test.go` — **new** — Opt 3 correctness gate (hard-gate)
  - `pkg/blockstore/engine/small_file_eager_dedup_test.go` — **new** — Opt 4 correctness gate (hard-gate)
  - `internal/bench/phase19_test.go` — D-21 aggregate runner (tighten to ≤1.00)
**Key risks**:
  - **Mega-PR review burden** (4 opts + `AddRef` × 3 backends + benches + cleanups) — staged commit ordering (additive surface → consumers wired → benches → cleanup sweeps) is the only mitigation; `git log -p` reviewability mandatory
  - **Atomic-merge constraint** (D-01) — each commit must keep `go build ./... + go vet ./... + go test ./...` green; no flag-gated half-state
  - **D-21 ≤1.00 tightening** (D-19) — bench variance can mask wins; aggregate runner + warm-cache invariant + bench infra cold-start hygiene required to reproduce
  - **STATE-01..03 invariant on LRU hit** (D-27) — temptation to skip-Pending or invent a "DedupReference" state is exactly the kind of optimization Phase 19 must NOT make; `AddRef` is refcount-only, no state transition
  - **Lock ordering in groupcommit** (D-09) — coordinator must NOT touch `bc.logsMu`; per-file `mu` first invariant from FIX-2/FIX-20 holds
  - **Idempotency contract on `OnChunkComplete`** (D-12) — "exactly once per successful touch" — fire-on-error or fire-twice both leave Cache in wrong state
  - **TOCTOU race on `AddRef` vs `engine.Delete` cascade** (D-04) — `AddRef` and `DecrementRefCount` cascade must serialize on the same row lock; conformance scenario explicitly covers this
  - **Cache thrash on Opt 3 push under burst** (D-11) — bounded by engine Cache LRU; if bench shows thrash, deferred skip-on-pressure signal moves into v0.17+ (already noted in CONTEXT.md `<deferred>`)
**Plans**: 10 plans
  - [ ] 19-01-PLAN.md — FileBlockStore.AddRef interface + ErrUnknownHash sentinel + storetest conformance scenarios (wave 1)
  - [ ] 19-02-PLAN.md — AddRef implementations across memory + badger + postgres backends (wave 1)
  - [ ] 19-03-PLAN.md — dedup_lru.go + groupcommit.go standalone units + pkg/config/blockstore.go DedupLRUSize knob (wave 1)
  - [ ] 19-04-PLAN.md — FSStoreOptions.OnChunkComplete + DedupLRUSize slots + FSStore field instantiation (wave 1)
  - [ ] 19-05-PLAN.md — Opt 1 wire-in: rollup.go LRU consult + AddRef fast-path (wave 2)
  - [ ] 19-06-PLAN.md — Opt 2 wire-in: appendwrite.go:259 fsync coalesce via per-logFile groupCommit (wave 2)
  - [ ] 19-07-PLAN.md — Opt 3 wire-in: chunkstore.lruTouch fires OnChunkComplete; engine wires to Cache.Put (wave 2)
  - [ ] 19-08-PLAN.md — Opt 4 wire-in: eager small-file dedup in engine.Flush before trySpeculativeFileLevelDedup (wave 2)
  - [ ] 19-09-PLAN.md — Correctness + perf benches + D-21 aggregate gate tightening to ≤1.00 (wave 3)
  - [ ] 19-10-PLAN.md — D-23 ClaimBatchSize deletion + D-24 admin-method audit + D-25 marker audit + D-26 tmpfs spill anchor + doc.go (wave 4)

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
| 16. Cache RAM-only (remove mmap read path) | 4/4 | Complete    | 2026-05-20 |

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

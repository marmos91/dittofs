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
- [ ] **Phase 12: CDC read path + metadata schema + engine API (A3)** — `[]BlockRef` API, Cache by ContentHash, schema migration (GH issue: #423)
- [ ] **Phase 13: Merkle root + file-level dedup (A4)** — `FileAttr.ObjectID` + short-circuit full-file dedup (GH issue: #424)
- [ ] **Phase 14: Migration tool (A5)** — `dfsctl blockstore migrate` offline re-chunk + re-hash (GH issue: #425)
- [ ] **Phase 15: Legacy cleanup (A6)** — Remove dual-read shim, delete deprecated symbols (GH issue: #426)

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
**Plans**: TBD

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
**Plans**: TBD

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
**Plans**: TBD

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
**Plans**: TBD

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
**Plans**: TBD

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
| 12. CDC read path + metadata schema + engine API (A3) | 0/? | Not started | - |
| 13. Merkle root + file-level dedup (A4) | 0/? | Not started | - |
| 14. Migration tool (A5) | 0/? | Not started | - |
| 15. Legacy cleanup (A6) | 0/? | Not started | - |

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

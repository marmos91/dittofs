# v0.15.0 Requirements — Block Store + Core-Flow Refactor

**Milestone:** v0.15.0 Block Store + Core-Flow Refactor
**Parent issue:** [#419](https://github.com/marmos91/dittofs/issues/419)
**GitHub milestone:** [v0.15.0](https://github.com/marmos91/dittofs/milestone/2)
**Source plan:** `/Users/marmos91/.claude/plans/i-spotted-a-problem-witty-llama.md`
**Defined:** 2026-04-23

**Core Value:** Make block keys immutable by construction (CAS), unblocking per-share atomic backups in v0.16.0; deliver 40–80% cross-VM dedup via FastCDC for the primary VM-backed NAS workload; bundle accumulated design-debt cleanup across the adapter → engine → metadata/block core flow.

---

## v0.15.0 Requirements

### Block-store key scheme & chunking (BSCAS)

- [ ] **BSCAS-01**: Remote block keys use content-addressable format `cas/{hash[0:2]}/{hash[2:4]}/{hash_hex}` with 2-level fanout. Keys carry `blake3:` prefix.
- [ ] **BSCAS-02**: FastCDC content-defined chunking with parameters min=1 MB / avg=4 MB / max=16 MB (normalization level 2). Runs at sync finalization over dirty regions with stabilization window.
- [ ] **BSCAS-03**: BLAKE3 hashing via `github.com/zeebo/blake3` replaces SHA-256 for block identity and file-level integrity.
- [ ] **BSCAS-04**: `FileAttr.ObjectID` populated lazily (at file quiesce) as BLAKE3 Merkle root over sorted block hashes.
- [ ] **BSCAS-05**: File-level dedup short-circuits chunking on full-file writes when provisional `ObjectID` matches an existing file — reuse `BlockRef` list, zero new uploads.
- [ ] **BSCAS-06**: Every S3 PUT includes `x-amz-meta-content-hash: blake3:{hex}` so external tooling can verify without DittoFS metadata.

### Local store — hybrid Logs + Blocks (LSL)

- [ ] **LSL-01**: Per-file append-only write log at `logs/{payloadID}.log` with 64-byte header (magic, version, consumed_pos) + CRC-per-record format.
- [ ] **LSL-02**: Hash-keyed chunk directory at `blocks/{hash[0:2]}/{hash[2:4]}/{hash_hex}` for long-lived on-disk chunks.
- [ ] **LSL-03**: `AppendWrite` replaces legacy `WriteAt` + `tryDirectDiskWrite` — one code path, append to log.
- [ ] **LSL-04**: Pressure channel signals syncer when log exceeds budget; writer blocks on excess, unblocks when syncer drains.
- [ ] **LSL-05**: `CommitChunks` is atomic: metadata txn + log `consumed_pos` advance happen as one unit.
- [ ] **LSL-06**: Crash recovery per-file: scan from `consumed_pos`, truncate at first bad CRC, re-chunk surviving records on first sync.
- [ ] **LSL-07**: `LocalStore` interface narrowed from 22 methods to ~17 — removes implementation-detail leaks (`MarkBlockRemote`, `GetDirtyBlocks`, `SetSkipFsync`, etc.).
- [ ] **LSL-08**: Local store no longer calls `FileBlockStore` on write hot path — eviction driven from on-disk state only.

### In-memory Cache (CACHE)

- [ ] **CACHE-01**: `Cache` type in `engine/cache.go` replaces `readbuffer` + `prefetcher` (one concept, one API).
- [ ] **CACHE-02**: Cache keyed by `ContentHash` — deduped chunks cache once, not once-per-file.
- [ ] **CACHE-03**: Sequential-detection prefetch (3+ consecutive reads triggers prefetch of next 1–8 chunks) runs internally with bounded concurrency.
- [ ] **CACHE-04**: `Cache.OnRead(payloadID, nextHashes)` is the sole hint API; consumers never see a separate prefetcher type.
- [ ] **CACHE-05**: `Cache.InvalidateFile(payloadID)` invalidates all cached chunks for a file on writes/truncates/deletes.
- [ ] **CACHE-06**: Zero-copy or single-copy reads for local hits (avoid double allocation of `GetBlockData` + `Cache.Put`).

### Engine API (API)

- [ ] **API-01**: `engine.BlockStore.ReadAt(ctx, blocks []BlockRef, dest, offset)` accepts pre-fetched blocks from caller.
- [ ] **API-02**: `WriteAt` signature cleaned so engine does not import `pkg/metadata` for hot paths.
- [ ] **API-03**: `CopyPayload` becomes O(1) — refcount increments on shared `BlockRef` list instead of block-by-block data copy.
- [ ] **API-04**: Binary search on `[]BlockRef` (sorted by offset) to locate chunks covering a read range.

### Metadata schema (META)

- [ ] **META-01**: `FileAttr.Blocks` changes from `[]string` (experimental, unused) to `[]BlockRef{Hash, Offset, Size}` — authoritative, sorted by offset, populated on every sync finalization.
- [ ] **META-02**: `FileAttr.ObjectID` changes from unused field to BLAKE3 Merkle-root populated at file quiesce.
- [ ] **META-03**: `FileBlockStore` interface narrowed to 6 methods keyed by `ContentHash` (`GetByHash`, `Put`, `Delete`, `IncrementRefCount`, `DecrementRefCount`, `ListPending`).
- [ ] **META-04**: Badger, Postgres, Memory backends all pass the extended conformance suite for `[]BlockRef` round-trip and `ObjectID` stability.

### Garbage collection (GC)

- [ ] **GC-01**: Mark phase: live set is union of all `FileAttr.Blocks[*].Hash` across the metadata store.
- [ ] **GC-02**: Sweep phase: list remote `cas/XX/YY/*` prefixes (parallelizable), delete anything absent from live set.
- [ ] **GC-03**: Fail-closed on any error during mark phase — sweep skipped rather than risk deleting referenced blocks.
- [ ] **GC-04**: No `BackupHoldProvider` coupling; backup integration is the v0.16.0 plan's concern.

### Dedup (DEDUP)

- [ ] **DEDUP-01**: `FindFileBlockByHash` path preserved (renamed `GetByHash`) as µs-cost pre-PUT dedup check.
- [ ] **DEDUP-02**: Dedup scope is global per metadata store — `RefCount` spans shares when shares share a remote config.
- [ ] **DEDUP-03**: Cross-VM dedup ratio ≥40% on synthetic VM-fleet fixture (primary business outcome).

### Block state machine (STATE)

- [ ] **STATE-01**: Three states only: `Pending → Syncing → Remote` (GC-eligible when RefCount reaches 0). No `Dirty`, no `Local`.
- [ ] **STATE-02**: `State=Remote` only after successful upload + successful metadata txn (no orphan uploads).
- [ ] **STATE-03**: State lives only in `FileBlock` indexed by `ContentHash` — no parallel state in memory buffers or fd pools.

### Invariants (INV)

- [ ] **INV-01**: CAS immutability — bytes at `cas/.../h` always equal BLAKE3 `h` or are absent (GC'd).
- [ ] **INV-02**: Refcount matches references — `∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)` across the store.
- [ ] **INV-03**: Log prefix monotone — `consumed_pos` only advances; `CommitChunks` atomic.
- [ ] **INV-04**: GC fail-closed — mark errors abort sweep.
- [ ] **INV-05**: Log length bounded — `len(log) ≤ maxLogBytes`; writer blocks on pressure channel.
- [ ] **INV-06**: Hash verified on remote read — every chunk downloaded from S3 is BLAKE3-verified before caller receives it.

### Tech-debt cleanup (TD)

- [x] **TD-01**: `pkg/blockstore/readbuffer`, `sync`, `gc` sub-packages merged into `pkg/blockstore/engine/`.
- [x] **TD-02**: HIGH-severity bugs fixed: (a) `FSStore.Start()` goroutine joined on Close; (b) `syncFileBlock` errors propagate (no `_ =` swallowing); (c) `engine.Delete` calls `DeleteAllBlockFiles` (no `.blk` leak); (d) local tier stops calling FileBlockStore on write path.
- [x] **TD-03**: Dead scaffolding removed: `BackupHoldProvider`, `FinalizationCallback`, `ReadAtWithCOWSource`, `COWSourcePayloadID`, unused `FileAttr.Blocks []string`, unset `FileAttr.ObjectID`.
    - _Note (Phase 08, 2026-04-23): A3 (Phase 12, META-01) will reintroduce `FileAttr.Blocks` as `[]BlockRef`; A4 (Phase 13, META-02) will reintroduce `FileAttr.ObjectID` as BLAKE3 Merkle root. Both reintroductions use new types — this line is the single breadcrumb. `ContentHash` type at `pkg/blockstore/types.go` is retained (used by `FindFileBlockByHash`, backend `objects.go` indexes, and BSCAS-06/A2 groundwork)._
- [x] **TD-04**: Five block-key parsers collapsed to two canonical parsers (`ParseStoreKey` for `{payloadID}/block-{N}` + `ParseBlockID` for `{payloadID}/{blockIdx}`; `ParseCASKey` added in A2/Phase 11 per BSCAS-01). Net: 5 → 2.
- [ ] **TD-05**: `nonClosingRemote` shim removed; engine's `Close()` respects shared-remote ref-counting.
- [ ] **TD-06**: `SyncNow` spin-wait replaced with channel-based notification.
- [ ] **TD-07**: Per-download bridge-goroutine in `enqueueDownload` removed (queue worker signals directly).
- [ ] **TD-08**: `Syncer.Close` double-drain consolidated to one canonical drain path.
- [ ] **TD-09**: `flushBlock` does not hold `mb.mu` during disk write (stages bytes, releases lock, syncs).
- [ ] **TD-10**: Dual-read compatibility shim removed after migration complete.

### Adapter cleanup (ADAPT)

- [ ] **ADAPT-01**: New shared package `internal/adapter/common/` with `ResolveForRead`, `ResolveForWrite`, `readFromBlockStore` helpers used by both NFS and SMB.
- [ ] **ADAPT-02**: SMB READ handler routes response-buffer allocation through `internal/adapter/pool` (pool parity with NFS).
- [ ] **ADAPT-03**: Single consolidated `metadata.ExportError → NFS3ERR_* / STATUS_*` mapping table; both protocol adapters read it.
- [ ] **ADAPT-04**: Adapter layer fetches `FileAttr.Blocks` and passes `[]BlockRef` into engine (enables API-01).
- [ ] **ADAPT-05**: Cross-protocol conformance test — same file operation over NFS and SMB produces consistent client-observable error codes.

### Migration (MIG)

- [ ] **MIG-01**: `dfsctl blockstore migrate --share <name>` offline tool — reads legacy blocks, re-chunks via FastCDC, uploads as CAS chunks, updates `FileAttr.Blocks`, deletes legacy keys after verification.
- [ ] **MIG-02**: Migration is resumable via state file; supports `--dry-run`, `--parallel N`, `--bandwidth-limit MB/s`.
- [ ] **MIG-03**: Dual-read compatibility shim in engine reads legacy `{payloadID}/block-{idx}` keys until migration complete (A2–A5 window).
- [ ] **MIG-04**: Post-migration integrity check: every `FileAttr.Blocks[i]` points to a CAS key that exists in S3.

### Verification (VER)

- [ ] **VER-01**: Canonical E2E test `TestBlockStoreImmutableOverwrites` passes (currently fails on develop — the proof of correctness).
- [ ] **VER-02**: Benchmark regression gates met: random write ≥600 IOPS, random read ≥1350 IOPS, sequential write ≥48 MB/s, sequential read ≥60 MB/s.
- [ ] **VER-03**: VM-fleet dedup fixture: ≥40% storage reduction.
- [ ] **VER-04**: Crash-injection suite green: kill mid-upload, kill mid-metadata-txn, corrupt S3 object.
- [ ] **VER-05**: All three metadata backends (Memory, Badger, Postgres) pass the extended conformance suite with `[]BlockRef` round-trip.
- [ ] **VER-06**: Property-based tests for FastCDC boundary stability and BLAKE3 reproducibility across platforms.

---

## Future Requirements (Deferred)

- **v0.16.0**: Per-share atomic backup primitives (`BackupShare` / `RestoreShare` in metadata stores)
- **v0.16.0**: Manifest format recording BLAKE3 CAS hashes (no v0.13.0 compat burden since backup was never released)
- **v0.16.0**: Per-share restore via shadow share + cutover
- **v0.16.0**: `BackupHold` reintegrated as retention mechanism (not correctness — CAS handles that)
- **Later**: Eager `ObjectID` update (not lazy) if dedup hit rate demands it
- **Later**: Rabin CDC fallback if FastCDC shows worse dedup on certain workloads
- **Later**: Cross-bucket dedup via global hash registry (speculative; not driven by a concrete customer need)

---

## Out of Scope for v0.15.0

- **Per-share atomic backup** — deferred to v0.16.0 by explicit user decision. This refactor prepares the foundation (immutable keys) but does not rebuild the backup system.
- **Block-level compression** — separate milestone (BlockStore Security); not needed for CAS/dedup to work.
- **Block-level encryption** — separate milestone (BlockStore Security); AES-256-GCM already available at transport layer.
- **v0.13.0 backup backward compatibility** — v0.13.0 was never released; no external consumers to preserve.
- **Cross-site (cross-bucket) dedup** — requires a global hash registry, not driven by a concrete customer need.
- **Variable chunker parameters exposed as config** — start with fixed 1/4/16 MB; revisit if real workloads demand it.
- **pNFS / scale-out architecture** — unrelated to this refactor.

---

## Traceability (Requirement → Phase)

Populated by gsd-roadmapper on 2026-04-23. VER-01..VER-06 are phase-independent milestone gates (see ROADMAP.md "Milestone Gates" section).

| Requirement | Phase | GH issue |
|---|---|---|
| BSCAS-01 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| BSCAS-02 | Phase 10 (A1) | [#421](https://github.com/marmos91/dittofs/issues/421) |
| BSCAS-03 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| BSCAS-04 | Phase 13 (A4) | [#424](https://github.com/marmos91/dittofs/issues/424) |
| BSCAS-05 | Phase 13 (A4) | [#424](https://github.com/marmos91/dittofs/issues/424) |
| BSCAS-06 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| LSL-01 | Phase 10 (A1) | [#421](https://github.com/marmos91/dittofs/issues/421) |
| LSL-02 | Phase 10 (A1) | [#421](https://github.com/marmos91/dittofs/issues/421) |
| LSL-03 | Phase 10 (A1) | [#421](https://github.com/marmos91/dittofs/issues/421) |
| LSL-04 | Phase 10 (A1) | [#421](https://github.com/marmos91/dittofs/issues/421) |
| LSL-05 | Phase 10 (A1) | [#421](https://github.com/marmos91/dittofs/issues/421) |
| LSL-06 | Phase 10 (A1) | [#421](https://github.com/marmos91/dittofs/issues/421) |
| LSL-07 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| LSL-08 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| CACHE-01 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| CACHE-02 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| CACHE-03 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| CACHE-04 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| CACHE-05 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| CACHE-06 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| API-01 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| API-02 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| API-03 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| API-04 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| META-01 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| META-02 | Phase 13 (A4) | [#424](https://github.com/marmos91/dittofs/issues/424) |
| META-03 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| META-04 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| GC-01 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| GC-02 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| GC-03 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| GC-04 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| DEDUP-01 | Phase 13 (A4) | [#424](https://github.com/marmos91/dittofs/issues/424) |
| DEDUP-02 | Phase 13 (A4) | [#424](https://github.com/marmos91/dittofs/issues/424) |
| DEDUP-03 | Phase 13 (A4) | [#424](https://github.com/marmos91/dittofs/issues/424) |
| STATE-01 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| STATE-02 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| STATE-03 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| INV-01 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| INV-02 | Phase 12 (A3) | [#423](https://github.com/marmos91/dittofs/issues/423) |
| INV-03 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| INV-04 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| INV-05 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| INV-06 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| TD-01 | Phase 08 (A0) | [#420](https://github.com/marmos91/dittofs/issues/420) |
| TD-02 | Phase 08 (A0) | [#420](https://github.com/marmos91/dittofs/issues/420) |
| TD-03 | Phase 08 (A0) | [#420](https://github.com/marmos91/dittofs/issues/420) |
| TD-04 | Phase 08 (A0) | [#420](https://github.com/marmos91/dittofs/issues/420) |
| TD-05 | Phase 15 (A6) | [#426](https://github.com/marmos91/dittofs/issues/426) |
| TD-06 | Phase 15 (A6) | [#426](https://github.com/marmos91/dittofs/issues/426) |
| TD-07 | Phase 15 (A6) | [#426](https://github.com/marmos91/dittofs/issues/426) |
| TD-08 | Phase 15 (A6) | [#426](https://github.com/marmos91/dittofs/issues/426) |
| TD-09 | Phase 11 (A2) | [#422](https://github.com/marmos91/dittofs/issues/422) |
| TD-10 | Phase 15 (A6) | [#426](https://github.com/marmos91/dittofs/issues/426) |
| ADAPT-01 | Phase 09 (ADAPT) | [#427](https://github.com/marmos91/dittofs/issues/427) |
| ADAPT-02 | Phase 09 (ADAPT) | [#427](https://github.com/marmos91/dittofs/issues/427) |
| ADAPT-03 | Phase 09 (ADAPT) | [#427](https://github.com/marmos91/dittofs/issues/427) |
| ADAPT-04 | Phase 09 (ADAPT) | [#427](https://github.com/marmos91/dittofs/issues/427) |
| ADAPT-05 | Phase 09 (ADAPT) | [#427](https://github.com/marmos91/dittofs/issues/427) |
| MIG-01 | Phase 14 (A5) | [#425](https://github.com/marmos91/dittofs/issues/425) |
| MIG-02 | Phase 14 (A5) | [#425](https://github.com/marmos91/dittofs/issues/425) |
| MIG-03 | Phase 14 (A5) | [#425](https://github.com/marmos91/dittofs/issues/425) |
| MIG-04 | Phase 14 (A5) | [#425](https://github.com/marmos91/dittofs/issues/425) |
| VER-01 | Milestone gate | — |
| VER-02 | Milestone gate | — |
| VER-03 | Milestone gate | — |
| VER-04 | Milestone gate | — |
| VER-05 | Milestone gate | — |
| VER-06 | Milestone gate | — |

---

## Phases mapped to GH issues

| Phase | GH Issue | Label | Scope (requirement IDs) |
|---|---|---|---|
| A0 | [#420](https://github.com/marmos91/dittofs/issues/420) | tech-debt | TD-01, TD-02, TD-03 (partial), TD-04 |
| A1 | [#421](https://github.com/marmos91/dittofs/issues/421) | enhancement, performance | BSCAS-02, LSL-01, LSL-02, LSL-03, LSL-04, LSL-05, LSL-06 |
| A2 | [#422](https://github.com/marmos91/dittofs/issues/422) | enhancement, performance | BSCAS-01, BSCAS-03, BSCAS-06, GC-01, GC-02, GC-03, GC-04, STATE-01, STATE-02, STATE-03, LSL-07, LSL-08, TD-09, INV-01, INV-03, INV-04, INV-05, INV-06 |
| A3 | [#423](https://github.com/marmos91/dittofs/issues/423) | enhancement, api, nfs, smb | API-01, API-02, API-03, API-04, META-01, META-03, META-04, CACHE-01, CACHE-02, CACHE-03, CACHE-04, CACHE-05, CACHE-06, INV-02 |
| A4 | [#424](https://github.com/marmos91/dittofs/issues/424) | enhancement | BSCAS-04, BSCAS-05, META-02, DEDUP-01, DEDUP-02, DEDUP-03 |
| A5 | [#425](https://github.com/marmos91/dittofs/issues/425) | enhancement | MIG-01, MIG-02, MIG-03, MIG-04 |
| A6 | [#426](https://github.com/marmos91/dittofs/issues/426) | tech-debt | TD-05, TD-06, TD-07, TD-08, TD-10 |
| ADAPT | [#427](https://github.com/marmos91/dittofs/issues/427) | tech-debt, nfs, smb | ADAPT-01, ADAPT-02, ADAPT-03, ADAPT-04, ADAPT-05 |

**Dependency graph:**

```
A0 ──► A1 ──► A2 ──► A3 ──► A4 ──► A5 ──► A6
ADAPT ─────────────► A3  (consumes shared helpers + []BlockRef API)
```

A0 and ADAPT proceed in parallel as independent pre-A1 cleanup tracks. Verification requirements (VER-01 through VER-06) are phase-independent and gate the milestone overall.

**GSD phase number mapping:**

| Plan phase | GSD phase # | GH issue |
|---|---|---|
| A0 | Phase 08 | #420 |
| ADAPT | Phase 09 | #427 |
| A1 | Phase 10 | #421 |
| A2 | Phase 11 | #422 |
| A3 | Phase 12 | #423 |
| A4 | Phase 13 | #424 |
| A5 | Phase 14 | #425 |
| A6 | Phase 15 | #426 |

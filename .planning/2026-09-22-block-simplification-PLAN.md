# pkg/block — simplification plan

Status: **APPROVED, not started.** Baseline: develop @ `65bbda7c9` (numbers re-derived 2026-09-22).

Supersedes the draft that proposed a durable per-shard checkpoint. Three review passes
(correctness, adversarial ×2, ponytail) rejected it; §Q2 records why, because the reasoning is
load-bearing for anyone tempted to re-propose it.

Read `2026-09-01-block-dataflow-MASTER-PLAN.md` first — its steps 0-5 closed 2026-09-21, and this
plan is the two gaps that plan named for itself (bucket §3d, and `remote/s3`).

## Context

`pkg/block` is 25,858 production LOC / 34,273 test LOC across 16 packages (develop @ `65bbda7c9`). Three asks:
split the god objects (**~400 LOC target, 800 hard ceiling**), explain why the `.idx` files are
unused and **redesign the journal from what it should do**, and collapse repeated abstractions.

The 2026-09-01 master plan's steps 0-5 **closed on 2026-09-21**; `journal` already stands alone
(`go list -deps ./pkg/block/journal` returns only itself, pinned by `foreign_imports_test.go`).
That plan named its own gaps — bucket §3d (GC/manifest: *"do that work BEFORE the API boundary
hardens"*) and `remote/s3` (*"the only remaining audit worth running"*). This plan is those gaps
plus the three asks.

**Calibration.** Function-level dead code is negligible: `deadcode` finds 20 unreachable funcs in
~26k LOC; `staticcheck U1000` finds zero in journal. The waste is *structural* — god objects, eight
overlapping "where are the bytes" mechanisms, 18 tautological type assertions, three silently-inert
features, and a 1.3:1 test-to-source ratio.

**One concern, stated once.** The user chose a full journal rewrite. The 2026-09-01 audit — 59
findings — chose *"refactor, don't rewrite"*, and all five HIGHs were **residency-truth failures**
(one component's belief about a byte disagreeing with another's, resolving to zeros or reclaiming
live data). Ten such incidents in a year. The rewrite below is therefore staged so the on-disk
format changes **last**, behind a checkpoint that ships alongside the existing replay with an
equality assertion for one full release. Proceeding as directed.

---

## Q1 — God objects: split, but delete first

| Type | Methods | Fields | Files | Pure one-line forwards |
|---|---|---|---|---|
| `journal.Store` | **80** | **28** | 11 | 22 (28%) — symptom of 28 fields |
| `engine.RemoteSync` | **60** | **33** | 5 | 8 (13%) — a genuine god object |
| `engine.Store` | **59** | 14 | 8 | **14 (24%) — a façade** |

Over the 800 ceiling: `journal/store.go` (1557), `engine/syncer.go` (1276), `journal/reclaim.go`
(1042). Eighteen more sit in 400-800.

`engine.Store` is substantially a façade, so **delete its forwards and re-measure before splitting
it**. The other two are real.

**Free wins found while measuring:**
- `engine.Store.syncedHashStore` — written by `New`, **never read again**. Dead field.
- `blockstoretest/remoteblock.go` (479 LOC) imports `testing` into the **production build graph**.
- `remote/s3/store.go:223-420` — ~200 LOC of SSRF/endpoint validation inside a block store. Moving
  it to `endpoint.go` drops the file under 560 without touching the type.
- Test seams in production structs: `journal/store.go:259,260,265` and `journal/reclaim.go:944`
  `var testStopBeforeUnlink` — a **mutable package global in a production file**.

**Field-isolated extraction seams** (fields used by exactly one cluster — these cut cleanly):

| Extract | Fields | Today in |
|---|---|---|
| `coldLog` | `coldMu`, `coldFD`, `coldBroken` | `journal/cold.go` only |
| `reclaimer` | `gcMu` | `journal/reclaim.go` only |
| `readaheadTracker` | `readahead`, `readaheadN`, `readaheadPruning` | `engine/readahead.go` only |
| `inflightFetches` | `inFlight`, `inFlightMu` | `engine/fetch.go` only |

`RemoteSync.bs *Store` is a **back-pointer cycle to its own owner**, used only for
`dataplaneMetrics()` and cache invalidation. Cutting it is the prerequisite for every other move.

---

## Q2 — Why `.idx` is unused, and the journal redesign

### Why it is unused

**It was never finished.** `idxEntry` landed in the *first* journal commit (`e1e2615e3`, "journal
package skeleton"); four commits touched it and **none added a decoder**. Recovery has been
`tail-scan + rebuild` since `93eb90536`. All three opens are `O_WRONLY|O_CREATE|O_APPEND`
(`segment.go:177`, `recovery.go:256`, `recovery.go:467`).

It also *could not* have worked as designed: `idxEntry.FileIDHash` is `fnv1a(FileID)` — lossy — so
`map[FileID]*fileIndex` can't be rebuilt from it without reading the `.seg` for the FileID strings
anyway.

Meanwhile it costs 40 bytes per record appended, an extra fd per active segment, a `stat` per
segment at recovery, an **fsync** on rebuild, and `Remove` handling in three places. Its comments
(*"losing it only forces a re-scan"*, `"(recovery slower)"`) describe a fast path that does not
exist, misleading every reader.

### How the journal actually indexes

```
shardFor(FileID) = fnv1a(id) & shardMask   →  shard                 (RAM)
shard.index      map[FileID]*fileIndex     →  fileIndex             (RAM)
fileIndex.ivs    []interval, sorted, NEVER coalesced                (RAM, 64 B/write)
interval.loc     → shard.segment(SegmentID) → *os.File              (RAM)
```

The only **durable** form of that index is the `.seg` record stream. Consequences:

- `Open` is **O(every byte ever written and not yet GC'd)**. `scanValidRecords` calls
  `readRecordAt(..., nil)`; its comment states *"recovery keeps every payload it scans"*. Every
  payload gets a fresh allocation and a full CRC32, then `insert` discards the superseded ones.
- **The sealed bit does not skip the scan.** `recovery.go:48-53` claims sealed segments are "trusted
  via their header's sealed bit" and only the active one is tail-scanned. That distinction does not
  exist in the code — the sealed bit only skips the *truncate* step.
- The index is **O(number of writes)**, not file size, and nothing ever coalesces adjacent
  intervals. A 1 GiB file written in random 4 KiB writes holds ~262,144 intervals ≈ **16 MiB of RAM
  index per GiB**.

There are **eight** "where are the bytes" mechanisms and **three** durable channels
(`.seg` records, `cold.log`, `.idx`) that do not agree on what they cover.

### The target design — after two review rounds

The first draft proposed a durable checkpoint replacing `.seg` replay + `cold.log` + `.idx`.
**Two independent reviews killed it**, and the reasoning is the valuable part.

**Why the checkpoint died.** Repack copies survivors at their *original* Version
(`reclaim.go:765`, repoint at `:903-905`) and eviction writes cold markers at the interval's
original version (`:250`) — **neither mints a new Version**. So `replay(Version > watermark)` can
never repair a stale entry, and the window opens at `retireSegment` and stays open until the next
checkpoint. Every repack and every eviction sits in it. Independently: the checkpoint is
O(all intervals) where `cold.log` is O(evicted ranges) — **300-1000× larger**, and `cold.log`
doesn't exist at all until the first eviction — with write amplification ~**535× the `.idx` being
deleted for being write amplification**, and a net **+550 LOC**.

### The shape: separate residency from location

The journal's `interval` conflates two facts with different lifetimes:

- **residency** — is this range local, remote-only, or absent. *Range-shaped. Survives GC.*
- **location** — which segment, which offset. *Destroyed by repack and eviction.*

`cold.log` stores **only residency**, which is exactly why it is repack- and eviction-immune. It is
not a wart to fold away; **it is the correct shape, applied to one of the four states.**

And the durable side only needs entries for states with **no record left to read**. A record
self-describes (29-byte header: FileID, offset, length, Version, Flags), and `Dirty` vs `Resident`
is the `flagSynced` bit *in that header*. Only `Remote` — bytes evicted, record gone — needs a
side-log. **That is what `cold.log` already is.**

```
S3                    packed blocks, post-carve
─────────────────────────────────────────────────────────────────
JOURNAL DISK          segments — the bytes, in self-describing records
  (durable)           cold.log — ranges with NO record left  (= residency)
─────────────────────────────────────────────────────────────────
RAM                   location index — range -> {segID, segOff, recOff}
  (volatile)          rebuilt at Open; repack/evict mutate it in place
                      staging buffer — one pending run per file  (below)
```

**Recovery becomes:** read `cold.log` → scan segment **headers only** (validate `HeaderCRC32` over
`[0,24)`, skip the payload via `PayloadLen`) → rebuild the location index, and derive
`records`/`syncedRecords`/`liveBytes`/`minVersion` from the same walk. Body-CRC only the active
segment's tail, the one place a torn record can be — sealing fsyncs before setting the sealed bit,
so a sealed segment cannot have one (`recovery.go:213-215`).

Today that walk reads **every payload byte and retains it** (`record.go:282`, *"recovery keeps every
payload it scans"*) — 131 KB allocated per 128 KiB record. Header-only is 74 B read / 45 B
allocated. **This attacks the measured bottleneck** (`perf/.../README.md:109`: *"Recovery is
allocation-bound, not read-bound"*) **with no format change.** The counters can never be missing
because they are derived, not stored — so RB-3 cannot exist.

### The staging buffer: one pending run per file

Today one client write = one `appendRecord` = one record = **one interval, forever**. That is the
source of *"262,144 intervals ≈ 16 MiB of RAM index per GiB"*. Note that coalescing intervals
*after* framing is **inert** — record framing (`segment.go:356-368`) means two records are never
`loc.Offset`-contiguous. **Coalescing before framing is the version that works.**

```
WriteAt(f, off, data)
    contiguous with f's pending run?  -> extend it
    else                              -> flush pending, start a new one

FLUSH TRIGGERS: COMMIT / SMB FLUSH · any read of f · Truncate · Delete
                · DirtyExpiry timer · eviction pressure

INVARIANT: nothing but WriteAt ever sees the buffer. Every other path flushes first.
```

Protocol-legal because DittoFS already serves an 8-byte **server-boot write verifier**
(`v4/handlers/write.go:267`, `commit.go:151`): a client whose COMMIT verifier no longer matches
resends. **The honest cost:** today an UNSTABLE write survives a *process* crash (bytes are in the
page cache, recoverable from the segment); buffered, it survives nothing until flush. That is a real
reduction in blast radius, traded for the only mechanism that reduces interval count.

The one-pending-run limit is deliberate. A general write-back tier would make the buffer and the
index two structures that must agree about residency at all times — the exact class behind #1850,
#1888 and #2110. One run per file, flushed before anyone looks, is auditable in a sentence.

### Three live bugs to fix first, independent of any of this

1. **`appendTombstone` never calls `seg.records.Add(1)`.** A tombstone is invisible to the counter
   `evictable()` gates on, so a segment whose only content is a tombstone passes `syncedRecords ==
   records`, gets retired, and the delete is lost — **the file resurrects with its pre-overwrite
   content**. `reclaimEmptied` reaches it straight from `Delete` (`store.go:904`). Same class as
   #2231. **Filed as #2829.**
2. **`evictable()` (`reclaim.go:212-215`) has no `records > 0` guard** where its sibling
   `sealableActive` (`:118`) does. One line.
3. **`compactColdLog` runs only at Open** (`recovery.go:400-410`) — its own `ponytail:` says so.

## Q3 — Repeated abstractions: 8 collapses

| # | Collapse | Evidence |
|---|---|---|
| 1 | **`nonClosingRemote`'s ~90 lines of manual forwards** (`shares/blockstore_config.go:156-212`) | It **embeds** `remote.RemoteStore`, so Go already promotes all seven methods. Only `Durable()` is genuinely needed. |
| 2 | **Delete `Reader`/`Writer`/`Flusher`/`ComposedStore`** (`filechunk.go:174-280`) | 4 interfaces, 16 methods, ~110 LOC. Combined references outside their own definitions: **one**, a `var _`. Move `Flusher.Flush`'s godoc to `engine.Store.Flush`. |
| 3 | **Three MetaViews → one**, + `blockSyncedMarkerGC` → `SyncedHashIndex` | `CompactMetaView ⊇ ReclaimMetaView ⊇ ReconcileMetaView` — a real superset chain, so one interface serves all three. **CORRECTION: their assertions are NOT tautological.** `ReconcileMetaView` needs `EnumerateSynced`, which `metadata.Store` does not declare (`SyncedHashStore` has only `IsSynced`/`MarkSynced`/`GetLocator`/`DeleteSynced`). They do real narrowing — keep every one. −3 interfaces, −0 assertions. |
| 4 | **Four flush sinks → one** (`flush.go:54-107`) | Both production sinks implement all four; the file asserts it at `:168-176`. The optionality serves **test fakes only**. Removes the blind `c.sink.(ManifestRowEnder)` at `flush_closure.go:234` — a latent nil-panic. |
| 5 | **Delete 4 dead capability seams** | `local.MetricsAware` (0 impls → two Prometheus metrics permanently zero); `legacyArchiveMigrator` (0 impls → `legacy_verify.go:135-170` unreachable); `syncingEnumerator` (0 production impls — badger is prod); `blockcodec.Sealer` (0 impls; all 3 call sites pass `nil`). |
| 6 | **`walkLive(sh, fn)` helper** in `journal/reclaim.go` | **Six** copies of one shard-index walk (`:257, :469, :634, :764, :884, :950`) differing only in accumulator. Makes `dropVictim` a map lookup. Must **not** lock — 4 of 6 run inside a wider critical section. |
| 7 | **Merge `carver.Chunk` into `engine.CarveChunk`** | Same struct, one renamed field, one extra key, an incidental `int64`/`int` split on `Size`. |
| 8 | **Strip 24 always-true type assertions** (13 non-test) | All rooted in `remote.RemoteStore` embedding `RemoteBlockStore`/`ChunkReader`/`ChunkSealer` (`remote/remote.go:43-46`) — e.g. `encryption/decorator.go:61,108`, `compression/decorator.go:61,145`, `remote/passthrough.go:44`, `runtime/blockgc.go:482,553`. **None are MetaViews.** |

**Deliberately not collapsed:** `local.LocalStore`'s 35 methods (2 full implementors, a `ponytail:`
marker owns the decision); `FileChunkStore` vs `EngineFileChunkStore` (strict superset, both have a
real consumer in `pkg/metadata/store.go`); journal-GC vs engine-compaction ratio gates (2 lines of
overlap, five axes of genuine difference); `ChunkRef` vs `Row` (transient projection vs persisted
row — collapsing them would put `RefCount` and `LastAccess` in every file's attribute blob).

---

## Target structure

### The root: evict, don't subdivide

`pkg/block` is a **pure leaf** — it imports nothing internal — and it is the repo's shared
vocabulary: `ContentHash` 613 external uses, `ChunkRef` 305, `FileChunk` 222, `ChunkLocator` 186,
`BlockRecord` 115. Subdividing *all* of that costs ~1,440 call-site edits and buys nothing, since
the pieces reference each other and would import each other right back.

So the manifest model gets its own package, everything that is not vocabulary leaves, and what
remains stays flat. **Nothing in the root is named "store" any more** — a store is the remote store.

| Leaves the root | Goes to | Why | Sites |
|---|---|---|---|
| `ChunkRef`, `FileChunk`→`Row`, row-ID grammar, **`ComputeObjectID`** | `pkg/block/manifest` | The manifest model, its own package, importing only `pkg/block`. **The check/repair tooling cannot follow** — it uses `metadata.File`, `FileHandle`, `EncodeFileHandle`, `FileTypeRegular`, `NamesOnly`, `IsNotFoundError`: concrete types, not narrowable to a local interface. It goes to `gc/`. | ~560 |
| `FileChunkStore`, `EngineFileChunkStore` | `pkg/metadata` | Implemented by `metadata/store/{sql,badger,memory}`, embedded by `metadata.Store`/`Transaction`, consumed by `engine` — which already imports `metadata` in 9 files. **This is what made `stores.go` misleading: it was never a block concern.** | ~40 |
| `holemap.go` | `pkg/metadata/sparse.go` | Pure math over `[]ChunkRef`; consumers are NFSv4 `seek.go`/`read_plus.go` and `metadata/sparse.go`, which already exists | ~12 |
| `defaults.go` | `pkg/controlplane/config` | Host RAM/CPU sizing read once by `cmd/dfs/commands/start.go` | ~8 |
| `retention.go` | `pkg/controlplane/models` | A share-config enum; the block layer collapses it to one bool | ~14 |
| `Meta`, `DurabilityReporter`, `IsDurable` | `pkg/block/remote` | They describe a *remote object*; all 6 implementors are remote stores | ~57 |

**Two things the cycle check moved.** `ComputeObjectID` takes `[]ChunkRef`, so leaving it in root
would make root import `manifest`; it goes to `manifest/`, where a Merkle root over the manifest
belongs anyway. The `ObjectID` *type* is an alias of `ContentHash` and stays in root.

**`BlockState` stays in root.** `BlockRecord.SyncState` is a `BlockState` and `BlockRecord` stays in
`locator.go`; moving the enum into `manifest/` makes root import `manifest` while `manifest` imports
root for `ContentHash` — a cycle. It is genuinely shared vocabulary.

### The tree

Every file ≤ 800, target ~400. `(N)` = approximate LOC after. **NEW** · **MOVED** · ~~deleted~~.

```
pkg/block/                       the vocabulary. A LEAF — imports nothing internal.
├── README.md                    pinned by a foreign_imports_test.go, like journal's
├── hash.go               (200)  ContentHash, HashSize, ParseContentHash, CASKey,
│                                ObjectID (the alias), HashSet
│                                NOT ComputeObjectID — it takes []ChunkRef, so it would
│                                drag root into importing manifest. It goes to manifest/.
├── state.go              (45)   BlockState — shared by manifest.Row AND BlockRecord
├── locator.go            (150)  ChunkLocator, BlockRecord, BlockChunkCommit, FormatBlockKey
├── errors.go             (105)  13 live sentinels (was 26)
│   ~~stores.go~~                → pkg/metadata          ~~holemap.go~~ → metadata/sparse.go
│   ~~defaults.go~~              → controlplane/config   ~~retention.go~~ → controlplane/models
│   ~~blockstore.go~~            Meta/DurabilityReporter/IsDurable → remote/ ; 33 lines of
│                                orphan godoc for a deleted `Store` deleted
│   ~~doc.go~~                   → README.md
│
├── manifest/                    the manifest MODEL. Imports only pkg/block.
│   ├── README.md                what a row is; the {payloadID}/{fileOffset} key and its
│   │                            deliberately non-unique hash index
│   └── manifest.go       (320)  ChunkRef, Row, PruneChunkRefsToSize, row-ID grammar,
│                                ComputeObjectID (a Merkle root over the manifest — it
│                                takes []ChunkRef, so it belongs here, not in root)
│
├── carver/
│   ├── README.md                the two-cursor read/cut invariant; "the carver knows nothing
│   │                            about the journal or the engine"
│   ├── carver.go         (390)  the carve loop ← flush_closure.go:102-147, :282-293
│   ├── batch.go          (180)  arena + emit (today's carver.go)
│   ├── carver_bench_test.go     NEW — BenchmarkCarver_Box, novel-heavy + dedup-heavy
│   └── chunker/
│       ├── README.md            FastCDC + THE warning: changing one gear-table entry
│       │                        re-chunks every byte ever written and resets dedup
│       ├── chunker.go    (95)   boundary search — stdlib-only
│       ├── gear.go       (20)
│       └── params.go     (40)
│
├── middleware/                  NEW — the generic interface + its two implementations
│   ├── README.md                the Layer contract; why Compose takes two NAMED slots
│   ├── middleware.go     (110)  Layer, decorator, Compose(inner, compress, encrypt)
│   ├── compression/             codec.go (95) · frame.go (110) · policy.go (90) · errors.go (20)
│   └── encryption/              layer.go (150) · frame.go (180) · policy.go (100) · errors.go (30)
│       └── keyprovider/         provider.go (60) · local.go (345) · kmip.go (335) — KEEP
│
├── journal/                     the rewrite. ONE durable index.
│   ├── README.md                the model; the checkpoint contract; the demote ordering rule;
│   │                            the leaf rule pinned by foreign_imports_test.go
│   ├── config.go         (176)  ├── checkpoint.go   (320)  NEW — the durable index
│   ├── store.go          (330)  ├── index.go        (330)
│   ├── background.go     (80)   ├── plan.go         (150)  read planning + coalescing
│   ├── write.go          (240)  ├── segment.go      (560)  −idxFD lifecycle
│   ├── namespace.go      (345)  ├── record.go       (290)  unchanged
│   ├── coldseed.go       (134)  ├── recovery.go     (350)  checkpoint + replay above wm
│   ├── shard.go          (200)  −hydrateFence, −deleteFences, −evictedFenceFloor
│   ├── state.go          (120)  demote stays THE residency-loss chokepoint
│   ├── flush.go          (440)  the content-agnostic seam — untouched
│   ├── evict.go (339) · space.go (191) · gc.go (229) · repack.go (278)   ← reclaim.go (1037)
│   ├── format.go (131) · statfs_{unix,windows}.go (36)
│       ~~cold.go~~ ~~coldstats.go~~  a cold interval is just state=Remote in the checkpoint
│       ~~admin.go~~                  5 forwards + 2 assertions proving nothing
│       (no restore.go — only restore_test.go; RestoreToVersion is in store.go, and 4C
│        splits it IN PLACE. #1454 would rewrite the same function.)
│       ~~*_bench_test.go~~ ×4        786 LOC that never execute
│
├── engine/                      composition root + the public API ~15 packages depend on
│   ├── README.md · engine.go (380) · readwrite.go (450) · copy_payload.go (290)
│   ├── read_internal.go (300) · selfheal.go (120) · fetch.go (400) · fetch_resolve.go (280)
│   ├── syncer.go (320) · sync_gating.go (157) · sync_metrics.go (113) · sync_fileops.go (228)
│   │     · sync_lifecycle.go (236) · sync_carve.go (187)        ← syncer.go (1249)
│   ├── readahead.go (180) · flush.go (400) · flush_closure.go (250) · upload_chain.go (180)
│   ├── snapshot_admin.go (180) ← flush.go:577-748 (restore is not flushing)
│   ├── cache.go (300) · cache_prefetch.go (150) · coordinator.go (160) · offline.go (383)
│   ├── health.go (105) · stats.go (300) · metrics.go (120) · dataextents.go (120)
│   └── syncer_config.go (174) ← types.go (holds only sync types)
│       ~~range.go~~                  findBlocksForRange: every caller is a test
│       ~~sync_queue.go~~ half        the download path is never fed
│
├── gc/                          NEW ← engine. Absorbs the manifest TOOLING (not its types).
│   ├── README.md                every pass here runs offline, never on the hot path; the
│   │                            live set MUST span every lifecycle state (#2265 reaped
│   │                            actives by scanning only the sealed set)
│   ├── gc.go (400) · gc_rootlock.go (120) · gc_errors.go (140) · gc_block.go (415)
│   ├── sweep_index.go (380) · sweepguard.go (196) ← dedup_sweep_guard.go
│   ├── gcstate.go (250)  badger-backed live-hash set (GC-specific)
│   ├── lastrun.go (90)   NEW — ONE persist-last-run. Replaces TWO copies:
│   │                     gcstate.go:302 → last-run.json, audit_state.go:256 → last-inv02.json
│   ├── orphan_reclaim.go (295) ← engine/reclaim.go (collided with journal/reclaim.go)
│   ├── compaction.go (353) · check.go (330) · repair.go (500) · ranges.go (80)
│   └── audit.go (200)    refcount audit, minus its own persist-last-run
│
├── remote/                      THE store. The only thing called a store.
│   ├── README.md · remote.go (150) · meta.go (80) ← block/blockstore.go
│   ├── blockframe.go     (250)  ← blockcodec, FOLDED IN as one file. It is the layout of a
│   │                            `blocks/<id>` object — remote's own concern. −the sealed path
│   ├── passthrough.go    (110)  −SliceRange, −5 unreachable blockInner branches
│   ├── memory/store.go   (290)
│   └── s3/store.go (555) · endpoint.go (200) ← store.go:223-420 (SSRF guard, not a store)
│       ~~verifier.go~~          dead; blockstore.go:23 advertised a ReadBlockVerified
│                                that never existed
│
├── uploadwindow/                ← pkg/block/syncer (which contains no syncer)
│   └── README.md · dynsem.go (140) · controller.go (170) + the adaptive window from
│                                engine/syncer.go:828-997
│
└── blockstoretest/remoteblock.go (479)  MOVED out of the production build graph
```

---

## Wave plan

Wave 1 needs no prerequisites — four concurrent PRs, today.

### Precondition — CLEARED 2026-09-22

The three worktrees that held unpushed commits in the files this plan rewrites all landed:
`block-legacy` = #2777, `block-api` = #2779, `block-upload` = #2780, all merged 2026-09-21;
worktrees and branches removed 2026-09-22. There are **zero open PRs**.

The rule that produced this check still stands for any branch opened against these files while the
plan runs: **land it or abandon it before Wave 2.** A rename or a file move turns an unmerged branch
into a modify/delete conflict, which git cannot help with — precisely how `fix/2423-one-put-window`
died (8 commits, never pushed, then unrebaseable; the first commit alone conflicted across 13 paths,
7 of them modify/delete against files develop had deleted).

### Open issues this plan touches

All 24 open issues read against this plan on 2026-09-22. Six bear on it. The rest are SMB (#2775,
#2631, #2625, #2602, #2591, #2511), NFS (#2437, #2407, #2402, #2401, #2258), or unrelated to the
block layer (#2263, #1603, #1418, #1417, #1290, #1199, #117).

| Issue | Bears on | Disposition |
|---|---|---|
| **#2829** — tombstone uncounted in `seg.records` | 4A | **Fixed by this plan.** It *is* 4A item 1, filed from it. |
| **#2817** — group commit amortises only when writers share a shard | 4B, 4E | **Sequence it, do not fold it in.** Its subject is `shard.groupCommit` (`store.go:753`), which 4B splits out of `store.go`, and 4E changes how many records reach it. Land the measurement it asks for **before** 4B, or re-derive it after — not concurrently. Note this plan deliberately dropped a shard-count retune; #2817's ask is the measurement that would justify one, which is a different thing. |
| **#2822** — cold-barrier worst-case pinning bound unmeasured | 1A, 4F | **Take the baseline before 4F.** Its mechanism is exactly `evictable()`'s "keep any segment holding a record not yet on the remote" — 1A adds the `records > 0` guard to that predicate and 4F rewrites reclaim around a reverse index. Measure residue-vs-segments-actually-pinned against today's code or the baseline is gone. |
| **#2353** — bulk pre-warm of a subtree | 1B | **A decision, not a blocker.** 1B deletes `EnqueueDownload`/`TransferDownload` because nothing feeds them; a queued bulk pre-warm is the one plausible future consumer. Delete them anyway — re-adding a queue arm is cheap, carrying a dead one is not. **Say so in the 1B PR** so it is a decision and not an accident. |
| **#1489** — remote manifest files for namespace DR | 3E | **Placement note only, nothing to build.** When it is built, the writer belongs in the new `pkg/block/manifest` beside `ComputeObjectID`; the check/repair *tooling* stays in `gc/`. That split is the reason to get 3E's boundary right. |
| **#1454** — mount snapshots on a separate read-only share | 4C | **Collision warning.** It changes restore semantics in `RestoreToVersion` (`store.go:1198`) and `runtime/snapshot.go` — the same function 4C splits in place. Whichever lands second pays the rebase; neither should start blind. |

Nothing above changes a wave's scope except #2829, which was already in it.

### Wave 0 — prerequisites
- **0.1** Cut `RemoteSync.bs *Store` (`engine.go:202`, `syncer.go:72,466-469`) → `MetricsSink`.
- **0.2** Interface stubs + placeholder fields on `engine.Store`, still backed by in-package types —
  absorbs the shared-file conflict before Wave 2 forks off `engine.go`.
- **0.3** Fix **CLAUDE.md invariant 7**: `BlockStoreConformance` does not exist; only
  `RemoteBlockStoreConformance` does.

### Wave 1 — pure deletion, 4 parallel tracks, ~2,900 LOC, zero behaviour change
- **1A journal** — `.idx` entirely, **keeping its `os.Remove` calls or adding an Open-time sweep**
  (`diskBytes` never counted `.idx`, so orphans are invisible to `MaxLocalBytes`); **4 of the 5**
  benchmark files (858 LOC total) — **NOT `wave7_group_commit_bench_test.go`, which landed at HEAD
  (`65bbda7c9`, #2818) and is live perf work**; 2 redundant `var _` assertions (`admin.go:29,42`);
  `evict`'s `allowActiveSeal` + its speculative doc (`reclaim.go:66-68`), keeping `sealSyncedActives`
  (second live caller at `:483`). Plus the one-line `records > 0` guard on `evictable()` — **take #2822's
  residue-vs-segments-pinned baseline before this lands**, since it measures that predicate.
- **1B engine** — `range.go` + `range_test.go` + `TestPerfGate_Phase12_BinarySearchOverhead` (a
  **`Test`**, not a Benchmark, burning 1M iterations gating a function nothing calls);
  `ErrPersistFileChunksNotWired`; `SetRemoteStore`; the dead `syncedHashStore` field.
  **`SyncQueue`: delete only the download ARMS**, not the file — `EnqueueDownload` (`:128`),
  `TransferDownload`, `pendingDownload` and the `downloads` channel cases. The **prefetch half is
  live in production** (`readahead.go:144` → `processDownload` → `fetchBlock`); deleting
  `sync_queue.go` deletes readahead. **#2353 asks for bulk subtree pre-warm** — the one plausible
  future consumer of the download arms. Delete them anyway and record the decision in the PR. Note these are exported from a `pkg/` package.
  Deleting `range.go` also deletes a CI perf gate — say so in the PR.
- **1C stores** — `remote/s3/verifier.go` + test. **It is the abandoned half of a deliberate
  decision, not an accident:** `s3/store.go:458-464`'s `ReadChunk` takes the expected hash as `_` and
  documents *"no verification here (the engine verifies the BLAKE3 after the decorator stack)"*, and
  `:178-179` disables SDK flexible checksums for the same reason. The live path is
  `engine/fetch.go:336-350`. Fix `blockstore.go:23` and the **four doc pages** in the same change: `implementing-stores.md:596,725,740`,
  `architecture.md:1592` all document a `ReadBlockVerified` that does not exist. The real path is
  `engine/fetch.go:336-350`); `remote.SliceRange`; 5 unreachable `blockInner()` branches; the
  redundant conformance duplicates across `remote/memory` and `s3`.
  **Also delete `pkg/block/local` and `pkg/block/local/memory`** — a 43-line interface-only package
  with one production implementor, and a 501-line test-only double whose `Flush` ignores `opts`
  entirely. Fold `LocalStore` into `journal`. (This dropped out of an earlier draft.)
- **1D root + dead capabilities** — `doc.go`; collapses #2 and #5; 11 zero-caller sentinels;
  `MergeChunkRefsByOffset`; `ParseBlockID`; `HashSet.Hashes`.

  **1D carries a decision.** `MetricsAware` is a *silently broken feature*: delete or implement —
  **never leave a no-op**. Recommend **implement** `SetMetrics` on `*journal.Store`, since two
  Prometheus metrics have been permanently zero.
  **`EvictLocal` is already gone** — deleted by `8243e9a6b` (#2821) mid-session. Nothing to do.

### Wave 2 — extraction + collapses (after Wave 0), 3 parallel tracks
- **2A** `pkg/block/gc` ← the GC cluster. **The file list in the tree was wrong and must be
  re-derived:** `gc_rootlock.go`, `gc_errors.go` and `ranges.go` **do not exist** (the root lock is
  `gc.go:76-128`, error classification `gc.go:595-689`), and `range.go` holds only
  `findBlocksForRange`, which Wave 1B **deletes** — the plan both moved and deleted it.
  **This move creates a cycle in BOTH directions, and `*Store` coupling is not the blocker — there
  are zero `func (bs *Store)` methods in the moving set:**
  - `engine → gc`: `BlockSize` is declared in **`gc.go:52`** and used at 17 sites in
    `fetch.go`/`syncer.go`/`warm.go`/`types.go`; `flush.go:149` calls `dedupGuard.adopt`;
    `reconcile.go:137` calls `resolveGracePeriod`/`Options` from `gc.go`.
  - `gc → engine`: `ReconcileMetaView`, `ReconcileClass`, `defaultReconcileSampleCap`
    (`reconcile.go`), `coalesceExtents` (`dataextents.go:131`), `newBlockID` (`carve_dispatch.go:163`).
  **Wave 0 must move `BlockSize` out of `gc.go` first.** `reconcile.go` and `carve_dispatch.go` must
  be placed explicitly — they appear nowhere in the tree today. `dedupGuard` is a deliberately
  package-private rendezvous between the write and sweep paths (`dedup_sweep_guard.go:92-94`);
  moving it means **exporting** it. Also note 2A is a **public-API change across four more trees** —
  `cmd/dfsctl`, `internal/controlplane/api/handlers`, `pkg/config`, `pkg/controlplane/runtime` all
  consume `engine.BlockSize`, `engine.GCStats`, `engine.ManifestCheckResult` and friends.
- **2B** Same package ← `manifest_check.go`, `manifest_repair.go`, `audit_state.go`. Move
  `repairPayload` (`manifest_check.go:374`) into `repair.go`; collapse the two persist-last-run
  implementations into `lastrun.go`. Collapses #3. *Land 2A first.*
- **2C** `pkg/block/middleware/` per the tree. Collapses #1, #8.

### Wave 3 — god-object splits (after 1 and 2)
- **3A** `carver` absorbs the carve loop; `chunker` nests under it; add `BenchmarkCarver_Box`.
- **3B** `engine/syncer.go` 1249 → six files; extract `readaheadTracker`, `inflightFetches`.
- **3C** `journal/reclaim.go` 1037 → four files. Collapses #6.
- **3D** `engine.Store` — **delete the 14 forwards, re-measure, then decide** whether to split.
- **3E** Renames + the `pkg/block/manifest` extraction. **MUST BE LAST**, after 2A/2B have moved
  files — 3E touches `syncer.go`, `fetch.go`, `flush.go`, `engine.go`, `readwrite.go`, i.e. every
  file 3B and 3D split, so 3A/3B/3D/3E are **not** parallel. Three corrections to the recipe:
  - **It is ~100 rename decisions, not one.** Only 222 of 606 `FileChunk` lines are the type; the
    rest are distinct identifiers gopls will not touch (`ListFileChunks` ×215,
    `EnumerateFileChunks` ×126, `FileChunkStore` ×119, `ErrFileChunkNotFound` ×80 …).
  - **The alias trap.** `pkg/metadata/object.go:55` is `type FileChunk = block.FileChunk`. gopls
    rewrites the RHS only, so **81 `metadata.FileChunk` sites compile untouched** and you ship a
    half-renamed tree that is green. Rename the alias explicitly.
  - **Run gopls three times** — default, `-tags=integration`, `-tags=e2e`. Four files are hidden
    from the default context, including five `TestPostgres_FileChunk*` tests.
  - ~150 string literals go stale, including `cmd/dfsctl/.../audit.go`, which feeds **generated**
    `docs/guide/cli.md` with no CI drift gate.
  Production-only churn is ~144 `ChunkRef` + ~101 `FileChunk` sites; the earlier "~560"/"~1,440"
  figures were with-tests totals.

### Wave 4 — the journal (after 1A and 3C). No segment format change.

- **4A — the three live bugs, as standalone PRs, first.** Tombstones uncounted in `seg.records`
  (resurrection — **#2829**); the `records > 0` guard on `evictable()`; `compactColdLog`'s
  recovery-only gate. The latter two have no issue yet — file them or fix them, do not leave them
  living only in this document. These stand on their own and should not wait for the restructuring.
- **4B** Split `journal/store.go` 1557 → six files — **`shard.groupCommit` moves here; see #2817
  before or after, never during**; extract `coldLog` (`coldMu`/`coldFD`/
  `coldBroken`) and `reclaimer` (`gcMu`) on their field-isolated seams; delete the 4 test seams from
  the production structs. **Extraction, not slicing.**
- **4C** `RestoreToVersion` — **split in place, do not move.** It walks `s.shards` under `sh.mu`
  (`:1221-1231`), calls `sh.segment` + `readVerifiedRecord` (`:1401-1410`), reads `loadCold`
  (`:1312`), mutates via `s.Delete`/`s.WriteAt`/`s.SeedColdBatch`, and inspects `sh.syncFailed`
  (`:1477-1481`). Moving it means exporting shard locks — undoing what `foreign_imports_test.go`
  pins. Split into `replayToVersion` + `materializeVView` + a ~40-line orchestrator.
  **It is a second, independent reader of `cold.log`** — any change to that file touches it.
- **4D — header-only recovery scan.** Validate `HeaderCRC32`, skip the payload via `PayloadLen`,
  derive the segment counters from the same walk. Full body-CRC stays for the active segment's tail.
  **No format change**, so it is independently revertible. This is the item that attacks the
  measured allocation cost.
- **4E — the staging buffer.** One pending contiguous run per file; flush on non-contiguous write,
  COMMIT/FLUSH, any read of that file, Truncate, Delete, `DirtyExpiry`, eviction pressure.
  Ship the flush-on-everything version first and only then consider relaxing a trigger.
- **4F — unify reclaim.** #2822's baseline must exist before this starts. One entry point, two strategies (drop a fully-synced sealed segment;
  repack a high-dead-ratio one), behind the **segment→intervals reverse index** that
  `reclaim.go:305-307` already asks for — which also collapses the six duplicate index walks
  (`:262, :474, :639, :769, :889, :955`) into one lock-held helper.
- ~~Per-shard checkpoint~~, ~~interval coalescing~~, ~~fence-map collapse~~, ~~format bump~~,
  ~~shard-count retune~~ — **all dropped**, see the design section.

### Wave 5 — tests (~3,000 LOC, independent)
- **Kill `newTestEngine`** (`engine_test.go:129`) — highest value per line here. It builds
  `memory.New()` + nil remote + nil coordinator + a stub whose refcount methods all `return nil`,
  carve **off**; production builds an engine at one place with all of them real. 26 call sites.
  **While it exists, a class of tests here cannot fail for a production reason.**
- Move `blockstoretest/remoteblock.go` out of the production build graph.
- Collapse: 5 `cold_read_*` files → one table (666→~200); `gc_test.go`'s over-reap family (312→~80);
  13 `TestTransition_*` → one table; 11 strict-subset journal tests; `copy_refcount_test.go` (two
  tests asserting production code is *unreachable*); `codec_test.go`'s three round-trip clones → one
  golden-bytes test.

---

## Verification

**Per PR:** `go build ./... && go vet ./... && go test -race ./pkg/block/...`, plus
`go vet -tags=integration` and `-tags=e2e`. `golangci-lint run` — `go vet` is not the real gate.

**Wave 3E / 4 additionally:** `go test ./pkg/metadata/storetest/...` (the conformance suite is the
contract) and `cd test/e2e && sudo ./run-e2e.sh`.

**Correction to an earlier draft, which prescribed a technique its own rig documents as
insufficient.** `test/crash/device-loss.sh:7-9` states verbatim: *"`kill -9` does not reproduce
this: the page cache outlives process death."* And both rigs write **one file**, so one FileID
hashes to one shard — a single-file rig **structurally cannot see a cross-shard ordering bug**. The
journal's own recovery tests are all close-and-reopen, which flushes everything. Any future work on
residency ordering needs `dm-flakey drop_writes`, N files across ≥2 shards, and a concurrent delete
in the window.

**Danger zones — test at the consumer, not the producer.** Every HIGH in this component's history
was a residency-truth failure, and #2265's own suite missed it by building only sealed fixtures.

| Zone | Consumer-level test |
|---|---|
| Checkpoint ↔ replay (4D/4E) | Kill the process mid-write; assert **no zero-filled range** after restart. Not a unit test asserting the checkpoint was written. |
| `demote` ordering (4E) | Force eviction with the checkpoint fsync failing; assert local bytes are **still there** and the read succeeds. |
| GC mark-sweep (2A) | Chunks in *every* lifecycle state simultaneously — pending / active-unsealed / sealed / synced — one sweep, assert all four survive. |
| RefCount / dedup reap (2A, 5) | `storetest`'s invariant `∑ Row.RefCount == ∑ len(FileAttr.Blocks)` after concurrent copy+delete. |
| Compress-before-encrypt (2C) | **Has no test today.** Enforced only by the order of `blockstore_config.go:703,710` — swap them and nothing fails; you just compress ciphertext at ratio ~1.0 forever. |
| Staging buffer (4E) | For **each** flush trigger, assert the buffered bytes are visible: write 4 KiB unaligned, then read it back **without** a COMMIT — it must return the written bytes, not a hole. Repeat for `DataExtents`/SEEK, `Truncate` mid-run, `Delete`, and a carve pass. A test that only checks COMMIT passes against a buffer that is invisible to reads. |
| Buffer + crash (4E) | Kill mid-burst and assert the write verifier **changed**, so a client knows to resend. A buffered write lost silently with an unchanged verifier is a protocol violation, not just lost data. |
| Tombstone eviction (4A) | Overwrite a file, delete it, force eviction of the segment holding the tombstone, restart — assert the file stays deleted. This is the live bug; write it first and watch it fail. |
| Eviction-health gate (0.1) | Remote down mid-write; assert local bytes are never evicted before mirroring. The gate is an AND of healthy **and** carve-wired (`engine.go:216-241`). |

**Verify every regression test by reverting the fix and watching it fail on the assertion.** A build
error from an unused import is not proof.

---

## Outcome

| | Now | After |
|---|---|---|
| Production LOC | 25,858 | ~21,800 |
| Test LOC | 34,273 | ~31,500 |
| Files over 800 LOC | 3 | 0 |
| Root package files | 13 | 4 + README |
| `journal.Store` methods / fields | 80 / 28 | ~45 / ~18 |
| Durable structures on journal disk | 3 (`.seg`, `cold.log`, `.idx`) | 2 (`.seg`, `cold.log`) |
| Recovery allocation per 128 KiB record | ~131 KB | ~45 B (header-only) |
| Intervals per N small sequential writes | N | 1 (staging buffer) |
| Segment on-disk format | — | **unchanged** |
| Packages with exactly one consumer | 3 | 0 |
| Silently-inert features | 3 | 0 |
| Always-true type assertions | 24 | 0 |

No format change and no migration anywhere in this plan. Every journal item is independently
revertible, which is the property the checkpoint could not offer.

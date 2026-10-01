# Decision record: where residency truth lives

**Status:** accepted, superseded as a specification by `docs/internals/rfc-0-data-lifecycle.md` and the RFC set it indexes. Retained as the decision record: the production incident, the measurements, and the reasoning that changed our minds. The RFCs state what the system IS; this states why.

**Provenance:** a production incident on 2026-09-21/22 (SMB, multi-hundred-GB
transfers), the 196-finding audit set behind `rfc-block-dataflow.md`, and a
line-by-line review of the current `pkg/block/journal` and
`pkg/metadata/store/badger`.

This is not a specification — read `rfc-0-data-lifecycle.md` for that. It argues that one structural choice —
*the journal is the only thing that knows what data exists* — produced ten field
incidents and one production outage, and that the fix is to stop asking it.

---

## 1. What happened in production

A field deployment moved large files over SMB into a share backed by S3. Three
symptoms were reported: the metadata store grew without bound, syncing to S3
stopped, and the journal "did not look well shaped". They are one defect.

Measured, from the operator's log and a dump of the metadata directory:

| Signal | Value |
| --- | --- |
| journal `disk_bytes` | 171,823,195,100 (160.0 GiB) |
| journal `max_local_bytes` | 32,908,387,942 (30.6 GiB) |
| ratio | **5.2× over its own cap** |
| journal `unsynced_bytes` | 165,742,510,080 (154.4 GiB) — **96% of everything on disk** |
| metadata `.vlog` files | 244 files, **245.01 GiB** |
| metadata `.sst` files | 7 files, **0.15 GiB** |
| vlog:sst ratio | **1633:1** |
| chunk hashes known to GC | 22 → 195,414 over the run |
| remote objects ever seen by GC | `objects_scanned=11`, `bytes_freed=0`, on all 101 runs |

The sequence:

```
14:17:07  flush dispatcher: file flush failed  file=export/bd8aeedc-…
          error=Conflict: badger WithTransaction: transaction conflict
          … ×16 on the SAME file, retrying every 2s, backing off to ~90s
16:50:37  flush dispatcher: file flush failed … error=DB Closed   ×10
16:59:03  journal local store full: nothing evictable, backpressuring writes
17:00:03  WRITE: content write failed path=fio.exe
          error=journal: local store full, all segments pinned by unsynced bytes
```

From 16:59:03 to 17:02:16, `disk_bytes` and `unsynced_bytes` are **byte-identical
in every log line**. The store was not slow. It was frozen, and nothing in the
design could unfreeze it short of deleting the journal by hand.

### 1.1 Why it froze

1. Flush could not commit its metadata transaction.
2. So `synced` was never set on those records.
3. So `evictable()` (`reclaim.go:102`), which requires
   `syncedRecords.Load() == records.Load()`, was false for **every** segment.
4. So `unsynced_bytes` could never decrease.
5. So every write failed, forever.

There is no fallback branch. Local reclaim is gated on remote durability, remote
durability is reported through a metadata transaction, and when that transaction
cannot commit the system has no defined behaviour other than to stop.

### 1.2 Two things that look like the cause and are not

- **`parallel_syncs=0`** in the startup line is the documented sentinel for
  adaptive auto-tuning (`AdaptiveUploadDefault`), not an empty worker pool.
- **Badger value-log GC never ran.** It did: `runValueLogGC` ticks every 5
  minutes and drains every rewritable file (`badger/store.go:467`). The 245
  GiB is not uncollected garbage, it is write volume outrunning reclamation.

## 2. The actual defect: a file's chunk list is one key

`pkg/metadata/store/badger/encoding.go`:

```
File Manifest  "fm:"  fm:<uuid>  →  []block.ChunkRef (JSON)
```

One key per file, holding the **entire** chunk list as a single JSON value.
`putManifest` (`badger/files.go:124`) rewrites all of it on every `SetManifest`
— every carve, truncate and punch.

Three consequences, which are the three reported symptoms:

**Write amplification is quadratic in file size.** A file gains a few chunk refs
per carve pass, and each pass rewrites the whole list: `1 + 2 + … + N` refs
written to store `N`. Each ref costs 163 bytes on the wire, measured:

```
{"hash":[11,18,25,…,228],"offset":123456789012,"size":1048576}   → 163 bytes
```

Of which **130 bytes is the hash**, because `ContentHash` is `[32]byte` — a Go
*array*, not a slice — with no `MarshalText`, so `encoding/json` emits it as a
list of decimal numbers. Base64 would be 46 bytes, hex 66. A 2.5× constant
factor sitting on top of a quadratic.

At the observed 195,414 chunks and 163 bytes/ref, a rewrite every ~12 chunks
reproduces 245 GiB. That is an order-of-magnitude fit rather than a proof — the
real workload spanned several files — but it is the only mechanism on the table
that generates hundreds of GiB from 195k chunks, and the 1633:1 vlog:sst ratio
independently says the bulk is in large values under few keys.

**That key is a per-file contention hotspot.** Every writer and the flush
dispatcher touch `fm:<uuid>`. The log shows the conflict on one file, retried
for over an hour and never succeeding: the retry re-reads and re-writes the hot
key while SMB writes keep landing on it. This is a **livelock**, not a transient
conflict, and no backoff schedule fixes it because the contention is structural.

**And the livelock wedges the journal**, by the chain in §1.1.

Two tells that this was already half-understood: `badger/transaction.go:446`
deliberately skips the manifest rewrite for attr-only updates (chmod, utimes,
rename) — the optimisation was applied to the cheap path and not the expensive
one. And postgres does not have the defect at all: `file_block_refs` is one row
per `(file_id, offset)`.

**Two backends, one interface, categorically different scaling.** The
`storetest` conformance suite cannot catch it, because conformance tests
correctness and this is an amplification property. That is a gap in the
contract, not just in badger.

## 3. One shape, eleven incidents

`rfc-block-dataflow.md` §1 established that every HIGH finding in the journal
audit is a *residency-truth* failure: the cache's map of what it holds
disagreeing with what it actually holds. Ten field incidents over a year, one
defect class — #1850, #1879, #1888, #2084, #1872/#2073/#2093, #1956, #2110.

The production outage is the same shape one level up. In the ten, the journal's
map disagreed with its disk and reads resolved to zeros. Here the journal's map
disagreed with the *remote* and writes resolved to a permanent stop. In both
cases the journal is the only oracle, and in both cases its answer is
load-bearing for a question it does not have the information to answer.

**The root cause is not a bug in any of the eleven. It is that a byte range's
existence and its location are stored in the same place.**

Today a range is described by three booleans in `index.go`'s `interval`:
`synced`, `cold`, and "no interval at all". The ambiguity is in the third: a
range that was **never written** and a range that was **evicted** are the same
observation — silence — and both render as zeros. `cold.log` exists *only* to
disambiguate them, which is why it is a durable side-log recording the absence
of data rather than its presence.

## 4. The cut: two oracles

Split the two questions to the two places that can actually answer them.

> **The journal answers exactly one question: do I hold these bytes on this
> disk, and at what offset. It has no opinion about what exists.**
>
> **The metadata store answers: does this range exist, which chunk covers it,
> which block holds it, and is that block durable remotely.**

Residency stops being a stored fact and becomes a join:

| metadata says | journal says | resolved state | read behaviour |
| --- | --- | --- | --- |
| no chunk covers this offset | — | `Absent` | zeros — **correct** |
| chunk exists, block not durable | present | `Dirty` | serve locally |
| chunk exists, block durable | present | `Resident` | serve locally |
| chunk exists, block durable | not present | `Remote` | **fetch**, then serve |
| chunk exists, block not durable | not present | `Lost` | **fail** — never zeros |

This is the five-state model of `rfc-block-dataflow.md` §2, with the key
difference that **no state is stored anywhere**. `StateLost` was the point of
that RFC — "the state the bugs kept falling into with nowhere to go, so they
rendered it as `StateAbsent` and served zeros". Here it is a cell in a join over
two independent sources. It cannot be forgotten, because nobody writes it.

The table is total, and every cell is reachable from data the two layers already
hold. That is the whole proposal; everything below is consequence.

### 4.1 Why this is affordable

Three facts, each verified in the current tree rather than assumed:

1. **Metadata is already on the cold-read path.** `chunkWindowResolver`
   (`engine/read_internal.go:152`) already asks metadata which chunk covers an
   offset and which block holds it, on the same request that would consult
   `interval.cold`. The `cold` bit is a cache of a fact we look up anyway.
2. **Eviction needs no new durable write.** `evictable()` already requires
   `syncedRecords == records`, so a segment only becomes a candidate once every
   record in it is on the remote — which means the metadata rows recording that
   durability were committed at sync time. Eviction flips rows that already say
   "durable remotely" to also say "not local". No two-phase commit, and the
   crash ordering is the one `cold.log` already uses: commit the fact, then
   unlink the bytes.
3. **The #1850 objection is dead.** Metadata-as-oracle was previously rejected
   because a truncate could resurrect a range through a stale chunk row. That
   was fixed by `narrowChunkRow`; verified in the current tree, not recalled.

### 4.2 What it costs

- **The journal stops being a dependency leaf.** Its declared dependency set is
  stdlib + `golang.org/x/sys`, enforced by `foreign_imports_test.go`. Under this
  design it must be handed a residency oracle. It should take a small declared
  interface it defines itself (so the import direction still points inward and
  the test still passes on the package), with the metadata store satisfying it
  at the composition root.
- **A read of an absent range costs a metadata lookup** in the worst case. See
  §4.1(1): on the cold path this is not a new round trip. On a genuinely sparse
  read it is.
- **`RestoreToVersion` is a second independent reader of `cold.log`** (plan item
  4C) and has to move with it.

## 5. Vocabulary: one word, one meaning

The current names cost real time in every discussion, and one of them is a
misnomer in the code rather than only in prose.

| Today | Becomes | Why |
| --- | --- | --- |
| `FileAttr.Blocks []ChunkRef` | `FileAttr.Chunks` | It holds chunk refs. It has never held blocks. |
| `block.FileChunk` | `block.Chunk` | Keyed by content hash and ref-counted, so one row is shared by every file using it. "FileChunk" reads as per-file, the opposite of the dedup property it implements. |
| `block.ChunkRef` | unchanged | One file's use of a chunk at an offset. The per-file side. |
| `BlockRecord` | `BlockRefState` | Not an identifier and not a journal record: it is the block's refcount (`live_chunk_count`), which is what makes remote deletion safe. |
| `journal` (the package) | *candidate for rename* | In filesystems a journal is a write-ahead log for metadata consistency. This is the data store itself, and the misnomer is part of why `GC` was read as touching remote refcounts. |
| `Segment` | unchanged | Standard term for an append-only, size-capped, sealed-then-immutable file. Kafka, Bitcask and the LSM family all use it. |
| `Shard` | *fold into segment + lock* | A shard is not a partition of files, it is a partition of the **write lock**: one active segment plus the mutex serialising appends to it. Once each active segment carries its own lock, the word adds nothing. |

The partitioning itself cannot be dropped — collapse to one active segment and
every concurrent write in the share serialises on one mutex. `ShardCount` is
already configurable and `1` is legal, so the single-lock shape is a config, not
a redesign. Reducing the count is worth considering on its own merits: a shard
is also the unit of fsync and of dirty tracking, so N shards means N fsyncs per
flush tick and N places for a crash to land mid-ordering.

### 5.1 Identity: three identifiers, three lifetimes

`FileAttr` carries three, and the doc that preceded this RFC named one. All
three are load-bearing:

| Field | Type | Lifetime | Job |
| --- | --- | --- | --- |
| `ID` | `uuid.UUID` | forever | metadata identity; survives rewriting contents; what `file_block_refs.file_id` references |
| `PayloadID` | `string` | per content assignment | the content-repository key — **this is what becomes the journal's `FileID`** |
| `ObjectID` | `[32]byte` | changes on every write | BLAKE3 Merkle root over sorted `ChunkRef.Hash` (`file_types.go:144`); the whole-file dedup oracle behind `FindByObjectID` |

Two problems worth fixing:

**The `PayloadID → FileID` conversion is invisible.** `journal.FileID` is
`type FileID string`, so every call site is a string-to-string cast and nothing
checks that the argument is a `PayloadID` rather than, say, a UUID rendered as
text. A constructor-only named type makes that a compile error.

**`PayloadID` may be deletable.** Its only job is to key content, and `ID` could
do that. What it costs today is badger's `pl:` reverse index
(`encoding.go:61`, `PayloadID → file UUID`), which exists precisely because the
two are independent and the journal only ever knows one of them — and which is
the subject of #1885, the O(N²) payload-index outage. **Open question:** does
anything rely on a file's content identity being reassignable independently of
its metadata identity? If not, deleting `PayloadID` removes an index and a bug
class. This RFC does not claim the answer; it flags it as the cheapest
structural simplification available if the answer is no.

### 5.2 Chunks and blocks, stated precisely

Both were questioned during review, and both current behaviours are defensible
but undocumented.

**A chunk has no minimum size.** The FastCDC profile is min 1 MiB, avg 4 MiB,
max 16 MiB (`chunker/doc.go:4`), but "min" only means *do not look for a
boundary before here*. On the final chunk, whatever is left is emitted whole. A
4 KiB file is one 4 KiB chunk. Nothing is ever padded.

**Content-defined sizing stays.** Fixed-size chunks would make `offset → chunk`
arithmetic, but inserting one byte at the start of a file shifts every
downstream boundary, so every chunk gets a new hash and the whole file
re-uploads. CDC is the reason append-heavy and edit-heavy workloads do not
re-upload the world. The profile parameters are fair game to tune; the property
is not. Losing it would trade the dedup mechanism for a lookup we already index.

**A block targets a size; it does not have one.** The carver accumulates chunks
until the batch reaches `BlockSize` and never splits mid-chunk, so a block
overshoots by at most one chunk. `BlockSize` is 8 MiB (`types.go:19`) while a
chunk may reach 16 MiB, so one large chunk can be an oversized block by itself.
Making blocks exactly fixed would let a chunk span two blocks, costing two
locators and two GETs per chunk and breaking "one chunk = one hash = one
addressable thing". The target-with-overshoot rule is worth more than an exact
size. `types.go`'s `BlockSize` const and the `CarveBlockSize` config should not
both exist; the config wins.

### 5.3 The abstract metadata layer already exists

`pkg/metadata/store.go` declares `FileChunkStore` and `BlockRecordStore` with
`ListFileChunks`, `GetFileChunk`, `PutSyncedLocators`, `EnumerateFileChunks` and
`FindByObjectID`; badger and postgres implement them differently; `storetest`
pins conformance. The abstraction is not missing. It has three defects:

1. **The names leak** (§5).
2. **One capability is negotiated by type assertion.** `GetFileChunkAtOffset` —
   the indexed covering-chunk lookup — is reached through an unexported
   `chunkAtOffsetResolver` that `read_internal.go:217` type-asserts, falling
   back to a full `ListFileChunks` walk when the assertion fails. A rename in a
   backend therefore **silently downgrades every cold read to a linear scan**
   instead of breaking the build. The same pattern exists on the journal side
   for snapshot pinning (`restorer`, `pinner`, `versioner`, `coldSeeder`,
   `coldRangeReporter`, `coldSeedTracker`). These belong on declared interfaces.
3. **The reverse direction is missing.** Nothing answers *"which chunks does
   this segment hold"*, which is what eviction needs (§7).

Conformance must also grow an **amplification** axis: assert that writing a file
of N chunks performs O(N) chunk-row writes, not O(N²). §2 is invisible to a
correctness suite.

## 6. Physical layout, and how it scales

A segment is capped, not unbounded: `defaultSegmentSize` is 256 MiB
(`store.go:111`), floor 1 MiB, configurable. What reaches hundreds of GB is the
*directory* — 160 GiB in the incident is roughly 640 segment files.

At TB scale, three things grow with segment count, and filesystem speed is not
one of them (reads are `pread` on an already-open fd, and ext4/xfs handle tens
of thousands of directory entries without trouble):

1. **Open file descriptors — the binding constraint.** Every sealed segment
   holds one. `reclaim.go:89` already names it: *"the ceiling is RLIMIT_NOFILE,
   not disk."* At 256 MiB, 1 TB of local cache is ~4,100 segments, past the
   common 1024 default before the first terabyte.
2. **Recovery scan** — O(total bytes) today; O(records) after the header-only
   scan (plan item 4D).
3. Directory enumeration in the orphan sweep.

### 6.1 The tension segment-granular eviction cannot resolve

- fd exhaustion pushes segment size **up**
- eviction precision pushes segment size **down**

Segment-granular eviction couples them, so the trade cannot be tuned away: at 1
GiB segments you free 1 GiB at a time and evict whatever else shared the file,
so hit rate collapses exactly when you sized up to survive the fd ceiling.

**Decouple them by punching holes instead of unlinking files.**
`fallocate(FALLOC_FL_PUNCH_HOLE)` releases a range's extents while leaving the
file and every offset in it valid. Then:

- eviction granularity becomes the **chunk**, independent of segment size
- no repack, no data movement, no index rewrite — survivors keep their offsets
- segments are sized purely for fd economy
- `unlink` remains the special case for a segment that goes fully empty

It fits the existing structure: the journal already has `statfs_unix.go` /
`statfs_windows.go` over `golang.org/x/sys`, so `unix.Fallocate` adds no
dependency and `foreign_imports_test.go` is unaffected.

Two caveats that are part of the work, not objections to it: `diskBytes` must
switch from file length to allocated blocks (`st_blocks`), or the accounting
will not see the reclaim; and filesystems without punch support need repack as
the fallback path.

## 7. Eviction and reclaim

Under §4, eviction is: *choose bytes, mark their chunks non-local in metadata,
release the bytes.* It is never allowed to touch a chunk whose block is not
durable remotely — that transition is `Dirty → Lost` and is data loss. This is
the transition table of `rfc-block-dataflow.md` §2 expressed as a precondition
on one operation rather than a guard someone remembered to add.

Three granularities were considered. **The production incident rules on this
question more clearly than any argument:** at 154 of 160 GiB unsynced, *no*
granularity would have freed a single byte, because every granularity requires
remote durability first. Granularity is an efficiency question, not a safety
one.

| Granularity | Frees disk? | New index? | Moves bytes? |
| --- | --- | --- | --- |
| Segment | yes, whole file | segment → chunks | no |
| Block | no — a block's chunks are scattered across segments, so it frees extents in several and unlinks none | no | only via repack |
| File | only segments that file alone filled | no | no |
| **Chunk, via hole punch** | **yes, per extent** | segment → chunks | **no** |

Chunk-via-punch dominates: it frees precisely what it marks, moves nothing, and
is the only option that survives §6.1. It needs the same segment → chunks
reverse index that plan item 4F already wants, so **4F is where `cold.log` dies**
and `cold.log`'s requirements are a constraint on 4F rather than a separate
project. Note that 4F's original reverse-index proposal was declined on
evidence (six duplicate index walks that a callback helper cannot serve); that
decline was about collapsing the walks, not about the index, and this RFC
re-motivates the index on different grounds.

## 8. Backpressure: the wedge needs a defined behaviour

`MaxLocalBytes` is advisory today, and the code says so — `evict.go:461`: *"reads
`diskBytes` without reserving, so N concurrent writers across shards can"* all
pass the check at once. That is how 30.6 GiB became 160 GiB under concurrent
SMB writers.

Fixing the manifest key (§2) removes the livelock that triggered this outage,
but the design still owes an answer to *"the remote has been unavailable for
hours and local is full"*. Three requirements:

1. **Make the cap real.** Reserve against `diskBytes` before accepting a write
   rather than reading it. A 5× overshoot is not backpressure.
2. **Fail early and clearly at the cap.** Under §4, dirty bytes can never be
   evicted — dropping them is `Dirty → Lost`. So refusing the write is the
   *correct* behaviour; it must simply happen at the cap instead of at 5× it.
3. **Surface sustained flush failure as share health, not a WARN line.** The
   operator had over two hours of `flush dispatcher: file flush failed` before
   the first write failed. That is a diagnosable state and it should be visible
   as one.

And structurally: **with per-chunk keys, a chunk's durability flip touches only
that chunk's key.** The per-file hot key disappears, and with it the conflict
class that livelocked this deployment.

Separately, `DB Closed` reached a live flush dispatcher at 16:50:37 — a
shutdown-ordering bug independent of everything above, and worth its own fix.

## 9. What gets deleted

- `cold.log`, `cold-seeded`, and the whole cold-log lifecycle: `loadCold`,
  `appendColdPublish`, `maybeCompactColdLog`, `coldCompactWorthIt`,
  `liveColdSnapshot`, `rewriteColdLocked`, the `coldMu`/`coldEntries`/
  `coldBroken`/`coldInFlight`/`coldRefusals` state, and the compaction-blocked
  reporting around it.
- `interval.cold` and `interval.synced`. Journal-local `interval` keeps
  `fileOff`, `length`, `version`, `loc`, `recOff`.
- `carryMarkersForward`, and the marker-ordering dance in `evict.go` and
  `reclaim.go` that exists to keep a tombstone alive across a retire — a
  tombstone's job was to make a delete durable in a log that is no longer the
  source of truth.
- The `coldSeeder` / `coldRangeReporter` / `coldSeedTracker` structural
  interfaces.
- The `sweepIdxSidecars` sweep (`recovery.go:545`), once no build writes
  sidecars. For the record: the `.idx` files were **write-only** — written on
  every seal and read by nobody, the index always rebuilt from the record
  stream. That is why they were deleted (`df4ac748d`). Reintroducing them is new
  work, not a restoration, and if an on-disk index is ever justified by
  measurement it belongs in a **segment footer** (as in an SST, which keeps its
  index inside the same file) rather than a sidecar: a footer cannot go missing
  independently of the data it indexes, which is precisely the failure mode this
  RFC exists to eliminate. Do the header-only scan (4D) and measure first.

## 10. Impact on the Wave 4 plan

`.planning/2026-09-22-block-simplification-PLAN.md` stays valid; its ordering
changes, and it gains a wave in front.

**New, and ahead of everything else — the production fix. Ships alone, on its own
PR, with no refactor attached:**

- **W0a — per-chunk manifest keys in badger.** Replace the single `fm:<uuid>`
  JSON blob with one key per chunk ref, so a carve pass writes O(chunks added)
  instead of O(chunks in file). This is the outage. Needs a format migration and
  a read path that tolerates both layouts.
- **W0b — `ContentHash` gets `MarshalText`/`UnmarshalText`.** 130 bytes → 66
  (hex) or 46 (base64) per hash. Independent of W0a, a 2.5× constant factor, a
  handful of lines. Touches every persisted JSON that embeds a hash, so it needs
  the same dual-read care.
- **W0c — make `MaxLocalBytes` a reservation**, and surface sustained flush
  failure as share health (§8).
- **W0d — fix the shutdown ordering** that let `DB Closed` reach a live flush
  dispatcher.

**Reordered:**

- **The vocabulary renames (§5) move to the front of the refactor**, before 4B's
  file split. They are mechanical and revertible, and every later discussion is
  cheaper in the new words. Renaming files and concepts in one pass costs less
  than renaming concepts inside freshly-split files.
- **4D (header-only recovery scan) stays where it is** and now also answers the
  open-latency question that an on-disk index would otherwise be proposed for.
- **4F becomes the landing site for §4 and §7**: one reclaim entry point over the
  segment → chunks reverse index, with hole-punch as the eviction primitive, and
  `cold.log`'s deletion inside it. #2822's baseline must still be taken before
  it starts.
- **4C (`RestoreToVersion`) must follow 4F**, because it is the second
  independent reader of `cold.log`.
- **4A′, 4B and the metrics/staging-buffer items are unaffected.** (The plan
  currently numbers two different items "4E"; that wants fixing.)

## 11. Verification

The danger-zone table in the plan still applies. This RFC adds four checks, each
aimed at a failure this design could plausibly reintroduce:

| Zone | Test at the consumer |
| --- | --- |
| Write amplification (W0a) | Write a file of N chunks; assert chunk-row **writes** are O(N), not O(N²). A correctness assertion on the resulting manifest passes against the quadratic implementation. |
| The join is total (§4) | Drive all five rows of the §4 table, including `Lost`. Assert the `Lost` read **fails** — a test that only checks the other four passes against a build that serves zeros. |
| Hole-punch accounting (§6.1) | Punch a range, then assert `diskBytes` **decreased**. Against a `diskBytes` derived from file length this fails, which is the point. |
| No-escape wedge (§8) | Break the remote, fill to the cap, assert writes are refused **at** the cap; then restore the remote and assert the store recovers **without operator action**. The current design fails the second half. |

The existing rigs cannot see the ordering bugs this touches: both write **one
file**, so one `FileID` hashes to one shard, and `test/crash/device-loss.sh:7-9`
states that `kill -9` does not reproduce the failure because the page cache
outlives process death. Residency-ordering work needs `dm-flakey drop_writes`, N
files across ≥2 shards, and a concurrent delete in the window.

And the standing rule: **verify every regression test by reverting the fix and
watching it fail on its assertion.** A build error from an unused import is not
proof.

## 12. Where to push back

The parts most likely to be wrong:

1. **The quadratic-manifest diagnosis is a fit, not a proof.** The mechanism is
   certain (one key, full rewrite, 163 bytes/ref — all read from the code and
   measured); that it accounts for *all* 245 GiB is inference from an
   order-of-magnitude match. If someone can decode the vlog and count actual
   `fm:` versions, do that instead of trusting this.
2. **The journal taking a residency oracle** is the largest structural
   concession here. It ends the pinned-leaf property that
   `foreign_imports_test.go` protects. The interface-defined-inward mitigation
   keeps the test passing but does not make the coupling go away.
3. **Hole punching** assumes `FALLOC_FL_PUNCH_HOLE` behaves across the
   filesystems deployments actually use, and that per-chunk punching does not
   fragment segments into an extent-map problem at TB scale. Neither is measured.
4. **Deleting `PayloadID`** (§5.1) is stated as an open question, not a
   proposal. If something needs content identity reassignable independently of
   metadata identity, it stays and the `pl:` index stays with it.
5. **Whether the vocabulary renames are worth a wave of churn** at all. The
   argument is that they are cheap now and compounding later; that is a
   judgement, not a measurement.

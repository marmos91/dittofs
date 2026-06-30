# #1414 Object Packing — Execution-Ready Design (PR3)

**Status:** design (build after PR1 lands). Tier-1 flagship small-file feature.
**Author context:** #1432 upload-perf wave. PR1 = sustain upload concurrency (the 13×, 25→337 Mbit/s, single-stream→saturated window). PR2 = drain-uploads inactivity timeout (shipped, PR #1460). PR3 = this = object packing, the small-file lever, closes the per-tiny-PUT drain ceiling.

## Latest news (2026-06-30)
- **PR2 shipped** (PR #1460, CI green): drain-uploads now inactivity-bounded, no total cap, `controlplane.drain_stall_timeout`.
- **PR1 in progress** (branch `1432-pr1-upload-concurrency`): continuous upload dispatcher so steady-state streaming holds inflight≈window instead of 1. First agent stalled (infra); relaunched with commit-per-milestone discipline. **PR3 rebases on PR1** — both touch the syncer mirror/CAS path; do not start PR3 code until PR1 is merged.
- **Sequencing dependency:** PR3 also interacts with #1433 (tombstone-GC refcount rework, plan approved). Pack liveness/compaction (PR3c) must be built ON TOP of #1433's per-chunk refcounts. Coordinate or land #1433 first.

## Terminology & data model (LOCKED 2026-06-30 — authoritative; supersedes "pack" wording below)
"Pack" as a noun is dropped — the packed object is just a **block**. "Packing" survives only as a verb.

| Term | Meaning |
|---|---|
| **Chunk** (a.k.a. *slice*) | content-defined piece of a file (FastCDC + BLAKE3). The dedup unit. Sized by `ChunkSize` (min 1 / avg 4 / max 16 MiB). |
| **Block** | the **unit of transfer** to/from the block store — what gets PUT/GET as one object (1+ chunks concatenated). Sized by `BlockSize` (target ~16 MiB = rclone part size); sized large precisely to amortize per-transfer overhead. Pre-packing: 1 chunk = 1 block. (Writes transfer a whole block; reads may range-fetch a single chunk via the block index.) |
| **FileChunk** *(rename of `FileBlock`)* | metadata manifest entry: file region → chunk hash/offset/length. Composition, not data. |
| **Block index** | chunk hash → `{BlockID, Offset, Length}`. Finds a chunk inside its block. (PR3a's `ChunkLocator` store.) |
| **BlockStore** | stores blocks. (Name already correct.) |

**Metadata tracks BOTH chunks and blocks:**
- **FileChunk** — file → chunk composition.
- **Chunk** — chunk hash → block-index entry `{BlockID, Offset, Length}` + **refcount** (dedup/GC granularity).
- **Block** — BlockID → member chunk hashes, size, **sync state** (Pending/Syncing/Remote — a block is PUT & synced as ONE unit), live-chunk count (compaction trigger).

Concern split: **refcount on Chunk** (dedup), **sync state + liveness on Block** (storage object). This fixes the legacy `BlockState`-on-`FileBlock` mismatch.

**`ChunkSize` vs `BlockSize`:** both legitimately exist now. `ChunkSize` (chunker params) = chunk/slice size — keep. The current `BlockSize = 8MB` in `types.go` is NOT a block-object size — it's a legacy fixed read-addressing grid (`blockIdx = offset/BlockSize` in fetch.go) that MISALIGNS with FastCDC boundaries (fetch.go's own comments document the footgun). **Rename the 8MB grid → `fetchWindow`/read-region (or delete if vestigial), freeing `BlockSize` to mean the block-object target (~16 MiB).** Tracked in #1470.

Naming for the read op (PR3a): `ReadChunk(blockID, offset, length, hash)` reads a chunk-slice from a block.

## Problem (from #1414 / #1411)
Small-file workload (16384×64KiB) drains at ~0.5 MiB/s, S3 inflight=1, because each tiny file → its own ~64 KiB CAS object → one tiny PUT per file. Two independent per-FILE walls:
1. Write ~64 files/s — per-file synchronous badger commit (fsync). **Out of scope for #1414** — that's the JuiceFS-style async/batched-commit lever (separate issue).
2. Rollup→S3 ~12 chunks/s — one tiny PUT per file. **This is what packing fixes.**

Packing is the *object-side* lever: coalesce N small CAS chunks into one larger S3 object (Haystack/SeaweedFS volume style). Cuts PUT count ~N×.

## Invariants to preserve (non-negotiable)
- **Per-chunk content addressing + dedup**: the chunk BLAKE3 hash stays the dedup key. A pack is just a container; identical chunk bytes anywhere still dedup to one stored copy.
- **Crash-safety Put-then-Mark**: a hash is marked synced only after its bytes are durably remote. Re-upload idempotent.
- **blockstoretest conformance** (`pkg/block/blockstoretest/`) passes at every step.
- **Big-file path unchanged**: large chunks already drain fast (143 MiB/s) — packing applies ONLY to small chunks; large chunks keep one-object-per-chunk.
- **Backward compatibility**: existing `cas/XX/YY/<hash>` objects remain readable with no migration. Packing is additive.

## Core abstraction: ChunkLocator
Today the remote resolution is implicit: hash → object at `cas/XX/YY/<hash>`, whole object. Introduce an explicit locator so a chunk can live either standalone or inside a pack.

```
type ChunkLocator struct {
    PackID string // "" = legacy/standalone object at cas/XX/YY/<hash>
    Offset int64  // byte offset within the pack object (0 for standalone)
    Length int64  // chunk byte length
}
```

- **Locator store**: extend the existing `SyncedHashStore` from `IsSynced(hash) bool` to also persist the locator. "Synced" becomes "has a remote locator." Legacy synced hashes (no recorded locator) default to `{PackID:"", Offset:0, Length:<whole>}` → read path falls back to direct GET. No migration.
- This locator index also IS the dedup oracle: before packing/uploading a chunk, if a locator already exists → dedup hit, skip.

## Read path
`Get(hash)`:
1. Look up locator. If `PackID == ""` → today's direct `GetObject(cas/XX/YY/<hash>)`.
2. Else → `GetObject(packs/<PackID>, Range: bytes=Offset-(Offset+Length-1))`.
3. Read-through cache stays keyed by chunk hash — unchanged (cache at chunk granularity, not pack).

## Write path / packer (UNIFIED — closes large AND small file gaps)
**Decision (2026-06-30):** packing is the unified upload-unit lever for BOTH use cases, not small-file-only. Rationale: the large-file gap (337→402 Mbit/s, ~16%) is purely upload-UNIT size — dittofs PUTs 4MiB FastCDC chunks, rclone PUTs 16MiB parts. Aggregating 4× 4MiB chunks into one 16MiB pack = rclone-parity upload unit **while keeping 4MiB dedup granularity** — strictly better than raising `AvgChunkSize` (which is global and trades dedup + RAM: at PR1's 64-wide window, 16MiB chunks = ~1GiB resident). So packs target 16MiB and aggregate chunks of ALL sizes.

At rollup→upload (the mirror dispatcher from PR1):
- **Classify** each ready chunk: `len(data) >= targetPackSize` (~16 MiB, i.e. already a full upload unit) → standalone object, today's path (no benefit from packing a 16MiB chunk). Everything smaller → feed the packer. (No separate small-chunk threshold — the only bypass is "already pack-sized".)
- **Packer** accumulates chunks into an in-memory buffer, recording (hash, offset, length) as it appends. Seals a pack when buffer ≥ `targetPackSize` (~16 MiB, = rclone part size) OR a flush deadline elapses (so a trickle still ships) OR on DrainAllUploads.
- **RAM bound:** packer buffer is capped at `targetPackSize`; with PR1's window of N in-flight packs, resident = N × 16MiB — same ceiling as today's N × maxChunkSize(16MiB), so no RAM regression vs the existing max-chunk case.
- **Seal**: one `PutObject(packs/<PackID>, buffer)` → then write all N locators → MarkSynced each hash → remove from pending. One PUT replaces N tiny PUTs.
- **Concurrency**: packers run within PR1's bounded upload window — multiple packs in flight = the inflight≈window parallelism, now carrying N chunks each.

### PackID & crash safety — two options
- **(A) Content-addressed pack** `PackID = BLAKE3(buffer)`. Put is idempotent (same content → same key). Crash between Put and locator-write → restart re-packs the same claimed set deterministically → same PackID → idempotent re-Put, then locators written. Requires the packer to be DETERMINISTIC over a claimed set (stable ordering). Cleanest; no orphans.
- **(B) Random PackID + orphan GC**: simpler packer (no determinism needed), but a crash after Put before locators = orphan pack (zero referencing locators) → GC sweeps packs with no live locators after a grace period.
- **Recommendation:** start with (A) content-addressed packs (no orphan class, matches CAS philosophy). Fall back to (B) only if deterministic claiming proves awkward against PR1's dispatcher.

## "I uploaded a small file — where is it?" (small-file UX / durability)
Packing must not make a lone small file appear to never sync. Two distinct expectations, kept separate:

**Durability (the real one).** The file is in the LOCAL block store + badger metadata the instant the write returns — crash-safe immediately, independent of packing (crash → local store has it → re-upload on restart). Packing only affects *when the bytes reach S3*, bounded by:
1. **Idle-seal (primary).** Seal+upload a pack when the input goes idle (no new chunk for `pack_idle_seal`, e.g. ~2s), not only when it hits 16MiB. So "user uploads one txt then stops" → pack seals ~2s after activity ends and uploads. A trickle never waits indefinitely.
2. **Flush deadline.** Hard cap (`pack_flush_interval`, e.g. ~5s) on how long any chunk sits unsealed, regardless of idle detection.
3. **drain-uploads / Close** (PR2) seal+flush all pending packs immediately — `dfsctl system drain-uploads` is the "force it now" button.

**Object visibility (a non-issue, but document it).** The user will NOT see `test.txt` as an object in their S3 browser — but that's already true with CAS today (data lives at `cas/<hash>`, not by filename). Packing changes `cas/<hash>` → `packs/<packID>`; it does not introduce the gap. DittoFS is a CAS filesystem, not a 1:1 file→object gateway (that's rclone/s3fs). The file IS synced and readable (locator resolves the hash); it's the mental model that differs. **Document this explicitly** in docs/guide/smb.md + nfs.md + a packing section.

**Knobs for the "I want per-file immediate sync" user:**
- `pack_idle_seal` / `pack_flush_interval` — tune low for latency-sensitive tests.
- `packing.enabled: false` — full passthrough, one object per chunk (today's behavior) for anyone who wants it.

**Observability:** expose `datapath_pending_unsealed_bytes` + `oldest_unsealed_chunk_age` so flush lag is visible (and to prove a small file did flush).

## Durability contract (DECIDED 2026-06-30: option A) — keyed off the store's Durable() capability
Do NOT hardcode "local = durable." Use the existing invariant already documented in `pkg/block/blockstore.go`:
```
committed := localDurable || (Finalized && remoteDurable)
```
where `localDurable`/`remoteDurable` = each store's `Durable()` report (`block.IsDurable(store)`; FS/S3 default true, memory default false, overridable via per-store `SetDurable`), and `Finalized` = the block sealed + uploaded.

- **Durable local store (FS — the normal case):** `localDurable==true` → a write is committed the instant it lands locally → `fsync`/`close`/NFS COMMIT/SMB flush satisfied locally; remote is async, fully-packed offload (option A). Not weaker than POSIX — POSIX `fsync` never promised off-host replication.
- **Non-durable local store (memory):** `localDurable==false` → committed ONLY when `Finalized && remoteDurable`, i.e. when the block seals + uploads to a durable remote. Deferred packing then means **nothing is durable until the block seals** → a memory-local store **cannot honor `fsync` by local persistence**. Such a config is non-durable / test-only: the packer must NOT claim local-immediate durability, and a durability-sensitive deployment must not run memory-local. (Enforce/warn: if `!IsDurable(local)`, either reject for durability use or force seal+upload on flush — TBD in PR3b.)

So the packer reads `IsDurable(local)`: defer remote only when the local tier actually satisfies durability; otherwise durability requires the (durable) remote and deferral breaks the contract.

**Docs guidance (user-facing):** `BlockSize` is the single lever for the efficiency-vs-remote-lag tradeoff — no `idle_seal`/`flush_interval` jargon needed. Document plainly: *"Blocks upload to S3 when they reach `block_size` (default 16MiB) or on force-sync. If you want data offloaded to S3 sooner — a smaller window of local-only data — set a smaller `block_size`, at the cost of more, smaller uploads (less efficient)."* This is the honest answer to "my small test file isn't in S3 yet": fill a block, force-sync, or lower `block_size`. Put this in docs/guide (smb.md/nfs.md + a packing/durability section), alongside the CAS-not-a-file-gateway note.

Max packing efficiency, zero new machinery. **B-upgrade path (future, if a user treats the local node as ephemeral and S3 as sole storage): group-commit** — coalesce concurrent `fsync`s (the bulk small-file case) into one block so durability-on-fsync keeps packing efficiency; isolated fsync degrades to a small block (not a throughput case). That's #1416's batched-commit lever extended to the data path. Not built now; documented as the clean A→B upgrade.

## S3 block upload: multipart (consider in PR3b)
At the **default 16MiB BlockSize**, a single `PutObject` per block is parity-sufficient: a 16MiB PUT ≈ one of rclone's 16MiB multipart parts, and PR1's block-level concurrency (≈64 blocks in flight) already provides the parallelism multipart would. Do NOT stack within-block multipart on default blocks — it just re-slices the same upload window.

Multipart IS required / beneficial when:
1. **`BlockSize` tuned large** (per-store knob → 64/128MiB) — parallel parts + S3's >5GiB hard requirement.
2. **Large standalone chunks** (chunk ≥ BlockSize) — same.
3. **Partial-retry on lossy WAN** — multipart re-sends only the failed part, not the whole block.
4. **Shallow-tail** (fewer pending blocks than the window) — within-block multipart can fill idle slots. Minor.

**Plan:** the S3 block store gains a multipart-upload path with a **cutover threshold** (single `PutObject` below, multipart above; default block = single PUT). Wire it so it composes with PR1's block-level concurrency (don't double-parallelize). Crash-safety unchanged: a block (single-PUT or multipart-completed) is durable before its chunks are MarkSynced + indexed.

## PR3b requirements (folded) + rejected alternatives — prior-art driven
**Folded into PR3b (blocking):**
1. **Self-describing blocks (rebuildable index).** Each block carries a footer listing its member chunks `{hash, offset, length}` (cf. restic pack header, SeaweedFS needle headers). The block index in metadata is then **reconstructable by scanning blocks** — lose/corrupt metadata and the data in S3 is still recoverable. Cheap footer, classic CAS-FS insurance.
2. **Per-chunk compression, then pack** (cf. restic/Borg — unanimous). Compress each chunk independently *before* the packer concatenates them. Preserves dedup (deterministic per chunk) AND range-GET (each chunk independently decompressible). Block index lengths are POST-compression. Wire compression decorator ahead of the packer. (Whole-block compression rejected: breaks range-GET + dedup.)
4. **Atomic multi-entry index write (correctness).** Sealing a block writes N chunk-index entries + the block's synced mark in ONE metadata transaction — a crash must not leave a block half-indexed. **Metadata-scaling:** the dual Chunk/Block metadata split mirrors JuiceFS's externalized-metadata design (the reason JuiceFS scales small files); aim for similar efficiency — compact index keys, batched writes — and MEASURE index growth/lookup at 16K+ chunks (Phase 0).

**Tracked separately (not PR3b-blocking):**
3. **Read amplification / prefetch** — range-GET one chunk vs fetch-whole-block-as-readahead (locality-packed blocks make whole-block fetch effective readahead; JuiceFS/SeaweedFS prefetch). Ties into read-through cache (#1362). New tracking issue.

**Rejected:**
- **Small-file inlining** (data inlined in metadata, skip chunking) — CONTRADICTS #1 (inlined data isn't in any block → can't rebuild from blocks → unrecoverable if metadata lost) AND the durability model (durability requires bytes in a `Durable()` block; metadata-inlined bytes bypass it). Out.
- **Whole-block compression** — breaks range-GET + dedup (see #2).

## Deletion, GC + compaction (build on #1433)
Prior art (Haystack/SeaweedFS vacuum, restic prune+repack, Borg compact, JuiceFS compaction) all converge on the same shape — adopted here:

- **Refcount per-chunk** (#1433 tombstone GC): deleting a chunk decrements its refcount; refcount 0 = chunk dead. Bytes inside the block stay dead (can't partial-delete an immutable object).
- **Per-block liveness** on the Block metadata: live-chunk count (decremented when a member chunk hits refcount 0).
- **Fully-dead block → delete** when live-count hits 0. Cheap, no data movement. The primary reclamation path; ship this FIRST (PR3c-1).
- **Partial-dead block → compaction** (PR3c-2): when live-fraction < `compact_below` (default 0.5 — like SeaweedFS's garbage threshold, we **tolerate waste** to avoid I/O churn; do NOT chase 100% packing), a background job reads live chunks → new block → atomically repoints the **block index** → deletes old block. Idempotent / crash-safe: Put-new → reindex → delete-old (same Put-then-Mark ordering; crash leaves old block resolvable, orphan new block GC'd).
- **Bound repack cost per run** (restic `--max-repack-size` style) so a fragmented store doesn't rewrite everything at once.
- **Write-time locality** (restic-style): pack chunks from the same file/dir into one block, so deleting a file tends to kill a WHOLE block → fully-dead-delete path, no compaction needed. Biggest lever to avoid compaction for the common case.
- **Phase discipline:** whole-block-delete first (covers immutable/backup workloads where chunks rarely die alone); add compaction only once fragmentation/space-amplification is *measured* (Phase 0 scorecard). Same measure-before-investing rule as the rest of the roadmap.

## Config (minimal — "less is more")
Block stores are per-share, so these live on the store and can be tuned per store. `BlockSize` is SAFE to vary per store — it only changes the transfer unit, not dedup (chunks are identical regardless of how they're packed). `ChunkSize` (dedup granularity) stays GLOBAL — varying it per store would fragment cross-store dedup.
```
block:
  block_size: 16MiB          # target block (transfer unit) size = seal threshold = rclone part size.
                             # chunks >= this go standalone. Per-store tunable.
  packing:
    enabled: true            # default on once stable
    compact_below: 0.5       # live-fraction trigger for compaction
```
One safety trigger only: **`max_sync_delay`** (idle timer, default 5min — see triggers below). Exposed as a real per-share config knob in the **syncer / block-store config** (NOT `APIConfig` — that hosts PR2's `drain_stall_timeout`, which bounds the API drain handler, a different layer). Sibling default: `DefaultMaxSyncDelay = 5*time.Minute` mirroring `DefaultDrainStallTimeout`; `mapstructure/yaml:"max_sync_delay"` + `DITTOFS_*` env. Field + wiring ship in PR3b alongside the sealer that consumes it (no dead knob beforehand). `ChunkSize` stays a global chunker constant. Resist adding knobs beyond these.

> **REVISION 2026-06-30 (supersedes the earlier "no idle_seal knob" call and the exploratory `pack_idle_seal` 2s / `pack_flush_interval` 5s hard-cap sketch above).** We DO add one idle-based seal trigger, but reframed as a **safety default, not a perf knob**, and as an **inactivity timer with no absolute cap** (consistent with PR2 drain's inactivity-timeout, no-total-cap choice). Rationale: it does NOT fix a commit-durability hole — under option A an FS-local chunk is fsync'd durable BEFORE the NFS-commit/SMB-close returns, so DittoFS promises *local* durability, not cloud, on commit. What it bounds is **time-to-second-copy (RPO)**: caps how long a quiescent small file sits local-only-before-mirror to ~5min, so a single-disk death loses ≤5min of un-mirrored writes instead of up-to-BlockSize indefinitely.

### Sync triggers (when a block reaches the store)
1. **BlockSize reached** — the block seals + uploads automatically. Active/streaming writers hit this → full ~16MiB blocks (best packing, rclone-parity transfer unit, fewest objects).
2. **Idle ≥ `max_sync_delay`** (default 5min, per-share overridable — **real config knob, not hardcoded**) — **inactivity timer on the open block, RESET on every new chunk appended to it.** Seals even if sub-BlockSize. NOT absolute block age — absolute would split an actively-growing hot block and wreck packing. So: a sustained writer keeps resetting the timer and seals via (1) at full size; only a **small copy followed by idle** trips this and seals a small block. The two triggers self-select — small/fragmented blocks happen ONLY when the user already stopped writing (so throughput isn't hurt), and the high-throughput path always gets full blocks. No absolute hard cap (matches PR2; slow-trickle-just-under-deadline-forever pathology accepted, revisit only if observed).
3. **Explicit force** — `dfsctl system drain-uploads` (PR2) seals ALL partial blocks and uploads them. The user-facing "make it durable in S3 now" button. **Also resets the idle timer** — it's a seal, so the open block closes and the next block's timer starts fresh on its first chunk; never leave a stale timer on an already-sealed block.
4. **Graceful shutdown / Close** — seal+flush partial blocks so a clean stop never strands un-synced data (correctness, not a knob).

A partial (sub-BlockSize) block otherwise stays **local-only** until (1)/(2)/(3)/(4). Durability model: **local-immediate, remote-on-seal-or-force** — local writes are crash-safe instantly; S3 (backup tier) lags by at most `max_sync_delay` of idle (or one partial block, or until forced). Document this explicitly (a lone small test file appears in S3 within ~5min of going idle, or immediately on force) — and note dittofs is CAS, not a 1:1 file→object gateway.

**TENSION to document in `docs/guide`:** an idle-seal emits a block < BlockSize → more/smaller S3 objects → mild read-amplification + worse packing ratio. This only bites the bursty-small-file-then-idle workload (the very workload packing targets), and sealed blocks are immutable so they stay small until **PR3c compaction** repacks them. Accepted: 5min idle is long enough to coalesce a burst, short enough to bound RPO.

## Phasing (reviewable PRs, each conformance-green)
- **PR3a — Locator indirection (no packing yet).** Extend SyncedHashStore→locator store; read path consults locator; all writes still produce standalone objects (locator PackID=""). Pure refactor + new store column. Conformance green, behavior identical. *De-risks everything downstream.*
- **PR3b — Packer on write path (UNIFIED).** Aggregate all sub-16MiB chunks → sealed 16MiB pack → one PUT → locator batch-write. Only ≥16MiB chunks go standalone. Config gate. Read path already handles packs from PR3a. **This is the headline win for BOTH large-file (16MiB upload units = rclone parity) and small-file (coalescing) throughput.**
- **PR3c — Pack GC + compaction.** Pack-liveness index, whole-pack delete, partial-pack compaction job. Depends on #1433 refcounts.
- **PR3d (optional, later) — Background repack of legacy small objects.** Sweep `cas/**` small objects into packs. Pure optimization; defer.

## Validation
- blockstoretest conformance (incl. append) at each PR.
- Re-run the small-file benchmark on a disposable SCW VM → Cubbit (the #1432 harness): 16384×64KiB drain should jump from ~0.5 MiB/s toward the big-file ceiling; PUT count drops ~N×; S3 inflight follows PR1's window.
- Encryption: a pack is a concatenation of independently-encrypted chunk ciphertexts — per-chunk crypto unchanged; verify range-GET decrypt boundaries.
- Crash test: kill between pack Put and locator-write → restart yields no data loss, no dedup corruption.

## Explicitly NOT in #1414
- The ~64 files/s **write** wall (synchronous per-file badger commit). That's the JuiceFS-style async/batched-commit lever — separate issue (#1411 lever A/B). Packing alone will NOT fix small-file *write* throughput, only the S3-drain side. Set expectations accordingly.

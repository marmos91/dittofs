# Plan: maximize end-user NFS/SMB read+write performance (three walls)

## Context

**Goal:** maximize real end-user NFS/SMB read/write throughput and latency for DittoFS.
Phase 0 (delete legacy CAS, make `LocalChunkIndex` mandatory, fix the seq-write bench —
PR #1688) and Phase 1 (collapse facet-interface plumbing — PR #1690) are **shipped**. This
plan is the perf work they unblocked.

Profiling + eight research/exploration agents (external: WiscKey, RocksDB BlobDB, Titan,
JuiceFS, Haystack/f4/SeaweedFS, restic, Ceph BlueStore; internal: read/carve path, append-log
lifecycle, RPC dispatch, Runtime orchestration, auth/authz) identified **three independent
walls** between the client and the bytes:

- **Wall A — data plane (block store):** each written byte hits disk ~2–3× (append-log →
  rollup copy to logblob → compaction). This is the seq-write wall (~148 MB/s VM).
- **Wall B — metadata plane:** badger `db.Sync` serialization gates create/stat/small-file ops
  (#1687). ~250 ops/s, last of the durable field.
- **Wall C — RPC/adapter/auth plane:** per-op global locks, uncached share/store resolution,
  redundant handle decodes, and per-op allocations that touch **100% of ops**.

**Decision locked with the user:** the block store moves to **chunk-at-carve, write-once**
(WiscKey/JuiceFS key-value separation). The local append blob stores raw written bytes once;
FastCDC + BLAKE3 + dedup run only when carving fixed-size blocks to S3, streaming from the
blob. Accepted trade: **local-disk dedup is dropped; remote (S3) dedup is preserved.**
Encryption already happens at carve today (local is plaintext) — no regression.

**Execution order (settled with the user):**
- **Wall C runs now, in parallel with Wall A's design session** — cheap, low-risk, independent,
  touches every op, and de-noises the Wall A benchmarks. All C1–C8, priority-ordered.
- **Wall A** — first a formal `superpowers:brainstorming` + `code-architect` session to turn the
  locked §1–§6 decisions into executor-ready specs, then the phased redesign.
- **Wall B — deferred**: do Wall A, re-profile the metadata cell, build group-commit only if it
  still walls (Wall A removes ~23.5% of the `db.Sync` load + moves the index off badger).

Each numbered item below is its own PR off `origin/develop`, signed, assignee marmos91,
conformance-gated.

**Cross-cutting engineering mandate (applies to every PR below — clean as you go, not a
separate purge).**
- **Delete dead code + legacy as each area is touched.** Pre-1.0, no prod users: clean cutover,
  never a dual path or deprecate-in-place. When a PR touches a file, sweep it for
  dead/never-called code and legacy compat layers and **delete them** — verify with
  `tokensave_dead_code` + call-graph, then build/test/-race. Legacy on-disk layouts get a
  one-shot migration, then the reader is deleted. Wall A alone removes rollup, the `logblob/`
  manager, the two-source read fallback, the `rollup_offset` fence, #953 splicing, and any
  lingering CAS/migration shims.
- **Simplify abstractions + structure as part of the change.** The recurring goal: a straighter,
  easier-to-trace data path. Collapse needless indirection, fold single-use interfaces onto a
  cohesive one (continuing Phase 1), and prefer the shortest structure that holds — don't leave
  a refactored area more layered than it needs to be.
- **Human, minimal comments — less is more.** Comments describe *what the code does / why*, in
  plain language. No phase/plan/decision IDs, no `FIX-N`/issue-number archaeology carried into
  rewritten code, no narrating the obvious. When rewriting a heavily-annotated area (e.g.
  rollup/compaction's dense `FIX-*` blocks), prune the scaffolding down to the load-bearing
  rationale.
- **Refresh the docs with the design.** As each wall lands, update `docs/` (esp.
  `docs/internals/architecture.md`, `docs/internals/implementing-stores.md`, `docs/guide/`
  config/CLI, `docs/guide/faq.md`) and in-repo `CLAUDE.md`/module docs to describe the new
  write-back-cache block store, the `dfsctl` eviction control, and the removed legacy paths.
  Regenerate `docs/guide/cli.md` via `go run ./cmd/gendocs` after any command/flag change. Docs
  are a user surface — no stale diagrams describing the deleted rollup/logblob design.

---

## Wall C — RPC / adapter / auth quick wins (DO FIRST)

Low-risk, mostly independent, each touches every op. Bank these before the redesign.

**C1. Kill the process-wide activity-timestamp write lock.**
`clients.Registry.UpdateActivity` takes `Registry.mu.Lock()` (write) on **every** NFS+SMB
request just to bump a timestamp (`pkg/controlplane/runtime/clients/service.go:110`). Replace
with a per-client `atomic.Int64` (or sample). Highest fan-in, smallest diff.

**C2. Cache the resolved per-share block/metadata store; decode the handle once.**
`GetBlockStoreForHandle` decodes the handle + `RLock`s the shares registry + map-lookups on
every op (`shares/service.go:2181`); the metadata `GetStoreForShare` does the same
(`runtime/.../service.go:622`). The per-share `*engine.Store` is immutable between
AddShare/RemoveShare. Add a `sync.Map[shareName]*engine.Store` fast-path (NFSv3, stateless)
and stash `*engine.Store` on SMB `OpenFile` at CREATE. Thread the already-decoded `ctx.Share`
down instead of re-decoding 3–4×/op (use existing `*ForShare` variants). Invalidate via the
existing `OnAuthCacheInvalidate`/share-event hook. Dominant orchestration tax.

**C3. Shard the NFS DRC lock.** `drc.go:105` is a single `sync.Mutex` serializing all cacheable
ops (WRITE/CREATE/REMOVE/RENAME) adapter-wide. Shard by client/XID bucket.

**C4. Pool the SMB per-request raw message.** `pkg/adapter/smb/connection.go:339` does
`make + hdr.Encode() + 2×copy` per request on the single-reader loop, unpooled. NFS already
pools (`connection.go:315`). Reuse `internal/adapter/pool/bufpool.go`.

**C5. Cache the SMB AuthContext per (session,tree).** `BuildAuthContextFromUser` re-allocates
Identity + GID slice + `slices.Concat`/dedup of group SIDs on **every** SMB read/write
(`smb/handlers/auth_helper.go:148`). Identity is fixed at SESSION_SETUP. Mirror the NFS
`GetCachedAuthContext`. Invalidate on re-auth (`tryReauthUpdate`); handle-bound ops keep using
the frozen `OpenFile.OpenerUser` snapshot.

**C6. Give the NFS auth cache a TTL + user/group-change invalidation** (`nfs/v3/handlers/doc.go`).
Today it has no TTL and is invalidated only on share-perm events → a UID/group-membership edit
is stale until restart (a **correctness** gap, not just perf). Add ~5 min TTL aligned with the
`pkg/identity` cache, or wire an `OnUserChange` hook to `ClearAuthCache`.

**C7. Micro-locks/allocs (one small PR):** `CommitWrite` RLocks only to read the
`deferredCommit` bool (`io.go:221`) → `atomic.Bool`; per-op oplock-breaker lookup+assert when
no SMB adapter (`read.go:207`) → `atomic.Value` sentinel; thread the already-loaded `*File`
through NFS WRITE permission checks (`PrepareWrite`/`CommitWrite` re-`GetFile` 1–2×/op — use the
existing file-variant `checkFilePermissionsFile`); make `ExtractClientIP` lazy (debug-only).

**C8. Raise the buffer-pool ceiling if rsize/wsize > 1 MB.** `bufpool.go:157` allocates+GCs a
fresh multi-MB buffer per bulk op above the 1 MB tier. Add a tier matching the negotiated max
I/O size. (Only if large rsize/wsize is in use — measure first.)

**Wall C — locked decisions / defaults:**
- **Scope = all C1–C8; runs now, in parallel with Wall A's design session** (independent, cheap).
- **Independent small PRs, not one mega-PR.** Priority by fan-in: **C1 + C2 first** (touch 100% of
  ops), then C5/C6 (auth), then C3/C4/C7, C8 last (measure-gated).
- **C1 = per-client `atomic.Int64` timestamp** (exact, cheap), not sampling.
- **C6 is also a correctness fix** (unbounded stale group-membership today) — ~5 min TTL aligned
  with the `pkg/identity` cache **plus** an `OnUserChange`/`OnGroupChange` → `ClearAuthCache` hook.
  Ship even if perf were neutral.
- **C3 DRC shard** = power-of-2 buckets keyed by client/XID (start 32); **C2 invalidation** rides
  the existing share-event/`OnAuthCacheInvalidate` hook; **C5** invalidates on SMB re-auth and
  handle-bound ops keep using the frozen `OpenFile.OpenerUser` snapshot.
- Each micro-fix keeps its own before/after micro-benchmark of the contended path (no "trust me"
  lock removals).

---

## Wall A — data-plane redesign: chunk-at-carve, write-once

Collapse the two local substrates (append-log + logblob) into **one append-only blob store**;
move chunking/dedup to the carve path. The read/carve seam already routes every consumer
through `LocalChunkLocation{id,offset,len} → one pread` (`GetLocalLocation` + `localBlobReader`),
so the read/carve code is largely unchanged — **the work is lifecycle/GC**, not reads.

**Unifying model — the append blob is a log-structured write-back cache over S3.** Client
writes and cold-read S3 fetches fold into **one primitive**:
`appendRecord(fileOffset, bytes, synced)` → append a record to the active segment + index it
(`fileOffset → {segment,offset,len}`) + set `evictable = synced`. Client WRITE →
`synced=false` (dirty, must carve+upload); cold-read fetch → `synced=true` (clean, mirrors S3).
Everything downstream is one rule keyed on that bit: **read** = newest-wins reconstruct over
local records, gap → fetch S3 block → `appendRecord(...,true)` → serve (L0→blob→S3); **carve** =
scan *dirty* records → chunk/hash/dedup → pack → upload → flip *clean*; **evict** (pressure-
gated) = drop *clean* records only, never dirty; **GC** = repack dead records. Write-ingest and
read-cache population are the *same append*. Asymmetry to handle: dirty records can't be evicted
(only local copy until carved), so if S3 is unreachable and dirty data fills the budget, writes
apply backpressure (JuiceFS-style).

**Packaging — a standalone, independently-benchmarkable library (design directive).** The block
store becomes a self-contained Go package owning **all** its persistent state (blob segments +
per-segment `.idx` + its chunk↔S3-block ref table). Public API keyed by an opaque `fileID`
(`Open`/`WriteAt`/`ReadAt`/`Commit`/`Carve`/`Evict`/`Close`/`Stats`); only injected deps are
narrow interfaces — `RemoteStore` (`PutBlock`/`GetBlock`/`GetRange`; memory impl for benches),
`Config`, `Clock`. **No dependency** on Runtime/adapters/NFS/SMB/metadata-namespace. A
standalone bench harness drives the API against a memory remote and measures MB/s, warm/cold
read latency, and true **write-amplification (bytes-to-disk ÷ bytes-ingested)** with no protocol
stack — prove the numbers in isolation, then wire into the e2e flow. The sidecar-`.idx` §1
decision is what makes this boundary clean (the store needs no shared metadata KV for its index).

**Design gate before implementation.** This is a "get-it-right-once" rewrite. Before any code,
run a dedicated **`superpowers:brainstorming` + `feature-dev:code-architect`** session that turns
the decisions below into an architecture/design doc (package layout, public API + dependency
interfaces, on-disk formats, the `appendRecord` state machine, PR sequence). This plan is that
session's input; §1–§6 record the decisions locked so far.

**Decisions locked (this planning session):**
- **§1 index location = sidecar `.idx` per segment** (Haystack/restic model): index writes bypass
  badger, so they don't feed the Wall B `db.Sync` bottleneck; recovery rebuilds `.idx` from the
  blob; fast cold start. The durable metadata store stays lean.
- **Self-describing, versioned records (foundational):** each blob record embeds the **`fileID`
  (key)** and a **monotonic `version`** —
  `⟨fileID, file_offset, len, version, crc, payload, synced⟩` — WiscKey `⟨key,value⟩` / Haystack
  needle model. `fileID` makes the `.idx` a **rebuildable accelerator** (recovery scans segments,
  each record self-identifies) and enables self-validating GC; `version` keeps **newest-wins**
  well-defined after GC/repack reorders physical layout (blob append-order = write-order only
  until repack). Segments are **shared** (many files packed) — needed for S3 packing + coarse
  whole-segment eviction; `fileID` is what makes shared segments recoverable.
- **Random / sparse writes (correctness vs locality):** writes append in arrival order, so a
  file's bytes scatter physically. **Reads use the interval index** (`fileID+offset → location`),
  not blob order: reconstruct the range newest-wins, zero-fill holes (NFS sparse). Correctness
  never needs blob-order = file-order. **A5 defrag** merges a file's scattered records into fewer
  offset-ordered ones purely for **read locality** + to bound interval-map size (JuiceFS
  slice-merge) — not for correctness.
- **§3 carve trigger = batched (age + size):** accumulate dirty records, carve when a fixed block
  can be filled OR a max-age cap is hit (JuiceFS flush / BlueStore deferred-write). Fixed block
  size = benchmark parameter, **start 4 MiB**. Per-share dedup (invariant #4). Encrypt at carve.
- **§5 eviction victim = coldest (approx-LRU):** per-segment last-access timestamp, evict coldest
  synced segment first (best warm-hit rate). Whole-segment, sealed-immutable. Dead-byte repack
  with garbage-ratio force trigger (#9235).
- **§6 migration = drain → discard → re-hydrate:** on upgrade drain/carve pending uploads (all
  data on S3), discard the old local store, start with an empty append blob that re-hydrates from
  S3 on demand. No converter code; cold cache post-upgrade only.
- **§2 `.idx` durability = lazy (rebuildable):** COMMIT fsyncs only the blob; the `.idx` is
  appended best-effort and rebuilt from the versioned, self-describing records on recovery. One
  fsync on the write path, no data-safety cost (WiscKey/Haystack). GC/repack updates `.idx`
  under the bytes-durable-before-index ordering invariant.

**Design rationale — the "why" (record for maintainers; mirror into a `docs/internals/`
block-store ADR).** Each choice and the alternative it beat:
- **Chunk-at-carve, not at-ingest** → write-once = fastest possible ingest (no FastCDC/BLAKE3/RMW
  on the hot path); overwrites become a trivial append; encryption already lived at carve. Cost
  accepted: local-disk dedup dropped (low value for a bounded cache; S3 dedup preserved).
- **One append blob = write-back cache over S3** → unifies write-ingest and read-cache population
  into one `appendRecord`; deletes the rollup copy, the logblob, the two-source read fallback.
  Simpler *and* faster (the recurring goal). Rejected "keep two substrates" (the copy is the
  amplification).
- **Sidecar `.idx`, self-describing + versioned records** → keeps the byte-index OFF the badger
  `db.Sync` path (helps Wall B), makes the index rebuildable, and keeps newest-wins correct after
  GC. Rejected "index in the metadata KV" (couples to and worsens Wall B) and "minimal records"
  (a lost index = lost data — the RocksDB/WiscKey/Haystack answer is to embed the key).
- **Lazy `.idx`, batched carve, lazy coldest-first eviction** → one fsync/COMMIT; full fixed-size
  S3 blocks for bandwidth; keep data warm for read-hit rate, reclaim only under real pressure.
- **Drain→discard→re-hydrate migration** → S3 already holds everything, so no converter code to
  write-then-delete. **Standalone library** → prove the numbers in isolation before wiring e2e.

Sequenced into reviewable PRs; each must pass `BlockStoreConformance` +
`BlockStoreAppendConformance` (the property-preservation gate) and `-race`.

**A1. Persist the append-log index + make the blob the store of record.** Today `logIndex`
(fileOffset → logPos/len) is in-memory, rebuilt on restart by replay. Persist per-record
locations so warm reads + carve can resolve straight from the blob. Locations become
offset-based (contiguous per record), never content-defined — sidesteps the FastCDC-scatter
problem entirely.

**A2. Move FastCDC + BLAKE3 + dedup to carve; stream into fixed-size S3 blocks.** Carver reads
raw bytes from the blob via the index (streaming, `io.Reader` from `ReadLocalAt`), chunks +
hashes on the fly, dedups against the remote hash index, packs **novel** chunks into
fixed-size blocks (restic pack-file shape: chunks ‖ trailing manifest), uploads once. Keep the
existing `blockcodec.Builder`/packed-block path (#1414). **Fixed block size (1–16 MiB) is a
benchmark parameter** — tune against Scaleway S3, don't guess (JuiceFS uses 4 MiB). Coalesce
small/unaligned writes in the blob before carving (BlueStore deferred-write principle) — don't
cut an object per tiny NFS/SMB write.

**A3. Replace rollup with the carve pipeline; delete the copy.** Remove `rollup.go`
(reconstruct→chunk→hash→`logBlob.Append`), the `logblob/` manager, the two-source read
fallback in `readpayload.go`, the `rollup_offset` monotone-fence machinery, and the #953
straddling-chunk splicing (only needed to re-chunk overwrites, which now just append). Warm
reads serve raw bytes by offset (no hash-verify — the local bytes are authoritative; #1648
verify-once becomes moot). Net: large deletion.

**A4. Lazy, pressure-gated eviction + segment GC (the concentrated risk).** The blob holds three
kinds of bytes: **live-unsynced** (not yet on S3 — must retain), **live-synced/evictable**
(uploaded, safe to drop, re-fetchable from S3), and **dead** (superseded by overwrite / deleted).

*Eviction policy (retain-warm by default — this is the read-perf win):*
- **At carve+upload, mark the chunks/bytes `evictable` in the metadata** (durably-on-S3 flag) —
  this only marks; it does **not** reclaim. Reuse `logblob.EvictBlob`'s synced-gate as the
  "safe to drop" signal.
- **Do NOT evict eagerly.** Keep evictable bytes local so reads stay warm. Read path is
  **L0 (active segment) → sealed blob segments → S3** (cold fallback only when the bytes were
  actually evicted).
- **Run eviction/GC ONLY under real pressure:** (a) local disk usage crosses the
  **user-configured threshold** (`LocalStoreSize`); or (b) an operator runs an explicit
  **`dfsctl` force-eviction** command (extend the existing `store block evict` / `DrainLocalSynced`
  path — that becomes the *forced* path, not the default); or (c) the disk is **near-full when no
  threshold is set**. When triggered, evict whole synced segments (**coldest first**, approx-LRU)
  until back under the target.
- **Disk-pressure is loud + actionable:** when pressure triggers eviction (or when dirty data is
  approaching backpressure), log clearly at `Warn` with the concrete remedy — current vs
  threshold usage, and the exact `dfsctl store block evict …` command to force reclamation and/or
  the `LocalStoreSize` knob to raise the budget. Never let the user hit a silent wall.

*Dead-byte GC (garbage from overwrites/deletes), same pressure gate:*
- Track **per-segment dead bytes** (Titan `discardable_size`); pick the highest-dead-ratio
  segment (size-tiered, not by age). **Repack** live bytes forward (reuse #1497
  relocate-survivors) with a garbage-ratio **force trigger** — the RocksDB #9235 space-amp stall
  (a near-dead segment pinned by a few live bytes) is the trap.
- Seal segments immutable; serve with concurrent stateless `ReadAt` (reuse logblob's sealed-fd pool).
- Ordering invariant (WiscKey/restic): **bytes durable before index durable** — `fsync(blob)`
  → `fsync(dir)` → commit index rows → only then reclaim old bytes. On repack: write new →
  update index → delete old last.
- **Shard GC + carve by partition** (HedgeDB `2^N` partitions): key segments/work by a hash
  partition so GC and carve run **fully in parallel** across partitions with no cross-lock.
  Reinforces C3 (shard the DRC lock the same way).
- Note the existing hazard: the union `BlockReclaimer` refcount-decrement is unsafe under
  concurrent GC (single-sweeper lock is per-`GCStateRoot`, not per-remote) — must be made safe
  when the blob becomes the reclamation target.

**A5. Local defrag compaction + per-segment read filter (read-perf).** Two read-amp defenses as
segments accumulate under write-once:
- Heavy random overwrite → many overlapping records → slow reconstruction. Merge records
  per-payload when overlap count crosses a threshold (JuiceFS slice-count trigger). Background,
  rate-limited via the existing rollup-slot throttle.
- **Per-sealed-segment probabilistic filter** (HedgeDB quotient filter / Bloom): on a warm read,
  skip segments that can't contain the target so lookup stays ~O(1) as segment count grows.

**A7. I/O-stack techniques (HedgeDB-inspired, later / cross-cutting).** Deferred behind A1–A6
and measurement, Linux-only:
- **`O_DIRECT` for background carve + GC bulk reads** so large sequential background I/O does
  not evict the page-cache-warm bytes that foreground warm reads + the (kept) hash-by-read
  depend on. Keep foreground ingest/read on buffered I/O.
- **`io_uring` batched submission** (e.g. an iouring-go binding) on the append + carve paths to
  cut the ~27% `rawsyscalln` overhead the profile shows. Higher risk, Linux-only, own PR, only
  if syscall overhead is still dominant after A1–A6.

**A6. Crash recovery + one-shot migration.** Recovery tail-scans the blob past the last
committed index offset, validates CRC, drops the torn tail, reconciles the chunk index against
surviving bytes (drop danglers) — adapt `recovery.go`. Migration: one-shot converter from the
old logblob+rollup layout (pre-1.0, "delete legacy eagerly" — no permanent dual path).

**A8. Cold-read = serve-direct + async hydrate (read-around + fill).** On cold start the local
blob is empty but the **metadata chunk index survives** (file-offset → chunk hash → S3 block
ref). Maximize cold-read latency by **never blocking the client on a local disk write**:
- Resolve file-offset → chunk → S3 block ref (metadata), `GetObject` the fixed-size block
  (optionally S3 `Range` for a partial block), unpack the needed chunk(s).
- **Deliver the requested bytes straight to the NFS/SMB adapter first** — the client read
  responds as soon as the S3 bytes arrive. Then **asynchronously hydrate** the blob off the
  critical path: `appendRecord(fileOffset, bytes, synced=true)` (clean/evictable immediately —
  already durable on S3) + `.idx`, so the *next* read is warm (L0 → blob → S3). Hydration is
  **best-effort**: on failure (disk pressure) the read already succeeded; the chunk stays cold
  and re-fetches next time.
- Since a fetched block packs many chunks, hydrate **all unpacked chunks** from that block, not
  just the requested one — free prefetch for the GET already paid. Bounded by the eviction
  policy so a cold scan can't blow the local budget.
- **Singleflight concurrent misses by chunk hash** (dedupe both the S3 fetch and the hydrate).
  Pin **buffer ownership**: the hydrator takes the pooled buffer only after the adapter has
  copied to the wire, or gets its own copy — no aliasing the in-flight reply.
- Reuse the read-through cache (#1362) + cold-read sliding-window readahead (#1625/#1630) —
  retarget their local write from the deleted logblob to the append blob.

---

## Wall B — metadata plane (#1687), parallel & independent

Execute the **already-approved** sub-plan at
`.planning/perf/2026-07-14-badger-dbsync-groupcommit-PLAN.md`: a group-commit leader at the
`syncIfRelaxed` chokepoint in `pkg/metadata/store/badger/` so all concurrent durable
`db.Sync` calls coalesce onto one fsync (strict group-commit, zero durability change).
Expected ~2–4× metadata ops/s, clearing ZeroFS. Gated on a Linux A/B (macOS fsync is not a
full barrier). Badger-only; sqlite/postgres out of scope.

**Locked decisions + Wall-A synergy:**
- **Group-commit, not per-writer WAL sharding** (HedgeDB's alternative): the profiled contention
  is badger's single internal Sync RWMutex, which sharding at our layer can't touch — coalescing
  is the profile-correct fix. Badger-internal; the reverted store-agnostic `SyncDurable` is not
  reused.
- **DEFERRED behind Wall A (locked).** The #1687 profile attributed `db.Sync` 76.5% to per-file
  `withTransaction` commits **+ 23.5% to `SetRollupOffset`**. Wall A **deletes rollup** → the
  23.5% disappears, and the sidecar `.idx` (§1) keeps the block byte-index off badger entirely.
  So **do Wall A first, then re-profile the metadata cell**; build the group-commit leader **only
  if** the remaining create/attr-commit load still walls (last of the field vs ZeroFS 354). This
  avoids building a fix for load Wall A removes. The approved sub-plan stays on the shelf, ready.

---

## Critical files

- **Wall C:** `pkg/controlplane/runtime/clients/service.go` (C1); `pkg/controlplane/runtime/shares/service.go`, `runtime.go`, `internal/adapter/{nfs,smb}` handlers (C2, C5, C7); `pkg/adapter/nfs/drc.go` (C3); `pkg/adapter/smb/connection.go`, `internal/adapter/pool/bufpool.go` (C4, C8); `internal/adapter/nfs/v3/handlers/doc.go` (C6).
- **Wall A:** `pkg/block/local/fs/{appendlog,appendwrite,logindex,readpayload,recovery,rollup,compaction,eviction}.go`, `pkg/block/local/logblob/manager.go` (delete), `pkg/block/block_record.go`, `pkg/block/engine/{carver,syncer}.go`, `pkg/controlplane/runtime/shares/service.go` (factory).
- **Wall B:** `pkg/metadata/store/badger/{store,commit_leader}.go`.
- **Reuse:** `blockstoretest` conformance suites (property gate), `metadata.NewMemoryMetadataStoreWithDefaults`, existing `syncLeader`/`sync_leader.go`, `pkg/identity` TTL cache, `internal/adapter/pool`.

## Verification

- **Every PR:** `go build ./...`; `go vet`; `gofmt -s -l`; `go test ./...` + `-race` on touched
  packages; simplifier→reviewer; Copilot/CI green; squash-merge.
- **Wall C:** micro-benchmark the contended path (e.g. concurrent `UpdateActivity`,
  store-resolution) before/after; confirm no auth regression via the storetest + SMB-ACL
  fidelity suites; watch NFS/SMB e2e latency.
- **Wall A:** `BlockStoreConformance` + `BlockStoreAppendConformance` must stay green (property
  preservation = dedup, durability, POSIX overwrite/hole, eviction). VM-free A/B on the fixed
  `BenchmarkSequentialWrite8MB` for write-count deltas; VM `dfsbench` seq-write + warm/cold
  read cells for absolute MB/s vs the Phase-0 baseline and competitors; e2e NFS/SMB
  write→read round-trip; kill-after-COMMIT durability + repack-crash tests for A4/A6.
- **Wall B:** Linux A/B on one SCW VM (binary-swap), metadata cell before/after; target beats
  ZeroFS (354 ops/s).

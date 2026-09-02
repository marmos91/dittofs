# pier — target design for extracting `pkg/block/journal` as a library

Status: **DESIGN / NOT APPROVED.** No code written. The audit that was still running when this
was drafted is **complete**, as are the sibling audits of `pkg/block/engine` and the `pkg/block`
root. All three are folded into the master plan's §12 — read that alongside §5 here.

Baseline commit: `ff14b24cb` (origin/develop). Audited in an immutable detached worktree at that commit.

Naming: **DECIDED — `pier`.** See §11.1 for the rationale and the vocabulary it gives us.

---

## 1. The job

A write-back cache for object storage absorbs an impedance mismatch on four axes:

| Client wants | Object store offers | Cache must supply |
|---|---|---|
| byte-range writes | immutable whole-object PUT | accumulation + chunking |
| immediate ack | ~450ms round trip | durable local staging |
| low-latency reads | high-latency GET | residency + prefetch |
| unbounded working set | unbounded capacity | bounded disk + eviction |

The current package does all four. Nothing in this design changes that.

## 2. The one hard problem

DittoFS's field bug history for this component, over roughly a year:

| Issue | Symptom |
|---|---|
| #1850 | cold intervals held in memory only → zeros after any restart |
| #1879 / #1888 | silent zeros; the guard meant to catch them was unreachable |
| #2084 | `Invalidate` marked a live *dirty* record cold → zeros, nothing logged |
| #1872 / #2073 / #2093 | reap classified rows by start offset → zeros, or resurrected old bytes |
| #1956 | crane dropped deduped chunks' manifest rows → zeros |
| #2110 | offline-readiness reported `Safe()` for a *lost* interval |

Every one is the same defect: **the cache's map of what it holds disagreed with what it
actually held, and the disagreement rendered as zeros.** None is a throughput, eviction-policy
or chunking bug.

The design goal that follows: make that bug class *unrepresentable*, not merely detectable.
Zeros are the correct answer for a hole and catastrophic for lost data, and the two are
byte-identical — so the distinction has to live in the type system, not in reviewer attention.

## 3. The model: five states, and `Lost` is one of them

```go
type State uint8

const (
    StateAbsent   State = iota // never written — zeros are correct
    StateDirty                 // local only; no other copy exists
    StateResident              // local AND durable remotely; free to reclaim
    StateRemote                // durable remotely only; fetch to read
    StateLost                  // written; no copy anywhere; reads MUST fail
)

type Extent struct {
    Off, Len int64
    State    State
    Durable  bool // survives device loss *now* (fsynced, or Resident/Remote)
}
```

Four of these exist today as `hole` / `dirty` / `warm` / `cold`. **`StateLost` is the addition**,
and it is the whole point: it is the state the bugs above kept falling into with nowhere to go,
so they rendered as `StateAbsent`.

`Durable` is deliberately a separate bit from `State`. A `StateDirty` range that has been
fsynced survives a crash but still has no second copy. Collapsing those two questions is how
a `DurableExtent` over-reports.

### The transition table IS the specification

| Operation | Legal transition | The trap it closes |
|---|---|---|
| `WriteAt` | `*` → `Dirty` | — |
| `Flush` | `Dirty` → `Resident` | flip-before-commit (#1872 family) |
| `Evict` | `Resident` → `Remote` | evicting `Dirty` = data loss |
| `Fill` | `{Absent, Remote}` → `Resident` | overwriting newer local bytes |
| `Invalidate` | `Resident` → `Remote` | **`Dirty` → `Remote` is #2084 exactly** |
| `Seed` | `Absent` → `Remote` | seeding over live local bytes |
| `Compact` | must never produce `Lost` | #2084, #2093 |

`Fill` covering `Absent` is not a rounding error: `fileIndex.hydratable` (`index.go:509-544`)
returns genuine holes as well as cold ranges, and the `Absent` side is the one behind #1888
(cold seeding skipped unplaceable rows → silent zeros) and #1887. A transition table that omits
it is not yet a specification.

#2084 was `Invalidate` performing `Dirty → Remote` when the only truthful transition was
`Dirty → Lost` — there was no remote copy to fetch. The shipped fix made it refuse. Here that
refusal is the operation's definition rather than a guard someone remembered to add.

### Three rules

1. **Any transition that reduces residency is durable before it takes effect.** The cold.log
   lesson (#1850), generalised. Today `Evict`, `Invalidate` and `Seed` each learned it
   independently and #2084 proves one had not. Target: a single internal `demote()` chokepoint
   that cannot run without first fsyncing its record.
2. **Exposure is exactly queryable.** *"If this disk died now, what is lost?"* — bytes, files,
   ETA. Answerable from the state map. `DurableExtent` already does the per-file half and is
   ahead of the field (JuiceFS's write-back docs warn about loss with no way to quantify it).
3. **In-flight uploads hold a pin.** A range being flushed must be pinned against evict and
   compact for the duration, crash-reconstructibly. `SetPinVersion` is the coarse form of this.

## 4. The surface

```go
// Package pier is a crash-safe local write-back cache for object storage.
//
// Bytes park in a container on the pier until a boat carries them out: a write
// lands locally and is acknowledged at once, Sync bolts it down so a crash
// cannot take it, and Flush puts it on the boat to the object store. A pier is
// a waiting place, never a destination — pier always knows which of its bytes
// have sailed, which have not, and which are gone.
package pier

type FileID string

// ── lifecycle ──
func Open(dir string, cfg Config) (*Cache, error)   // format check folded in
func (c *Cache) Close() error

// ── data plane ──
func (c *Cache) WriteAt(ctx context.Context, id FileID, off int64, p []byte) error
func (c *Cache) ReadAt(ctx context.Context, id FileID, off int64, dst []byte) (n int, fetch []Extent, err error)
func (c *Cache) Fill(ctx context.Context, id FileID, off int64, p []byte, notAfter uint64) error
func (c *Cache) Sync(ctx context.Context, id FileID) error

// ── queries ──
func (c *Cache) Size(ctx context.Context, id FileID) (int64, bool)
func (c *Cache) Extents(ctx context.Context, id FileID) ([]Extent, error)
func (c *Cache) DurableExtent(ctx context.Context, id FileID) (int64, bool) // NOT derivable — see 4.2
func (c *Cache) ColdExtents(ctx context.Context) (bytes, extents int64, err error) // store-wide
func (c *Cache) Files(ctx context.Context) []FileID
func (c *Cache) Stats() Stats

// ── transitions ──
func (c *Cache) Flush(ctx context.Context, id FileID, opts FlushOptions,
        fn func(context.Context, Run) (durable []Extent, err error)) error
func (c *Cache) Evict(ctx context.Context, targetBytes int64) (Reclaimed, error)
func (c *Cache) Invalidate(ctx context.Context, id FileID, off, length int64) error
func (c *Cache) Seed(ctx context.Context, seeds []Seed) error
func (c *Cache) Compact(ctx context.Context, opts CompactOptions) (Reclaimed, error)

// ── mutation ──
func (c *Cache) Delete(ctx context.Context, id FileID) error
func (c *Cache) Truncate(ctx context.Context, id FileID, size int64) error

// ── versions ──
func (c *Cache) Version() uint64
func (c *Cache) Pin(v uint64)
func (c *Cache) Restore(ctx context.Context, v uint64) error

// ── control ──
func (c *Cache) SetEvictionEnabled(bool)
func (c *Cache) MarkSeeded() error

// ── errors: a closed set, each implying a different caller action ──
var ErrFull   = errors.New("pier: local capacity exhausted, all segments pinned dirty")
var ErrLost   = errors.New("pier: range has no surviving copy")
var ErrClosed = errors.New("pier: closed")

type CorruptError struct{ ID FileID; Off, Len int64 } // healable: invalidate + refetch
```

### `Flush` — the seam (v5)

Four earlier drafts were rejected. §4.3 records why, so they are not re-walked.

```go
func (c *Cache) Flush(ctx context.Context, id FileID, opts FlushOptions,
        fn func(ctx context.Context, r Run) (durable []Extent, err error)) error

// Run is one offer: a contiguous dirty region of one file, plus its bytes.
// It is NOT a block. pier does not know what a block is — that is the whole
// point of being chunk-agnostic. crane turns Runs into Blocks.
type Run struct {
    ID          FileID
    Extent      Extent   // the dirty region being offered
    DurableTail []Extent // contiguous already-durable extents immediately after:
                         // BOTH Resident and Remote, because a clobbered row's owed
                         // range can straddle evicted bytes (carve.go:749 unions both)
    Final       bool     // last call for this file; drain anything buffered
    io.ReaderAt          // the run's bytes, addressed by file offset
}

type FlushOptions struct {
    MaxAge time.Duration // dirty-age eligibility gate
    MinSize int64        // dirty-bytes eligibility gate

    // AfterFile runs once per file after the last flip, WHILE pier still holds
    // the shard's flush lock. The manifest reap goes here. MANDATORY — see §4.2.
    AfterFile func(ctx context.Context, id FileID) error
}
```

**No `Concurrency` field.** All upload concurrency lives in ferry. pier is sequential.

### 4.1 The contract, stated completely

Everything below was implicit in v3.1 and is load-bearing.

**C1 — Call ordering.** `fn` is called **strictly sequentially** for one file: one run at a
time, ascending file offset, and call N+1 does not begin until call N returns. Block boundaries
depend on it (a block is closed by accumulated size, and the accumulator is `fn`'s), and so does
the reap's contiguous-prefix assumption (`carve_pack.go:16-24`).

**C2 — Locks held when `fn` runs.** pier holds the shard's `flushMu` (which serializes passes
for that shard) and **does NOT hold `sh.mu`**, the index lock. This mirrors today exactly:
`sh.mu` is taken and released in short sections (`carve.go:239-240`, `:275-285`) and the sink
call at `:449` runs outside it. `fn` performs network I/O; holding the index lock across it
would stall every read on the shard.

**C3 — Deferred credit.** `fn` may return an empty `durable` slice for any number of calls while
it buffers toward a block. It reports extents whose uploads have **since** completed on a later
call. Consequence, accepted: for a file with few runs but slow uploads, most flips arrive on the
`Final` call, which blocks until this file's in-flight uploads resolve.

**Credit is tracked per `Submit` call, never through a shared stream.** `ferry.Submit` returns a
future scoped to that one upload; `fn` waits on futures in submission order, exactly as
`journal/carve_dispatch.go:109-114` chains `prev`/`mine` today. A single shared
`Completions()` channel CANNOT work: Go delivers each message to exactly one receiver, and
`engine/carve_dispatch.go:91-127` already flushes many files concurrently, so file B's completion
would be delivered to file A's `fn` with no `FileID` in `Submit` and no demultiplexer anywhere.
That loses durability credit silently — the disease this design exists to cure, relocated to the
transport. See §7.3.

**C4 — Credit is validated per FRAGMENT, not per extent.** pier decomposes each returned
`(offset, length)` against its own retained snapshot of the fragments it offered, and flips
**fragment by fragment**, skipping any whose version no longer matches while still flipping every
sibling that does. This mirrors `flipUpTo` (`carve.go:844-865`), which already validates per live
interval via `fi.findRecord(iv.fileOff, iv.version)`.

A monolithic (offset, length, version) match on the whole returned extent would be WRONG, and
routinely so: `splitRuns` (`carve.go:364-375`) groups runs by file-offset adjacency only, never
by version, so one offered extent can already span differently-versioned fragments *before* any
race — and a concurrent partial overwrite of one byte (`clamp`, `index.go:53-67`, which preserves
version on untouched sub-ranges) splits it further. `fn` reports at block/chunk granularity, a
different partitioning again. Whole-extent matching would forfeit credit for an entire run
because one unrelated byte moved, on exactly the scattered-write workload this package is tuned
for (`carve.go:377-387`).

The version check itself is pier's own bookkeeping — `Extent` carries no version field and `fn`
never sees one. Skipped fragments are logged and stay dirty; the next pass's dedup collapses the
re-upload to a no-op, so this costs repeated CDC/hash work, not correctness.

**C5 — Partial credit with error.** `fn` may return a non-empty `durable` **together with** a
non-nil error: "these committed, then I failed." pier flips the validated extents, stops
offering runs for this file, and **still calls `AfterFile`** — because the committed prefix must
be reaped. `TestCarvePackSpanningBlockFailureReapsTheCommittedPrefix` pins exactly this. Then
`Flush` returns the error; remaining dirty ranges stay dirty for the next pass, where the dedup
oracle collapses any re-upload.

**C6 — Backpressure.** If ferry's window is full, `fn` blocks in `Submit`, which stalls pier's
sequential loop while `flushMu` is held. **Not a regression** — today's uploads run under
`carveMu` for the same duration — but it means a slow remote delays that shard's next flush
pass, not its reads or writes. `ctx` is the escape hatch.

**C7 — Cancellation.** On `ctx` cancellation pier stops offering runs, flips whatever was
already validated, and still calls `AfterFile`. A cancel between the last flip and the reap can
strand superseded rows — the same window today's `ponytail:` marker at `carve.go:335` documents.
Unchanged, and explicitly not made worse.

**C8 — Buffer lifetime, and NO extra copy.** `Run`'s `ReaderAt` is a **lazy pass-through to the
underlying record reads** — it does NOT pre-materialize into a pier-owned buffer. This is
deliberate and is the difference between two copies and three:

- today: record -> `buf` (pooled scratch, `carve.go:530`) -> `arena` (`copy`, `:589`). Two hops.
- pre-materializing would add record -> pier-buf -> fn-scratch -> crane arena. Three.
- lazy pass-through keeps it at two: `fn` reads straight into its own scratch, then crane copies
  accepted chunk bytes into its block arena.

`ReaderAt` is valid only for the duration of the `fn` call. An implementation needing the bytes
longer must copy them — the same contract `BlockSink.CommitBlock` states today
(`carve.go:100-103`).

**C9 — `fn` and its accumulator MUST be constructed fresh per `Flush` call.** pier cannot enforce
this and cannot detect a violation: it is chunk-agnostic, so it has no visibility into what `fn`
does with the bytes. **Caller obligation, stated because it is silent when broken.**
`engine/carve_dispatch.go:91-127` launches one goroutine per file, gated only by a *count*
limiter, so files on different shards flush truly concurrently. Today that is safe because
`packRuns` allocates its chunker (`carve.go:513`) and dispatcher (`:400`) fresh per call. In v5
that state is `crane.Boxer`, living in the engine's closure — and hoisting it to a
`Syncer`-lifetime field reads like harmless stateless config while actually interleaving two
files' bytes into one block. That is cross-file corruption, worse than anything in §2.

**C10 — One `Flush` per file at a time.** Guaranteed by `flushMu` being shard-scoped and
`shardFor(id)` deterministic, so a manual `SyncNow` racing the periodic pass blocks rather than
interleaving. Stated once here rather than left to be assembled from C1, C2 and §4.2.

### 4.2 `AfterFile` is mandatory

The reap cannot move outside the lock. `engine/carve_dispatch.go:33-58` runs a pass per file
every `UploadInterval` (default 2s), and manual `SyncNow` is a second trigger, so any pass
outlasting one tick is re-entered. Today only `sh.carveMu` holds passes apart, and it covers
BOTH the flip and the reap (`carve.go:274-360`). Release it at `Flush` return and:

1. `Flush(X)` packs, flips, returns; the caller's closure holds `spans`/`newOffsets`.
2. The next tick starts `Flush(X)` again — the lock that held passes apart is gone.
3. Pass 2 commits a fresh manifest row inside pass 1's about-to-be-reaped span.
4. Pass 1's delayed reap runs against a `newOffsets` captured before pass 2 existed, does not
   recognise pass 2's row as its own, and **deletes a load-bearing row.**

That is #1872/#2093 reintroduced by construction, on a timer. A caller-side lock is the wrong
alternative: it makes the engine invent a second per-shard scheme with no visibility into pier's
partitioning. Fold `maybeResetDirtyClock` (`carve.go:920-935`) into `AfterFile` too.

**`AfterFile` needs no parameters beyond the id**: `fn` and `AfterFile` share the caller's closure,
so the `spans`/`newOffsets` the reap needs are already in scope.

**`DurableTail` supplies residency only.** It answers "is anything durable past this run, and is
it local or remote" — the `anySyncedFrom`/`warmAt` half of `extendRunToRowEnd` (`carve.go:673-711`).
It does NOT answer where the manifest row ends: that is `ManifestRowEndAfter`
(`blocksink.go:190-213`), a metadata-store call pier will never have, so `fn` makes it directly.
And to re-chunk the tail, `fn` reads it via `pier.ReadAt` — `Run.ReaderAt` is scoped to
`Run.Extent`, not the tail. A reader would otherwise assume `DurableTail` is self-sufficient and
go looking for a manifest hook that will never exist.

### 4.3 Rejected drafts — do not re-walk

| Draft | Shape | Why it failed |
|---|---|---|
| v1 | one call per run, returns `consumed int64` | Cannot name bytes from an *earlier* call. `packRuns` does not reset `blockFirstRun` per run (`carve.go:442`, `:600-603`), so one block routinely spans runs. Forced either one small object per run, or the caller rebuilding the accumulator. |
| v2 | pier accumulates to `BlockSize`, calls `fn` per block | Impossible. `carve.go:598-601` closes a block only after a whole chunk is appended, so **block boundaries are always chunk boundaries** — and a chunk-agnostic pier cannot know where a block may legally end. |
| v3 | v4's shape, but the parameter was named `Block` and pier owned "bounded concurrency" | Two errors: `Block` collides with crane's output type and implies pier understands blocks, which contradicts the chunk-agnostic requirement; and pier-owned concurrency contradicts C1's sequential calling — concurrent calls race over one accumulator. |
| v4 | v5's shape, but ferry exposed one shared `Completions()` channel, C4 matched whole extents, C8 pre-materialized, and C9/C10 were unstated | `Completions()` is undemultiplexable: Go delivers each message to one receiver, `Submit` carries no `FileID`, and many files flush concurrently — file B's completion reaches file A's `fn`. Whole-extent matching forfeits credit whenever one unrelated byte moves, which is the common case. |

### 4.4 What still needs proving before code

1. **C3's latency.** Deferring most flips to `Final` on upload-bound files is accepted in theory.
   Measure it: dirty-byte residency and time-to-flip vs. today, on a scattered-write workload.
2. **C4's cost.** A per-extent index lookup replaces a cursor advance. Bounded by `sort.Search`
   like `findRecord`, it should be negligible; benchmark against `BenchmarkCarveScatteredPass`.
3. **The whole seam against the pinned tests.** `TestCarvePackFlipPlanWatermarks`,
   `TestCarvePackSpanningBlockFailureReapsTheCommittedPrefix`,
   `TestCarvePackSeamRunFailureLeavesSuffixDirty`, `TestCarveCommitStrictlyBeforeFlip` must all
   be expressible and passing. Port them **before** rewriting the implementation.


### Method-count delta

| | Today | Target |
|---|---|---|
| exported `*Store` methods | 31 | 22 |
| package funcs | 3 (`Open`, `CheckFormat`, `SystemClock`) | 1 (`Open`) |
| undeclared type-asserted interfaces | 3 | 0 |

Nothing is a lost capability:

- five queries → one (`FileSize`/`DataExtents`/`DurableExtent`/`ColdExtents`/warm-tail → `Extents`)
- `SeedCold` + `SeedColdBatch` → `Seed` (single file is a one-element slice)
- `SetVerifyReads` → `Config.VerifyReads` (set once at construction, never changes)
- `CheckFormat` → folded into `Open` (a guard a caller can forget is not a guard)
- crane moves out entirely (§5)
- dead API deleted (§6)

## 5. The split: subtraction, not partition

**Do not create two packages.** Move crane *up* into `pkg/block/engine`, where the manifest it
serves already lives. What remains in the package is the generic library.

Evidence this is cheap: every crane helper — `splitRuns`, `warmTail`, `anySyncedFrom`,
`syncedRanges`, `flipUpTo`, `flipRecordSynced`, `recordHasDirtyFragment`,
`maybeResetDirtyClock`, `runReader` — has **zero** uses outside `carve*.go`. They become the
body of `Flush` or move with crane.

Counter-evidence to respect: crane touches `fi.ivs` 28× and `sh.mu` 20×. It is *inside* the
index today, not a client of it. That is precisely why the seam must be a **callback**, not a
lease — a lease API would have to materialise all that state as values and invent a token to
fence what a held lock already fenced.

Stays in the library (generic): segments, records, interval index, recovery, group-commit
fsync + watermark, eviction, compaction, cold log, format stamp, version/pin/restore.

Moves to engine (DittoFS policy): FastCDC chunking, BLAKE3 hashing, dedup oracle, block
packing, every manifest call, and the cross-file scheduling that already lives in
`engine/carve_dispatch.go`.

Ordered work:

1. Delete the dead API (§6). Pure subtraction, no consumer changes.
2. Sever `internal/logger` → `Config.Logger *slog.Logger` (11 `Warn` sites, all advisory).
   **This is the hard extraction blocker** — Go forbids importing `internal/` from outside
   the module.
3. Replace `block.ErrFutureFormat` with a package-local sentinel (2 uses, `format.go`).
4. Collapse the five extent queries into `Extents`.
5. Invert crane into `Flush` + `Run.WarmTail`; move chunking/hashing/manifest to engine.
   `chunker` and `blake3` leave with it.
6. Rename to the agreed name; convert `FSStore`'s embed to a named field (see §6).
7. README, `Example_` tests, benchmark set, and the test suite closing §6's gaps.

After step 5 the package's only non-stdlib import is `golang.org/x/sys` (statfs, build-tagged),
at which point `git subtree split` is mechanical — **if** a second consumer ever justifies it.

### 5.0 Vocabulary — one word, one meaning

The family reads in order: **pier** stages -> **crane** cuts blocks -> **ferry** moves them ->
**dittofs** orchestrates and decides what any of it means.

#### Data flow — note the return path

```
WRITE                                                    ack ────┐
  NFS/SMB ──► pier.WriteAt ──► staged locally, StateDirty ───────┘

FLUSH  (dittofs schedules; pier offers, dittofs disposes)
  pier.Flush(id, fn) ──► offers dirty runs ──► fn:
                                                crane.Crane    cut blocks
                                                ferry.Upload  PUT
                                                dittofs        commit manifest rows
                          ◄── returns durable []Extent ──────────┘
  pier flips exactly those ──► StateResident        ← THE INVARIANT LIVES HERE

READ
  NFS/SMB ──► pier.ReadAt ──► (n, fetch []Extent)
                 fetch non-empty ──► dittofs resolves manifest
                                  ──► ferry.Fetch
                                  ──► pier.Fill ──► StateResident ──► re-read
```

**Nothing reaches `StateResident` except by pier being told what became durable.** That is why
`Flush` is a callback and not a pipeline stage: a linear `write -> crane -> sync` has nowhere to
put the acknowledgement, and dropping it is the entire #1872 / #2073 / #2093 family. The flow is
a loop, and the return edge is the crash-safety-critical one.

**Modules are a distribution decision; packages are a design decision.** Four `go.mod` files for
one team and one consumer is real overhead: CI matrices, version skew, cross-cutting fixes
needing two PRs (see #1872, one defect spanning journal and engine). Get the four PACKAGE
boundaries right in-tree now; promote to modules only when something is actually published.
`git subtree split` stays mechanical as long as each package's imports stay clean, which is
exactly what §8.1's import-graph test enforces.

`crane` today means chunking AND packing AND uploading AND manifest commit AND flipping records
— **541 occurrences across 60 non-test files**. The smear is visible in the type name
`CarveChunk`, which needs two words to name one thing.

| Term | Means exactly | Lives in |
|---|---|---|
| **write** | stage bytes locally and acknowledge the client | `pier.WriteAt` |
| **sync** | make durable HERE (fsync) | `pier.Sync` |
| **flush** | pier's pass: offer dirty runs, accept durability reports | `pier.Flush` |
| **chunk** | find one content-defined boundary | inside `crane` |
| **box** | group chunks into one BlockSize block | `crane.Box` |
| **put** / **get** | make durable THERE / bring it back | `ferry.Put` / `ferry.Get` |

`Box` is a verb; its output noun is **`Block`**, not `Box` — `crane.Box(...) []Block`. Never
introduce a `Box` type. pier's on-disk unit stays **segment**; only crane produces **blocks**.
"Container" is not a term of art in either package — it was ambiguous between the two.

`crane` is kept, not invented: **file boxing** is standard digital-forensics vocabulary for
extracting structured units from a raw byte blob with no metadata to guide you. That is exactly
library two's job. What changes is that it stops also meaning "the upload pass" — that is
`Flush` — so `CarveChunk` becomes `crane.Chunk`.

Renames implied: `CarveOptions`/`CarveResult` -> gone (`FlushOptions`, and the result is what
`fn` returns) - `SetCarveTargets` -> gone - `carveMu` -> `flushMu` - `CarveBlockSize` ->
`crane.Options.BlockSize` - `CarveMaxAge`/`CarveUploadConcurrency` -> `FlushOptions` -
engine `carvePass` -> `flushPass`.

### 5.1 Topology — four packages, promoted to modules only on publication

```
pier    — stages bytes locally, crash-safe. Knows nothing of chunking, hashing, uploads
          or manifests. Chunk-agnostic by requirement: a consumer may ingest blobs it
          never chunks.
          deps: stdlib + golang.org/x/sys

crane   — cuts blocks out of a blob: content-defined chunk boundaries plus accumulation
          into BlockSize blocks. OPTIONAL companion to pier.
          deps: stdlib

ferry   — moves blocks BOTH WAYS: Put (bounded/adaptive concurrency, retry, ORDERED
          COMPLETION REPORTING, backpressure, remote-health) and Get/GetRange, which is
          what feeds pier.Fill on a cold read. A ship sails away; a ferry returns — and
          the return trip is the cold-read path that has had three bugs in it.
          deps: stdlib (the remote itself is an injected interface)

          Named `ferry` (what lifts containers from the pier onto the ship) rather than
          `syncer`, because `pier.Sync` already means fsync. pier.Sync = durable HERE;
          ferry.Upload = durable THERE. No collision.

dittofs — policy only: dedup oracle, what a block means, manifest rows, scheduling.
          deps: pier + crane + ferry + pkg/metadata + pkg/block/remote
```

**Chunking is optional by construction, not by configuration.** `Flush` hands the caller a `Run`
— bytes plus an extent — and what becomes of those bytes is entirely the caller's. A consumer who
wants content-defined dedup imports `fastcdc`; a consumer who wants to `PUT` each run as one
object named by its offset imports nothing extra and writes four lines. Verified: after crane
moves out, pier's only chunker references are `Config.ChunkParams` (crane config, leaves with it)
and one arbitrary pool-sizing constant at `index.go:373` that becomes a local number.

**Why `fastcdc` is its own module rather than a pier subpackage or in-tree with crane:**

1. **Chunk boundaries are a compatibility surface.** Two pier users who chunk differently cannot
   share a dedup pool. Publishing the boundary definition alongside pier is what makes interop
   possible at all; withholding it guarantees fragmentation.
2. **Different compatibility guarantees, therefore different semver.** The masks are pinned
   forever — `params.go`'s `#1569` note records that changing them re-chunks all existing data.
   That makes it a *format*. pier is an API and will evolve. Coupling their versions forces a
   choice between freezing pier and breaking dedup.
3. **It is already a library in everything but distribution** — 216 non-test LOC, **zero
   non-stdlib imports**, 94.2% coverage, a clean contract (`Chunker`, `Params`, `Next`,
   `Validate`). The cheapest module in the tree to publish.
4. **It has a real differentiator.** `Next(data []byte, final bool) (int, bool)` is
   allocation-free and I/O-free — no `io.Reader` wrapper, the caller owns the buffers — which is
   why it composes with pooled arenas. Every published Go FastCDC alternative
   (`jotfs/fastcdc-go`, `tigerwill90/fastcdc`) wraps a reader and allocates.

**`ferry` resolves a structural oddity, it does not invent one.** Recon cleared
`pkg/block/engine/carve_dispatch.go` (parallelises across files) and
`pkg/block/journal/carve_dispatch.go` (overlaps blocks within one file) as "complementary, not
duplicates". True but incurious: they are two halves of an upload scheduler that has no home.
`ferry` is that home, and merging them is the point.

**The dead `RemoteStore` is ferry's contract.** §6 records `journal.RemoteStore{PutBlock,
GetBlock, GetRange}` as entirely dead — never implemented, never read, the one production call
site passes `nil`. It was not a fossil of a design that was abandoned. It was **in the wrong
package**: it is precisely the interface `ferry` injects. Finding its true owner is corroboration
for this topology, not a coincidence.

**Ordered completion is why ferry is separate rather than folded into crane.** pier can only flip
in watermark order if something reports what completed and in what sequence. That reporting is
ferry's contract with pier, and it is the reason the two carve_dispatch files exist at all.

**The crane is not a library and must never become one.** What to upload, when, how to pack it,
and how to reconcile the manifest are all DittoFS decisions. Crane is the thing being *removed*
from the library, not extracted beside it.

**Naming asymmetry, deliberate.** `pier` is coined because it is a novel component nobody
searches for by algorithm — the name's job is to be memorable and unclaimed. `fastcdc` should be
**descriptive**, because people searching for this will type "go fastcdc" and a themed name
(`crate`, `bale`, `stow`, `parcel`) would be invisible to them. Discoverability wins where the
thing is a known algorithm; distinctiveness wins where it is not.

## 6. Code and folder structure

### 6.1 pier stays ONE Go package — a justified exception

CLAUDE.md prefers "subpackage over underscore filenames". pier is the exception, and the reason
is mechanical: `Store` methods touch `shard.mu`, `fileIndex.ivs` and `segmentMeta.fd` throughout
(`carve.go` alone: 28 `fi.ivs`, 20 `sh.mu`). A `segment/`, `index/` or `cold/` subpackage forces
exporting every one of those internals, which is strictly worse than the file-length problem it
would solve. Only code that touches NO `Store` state may be a subpackage — record framing and
the statfs shims qualify; nothing else does.

Navigability therefore comes from file naming, under one rule:

> **A file named for a TYPE contains that type and its methods.
> A file named for a VERB contains that code path, end to end.**

Today this is violated where it matters most: `segment.go:271` holds `appendRecord` — the entire
write hot path — and `index.go:295` holds `ReadAt`. A reader asking "how does a write land on
disk" finds it in no file whose name suggests writing.

### 6.2 Target layout

```
pier/
  doc.go            the metaphor, the state model, the invariants
  pier.go           Cache, Config, Stats, State, Extent, errors — the public vocabulary
  open.go           Open, Close, background loops
  write.go          WriteAt, Fill, appendRecord, tombstone + truncate markers   [was segment.go]
  read.go           ReadAt, verifiedRead, Size, Extents, DurableExtent          [was index.go]
  index.go          fileIndex, interval, insert, plan — ALGORITHM ONLY, no Store methods
  shard.go          shard, group commit, Sync
  segment.go        segmentMeta, create / seal / rotate — TYPE ONLY, no hot-path methods
  flush.go          Flush, run grouping, the fence, watermark-ordered flip  [was carve*.go x3]
  evict.go          Evict, ensureSpace, write-path backpressure              [was reclaim.go]
  compact.go        Compact, repack, victim selection                       [was reclaim.go]
  retire.go         the retirement tail evict and compact share             [was reclaim.go]
  cold.go           cold log, Invalidate, Seed, MarkSeeded, ColdExtents     [+ coldstats.go]
  recover.go        recoveryState and its phases
  restore.go        Version, Pin, Restore
  format.go         the on-disk format stamp
  internal/wire/    record framing codec — pure, no Store                   [was record.go]
  internal/fsutil/  statfs_unix.go, statfs_windows.go, fsyncDir
  piertest/         exported conformance suite (mirrors pkg/metadata/storetest)
  example_test.go   godoc-surfaced usage
  README.md

crane/
  doc.go            "FastCDC content-defined chunking" in line one, for searchability
  crane.go          Boxer, Options — cuts a blob into BlockSize blocks
  chunker.go        FastCDC boundary finding
  gear.go           the gear table
  params.go         Params, Validate, DefaultParams — THE FORMAT; masks pinned (#1569)
  README.md

ferry/
  doc.go
  ferry.go          Ferry, Options, Upload, ordered completion reporting
  window.go         adaptive concurrency window                    [was engine/carve_dispatch.go]
  retry.go          backoff; safe by construction — content-addressed PUTs are idempotent
  remote.go         the injected remote interface  [journal.RemoteStore, finally in the right package]
  README.md

dittofs/pkg/block/engine/
                    orchestration only: schedules Flush, wires crane + ferry,
                    owns the dedup oracle and the manifest.
```

### 6.3 What this closes

| Problem today | Closed by |
|---|---|
| `store.go` 1297 lines, 8-9 responsibilities | `pier.go` + `open.go` + `write.go` + `read.go` + `restore.go` |
| `reclaim.go` 999 lines, two policies + shared tail | `evict.go` + `compact.go` + `retire.go` |
| `carve.go` 995 lines, gocyclo 35 | `flush.go` (fence + flip only) — chunking and packing leave |
| `segment.go` / `index.go` hold the hot paths their names disown | `write.go` / `read.go` |
| `carve_pack.go` — 37 lines, no independent identity | merged into `flush.go` |
| `coldstats.go` vs `cold.go` — confusing adjacency, one method | merged into `cold.go` |
| Two files named `carve_dispatch.go` in different packages | one `ferry/window.go` |
| Six near-duplicate test store-openers | one options-taking harness in `piertest/` |

`recover.go` keeps its current shape deliberately: `recoveryState`'s phased-builder is the best
structure in the package and is the template for anything else here that grows multi-phase.

## 7. The three public interfaces

Each is designed to stand alone as its own OSS repository: no import of the others except
where stated, no host-app types in a signature, and a contract a stranger can implement against.

### 7.1 pier — the write-back cache over append blobs

Full surface in §4. One-line contract: **stages byte ranges for many blobs on local disk,
crash-safely, and always knows which of its bytes have sailed, which have not, and which are
gone.** Imports nothing from crane or ferry — it never learns what a block is.

### 7.2 crane — content-defined, content-addressed block assembly

Takes a pier-shaped append blob and carves content-addressed blocks out of it: FastCDC
boundaries, BLAKE3-256 identity, accumulation to `BlockSize`.

```go
package crane // FastCDC content-defined chunking with BLAKE3 content addressing

type Hash [32]byte // BLAKE3-256 of the chunk's plaintext

type Chunk struct {
    Offset int64 // logical offset in the source blob
    Size   int
    Hash   Hash
}

type Block struct {
    ID     Hash    // identity of the assembled block
    Chunks []Chunk // ascending, contiguous
    Bytes  []byte  // packed plaintext; EXCLUDES chunks Skip reported as already present
}

type Options struct {
    Params    Params // FastCDC sizing; zero value = DefaultParams
    BlockSize int64  // accumulate to at least this before emitting a Block
    // Skip reports that a chunk is already durable elsewhere, so its bytes need not be
    // packed. It still appears in Block.Chunks — the caller needs the record even when
    // the payload is redundant. Nil means pack everything.
    Skip func(Hash) (bool, error)
}

func New(o Options) *Boxer
// Box consumes data at off and returns whatever complete Blocks it can. It buffers
// across calls; final drains the remainder. Allocation-free on the boundary path.
func (b *Boxer) Box(data []byte, off int64, final bool) (blocks []Block, consumed int, err error)

type Params struct{ Min, Avg, Max int }
func DefaultParams() Params
func (p Params) Validate() error
```

**`Skip` is how dedup enters without the oracle entering.** DittoFS supplies a closure over its
synced-hash store; a standalone consumer passes nil. crane never learns what a metadata store is.

**`Params` is a FORMAT, not a tuning knob.** The masks are pinned (`#1569`: changing them
re-chunks all existing data). Semver it as a format: a `Params` change is a major version.

### 7.3 ferry — moving blocks to and from a block store

```go
package ferry // bounded, retried, order-reporting transport for immutable blocks

type BlockID string

// Store is the injected backend. S3 is one implementation; a filesystem, GCS, or an
// in-memory map are others. ferry imports no cloud SDK.
type Store interface {
    Put(ctx context.Context, id BlockID, r io.Reader, size int64) error
    Get(ctx context.Context, id BlockID) (io.ReadCloser, error)
    GetRange(ctx context.Context, id BlockID, off, n int64) (io.ReadCloser, error)
}

func New(s Store, o Options) *Ferry

// Put and Get are the synchronous forms.
func (f *Ferry) Put(ctx context.Context, id BlockID, data []byte) error
func (f *Ferry) Get(ctx context.Context, id BlockID, off, n int64) (io.ReadCloser, error)

// Submit queues an upload and returns a future for THAT CALL ONLY. Uploads overlap
// up to the adaptive window; the caller waits on futures in submission order, which
// is what lets pier flip its watermark safely.
//
// There is deliberately NO shared Completions() channel. Go delivers each message to
// exactly one receiver, Submit carries no FileID, and many files flush concurrently
// (engine/carve_dispatch.go:91-127) — so a shared stream routes one file's completion
// to another file's callback, silently losing durability credit. Per-call futures make
// that unrepresentable, and mirror journal/carve_dispatch.go:109-114's prev/mine chain.
func (f *Ferry) Submit(ctx context.Context, id BlockID, data []byte) (<-chan Completion, error)
type Completion struct { Err error }

// A window slot is released when the upload RESOLVES — success, exhausted retries, or
// cancellation — independent of whether anyone reads the returned future. Otherwise a
// caller blocked in Submit could never free the slot it is waiting for.

func (f *Ferry) Health() Health // drives pier.SetEvictionEnabled when the remote degrades

type Options struct {
    MaxConcurrent int           // 0 = adaptive
    MaxAttempts   int           // retry is safe by construction: content-addressed PUTs
    Backoff       func(int) time.Duration
}
```

**Ordered completion is why ferry is a module and not a helper.** pier can only flip in
watermark order if something reports what completed and in what sequence; that reporting is the
contract between them, and it is why two files named `carve_dispatch.go` exist today in two
different packages.

**ferry owns the interface that was dead in journal.** `journal.RemoteStore{PutBlock, GetBlock,
GetRange}` — never implemented, never read, `nil` at its one production call site — is
`ferry.Store`. It was not an abandoned design; it was in the wrong package.

### 7.4 What each may import

| | pier | crane | ferry |
|---|---|---|---|
| stdlib | yes | yes | yes |
| `golang.org/x/sys` | yes (statfs) | no | no |
| BLAKE3 | no | yes | no |
| the other two | **no** | **no** | **no** |
| any DittoFS package | **no** | **no** | **no** |
| any cloud SDK | no | no | **no** — `Store` is injected |

Enforced by the import-graph test in §8.1, in each repo. Only DittoFS imports all three.

## 8. Findings that motivate this (recon; audit verification pending)

**Dead exported API — delete:**

- `RemoteStore` and `BlockID` are **entirely dead**. `s.remote` is assigned in `Open`
  (`store.go:174`, `:288`) and *never read anywhere in the package*. The one production call
  passes `nil` (`local/fs/fs.go:165`). The only implementation in the tree is a test stub
  (`engine/blocksink_test.go:17`). The engine fetches remote bytes itself and pushes them in via
  `Hydrate` — **the real remote seam is already inbound-only**, which is why this design works.
- `PinVersion()` — zero callers anywhere, including tests.
- `SegmentLocation` — never referenced outside the package.
- `GC`/`GCOptions`/`GCResult` — no external caller; only the internal background loop.
  `engine/carve_dispatch.go:28` documents that the engine deliberately never calls it.

**Doc drift:**

- `doc.go:23` cites `gc.go`; no such file (the code is in `reclaim.go`).
- `reclaim.go:46` cites `logblob.EvictBlob`; that package was removed in the journal switchover.
- `doc.go:6-9` claims dependence on "the standard library plus a pair of narrow injected
  interfaces (RemoteStore, Clock)". Actual non-stdlib imports: `internal/logger`, `pkg/block`,
  `pkg/block/chunker`, `lukechampine.com/blake3`, `golang.org/x/sys` — and **one of the named
  pair was never implemented**, while the interfaces that carry the design (`Deduper`,
  `BlockSink`, plus three type-asserted ones) go unmentioned.

**Untested exported API — zero call sites across all 29 test files:**
`RestoreToVersion` (gocyclo 37, the snapshot-restore primitive, longest doc comment in the
package), `CheckFormat` (the format-refusal guard), `JournalVersion`, `PinVersion`,
`SetPinVersion`, `FileCount`. Package coverage is 79.2% and hides all six.

**Surface leak:** `local/fs/fs.go:68` embeds `*journal.Store` rather than holding it as a
field, with a first-party `ponytail:` comment (`fs.go:57-63`) naming the leak as a deferred
hazard. `local.LocalStore` declares 18 methods; `*Store` exposes 31; `pkg/block/engine` reaches
past the interface to the concrete type for `SeedColdBatch`, `RestoreToVersion`,
`SetPinVersion` via unexported structural interfaces that **silently no-op if the assertion
fails**. Two contracts exist, one declared and one informal.

**Not findings (checked and cleared):**

- `engine/carve_dispatch.go` vs `journal/carve_dispatch.go` are complementary, not duplicates:
  the former parallelises across files, the latter overlaps blocks within one file. Only the
  filename collides.
- `index.go:203` `appendAssign` (gocritic) is a false positive — `survivors` starts nil and is
  only appended to locally.
- The 13 `ponytail:` markers are a sanctioned debt ledger per CLAUDE.md, not TODOs.

## 9. Standalone test contract

**Requirement: `pier`'s test suite must pass with zero imports outside stdlib + `golang.org/x/sys`.**
Not as a policy — as a test.

### 8.1 The enforcing mechanism

```go
// TestNoForeignImports fails if pier's non-test OR test files import anything
// outside the standard library and golang.org/x/sys.
func TestNoForeignImports(t *testing.T) {
    // go/build is STDLIB and reports Imports + TestImports for a directory.
    // Deliberately not go/packages: that lives in golang.org/x/tools, which is
    // exactly what this test forbids — it would fail itself.
    pkg, err := build.ImportDir(".", 0)
    ...
}
```

This is the whole point. `doc.go`'s "depends only on the standard library plus a pair of narrow
injected interfaces" drifted to false — five non-stdlib imports and one of the named pair never
implemented — precisely because **nothing checked it**. A prose claim about the dependency graph
rots; an assertion about it cannot. Everything else in this section is downstream of that test
passing.

Today's violations, all removable:

| Import | Where | Fix |
|---|---|---|
| `internal/logger` | `cold.go`, `reclaim.go`, `recovery.go`, `store.go` (11 `Warn` sites) | `Config.Logger *slog.Logger`, nil = discard |
| `pkg/block` | `format.go` (`ErrFutureFormat`, 2 uses) | package-local sentinel |
| `pkg/block/chunker` | `carve.go`, `store.go`, `index.go:373` | leaves with crane; `index.go:373` becomes a local const |
| `lukechampine.com/blake3` | `carve.go:558` (one call) | leaves with crane |
| `badgerstore` (**test**) | `carve_pack_test.go:11`, used only by `BenchmarkCarveScatteredPass` | latency-injecting fake `Deduper`; the real-Badger measurement belongs in DittoFS's suite, not the library's |

### 8.2 Coverage gaps to close

Six exported methods have **zero** test call sites across all 29 test files — package coverage
of 79.2% hides every one:

`RestoreToVersion` (gocyclo 37, snapshot-restore primitive, longest doc comment in the package),
`CheckFormat` (the format-refusal guard — a guard that has never refused anything is unverified,
not proven), `JournalVersion`, `PinVersion`, `SetPinVersion`, `FileCount`.

`Restore` is the priority: an audit finder flagged it as blind to `cold.log`, deleting
remote-durable files and zero-filling cold ranges. Pending verification, but it is exactly the
`StateRemote`-read-as-`StateAbsent` confusion this design exists to prevent.

**Every new regression test must be run against a deliberately broken build before it is
trusted.** A test that passes on unmodified code proves nothing about the bug it claims to pin.

### 8.3 What a standalone suite needs that this one lacks

1. **One canonical harness.** Six near-duplicate store-openers exist today — `testStore`,
   `carveStore`, `evictStore`, `ctrlStore`, `openDirtyExpireStore`, `benchStore` — each
   re-implementing `Open(t.TempDir(), cfg, fakes) + Cleanup`. Replace with one options-taking
   helper.
2. **`piertest` conformance suite**, exported, mirroring this repo's own convention
   (`pkg/metadata/storetest`, `pkg/block/blockstoretest`). Lets a consumer validate a fake, and
   lets a second implementation exist at all.
3. **Property/model test for the interval index.** `fileIndex.insert`/`plan` is the algorithmic
   core; test it against a naive `[]byte` + `[]state` oracle under randomised
   write/overwrite/truncate/evict sequences. An audit finder reports the equal-`Version`
   tie-break diverging between `insert`'s fast path and its general path (`index.go:166`) — a
   model test finds that class automatically. Native `go test -fuzz` for the record and cold-log
   decoders.
4. **In-process crash injection.** The existing rigs (`test/crash/device-loss.sh`,
   `invalidate-cold-loss.sh`) are shell + `dm-flakey`, live outside the package, and need Linux
   and root. A library must be able to test torn writes, failed fsync and partial rename inside
   `go test` on any platform — which means an injectable file seam (interface or failpoint)
   rather than raw `os.File`. The hardware rigs stay in DittoFS as the belt to this suspenders.
5. **`Example_` functions.** None exist. At minimum: open/write/sync/read, a `Flush` loop, and
   handling `ErrFull`. These are the README's executable half.
6. **README.** None exists. Lead with `DurableExtent`-equivalent exposure accounting — it is the
   package's best idea and the thing the field does not have.

## 10. Benchmark contract

**One benchmark per state transition**, so the benchmark set and the transition table in §3 are
the same list. Today 11 benchmarks cover the write path well and nothing else.

| Transition | Today | Needed |
|---|---|---|
| `*` → `Dirty` (`WriteAt`) | `BenchmarkWriteAt`, `BenchmarkRandWrite4K`, `BenchmarkSeqWrite4K`, `BenchmarkRandWriteBounded` | keep |
| durability (`Sync`) | `BenchmarkConcurrentCommit1/32/128`, `BenchmarkTinyWritesCommit` | keep |
| `Dirty` → `Resident` (`Flush`) | `BenchmarkCarve`, `BenchmarkCarveScatteredPass` | de-Badger the second |
| warm read | `BenchmarkReadWarm` | keep |
| **cold read** | — | **add** |
| `Remote` → `Resident` (`Fill`) | — | **add** |
| `Resident` → `Remote` (`Evict`) | — | **add** |
| `Compact` / repack | — | **add** |
| recovery (`Open` on a populated dir) | — | **add — this is a startup-latency SLO** |
| `Truncate` / `Delete` | — | **add** |
| `Extents` (the merged query) | — | **add — it replaces five methods; prove it is not slower than the one it replaces on the hot path** |

Rules:

- **Every benchmark reports allocations.** Not one of the current 11 calls `b.ReportAllocs()`.
- **Keep the custom metrics.** `randwrite_bench_test.go` reports `write-amp` (on-disk bytes ÷
  payload bytes), `segments` and `intervals`. Write amplification is the number that decides
  whether this design is viable at all; it belongs on every write-path benchmark.
- **Baseline discipline.** A change ships only against a recorded before/after on the same
  hardware. Measurements taken inside Docker Desktop are not admissible — it inflates DB
  round-trips ~2.7x and has already produced one wrong headline number in this codebase.
- **`Extents` needs a defence.** Collapsing `FileSize`/`DataExtents`/`DurableExtent`/
  `ColdExtents` into one slice-returning query is the design's biggest performance risk:
  `FileSize` is on the hot read path (`engine/readwrite.go:40,53`) and `ColdExtents` is
  documented as O(live intervals) and deliberately avoids holding all shard locks at once.
  `Size` stays separate for exactly this reason; benchmark it and keep the receipts.

## 11. Open decisions

1. **Name — DECIDED: `pier`.** One syllable, unambiguous to spell, no collision anywhere in
   the tree, and it follows the coined-name convention of Go storage libraries (`bolt`,
   `badger`, `pebble`, `pogreb`, `moss`).

   **The metaphor, which is load-bearing:** bytes park in a *container* on the *pier* until a
   *boat* carries them out to the object store. A pier is definitionally a waiting place, never
   a destination — which is exactly the thing this component must never be confused about, and
   exactly what every bug in §2 got wrong. The metaphor is also bidirectional, matching the
   real data flow: goods stage on a pier before loading (`Flush`) and land on it after
   unloading (`Fill`).

   It also makes the `Sync`/`Flush` pair intuitive rather than merely conventional: `Sync`
   bolts the container down so a storm cannot take it (durable locally); `Flush` puts it on the
   boat (durable remotely). Two different guarantees, two different words, one picture.

   Rejected: `blockcache` — `block` is the most overloaded word in the tree (`pkg/block`,
   `block.Store`, `BlockSink`, `BlockID`, `blockcodec`), and a DB "block cache" is a
   fixed-size-page buffer pool, which this is not. `journal` — in filesystems that means a
   metadata write-ahead log for crash consistency; this is the data store itself, and the
   misnomer is part of why `GC` was read as touching remote refcounts. Also considered:
   `blobcache`, `rangecache`, `seglog`, `molo`, `rada`, `hopper`, `sluice`, `till`, `scree`.

   **TODO before any public release:** verify the module path is free on `pkg.go.dev` and the
   proxy. Not checked as of this writing.
2. **`Files()`** exists so the caller drives a per-file flush loop. A `Flush` that walks
   eligible files itself would delete it — but then the library owns scheduling policy, which
   `engine/carve_dispatch.go`'s upload window currently does well. Recommend keeping `Files()`.
3. **`Restore`.** "Rewind to LSN V" is generic, but it is ~180 lines, gocyclo 37, and has zero
   tests. Keep and test, or move out? Recommend keep + test. Note the S2 restore-to-zeros bug
   was exactly `Restore` leaving ranges `StateAbsent` that should have been `StateRemote` — a
   good validation case for the model.
4. **`Run` naming** — `Region` reads more clearly standalone; `Run` is the interval-literature
   term and matches existing code.
5. **Repo split: NOT NOW.** One consumer; fix history spans journal+engine (#1872 was one
   defect touching both — across a module boundary that is a two-PR release dance); and the DBS
   SDK reorg is about to move the thing on the other side of the remote seam. Trigger to
   revisit: a second real consumer.

## 12. Deliberately out of scope

Gaps identified against mature implementations of this component class, recorded so they are
not silently forgotten — none are prerequisites for the extraction:

- **Degraded write-through** when local disk is full/unhealthy (bypass cache, write straight to
  remote). Today: block, then `ErrFull`. This is the difference between degraded and down.
- **Continuous background scrub** re-verifying resident ranges against CRCs and demoting
  mismatches. Detector exists (#1882); #2076 records that it never runs on its own.
- **Content-addressed local tier** — knowingly given up in the switchover, which is why
  local-only clone is an O(n) copy rather than a refcount bump (#1795).
- **Fetch as a scheduled, coalescing, prioritised queue** rather than serial demand (#1625).
- **Preferential draining to empty whole segments of dirtiness**, so eviction always has
  candidates (the policy JuiceFS gets structurally by splitting `rawstaging/` from `raw/`).

## 13. What not to change

Shared append-only segments over file-per-object (the reason small-file workloads survive);
interval index with LSN newest-wins; group-commit fsync with a shared watermark (uncommon and
correct); segment-granularity eviction (imprecise but fragmentation-free); and `DurableExtent`,
which is the best idea in the package and the thing a README should lead with.

## 14. Repo-wide documentation update

The rename and the split are not done until the docs describe them. Every surface below
currently describes "the journal" and its "carve pass"; both words are being retired.

**Files to rewrite** (all confirmed to mention the journal today):

| File | What changes |
|---|---|
| `docs/internals/architecture.md:245-330` | The "Block Store — Local Journal Tier" section. It explicitly defers to the package doc comment as authoritative, so it inherits every drift — rewrite as the four-part model (pier / crane / ferry / orchestration) with the data-flow diagram from §5.0. |
| `docs/guide/durability.md` | fsync semantics and the loss window. Should now be able to state exposure exactly (`Sync` = durable here, `ferry.Put` = durable there) instead of describing it prose-wise. |
| `docs/guide/configuration.md:298-325` | Journal-as-substrate-of-`fs`, plus the vestigial pre-journal knobs it already flags (`use_append_log`, `rollup_workers`, `stabilization_ms`, `orphan_log_min_age_seconds`). Delete the vestigial ones or say plainly that they do nothing. |
| `docs/internals/implementing-stores.md:311-490` | Points implementers at `pkg/block/journal/` as the model to study. Repoint at pier, and at `piertest/` as the conformance suite they must pass. |
| `docs/guide/snapshots.md:505` | Snapshot/restore now rests on `pier.Version`/`Pin`/`Restore`. |
| `docs/guide/block-store-migration.md:22-31` | Tier vocabulary. |
| `docs/guide/faq.md` | Known limitations — add the §12 gaps (no degraded write-through, no background scrub) so they are stated rather than discovered. |
| `docs/BENCHMARKS.md` | Three journal mentions; repoint at the §10 benchmark set. |
| `docs/internals/contributing.md:298-307` | "run the journal package tests" -> the three libraries' suites plus the conformance suite. |
| `CLAUDE.md` | The Architecture-invariants section names the journal and its rules. Invariants 4, 5 and 6 change shape. |
| `README.md` | If the feature matrix or architecture summary names the journal. |

**Three READMEs that do not exist yet** — `pier/`, `crane/`, `ferry/`. Each is the front door of
a would-be OSS repo, so each needs: what it is in one sentence, the model, a runnable example,
the guarantees it makes, and the guarantees it explicitly does NOT make. pier's should lead with
exposure accounting (`DurableExtent`), which is its best idea and the thing the field lacks.

**Do NOT rewrite** `.planning/**` — those are historical records of decisions taken under the
old vocabulary, and rewriting them destroys the audit trail. `docs/guide/cli.md` is generated by
`cmd/gendocs`; change the Cobra definitions and regenerate, never hand-edit.

**Grep gate for "done":** `rg -i 'journal|carve' docs/ README.md CLAUDE.md` returns only
deliberate historical references. No user-facing config key or env var carries either word today
(verified: yaml/mapstructure tags are clean, `DITTOFS_JOURNAL_ASYNC_COMMIT` is already gone from
develop), so there is no breaking-change or deprecation story to write — this is docs and
internals only.

---

## PENDING — to fold into the master plan

Captured so they are not lost while the audit's verify gate and the second seam review complete.

### P1. Test and benchmark contracts apply to ALL THREE libraries

§9 and §10 currently specify pier only. Each library ships its own suite and its own benchmarks,
because each is a candidate OSS repo:

- **pier** — as specified in §9/§10.
- **crane** — correctness: boundary stability across buffer splits (the same blob chunked in one
  call vs. many must produce identical boundaries — this is the property that makes dedup work at
  all), `Params` validation, `Skip` behaviour, block accumulation to `BlockSize`, `final` drain.
  Benchmarks: throughput per MiB, allocations per block (target: zero on the boundary path),
  and boundary-shift resistance under insertion.
- **ferry** — correctness: ordered completion under concurrency, retry/backoff, partial failure,
  cancellation, health transitions. Benchmarks: throughput vs. window size, latency
  distribution, and behaviour under an injected-latency `Store` (no cloud SDK in the test path).

Each repo gets its own import-graph test per §8.1.

### P2. Legacy cleanup across the whole data flow

Requirement: after the refactor lands in-tree, remove **all** legacy code around the data flow —
not just what the refactor touches.

Measured scope: **339 `legacy`/`Legacy` mentions in non-test `pkg/block/**`**, most of it NOT in
the journal. Known clusters, to be inventoried properly before sequencing:

- `pkg/block/legacy_cas.go` — migration-only standalone-CAS layout; write/read paths stopped
  using it at #1493. Consumers are the remote backends' `legacy_cas_migration.go` accessors.
- `pkg/block/local/fs/legacy_migrate.go` + `materializeLegacy`, called on **every** `WriteAt`
  and `ReadAt` in `fs.go` — a per-op check for a pre-journal layout.
- Vestigial config knobs already flagged in `docs/guide/configuration.md:298-325`:
  `use_append_log`, `rollup_workers`, `stabilization_ms`, `orphan_log_min_age_seconds`.
- `block.Locator`'s "legacy standalone form"; `block.ContentHash`'s two legacy JSON encodings;
  `FileChunk`'s multi-row-per-hash tolerance for legacy data.
- The `cas -> blocks` migration repacker referenced from `engine/carve_dispatch.go`.
- The dead exported API in §8 (`RemoteStore`, `BlockID`, `PinVersion`, `SegmentLocation`, `GC`).
- The `FSStore` embed leak and the informal structural interfaces in `engine/flush.go`
  (`restorer`, `pinner`, `versioner`, `coldSeeder`) and `shares/legacy_verify.go`
  (`coldSeedTracker`) — two contracts where there should be one.

**Sequencing constraint:** legacy removal is a separate, independently-revertable workstream from
the refactor. Removing a migration path is irreversible for anyone who has not yet migrated, so
each removal needs its own answer to "which release forces the migration, and what happens to a
store that skipped it". The repo's own release history (v0.18/v0.19 tagging off develop) is the
cautionary note. Do not bundle these into the refactor PRs.

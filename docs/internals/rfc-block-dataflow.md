# RFC: splitting the block data flow into pier, crane and ferry

**Status:** proposed, open for comment. Nothing here is built.
**Discussion:** https://github.com/marmos91/dittofs/discussions/2234
**Detail:** `.planning/2026-09-01-block-dataflow-MASTER-PLAN.md` (plan),
`.planning/2026-09-01-pier-library-design-PLAN.md` (design),
`.planning/2026-09-01-journal-audit-report.md`, `-engine-audit-report.md` and
`-block-root-audit-report.md` (196 verified findings across the three).

If you are about to work in `pkg/block/`, read this first — it describes where that code is
going, and retires two words (`journal`, `carve`) that currently mean several things each.

---

## 1. Why

Three audits — `pkg/block/journal`, `pkg/block/engine` and the `pkg/block` root, each 7 lenses
with an adversarial verify gate, 204 agents over 19,632 LOC — produced **196 findings: 7 HIGH,
23 MED, 117 LOW**. The bug count is not the interesting part. The shape is:

> **Every one of the journal audit's five distinct HIGH findings is a residency-truth failure**:
> the cache's map of what it holds disagreeing with what it actually holds, resolving either to
> zeros or to reclaiming live data. Not one is a throughput, eviction-policy or chunking bug.

(Six HIGH were filed against the journal, five of them distinct defects; the engine audit adds a
seventh, #2238, discussed below. Hence 7 HIGH in total but five in the claim above.)

That is also the signature of the entire prior field history — #1850, #1879, #1888, #2084,
#1872/#2073/#2093, #1956, #2110. Ten incidents, one defect class, over a year.

The five journal HIGHs are filed as #2227, #2228, #2229, #2230, #2231.

The engine audit added a sixth, **#2238**, which extends the class rather than repeating it: a
new file dedups onto a past-grace orphaned hash, writing only a manifest row that the GC's
already-taken mark snapshot cannot see, and the sweep frees the only durable copy. Where the
journal's five are disagreements *at rest*, this is a time-of-check/time-of-use race — every view
individually correct, read at different instants. **Making the states explicit is necessary and
not sufficient; the model has to pin transitions under concurrency too.**

One caveat worth stating plainly rather than burying: the extraction moves roughly 500 of
engine's 11,304 LOC, and #2238 lives in `gc_sweep_index.go`, which does not move. GC, reclaim,
manifest check/repair and cold-read resolution all stay. The split is still worth doing, but it
does not by itself reach the newest instance of the class that justifies it — which is why that
work is sequenced *before* the API boundary hardens.

Three of the five live in `RestoreToVersion` — a 180-line, gocyclo-37 method with **zero test
call sites** across all 29 test files. Package coverage is 79.2% and hides that completely.

## 2. The core idea

Today a byte range is described by three booleans (`synced`, `cold`, plus "no interval at all").
The bugs happen in the gaps between them: a range that is *gone* renders identically to a range
that was *never written*, because both come back as zeros.

Make the states explicit and total, and add the one that has no representation today:

```go
type State uint8
const (
    StateAbsent   State = iota // never written — zeros are CORRECT
    StateDirty                 // local only; no other copy exists
    StateResident              // local AND durable remotely; free to reclaim
    StateRemote                // durable remotely only; fetch to read
    StateLost                  // written; no copy anywhere; reads MUST fail
)
```

`StateLost` is the point. It is the state the bugs kept falling into with nowhere to go, so they
rendered it as `StateAbsent` and served zeros.

Then every operation is a transition, and the transition table is the specification:

| Operation | Legal transition | The trap it closes |
|---|---|---|
| `WriteAt` | `*` → `Dirty` | — |
| `Flush` | `Dirty` → `Resident` | flip-before-commit (#1872 family) |
| `Evict` | `Resident` → `Remote` | evicting `Dirty` is data loss (#2228) |
| `Fill` | `{Absent, Remote}` → `Resident` | overwriting newer local bytes |
| `Invalidate` | `Resident` → `Remote` | `Dirty` → `Remote` is #2084 exactly |
| `Seed` | `Absent` → `Remote` | seeding over live local bytes |
| `Compact` | must never produce `Lost` | #2084, #2093 |

#2084 was `Invalidate` performing `Dirty → Remote` when the only truthful transition was
`Dirty → Lost`. The shipped fix made it refuse. Here that refusal is the operation's definition,
not a guard someone remembered to add.

## 3. Three libraries

```
pier    — stages bytes locally, crash-safe. Chunk-agnostic: you can ingest blobs you
          never chunk. deps: stdlib + golang.org/x/sys

crane   — cuts content-addressed blocks out of a blob: FastCDC boundaries, BLAKE3
          identity, accumulation to BlockSize. OPTIONAL. deps: stdlib

ferry   — moves blocks BOTH WAYS: Put (bounded concurrency, retry, ordered completion
          reporting) and Get/GetRange, which feeds pier.Fill on a cold read.
          deps: stdlib — the block store itself is injected

dittofs — orchestration and policy: dedup oracle, manifest rows, scheduling.
```

Data flow — **note the return edge, which is the crash-safety-critical one:**

```
WRITE                                                    ack ────┐
  NFS/SMB ──► pier.WriteAt ──► staged locally, StateDirty ───────┘

FLUSH  (dittofs schedules; pier offers, dittofs disposes)
  pier.Flush(id, fn) ──► offers dirty runs ──► fn:
                                                crane.Box   cut blocks
                                                ferry.Put   upload
                                                dittofs     commit manifest rows
                          ◄── returns durable []Extent ──────────┘
  pier flips exactly those ──► StateResident        ← THE INVARIANT LIVES HERE

READ
  NFS/SMB ──► pier.ReadAt ──► (n, fetch []Extent)
                 fetch non-empty ──► dittofs resolves manifest
                                  ──► ferry.Get
                                  ──► pier.Fill ──► StateResident ──► re-read
```

Nothing reaches `StateResident` except by pier being **told** what became durable. `Flush` is a
callback rather than a pipeline stage because a linear `write → carve → sync` has nowhere to put
that acknowledgement — and dropping it is the #1872 family.

## 4. Vocabulary — one word, one meaning

`carve` currently means chunking AND packing AND uploading AND manifest commit AND flipping
records: **541 occurrences across 60 non-test files**. The smear is visible in the type name
`CarveChunk`, which needs two words to say one thing. It is retired, and nothing inherits it.

| Term | Means exactly |
|---|---|
| **write** | stage bytes locally and acknowledge the client |
| **sync** | make durable HERE (fsync) |
| **flush** | pier's pass: offer dirty runs, accept durability reports |
| **chunk** | find one content-defined boundary |
| **box** | group chunks into one block |
| **put** / **get** | make durable THERE / bring it back |

`journal` goes too: in filesystems that word means a metadata write-ahead log for crash
consistency. This is the data store itself, and the misnomer is part of why `GC` was read as
touching remote refcounts (it never did — it is now `Compact`).

## 5. Why libraries, and why not separate repos yet

Designing to the external bar is the forcing function: it is what makes the manifest semantics
currently hidden in three type-asserted interfaces move to the side that owns them. Each library
gets a clean public API, its own tests, its own benchmarks, and an **import-graph test** that
fails if it ever reaches back into DittoFS.

But they stay in this repo for now. One consumer; fix history that spans journal *and* engine
(#1872 was one defect touching both — across a module boundary that is a two-PR release dance);
and the block-store SDK on the other side of the remote seam is about to move. `git subtree
split` stays mechanical as long as the import-graph test passes, so the option costs nothing to
hold.

**DittoFS will use only the libraries' public APIs.** Today there are two contracts: the declared
`local.LocalStore` (18 methods) and an informal one reached by embedding `*journal.Store` and by
unexported structural interfaces (`restorer`, `pinner`, `versioner`, `coldSeeder`,
`coldRangeReporter`, `coldSeedTracker`). Those assertions **no-op silently on failure** — a
rename inside pier would quietly disable snapshot pinning rather than break the build. After the
split, only the construction site may name the concrete type; everything else holds a declared
interface, so a missing capability is a compile error.

## 6. Plan

**Order: ferry → crane → pier**, reverse of the data flow. Steps 1 and 2 are
behaviour-preserving — no format change, no state-model change — so all semantic risk lands in
step 3, taken last.

| Step | What | Risk |
|---|---|---|
| **0** | Fix #2227-#2231 and #2238 with regression tests first | live bugs, unblocks everything |
| **1** | ferry — upload transport, ordered completion | behaviour-preserving |
| **2** | crane — chunking + block assembly | behaviour-preserving |
| **3** | pier — the seam inversion and state model | the real change |
| **4** | legacy cleanup across the data flow | separate workstream |

Green bar for every step: unit + race + E2E green, **crash rigs green on both the old and the new
build** (a rig passing only on the new build proves nothing), benchmarks recorded before/after on
the same hardware, and every new regression test verified against a deliberately broken build
before it is trusted.

## 7. Where to push back

The parts most likely to be wrong, and where comment is most useful:

1. **The `Flush` callback seam** (§4 of the design plan). It has already been redesigned twice
   under adversarial review — v1 could not express blocks spanning multiple dirty runs; v2 put
   block accumulation in pier, which is impossible because block boundaries are chunk boundaries
   and pier is chunk-agnostic. v3.1 is the current form. It may still be wrong.
2. **Collapsing five extent queries into `Extents()`.** `Size` and `DurableExtent` were pulled
   back out after review showed the durable-frontier walk is not derivable from a flat slice.
   The remainder still needs a benchmark to defend it.
3. **Whether `RestoreToVersion` belongs in pier at all** — 180 lines, gocyclo 37, zero tests,
   three of the five HIGH findings.
4. **Anything in the 117 LOW findings** you think is actually a MED or HIGH.

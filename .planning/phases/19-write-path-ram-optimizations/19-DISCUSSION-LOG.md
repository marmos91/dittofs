# Phase 19 Discussion Log

**Phase:** 19 — Write-path RAM optimizations
**Date:** 2026-05-21
**Mode:** discuss (default, all areas selected)
**GH issue:** #519

---

## Areas Discussed (8 total)

### 1. PR shape
**Q:** Phase 19 ships as how many PRs?
**Options:** mega-PR (match 17/18) / 4 atomic per-opt PRs / 2-PR hybrid (opts 1+4, opts 2+3)
**Selected:** mega-PR
**Captured as:** D-01

### 2. Opt 1 — In-memory hash dedup LRU
**Q1:** LRU scope?
**Options:** Global (per-FSStore) / Per-share / Per-payload
**Selected:** asked for recommendation → recommended per-FSStore (= per-share by architecture invariant); user accepted
**Captured as:** D-02

**Q2:** LRU size — fixed vs config knob?
**Options:** Config knob default 4096 / Fixed const / Adaptive
**Selected:** Config knob default 4096
**Captured as:** D-03

**Q3:** Hit refcount path?
**Options:** IncrementRefCount(hash) / Put idempotent / new AddRef surface
**Selected:** new `FileBlockStore.AddRef(hash, payloadID, blockRef)` surface
**Captured as:** D-04

### 3. Opt 2 — Group commit / batched fsync
**Q1:** Group commit window policy?
**Options:** Fixed 1ms + adaptive depth=1 bypass / window OR depth hybrid / config knob
**Selected:** Fixed 1ms + adaptive depth=1 bypass
**Captured as:** D-06

**Q2:** Fsync coalesce granularity?
**Options:** per-file log fsync / cross-file shared journal
**Selected:** per-file log fsync
**Captured as:** D-07

**Q3:** Backpressure shape?
**Options:** caller blocks (sync semantics) / async ack via channel
**Selected:** caller blocks
**Captured as:** D-08

### 4. Opt 3 — Direct-to-Cache on chunk completion
**Q1:** Cache push direction?
**Options:** callback via FSStoreOptions.OnChunkComplete / event channel / direct import
**Selected:** callback via FSStoreOptions.OnChunkComplete
**Captured as:** D-10

**Q2:** RAM ceiling on push?
**Options:** existing LRU bound / skip large chunks / skip under pressure
**Selected:** existing LRU bound (no extra cap)
**Captured as:** D-11

### 5. Opt 4 — Eager small-file dedup
**Q1:** Threshold?
**Options:** FastCDC min_chunk (1 MiB) / configurable / avg_chunk (4 MiB)
**Selected:** FastCDC min_chunk (1 MiB)
**Captured as:** D-13

**Q2:** Hook site?
**Options:** engine.Flush pre-rollup hook / adapter-side / WriteAt-side
**Selected:** engine.Flush pre-rollup hook (alongside trySpeculativeFileLevelDedup)
**Captured as:** D-14

**Q3:** RAM guard?
**Options:** bounded by concurrent Flush count / explicit semaphore
**Selected:** bounded by concurrent Flush count
**Captured as:** D-15

### 6. Bench / CI strategy
**Q1:** Per-opt benches gate-blocking vs yellow-flag?
**Options:** all gate / correctness gate + perf yellow / all yellow
**Selected:** correctness gate + perf yellow
**Captured as:** D-17

**Q2:** Cold-cache deferred bench folded in?
**Options:** stay deferred (per #519) / fold as advisory / fold as gate
**Selected:** stay deferred to v0.17+
**Captured as:** D-18

**Q3:** D-21 aggregate gate strictness?
**Options:** stay ≤1.02 / tighten to ≤1.00 / tighten to ≤0.85
**Selected:** tighten to ≤1.00 (no regression, win required)
**Captured as:** D-19

### 7. Code structure (added mid-discussion at user request)
**Q1:** New files for opt 1 + opt 2?
**Options:** dedicated dedup_lru.go + groupcommit.go / inline / combined ramopts.go
**Selected:** dedicated files
**Captured as:** D-22a

**Q2:** FileBlockStore.AddRef placement?
**Options:** existing interface / new sub-interface / free function
**Selected:** existing interface (FileBlockStore expands 6 → 7 methods)
**Captured as:** D-22b

**Q3:** Config knob namespace?
**Options:** blockstore.local.dedup_lru_size / blockstore.write_path.* / top-level
**Selected:** blockstore.local.dedup_lru_size
**Captured as:** D-22c

**Q4:** Group commit window knob now?
**Options:** no knob (const) / hidden const + TRANSITIONAL marker / surface knob
**Selected:** no knob (const groupCommitWindow = 1ms)
**Captured as:** D-22c

### 8. Deprecations (added mid-discussion at user request)
**Q1:** Cleanups to fold in (multiSelect)?
**Options:** Phase 18 ClaimBatchSize / dead LocalStore admin sweep / TRANSITIONAL marker sweep / nothing else
**Selected:** ClaimBatchSize + dead admin sweep + TRANSITIONAL marker sweep
**Captured as:** D-23, D-24, D-25

**Q2:** v0.17+ deferral markers?
**Options:** yes add at hook sites / no (spec covers) / yes only for opt-4 adjacent
**Selected:** yes add at hook sites
**Captured as:** D-26

**Q3:** BlockState audit?
**Options:** strict (LRU hit honors state machine) / allow skip-Pending / new DedupReference state
**Selected:** strict (LRU hit = AddRef only, no state transition)
**Captured as:** D-27

---

## Routing Issues Surfaced

- `gsd-sdk init.phase-op 19` returned the wrong phase dir (`.planning/milestones/v3.0-phases/19-session-lifecycle`) because multiple `19-*` dirs exist across milestones and the SDK matched the first one. Correct path for current work: `.planning/phases/19-write-path-ram-optimizations/`. Dir created manually before CONTEXT.md write.

---

## Deferred Ideas (preserved for v0.17+ or backlog)

- Pinned hot-tail RAM cache tier
- tmpfs spill for log overflow
- O_DIRECT for log writes
- zstd compression of chunks
- Speculative chunker lookahead
- Cold-cache read-path bench (`BenchmarkRandReadVerified_ColdCache`)
- Cross-share dedup LRU (covered by metadata `GetByHash` µs path; LRU per-share sufficient)
- Adaptive LRU sizing by available RAM
- Group commit window config knob (defer until bench data justifies)
- Cache push pressure signal
- Eager small-file semaphore (concurrent Flush count bounds RAM)

---

## Claude's Discretion (planner's call)

- Internal commit wave ordering within mega-PR
- LRU library choice (groupcache/lru vs hand-rolled stripe-locked)
- OnChunkComplete callback firing order vs lruTouch internal map update
- Bench `-race` mode handling (match existing `raceEnabled` pattern)
- `FileBlockStore.AddRef` error sentinel naming and file location
- D-24 audit result (no-op acceptable)
- D-25 per-marker target-version assignment

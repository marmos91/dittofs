# Blockstore Perf Bottleneck Remediation Plan (issue #829)

Execution plan for the HIGH bottlenecks found in B-H2 (REVIEW §5). Grounded in pprof + a code-explorer deep-dive (B1/B2/B3 mechanisms + constraints) + a code-architect blueprint. Targets the `v1.0-perf-blockstore` follow-up (#829), feeds Wave 2 stream 5.

## TL;DR — phasing

| Phase | Fix | Payoff | Risk | Independent? |
|---|---|---|---|---|
| **1a** | B2 pooled reconstruct buffer | kills 88.5 GB alloc_space (top heap) | LOW | yes — local to `local/fs` |
| **1b** | B3 Tier 1 caller-supplied scratch slice | kills 21 GB alloc_space | LOW | yes — 1 sig + 1 callsite |
| **2** | B1 in-memory pending-upload set | kills full CAS walk per Flush/tick (76.85% cum) | MED | after 1 (touches syncer + startup) |
| **3** | B3 Tier 2 fileOff interval index | kills O(n) scan (n≤250K) | MED | only if post-1b scan still hot |
| — | B4 GC churn | resolves once 1a+1b+2 land | — | re-profile after |
| — | B5 record-cap / harness seed | doc + harness fix, not prod bug | LOW | bench-only |

Do **Phase 1 first** (both items): biggest alloc wins, lowest risk, no interface churn beyond one signature. Phase 2 is the structural win but touches the crash-recovery path — needs the restart-reconciliation invariant preserved.

---

## B1 — Syncer rediscovers unsynced chunks by full CAS walk (HIGH, structural)

**Mechanism.** `engine.Store.Flush → Syncer.Flush → mirrorOnce (syncer.go:321) → local.ListUnsynced (blockstore_methods.go:228) → FSStore.Walk → filepath.WalkDir(blocks/)`. Fires on **every 2 s periodic tick AND every Flush** that wins the `uploading` gate. `ListUnsynced` = full dir walk → snapshot slice of ALL on-disk hashes → N×`IsSynced`. O(total chunks)/pass → quadratic over a flush-heavy run.

**Why the walk exists (hard constraint).** It is the **crash-recovery** mechanism: after a restart, the only way to find chunks written-to-`blocks/`-but-never-uploaded is `{disk} − {SyncedHashStore.IsSynced}`. `recoverStaleSyncing` only flips `Syncing→Pending` FileBlock rows; it does NOT enumerate orphaned CAS chunks. `SyncedHashStore` is presence-only (no enumerate). So the fix moves the walk from every-tick to once-at-startup, NOT removes it.

**Fix — in-memory pending set on `Syncer`.**
- New `pendingHashes map[ContentHash]struct{}` + `pendingMu` on `Syncer` (syncer.go). Per-share (each share owns its Syncer) — no global set.
- **Add point** = the single chunk-creation chokepoint `StoreChunk → onChunkComplete` (chunkstore.go:124, wired engine.go:221). Extend the callback (currently `bs.cache.Put` only) to also `bs.syncer.addPendingHash(hash)`. O(1) map insert.
- **Drain point** = `mirrorOnce`: snapshot the set under `pendingMu`, release, then per-hash run the existing **blake3 re-hash gate → remote.Put → MarkSynced**, and `delete` from the set **only after MarkSynced succeeds** (preserves Put-then-Mark + crash safety). Remove the `ListUnsynced`/Walk from the hot path.
- **Startup reconciliation** = new `FSStore.SeedPendingHashes(ctx, fn)` (the old walk + IsSynced filter), called once in `Syncer.Start` after `recoverStaleSyncing`, before `startPeriodicUploader`. Seeds the set from disk so post-crash orphans are found.
- **Drift reconciler** = keep the full walk as a slow background ticker (`PendingReconcileInterval`, default 10 min) for defense-in-depth against any future path that bypasses the callback. Not the 2 s hot path.
- **Dedup-hit/AddRef path** (rollup.go:403, no new file) needs **no change** — the chunk already existed and was tracked when first stored (or seeded at startup).
- **Eviction interaction**: if a pending hash was evicted (eviction only runs on already-synced chunks), `mirrorOnce`'s `Get` returns `ErrChunkNotFound` → log Debug + delete from set + continue (don't hard-fail).

**Files**: `engine/syncer.go` (fields, `addPendingHash`, `mirrorOnce`, `Start`, reconcile ticker in `periodicUploader`), `engine/engine.go:221` (callback), `local/fs/blockstore_methods.go` (`SeedPendingHashes`), new `engine/pending_seeder.go` (interface).

**Tests**: `syncer_test.go` add/drain cycle; crash-recovery (SeedPendingHashes → set populated); dedup-hit no-op; `go test -race` concurrent add+drain. Perf gate: `BenchmarkFlush_WithPendingSet` with 10K chunks — flush wall-clock O(pending) not O(disk); flush-churn workload via `cmd/bench blockstore` no longer quadratic.

**Risk callouts**: (1) map unbounded on persistent remote failure — bounded by local disk (= same as `blocks/` size); health circuit-breaker already skips drain when remote unhealthy. (2) startup `SeedPendingHashes` error (SyncedHashStore down) → Warn + 10-min reconciler retries; chunks safe on disk. (3) callback installed at construction (before traffic) → no add-gap window.

---

## B2 — `reconstructStream` full-extent buffer never pooled (HIGH, alloc)

**Mechanism.** `reconstructStream` (rollup.go:616) `buf := make([]byte, maxEnd)` every rollup pass; `maxEnd` = furthest record byte (tens–hundreds MiB). 88.5 GB alloc_space (sequential), dominant GC source.

**Why not streaming.** True streaming impossible: last-write-wins overlaps + sparse zero-fill gaps + FIX-3 (buffer anchored at file byte 0 for FastCDC boundary stability) + stateless chunker `Next(data []byte, final bool)` takes `[]byte`, not `io.Reader`. Answer = **pooled buffer**.

**Fix — channel-based bucketed pool** (mirror house pattern `blockBufPool`, block.go:10 — channel not `sync.Pool`, avoids MADV_DONTNEED churn on multi-MiB buffers).
- New `local/fs/reconstruct_pool.go`: `getReconstructBuf(size)` / `putReconstructBuf(buf)`. Two buckets (~64 MiB depth 8, ~512 MiB depth 4); fresh-alloc + no-pool above 512 MiB (don't hold 16 GiB idle). `clear(buf[:size])` on checkout (stale-byte safety — sparse gaps must be zero per FIX-3).
- `reconstructStream` calls `getReconstructBuf(maxEnd)`; `rollupFile` adds `defer putReconstructBuf(stream)`.
- **Safe to pool**: buffer used entirely inside `rollupFile` under the per-file mutex (rollup.go:169), not captured/escaped; chunk slices `stream[pos:]` consumed synchronously by `StoreChunk` before the defer fires at function tail.

**Files**: new `local/fs/reconstruct_pool.go`; `local/fs/rollup.go` (`reconstructStream`, `rollupFile`).

**Tests**: pool reuse (same backing array), clear-on-reuse (no stale bytes), `b.ReportAllocs()` → `allocs/op==0` on pool hit; existing rollup conformance/regression tests unchanged. pprof gate: `reconstructStream` alloc_space ~88 GB → ~0. **Backstop**: blake3 re-hash gate in `mirrorOnce` catches any pool-poisoning corruption before upload.

---

## B3 — `EntriesForInterval` per-call alloc + O(n) scan (HIGH, alloc+algo)

**Mechanism.** `EntriesForInterval` (logindex.go:144) `make([]logEntry,0,4)` + linear scan over ALL `idx.entries` (logPos-ordered, fileOff UNSORTED). Sole prod caller rollup.go:222 (result caller-local, no escape). n up to ~250K at 4 KiB writes (bounded maxLogBytes/writeSize). `idx.mu` held during scan blocks AppendWrite. 21 GB alloc_space (mixed-rw).

**Fix Tier 1 (do now, kills the alloc).** Change signature to caller-supplied scratch:
`EntriesForInterval(fileOff, length uint64, dst []logEntry) []logEntry` (append into `dst`, return). Caller passes a stack array `var scratch [32]logEntry; idx.EntriesForInterval(off, len, scratch[:0])`. logEntry = 3 ints, no pointers, no escape → safe. Update the test callsites (mechanical). Eliminates 21 GB alloc in the common case.

**Fix Tier 2 (deferred — only if scan cost still hot post-1b).** Auxiliary `entriesByFileOff []int` (indices into entries, sorted by fileOff) maintained insertion-sorted in `Append`; `EntriesForInterval` binary-searches the fileOff prefix then re-sorts the small result by logPos. Rebuild on `trimBelowFenceLocked`. Turns O(n)→O(log n + k). Bigger change (duplicate fileOffs from overwrites; must return ALL overlaps) — the existing `consumedCoverage` coverageSet answers existence only, insufficient. At 250K entries a value-struct linear scan is ~1 ms; gate Tier 2 on a real post-Tier-1 profile.

**Files**: `local/fs/logindex.go` (sig; Tier 2: field + `Append` + `trimBelowFenceLocked`), `local/fs/rollup.go:222` (callsite), `logindex_test.go` (callsites).

**Tests**: `testing.AllocsPerRun` → 0 allocs with pre-sized dst; Tier 2: binary-search output == linear-scan baseline on 10K random-fileOff entries; `BenchmarkEntriesForInterval_250K`; `go test -race` concurrent append+lookup.

---

## B5 — rollup 17 MiB record cap vs harness 64 MiB seed (MED, harness-realism)

`maxRecordPayload = 17 MiB` (appendlog.go:38) rejects single records ≥17 MiB at rollup-read. Production writes arrive ≤1 MiB chunked (NFS/SMB) → not a prod bug. Two cheap actions: (1) chunk the bench harness seed (`bench/blockstore` `seedAndOffsetCap`) into ≤1 MiB writes — also fixes harness gap H1 (ws≥4 pressure stall); (2) optionally fail the *write* fast when a single record would exceed the cap, rather than wedging *rollup* with an Error loop. Pure cleanup, no perf impact.

---

## Sequencing & acceptance

1. **Phase 1** (parallel-safe, one PR or two): B2 + B3 Tier 1. Re-run `cmd/bench blockstore` sequential/mixed-rw/flush-churn → assert alloc_space drop. `go test -race ./pkg/blockstore/...`.
2. **Phase 2**: B1. Re-run flush-churn (restore 20K ops) → wall-clock linear not quadratic; add/drain + crash-recovery race tests.
3. **Phase 3** (conditional): B3 Tier 2 only if profile shows scan still dominant.
4. **Harness**: B5 seed-chunking + gaps H1–H4 (read-only workload, gc pre-seed, block/mutex profile #671) folded into the perf-pass stream (#680).
5. **Final acceptance**: Scaleway macro-bench (>5% gate per PLAN), re-profile to confirm B4 GC churn cleared.

Per-area PR-B scope = none of these (structural) — all land under #829 as their own PR cycle (simplifier + reviewer + `-race` + verify per [[feedback_sim_review_before_pr]]).

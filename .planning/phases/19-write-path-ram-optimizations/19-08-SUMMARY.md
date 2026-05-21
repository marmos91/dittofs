---
phase: 19-write-path-ram-optimizations
plan: 08
subsystem: blockstore
tags: [blockstore, engine, dedup, fast-path, opt4, write-path]

requires:
  - phase: 19-write-path-ram-optimizations
    plan: 04
    provides: "applyFileLevelDedupHit shared finalize machinery (Plan 06 / pre-existing) — reused on hit"
  - phase: 19-write-path-ram-optimizations
    plan: 07
    provides: "OnChunkComplete cache wiring — D-16 MISS-path warm-after-write that the eager path inherits transparently"
provides:
  - "pkg/blockstore/engine/dedup.go — Syncer.tryEagerSmallFileDedup eager file-level dedup fast-path"
  - "pkg/blockstore/engine/engine.go — engine.Flush invokes tryEagerSmallFileDedup BEFORE trySpeculativeFileLevelDedup"
  - "pkg/blockstore/engine/coordinator_test.go — fakeCoordinator.getFileObjectIDCalls counter (distinguishing signal: eager hit ⇒ 0, speculative reached ⇒ 1)"
affects:
  - "Small-file rewrite hot-path (.config / dotfiles / templates) — chunker + appendlog + CAS write skipped entirely on hit"
  - "Cross-VM dedup (D-41 ≥40%) hit-rate is further amplified for the workload Phase 19 targets"
  - "D-26 transitional markers — unchanged; Plan 07's cache wire-in remains the warm-after-write seam for the MISS path"

tech-stack:
  added: []
  patterns:
    - "Pre-rollup hook layering: eager (cheap pre-filter, single-chunk only) → speculative (general post-snapshot case) → rollup (default path). Each layer short-circuits the next on hit."
    - "Outer size gate at the call site avoids the ReadPayloadAt alloc + I/O for large files; inner gate inside tryEagerSmallFileDedup is the real authority."
    - "Shared finalize machinery (applyFileLevelDedupHit) handles D-11 appendlog cleanup; eager hit inherits cleanup without a separate DeleteAppendLog wire-in."
    - "Distinguishing observable for branch-ordering tests: fakeCoordinator.getFileObjectIDCalls counter — bumped only by the speculative branch (engine.go ~line 730)."

key-files:
  created:
    - "pkg/blockstore/engine/eager_dedup_test.go — 7 unit tests for tryEagerSmallFileDedup (threshold gates, hit/miss, cache warming, nil-coord)"
    - "pkg/blockstore/engine/eager_dedup_flush_test.go — 5 engine.Flush integration tests (hit short-circuit, miss fall-through, large bypass, appendlog cleanup, ordering)"
  modified:
    - "pkg/blockstore/engine/dedup.go — new tryEagerSmallFileDedup function placed adjacent to trySpeculativeFileLevelDedup; chunker + blake3 imports added"
    - "pkg/blockstore/engine/engine.go — pre-rollup eager hook in Flush at lines 683-712 (BEFORE the existing speculative-dedup block); chunker import added"
    - "pkg/blockstore/engine/coordinator_test.go — getFileObjectIDCalls counter on fakeCoordinator"

key-decisions:
  - "Source-of-truth for in-RAM bytes: bs.local.ReadPayloadAt (NOT a new peekPayloadBuffer accessor). The plan flagged this as a potential 'real implementation challenge' with three options; the existing LocalStore.ReadPayloadAt method already serves the appendlog (pre-rollup, FSStore production case) AND walks the FileBlock manifest (post-rollup, memory backend / steady state). Either path returns the same bytes for the eager hash + lookup. No new accessor needed — Wave-0 seam-creation was avoidable."
  - "Cache access via the BlockStore back-reference (m.bs.cache.Put), NOT a new field on Syncer. The Plan's three options (A: constructor arg, B: move to BlockStore receiver, C: pass cache as arg) all required structural changes. Option-equivalent-D — m.bs is already wired by engine.New (engine.go:247) for the speculative path's InvalidateFile call; eager reuses the same back-reference for Put. Zero new plumbing."
  - "applyFileLevelDedupHit signature retained as-is — no bool 'isEager' flag added. The plan offered both shapes; the existing 6-arg signature (with isRetry bool) is sufficient. Eager passes isRetry=false; the helper's STATE-01..03 + InvalidateFile + DeleteLog + ListFileBlocks-cleanup invariants apply identically. The plan's note 'omit it and route through the existing call' is the path taken."
  - "Speculative single-block ref passed to applyFileLevelDedupHit (NOT empty). Target's ObjectID == provisional ObjectID forces target's BlockRef list to also reduce to one ref with the same content hash (the only single-block input that produces the same Merkle root BLAKE3(prefix||h)). The set-difference math therefore yields an empty speculative-only set; no spurious DecrementRefCount / InvalidateFile fires. Coordinator.DecrementRefCount tolerates 'row not found' as a no-op anyway, but the equality argument makes the path correct on its own."
  - "ObjectID for n=1 is NOT a bare leaf hash. ComputeObjectID([{Hash:h}]) = BLAKE3(prefix || h) per pkg/blockstore/objectid.go:25 — the canonical Merkle root with domain-separation prefix. D-14's 'compute trivial ObjectID (= that hash for single-block files)' is approximate language; the actual semantics are 'a single hash function over [prefix || h]'. The eager path uses blockstore.ComputeObjectID literally so it is byte-identical to the speculative path's output for the same single-chunk content — a previously-quiesced file dedups to this ObjectID only when its FileAttr.Blocks list is also exactly one block with the same content hash. Documented inline in tryEagerSmallFileDedup's godoc."
  - "Outer size gate at the engine.Flush call site (NOT just inside tryEagerSmallFileDedup). The plan called the outer gate 'intentionally defensive'; we kept it because (1) it skips the ReadPayloadAt buffer alloc + I/O entirely for large files (where the inner gate would also bypass, but only after the call site has already paid the alloc cost), and (2) the local.GetFileSize lookup is an in-memory hash-map probe (fs.go:880 — no I/O), so the gate is cheap. The inner gate inside tryEagerSmallFileDedup remains the real authority — the outer is opportunistic short-circuit."
  - "D-11 appendlog cleanup handled by the shared applyFileLevelDedupHit path (NOT a separate DeleteAppendLog call after the hit). The plan's example sketch in engine.go called bs.deleteAppendLog(ctx, payloadID) after a hit; reading applyFileLevelDedupHit step 5 (dedup.go:248-253 in HEAD) shows m.local.DeleteLog(ctx, payloadID) already fires from inside the shared machinery — the eager hit inherits the cleanup. The TestEngine_Flush_EagerHit_DeletesAppendLog test pins this end-to-end: post-hit ReadPayloadAt returns ErrFileBlockNotFound, proving the appendlog was dropped."
  - "Recording cache for D-16 cache-warming tests: separate recordingPutCache type (in eager_dedup_test.go) rather than extending the existing recordingCache. The existing recordingCache has a no-op Put (engine_test.go:332) and is reused by many tests; widening Put to record would require auditing all consumers and lifting the no-op contract. The new recordingPutCache lives in the eager-dedup test file's package scope so the two flush-test files share it without cross-package coupling."

requirements-completed: [D-13, D-14, D-15, D-16]

duration: ~45min
completed: 2026-05-21
---

# Phase 19 Plan 08: Wire Opt 4 — eager small-file dedup fast-path

**Wave 2 plan that closes Phase 19 by adding `Syncer.tryEagerSmallFileDedup` as the first pre-rollup hook in `engine.Flush`. Files at or below `chunker.MinChunkSize` (1 MiB) hash whole content in RAM via `blake3.Sum256`, compute the single-block ObjectID `ComputeObjectID([{Hash:h, Offset:0, Size:size}])`, and consult `metadata.FindByObjectID`. On hit, the existing `applyFileLevelDedupHit` machinery finalizes the swap (STATE-01..03 preserved; D-11 appendlog cleanup via the same path) and the engine `Cache.Put(h, data)` warms the just-hashed bytes (D-16) before returning `&blockstore.FlushResult{Finalized: true}, nil`. On miss the eager call returns false and `engine.Flush` falls through to the existing `trySpeculativeFileLevelDedup` → `bs.syncer.Flush` rollup chain — which now also populates `Cache` via Plan 07's `OnChunkComplete` wiring. Ordering is pinned by a new `fakeCoordinator.getFileObjectIDCalls` counter: eager-hit short-circuits ⇒ 0; eager-miss / large-file bypass ⇒ 1 (speculative branch ran).**

## Performance

- **Duration:** ~45 minutes
- **Started:** 2026-05-21
- **Completed:** 2026-05-21
- **Tasks:** 2 (each TDD RED + GREEN)
- **Files modified:** 3 source + 2 new test files + 1 test helper

## Accomplishments

### Task 1 — `tryEagerSmallFileDedup` in `pkg/blockstore/engine/dedup.go`

- New `Syncer.tryEagerSmallFileDedup(ctx, payloadID, data) (hit bool, err error)` placed immediately before `trySpeculativeFileLevelDedup` for code locality.
- Threshold gate `len(data) == 0 || len(data) > chunker.MinChunkSize` returns `(false, nil)` BEFORE hashing (D-13).
- Nil-coordinator gate mirrors `trySpeculativeFileLevelDedup` for test ergonomics.
- Hash via `blake3.Sum256` → `provisional = blockstore.ComputeObjectID([{Hash:h, Offset:0, Size:size}])`. ObjectID semantics verified against `pkg/blockstore/objectid.go`: BLAKE3(prefix || h), not a bare leaf hash. Documented inline.
- Hit path delegates to `applyFileLevelDedupHit` (the speculative path's shared finalize machinery) — STATE-01..03 + cache invalidation + DeleteLog invariants preserved (D-14).
- After successful finalize, `m.bs.cache.Put(h, data)` warms the engine Cache with the in-RAM bytes (D-16). The back-reference is the same one engine.New wires for the speculative path's `InvalidateFile` call; eager reuses it.
- Empty data and nil-coordinator return `(false, nil)` defensively.

### Task 2 — `engine.Flush` pre-rollup hook in `pkg/blockstore/engine/engine.go`

- Hook inserted BEFORE the existing `if bs.coordinator != nil { specBlocks, ... := bs.syncer.snapshotPendingBlockRefs ... }` block — eager runs first (D-14).
- Source-of-truth for in-RAM bytes: `bs.local.GetFileSize` (in-memory probe, no I/O) then `bs.local.ReadPayloadAt` (consults appendlog → manifest). The plan flagged the data source as a potential implementation challenge; the existing `LocalStore.ReadPayloadAt` already serves both pre-rollup appendlog bytes (FSStore production) and manifest-resolved bytes (memory backend / FSStore steady state). No new accessor needed.
- Outer size gate `size > 0 && size <= chunker.MinChunkSize` opportunistically skips the alloc + I/O for large files. Inner gate inside `tryEagerSmallFileDedup` remains the real authority.
- Short / errored read degrades gracefully to the speculative path — the eager optimisation is opportunistic and never blocks Flush.

### Test infrastructure — `fakeCoordinator.getFileObjectIDCalls`

- New counter incremented by `fakeCoordinator.GetFileObjectID` (bumped only by the speculative branch — eager does NOT consult GetFileObjectID).
- Distinguishing signal for branch-ordering tests: eager-hit short-circuits ⇒ counter == 0; eager-miss or large-file bypass ⇒ counter == 1.

## Task Commits

TDD cadence — RED first, GREEN second, signed.

1. **Task 1 RED: failing tests for tryEagerSmallFileDedup** — `8d8586b7` (test)
2. **Task 1 GREEN: implement tryEagerSmallFileDedup in dedup.go** — `89a06b54` (feat)
3. **Task 2 RED: failing tests for engine.Flush eager-dedup hook** — `0f741b35` (test)
4. **Task 2 GREEN: wire tryEagerSmallFileDedup into engine.Flush** — `a47366a8` (feat)

## Files Created/Modified

### Created

- `pkg/blockstore/engine/eager_dedup_test.go` — 7 unit tests for `tryEagerSmallFileDedup`:
  - `TestTryEagerSmallFileDedup_DataAboveThreshold_ReturnsFalse` — D-13: files > MinChunkSize bypass without consulting FindByObjectID.
  - `TestTryEagerSmallFileDedup_DataAtThreshold_Proceeds` — D-13: files == MinChunkSize trigger (inclusive upper bound).
  - `TestTryEagerSmallFileDedup_Hit_ReturnsTrue` — D-14: seeded ObjectID hit invokes applyFileLevelDedupHit (PersistFileBlocks + IncrementRefCount fingerprint).
  - `TestTryEagerSmallFileDedup_Miss_ReturnsFalse` — Miss path: no PersistFileBlocks / no IncrementRefCount.
  - `TestTryEagerSmallFileDedup_Hit_PopulatesCache` — D-16: Cache.Put recorded with content hash + identical bytes (via local recordingPutCache).
  - `TestTryEagerSmallFileDedup_EmptyData_ReturnsFalse` — defensive gate for `nil` and `[]byte{}`.
  - `TestTryEagerSmallFileDedup_NilCoordinator_ReturnsFalse` — mirror trySpeculative's nil-coord gate.
- `pkg/blockstore/engine/eager_dedup_flush_test.go` — 5 integration tests for the engine.Flush hook:
  - `TestEngine_Flush_SmallFile_Hit_ShortCircuits` — Finalized=true; getFileObjectIDCalls==0; Cache.Put fingerprint observed.
  - `TestEngine_Flush_SmallFile_Miss_FallsThroughToRollup` — getFileObjectIDCalls==1 (speculative ran).
  - `TestEngine_Flush_LargeFile_Bypasses_EagerPath` — content > MinChunkSize: getFileObjectIDCalls==1.
  - `TestEngine_Flush_EagerHit_DeletesAppendLog` — D-11: post-hit ReadPayloadAt returns ErrFileBlockNotFound.
  - `TestEngine_Flush_EagerHit_BeforeSpeculative_Ordering` — explicit ordering: eager fires (Cache.Put observed) AND speculative does NOT (getFileObjectIDCalls==0, findCalls==1).

### Modified

- `pkg/blockstore/engine/dedup.go`
  - Imports: added `chunker` and `lukechampine.com/blake3`.
  - New `tryEagerSmallFileDedup` function placed immediately before `trySpeculativeFileLevelDedup` (~ 90 lines including godoc).
- `pkg/blockstore/engine/engine.go`
  - Import: added `chunker`.
  - `Flush` method: new pre-rollup hook block (lines 683-712) inserted BEFORE the existing `if bs.coordinator != nil { specBlocks, ... }` block. Hook reads `bs.local.GetFileSize` + `bs.local.ReadPayloadAt` + calls `bs.syncer.tryEagerSmallFileDedup`; on hit returns `&FlushResult{Finalized: true}, nil`; on miss or short read falls through to the existing speculative chain.
- `pkg/blockstore/engine/coordinator_test.go`
  - New `getFileObjectIDCalls int` field on `fakeCoordinator`; `GetFileObjectID` increments it under the existing mutex.

## Decisions Made

1. **ReadPayloadAt as the in-RAM bytes source — no new accessor.** The plan flagged this as a "real implementation challenge" with three options (peekPayloadBuffer / new BlockStore accessor / disk-read fallback). `LocalStore.ReadPayloadAt` already handles BOTH the pre-rollup appendlog case (FSStore production) AND the manifest-resolved case (memory backend / FSStore steady state). Wave-0 seam-creation was avoidable.
2. **Cache via the BlockStore back-reference (m.bs.cache.Put), not a new Syncer field.** `m.bs` is wired by `engine.New` (engine.go:247) for the speculative path's `InvalidateFile` call; eager reuses the same back-reference for `Put`.
3. **applyFileLevelDedupHit signature retained unchanged.** No `isEager` bool flag added. The 6-arg signature (with `isRetry bool`) is sufficient — eager passes `isRetry=false` and shares STATE-01..03 + InvalidateFile + DeleteLog + ListFileBlocks-cleanup invariants identically.
4. **Speculative single-block ref passed (not empty).** Target's ObjectID equality forces target's BlockRef list to also reduce to one ref with the same content hash; the set-difference math yields an empty speculative-only set; no spurious decrement / invalidate fires. Mathematically equivalent to passing empty, but the explicit ref is the cleanest contract.
5. **ObjectID for n=1 is BLAKE3(prefix || h), NOT a bare leaf hash.** Verified against `pkg/blockstore/objectid.go:25`. Documented inline in `tryEagerSmallFileDedup`'s godoc — D-14's "= that hash" is approximate language; the actual semantics are a single hash over `[prefix || h]`.
6. **Outer size gate kept defensive.** Plan called it "intentionally defensive"; we kept it because `GetFileSize` is an in-memory probe (no I/O) and the gate skips the `ReadPayloadAt` alloc entirely for large files. Inner gate inside `tryEagerSmallFileDedup` is the real authority.
7. **D-11 appendlog cleanup via the shared machinery, not a separate DeleteAppendLog call.** `applyFileLevelDedupHit` step 5 (dedup.go ~line 248) already fires `m.local.DeleteLog(ctx, payloadID)` from inside the shared finalize machinery — eager hit inherits the cleanup. `TestEngine_Flush_EagerHit_DeletesAppendLog` pins this end-to-end.
8. **Separate recordingPutCache type for D-16 cache-warming tests.** The existing `recordingCache.Put` is a no-op (engine_test.go:332); widening it would require auditing all consumers and lifting the no-op contract. New `recordingPutCache` lives in `eager_dedup_test.go`'s package scope and is shared with `eager_dedup_flush_test.go`.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Test 5 deadlock against recordingPutCache.mu**
- **Found during:** Task 1 GREEN (test run after wiring tryEagerSmallFileDedup)
- **Issue:** `TestTryEagerSmallFileDedup_Hit_PopulatesCache` acquired `rec.mu.Lock()` with `defer rec.mu.Unlock()`, then called `rec.Get(...)` inside the critical section. `Get` also acquires `rec.mu` → 60s test timeout.
- **Fix:** Capture state (`putCalls`, `putHashes`) under lock, release the lock, then assert outside the critical section (including the `Get` call).
- **Files modified:** `pkg/blockstore/engine/eager_dedup_test.go`
- **Committed in:** `89a06b54` (Task 1 GREEN — RED and the fix folded into the same GREEN commit because the RED commit's test was uncompilable, masking the latent deadlock).

### Other deviations

- The plan's Task 2 test 1 sketch ("assert NO chunker invocation … counter via test hook on chunker.NewChunker") was not implemented as written — no such hook exists in the codebase, and the memory backend's synchronous rollup means chunks land in `fileBlockStore` BEFORE `Flush` is called. The semantically equivalent assertion is: **the eager-hit fingerprint (Cache.Put with the content hash) is observed AND the speculative branch's GetFileObjectID call did NOT fire**. Both fingerprints are tested. Documented in the test file's package doc and in `TestEngine_Flush_SmallFile_Hit_ShortCircuits`'s comment.
- The plan's Task 2 test 4 example referenced `bs.deleteAppendLog(ctx, payloadID)` as if it were a new helper. The actual D-11 cleanup is already inside `applyFileLevelDedupHit` (dedup.go step 5); no new helper was added. The test still asserts the end-to-end invariant (post-hit ReadPayloadAt returns ErrFileBlockNotFound).

## Issues Encountered

- One transient `-race` failure on `pkg/blockstore/engine` during the wider regression run; passed cleanly on re-run. No race annotations were reported in the failure output (timeout-shape, not a data race). Likely a slow-test/timeout artifact of the shared test machine. Three subsequent runs all pass.

## Verification Suite Run

- `go test -race ./pkg/blockstore/engine/... -run "TryEagerSmallFileDedup" -count=1 -timeout 60s` → **7/7 PASS**.
- `go test -race ./pkg/blockstore/engine/... -run "Engine_Flush_SmallFile|Engine_Flush_EagerHit|Engine_Flush_LargeFile_Bypasses" -count=1 -timeout 60s` → **5/5 PASS**.
- `go test -race ./pkg/blockstore/... -count=1 -timeout 180s` → **all PASS** (engine 10.2 s, local/fs 12.7 s, chunker 19.2 s, others sub-3 s).
- `go test ./... -count=1 -timeout 300s` → no failures (build / vet exit 0).
- `grep -c "tryEagerSmallFileDedup" pkg/blockstore/engine/engine.go` → 2.
- `grep -c "tryEagerSmallFileDedup" pkg/blockstore/engine/dedup.go` → 2.
- `awk '/func \(bs \*BlockStore\) Flush/,/^}/' pkg/blockstore/engine/engine.go | grep -n "tryEagerSmallFileDedup\|trySpeculativeFileLevelDedup"` shows eager at line 33, speculative at line 56 — ordering verified.

## Output Spec Confirmation (from PLAN §<output>)

- **Actual accessor name used for in-RAM buffered-data peek:** `bs.local.ReadPayloadAt` (existing LocalStore interface method — no new accessor created). The plan's hypothetical `peekPayloadBuffer` was not needed.
- **Whether a new accessor had to be created (Wave-0 seam) or whether an existing accessor was already available:** Existing — `LocalStore.ReadPayloadAt` covers both pre-rollup appendlog (FSStore production) and manifest-resolved (memory backend / FSStore steady state) cases.
- **Whether `m.cache` was already on Syncer or whether cache wiring needed to be plumbed through:** Neither — cache is reached via `m.bs.cache` (the BlockStore back-reference that `engine.New` already wires at line 247 for the speculative path's InvalidateFile call). Zero new wiring.
- **ComputeObjectID single-element semantics confirmed:** ObjectID == BLAKE3(prefix || h) for n=1, **NOT** a bare leaf hash. Verified against `pkg/blockstore/objectid.go:25`. D-14's "= that hash" language is approximate; documented inline in `tryEagerSmallFileDedup`'s godoc.
- **Whether the bool flag was added to applyFileLevelDedupHit signature or omitted:** Omitted. The existing 6-arg signature is sufficient — eager passes `isRetry=false` and shares the helper's invariants identically.

## Self-Check: PASSED

- `pkg/blockstore/engine/dedup.go`:
  - `func (m *Syncer) tryEagerSmallFileDedup(` → 1 occurrence.
  - `chunker.MinChunkSize` → 1 occurrence.
  - `m.bs.cache.Put(h, data)` → 1 occurrence (D-16 cache-warming on hit).
  - `applyFileLevelDedupHit(` → 2 occurrences (the eager call site + the existing speculative call site).
- `pkg/blockstore/engine/engine.go`:
  - `tryEagerSmallFileDedup` → 2 occurrences (godoc reference + the call site).
  - `trySpeculativeFileLevelDedup` → 1 occurrence (the existing speculative call site).
  - Flush-body ordering (awk gate): `tryEagerSmallFileDedup` appears at relative line 33, `trySpeculativeFileLevelDedup` at relative line 56 — eager BEFORE speculative.
- `pkg/blockstore/engine/coordinator_test.go`:
  - `getFileObjectIDCalls int` → 1 (struct field).
  - `f.getFileObjectIDCalls++` → 1 (counter increment in GetFileObjectID).
- Commits `8d8586b7` / `89a06b54` / `0f741b35` / `a47366a8` — all present in `git log --oneline`.
- Final regression suite: `go test ./... -count=1` no failures; `go build ./... && go vet ./...` exit 0.

---
*Phase: 19-write-path-ram-optimizations*
*Plan: 08*
*Completed: 2026-05-21*

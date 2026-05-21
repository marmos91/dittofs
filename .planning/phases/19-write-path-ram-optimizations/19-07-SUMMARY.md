---
phase: 19-write-path-ram-optimizations
plan: 07
subsystem: blockstore
tags: [blockstore, fs, engine, cache, callback, opt3, write-path, ram]

requires:
  - phase: 19-write-path-ram-optimizations
    plan: 04
    provides: "FSStoreOptions.OnChunkComplete slot + FSStore.onChunkComplete field + SetOnChunkComplete post-hoc setter (Plan 04)"
provides:
  - "pkg/blockstore/local/fs/chunkstore.go — StoreChunk fires bc.onChunkComplete after lruTouch on success (D-10/D-12 producer side)"
  - "pkg/blockstore/engine/engine.go — engine.New installs SetOnChunkComplete to bind callback to bs.cache.Put (D-11/D-16 consumer side)"
  - "pkg/controlplane/runtime/shares/service.go — dedup_lru_size config knob plumbed through to FSStoreOptions.DedupLRUSize (Plan 03 plumbing completion)"
affects:
  - "v0.16+ NFS COMMIT-then-READ — chunks written via rollup are now warm in engine.Cache without a disk round-trip"
  - "D-11 RAM ceiling — bounded by existing Cache LRU; no extra cap needed"
  - "D-26 transitional markers — three new TRANSITIONAL-NEXT-MILESTONE pin sites added"

tech-stack:
  added: []
  patterns:
    - "Structural-interface setter install at engine.New (mirrors SetObjectIDPersister at engine.go:156-188) — avoids cross-package named-type ceremony, accommodates the BlockStore.Start cache-materialization lifecycle"
    - "Closure captures *BlockStore (not bs.cache directly) so the Null-Object→real-Cache swap inside Start is observed transparently"
    - "Producer-side firing AFTER lruTouch returns (lock released) — avoids widening the lruMu hot lock window across an unrelated consumer lock (Cache.Put's own mu)"

key-files:
  created:
    - "pkg/blockstore/engine/onchunkcomplete_test.go — 3 integration tests for Plan 07's engine-to-FSStore wire-in"
  modified:
    - "pkg/blockstore/local/fs/chunkstore.go — StoreChunk fires onChunkComplete behind nil-guard + 2 D-26 markers (pinned hot-tail RAM, zstd compression)"
    - "pkg/blockstore/local/fs/chunkstore_test.go — 5 unit tests covering exactly-once / nil-safe / error-skip / outside-lruMu invariants"
    - "pkg/blockstore/engine/engine.go — structural-interface SetOnChunkComplete install with bs.cache.Put-wrapping closure"
    - "pkg/blockstore/engine/cache.go — cold-cache prefetch TRANSITIONAL-NEXT-MILESTONE marker (D-26)"
    - "pkg/controlplane/runtime/shares/service.go — dedup_lru_size config knob → FSStoreOptions.DedupLRUSize"

key-decisions:
  - "Used Plan 04's SetOnChunkComplete setter (not the FSStoreOptions literal field) for the engine wiring. The plan listed both approaches; the setter is mandatory because bs.cache only materializes inside BlockStore.Start (engine.go:267-270 swaps nullCache{} → realCache), AFTER the FSStore was already constructed in shares/service.go and passed in via cfg.Local. The setter mirrors SetObjectIDPersister's existing pattern at engine.go:156-188."
  - "Closure captures bs (the *BlockStore), reads bs.cache at fire time (not at install time). Required so the Null-Object substitution at engine.go:130 followed by the real-Cache swap at engine.go:269 is observed transparently — install once at engine.New; fire correctly throughout the Start-then-serve-traffic lifetime. nullCache.Put is a no-op (cache.go:138) so the install is panic-free even when the budget is zero."
  - "Path arg of the OnChunkComplete callback is discarded (`_ string`) at the engine wiring site. Cache.Put doesn't need it; the firing-site contract still passes the path so future mmap-or-copy or zero-copy strategies (the open TRANSITIONAL marker on cache.go documents one) can adopt it without changing the producer signature."
  - "DedupLRUSize plumbing landed in shares/service.go (config map → FSStoreOptions.DedupLRUSize) — NOT in engine.go as the plan's task 2 example pseudocode suggested. The engine has no access to the operator config; the share config map is read in shares/service.go where the FSStoreOptions literal is actually built (mirroring the existing rollup_workers / stabilization_ms / orphan_log_min_age_seconds knobs). The plan anticipated this: 'If the config is reached differently (e.g., via opts.Config or via a runtime), adapt accordingly.'"
  - "Test 5 (callback-outside-lruMu / reentrant-no-deadlock) uses a sentinel non-disk path (`/dev/null/touch-target`) because lruTouch is an index-only insert — it does not stat or open the path. A 5s timeout via select-channel asserts no deadlock; a follow-up lruIndex membership check confirms the reentrant probe actually exercised the lock."

requirements-completed: [D-10, D-11, D-12, D-16]

duration: ~30min
completed: 2026-05-21
---

# Phase 19 Plan 07: Wire Opt 3 — direct-to-Cache on chunk completion

**Wave 2 plan that closes the Plan 04 surface and binds the write path to the engine Cache. `chunkstore.StoreChunk` fires `bc.onChunkComplete(hash, data, path)` once per successful disk store + LRU touch; `engine.New` installs the callback to wrap `bs.cache.Put` via a structural-interface assertion on `cfg.Local` (mirroring `SetObjectIDPersister`). Every successful chunk write now leaves a warm Cache entry — the NFS COMMIT-then-READ pattern no longer triggers a disk hop for the just-written chunk. Two unit tests cover the producer-side D-12 exactly-once / error-skip / nil-safe / outside-lruMu invariants; three integration tests cover the engine-side wiring with realCache, Cache-size-cap behavior, and nullCache panic-safety.**

## Performance

- **Duration:** ~30 minutes
- **Started:** 2026-05-21
- **Completed:** 2026-05-21
- **Tasks:** 2 (TDD RED/GREEN per task)
- **Files modified:** 5 (3 source, 2 tests + 1 new test file)

## Accomplishments

### Producer side (Task 1 — chunkstore.go)

- `StoreChunk` now fires `bc.onChunkComplete(h, data, path)` AFTER `lruTouch` returns, behind a `bc.onChunkComplete != nil` nil-guard.
- Exactly-once contract: the call sits between `lruTouch` and the `return nil` line. Every error path above returns before reaching the firing site — Test 4 (`DoesNotFireOnError`) pins this with a `MkdirAll`-fails injection (pre-creating `blocks/<hh>` as a regular file).
- Outside-lruMu contract: `lruTouch` uses `defer bc.lruMu.Unlock()` and the callback site follows it — the lruMu lock is released before the consumer's lock (typically `Cache.mu`) is taken. Test 5 (`FiresOutsideLruMuLock`) injects a reentrant `bc.lruTouch(otherHash, ...)` call into the callback; if the firing site held lruMu, the reentrancy would deadlock (Go mutexes are not reentrant). The test asserts the StoreChunk goroutine completes within 5s, then probes `bc.lruIndex[otherHash]` to confirm the reentrant probe actually fired.
- Two D-26 TRANSITIONAL-NEXT-MILESTONE markers added:
  - `pinned hot-tail RAM` (StoreChunk may bypass disk write in v0.17+).
  - `zstd compression` (callback fires UNCOMPRESSED data so Cache can serve reads without a decompress hop).

### Consumer side (Task 2 — engine.go + cache.go)

- `engine.New` installs the callback via structural-interface assertion (mirrors `SetObjectIDPersister` precedent at engine.go:156-188):
  ```go
  if setter, ok := cfg.Local.(interface {
      SetOnChunkComplete(fn func(hash blockstore.ContentHash, data []byte, path string))
  }); ok {
      setter.SetOnChunkComplete(func(hash blockstore.ContentHash, data []byte, _ string) {
          bs.cache.Put(hash, data)
      })
  }
  ```
- Closure captures `bs`, reads `bs.cache` at fire time — observes the `nullCache{} → realCache` swap inside `BlockStore.Start` (engine.go:267-270) transparently.
- `Cache.Put` is nil-safe, closed-safe, and max-bytes-safe (cache.go:229-235) — the binding is canonical without extra guards at the engine seam.
- Cold-cache prefetch D-26 TRANSITIONAL-NEXT-MILESTONE marker added to cache.go documenting that the write-side wiring covers warm-after-write; cold-cache prefetch (#519 deferred) covers the restart-then-read case.

### DedupLRUSize plumbing (shares/service.go)

- New `dedup_lru_size` block-store config knob plumbed through to `FSStoreOptions.DedupLRUSize` — mirrors the existing `rollup_workers` / `stabilization_ms` / `orphan_log_min_age_seconds` knob shapes (`float64 → int` with > 0 validation). Zero falls back to FSStore's default-on-zero idiom (4096 slots) from Plan 04.

## Task Commits

TDD cadence — RED first, GREEN second. All commits signed.

1. **Task 1 RED: failing tests for chunkstore OnChunkComplete firing site** — `39685055` (test)
2. **Task 1 GREEN: fire OnChunkComplete from chunkstore.StoreChunk** — `6357eb5e` (feat)
3. **Task 2 RED: failing tests for engine OnChunkComplete cache wire-in** — `427aa870` (test)
4. **Task 2 GREEN: wire OnChunkComplete to engine Cache.Put + plumb DedupLRUSize** — `5ba83e40` (feat)

## Files Created/Modified

### Created

- `pkg/blockstore/engine/onchunkcomplete_test.go` — three integration tests:
  - `TestEngine_OnChunkComplete_WiredToCache` — FS-backed engine + 64 MiB cache budget, `StoreChunk` → `bs.cache.Get` HIT with byte-identical payload.
  - `TestEngine_OnChunkComplete_LargeChunkRespectsCacheCap` — 4 KiB cache + 8 KiB chunk → `Cache.Put`'s `> c.maxBytes` guard fires; `bs.cache.Get` MISS (chunk skipped).
  - `TestEngine_OnChunkComplete_NilCache_NoPanic` — `ReadBufferBytes=0` → `nullCache{}`; `StoreChunk` succeeds without panic; `bs.cache.Get` MISS (Null Object).

### Modified

- `pkg/blockstore/local/fs/chunkstore.go`
  - StoreChunk godoc: zstd-compression TRANSITIONAL-NEXT-MILESTONE marker.
  - Post-lruTouch body: pinned-hot-tail-RAM TRANSITIONAL-NEXT-MILESTONE marker + the new nil-guarded firing site (`if bc.onChunkComplete != nil { bc.onChunkComplete(h, data, path) }`).
- `pkg/blockstore/local/fs/chunkstore_test.go`
  - Imports: added `sync/atomic`, `time` for the new tests.
  - Five new tests appended in a `// --- Phase 19 Plan 07 ---` section: fires-on-success / nil-no-op / exactly-once / does-not-fire-on-error / outside-lruMu.
- `pkg/blockstore/engine/engine.go`
  - New block between the `SetObjectIDPersister` install and the `SetChunkEmitter` install: structural-interface SetOnChunkComplete install wrapping `bs.cache.Put`.
- `pkg/blockstore/engine/cache.go`
  - Cold-cache prefetch TRANSITIONAL-NEXT-MILESTONE marker placed above the `Cache` struct doc block.
- `pkg/controlplane/runtime/shares/service.go`
  - New config-map decode block for `dedup_lru_size` → `fsOpts.DedupLRUSize`, mirroring the surrounding knobs.

## Decisions Made

1. **Setter (not literal) for engine wiring.** Plan 04 already documented this: `bs.cache` materializes inside `BlockStore.Start`, AFTER `cfg.Local` was constructed in `shares/service.go`. The FSStoreOptions literal at the construction site cannot capture `bs`. The setter is the canonical wiring; matches the SetObjectIDPersister precedent.
2. **Closure-captures-bs, reads-bs.cache-at-fire-time.** Required so the Null-Object pattern works: `nullCache{}` until Start, real `*Cache` after. Both implement `Put` correctly (nullCache.Put is a no-op).
3. **Path arg discarded at the engine seam.** Cache.Put doesn't consume it. The producer signature retains `path string` to keep future mmap-or-copy strategies viable.
4. **DedupLRUSize plumbed in shares/service.go, not engine.go.** The plan's pseudocode (`DedupLRUSize: bs.cfg.Blockstore.Local.DedupLRUSize`) doesn't match the actual codebase — engine.go has no `cfg` field carrying the operator config tree. The shares-service path matches existing knobs.
5. **Test 5 reentrancy probe via `/dev/null/touch-target`.** `lruTouch` is index-only — it does not open/stat the path. Using a sentinel non-existent path avoids creating real disk artifacts.

## Deviations from Plan

### Auto-fixed Issues

None. All choices above are explicitly anticipated by the plan's "If construction-time wiring is sufficient, skip…" / "If `bs.cache` is not in scope … fall back to using `bs.fs.SetOnChunkComplete(...)`" / "If the config is reached differently (e.g., via opts.Config or via a runtime), adapt accordingly" hedges.

### Other deviations

- The plan's task 2 `<verify>` grep pattern (`OnChunkComplete: func`) targeted a literal `FSStoreOptions{ OnChunkComplete: ... }` shape. The actual wiring uses the setter-fallback the plan also describes, so `setter.SetOnChunkComplete(func(...))` is what landed. Grep counts for the producer-side verification commands (the `bc.onChunkComplete(h, data, path)` literal + the two chunkstore TRANSITIONAL markers) still match 1 each.

## Issues Encountered

None. The Plan 04 surface (slot + struct field + setter) was correctly anticipated; Plan 07 only had to fire from the producer and bind at the consumer.

## Verification Suite Run

- `go test -race ./pkg/blockstore/local/fs/... -run "Chunkstore_OnChunkComplete|Chunkstore_NilOnChunkComplete" -v -count=1` → **5/5 PASS**.
- `go test -race ./pkg/blockstore/engine/... -run "Engine_OnChunkComplete" -v -count=1` → **3/3 PASS**.
- `go test -race ./pkg/blockstore/... -count=1` → **all PASS** (engine 10.0 s, local/fs 14.8 s, chunker 19.1 s, others sub-3 s).
- `go test ./pkg/controlplane/runtime/shares/... -count=1` → **PASS**.
- `go build ./...` → exit 0.
- `go vet ./...` → exit 0.

## Output Spec Confirmation (from PLAN §<output>)

- **Construction order swap or setter?** Setter (`SetOnChunkComplete`) via structural-interface assertion at `engine.go` line ~206 — placed between the existing `SetObjectIDPersister` install and the `SetChunkEmitter` install. No construction-order swap; the Null Object substitution at engine.go:130 + the real-Cache swap at engine.go:269 do the work.
- **Closure form:** `func(hash blockstore.ContentHash, data []byte, _ string) { bs.cache.Put(hash, data) }` — anonymous closure capturing `bs`, path arg discarded. Not a named helper method.
- **DedupLRUSize reachable path:** `pkg/config/blockstore.go` defines `BlockstoreLocalConfig.DedupLRUSize` with apply-defaults (4096). The share-creation path reaches `pkg/controlplane/runtime/shares/service.go` with a per-share `config map[string]any`; the new decode block reads `config["dedup_lru_size"]` (float64 from JSON), validates > 0, and assigns to `fsOpts.DedupLRUSize`. **Not** wired through `cfg.Blockstore.Local.DedupLRUSize` directly because the per-share factory does not have the top-level config tree in scope — the per-share config map is the canonical knob source at this seam, mirroring `rollup_workers` / `stabilization_ms`.

## Self-Check: PASSED

- `pkg/blockstore/local/fs/chunkstore.go`:
  - `bc.onChunkComplete(h, data, path)` → 1 occurrence (the firing site).
  - `TRANSITIONAL-NEXT-MILESTONE: pinned hot-tail RAM` → 1.
  - `TRANSITIONAL-NEXT-MILESTONE: zstd compression` → 1.
- `pkg/blockstore/engine/engine.go`:
  - `SetOnChunkComplete(fn func(hash blockstore.ContentHash, data []byte, path string))` → 1 (structural-interface assertion).
  - `bs.cache.Put(hash, data)` → 1 (closure body).
- `pkg/blockstore/engine/cache.go`:
  - `TRANSITIONAL-NEXT-MILESTONE: cold-cache prefetch` → 1.
- `pkg/controlplane/runtime/shares/service.go`:
  - `fsOpts.DedupLRUSize = int(n)` → 1.
- Commits `39685055` / `6357eb5e` / `427aa870` / `5ba83e40` — all present in `git log --oneline`.
- Final test suite under `-race`: all PASS.

---
*Phase: 19-write-path-ram-optimizations*
*Plan: 07*
*Completed: 2026-05-21*

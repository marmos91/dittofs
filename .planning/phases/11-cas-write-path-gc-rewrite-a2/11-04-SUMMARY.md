---
phase: 11-cas-write-path-gc-rewrite-a2
plan: 04
subsystem: blockstore
tags: [td-09, lsl-07, lsl-08, inv-05, stage-and-release, lru, eviction, content-hash]

requires:
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: "FormatCASKey/ParseCASKey, BlockState (Pending/Syncing/Remote), FileBlock.LastSyncAttemptAt, RemoteStore.WriteBlockWithHash, engine.Syncer claim+upload+janitor (plans 11-01, 11-02)"
  - phase: 10-block-store-spec-pass-l-s-l
    provides: "AppendWrite + StoreChunk/CommitChunks + blocks/{hh}/{hh}/{hex} layout; pressure channel + log length bound (INV-05)"
provides:
  - "TD-09 stage-and-release pattern in flushBlock — bytes.Clone snapshot under mb.mu, all disk I/O outside the lock, post-write writeGen check preserves concurrent-writer bytes"
  - "LSL-07 narrowed LocalStore (drops 5 methods exactly: MarkBlockRemote, MarkBlockSyncing, MarkBlockLocal, GetDirtyBlocks, SetSkipFsync)"
  - "LSL-08 in-process LRU eviction keyed by ContentHash (FSStore.lruIndex / lruList / lruTouch / lruEvictOne / seedLRUFromDisk)"
  - "RunEvictionLSL08Suite conformance sub-suite (5 D-27 scenarios, including the load-bearing eviction_no_fbs_calls assertion)"
  - "FBSCounter exported interface so cross-package test wrappers can satisfy the no-FBS-call assertion"
affects: [11-05-restart-recovery, 11-06-gc-mark-sweep, 11-08-e2e]

tech-stack:
  added:
    - "container/list (LRU list — already imported in fdpool.go)"
    - "encoding/hex (LRU disk seeding decode)"
    - "sort (deterministic cold-start LRU order)"
  patterns:
    - "writeGen counter on memBlock — flushBlock stages it and only nils mb.data if no writer interleaved during disk I/O"
    - "CAS chunk LRU is independent of FileBlockStore — disk presence is the source of truth"
    - "Cold-start LRU seed is alphabetically sorted by hash hex for reproducibility"
    - "Per-flush fsync is now unconditional (skipFsync field gone with SetSkipFsync method)"

key-files:
  created:
    - "pkg/blockstore/local/fs/flush_test.go"
    - "pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go"
    - "pkg/blockstore/local/localtest/eviction_lsl08_suite.go"
    - ".planning/phases/11-cas-write-path-gc-rewrite-a2/11-04-SUMMARY.md"
  modified:
    - "pkg/blockstore/local/fs/flush.go"
    - "pkg/blockstore/local/fs/block.go"
    - "pkg/blockstore/local/fs/write.go"
    - "pkg/blockstore/local/fs/fs.go"
    - "pkg/blockstore/local/fs/eviction.go"
    - "pkg/blockstore/local/fs/eviction_test.go"
    - "pkg/blockstore/local/fs/chunkstore.go"
    - "pkg/blockstore/local/fs/appendwrite.go"
    - "pkg/blockstore/local/fs/test_hooks.go"
    - "pkg/blockstore/local/fs/fs_test.go"
    - "pkg/blockstore/local/fs/manage_test.go"
    - "pkg/blockstore/local/local.go"
    - "pkg/blockstore/local/memory/memory.go"
    - "pkg/blockstore/local/localtest/suite.go"
    - "pkg/controlplane/runtime/shares/service.go"

key-decisions:
  - "LocalStore had 28 methods at the start of this plan, not 22 as the plan's <done> assertion expected — the interface drifted with extra accessors (Stats, ListFiles, GetStoredFileSize, ExistsOnDisk, Healthcheck) added in earlier phases. Dropped exactly the 5 methods the plan named (LSL-07 spec target), landing at 23 methods. The 17 target in the plan's grep assertion is therefore stale; the 5-method delta is the load-bearing invariant and is fully satisfied."
  - "Added writeGen counter to memBlock to safely handle concurrent writers during stage-and-release. Without it, the post-write phase would unconditionally nil mb.data and lose any bytes the writer added during the disk I/O window. flushBlock stages writeGen at lock-release time and only clears the buffer if the counter is unchanged at re-acquire time — matching the plan's 'handle a brief intermediate state' guidance."
  - "Removed the skipFsync field entirely (not just its setter). The new flushBlock and appendwrite.go log fsync are unconditional. Local-disk durability is now baseline; per the plan, 'SetSkipFsync becomes irrelevant once writes go through AppendWrite' (D-26)."
  - "Tmp+rename + fsync per-block in flushBlock is heavier than the previous direct-write-no-fsync — chosen to honor the plan's <done> assertion that 'os.OpenFile/f.Write/f.Sync' must appear in flush.go after mb.mu.Unlock. The CPU cost lives outside the lock so concurrent readers/writers are not blocked."
  - "FBSCounter interface is exported (capitalized methods) so wrappers in different `_test.go` packages can satisfy it. The package-internal countingFileBlockStore in fs_test.go and the cross-package countingFBSWrapper in fs_test (external) both implement it — the suite's no-FBS-call assertion works regardless of which wrapper is in front."
  - "DeleteChunk also prunes the in-process LRU entry. Without it, a Phase-11-06 mark-sweep GC that calls DeleteChunk would leave stale entries that lruEvictOne later tries to unlink — silently OK because os.Remove on missing files returns ENOENT and we ignore it, but the bookkeeping is cleaner with explicit pruning."

requirements-completed: [TD-09, LSL-07, LSL-08, INV-05]

duration: 90min
completed: 2026-04-25
---

# Phase 11 Plan 04: TD-09 Stage-and-Release + LSL-07 Interface Narrowing + LSL-08 LRU Eviction Summary

**flushBlock now snapshots bytes via bytes.Clone under mb.mu and performs all disk I/O (mkdir + .tmp + write + fsync + rename) outside the lock; concurrent readers and writers of the same memBlock are unblocked during the disk I/O window. The LocalStore interface drops the five legacy methods (MarkBlockRemote, MarkBlockSyncing, MarkBlockLocal, GetDirtyBlocks, SetSkipFsync). Eviction is now driven entirely from on-disk presence under blocks/{hh}/{hh}/{hex} via an in-process LRU keyed by ContentHash — zero FileBlockStore consultation on the write hot path, with the load-bearing assertion enforced by a new conformance sub-suite.**

## Performance

- **Duration:** ~90 min (3 tasks, 4 commits)
- **Started:** 2026-04-25T15:52:25Z
- **Completed:** 2026-04-25T~17:25Z (UTC, after final task commit)
- **Tasks:** 3 (Task 1 was strict TDD with separate RED/GREEN; Tasks 2/3 combined RED+GREEN per plan-approved alternative for tightly-coupled refactors)
- **Files created:** 4 source/test/doc, **modified:** 15

## Accomplishments

### Task 1 — TD-09 stage-and-release in flushBlock

- `flushBlock` now follows the strict three-phase pattern:
  1. **STAGE under mb.mu:** capture `dataSize`, `bytes.Clone(mb.data[:dataSize])`, snapshot `writeGen`, release lock.
  2. **DISK I/O outside mb.mu:** mkdir, write to `.tmp`, fsync, close, rename to final path. Concurrent readers/writers of the same memBlock proceed unimpeded.
  3. **POST-WRITE re-acquire:** if `writeGen` is unchanged, nil mb.data/dataSize/dirty and return the buffer to the pool. If `writeGen` advanced, leave mb.data alone — a writer interleaved and the bytes will be picked up by the next flush.
- Added `writeGen uint64` field to `memBlock`; `WriteAt` bumps it under lock to signal interleaved mutations.
- `bc.diskUsed.Add(dataSize - prevDiskSize)` runs only AFTER the rename succeeds — no ghost size delta on a failed write.
- 5 new tests in `flush_test.go` covering: lock-contention bound, on-disk consistency vs concurrent writer, state coherence, diskUsed delta, no-op on clean block.

### Task 2 — LSL-07 narrow LocalStore (drop 5 methods)

- Deleted from `local.LocalStore` interface and both fs + memory implementations:
  - `MarkBlockRemote`, `MarkBlockSyncing`, `MarkBlockLocal` — state transitions are now owned by `engine.Syncer` per D-15 (plan 11-02).
  - `GetDirtyBlocks` — superseded by direct on-disk inspection via Phase-10 AppendWrite.
  - `SetSkipFsync` — S3-backend hint obsolete now that writes route through AppendWrite + CAS.
- Deleted the `skipFsync` field from FSStore and MemoryStore. `flush.go` and `appendwrite.go` log fsync are now unconditional.
- Deleted the `transitionBlockState` helper (only used by the three Mark* methods) from FSStore.
- Dropped the `localStore.SetSkipFsync(remoteStore != nil)` call in `pkg/controlplane/runtime/shares/service.go`.
- Conformance suite (`pkg/blockstore/local/localtest/suite.go`) loses `testMarkBlockState` and `testGetDirtyBlocks` plus their `t.Run` registry entries.

### Task 3 — LSL-08 in-process LRU eviction by ContentHash

- New FSStore fields: `lruMu sync.Mutex`, `lruIndex map[ContentHash]*list.Element`, `lruList *list.List`.
- New helpers on FSStore:
  - `lruTouch(h, size, path)` — promotes (or inserts) a chunk to the most-recent end. Idempotent.
  - `lruEvictOne()` — pops from the LRU back, unlinks the file, returns freed bytes. Re-inserts at back on os.Remove failure to avoid losing bookkeeping.
  - `seedLRUFromDisk()` — scans `<baseDir>/blocks/` at New() and populates the LRU in alphabetical-hash order for deterministic cold-start eviction.
- `ensureSpace` rewritten to use LRU only — no FileBlockStore consultation. Pin mode + eviction-disabled retain their early-return semantics.
- Wired `lruTouch` from `StoreChunk` (after rename success) and `ReadChunk` (on cache hit) and `DeleteChunk` (prunes the LRU entry).
- New `RunEvictionLSL08Suite` conformance sub-suite in `localtest/eviction_lsl08_suite.go` covers all 5 D-27 scenarios:
  - `eviction_lru_order` — least-recently-touched evicted first.
  - `eviction_no_fbs_calls` — load-bearing invariant via FBSCounter wrapper.
  - `eviction_re_fetch_after_evict` — ReadChunk after evict surfaces ErrChunkNotFound.
  - `eviction_concurrent_writes_safe` — race-clean under concurrent StoreChunk + ReadChunk.
  - `eviction_lru_seed_on_startup` — seeded chunks are evictable.
- Replaced `eviction_test.go` content end-to-end: legacy `.blk`-driven `TestEviction_*` tests are gone; new `TestLSL08_*` suite mirrors the conformance scenarios with the local fixture.
- Updated `TestManageSetEvictionReEnabled` and the `eviction_path` subtest of `TestLocalWritePath_NoFileBlockStoreCall` to use `StoreChunk` (the canonical post-LSL-08 write path) instead of `populateRemoteBlock` (legacy `.blk`).
- Exported new test hooks on FSStore: `EnsureSpaceForTest`, `ChunkPathForTest`, `SeedLRUFromDiskForTest`, `ResetFBSCallCounterForTest`, `FBSCallCountForTest` (the last two probe via the new `FBSCounter` interface).

## Task Commits

All commits GPG/SSH-signed (ED25519) and pass `git verify-commit`:

1. **Task 1 RED** — `cf637bf2` test(11-04): add failing TD-09 stage-and-release tests for flushBlock
2. **Task 1 GREEN** — `d7860513` feat(11-04): TD-09 stage-and-release pattern in flushBlock
3. **Task 2** — `ffe88c47` feat(11-04): LSL-07 narrow LocalStore — drop 5 methods (Mark*/GetDirty/SetSkipFsync)
4. **Task 3** — `05c71140` feat(11-04): LSL-08 in-process LRU eviction by ContentHash

## Files Created/Modified

### Created

- `pkg/blockstore/local/fs/flush_test.go` — 5 TD-09 tests covering stage-and-release invariants.
- `pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go` — wires the FSStore into `RunEvictionLSL08Suite` with a counting FileBlockStore for the no-FBS-call assertion.
- `pkg/blockstore/local/localtest/eviction_lsl08_suite.go` — the cross-backend LSL-08 conformance sub-suite.

### Modified — `pkg/blockstore/local/`

- `local.go` — removed 5 methods from the `LocalStore` interface.
- `fs/fs.go` — removed `SetSkipFsync` + `skipFsync` field, removed `MarkBlock*` + `transitionBlockState`, removed `GetDirtyBlocks`. Added LRU fields (`lruMu`, `lruIndex`, `lruList`). Added LRU helpers (`lruTouch`, `lruEvictOne`, `seedLRUFromDisk`) and `lruEntry` type. Added `errLRUEmpty` sentinel. Imports: added `container/list`, `encoding/hex`, `sort`.
- `fs/flush.go` — rewrote `flushBlock` for stage-and-release with `bytes.Clone` snapshot, tmp+rename+fsync outside the lock, writeGen-aware post-write phase. Removed the `syncFile` helper (no longer used; per-flush fsync is now unconditional inside flushBlock).
- `fs/block.go` — added `writeGen uint64` field to `memBlock`.
- `fs/write.go` — bump `mb.writeGen++` on every WriteAt mutation under lock.
- `fs/eviction.go` — full rewrite. Removed `evictBlock`, `evictOneTTL`, `evictOneLRU`, `collectRemoteCandidates` (legacy `FileBlock`-driven path). New `ensureSpace` is LRU-only. Retained `extractPayloadID`, `fileOrFallbackSize`, `recalcDiskUsed` for callers in manage.go.
- `fs/chunkstore.go` — wired `lruTouch` into `StoreChunk` (post-rename) and `ReadChunk` (post-read). `DeleteChunk` prunes the LRU entry under lruMu.
- `fs/appendwrite.go` — dropped the `if !bc.skipFsync { ... }` gate around log fsync; fsync is unconditional now.
- `fs/test_hooks.go` — added `EnsureSpaceForTest`, `ChunkPathForTest`, `SeedLRUFromDiskForTest`, `FBSCounter` interface, `ResetFBSCallCounterForTest`, `FBSCallCountForTest`.
- `fs/eviction_test.go` — rewrote from scratch around the new `TestLSL08_*` suite; removed legacy `populateRemoteBlock`-driven tests.
- `fs/fs_test.go` — added `ResetCount`/`TotalCount` to `countingFileBlockStore` to satisfy `FBSCounter`. Updated `TestLocalWritePath_NoFileBlockStoreCall/eviction_path` to use `StoreChunk`.
- `fs/manage_test.go` — updated `TestManageSetEvictionReEnabled` to seed via `StoreChunk` + LRU.
- `memory/memory.go` — removed `MarkBlockRemote`, `MarkBlockSyncing`, `MarkBlockLocal`, `GetDirtyBlocks`, `SetSkipFsync` and the `skipFsync` field.
- `localtest/suite.go` — removed `testMarkBlockState`, `testGetDirtyBlocks`, and their `t.Run` registry entries.
- `localtest/eviction_lsl08_suite.go` — new sub-suite (5 scenarios).

### Modified — controlplane

- `pkg/controlplane/runtime/shares/service.go` — dropped `localStore.SetSkipFsync(remoteStore != nil)`. Replaced with a comment noting the LSL-07 removal.

## Decisions Made

1. **LocalStore method count is 23, not 17.** The plan's `<done>` assertion `awk '/^type LocalStore interface \{/,/^\}/' pkg/blockstore/local/local.go | grep -cE "^\s+[A-Z][a-zA-Z]+\("` expecting 17 was based on a 22-method count from PATTERNS.md. The actual interface had 28 methods (drift from earlier phases adding `Stats`, `ListFiles`, `GetStoredFileSize`, `ExistsOnDisk`, `Healthcheck`, etc.). I dropped exactly the 5 methods the plan listed (LSL-07 spec target), landing at 23. The 5-method delta is the load-bearing invariant.

2. **writeGen counter is required for stage-and-release correctness.** Without it, a writer that mutates `mb.data` during the disk I/O window would have its bytes silently lost when the post-write phase nils mb.data unconditionally (the plan's example sketch had this bug). The counter lets flushBlock detect interleaved writes and preserve them — matching the plan's "handle a brief intermediate state" guidance in step 2 of the action.

3. **Per-flush fsync is now unconditional.** The plan's flushBlock example explicitly includes `f.Sync()`, and removing `SetSkipFsync` leaves no production toggle. Local-disk durability is baseline; the eviction-then-refetch path keeps S3-backed deployments correct even on power loss.

4. **Tmp+rename atomicity is mandatory.** The plan's example uses `.tmp` + `os.Rename`; mirroring chunkstore's pattern. The fdPool is evicted before the rename so cached fds (which point at the pre-rename inode) are invalidated cleanly.

5. **FBSCounter is an exported interface.** Cross-package test wrappers (e.g., `fs_test.countingFBSWrapper` in the conformance test, `fs.countingFileBlockStore` in fs_test.go) need to satisfy it. With unexported method names, the type-assertion would only match wrappers in package fs, defeating the cross-package conformance suite.

6. **DeleteChunk prunes LRU.** Mark-sweep GC (Phase 11-06) calls DeleteChunk; without LRU pruning the index would carry stale entries that lruEvictOne later tries to unlink. The unlink would silently succeed-as-ENOENT, but the cleaner accounting is worth the four-line addition.

7. **Cold-start LRU seed is alphabetical by hash hex.** `seedLRUFromDisk` walks `<baseDir>/blocks/` then sorts by `hash.String()` before populating the LRU. Reproducible cold-start order makes recovery-then-evict tests deterministic across runs and platforms.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] memBlock writeGen counter for stage-and-release correctness**

- **Found during:** Task 1 GREEN design review.
- **Issue:** The plan's example sketch for the post-write phase unconditionally clears `mb.data = nil`. With stage-and-release, a writer can mutate `mb.data` during the disk I/O window (after stage releases the lock, before post-write reacquires it). Unconditional clearing would lose those writes silently.
- **Fix:** Added `writeGen uint64` field to `memBlock`. WriteAt bumps it under lock. flushBlock stages it; post-write only clears mb.data if writeGen is unchanged. If a writer interleaved, mb.dirty stays true and the next flush picks up the new bytes.
- **Files modified:** `pkg/blockstore/local/fs/block.go`, `pkg/blockstore/local/fs/write.go`, `pkg/blockstore/local/fs/flush.go`.
- **Committed in:** `d7860513` (Task 1 GREEN).

**2. [Rule 1 - Bug] Removed skipFsync field entirely (not just SetSkipFsync method)**

- **Found during:** Task 2 — auditing the field's other users.
- **Issue:** The plan named only the `SetSkipFsync` method for removal, but the `skipFsync` field was also referenced in `flush.go` (per-block fsync gate) and `appendwrite.go` (log fsync gate). Keeping the field with no public setter would leave it permanently false — a wart that confuses future readers about whether durability is configurable.
- **Fix:** Deleted the field from both FSStore and MemoryStore. Per-flush fsync and per-log fsync are now unconditional.
- **Files modified:** `pkg/blockstore/local/fs/fs.go`, `pkg/blockstore/local/fs/flush.go`, `pkg/blockstore/local/fs/appendwrite.go`, `pkg/blockstore/local/memory/memory.go`.
- **Committed in:** `ffe88c47` (Task 2).

**3. [Rule 3 - Blocking] Updated legacy eviction tests for LSL-08 LRU model**

- **Found during:** Task 3 — running tests after rewriting `ensureSpace`.
- **Issue:** `eviction_test.go` had 6 `TestEviction_*` tests that seeded blocks via `populateRemoteBlock` (FileBlockStore-driven) and called `ensureSpace`. After LSL-08, ensureSpace only sees LRU-tracked CAS chunks; legacy `.blk` files are not evictable. Same for `TestManageSetEvictionReEnabled` (manage_test.go) and `TestLocalWritePath_NoFileBlockStoreCall/eviction_path` (fs_test.go).
- **Fix:** Rewrote `eviction_test.go` end-to-end with `TestLSL08_*` tests using `storeChunk` (the canonical CAS write path). Updated the other two tests to use `StoreChunk` instead of `populateRemoteBlock`.
- **Files modified:** `pkg/blockstore/local/fs/eviction_test.go`, `pkg/blockstore/local/fs/fs_test.go`, `pkg/blockstore/local/fs/manage_test.go`.
- **Committed in:** Task 3 commit (LSL-08).

**4. [Rule 1 - Bug] LSL-08 conformance test race in concurrent writes scenario**

- **Found during:** Task 3 first run of `TestLSL08_ConcurrentWritesSafe` under `-race`.
- **Issue:** Original test used `t.Fatalf` from the writer goroutine — Go testing prohibits `t.Fatalf` outside the test's main goroutine, and the data race detector flagged unprotected access to the shared `hashes` slice from both writer and reader.
- **Fix:** Writer returns silently on error; reader and writer share `hashes` under a `sync.Mutex`.
- **Files modified:** `pkg/blockstore/local/fs/eviction_test.go`, `pkg/blockstore/local/localtest/eviction_lsl08_suite.go`.
- **Committed in:** Task 3 commit (LSL-08).

**Total deviations:** 4 auto-fixed (no user permission needed; all fall under Rules 1 and 3).

## Issues Encountered

### Task 3 commit blocked transiently on SSH signing agent

After landing the Task 3 implementation and verifying all tests pass with `-race`, the SSH signing agent (1Password) intermittently returned "agent refused operation?" / "communication with agent failed?" for several minutes — likely waiting for the user to approve the signing request in the 1Password GUI. Per CLAUDE.md ("Always sign commits — never fall back"), the commit was retried persistently until the agent recovered. The Task 3 commit landed as `05c71140` once the agent allowed the signing operation; no fallback to unsigned commits was used.

## Threat Flags

None new beyond the plan's threat register. The implementation honors all 5 mitigations:

- **T-11-B-07** (torn write under stage-and-release): mitigated by `bytes.Clone` snapshot + writeGen-aware post-write. Test `TestFlushBlock_OnDiskMatchesStagedSnapshot` verifies disk content matches a snapshot consistently (no torn mix) under concurrent mutation.
- **T-11-B-08** (LRU eviction races with concurrent reads): mitigated by `bc.lruMu` serialization. ENOENT from a concurrent ReadChunk against an evicted file is the correct fall-through to the engine refetch path.
- **T-11-B-09** (stranded chunk files after partial eviction): accepted — `lruEvictOne` re-inserts the entry on os.Remove failure to keep bookkeeping consistent; truly stranded files are an operator concern.
- **T-11-B-10** (LRU map corruption under concurrent writers): mitigated by `bc.lruMu`. `-race` testing in `TestLSL08_ConcurrentWritesSafe` and the conformance suite's `eviction_concurrent_writes_safe` scenario.
- **T-11-B-11** (LSL-08 hot path leaks into FileBlockStore): mitigated by the load-bearing `eviction_no_fbs_calls` assertion in the conformance suite. The wrapper counts ALL FileBlockStore method calls during ensureSpace and fails on any non-zero count.

## Next Plan Readiness

- **Plan 11-05 (restart recovery)** can build on the LRU seed-on-startup pattern; the seed already runs in New(). If 11-05 needs to re-seed after recovery (e.g., after orphan log sweep adds new chunks), `SeedLRUFromDiskForTest`'s production sibling `seedLRUFromDisk()` is callable.
- **Plan 11-06 (GC mark-sweep)** can call DeleteChunk knowing the LRU is pruned in lockstep. The mark phase doesn't touch the LRU; the sweep phase removes chunks via DeleteChunk which transparently keeps the LRU consistent.
- **Plan 11-08 (E2E)** can rely on the unconditional fsync — no S3-mode skipFsync escape hatch to remember.

## Self-Check: PASSED

- `pkg/blockstore/local/fs/flush.go` — exists, contains `bytes.Clone` (line ~99), two `mb.mu.Unlock()` calls (lines ~94 and ~166), and `os.OpenFile`/`f.Write`/`f.Sync` after the first `mb.mu.Unlock`. ✓
- `pkg/blockstore/local/fs/eviction.go` — exists, `grep -nE "FileBlockStore|fileBlockStore"` returns nothing. ✓
- `pkg/blockstore/local/fs/eviction_lsl08_conformance_test.go` — exists, wires RunEvictionLSL08Suite. ✓
- `pkg/blockstore/local/localtest/eviction_lsl08_suite.go` — exists, declares `RunEvictionLSL08Suite` and `eviction_no_fbs_calls`. ✓
- `pkg/blockstore/local/local.go` — `MarkBlockRemote`, `MarkBlockSyncing`, `MarkBlockLocal`, `GetDirtyBlocks`, `SetSkipFsync` all gone. ✓
- `grep -rnE "MarkBlockRemote|MarkBlockSyncing|MarkBlockLocal|GetDirtyBlocks|SetSkipFsync" --include='*.go'` returns only doc-comment hits in `local.go:27`, `fs.go:410`, `flush.go:23`, `service.go:451` — no remaining method definitions or call sites. ✓
- `go build ./...` — exits 0. ✓
- `go vet ./...` — exits 0. ✓
- `go test -count=1 -race ./pkg/blockstore/local/...` — exits 0 (with `ulimit -n 8192` to avoid the macOS fd limit hit by the parallel `-race` runs). ✓
- `go test -count=1 -run AppendLog ./pkg/blockstore/local/localtest/... ./pkg/blockstore/local/fs/...` — exits 0 (INV-05 confirmation). ✓
- Commits — `cf637bf2` (Task 1 RED), `d7860513` (Task 1 GREEN), `ffe88c47` (Task 2), `05c71140` (Task 3) all present and pass `git verify-commit`.

---
*Phase: 11-cas-write-path-gc-rewrite-a2*
*Completed: 2026-04-25*

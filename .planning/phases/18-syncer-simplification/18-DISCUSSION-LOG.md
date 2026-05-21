# Phase 18: Syncer simplification + ObjectID relocation - Discussion Log

**Date:** 2026-05-21
**Areas discussed:** 5 (ListUnsynced API shape, MarkSynced persistence + crash semantics, TrySpeculativeFileLevelDedup new home, Test rewrite + Phase 17 deferred follow-ups, Code structure and deprecations)

## Area 1: ListUnsynced API shape

**Question:** Where does hash-keyed unsynced state live? Current FileBlock.BlockState is path-keyed and dedup breaks it.
**Options:** local BlockStore-owned hash set / metadata-store FileBlock state / dedicated sync_state table
**User pushback:** "Don't like a separate DB just for this; also like decoupling. How would you suggest? What about badger/postgres stores?"
**Resolution:** Surfaced existing `metadata.RollupStore` pattern (`pkg/metadata/rollup_store.go`) — small interface fulfilled by each metadata backend (badger/postgres/memory). Not a separate DB; same backend, separate logical bucket. Postgres = small table, Badger = key prefix, memory = map.
**Selected:** SyncedHashStore interface mirroring RollupStore.

**Question:** ListUnsynced return shape — iter.Seq2, paginated slice, or channel?
**Selected:** `iter.Seq2[ContentHash, error]` (Go 1.23 push iterator).

**Question:** Concurrency vs concurrent rollup writes.
**Selected:** Snapshot at iterator start (new chunks picked up next pass).

## Area 2: MarkSynced persistence + crash semantics

**Question:** Crash window between remote.Put and MarkSynced.
**Selected:** Put-then-Mark, idempotent replay. remote.Put is idempotent on (hash, identical bytes); MarkSynced fires only on Put success.

**Question:** Refcount cascade on engine.Delete when last FileBlock referencing hash is removed.
**Selected:** Yes — cascade `syncedStore.DeleteSynced(hash)` from engine.Delete when local chunk is deleted. Keeps synced set as subset of local CAS.

**Question:** Filter cost — per-hash IsSynced vs batched set difference.
**Selected:** Per-hash IsSynced (O(1) backend lookup; matches Walk callback shape).

## Area 3: TrySpeculativeFileLevelDedup new home

**Question:** Spec says "adapter/engine layer (pre-rollup)". Where exactly?
**Selected:** engine.Flush() pre-rollup hook (adapter is wrong layer — doesn't know ObjectID; Phase 19 Opt-4 is different mechanic, can't supersede).

**Question:** Phase 13 conformance tests — rewrite or hide?
**Selected:** Rewrite tests to target engine.Flush surface.

## Area 4: Test rewrite + Phase 17 deferred follow-ups

**Question:** Recreate stale Phase 17 integration-tagged syncer_test.go on unified Put/Get path now?
**Selected:** Yes — fold into Phase 18 as `syncer_mirror_test.go` (integration-tagged). Closes Phase 17 deferred follow-up.

**Question:** Phase 13 dedup test rewrite scope — minimum named test or full sweep?
**Selected:** Sweep all Phase 13 dedup conformance to avoid partial CI red.

**Question:** PR shape — mega-PR or split?
**Selected:** Mega-PR (matches Phase 17 shape; project rule forbids flag-gated half-states).

## Area 5: Code structure and deprecations

**Question:** Syncer file fate after ~600 → ~50 LoC collapse.
**Selected:** Keep `syncer.go` in place — git blame preserved, Syncer struct retains auxiliary state.

**Question:** `engine/dedup.go` after public seam moves.
**Selected:** Keep file, retarget public seam (`TrySpeculativeFileLevelDedup` exported deleted; private function becomes engine.Flush's hook).

**Question:** Further narrow LocalStore beyond the 7 TRANSITIONAL methods?
**Selected:** No — keep admin-superset. Lifecycle methods (Healthcheck, Stats, EvictMemory, retention, etc.) are legitimate boot/observability surface, not transitional.

**Question:** Grep marker convention for Phase 19+ deferrals.
**Selected:** `TRANSITIONAL-NEXT-MILESTONE:` generic forward-pointer. Document in `pkg/blockstore/doc.go`.

## Deferred Ideas (not Phase 18)

- Eager small-file dedup (Phase 19 Opt 4)
- In-memory hash dedup LRU (Phase 19 Opt 1)
- SyncedHashStore observability (Count/Stats)
- Per-share sync_state isolation
- Speculative chunk lookahead (v0.17+)

## Claude's Discretion (planner-decided)

- Exact LoC delta + per-file boundary between staged commit waves
- claimBatch worker pool bounded-by-config vs fixed-N
- SyncedHashStore Stats() / Count() addition
- File organization for refcount-cascade DeleteSynced unit test
- `iter.Seq2` import path naming conventions

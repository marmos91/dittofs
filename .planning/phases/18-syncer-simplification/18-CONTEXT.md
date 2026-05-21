# Phase 18: Syncer simplification + ObjectID relocation - Context

**Gathered:** 2026-05-21
**Status:** Ready for planning

<domain>
## Phase Boundary

Rewrite the Syncer (`pkg/blockstore/engine/syncer.go`) from a per-block chunk-and-upload orchestrator into a byte-identical local→remote mirror loop:

```go
for hash := range local.ListUnsynced(ctx) {
    data, _ := local.Get(ctx, hash)
    remote.Put(ctx, hash, data)
    syncedStore.MarkSynced(ctx, hash)
}
```

Move `ComputeObjectID` out of Syncer and into `pkg/blockstore/local/fs/rollup.go`'s `CommitChunks` post-hook so local-only shares get ObjectIDs too (was a real surprise during Phase 13 UAT). Move `TrySpeculativeFileLevelDedup` to `engine.Flush()` as a pre-rollup hook. Delete the 7 TRANSITIONAL-PHASE-18 admin methods on `LocalStore` (the bridge surface Phase 17 retained for atomic deletion in this phase) plus `Flush` + `FlushedBlock`.

Add a new `metadata.SyncedHashStore` interface mirroring the existing `metadata.RollupStore` pattern (3 methods: `IsSynced`, `MarkSynced`, `DeleteSynced`) implemented across `badger`, `postgres`, `memory` metadata backends and injected into `FSStore` via `FSStoreOptions.SyncedHashStore`. Local sync state lives in whichever metadata backend the operator already chose — no new infrastructure.

This is Phase 3 of 4 in v0.16.0 (CAS Convergence). Depends on Phase 17 (Unified BlockStore, shipped PR #527, merged commit `d225926f`). Unblocks Phase 19 (Write-path RAM optimizations).

**In scope:** Syncer Flush body rewrite as mirror loop; ListUnsynced + MarkSynced + DeleteSynced surface (LocalStore + SyncedHashStore split); ObjectID compute relocation to rollup CommitChunks; TrySpeculativeFileLevelDedup relocation to engine.Flush pre-rollup hook; deletion of `uploadOne`, `drainPayloadToRemote`, `persistFileBlocksAfterFlush`, the in-Syncer BLAKE3 recompute at upload.go:86; deletion of the 7 TRANSITIONAL-PHASE-18 LocalStore methods + `Flush` + `FlushedBlock` + `Syncer.TrySpeculativeFileLevelDedup` public seam; rewrite Phase 13 dedup conformance tests onto the new entrypoint; re-create `pkg/blockstore/engine/syncer_test.go` (integration-tagged) on unified Put/Get path.

**Out of scope:** write-path RAM optimizations (Phase 19); cold-cache benchmarks (deferred to v0.17+); any backward-compat shim or feature flag (project rule — no DittoFS prod users; major releases delete legacy in-line); further LocalStore narrowing beyond the 7 transitional methods + Flush/FlushedBlock; deletion of admin-superset methods (Truncate, EvictMemory, SetRetentionPolicy, SetEvictionEnabled, Stats, ListFiles, GetStoredFileSize, Healthcheck, SyncFileBlocks, SyncFileBlocksForFile, Start, Close, DeleteAppendLog) — those are legitimate boot/observability surface, not transitional.

</domain>

<decisions>
## Implementation Decisions

### PR shape
- **D-01:** Phase 18 ships as a single mega-PR, matching Phase 17's atomic-merge shape. SyncedHashStore additions across 3 backends + Syncer rewrite + ObjectID relocation + TrySpeculativeFileLevelDedup relocation + 7 TRANSITIONAL-PHASE-18 deletions + Flush/FlushedBlock deletions + Phase 13 + Phase 17 test reshape all land together. Internal commit ordering may stage (additive interfaces → consumers migrated → deletions) for `git log -p` reviewability, but no commit may leave develop unbuildable and no flag-gated half-state is permitted. Splitting into `additive + deletive` PRs is rejected because PR 1 would ship dead code until PR 2 lands — contradicts the v0.16.0 spec's "no flag-gated half-states" rule.

### Sync state storage
- **D-02:** Sync state lives in a new `metadata.SyncedHashStore` interface mirroring the existing `metadata.RollupStore` (`pkg/metadata/rollup_store.go`). 3 methods: `IsSynced(ctx, hash) (bool, error)`, `MarkSynced(ctx, hash) error`, `DeleteSynced(ctx, hash) error`. Implemented on every existing metadata backend (`badger`, `postgres`, `memory`). Injected into `*fs.FSStore` via `FSStoreOptions.SyncedHashStore`. Postgres = one tiny table (`synced_hashes (hash bytea PRIMARY KEY, synced_at timestamptz)`); Badger = key prefix `synced/<hex-hash>` with empty value or `synced_at` int64; memory = `map[ContentHash]time.Time`. No new infrastructure — same DB the operator already chose. Decouples Syncer from FileBlock metadata while reusing a proven injection pattern.
- **D-03:** Rejected alternatives: (a) tracking on existing `FileBlock.BlockState` — requires Syncer to dedup by hash in-memory each pass since dedup means N FileBlocks share 1 hash, and couples sync state to the metadata layer Phase 17 was trying to reduce; (b) on-disk sentinel file per hash in local CAS dir — adds disk noise and Walk overhead at scale; (c) separate sync_state.db — sprawl without benefit.

### ListUnsynced API shape
- **D-04:** `local.LocalStore` exposes `ListUnsynced(ctx context.Context) iter.Seq2[ContentHash, error]` (Go 1.23 push iterator). Backend implementation: `Walk()` over local CAS chunks + filter each hash via `SyncedHashStore.IsSynced`. Yields only hashes present locally but not marked synced. Caller decides when to stop iterating; backend streams without materializing the full set. Matches Walk/range idioms already established in Phase 17. Crash mid-iteration is safe because `MarkSynced` is idempotent.
- **D-05:** Concurrency vs concurrent rollup writes: snapshot at iterator start. The iterator captures the hash set existing at iteration begin; new chunks rollup produces mid-iteration are picked up on the NEXT Syncer pass. Simple, no torn state, no infinite loops on hot-write workloads. Rejected: live tail catch-up (lower latency for fresh chunks but iteration may never terminate on busy stores).
- **D-06:** Filter cost is `O(N)` per-hash IsSynced calls during Walk. Acceptable because hash-keyed lookup is `O(1)` in all 3 backends (Badger keyed Get, Postgres index seek, memory map lookup). Rejected: `ListSyncedHashes` + in-Syncer set difference — wins only at very large N and adds interface surface.

### Mirror-loop ordering and crash semantics
- **D-07:** Order is `remote.Put(hash, data)` THEN `syncedStore.MarkSynced(hash)`. Crash between them → next Syncer pass re-Puts the same hash. `remote.Put` is idempotent on `(hash, identical bytes)` per the unified BlockStore contract; duplicate Put wastes bandwidth but never corrupts. MarkSynced fires only after Put returns nil. No transaction across stores needed.
- **D-08:** Rejected alternatives: (a) Mark-then-Put with rollback on error — opens a worse crash window (marked-synced but never uploaded → silent drift); (b) two-phase commit across block + metadata stores — complex, undermines the decoupling D-02 establishes.
- **D-09:** Refcount = 0 (last `FileBlock` referencing a hash deleted, `engine.Delete` invokes `DecrementRefCount` and the local chunk is deleted via `DeleteChunk`): cascade `syncedStore.DeleteSynced(hash)` from engine.Delete in the same critical section that fires DeleteChunk. Keeps the synced set as a strict subset of local CAS contents. Rejected: append-only sync rows + periodic janitor sweep — adds lag and a janitor goroutine; eager cascade is simpler and matches existing engine.Delete shape.

### ObjectID relocation
- **D-10:** `ComputeObjectID` moves to `pkg/blockstore/local/fs/rollup.go`. After `rollupStore.SetRollupOffset(payloadID, targetPos)` returns nil, the rollup commit hook calls `coordinator.PersistFileBlocks(payloadID, blocks, ComputeObjectID(blocks))`. ObjectID is now computed at rollup completion (when the BlockRef manifest stabilizes), NOT at remote upload completion. Local-only shares get ObjectIDs.
- **D-11:** Deletions in `pkg/blockstore/engine/syncer.go`: `persistFileBlocksAfterFlush` (line ~251), the ObjectID compute call site, the BLAKE3 recompute at `upload.go:86`. Phase 13 `Syncer.Flush_InvokesPostFlushHook` test rewrites to `TestRollup_CommitChunks_PersistsObjectID` per v0.16.0 spec mandate.

### TrySpeculativeFileLevelDedup relocation
- **D-12:** Public `Syncer.TrySpeculativeFileLevelDedup` (engine/syncer.go:176) DELETED. Hook relocates to `engine.Flush()` pre-rollup: engine.Flush calls into the existing private `trySpeculativeFileLevelDedup` (engine/dedup.go) before triggering rollup. Adapter (NFS/SMB) layer is the wrong home — adapters don't know about ObjectID. `pkg/blockstore/engine/dedup.go` stays as the file; only the public seam moves up to engine.Flush.
- **D-13:** Phase 13 dedup conformance tests retarget the new `engine.Flush` entrypoint. Same scenarios (BSCAS-05 hit/miss/race), different caller. Rejected: private internal test via `dedup_for_test.go` — testing internal logic privately is a smell.

### Test reshape
- **D-14:** Sweep ALL Phase 13 dedup conformance tests, not just the spec-named `Syncer.Flush_InvokesPostFlushHook`. Every test that touches `Syncer.TrySpeculativeFileLevelDedup`, `persistFileBlocksAfterFlush`, or the in-Syncer ObjectID compute gets ported to the new entrypoint in one cleanup pass. Avoids partial-state CI red on neighbor tests.
- **D-15:** Re-create `pkg/blockstore/engine/syncer_test.go` (`//go:build integration`) on the unified Put/Get path. New scenarios: mirror-loop happy path, crash-replay window (Put-then-Mark), ListUnsynced snapshot semantics, refcount cascade DeleteSynced. Exercises s3 + memory remote backends via the unified RemoteStore surface Phase 17 landed. This closes the Phase 17 deferred follow-up surfaced in 17-VERIFICATION.md.

### Code structure
- **D-16:** `pkg/blockstore/engine/syncer.go` keeps its filename. Body shrinks from ~600 LoC orchestrator to ~50 LoC mirror loop + ListUnsynced consumer + MarkSynced caller. The `Syncer` struct keeps its name and retains the periodic uploader goroutine, claimBatch worker pool (still useful for parallel uploads with pre-computed hashes), `uploading` atomic gate against periodic-vs-explicit-drain races, health monitor, and backpressure-on-remote-outages logic. Git blame preserved. Rejected: rename to `mirror.go` (file/struct mismatch); inline into `engine.go` (loses the Syncer struct's auxiliary state boundary).
- **D-17:** `pkg/blockstore/engine/dedup.go` keeps its filename and houses the private `trySpeculativeFileLevelDedup`. The public `TrySpeculativeFileLevelDedup` exported seam deletes; engine.Flush calls the private function directly.
- **D-18:** Keep `LocalStore` as a BlockStoreAppend admin-superset. Phase 18 deletes ONLY the 7 TRANSITIONAL-PHASE-18 methods (`ReadAt`, `WriteAt`, `Flush`, `IsBlockLocal`, `GetBlockData`, `WriteFromRemote`, `DeleteAllBlockFiles`) + `FlushedBlock` + the bridge `Flush` return type. Lifecycle/admin methods (Truncate, EvictMemory, SetRetentionPolicy, SetEvictionEnabled, Stats, ListFiles, GetStoredFileSize, Healthcheck, SyncFileBlocks, SyncFileBlocksForFile, Start, Close, DeleteAppendLog) STAY — legitimate boot/observability surface, not transitional. Rejected: split into `BlockStoreAppend + LocalStoreAdmin` — all production callers want both.

### Deprecation marker convention
- **D-19:** Establish `TRANSITIONAL-NEXT-MILESTONE:` as the generic forward-pointer grep marker for any deferral discovered mid-Phase-18. Generic (not version-pinned) avoids stale markers if subsequent milestones reshuffle. Cleanup sweep happens at next major milestone planning. Document the convention in `pkg/blockstore/doc.go` alongside the existing TRANSITIONAL-PHASE-18: explanation Phase 17 added.

### Claude's discretion
- Exact LoC delta and per-file boundary between the staged commit waves (additive → migrate → delete) — planner's call; the only hard rule is each wave keeps `go build ./... + go vet ./... + go test ./...` green.
- Whether the `claimBatch` worker pool stays bounded by a config knob or fixed-N internal — planner decides based on existing `SyncerConfig`.
- Whether `metadata.SyncedHashStore` adds a `Stats()` or `Count()` method for observability — defer to planner per existing RollupStore precedent.
- Whether to fold a small refcount-cascade-DeleteSynced unit test into `engine_test.go` or a new `engine_delete_test.go` — file-organization detail.
- Exact `iter.Seq2` import path naming conventions in the LocalStore interface declaration — go idiom: `iter.Seq2[blockstore.ContentHash, error]`.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### v0.16.0 design (locked spec — SOURCE OF TRUTH)
- `~/.claude/plans/reactive-sprouting-moonbeam.md` §"Decision 3 — Syncer is a byte-identical local→remote mirror" (lines 129–166) + §"Sequencing" Phase 18 row (line 224) + §"Critical Files (modified) — Syncer (Decision 3, Phase 18)" (line 258) — locks mirror-loop intent, ObjectID compute location, deletion list, test rewrite name.
- `.planning/ROADMAP.md` line 47 (Phase 18 entry) + lines 359–415 (Phase 17 detail block carrying the v0.16.0 spec carry-forward).

### Phase 17 carry-forward (already shipped, merged commit `d225926f`)
- `.planning/phases/17-unified-blockstore/17-CONTEXT.md` §`<decisions>` — D-01 mega-PR shape, D-07 ErrStopWalk Walk semantics, D-08 Meta struct shape, D-09 conformance two-entrypoint, D-11 boot guard exit 78. These constrain Phase 18's interaction with the unified surface.
- `.planning/phases/17-unified-blockstore/17-VERIFICATION.md` — "Deferred follow-up" block on stale integration-tagged `pkg/blockstore/engine/syncer_test.go` is consumed by D-15.

### Phase 13 (file-level dedup carry-forward)
- `.planning/phases/13-merkle-root-file-level-dedup-a4/` — Phase 13 test surfaces (TrySpeculativeFileLevelDedup conformance, persistFileBlocksAfterFlush hook test) are reshaped per D-13 + D-14.

### Pattern references (mirror this in implementation)
- `pkg/metadata/rollup_store.go` — `RollupStore` interface shape + 3-backend implementation pattern. D-02 mirrors this for `SyncedHashStore`.
- `pkg/blockstore/local/fs/fs.go` lines 201, 510–513, 565 — `RollupStore` injection through `FSStoreOptions`. D-02 mirrors this for `SyncedHashStore`.
- `pkg/blockstore/local/fs/rollup.go` line 35 (`StartRollup`) + the `chunkRollupWorker` body — where ObjectID compute (D-10) hooks in post-CommitChunks.
- `pkg/blockstore/engine/syncer.go` lines 47, 151, 176, 374 (TrySpeculativeFileLevelDedup) + line 251 (persistFileBlocksAfterFlush) + line ~600 (Flush body to collapse) — primary deletion + relocation targets.
- `pkg/blockstore/local/local.go` lines 139–193 — the 7 TRANSITIONAL-PHASE-18 methods + FlushedBlock — deletion targets.

### Project conventions
- `~/.claude/projects/-Users-marmos91-Projects-dittofs/memory/feedback_no_prod_users_delete_eagerly.md` — no DittoFS prod users; major releases delete legacy in-line. Validates D-01 (no split, no flag-gate).
- `~/.claude/projects/-Users-marmos91-Projects-dittofs/memory/feedback_sign_commits.md` — sign all commits with `-S`.
- `~/.claude/projects/-Users-marmos91-Projects-dittofs/memory/feedback_no_phase_comments_in_code.md` — phase/decision IDs stay in `.planning/` only; never in godoc, comments, test names, BENCHMARKS.md.

</canonical_refs>

<code_context>
## Reusable Assets and Patterns

### Already-built (reuse, do not reinvent)
- **`metadata.RollupStore`** (`pkg/metadata/rollup_store.go`) — proven small-interface-injection pattern with 3 backends + 1 conformance suite. `SyncedHashStore` (D-02) is a structural clone.
- **`blockstore.ContentHash`** + **`Meta{Size, LastModified}`** (`pkg/blockstore/blockstore.go` + `pkg/blockstore/types.go`) — Phase 17 unified type surface. ListUnsynced (D-04) emits ContentHash directly.
- **`local.LocalStore.Walk`** (`pkg/blockstore/local/fs/blockstore_methods.go`) — Phase 17 unified Walk surface. ListUnsynced's implementation walks local CAS + filters via SyncedHashStore.IsSynced.
- **`remote.RemoteStore.Put`** + **`remote.RemoteStore.Get`** (`pkg/blockstore/remote/remote.go`) — Phase 17 renamed surface. Mirror loop calls these directly; no chunking, no hashing, no metadata writes inside.
- **`fs.FSStoreOptions`** (`pkg/blockstore/local/fs/fs.go:510`) — injection seam for SyncedHashStore (mirrors existing RollupStore slot).
- **`engine.Cache`** + **`engine.Syncer`** — keep auxiliary state (periodic uploader, claimBatch worker pool, uploading atomic gate, health monitor).

### Integration points to retarget
- `pkg/blockstore/engine/engine.go::Flush` — pre-rollup hook for relocated `trySpeculativeFileLevelDedup` (D-12).
- `pkg/blockstore/local/fs/rollup.go::rollupFile` — post-CommitChunks hook for `ComputeObjectID` + `coordinator.PersistFileBlocks` (D-10).
- `pkg/blockstore/engine/engine.go::Delete` — cascade `syncedStore.DeleteSynced(hash)` when DeleteChunk fires (D-09).

### Anti-patterns to avoid
- Inline ContentHash dedup in Syncer mid-iteration — handled by D-02's hash-keyed sync state.
- Transaction spanning block store + metadata store — D-07 idempotent-replay avoids this.
- Two-phase commit on Put-then-Mark — D-07 rejected.
- Live-tail ListUnsynced iterator — D-05 rejected (infinite-loop risk).
- Reintroducing FileBlock.BlockState as the sync-tracking primitive — D-03 rejected (dedup races and metadata coupling).
- Comments / godoc / test names mentioning "Phase 18" or "D-NN" — project convention forbids it (see canonical_refs).

</code_context>

<deferred>
## Noted for Later

- **Eager small-file dedup (Phase 19 Opt 4)** — file-level dedup short-circuit at NFS COMMIT for files ≤ FastCDC min_chunk. Different mechanic from TrySpeculativeFileLevelDedup; tracked in v0.16.0 spec §"Opt 4 — Eager small-file dedup" — already in Phase 19 roadmap, no new tracking needed.
- **In-memory hash dedup LRU (Phase 19 Opt 1)** — N-slot hash LRU between FastCDC `Next()` and `StoreChunk` for VM-disk overwrites with idempotent bytes. Already in Phase 19 roadmap.
- **`SyncedHashStore.Count()` / `.Stats()` observability** — defer until a real consumer (operator surface, telemetry) needs it.
- **Per-share sync_state isolation** — current design is hash-global (one SyncedHashStore per metadata backend, shared across shares). A future per-share sync state would let operators rebalance shares across remotes independently; tracked here, no Phase 18 work.
- **Speculative chunk lookahead** — v0.16.0 spec §"Deferred to v0.17+" — not Phase 18.

</deferred>

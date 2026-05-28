---
phase: 18-syncer-simplification
verified: 2026-05-21T13:30:00Z
status: gaps_found
score: 10/12 must-haves verified
overrides_applied: 0
gaps:
  - truth: "Zero 'Phase 18' / 'D-NN' / '.planning' provenance leakage in source files"
    status: failed
    reason: "Phase 18-08 sweep introduced 5 new 'Phase 18-08 sweep' / 'After Phase 18' references in production + test godoc, violating the project-wide convention documented in feedback_no_phase_comments_in_code.md (GSD metadata stays in .planning/, never godoc/comments/test names). One new 'D-07' reference also added in syncer_put_error_test.go."
    artifacts:
      - path: "pkg/blockstore/engine/engine.go:505"
        issue: "Comment introduced in commit 5f5d31c0 reads 'After Phase 18 there are no remaining legacy .blk files'"
      - path: "pkg/blockstore/engine/api_blockref_test.go:213-215"
        issue: "Comment introduced in commit f9faa9d4 reads 'removed in the Phase 18-08 sweep'"
      - path: "pkg/blockstore/engine/engine_test.go:386"
        issue: "Comment introduced in commit f9faa9d4 reads 'Removed in the Phase 18-08 sweep'"
      - path: "pkg/blockstore/engine/engine_offline_test.go:101-102,155"
        issue: "Two comments introduced in commit f9faa9d4 reference 'Phase 18-08 sweep'"
      - path: "pkg/blockstore/engine/syncer_put_error_test.go:72"
        issue: "Comment introduced in commit f9faa9d4 reads 'pins the crash-safety contract in D-07'"
    missing:
      - "Rewrite the 5 phase-named comments to describe the invariant in milestone-agnostic terms (e.g. 'Under the unified CAS surface…' rather than 'After Phase 18 / in the Phase 18-08 sweep…')"
      - "Replace the 'D-07' reference with a description of the put-then-mark crash window (the comment already describes it correctly — just drop the 'D-07' tag)"
  - truth: "Auxiliary Syncer state (periodic uploader, claimBatch worker pool, health monitor, backpressure) preserved per D-16"
    status: partial
    reason: "Periodic uploader (syncer.go:585, 699), health monitor (syncer.go:567), uploading atomic gate (syncer.go:273), and backpressure-on-outage logic (syncer.go:721) ARE preserved. However the 'claimBatch worker pool' D-16 promised to retain was actually retired in 18-06 — the SyncQueue + mirror loop replaced it. CONTEXT D-16 wording ('claimBatch worker pool … still useful for parallel uploads') is now stale; one orphan godoc comment in syncer.go:636 still references 'claimBatch'. ClaimBatchSize/ClaimTimeout config fields linger as dead config (syncer.go:106-114; types.go:68,86)."
    artifacts:
      - path: "pkg/blockstore/engine/syncer.go:636"
        issue: "Godoc references a no-longer-existing 'claimBatch' symbol — historical drift"
      - path: "pkg/blockstore/engine/syncer.go:106-114"
        issue: "ClaimBatchSize and ClaimTimeout config defaults applied but no consumer remains (only janitor uses ClaimTimeout at syncer.go:650 — kept legitimately)"
      - path: "pkg/blockstore/engine/types.go:68,86"
        issue: "ClaimBatchSize field defined+defaulted; no consumer in production code"
    missing:
      - "Update CONTEXT D-16 / PATTERNS to mark claimBatch as 'retired in 18-06' OR drop the dead ClaimBatchSize config knob OR document why it survives (operator-facing config still has it)"
      - "Fix the orphan claimBatch godoc reference in syncer.go:636 to describe the actual janitor behaviour"
deferred: []
---

# Phase 18: Syncer Simplification Verification Report

**Phase Goal:** Simplify Syncer to byte-identical local→remote mirror loop, persist sync state in metadata via SyncedHashStore, delete 7 transitional LocalStore admin methods, close Phase 13 UAT ObjectID-at-rollup surprise.

**Verified:** 2026-05-21T13:30:00Z
**HEAD:** `69f9f283` (gsd/phase-18-syncer-simplification)
**Base:** `d225926f` (Phase 17 merge)
**Status:** gaps_found (10/12 — 2 doc-only gaps; functional goal achieved)
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths

| #  | Truth                                                                                                         | Status      | Evidence                                                                                                                                                                                                                                                              |
| -- | ------------------------------------------------------------------------------------------------------------- | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 1  | Syncer.Flush is ~50 LoC mirror loop with no per-block orchestration, no BLAKE3 recompute                       | ✓ VERIFIED  | `pkg/blockstore/engine/syncer.go:248-282` — Flush is 35 LoC. The mirror body lives in `mirrorOnce` (lines 301-324, 24 LoC). No BLAKE3 recompute (calls `m.local.Get(ctx, hash)` directly — chunk bytes addressed by their already-known hash).                          |
| 2  | SyncedHashStore: interface + suite + 3 backends all wired to engine                                            | ✓ VERIFIED  | Interface: `pkg/metadata/synced_hash_store.go:32`. Suite: `pkg/metadata/synced_hash_store_suite.go`. Backends: `pkg/metadata/store/{memory,badger,postgres}/synced_hash_store.go`. Engine wiring: `engine.go:122,141-143`. Production: `runtime/shares/service.go:530-531, 1445-1447`. |
| 3  | 7 TRANSITIONAL-PHASE-18 methods + FlushedBlock + bridge Flush deleted from LocalStore                          | ✓ VERIFIED  | `pkg/blockstore/local/local.go` — interface contains ListUnsynced, Start, Close, GetFileSize, Truncate, EvictMemory, ListFiles, GetStoredFileSize, SyncFileBlocks, SyncFileBlocksForFile, SetEvictionEnabled, SetRetentionPolicy, Stats, Healthcheck (14 methods). Zero hits for WriteAt, ReadAt, IsBlockLocal, GetBlockData, WriteFromRemote, DeleteAllBlockFiles, FlushedBlock. Zero `TRANSITIONAL-PHASE-18` markers anywhere in `pkg/`. |
| 4  | CAS read+write paths (engine.go, fetch.go) rewired to local.Get/Has/Put/AppendWrite                            | ✓ VERIFIED  | Write: `engine.go:408` (`bs.local.AppendWrite`). Read by hash: `engine.go:292` (`bs.local.Get(ctx, hash)`), `engine.go:1042` (`bs.local.Get(ctx, row.fb.Hash)`). Remote fallback persists locally via `fetch.go:151` (`m.local.Put(ctx, fb.Hash, data)`).             |
| 5  | ObjectIDPersister wired from engine.BlockStore into FSStore via rollup hook                                    | ✓ VERIFIED  | Hook install: `engine.go:155-187`. Rollup invocation: `pkg/blockstore/local/fs/rollup.go:373-381` (computes `ComputeObjectID(blocks)` post-`SetRollupOffset` and calls the persister). Local-only shares get non-zero ObjectIDs because the compute runs at rollup, not at remote.Put. FSStore slot: `pkg/blockstore/local/fs/fs.go:546`. |
| 6  | Refcount cascade DeleteSynced in engine.Delete (D-09)                                                          | ✓ VERIFIED  | `pkg/blockstore/engine/engine.go:548-572` — loop over BlockRefs, decrement RefCount per hash, and on `newCount == 0 && bs.syncedHashStore != nil` invoke `DeleteSynced` in the same critical section that fires DeleteChunk. Failure logged at Warn (orphan marker is benign). |
| 7  | TrySpec dedup pre-hook lives in engine.Flush, not on Syncer public seam (D-12/D-17)                            | ✓ VERIFIED  | `pkg/blockstore/engine/engine.go:657-681` — `BlockStore.Flush` calls `snapshotPendingBlockRefs` + `coordinator.GetFileObjectID` + `bs.syncer.trySpeculativeFileLevelDedup` (lowercase t — package-private) before delegating to `bs.syncer.Flush`. The public `TrySpeculativeFileLevelDedup` seam was deleted in commit `e51036b0`; production callers gone (only `docs/ARCHITECTURE.md:1130,1163` still reference the dead symbol — doc drift). |
| 8  | Auxiliary Syncer state preserved per D-16                                                                      | ⚠️ PARTIAL  | Periodic uploader (`syncer.go:585,699`), health monitor (`syncer.go:567`), uploading gate (`syncer.go:273`), backpressure-on-outage (`syncer.go:721`) all present. **But:** `claimBatch` worker pool D-16 promised was retired in 18-06; dead `ClaimBatchSize` config + orphan godoc at `syncer.go:636` remain.    |
| 9  | Integration test suite at pkg/blockstore/engine/syncer_test.go covers 4 scenarios under `//go:build integration` | ✓ VERIFIED  | `pkg/blockstore/engine/syncer_test.go:1` carries `//go:build integration`. Four `func Test*`: `TestSyncer_MirrorLoop_HappyPath`, `TestSyncer_MirrorLoop_PutThenMark_CrashReplay`, `TestSyncer_MirrorLoop_ListUnsyncedSnapshotSemantics`, `TestEngine_Delete_CascadesDeleteSynced` (915 LoC). |
| 10 | TRANSITIONAL-NEXT-MILESTONE convention documented in pkg/blockstore/doc.go (D-19)                              | ✓ VERIFIED  | `pkg/blockstore/doc.go:180-203` — "Transitional-marker convention" section documents both `TRANSITIONAL-PHASE-N:` (specific milestone) and `TRANSITIONAL-NEXT-MILESTONE:` (generic forward pointer), with rationale about avoiding `staticcheck SA1019` and the cleanup-sweep workflow. |
| 11 | Zero "Phase 18" / "D-NN" / ".planning" provenance leakage in any source file/godoc/test name                   | ✗ FAILED    | 5 NEW Phase 18 references introduced in commits `5f5d31c0` and `f9faa9d4`: `engine.go:505`, `api_blockref_test.go:213-215`, `engine_test.go:384-386`, `engine_offline_test.go:101-102 + 155`. 1 NEW "D-07" reference: `syncer_put_error_test.go:72`. Violates `feedback_no_phase_comments_in_code.md`. Note: significant *pre-existing* D-NN leakage from Phases 10-17 (e.g. `dedup_test.go:7`, `rollup.go:240,423`, `appendwrite.go:41,131,147,421`, `fs.go:340-341`) is not introduced by this phase — out of scope here but worth a doc-debt cleanup ticket. |
| 12 | `go build ./...` GREEN                                                                                         | ✓ VERIFIED  | `go build ./...` returns no output (clean exit) from repo root. Test green status accepted on orchestrator's word per task brief. |

**Score:** 10/12 truths verified (2 doc-only gaps; functional invariants all met)

### Required Artifacts

| Artifact                                                            | Expected                                                                              | Status      | Details                                                                                                                            |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------- | ----------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `pkg/blockstore/engine/syncer.go`                                   | Body shrinks to ~50 LoC mirror loop, struct + auxiliary state preserved               | ✓ VERIFIED  | 812 LoC total (mostly auxiliary: janitor, health monitor, GetFileSize/Exists/Truncate/Delete, periodic uploader). Flush body 35 LoC, mirrorOnce 24 LoC. |
| `pkg/blockstore/engine/dedup.go`                                    | Public `TrySpeculativeFileLevelDedup` seam deleted; private `try…` retained           | ✓ VERIFIED  | Only `trySpeculativeFileLevelDedup` (lowercase) exists (line 42). No exported seam.                                                |
| `pkg/blockstore/engine/upload.go`                                   | `uploadOne` / `drainPayloadToRemote` deleted; `syncLocalBlocks` becomes mirror-loop tick | ✓ VERIFIED  | Now 56 LoC. `syncLocalBlocks` (line 17) calls `mirrorOnce`. `uploadBlock` retained as SyncQueue target (acknowledged vestigial at line 33-36). No uploadOne / drainPayloadToRemote / persistFileBlocksAfterFlush. |
| `pkg/blockstore/local/local.go`                                     | 14 methods after deletion of 7 transitional + FlushedBlock                            | ✓ VERIFIED  | 14 methods on interface (audit-grep.txt:34 confirms). Audit also confirms 0 m.local.Flush call sites, 2 m.local.SyncFileBlocksForFile sites. |
| `pkg/metadata/synced_hash_store.go`                                 | 3-method interface mirroring RollupStore shape                                        | ✓ VERIFIED  | 49 LoC. Methods: `IsSynced`, `MarkSynced`, `DeleteSynced` — idempotent contract documented.                                        |
| `pkg/metadata/synced_hash_store_suite.go`                           | Conformance suite                                                                     | ✓ VERIFIED  | 184 LoC.                                                                                                                            |
| `pkg/metadata/store/memory/synced_hash_store.go`                    | Memory backend                                                                        | ✓ VERIFIED  | Wired into MemoryStore (line 250-252 documents per-test mutex).                                                                    |
| `pkg/metadata/store/badger/synced_hash_store.go`                    | Badger backend                                                                        | ✓ VERIFIED  | + test file at same path.                                                                                                          |
| `pkg/metadata/store/postgres/synced_hash_store.go`                  | Postgres backend                                                                      | ✓ VERIFIED  | 75 LoC + migration 000015_synced_hashes.up.sql / .down.sql.                                                                        |
| `pkg/blockstore/local/fs/rollup.go::rollupFile`                     | Post-CommitChunks: ComputeObjectID + ObjectIDPersister callback                       | ✓ VERIFIED  | Lines 361-381 — runs after `SetRollupOffset`, computes ObjectID over BlockRef manifest, invokes persister.                          |
| `pkg/blockstore/local/fs/fs.go::FSStoreOptions.SyncedHashStore`     | Injection slot                                                                        | ✓ VERIFIED  | Field at line 546; consumed at line 607.                                                                                            |
| `pkg/blockstore/local/fs/blockstore_methods.go::ListUnsynced`       | Walk + per-hash IsSynced filter, snapshot-at-start                                    | ✓ VERIFIED  | Lines 209-241 implement the iter.Seq2 surface.                                                                                      |
| `pkg/blockstore/doc.go`                                             | TRANSITIONAL-NEXT-MILESTONE convention                                                | ✓ VERIFIED  | Lines 180-203.                                                                                                                      |
| `pkg/blockstore/engine/syncer_test.go`                              | Integration-tagged with 4 scenarios                                                   | ✓ VERIFIED  | `//go:build integration` + 4 Test funcs.                                                                                            |

### Key Link Verification

| From                                | To                                              | Via                                                    | Status   | Details                                                                  |
| ----------------------------------- | ----------------------------------------------- | ------------------------------------------------------ | -------- | ------------------------------------------------------------------------ |
| `Syncer.Flush`                      | `LocalStore.SyncFileBlocksForFile`              | direct call                                            | ✓ WIRED  | `syncer.go:260`                                                          |
| `Syncer.Flush`                      | `Syncer.mirrorOnce`                             | direct call                                            | ✓ WIRED  | `syncer.go:278`                                                          |
| `Syncer.mirrorOnce`                 | `LocalStore.ListUnsynced`                       | range over iter.Seq2                                   | ✓ WIRED  | `syncer.go:308`                                                          |
| `Syncer.mirrorOnce`                 | `LocalStore.Get(hash) + RemoteStore.Put + SyncedHashStore.MarkSynced` | Put-then-Mark serial chain                      | ✓ WIRED  | `syncer.go:312-321` — Put-then-Mark ordering enforced                    |
| `BlockStore.Flush`                  | `Syncer.trySpeculativeFileLevelDedup` (private) | pre-rollup dedup hook                                  | ✓ WIRED  | `engine.go:670`                                                          |
| `BlockStore.Delete`                 | `SyncedHashStore.DeleteSynced` (when refcount=0)| cascade in DecrementRefCount loop                      | ✓ WIRED  | `engine.go:567`                                                          |
| `engine.New`                        | `Syncer.SetSyncedHashStore`                     | construction-time injection                            | ✓ WIRED  | `engine.go:142`                                                          |
| `engine.New`                        | `FSStore.SetObjectIDPersister`                  | runtime interface assertion                            | ✓ WIRED  | `engine.go:155-187`                                                      |
| `FSStore.rollupFile`                | `ObjectIDPersister callback → coordinator.PersistFileBlocks` | rollup post-commit                              | ✓ WIRED  | `rollup.go:377-381` + `engine.go:185`                                    |
| `runtime/shares/service.go`         | `engineCfg.SyncedHashStore = fileBlockStore.(metadata.SyncedHashStore)` | runtime interface assertion                  | ✓ WIRED  | `service.go:530-531, 1445-1447` — both engine and FSStore paths          |

### Data-Flow Trace (Level 4)

| Artifact                                | Data Variable                  | Source                                          | Produces Real Data           | Status      |
| --------------------------------------- | ------------------------------ | ----------------------------------------------- | --------------------------- | ----------- |
| `Syncer.mirrorOnce`                     | `hash, data`                   | `local.ListUnsynced` + `local.Get(hash)`         | Yes — real CAS chunks       | ✓ FLOWING   |
| `ObjectIDPersister callback`            | `payloadID, blocks, objectID`  | `rollupFile.CommitChunks` output                | Yes — real BlockRef manifest| ✓ FLOWING   |
| `engine.Delete` cascade                 | `b.Hash, newCount`             | `coordinator.DecrementRefCount`                  | Yes — real refcount         | ✓ FLOWING   |
| `engine.Flush` dedup hook               | `specBlocks, fileObjectID`     | `Syncer.snapshotPendingBlockRefs` + `coordinator.GetFileObjectID` | Yes               | ✓ FLOWING   |

### Behavioral Spot-Checks

| Behavior                                | Command                                            | Result            | Status   |
| --------------------------------------- | -------------------------------------------------- | ----------------- | -------- |
| `go build ./...` clean                  | `go build ./...`                                   | (empty output)    | ✓ PASS   |
| Mirror-loop integration suite runnable  | `go test -tags=integration -count=1 -list '.*' ./pkg/blockstore/engine/ -run TestSyncer_MirrorLoop` | (not invoked — orchestrator already confirmed) | ? SKIP |

### Requirements Coverage

PLAN frontmatter `requirements:` references the D-NN decisions in 18-CONTEXT.md rather than .planning/REQUIREMENTS.md REQ-IDs. The 19 D-decisions map directly onto the truths in the table above (D-01 PR shape, D-02 SyncedHashStore, D-04/05/06 ListUnsynced, D-07/08 mirror ordering, D-09 cascade, D-10/11 ObjectID relocation, D-12/13 TrySpec relocation, D-14/15 test reshape, D-16/17/18 code structure, D-19 marker convention). Coverage per D-NN:

| D-NN  | Concern                                          | Status         |
| ----- | ------------------------------------------------ | -------------- |
| D-01  | Single mega-PR (no flag-gated half states)        | ✓ SATISFIED    |
| D-02  | SyncedHashStore interface + 3 backends            | ✓ SATISFIED    |
| D-04  | iter.Seq2 ListUnsynced                            | ✓ SATISFIED    |
| D-05  | Snapshot-at-start semantics                       | ✓ SATISFIED    |
| D-07  | Put-then-Mark crash-safe ordering                 | ✓ SATISFIED    |
| D-09  | Refcount=0 → DeleteSynced cascade                 | ✓ SATISFIED    |
| D-10  | ComputeObjectID at rollup (local-only shares)     | ✓ SATISFIED    |
| D-12  | TrySpec relocated to engine.Flush (private seam)  | ✓ SATISFIED    |
| D-15  | Re-created integration syncer_test.go             | ✓ SATISFIED    |
| D-16  | Auxiliary Syncer state preserved                  | ⚠️ PARTIAL — claimBatch retired, doc drift |
| D-18  | LocalStore admin-superset minus 7 transitional    | ✓ SATISFIED    |
| D-19  | TRANSITIONAL-NEXT-MILESTONE doc                   | ✓ SATISFIED    |

### Anti-Patterns Found

| File                                          | Line     | Pattern                                                                  | Severity   | Impact                                                                 |
| --------------------------------------------- | -------- | ------------------------------------------------------------------------ | ---------- | ---------------------------------------------------------------------- |
| pkg/blockstore/engine/engine.go               | 505      | NEW comment introduced this phase: "After Phase 18 there are no remaining…"  | ⚠️ Warning  | Violates project convention `feedback_no_phase_comments_in_code.md`     |
| pkg/blockstore/engine/api_blockref_test.go    | 213-215  | NEW: "removed in the Phase 18-08 sweep"                                  | ⚠️ Warning  | Same                                                                   |
| pkg/blockstore/engine/engine_test.go          | 384-388  | NEW: "Removed in the Phase 18-08 sweep"                                  | ⚠️ Warning  | Same                                                                   |
| pkg/blockstore/engine/engine_offline_test.go  | 101-105  | NEW: "removed in the Phase 18-08 sweep"                                  | ⚠️ Warning  | Same                                                                   |
| pkg/blockstore/engine/engine_offline_test.go  | 155      | NEW: "was removed in the Phase 18-08 sweep"                              | ⚠️ Warning  | Same                                                                   |
| pkg/blockstore/engine/syncer_put_error_test.go| 72       | NEW: "pins the crash-safety contract in D-07"                            | ⚠️ Warning  | D-NN reference in test godoc                                           |
| pkg/blockstore/engine/syncer.go               | 636      | Orphan godoc "claimBatch will not double-claim" — claimBatch retired in 18-06 | ℹ️ Info     | Documentation drift, no functional impact                              |
| pkg/blockstore/store.go                       | 44, 62   | Pre-existing references to deleted `engine.uploadOne` symbol             | ℹ️ Info     | Stale; not new this phase but the cleanup window was open               |
| pkg/metadata/storetest/file_block_ops.go      | 87, 505  | Pre-existing `engine.uploadOne` references                               | ℹ️ Info     | Same                                                                   |
| pkg/blockstore/engine/types.go                | 68, 86   | `ClaimBatchSize` config field with no consumer post-18-06                | ℹ️ Info     | Dead config knob                                                       |

### Human Verification Required

None — every Phase 18 must-have is observable from the codebase. Functional behaviour (crash-replay, snapshot semantics, refcount cascade) is exercised by the new integration suite at `pkg/blockstore/engine/syncer_test.go` and PR review can rely on `go test -tags=integration ./pkg/blockstore/engine/...`.

### Gaps Summary

Phase 18 achieves its functional goal: the Syncer is a byte-identical local→remote mirror loop, sync state lives in a metadata-backed SyncedHashStore wired across all 3 backends + production controlplane, the 7 transitional LocalStore methods + FlushedBlock + bridge Flush are deleted (audit-grep confirms 0 residual production callers), the CAS read+write paths route through `local.Get(hash) / local.Has(hash) / local.Put(hash) / AppendWrite`, ObjectID is computed at rollup (closing the Phase 13 UAT surprise for local-only shares), the refcount cascade keeps the synced set a strict subset of local CAS contents, and a 4-scenario integration suite covers the mirror loop end-to-end against memory + s3 remotes.

Two gaps remain — both documentation-only, neither blocks the next milestone:

1. **Provenance leakage (truth 11)** — 5 new "Phase 18-08 sweep" / "After Phase 18" godoc comments and 1 new "D-07" test-godoc reference were introduced in commits `5f5d31c0` and `f9faa9d4`, violating `feedback_no_phase_comments_in_code.md`. Pure text edits to make these milestone-agnostic. Pre-existing D-NN/Phase-N leakage from Phases 10-17 (e.g. `dedup_test.go:7`, `rollup.go:240,423`, `appendwrite.go:41`, `fs.go:340-341`) is out of scope for Phase 18 but worth a separate doc-debt ticket.

2. **D-16 claimBatch drift (truth 8)** — CONTEXT D-16 says the `claimBatch` worker pool should be preserved as auxiliary state. It was actually retired in 18-06 when the SyncQueue + mirror loop replaced per-block claiming. One orphan godoc at `syncer.go:636` references the dead symbol; `ClaimBatchSize` config field at `types.go:68,86` is now consumer-free. Either resurrect the doc comment to describe the SyncQueue or formally drop the dead config and update CONTEXT D-16.

Recommend filing two GitHub follow-up issues:

- **doc-debt: scrub Phase 18 provenance leakage in pkg/blockstore/engine/**: 5 godoc + 1 test comment rewrites.
- **chore: retire dead ClaimBatchSize config knob + claimBatch godoc**: remove the field, defaults, and stale comment OR document why it stays.

Both are independent of Phase 19 (Write-path RAM optimizations) and can be picked up at any time before the next milestone planning sweep.

---

_Verified: 2026-05-21T13:30:00Z_
_Verifier: Claude (gsd-verifier)_

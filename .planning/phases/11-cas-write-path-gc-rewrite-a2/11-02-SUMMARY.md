---
phase: 11-cas-write-path-gc-rewrite-a2
plan: 02
subsystem: blockstore
tags: [cas, blake3, syncer, claim-batch, janitor, inv-03, blockstate, postgres-migration]

requires:
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: "FormatCASKey/ParseCASKey, BlockState (Pending/Syncing/Remote), FileBlock.LastSyncAttemptAt, RemoteStore.WriteBlockWithHash, ErrCASContentMismatch/Malformed (plan 11-01)"
provides:
  - "engine.Syncer.claimBatch — D-13 batched Pending→Syncing transition (single metadata txn)"
  - "engine.Syncer.uploadOne — single owner of Syncing→Remote (D-15) with INV-03 ordering"
  - "engine.Syncer.recoverStaleSyncing — D-14 restart-recovery janitor"
  - "engine.SyncerConfig.{ClaimBatchSize,UploadConcurrency,ClaimTimeout} + DefaultConfig"
  - "config.SyncerConfig top-level knob struct with ApplyDefaults + Validate"
  - "syncingEnumerator optional FileBlockStore capability (memory backend implements it)"
  - "MemoryMetadataStore.EnumerateSyncingBlocks helper"
  - "Postgres migration 000010_file_blocks.up.sql — first migration that actually creates the file_blocks table"
  - "Postgres backend aligned with Phase-11 state values (0=Pending, 1=Syncing, 2=Remote)"
  - "INV-03 deterministic crash-injection unit test (3 kill points + janitor recovery scenario)"
affects: [11-03-dual-read-resolver, 11-04-gc-mark-sweep, 11-05-restart-recovery, 11-08-e2e]

tech-stack:
  added: []
  patterns:
    - "PUT-then-meta ordering (D-11): RemoteStore.WriteBlockWithHash succeeds first, then PutFileBlock(state=Remote) — INV-03 guarantees no orphaned Remote rows"
    - "Bounded share-wide parallel pool (D-25): ClaimBatchSize sized batches drained by UploadConcurrency goroutines via semaphore + waitgroup"
    - "Optional capability interface (syncingEnumerator) for backend-specific janitor support without forcing every backend to implement a state-filtered cursor"
    - "Postgres NULL semantics: LastSyncAttemptAt=time.Time{} stored as SQL NULL so the janitor predicate WHERE last_sync_attempt_at < cutoff naturally excludes never-claimed rows"

key-files:
  created:
    - "pkg/blockstore/engine/syncer_unit_test.go"
    - "pkg/blockstore/engine/syncer_crash_test.go"
    - "pkg/config/syncer_test.go"
    - "pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql"
    - "pkg/metadata/store/postgres/migrations/000010_file_blocks.down.sql"
    - ".planning/phases/11-cas-write-path-gc-rewrite-a2/11-02-SUMMARY.md"
  modified:
    - "pkg/blockstore/engine/syncer.go"
    - "pkg/blockstore/engine/upload.go"
    - "pkg/blockstore/engine/types.go"
    - "pkg/config/config.go"
    - "pkg/config/defaults.go"
    - "pkg/metadata/store/memory/objects.go"
    - "pkg/metadata/store/postgres/objects.go"
    - "pkg/metadata/storetest/file_block_ops.go"

key-decisions:
  - "Postgres needed BOTH a new column (last_sync_attempt_at TIMESTAMPTZ NULL) AND a one-time state-encoding fix — the pre-Phase-11 backend used legacy state values (1=Local, 3=Remote) in its WHERE clauses, which silently broke after plan 11-01 collapsed BlockState to Pending=0/Syncing=1/Remote=2. Migration 000010_file_blocks.up.sql codifies the table (it had been a never-run inline `fileBlocksTableMigration` const) and uses the new state encoding from day one."
  - "Recovery janitor surface: introduced a Phase-11-internal optional capability `syncingEnumerator` on FileBlockStore rather than promote it to the public interface. The memory backend implements it directly; the badger and postgres backends will gain it in plan 04 alongside the EnumerateFileBlocks cursor (D-02). Until then, a backend without the capability degrades to a safe no-op janitor — the row stays Syncing and the next claim cycle will not double-claim because the row is no longer Pending."
  - "Removed dead inline `fileBlocksTableMigration` const from postgres/objects.go (it was guarded by `var _ = fileBlocksTableMigration` and used the legacy 4-state encoding) — replaced with a comment pointing at the embedded migration."
  - "Legacy syncFileBlock retained as a deprecated thin shim — it now flips Pending→Syncing then delegates to uploadOne, preserving the contract that pkg/blockstore/engine/syncer_put_error_test.go validates (post-PUT metadata error must propagate). All other callers (SyncNow, syncLocalBlocks, uploadBlock) go through the CAS path directly."

requirements-completed: [BSCAS-01, BSCAS-03, BSCAS-06, STATE-01, STATE-02, STATE-03, INV-03]

duration: 13min
completed: 2026-04-25
---

# Phase 11 Plan 02: CAS Write Path + State Lifecycle + INV-03 Crash Test

**The syncer now drives all uploads through BLAKE3 + CAS keys via WriteBlockWithHash; the three-state lifecycle (Pending → Syncing → Remote) is persisted on FileBlock.State across all backends; a batched claim cycle and bounded parallel upload pool serialize against duplicate uploads; a restart-recovery janitor requeues abandoned Syncing rows; a deterministic crash-injection unit test proves INV-03 at three kill points.**

## Performance

- **Duration:** ~13 min
- **Started:** 2026-04-25T15:33:30Z
- **Completed:** 2026-04-25T15:46:31Z
- **Tasks:** 3 (all TDD, total 5 commits)
- **Files created:** 5 source / 1 doc, **modified:** 8

## Accomplishments

### Task 1 — SyncerConfig knobs + LastSyncAttemptAt persistence

- New `config.SyncerConfig` with `ClaimBatchSize=32`, `UploadConcurrency=8`, `ClaimTimeout=10m`, `Tick=30s`, plus `ApplyDefaults`/`Validate`. Wired into the global `ApplyDefaults`.
- Conformance suite gained `PutGet_LastSyncAttemptAt` and `PutGet_LastSyncAttemptAt_Zero` sub-tests; all three backends pass them.
- Memory backend: zero changes needed (FileBlock stored by reference).
- Badger backend: zero changes needed (full JSON encoding).
- Postgres backend: full schema overhaul:
  - New migration `000010_file_blocks.up.sql` creating the `file_blocks` table with the Phase-11 state encoding (`0=Pending, 1=Syncing, 2=Remote`) plus the `last_sync_attempt_at TIMESTAMPTZ` column. The table was previously declared by an in-process `fileBlocksTableMigration` const that was never wired to the migration runner — removed in this commit.
  - Indexes: hash partial UNIQUE, pending+cache_path partial, remote+cache_path partial, unreferenced (ref_count=0), and a new `idx_file_blocks_syncing_age` for the janitor's stale-Syncing scan.
  - All queries updated: `FindFileBlockByHash` now matches `state = 2 /* Remote */` (was `state = 3`); `ListLocalBlocks` matches `state = 0 /* Pending */` (was `state = 1`); `ListRemoteBlocks` matches `state = 2` (was `state = 3`); `scanFileBlock`/`scanFileBlockRows` read `last_sync_attempt_at` and de-NULL it via `sql.NullTime`.

### Task 2 — Syncer + upload rewrite for CAS

- `engine/upload.go`:
  - Replaced `crypto/sha256` + `FormatStoreKey` with `lukechampine.com/blake3` + `FormatCASKey` + `WriteBlockWithHash`.
  - New `uploadOne` — single owner of the Syncing→Remote transition (D-15), enforces INV-03 ordering (PUT first, then metadata-txn), pre-PUT dedup short-circuit if hash already Remote.
  - `revertToLocal` renamed to `revertToPending` for clarity.
  - Legacy `syncFileBlock` retained as a deprecated shim (flips to Syncing then delegates to uploadOne) so the existing `syncer_put_error_test.go` contract still holds.
  - `uploadBlock` (queue-worker entry) also taken to the CAS path.
- `engine/syncer.go`:
  - Rewrote `SyncNow`: claimBatch → bounded parallel uploadOne pool → drain loop until claimBatch returns empty.
  - New `claimBatch(ctx, max)` flips up to `max` Pending rows to Syncing in one logical metadata batch and stamps `LastSyncAttemptAt = time.Now()` (D-13).
  - New `recoverStaleSyncing(ctx)` requeues Syncing rows whose `LastSyncAttemptAt` is older than `ClaimTimeout` back to Pending (D-14).
  - Janitor invoked once at `Start()` before the periodic loop launches; failure logs WARN, does not block startup.
  - `syncLocalBlocks` (periodic-loop tick path) updated to drive the new claim/upload pipeline.
- `engine/types.go`: added `ClaimBatchSize`/`UploadConcurrency`/`ClaimTimeout` to `SyncerConfig` + `DefaultConfig`. `NewSyncer` defaults them.
- Memory backend: added `EnumerateSyncingBlocks` so the janitor can surface stranded Syncing rows.

### Task 3 — INV-03 crash-injection tests

- New `pkg/blockstore/engine/syncer_crash_test.go` with four scenarios:
  - `TestSyncerCrash_PrePut`: WriteBlockWithHash returns sentinel before any S3 write — assert 0 objects, persisted row Syncing, no metadata write attempted.
  - `TestSyncerCrash_BetweenPutAndMeta`: PUT succeeds, post-PUT PutFileBlock fails — assert 1 object exists at the CAS key, persisted row stays Syncing, LastSyncAttemptAt preserved.
  - `TestSyncerCrash_PostMeta`: both succeed — assert happy-path row=Remote with parseable CAS BlockStoreKey + bytes retrievable from remote.
  - `TestSyncerCrash_RecoveryRequeuesStale`: drives BetweenPutAndMeta crash, waits past ClaimTimeout, runs recoverStaleSyncing — assert row flips back to Pending with zero LastSyncAttemptAt.
- Crash wrappers (`crashingRemoteStore`, `crashingFileBlockStore`) hand-rolled per D-32 — no generic harness in this phase.

## Task Commits

All five commits are GPG-signed (ED25519) and pass `git verify-commit`:

1. **Task 1 RED** — `8451947d` test(11-02): add failing tests for SyncerConfig and LastSyncAttemptAt round-trip
2. **Task 1 GREEN** — `acb3920d` feat(11-02): SyncerConfig knobs + LastSyncAttemptAt persistence in postgres
3. **Task 2 RED** — `35282b16` test(11-02): add failing tests for claimBatch, uploadOne, recoverStaleSyncing
4. **Task 2 GREEN** — `a6eeed6c` feat(11-02): rewrite syncer for CAS uploads, batched claim, parallel pool, restart janitor
5. **Task 3** — `3997a291` test(11-02): INV-03 deterministic crash injection at three kill points

## Files Created/Modified

### Created
- `pkg/blockstore/engine/syncer_unit_test.go` — non-integration tests for claimBatch, uploadOne, recoverStaleSyncing, SyncNow drain loop.
- `pkg/blockstore/engine/syncer_crash_test.go` — INV-03 deterministic crash injection (3 kill points + janitor recovery).
- `pkg/config/syncer_test.go` — SyncerConfig defaults + validation.
- `pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql` — first authoritative file_blocks DDL with Phase-11 state encoding.
- `pkg/metadata/store/postgres/migrations/000010_file_blocks.down.sql`.

### Modified
- `pkg/blockstore/engine/syncer.go` — Start invokes janitor; new claimBatch + recoverStaleSyncing + collectSyncingCandidates + syncingEnumerator capability; SyncNow rewritten to drive the bounded parallel pool.
- `pkg/blockstore/engine/upload.go` — uploadOne (CAS + BLAKE3 + INV-03 ordering); legacy SHA-256 + FormatStoreKey + revertToLocal removed/renamed; syncFileBlock retained as shim.
- `pkg/blockstore/engine/types.go` — SyncerConfig + DefaultConfig gain ClaimBatchSize/UploadConcurrency/ClaimTimeout.
- `pkg/config/config.go` — top-level SyncerConfig struct + ApplyDefaults + Validate; wired into Config.
- `pkg/config/defaults.go` — global ApplyDefaults invokes cfg.Syncer.ApplyDefaults.
- `pkg/metadata/store/memory/objects.go` — added EnumerateSyncingBlocks helper.
- `pkg/metadata/store/postgres/objects.go` — fixed every state literal (legacy 1/3 → Phase-11 0/2), added last_sync_attempt_at to all SELECT/INSERT/scan paths, removed dead inline fileBlocksTableMigration const.
- `pkg/metadata/storetest/file_block_ops.go` — added PutGet_LastSyncAttemptAt + zero-value sub-tests.

## Decisions Made

1. **Postgres state-value drift was a Rule 1 bug from plan 11-01.** The earlier plan collapsed BlockState constants in Go but left the postgres backend's `state = 1` / `state = 3` literals untouched. Post-collapse, those queries silently return wrong rows (matching Syncing instead of Pending; matching nothing instead of Remote). Fixed inline as part of this plan's postgres column-add work.
2. **Migration 000010 is the FIRST migration that actually creates the file_blocks table.** Previously the table was declared by an in-process inline DDL string (`fileBlocksTableMigration` in objects.go) that was guarded by `var _ = fileBlocksTableMigration` and never executed. Removed the dead code; codified the schema in the migration runner so production deployments and integration tests share one truth.
3. **`syncingEnumerator` as an optional capability, not a public interface change.** Avoids forcing every metadata backend to implement a state-filtered cursor in plan 11-02. Memory backend implements it; badger and postgres will gain it in plan 11-04 alongside `EnumerateFileBlocks` (D-02). Until then, a backend without the capability degrades to a safe no-op janitor.
4. **`syncFileBlock` kept as a deprecated shim** rather than deleted, because `syncer_put_error_test.go` validates the post-PUT metadata-error propagation contract. The shim flips Pending→Syncing then delegates to uploadOne, so all upload paths share the same INV-03 ordering.
5. **D-28 (sync/ rename) confirmed already done.** `pkg/blockstore/sync/` does not exist; the syncer has lived at `pkg/blockstore/engine/syncer.go` since Phase 10. No-op decision recorded for traceability.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Postgres state-value drift after Phase-11 collapse**

- **Found during:** Task 1 plan-of-record reading.
- **Issue:** Plan 11-01 collapsed `BlockState` constants in Go (Pending=0, Syncing=1, Remote=2) but left the postgres backend's WHERE-clause literals at the legacy values (`state = 1 /* Local */`, `state = 3 /* Remote */`). Post-collapse, `ListLocalBlocks` would match Syncing rows, `FindFileBlockByHash`/`ListRemoteBlocks` would match nothing.
- **Fix:** Updated every state literal in `pkg/metadata/store/postgres/objects.go` to the Phase-11 encoding. Codified the canonical schema in new migration `000010_file_blocks.up.sql`.
- **Files modified:** `pkg/metadata/store/postgres/objects.go`, `pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql`, `pkg/metadata/store/postgres/migrations/000010_file_blocks.down.sql`.
- **Committed in:** `acb3920d` (Task 1 GREEN).

**2. [Rule 3 - Blocking] Postgres file_blocks table never reached the migration runner**

- **Found during:** Task 1 — looking for the existing column schema.
- **Issue:** The pre-Phase-11 inline `fileBlocksTableMigration` const in `pkg/metadata/store/postgres/objects.go` was guarded by `var _ = fileBlocksTableMigration` and never executed by the golang-migrate runner. Production postgres deployments would hit "relation file_blocks does not exist" on first PutFileBlock.
- **Fix:** New migration `000010_file_blocks.up.sql` creates the table; deleted the dead inline const and replaced with a doc comment pointing at the migration.
- **Committed in:** `acb3920d` (Task 1 GREEN).

**3. [Rule 3 - Blocking] crashingFileBlockStore wrapper hid EnumerateSyncingBlocks**

- **Found during:** Task 3 first run of TestSyncerCrash_RecoveryRequeuesStale.
- **Issue:** The `crashingFileBlockStore` wrapper embedded the underlying `MemoryMetadataStore` only via the public `FileBlockStore` interface. The janitor's optional `syncingEnumerator` capability was therefore not visible through the wrapper, so `collectSyncingCandidates` returned nil and the janitor found no rows to requeue.
- **Fix:** Added an `EnumerateSyncingBlocks` forwarder on the wrapper that delegates to the embedded store if it implements the capability.
- **Committed in:** `3997a291` (Task 3).

**Total deviations:** 3 auto-fixed (no user permission needed; all fall under Rules 1 and 3).

## Issues Encountered

None outside the deviations above.

## Threat Flags

None new. The plan's `<threat_model>` (T-11-A-05 through T-11-A-09) is fully covered by:
- T-11-A-05 (orphan upload tampering) → mitigated by INV-03 ordering, proven by `TestSyncerCrash_BetweenPutAndMeta`.
- T-11-A-06 (concurrent SyncNow double-upload) → mitigated by claimBatch's per-row PutFileBlock serialization + the `m.uploading` gate.
- T-11-A-07 (DoS via unbounded UploadConcurrency) → mitigated by the bounded semaphore in SyncNow + the `Validate` rule rejecting `UploadConcurrency > ClaimBatchSize`.
- T-11-A-08 (janitor over-eager requeue) → accepted; CAS idempotency makes it benign.
- T-11-A-09 (path leakage in error messages) → accepted; LocalPath is internal.

## Next Plan Readiness

- **Plan 11-03 (dual-read resolver)** can now read `block.State == BlockStateRemote` and `block.BlockStoreKey` (a CAS key parsable via `ParseCASKey`) to route reads to the new path. Legacy rows still have `state = 0 (Pending) AND BlockStoreKey != ""` per D-21's dual-read fallback.
- **Plan 11-04 (GC mark-sweep)** can use `ParseCASKey` to discriminate `cas/...` objects, and gains a clear surface for adding `EnumerateFileBlocks` to the metadata interface (the Phase-11-internal `syncingEnumerator` capability is the immediate predecessor pattern).
- **Plan 11-05 (restart recovery)** can build on `recoverStaleSyncing` — it may want to add backend-native variants for badger and postgres alongside the EnumerateFileBlocks cursor work.
- **Plan 11-08 (e2e)** can drive the full path through real shares, observing CAS keys, the `x-amz-meta-content-hash` header on S3, and the three-state transitions.

## Postgres Migration Notes (per `<output>` requirement)

- **Did postgres need a column-add migration?** Yes — `000010_file_blocks.up.sql`. This migration is also the first one to create the `file_blocks` table itself; the pre-Phase-11 inline `fileBlocksTableMigration` const was never executed by golang-migrate. The new migration uses the Phase-11 state encoding (0=Pending, 1=Syncing, 2=Remote) and adds the new `last_sync_attempt_at TIMESTAMPTZ` column with a partial index `idx_file_blocks_syncing_age` for the janitor.
- **Migration path:** `pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql`.
- **Down migration:** `pkg/metadata/store/postgres/migrations/000010_file_blocks.down.sql`.

## FileBlockStore Helper Names (per `<output>` requirement)

- Added optional capability interface `syncingEnumerator` (engine-internal) with single method `EnumerateSyncingBlocks(ctx) ([]*FileBlock, error)`.
- Added implementation `MemoryMetadataStore.EnumerateSyncingBlocks`.
- No new method on the public `FileBlockStore` interface — by design, to keep the contract stable. Backends opt in by implementing `EnumerateSyncingBlocks` directly on their concrete type; the syncer uses a runtime type assertion.

## D-28 Confirmation (per `<output>` requirement)

- `pkg/blockstore/sync/` directory does not exist; verified via `ls`. The syncer has lived at `pkg/blockstore/engine/syncer.go` since Phase 10. D-28 is a confirmed no-op for this plan.

## TDD Gate Compliance

Plan-level type was `execute`, but Tasks 1, 2, 3 were tagged `tdd="true"` and followed the RED → GREEN cycle:

- **Task 1 RED** (`8451947d`) — wrote failing tests (build error in `pkg/config`); GREEN (`acb3920d`) — added SyncerConfig + postgres column.
- **Task 2 RED** (`35282b16`) — wrote failing tests (build errors for claimBatch/uploadOne/recoverStaleSyncing); GREEN (`a6eeed6c`) — implemented the rewrite.
- **Task 3** combined RED + GREEN inside one commit (`3997a291`) — the crash wrappers and the test bodies are inseparable: the test asserts an INV-03 contract that already holds in the code, so a separate failing-test commit would have been an artificial split.

## Self-Check: PASSED

- `pkg/blockstore/engine/syncer.go` — claimBatch, recoverStaleSyncing, collectSyncingCandidates, syncingEnumerator all present; `Start()` calls recoverStaleSyncing.
- `pkg/blockstore/engine/upload.go` — uploadOne present, uses BLAKE3 + FormatCASKey + WriteBlockWithHash; no `crypto/sha256` reference; no `FormatStoreKey` calls (only in a doc comment about removal).
- `pkg/blockstore/engine/syncer_crash_test.go` — exists, contains 4 test functions matching `TestSyncerCrash_*` and uses `errKillPoint`/`crashingRemoteStore`/`crashingFileBlockStore`.
- `pkg/config/config.go` — `type SyncerConfig` declared with all four knobs + ApplyDefaults + Validate.
- `pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql` — exists, creates file_blocks with state encoding 0/1/2 and last_sync_attempt_at column.
- `grep -rE 'BlockStateLocal|BlockStateDirty' pkg/metadata/store/{memory,badger,postgres}/` — returns 0 lines.
- `go build ./...` — exits 0.
- `go vet ./...` — exits 0.
- `go test -short -count=1 ./pkg/blockstore/engine/... ./pkg/metadata/...` — exits 0.
- All 5 commits present in `git log`, all pass `git verify-commit`.

---
*Phase: 11-cas-write-path-gc-rewrite-a2*
*Completed: 2026-04-25*

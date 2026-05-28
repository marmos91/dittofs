---
phase: 24-restore-flow
plan: 03
subsystem: controlplane
tags: [restore, snapshot, orchestration, sync, verify]

requires:
  - phase: 22-snapshot-records-hash-manifest-gc-hold
    provides: Snapshot model + ManifestPath/MetadataDumpPath + StateReady constant + ErrSnapshotStateConflict sentinel
  - phase: 23-snapshot-create-orchestration-sync-gate
    provides: Runtime.CreateSnapshot + Runtime.WaitForSnapshot + snapshot.VerifyRemoteDurability + snapshot.ReadManifest + SyncGateConcurrency default
  - phase: 24-01
    provides: metadata.Resetable interface on memory/badger/postgres backends
  - phase: 24-02
    provides: 7 Phase-24 sentinels (models.ErrShareEnabled, ErrSnapshotNotDurable, …) + RestoreSnapshotOpts struct

provides:
  - "snapshot.HashSetFromMetadataStore(ctx, store) (*HashSet, error) — backend-agnostic post-verify walker (D-24-14)"
  - "Runtime.RestoreSnapshot(ctx, shareName, snapID, RestoreSnapshotOpts) error — synchronous 8-step orchestration (D-24-09)"

affects:
  - "24-04-restore-e2e-integration-test: has a stable Runtime.RestoreSnapshot surface to exercise"
  - "25-restore-cli-rest-handler (future phase): wraps Runtime.RestoreSnapshot, maps the 7 Phase-24 sentinels to HTTP status codes via errors.Is"

tech-stack:
  added: []
  patterns:
    - "Reuse of pre-existing MetadataStore.EnumerateFileBlocks for post-verify hash walk (same surface GC mark phase uses)"
    - "Error wrapping with both %w (sentinel) and %v (inner err) so errors.Is works while preserving operator-readable detail"
    - "Step-5/6/7 failure messages embed safety-snap=<id> for operator-driven rollback (D-24-13)"
    - "Sync orchestration with no goroutines — no new in-flight registry, no new locking primitive (D-24-02)"

key-files:
  created:
    - pkg/snapshot/hashset_from_metadata.go
    - pkg/snapshot/hashset_from_metadata_test.go
  modified:
    - pkg/controlplane/runtime/snapshot.go

key-decisions:
  - "HashSetFromMetadataStore wraps MetadataStore.EnumerateFileBlocks, NOT the public ListShares→GetRootHandle→ReadDirectory walk. EnumerateFileBlocks is already the canonical interface method for this workload (GC mark phase uses it), works against all 3 backends without AuthContext gymnastics, and inherits the streaming-via-cursor + ctx.Done() contract from the interface contract."
  - "Zero ContentHash is skipped in the walk per the EnumerateFileBlocks interface contract (legacy pre-CAS rows emit zero; including would cause spurious ErrRestoreVerifyFailed)."
  - "RestoreSnapshot reuses Phase 23 ErrSnapshotStateConflict for the 'snap is not Ready' branch — matches CreateSnapshot's vocabulary so Phase 25 maps a single sentinel to 409/precondition."
  - "Safety snapshot is created with CreateSnapshotOpts{} (zero value) — default NoSyncGate=false ensures the safety snap is sync-gate-drained + remote-verified before becoming the recovery primitive (D-24-04)."
  - "Safety-snap auto-generated UUID is the operator-discovery primitive (via ListSnapshots filtered by created_at desc); no Name field added to CreateSnapshotOpts in this plan."
  - "Manifest-open ENOENT wraps ErrSnapshotMetadataDumpMissing for triage symmetry with dump-open ENOENT — operator action is identical (look for either file under the snapshot dir)."

patterns-established:
  - "Sentinel-wrap + safety-snap embedding: `fmt.Errorf(\"... (safety-snap=%s): %w: %v\", safetyID, sentinel, innerErr)` for any error that lands AFTER the safety snapshot is created and BEFORE return-nil."

requirements-completed: [REST-01, REST-02, REST-03]

metrics:
  duration: ~25min
  completed: 2026-05-28
---

# Phase 24 Plan 03: Runtime.RestoreSnapshot 8-step orchestration — Summary

`Runtime.RestoreSnapshot` synchronously swaps a share's metadata-store contents from a previously-created snapshot's dump, gated by pre+post-restore remote-block verification and a sync-gated pre-restore safety snapshot. Plus `snapshot.HashSetFromMetadataStore` — the backend-agnostic post-verify walker used by step 7.

## What landed

**`pkg/snapshot/hashset_from_metadata.go` (~55 LoC):**
Wraps `MetadataStore.EnumerateFileBlocks(ctx, fn)` — the same surface the GC mark phase uses to populate its live-block set — to build a deduplicated `*blockstore.HashSet`. The zero ContentHash is skipped (legacy pre-CAS rows emit it by interface contract). Streaming + ctx.Done() are inherited from the interface contract.

**`pkg/snapshot/hashset_from_metadata_test.go` (5 tests, ~180 LoC):**
- `TestHashSetFromMetadataStore_Empty` — empty store returns non-nil HashSet, Len()==0
- `TestHashSetFromMetadataStore_ThreeUniqueHashes` — 3 rows, 3 distinct hashes, all present
- `TestHashSetFromMetadataStore_Deduplication` — 4 rows, shared+unique1+unique2, Len()==3 (D-24-14 dedup property pinned)
- `TestHashSetFromMetadataStore_CtxCancellation` — pre-cancelled ctx surfaces `context.Canceled` through the wrap
- `TestHashSetFromMetadataStore_SkipsZeroHash` — legacy zero-hash row excluded from result (no phantom hash in post-verify)

All 5 pass against an in-memory backend; `go vet ./pkg/snapshot/...` clean.

**`pkg/controlplane/runtime/snapshot.go` (extended, ~260 LoC added):**
`func (r *Runtime) RestoreSnapshot(ctx, shareName, snapID, opts RestoreSnapshotOpts) error` appended as a sibling to `CreateSnapshot`. Flat sequential body, no goroutines, no new in-flight registry, no new locking primitive (per D-24-02). Steps 1-8 from D-24-09:

| Step | Action | Failure wraps |
|------|--------|---------------|
| 1 | precheck: IsShareEnabled, GetSnapshot, State==Ready, RemoteDurable\|\|AllowNonDurable | `ErrShareEnabled` / propagate `ErrSnapshotNotFound` / `ErrSnapshotStateConflict` / `ErrSnapshotNotDurable` |
| 2 | open manifest, ReadManifest, VerifyRemoteDurability (REST-03 first half) | `ErrSnapshotMetadataDumpMissing` (ENOENT) / `ErrRestoreVerifyFailed` |
| 3 | CreateSnapshot{} + WaitForSnapshot, assert State==Ready | `ErrRestoreSafetySnapFailed` |
| 4 | Open dump file at MetadataDumpPath | `ErrSnapshotMetadataDumpMissing` (ENOENT) / `ErrRestoreAborted` |
| 5 | Type-assert Resetable, type-assert Backupable, Reset(ctx) | `ErrMetadataStoreNotResetable` / `ErrRestoreAborted (safety-snap=<id>)` |
| 6 | Restore(ctx, dumpFile) | `ErrRestoreAborted (safety-snap=<id>)` |
| 7 | HashSetFromMetadataStore + VerifyRemoteDurability (REST-03 second half) | `ErrRestoreVerifyFailed (safety-snap=<id>)` |
| 8 | return nil — share STAYS DISABLED per D-24-01 | — |

`r.sharesSvc.EnableShare` is NEVER called from RestoreSnapshot — the operator runs `dfsctl share enable` manually after inspection.

## Accessor names confirmed during audit

- `r.sharesSvc.IsShareEnabled(name) (bool, error)` — `pkg/controlplane/runtime/shares/service.go:928`
- `r.sharesSvc.LocalStoreDir(name) (string, error)` — line 988
- `r.sharesSvc.GetBlockStoreForShare(name) (*engine.BlockStore, error)` — line 1126
- `bs.RemoteStore() remote.RemoteStore` — `pkg/blockstore/engine/engine.go:878`
- `r.snapshotDefaults() SnapshotDefaults` — `pkg/controlplane/runtime/runtime.go:722`; `.SyncGateConcurrency` is the field (default 16)
- `r.GetMetadataStoreForShare(name) (metadata.MetadataStore, error)` — `pkg/controlplane/runtime/runtime.go:233`
- `r.store.GetSnapshot(ctx, shareName, snapID)` — already used by CreateSnapshot's retry path

## Phase 23 sentinel reused

`models.ErrSnapshotStateConflict` (Phase 22 declaration, Phase 23 usage in retry path) — reused verbatim for the "snap is not in StateReady" precheck branch. Phase 25 will map a single sentinel to a 409/precondition status across both CreateSnapshot and RestoreSnapshot.

## Threat model compliance

- **T-24-03-01** (restore on enabled share): step 1 `IsShareEnabled` precheck returns `ErrShareEnabled`. ✓
- **T-24-03-02** (restore from non-durable without intent): step 1 refuses unless `opts.AllowNonDurable=true`. ✓
- **T-24-03-03** (info disclosure via safety-snap ID): operator-controlled identifier, accepted risk. ✓
- **T-24-03-07** (post-verify slog leakage): structured logging emits `restored_count` and `verify_concurrency` only, NOT individual ContentHash strings. ✓
- **T-24-03-08** (block deleted between pre + post verify): post-verify is the explicit gate; `ErrRestoreVerifyFailed (safety-snap=<id>)` is the recovery surface. ✓
- **T-24-03-09** (repudiation): structured logs at every orchestration boundary (`precheck ok` / `pre-verify start` / `safety snapshot ready` / `reset start` / `reset ok` / `restore start` / `restore ok` / `post-verify start` / `complete`) with `snapshot_id` + `share` + `safety_snap_id`. ✓

## Verification

```
go test ./pkg/snapshot/... -run "TestHashSetFromMetadataStore" -count=1   # PASS (5 tests)
go test ./pkg/snapshot/... ./pkg/controlplane/runtime/... -count=1        # PASS (no regressions)
go build ./pkg/controlplane/runtime/...                                   # exit 0
go vet ./pkg/controlplane/...                                             # exit 0
go vet ./pkg/snapshot/...                                                 # exit 0
```

Plan-level acceptance grep gates (all positive):
- `func (r *Runtime) RestoreSnapshot` ✓
- `models.ErrShareEnabled` / `models.ErrSnapshotNotDurable` / `models.ErrSnapshotMetadataDumpMissing` / `models.ErrMetadataStoreNotResetable` / `models.ErrRestoreSafetySnapFailed` / `models.ErrRestoreAborted` / `models.ErrRestoreVerifyFailed` ✓ (all 7)
- `snapshot.VerifyRemoteDurability` / `snapshot.HashSetFromMetadataStore` ✓
- `metadata.Resetable` / `metadata.Backupable` ✓ (both type-asserted)
- `r.CreateSnapshot` / `r.WaitForSnapshot` ✓
- `safety-snap=` ✓ (steps 5, 6, 7 + GetMetadataStoreForShare error path)
- No `EnableShare` call in `pkg/controlplane/runtime/snapshot.go` ✓ (D-24-01)

## Deviations from Plan

**1. [Rule 2 - Auto-add missing critical functionality] Manifest open ENOENT wraps `ErrSnapshotMetadataDumpMissing` (not `ErrRestoreVerifyFailed`)**
- **Found during:** Task 2 step 2 implementation.
- **Issue:** The plan's `<interfaces>` step 2 said "Open manifest file at `snap.ManifestPath(localStoreDir)`; ENOENT -> wrap ErrSnapshotMetadataDumpMissing (or a sibling sentinel — manifest-missing is functionally same triage as dump-missing for operators)". I picked `ErrSnapshotMetadataDumpMissing` for triage symmetry: a missing manifest and a missing dump are operator-observed identically ("the snapshot dir lost a file"), so collapsing them onto one sentinel keeps the Phase 25 HTTP-status mapping table smaller.
- **Fix:** Both step 2 (manifest open ENOENT) and step 4 (dump open ENOENT) wrap `ErrSnapshotMetadataDumpMissing`. Non-ENOENT manifest-open failures still wrap `ErrRestoreVerifyFailed` (a transient I/O failure on the manifest is a verify-path failure, not a missing-file failure).
- **Files modified:** `pkg/controlplane/runtime/snapshot.go`.
- **Commit:** `3547be1a`.

**2. [Rule 3 - Blocking issue] `time` import not added; safety-snap "name" reduced to log field only**
- **Found during:** Task 2 step 3 implementation.
- **Issue:** The plan's `<action>` Step 3 mentioned `safetyName := fmt.Sprintf("pre-restore-%s", time.Now().UTC().Format(time.RFC3339))` "for log only" then noted that `CreateSnapshotOpts` lacks a Name field. Since the name is never written anywhere (no Name field on CreateSnapshotOpts, no Name field on Snapshot model — see `pkg/controlplane/models/snapshot.go:22`), the formatted string would have been dead code. Removing it also drops the `time` import I would otherwise have to add.
- **Fix:** Log `safety_snap_id=<auto-uuid>` directly at every orchestration boundary; operator discovers the safety snap via `ListSnapshots` filtered by `created_at` desc per the plan body. No `time` import added.
- **Files modified:** `pkg/controlplane/runtime/snapshot.go` (no addition).
- **Commit:** `3547be1a`.

## Self-Check: PASSED

- `pkg/snapshot/hashset_from_metadata.go` — FOUND
- `pkg/snapshot/hashset_from_metadata_test.go` — FOUND
- `pkg/controlplane/runtime/snapshot.go` — extended (RestoreSnapshot appended at file tail)
- Commit `a5c2dc30` — FOUND (Task 1, hashset_from_metadata walker + 5 tests)
- Commit `3547be1a` — FOUND (Task 2, Runtime.RestoreSnapshot 8-step orchestration)
- All 5 hashset_from_metadata tests pass; `go vet` clean; `go build ./pkg/controlplane/runtime/...` succeeds
- Plan grep gates all positive (see Verification section)

---
*Phase: 24-restore-flow*
*Completed: 2026-05-28*

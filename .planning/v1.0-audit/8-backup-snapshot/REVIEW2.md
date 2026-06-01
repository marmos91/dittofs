# Backup / Snapshot / Restore — v1.0 Area Audit REVIEW (Round 2)

**Status:** NEEDS-FIX — 3 HIGH integrity holes (data-loss / silent-corruption) found at cross-component seams round-1 missed; architecture invariants still hold.
**Date:** 2026-06-01
**Scope:** ROUND 2 — missed-findings + integration/seam lens over the snapshot area. Re-verified all round-1 findings on current `develop` (every round-1 finding treated as KNOWN and NOT re-reported here unless its severity changed). New focus: cross-area/cross-component CONTRACT seams; ERROR & FAILURE paths (partial failure, mid-op crash, rollback, ctx-cancel, cleanup-on-error); CONCURRENCY under load (races, lock-ordering, use-after-close, goroutine/fd leaks); and whether round-1 HIGHs (there were none) and MEDs still reproduce. Covers `pkg/controlplane/runtime/snapshot*.go`, `pkg/controlplane/runtime/{blockgc,runtime}.go`, `pkg/blockstore/engine/{gc,flush}.go`, `pkg/blockstore/local/fs/{drain_reset,rollup,recovery,fs}.go`, `pkg/snapshot/snapshot_hold.go`, `pkg/metadata/store/{memory,badger,postgres}/{objects,backup,file_block_refs}.go`, `pkg/controlplane/store/snapshots.go`, `pkg/metadata/storetest/backup_conformance.go`.
**Cross-check refs:** Round-1 REVIEW (`.planning/v1.0-audit/8-backup-snapshot/REVIEW.md`, 0 HIGH / 4 MED / 12 LOW, PR-B NOT done). PR #797 (#789 multi-pass block-ref loss), #868 (#830/#831/#832 scale + `DecrementRefCountAndReap`), #853 (postgres byte-verify matrix), #838/#839 (postgres null-hash `file_blocks` reconcile). Engine reap economics: blockstore area audit. Round-1 MEDs C1/C2/R1/G1 and LOWs R-L1/R-L2/C-L1/C-L2/C-L3/G-L2 all RE-VERIFIED still present on develop (none fixed by shipped PRs) and are NOT re-reported except where round-2 sharpens or supersedes them.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---|---|---|---|
| incompleteness-deep | 1 | 3 | 1 | 0 |
| restore-failure-races | 1 | 0 | 2 | 0 |
| gc-hold-crossshare | 0 | 2 | 0 | 0 |
| snapshot-engine-integration | 1 | 0 | 1 | 0 |
| **Total** | **3** | **5** | **4** | **0** |

(De-duplicating the two flush.go-godoc LOWs reported independently by `restore-failure-races` and `snapshot-engine-integration` — same R-L2 item — the distinct LOW count is 3.)

**Verdict: NEEDS-FIX.** Round-1 audited each sub-area in isolation and used the no-op memory local store, so it scored 0 HIGH and rated the area PATCH-grade. Round 2 looked specifically at the *boundaries between* components and the *failure/concurrency* paths, and surfaced **three independently-verified HIGH integrity holes**, all silent (no runtime signal, no CI coverage), all reachable on ordinary admin/boot paths:

1. **H1 — manifest vs GC-live-set source divergence** (data loss on an ordinary snapshot *delete*): the GC live set comes from `EnumerateFileBlocks` while the manifest/hold comes from a *different* per-backend structure; a live block present only in the manifest-source is reaped after the snapshot is deleted.
2. **H2 — safety snapshot deletable during/after restore** (unrecoverable data loss / wedged share): `DeleteSnapshot` never fences against the per-share restore marker, so the sole rollback primitive can be deleted out from under an in-flight or crash-interrupted restore.
3. **H3 — startup rollback does not quiesce the fs rollup workers** (silent corruption of the rolled-back share): the rollback path skips the safety-snap's `DrainRollups`, and `ResetLocalState` never fences in-flight rollups, so a worker can overwrite restored `FileAttr.Blocks` with discarded post-snapshot chunk refs.

All three are the same *silent-incompleteness/corruption* family round-1 named — but reached through cross-component seams (GC↔backend structure, delete↔restore-marker, rollback↔rollup-worker) that an in-isolation audit could not see. None are caught by existing CI: the crash/rollback tests use the memory local store (rollup/reset no-ops), and the #853 byte-verify matrix exercises only the operator (non-rollback) restore path.

**Architecture invariants hold.** Block stores remain per-share; snapshot stays protocol-agnostic; file handles/paths are still DB- or registry-sourced and existence-gated (round-1 path-traversal refutation still stands). The HIGHs are integrity/concurrency bugs *within* the established design, not invariant violations — none requires a rewrite. Two adversarially-tested cross-area HIGH candidates were REFUTED as fail-safe (§3).

---

## 2. HIGH findings

Ranked by blast radius (ordinary-operation reachability × silence × irrecoverability).

### H1 — Manifest and GC live-set are built from DIFFERENT per-backend structures; a live block present only in the manifest-source is reaped by GC after the snapshot is deleted — `pkg/blockstore/engine/gc.go:403` (+ `objects.go` / `backup.go` per backend)

**What.** GC mark builds the live set from exactly two inputs: `EnumerateFileBlocks` per share (`gc.go:403`) plus `HoldProvider.HeldHashes` (`gc.go:409`); `addHash` skips zero hashes (`gc.go:384`). The hold provider streams the on-disk manifest (`snapshot_hold.go:53` → `snapshot.ReadManifest`). But the manifest is extracted at create time from a **different structure** than `EnumerateFileBlocks` in all three backends:
- **postgres:** enumerate = `SELECT hash FROM file_blocks` (`objects.go:350`); manifest = `SELECT DISTINCT hash FROM file_block_refs` (`backup.go:189`).
- **badger:** enumerate iterates `fb:` FileBlock entries → `block.Hash` (`objects.go:558`); manifest decodes `f:` File entries → `file.Blocks[].Hash` (`backup.go:128`).
- **memory:** enumerate over `s.fileBlockData.blocks` (`objects.go:174`); manifest over `s.files[].Attr.Blocks` (`backup.go:188`).

These pairs are populated by **independent write paths** — `PutFile` writes `file_block_refs` / `f.Blocks` / `Attr.Blocks` (the read+manifest authoritative source: `GetFile` populates `FileAttr.Blocks` from `file_block_refs` at `file_block_refs.go:62-64`, and the engine read/Delete path uses `FileAttr.Blocks`), while the `file_blocks` / `fb:` / `fileBlockData` rows are written separately by the rollup persister's `FileBlockStore.Put`. No single atomic op keeps them co-present.

**Why (data loss on an ordinary snapshot delete).** If a live file references hash H via the *manifest-source* but H's `EnumerateFileBlocks`-source row is missing or NULL, `EnumerateFileBlocks` emits zero for H (skipped at `gc.go:384`), so GC does **not** mark H live. While a snapshot is held, its manifest covers H and masks the gap. Delete that snapshot → the manifest hold disappears → the next `RunBlockGC` marks H neither live nor held and reaps it after the grace TTL, **even though a live file still references it.** Round-1's MED-C1/MED-R1 treated the two sources as equivalent; they are not, so a manifest that is a *superset* of `EnumerateFileBlocks` is invisible to the live set. The desync is not hypothetical: the codebase ships an active reconciliation (`reconcileFileBlockHashes`, `postgres/backup.go:340`: `UPDATE file_blocks fb SET hash=… FROM file_block_refs r WHERE fb.hash IS NULL`) that exists *precisely because* `file_blocks.hash` can be NULL while `file_block_refs.hash` holds the live value — the documented #838 class.

**Verifier rationale (HIGH, not CRITICAL).** Every structural claim and the loss chain were code-confirmed. Severity is HIGH rather than CRITICAL because the #838 `Put` fix (`postgres/objects.go:62-77`, persist hash whenever non-zero regardless of state) closes the common fresh-write NULL-hash vector; the live trigger now leans on residual/legacy NULL rows or a fully-absent `file_blocks` row — narrower, but the divergent-source architecture and the still-present reconciliation make the loss vector real and live-data-affecting.

**Fix.** Pin a conformance invariant `Backup` HashSet == `EnumerateFileBlocks`; and either make GC also enumerate the manifest-source set, or refuse to sweep a `file_block_refs`/`File.Blocks` hash that is absent from `file_blocks`/`fb:`/`fileBlockData`. (See §7 PR-B1.)

### H2 — `DeleteSnapshot` does not fence against the per-share restore marker — the safety snapshot (sole rollback primitive) is freely deletable during/after an interrupted restore → unrecoverable data loss / permanently-wedged share — `pkg/controlplane/runtime/snapshot.go:1530-1584` (fences) ; `:1017` (rollback restores `m.SafetySnapshotID`) ; `store/snapshots.go:65-76` (no guard) ; `snapshot_hold.go:92-102` (manifest = sole hold)

**What.** `DeleteSnapshot` fences only on the path-separator guard (`:1533`) and `isSnapInFlight` (`:1546`, which covers create/retry orchestration only). It never consults the per-share `RestoreMarker`, even though `GetRestoreMarker(shareName)` exists (`store/interface.go:527`) and the marker durably records `SafetySnapshotID` and `TargetSnapshotID` (`models/restore_marker.go:50-53`). A safety snapshot reaches `StateReady` (`:1273-1276`) **before** the marker is written (`putRestoreMarker` at `:1322`), and the orchestration goroutine deregisters at `StateReady`, so the safety snap is NOT in-flight and is freely deletable. Two reachable paths:
- **(A) live restore in-flight** — restore holds `restoreLock` (`*sync.Mutex`, `snapshot_hold.go:259`) but `DeleteSnapshot` takes the *different* `snapshotDeleteLock` (`*sync.RWMutex`, `snapshot_hold.go:239`); an admin `DeleteSnapshot(safetySnapID)` succeeds while the restore runs, deleting the row + `os.RemoveAll`-ing the dir (manifest + dump) the restore would roll back to.
- **(B) crash + reboot** — a marker references safety snap S; an admin deletes S as apparent clutter; reboot runs `recoverInterruptedRestores` → `restoreSnapshot(…,S,…)` at `:1017`; `GetSnapshot` returns `ErrSnapshotNotFound`; rollback fails; the marker is **intentionally retained** (`:1020-1031`, documented `:960-965`) and re-fails every boot.

Critically, `DeleteSnapshot`'s `os.RemoveAll` of the dir (`:1576`) removes S's manifest, which is the **only** GC hold for S's blocks (`snapshot_hold.go:92-102`, "no manifest = no hold"). S's exclusively-referenced blocks — frequently OLD blocks the default 1h grace TTL (`gc.go:280,505`) does not protect — then become reapable by any on-demand GC, so the pre-restore state is **permanently lost.**

**Why.** The safety snapshot is the entire integrity guarantee of restore: the orchestration goes destructive (`ResetLocalState`→`Reset`→`Restore`, `:1355-1389`) trusting that a crash or failure can roll back to S. With no fence, that guarantee is void — the share is left half-restored with no recoverable prior state, and the marker wedges every subsequent boot. Round-1 C-L1 fenced only the *target* snapshot-dir read and explicitly rated delete-vs-restore non-corrupting; it never considered the *safety* snap, so this class is uncaught by both the audit and the crash tests (`snapshot_restore_crash*_test.go` never delete the marker's safety snap).

**Fix.** In `DeleteSnapshot`, after the in-flight check and before `store.DeleteSnapshot`, load the per-share marker via `GetRestoreMarker`; if it exists and `snapID == marker.SafetySnapshotID` (ideally also `== marker.TargetSnapshotID`), refuse with a 409-mapped sentinel (`ErrSnapshotInFlight` or a new `ErrSnapshotMarkerProtected`). This runs under the `snapshotDeleteLock` already held. Add a regression test: write a marker naming S, `DeleteSnapshot(S)` expects refusal, plus a crash-reopen variant proving recovery still rolls back. Optionally back it with a DB-level guard so protection survives a runtime that bypasses the runtime layer.

### H3 — Startup rollback runs `ResetLocalState` WITHOUT quiescing the fs rollup workers — an in-flight rollup can overwrite restored `FileAttr.Blocks` (silent corruption of the rolled-back share) — `pkg/controlplane/runtime/snapshot.go:1254` (safety-snap skipped for `isRollback`) + `:1355` (ResetLocalState) vs `pkg/blockstore/local/fs/drain_reset.go:219-277` (no per-file-mu / rollupWg wait) + `rollup.go:158-264,506-514`

**What.** On the operator (enabled) restore path, the pre-restore safety snapshot's `CreateSnapshot` calls `bs.DrainRollups` (`snapshot.go:473`), which blocks on in-flight rollups via the per-file mutex (`drain_reset.go:65` `dirtyLen`→`mu.Lock`) and, with the share disabled, leaves `dirtyIntervals` empty before `ResetLocalState` — workers are quiesced. The **rollback** path (`recoverInterruptedRestores:1017` → `restoreSnapshot` `isRollback=true`) **skips the safety snapshot** (`:1254` gates the `CreateSnapshot` behind `if !internal.isRollback`) and therefore performs **no** `DrainRollups`/`StopRollup`/`rollupWg.Wait` before `ResetLocalState` (`:1355`).

Meanwhile the fs rollup worker pool is already alive on boot-recovered dirty intervals: `StartRollup` runs inside `AddShare` (`shares/service.go:1627`), which the boot sequence completes BEFORE `Serve`→`recoverInterruptedRestores` (`runtime.go:476-499`); boot recovery (`recovery.go:459-472`) rebuilds `dirtyIntervals`/`logIndices`/`logFDs` from the half-restored share's on-disk `.log` files, and the ticker (`rollup.go:89-90`→`scanAllFiles`) drives `rollupFile` on them. `ResetLocalState` only takes `bc.logsMu` and `clear()`s the per-payload maps (`drain_reset.go:224-256`); it never acquires the per-file mutex nor waits on `rollupWg` (that happens only at `Close`, `fs.go:847`). A worker that has already entered `rollupFile` and passed its `lf` re-validation (`rollup.go:191-196`) holds its captured `lf`/`tree`/`idx` as local variables and proceeds to `ObjectIDPersister` (`rollup.go:510-514`) → `coordinator.PersistFileBlocks` (`shares/coordinator.go:188-205`). If that persister lands **after** the rollback's `Reset` (`:1371`) + `Restore` (`:1389`) and the restored safety-snap metadata holds the same `payloadID`, `PersistFileBlocks` does `GetFileByPayloadID` → `file.Blocks=blocks` → `file.ObjectID=objectID` → `PutFile` unconditionally, overwriting the restored file's content pointer with chunk refs from the discarded post-snapshot append-log state.

**Why.** The rollback exists to *discard* a half-restored state and return to a verified safety snapshot. A racing rollup worker re-injecting discarded-state block refs into the just-restored file silently corrupts the recovered data — the file's bytes now resolve to post-snapshot chunks the operator explicitly chose to abandon. The orchestration comment (`:1342-1354`) only argues that clearing the overlay first leaves no dirty intervals "for a worker to flush" and that "BOTH the snapshot AND the safety snapshot drained rollups" — but on a rollback **no safety snapshot is created**, so that `DrainRollups` premise does not hold, and the comment does not address a worker already past `lf`-revalidation when `ResetLocalState` fires.

**Verifier rationale.** All five material claims code-confirmed. Window is narrow (worker must be mid-`rollupFile` at `Reset`→`Restore` and land its persister after `Restore`) but consequence is silent corruption of data the operator explicitly chose to recover — HIGH. **CI-blind:** the crash/rollback tests model "restart" by re-registering in-memory block stores whose `ResetLocalState`/`DrainRollups` are no-ops with inline rollup (`memory.go:454-474`), so no real worker ever races; the only fs-backed restore test (`snapshot_byteverify_test.go`, `Type:"fs"` at line 88) exercises the operator/non-rollback path.

**Fix.** On the rollback path, quiesce the fs rollup workers before `ResetLocalState`: either call `bs.DrainRollups(ctx)` at the top of `restoreSnapshot` for `isRollback` (cheap; waits via the per-file mutex), or have `ResetLocalState` acquire each per-file mutex (or wait on `rollupWg`) so it fences in-flight `rollupFile` calls rather than only taking `logsMu`. Add an fs-store-backed rollback regression test that seeds boot-recovered dirty intervals and runs `recoverInterruptedRestores` with the rollup ticker active, asserting the restored `FileAttr.Blocks` are not mutated.

---

## 3. Triage downgrades / RESOLVED

Two cross-area HIGH candidates at the engine seam were adversarially verified and **REFUTED** (kept here to document that the obvious "use-after-close / over-delete during share churn" hypotheses are fail-safe, not bugs):

**REFUTED — "GC sweep against a `remote.RemoteStore` that a concurrent `RemoveShare`/`releaseRemoteStore` `Close()`d is a use-after-free / corruption."** Fail-SAFE, not unsafe. S3 `Store.Close()` (`pkg/blockstore/remote/s3/store.go:527-533`) only sets `s.closed=true` under `mu`; `Get`/`Has`/`Delete`/`Walk` all gate on `checkClosed()` (`store.go:268,391,446,470`) returning `ErrStoreClosed`, so the sweep's `Delete` (`gc.go:525`) errors and is captured via `addError` — no crash, no corruption, no spurious delete.

**REFUTED — "`markPhase` mid-GC share removal over-deletes."** Fail-CLOSED. `GetMetadataStoreForShare` failing after a concurrent `RemoveStoreForShare` (`runtime.go:389`) returns an error from `markPhase` (`gc.go:399-401`) that aborts `CollectGarbage`'s mark phase **before any sweep/Delete runs** — no over-deletion.

Also re-confirmed (not bugs): no lock-ordering deadlock between `DeleteSnapshot` (one per-share lock) and `HeldHashes` (RLock-only, no nested lock acquisition); `snapDeleteLock` pointer identity correct (same `*sync.RWMutex` per share name — #701 regression cannot recur); `RemoveShare`'s `snapshots/` wipe racing `HeldHashes` is benign (`fs.ErrNotExist` short-circuit); `openManifestWithRetry`/`streamManifest` have no fd leak (deferred `f.Close()` on every success path) and use a bounded 1MiB scanner.

---

## 4. MED findings

### incompleteness-deep

- **Restore pre-verify and post-verify probe DIFFERENT hash sources — a restore can pass both gates while silently incomplete** — `snapshot.go:1229` (pre-verify probes the manifest source: `file_block_refs`/`File.Blocks`) vs `:1418` (post-verify probes `EnumerateFileBlocks` — the other source). No step asserts they match; `restoredCount` (`:1406`) and `snap.ManifestCount` are only logged (`:1457`). If the two sources disagree, both gates pass over an incomplete share whose extra block is GC-unprotected. *This is the restore-side face of H1.* (conf 76) — *Fix:* use the manifest source for both verifies; compare `restoredCount` to `snap.ManifestCount`; wrap `ErrRestoreVerifyFailed` on mismatch. **Supersedes round-1 MED-R1**, whose superset proposal false-fails the common #838 direction.

- **Round-1 PR-B1's create-side guard is unsound as proposed — it compares two different structures** — `snapshot.go:582-608`. The empty-manifest guard cross-checks manifest against `EnumerateFileBlocks`; round-1 MED-C1 proposes making it unconditional. But manifest = `file_block_refs`/`File.Blocks` and the check reads `file_blocks`/`fb`/`fileBlockData`, so the dangerous case (manifest missing a block a live file references) uses the *wrong* source and gains false confidence. (conf 68) — *Fix:* enumerate the manifest-source structure per backend; **do not implement round-1 PR-B1 as written.**

- **No conformance invariant asserts `Backup` hashset == `EnumerateFileBlocks`** — `pkg/metadata/storetest/backup_conformance.go:536-580`. `HashSetCorrectness` populates block refs only via `File.Blocks` (`PutFile` 563/577) and asserts `Backup` HashSet matches a manual set; it never populates the FileBlock CAS index nor asserts `EnumerateFileBlocks` == `Backup`, so it cannot catch the H1 divergence. Prior desyncs were caught only by the #853 byte-verify matrix. (conf 74) — *Fix:* add a storetest case writing through both the `Put` CAS path and `File.Blocks`, asserting `Backup` HashSet == `EnumerateFileBlocks`, plus a negative divergence case.

### gc-hold-crossshare

- **GC per-remote share scope is captured ONCE at start — a share added to a shared remote mid-GC is invisible to BOTH mark and hold phases (cross-share extension of MED-G1, untested)** — `pkg/controlplane/runtime/blockgc.go:43,60,64,76`. `RunBlockGC` calls `DistinctRemoteStores()` once under RLock, capturing `entry.Store` and `entry.Shares`, then runs the whole mark+sweep WITHOUT holding `sharesSvc.mu`. Both the live-set source (`perRemoteReconciler{shares: entry.Shares}` → `markPhase` enumerates only these, `gc.go:395-406`) and the snapshot hold provider (`snapshotHoldForRemote(entry.Shares)` → `snapshot_hold.go:73` iterates only this captured slice) are point-in-time. A NEW share C added to the SAME remote config after capture but mid-sweep (`acquireRemoteStore` bumps refCount on the existing configID, `shares/service.go:636-640`) has its live FileBlocks un-enumerated and its ready-snapshot manifest holds un-streamed; a CAS object referenced ONLY by C and older than `snapshotTime-grace` gets `Delete`d (`gc.go:505-525`). `StressGCvsDeleteUnderChurn` (`snapshot_hold_stress_test.go:79`) uses a FIXED share list, so it never exercises this. Narrowed to MED by the 1h grace TTL (C's fresh writes protected) and by old shared blocks remaining reachable through an enumerated co-tenant — real chiefly on the restore/import path where C comes online referencing pre-existing past-grace blocks mid-GC. (conf 60) — *Fix:* re-read per-remote share membership at mark-phase start under the shares lock and union late-added shares; or resolve membership lazily by configID; or take a read barrier blocking `AddShare`-to-an-in-GC-remote for the mark+hold span. Add a churn test that `AddShare`s a co-tenant with a ready snapshot mid-scan. Shares a root cause with round-1 MED-G1.

- **Round-1 MED-G1's manifest-ordering fix is necessary but NOT sufficient — the hold is a point-in-time read, not a continuously-consulted source** — `gc.go:318` (snapshotTime/mark), `:408-412` (HeldHashes read once), `:505` (grace filter). `CollectGarbage` captures the live+hold set once during mark and sweeps the frozen `gcs`. Round-1 MED-G1's fix (b) ("write the manifest before/atomically-with the dump") closes the window only for GC runs whose *mark* begins AFTER the manifest is on disk. A GC that already finished mark+hold before the new manifest was written will not observe the new hold even though the manifest now exists on disk before its `Delete`. So a snapshot created entirely within an in-progress GC's mark→sweep gap is still unprotected, and the freshly-de-referenced old block it captured can be reaped. (conf 55) — *Fix:* pair the manifest-ordering fix with a create barrier that prevents a snapshot create from flipping ready while a GC for that remote is between mark and sweep (e.g. take `snapshotDeleteLock` over the `Backup`→manifest span AND have the sweep re-consult the hold under that lock, or re-run `HeldHashes` immediately before each sweep `Delete`). Add a regression test: start GC, let mark complete, then create+finalize a snapshot referencing an about-to-be-de-referenced old block before the sweep deletes.

---

## 5. LOW findings

### restore-failure-races

- **R-L1 (round-1) — crash-recovery rollback re-fails permanently if the share is loaded Enabled at boot — STILL REPRODUCES on develop** — `snapshot.go:1149-1156` (precheck, no `isRollback` exemption) ; `:1017-1019` (rollback call). `recoverInterruptedRestores` invokes `restoreSnapshot(isRollback=true)`, but the share-enabled precheck at `:1153` returns `models.ErrShareEnabled` unconditionally — unlike the safety-snap (`:1254`) and marker (`:1321`) blocks, there is no `!internal.isRollback` exemption. An Enabled share with a marker present aborts the rollback every boot, leaving the share wedged half-restored. Liveness/operability gap, not data loss (safety snap retained — *assuming H2 is also fixed so the safety snap still exists*). *Fix:* skip/relax the Enabled precheck when `internal.isRollback` (force-disable for the rollback duration), mirroring the existing `isRollback` exemptions.

- **R-L2 (round-1) — `engine.ResetLocalState` godoc claims it runs AFTER metadata `Restore`, contradicting the correct BEFORE ordering — STILL REPRODUCES on develop** — `pkg/blockstore/engine/flush.go:114-121`. Godoc says "calls this AFTER the metadata Restore()", but the orchestration correctly calls `ResetLocalState` (`snapshot.go:1355`) BEFORE `Reset` (`:1371`) and `Restore` (`:1389`); `drain_reset.go:199-218` and the inline comment (`snapshot.go:1342-1354`) both justify BEFORE. Maintenance hazard at the exact seam H3 is about — a future edit trusting "AFTER" could reorder and reintroduce the closed stale-append-log corruption window. *Fix:* correct the `flush.go` godoc to BEFORE. (Independently reported by both the `restore-failure-races` and `snapshot-engine-integration` sub-audits — same item, counted once.)

---

## 6. Verified-correct

Re-checked on current develop and found OK (round-1 verified-correct items that still hold are not all repeated; these are the round-2-relevant boundary/failure/concurrency confirmations):

**Restore crash-consistency & destructive ordering (re-verified, still correct).**
- Durable `RestoreStepStarted` marker (`snapshot.go:1322`) written before `ResetLocalState` (`:1355`) → `Reset` (`:1371`) → `Restore` (`:1389`); a crash at any boundary leaves a marker naming the safety snap and `recoverInterruptedRestores` rolls back idempotently (step-independent).
- Safety snapshot genuinely created and waited to `StateReady` (`:1255-1276`) before any destructive op, using a `runtimeCtx`-derived wait so a client disconnect cannot abandon the wait; `safetySnapshotID` returned on every post-safety-snap failure path.
- Marker clear is the restore commit point: `DeleteRestoreMarker` failure after a successful post-verify returns `ErrRestoreAborted` (`:1432-1449`) rather than a success the next boot would undo.
- `recoverOrphanedSnapshots` runs BEFORE `recoverInterruptedRestores` and only flips `creating`→`failed`; it cannot touch a Ready safety snapshot, preserving the rollback target's `StateReady` requirement (`:1162`) across reboot.
- Rollback (`isRollback=true`) correctly suppresses both safety-snap creation (`:1254`) and marker writes (`:1321,1484`) — crash-during-rollback re-runs the identical rollback (no unbounded safety-snap chain, no marker overwrite).
- `ResetLocalState` (`drain_reset.go:219-277`) takes `bc.logsMu` for the whole teardown and, even when a per-fd `os.Remove` fails and sets `firstErr`, the trailing residual-log directory sweep (`:262-274`) still removes the on-disk `.log` files — an errored `ResetLocalState` that aborts restore does not leave a stale append-log overlay. *(Note: this protects against stale-overlay-on-error; it does NOT fence in-flight rollups — see H3.)*
- GC hold protects the TARGET snapshot's blocks throughout the empty-metadata (Reset→Restore) window: restore never deletes the target manifest, and `markPhase` holds via on-disk manifest existence (`snapshot_hold.go:92-102`), so blocks survive even when `EnumerateFileBlocks` is momentarily empty.
- `restoreSnapshot` serializes per-share via `restoreLock.TryLock` (`:1142`) with fail-fast `ErrRestoreInProgress`; two restores cannot interleave their destructive `Reset`+`Restore`.
- `DeleteSnapshot`'s path-separator guard (`:1533`) + parameterized `(share_name,id)` delete (`store/snapshots.go:66-68`) keep `snapID` opaque and prevent dir-escape; `ErrSnapshotNotFound` propagates with no `os.RemoveAll`. *(The marker-fence gap is H2; the path-traversal posture is unchanged from round-1.)*

**GC / engine concurrency seams (re-verified, fail-safe — see §3 for the two REFUTED HIGH candidates).**
- No periodic GC scheduler ships in v1.0 (`cmd/dfs/commands/start.go:194-201`) — GC is on-demand only, so the restore-window-vs-GC and create-window races (MED-G1 family) require a deliberate concurrent admin `gc` trigger. This narrows (does not eliminate) the exposure of H1/the two GC MEDs.
- `snapDeleteLock` pointer identity intact (`snapshot_hold.go:219-251` resolve the same per-share `*sync.RWMutex` from the shared registry) — #701 cannot recur; pinned by `StressGCvsDeleteUnderChurn`.
- `RemoveShare`'s `snapshots/` wipe racing `HeldHashes` is benign (`fs.ErrNotExist` short-circuit at `snapshot_hold.go:94-100,105-110`; co-tenant holds already captured in the frozen `gcs`; removed share's now-unreferenced blocks over-retained this pass — safe).

**Engine ↔ metadata seam.**
- Encrypted-share verify seam verified correct: manifest stores plaintext content hashes while the `EncryptedRemote` keys CAS objects by the same plaintext hash, so verify HEAD-probes resolve correctly on encrypted shares.

---

## 7. Recommended PR-B shape

Three focused fix PRs (one per HIGH), then defer MED/LOW as tracked issues.

**PR-B1 (highest value — closes the source-divergence data-loss class and corrects round-1's unsound guard). Targets H1 + the two incompleteness-deep MEDs + the conformance MED.**
- Pin a conformance invariant in `backup_conformance.go`: write through BOTH the `Put` CAS path and `File.Blocks`, assert `Backup` HashSet == `EnumerateFileBlocks`, add a negative divergence case (`storetest:536-580`).
- Make GC either enumerate the manifest-source set too, or refuse to sweep a `file_block_refs`/`File.Blocks` hash absent from `file_blocks`/`fb:`/`fileBlockData` (`gc.go:403`).
- Replace round-1's proposed create-side guard (`snapshot.go:582-608`) with one that enumerates the *manifest-source* structure per backend — **do not ship round-1 PR-B1 as written.**
- Make restore pre- and post-verify (`snapshot.go:1229,1418`) probe the SAME (manifest) source; compare `restoredCount` to `snap.ManifestCount`; wrap `ErrRestoreVerifyFailed` on mismatch. This subsumes/supersedes round-1 MED-R1.

**PR-B2 — marker-fence on `DeleteSnapshot`. Targets H2.** After the in-flight check, load `GetRestoreMarker(share)` and refuse `DeleteSnapshot` when `snapID` matches `SafetySnapshotID` (and ideally `TargetSnapshotID`) with a 409-mapped sentinel; runs under the already-held `snapshotDeleteLock`. Add the write-marker-then-attempt-delete regression test + a crash-reopen variant proving recovery still rolls back. Optionally add a DB-level guard. Cheap and self-contained.

**PR-B3 — quiesce fs rollup workers on the rollback path. Targets H3.** Call `bs.DrainRollups(ctx)` at the top of `restoreSnapshot` for `isRollback`, OR have `ResetLocalState` acquire each per-file mutex / wait on `rollupWg` so it fences in-flight `rollupFile`. Add an fs-store-backed rollback regression test that seeds boot-recovered dirty intervals with the rollup ticker active and asserts restored `FileAttr.Blocks` are not mutated. Bundle the R-L2 `flush.go` godoc fix here (same seam).

**Defer as tracked GitHub issues (no PR):**
- **GC cross-share / point-in-time MEDs:** the `blockgc.go` captured-share-scope MED and the MED-G1 "hold is point-in-time, not continuous" refinement — file together (shared "GC mark/hold resolves live membership and re-consults holds before sweep, not a captured slice" root cause). Mitigated by on-demand-only GC + 1h grace.
- **LOW liveness:** R-L1 (rollback Enabled-precheck) — group with PR-B2/PR-B3 if cheap (interacts with H2/H3).
- **LOW maintenance:** R-L2 (flush.go godoc) — fold into PR-B3.
- **Round-1 LOWs still reproducing (already filed/known):** C-L3 (snapDeleteLocks/restoreLocks map leak), G-L2/C-L2 (`AcquireDeleteLock` dead prod code), G-L1 (postgres multi-row decrement) — confirmed present, not re-reported.

---

## 8. Coverage

**Audited (four parallel round-2 sub-audits, every HIGH independently adversarially verified, round-1 findings treated as KNOWN baseline):**
- **incompleteness-deep** — manifest-source vs `EnumerateFileBlocks`-source structural divergence across all three backends (`gc.go`, `objects.go`, `backup.go`, `file_block_refs.go`); pre/post-verify source mismatch (`snapshot.go:1229,1418`); soundness of round-1's proposed create-side guard; conformance-suite coverage gap (`backup_conformance.go`); postgres `reconcileFileBlockHashes` NULL-`content_id` no-op residue.
- **restore-failure-races** — `DeleteSnapshot` vs restore-marker fence (H2); crash-consistency / destructive-step ordering re-verification; safety-snap creation/wait; marker-clear commit point; orphan-vs-interrupted recovery ordering; rollback suppression of safety-snap+marker; `ResetLocalState` error-path overlay cleanup; per-share `restoreLock` serialization; path-separator guard.
- **gc-hold-crossshare** — GC per-remote share-scope capture-once (cross-share MED-G1 extension); MED-G1 point-in-time-hold refinement; REFUTED use-after-`Close` of `RemoteStore` during `RemoveShare`; REFUTED over-delete on mid-GC `RemoveStoreForShare`; lock-ordering (DeleteSnapshot vs HeldHashes); snapDeleteLock pointer identity; `RemoveShare` wipe vs `HeldHashes`; manifest fd-leak / scanner bound.
- **snapshot-engine-integration** — startup-rollback ↔ fs-rollup-worker quiesce seam (H3); `AddShare`/`StartRollup` vs boot-recovery vs `Serve` ordering; `recovery.go` dirty-interval rebuild; `rollupFile` lf-revalidation window → `coordinator.PersistFileBlocks`; encrypted-share verify seam; R-L2 godoc.

**Not audited / out of scope (track separately if needed):**
- Block-store engine reap *internals* beyond the snapshot-relevant `Delete`/`Truncate` callers and `DecrementRefCountAndReap` contracts — blockstore area audit.
- Live multi-node / replicated-snapshot semantics — DittoFS is single-node (per FAQ).
- End-to-end mounted-share byte-verify under live NFS/SMB load — #853 CI byte-verify matrix + e2e suite; note the matrix covers only the operator (non-rollback) restore path, so H3 remains CI-blind until an fs-backed rollback test is added.
- The unrelated CAS block-layout migration — confirmed (round-1) not a snapshot compat shim; not re-examined.

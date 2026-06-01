I have all the verified findings data I need. Let me synthesize the REVIEW.md directly.

# Backup / Snapshot / Restore — v1.0 Area Audit REVIEW

**Status:** PATCH-grade — no HIGH integrity holes; architecture invariants hold.
**Date:** 2026-06-01
**Scope:** Reference-CAS snapshots (metadata dump + hash manifest + GC hold), create / restore / delete orchestration, GC reap + hold, concurrency-lifecycle, security surface, simplicity-bloat. Covers `pkg/controlplane/runtime/snapshot*.go`, `pkg/snapshot/`, `pkg/metadata/store/{memory,badger,postgres}/backup.go`, `pkg/controlplane/runtime/{shares,lifecycle}/`, `pkg/blockstore/engine/` reap callers, `internal/controlplane/api/handlers/snapshot.go`, `cmd/dfsctl/.../snapshot/`.
**Cross-check refs:** PR #665 (SNAP-01..05), #797 (multi-pass block-ref data-loss fix, #789), #868 (#830/#831/#832 scale ceilings + GC RefCount-0 reaping via atomic `DecrementRefCountAndReap`), #853 (postgres byte-verify matrix), #838 (postgres null-hash). Area-7 carryovers (snapshot-show path traversal; parallel squash drift) addressed in §3.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---|---|---|---|
| snapshot-create | 0 | 2 | 0 | 0 |
| restore | 0 | 1 | 2 | 0 |
| gc-hold | 0 | 1 | 2 | 0 |
| concurrency-lifecycle | 0 | 0 | 3 | 0 |
| security-surface | 0 | 0 | 2 | 0 |
| simplicity-bloat | 0 | 0 | 3 | 0 |
| **Total** | **0** | **4** | **12** | **0** |

**Verdict: PATCH-grade.** Zero HIGH findings across all six sub-audits. Every HIGH candidate was adversarially verified and either confirmed-absent or downgraded (see §3 for the refuted area-7 path-traversal carryover). The orchestration is crash-consistent, destructive-step ordering is correct, all three metadata-store `Restore` backends are atomic-or-self-cleaning, the GC mark-sweep is fail-closed, `DecrementRefCountAndReap` is TOCTOU-free in all three backends, and the Copilot-caught over-decrement class (#868) is genuinely fixed and pinned by storetest.

**Architecture invariants hold.** Block stores remain per-share; snapshot is protocol-agnostic (all adapters converge at `common.WriteToBlockStore`); file handles / paths are never built from raw client input (all path components are DB- or registry-sourced and existence-gated); every snapshot route inherits `RequireAdmin`.

**The unifying theme across all 4 MED findings is silent incompleteness of the #789/#838 class:** create and restore both verify *only the hashes that are present* (in the manifest, or in the restored metadata) and never cross-check that set against an independent ground-truth count. A backend `Backup`/`Restore` that silently *undercounts* FileBlock rows produces a durable-looking but incomplete snapshot/restore with **zero runtime signal** — today this regression class is caught only by the #853 byte-verify CI matrix, not by any production guard. The recommended PR-B (§7) closes this class at both ends with a single cheap manifest-superset assertion.

---

## 2. HIGH findings

**None.** All HIGH candidates were verified-absent or downgraded. See §3 for the one explicitly-refuted carryover and §4 for the silent-incompleteness MEDs that, in aggregate, are the area's most consequential gap.

---

## 3. Triage downgrades / RESOLVED

**Area-7 carryover: "snapshot-show URL share-name / snapshot-id reaches `os.Stat`/`os.Open`/`filepath.Join` without `../`-separator rejection" — REFUTED (not a finding).**
Every filesystem path component is DB- or registry-sourced and gated by an existence lookup *before* any `os.Stat`/`os.Open`/`filepath.Join`:
- The show handler's `toWire` (`internal/controlplane/api/handlers/snapshot.go:250,256`) builds paths from `s.ID` and `s.ShareName` returned by `GetSnapshot`, which is a parameterized `WHERE share_name=? AND id=?` (`pkg/controlplane/store/snapshots.go:37`). A traversal `snapID`/name never matches a row → `ErrSnapshotNotFound` (404) before any path is computed.
- `localStoreDir` is a registry map lookup keyed by the DB-stored share name (`pkg/controlplane/runtime/shares/service.go:1011`); a traversal name is never a registered key (`ErrShareNotFound`). The dir itself is derived at `AddShare` via `sanitizeShareName` (`url.PathEscape`, `service.go:291`).
- Restore (`snapshot.go:1184,1285`) and the GC hold scan (`snapshot_hold.go:93`) likewise build paths only from DB-sourced `snap.ID`/`snap.ShareName`.
- `DeleteSnapshot` additionally carries an explicit separator guard (`snapshot.go:1533`) **and** a prior row-match.

The residual defense-in-depth gap (the guard is implicit/inconsistent across call sites) is retained as a LOW (§5, security S-2), not a HIGH.

**Area-7 carryover: "two parallel squash impls can drift" — REFUTED (not a finding).**
The simplicity-bloat audit confirmed a *single* `Backup→dump→manifest→drain→verify` pipeline, a *single* merge-by-offset block-ref persister (the #789 fix is not duplicated), exactly one `Backup`/`Restore` per engine behind one shared envelope (`EnvelopeVersion=1`, no legacy/v2 branches). The per-snapshot-dir wipe (`DeleteSnapshot`) vs whole-snapshots-tree wipe (`RemoveShare`) are different-granularity operations, not a duplicated impl. No v0.13 backup compat shim exists in the backup path.

---

## 4. MED findings

All four MEDs are the same silent-incompleteness (#789/#838) class — partial-undercount producing a durable-looking but incomplete artifact with no runtime signal.

### snapshot-create

**MED-C1 — Verify gate never cross-checks a NON-empty manifest against live block refs — partial undercount marks snapshot durable** — `pkg/controlplane/runtime/snapshot.go:582,654` (confidence 80)
*What:* The empty-manifest completeness cross-check (`HashSetFromMetadataStore` vs manifest) only runs when `manifestCount==0` (line 582). `VerifyRemoteDurability` (line 654) only probes hashes that *are* in the manifest. If `Backup()` undercounts — captures e.g. 90 of 100 referenced hashes — the manifest is partial, verify passes (the 90 are durable), and the row flips `ready + remote_durable=true`. The 10 missing blocks are neither verified nor GC-held (the on-disk manifest is the hold source in `snapshot_hold.go`), so a later GC can reap them and the snapshot becomes silently unrestorable.
*Why:* Exactly the multi-pass per-pass-REPLACE data-loss class (#789). The only defense on the non-empty success path is the `Backupable` single-view contract being correct; there is no manifest-vs-live count/identity cross-check.
*Fix:* Make the completeness cross-check unconditional (not gated on `manifestCount==0`): after `Backup`, enumerate live referenced hashes (`HashSetFromMetadataStore` over the same view) and fail the snapshot if the manifest is not a superset of the live set (or at minimum compare counts). `EnumerateFileBlocks` already streams cheaply, so the extra walk is bounded and converts a silent-incompleteness bug into a loud create failure.

**MED-C2 — Badger `Backup` silently drops a file's block hashes from the manifest on malformed `f:` JSON while still writing the file to the dump** — `pkg/metadata/store/badger/backup.go:123` (confidence 72)
*What:* During hash extraction, badger backup `json.Unmarshal`s each `f:` entry; on error it logs a `Warn` and `continue`s (lines 123–126), skipping that file's `File.Blocks` hashes — but the raw key/value bytes for the same entry were already written to the dump above (lines 102–118). The dump round-trips the file on restore, but its block hashes are absent from the manifest, so they are neither remote-verified nor GC-held.
*Why:* A file that restores but whose blocks were GC-reaped (manifest never held them) reads as missing/zeros — silent data loss. The `manifestCount==0` cross-check won't fire if other files keep the manifest non-empty, and `Backup`'s hash source (`f:` `File.Blocks`) differs from `EnumerateFileBlocks`' source (`fb:` CAS index), so the guard cannot reliably catch this even when it runs.
*Fix:* Treat a malformed `f:` value during backup as a hard error (`return` wrapped `metadata.ErrBackupAborted`) rather than warn+continue — the store wrote that JSON, so a decode failure indicates corruption that must fail the snapshot.

### restore

**MED-R1 — Post-verify never compares restored hash set against the snapshot manifest — silent FileBlock-row loss in `Restore` would pass the restore gate** — `pkg/controlplane/runtime/snapshot.go:1400-1422` (confidence 78)
*What:* After Reset+Restore, post-verify calls `HashSetFromMetadataStore` on the *freshly-restored* metadata (line 1401) and HEAD-probes those hashes on the remote (line 1418). It never compares that set, or `restoredCount` (computed at 1406 but only logged), against the snapshot manifest already in hand (parsed at line 1194) or `snap.BlockCount`. If a metadata `Restore` silently drops some FileBlock rows (the exact #789 class that shipped to production), the restored metadata references fewer hashes, every surviving hash still resolves on the remote, and post-verify returns success — reporting a complete restore over a partially-restored share.
*Why:* The restore post-verify is sold (godoc, `RestoreStepRestored` marker) as the integrity gate. Verifying only what survived cannot detect what was lost. The manifest is the snapshot's recorded ground truth and is already loaded for pre-verify. Today covered only by the #853 byte-verify CI matrix, not any runtime guard.
*Fix:* After post-verify, assert the restored hash set is a superset of (ideally equal to) the snapshot manifest — for every `h` in manifest require `restoredHashes.Contains(h)` — and/or compare `restoredCount` against `snap.BlockCount`/`manifest.Len()`; on mismatch wrap `models.ErrRestoreVerifyFailed` (safety snap is already retained, so next-boot recovery rolls back). Cheap: manifest is already in scope at line 1194.

### gc-hold

**MED-G1 — Snapshot create-window: hashes captured by `Backup` are unprotected until the manifest is written on disk; concurrent delete+GC can sweep a still-to-be-referenced chunk** — `pkg/controlplane/runtime/snapshot.go:490-533` (confidence 62)
*What:* `runSnapshotOrchestration` captures the live FileBlock hash set in memory via `WriteMetadataDumpAtomic`/`Backup` (line 490) and only later persists `manifest.hashes` via `WriteManifestAtomic` (line 523). The GC `HoldProvider` protects a snapshot's chunks *only* by on-disk manifest existence (`snapshot_hold.go:92-102`), and snapshot create does **not** take the `snapshotDeleteLock` RWMutex (only `DeleteSnapshot` does, `snapshot.go:1537`). In the window between `Backup` and manifest write, the captured hashes have no hold. If during that window the last live file referencing chunk X is deleted (`DecrementRefCountAndReap` drops RefCount→0, removing X from `EnumerateFileBlocks`) and a concurrent `RunBlockGC` mark/sweep runs, X is absent from the live set and unprotected by any manifest, so `sweepPhase` (`gc.go:508-525`) deletes it from the remote. The manifest is then written referencing the now-deleted X → a snapshot that fails to restore (silent data loss).
*Why:* The hold mechanism's entire purpose is to guarantee snapshot-referenced chunks are never reaped. The on-disk-manifest-as-ground-truth model leaves the dump→manifest interval uncovered, and create takes no lock against the GC mark phase the way delete does. (Verifier kept this MED, not HIGH: requires a concurrent cross-share GC + last-reference delete landing inside a narrow window, and the default 1h grace TTL mitigates the *recent*-chunk case — though notably *not* an old chunk being snapshotted then deleted, which grace does not protect.)
*Fix:* Either (a) acquire the per-share `snapshotDeleteLock` for the `Backup`→manifest span (RLock is insufficient — must block reap-driving operations or fold into the same mutex), or (b) write the manifest before/atomically-with the dump so the hold exists the instant any captured hash could be reaped. Add a regression test that deletes the sole referencing file between `Backup` and manifest write while a GC sweep runs.

---

## 5. LOW findings

### restore
- **R-L1 — Startup rollback fails permanently if the share is Enabled at boot** — `snapshot.go:1149-1156` (conf 62). `recoverInterruptedRestores` calls `restoreSnapshot` with `isRollback=true`, which still runs the precheck returning `models.ErrShareEnabled` when `IsShareEnabled` is true. If a share is loaded as enabled at startup with a restore marker present, the rollback aborts, the marker is retained, and the same failing rollback re-attempts every boot — share wedged half-restored (non-destructive: safety snap retained, share not served since recovery precedes adapter `Serve`). *Fix:* skip/relax the Enabled precheck when `internal.isRollback`, or force-disable the share before invoking the rollback.
- **R-L2 — `engine.ResetLocalState` godoc states it runs AFTER metadata `Restore`, contradicting the (correct) orchestration which runs it BEFORE** — `pkg/blockstore/engine/flush.go:114-121` (conf 88). Orchestration calls `ResetLocalState` (line 1355) *before* `Reset` (1371) and `Restore` (1389); `drain_reset.go:200` and the inline comments correctly justify BEFORE. Stale comment is a maintenance hazard (a future edit trusting it could reintroduce the closed corruption window). *Fix:* correct the `flush.go` godoc to BEFORE.

### gc-hold
- **G-L1 — Postgres multi-row-per-hash: `AddRef` bumps all rows but `DecrementRefCountAndReap` (via `GetByHash`) targets one non-deterministic row** — `pkg/metadata/store/postgres/objects.go:266-279` (conf 70). `GetByHash` has no `ORDER BY`/`LIMIT`; `AddRef` bumps ALL matching rows (migration 000011). On legacy multi-row data increment/decrement cardinalities diverge — but failure mode is **over-retention only** (any surviving row keeps the hash in the GC live set); no data loss. *Fix:* make `GetByHash` deterministic with a documented chosen-row contract, or collapse multi-row hashes in the coordinator. Low priority — legacy pre-migration data only, safe bias.
- **G-L2 — `SnapshotHoldProvider.AcquireDeleteLock` is exercised only by tests; production `DeleteSnapshot` bypasses it** — `snapshot_hold.go:203-207` (conf 66). Production takes the same mutex directly via `r.snapshotDeleteLock(share).Lock()` (`snapshot.go:1537-1539`); both resolve the identical shared mutex (no correctness divergence). Mild surface bloat + drift risk. *Fix:* route production through `AcquireDeleteLock`, or drop the method and have tests take the mutex directly. (Same item independently flagged by concurrency-lifecycle, see C-L2.)

### concurrency-lifecycle
- **C-L1 — `DeleteSnapshot` does not fence against an in-flight `RestoreSnapshot` reading the same snapshot dir** — `snapshot.go:1530-1576` vs `1141-1295` (conf 80). `DeleteSnapshot` fences only on `isSnapInFlight` (create/retry), not on a restore reading the same snapshot's manifest/dump (restore holds a *different* mutex, `restoreLock`). Non-corrupting: on Unix the dump `*os.File` survives unlink so an in-progress read completes; if delete wins the pre-open window, `os.Open` returns ENOENT and restore aborts with `ErrSnapshotMetadataDumpMissing` *before* the first destructive op — no half-restore. Only observable effects are a confusing late abort (a safety snap may already exist) and, on Windows, `os.RemoveAll` failing while the dump is open. *Fix:* take the per-share `snapshotDeleteLock` (read side) around restore source-dir reads, or add an `isRestoreInProgress` fence to `DeleteSnapshot`.
- **C-L2 — `AcquireDeleteLock` is dead production code (tests-only)** — `snapshot_hold.go:203-205` (conf 85). Duplicate of G-L2; consolidate.
- **C-L3 — `snapDeleteLocks` / `restoreLocks` maps grow unbounded across share add/remove churn** — `runtime.go:90-91/101-102`; `RemoveShare` at `380-391` never deletes entries (conf 78). `RemoveShare` already prunes `metadataService.RemoveStoreForShare` for exactly this reason but does not prune these two maps. Not racy (mutexes carry no share-specific state; same-name re-add safely reuses the stale mutex). *Fix:* in `RemoveShare`, after `cancelAndWaitInFlightSnaps`, delete the share's entry from both maps under their respective mutexes.

### security-surface
- **S-L1 — Restored metadata dump is replayed without integrity verification (no signature/MAC over the dump)** — `snapshot.go:1286-1392` (conf 72). Pre/post-verify only HEAD-probe that CAS *content* hashes resolve; they do not detect tampering of non-block metadata (paths, owners/UIDs, mode bits, ACLs, directory structure) referencing still-valid CAS hashes. The dump is written `0o600` but nothing binds it to the snapshot row or signs it. Defense-in-depth only: requires local data-dir write = host-root-equivalent, op is admin-gated, leaves the share disabled for inspection. *Fix:* record a content hash (or HMAC) of `metadata.dump` on the snapshot row at create time, verify before `backupable.Restore`; or document the trust assumption (data-dir integrity == TCB).
- **S-L2 — Path-traversal guard exists only on `DeleteSnapshot`; other snapshot paths rely solely on the DB-existence gate (defense-in-depth inconsistency)** — `snapshot.go:1533` (conf 60). `DeleteSnapshot` builds `SnapshotDir` from the raw URL `snapID` so it needs the explicit `strings.ContainsAny` guard; show/restore/hold-scan build paths only from DB-returned values *after* a parameterized existence lookup, so they are safe but implicitly so. Risk is future drift — a new path built from raw URL input before a DB check would inherit no guard. *Fix:* add a shared `snapID` validator (UUID-shape / reject separators) applied at handler entry for all snapshot routes, mirroring the `DeleteSnapshot` guard.

### simplicity-bloat
- **SB-L1 — Planning/phase IDs leaked into snapshot/runtime source comments (violates no-plan-IDs convention)** — `snapshot_hold.go:25,27,43,58,96,192,197,238`; `runtime.go:76-79,89,105-107,140-141,171,370-381,479-483,510,760-761`; `dump.go:19,26`; `retry.go:12`; `lifecycle/service.go:47,160,249` (conf 95). Comments embed `D-23-02`, `D-23-04`, `D-23-17 #R3-1`, `Phase 22 D-04/D-19`, `Phase 23 plan 23-04/23-05` etc. (~20 in `runtime.go`, 8 in `snapshot_hold.go`). Violates `feedback_no_phase_comments_in_code`; meaningless without the planning tree and rots as plans archive. *Fix:* strip the bracketed/inline plan IDs while keeping the surrounding prose. Mechanical, ~40 comment lines / 5 files, text-only, zero behavior risk.
- **SB-L2 — `pkg/snapshot/retry.go` is a 30-line file for one 8-line validator with a single caller** — `retry.go:20-29` (conf 55). `ValidateRetryTarget` called once from `snapshot.go:133`. Borderline — has dedicated tests and lives alongside other format helpers; the seam is defensible. *Fix (optional):* inline into the `RetryOf` path and delete `retry.go`+`retry_test.go` (~45 LOC). Acceptable to leave.
- **SB-L3 — `WriteManifest` (non-atomic) exported but only called by `WriteManifestAtomic`** — `manifest.go:29-43` (conf 45). Exported with one internal caller; `doc.go` frames it as the public format writer and it is independently unit-tested. *Fix (optional):* unexport to `writeManifest`. Low value; safe to leave.

---

## 6. Verified-correct

**Create / dump / manifest atomicity**
- `WriteMetadataDumpAtomic` / `WriteManifestAtomic` (`dump.go`, `manifest.go`) use temp+Flush+fsync+Close+rename with temp removal on any error — a crash mid-write leaves either old content or absence, never a half-file.
- Dump and verified hash set cannot drift: the in-memory `HashSet` returned by `Backup` is written to the manifest *and* passed unchanged to `VerifyRemoteDurability` — verify probes exactly the manifest contents, not a re-read (`snapshot.go:490,523,654`).
- Step-0 `DrainRollups` runs *before* `Backup` (`snapshot.go:473`) so `FileAttr.Blocks` is fully populated before the manifest is derived (prevents capturing an empty/partial manifest from un-rolled-up append-log data).
- Empty-manifest hollow-durability guard (`snapshot.go:582-611`) cross-checks an empty manifest against live `HashSetFromMetadataStore` and fails if the share still references hashes (closes the empty-manifest-on-non-empty-share window, #791-related).
- Memory/Postgres `Backup` are single-view consistent: memory derives the manifest from `fd.Attr.Blocks` under `s.mu.RLock` held through gob `Encode` (`memory/backup.go:101-191`); postgres derives dump (`file_block_refs COPY`) and manifest (`SELECT DISTINCT hash`) inside one REPEATABLE READ txn (`postgres/backup.go:86-133`).
- `ValidateRetryTarget` (`retry.go`) restricts retry to `StateFailed`; `RetryOf` reuses the dir and atomically overwrites dump then manifest; `StateReady` required for restore so a partially-rewritten failed row is never restorable.
- `VerifyRemoteDurability` distinguishes a sibling-cancel ctx error (safe to drop) from a remote-side ctx error while `errCtx` is still live (recorded as a real failure, `verify.go:95-109`).
- GC-hold-vs-create ordering is satisfied: `markPhase` (`gc.go:395-412`) walks live `EnumerateFileBlocks` per share *plus* the manifest hold; during create the share's live metadata still references every snapshotted block, so blocks are protected by the live mark independent of manifest finalization timing. (The narrow exception is the dump→manifest+concurrent-delete window — see MED-G1.)

**Restore ordering, atomicity, recovery**
- Destructive-step ordering: durable marker (`RestoreStepStarted`, `snapshot.go:1322`) is written *before* `ResetLocalState` (1355), then `Reset` (1371), then `Restore` (1389), with step advances at each boundary; a crash at any point leaves a marker naming the safety snap.
- `Reset` always precedes `Restore`, so the empty-destination precondition holds in every backend even after a partial prior restore.
- All three metadata `Restore` backends are atomic or self-cleaning: postgres `BEGIN`/deferred `ROLLBACK`, CRC verified before `COMMIT` (`backup.go:282,285,308-325`); memory decodes + CRC-verifies the full payload before any field mutation under `s.mu.Lock` (`backup.go:301,315-327`); badger streams via `WriteBatch` and `DropAll`s applied data on any read/ctx/CRC failure (`backup.go:206-212,281-286`).
- Pre-verify (1229) and post-verify (1418) both run against the remote; skipped only for genuinely local-only shares (`remoteStore==nil`) with the safety-snap `NoVerify` flag mirrored (1255).
- GC hold cannot reap blocks mid-restore: the source snapshot's manifest stays on disk throughout restore (never deleted by restore), so `HeldHashes` keeps its blocks held.
- Marker clear is the restore commit point: `DeleteRestoreMarker` failure after a successful post-verify returns `ErrRestoreAborted` (`snapshot.go:1432-1449`) rather than reporting success the next boot would undo.
- Safety snapshot is genuinely created and waited to `StateReady` before any destructive op (`snapshot.go:1255-1276`) using a `runtimeCtx`-derived wait; `safetySnapshotID` returned on every post-safety-snap failure path.
- `--force`/`AllowNonDurable` gating correct end-to-end (dfsctl `restore.go:81` → 412 refusal; `snapshot.go:1166-1169`; zero-value default refuses).
- Crash recovery runs on `r.runtimeCtx` before adapters `Serve` (`runtime.go:497`), after `recoverOrphanedSnapshots`; rolls back to the verified safety snap (`isRollback=true` suppresses safety-snap + marker writes); marker retained on failure for next-boot retry; idempotent.

**GC reap + hold**
- `DecrementRefCountAndReap` is atomic and TOCTOU-free in all three backends: memory under `s.mu` write lock + reap-at-zero; postgres single-tx `UPDATE` + conditional `DELETE WHERE ref_count=0`; badger single `Update` txn read-modify-write + reap.
- The two reap callers (engine `Delete` and `Truncate`) decrement at-most-once-per-distinct-hash with a kept-set guard — a hash shared across offsets/files is never over-dropped to zero. The Copilot-caught over-decrement class (#868) is genuinely fixed and pinned by storetest.
- Mark-sweep is fail-closed: mark errors abort the sweep; empty share list is a hard error; missing `LastModified` preserves the object.
- `ReadManifest` uses a bounded `bufio.Scanner` buffer (`maxManifestLine=1MiB`, `manifest.go:23,86`) and strictly validates each line as 64-hex `ContentHash` — no unbounded allocation/DoS from a hostile or corrupt manifest.

**Concurrency / lifecycle**
- GC-mark-vs-snapshot-delete race closed: `snapshotDeleteLock` returns the SAME `*sync.RWMutex` per share name for every provider instance; `HeldHashes` RLocks all borrowed locks for the whole scan; `DeleteSnapshot` Lock-s the same mutex. The #701 per-instance-mutex regression cannot recur; pinned by `TestSnapshotHoldProvider_StressGCvsDeleteUnderChurn`.
- No use-after-Close of `engine.Store` from the orchestration goroutine: `RemoveShare` calls `cancelAndWaitInFlightSnaps` (`wg.Wait`) *before* `sharesSvc.RemoveShare` closes the store. The area-7 H-A use-after-Close class does not reach the snapshot path.
- `HeldHashes` tolerates share removal mid-scan (`ErrShareNotFound`/ENOENT short-circuits; other IO errors propagate fail-closed).
- Restore serialized per-share via `restoreLock.TryLock` (fail-fast `ErrRestoreInProgress`); no destructive interleaving.
- `registerSnapInFlight`/`cancelAndWaitInFlightSnaps` publish-race handled (mutex held across `wg.Add(1)`; draining entry replaced with fresh slot); `WaitForSnapshot` does not leak goroutines or block forever (buffered cap-1 `doneCh`, send-then-close exactly once).
- No hold leak on synchronous create failure (`MkdirAll` path → `abortSnapInFlight` unregisters + close + `wg.Done` exactly once; orchestration goroutine not launched).

**Security**
- Encryption keys do not leak into snapshot artifacts: keys live in the remote `BlockStoreConfig "encryption.key"` sub-config (`shares/service.go:698-727`); the dump captures metadata-store content, the manifest captures CAS content hashes only — neither serializes `BlockStoreConfig` or key material.
- All snapshot REST routes inherit `RequireAdmin` (`router.go:186` parent group; snapshots nested at `203-208`).
- No snapshot import/upload-from-arbitrary-path surface: CLI restore is `cobra.ExactArgs(2)`; REST restore body carries only `allow_non_durable`; dump+manifest are only ever server-created.
- `mapSnapshotError` (`handlers/snapshot.go:279-329`) writes fixed strings per sentinel and never interpolates the raw error into the HTTP body; raw errors logged server-side only.
- Snapshot artifact files owner-only: `metadata.dump`/manifest temp `0o600`, snapshot dirs `0o750`.
- `RetryOf` is user-supplied but safe: routed only into the parameterized `GetSnapshot` lookup + `ValidateRetryTarget`, never into a filesystem path.

**Simplicity**
- Single `Backup→dump→manifest→drain→verify` pipeline; single merge-by-offset block-ref persister (#789 fix not duplicated); one `Backup`/`Restore` per engine behind one shared envelope (`EnvelopeVersion=1`, no legacy/v2 decode path).
- All `pkg/snapshot` exported helpers (`ValidateRetryTarget`, `WriteMetadataDumpAtomic`, `WriteManifestAtomic`, `ReadManifest`, `HashSetFromMetadataStore`, `VerifyRemoteDurability`, `VerifyEngine`, `deriveWaitCtx`) have live production callers — no dead code. `resolveSnapshotID` is a single shared dfsctl helper reused across create/show/delete/restore.
- No v0.13 backup compat shim in the snapshot path (the only v0.13 refs are in the unrelated CAS block-layout migration).

---

## 7. Recommended PR-B shape

**PR-B1 (highest value — closes the silent-incompleteness class at both ends). Targets MED-C1 + MED-R1.** Single theme, single helper. Add an unconditional manifest-superset assertion:
- Create side: after `Backup`, enumerate live referenced hashes over the same view and fail the snapshot unless the manifest ⊇ live set (`snapshot.go:582,654`).
- Restore side: after post-verify, assert restored hash set ⊇ snapshot manifest (already in scope at line 1194), wrap `ErrRestoreVerifyFailed` on mismatch (`snapshot.go:1400-1422`).
- Ship with a regression test that injects an undercounting `Backup`/`Restore` and asserts a loud failure — converting the #853 byte-verify CI-only coverage into a runtime guard.

**PR-B2 — badger backup hard-fail on malformed `f:` JSON. Targets MED-C2.** Replace warn+continue with `return metadata.ErrBackupAborted` (`badger/backup.go:123`). Small, self-contained; add a storetest case feeding a corrupt `f:` value.

**PR-B3 — close the create-window hold gap. Targets MED-G1.** Implement fix (b) — write the manifest atomically-with/before the dump so the hold exists the instant any captured hash is reapable — or take the per-share lock for the `Backup`→manifest span. Add the concurrent-delete-during-create-window regression test. (Lower urgency: narrow window, mitigated by grace TTL; but it is the one MED that can still lose data on the create path even with PR-B1 in place, since PR-B1's cross-check runs *after* the manifest write.)

**Defer as tracked GitHub issues (no PR):**
- LOW lifecycle/correctness: R-L1 (rollback Enabled-precheck), R-L2 (flush.go godoc), C-L1 (delete-vs-restore fence), C-L3 (map leak). Group R-L1+R-L2 with PR-B1 if cheap.
- LOW dedup/bloat: G-L2 == C-L2 (`AcquireDeleteLock` dead — file one issue), G-L1 (postgres multi-row decrement), SB-L1 (plan-ID comment strip — mechanical, can ride any PR touching the files), SB-L2/SB-L3 (optional inline/unexport).
- LOW security defense-in-depth: S-L1 (dump MAC — gate on whether data-dir is in TCB), S-L2 (centralized snapID validator). File as one "snapshot defense-in-depth" issue.

---

## 8. Coverage

**Audited (six parallel sub-audits, every HIGH candidate independently adversarially verified):**
- **snapshot-create** — `runSnapshotOrchestration`, dump/manifest atomicity, verify gate, ready+durable flip, orphan-row reconcile, all three backend `Backup` consistency contracts, `DrainRollups` ordering, `ValidateRetryTarget`/`RetryOf`.
- **restore** — `restoreSnapshot`, `restore_marker.go`, `verify.go`, `restore_opts.go`; destructive ordering, all three backend `Restore` atomicity, pre/post-verify, GC-hold-during-restore, `--force` gating, crash recovery + rollback idempotency.
- **gc-hold** — mark-sweep fail-closed design, `DecrementRefCountAndReap` TOCTOU across all three backends, the two reap callers' kept-set guard, manifest-existence hold model, snapshot-delete-vs-GC-mark race.
- **concurrency-lifecycle** — shared per-share RWMutex, orchestration-goroutine vs store-Close, `HeldHashes` mid-removal tolerance, restore `TryLock` serialization, register/cancel publish race, `WaitForSnapshot`, crash-recovery ordering.
- **security-surface** — path traversal (refuted), manifest/dump tampering, unintended-share overwrite, secret/key leakage, encrypted-share handling, auth gating, error-body sanitization, file modes.
- **simplicity-bloat** — single-pipeline / no-duplicate-squash verification, dead-code sweep, envelope versioning, v0.13 shim search, plan-ID-leakage scan.

**Not audited / out of scope (track separately if needed):**
- The block-store engine reap *internals* beyond the two snapshot-relevant callers (`Delete`/`Truncate`) and the `DecrementRefCountAndReap` backend contracts — full engine GC sweep economics belong to the blockstore area audit.
- Live multi-node / replicated-snapshot semantics — DittoFS is single-node (per FAQ); not exercised.
- End-to-end mounted-share byte-verify under live NFS/SMB load — covered by the #853 CI byte-verify matrix and the e2e suite, not re-run here.
- The unrelated CAS block-layout migration (`migrate_to_cas.go`, `types.go` dual-read) — touched only to confirm it is *not* a snapshot compat shim.

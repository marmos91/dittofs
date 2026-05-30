# Lock durability + grace — PR-B implementation blueprint

Decision (2026-05-30): **WIRE IT UP** — real cross-restart lock recovery. Source: code-architect analysis on `v1.0/lock-acl-audit`.

## Key facts
- Backends fully implement `LockStore` across all 3 stores: `pkg/metadata/store/{memory,postgres,badger}/locks.go` (store + MetadataStore + transaction layers). `DurableHandleStore` + `ClientRegistrationStore` likewise.
- `SetLockStore` (`lock/manager.go:888`) + `NewManagerWithGracePeriod`/`EnterGracePeriod` have **zero production callers**. Production builds at `pkg/metadata/service.go:95` via `NewLockManager()` (no store, no grace).
- **Real gap, not pure wiring**: lease persist paths already call `PutLock` (nil-gated). Byte-range `Lock()` (`manager.go:639`) + `AddUnifiedLock()` (`manager.go:1276`) have **NO** PutLock calls at all → Phase 1 must add persist/delete on byte-range acquire/release, not just flip the store.
- **Two grace machines**: NFSv4 `StateManager.gracePeriod` (`internal/adapter/nfs/v4/state/grace.go`, partially wired for OPEN) vs lock manager `GracePeriodManager` (`lock/grace.go`, NLM/SMB leases). Both no-op in prod. Phase 2 must coordinate (enter/exit together via callback).
- Wiring seam: `store.(lock.LockStore)` type-assert in `RegisterStoreForShare` (`var _ lock.LockStore = (*BadgerMetadataStore)(nil)` confirms backends satisfy it).

## Phase 1 — persistence wiring + byte-range PutLock (PR-B1, ~150-200 LOC, MED)
Closes area-5 H-2, NFS H7/H8.
- `service.go RegisterStoreForShare`: type-assert store→LockStore, `SetLockStore`, new `initLockManagerFromStore(ctx.Background)` helper = `IncrementServerEpoch` + `ListLocks{ShareName}` + replay into `unifiedLocks`/`locks` (no conflict-check on reload).
- `manager.go Lock()` (639): add `PutLock` on grant. Needs stable lock ID — deterministic key `share:sessionID:openID:offset:length` (avoids FileLock struct change) OR add `lockID` field (cleaner). 
- `manager.go Unlock()` (688) + `UnlockAllForOpen` (720) + `UnlockAllForSession` (754): `DeleteLock` on removal.
- `manager.go AddUnifiedLock()` (1276): `PutLock` on grant. `RemoveUnifiedLock()` (1315): `DeleteLock`.
- `RemoveClientLocks` (2351): `DeleteLocksByClient`. `RemoveAllLocks` (2342): `DeleteLocksByFile`.
- Latent bugs to pre-fix: `ToPersistedLock` double-log (`store.go:293`); FileLock has no ID field.
- Tests: `TestLock_PersistsAndReloads`, `TestAddUnifiedLock_PersistsAndReloads`, storetest lock round-trip across backends. Existing `leases_test.go:620/664/718/1097/1135` already call SetLockStore = regression suite.

## Phase 2 — grace + reclaim (PR-B2, stacked, ~200-250 LOC, MED-HIGH)
Closes area-5 H-1, NFS H15.
- `initLockManagerFromStore`: collect unique ClientIDs from persisted locks = expectedClients; `NewGracePeriodManager(dur, onGraceEnd=sweep-unreclaimed)`; `NewManagerWithGracePeriod`; `EnterGracePeriod(expectedClients)` if >0 (skip on fresh server).
- Config: reuse `NFSAdapterSettings.GracePeriod` (`pkg/controlplane/store/adapters.go:134`); pass via `LockManagerOptions.GracePeriod` (default 90s).
- `nlm.go LockFileNLM` (74): `IsOperationAllowed({IsNew:true})` before AddUnifiedLock when !reclaim.
- `nlm/handlers/lock.go` (88/122): map `ErrGracePeriod`→`NLM4DeniedGrace`.
- `reclaim.go` + NLM reclaim handler: call `MarkReclaimed(clientID)` on success (enables early grace exit).
- Coordinate: `GracePeriodManager.onGraceEnd`→`v4StateManager.ForceEndGrace`; `EnterGracePeriod`→`v4StateManager.StartGracePeriod` (register callbacks in NFS adapter `New()`).
- Verify `v4 AcquireLock` calls `CheckGraceForNewState` for non-reclaim.
- Tests: NLMLockDenied, NLMReclaimAllowed, EarlyExit, restart-integration.

## Phase 3 — two-store byte-range cross-scan (PR-B3, ~60-80 LOC, MED)
Closes area-5 H-3. NOT a full data-structure merge (deferred) — a cross-map conflict scan at admission.
- `Lock()` (639): after scanning `lm.locks`, also scan `lm.unifiedLocks[handleKey]` for overlap+exclusivity → `conflictFromUnified` adapter.
- `AddUnifiedLock()` (1276): after scanning unifiedLocks, also scan `lm.locks[handleKey]` → `fromFileLockToUnified` adapter.
- Keep both in-memory cross-scan AND store-based `CheckNLMLocksForLeaseConflict` (memory backend + pre-first-commit window).
- Tests: SMBLockBlocksNLM, NLMLockBlocksSMB, SameRangeSharedAllowed.

## PR sequence
PR-B1 (Phase 1) → PR-B2 (Phase 2, stacked) → PR-B3 (Phase 3, stacked, independent logic). Each: TDD RED→GREEN, code-simplifier + code-reviewer, `go test -race ./pkg/metadata/lock/...`, lint before push, assign marmos91, sign commits.

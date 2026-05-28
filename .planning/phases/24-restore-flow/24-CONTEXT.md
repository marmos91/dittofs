# Phase 24: Restore Flow - Context

**Gathered:** 2026-05-28
**Status:** Ready for planning
**GH issue:** [#643](https://github.com/marmos91/dittofs/issues/643)
**Milestone:** v0.16.0 Share Snapshots — Phase 5 of 6
**Depends on:** Phase 21 (per-engine `Backupable` drivers), Phase 22 (snapshot records + manifest + `HoldProvider`), Phase 23 (`Runtime.CreateSnapshot` orchestration + `WaitForSnapshot` helper + revised `HoldProvider` filter + sync gate)

<domain>
## Phase Boundary

Implement reference restore: `Runtime.RestoreSnapshot(ctx, shareName, snapID, opts) error` that swaps a share's metadata store contents from a snapshot's `metadata.dump`, gated by pre- and post-restore block verification, with a pre-restore safety snapshot for rollback. Share lifecycle (Disable/Enable) stays operator-driven via `dfsctl` — Restore refuses on enabled shares. No CLI command, no REST handler, no API client (those land in Phase 25).

**In scope:**
- `pkg/metadata/resetable.go` — new `Resetable` optional interface: `Reset(ctx context.Context) error` (truncate all metadata-store contents in-place; instance reused)
- Three backend impls: `pkg/metadata/store/memory/reset.go` (re-init maps under `mu.Lock`), `pkg/metadata/store/badger/reset.go` (`db.DropAll`), `pkg/metadata/store/postgres/reset.go` (`TRUNCATE TABLE ... CASCADE` on every Phase 20/21/22 table inside a single `REPEATABLE READ` tx)
- `pkg/metadata/storetest/reset_conformance.go` — `ResetThenRestoreConformance(t, factory)` scenario (Reset on populated store → state empty → `Restore` from dump → state equals original); applied to all 3 backends
- `pkg/controlplane/runtime/snapshot.go` — extended with `Runtime.RestoreSnapshot(ctx, shareName, snapID string, opts RestoreSnapshotOpts) error` (sync orchestration)
- `RestoreSnapshotOpts` struct: `AllowNonDurable bool` (per D-24-06), `SkipPreVerify bool` (escape hatch for D-24-08 paranoia tradeoff — planner discretion whether to expose)
- Orchestration sequence (D-24-09):
  1. Validate: share exists; `Enabled=false` else `ErrShareEnabled`; snapshot exists; `Snapshot.State=ready`; `Snapshot.RemoteDurable=true` OR `opts.AllowNonDurable=true` else `ErrSnapshotNotDurable`
  2. Pre-verify: read `manifest.hashes` for `snapID` → `VerifyRemoteDurability(ctx, remote, manifest, snapshot.sync_gate_concurrency)` (reuses Phase 23 P23-01 helper); fail-fast aborts before any destructive op
  3. Safety snapshot: `safetyID, _ := Runtime.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{Name: "pre-restore-<RFC3339>"})` then `_, _ = Runtime.WaitForSnapshot(ctx, safetyID)` (sync-gated; see D-24-04); on failure or non-ready final state → return `ErrRestoreSafetySnapFailed` before any destructive op
  4. Open snapshot dump: `os.Open(models.MetadataDumpPath(shareDataDir, snapID))`; on miss → `ErrSnapshotMetadataDumpMissing`
  5. Reset: `store.(Resetable).Reset(ctx)`; on miss-of-interface → `ErrMetadataStoreNotResetable`; on Reset error → wrap `ErrRestoreAborted` (share stays disabled, safety snapshot intact on disk for operator-driven rollback)
  6. Restore: `store.(Backupable).Restore(ctx, dumpReader)`; on error → wrap `ErrRestoreAborted` (same rollback story)
  7. Post-verify (REST-03): re-read freshly-restored `FileAttr.Blocks` hashes union via metadata-store walk → `VerifyRemoteDurability(ctx, remote, restoredHashes, snapshot.sync_gate_concurrency)`; on fail → `ErrRestoreVerifyFailed` (share stays disabled; safety snapshot intact for operator rollback via second `RestoreSnapshot` call against the safety-snap ID)
  8. Success: leave share disabled per D-24-01; operator runs `dfsctl share enable` after inspection; safety snapshot retained on disk (no auto-delete per D-24-04, operator deletes manually after verifying restore)
- Typed error sentinels in `pkg/controlplane/models/errors.go` (next to Phase 22/23 sentinels): `ErrShareEnabled`, `ErrSnapshotNotDurable`, `ErrSnapshotMetadataDumpMissing`, `ErrMetadataStoreNotResetable`, `ErrRestoreSafetySnapFailed`, `ErrRestoreAborted`, `ErrRestoreVerifyFailed`
- E2E integration test in `pkg/controlplane/runtime/snapshot_restore_test.go`: memory metadata + memory `RemoteStore` + memory `LocalStore` fixture (matches Phase 22 D-21 / Phase 23 P23-06 patterns); covers happy path (write → snap → mutate → restore → assert original), interrupted-restore (kill mid-Reset → assert share disabled + safety snap on disk), non-durable refused (RemoteDurable=false + AllowNonDurable=false → ErrSnapshotNotDurable), --force path (AllowNonDurable=true), pre-verify-fails-fast (manifest hash missing → returns before any Reset call), post-verify-fails (blocks deleted after Reset → ErrRestoreVerifyFailed + safety snap intact)

**Out of scope:**
- CLI command `dfsctl share snapshot restore` (Phase 25)
- REST handler `POST /api/v1/shares/{name}/snapshots/{id}/restore` (Phase 25)
- API client `pkg/apiclient/snapshots.go` Restore method (Phase 25)
- Auto-disable / auto-enable around Restore — operator-driven only per D-24-01
- Async orchestration mirroring Phase 23 D-23-13 — sync per D-24-02 (no restore-tracking row, no goroutine registry, no startup recovery for in-flight restores, no WaitForRestore helper)
- Automatic safety-snapshot cleanup on success — operator-driven manual cleanup per D-24-04
- Postgres/Badger coverage in the orchestration integration test (per Phase 23 P23-06 precedent; the Phase 21 conformance suite + new Phase 24 `ResetThenRestoreConformance` cover backend correctness)
- Metrics surface (deferred per Phase 23 D-23-16; promote in v0.17 observability pass if needed)
- Concurrent-restore exclusion via a new shared lock — share `Enabled=false` precondition (D-24-01) is the barrier; second `RestoreSnapshot` against same share while first is mid-flight would observe `Enabled=true` only after the first re-Enables (which it doesn't, per D-24-01), so the natural state is one-restore-at-a-time. Document the invariant; no additional lock.
- Cross-share restore (snapshot of share A restored into share B) — REST-01 scopes to same-share reference restore
- Block store mutations — blocks are CAS-immutable + HoldProvider-pinned via manifest; Phase 24 only verifies reachability

</domain>

<decisions>
## Implementation Decisions

### Share lifecycle and orchestration shape

- **D-24-01:** Share Disable/Enable stays OPERATOR-DRIVEN, bracketing `RestoreSnapshot`. Sequence: operator runs `dfsctl share disable <share>` → `dfsctl share snapshot restore <share> <id>` (Phase 25 wraps `Runtime.RestoreSnapshot`) → `dfsctl share enable <share>`. `Runtime.RestoreSnapshot` validates `share.Enabled=false` at entry; returns `ErrShareEnabled` otherwise. Failure path leaves share `Enabled=false` (REST-02 invariant for free; operator inspects + decides next step — re-enable original, retry restore, or rollback via safety snapshot). Rationale: explicit operator intent at every step; no implicit destructive op; pairs naturally with REST-02; consistent with existing `shares/service.go::DisableShare/EnableShare` API surface (`ErrShareAlreadyDisabled` sentinel pattern).

- **D-24-02:** `Runtime.RestoreSnapshot` is SYNCHRONOUS. Signature: `RestoreSnapshot(ctx context.Context, shareName, snapID string, opts RestoreSnapshotOpts) error`. Returns when all orchestration steps complete or first error. No restore-tracking DB row, no goroutine registry, no startup recovery for orphaned in-flight restores, no `WaitForRestore` helper. Rationale: restore is rare (disaster recovery / explicit operator action), already gated by share-disabled; the async machinery Phase 23 needed for `CreateSnapshot` (multiple-per-share, GUI-triggered) doesn't apply. Long-running restore handled by caller-supplied `ctx` deadline; HTTP handler in Phase 25 sets its own timeout.

### Metadata store reset

- **D-24-03:** New `Resetable` optional interface: `Resetable interface { Reset(ctx context.Context) error }`. Defined alongside existing `Backupable` in `pkg/metadata/` (file `resetable.go`, matching Phase 21's `backupable.go` placement). Reset truncates all store contents in-place — same instance reused, no close/reopen, no shares.Service unregister/re-register. Implemented on all 3 backends:
  - **memory:** acquire `mu.Lock`; reassign maps to fresh empty `make(...)`s (or `clear()` per-map)
  - **badger:** `db.DropAll()` (Badger's documented atomic truncate); same `*badger.DB` handle stays valid
  - **postgres:** single `REPEATABLE READ` tx; `TRUNCATE TABLE files, file_block_refs, file_blocks, objects, snapshots, synced_hashes, …` (enumerate every table created by Phase 20/21/22 migrations) `CASCADE`; same `*pgx.Pool` handle stays valid

  Phase 21 `ErrRestoreDestinationNotEmpty` semantics untouched — Reset makes the store empty, Restore then applies the dump. Two sequential ops under the share-disabled barrier; atomicity guaranteed by (a) share is disabled so no concurrent serving, (b) safety snapshot per D-24-04 enables rollback if Restore fails after Reset.

  Why this over recreate-store: avoids shares.Service unregister/re-register dance, avoids cached constructor args (paths/DSN), avoids race window where store is registered as nil, simpler test fixture, mirrors Phase 21's optional-interface pattern.

  Why this over truncate-in-Restore: keeps Phase 21 `ErrRestoreDestinationNotEmpty` contract intact (just-shipped semantic), separates reset-vs-restore for clean unit testing, doesn't bend the Backupable contract.

- **D-24-12:** `ResetThenRestoreConformance(t, factory)` scenario added to `pkg/metadata/storetest/`. Lives in new file `reset_conformance.go`. Steps: populate store with N files via existing Phase 21 fixture → `store.Backup(ctx, &dumpBuf)` → `store.(Resetable).Reset(ctx)` → assert empty (zero files via `ListFiles` or backend-specific count) → `store.(Backupable).Restore(ctx, &dumpBuf)` → assert restored count + content equality. Wired into each backend's existing `TestXxxStore_BackupConformance` test entry.

### Atomicity and interrupted-restore recovery

- **D-24-04:** Pre-restore SAFETY SNAPSHOT created at orchestration step 3. Mechanics: internal call to `Runtime.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{Name: "pre-restore-<RFC3339-timestamp>"})` then `Runtime.WaitForSnapshot(ctx, safetyID)` (Phase 23 D-23-19 helper) for sync wait until `state=ready`. **Sync gate ENABLED** (no `NoSyncGate: true`) — drains + verifies durability so safety snapshot is fully recoverable from remote, surviving subsequent remote-only failures. **Visible** in `share snapshot list` (no hidden Kind field; just a regular snapshot with a conventional name prefix). **No auto-delete** on Restore success — operator manually deletes via `dfsctl share snapshot delete <share> pre-restore-<...>` after confirming restored data is good. Failure paths: safety snapshot stays on disk + in DB, named so operator can identify + restore-from-it via second `RestoreSnapshot(safetyID)` call.

- **D-24-05:** Restore-from-safety-snapshot is just `RestoreSnapshot(shareName, safetyID)` — no new "rollback" API. Operator-initiated, same orchestration runs (including a SECOND safety snapshot of the bad/partial state — they'll want to delete that one too). Cost is bounded (max ~2 safety snapshots in disaster scenarios); operator cleanup is the price for a clean primitive.

- **D-24-13:** Failure-mode taxonomy + invariants for REST-02 ("interrupted restore leaves share disabled with original data intact"):
  - **Pre-Reset failures** (share-enabled, snapshot-not-found, snapshot-not-ready, snapshot-not-durable + no --force, pre-verify-fail, safety-snap-fail, dump-missing): original metadata UNCHANGED; share already disabled; return error. "Original data intact" = literally untouched.
  - **Reset-fail / Restore-fail** (steps 5-6): original metadata WIPED or partially overwritten; share disabled; safety snapshot on disk + in DB. "Original data intact" = recoverable via `RestoreSnapshot(safetyID)`. Error wraps `ErrRestoreAborted` with the safety snap ID in the message so operator sees recovery path.
  - **Post-verify-fail** (step 7): restored metadata in place but blocks missing; share disabled; safety snapshot on disk. "Original data intact" = recoverable via `RestoreSnapshot(safetyID)`. Error is `ErrRestoreVerifyFailed`.
  - **Process crash mid-Reset** (step 5): startup observes share `Enabled=false` (operator disabled it before restore); metadata partially wiped; safety snapshot on disk + in DB (state=ready since CreateSnapshot completed before Reset began). Operator runs `RestoreSnapshot(shareName, safetyID)` to recover. No restore-tracking row needed because share-disabled IS the recovery marker (operator never auto-re-enables).
  - **Process crash mid-Restore** (step 6): same as mid-Reset — safety snapshot is the recovery primitive.

  Sync orchestration (D-24-02) means there's no in-flight-restore row to recover at startup; the on-disk safety snapshot + share-disabled state is the entire recovery contract.

### Verification gate and source-snapshot durability

- **D-24-06:** Restore from snapshot with `RemoteDurable=false` (created via `CreateSnapshotOpts.NoSyncGate=true` per Phase 23 D-23-11): DEFAULT REFUSES with `ErrSnapshotNotDurable`. Opt-in via `RestoreSnapshotOpts.AllowNonDurable bool` (Phase 25 CLI surfaces as `--force-non-durable` or similar). Rationale: default-refuse keeps the obvious-correct-path UX; pre-verify (D-24-07) is the real safety gate so `--force` is still safe-ish (fails fast if blocks actually missing); refusing outright over-restricts legit dev/test scenarios. `RemoteDurable=false` flag stays meaningful as the default-refuse trigger without becoming a hard wall.

- **D-24-07:** BOTH pre-verify and post-verify run:
  - **Pre-verify** (step 2, before any destructive op): read `manifest.hashes` from snapshot dir → call Phase 23 P23-01 `VerifyRemoteDurability(ctx, remote, manifest, snapshot.sync_gate_concurrency)`. On fail: return error before Reset/Restore are touched. Catches the 99% case (snapshot durable at create-time but remote object deleted out-of-band by operator S3 mistake / lifecycle policy) without destructive cost.
  - **Post-verify** (step 7, after Restore succeeds — REST-03 contract): walk the freshly-restored metadata store to union all `FileAttr.Blocks[*].Hash` into a `HashSet` → `VerifyRemoteDurability(ctx, remote, restoredHashes, snapshot.sync_gate_concurrency)`. On fail: `ErrRestoreVerifyFailed`. Rationale: REST-03 literal wording demands post-verify; also catches the rare window where pre-verify passed but a block was deleted during the Restore op itself.

  Pre-verify uses the snapshot manifest (fast, single file read); post-verify walks the live metadata store (slower, but mandatory for REST-03 wording and catches mid-restore deletions). Both reuse the same `VerifyRemoteDurability` helper + the same `snapshot.sync_gate_concurrency` knob from Phase 23 D-23-22.

- **D-24-14:** Post-verify hash enumeration uses metadata store walk over `FileAttr.Blocks`. The `MetadataStore` interface already exposes file enumeration (used by GC mark phase in `pkg/blockstore/engine/gc.go` and by Phase 21 `Backupable.Backup` hash extraction). Reuse the Phase 21 backup-time hash extraction helper if present (planner audit); otherwise add a minimal `HashSetFromMetadataStore(ctx, store) (*HashSet, error)` helper in `pkg/snapshot/` next to `manifest.go`. Per-backend cost is bounded by file count; acceptable since restore is rare.

### Error sentinels

- **D-24-08:** New typed error sentinels in `pkg/controlplane/models/errors.go` (next to Phase 22 `ErrSnapshotNotFound` and Phase 23 D-23-12 sentinels). Phase 25 maps to HTTP codes via `errors.Is`:
  - `ErrShareEnabled` — share must be disabled before Restore (400)
  - `ErrSnapshotNotDurable` — `RemoteDurable=false` and `AllowNonDurable=false` (412)
  - `ErrSnapshotMetadataDumpMissing` — `metadata.dump` file absent in snapshot dir (500)
  - `ErrMetadataStoreNotResetable` — backend doesn't implement `Resetable` (500; should never happen in prod — all 3 backends implement)
  - `ErrRestoreSafetySnapFailed` — pre-restore safety snapshot creation/wait failed (500; wraps inner CreateSnapshot/WaitForSnapshot error)
  - `ErrRestoreAborted` — Reset or Restore step failed; wraps inner error; message includes safety snapshot ID for operator-driven rollback (500)
  - `ErrRestoreVerifyFailed` — post-verify found missing hashes (500; wraps `VerifyRemoteDurability` error with first missing hash)

  Phase 22 `ErrSnapshotNotFound`, Phase 22 D-08 partial-index DB error, Phase 23 `ErrSnapshotVerifyFailed`, and Phase 23 `ErrSnapshotDrainTimeout` are inherited as-is via the safety-snapshot path.

### Code structure and plan breakdown

- **D-24-09:** Sequential orchestration steps inside `Runtime.RestoreSnapshot` (D-24-02 sync; D-24-13 failure-mode mapping):
  1. precheck (share enabled? snapshot exists? state=ready? durable-or-force?)
  2. pre-verify (manifest hashes on remote, fail-fast)
  3. safety snapshot (CreateSnapshot + WaitForSnapshot, sync-gated, visible, named `pre-restore-<RFC3339>`)
  4. open dump (`os.Open(MetadataDumpPath(...))`)
  5. Reset (`store.(Resetable).Reset(ctx)`)
  6. Restore (`store.(Backupable).Restore(ctx, dumpReader)`)
  7. post-verify (walk restored metadata → VerifyRemoteDurability, REST-03 gate)
  8. return nil; share stays disabled (operator runs `share enable`)

- **D-24-10:** PR shape — 4 plans / 2 waves / single PR against `develop`, mirroring Phase 22/23 cadence:
  - **Wave 1 (parallel, no inter-dependencies):**
    - **P24-01** — `pkg/metadata/resetable.go` (interface) + 3 backend impls (`memory/reset.go`, `badger/reset.go`, `postgres/reset.go`) + `pkg/metadata/storetest/reset_conformance.go` (`ResetThenRestoreConformance`) + wiring into existing 3 backend test entries
    - **P24-02** — 7 typed error sentinels in `pkg/controlplane/models/errors.go` (D-24-08) + `errors.Is` round-trip tests + `RestoreSnapshotOpts` struct definition in `pkg/controlplane/runtime/snapshot.go` (or sibling file) with `AllowNonDurable bool`
  - **Wave 2 (sequential, both touch `runtime/snapshot.go`):**
    - **P24-03** — `Runtime.RestoreSnapshot` end-to-end orchestration per D-24-09: precheck → pre-verify → safety-snap (CreateSnapshot+WaitForSnapshot reuse) → open dump → Reset → Restore → post-verify (with `HashSetFromMetadataStore` helper if not already present from Phase 21). Includes failure-mode wiring per D-24-13.
    - **P24-04** — E2E integration test `pkg/controlplane/runtime/snapshot_restore_test.go` covering: happy path, non-durable-refused, `--force` path, pre-verify-fails-fast, post-verify-fails (block deleted between Reset and post-verify), interrupted-restore (kill mid-Reset → assert share disabled + safety snap on disk + recoverable via second RestoreSnapshot call), enabled-share-refuses, snapshot-not-found / not-ready cases. Uses memory metadata + memory `RemoteStore` per Phase 22 D-21 / Phase 23 P23-06 patterns.

- **D-24-11:** Single PR against `develop`. Staged commits (one per plan, plus review-pass fixups). Branch name: `gsd/phase-24-restore-flow` (already created). Each commit independently buildable; reviewers walk commit-by-commit. No flag-gated half-states.

### Claude's Discretion

- D-24-03 Postgres `TRUNCATE` table list — planner audits Phase 20/21/22 migrations to enumerate every table; reuse a migration-introspection helper if one exists, else hard-code the list with a CI test that fails if a new table lands without a Reset update.
- D-24-04 safety-snapshot name format — `pre-restore-<RFC3339>` is the suggestion; planner picks final format (collision-avoidance vs readability). Phase 22 D-08 partial-unique-index on `(share, state=creating)` already protects against concurrent-name collisions if the format isn't unique enough on its own.
- D-24-09 step ordering — sequential per the list; planner may sub-split steps 5-6 into a helper that captures "reset-then-restore" semantics as a single named operation.
- D-24-14 `HashSetFromMetadataStore` placement — `pkg/snapshot/` if not already extracted from Phase 21 backup code; planner audits Phase 21 driver impls (`memory/backup.go`, `badger/backup.go`, `postgres/backup.go`) for an existing helper to reuse rather than duplicate.
- `RestoreSnapshotOpts.SkipPreVerify bool` — listed in scope as planner discretion; pre-verify is cheap on durable snapshots (manifest is sorted hex hashes, `Head()` per hash with bounded concurrency) so skipping is rarely justified. Planner may omit the field; can always add later in a non-breaking way.
- Concurrent-restore exclusion — out of scope per the "share disabled is the barrier" reasoning; planner may add a documented invariant comment but not a runtime lock.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements and roadmap
- `.planning/REQUIREMENTS.md` §REST (lines covering REST-01..03) — Phase 24's three requirements
- `.planning/ROADMAP.md` §"Phase 24: Restore Flow" — goal, success criteria, files-to-touch list

### Phase 23 foundation (direct dependency — orchestration glue + sync gate primitive)
- `.planning/phases/23-snapshot-create-orchestration-sync-gate/23-CONTEXT.md` — all 23 Phase 23 decisions. Critical: D-23-02 (revised HoldProvider filter — manifest-on-disk = held), D-23-09 (failed-snapshot retention; safety snap recovery relies on this), D-23-11 (NoSyncGate / RemoteDurable invariant — Phase 24 enforces refuse-by-default per D-24-06), D-23-13 (CreateSnapshot async — Phase 24 composes via WaitForSnapshot), D-23-19 (WaitForSnapshot helper — Phase 24 step 3 calls this), D-23-22 (snapshot.sync_gate_concurrency knob — Phase 24 reuses for both pre+post verify)
- `.planning/phases/23-snapshot-create-orchestration-sync-gate/23-VERIFICATION.md` — Phase 23 verification report
- `pkg/snapshot/syncgate.go` — `VerifyRemoteDurability(ctx, remote, manifest *HashSet, concurrency int) error` (P23-01); reused verbatim by Phase 24 for both pre-verify (step 2) and post-verify (step 7)
- `pkg/controlplane/runtime/snapshot.go` — `Runtime.CreateSnapshot`, `Runtime.WaitForSnapshot`, `CreateSnapshotOpts`, in-flight registry. Phase 24 extends THIS FILE with `RestoreSnapshot` + `RestoreSnapshotOpts` (per ROADMAP "extend" directive)
- `pkg/controlplane/models/errors.go` — Phase 22/23 sentinels (`ErrSnapshotNotFound`, `ErrSnapshotBackupFailed`, `ErrSnapshotVerifyFailed`, `ErrSnapshotDrainTimeout`, `ErrSnapshotRetryTarget*`). Phase 24 adds 7 more per D-24-08
- `pkg/controlplane/runtime/snapshot_hold.go` — `SnapshotHoldProvider` (revised filter from D-23-02). No change in Phase 24; safety-snap relies on this filter to pin its manifest blocks

### Phase 22 foundation (snapshot model + manifest I/O)
- `.planning/phases/22-snapshot-records-hash-manifest-gc-hold/22-CONTEXT.md` — Phase 22 decisions. Critical: D-04 (manifest = ground truth), D-15 (RemoveShare wipes snapshots tree — out of scope for Phase 24 but informs interaction model), D-19 (atomic manifest write via temp+rename), D-21 (memory-only integration test pattern — Phase 24 follows)
- `pkg/controlplane/models/snapshot.go` — `Snapshot` struct + state constants + path helpers (`SnapshotDir`, `ManifestPath`, `MetadataDumpPath`); `Snapshot.State`, `Snapshot.RemoteDurable`, `Snapshot.Name` read by Phase 24 precheck
- `pkg/controlplane/store/snapshots.go` — `SnapshotStore` CRUD (`GetSnapshot`, `ListSnapshots` used by precheck; `CreateSnapshot` used transitively via safety-snap path)
- `pkg/snapshot/manifest.go` — `WriteManifestAtomic` (used by safety-snap path transitively), `ReadManifest` (Phase 24 step 2 pre-verify reads `manifest.hashes` for the source snapshot)

### Phase 21 foundation (Backupable interface + per-backend Restore)
- `.planning/phases/21-per-engine-backup-drivers/21-CONTEXT.md` — Phase 21 driver decisions
- `pkg/metadata/backupable.go` — `Backupable` optional interface; `Restore(ctx context.Context, r io.Reader) error` is the exact call Phase 24 makes after `Reset`. Existing `ErrRestoreDestinationNotEmpty` sentinel semantics PRESERVED — Reset makes the store empty so the precondition is satisfied
- `pkg/metadata/store/memory/backup.go`, `pkg/metadata/store/badger/backup.go`, `pkg/metadata/store/postgres/backup.go` — three backend `Restore` impls; Phase 24 adds sibling `reset.go` files (D-24-03)
- `pkg/metadata/storetest/backup_conformance.go` — Phase 21 conformance scenarios; Phase 24 adds `reset_conformance.go` sibling (D-24-12)

### Phase 20 foundation (transitive types)
- `pkg/blockstore/hashset.go` — `HashSet` type used by manifest I/O + `VerifyRemoteDurability` input
- `pkg/blockstore/types.go` — `ContentHash` type

### Block store remote contract (Phase 24 verifies through here)
- `pkg/blockstore/remote/remote.go` — `RemoteStore` interface; `Head(ctx, hash)` is the probe; returns `blockstore.ErrBlockNotFound` for absent objects
- `pkg/blockstore/remote/memory/store.go` — in-memory `RemoteStore` for integration test

### Runtime + shares integration points
- `pkg/controlplane/runtime/runtime.go:172` — `GetMetadataStoreForShare(shareName)` — Phase 24 obtains the `metadata.MetadataStore` then type-asserts to both `metadata.Backupable` and `metadata.Resetable`
- `pkg/controlplane/runtime/runtime.go:296` — `Runtime.DisableShare` — Phase 24 does NOT call this; operator runs `dfsctl share disable` first (D-24-01). Phase 24 only READS state via `Runtime.IsShareEnabled` or equivalent
- `pkg/controlplane/runtime/runtime.go:302` — `Runtime.EnableShare` — Phase 24 does NOT call this; operator runs `dfsctl share enable` after success (D-24-01)
- `pkg/controlplane/runtime/shares/service.go:853` — `Service.DisableShare` + `ErrShareAlreadyDisabled` — the existing pattern Phase 24's `ErrShareEnabled` mirrors
- `pkg/controlplane/runtime/shares/service.go:928` — `Service.IsShareEnabled(name)` — Phase 24 precheck reads this
- `pkg/controlplane/runtime/runtime.go:350` (approx) — `sharesSvc.GetBlockStoreForShare(shareName)` returns the per-share `*engine.BlockStore` whose `Remote()` accessor returns the `RemoteStore` Phase 24 verifies against. Audit exact accessor name in Phase 23-shipped tree

### Config plumbing
- `.config/dfs/config.yaml` schema + Go bindings — Phase 23 added `snapshot.sync_gate_concurrency` (default 16); Phase 24 reuses for both pre+post verify (no new knob this phase)

### CLAUDE.md and standing instructions
- `CLAUDE.md` §"Architecture invariants" — invariant 5 (WRITE ordering — not directly applicable but informs the "no flag-gated half-state" stance); invariant 6 (error code conventions); invariant 7 (metadata store contract — Resetable is an optional extension following Backupable's precedent)
- `CLAUDE.md` §"Commits & PRs" — no Claude/AI mentions; concise messages; sign commits

### New files (Phase 24 creates)
- `pkg/metadata/resetable.go` — `Resetable` interface (D-24-03)
- `pkg/metadata/store/memory/reset.go` — memory impl
- `pkg/metadata/store/badger/reset.go` — badger impl (`db.DropAll`)
- `pkg/metadata/store/postgres/reset.go` — postgres impl (`TRUNCATE ... CASCADE` in REPEATABLE READ)
- `pkg/metadata/storetest/reset_conformance.go` — `ResetThenRestoreConformance` (D-24-12)
- `pkg/controlplane/runtime/snapshot_restore_test.go` — E2E integration test (P24-04)
- `pkg/snapshot/hashset_from_metadata.go` (planner discretion, only if no Phase 21 helper to reuse) — `HashSetFromMetadataStore(ctx, store) (*HashSet, error)` helper for post-verify hash enumeration (D-24-14)

### Files Phase 24 modifies
- `pkg/controlplane/runtime/snapshot.go` — extend with `Runtime.RestoreSnapshot` + `RestoreSnapshotOpts` (per ROADMAP)
- `pkg/controlplane/models/errors.go` — 7 new sentinels per D-24-08
- 3 backend test files (`memory_backup_test.go`, `badger_backup_test.go`, `postgres_backup_test.go` or equivalent) — wire `ResetThenRestoreConformance(t, factory)` into existing test entries

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/snapshot/syncgate.go::VerifyRemoteDurability(ctx, remote, manifest *HashSet, concurrency int) error` (Phase 23 P23-01) — reused VERBATIM for both pre-verify (step 2, input = source snapshot's manifest) and post-verify (step 7, input = freshly-walked restored metadata hashes). No engine surface changes needed.
- `pkg/snapshot/manifest.go::ReadManifest(path) (*HashSet, error)` (Phase 22 D-19) — Phase 24 step 2 reads source snapshot's `manifest.hashes` via this helper.
- `pkg/controlplane/runtime/snapshot.go::Runtime.CreateSnapshot + Runtime.WaitForSnapshot` (Phase 23 D-23-13, D-23-19) — composed by Phase 24 step 3 for the sync-gated safety snapshot.
- `pkg/controlplane/runtime/snapshot.go::CreateSnapshotOpts` (Phase 23 D-23-15) — Phase 24 calls with `{Name: "pre-restore-<RFC3339>"}` and DEFAULT `NoSyncGate: false` per D-24-04.
- `pkg/controlplane/models/snapshot.go::MetadataDumpPath(shareDataDir, snapID), ManifestPath(...)` (Phase 22) — Phase 24 step 4 opens dump via `MetadataDumpPath`; step 2 reads manifest via `ManifestPath`.
- `pkg/controlplane/store/snapshots.go::GetSnapshot, ListSnapshots` (Phase 22) — used by Phase 24 precheck (step 1).
- `pkg/controlplane/runtime/runtime.go::GetMetadataStoreForShare(shareName)` — Phase 24 obtains the store; type-asserts to `metadata.Backupable` AND new `metadata.Resetable`.
- `pkg/controlplane/runtime/shares/service.go::IsShareEnabled(name) (bool, error)` — Phase 24 precheck reads this; returns `ErrShareEnabled` if true.
- Phase 21 backup-time hash-extraction logic in `pkg/metadata/store/{memory,badger,postgres}/backup.go` — planner audits to determine whether a shared helper already exists for `HashSetFromMetadataStore` (D-24-14); if so, reuse; if not, add `pkg/snapshot/hashset_from_metadata.go`.

### Established Patterns
- **Optional-interface pattern** (Phase 21 `Backupable`, CLAUDE.md "established patterns"): Phase 24's `Resetable` follows the same pattern — call site does `store.(Resetable)` and returns `ErrMetadataStoreNotResetable` on miss. Conformance tests assert each shipped backend implements both Backupable and Resetable.
- **Idempotency-via-sentinel pattern** (`ErrShareAlreadyDisabled`): Phase 24's `ErrShareEnabled` mirrors — callers can `errors.Is(err, ErrShareEnabled)` to special-case "already in target state" semantics. (Asymmetric to `ErrShareAlreadyDisabled` because Phase 24 actively wants to refuse on enabled rather than allow idempotently.)
- **memory-only integration test pattern** (Phase 22 D-21, Phase 23 P23-06): Phase 24 P24-04 follows — memory metadata + memory RemoteStore + memory LocalStore; backend matrix coverage delegated to per-backend conformance suites (Phase 21 backup + Phase 24 reset).
- **Sync-gated CreateSnapshot composition pattern** (Phase 23 D-23-19): `CreateSnapshot → WaitForSnapshot → check final state`. Phase 24 step 3 follows verbatim for the safety snapshot.
- **Per-snapshot RWMutex on HoldProvider** (Phase 23 D-23-04): `SnapshotHoldProvider.HeldHashes` takes RLock per snapshot during manifest streaming. Phase 24 doesn't add new locks — it relies on share-disabled + sync orchestration as the exclusion model.

### Integration Points
- **`Runtime.RestoreSnapshot` lives in `pkg/controlplane/runtime/snapshot.go`** (per ROADMAP "extend") next to `CreateSnapshot`/`WaitForSnapshot`. Same file = same package access to `r.store`, `r.sharesSvc`, `r.GetMetadataStoreForShare`, `r.LocalStoreDir`, in-flight registry (read-only from Phase 24 — no new entries added since Restore is sync).
- **Safety-snapshot composition** calls `Runtime.CreateSnapshot` from within `Runtime.RestoreSnapshot`. Phase 23's in-flight registry handles cancellation correctly on parent-ctx cancellation. The safety-snap's goroutine still runs to completion after the parent Restore returns (sync orchestration but the underlying CreateSnapshot is async); `WaitForSnapshot` blocks Restore on completion. Verify in P24-04 test that ctx-cancel mid-safety-snap propagates cleanly (Phase 23 D-23-17 guarantees this).
- **Post-verify hash extraction** reads from the freshly-restored metadata store via the standard `MetadataStore` interface (enumerates files → unions `FileAttr.Blocks[*].Hash`). No new metadata-store method; uses what GC mark phase + Phase 21 backup hash-extraction already use.
- **No new HoldProvider interaction**: the source snapshot's manifest is pinned by Phase 23 D-23-02 revised filter (manifest-on-disk = held, all states). The safety snapshot's manifest is pinned by the same filter. Restore success leaves both snapshots intact; operator-driven cleanup eventually deletes them, at which point HoldProvider unhooks and GC can reclaim.

</code_context>

<specifics>
## Specific Ideas

- **User emphasis on "atomic between metadata and block stores"** (during D-24-03 discussion): interpreted as "restored metadata's BlockRefs must all resolve to live blocks" — satisfied by D-24-07 post-verify gate. Block stores themselves are not "restored" (CAS-immutable + HoldProvider-pinned via manifest); the atomicity is the reachability invariant at the moment the share is re-enabled. Phase 24 enforces this via post-verify; operator only re-enables after success.
- **Safety snapshot visibility decision** (D-24-04): operator deliberately accepts the snapshot-list clutter in exchange for fully-durable rollback story (sync gate enabled). No hidden `Kind` field, no auto-cleanup. Operator manually deletes after restore is confirmed good.
- **Sync over async** (D-24-02): user reasoning is that restore is rare, operator-blocking, and disaster-driven. Phase 23's async machinery was justified by `CreateSnapshot` being routine + GUI-triggered; restore doesn't share those properties.

</specifics>

<deferred>
## Deferred Ideas

- **`dfsctl share snapshot restore` CLI command** — Phase 25
- **`POST /api/v1/shares/{name}/snapshots/{id}/restore` REST endpoint** — Phase 25 (returns 200 on sync orchestration success; HTTP timeout coordination is Phase 25's problem)
- **`pkg/apiclient/snapshots.go` Restore method** — Phase 25
- **Auto-disable / auto-enable around Restore** — explicitly rejected per D-24-01; could revisit in v0.17 if operator UX feedback demands it
- **Async Restore orchestration with restore-tracking row** — explicitly rejected per D-24-02; could revisit if HTTP-timeout pain emerges in Phase 25
- **Automatic safety-snapshot cleanup** — operator-driven per D-24-04; could add a `RestoreSnapshotOpts.AutoCleanupSafetySnap bool` later if operators demand it (non-breaking addition)
- **Cross-share restore** (snapshot of share A → share B) — REST-01 scopes to same-share reference restore; cross-share would need namespace/identity translation and is a separate feature
- **Snapshot encryption / per-snapshot encryption keys** — orthogonal feature; not in scope for v0.16.0
- **Restore-progress metrics / observability surface** — deferred per Phase 23 D-23-16 stance; structured `slog` only this phase
- **Concurrent-restore exclusion lock** — share-disabled is the barrier per D-24-13; if real-world reports of double-restore corruption emerge, add a per-share `restoreInFlight` atomic boolean

</deferred>

---

*Phase: 24-restore-flow*
*Context gathered: 2026-05-28*

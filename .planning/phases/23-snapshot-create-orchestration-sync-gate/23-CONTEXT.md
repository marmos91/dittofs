# Phase 23: Snapshot Create Orchestration + Sync Gate - Context

**Gathered:** 2026-05-28
**Status:** Ready for planning
**GH issue:** [#643](https://github.com/marmos91/dittofs/issues/643)
**Milestone:** v0.16.0 Share Snapshots — Phase 4 of 6
**Depends on:** Phase 21 (per-engine `Backupable` drivers), Phase 22 (snapshot records + manifest + `HoldProvider`)

<domain>
## Phase Boundary

Wire end-to-end snapshot creation by composing Phase 21's `Backupable.Backup` with Phase 22's hash manifest writer and the new sync gate, then flip the snapshot row to `ready`. Two new files: `pkg/snapshot/syncgate.go` (pure `VerifyRemoteDurability` probe) and `pkg/controlplane/runtime/snapshot.go` (`Runtime.CreateSnapshot` async orchestrator). Also revise the Phase 22 `SnapshotHoldProvider` filter, add a per-snapshot RWMutex for delete/HeldHashes races (Phase 22 deferred), expose a `WaitForSnapshot` helper, and wire startup recovery for orphaned `creating` rows.

**In scope:**
- `pkg/snapshot/syncgate.go` — `VerifyRemoteDurability(ctx, remote, manifest *HashSet, concurrency int) error` (bounded-parallel `Head()` probes, fail-fast)
- `pkg/controlplane/runtime/snapshot.go` — `Runtime.CreateSnapshot(ctx, share, CreateSnapshotOpts) (snapID string, err error)` (async; returns after `creating` row insert; background goroutine runs backup → manifest → drain → verify → ready/failed)
- `Runtime.WaitForSnapshot(ctx, snapID) (*models.Snapshot, error)` helper for poll-free completion observation
- `CreateSnapshotOpts` struct: `NoSyncGate bool`, `RetryOf string` (failed-snapshot ID to retry against — reuses dir, overwrites manifest atomically)
- Async goroutine registry on Runtime keyed by share name (`map[string]*snapInFlight` with `WaitGroup` + `[]CancelFunc`)
- `RemoveShare` integration: cancel + `WaitGroup.Wait` for in-flight snap goroutines BEFORE wiping `<share>/snapshots/` tree (Phase 22 D-15 cleanup hook)
- `Runtime.Shutdown` integration: cancel all entries + `WaitGroup.Wait`
- Startup recovery in Runtime init: scan `state=creating` rows, flip to `failed` with reason `abandoned at startup` (manifest+dump retained if present)
- Revise Phase 22 D-05 hold filter: include any snapshot whose `manifest.hashes` exists on disk regardless of state (covers `creating`, `ready`, `failed` — protects manifest-defined blocks across the create + retry window). Implementation (DB join vs FS walk) is planner discretion.
- Per-snapshot RWMutex in `SnapshotHoldProvider`: `HeldHashes` takes RLock while streaming manifest; `Delete` takes Lock before row + dir removal (Phase 22 deferred concern resolved)
- Typed error sentinels in `pkg/controlplane/models/`: `ErrSnapshotBackupFailed`, `ErrSnapshotVerifyFailed`, `ErrSnapshotDrainTimeout`, `ErrSnapshotRetryTargetNotFound`, `ErrSnapshotRetryTargetNotFailed` (the existing D-08 partial-index error continues to surface concurrent-create)
- YAML config knob: `snapshot.sync_gate_concurrency` (default 16) wired through to `VerifyRemoteDurability`
- Structured logs at each orchestration step (`slog` debug/info: backup start/end+bytes, manifest written+hash count, drain start/end+blocks, verify pass/fail, state transition)
- Integration test: end-to-end `CreateSnapshot` against memory metadata + memory `RemoteStore` exercising sync-gate drain path, retry-of-failed path, `--no-sync-gate` path, and RemoveShare-cancels-in-flight path

**Out of scope:**
- Restore flow (Phase 24)
- REST handler, `dfsctl` CLI commands, API client (Phase 25)
- Metrics surface (deferred — no `prometheus` layer in runtime/blockstore yet; promote in Phase 25 if needed)
- Postgres/Badger coverage in the orchestration integration test (Phase 21 conformance suite covers driver round-trip; Phase 23 test focuses on orchestration semantics, not driver matrix)
- Async GC + 202+poll REST pattern for `RunBlockGC` (Phase 11 IN-4-03 deferred — independent of snapshot async)

</domain>

<decisions>
## Implementation Decisions

### Snapshot row lifecycle and GC hold during create

- **D-23-01:** Insert snapshot row with `state=creating` BEFORE any I/O (backup, manifest, drain, verify). Reasons: (a) backup writes into `<share>/snapshots/<id>/` which needs ID known up front; (b) Phase 22 D-08 partial unique index `idx_share_creating` only enforces "one creating per share" if the row exists during create; (c) Phase 22 D-06 retry semantics (`failed → creating`, same ID, same dir) require the row to survive failure; (d) failures are observable to the caller via `GetSnapshot`/`WaitForSnapshot`, no orphan dirs.

- **D-23-02 (REVISES Phase 22 D-05):** `HoldProvider` filter = "any snapshot whose `manifest.hashes` exists on disk, regardless of `state`." Phase 22's `state='ready'`-only filter is replaced. The on-disk manifest is the ground truth (Phase 22 D-04) — its existence is the only fact GC needs. This covers three windows the prior filter missed:
  1. `creating` snapshot post-manifest-write but pre-ready-flip
  2. `failed` snapshot retained for retry per D-23-09 (would otherwise lose its blocks before retry)
  3. `failed → creating` retry per Phase 22 D-06 (same ID, same dir, atomic overwrite)

  Implementation strategy (DB-driven `WHERE manifest.hashes EXISTS` via stat vs FS-walk `<shareDataDir>/snapshots/*/manifest.hashes`) is planner discretion. Both are correct; FS-walk avoids one DB round-trip but requires per-share-data-dir resolution.

- **D-23-03:** State flip to `ready` happens AFTER `VerifyRemoteDurability` passes, in the same `UPDATE` that sets `RemoteDurable=true`. Failed verify (after drain retry) flips to `state=failed` with `RemoteDurable=false`. Clean DB invariant: `state=ready AND RemoteDurable=true` always means durable; `state=ready AND RemoteDurable=false` only occurs under `--no-sync-gate` (D-23-11); `state=failed` means orchestration didn't complete.

- **D-23-04:** Per-snapshot `sync.RWMutex` inside `SnapshotHoldProvider` resolves the Phase 22 deferred delete-vs-HeldHashes race. `HeldHashes` takes RLock for the duration of streaming one snapshot's manifest; `Delete` takes Lock before removing row + dir. Lock granularity (provider-level single RWMutex vs `sync.Map[snapID]*RWMutex` for per-ID) is planner discretion — provider-level is simpler and acceptable for typical snapshot counts (≤ low hundreds per share), per-ID avoids head-of-line blocking if a `HeldHashes` stream is slow.

### Sync gate behavior

- **D-23-05:** Verify-fail recovery uses `engine.Syncer.DrainAllUploads(ctx)` (already exists in `pkg/blockstore/engine/syncer.go:388`) followed by one re-verify. If re-verify still finds missing hashes, orchestration fails with `ErrSnapshotVerifyFailed`. No retry-loop on the drain — one shot is sufficient because `DrainAllUploads` is synchronous and uploads every local block before returning.

- **D-23-06:** `VerifyRemoteDurability` is fail-fast: first `Head() != ErrBlockNotFound` aborts via context cancellation of sibling goroutines; returns wrapped error naming the missing hash. Matches existing INV-04 fail-closed pattern in blockstore engine. Collect-all-missing is unnecessary because the drain+re-verify path handles the common "syncer is behind" case before reaching this error.

- **D-23-07:** Bounded concurrency exposed as YAML config knob `snapshot.sync_gate_concurrency` (default 16). Wired through `Runtime` → `VerifyRemoteDurability(..., concurrency int)`. Tunable for slow remotes or restrictive endpoints; default matches existing `Syncer` upload parallelism order-of-magnitude.

- **D-23-08:** `VerifyRemoteDurability` honors caller `ctx.Done()` only — no internal default timeout. `Runtime.CreateSnapshot` derives the orchestration `ctx`; caller (Phase 25 REST handler, CLI, test) sets the deadline. Cleanest Go idiom.

### Failure and retry semantics

- **D-23-09:** Orchestration failure (`Backup` error, manifest write error, drain timeout, post-drain verify fail) flips row to `state=failed` and RETAINS `metadata.dump` + `manifest.hashes` on disk for retry. Combined with D-23-02 (manifest-on-disk = held), failed snapshots continue to hold their blocks until either retried or hard-deleted via `DeleteSnapshot`. No automatic cleanup — operator-driven.

- **D-23-10:** Retry is caller-driven via opt-in `CreateSnapshotOpts.RetryOf = "<failed-id>"`. Semantics: look up the failed row, validate `state=failed`, flip back to `state=creating`, reuse `<id>/` directory, re-run backup + manifest write (atomically overwriting `manifest.hashes` via Phase 22 D-19 temp+rename), then drain + verify + ready/failed as usual. New retry against the same ID is allowed only if current state is `failed` (errors with `ErrSnapshotRetryTargetNotFailed` otherwise; `ErrSnapshotRetryTargetNotFound` if no such ID). New `CreateSnapshot` without `RetryOf` always generates a fresh UUID.

- **D-23-11:** `CreateSnapshotOpts.NoSyncGate = true` skips `DrainAllUploads` + `VerifyRemoteDurability` entirely. Final state: `state=ready, RemoteDurable=false`. Hold filter (D-23-02) still applies — local blocks remain GC-safe. Phase 24 restore reads `RemoteDurable=false` and refuses (or warns + requires `--force`) so users can't restore from an unverified snapshot. Phase 25 CLI/REST surfaces the flag as `--no-sync-gate` and the boolean as a column in `list`.

- **D-23-12:** Typed error sentinels in `pkg/controlplane/models/` for Phase 25 to map to HTTP status codes via `errors.Is`:
  - `ErrSnapshotBackupFailed` — wraps `Backupable.Backup` error
  - `ErrSnapshotVerifyFailed` — sync gate found missing hashes after drain
  - `ErrSnapshotDrainTimeout` — `DrainAllUploads` returned timeout/context error
  - `ErrSnapshotRetryTargetNotFound` — `RetryOf` references a non-existent snapshot
  - `ErrSnapshotRetryTargetNotFailed` — `RetryOf` references a snapshot whose state is not `failed`

  Phase 22's `ErrSnapshotNotFound` and the D-08 partial-index DB error (surfacing concurrent-create) are inherited as-is.

### API shape and async orchestration

- **D-23-13:** `Runtime.CreateSnapshot` is ASYNC. Signature: `CreateSnapshot(ctx context.Context, shareName string, opts CreateSnapshotOpts) (snapID string, err error)`. Returns `(uuid, nil)` immediately after the `state=creating` row insert + on-disk dir creation succeed. The backup → manifest → drain → verify → ready/failed pipeline runs in a goroutine. Synchronous errors at call time are only: share-not-found, concurrent-create-violation (D-08), retry-target validation failures, on-disk dir creation failure. Phase 25 REST returns `202 Accepted` with `Location: /shares/{name}/snapshots/{id}` and `Get`-based polling.

- **D-23-14:** Orchestration entrypoint lives at `pkg/controlplane/runtime/snapshot.go` as a method on `*Runtime` (file already named in ROADMAP). No new `snapshots.Service` sub-service — keeps things flat per "less is more." `pkg/snapshot/` is the natural home for pure helpers (D-23-21); orchestration glue stays on Runtime to access stores/identity/shares directly.

- **D-23-15:** `CreateSnapshotOpts` is a plain struct with exported fields (`NoSyncGate bool`, `RetryOf string`). Matches existing Runtime APIs (`ShareConfig`, `RemoteCfg`). Functional-options pattern not used elsewhere in DittoFS — avoid the precedent.

- **D-23-16:** Observability = structured `slog` logs only this phase. At each orchestration step (per Plan 2-derived breakdown above), log `slog.Debug` for entry + `slog.Info` for completion with key fields (`snapshot_id`, `share`, `bytes_dumped`, `manifest_count`, `drain_blocks`, `verify_concurrency`, `final_state`). No metrics layer (`prometheus` / OTel) introduced — defer to Phase 25 or a future v0.17 observability pass.

- **D-23-17:** Async goroutine lifecycle uses a CENTRALIZED REGISTRY on `Runtime`. Structure: `map[shareName]*snapInFlight` where `snapInFlight = { wg *sync.WaitGroup; cancels []context.CancelFunc; mu sync.Mutex }`. On `CreateSnapshot`: derive child `ctx` from a long-lived `runtimeCtx`, append `CancelFunc` to the share's entry, `wg.Add(1)`, launch goroutine that defers `wg.Done()`. On `RemoveShare`: under share-lifecycle lock, fetch the entry, cancel all + `wg.Wait`, then proceed with Phase 22 D-15 tree wipe + DB cascade. On `Runtime.Shutdown`: walk the registry, cancel all + `wg.Wait` per share. This is logically option C (ctx tied to share lifecycle) but the bookkeeping is centralized on Runtime — avoids race window where a snap goroutine outlives `RemoveShare` and writes into a doomed tree.

- **D-23-18:** Startup recovery for orphaned `state=creating` rows runs in `Runtime` init AFTER metadata-store registration but BEFORE adapters start serving. Scan: `SELECT * FROM snapshots WHERE state='creating'`. For each: flip to `state=failed` with a recovery-reason marker (either an existing `Error` column if present, or a structured `slog.Warn` if not — planner picks). Manifest + dump (if present from a crash mid-write) are retained per D-23-09 → D-23-02 still protects their blocks → operator can retry via D-23-10 or `DeleteSnapshot`.

- **D-23-19:** `Runtime.WaitForSnapshot(ctx, snapID) (*models.Snapshot, error)` helper for caller observation. Implementation: subscribe to a per-snapshot completion signal (per-ID `chan struct{}` closed when goroutine exits, stored alongside `snapInFlight` or in a parallel map), then `GetSnapshot` for final state. Falls back to polling `GetSnapshot` if the snap completed before `Wait` was called (chan already closed / entry already removed). Allows CLI/REST to block efficiently in Phase 25 without busy-polling.

### Code structure and plan breakdown

- **D-23-20:** Six plans / three waves, mirroring the Phase 22 layout for review-velocity continuity:
  - **Wave 1 (parallel, no inter-dependencies):**
    - **P23-01** — `pkg/snapshot/syncgate.go`: `VerifyRemoteDurability` pure func with bounded-parallel `Head()` probes, fail-fast, ctx-only timeout. Unit tests in `syncgate_test.go` using memory `RemoteStore`.
    - **P23-02** — Typed error sentinels in `pkg/controlplane/models/errors.go` (or wherever `ErrSnapshotNotFound` lives from Phase 22). Five new vars per D-23-12. `errors.Is` round-trip tests.
    - **P23-03** — `SnapshotHoldProvider` filter revision per D-23-02 (manifest-on-disk = held, all states) + per-snapshot RWMutex per D-23-04. Update `pkg/controlplane/runtime/snapshot_hold.go` + the existing Phase 22 tests. Add a regression test for delete-vs-HeldHashes race using `go test -race`.
  - **Wave 2 (sequential, share runtime/snapshot.go):**
    - **P23-04** — `pkg/controlplane/runtime/snapshot.go`: `CreateSnapshot` orchestration + `CreateSnapshotOpts` struct + goroutine + in-flight registry + child-ctx derivation. Includes per-area D-23-13..D-23-17 wiring.
    - **P23-05** — `RemoveShare` integration (cancel + `wg.Wait` before tree wipe) + `Runtime.Shutdown` integration (cancel all + `wg.Wait`) + startup recovery scan per D-23-18.
  - **Wave 3:**
    - **P23-06** — `WaitForSnapshot` helper per D-23-19 + integration test (memory metadata + memory `RemoteStore`) covering: happy path, drain-then-verify-passes, drain-then-verify-fails, retry-of-failed, `--no-sync-gate`, RemoveShare-cancels-in-flight, startup-recovery-after-simulated-crash.

  YAML config knob `snapshot.sync_gate_concurrency` lands in P23-01 (where it's consumed) or P23-04 (where it's wired) — planner picks.

- **D-23-21:** Pure helpers extracted into `pkg/snapshot/` rather than living private to `runtime/snapshot.go`. Candidates: dump-writer wrapper (file create + buffered writer + sync + close + atomic rename, mirroring D-19 manifest-write pattern), retry-eligibility validator (fetch + check `state=failed` invariant). Tested without Runtime fixtures. Orchestration glue (`CreateSnapshot` body, goroutine, registry) stays on Runtime — it needs `r.store`, `r.sharesSvc`, `r.GetMetadataStoreForShare`, etc.

- **D-23-22:** YAML config knob `snapshot.sync_gate_concurrency` (default 16) added to the config schema this phase. Operators may want to tune for slow/restrictive remotes during snapshot verification before Phase 25 ships the user-facing surface.

- **D-23-23:** Single PR against `develop`, staged commits (one per plan, plus review-pass fixups). Matches Phase 22 cadence (`fc03673e`/`cc6e75da`/`7af98a0d`/`bd548740`/`f42d61da`). Each commit independently buildable; reviewers walk commit-by-commit. Branch name: `gsd/phase-23-snapshot-create-orchestration-sync-gate`.

### Claude's Discretion

- D-23-02 implementation (DB-driven `WHERE manifest exists via stat` vs FS-walk of `<shareDataDir>/snapshots/*/manifest.hashes`) — both correct; planner picks based on plumbing cost.
- D-23-04 lock granularity (provider-level single `RWMutex` vs per-snapshot `sync.Map[snapID]*RWMutex`) — provider-level simpler; per-ID avoids head-of-line blocking.
- D-23-18 reason-marker storage (existing column vs structured log) — depends on whether Phase 22 included an `Error string` column on `models.Snapshot`.
- D-23-20 placement of YAML config knob registration (P23-01 vs P23-04) — wherever the wiring is cleanest.
- D-23-21 exact pure-helper boundary in `pkg/snapshot/` (dump-writer signature, retry-validator signature) — planner picks.
- Sentinel naming variants (`ErrSnapshot*Failed` vs `ErrSnapshot*` family) — match whichever style Phase 22 settled on for `ErrSnapshotNotFound`.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements and roadmap
- `.planning/REQUIREMENTS.md` §ORCH (lines covering ORCH-01..03) — Phase 23's three requirements
- `.planning/ROADMAP.md` §Phase 23 — goal, success criteria, files-to-touch list

### Phase 22 foundation (direct dependency)
- `.planning/phases/22-snapshot-records-hash-manifest-gc-hold/22-CONTEXT.md` — all 21 Phase 22 decisions. Critical: D-04 (manifest = ground truth), D-05 (PHASE 22 ORIGINAL — now REVISED by D-23-02), D-06 (retry idempotency), D-08 (concurrent-create guard), D-15 (RemoveShare wipes snapshots tree), D-19 (atomic manifest write), D-21 (memory-only integration test pattern)
- `.planning/phases/22-snapshot-records-hash-manifest-gc-hold/22-VERIFICATION.md` — Phase 22 verification (5/5) — confirms the Phase 22 surfaces Phase 23 builds on
- `pkg/controlplane/models/snapshot.go` — `Snapshot` struct + state constants + path helpers (`SnapshotDir`, `ManifestPath`, `MetadataDumpPath`)
- `pkg/controlplane/store/snapshots.go` — `SnapshotStore` CRUD (`CreateSnapshot`, `GetSnapshot`, `ListSnapshots`, `DeleteSnapshot`, `UpdateSnapshotState`)
- `pkg/controlplane/store/interface.go` — `Store` composition (where `SnapshotStore` is embedded; Phase 23 may need an `UpdateSnapshotDurable(id string, durable bool) error` addition or use the existing `UpdateSnapshotState` pattern)
- `pkg/snapshot/manifest.go` — `WriteManifest` / `WriteManifestAtomic` / `ReadManifest` (Phase 23 calls `WriteManifestAtomic` during orchestration and `ReadManifest` inside `HoldProvider`/`VerifyRemoteDurability` caller)
- `pkg/controlplane/runtime/snapshot_hold.go` — `SnapshotHoldProvider` (Phase 23 revises filter per D-23-02, adds RWMutex per D-23-04)
- `pkg/controlplane/runtime/blockgc.go` — `RunBlockGC` injection point (no change expected; just verify the revised filter behaves)
- `pkg/controlplane/runtime/snapshot_lifecycle_test.go` — Phase 22 integration test (Phase 23 follows the same fixture pattern; may extend or sibling)

### Phase 21 foundation (direct dependency)
- `.planning/phases/21-per-engine-backup-drivers/21-CONTEXT.md` — Phase 21 driver decisions
- `pkg/metadata/backupable.go` — `Backupable` interface; `Backup(ctx, w io.Writer) (*HashSet, error)` is the exact call CreateSnapshot makes via the metadata-store-for-share lookup
- `pkg/metadata/store/memory/backup.go` — memory engine `Backup` reference for the integration test
- `pkg/metadata/store/badger/backup.go`, `pkg/metadata/store/postgres/backup.go` — production drivers (NOT under test in Phase 23; Phase 21 conformance covers them)

### Phase 20 foundation (transitive)
- `pkg/blockstore/hashset.go` — `HashSet` consumed by manifest I/O + `VerifyRemoteDurability` input
- `pkg/blockstore/types.go` — `ContentHash` type

### Block store remote contract (Phase 23 directly consumes)
- `pkg/blockstore/remote/remote.go` — `RemoteStore` interface; `Head(ctx, hash) (Meta, error)` is the probe `VerifyRemoteDurability` uses; returns `blockstore.ErrBlockNotFound` for absent objects
- `pkg/blockstore/remote/memory/store.go` — in-memory `RemoteStore` for integration test (matches Phase 22 D-21 pattern)

### Engine surfaces (Phase 23 calls)
- `pkg/blockstore/engine/syncer.go:388` — `Syncer.DrainAllUploads(ctx) error` — synchronous drain used by sync-gate recovery path (D-23-05). Returns when every locally-pending block has been Put to remote (or error).
- `pkg/blockstore/engine/gc.go` — `HoldProvider` interface (Phase 22 D-01..D-02) — no signature change in Phase 23; only the implementation's filter is revised per D-23-02

### Runtime integration points (Phase 23 modifies)
- `pkg/controlplane/runtime/runtime.go` — `Runtime` struct gains in-flight registry per D-23-17; init gains startup recovery per D-23-18; `Shutdown` gains cancel + WG.Wait per D-23-17
- `pkg/controlplane/runtime/runtime.go:172` — `GetMetadataStoreForShare` — orchestration calls this to obtain the `metadata.MetadataStore` to type-assert to `metadata.Backupable`
- `pkg/controlplane/runtime/runtime.go:350` — `sharesSvc.GetBlockStoreForShare(shareName)` returns the per-share `*engine.BlockStore` whose `Syncer` exposes `DrainAllUploads`
- `pkg/controlplane/runtime/runtime.go:194` — `LocalStoreDir(shareName)` — base dir for `<shareDataDir>/snapshots/<id>/`
- `pkg/controlplane/runtime/shares/service.go` — `RemoveShare` lifecycle path; Phase 23 inserts the cancel + WG.Wait per D-23-17 BEFORE the Phase 22 D-15 wipe hook
- `pkg/controlplane/models/errors.go` (or wherever Phase 22 placed `ErrSnapshotNotFound`) — Phase 23 adds five sentinels per D-23-12

### Config plumbing
- `.config/dfs/config.yaml` schema + Go bindings (search for existing `snapshot.*` or `gc.*` knob to find the schema file) — Phase 23 adds `snapshot.sync_gate_concurrency` (default 16) per D-23-22

### CLAUDE.md and standing instructions
- `CLAUDE.md` §"Architecture invariants" — invariants 5 (WRITE ordering), 6 (error code conventions), 7 (metadata store contract)
- `CLAUDE.md` §"Reference implementations" — not directly relevant for snapshots but kept as standard ref

### New files (Phase 23 creates)
- `pkg/snapshot/syncgate.go` (new) — `VerifyRemoteDurability` + tests
- `pkg/controlplane/runtime/snapshot.go` (new) — `Runtime.CreateSnapshot`, `Runtime.WaitForSnapshot`, in-flight registry, startup recovery, RemoveShare integration entry points

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/snapshot/manifest.go::WriteManifestAtomic(path, *HashSet) error` — directly used by orchestration after `Backupable.Backup` returns the `*HashSet`. Atomic temp+fsync+rename (Phase 22 D-19) means no observer ever sees a half-written manifest.
- `pkg/blockstore/engine/syncer.go::Syncer.DrainAllUploads(ctx) error` — exact API the sync-gate recovery path needs. No new engine surface required.
- `pkg/blockstore/remote/remote.go::RemoteStore.Head(ctx, hash)` — probe primitive for `VerifyRemoteDurability`. Returns `blockstore.ErrBlockNotFound` on absent; backends already conform.
- `pkg/controlplane/runtime/runtime.go::GetMetadataStoreForShare(shareName)` — returns the per-share `metadata.MetadataStore`; type-assert to `metadata.Backupable` (interface is optional per CLAUDE.md "established patterns" — nil-safe at call site, error if not implemented).
- `pkg/controlplane/runtime/runtime.go::LocalStoreDir(shareName)` — base dir for snapshot dump + manifest paths via Phase 22 model helpers.
- `pkg/controlplane/store/snapshots.go::CreateSnapshot / UpdateSnapshotState / GetSnapshot / DeleteSnapshot` — DB-side CRUD used throughout orchestration.
- `pkg/controlplane/runtime/snapshot_lifecycle_test.go` — fixture pattern (`lifecycleFixture` with memory metadata + memory remote + local store dir) for Phase 23 integration test.
- `pkg/controlplane/runtime/snapshot_hold.go` — extend per D-23-02 + D-23-04 in place; no new file.

### Established Patterns
- Optional capability interfaces (`Backupable`, `HoldProvider`, `ObjectIDIndexAccessor`) — nil-safe at call site. `CreateSnapshot` type-asserts `metadata.MetadataStore.(metadata.Backupable)`; if assertion fails, return a fixed error (the metadata engine doesn't support snapshots — only `memory`/`badger`/`postgres` do via Phase 21).
- Phase 22 GORM model conventions: `gorm:"primaryKey;size:36"` UUID, `autoCreateTime`/`autoUpdateTime`, `gorm:"index;not null"` on FK lookups.
- Error sentinels as `var ErrX = errors.New(...)` in `pkg/controlplane/models/` (Phase 22 already added `ErrSnapshotNotFound`).
- Mark-phase fail-closed per INV-04 — `HoldProvider` errors abort the GC sweep. The revised filter (D-23-02) must propagate I/O errors (e.g., manifest unreadable) rather than swallow them.
- `RunBlockGC` per-remote loop with deduplicated `DistinctRemoteStores()` entries — Phase 22 D-03 per-remote scope is preserved by Phase 23 (no change to the loop, just the filter inside `SnapshotHoldProvider.HeldHashes`).
- Single PR vs `develop` with staged per-plan commits — Phase 22 cadence (D-20 from Phase 22 + D-23-23 here).
- Memory-only integration test for orchestration semantics — Phase 22 D-21 sets the precedent; Phase 21 conformance suite covers driver matrix.

### Integration Points
- `pkg/controlplane/runtime/snapshot.go` (new) — `Runtime.CreateSnapshot` + `Runtime.WaitForSnapshot` + registry + startup recovery method (called from `Runtime` init)
- `pkg/controlplane/runtime/shares/service.go` (modify `RemoveShare`) — cancel + `WaitGroup.Wait` for in-flight snap goroutines for that share, BEFORE the Phase 22 D-15 snapshots-tree wipe
- `pkg/controlplane/runtime/runtime.go` (modify init + Shutdown) — registry initialization, startup recovery invocation, Shutdown cancel + WG.Wait
- `pkg/controlplane/runtime/snapshot_hold.go` (modify) — revise filter per D-23-02, add RWMutex per D-23-04
- `pkg/controlplane/models/errors.go` (modify) — add five sentinels per D-23-12
- Config schema (path TBD by planner) — add `snapshot.sync_gate_concurrency` (default 16)

</code_context>

<specifics>
## Specific Ideas

- Async orchestration mirrors the deferred Phase 11 IN-4-03 pattern (`RunBlockGC` async + 202+poll REST) but stays SCOPED to snapshot creation only. Don't generalize; Phase 11 follow-up is independent.
- The `RetryOf` opt is the API-shape companion to Phase 22 D-06 (`failed → creating` retry semantics). Naming and validation behavior must match the D-06 invariant verbatim.
- `VerifyRemoteDurability` returns the missing hash in its error message so operators can grep logs and reconstruct what's unsynced. Don't redact.
- Structured log keys should match Phase 22's snapshot logging style (`snapshot_id`, `share`, etc.) so log aggregation works across phases.
- Integration test should exercise the `RemoveShare-cancels-in-flight` path by starting a `CreateSnapshot` with an artificially slow `Backupable` fixture, then calling `RemoveShare` mid-flight and asserting (a) `WaitForSnapshot` returns with a cancelled error, (b) `<share>/snapshots/` is fully removed, (c) no panic from a goroutine writing into a removed tree.

</specifics>

<deferred>
## Deferred Ideas

- **Metrics surface for snapshot lifecycle** — counters (`snapshots_created_total{state}`, `snapshot_create_duration_seconds`, `sync_gate_drain_blocks_total`). Promoted in Phase 25 or a future v0.17 observability pass once a prometheus/OTel layer exists in runtime.
- **Streaming progress events from CreateSnapshot** — caller-supplied `chan<- Progress` for live progress UI in CLI. Defer to Phase 25 if CLI UX demands it; polling `GetSnapshot` is sufficient for v0.16.0.
- **Collect-all-missing diagnostic mode on `VerifyRemoteDurability`** — current fail-fast (D-23-06) loses visibility into how many blocks are unsynced. Add a `VerifyRemoteDurabilityVerbose` or an opt flag if operator diagnosis becomes painful. Drain+re-verify usually masks the need.
- **Async GC + 202+poll REST for `RunBlockGC`** (Phase 11 IN-4-03 deferred) — independent of snapshot async; tackle in its own phase.
- **Per-snapshot RWMutex granularity upgrade** — if a provider-level RWMutex (D-23-04) starts causing head-of-line blocking under high snapshot counts, upgrade to `sync.Map[snapID]*RWMutex`. Benchmark first.
- **Auto-cleanup of long-failed snapshots** — TTL-based cleanup of `state=failed` rows + dirs to bound disk usage. Operator-driven `DeleteSnapshot` is sufficient for now.
- **`WaitForSnapshot` event-stream API for many subscribers** — current per-snapshot `chan struct{}` is single-subscriber-ish; if Phase 25 grows multiple concurrent watchers, broadcast via `sync.Cond` or a closed-on-completion channel.

</deferred>

---

*Phase: 23-Snapshot Create Orchestration + Sync Gate*
*Context gathered: 2026-05-28*

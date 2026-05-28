# Phase 22: Snapshot Records + Hash Manifest + GC Hold - Context

**Gathered:** 2026-05-28
**Status:** Ready for planning
**GH issue:** [#643](https://github.com/marmos91/dittofs/issues/643)
**Milestone:** v0.16.0 Share Snapshots — Phase 3 of 6
**Depends on:** Phase 20 (Backupable interface + HashSet), Phase 21 (per-engine backup drivers)

<domain>
## Phase Boundary

Build the three components that glue Phase 21's backup drivers to Phase 23's snapshot orchestration: (1) GORM-backed snapshot lifecycle records, (2) on-disk hash manifest writer/reader, (3) `SnapshotHoldProvider` that injects active-snapshot hashes into the GC mark phase so referenced blocks are never collected.

**In scope:**
- `Snapshot` GORM model with UUID ID, ShareName, State (creating/ready/failed), MetadataEngine, ManifestCount, RemoteDurable, timestamps
- Path-helper methods on the model (`SnapshotDir`, `ManifestPath`, `MetadataDumpPath`)
- `SnapshotStore` sub-interface + GORM implementation (Create/Get/List/Delete/UpdateState)
- Unique partial index preventing concurrent `creating` snapshots per share
- Cascade-delete from share → snapshot rows + Runtime hook to rm snapshot dirs on share removal
- `pkg/snapshot/manifest.go` — `WriteManifest(io.Writer, *HashSet)` and `ReadManifest(io.Reader) (*HashSet, error)`
- `HoldProvider` interface in `pkg/blockstore/engine/gc.go` + `markPhase` wiring
- `SnapshotHoldProvider` implementation in `pkg/controlplane/runtime/blockgc.go` (per-remote scoped)
- Integration test against memory metadata store: create snapshot → GC → blocks survive → delete snapshot → GC → blocks collected

**Out of scope:**
- Snapshot create orchestration / sync gate (Phase 23)
- Restore flow (Phase 24)
- CLI/REST API (Phase 25)
- Postgres/Badger driver coverage in the GC-hold integration test (covered by Phase 21 conformance)

</domain>

<decisions>
## Implementation Decisions

### GC hold injection
- **D-01:** `HoldProvider` interface lives in `pkg/blockstore/engine/gc.go` alongside `MetadataReconciler`. Wired via a new `HoldProvider` field on `engine.Options`. `markPhase` calls `HeldHashes` AFTER `EnumerateFileBlocks` so held hashes land in the same disk-backed `GCState` live set used by the sweep. Single live set, single sweep — no new code paths in `sweepPhase`.
- **D-02:** Interface signature: `HeldHashes(ctx, remoteEndpointID string, shares []string, fn func(blockstore.ContentHash) error) error`. Mirrors `EnumerateFileBlocks` callback shape so `markPhase` can reuse its hash-add loop verbatim. Errors fail-closed per INV-04 (mark abort) — orphan-not-deleted is always preferred over live-data-deleted.
- **D-03:** Per-remote scoped hold. `Runtime.RunBlockGC` loops per distinct remote already; it constructs a `SnapshotHoldProvider` scoped to the share-names targeting that remote. The provider filters snapshots to `state='ready'` on those shares, then streams each snapshot's `manifest.hashes` file into the callback. Matches existing per-remote GC architecture.
- **D-04:** Ground-truth source for held hashes is the on-disk `manifest.hashes` file, not a DB column. The DB record carries `ManifestCount` for quick stats but the hashes themselves live only on disk. Avoids dual-write divergence.
- **D-05:** Only `state='ready'` snapshots hold. `creating` snapshots have no complete manifest yet; if a block they reference is collected mid-create, the snapshot fails and the user retries. GC grace period covers most of the create window naturally.

### Snapshot state machine
- **D-06:** Three states: `creating → ready` (success) or `creating → failed` (error). `failed` is **not** terminal — Phase 23 orchestration may transition `failed → creating` for retry. Retry must be idempotent (re-use the same snapshot ID and directory; overwrite manifest atomically).
- **D-07:** Delete is hard-delete: remove GORM row + rm `<share-data-dir>/snapshots/<id>/` atomically (best-effort filesystem cleanup; row deletion is the source of truth). No `deleting` state. If filesystem cleanup fails after row deletion, GC hold is already released — orphaned files are harmless (next manifest read for that ID will fail and the dir can be removed administratively).
- **D-08:** Unique partial index on `(share_name)` where `state='creating'`. Prevents two concurrent `creating` snapshots per share. Multiple `ready` snapshots per share are explicitly allowed. GORM tag: `gorm:"index:idx_share_creating,where:state='creating',unique"`.

### GORM model
- **D-09:** ID is UUID via `github.com/google/uuid` (matches every existing model — Share, User, Group, etc.). SNAP-01's "ULID" wording is overridden in favor of consistency. List ordering uses `ORDER BY created_at DESC` when chronological order is needed.
- **D-10:** `MetadataEngine` field stores the engine tag (`"memory"`/`"badger"`/`"postgres"`) captured at snapshot-create time. Phase 24 restore validates engine match before attempting restore — protects against share-config changes between snapshot and restore.
- **D-11:** Fields: `ID string` (PK), `ShareName string` (indexed, FK cascade), `State string` (creating/ready/failed), `MetadataEngine string`, `ManifestCount int64`, `RemoteDurable bool`, `CreatedAt time.Time`, `UpdatedAt time.Time`. Plus state-constant exports: `StateCreating`, `StateReady`, `StateFailed`.
- **D-12:** Path helpers as methods on `*Snapshot`:
  - `SnapshotDir(shareDataDir string) string` → `<shareDataDir>/snapshots/<id>`
  - `ManifestPath(shareDataDir string) string` → `<SnapshotDir>/manifest.hashes`
  - `MetadataDumpPath(shareDataDir string) string` → `<SnapshotDir>/metadata.dump`
  Single source of truth for SNAP-02 layout; Phase 23/24 callers never compute paths themselves.

### Control plane store
- **D-13:** New `SnapshotStore` sub-interface in `pkg/controlplane/store/interface.go`. Methods: `CreateSnapshot(ctx, *Snapshot) (string, error)`, `GetSnapshot(ctx, shareName, snapID string) (*Snapshot, error)`, `ListSnapshots(ctx, shareName string) ([]*Snapshot, error)`, `DeleteSnapshot(ctx, shareName, snapID string) error`, `UpdateSnapshotState(ctx, id string, state string) error`. Composed into top-level `Store` interface alongside `ShareStore`, `UserStore`, etc.
- **D-14:** `ListSnapshots` returns ALL states; callers filter. `HoldProvider` filters to `state='ready'` itself. Keeps store API minimal; the filter set may evolve in Phase 25 (CLI/REST) without churning the interface.
- **D-15:** GORM FK constraint with `OnDelete:CASCADE` on `ShareName`. Combined with a hook in `Runtime.RemoveShare` that runs `os.RemoveAll(<share-data-dir>/snapshots/)` BEFORE the DB cascade fires — avoids orphaned manifest files.

### Manifest format
- **D-16:** Plain text, one hex-encoded `ContentHash` per line, sorted ascending. Matches SNAP-02 spec verbatim. Human-readable + greppable; ~6.5 MB for 100k hashes (acceptable per VM-workload sizing). No compression — disk space is not the binding constraint at this scale.
- **D-17:** `WriteManifest(w io.Writer, hs *HashSet) error` iterates `hs.Sorted()` and writes hex lines via `bufio.NewWriter` (low write-side memory). Phase 20 D-03 contract preserved: backup returns HashSet in-memory, manifest writer consumes it.
- **D-18:** `ReadManifest(r io.Reader) (*HashSet, error)` parses hex lines via `bufio.Scanner` and returns a populated `*HashSet`. GC hold provider and Phase 23 sync gate both consume `HashSet`. ~3.2 MB per snapshot for 100k hashes — well within RAM.
- **D-19:** Atomic manifest write: write to `manifest.hashes.tmp`, fsync, rename to `manifest.hashes`. Prevents readers from observing a half-written manifest if the writer crashes mid-create. Phase 23 owns the orchestration; Phase 22 just exposes a `WriteManifestAtomic(path string, hs *HashSet) error` convenience that handles temp-file + rename. Plain `WriteManifest(io.Writer, ...)` remains for tests.

### Code structure
- **D-20:** Single PR against develop with staged commits, each building independently:
  1. `feat(controlplane): snapshot model + path helpers + state constants`
  2. `feat(snapshot): manifest writer/reader + atomic write helper`
  3. `feat(controlplane): SnapshotStore interface + GORM impl + cascade-delete hook`
  4. `feat(engine): HoldProvider interface + markPhase wiring`
  5. `feat(runtime): SnapshotHoldProvider implementation + RunBlockGC wiring`
  6. `test(runtime): integration test — snapshot lifecycle vs GC`
- **D-21:** Integration test uses the memory metadata store only. Phase 21 conformance suite already proves backup/restore round-trip for all three engines; Phase 22's test focuses on GC-hold semantics, not driver coverage. Test path: create snapshot via Phase 21 backup → mark ready → run GC → assert held blocks survive → delete snapshot → run GC → assert blocks now collected.

### Claude's Discretion
- D-01 / D-02 (GC hold injection mechanism — `Options` field vs alternatives) — Claude picks based on existing `Options`/`MetadataReconciler` patterns
- D-04 (manifest file is ground truth vs DB column) — derived from minimal-surface principle
- Manifest scanner buffer sizing (default `bufio.Scanner` buffer may need `Buffer()` override for hex lines + LF)
- Whether to expose `ManifestCount` validation (read manifest, count lines, compare to DB) as an explicit method or fold into Phase 23

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements and design
- `.planning/REQUIREMENTS.md` §Snapshot records, hash manifest, and GC hold (SNAP) — SNAP-01..05 are Phase 22's requirements
- `.planning/ROADMAP.md` §Phase 22 — success criteria and files-to-touch list

### Phase 20 foundation (direct dependency)
- `pkg/blockstore/hashset.go` — `HashSet` type with `Add`/`Contains`/`Len`/`ForEach`/`Sorted`/`Hashes` (consumed by manifest writer/reader and HoldProvider)
- `pkg/blockstore/types.go` — `ContentHash` type with `String()`/`CASKey()` methods (hex encoding source of truth)
- `pkg/metadata/backupable.go` — `Backupable` interface (Phase 22 does NOT extend; Phase 23 calls it during orchestration)
- `.planning/phases/20-backupable-interface-conformance-suite-cleanup/20-CONTEXT.md` — Phase 20 decisions (D-01..D-21), especially D-03 (HashSet separate from stream), D-08 (map[ContentHash]struct{}), D-11 (Sorted method), D-12 (no MarshalBinary on HashSet)

### Phase 21 foundation (direct dependency)
- `.planning/phases/21-per-engine-backup-drivers/21-CONTEXT.md` — Phase 21 decisions (D-01..D-09)
- `pkg/metadata/store/memory/backup.go` — memory engine `Backup` returns `*HashSet` for manifest construction (Phase 23 caller, but referenced for integration test)

### GC integration (modify)
- `pkg/blockstore/engine/gc.go` — mark-sweep GC; Phase 22 adds `HoldProvider` interface + `Options.HoldProvider` field + `markPhase` injection call. Key existing types: `Options` struct (line 125), `MetadataReconciler` / `MultiShareReconciler` interfaces (line 177/185), `CollectGarbage` entry (line 200), `markPhase` (line 329)
- `pkg/blockstore/engine/gcstate.go` — disk-backed live set; HoldProvider hashes go into the same `GCState` as `EnumerateFileBlocks` output via `gcs.Add()` + `gcs.FlushAdd()`
- `pkg/controlplane/runtime/blockgc.go` — `Runtime.RunBlockGC` per-remote loop (line 33+); Phase 22 wires `opts.HoldProvider = r.snapshotHoldForRemote(entry.ConfigID, entry.ShareNames)` here
- `pkg/blockstore/engine/gc_test.go:396` — `TestGCMarkSweep_NoBackupHoldProvider` (legacy v0.13.0 hold provider absence — Phase 22 supersedes with `HoldProvider`; this test may need rename or replacement to assert `NoSnapshotHoldProvider` and a new positive test for hold semantics)

### Control plane patterns (extend)
- `pkg/controlplane/store/interface.go` — sub-interface composition pattern (UserStore, ShareStore, etc., composed into `Store`); Phase 22 adds `SnapshotStore` alongside
- `pkg/controlplane/store/shares.go` — GORM CRUD pattern via `getByField`/`listAll` helpers + `uuid.New().String()` ID generation
- `pkg/controlplane/store/gorm.go` — GORM bootstrap; new `Snapshot` model must be registered for automigrate
- `pkg/controlplane/models/share.go` — GORM model conventions (primaryKey size:36, indexes, JSON tags, autoCreateTime/autoUpdateTime)
- `pkg/controlplane/models/errors.go` (search location) — error sentinel pattern (`ErrShareNotFound`); Phase 22 adds `ErrSnapshotNotFound`, `ErrSnapshotStateConflict`

### Runtime integration (extend)
- `pkg/controlplane/runtime/runtime.go` — `Runtime` struct; Phase 22 adds `snapshotHoldForRemote(configID string, shares []string) engine.HoldProvider` method that returns a closure reading manifests from disk
- `pkg/controlplane/runtime/shares/` — share lifecycle; `RemoveShare` gains the snapshots-dir cleanup hook per D-15

### New files
- `pkg/controlplane/models/snapshot.go` (new) — Snapshot struct + state constants + path helpers
- `pkg/controlplane/store/snapshots.go` (new) — `SnapshotStore` GORM impl
- `pkg/snapshot/manifest.go` (new) — `WriteManifest` / `WriteManifestAtomic` / `ReadManifest`

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/blockstore/hashset.go` — `HashSet.Sorted()`, `HashSet.Len()`, `NewHashSet(capacity)` directly consumed by manifest I/O and HoldProvider
- `pkg/blockstore/types.go` — `ContentHash` already has hex `String()` method usable as manifest line encoding; reverse path needs `hex.DecodeString` + length-check (32 bytes expected)
- `pkg/controlplane/store/shares.go` — `getByField`/`listAll` generic helpers reusable for `GetSnapshot`/`ListSnapshots`
- `pkg/blockstore/engine/gcstate.go` — disk-backed `GCState` already supports batched `Add` + `FlushAdd`; HoldProvider hashes use the same Add path inside `markPhase`

### Established Patterns
- Optional capability interfaces (`ObjectIDIndexAccessor`, `FileBlockRefsAccessor`, `Backupable`) — `HoldProvider` follows the same shape: nil-safe at call site, no inheritance from any base
- GORM model conventions: `gorm:"primaryKey;size:36"` for UUIDs, `gorm:"index;not null"` for FK lookups, `autoCreateTime`/`autoUpdateTime` for timestamps
- Sub-interface composition in `pkg/controlplane/store/interface.go` — `Store` embeds focused sub-interfaces; consumers accept the narrowest needed
- Error sentinels as `var ErrX = errors.New(...)` in `pkg/controlplane/models/`
- Mark-phase fail-closed per INV-04 (any error aborts sweep) — HoldProvider errors MUST propagate up through `markPhase` to abort sweep
- `RunBlockGC` per-remote loop with deduplicated `DistinctRemoteStores()` entries — natural injection point for per-remote `HoldProvider`

### Integration Points
- `pkg/blockstore/engine/gc.go` — `Options` struct gains `HoldProvider HoldProvider` field; `markPhase` signature gains `hold HoldProvider` parameter; `CollectGarbage` threads it through
- `pkg/controlplane/runtime/blockgc.go` — `RunBlockGC` constructs the provider per remote inside the existing entries loop
- `pkg/controlplane/runtime/shares/` (or wherever `RemoveShare` lives) — cleanup hook runs BEFORE the DB cascade
- `pkg/controlplane/store/gorm.go` — automigrate registration for `models.Snapshot`
- `internal/cli/` is untouched in Phase 22 (CLI lives in Phase 25)

</code_context>

<specifics>
## Specific Ideas

- Manifest atomic write follows the same temp-file + fsync + rename pattern used elsewhere in the codebase for crash-safe writes
- The integration test should be wired against a "fake remote" (in-memory `RemoteStore`) so the GC sweep can be invoked deterministically without S3/Localstack — match the pattern used in existing `gc_test.go` tests
- `MetadataEngine` tag values mirror Phase 20 envelope tags (`"memory"`/`"badger"`/`"postgres"`) — single source of truth across the backup stack

</specifics>

<deferred>
## Deferred Ideas

- **Hash-list DB index for faster snapshot listing under huge counts** — currently the manifest file is the only on-disk source; if listing performance becomes an issue at 1000s of snapshots, a denormalized index could be added later without breaking compatibility
- **Async GC hold release** — if delete is hard + immediate (D-07) and a GC run is concurrently reading the manifest, the read may fail mid-stream. Phase 23 orchestration should hold a brief lock around `Delete` to serialize against in-flight `HeldHashes` calls; currently parked as a Phase 23 concern, not Phase 22
- **`deleting` soft-state for crash-safe cleanup** — deferred per D-07; revisit if orphaned manifest files become operationally painful
- **ULID adoption project-wide** — SNAP-01 mentioned ULID but D-09 chose UUID for consistency; if chronological sortability becomes broadly valuable, that's a separate cross-cutting migration

</deferred>

---

*Phase: 22-Snapshot Records + Hash Manifest + GC Hold*
*Context gathered: 2026-05-28*

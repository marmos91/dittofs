# Phase 24: Restore Flow - Pattern Map

**Mapped:** 2026-05-28
**Files analyzed:** 9 (7 new + 2 modified; one optional `hashset_from_metadata.go` is planner-discretion)
**Analogs found:** 9 / 9

> Phase 23 has not yet merged to `develop`; the `Resetable` work sits on `gsd/phase-24-restore-flow` which is itself branched off Phase 23 work (`gsd/phase-23-snapshot-create-orchestration-sync-gate`). All Phase 23 analog excerpts below are quoted from that branch; line numbers are blob line numbers (read via `git show <ref>:<path>`). The phase-23 tip is at commit `b1f6ff47`.

## File Classification

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|-------------------|------|-----------|----------------|---------------|
| `pkg/metadata/resetable.go` | interface (optional capability) | request-response | `pkg/metadata/backupable.go` | exact |
| `pkg/metadata/store/memory/reset.go` | store (driver impl) | transform / mutate | `pkg/metadata/store/memory/backup.go` (Restore half) | exact |
| `pkg/metadata/store/badger/reset.go` | store (driver impl) | batch (KV truncate) | `pkg/metadata/store/badger/backup.go` (Restore half) | exact |
| `pkg/metadata/store/postgres/reset.go` | store (driver impl) | batch (DDL/DML in tx) | `pkg/metadata/store/postgres/backup.go` (`Restore` + `truncateAllTables`) | exact |
| `pkg/metadata/storetest/reset_conformance.go` | test (conformance suite) | request-response | `pkg/metadata/storetest/backup_conformance.go` (`RunBackupConformanceSuite` + `testBackup_NonEmptyDest` + `populateTestData`) | exact |
| `pkg/controlplane/runtime/snapshot.go` (extended with `RestoreSnapshot` + `RestoreSnapshotOpts`) | controller/service (orchestration) | request-response (synchronous) | `pkg/controlplane/runtime/snapshot.go::Runtime.CreateSnapshot` (Phase 23 branch) | role-match (sync vs async) |
| `pkg/controlplane/models/errors.go` (7 new sentinels) | model (error sentinels) | n/a | `pkg/controlplane/models/errors.go` Phase 22/23 sentinels block | exact |
| `pkg/controlplane/runtime/snapshot_restore_test.go` | test (integration) | request-response | `pkg/controlplane/runtime/snapshot_integration_test.go` (Phase 23 branch) | exact |
| `pkg/snapshot/hashset_from_metadata.go` (planner discretion, only if no Phase 21 helper to reuse) | utility (helper) | transform (walk → set) | `pkg/metadata/store/memory/backup.go` lines 182-191 (hash-extraction inside `Backup`) | partial (extracted from inline code) |

---

## Pattern Assignments

### `pkg/metadata/resetable.go` (interface, request-response)

**Analog:** `pkg/metadata/backupable.go`

**Package + import pattern** (`backupable.go` lines 1-9):
```go
package metadata

import (
	"context"
	"errors"
	"io"

	"github.com/marmos91/dittofs/pkg/blockstore"
)
```
Phase 24 needs only `context` and `errors`. No `io` or `blockstore` imports.

**Doc-comment + assertion pattern** (`backupable.go` lines 11-39): the optional-capability docstring explicitly tells call sites to do a type assertion, names the sentinel, and warns it is NOT embedded in `MetadataStore`. Copy the structure verbatim, substituting "Reset" semantics:

```go
// Resetable is an optional capability that metadata store backends may
// implement to support truncate-in-place semantics for share restore.
// It is deliberately NOT embedded in MetadataStore so that protocol
// handlers and the runtime never depend on reset support existing.
//
// Call sites discover the capability via a type assertion:
//
//	if r, ok := store.(metadata.Resetable); ok {
//	    if err := r.Reset(ctx); err != nil { ... }
//	}
//
// Reset truncates all metadata-store contents in-place. The same store
// instance is reused — no close/reopen, no Service unregister/register
// dance, no recreate cost. Implementations MUST preserve the live store
// handle (the underlying *badger.DB, *pgx.Pool, in-memory maps' backing
// allocator) so callers can immediately follow up with Backupable.Restore
// or other store operations without re-resolving the share.
type Resetable interface {
	Reset(ctx context.Context) error
}
```

**Error sentinel pattern** (`backupable.go` lines 41-63): if any Reset-specific sentinels are needed (planner discretion — likely none, since Reset returns wrapped backend errors), follow the `errors.New("metadata: ...")` shape used by `ErrRestoreDestinationNotEmpty`. The runtime-level sentinel `ErrMetadataStoreNotResetable` lives in `models/errors.go` instead (per D-24-08), so this file likely needs no `var ( ... )` block at all.

---

### `pkg/metadata/store/memory/reset.go` (driver impl, transform/mutate)

**Analog:** `pkg/metadata/store/memory/backup.go` (`Restore` lines 239-415)

**Compile-time assertion pattern** (`backup.go` line 90):
```go
var _ metadata.Backupable = (*MemoryMetadataStore)(nil)
```
Mirror as:
```go
var _ metadata.Resetable = (*MemoryMetadataStore)(nil)
```

**Lock + reassignment pattern** (`backup.go` lines 314-365 — the tail end of `Restore` that wipes-and-repopulates under `s.mu.Lock`):
```go
// Acquire write lock and populate all store fields.
s.mu.Lock()
defer s.mu.Unlock()

s.shares = snap.Shares
s.files  = snap.Files
// ...

s.rollupMu.Lock()
s.rollupOffsets = snap.RollupOffsets
s.rollupMu.Unlock()

s.syncedMu.Lock()
s.synced = snap.Synced
s.syncedMu.Unlock()
```
Reset performs the same mutation shape but assigns *empty* maps (matching `NewMemoryMetadataStore` lines 322-345 — see below) instead of decoded snapshot data. Order of operations:
1. `ctx.Err()` early-out (mirror `backup.go` line 240-242).
2. `s.mu.Lock(); defer s.mu.Unlock()`.
3. Reassign every map listed in `NewMemoryMetadataStore` to a fresh empty `make(...)` of the same key/value type.
4. Acquire `rollupMu.Lock()` and reset `rollupOffsets`; release.
5. Acquire `syncedMu.Lock()` and reset `synced`; release.
6. Re-init transient state (`sortedDirCache`, `sessions`).
7. `s.usedBytes.Store(0)`.
8. Reset lazy sub-stores to nil (matches `backup.go` lines 376-399 nil branches).

**Constructor map list** (`pkg/metadata/store/memory/store.go` lines 322-345) — the authoritative enumeration of fields Reset must zero:
```go
store := &MemoryMetadataStore{
    shares:          make(map[string]*shareData),
    files:           make(map[string]*fileData),
    parents:         make(map[string]metadata.FileHandle),
    children:        make(map[string]map[string]metadata.FileHandle),
    linkCounts:      make(map[string]uint32),
    deviceNumbers:   make(map[string]*deviceNumber),
    pendingWrites:   make(map[string]*metadata.WriteOperation),
    capabilities:    config.Capabilities,
    maxStorageBytes: config.MaxStorageBytes,
    maxFiles:        config.MaxFiles,
    sessions:        make(map[string]*metadata.ShareSession),
    sortedDirCache:  make(map[string][]string),
    storeID:         ulid.Make().String(),
    rollupOffsets:   make(map[string]uint64),
    objectIndex:     make(map[blockstore.ContentHash]string),
}
```

> **Implementation note for the planner:** capabilities / maxStorageBytes / maxFiles / storeID must NOT be reset — these are config, not data. The plan should explicitly enumerate "data" fields vs "config" fields with a comment so a future reviewer doesn't accidentally widen Reset.

---

### `pkg/metadata/store/badger/reset.go` (driver impl, batch)

**Analog:** `pkg/metadata/store/badger/backup.go` (`Restore` lines 170-286, helper `isStoreEmpty` lines 290-307)

**Compile-time assertion** (`backup.go` line 38):
```go
var _ metadata.Backupable = (*BadgerMetadataStore)(nil)
```
Mirror:
```go
var _ metadata.Resetable = (*BadgerMetadataStore)(nil)
```

**Core pattern — `db.DropAll`** (per D-24-03). Badger documents `DropAll` as the atomic-truncate primitive; the underlying `*badger.DB` handle (`s.db`, declared `pkg/metadata/store/badger/store.go:60`) stays valid afterward. Skeleton:
```go
func (s *BadgerMetadataStore) Reset(ctx context.Context) error {
    if err := ctx.Err(); err != nil {
        return fmt.Errorf("badger reset cancelled: %w", err)
    }
    if err := s.db.DropAll(); err != nil {
        return fmt.Errorf("badger reset: drop all: %w", err)
    }
    return nil
}
```

**Imports pattern** (`backup.go` lines 1-17): Reset needs only `context`, `fmt`, and the badger driver alias used elsewhere in the file:
```go
import (
    "context"
    "fmt"

    badgerdb "github.com/dgraph-io/badger/v4"

    "github.com/marmos91/dittofs/pkg/metadata"
)
```
The `badgerdb` alias is referenced indirectly via `s.db.DropAll()`; planner may drop the import if no symbol is touched (Go vet will complain otherwise).

**Error wrapping pattern** (`backup.go` lines 53, 73, 78, 91, etc.): every error wraps with `fmt.Errorf("...: %w", err)` and prefixes with a short context tag. Reuse "badger reset" as the tag.

---

### `pkg/metadata/store/postgres/reset.go` (driver impl, batch DDL/DML)

**Analog:** `pkg/metadata/store/postgres/backup.go` — specifically `Restore` (lines 226-316), `truncateAllTables` (lines 321-329), and the shared `backupTables` enumeration (lines 39-55).

**Compile-time assertion** (`backup.go` line 58):
```go
var _ metadata.Backupable = (*PostgresMetadataStore)(nil)
```
Mirror.

**Truncate pattern** (`backup.go` lines 321-329 — exact code to copy):
```go
func truncateAllTables(ctx context.Context, raw *pgconn.PgConn) error {
    sql := "TRUNCATE " + strings.Join(backupTables, ", ") + " CASCADE"
    if _, err := raw.Exec(ctx, sql).ReadAll(); err != nil {
        return fmt.Errorf("TRUNCATE: %w", err)
    }
    return nil
}
```

**Table list to reuse** (`backup.go` lines 39-55 — `backupTables` is exported within the package and already enumerated; Reset MUST reuse it so additions stay in sync. This is the "single source of truth" the planner should call out in the plan):
```go
var backupTables = []string{
    "server_config", "filesystem_capabilities", "files", "shares",
    "parent_child_map", "link_counts", "pending_writes",
    "file_block_refs", "file_blocks", "locks", "server_epoch",
    "nsm_client_registrations", "durable_handles", "rollup_offsets",
    "synced_hashes",
    // Phase 22 added snapshots; planner audits 000016+ migrations and
    // adds e.g. "snapshots" to this list if not already present.
}
```

> **Planner audit task:** `pkg/metadata/store/postgres/migrations/` contains 16 numbered migrations as of this branch (`000001`-`000016`). Verify that `backupTables` covers every CREATE TABLE in those files. The D-24-03 sub-task adds a CI test that fails if a new migration introduces a table not in the list.

**Connection acquire + tx pattern** (`backup.go` lines 267-282, the Restore tx setup — copy verbatim, REPEATABLE READ swap not required since Reset does pure DDL):
```go
acquireCtx, acquireCancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
defer acquireCancel()

conn, err := s.pool.Acquire(acquireCtx)
if err != nil {
    return fmt.Errorf("reset: acquire connection: %w", err)
}
defer conn.Release()

pgRaw := conn.Conn().PgConn()

if _, err := pgRaw.Exec(ctx, "BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ").ReadAll(); err != nil {
    return fmt.Errorf("reset: begin txn: %w", err)
}
defer func() { _, _ = pgRaw.Exec(ctx, "ROLLBACK").ReadAll() }()

// ... truncateAllTables(ctx, pgRaw) ...

if _, err := pgRaw.Exec(ctx, "COMMIT").ReadAll(); err != nil {
    return fmt.Errorf("reset: commit: %w", err)
}
```

**Error wrapping prefix:** "reset" (matching the "backup" / "restore" tags used for symmetry).

**Imports pattern** (`backup.go` lines 1-19): Reset trims to:
```go
import (
    "context"
    "fmt"
    "strings"

    "github.com/jackc/pgx/v5/pgconn"
    "github.com/marmos91/dittofs/pkg/metadata"
)
```
(`pgconn` only needed if Reset calls `truncateAllTables` directly; otherwise the planner may inline the TRUNCATE statement and drop the import.)

---

### `pkg/metadata/storetest/reset_conformance.go` (test, request-response)

**Analog:** `pkg/metadata/storetest/backup_conformance.go` — `RunBackupConformanceSuite` (lines 35-63), `asBackupable` (lines 67-74), `populateTestData` (lines 78-121), `testBackup_NonEmptyDest` (lines 474-499).

**Factory + suite-runner pattern** (`backup_conformance.go` lines 16-63):
```go
type BackupableStoreFactory func(t *testing.T) metadata.MetadataStore

func RunBackupConformanceSuite(t *testing.T, factory BackupableStoreFactory) {
    t.Helper()
    probe := factory(t)
    if _, ok := probe.(metadata.Backupable); !ok {
        t.Fatal("factory must return a store implementing metadata.Backupable")
    }
    t.Run("RoundTrip", func(t *testing.T) { testBackup_RoundTrip(t, factory) })
    // ...
}
```
Mirror with `ResetThenRestoreConformance`. Per D-24-12 the suite is a single scenario (not a multi-subtest tree); the simpler shape is:
```go
// ResetThenRestoreConformance verifies that a store implementing both
// metadata.Backupable AND metadata.Resetable satisfies the Phase 24
// restore-flow contract: a populated store can be backed up, then Reset
// to empty, then restored from the same backup to its original content.
//
// The factory must return a store implementing both capabilities;
// t.Fatal otherwise (mirroring the Backupable conformance pattern).
func ResetThenRestoreConformance(t *testing.T, factory BackupableStoreFactory) {
    t.Helper()
    probe := factory(t)
    if _, ok := probe.(metadata.Backupable); !ok {
        t.Fatal("factory must return a store implementing metadata.Backupable")
    }
    if _, ok := probe.(metadata.Resetable); !ok {
        t.Fatal("factory must return a store implementing metadata.Resetable")
    }
    // ... single scenario per D-24-12 ...
}
```

**Type-assert helper pattern** (`backup_conformance.go` lines 67-74):
```go
func asBackupable(t *testing.T, store metadata.MetadataStore) metadata.Backupable {
    t.Helper()
    b, ok := store.(metadata.Backupable)
    if !ok {
        t.Fatal("store does not implement metadata.Backupable")
    }
    return b
}
```
Add a sibling `asResetable` helper with identical shape.

**Population fixture to reuse** (`backup_conformance.go` lines 78-121): `populateTestData` already creates a share with 2 files carrying 3 unique block hashes. Reuse verbatim — no new fixture needed. Reset conformance just calls `populateTestData(t, store, "rst")` (unique share-name prefix to avoid collisions with the Backup suite when both run against the same `*MemoryMetadataStore` factory closure).

**Scenario body sketch** (per D-24-12 — combining patterns from `testBackup_RoundTrip` lines 129-212 and `testBackup_NonEmptyDest` lines 474-499):
```go
store := factory(t)
b := asBackupable(t, store)
r := asResetable(t, store)
ctx := t.Context()

shareName, uniqueHashes := populateTestData(t, store, "rst")

var dumpBuf bytes.Buffer
hs, err := b.Backup(ctx, &dumpBuf)
if err != nil { t.Fatalf("Backup: %v", err) }
if hs.Len() != len(uniqueHashes) {
    t.Fatalf("HashSet.Len() = %d, want %d", hs.Len(), len(uniqueHashes))
}

// Reset wipes the store in-place.
if err := r.Reset(ctx); err != nil {
    t.Fatalf("Reset: %v", err)
}

// Assert empty: ListShares returns zero entries.
shares, err := store.ListShares(ctx)
if err != nil { t.Fatalf("ListShares post-Reset: %v", err) }
if len(shares) != 0 {
    t.Fatalf("post-Reset ListShares = %v, want empty", shares)
}

// Restore from the same dump into the (now-empty) store.
if err := b.Restore(ctx, &dumpBuf); err != nil {
    t.Fatalf("Restore post-Reset: %v", err)
}

// Verify shares + a representative file survived round-trip
// (mirror lines 157-211 of backup_conformance.go).
```

**Wiring per-backend** (analog: `pkg/metadata/store/memory/memory_conformance_test.go` lines 17-21):
```go
func TestBackupConformance(t *testing.T) {
    storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        return memory.NewMemoryMetadataStoreWithDefaults()
    })
}
```
Add a sibling `TestResetThenRestoreConformance` in each of:
- `pkg/metadata/store/memory/memory_conformance_test.go`
- `pkg/metadata/store/badger/badger_conformance_test.go`
- `pkg/metadata/store/postgres/postgres_conformance_test.go` (note: uses `newPostgresStoreFactory()` per line 70 — reuse the same helper).

---

### `pkg/controlplane/runtime/snapshot.go` (extended: `Runtime.RestoreSnapshot`, `RestoreSnapshotOpts`)

**Analog:** Phase 23 branch's same file — `Runtime.CreateSnapshot` (lines 65-166), `CreateSnapshotOpts` struct (lines 24-35), `WaitForSnapshot` (lines 201-244), `failSnap` (lines 506-514), `runSnapshotOrchestration` (lines 289-494).

**Doc + signature pattern** (Phase 23 `CreateSnapshot` lines 37-65):
```go
// CreateSnapshotOpts is the operator-facing configuration for one
// CreateSnapshot invocation. ...
type CreateSnapshotOpts struct {
    NoSyncGate bool
    RetryOf    string
}
```
Mirror for Restore (per D-24-09):
```go
// RestoreSnapshotOpts is the operator-facing configuration for one
// RestoreSnapshot invocation. Zero-value (AllowNonDurable=false)
// requests the default behavior: refuse snapshots with
// RemoteDurable=false.
type RestoreSnapshotOpts struct {
    // AllowNonDurable (D-24-06) opts into restoring from snapshots
    // created with CreateSnapshotOpts.NoSyncGate=true. Pre-verify
    // (step 2) is still the real safety gate; this flag only relaxes
    // the default-refuse precondition.
    AllowNonDurable bool

    // SkipPreVerify (D-24-08 planner discretion) skips the manifest-
    // hash remote durability check in step 2. Rarely justified;
    // pre-verify is cheap (manifest is sorted hex hashes, bounded
    // concurrency). Planner may omit this field — it can be added
    // later in a non-breaking way.
    SkipPreVerify bool
}
```

**Synchronous orchestration shape (NEW pattern this phase — sync vs Phase 23's async).** D-24-02 explicitly excludes the goroutine/registry machinery that Phase 23 needed. The Phase 23 `CreateSnapshot` body splits into "register + spawn goroutine"; Phase 24 should be a flat sequential function. The pattern to follow is closer to a service-layer method than Phase 23's "spawn + return ID" shape:

```go
// RestoreSnapshot orchestrates a synchronous reference-restore for a
// share's metadata from a previously-created snapshot dump.
//
// Preconditions (returned as wrapped sentinels for errors.Is):
//   - share.Enabled == false (operator runs `dfsctl share disable`
//     beforehand per D-24-01) — else models.ErrShareEnabled
//   - snapshot exists and snap.State == StateReady — else
//     models.ErrSnapshotNotFound / models.ErrSnapshotStateConflict
//   - snap.RemoteDurable OR opts.AllowNonDurable — else
//     models.ErrSnapshotNotDurable (D-24-06)
//
// Orchestration steps (D-24-09):
//   1. precheck
//   2. pre-verify (snapshot manifest against remote)
//   3. safety snapshot (CreateSnapshot + WaitForSnapshot, sync-gated)
//   4. open snapshot's metadata.dump
//   5. store.(Resetable).Reset
//   6. store.(Backupable).Restore
//   7. post-verify (freshly-restored hashes against remote)
//   8. return nil; share stays disabled (D-24-01)
//
// Failure-mode taxonomy: see CONTEXT D-24-13.
func (r *Runtime) RestoreSnapshot(
    ctx context.Context,
    shareName, snapID string,
    opts RestoreSnapshotOpts,
) error {
    if r == nil || r.store == nil {
        return errors.New("runtime: nil store")
    }
    // ... sequential steps ...
}
```

**Step 1 — share + snapshot precheck** (analog: Phase 23 lines 72-127 share/snapshot lookup):
```go
// (1a) Share must exist + be disabled.
enabled, err := r.sharesSvc.IsShareEnabled(shareName)
if err != nil {
    return err // ErrShareNotFound propagates via Phase 22 wrap
}
if enabled {
    return fmt.Errorf("restore snapshot %q on share %q: %w",
        snapID, shareName, models.ErrShareEnabled)
}

// (1b) Snapshot must exist + be ready + (durable or --force).
snap, err := r.store.GetSnapshot(ctx, shareName, snapID)
if err != nil {
    return err // ErrSnapshotNotFound propagates
}
if snap.State != models.StateReady {
    return fmt.Errorf("restore snapshot %q: state=%q, want %q: %w",
        snapID, snap.State, models.StateReady, models.ErrSnapshotStateConflict)
}
if !snap.RemoteDurable && !opts.AllowNonDurable {
    return fmt.Errorf("restore snapshot %q: %w",
        snapID, models.ErrSnapshotNotDurable)
}
```

**Step 2 — pre-verify** (analog: Phase 23 `runSnapshotOrchestration` step 5, lines 427-468). Read the source snapshot's manifest, call `VerifyRemoteDurability`. Reuse the helper verbatim:
```go
localStoreDir, err := r.sharesSvc.LocalStoreDir(shareName)
if err != nil {
    return err
}

if !opts.SkipPreVerify {
    manifestPath := snap.ManifestPath(localStoreDir)
    f, err := os.Open(manifestPath)
    if err != nil {
        if errors.Is(err, os.ErrNotExist) {
            return fmt.Errorf("restore snapshot %q: %w",
                snapID, models.ErrSnapshotMetadataDumpMissing) // or a sibling sentinel for manifest-missing
        }
        return fmt.Errorf("restore snapshot %q: open manifest: %w", snapID, err)
    }
    manifest, err := snapshot.ReadManifest(f)
    _ = f.Close()
    if err != nil {
        return fmt.Errorf("restore snapshot %q: read manifest: %w", snapID, err)
    }

    bs, err := r.sharesSvc.GetBlockStoreForShare(shareName)
    if err != nil || bs == nil {
        return fmt.Errorf("restore snapshot %q: get block store: %w", snapID, err)
    }
    remoteStore := bs.RemoteStore()
    if remoteStore == nil {
        return fmt.Errorf("restore snapshot %q: no remote store: %w",
            snapID, models.ErrRestoreVerifyFailed)
    }

    concurrency := r.snapshotDefaults().SyncGateConcurrency
    if err := snapshot.VerifyRemoteDurability(ctx, remoteStore, manifest, concurrency); err != nil {
        return fmt.Errorf("restore snapshot %q: pre-verify: %w: %v",
            snapID, models.ErrRestoreVerifyFailed, err)
    }
}
```

**Step 3 — safety snapshot composition** (analog: Phase 23 `WaitForSnapshot` lines 201-244, called immediately after `CreateSnapshot`). D-24-04 mandates sync gate ENABLED + visible name:
```go
safetyName := fmt.Sprintf("pre-restore-%s", time.Now().UTC().Format(time.RFC3339))
safetyID, err := r.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{
    // NoSyncGate intentionally left at default false per D-24-04.
})
if err != nil {
    return fmt.Errorf("restore snapshot %q: safety snap create: %w: %v",
        snapID, models.ErrRestoreSafetySnapFailed, err)
}
// NOTE: CreateSnapshot does not currently accept a Name; planner audits
// CreateSnapshotOpts on the phase-23 branch — if Name isn't there yet,
// the safety-snap naming is deferred to a follow-up or wired via a new
// opts field. D-24-04 spec assumes Name support exists; if absent, the
// generated UUID is sufficient for recovery (operator finds it via
// ListSnapshots filtered by created_at descending).

safetySnap, err := r.WaitForSnapshot(ctx, shareName, safetyID)
if err != nil {
    return fmt.Errorf("restore snapshot %q: safety snap wait: %w: %v",
        snapID, models.ErrRestoreSafetySnapFailed, err)
}
if safetySnap.State != models.StateReady {
    return fmt.Errorf("restore snapshot %q: safety snap final state=%q: %w",
        snapID, safetySnap.State, models.ErrRestoreSafetySnapFailed)
}
_ = safetyName // bind to log only if Name field exists
```

**Step 4-6 — open dump, Reset, Restore** (analog: Phase 23 step 1 backup file open, lines 322-330, run in reverse):
```go
dumpPath := snap.MetadataDumpPath(localStoreDir)
dumpFile, err := os.Open(dumpPath)
if err != nil {
    if errors.Is(err, os.ErrNotExist) {
        return fmt.Errorf("restore snapshot %q: %w",
            snapID, models.ErrSnapshotMetadataDumpMissing)
    }
    return fmt.Errorf("restore snapshot %q: open dump: %w", snapID, err)
}
defer dumpFile.Close()

metaStore, err := r.GetMetadataStoreForShare(shareName)
if err != nil {
    return err
}
resetable, ok := metaStore.(metadata.Resetable)
if !ok {
    return fmt.Errorf("restore snapshot %q: %w", snapID, models.ErrMetadataStoreNotResetable)
}
backupable, ok := metaStore.(metadata.Backupable)
if !ok {
    return fmt.Errorf("restore snapshot %q: metadata engine missing Backupable: %w",
        snapID, models.ErrRestoreAborted)
}

if err := resetable.Reset(ctx); err != nil {
    return fmt.Errorf("restore snapshot %q: reset (safety-snap=%s): %w: %v",
        snapID, safetyID, models.ErrRestoreAborted, err)
}
if err := backupable.Restore(ctx, dumpFile); err != nil {
    return fmt.Errorf("restore snapshot %q: restore (safety-snap=%s): %w: %v",
        snapID, safetyID, models.ErrRestoreAborted, err)
}
```

**Step 7 — post-verify** (analog: same `VerifyRemoteDurability` call as step 2, but with hashes walked from the freshly-restored store). See planner-discretion `hashset_from_metadata.go` below for the walk helper:
```go
restoredHashes, err := snapshot.HashSetFromMetadataStore(ctx, metaStore)
if err != nil {
    return fmt.Errorf("restore snapshot %q: walk restored metadata (safety-snap=%s): %w: %v",
        snapID, safetyID, models.ErrRestoreVerifyFailed, err)
}
// remoteStore + concurrency captured earlier; if planner moves their
// resolution into a sub-helper, ensure they're available here.
if err := snapshot.VerifyRemoteDurability(ctx, remoteStore, restoredHashes, concurrency); err != nil {
    return fmt.Errorf("restore snapshot %q: post-verify (safety-snap=%s): %w: %v",
        snapID, safetyID, models.ErrRestoreVerifyFailed, err)
}

return nil
```

**Logging pattern** (analog: Phase 23 lines 159-164, 310-315, 346-350, etc.): every orchestration boundary emits `logger.Info` / `logger.Debug` / `logger.Error` with `"snapshot restore: <step>"` prefix and structured fields `snapshot_id`, `share`, plus step-specific data (`safety_snap_id`, `manifest_count`, `verify_concurrency`).

---

### `pkg/controlplane/models/errors.go` (model, n/a)

**Analog:** existing Phase 22/23 sentinel block in the same file. Phase 23 branch (commit `b1f6ff47`) adds five sentinels under the comment `// Phase 23 (D-23-12): orchestration sentinels surfaced to REST in Phase 25.` — Phase 24 adds seven more under an analogous comment.

**Exact pattern to follow** (Phase 23 branch `errors.go` lines 32-37):
```go
// Phase 23 (D-23-12): orchestration sentinels surfaced to REST in Phase 25.
ErrSnapshotBackupFailed         = errors.New("snapshot backup failed")
ErrSnapshotVerifyFailed         = errors.New("snapshot verify failed: missing hashes on remote after drain")
ErrSnapshotDrainTimeout         = errors.New("snapshot drain timed out")
ErrSnapshotRetryTargetNotFound  = errors.New("snapshot retry target not found")
ErrSnapshotRetryTargetNotFailed = errors.New("snapshot retry target is not in failed state")
```

**Phase 24 additions** (per D-24-08):
```go
// Phase 24 (D-24-08): restore orchestration sentinels surfaced to REST in Phase 25.
ErrShareEnabled                = errors.New("share must be disabled before restore")
ErrSnapshotNotDurable          = errors.New("snapshot is not remote-durable; pass AllowNonDurable to override")
ErrSnapshotMetadataDumpMissing = errors.New("snapshot metadata dump file is missing")
ErrMetadataStoreNotResetable   = errors.New("metadata engine does not implement Resetable")
ErrRestoreSafetySnapFailed     = errors.New("restore safety snapshot creation or wait failed")
ErrRestoreAborted              = errors.New("restore aborted; safety snapshot retained for rollback")
ErrRestoreVerifyFailed         = errors.New("restore verify failed: missing hashes on remote")
```

> **Phase 25 mapping reminder** (forward-looking context for D-24-08): Phase 25's REST handler maps these via `errors.Is`. The mapping is `ErrShareEnabled → 400`, `ErrSnapshotNotDurable → 412`, others → `500`. This file change is purely declarative; the mapping lives in Phase 25.

---

### `pkg/controlplane/runtime/snapshot_restore_test.go` (test, integration)

**Analog:** Phase 23 branch `pkg/controlplane/runtime/snapshot_integration_test.go` (705 lines) — specifically the `TestCreateSnapshot_Integration` dispatcher (lines 47-55), the per-sub-test functions (e.g., `testHappyPath` lines 59-92, `testDrainThenVerifyFails` lines 124-160), and the `orchestrationFixture` (lines 434-499+).

**Imports pattern** (Phase 23 `snapshot_integration_test.go` lines 1-23):
```go
import (
    "context"
    "errors"
    "io"
    "os"
    "path/filepath"
    "sync"
    "testing"
    "time"

    "github.com/marmos91/dittofs/pkg/blockstore"
    "github.com/marmos91/dittofs/pkg/blockstore/engine"
    bsmemory "github.com/marmos91/dittofs/pkg/blockstore/local/memory"
    "github.com/marmos91/dittofs/pkg/blockstore/remote"
    remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
    "github.com/marmos91/dittofs/pkg/controlplane/models"
    "github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
    cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
    "github.com/marmos91/dittofs/pkg/health"
    metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)
```
Phase 24 test reuses the same set; planner may drop `io` / `health` / `sync` if not needed.

**Test dispatcher pattern** (Phase 23 lines 47-55):
```go
func TestCreateSnapshot_Integration(t *testing.T) {
    t.Run("HappyPath", testHappyPath)
    t.Run("DrainThenVerifyPasses", testDrainThenVerifyPasses)
    t.Run("DrainThenVerifyFails", testDrainThenVerifyFails)
    t.Run("RetryOfFailed", testRetryOfFailed)
    t.Run("NoSyncGate", testNoSyncGate)
    t.Run("RemoveShareCancelsInFlight", testRemoveShareCancelsInFlight)
    t.Run("StartupRecovery", testStartupRecovery)
}
```
Phase 24 equivalent (per D-24-10 P24-04 scope):
```go
func TestRestoreSnapshot_Integration(t *testing.T) {
    t.Run("HappyPath", testRestoreHappyPath)
    t.Run("EnabledShareRefuses", testRestoreEnabledShareRefuses)
    t.Run("SnapshotNotFound", testRestoreSnapshotNotFound)
    t.Run("SnapshotNotReady", testRestoreSnapshotNotReady)
    t.Run("NonDurableRefused", testRestoreNonDurableRefused)
    t.Run("AllowNonDurable", testRestoreAllowNonDurable)
    t.Run("PreVerifyFailsFast", testRestorePreVerifyFailsFast)
    t.Run("PostVerifyFails", testRestorePostVerifyFails)
    t.Run("InterruptedRestore", testRestoreInterruptedReset)
}
```

**Sub-test happy-path pattern** (Phase 23 `testHappyPath` lines 59-92):
```go
func testHappyPath(t *testing.T) {
    fx := newOrchestrationFixture(t)
    defer fx.close()

    hashes := makeHashes(3, 0xa0)
    fx.setBackupHashes(hashes)
    fx.seedRemoteAll(hashes)

    ctx := fx.ctx()
    snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
    if err != nil { t.Fatalf("CreateSnapshot: %v", err) }

    snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
    if werr != nil { t.Fatalf("WaitForSnapshot: err = %v, want nil", werr) }
    if snap.State != models.StateReady { t.Fatalf("snap.State = %q, want %q", snap.State, models.StateReady) }
    if !snap.RemoteDurable { t.Fatalf("snap.RemoteDurable = false, want true (D-23-03)") }

    dumpPath := snap.MetadataDumpPath(fx.localStoreDir)
    manifestPath := snap.ManifestPath(fx.localStoreDir)
    mustFileNonEmpty(t, dumpPath, "metadata.dump")
    mustFileNonEmpty(t, manifestPath, "manifest.hashes")
}
```
Phase 24 happy path: create a snapshot, mutate the live store (delete a file), call `RestoreSnapshot`, verify the deleted file is back and the share is still `Enabled=false`.

**Fixture pattern** (Phase 23 lines 434-499): `orchestrationFixture` composes cpstore + runtime + memory metadata + `controlledBackupable` wrapper + memory remote + memory local + `interceptingRemote` wrapper. Reuse verbatim — Phase 24 fixture might need an extra `disableShare()` helper and a way to seed pre-existing snapshots (likely by calling `fx.rt.CreateSnapshot(..., CreateSnapshotOpts{})` then `fx.rt.WaitForSnapshot(...)`).

**Failure-mode assertion pattern** (Phase 23 `testDrainThenVerifyFails` lines 124-160):
```go
snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
if !errors.Is(werr, models.ErrSnapshotVerifyFailed) {
    t.Fatalf("WaitForSnapshot err = %v, want errors.Is(ErrSnapshotVerifyFailed)", werr)
}
if snap.State != models.StateFailed {
    t.Fatalf("snap.State = %q, want %q", snap.State, models.StateFailed)
}
mustFileNonEmpty(t, snap.MetadataDumpPath(fx.localStoreDir), "metadata.dump (retained on failure)")
```
Phase 24 mirrors for each error sentinel — e.g. `errors.Is(err, models.ErrShareEnabled)`, `errors.Is(err, models.ErrSnapshotNotDurable)`. The "safety snap retained on disk" assertion mirrors the Phase 23 D-23-09 retention assertion (use `os.Stat(safetySnap.MetadataDumpPath(...))` post-failure).

**Memory-only test scope (D-24-10 P24-04 confirmation):** Phase 23 P23-06 already established memory-only as the integration test default (see lines 30-31 of `snapshot_integration_test.go`: *"against a memory-only fixture (cpstore SQLite + memory metadata + memory remote) per CONTEXT line 211"*). Phase 24 follows verbatim.

---

### `pkg/snapshot/hashset_from_metadata.go` (utility, transform) — planner discretion

**Analog:** `pkg/metadata/store/memory/backup.go` lines 182-191 (the hash-extraction loop inside `Backup`):
```go
// Extract every unique block hash into a HashSet.
hs := blockstore.NewHashSet(len(s.files))
for _, fd := range s.files {
    if fd.Attr == nil {
        continue
    }
    for _, br := range fd.Attr.Blocks {
        hs.Add(br.Hash)
    }
}
```

**Why a new helper rather than reusing `Backup`:** post-verify (step 7) walks the *freshly restored* store. Calling `Backup` would (a) serialize gigabytes of metadata into RAM for a hash-only purpose, and (b) acquire the read lock and produce an envelope that nobody consumes. The helper is a no-op-cost walk over `MetadataStore` interface methods.

**Audit task for planner (per D-24-14):** check `pkg/metadata/store/{memory,badger,postgres}/backup.go` for an already-extracted helper. The memory backup inlines the loop above (no helper). Badger backup extracts hashes during the KV iteration (`backup.go` lines 121-131 — `json.Unmarshal` of `f:` prefix entries). Postgres backup uses a dedicated SQL `extractHashes` (`backup.go` lines 185-221). **None of the three is a standalone `HashSetFromMetadataStore(ctx, store)` helper** — each is bound to the engine-specific Backup path. Phase 24 must add a new public helper.

**Helper shape (planner suggestion):**
```go
// HashSetFromMetadataStore walks every file in every share of store and
// returns the union of FileAttr.Blocks[*].Hash. Used by Phase 24's
// post-verify step (D-24-14) to assemble the input to
// VerifyRemoteDurability after a successful Restore.
//
// Iteration order is undefined; the returned HashSet collapses
// duplicates inherently. ctx is honored at directory boundaries; large
// stores will check at least once per share.
func HashSetFromMetadataStore(ctx context.Context, store metadata.MetadataStore) (*blockstore.HashSet, error) {
    // ...
}
```
Note: the metadata store interface as of this branch exposes `ListShares(ctx)` (`pkg/metadata/store.go:166-167`) + `GetRootHandle(ctx, shareName)` (`pkg/metadata/interface.go:170`) + `ReadDirectory(authCtx, dirHandle, cookie, maxBytes)` (`pkg/metadata/interface.go:77`). The walker needs to construct an `*AuthContext` permissive enough to traverse every dir; planner audits `pkg/metadata/auth_identity.go` for a "system" / "root" context constructor (analog: GC walks). If no such constructor exists, this becomes a bigger plan; alternatively, the planner can add a thin engine-specific `EnumerateHashes(ctx)` to each backend (memory: walk `s.files`; badger: iterate `f:` prefix; postgres: `SELECT DISTINCT hash FROM file_block_refs`) — that matches the Phase 21 Backup hash-extraction pattern more cleanly. This is a real design choice the planner must resolve in P24-03.

---

## Shared Patterns

### Optional-capability type-assertion + sentinel
**Source:** `pkg/metadata/backupable.go` lines 11-46 + call site in Phase 23 `pkg/controlplane/runtime/snapshot.go` lines 86-90.
**Apply to:** every callsite that uses `Resetable` (only one: the orchestration step 5 in `Runtime.RestoreSnapshot`).
```go
backupable, ok := metaStore.(metadata.Backupable)
if !ok {
    return "", fmt.Errorf("snapshot create %q: metadata engine does not implement Backupable: %w",
        shareName, models.ErrSnapshotBackupFailed)
}
```
Phase 24 mirrors with `metadata.Resetable` and `models.ErrMetadataStoreNotResetable`.

### Error wrapping with `errors.Is`-compatible sentinels
**Source:** Phase 23 `pkg/controlplane/runtime/snapshot.go` lines 332-334 (`fmt.Errorf("snapshot create %s: backup: %w: %v", snapID, sentinel, err)`) and `pkg/metadata/store/memory/backup.go` line 250 (`return metadata.ErrRestoreDestinationNotEmpty`).
**Apply to:** every error return path in `Runtime.RestoreSnapshot`, in the three `reset.go` files, and in the conformance helpers.
The convention is `fmt.Errorf("<operation> %q: <step>: %w: %v", id, sentinel, innerErr)` — the `%w` carries the sentinel for `errors.Is`, the `%v` provides the operator-readable detail. Never `errors.New` inside `RestoreSnapshot` — always wrap one of the seven D-24-08 sentinels.

### Compile-time interface assertion at file head
**Source:** `pkg/metadata/store/{memory,badger,postgres}/backup.go` lines 90 / 38 / 58.
**Apply to:** every new `reset.go` and (if a helper is added) the new `hashset_from_metadata.go` if it exposes an interface.
```go
var _ metadata.Resetable = (*MemoryMetadataStore)(nil)
```

### Structured logging at orchestration boundaries
**Source:** Phase 23 `pkg/controlplane/runtime/snapshot.go` lines 159-164 (accepted), 310-315 (start), 346-350 (step complete), 415-422 (step failed).
**Apply to:** every step boundary in `Runtime.RestoreSnapshot`. Use `"snapshot restore: <step>"` as the message prefix (mirroring `"snapshot create: <step>"`); include `snapshot_id`, `share`, and step-specific structured fields.
```go
logger.Info("snapshot restore: accepted",
    "snapshot_id", snapID, "share", shareName,
    "allow_non_durable", opts.AllowNonDurable,
)
```

### Memory-only integration test fixture composition
**Source:** Phase 23 `pkg/controlplane/runtime/snapshot_integration_test.go` `newOrchestrationFixture` (lines 445-499+). Combines `cpstore.SQLite(:memory:)` + memory metadata + memory local + memory remote + injected `*shares.Share` via `InjectShareForTesting` + `SetLocalStoreDirForTesting`.
**Apply to:** `pkg/controlplane/runtime/snapshot_restore_test.go`. Reuse the fixture verbatim; add helpers for `disableShare(name)` and `createReadySnapshot(name)` if needed.

### Sentinels go in `pkg/controlplane/models/errors.go` with a phase-tagged comment
**Source:** Phase 23 branch `errors.go` lines 32-37 (`// Phase 23 (D-23-12): ...`).
**Apply to:** the 7 new D-24-08 sentinels — add them under a single `// Phase 24 (D-24-08): ...` comment block.

---

## No Analog Found

All Phase 24 files have a strong analog. The "synchronous orchestration" shape of `Runtime.RestoreSnapshot` (no goroutine / no registry / no `WaitForRestore`) is a *deliberate departure* from Phase 23's async `CreateSnapshot`, but the per-step pattern (precheck → verify → metadata-store call → log) is the same shape — just flattened.

| Candidate "no analog" concern | Resolution |
|------------------------------|------------|
| Sync orchestration entry point | Use `Runtime.CreateSnapshot`'s precheck shape (lines 65-127) but skip the `registerSnapInFlight` + goroutine spawn (lines 143-165). |
| Pre-restore-safety snapshot composition | Phase 23 `WaitForSnapshot` exists (D-23-19); compose verbatim. |
| Walking a metadata store for hashes (no engine-specific access) | No standalone helper exists; planner must add `HashSetFromMetadataStore` (or per-engine `EnumerateHashes`). See `hashset_from_metadata.go` section above for the design choice. |

---

## Metadata

**Analog search scope:** `pkg/metadata/`, `pkg/controlplane/runtime/`, `pkg/controlplane/models/`, `pkg/snapshot/` on current `gsd/phase-24-restore-flow` branch (Phase 21/22 merged to develop) plus `gsd/phase-23-snapshot-create-orchestration-sync-gate` branch tip `b1f6ff47` (Phase 23 not yet merged — Phase 24 sits on top of it).

**Files scanned (read, not re-read):**
- `pkg/metadata/backupable.go` (64 lines)
- `pkg/metadata/store/memory/backup.go` (416 lines)
- `pkg/metadata/store/badger/backup.go` (308 lines)
- `pkg/metadata/store/postgres/backup.go` (397 lines)
- `pkg/metadata/storetest/backup_conformance.go` (650 lines, targeted reads only)
- `pkg/metadata/store/memory/store.go` (struct + constructor sections only)
- `pkg/metadata/store/memory/memory_conformance_test.go` (22 lines)
- `pkg/controlplane/models/errors.go` (44 lines on develop, 50 on phase-23 branch)
- `pkg/controlplane/runtime/runtime.go` (snippet, lines 160-200 + grep)
- `pkg/controlplane/runtime/shares/service.go` (`IsShareEnabled` lines 920-952 only)
- `pkg/controlplane/models/snapshot.go` (path-helper grep)
- `pkg/snapshot/manifest.go` (`ReadManifest` lines 77-107 only)

**Read via `git show <phase-23-branch>:<path>`:**
- `pkg/controlplane/runtime/snapshot.go` (705 lines)
- `pkg/snapshot/syncgate.go` (123 lines)
- `pkg/snapshot/dump.go` (lead 60 lines)
- `pkg/controlplane/runtime/snapshot_integration_test.go` (lines 1-500 of 705)
- `pkg/controlplane/models/errors.go` (Phase 23 sentinel block confirmation)

**Pattern extraction date:** 2026-05-28

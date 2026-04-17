# Phase 5: Restore Orchestration + Safety Rails — Pattern Map

**Mapped:** 2026-04-16
**Files analyzed:** 15 touched + 3 new packages
**Analogs found:** 15 / 15 — every file has an in-repo precedent
**Ground truth read:** `05-CONTEXT.md`, `04-CONTEXT.md`, `02-CONTEXT.md`, `03-CONTEXT.md`, plus the concrete analog files listed per assignment below.

Phase 5 extends the Phase-4 `storebackups.Service` entrypoint shape (`RunBackup` → sibling `RunRestore`), reuses the per-repo `OverlapGuard` verbatim, mirrors the Phase-4 `runtime/adapters` sub-service hot-reload idioms for `DisableShare`/`EnableShare`, and bolts a small hold-provider onto the pre-existing `pkg/blockstore/gc` orphan check. Everything else is new surface (three new files, one new package `pkg/backup/restore/`, one Phase-5-only `backup_hold.go`, one non-trivial NFSv4 boot-verifier hoist, and a linear `shares.Enabled` schema migration).

---

## File Classification

| # | File (new or modified) | Role | Data flow | Closest analog | Match quality |
|---|------------------------|------|-----------|----------------|---------------|
| 1 | `pkg/controlplane/runtime/storebackups/service.go` (MODIFIED — extend) | sub-service entrypoint | request-response + transactional swap | `storebackups.Service.RunBackup` (same file, lines 265-327) | **exact** (sibling method) |
| 2 | `pkg/controlplane/runtime/storebackups/errors.go` (MODIFIED — append sentinels) | typed error registry | n/a | same file (lines 1-15) | **exact** |
| 3 | `pkg/controlplane/runtime/storebackups/backup_hold.go` (NEW) | hold-set provider | request-response (reads across all repos) | `executor.payloadIDSetToSlice` + `destinationFactoryFromRepo` pattern | role-match |
| 4 | `pkg/controlplane/runtime/shares/service.go` (MODIFIED — add methods + field) | sub-service | request-response + callback notify | `shares.Service.AddShare` / `RemoveShare` / `UpdateShare` / `notifyShareChange` (same file) | **exact** |
| 5 | `pkg/controlplane/runtime/shares/errors.go` (NEW) | typed error registry | n/a | `pkg/controlplane/runtime/storebackups/errors.go` | **exact** |
| 6 | `pkg/controlplane/runtime/stores/service.go` (MODIFIED — add 2 methods) | runtime registry | CRUD + swap-under-lock | `stores.Service.RegisterMetadataStore` / `CloseMetadataStores` | **exact** |
| 7 | `pkg/controlplane/models/share.go` (MODIFIED — add `Enabled` column) | GORM model | schema | `models.Share.ReadOnly` (same file, line 26) | **exact** |
| 8 | `pkg/controlplane/store/gorm.go` (MODIFIED — migration gate) | persistence bootstrap | migration | `gorm.go` Phase-4 D-26 column-rename + backfill block (lines 253-279) | **exact** |
| 9 | `pkg/backup/restore/restore.go` (NEW) | orchestration executor | transactional swap | `pkg/backup/executor/executor.go` (`executor.RunBackup`) | **exact** |
| 10 | `pkg/backup/restore/fresh_store.go` (NEW) | engine-typed construction helper | lifecycle | `shares.CreateLocalStoreFromConfig` (kind-dispatch) + `badger.New` constructor | role-match |
| 11 | `pkg/backup/restore/swap.go` (NEW) | coordinator (close old + rename temp) | transactional | `engine.BlockStore.Close` + `fs.Store.PutBackup` os.Rename publish moment | role-match |
| 12 | `pkg/backup/restore/errors.go` (NEW) | typed error registry | n/a | `pkg/backup/destination/errors.go` (Phase 3) + `pkg/backup/backupable.go` sentinels | **exact** |
| 13 | `pkg/backup/destination/destination.go` (MODIFIED — add `GetManifestOnly`) | driver interface | request-response | `Destination.GetBackup` (same file, lines 31-38) | **exact** (sibling method) |
| 14 | `pkg/backup/destination/fs/store.go` (MODIFIED — add `GetManifestOnly`) | FS driver | read-only | `fs.readManifest` (same file, lines 378-393) | **exact** (helper exists, just hoist + wrap) |
| 15 | `pkg/backup/destination/s3/store.go` (MODIFIED — add `GetManifestOnly`) | S3 driver | read-only | `s3.Store.GetBackup` lines 467-483 (the manifest-read prologue) | **exact** (extract the prologue into its own method) |
| 16 | `pkg/blockstore/gc/gc.go` (MODIFIED — add BackupHoldProvider interface + hold check) | GC sweep | batch scan | same file lines 136-147 (existing `MetadataReconciler` union check) | **exact** |
| 17 | `internal/adapter/nfs/v4/handlers/write.go` (MODIFIED — hoist verifier to atomic) | package-level state | read/write | same file lines 17-24 (existing `init()` assignment) | **exact** |
| 18 | `internal/adapter/nfs/mount/handlers/mount.go` + `internal/adapter/smb/v2/handlers/tree_connect.go` (MODIFIED — consult `Share.Enabled`) | dispatch gate | request-response | NFS: `mount.go` already reads share → add `if !share.Enabled { return MountErrAccess }` before the root-handle return; SMB: tree_connect equivalent | role-match |
| 19 | `pkg/metadata/store/{memory,badger,postgres}/backup.go` (MODIFIED — populate persistent store_id) + per-engine first-open hook | engine persistence | lifecycle | each engine already has a "first-open" / `ensureSeeded` codepath; the new logic is bootstrap-a-stable-UUID-once | role-match (D-06 gap identified) |
| 20 | `pkg/metadata/store/*/backup_conformance_test.go` or equivalent (MODIFIED — verify StoreID invariant) | test | n/a | existing conformance suite in `pkg/metadata/storetest/` | **exact** |
| 21 | `pkg/controlplane/store/backup.go` (MODIFIED — add `ListSucceededRecordsByRepo`) | persistence | CRUD | same file's `ListSucceededRecordsForRetention` (lines 152-162) | **exact** (variant of existing) |

---

## Pattern Assignments

### 1. `pkg/controlplane/runtime/storebackups/service.go` — extend with `RunRestore`

**Analog:** `RunBackup(ctx, repoID)` in the SAME file, lines 265-327.

**Why this is the primary analog (per CONTEXT D-05, D-07, D-21):** same entrypoint shape (callable from scheduler OR on-demand); same per-repo `overlap.TryLock` contract; same `deriveRunCtx` helper for shutdown cancellation; same `destFactory`/`resolver` composition.

**Mirror exactly:**

Imports block (lines 1-17) — no change needed; restore imports the new `pkg/backup/restore` package alongside the existing `pkg/backup/executor`.

**Mutex + deriveRunCtx pattern (copy verbatim)** — lines 266-278:
```go
unlock, acquired := s.overlap.TryLock(repoID)
if !acquired {
    return nil, fmt.Errorf("%w: repo %s", ErrBackupAlreadyRunning, repoID)
}
defer unlock()

runCtx, cancelRun := s.deriveRunCtx(ctx)
defer cancelRun()
```

`RunRestore` reuses this EXACT block with `repoID` as the lock key (D-07). Concurrent backup+restore on the same repo is rejected with `ErrBackupAlreadyRunning` verbatim.

**Repo + resolver + destination composition (mirror)** — lines 279-301:
```go
repo, err := s.store.GetBackupRepoByID(runCtx, repoID)
// ... ErrRepoNotFound mapping
source, storeID, storeKind, err := s.resolver.Resolve(runCtx, repo.TargetKind, repo.TargetID)
// ...
dst, err := s.destFactory(runCtx, repo)
defer dst.Close()
```

In `RunRestore`, the resolver returns the current live store (we need its `storeID`/`storeKind` only as inputs to the `manifest.store_kind == target.kind` and `manifest.store_id == target.store_id` checks in D-05 step 4). Source is NOT used as a `Backupable` in restore — we build a fresh engine instead (D-08).

**Executor delegation pattern (mirror, but to `pkg/backup/restore.Executor`)** — line 302:
```go
rec, err := s.exec.RunBackup(runCtx, source, dst, repo, storeID, storeKind)
```

Becomes:
```go
err := s.restoreExec.RunRestore(runCtx, restore.Params{
    Repo: repo, Dst: dst, Resolver: s.resolver,
    RecordID: recordID, ShareService: s.shares, StoreService: s.stores,
    BumpBootVerifier: writeHandlers.BumpBootVerifier,
})
```

**Pre-flight share-disabled check (D-05 step 1, NEW code)** — add helper method `ensureSharesDisabledForStore(ctx, storeID)`; iterate `s.shares.ListEnabledSharesForStore(storeID)`; return `ErrRestorePreconditionFailed` if non-empty.

**NEW method signature (D-21, CONTEXT line 40):**
```go
// RunRestore runs one restore attempt. recordID == nil selects the latest
// succeeded BackupRecord for the repo (D-15). Same per-repo mutex as
// RunBackup (D-07) — concurrent backup/restore on the same repo returns
// ErrBackupAlreadyRunning.
func (s *Service) RunRestore(ctx context.Context, repoID string, recordID *string) error
```

---

### 2. `pkg/controlplane/runtime/storebackups/errors.go` — append Phase-5 sentinels

**Analog:** same file, lines 1-15.

**Existing pattern** (1-15):
```go
package storebackups
import "github.com/marmos91/dittofs/pkg/controlplane/models"

var (
    ErrScheduleInvalid      = models.ErrScheduleInvalid
    ErrRepoNotFound         = models.ErrRepoNotFound
    ErrBackupAlreadyRunning = models.ErrBackupAlreadyRunning
    ErrInvalidTargetKind    = models.ErrInvalidTargetKind
)
```

**Append (D-26):**
```go
// Phase-5 restore sentinels. Two-layer wrap: runtime layer (here) +
// shares layer (pkg/controlplane/runtime/shares/errors.go).
var (
    ErrRestorePreconditionFailed = errors.New("restore precondition failed: one or more shares still enabled")
    ErrNoRestoreCandidate        = errors.New("no succeeded backup record available to restore")
    ErrStoreIDMismatch           = errors.New("manifest store_id does not match target store")
    ErrStoreKindMismatch         = errors.New("manifest store_kind does not match target engine")
    ErrRecordNotRestorable       = errors.New("backup record status is not succeeded; not restorable")
    ErrRecordRepoMismatch        = errors.New("backup record belongs to a different repo")
    ErrManifestVersionUnsupported = errors.New("manifest version not supported by this binary")
)
```

Keep using `errors.New` (not `fmt.Errorf`) per CONTEXT "Error sentinels are `errors.New`, not `fmt.Errorf`".

---

### 3. `pkg/controlplane/runtime/storebackups/backup_hold.go` — NEW

**Analog:** `pkg/backup/executor/executor.go::payloadIDSetToSlice` (sort + dedup pattern) + Phase-3 `destinationFactoryFromRepo` composition.

**Purpose (D-11, D-12, D-13):** implement `gc.BackupHoldProvider`. For every succeeded record across every repo, `GetManifestOnly(ctx, id)` → union PayloadIDSets → return `map[PayloadID]struct{}`.

**Imports pattern (mirror `service.go`):**
```go
package storebackups

import (
    "context"
    "github.com/marmos91/dittofs/internal/logger"
    "github.com/marmos91/dittofs/pkg/backup/destination"
    "github.com/marmos91/dittofs/pkg/blockstore/gc"
    "github.com/marmos91/dittofs/pkg/controlplane/store"
    "github.com/marmos91/dittofs/pkg/metadata"
)
```

**Composition (D-11):**
```go
type BackupHold struct {
    store       store.BackupStore
    destFactory DestinationFactoryFn  // reuse Service's existing type
}

// HeldPayloadIDs implements gc.BackupHoldProvider.
func (h *BackupHold) HeldPayloadIDs(ctx context.Context) (map[metadata.PayloadID]struct{}, error) {
    repos, err := h.store.ListAllBackupRepos(ctx)
    if err != nil { return nil, err }

    out := make(map[metadata.PayloadID]struct{})
    for _, repo := range repos {
        dst, err := h.destFactory(ctx, repo)
        if err != nil {
            logger.Warn("BackupHold: skip repo on destFactory error",
                "repo_id", repo.ID, "error", err)
            continue
        }
        records, err := h.store.ListSucceededRecordsByRepo(ctx, repo.ID)
        if err != nil {
            _ = dst.Close()
            logger.Warn("BackupHold: skip repo on list error",
                "repo_id", repo.ID, "error", err)
            continue
        }
        for _, rec := range records {
            m, err := dst.GetManifestOnly(ctx, rec.ID)
            if err != nil {
                logger.Warn("BackupHold: skip record on manifest fetch error",
                    "repo_id", repo.ID, "record_id", rec.ID, "error", err)
                continue
            }
            for _, pid := range m.PayloadIDSet {
                out[metadata.PayloadID(pid)] = struct{}{}
            }
        }
        _ = dst.Close()
    }
    return out, nil
}
```

**Error-handling policy (mirror D-13 retention continue-on-error):** log WARN and skip per-repo / per-record on destination errors; never fail the whole hold (better to under-hold slightly than to fail GC).

**Compile-time check:**
```go
var _ gc.BackupHoldProvider = (*BackupHold)(nil)
```

---

### 4. `pkg/controlplane/runtime/shares/service.go` — `DisableShare` / `EnableShare` / `IsShareEnabled` / `ListEnabledSharesForStore`

**Analog:** `AddShare` (lines 230-295), `RemoveShare` (lines 567-592), `UpdateShare` (lines 594-631), and `notifyShareChange` (lines 692-707) in the SAME file.

**Why this is the primary analog (D-22):** `DisableShare` needs DB-first-then-runtime (line 276 `RegisterStoreForShare` precedent), map-write under `s.mu.Lock` (line 283), and `notifyShareChange()` as the final step (line 292).

**Add `Enabled bool` field to the runtime `Share` struct** (CONTEXT D-22 line 438):

Currently lines 29-67 of the Share struct. Insert alongside `ReadOnly`:
```go
type Share struct {
    Name          string
    MetadataStore string
    RootHandle    metadata.FileHandle
    ReadOnly      bool
    Enabled       bool  // NEW — D-01. Default true when populated from DB.
    // ... rest unchanged
}
```

Also add to `ShareConfig` (lines 70-108) so callers pass the loaded-from-DB `Enabled` value on `AddShare`:
```go
type ShareConfig struct {
    // ... existing fields ...
    Enabled bool  // NEW — when absent from DB (pre-migration rows), defaults to true via GORM tag
}
```

**New method — `DisableShare` (D-02, D-03, D-22):**

Mirror the `UpdateShare` lock + lookup prologue (lines 595-601):
```go
func (s *Service) DisableShare(ctx context.Context, store ShareStore, name string) error {
    // DB-first (D-22): persist enabled=false before touching the runtime
    // so a crash mid-disable is crash-consistent (operator re-runs cleanly).
    dbShare, err := store.GetShare(ctx, name)
    if err != nil { return err }
    if !dbShare.Enabled {
        return ErrShareAlreadyDisabled
    }
    dbShare.Enabled = false
    if err := store.UpdateShare(ctx, dbShare); err != nil {
        return fmt.Errorf("persist disabled state: %w", err)
    }

    // Then flip the runtime flag.
    s.mu.Lock()
    share, exists := s.registry[name]
    if !exists {
        s.mu.Unlock()
        return fmt.Errorf("share %q not found in runtime", name)
    }
    share.Enabled = false
    s.mu.Unlock()

    // notifyShareChange() kicks adapter callbacks (D-02 disconnect path).
    // Adapters consult Share.Enabled on each request — existing sessions
    // tear down as their next request sees Enabled=false.
    s.notifyShareChange()
    return nil
}
```

**Synchronous wait (D-03):** `notifyShareChange` today invokes callbacks serially under no lock (lines 703-706). That's already synchronous — no new plumbing needed; adapters that need to drop connections do so inside their callback and `DisableShare` returns after every callback returns.

**Timeout bound (D-03, Discretion line 502):** D-03 reuses `lifecycle.ShutdownTimeout` (default 30s). Simplest impl: pass the timeout on the `ctx` from the caller (Phase 5 `RunRestore` builds `ctx` with `context.WithTimeout(parent, lifecycle.ShutdownTimeout())`). No new parameter on `DisableShare`.

**`EnableShare` — symmetric inversion:**
```go
func (s *Service) EnableShare(ctx context.Context, store ShareStore, name string) error {
    dbShare, err := store.GetShare(ctx, name)
    if err != nil { return err }
    dbShare.Enabled = true
    if err := store.UpdateShare(ctx, dbShare); err != nil { return err }

    s.mu.Lock()
    share, exists := s.registry[name]
    if !exists { s.mu.Unlock(); return fmt.Errorf("share %q not found", name) }
    share.Enabled = true
    s.mu.Unlock()

    s.notifyShareChange()
    return nil
}
```

**`IsShareEnabled` — read path (mirror `GetShare`, lines 633-642):**
```go
func (s *Service) IsShareEnabled(name string) (bool, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    share, exists := s.registry[name]
    if !exists { return false, fmt.Errorf("share %q not found", name) }
    return share.Enabled, nil
}
```

**`ListEnabledSharesForStore` — for RunRestore pre-flight gate (D-05 step 1):**
Scan `s.registry`; return `[]string` of share names where `share.Enabled && share.MetadataStore == storeName`. Mirror `ListShares` (lines 655-664). The caller (`RunRestore`) resolves the metadata store NAME from the (target_kind, target_id) polymorphic key and passes the name.

```go
func (s *Service) ListEnabledSharesForStore(metadataStoreName string) []string {
    s.mu.RLock()
    defer s.mu.RUnlock()
    var out []string
    for name, share := range s.registry {
        if share.Enabled && share.MetadataStore == metadataStoreName {
            out = append(out, name)
        }
    }
    return out
}
```

**Population on AddShare (D-04 persisted not ephemeral):** inside `prepareShare` (line 301), when constructing the `Share` struct (line 355), copy `Enabled` from `config.Enabled`. When Phase 5 wires `AddShare` callers (outside this file), callers read `models.Share.Enabled` from the DB and pass it through `ShareConfig.Enabled`.

---

### 5. `pkg/controlplane/runtime/shares/errors.go` — NEW

**Analog:** `pkg/controlplane/runtime/storebackups/errors.go` (same 15-line shape).

```go
// Package shares provides typed errors for share lifecycle operations.
package shares

import "errors"

var (
    ErrShareAlreadyDisabled = errors.New("share is already disabled")
    ErrShareStillInUse      = errors.New("share still has active mounts after disable timeout")
    // ErrShareNotFound is re-exported from models for convenience.
)
```

Do NOT re-declare `ErrShareNotFound` — mirror the storebackups pattern of re-exporting the `models` identity so `errors.Is` matches across package boundaries:
```go
var ErrShareNotFound = models.ErrShareNotFound
```

---

### 6. `pkg/controlplane/runtime/stores/service.go` — `SwapMetadataStore` + `OpenMetadataStoreAtPath`

**Analog:** `RegisterMetadataStore` (lines 25-42) and `CloseMetadataStores` (lines 73-87) in the SAME file.

**Why this is the primary analog (D-08, D-23):** registry write happens under `s.mu.Lock()` (lines 33-34); `CloseMetadataStores` already iterates `io.Closer` implementations (lines 80-86).

**`SwapMetadataStore` (D-23) — add new method:**
```go
// SwapMetadataStore atomically replaces the registered store named `name`
// with `newStore`. Returns the displaced store so the caller (Phase 5
// restore orchestrator) can Close() it and clean up its backing path.
// Errors with "not registered" if `name` is unknown; "nil newStore"
// rejected up-front.
//
// Commit point for Phase 5's D-05 restore flow: the write lock is the
// atomic moment at which the fresh engine goes live.
func (s *Service) SwapMetadataStore(name string, newStore metadata.MetadataStore) (old metadata.MetadataStore, err error) {
    if newStore == nil {
        return nil, fmt.Errorf("cannot swap to nil metadata store")
    }
    if name == "" {
        return nil, fmt.Errorf("cannot swap metadata store with empty name")
    }
    s.mu.Lock()
    defer s.mu.Unlock()
    old, exists := s.registry[name]
    if !exists {
        return nil, fmt.Errorf("metadata store %q not registered", name)
    }
    s.registry[name] = newStore
    return old, nil
}
```

**`OpenMetadataStoreAtPath` (D-08) — add new method:**

This delegates to engine-specific constructors at a CALLER-PROVIDED path. The analog for the `switch cfg.Type` kind dispatch is `shares.CreateLocalStoreFromConfig` (shares/service.go lines 929-983, `switch storeType { case "fs": ... case "memory": ... }`). Signature:
```go
// OpenMetadataStoreAtPath constructs a fresh engine instance using the
// given MetadataStoreConfig but overrides the backing path/schema so the
// engine lands at a temporary location. Does NOT register the engine.
// Callers (Phase 5 restore) use this to open the side-engine, call
// Backupable.Restore(r) against it, and then SwapMetadataStore.
//
// pathOverride semantics per engine kind:
//   - "badger": tempDir path (filesystem directory)
//   - "postgres": temp schema name (e.g. "<original>_restore_<ulid>")
//   - "memory": pathOverride is ignored; always returns a fresh instance
//
// The caller owns cleanup of pathOverride on failure.
func (s *Service) OpenMetadataStoreAtPath(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error)
```

Implementation is kind-dispatch — mirror `shares.CreateLocalStoreFromConfig` pattern (shares/service.go 960-982). The actual constructors live in `pkg/backup/restore/fresh_store.go` (see #10) so this file stays a thin registry and doesn't pull in every engine package.

---

### 7. `pkg/controlplane/models/share.go` — add `Enabled` field

**Analog:** `ReadOnly bool` at line 26.

Existing field (line 26):
```go
ReadOnly           bool      `gorm:"default:false" json:"read_only"`
```

**Add parallel field (D-01, D-25):**
```go
Enabled            bool      `gorm:"default:true;not null" json:"enabled"`
```

Place logically near `ReadOnly` for operator readability; GORM tag `default:true;not null` is the binding contract per D-25.

No change needed in `TableName()` or anywhere else. `AutoMigrate` will add the column with the default on boot (D-25 forward-safe additive migration).

---

### 8. `pkg/controlplane/store/gorm.go` — handle migration timing

**Analog:** Phase-4 D-26 column-rename + backfill block, lines 253-279 in the SAME file.

Pattern to mirror (read lines 246-279 in the current file):
```go
// Pre-migration: rename read_cache_size column to read_buffer_size if it exists.
if db.Migrator().HasColumn(&models.Share{}, "read_cache_size") {
    if err := db.Migrator().RenameColumn(...); err != nil { ... }
}
// Pre-migration (D-26 step 1): rename backup_repos.metadata_store_id → target_id.
if db.Migrator().HasColumn(&models.BackupRepo{}, "metadata_store_id") {
    if err := db.Migrator().RenameColumn(&models.BackupRepo{}, "metadata_store_id", "target_id"); err != nil {
        return nil, fmt.Errorf("failed to rename ...: %w", err)
    }
}
// Run auto-migration
if err := db.AutoMigrate(models.AllModels()...); err != nil { ... }
// Post-migration (D-26 step 3): backfill target_kind for rows that existed
// before the column was added.
if err := db.Exec("UPDATE backup_repos SET target_kind = ? WHERE ...", "metadata").Error; err != nil { ... }
```

**Phase-5 migration (D-25) — purely additive, no pre/post steps needed:**

`AutoMigrate` picks up the new `Enabled` column via the GORM tag `default:true;not null`. SQLite and Postgres both honor `DEFAULT true NOT NULL` on `ALTER TABLE ... ADD COLUMN`. Existing rows take the default.

However, to guarantee correctness on SQLite dialects that historically left NULL on ADD COLUMN, add a post-AutoMigrate backfill mirroring the Phase-4 D-26 backfill:
```go
// Post-migration (D-25): backfill shares.enabled for rows that predate
// the column. Matches the Phase-4 target_kind backfill pattern above.
if err := db.Exec(
    "UPDATE shares SET enabled = ? WHERE enabled IS NULL",
    true,
).Error; err != nil {
    return nil, fmt.Errorf("failed to backfill shares.enabled: %w", err)
}
```

No column rename — nothing to add before `AutoMigrate`.

---

### 9. `pkg/backup/restore/restore.go` — NEW

**Analog:** `pkg/backup/executor/executor.go::Executor.RunBackup` (lines 69-271).

**Why this is the exact analog (D-21, CONTEXT line 420-434):** same state machine shape (create `BackupJob{kind=restore}` → do work → update job terminal status), same ULID allocation pattern (lines 88-89), same D-18 ctx-canceled→interrupted mapping (lines 187-191), same JobStore narrow interface idiom (lines 22-26), same progressive error aggregation pattern.

**Imports pattern (mirror executor.go):**
```go
package restore

import (
    "context"
    "errors"
    "fmt"
    "io"

    "github.com/oklog/ulid/v2"

    "github.com/marmos91/dittofs/internal/logger"
    "github.com/marmos91/dittofs/pkg/backup"
    "github.com/marmos91/dittofs/pkg/backup/destination"
    "github.com/marmos91/dittofs/pkg/backup/manifest"
    "github.com/marmos91/dittofs/pkg/controlplane/models"
)
```

**JobStore narrow interface (copy from executor.go lines 22-26):**
```go
type JobStore interface {
    CreateBackupJob(ctx context.Context, job *models.BackupJob) (string, error)
    UpdateBackupJob(ctx context.Context, job *models.BackupJob) error
    GetBackupRecordByID(ctx context.Context, id string) (*models.BackupRecord, error)
    ListSucceededRecordsByRepo(ctx context.Context, repoID string) ([]*models.BackupRecord, error)
}
```

Note: `ListSucceededRecordsByRepo` does NOT exist yet in `BackupStore`; add it as part of this phase (see File #21). It's a variant of the existing `ListSucceededRecordsForRetention` (`backup.go` lines 152-162) that INCLUDES pinned rows (pinned records are restorable).

**Executor type + constructor (copy shape from executor.go lines 28-40):**
```go
type Executor struct {
    store JobStore
    clock backup.Clock
}

func New(store JobStore, clock backup.Clock) *Executor {
    if clock == nil { clock = backup.RealClock{} }
    return &Executor{store: store, clock: clock}
}
```

**RunRestore core sequence (mirror executor.go lines 69-270; adapt to restore):**

```go
// RunRestore executes one restore attempt. Returns nil on a successful swap;
// non-nil on any failure before/during swap. Post-swap failures (close old /
// delete old backing) are logged but do not fail the restore (D-05 step 11+).
//
// recordID == nil selects the latest succeeded record (D-15); non-nil validates
// repo match + status=succeeded (D-16).
//
// Failure semantics map directly to CONTEXT D-05:
//   - steps 1-4 (pre-flight, select, validate): old store untouched, no temp
//   - step 6 (fresh engine open fails): old untouched, no temp to clean
//   - step 8 (Restore fails): old untouched, freshStore + temp cleaned via defer
//   - step 9 (SHA-256 mismatch): old untouched, clean as above
//   - step 10 (swap): commit point — atomic under stores.Service write lock
//   - step 11+ (close old / delete old): logged, job stays succeeded
func (e *Executor) RunRestore(ctx context.Context, p Params) (err error) {
    // Create BackupJob{Kind: restore, Status: running}. Mirror executor.go
    // lines 87-103 except Kind is BackupJobKindRestore.
    startedAt := e.clock.Now()
    jobID := ulid.Make().String()
    job := &models.BackupJob{
        ID:        jobID,
        Kind:      models.BackupJobKindRestore,
        RepoID:    p.Repo.ID,
        Status:    models.BackupStatusRunning,
        StartedAt: &startedAt,
    }
    if _, err := e.store.CreateBackupJob(ctx, job); err != nil {
        return fmt.Errorf("create restore job: %w", err)
    }

    // Defer a terminal-state update. Mirror executor.go lines 182-208
    // status classification (ctx cancel → interrupted; other → failed).
    defer func() {
        finishedAt := e.clock.Now()
        if err == nil {
            _ = e.store.UpdateBackupJob(ctx, &models.BackupJob{
                ID: jobID, Status: models.BackupStatusSucceeded,
                StartedAt: &startedAt, FinishedAt: &finishedAt,
                BackupRecordID: &p.RecordID,
            })
            return
        }
        status := models.BackupStatusFailed
        if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
           errors.Is(err, backup.ErrBackupAborted) {
            status = models.BackupStatusInterrupted
        }
        _ = e.store.UpdateBackupJob(ctx, &models.BackupJob{
            ID: jobID, Status: status, StartedAt: &startedAt,
            FinishedAt: &finishedAt, Error: err.Error(),
        })
    }()

    // D-05 step 3: Download manifest only.
    m, err := p.Dst.GetManifestOnly(ctx, p.RecordID)
    if err != nil { return fmt.Errorf("fetch manifest: %w", err) }

    // D-05 step 4: manifest validation gates.
    if m.ManifestVersion != manifest.CurrentVersion {
        return fmt.Errorf("%w: got %d, want %d", ErrManifestVersionUnsupported, m.ManifestVersion, manifest.CurrentVersion)
    }
    if m.StoreKind != p.TargetStoreKind {
        return fmt.Errorf("%w: manifest=%q target=%q", ErrStoreKindMismatch, m.StoreKind, p.TargetStoreKind)
    }
    if m.StoreID != p.TargetStoreID {
        return fmt.Errorf("%w: manifest=%q target=%q", ErrStoreIDMismatch, m.StoreID, p.TargetStoreID)
    }
    if m.SHA256 == "" {
        return fmt.Errorf("%w: manifest SHA-256 is empty", backup.ErrRestoreCorrupt)
    }

    // D-05 step 6: fresh engine at temp path. Delegates to fresh_store.go.
    freshStore, tempIdentity, err := OpenFreshEngineAtTemp(ctx, p.StoresService, p.TargetStoreCfg)
    if err != nil { return fmt.Errorf("open fresh engine: %w", err) }

    // Cleanup temp engine on any subsequent failure (D-05 steps 8/9 semantics).
    cleanupTemp := true
    defer func() {
        if cleanupTemp {
            if closer, ok := freshStore.(io.Closer); ok { _ = closer.Close() }
            if err := CleanupTempBacking(tempIdentity); err != nil {
                logger.Warn("restore: temp cleanup error", "error", err)
            }
        }
    }()

    // D-05 step 7: GetBackup streams plaintext+SHA-256-verify+decrypt (Phase 3 D-11).
    _, reader, err := p.Dst.GetBackup(ctx, p.RecordID)
    if err != nil { return fmt.Errorf("fetch backup payload: %w", err) }

    // D-05 step 8: freshStore.Restore — Backupable.Restore must see the
    // "destination is empty" invariant (Phase 2 D-06) which holds by
    // construction for a just-opened temp engine.
    freshBackupable, ok := freshStore.(backup.Backupable)
    if !ok {
        _ = reader.Close()
        return fmt.Errorf("%w: fresh engine %q does not implement Backupable", backup.ErrBackupUnsupported, p.TargetStoreCfg.Type)
    }
    restoreErr := freshBackupable.Restore(ctx, reader)

    // D-05 step 9: closing the reader returns ErrSHA256Mismatch on hash
    // divergence — the verify-while-streaming reader from Phase 3 D-11.
    closeErr := reader.Close()
    if restoreErr != nil { return fmt.Errorf("restore into fresh engine: %w", restoreErr) }
    if closeErr != nil { return fmt.Errorf("verify payload: %w", closeErr) }

    // D-05 step 10: atomic swap. Commit point.
    oldStore, err := p.StoresService.SwapMetadataStore(p.TargetStoreCfg.Name, freshStore)
    if err != nil {
        return fmt.Errorf("swap store: %w", err)
    }
    cleanupTemp = false  // swap succeeded; fresh engine is now live at temp

    // D-05 step 11-12: close old and rename temp → canonical. Logged, not fatal.
    if err := CommitSwap(ctx, oldStore, tempIdentity, p.TargetStoreCfg); err != nil {
        logger.Warn("restore: post-swap cleanup had errors (restore still succeeded)",
            "repo_id", p.Repo.ID, "record_id", p.RecordID, "error", err)
    }

    // D-05 step 13: bump NFSv4 boot verifier.
    if p.BumpBootVerifier != nil { p.BumpBootVerifier() }

    return nil
}
```

**Params struct (colocated — mirrors executor.RunBackup multi-param signature):**
```go
type Params struct {
    Repo             *models.BackupRepo
    Dst              destination.Destination
    RecordID         string
    TargetStoreKind  string
    TargetStoreID    string
    TargetStoreCfg   *models.MetadataStoreConfig
    StoresService    StoresService    // interface satisfied by runtime/stores.Service
    BumpBootVerifier func()
}
```

---

### 10. `pkg/backup/restore/fresh_store.go` — NEW

**Analog:** `pkg/controlplane/runtime/shares/service.go::CreateLocalStoreFromConfig` (lines 929-983) — kind-dispatch constructor pattern.

**Purpose (D-05 step 6, D-08):** per-engine "open at temp path" helper. Per CONTEXT:
- Badger: `tempDir := <store.path>.restore-<ulid>` or `<parent>/.restore/<name>-<ulid>` (Discretion allows either)
- Postgres: `CREATE SCHEMA "<current>_restore_<ulid>"`
- Memory: new `MemoryMetadataStore{}` (pathOverride ignored)

**Signature:**
```go
// TempIdentity holds information about a fresh engine's temp backing so the
// orchestrator can clean up on failure or commit (rename) on success.
type TempIdentity struct {
    Kind         string  // "badger"|"postgres"|"memory"
    OriginalPath string  // destination for post-swap rename (Badger, Postgres)
    TempPath     string  // transient location (Badger path, Postgres schema name)
    ULID         string  // for debugging + orphan sweep correlation
}

// OpenFreshEngineAtTemp opens a fresh engine instance at a temporary
// location based on cfg.Type. Does NOT register with any runtime registry.
// Caller (restore.RunRestore) owns close + cleanup on failure.
func OpenFreshEngineAtTemp(
    ctx context.Context,
    stores StoresService,
    cfg *models.MetadataStoreConfig,
) (metadata.MetadataStore, TempIdentity, error) {
    tempULID := ulid.Make().String()
    switch cfg.Type {
    case "memory":
        // pathOverride ignored for memory — just construct a fresh instance.
        store, err := stores.OpenMetadataStoreAtPath(ctx, cfg, "")
        return store, TempIdentity{Kind: "memory", ULID: tempULID}, err
    case "badger":
        raw, _ := cfg.GetConfig()
        origPath, _ := raw["path"].(string)
        tempPath := fmt.Sprintf("%s.restore-%s", origPath, tempULID)
        store, err := stores.OpenMetadataStoreAtPath(ctx, cfg, tempPath)
        return store, TempIdentity{
            Kind: "badger", OriginalPath: origPath, TempPath: tempPath, ULID: tempULID,
        }, err
    case "postgres":
        raw, _ := cfg.GetConfig()
        origSchema, _ := raw["schema"].(string)
        if origSchema == "" { origSchema = "public" }
        tempSchema := fmt.Sprintf("%s_restore_%s", origSchema, strings.ToLower(tempULID))
        store, err := stores.OpenMetadataStoreAtPath(ctx, cfg, tempSchema)
        return store, TempIdentity{
            Kind: "postgres", OriginalPath: origSchema, TempPath: tempSchema, ULID: tempULID,
        }, err
    default:
        return nil, TempIdentity{}, fmt.Errorf("unsupported store type %q", cfg.Type)
    }
}

// CleanupTempBacking removes the temp path/schema. Safe to call on a
// partially-initialized TempIdentity (no-op if fields are zero).
func CleanupTempBacking(id TempIdentity) error {
    switch id.Kind {
    case "badger":
        if id.TempPath != "" { return os.RemoveAll(id.TempPath) }
    case "postgres":
        // DROP SCHEMA "..." CASCADE — goes through the same DB connection
        // the engine uses; requires a small helper on stores.Service.
        // Plan detail: exposed via stores.Service.DropPostgresSchema(name).
    case "memory":
        // no-op — dropped reference is collected by GC
    }
    return nil
}
```

Patterns copied: `shares.CreateLocalStoreFromConfig` switch-case (line 960 `switch storeType { case "fs": ... case "memory": ...}`). The ulid.Make pattern mirrors `executor.go:89` (`recordID := ulid.Make().String()`).

---

### 11. `pkg/backup/restore/swap.go` — NEW

**Analog:** `pkg/backup/destination/fs/store.go::PutBackup` rename-publish moment (lines 293-305) + `engine.BlockStore.Close` pattern used by `shares.Service.RemoveShare` (shares/service.go lines 579-587).

**Purpose (D-05 steps 11-12):** after the atomic swap commits, close the displaced store and delete its backing path; rename temp → canonical.

```go
// CommitSwap finalizes a successful store swap:
//   1. Close old store (typically io.Closer implemented by each engine)
//   2. Delete old backing:
//      - Badger: os.RemoveAll(originalPath) — path is now free
//      - Postgres: DROP SCHEMA "original" CASCADE
//      - Memory: no-op
//   3. Rename temp → canonical:
//      - Badger: os.Rename(tempPath, originalPath)
//      - Postgres: RENAME SCHEMA "temp" TO "original"
//      - Memory: no-op (swap already points at the fresh instance)
//
// Errors here are logged but not fatal to the restore — the swap has
// committed and clients see the new data. Orphan temp paths / schemas
// are reclaimed by the D-14 startup sweep.
func CommitSwap(ctx context.Context, oldStore metadata.MetadataStore, id TempIdentity, cfg *models.MetadataStoreConfig) error {
    // Close old.
    if closer, ok := oldStore.(io.Closer); ok {
        if err := closer.Close(); err != nil {
            return fmt.Errorf("close old store: %w", err)
        }
    }

    switch id.Kind {
    case "badger":
        if err := os.RemoveAll(id.OriginalPath); err != nil {
            return fmt.Errorf("remove old badger path %q: %w", id.OriginalPath, err)
        }
        if err := os.Rename(id.TempPath, id.OriginalPath); err != nil {
            return fmt.Errorf("rename temp %q → %q: %w", id.TempPath, id.OriginalPath, err)
        }
    case "postgres":
        // Delegated to a stores.Service helper — DB-level schema ops.
        // Plan adds DropSchema + RenameSchema to stores.Service.
    case "memory":
        // no-op
    }
    return nil
}
```

**Pattern match:**
- `shares/service.go:579-587` — the "close BlockStore after removing from registry" pattern (close-after-unregister).
- `destination/fs/store.go:293-295` — `os.Rename(tmpDir, finalDir)` as the atomic commit. Restore uses `os.Rename(tempPath, originalPath)` identically.

---

### 12. `pkg/backup/restore/errors.go` — NEW

**Analog:** `pkg/backup/destination/errors.go` (Phase-3 typed sentinel registry) + `pkg/backup/backupable.go` (Phase-2 sentinel style).

Re-export storebackups sentinels for caller convenience AND add package-local ones:
```go
package restore

import (
    "errors"
    "github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
)

// Re-exports preserve errors.Is matching across package boundaries
// (mirrors storebackups/errors.go which re-exports from models).
var (
    ErrStoreIDMismatch            = storebackups.ErrStoreIDMismatch
    ErrStoreKindMismatch          = storebackups.ErrStoreKindMismatch
    ErrManifestVersionUnsupported = storebackups.ErrManifestVersionUnsupported
)

// Package-local sentinels. Restore orchestration-specific; no external
// consumer needs to match these so they live here only.
var (
    ErrRestoreAborted    = errors.New("restore aborted mid-operation")
    ErrFreshEngineExists = errors.New("temp engine path already exists")
)
```

---

### 13. `pkg/backup/destination/destination.go` — add `GetManifestOnly`

**Analog:** `Destination.GetBackup` (same file, lines 31-38).

Existing method signature (lines 31-38):
```go
// GetBackup returns the manifest and a payload reader...
GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error)
```

**Sibling (D-12) — insert before `GetBackup`:**
```go
// GetManifestOnly returns the manifest for backup id without fetching the
// payload. Used by restore pre-flight (Phase 5 D-03 manifest validation)
// and by the block-GC hold provider (Phase 5 D-11) which unions all
// retained manifests' PayloadIDSets without downloading gigabytes of
// payload.bin.
//
// Errors: ErrManifestMissing (no manifest.yaml for id — orphan / never
// published), ErrDestinationUnavailable (transient I/O).
GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error)
```

**Return type choice (Discretion line 498):** return the parsed `*manifest.Manifest` directly (not raw bytes); all callers need the parsed form, and both existing drivers already parse internally (fs: `readManifest` line 378; s3: inline at lines 479-483).

---

### 14. `pkg/backup/destination/fs/store.go` — implement `GetManifestOnly`

**Analog:** `fs.readManifest` (same file, lines 378-393) — private helper that already does what we need.

Existing code (lines 378-393):
```go
func readManifest(dir string) (*manifest.Manifest, error) {
    p := filepath.Join(dir, manifestFilename)
    f, err := os.Open(p)
    if err != nil {
        if errors.Is(err, os.ErrNotExist) {
            return nil, fmt.Errorf("%w: %s", destination.ErrManifestMissing, dir)
        }
        return nil, fmt.Errorf("%w: open %s: %v", destination.ErrDestinationUnavailable, p, err)
    }
    defer func() { _ = f.Close() }()
    m, err := manifest.ReadFrom(f)
    if err != nil {
        return nil, fmt.Errorf("%w: parse %s: %v", destination.ErrDestinationUnavailable, p, err)
    }
    return m, nil
}
```

**Add public method (wraps the helper, adds ctx check + id → dir mapping):**
```go
// GetManifestOnly implements destination.Destination. Reads <root>/<id>/manifest.yaml
// without touching payload.bin.
func (s *Store) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
    if err := ctx.Err(); err != nil { return nil, err }
    dir := filepath.Join(s.root, id)
    return readManifest(dir)
}
```

---

### 15. `pkg/backup/destination/s3/store.go` — implement `GetManifestOnly`

**Analog:** `s3.Store.GetBackup` prologue (same file, lines 467-483) — the "fetch + parse manifest" block already exists, just extract it.

Existing prologue (lines 467-483):
```go
mOut, err := s.client.GetObject(ctx, &s3.GetObjectInput{
    Bucket: aws.String(s.bucket),
    Key:    aws.String(s.manifestKey(id)),
})
if err != nil {
    if isNotFound(err) {
        return nil, nil, fmt.Errorf("%w: %s", destination.ErrManifestMissing, id)
    }
    return nil, nil, classifyS3Error(fmt.Errorf("get manifest: %w", err))
}
m, perr := manifest.ReadFrom(mOut.Body)
_ = mOut.Body.Close()
if perr != nil {
    return nil, nil, fmt.Errorf("%w: parse manifest: %v", destination.ErrDestinationUnavailable, perr)
}
```

**Extract to new method + refactor GetBackup to call it:**
```go
// GetManifestOnly implements destination.Destination. Single GetObject for
// manifest.yaml only — no payload bandwidth spent.
func (s *Store) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
    mOut, err := s.client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(s.manifestKey(id)),
    })
    if err != nil {
        if isNotFound(err) {
            return nil, fmt.Errorf("%w: %s", destination.ErrManifestMissing, id)
        }
        return nil, classifyS3Error(fmt.Errorf("get manifest: %w", err))
    }
    defer func() { _ = mOut.Body.Close() }()
    m, perr := manifest.ReadFrom(mOut.Body)
    if perr != nil {
        return nil, fmt.Errorf("%w: parse manifest: %v", destination.ErrDestinationUnavailable, perr)
    }
    return m, nil
}
```

Then `GetBackup` is refactored to call `GetManifestOnly` and continue into the payload-fetch block.

---

### 16. `pkg/blockstore/gc/gc.go` — add `BackupHoldProvider` + hold check at orphan point

**Analog:** existing `MetadataReconciler` interface (same file, lines 44-49) and the orphan-detection union (lines 136-147).

Existing union check (lines 136-147):
```go
metaStore, err := reconciler.GetMetadataStoreForShare(shareName)
if err != nil {
    logger.Debug("GC: share not found, treating as orphan", ...)
    // Fall through to delete blocks
} else {
    _, err = metaStore.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID))
    if err == nil {
        continue  // metadata reference exists — not an orphan
    }
}
// (fall through: no metadata reference, treat as orphan)
stats.OrphanFiles++
// ... delete
```

**Add new interface (next to `MetadataReconciler`, D-11):**
```go
// BackupHoldProvider returns the set of PayloadIDs that are "held" by
// retained backup manifests and must NOT be treated as orphans even if no
// live metadata references them. Implementations (pkg/controlplane/runtime/
// storebackups/backup_hold.go) compute the set at GC time by unioning
// PayloadIDSet fields from every succeeded BackupRecord's manifest.
//
// A nil BackupHoldProvider passed to CollectGarbage disables the hold
// check (pre-Phase-5 behavior / tests without backup infrastructure).
type BackupHoldProvider interface {
    HeldPayloadIDs(ctx context.Context) (map[metadata.PayloadID]struct{}, error)
}
```

**Extend Options (lines 27-42) with hold provider:**
```go
type Options struct {
    SharePrefix        string
    DryRun             bool
    MaxOrphansPerShare int
    ProgressCallback   func(stats Stats)
    // NEW D-11: BackupHold is consulted before marking a payload an orphan.
    // nil → no hold check (backward compatible).
    BackupHold BackupHoldProvider
}
```

**Modify the union at lines 136-147 (D-13 applies to orphan-block path only):**
```go
// Existing check: metadata says "not orphan"
if metaStore, err := reconciler.GetMetadataStoreForShare(shareName); err == nil {
    if _, err := metaStore.GetFileByPayloadID(ctx, metadata.PayloadID(payloadID)); err == nil {
        continue  // metadata reference — not an orphan
    }
}

// NEW D-11: backup hold says "hold for DR"
if options.BackupHold != nil {
    if held, err := options.BackupHold.HeldPayloadIDs(ctx); err != nil {
        logger.Warn("GC: backup hold provider failed, skipping hold check",
            "error", err)
    } else if _, isHeld := held[metadata.PayloadID(payloadID)]; isHeld {
        logger.Info("GC: holding orphan for backup",
            "payloadID", payloadID)
        continue
    }
}

// Orphan path unchanged.
stats.OrphanFiles++
```

**Performance note:** `HeldPayloadIDs` is called once per GC run (not per block), so the O(repos × records) cost amortizes. If benchmarks show it's hot, cache the set at the top of `CollectGarbage` — but YAGNI for v0.13.0.

---

### 17. `internal/adapter/nfs/v4/handlers/write.go` — hoist `serverBootVerifier` to atomic

**Analog:** existing `init()` assignment (same file, lines 17-24).

Existing code (lines 17-24):
```go
// serverBootVerifier is an 8-byte verifier derived from server boot time.
var serverBootVerifier [8]byte

func init() {
    binary.BigEndian.PutUint64(serverBootVerifier[:], uint64(time.Now().UnixNano()))
}
```

**Refactor (D-09, D-10):**
```go
// serverBootVerifier is an 8-byte verifier. Initialized to server boot time;
// can be bumped on restore completion (Phase 5 D-09) to invalidate client
// caches that survived the disable/reenable cycle.
var serverBootVerifier atomic.Pointer[[8]byte]

func init() {
    var v [8]byte
    binary.BigEndian.PutUint64(v[:], uint64(time.Now().UnixNano()))
    serverBootVerifier.Store(&v)
}

// BumpBootVerifier replaces the current verifier with a fresh time-derived
// value. Phase 5 restore path calls this after a successful swap (D-09):
// NFSv4 clients whose next request lands post-swap see a new verifier and
// are forced through the reclaim-grace path with NFS4ERR_RECLAIM_BAD.
func BumpBootVerifier() {
    var v [8]byte
    binary.BigEndian.PutUint64(v[:], uint64(time.Now().UnixNano()))
    serverBootVerifier.Store(&v)
}

// bootVerifierBytes returns the current 8-byte verifier. Internal accessor.
func bootVerifierBytes() [8]byte { return *serverBootVerifier.Load() }
```

**Update call sites:**
- `write.go:243` — `buf.Write(serverBootVerifier[:])` becomes `v := bootVerifierBytes(); buf.Write(v[:])`
- `commit.go:161` — same change
- `io_test.go:976,1100` — test asserts become `bytes.Equal(verf, bootVerifierBytes()[:])`

---

### 18. `internal/adapter/nfs/mount/handlers/mount.go` + `internal/adapter/smb/v2/handlers/tree_connect.go` — consult `Share.Enabled`

**Analog:** existing mount/tree_connect paths that already read `Share` from registry.

**NFS mount (mount.go, around line 68-130):** the handler currently looks up the share and returns root handle. Add an `if !share.Enabled { return MountErrAccess }` check BEFORE the root-handle return. Constant `MountErrAccess = 13` already exists (constants.go line 310).

**NFSv4 PUTFH path:** similar — wherever the handler resolves the share name from a handle, check `share.Enabled` and return `NFS4ERR_STALE` on false. Mirror the existing `RequireCurrentFH` (write.go line 33) error-return pattern.

**SMB TREE_CONNECT:** locate `handleTreeConnect`; add the enabled check; return `STATUS_NETWORK_NAME_DELETED`.

**Canonical adapter check shape (mirror NFS write.go:33 pseudo-FS check at line 42-48):**
```go
if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
    return &types.CompoundResult{
        Status: types.NFS4ERR_ROFS,
        OpCode: types.OP_WRITE,
        Data:   encodeStatusOnly(types.NFS4ERR_ROFS),
    }
}
```

Becomes:
```go
share, err := h.Runtime.Shares().GetShare(shareName)
if err == nil && !share.Enabled {
    return &types.CompoundResult{
        Status: types.NFS4ERR_STALE,
        OpCode: ...,
        Data:   encodeStatusOnly(types.NFS4ERR_STALE),
    }
}
```

---

### 19. `pkg/metadata/store/{memory,badger,postgres}/backup.go` + first-open hook — persistent `store_id`

**D-06 gap analysis (confirmed read of existing code):** The current `storebackups.Service.RunBackup` flow threads `storeID = cfg.ID` (target.go lines 98-119: `return src, cfg.ID, cfg.Type, nil`). `cfg.ID` is the control-plane DB's `MetadataStoreConfig.ID` — not a per-engine persistent UUID.

This means **if the control-plane DB is recreated (fresh SQLite, operator-triggered reset), the same physical Badger directory will get a NEW `cfg.ID`** — and restore into it from an old backup would pass D-06's "store_id gate" check in an unsafe way (or fail when it should succeed).

Phase 5 D-06 fixes this by making each engine persist its OWN stable `store_id` and surfacing it.

**Per-engine bootstrap logic (CONTEXT lines 233-245):**
- **Badger:** new `cfg:store_id` key; set on first open if absent.
- **Postgres:** add `store_id` column to `server_config` row; set on first open if NULL.
- **Memory:** assign on construction (`MemoryMetadataStore{storeID: ulid.Make()}`).

**Analogs to mirror for bootstrap-once-at-first-open:**

For Badger — the analog is `badger/store.go` (the constructor that runs `initUsedBytesCounter` once). Add `ensureStoreID(s.db) → (storeID, error)`:
```go
func ensureStoreID(db *badgerdb.DB) (string, error) {
    var existing string
    err := db.View(func(txn *badgerdb.Txn) error {
        item, err := txn.Get([]byte("cfg:store_id"))
        if errors.Is(err, badgerdb.ErrKeyNotFound) { return nil }
        if err != nil { return err }
        return item.Value(func(v []byte) error { existing = string(v); return nil })
    })
    if err != nil { return "", err }
    if existing != "" { return existing, nil }
    fresh := ulid.Make().String()
    err = db.Update(func(txn *badgerdb.Txn) error {
        return txn.Set([]byte("cfg:store_id"), []byte(fresh))
    })
    return fresh, err
}
```

For Postgres — the analog is the `server_config` singleton (already used by `server.go` to store per-store config). Add a `store_id` column and a bootstrap migration step that inserts a fresh ULID when NULL/missing.

For Memory — trivial:
```go
type MemoryMetadataStore struct {
    // ... existing ...
    storeID string  // NEW
}

func New() *MemoryMetadataStore {
    return &MemoryMetadataStore{
        // ... existing init ...
        storeID: ulid.Make().String(),
    }
}
```

**Expose `StoreID()` on each engine:**
```go
// GetStoreID returns the engine-persistent store identifier. Used by
// Phase 5 restore to verify manifest.store_id matches the target store
// (D-06 cross-store contamination gate / Pitfall #4).
func (s *BadgerMetadataStore) GetStoreID() string { return s.storeID }
```

**Wire into `storebackups.target.go` DefaultResolver (line 118) so manifest.StoreID is the engine-persistent ID, NOT `cfg.ID`:**
```go
// CURRENT:
return src, cfg.ID, cfg.Type, nil

// REPLACE WITH:
storeIDer, ok := metaStore.(interface{ GetStoreID() string })
if !ok {
    return nil, "", "", fmt.Errorf("store %q (type=%s) does not expose GetStoreID",
        cfg.Name, cfg.Type)
}
return src, storeIDer.GetStoreID(), cfg.Type, nil
```

This is the ONLY change needed to `target.go` — the existing `storeID` string in the manifest becomes engine-persistent rather than control-plane-DB-persistent.

**Compile-time assertion per engine:**
```go
var _ interface{ GetStoreID() string } = (*BadgerMetadataStore)(nil)
// similar for memory/postgres
```

---

### 20. Tests: extend `pkg/metadata/storetest/backup_conformance.go`

**Analog:** Phase-2 D-08 conformance suite in `pkg/metadata/storetest/` — existing tests for round-trip, corruption, empty-destination rejection.

**New conformance test (D-06 verification):**
```go
// TestStoreID_PersistedAcrossRestart verifies that each engine's store_id
// is stable across reopen (Phase 5 D-06). Engines that return different
// IDs after close+reopen fail the test loudly.
func TestStoreID_PersistedAcrossRestart(t *testing.T, newStore NewStoreFn) {
    s1 := newStore(t)
    id1 := s1.(interface{ GetStoreID() string }).GetStoreID()
    require.NotEmpty(t, id1)
    require.NoError(t, s1.(io.Closer).Close())

    s2 := newStore(t)
    id2 := s2.(interface{ GetStoreID() string }).GetStoreID()
    require.Equal(t, id1, id2, "store_id must persist across restart")
}
```

Memory engine is exempt from the "across restart" clause since it's ephemeral by design — but the interface contract still applies (construction always yields a non-empty ID).

---

### 21. `pkg/controlplane/store/backup.go` — add `ListSucceededRecordsByRepo`

**Analog:** `ListSucceededRecordsForRetention` (same file, lines 152-162).

Existing (retention-specific, skips pinned per D-10 of Phase 4):
```go
func (s *GORMStore) ListSucceededRecordsForRetention(ctx context.Context, repoID string) ([]*models.BackupRecord, error) {
    var results []*models.BackupRecord
    if err := s.db.WithContext(ctx).
        Where("repo_id = ? AND status = ? AND pinned = ?",
            repoID, models.BackupStatusSucceeded, false).
        Order("created_at ASC").
        Find(&results).Error; err != nil {
        return nil, err
    }
    return results, nil
}
```

**New variant (D-15 restore selection — pinned records ARE restorable):**
```go
// ListSucceededRecordsByRepo returns all succeeded records for a repo
// (INCLUDING pinned), sorted newest-first. Used by Phase 5 restore to
// select the latest-successful candidate (D-15) and by the block-GC
// hold provider (D-11) to union all retained-manifest PayloadIDSets.
//
// Contrast with ListSucceededRecordsForRetention which excludes pinned
// (D-10) and sorts oldest-first (retention prunes from the tail).
func (s *GORMStore) ListSucceededRecordsByRepo(ctx context.Context, repoID string) ([]*models.BackupRecord, error) {
    var results []*models.BackupRecord
    if err := s.db.WithContext(ctx).
        Where("repo_id = ? AND status = ?",
            repoID, models.BackupStatusSucceeded).
        Order("created_at DESC").
        Find(&results).Error; err != nil {
        return nil, err
    }
    return results, nil
}
```

Also add to `BackupStore` interface (`pkg/controlplane/store/interface.go` ~line 396 in the existing `ListSucceededRecordsForRetention` block).

---

## Shared Patterns

### DB-first-then-runtime

**Source:** `pkg/controlplane/runtime/shares/service.go:276-290` (`AddShare`: `RegisterStoreForShare` THEN `registry[...] = share`).
**Apply to:** `DisableShare`, `EnableShare`.
**Rationale:** a crash between DB commit and runtime flip leaves the DB as source of truth; the next boot reconciles runtime from DB.

### Overlap guard shared across backup/restore

**Source:** `pkg/backup/scheduler/overlap.go` + `storebackups/service.go:266-270`.
**Apply to:** Phase-5 `RunRestore`.
```go
unlock, acquired := s.overlap.TryLock(repoID)
if !acquired { return ..., ErrBackupAlreadyRunning }
defer unlock()
```
**Rationale (D-07):** Same mutex → physically-incoherent concurrent backup+restore on the same repo is machine-rejected.

### ctx derivation for shutdown cancellation

**Source:** `pkg/controlplane/runtime/storebackups/service.go:346-369` (`deriveRunCtx`).
**Apply to:** `RunRestore` — invoke `s.deriveRunCtx(ctx)` verbatim.
**Rationale (D-17):** SIGTERM → serveCtx cancel → restore's GetBackup reader returns ctx.Err() → freshStore.Restore aborts → defer cleans temp → job marked `interrupted` via the same defer pattern.

### JobKind-aware interrupted-job recovery

**Source:** `pkg/controlplane/store/backup.go:270-285` (`RecoverInterruptedJobs` already updates ANY `status=running` row regardless of `kind`).
**Apply to:** nothing — Phase-5 adds zero new code here (CONTEXT line 48-54). Restore-kind jobs recovered by the same path at the same Serve-time invocation.

### Typed sentinels with `errors.New` + `%w` wrap

**Source:** `pkg/backup/backupable.go:62-92` (Phase 2), `pkg/backup/destination/errors.go` (Phase 3), `pkg/controlplane/runtime/storebackups/errors.go` (Phase 4).
**Apply to:** Phase-5 `errors.go` files (runtime + shares + restore package).

### Narrow JobStore interface

**Source:** `pkg/backup/executor/executor.go:22-26`.
**Apply to:** `pkg/backup/restore/restore.go` Executor — local `JobStore` interface with just the 3-4 methods it needs, keeps test fakes trivial.

### Clock injection for testable time

**Source:** `pkg/backup/executor/executor.go:31-50` (`backup.Clock`, `backup.RealClock{}`, `SetClock`).
**Apply to:** `pkg/backup/restore/restore.go::Executor`.

### Kind-dispatch constructor

**Source:** `pkg/controlplane/runtime/shares/service.go:929-983` (`CreateLocalStoreFromConfig`, `switch storeType { case "fs": ... }`).
**Apply to:** `pkg/backup/restore/fresh_store.go::OpenFreshEngineAtTemp` (switch on `cfg.Type`).

### GORM migration — additive ADD COLUMN with default

**Source:** `pkg/controlplane/store/gorm.go:253-279` (Phase-4 D-26 backfill).
**Apply to:** Phase-5 shares.enabled column. `AutoMigrate` adds the column via tag; add a post-AutoMigrate backfill for safety on SQLite dialects.

### compile-time interface assertions

**Source:** `pkg/backup/destination/fs/store.go:33` (`var _ destination.Destination = (*Store)(nil)`), `pkg/metadata/store/memory/backup.go:120` (`var _ metadata.Backupable = (*MemoryMetadataStore)(nil)`), `storebackups/target.go:123-126`.
**Apply to:** all new types implementing Phase-5 interfaces — `BackupHold → gc.BackupHoldProvider`, each engine → `interface{ GetStoreID() string }`.

### Log-warn-continue for sub-operations that shouldn't fail the parent

**Source:** `pkg/controlplane/runtime/storebackups/service.go:310-316` (retention errors don't degrade job status — D-15).
**Apply to:** Phase-5 D-05 steps 11-12 (close old / delete old / rename temp): log WARN, do not fail the restore.

---

## No Analog Found

All 21 files have in-repo analogs. No Phase-5 file is entirely greenfield — every new package borrows structure from Phase-2/3/4 siblings, every modified file extends an existing method-group or sentinel-list.

---

## Metadata

**Analog search scope:** `pkg/controlplane/runtime/` (all sub-services), `pkg/backup/` (all sub-packages), `pkg/controlplane/store/`, `pkg/controlplane/models/`, `pkg/metadata/store/{memory,badger,postgres}/`, `pkg/blockstore/gc/`, `internal/adapter/nfs/v4/handlers/`, `internal/adapter/nfs/mount/handlers/`, `internal/adapter/smb/v2/handlers/`.

**Files scanned (full read):** 18 source files plus the four CONTEXT markdown documents.

**Pattern extraction date:** 2026-04-16.

*Phase: 05-restore-orchestration-safety-rails*

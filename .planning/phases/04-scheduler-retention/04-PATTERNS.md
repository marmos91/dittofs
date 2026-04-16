# Phase 4: Scheduler + Retention - Pattern Map

**Mapped:** 2026-04-16
**Files analyzed:** 13 new + modified
**Analogs found:** 12 / 13

## File Classification

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|-------------------|------|-----------|----------------|---------------|
| `pkg/backup/backupable.go` (NEW — MOVED from `pkg/metadata/backup.go`) | import-move / interface | none (type decls) | `pkg/metadata/backup.go` | exact (same file, new path) |
| `pkg/backup/scheduler/scheduler.go` (NEW pkg) | scheduler-primitives | event-driven (cron ticks) | `pkg/controlplane/runtime/settings_watcher.go` (polling antipattern — invert) + `pkg/blockstore/gc/` (background loop) | partial |
| `pkg/backup/scheduler/doc.go` | doc | — | `pkg/controlplane/runtime/adapters/doc.go` | exact |
| `pkg/backup/scheduler/scheduler_test.go` | test | — | `pkg/controlplane/runtime/shares/healthcheck_test.go` (table-driven) | good |
| `pkg/backup/executor/executor.go` (NEW pkg) | executor / orchestrator | pipeline (stream in → stream out) | `pkg/metadata/store/memory/backup.go` (Backup flow) + destination driver consumer sketch | partial |
| `pkg/backup/executor/executor_test.go` | test | — | `pkg/metadata/store/memory/backup_test.go` | good |
| `pkg/controlplane/runtime/storebackups/service.go` (NEW sub-service) | new-subservice | CRUD + lifecycle | `pkg/controlplane/runtime/adapters/service.go` | exact (D-22 mirror) |
| `pkg/controlplane/runtime/storebackups/doc.go` | doc | — | `pkg/controlplane/runtime/adapters/doc.go` | exact |
| `pkg/controlplane/runtime/storebackups/service_test.go` | test | — | `pkg/controlplane/runtime/init_test.go` (+ `shares/healthcheck_test.go`) | good |
| `pkg/controlplane/models/backup.go` (MODIFIED — rename columns D-26) | model-change | GORM schema | existing same file (rename MetadataStoreID → TargetID, add TargetKind) | exact |
| `pkg/controlplane/store/backup.go` (MODIFIED — rename list method, add retention-query) | store-method-change | CRUD | existing same file | exact |
| `pkg/controlplane/store/gorm.go` (MODIFIED — add migration) | migration | schema-migration | `gorm.go:211–251` (existing pre-migrate rename patterns) | exact |
| `pkg/metadata/store/{memory,badger,postgres}/backup.go` (MODIFIED — import-only) | import-move | — | existing same files | trivial (`pkg/metadata` → `pkg/backup` only) |

---

## Pattern Assignments

### `pkg/backup/backupable.go` (interface, import-move per D-27)

**Analog:** `pkg/metadata/backup.go` (identical file, moved).

Move verbatim. **No signature change.** Package declaration changes from `package metadata` to `package backup`. The sentinel errors (`ErrBackupUnsupported`, `ErrRestoreDestinationNotEmpty`, `ErrRestoreCorrupt`, `ErrSchemaVersionMismatch`, `ErrBackupAborted`) move with the interface (kept in one top-level file for discoverability per D-27).

**Pattern to copy exactly** (from `pkg/metadata/backup.go:26–92`):

```go
// Backupable is the capability interface opted into by metadata stores ...
type Backupable interface {
    Backup(ctx context.Context, w io.Writer) (PayloadIDSet, error)
    Restore(ctx context.Context, r io.Reader) error
}

type PayloadIDSet map[string]struct{}

func NewPayloadIDSet() PayloadIDSet           { return make(PayloadIDSet) }
func (s PayloadIDSet) Add(id string)          { s[id] = struct{}{} }
func (s PayloadIDSet) Contains(id string) bool { _, ok := s[id]; return ok }
func (s PayloadIDSet) Len() int               { return len(s) }

var ErrBackupUnsupported         = errors.New("backup not supported by this metadata store")
var ErrRestoreDestinationNotEmpty = errors.New("restore destination is not empty")
var ErrRestoreCorrupt            = errors.New("restore stream is corrupt")
var ErrSchemaVersionMismatch     = errors.New("restore archive schema version mismatch")
var ErrBackupAborted             = errors.New("backup aborted")
```

**Compat shim (optional — planner decides):** keep `pkg/metadata/backup.go` as a thin shim that re-exports:

```go
// pkg/metadata/backup.go (post-move)
package metadata

import "github.com/marmos91/dittofs/pkg/backup"

type Backupable   = backup.Backupable
type PayloadIDSet = backup.PayloadIDSet

var (
    NewPayloadIDSet              = backup.NewPayloadIDSet
    ErrBackupUnsupported         = backup.ErrBackupUnsupported
    ErrRestoreDestinationNotEmpty = backup.ErrRestoreDestinationNotEmpty
    ErrRestoreCorrupt            = backup.ErrRestoreCorrupt
    ErrSchemaVersionMismatch     = backup.ErrSchemaVersionMismatch
    ErrBackupAborted             = backup.ErrBackupAborted
)
```

Avoids touching every Phase-2 engine file. D-27 says "All existing importers … update their import path" — either approach satisfies the decision; the shim is the lower-risk default.

---

### `pkg/backup/scheduler/scheduler.go` (scheduler-primitives, event-driven)

**No direct analog.** This is a new package of generic primitives. Closest precedents:

- `pkg/controlplane/runtime/settings_watcher.go:30–42` — what **NOT** to do. It polls on a `DefaultPollInterval = 10 * time.Second`. D-22 explicitly rejected this pattern for hot-reload: scheduler uses explicit `RegisterRepo`/`UnregisterRepo` API, not polling.
- `pkg/controlplane/runtime/adapters/service.go:326–353` — the `registerAndRunAdapterLocked` goroutine-per-entity pattern matches one option for scheduler internals (one goroutine per repo).

**Primitives to ship (store-agnostic — takes an abstract `Target` interface):**

| Primitive | Signature | Source |
|-----------|-----------|--------|
| Cron parser | `cron.ParseStandard(expr)` or `cron.New(cron.WithParser(...))` | `robfig/cron/v3` (new dep) |
| Jitter | `func phaseOffset(repoID string, max time.Duration) time.Duration` | `hash/fnv` stdlib (D-03: `fnv64a(repo_id) % max_jitter_seconds`) |
| Overlap mutex | `sync.Map[string]*sync.Mutex` + `tryLock` | D-07 |
| Schedule validator | `func ValidateSchedule(expr string) error` | Phase 6 CLI + Phase 4 Serve-time skip-with-WARN (D-06) |

**Jitter pattern (copy verbatim from D-03):**

```go
// pkg/backup/scheduler/jitter.go
import "hash/fnv"

// phaseOffset returns a stable per-repo time offset within [0, max).
// Same repoID always returns the same offset — operator-debuggable, survives restart.
func phaseOffset(repoID string, max time.Duration) time.Duration {
    if max <= 0 {
        return 0
    }
    h := fnv.New64a()
    _, _ = h.Write([]byte(repoID))
    return time.Duration(h.Sum64() % uint64(max/time.Second)) * time.Second
}
```

**Overlap guard pattern (D-07):**

```go
// pkg/backup/scheduler/overlap.go
type overlapGuard struct {
    mu sync.Map // repoID -> *sync.Mutex
}

func (g *overlapGuard) tryLock(repoID string) (unlock func(), acquired bool) {
    m, _ := g.mu.LoadOrStore(repoID, &sync.Mutex{})
    mu := m.(*sync.Mutex)
    if !mu.TryLock() {
        return nil, false
    }
    return mu.Unlock, true
}
```

Same-file mutex-per-key pattern appears in `pkg/adapter/nfs/connection.go` and `pkg/adapter/nfs/adapter.go` (both use `sync.Map` for connection tracking). Mirror that convention.

**Default jitter window (D-04):** `const DefaultMaxJitter = 5 * time.Minute` as a package-level constant (matches `adapters.DefaultShutdownTimeout = 30 * time.Second` at `pkg/controlplane/runtime/adapters/service.go:17`).

---

### `pkg/backup/executor/executor.go` (executor, pipeline)

**No direct analog.** The executor is the new seam that composes `Backupable` + `Destination` + `BackupStore` CRUD. Closest precedents:

- Existing `Backupable.Backup(ctx, w)` callers in Phase 2 tests (`pkg/metadata/store/memory/backup_test.go`) — the producer side.
- `pkg/backup/destination/destination.go:26–64` — the consumer side (`PutBackup`, `Delete`, `List`).

**Expected shape (derived from D-21 sequence):**

```go
// pkg/backup/executor/executor.go
package executor

import (
    "context"
    "io"

    "github.com/marmos91/dittofs/pkg/backup"
    "github.com/marmos91/dittofs/pkg/backup/destination"
    "github.com/marmos91/dittofs/pkg/backup/manifest"
    "github.com/marmos91/dittofs/pkg/controlplane/models"
    "github.com/oklog/ulid/v2"
)

// JobStore is the narrow interface the executor needs for BackupJob + BackupRecord CRUD.
// Take the narrowest interface (pattern from pkg/controlplane/store/interface.go:349
// BackupStore sub-interface — this is a further narrowing for testability).
type JobStore interface {
    CreateBackupJob(ctx context.Context, job *models.BackupJob) (string, error)
    UpdateBackupJob(ctx context.Context, job *models.BackupJob) error
    CreateBackupRecord(ctx context.Context, rec *models.BackupRecord) (string, error)
}

type Executor struct {
    store JobStore
    now   func() time.Time // testability (D-Claude's Discretion: clock.Clock optional)
}

// RunBackup executes one backup attempt for the given target/repo/destination.
// Sequence matches D-21:
//  1. Allocate recordID (ULID)
//  2. Create BackupJob row (status=running)
//  3. Build Manifest{ BackupID: recordID, StoreID, Encryption, ... }
//  4. Pipe: source.Backup(ctx, pipeW) → destination.PutBackup(ctx, manifest, pipeR)
//  5. On success: create BackupRecord, update BackupJob(succeeded, BackupRecordID=&recordID)
//  6. On error: update BackupJob(failed|interrupted, error=err.Error())
func (e *Executor) RunBackup(
    ctx context.Context,
    source backup.Backupable,
    dst destination.Destination,
    repo *models.BackupRepo,
    storeID string, // snapshot into BackupRecord.StoreID guard
) (*models.BackupRecord, error)
```

**Pipe pattern between producer and consumer:** use `io.Pipe` to stream `Backupable.Backup(w)` output directly into `Destination.PutBackup(payload io.Reader)`. The manifest's `SHA256`, `SizeBytes`, and `PayloadIDSet` are populated inside the driver (see `pkg/backup/destination/destination.go:20–29`) and on the `Backupable` side respectively; executor plumbs them through.

**ULID generation (D-21):** use `ulid.Make()` from `github.com/oklog/ulid/v2` — already in go.mod, already used by `pkg/controlplane/store/backup.go:141`:

```go
// from pkg/controlplane/store/backup.go:139-145
func (s *GORMStore) CreateBackupRecord(ctx context.Context, rec *models.BackupRecord) (string, error) {
    if rec.ID == "" {
        rec.ID = ulid.Make().String()
    }
    ...
}
```

Executor generates the ID **before** the CreateBackupRecord call so manifest + destination key + record share the same ULID (D-21).

**Error handling pattern (from Phase 3 `pkg/backup/destination/errors.go` + D-07 carryover):** wrap with `fmt.Errorf("%w: …", sentinel)` for `errors.Is` matching. Context cancellation → wrap with new `ErrBackupAborted` (from the moved `backupable.go`).

---

### `pkg/controlplane/runtime/storebackups/service.go` (NEW sub-service — the 9th, D-25)

**Analog:** `pkg/controlplane/runtime/adapters/service.go` (MIRROR EXACTLY).

This is the central pattern assignment for Phase 4. The adapters sub-service is the closest-matching existing sub-service: it owns per-entity resources (adapter goroutines), exposes explicit hot-reload CRUD (`CreateAdapter`/`DeleteAdapter`/`UpdateAdapter`/`EnableAdapter`/`DisableAdapter`), coordinates startup-from-store (`LoadAdaptersFromStore`), and implements graceful shutdown (`StopAllAdapters`).

**Package + imports (from `adapters/service.go:1–14`):**

```go
package storebackups

import (
    "context"
    "errors"
    "fmt"
    "sync"
    "time"

    "github.com/marmos91/dittofs/internal/logger"
    "github.com/marmos91/dittofs/pkg/controlplane/models"
    "github.com/marmos91/dittofs/pkg/controlplane/store"
)
```

**Service struct shape (from `adapters/service.go:52–60`):**

```go
const DefaultShutdownTimeout = 30 * time.Second

// Service manages scheduled backup execution for registered repos.
// 9th sub-service under pkg/controlplane/runtime/ (D-25).
type Service struct {
    mu      sync.RWMutex
    entries map[string]*repoEntry // keyed by repo ID

    store           store.BackupStore     // narrow interface (D-26: uses ListReposByTarget)
    storeResolver   StoreResolver         // resolves (target_id, target_kind) → Backupable (D-26)
    destFactory     DestinationFactoryFn  // builds destination.Destination from *models.BackupRepo
    shutdownTimeout time.Duration

    // scheduler + overlap + clock injected here (D-Claude's Discretion)
    scheduler   *scheduler.Scheduler
    overlap     *scheduler.OverlapGuard
    clock       clock.Clock // testability

    runtime any // SetRuntime hook (mirrors adapters.Service.runtime)
}

type repoEntry struct {
    repo    *models.BackupRepo
    entryID cron.EntryID // robfig/cron handle (not persistent — re-registered on Serve)
    ctx     context.Context
    cancel  context.CancelFunc
}
```

**Hot-reload API (D-22 — mirrors `adapters/service.go:91–131` exactly):**

```go
// RegisterRepo persists nothing — caller has already committed the DB row
// (Phase 6 handler). This method loads the repo from the store and installs
// the cron entry.  D-22: "Deterministic, testable, no eventual-consistency lag."
func (s *Service) RegisterRepo(ctx context.Context, repoID string) error {
    repo, err := s.store.GetBackupRepoByID(ctx, repoID)
    if err != nil {
        return fmt.Errorf("failed to load repo: %w", err)
    }
    if err := s.installEntry(ctx, repo); err != nil {
        return fmt.Errorf("failed to install schedule for repo %s: %w", repoID, err)
    }
    return nil
}

// UnregisterRepo removes the cron entry and cancels any in-flight run.
// Caller has already deleted the DB row.
func (s *Service) UnregisterRepo(ctx context.Context, repoID string) error {
    return s.removeEntry(repoID)
}

// UpdateRepo = Unregister + Register (matches D-22 comment:
// "edit = Unregister + Register with the new schedule").
func (s *Service) UpdateRepo(ctx context.Context, repoID string) error {
    _ = s.removeEntry(repoID) // best-effort remove
    return s.RegisterRepo(ctx, repoID)
}
```

Compare directly to `adapters/service.go:91–131`:

```go
// adapters/service.go:90-102
func (s *Service) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
    if _, err := s.store.CreateAdapter(ctx, cfg); err != nil {
        return fmt.Errorf("failed to save adapter config: %w", err)
    }
    if err := s.startAdapter(cfg); err != nil {
        _ = s.store.DeleteAdapter(ctx, cfg.Type) // rollback
        return fmt.Errorf("failed to start adapter: %w", err)
    }
    return nil
}
```

**On-demand API (D-23 — matches `RunBackup` semantics):**

```go
// RunBackup is called by BOTH the cron tick AND Phase 6's POST /backups handler.
// Acquires the per-repo mutex; returns ErrBackupAlreadyRunning (409 Conflict in
// Phase 6) if another run holds the lock.
func (s *Service) RunBackup(ctx context.Context, repoID string) (*models.BackupRecord, error) {
    unlock, acquired := s.overlap.TryLock(repoID)
    if !acquired {
        return nil, ErrBackupAlreadyRunning
    }
    defer unlock()
    // ... delegate to executor, run retention inline (D-08), return record
}
```

**Serve/Stop coordination (D-18/D-19 — mirrors `lifecycle/service.go:143–159`):**

```go
// Serve starts the scheduler. Called from runtime.Runtime.Serve (composition
// follows the lifecycle.Service.Serve pattern at lifecycle/service.go:143).
//
// D-19: On entry, call s.store.RecoverInterruptedJobs(ctx) ONCE — transitions
// any `running` BackupJob rows with no in-memory worker to `interrupted`.
// This invokes the SAFETY-02 helper already implemented at
// pkg/controlplane/store/backup.go:245–260 (Phase 1 delivered the helper;
// Phase 4 is the first consumer that wires it on boot).
//
// D-18: The ctx passed here cancels on SIGTERM. The scheduler's cron entries
// receive a derived ctx — when the parent cancels, in-flight Backup() calls
// see ctx.Err() and the executor marks the BackupJob `interrupted`. No
// indefinite wait, no drain.
func (s *Service) Serve(ctx context.Context) error {
    // 1. SAFETY-02 recovery pass (D-19)
    if n, err := s.store.RecoverInterruptedJobs(ctx); err != nil {
        logger.Warn("Failed to recover interrupted jobs on boot", "error", err)
    } else if n > 0 {
        logger.Info("Recovered interrupted jobs", "count", n)
    }

    // 2. Load all repos and install schedules (D-06: skip invalid with WARN)
    repos, err := s.store.ListAllBackupRepos(ctx)
    if err != nil {
        return fmt.Errorf("failed to list backup repos: %w", err)
    }
    for _, repo := range repos {
        if repo.Schedule == nil || *repo.Schedule == "" {
            continue
        }
        if err := s.installEntry(ctx, repo); err != nil {
            // D-06: one bad row does NOT deny-of-service the entire scheduler
            logger.Warn("Skipping repo with invalid schedule",
                "repo_id", repo.ID, "schedule", *repo.Schedule, "error", err)
            continue
        }
    }
    s.scheduler.Start() // robfig/cron.Start() — non-blocking, returns immediately
    return nil
}

// Stop cancels all in-flight runs immediately (D-18). Does NOT wait for them to finish.
func (s *Service) Stop(ctx context.Context) error {
    stopCtx := s.scheduler.Stop() // robfig/cron returns a ctx that closes when jobs finish
    // D-18: we intentionally do NOT wait on stopCtx — the per-run context is
    // already cancelled via parent ctx, executor sees ctx.Err() and marks
    // BackupJob `interrupted`. Destination.PutBackup sees ctx.Err() and aborts
    // the S3 multipart / local FS tmp. Orphan cleanup is Phase 3's problem.
    _ = stopCtx
    return nil
}
```

Compare to `lifecycle/service.go:197–210`:

```go
// lifecycle/service.go:197-210
select {
case <-ctx.Done():
    logger.Info("Shutdown signal received", "reason", ctx.Err())
    shutdownErr = ctx.Err()
case err := <-apiErrChan:
    ...
}
s.shutdown(settings, adapterLoader, metadataFlusher, storeCloser)
```

**Error sentinels (pattern from `pkg/backup/destination/errors.go`):**

```go
// pkg/controlplane/runtime/storebackups/errors.go
var (
    ErrScheduleInvalid       = errors.New("invalid cron schedule expression")
    ErrRepoNotFound          = errors.New("backup repo not found in registry")
    ErrBackupAlreadyRunning  = errors.New("backup already running for this repo")
    ErrInvalidTargetKind     = errors.New("unknown target kind")
)
```

Follows `errors.New` (not `fmt.Errorf`) + `%w`-wrapping convention (see `pkg/backup/destination/errors.go:12–68`, `pkg/metadata/backup.go:63–92`).

**SetRuntime hook** (mirrors `adapters/service.go:74`):

```go
func (s *Service) SetRuntime(rt any) { s.runtime = rt }
```

**Runtime composition** (mirrors `runtime/runtime.go:82–104`):

```go
// in runtime/runtime.go New():
rt.storeBackupsSvc = storebackups.New(s, DefaultShutdownTimeout)
rt.storeBackupsSvc.SetRuntime(rt)
```

And delegation methods (mirrors `runtime/runtime.go:120–146`):

```go
// --- Store Backup Management (delegated to storebackups.Service) ---
func (r *Runtime) RegisterBackupRepo(ctx context.Context, repoID string) error {
    return r.storeBackupsSvc.RegisterRepo(ctx, repoID)
}
func (r *Runtime) UnregisterBackupRepo(ctx context.Context, repoID string) error {
    return r.storeBackupsSvc.UnregisterRepo(ctx, repoID)
}
func (r *Runtime) RunBackup(ctx context.Context, repoID string) (*models.BackupRecord, error) {
    return r.storeBackupsSvc.RunBackup(ctx, repoID)
}
```

---

### `pkg/controlplane/runtime/storebackups/doc.go` (package doc)

**Analog:** `pkg/controlplane/runtime/adapters/doc.go` (identical structure).

```go
// Package storebackups provides scheduled backup execution for registered
// store-backup repos (metadata-store target in v0.13.0; block-store target
// additive per D-25).
//
// The Service composes a cron-based scheduler, per-repo overlap mutex,
// backup executor, and retention pass. It exposes an explicit hot-reload
// API (RegisterRepo/UnregisterRepo/UpdateRepo) consumed by Phase 6's
// repo CRUD handlers after DB commit (D-22).
package storebackups
```

Mirror `adapters/doc.go:1–7`:

```go
// Package adapters provides protocol adapter lifecycle management.
//
// The Service manages protocol adapter (NFS, SMB) creation, startup,
// shutdown, and configuration. It coordinates with the persistent store
// to ensure adapter configurations are saved alongside in-memory state.
package adapters
```

---

### `pkg/controlplane/runtime/storebackups/service_test.go`

**Analog:** `pkg/controlplane/runtime/init_test.go` (for runtime setup) + `pkg/controlplane/runtime/shares/healthcheck_test.go` (for table-driven test convention).

**Setup pattern (from `init_test.go:13–53`):**

```go
func setupTestService(t *testing.T) (*Service, cpstore.Store) {
    t.Helper()
    s, err := cpstore.New(&cpstore.Config{
        Type:   cpstore.DatabaseTypeSQLite,
        SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
    })
    if err != nil {
        t.Fatalf("failed to create test store: %v", err)
    }
    svc := New(s, 100*time.Millisecond)
    // inject fake clock for deterministic scheduling
    svc.clock = clock.NewFake()
    t.Cleanup(func() { _ = svc.Stop(context.Background()) })
    return svc, s
}
```

**Seed helpers (from `pkg/controlplane/store/backup_test.go:14–38`):**

```go
// Reuse seedMetaStore, seedRepo patterns from backup_test.go but
// DROP the `//go:build integration` tag — scheduler tests use fake clock + in-memory store.
func seedRepo(t *testing.T, s cpstore.Store, targetID, name string) *models.BackupRepo {
    t.Helper()
    ctx := context.Background()
    sched := "0 * * * *"
    repo := &models.BackupRepo{
        TargetID:   targetID,     // D-26 renamed field
        TargetKind: "metadata",   // D-26 new field
        Name:       name,
        Kind:       models.BackupRepoKindLocal,
        Schedule:   &sched,
    }
    if _, err := s.CreateBackupRepo(ctx, repo); err != nil {
        t.Fatalf("seed repo: %v", err)
    }
    return repo
}
```

**Test tags (per context §Code Context):** scheduler unit tests use fake clock + in-memory SQLite — no build tag needed. Integration tests that exercise real PG or real S3 stay behind `//go:build integration`.

---

### `pkg/controlplane/models/backup.go` (MODIFIED — D-26 field rename)

**Analog:** existing `pkg/controlplane/models/backup.go:55–82` (same file, evolve in-place).

**Before (lines 55–58):**

```go
type BackupRepo struct {
    ID              string         `gorm:"primaryKey;size:36" json:"id"`
    MetadataStoreID string         `gorm:"not null;size:36;uniqueIndex:idx_backup_repo_store_name" json:"metadata_store_id"`
    Name            string         `gorm:"not null;size:255;uniqueIndex:idx_backup_repo_store_name" json:"name"`
```

**After (D-26 polymorphic target):**

```go
type BackupRepo struct {
    ID         string `gorm:"primaryKey;size:36" json:"id"`
    TargetID   string `gorm:"not null;size:36;uniqueIndex:idx_backup_repo_target_name" json:"target_id"`
    TargetKind string `gorm:"not null;size:10;default:'metadata';index" json:"target_kind"` // 'metadata' | 'block' (future)
    Name       string `gorm:"not null;size:255;uniqueIndex:idx_backup_repo_target_name" json:"name"`
    // ... rest unchanged
}
```

**Note:** the composite unique index name changes from `idx_backup_repo_store_name` → `idx_backup_repo_target_name`. The legacy FK association (`MetadataStore MetadataStoreConfig \`gorm:"foreignKey:MetadataStoreID"\``) at line 78 must be **removed** per D-26 step 4 ("Drops the direct FK to `metadata_store_configs`").

**Json tag migration note:** the JSON tag `metadata_store_id` also changes to `target_id` (+ new `target_kind`). Any API handler or serializer checking the old field name will break — scan for callers.

---

### `pkg/controlplane/store/backup.go` (MODIFIED — D-26 method rename + retention queries)

**Analog:** existing `pkg/controlplane/store/backup.go:34–46` (rename in place).

**Before:**

```go
// pkg/controlplane/store/backup.go:34-42
func (s *GORMStore) ListBackupReposByStore(ctx context.Context, storeID string) ([]*models.BackupRepo, error) {
    var results []*models.BackupRepo
    if err := s.db.WithContext(ctx).
        Where("metadata_store_id = ?", storeID).
        Find(&results).Error; err != nil {
        return nil, err
    }
    return results, nil
}
```

**After (D-26):**

```go
func (s *GORMStore) ListReposByTarget(ctx context.Context, kind, targetID string) ([]*models.BackupRepo, error) {
    var results []*models.BackupRepo
    if err := s.db.WithContext(ctx).
        Where("target_kind = ? AND target_id = ?", kind, targetID).
        Find(&results).Error; err != nil {
        return nil, err
    }
    return results, nil
}
```

Corresponding sub-interface change in `pkg/controlplane/store/interface.go:360–361`:

```go
// Before:
ListBackupReposByStore(ctx context.Context, storeID string) ([]*models.BackupRepo, error)
// After:
ListReposByTarget(ctx context.Context, kind, targetID string) ([]*models.BackupRepo, error)
```

**New retention-query method (D-12):**

```go
// ListSucceededRecordsForRetention returns succeeded, non-pinned records for a repo,
// sorted chronologically (oldest first — retention prunes from the tail).
// Pattern mirrors ListBackupRecordsByRepo (pkg/controlplane/store/backup.go:128-137)
// but filters to status=succeeded and pinned=false (D-10, D-12).
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

---

### `pkg/controlplane/store/gorm.go` (MODIFIED — add D-26 migration)

**Analog:** existing pre-migrate rename patterns at `pkg/controlplane/store/gorm.go:211–251`.

**Pattern to copy verbatim** (from lines 246–251):

```go
// Pre-migration: rename read_cache_size column to read_buffer_size if it exists.
if db.Migrator().HasColumn(&models.Share{}, "read_cache_size") {
    if err := db.Migrator().RenameColumn(&models.Share{}, "read_cache_size", "read_buffer_size"); err != nil {
        return nil, fmt.Errorf("failed to rename read_cache_size column: %w", err)
    }
}
```

**Apply to BackupRepo (D-26 steps 1–3) — insert in pre-AutoMigrate block at ~line 252:**

```go
// Pre-migration: D-26 — rename metadata_store_id to target_id and add target_kind.
// Matches the column rename convention used for share.read_cache_size above.
if db.Migrator().HasColumn(&models.BackupRepo{}, "metadata_store_id") {
    if err := db.Migrator().RenameColumn(&models.BackupRepo{}, "metadata_store_id", "target_id"); err != nil {
        return nil, fmt.Errorf("failed to rename backup_repos.metadata_store_id: %w", err)
    }
}
// target_kind is created by AutoMigrate via the new `default:'metadata'` GORM tag.
// Belt-and-suspenders backfill for rows that existed before the column default
// took effect (AutoMigrate ADD COLUMN can leave NULLs on some dialects, same
// issue observed at gorm.go:308-314 for portmapper_port).
//
// Executed post-AutoMigrate (after the column exists).
```

**Post-AutoMigrate backfill (mirrors `gorm.go:308–314`):**

```go
// Post-migration: backfill target_kind for pre-existing rows (D-26 step 3).
// Identical pattern to the portmapper_port default backfill immediately above.
if err := db.Exec(
    "UPDATE backup_repos SET target_kind = ? WHERE target_kind = '' OR target_kind IS NULL",
    "metadata",
).Error; err != nil {
    return nil, fmt.Errorf("failed to backfill target_kind: %w", err)
}
```

**Pattern to copy for portmapper backfill reference (`gorm.go:307–314`):**

```go
// Post-migration: fix portmapper_port for existing NFS adapter settings.
// ALTER TABLE ADD COLUMN sets int to 0, not the default 10111.
if err := db.Exec(
    "UPDATE nfs_adapter_settings SET portmapper_port = ? WHERE portmapper_port = ?",
    10111, 0,
).Error; err != nil {
    return nil, fmt.Errorf("failed to apply portmapper defaults: %w", err)
}
```

---

### `pkg/metadata/store/{memory,badger,postgres}/backup.go` (MODIFIED — D-27 import-only)

**Analog:** same files unchanged semantically.

**Change:** import path `github.com/marmos91/dittofs/pkg/metadata` → add `github.com/marmos91/dittofs/pkg/backup` import, update every type reference:

| Before (Phase 2) | After (Phase 4 D-27) |
|------------------|----------------------|
| `metadata.Backupable` | `backup.Backupable` |
| `metadata.PayloadIDSet` | `backup.PayloadIDSet` |
| `metadata.NewPayloadIDSet()` | `backup.NewPayloadIDSet()` |
| `metadata.ErrBackupAborted` | `backup.ErrBackupAborted` |
| `metadata.ErrRestoreDestinationNotEmpty` | `backup.ErrRestoreDestinationNotEmpty` |
| `metadata.ErrRestoreCorrupt` | `backup.ErrRestoreCorrupt` |
| `metadata.ErrSchemaVersionMismatch` | `backup.ErrSchemaVersionMismatch` |

Concrete example: `pkg/metadata/store/badger/backup.go:153–155`:

```go
// Before:
// Ensure BadgerMetadataStore implements metadata.Backupable.
var _ metadata.Backupable = (*BadgerMetadataStore)(nil)

// After (D-27):
var _ backup.Backupable = (*BadgerMetadataStore)(nil)
```

**If the compat shim approach is taken (see `pkg/backup/backupable.go` section above), these files need no changes** — `metadata.Backupable` remains a type alias for `backup.Backupable`. Planner decides which path; both satisfy D-27.

---

## Shared Patterns

### Package-level constants for default timeouts

**Source:** `pkg/controlplane/runtime/adapters/service.go:17`, `pkg/controlplane/runtime/lifecycle/service.go:13`

**Apply to:** `pkg/controlplane/runtime/storebackups/service.go`, `pkg/backup/scheduler/scheduler.go`

```go
// adapters/service.go:17
const DefaultShutdownTimeout = 30 * time.Second

// lifecycle/service.go:13
const DefaultShutdownTimeout = 30 * time.Second
```

Mirror this for:
```go
const DefaultShutdownTimeout = 30 * time.Second
const DefaultMaxJitter       = 5 * time.Minute  // D-04
const DefaultJobRetention    = 30 * 24 * time.Hour // D-17 (30 days)
```

---

### Narrow-interface-over-composite-store pattern

**Source:** `pkg/controlplane/store/interface.go:349` (BackupStore sub-interface) + `pkg/controlplane/runtime/adapters/service.go:58` (`store store.AdapterStore` — narrowest)

**Apply to:** `pkg/controlplane/runtime/storebackups/service.go`

```go
// adapters/service.go:58:
store           store.AdapterStore   // takes narrowest interface, not composite store.Store
```

The storebackups Service should take `store.BackupStore` (not the composite `store.Store`), which exposes only the methods actually needed. This is the prevailing convention: see also `MetadataStore` provider patterns throughout `pkg/controlplane/runtime/shares/service.go:118–130`.

---

### Error wrapping with typed sentinels

**Source:** `pkg/backup/destination/errors.go:12–68`, `pkg/metadata/backup.go:63–92`

**Apply to:** `pkg/controlplane/runtime/storebackups/errors.go`, `pkg/backup/scheduler/errors.go` (if needed), `pkg/backup/executor/errors.go`

Pattern (from `destination/errors.go:12–21`):

```go
var ErrDestinationUnavailable = errors.New("destination unavailable")
// Usage:
return fmt.Errorf("put manifest: %w", ErrDestinationUnavailable)
// Caller: errors.Is(err, destination.ErrDestinationUnavailable)
```

Fixed-identity sentinels via `errors.New` (never `fmt.Errorf`), wrapped at call sites with `%w`, matched via `errors.Is` / `errors.As`. See also `pkg/controlplane/store/interface.go` where every store method documents the specific sentinel returned.

---

### sync.Map + per-key mutex for overlap prevention

**Source:** `pkg/adapter/nfs/connection.go`, `pkg/adapter/nfs/adapter.go` (both use sync.Map for connection tracking with goroutine-per-connection model)

**Apply to:** `pkg/backup/scheduler/overlap.go` (D-07 per-repo mutex)

```go
// Pattern:
type overlapGuard struct {
    mu sync.Map // key -> *sync.Mutex
}
func (g *overlapGuard) TryLock(key string) (unlock func(), ok bool) {
    m, _ := g.mu.LoadOrStore(key, &sync.Mutex{})
    mu := m.(*sync.Mutex)
    if !mu.TryLock() { return nil, false }
    return mu.Unlock, true
}
```

---

### Goroutine-per-entity with ctx-cancel + errCh

**Source:** `pkg/controlplane/runtime/adapters/service.go:326–353`

**Apply to:** `pkg/backup/scheduler/scheduler.go` (if scheduler uses one goroutine per repo — Claude's discretion per D-discretion notes)

```go
// adapters/service.go:332-350
ctx, cancel := context.WithCancel(context.Background())
errCh := make(chan error, 1)

go func() {
    logger.Info("Starting adapter", "protocol", adp.Protocol(), "port", adp.Port())
    err := adp.Serve(ctx)
    if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
        logger.Error("Adapter failed", "protocol", adp.Protocol(), "error", err)
    }
    errCh <- err
}()

s.entries[cfg.Type] = &adapterEntry{
    adapter: adp, config: cfg, ctx: ctx, cancel: cancel, errCh: errCh,
}
```

---

### Structured logging with `logger.Info` / `logger.Warn` / `logger.Debug`

**Source:** used consistently in `pkg/controlplane/runtime/adapters/service.go` (lines 200, 203, 243, 247, 336, 339, 352), `pkg/controlplane/runtime/lifecycle/service.go` (lines 169, 177, 199, 211)

**Apply to:** all Phase 4 files

Key-value style, never sprintf:

```go
logger.Info("Starting adapter", "protocol", adp.Protocol(), "port", adp.Port())
logger.Warn("Adapter stop timed out", "type", adapterType)
logger.Error("API server error", "error", err)
```

D-06 requires:
```go
logger.Warn("Skipping repo with invalid schedule",
    "repo_id", repo.ID, "schedule", *repo.Schedule, "error", err)
```

---

### GORM column rename via `db.Migrator().RenameColumn`

**Source:** `pkg/controlplane/store/gorm.go:246–251`

**Apply to:** D-26 migration for `backup_repos.metadata_store_id` → `target_id`

Already quoted above in the `gorm.go` section. The pattern is: check `HasColumn`, then call `RenameColumn`. AutoMigrate does not handle renames on its own (from context §Established Patterns).

---

### AllModels registration (no change needed for D-26)

**Source:** `pkg/controlplane/models/models.go:1–26`

BackupRepo is already registered at line 22. D-26 changes the struct shape but does not add or remove models — no `AllModels()` edit required. AutoMigrate picks up the new `TargetKind` column automatically from the GORM tag.

---

## No Analog Found

| File | Role | Data Flow | Reason |
|------|------|-----------|--------|
| `pkg/backup/scheduler/scheduler.go` | scheduler-primitives | event-driven | No existing cron-based scheduler in the codebase. Use `robfig/cron/v3` as-is (per D-specifics: "robfig/cron/v3 limitations are accepted, not abstracted over"). Only `settings_watcher.go` does periodic-timed work, and D-22 explicitly rejects its polling model. |

**Planner note:** for the scheduler package, the RESEARCH.md `robfig/cron/v3` reference + `hash/fnv` stdlib + `sync.Map` idioms (shown above) are the template. The package is small (4–5 files) and greenfield — pattern-match on external library usage rather than internal analogs.

---

## Metadata

**Analog search scope:**
- `pkg/controlplane/runtime/` — sub-service conventions (adapters, shares, stores, lifecycle)
- `pkg/controlplane/store/` — GORM patterns, sub-interface composition, migration conventions
- `pkg/controlplane/models/` — schema registration
- `pkg/backup/` — existing destination/manifest packages, sentinel error conventions
- `pkg/metadata/` — Backupable interface source location (D-27 source file)
- `pkg/metadata/store/{memory,badger,postgres}/` — Phase 2 backup implementers (D-27 import sites)
- `pkg/adapter/nfs/` — sync.Map + per-key mutex convention

**Files scanned:** ~40 across 7 directories

**Pattern extraction date:** 2026-04-16

---

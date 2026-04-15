# Architecture Research — v0.13.0 Metadata Backup & Restore

**Domain:** Disaster-recovery subsystem for per-store metadata snapshots inside DittoFS's Runtime/Store architecture
**Researched:** 2026-04-15
**Confidence:** HIGH (direct source analysis of `pkg/controlplane/runtime/runtime.go`, `pkg/controlplane/store/interface.go`, `pkg/metadata/store.go`, `pkg/blockstore/remote/remote.go`, `cmd/dfs/commands/backup/controlplane.go`, `pkg/controlplane/models/stores.go`)
**Reference:** Issue #368 ("persisted alongside the store config so triggers and scheduler are stateless consumers")

## Executive Summary

Backup/restore is a **new runtime sub-service** (`pkg/controlplane/runtime/backups/`) that owns three concerns: (1) a scheduler goroutine tied to `Runtime.Serve`, (2) a backup-destination driver registry parallel to `pkg/blockstore/remote/` (but NOT reused — different key-space, different semantics), and (3) an orchestration layer that calls a new `Backupable`/`Restorable` capability on `MetadataStore` implementations.

Repository configuration lives in a **new `BackupRepoConfig` row** linked to `MetadataStoreConfig` by store ID (1:1 or 1:N). Triggers (on-demand CLI, REST, scheduler) are stateless consumers that look up the repo config, instantiate the destination driver, call the store's native `Backup(ctx, writer)`, and record a new `BackupRecord` (a new GORM entity for list/retention/status). This matches issue #368's constraint exactly: scheduler and triggers never hold state — all state is in the GORM store.

Restore is the **dangerous path**. Architecture must quiesce the share: disable adapters for that share's shares (or refuse with `409 Conflict` if any mount exists), close the live `MetadataStore` instance via `storesSvc`, restore under a temporary handle, swap atomically, re-register, then re-enable. The API model is an async **Job** (new `models.BackupJob`) with `GET /api/jobs/{id}` polling — identical pattern to how NFSv4.2 server-side COPY was planned (async + poll).

Six new components, two modified components. Dependency chain: `BackupRepoConfig` + `BackupRecord` models → `Backupable` interface on stores → per-store implementations (memory: no-op, Badger: native backup API, Postgres: `pg_dump`-equivalent via COPY) → destination drivers (local FS, S3) → orchestrator service → scheduler → REST handlers → API client → `dfsctl` subcommands. Telemetry (OTEL spans + Prom metrics) slots in at orchestrator boundary — zero overhead when disabled, matching existing pattern.

## Standard Architecture

### System Overview

```
+--------------------------------------------------------------------+
|                         Trigger Surfaces                            |
|                                                                     |
|  CLI (dfsctl)      REST API          Scheduler          Operator   |
|  store metadata    POST/GET          cron ticker        (future)   |
|  <name> backup     /backups          internal only                 |
+-----------+----------------+----------------+---------------------+
            |                |                |
            v                v                v
+--------------------------------------------------------------------+
|                  Runtime (pkg/controlplane/runtime/)                |
|                                                                     |
|   +----------+  +--------+  +--------+  +----------------------+   |
|   | adapters |  | stores |  | shares |  |   backups (NEW)      |   |
|   |          |  |  svc   |  |        |  |  - Orchestrator      |   |
|   +----------+  +---+----+  +---+----+  |  - Scheduler         |   |
|                     |           |       |  - DriverRegistry    |   |
|                     |  +--------+       |  - JobTracker        |   |
|                     |  |                +-----------+----------+   |
|                     v  v                            |               |
|            +---------------------+                  |               |
|            | MetadataService     |                  |               |
|            | (quiesce hooks NEW) |<-----------------+               |
|            +---------+-----------+                                  |
+----------------------|----------------------------------------------+
                       |
                       v
+--------------------------------------------------------------------+
|           MetadataStore backends (new Backupable capability)        |
|                                                                     |
|  +----------+   +-----------+   +-------------+                    |
|  | memory   |   | badger    |   | postgres    |                    |
|  | (no-op / |   | native    |   | COPY TO /   |                    |
|  | JSON     |   | Backup()  |   | pg_dump     |                    |
|  | export)  |   | API       |   | equivalent  |                    |
|  +----------+   +-----------+   +-------------+                    |
+--------------------------------------------------------------------+
                       |
                       v
+--------------------------------------------------------------------+
|     Backup Destination Drivers (NEW pkg/backup/destination/)        |
|                                                                     |
|     +----------+      +----------+      (future: azure, gcs)        |
|     |  localfs |      |    s3    |                                  |
|     +----------+      +----------+                                  |
|                                                                     |
|  Separate from pkg/blockstore/remote/ — key-space is backup-IDs,    |
|  not block keys; semantics are write-once immutable archives.       |
+--------------------------------------------------------------------+
                       |
                       v
+--------------------------------------------------------------------+
|    Persistence (GORM, pkg/controlplane/store/)                      |
|                                                                     |
|   metadata_stores                                                   |
|   +-- backup_repos (NEW: 1:N by metadata_store_id)                  |
|   +-- backup_records (NEW: 1:N by backup_repo_id)                   |
|   +-- backup_jobs (NEW: transient for restore/in-progress backup)   |
+--------------------------------------------------------------------+
```

### Component Responsibilities

| Component | Responsibility | New / Modified | Typical Implementation |
|-----------|---------------|----------------|------------------------|
| `BackupRepoConfig` (models) | Per-store destination + schedule + retention | NEW | GORM row, FK to `MetadataStoreConfig.ID` |
| `BackupRecord` (models) | Completed backup metadata (id, size, sha256, started/completed, status) | NEW | GORM row, FK to `BackupRepoConfig.ID` |
| `BackupJob` (models) | In-flight backup/restore tracking | NEW | GORM row with state machine (pending/running/succeeded/failed/canceled) |
| `BackupRepoStore` (store iface) | CRUD for repos + records + jobs | NEW | New sub-interface in `pkg/controlplane/store/interface.go`, embedded in composite `Store` |
| `Backupable` (metadata iface) | Per-store snapshot capability | NEW | Optional interface on `MetadataStore`; memory returns `ErrUnsupported` or JSON export |
| `backups.Service` (runtime) | Orchestrates quiesce → snapshot → upload → record | NEW | `pkg/controlplane/runtime/backups/service.go` |
| `backups.Scheduler` | Cron ticker, per-repo polling | NEW | `pkg/controlplane/runtime/backups/scheduler.go`; tied to `Runtime.Serve` lifecycle |
| `backups.Driver` | Destination abstraction (localfs, s3) | NEW | `pkg/backup/destination/` with `fs/` and `s3/` subpackages |
| `backups.Registry` | Instantiate drivers by type + config | NEW | Mirrors `shares.Service` driver resolution |
| `MetadataStoreManager` (stores svc) | Safe close/reopen for restore | MODIFIED | Add `ReopenStore(name)` method |
| `MetadataService` | Quiesce hooks (read-only lock per share) | MODIFIED | Add `PauseShare(name)` / `ResumeShare(name)` |
| REST handlers | Thin delegation to `backups.Service` | NEW | `pkg/controlplane/api/handlers/backups.go` |
| `pkg/apiclient` methods | Typed client for backup endpoints | NEW | `backups.go` in apiclient |
| `dfsctl` subtree | Cobra commands | NEW | `cmd/dfsctl/commands/store/metadata/backup/` |

## Recommended Project Structure

```
pkg/
├── backup/                                  # NEW: driver/destination abstractions (public)
│   ├── doc.go
│   ├── destination.go                       # Driver interface: Put(id, io.Reader), Get(id) -> io.Reader, List, Delete, Stat
│   ├── destination/
│   │   ├── fs/                              # Local filesystem driver (flat-file archive)
│   │   └── s3/                              # S3 driver (reuses AWS SDK from pkg/blockstore/remote/s3 where feasible)
│   ├── archive.go                           # Archive format (magic header, version, compression hint, checksum)
│   └── errors.go
│
├── controlplane/
│   ├── models/
│   │   ├── backup_repo.go                   # NEW: BackupRepoConfig
│   │   ├── backup_record.go                 # NEW: BackupRecord
│   │   └── backup_job.go                    # NEW: BackupJob
│   │
│   ├── store/
│   │   ├── interface.go                     # MODIFIED: add BackupRepoStore sub-interface to composite Store
│   │   └── backups.go                       # NEW: GORM implementation
│   │
│   ├── runtime/
│   │   └── backups/                         # NEW sub-service
│   │       ├── service.go                   # Orchestrator (CreateBackup, Restore, List, Delete)
│   │       ├── scheduler.go                 # Cron evaluator, retention enforcer
│   │       ├── jobs.go                      # Job tracking (pending/running/succeeded/failed)
│   │       ├── quiesce.go                   # Share pause/resume helpers
│   │       └── registry.go                  # Driver factory
│   │
│   └── api/
│       └── handlers/
│           └── backups.go                   # NEW: REST handlers
│
└── metadata/
    ├── store.go                             # MODIFIED: add optional Backupable interface
    └── store/
        ├── memory/backup.go                 # NEW: JSON export (for completeness/testing)
        ├── badger/backup.go                 # NEW: wraps *badger.DB.Backup() native API
        └── postgres/backup.go               # NEW: COPY TO via pgx / pg_dump shell-out

cmd/
└── dfsctl/
    └── commands/
        └── store/
            └── metadata/
                └── backup/                  # NEW Cobra subtree
                    ├── backup.go            # `dfsctl store metadata <name> backup` (on-demand)
                    ├── list.go              # `backup list`
                    ├── show.go              # `backup show <id>`
                    ├── delete.go            # `backup delete <id>`
                    ├── repo.go              # `backup repo add|show|edit|remove` (configure destination + schedule)
                    └── restore.go           # `restore [--from <id>]` (sibling, not under backup/)

pkg/apiclient/
└── backups.go                               # NEW: typed client methods
```

### Structure Rationale

- **`pkg/backup/` as public package (not `internal/`):** enables external operators, future K8s operator integration, and clean driver registration. Mirrors `pkg/blockstore/` structure.
- **Destination drivers separate from `pkg/blockstore/remote/`:** different semantics (immutable archive objects vs. block-addressable chunks), different key-space (backup UUIDs vs. `payloadID/block-N`), different lifecycle (retention, not GC). Reusing `RemoteStore` would bleed block-store concerns (`CopyBlock`, `ReadBlockRange`, `ListByPrefix`) into a fundamentally different contract. However, the S3 driver MAY internally reuse the AWS client setup/config loading from `pkg/blockstore/remote/s3/` — share the plumbing, not the interface.
- **Sub-service under `runtime/backups/` (not a new top-level):** consistent with existing `adapters/`, `stores/`, `shares/`, `lifecycle/`, `identity/`, `mounts/` pattern. Runtime stays the single entrypoint.
- **Per-backend `backup.go` in metadata stores:** keeps snapshot logic next to the backend implementation. Avoids a monolithic orchestrator that knows every store type.
- **Repo config is a sibling entity (not a column on `MetadataStoreConfig`):** allows 1:N (future: multiple repos per store for 3-2-1 backup strategy), avoids bloating `MetadataStoreConfig`, matches issue #368's explicit separation.
- **Scheduler inside runtime, not a standalone daemon:** tied to `Runtime.Serve` lifecycle, stops on SIGTERM, in-flight job drained or marked `interrupted`.
- **`dfsctl store metadata <name> backup` (not `dfsctl backup`):** scopes the commands to a store, matches issue #368 surface. `backup list` is a sub-verb, not a sibling noun.

## Architectural Patterns

### Pattern 1: Capability-Interface on MetadataStore

**What:** Define a new optional `Backupable` interface in `pkg/metadata/`. `MetadataStore` implementations opt in via type assertion at orchestration time — no change to the core `MetadataStore` signature.

**When to use:** A cross-cutting feature that not every backend can implement (memory store: ephemeral, BadgerDB: native API, Postgres: COPY-based).

**Trade-offs:**
- Pro: zero impact on existing callers; memory store can return `ErrUnsupported` cleanly.
- Pro: matches how `NetgroupStore`, `IdentityMappingStore` are already optional in `pkg/controlplane/store/interface.go`.
- Con: orchestrator must handle "store doesn't support backup" — but that's the honest behavior.

**Example:**
```go
// pkg/metadata/backup.go (NEW)
package metadata

import (
    "context"
    "io"
)

// Backupable is an optional capability for MetadataStore implementations.
// Stores that implement this can produce a consistent snapshot to a writer
// and restore from a reader.
type Backupable interface {
    // Backup writes a point-in-time snapshot of all metadata for the given
    // share(s). If shareNames is empty, backs up the entire store.
    // Must be callable while the store is serving reads (writes may be
    // quiesced externally by the orchestrator).
    Backup(ctx context.Context, w io.Writer, shareNames []string) (BackupManifest, error)

    // Restore reads a snapshot from r and replaces the store's contents.
    // Caller must ensure the store is not serving any share (fully quiesced).
    Restore(ctx context.Context, r io.Reader) error
}

// Orchestrator check:
if b, ok := metaStore.(metadata.Backupable); ok {
    manifest, err := b.Backup(ctx, archiveWriter, []string{shareName})
    ...
}
```

### Pattern 2: Stateless Trigger + Persistent State

**What:** Every trigger (CLI, REST, scheduler) is stateless. All durable state — schedules, records, in-flight job status — lives in GORM tables. A job is `INSERT`ed first; the worker goroutine updates rows; on server restart, `running` jobs with no active worker are transitioned to `interrupted` during `Runtime.Serve` startup.

**When to use:** Matches issue #368 requirement verbatim. Standard async-job pattern when process crashes must not corrupt tracking.

**Trade-offs:**
- Pro: scheduler restart is trivial (re-read rows, resume cron evaluation).
- Pro: multi-instance safe if we ever need HA (advisory lock on job claim).
- Con: one extra DB write per state transition — negligible given backup is infrequent.

**Example:**
```go
// Scheduler loop (simplified)
func (s *Scheduler) tick(ctx context.Context) {
    repos, _ := s.store.ListDueBackupRepos(ctx, time.Now())
    for _, repo := range repos {
        job := &models.BackupJob{RepoID: repo.ID, State: "pending", TriggerKind: "scheduled"}
        s.store.CreateBackupJob(ctx, job)  // Durable claim
        go s.orch.RunJob(ctx, job.ID)       // Worker updates state
    }
}
```

### Pattern 3: Async Job + Polling (for Restore)

**What:** Restore is not synchronous. `POST /api/stores/metadata/{name}/restore` returns `202 Accepted` with `{"job_id": "..."}`. Client polls `GET /api/backup-jobs/{id}` until terminal state. `dfsctl restore` polls with progress bar; UI drives its own polling.

**When to use:** Long-running operations (GB-scale metadata restores can take minutes). Same pattern as NFSv4.2 COPY with OFFLOAD_STATUS.

**Trade-offs:**
- Pro: no HTTP timeout; client disconnect doesn't cancel.
- Pro: naturally supports UI progress bars.
- Con: needs cancellation endpoint (`DELETE /api/backup-jobs/{id}`). Minor.

### Pattern 4: Quiesce-Swap-Resume for Restore

**What:** Restore cannot be done in-place on a live store. Sequence:
1. `GET` running mounts for shares using this store. If any exist and `--force` not set → `409 Conflict`.
2. `shares.Service.DisableShare(name)` for each affected share (new method; returns NFS3ERR_STALE on subsequent ops).
3. `stores.Service.CloseStore(name)` — close the live `MetadataStore` instance.
4. Restore into a **temporary path** (e.g., `<origpath>.restore-<jobid>`).
5. Atomic swap: rename directory (for Badger) or transactional schema swap (for Postgres).
6. `stores.Service.ReopenStore(name)` — re-instantiate from config.
7. `shares.Service.EnableShare(name)`.
8. On failure at any step after (3): abort, mark job failed, leave the original store file intact (temp path is discarded). Original data is untouched because we never mutated it.

**When to use:** Any store type where live restore is unsafe (all of them except truly in-memory).

**Trade-offs:**
- Pro: crash-safe — original data never mutated until atomic swap.
- Pro: honest failure mode — failed restore leaves server in pre-restore state.
- Con: share IS unavailable during restore. Documented, not worked around.

## Data Flow

### Backup Flow (On-Demand)

```
dfsctl store metadata my-store backup
        |
        v  HTTP POST /api/stores/metadata/my-store/backups
        |
[REST handler] --> backups.Service.CreateBackup(ctx, storeName)
        |
        +--> store.GetBackupRepoByStore(storeName)           [config lookup]
        |
        +--> store.CreateBackupJob(state=running)            [durable claim]
        |
        +--> Runtime.GetMetadataStore(storeName)
        |         |
        |         v
        |    if _, ok := ms.(metadata.Backupable); !ok --> fail
        |
        +--> quiesce: metadataSvc.PauseShare(s) for s in sharesUsing(store)
        |    (read-only lock; existing ops finish, new writes block briefly)
        |
        +--> driver := registry.Resolve(repo.Destination)    [localfs or s3]
        |
        +--> pipe := io.Pipe()
        |    go backupable.Backup(ctx, pipe.Writer, shares)  [native snapshot]
        |    driver.Put(ctx, backupID, pipe.Reader)          [streamed upload]
        |
        +--> metadataSvc.ResumeShare(s)  [as soon as Badger snapshot done;
        |                                  upload continues async]
        |
        +--> store.CreateBackupRecord(id, size, sha256, duration)
        |
        +--> store.UpdateBackupJob(state=succeeded)
        |
        +--> retention: delete records older than repo.RetentionPolicy
```

### Backup Flow (Scheduled)

```
Runtime.Serve()
    |
    v
lifecycle.Service.Serve()
    |
    +--> backups.Scheduler.Start(ctx)
             |
             v
        Every 60s tick:
            repos := store.ListBackupRepos(ctx)
            for repo in repos:
                next := cron.Next(repo.Schedule, repo.LastRunAt)
                if next <= now:
                    job := store.CreateBackupJob(repo, trigger="scheduled")
                    go orchestrator.RunJob(job)
```

### Restore Flow

```
dfsctl store metadata my-store restore --from backup-abc123
        |
        v  POST /api/stores/metadata/my-store/restore
           body: {"backup_id": "backup-abc123", "force": false}
        |
[REST handler] --> backups.Service.CreateRestore(ctx, storeName, backupID, force)
        |
        +--> mounts := runtime.Mounts().FilterByShares(sharesUsing(store))
        |    if len(mounts) > 0 && !force: return 409 Conflict
        |
        +--> store.CreateBackupJob(kind=restore, state=running)
        |
        +--> (async worker)
        |       |
        |       v
        |   for each share s in sharesUsing(store):
        |       shares.Service.DisableShare(s)  [force close mounts]
        |
        |   stores.Service.CloseStore(storeName)
        |
        |   tempPath := <orig>.restore-<jobID>
        |   driver.Get(ctx, backupID) --> pipe
        |   tempStore := backend.New(tempPath)
        |   tempStore.(Backupable).Restore(ctx, pipe)
        |
        |   ATOMIC SWAP:
        |       os.Rename(orig, orig.old)
        |       os.Rename(tempPath, orig)
        |       os.RemoveAll(orig.old)  [on success]
        |
        |   stores.Service.ReopenStore(storeName)
        |   for each share s:
        |       shares.Service.EnableShare(s)
        |
        +--> store.UpdateBackupJob(state=succeeded)
        |
        v
Client polls GET /api/backup-jobs/{id} until state ∈ {succeeded, failed, canceled}
```

### List / Show / Delete

Pure CRUD against `backup_records` table via `BackupRepoStore`. No quiesce. Delete calls `driver.Delete(backupID)` then removes the record.

### Key Data Flows Summary

1. **Config write:** `dfsctl store metadata <name> backup repo add` → `store.CreateBackupRepo`. Stateless triggers read this.
2. **Backup trigger → durable job → async worker → record.** Three DB writes (create job running, create record, update job succeeded) + N driver calls.
3. **Restore trigger → quiesce → swap → resume.** Share is OFFLINE during the swap window (seconds for Badger; minutes for large Postgres). This MUST be documented.
4. **Scheduler loop is a pure GORM consumer** — no in-memory schedule state.

## Scaling Considerations

| Scale | Architecture Adjustments |
|-------|--------------------------|
| 1 store, manual backups | Initial implementation. No scheduler needed beyond tick loop. |
| 10 stores, nightly cron | Scheduler tick loop handles it trivially. Driver connection pool reused (one S3 client per driver instance, cached in registry). |
| 100+ stores, diverse schedules | Consider per-repo goroutine with timer.Reset instead of O(N) scan every tick. Not needed for v0.13.0. |
| Multi-instance HA (future) | Advisory lock on job claim (Postgres: `pg_try_advisory_lock(job_id)`, SQLite: single-instance only). Out of scope. |

### Scaling Priorities

1. **First bottleneck:** Simultaneous backup + live NFS writes causing contention on BadgerDB's LSM compactor. Mitigation: schedule during off-hours (already the usual pattern). Metric: `dittofs_backup_duration_seconds` histogram — alert if p95 climbs.
2. **Second bottleneck:** S3 driver throughput for large Postgres dumps. Mitigation: multipart upload in S3 driver (standard AWS SDK behavior). Do not build our own chunking.
3. **Third bottleneck:** Retention cleanup walking thousands of records. Mitigation: index `backup_records(repo_id, created_at)` — already standard GORM pattern.

## Anti-Patterns

### Anti-Pattern 1: Reusing `pkg/blockstore/remote/RemoteStore` as Backup Destination

**What people do:** "S3 is S3 — reuse `remote/s3`."
**Why it's wrong:** Block store is block-addressable (`{payloadID}/block-N`), mutable via overwrite, and deleted by GC. Backups are whole-file immutable archives with retention-driven lifecycle. Reusing the interface leaks block semantics (`CopyBlock`, `ReadBlockRange`, `DeleteByPrefix`) into a simpler domain and couples two unrelated rollouts. Also conflates the bucket layout — backups in the same bucket as live blocks makes operator reasoning and IAM policy harder.
**Do this instead:** New `pkg/backup/destination/Driver` interface with `Put/Get/List/Delete/Stat`. Share AWS client construction code with `pkg/blockstore/remote/s3/` (factor shared config loading into an `internal/awsclient` helper), not the interface.

### Anti-Pattern 2: In-Place Restore of a Live Store

**What people do:** "Just truncate and re-import the Badger directory while the share is mounted."
**Why it's wrong:** Clients hold open file handles. READ/WRITE operations in flight will hit inconsistent state mid-restore. Badger's goroutines will panic on a rug-pulled directory. Corrupts live data on partial restore.
**Do this instead:** Quiesce → close → restore into temp path → atomic rename → reopen → resume. Accept the downtime window. Document it.

### Anti-Pattern 3: Fire-and-Forget Scheduler Without Durable Claims

**What people do:** Goroutine reads a schedule, runs backup, no DB writes until completion.
**Why it's wrong:** Server crash mid-backup leaves no record. User doesn't know a backup was attempted. Scheduler can't resume or mark partial state. UI has nothing to show.
**Do this instead:** `INSERT BackupJob(state=pending) → UPDATE state=running → UPDATE state=succeeded|failed`. On startup, transition orphaned `running` jobs (no active worker) to `interrupted`.

### Anti-Pattern 4: Storing Backup Config as Column on MetadataStoreConfig

**What people do:** Add `backup_destination`, `backup_schedule`, `backup_retention` columns to `metadata_stores`.
**Why it's wrong:** Prevents multiple repos per store (3-2-1 backup strategy), bloats a core table, couples backup migrations to store migrations. Issue #368 explicitly says "persisted alongside the store config" — alongside ≠ inside.
**Do this instead:** New `backup_repos` table with FK to `metadata_stores.id`. 1:N. Composable.

### Anti-Pattern 5: Memory Store Silent No-Op

**What people do:** Memory store's `Backup()` returns nil without doing anything.
**Why it's wrong:** User thinks backup succeeded. On restart (memory store loses state anyway), "restore" yields nothing. Silent data loss.
**Do this instead:** Memory store either (a) returns `ErrUnsupported` and UI/CLI rejects backup repo creation on memory stores, or (b) implements JSON export (useful for tests). Be explicit either way.

### Anti-Pattern 6: Long HTTP Request for Restore

**What people do:** Synchronous `POST /restore` that blocks for minutes.
**Why it's wrong:** Reverse proxies (nginx, K8s Ingress) enforce timeouts (default 60s). Clients disconnect. Server doesn't know if it should continue.
**Do this instead:** `202 Accepted` + job ID + polling. Same as NFSv4.2 COPY + OFFLOAD_STATUS.

## Integration Points

### External Services

| Service | Integration Pattern | Notes |
|---------|---------------------|-------|
| S3 (AWS/Localstack/Scaleway/MinIO) | AWS SDK v2, new `pkg/backup/destination/s3/`. Use multipart for large objects. | Reuse region/endpoint/credential loading from `pkg/blockstore/remote/s3/`. Lifecycle policy on the bucket can complement app-level retention. |
| Local filesystem | Directory with `<uuid>.archive` files + `manifest.json`. `os.Rename` for atomicity. | Same host as server — useful for single-node or ops bastion. |
| PostgreSQL (metadata store) | `COPY TO` via pgx for streaming export, or shell out to `pg_dump` when available (match `cmd/dfs/commands/backup/controlplane.go` precedent). | `pg_dump` absence must degrade gracefully with `ErrUnsupported` + clear operator message. |
| BadgerDB (metadata store) | Native `(*DB).Backup(io.Writer, since uint64)` + `(*DB).Load(io.Reader, numGoroutines)`. | Since=0 for full backups. Incremental is v0.14+ scope. |
| OpenTelemetry | Spans: `backup.create`, `backup.upload`, `backup.restore`, `backup.quiesce`. Parent span at `backups.Service.RunJob`. | Zero overhead when `telemetry.enabled=false`. Matches existing pattern. |
| Prometheus | `dittofs_backup_total{store,status}`, `dittofs_backup_duration_seconds{store}`, `dittofs_backup_size_bytes{store}`, `dittofs_backup_jobs_inflight{kind}`. | Exposed at `/metrics`. Zero overhead when `server.metrics.enabled=false`. |

### Internal Boundaries

| Boundary | Communication | Notes |
|----------|---------------|-------|
| REST handler ↔ `backups.Service` | Direct Go call, handler is thin | Matches existing adapter/share handlers. Error mapping via `MapStoreError`. |
| `backups.Service` ↔ `stores.Service` | Needs new `CloseStore`/`ReopenStore` methods on `stores.Service` | Restore path only. |
| `backups.Service` ↔ `shares.Service` | Needs new `DisableShare`/`EnableShare` methods | Restore path only. `DisableShare` must also terminate active mounts via `mounts.Service`. |
| `backups.Service` ↔ `MetadataService` | New `PauseShare`/`ResumeShare` (read-only gate) | Backup path (brief) and restore path (long). |
| `backups.Service` ↔ GORM `Store` | New `BackupRepoStore` sub-interface | Embedded in composite `Store`. Matches 9 existing sub-interfaces. |
| `Scheduler` ↔ `Orchestrator` | Goroutine per job; scheduler creates `BackupJob` rows, orchestrator claims and runs | Durable handoff via DB. |
| `dfsctl` ↔ REST API | `pkg/apiclient/backups.go` typed methods | Same auth flow as all other commands. Dittofs-pro UI uses identical endpoints. |

## Suggested Build Order

Respecting dependency chain (each step compiles + tests independently, per project constraint):

1. **Models + GORM migrations** — `BackupRepoConfig`, `BackupRecord`, `BackupJob` in `pkg/controlplane/models/`. Add `BackupRepoStore` sub-interface; embed in `Store`. Migration + CRUD tests.
2. **`Backupable` interface + memory stub** — define interface in `pkg/metadata/`. Memory store returns `ErrUnsupported` (or JSON export). Storetest conformance stub.
3. **Destination driver interface + localfs driver** — `pkg/backup/destination/`. Localfs first (no external deps). Unit tests using `t.TempDir()`.
4. **`backups.Service` orchestrator — on-demand backup path only** — compose driver + store. No scheduler yet. No restore yet. E2E test: memory store (JSON export) → localfs → list → delete.
5. **BadgerDB `Backupable` implementation** — wraps native `DB.Backup()`. Storetest conformance run.
6. **S3 destination driver** — with Localstack in integration tests. Share AWS client plumbing with `pkg/blockstore/remote/s3/` (factor to internal helper).
7. **Restore path** — quiesce helpers on `shares.Service`, `stores.Service.ReopenStore`, `MetadataService.PauseShare`, atomic swap. E2E test: backup → restore → verify.
8. **Scheduler + retention** — cron evaluator, retention policy enforcement. E2E test with short intervals.
9. **PostgreSQL `Backupable`** — COPY TO / pg_dump variants. Match existing `cmd/dfs/commands/backup/controlplane.go` logic.
10. **REST API handlers** — `pkg/controlplane/api/handlers/backups.go`. OpenAPI/handler tests.
11. **`pkg/apiclient/backups.go`** — typed client methods.
12. **`dfsctl` commands** — Cobra subtree under `store/metadata/<name>/backup/` + sibling `restore`.
13. **Telemetry** — OTEL spans + Prom metrics at orchestrator boundary. Zero-overhead-when-disabled check.
14. **Documentation** — `docs/BACKUP.md`, update `README.md`, `CLAUDE.md`.

**Critical path:** 1 → 2 → 3 → 4 is the MVP skeleton (memory + localfs on-demand). Everything else is incremental capability extension.

## Risks to Live-Share Operation

| Risk | Likelihood | Mitigation |
|------|------------|------------|
| Backup contends with live writes, causing latency spikes | HIGH (any load) | Read-only quiesce during snapshot only (Badger: seconds). Upload is async after snapshot is captured in-memory. Document recommended schedule windows. |
| Restore triggered with active mounts → data loss | MEDIUM | Default-deny: require `--force` or explicit acknowledgment. REST returns `409 Conflict` with list of active mounts. `DisableShare` terminates mounts with NFS3ERR_STALE before restore begins. |
| Restore leaves store in broken state on failure | LOW (with atomic swap) | Temp-path + rename pattern. Original untouched until final rename. On swap failure: abort, rename-back, mark job failed. |
| Scheduler drift / missed cron during server downtime | MEDIUM | `LastRunAt` per repo in DB. On startup, if `now - LastRunAt > schedule.Interval * 2`, schedule immediate catch-up run (configurable, default on). |
| Two scheduler ticks racing on same repo | LOW (single-instance) | Single scheduler goroutine. For HA future: advisory lock on job claim. |
| Backup archive corruption (silent) | MEDIUM | SHA-256 in `BackupRecord`. Restore verifies checksum before swap. Corrupt archive → fail before touching live store. |
| Memory store backup gives user false confidence | HIGH if unchecked | `CreateBackupRepo` validates target store type; reject memory with clear message unless `allow_ephemeral=true` opt-in. |
| Orphaned `running` jobs after crash | HIGH | Startup sweep in `backups.Service.Start()`: `UPDATE backup_jobs SET state='interrupted' WHERE state='running' AND updated_at < now() - 5m`. |

## Sources

- `/Users/marmos91/Projects/dittofs-368/pkg/controlplane/runtime/runtime.go` — Runtime composition pattern, sub-service layout, Serve lifecycle
- `/Users/marmos91/Projects/dittofs-368/pkg/controlplane/store/interface.go` — 10 existing sub-interfaces (UserStore, GroupStore, ShareStore, PermissionStore, MetadataStoreConfigStore, BlockStoreConfigStore, AdapterStore, SettingsStore, AdminStore, HealthStore) and composition pattern into `Store`
- `/Users/marmos91/Projects/dittofs-368/pkg/metadata/store.go` — `Files`, `Shares` interface decomposition; precedent for optional capabilities
- `/Users/marmos91/Projects/dittofs-368/pkg/blockstore/remote/remote.go` — `RemoteStore` interface (what NOT to reuse for backup, but share AWS plumbing)
- `/Users/marmos91/Projects/dittofs-368/pkg/controlplane/models/stores.go` — `MetadataStoreConfig`, `BlockStoreConfig` GORM patterns for config storage
- `/Users/marmos91/Projects/dittofs-368/cmd/dfs/commands/backup/controlplane.go` — existing `dfs backup controlplane` precedent (SQLite VACUUM INTO, pg_dump, JSON export fallback) — reuse approach in Postgres `Backupable`
- `/Users/marmos91/Projects/dittofs-368/.planning/PROJECT.md` — milestone v0.13.0 goals, issue #368 requirements
- [BadgerDB native backup/restore](https://pkg.go.dev/github.com/dgraph-io/badger/v4#DB.Backup)
- [PostgreSQL COPY TO streaming](https://www.postgresql.org/docs/current/sql-copy.html)

---
*Architecture research for: DittoFS v0.13.0 Metadata Backup & Restore*
*Researched: 2026-04-15*

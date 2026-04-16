# Phase 4: Scheduler + Retention - Context

**Gathered:** 2026-04-16
**Status:** Ready for planning
**Requirements covered:** SCHED-01, SCHED-02, SCHED-03, SCHED-04, SCHED-05, SCHED-06

<domain>
## Phase Boundary

Deliver reliable scheduled metadata-store backups with a separate post-upload retention
pass. Specifically:

1. **In-process scheduler** (`robfig/cron/v3`) that reads persisted `backup_repos.schedule`
   and fires per-repo cron entries with `CRON_TZ=` timezone support.
2. **Per-repo overlap guard** — two scheduler ticks against the same repo never produce
   concurrent runs (per-repo mutex).
3. **Thundering-herd prevention** — stable per-repo phase offset so repos sharing a cron
   expression fire at spread times.
4. **Backup executor** — orchestrates `Backupable.Backup(w)` → `Destination.PutBackup(…)`
   → `BackupRecord` creation, with `BackupJob` state machine tracking.
5. **Retention pass** — count + age + pin policy, runs **inline after** each successful
   backup (SCHED-06 "separate pass after upload confirmed"), never deletes the only
   successful backup.
6. **Interrupted-job recovery hook** — at startup, transitions any `running` `BackupJob`
   with no worker to `interrupted` (SAFETY-02 was completed in Phase 1 at the store layer;
   Phase 4 is the first consumer that needs it wired during `Serve`).
7. **Generic "store backup" framework refactor** — hoist generic primitives (scheduler,
   executor, Backupable interface) into `pkg/backup/` so a future block-store-backup
   milestone reuses them without refactoring Phase 4's code.

**Out of scope for this phase:**
- Restore orchestration (quiesce/swap/resume/share-disable) — Phase 5
- Block-store GC hold consulting retained manifests — Phase 5
- CLI + REST API surface (`dfsctl`, `POST /api/.../backups`) — Phase 6
- Observability wiring (Prometheus metrics, OTel spans, heartbeat metric / watchdog) —
  Phase 5 (Phase 4 leaves interface hooks, no concrete collector)
- Block-store backup (future milestone — framework accommodates it without refactoring)
- Missed-run catch-up policy — deferred (`SCHED-CATCHUP` in future requirements)
- K8s operator CRD integration for backup config — explicitly deferred from v0.13.0
  (research SUMMARY §165)

</domain>

<decisions>
## Implementation Decisions

### Scheduling & Timing

- **D-01 — Missed-run policy: skip entirely.**
  When the server was down across a scheduled cron tick, that tick is dropped. Next run
  fires on the normal next cron occurrence. Matches `robfig/cron/v3` default, matches
  etcd/k3s/JuiceFS precedent, matches `SCHED-CATCHUP` being deferred to a future milestone.
  No fire-once, no fire-all. Documented limitation.

- **D-02 — No startup warm-up delay.**
  Scheduler registers cron entries during `Service.Serve()` and begins firing on the
  normal next-tick basis. No artificial sleep, no readiness gating. If an operator needs
  to stagger boot vs. first backup, they adjust the cron expression.

- **D-03 — Jitter: stable per-repo phase offset.**
  `offset_seconds = fnv64a(repo_id) % max_jitter_seconds`. Each repo fires at the SAME
  shifted time every tick — deterministic, operator-debuggable ("repo X always fires at
  00:03:42"), survives server restart with identical phase. NOT per-run random offset
  (would drift daily, harder to correlate with ops events). NOT global concurrency cap
  (unnecessary complexity for v0.13.0; can add later without breaking schedules).

- **D-04 — Default jitter window: 0–5 minutes.**
  `max_jitter_seconds = 300` by default. Spreads 20 repos over 300s = one every ~15s
  average. Enough to avoid S3 rate-limit spikes without meaningfully delaying anyone.
  NOT operator-configurable in v0.13.0 (no per-repo `jitter_seconds` column); revisit
  only if users request it.

- **D-05 — No global concurrency cap.**
  Per-repo mutex (D-07) prevents self-overlap; jitter (D-03) spreads starts. With
  metadata-store-sized backups in the tens, no global backup semaphore. Keeps the
  scheduler model simple — one cron expression, one tick, one backup run.

- **D-06 — Schedule validation: strict at write time, permissive at startup.**
  Phase 6's `repo create` / `repo update` API validates the cron expression synchronously
  via `storebackups.Service.ValidateSchedule(expr)` and rejects invalid strings with a
  400 before persisting. At startup, if an invalid schedule somehow exists in
  `backup_repos.schedule` (out-of-band DB edit, migration bug), the scheduler **skips
  that repo** with a loud WARN log `{repo_id, schedule, parse_error}` and an error
  metric; server `Serve()` still succeeds. One bad row does NOT deny-of-service the
  entire scheduler.

- **D-07 — Per-repo mutex for overlap prevention.**
  `sync.Map[repoID → *sync.Mutex]`. Scheduler tick: `if !tryLock { skip + log + metric
  backup_overlap_skipped_total; return }`. Lock held for the entire
  backup+retention inline window. Phase 6's on-demand API (`POST /backups`) acquires
  the same mutex — if a scheduled tick is running, on-demand returns 409 (documented
  contract for Phase 6).

### Retention

- **D-08 — Retention runs inline after each successful backup.**
  Backup goroutine sequence: (1) acquire per-repo mutex, (2) `Destination.PutBackup`,
  (3) persist `BackupRecord` with `status=succeeded`, (4) run retention pass,
  (5) release mutex. The mutex covers the whole window — retention NEVER races with
  an in-flight upload (SCHED-06 invariant). No separate retention cron, no on-demand-only
  retention. One cron tick = one backup + one cleanup cycle.

- **D-09 — Retention combination: union (keep if EITHER count OR age matches).**
  `kept = top_N_by_created_at(records, keep_count) ∪ where(records, created_at ≥ now − keep_age_days)`.
  More permissive: a record inside either window is preserved. Matches restic / kopia
  / borg / k3s-etcd-snapshot convention. Operator intuition: "keep last 7 OR last 14
  days" means both safety nets work.

- **D-10 — Pinned records are outside the count math.**
  `keep_count=7` with 3 pinned records = 3 pinned + last 7 non-pinned successful = 10
  total kept. Pins are "extra". Retention loop: compute candidates on non-pinned
  successful records only; pinned records are never pruned regardless of age.

- **D-11 — Safety rail always wins.**
  If a repo's only successful backup is older than `keep_age_days`, retention still
  keeps it. Losing DR capability entirely is strictly worse than retaining one stale
  backup. SCHED-05 is an absolute invariant over age policy. Operator can explicitly
  delete via CLI (Phase 6) if they want a clean slate.

- **D-12 — Retention only touches `BackupRecord` rows with `status=succeeded`.**
  Failed / interrupted / pending records are never considered retention candidates
  (but see D-16 — failed/interrupted attempts don't create `BackupRecord` rows anyway).
  Retention decisions are over the set of restorable archives only.

- **D-13 — Retention error handling: continue-on-error, summary report.**
  Destination.Delete failures don't abort the pass. Each failure is logged at WARN
  (`{repo_id, backup_id, error}`) with a counter metric `backup_retention_delete_errors_total`.
  Successfully-deleted entries update the DB in the same iteration. Next retention
  pass retries the failed IDs. Maximizes cleanup throughput; one flaky S3 record
  doesn't block all cleanups.

- **D-14 — Destination-first delete ordering.**
  For each retention candidate: (1) `Destination.Delete(ctx, id)`, (2) on success,
  `DELETE FROM backup_records WHERE id=?`. If destination fails, DB row remains — retry
  on next pass. Never the reverse: leaving destination objects orphaned (DB deleted
  but archive present) would bypass Phase 3's orphan sweep (which only handles
  incomplete uploads, not published-but-DB-orphaned), creating a silent leak.

- **D-15 — Retention failures do NOT degrade the parent backup job status.**
  `BackupJob.Status = succeeded` even if retention reports failures. The archive WAS
  produced. Retention is "best-effort cleanup", tracked separately via
  `backup_retention_delete_errors_total`. Operator alerting reads the counter, not
  the job status. Avoids false "backup failed" pages when a backup is actually restorable.

### Records, Jobs, Lifecycle

- **D-16 — Failed / interrupted attempts create `BackupJob` rows only, no `BackupRecord`.**
  `BackupRecord` is the list of restorable archives (manifest.yaml present at the
  destination). A failed attempt has no manifest → nothing to restore → no record.
  `BackupJob` tracks the attempt history (`started_at`, `finished_at`, `error`,
  `status`, `kind`, `repo_id`). Cleaner separation: "what can I restore from?" vs
  "what happened?". Retention never touches `BackupJob`.

- **D-17 — BackupJob pruner: rows older than 30 days are deleted automatically.**
  Phase 4 runs a trivial `DELETE FROM backup_jobs WHERE finished_at < now - 30d` as
  part of the retention pass (or on its own cheap ticker). Bounds DB growth. Operators
  keep recent attempt history for debugging but don't accumulate forever. Configurable
  via a future setting if operators need longer history.

- **D-18 — Shutdown behavior: cancel immediately, mark interrupted.**
  SIGTERM → lifecycle context cancelled → backup goroutine's context cancelled →
  `Destination.PutBackup` returns with ctx error → BackupJob transitions to
  `interrupted`, BackupRecord is NOT created, partial destination artifact is left for
  Phase 3's orphan sweep to clean up on next startup. Fast shutdown, no indefinite wait.
  Scheduler doesn't wait for in-flight backups; small backups occasionally get lost
  to timing, which is acceptable — next cron tick retries.

- **D-19 — Interrupted-job recovery hook at Serve-time.**
  On `storebackups.Service.Serve(ctx)`: run the Phase-1 interrupted-job transition
  (already implemented at the store layer for SAFETY-02). Any `BackupJob` with
  `status=running` + no in-memory worker → `status=interrupted, error='worker
  terminated unexpectedly'`. Phase 5 extends this with restore-job recovery logic.

- **D-20 — BackupJob.ID and BackupRecord.ID are distinct ULIDs.**
  Job tracks the attempt; Record is the resulting archive. Retry-after-interrupt creates
  a new Job (new ID) that may or may not produce a Record (new ID). Matches Phase 1
  schema where `BackupJob.BackupRecordID` is nullable (set only on restore kind linking
  job-to-record-to-restore-from, or optionally set on successful backup kind).

- **D-21 — Executor generates ULIDs BEFORE calling `Destination.PutBackup`.**
  Sequence:
  1. `recordID := ulid.Make()`
  2. Create `BackupJob` row (`status=running`, fresh ULID job ID)
  3. Populate `manifest.Manifest{ BackupID: recordID, StoreID: …, …}`
  4. Call `Destination.PutBackup(ctx, manifest, payloadStream)` — destination uses the
     ID to build `<repo-prefix>/<recordID>/` key structure per Phase 3 D-01
  5. On success, persist `BackupRecord{ ID: recordID, RepoID: …, SHA256: manifest.SHA256,
     SizeBytes: manifest.SizeBytes, Status: succeeded, StoreID: snapshot, ManifestPath: … }`
  6. Update `BackupJob{ Status: succeeded, FinishedAt: now, BackupRecordID: &recordID }`
  Single source of truth for the archive ID; DB row and destination artifact share the
  same ULID key.

### Hot-Reload & Runtime Integration

- **D-22 — Scheduler hot-reload via explicit API, not polling.**
  `storebackups.Service` exposes:
  ```go
  RegisterRepo(ctx context.Context, repoID string) error
  UnregisterRepo(ctx context.Context, repoID string) error
  // edit = Unregister + Register with the new schedule
  ```
  Phase 6's `repo add` / `repo update` / `repo remove` handlers call these after the
  DB write commits. Deterministic, testable, no eventual-consistency lag. Mirrors
  `runtime/adapters/` CreateAdapter/DeleteAdapter pattern (existing convention since v3.5
  runtime decomposition).

- **D-23 — On-demand backup API uses the same executor path.**
  `storebackups.Service.RunBackup(ctx, repoID)` is called from BOTH the cron tick AND
  Phase 6's `POST /api/.../backups` handler. Same per-repo mutex acquires, same executor,
  same job/record persistence. If a cron-triggered run is holding the mutex, on-demand
  returns 409 Conflict with the running job ID (documented in Phase 6).

### Code Structure & Naming

- **D-24 — Generic framework lives in `pkg/backup/`.**
  New packages in Phase 4:
  ```
  pkg/backup/scheduler/           (cron+jitter+overlap+retention-policy primitives —
                                    store-agnostic, takes an abstract Target interface)
  pkg/backup/executor/             (runs Backupable → manifest → destination pipeline —
                                    store-agnostic; the seam that makes Phase 4 reusable)
  pkg/backup/backupable.go         (MOVED from pkg/metadata/backup.go — the Backupable
                                    interface + PayloadIDSet + sentinel errors. Generic
                                    now that the framework doesn't live inside pkg/metadata.)
  ```
  Existing generic packages stay put: `pkg/backup/manifest/`, `pkg/backup/destination/`.

- **D-25 — Sub-service name: `runtime/storebackups/`.**
  Location: `pkg/controlplane/runtime/storebackups/`. Scope in v0.13.0: metadata-store
  target only. The name is the generic umbrella; Phase 4's contribution inside it is
  specifically "metadata store backup". Future block-store-backup target = additional
  wiring files in the same package (no rename, no refactor).

- **D-26 — Schema migration: rename `backup_repos.metadata_store_id` → `target_id`,
  add `target_kind`.**
  Phase 4 ships a GORM migration that:
  1. Renames column `metadata_store_id` → `target_id` (size:36).
  2. Adds column `target_kind` (size:10, NOT NULL, default `'metadata'`, indexed).
  3. Backfills `target_kind = 'metadata'` for all existing rows.
  4. Drops the direct FK to `metadata_store_configs` (target_id becomes polymorphic);
     validation of the target reference moves to service-layer (runtime/storebackups/
     validates `(target_id, target_kind)` resolves to an actual store before
     registering).
  5. Updates `BackupStore` sub-interface methods:
     `ListReposByMetadataStore(id)` → `ListReposByTarget(kind, id)`.

  Affected code: `pkg/controlplane/models/backup.go`, `pkg/controlplane/store/backup.go`,
  GORM auto-migrate registration, any query sites in Phase 1 integration tests.

- **D-27 — Moving `Backupable` to `pkg/backup/backupable.go`.**
  Interface signature unchanged. All existing importers (`pkg/metadata/store/{memory,
  badger,postgres}/backup.go`) update their import path from `pkg/metadata` →
  `pkg/backup`. Phase 2 sentinel errors (`ErrBackupUnsupported`, `ErrRestoreDestinationNotEmpty`
  etc.) move with the interface. Kept in one top-level file for discoverability.

### Claude's Discretion

- Exact clock/time abstraction shape for testability — planner may introduce a
  `clock.Clock` interface injected into `storebackups.Service` or may leverage
  `benbjohnson/clock` if already in go.mod; matches whatever existing runtime services use.
- Internal structure of `pkg/backup/scheduler/` — one goroutine-per-repo vs. central
  loop with a work channel; either is fine as long as overlap (D-07), jitter (D-03),
  and hot-reload (D-22) are honored.
- Exact ULID package choice — must match whatever Phase 1 locked in
  (likely `oklog/ulid/v2`); no new dep.
- Whether to surface a `storebackups.Service.ListInterrupted(ctx)` helper for Phase 6's
  job-listing endpoint or let Phase 6 query the store directly — minor.
- Observability hook shape (noop collector interface that Phase 5 fills in) — planner
  decides whether to ship stubs now or let Phase 5 retrofit.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner) MUST read these before planning or implementing.**

### Phase 1 + 2 + 3 lock-ins (binding contracts)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md` — Phase 1 context (models, manifest schema, Backupable interface, SAFETY-02 interrupted-job transition)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-02-SUMMARY.md` — `BackupStore` sub-interface (Phase 4 extends with `ListReposByTarget`)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-03-SUMMARY.md` — manifest v1 + `Backupable` + `PayloadIDSet`
- `.planning/phases/02-per-engine-backup-drivers/02-CONTEXT.md` — Phase 2 context (per-engine cleartext stream producers, error sentinels, `Backupable` implementers)
- `.planning/phases/03-destination-drivers-encryption/03-CONTEXT.md` — Phase 3 context (destination interface D-11, two-phase commit, orphan sweep, retention contract expectations)
- `pkg/backup/manifest/manifest.go` — Manifest v1 struct, `BackupID` (ULID), `StoreID`, `SHA256`, `SizeBytes`, `Encryption`, `PayloadIDSet`, `EngineMetadata`
- `pkg/backup/destination/destination.go` — `Destination` interface (`PutBackup`, `GetBackup`, `List`, `Stat`, `Delete`, `ValidateConfig`, `Close`)
- `pkg/metadata/backup.go` — `Backupable` interface, `PayloadIDSet` type (MOVES to `pkg/backup/backupable.go` in Phase 4 per D-27)
- `pkg/controlplane/models/backup.go` — `BackupRepo`, `BackupRecord`, `BackupJob` (Phase 4 migrates BackupRepo per D-26)

### Project-level
- `.planning/REQUIREMENTS.md` §SCHED — SCHED-01..06 (Phase 4 requirements)
- `.planning/REQUIREMENTS.md` §Future Requirements — `SCHED-CATCHUP` deferred (D-01 rationale)
- `.planning/REQUIREMENTS.md` §Out of Scope — K8s operator integration deferred (addresses user question)
- `.planning/research/SUMMARY.md` §Phase 03 scheduler subset — `robfig/cron/v3` + overlap mutex + jitter rationale
- `.planning/research/SUMMARY.md` §Phase 05 — retention-as-separate-pass rationale (SCHED-06, D-08)
- `.planning/research/PITFALLS.md` §Pitfall 7 — Scheduler edge cases (overlap, DST, thundering herd, missed runs) — drove D-01, D-03, D-07
- `.planning/research/PITFALLS.md` §Pitfall 8 — S3 partial + retention race — drove D-08, D-14
- `.planning/research/PITFALLS.md` §Pitfall 10 — Silent failures (retention-induced data loss) — drove D-11, D-12, D-13
- `.planning/PROJECT.md` — single-instance, no clustering constraint (scheduler is in-process, no distributed leader election)

### Scheduler library (external)
- `robfig/cron/v3` — https://pkg.go.dev/github.com/robfig/cron/v3
  - `CRON_TZ=` prefix support: https://pkg.go.dev/github.com/robfig/cron/v3#readme-cron-expression-format
  - `ParseStandard` for 5-field syntax, `Parse` for with-seconds
  - `EntryID` lifecycle (note: NOT persistent across restarts; scheduler re-registers on Serve)

### Runtime patterns to mirror
- `pkg/controlplane/runtime/adapters/` — sub-service pattern (CreateAdapter/DeleteAdapter, lifecycle hooks) — template for D-22 hot-reload API
- `pkg/controlplane/runtime/shares/` — sub-service pattern (owns per-share resources, lifecycle)
- `pkg/controlplane/runtime/stores/` — MetadataStoreManager registry — target lookup source for D-26 service-layer FK validation
- `pkg/controlplane/runtime/lifecycle/service.go` — Serve/Stop coordination, ShutdownTimeout (D-18 cancellation propagates via ctx)
- `pkg/controlplane/runtime/settings_watcher.go` — reference for what NOT to do for hot-reload (D-22 chose explicit API over polling)

### BackupStore sub-interface
- `pkg/controlplane/store/backup.go` — existing CRUD for `backup_repos` / `backup_records` / `backup_jobs`. Phase 4 renames list method per D-26 and adds retention-query helpers (`ListSucceededRecordsForRetention(repoID)`).

### External docs (read at plan/execute time)
- FNV-1a hash (D-03) — `hash/fnv` stdlib, zero dep
- ULID v2 — https://pkg.go.dev/github.com/oklog/ulid/v2 (already locked Phase 1)
- GORM column rename migration — https://gorm.io/docs/migration.html#Migration

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets

- **`pkg/backup/destination/destination.go`** — `Destination` interface is stable and
  ready to consume. Phase 4 is the first caller of `Destination.Delete` in production
  code (conformance tests call it already).
- **`pkg/controlplane/runtime/stores/`** — `MetadataStoreManager.Get(id)` resolves a
  `metadata.Store` by ID; this is how D-26's service-layer FK validation looks up
  `(target_id, target_kind='metadata')`.
- **`pkg/controlplane/runtime/lifecycle/service.go`** — `Service.shutdownTimeout` +
  ctx propagation; the scheduler sub-service hangs off the same ctx tree, no new
  shutdown plumbing.
- **`pkg/controlplane/store/backup.go`** — existing `BackupStore` sub-interface;
  Phase 4 renames/extends per D-26.
- **`pkg/metadata/backup.go`** — `Backupable` interface that Phase 2 implementations
  already satisfy. Phase 4 moves it per D-27; Phase 2 files get an import-path update
  (no behavior change).
- **ULID generation** — `oklog/ulid/v2` locked in Phase 1; use `ulid.Make()` for
  both Job IDs and Record IDs (D-20, D-21).

### Established Patterns

- **Sub-service composition** — 8 existing sub-services under `pkg/controlplane/runtime/`
  (adapters, shares, mounts, stores, lifecycle, identity, clients, blockstoreprobe).
  Phase 4's `storebackups/` is the 9th, following the same structure: `service.go`
  with a `Service` struct, `Serve(ctx)`, `Stop(ctx)`, and domain-specific methods.
- **Explicit runtime hot-reload API** — `runtime/adapters/` exposes CreateAdapter /
  DeleteAdapter; Phase 4 mirrors with RegisterRepo / UnregisterRepo (D-22).
- **Interrupted-job transition at Serve-time** — SAFETY-02 already implemented in the
  store layer (Phase 1); Phase 4 is the first sub-service that INVOKES it during its
  own `Serve(ctx)`. Phase 5 extends with restore-kind semantics.
- **GORM migration via AllModels + AutoMigrate** — existing pattern in
  `pkg/controlplane/store/gorm.go`. D-26 column rename needs an explicit
  `Migrator().RenameColumn()` + backfill UPDATE (AutoMigrate doesn't handle renames).
- **Typed sentinel errors wrapped with `%w`** — `errors.Is`/`errors.As` checks; Phase 4
  adds `ErrScheduleInvalid`, `ErrRepoNotFound`, `ErrBackupAlreadyRunning`, `ErrInvalidTargetKind`.
- **Shared `//go:build integration` for tests that hit a real DB** — Phase 4 scheduler
  tests primarily use a fake clock + in-memory store (fast); integration tests use the
  existing SQLite fixtures.

### Integration Points

- **Phase 5 (restore)** extends `storebackups.Service` with `RunRestore(ctx, recordID)`
  and restore-job lifecycle (D-25 single-service-for-both-kinds).
- **Phase 5 block-GC hold** reads retained-backup manifests via `storebackups.Service.
  ListRetainedPayloadIDSets(ctx)` — Phase 4 exposes the helper; Phase 5 owns the
  `pkg/blockstore/gc/` integration.
- **Phase 6 CLI `backup on-demand`** calls `storebackups.Service.RunBackup(ctx, repoID)`
  (D-23).
- **Phase 6 CLI `repo add/update/remove`** calls RegisterRepo/UnregisterRepo after DB
  commit (D-22).
- **Phase 3 destination drivers** are consumed as-is — Phase 4 does not modify them.
- **Phase 2 engines** are consumed via the relocated `Backupable` interface (D-27);
  no Phase 2 behavior change.

</code_context>

<specifics>
## Specific Ideas

- **"We are speaking about store backup" (user, session 2026-04-16).** The subsystem
  is conceptually about backing up DittoFS stores. Metadata stores are the v0.13.0
  target; block-store backup is plausible future work. Code structure (D-24, D-25,
  D-26, D-27) makes this explicit at the package layout level so the future extension
  is additive, not rewriting.
- **User emphasized "reliable and safe" as the core v0.13.0 quality (carried over from
  Phases 2+3).** Every gray-area choice defaulted to the conservative option:
  skip-missed over fire-all-missed (D-01), destination-first delete over DB-first
  (D-14), safety-rail-always-wins over strict-age-policy (D-11), cancel-immediately
  over wait-on-shutdown (D-18).
- **K8s operator integration explicitly out of scope** (research SUMMARY §165 +
  user confirmation in discussion). Phase 4 ships no operator-facing changes; future
  operator work patches `spec.paused=true` before calling `POST /restore` (Phase 5/6
  surface, not Phase 4).
- **robfig/cron/v3 limitations are accepted, not abstracted over.** `EntryID` is not
  persistent across restart — we re-register from DB on Serve. `CRON_TZ=` prefix
  requires explicit opt-in. No catch-up / missed-run behavior by default (matches D-01).
  We're not adding a wrapper to "fix" cron's defaults; we're using it as-is with
  explicit invariants at the orchestrator layer.

</specifics>

<deferred>
## Deferred Ideas

- **Missed-run catch-up policy** — `SCHED-CATCHUP` in REQUIREMENTS.md Future
  Requirements. v0.13.0 skips missed runs entirely (D-01). Future milestone may add
  an explicit `fire_once` / `fire_all` column on `backup_repos`.
- **Per-repo configurable jitter window** — v0.13.0 hardcodes `max_jitter=5min`.
  Future milestone may add `backup_repos.jitter_seconds` column.
- **Global concurrency cap on concurrent backups** — rejected for v0.13.0 (D-05).
  Revisit if operators report S3-throttling with 50+ simultaneously-scheduled repos.
- **Observability wiring** — Prometheus metrics (last-success timestamp, duration
  histogram, retention-deletes counter, overlap-skipped counter) + OpenTelemetry
  spans — Phase 5 per research SUMMARY §Phase 05. Phase 4 leaves interface hooks
  (noop collectors) that Phase 5 fills in.
- **Heartbeat / watchdog metric for silent-failure detection** — research PITFALLS
  #10. Phase 5 scope.
- **K8s operator backup CRD fields** — explicitly deferred from v0.13.0 per research
  SUMMARY §165. Future operator milestone will add `DittoServer.spec.backupRepos[]`
  that the operator reconciles into `POST /api/.../repos`.
- **Block-store backup** — entire consumer, uses the generic framework Phase 4 ships.
  Separate future milestone. Phase 4's structure (D-24, D-25, D-26) makes it purely
  additive (no refactor at that point).
- **Cross-engine restore (JSON IR)** — deferred to `XENG-01`. Does not affect Phase 4
  scheduler/retention.
- **Automatic test-restore ("backup verify") job** — `AUTO-01` future work. Phase 4
  does not introduce verify-runs.
- **BackupJob prune beyond 30 days configurable** — D-17 hardcodes 30 days for
  v0.13.0; future setting if operators need longer history.
- **Per-run random jitter mode as operator-selectable alternative to stable hash**
  — v0.13.0 ships with stable hash only (D-03).

### Reviewed Todos (not folded)

None — `gsd-tools todo match-phase 4` returned no matches.

</deferred>

---

*Phase: 04-scheduler-retention*
*Context gathered: 2026-04-16*

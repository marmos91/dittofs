# Phase 5: Restore Orchestration + Safety Rails - Context

**Gathered:** 2026-04-16
**Status:** Ready for planning
**Mode:** `--auto` (every decision below is Claude's recommended default; reviewer
should audit inline rationale before planning)
**Requirements covered:** REST-01, REST-02, REST-03, REST-04, REST-05, SAFETY-01,
SAFETY-02 (SAFETY-02 store-layer primitive already delivered in Phase 1; Phase 5
extends it to the `restore` job kind)

<domain>
## Phase Boundary

Deliver safe, in-place restore of a metadata store from a previously-captured
backup, plus the safety rails that keep live clients and block storage
consistent during and after the restore. Specifically:

1. **Share-disabled precondition (REST-02).** Add an `Enabled` column to the
   `shares` table plus runtime enforcement: a disabled share disconnects active
   connections and refuses new connections. Restore refuses with 409 Conflict
   if any share referencing the target store is still enabled.

2. **Manifest pre-flight verification (REST-03).** Download `manifest.yaml`
   only (cheap); validate `manifest_version`, `store_kind`, `store_id`, and
   the declared SHA-256. Hard-reject `store_kind` / `store_id` mismatch
   (Pitfall #4). Payload SHA-256 is verified streaming during
   `Destination.GetBackup` per Phase 3 D-11 — full-payload download aborts
   before touching live state on mismatch.

3. **Quiesce → side-engine restore → atomic swap → reopen → resume (REST-01).**
   Open a fresh empty engine instance at a temp path/schema; call
   `Backupable.Restore(r)` into it (Phase 2 invariant: destination must be
   empty); atomically swap the `stores.Service` registry pointer; close the
   old engine; delete the old backing data. Orchestrator (not engine) owns the
   atomic-rename moment.

4. **Default-latest + `--from <backup-id>` selector (REST-04).** Phase 5
   exposes `storebackups.Service.RunRestore(ctx, repoID, recordID *string)`;
   `nil` → latest successful `BackupRecord` by `created_at`. `--from` CLI /
   REST plumbing is Phase 6's problem; Phase 5 provides the callable path.

5. **Safe retry after interruption (REST-05).** Each attempt creates a new
   `BackupJob{Kind: restore}` row. Mid-swap abort rolls back to the old
   engine (registry pointer never flipped on failure) and cleans up the
   fresh engine on temp path. Operator re-runs same command → same source
   record, new job, clean destination precondition still holds.

6. **Interrupted-restore job recovery (SAFETY-02 extension).** The Phase-1
   `RecoverInterruptedJobs` primitive is already called by
   `storebackups.Service.Serve()` (Phase 4 D-19) — Phase 5 adds zero new
   code for recovery. What's new: Phase 5 writes `restore`-kind jobs. Recovery
   already treats them uniformly with `backup`-kind jobs (transitions
   `status=running, no worker` → `status=interrupted, error="worker terminated
   unexpectedly"`).

7. **Block-store GC retention hold (SAFETY-01).** `pkg/blockstore/gc/` gains
   a backup-aware hold: before each GC run, iterate every
   `succeeded` `BackupRecord` across every repo, fetch the archived
   `manifest.yaml` (cheap — ~KB), union the `PayloadIDSet` fields into a
   held-set. GC treats held PayloadIDs as live even when no metadata
   references them. When retention prunes a record and its manifest, GC
   naturally re-computes a smaller held-set on the next run — no explicit
   cascade needed.

8. **Post-restore client-handle invalidation (defense-in-depth).** Because
   REST-02 gates restore on share-disabled state, all adapter connections
   are already closed. Belt-and-suspenders: bump the NFSv4 server boot
   verifier on successful restore (forces clients that reconnect after
   share re-enable into the reclaim-grace path with guaranteed
   NFS4ERR_STALE_STATEID / NFS4ERR_BAD_SESSION). SMB durable handles and
   leases are persisted **inside** the metadata store itself (Badger keys
   `dh:*`, `lock:*`) — they're naturally replaced by the snapshot's state;
   no separate clear step is required.

9. **Lightweight observability hooks.** Minimal Prometheus counters +
   OpenTelemetry spans on the restore path, plus wire Phase 4's deferred
   backup metrics (Phase 4 D-15 "noop collectors Phase 5 fills in"). Full
   metric suite (duration histograms, retention counters) is deferred;
   restore ships `backup_restore_total{outcome}` and
   `backup_last_success_timestamp_seconds{repo_id, kind}` at minimum.

**Out of scope for this phase:**
- CLI / REST API surface for restore (`dfsctl store metadata … restore`,
  `POST /api/stores/metadata/{name}/restore`, `GET /api/backup-jobs/{id}`) —
  Phase 6. Phase 5 exposes `storebackups.Service.RunRestore(ctx, repoID,
  recordID)` as the single callable entrypoint.
- Operator UX: confirmation prompts, `--yes`, `--dry-run`, `--wait` / `--async`
  semantics — Phase 6.
- Restore-to-a-different store (cross-store / staging restore) —
  `REST2NEW-01` deferred.
- Cross-engine restore (Badger ↔ Postgres via JSON IR) — `XENG-01` deferred.
- Automatic backup verification / "check" command — `AUTO-01` deferred.
- Block-store data backup — not in milestone (metadata-only scope).
- Full Prometheus / OTel suite (duration histograms, job-inflight gauges,
  detailed retention counters) — beyond the minimal hooks below; Phase 7 may
  extend.
- End-to-end / chaos / cross-version test matrix — Phase 7.
- K8s operator integration for restore triggers (operator patching
  `spec.paused=true` before `POST /restore`) — deferred to future operator
  milestone.

</domain>

<decisions>
## Implementation Decisions

### Share Disabled State (REST-02)

- **D-01 — Schema: add `Enabled bool DEFAULT true NOT NULL` column to
  `shares`.**
  [Auto-selected, recommended] Simplest schema that expresses the
  enabled/disabled binary REST-02 requires. Matches the existing
  `Share.ReadOnly bool` pattern at line `pkg/controlplane/models/share.go:26`.
  Default `true` keeps every existing share enabled on migration — no
  behavior change for operators who don't use backup/restore.
  Rejected: `DisabledAt *time.Time` (adds nullable-column complexity for a
  v0.13.0 where "when was it disabled" is not a product requirement);
  separate `share_states` table (over-engineered for a single boolean).

- **D-02 — Enforcement: disable disconnects + refuses new (REST-02 literal).**
  [Auto-selected]
  1. `shares.Service.DisableShare(ctx, name)` sets the runtime `Share.Enabled=false`,
     flips `Share.Enabled` in the control-plane DB row, and triggers
     `notifyShareChange` so adapters close existing sessions that reference the
     share (NFS: evict mount-tracker entries, invalidate NFSv4 clients via
     `CB_RECALL`-on-none-remaining; SMB: send lease-break-to-None, close sessions).
  2. When disabled: NFS MOUNT returns `MNT3ERR_ACCES`; NFSv4 PUTFH returns
     `NFS4ERR_STALE`; SMB TREE_CONNECT returns `STATUS_NETWORK_NAME_DELETED`.
  3. `EnableShare(ctx, name)` reverses both flags.
  4. In-process state: `Share.Enabled` lives in the runtime Share struct (new
     field). Adapters read it on each request via the existing `GetShare(name)`
     path — cheap + already thread-safe.

  Rejected: refuse-only (legacy connections linger) — violates REST-02 literal
  "disconnects all clients and refuses new connections".

- **D-03 — Transition is synchronous: Disable returns after adapters have
  dropped connections.**
  [Auto-selected]
  `DisableShare` blocks until the OnShareChange callback chain completes.
  Phase 5's `RunRestore` pre-flight reads the DB row (not the callback state)
  → if operator manually toggled Enabled=false in the DB, we still proceed.
  The synchronous wait keeps the caller's mental model simple: "by the time
  Disable returns, no new request will touch the store."
  Timeout bound: the existing `lifecycle.ShutdownTimeout` (default 30s) also
  gates share-disable. After the timeout, disable succeeds anyway; restore
  is free to run even if one stubborn adapter is still tearing down — the
  side-engine swap (D-05) is safe regardless.

- **D-04 — Enabled state is persisted, not ephemeral.**
  [Auto-selected] Restore-completion does NOT auto-re-enable shares.
  Rationale: after restore, operator inspects metadata (possibly runs
  `dfsctl share list`), verifies integrity, then explicitly re-enables via
  `dfsctl share enable`. Matches Pitfall #2 recovery model: forcing the
  operator's hand is safer than silently resuming with possibly-mismatched
  client expectations (cache invalidation, handle versioning, auth rebinds).
  This belongs in Phase 6 command wiring; Phase 5 just persists the flag.

### Restore Orchestration (REST-01)

- **D-05 — Side-engine restore + atomic registry swap.**
  [Auto-selected, safety-first]

  ```text
  storebackups.Service.RunRestore(ctx, repoID, recordID *string):
    1. Pre-flight: load BackupRepo; resolve target store; verify all shares
       for that store have Enabled=false (REST-02). If any enabled → 409
       ErrRestorePreconditionFailed.
    2. Select source record:
       - recordID == nil → latest successful BackupRecord in repo
                         (BackupStore.ListSucceededRecordsByRepo, first row)
       - recordID != nil → load + verify repo_id match + status=succeeded
    3. Download manifest.yaml only (Destination.GetManifestOnly(ctx, id)).
    4. Validate manifest.store_kind == target.kind (hard reject mismatch).
       Validate manifest.store_id == target.store_id (hard reject, D-06).
       Validate manifest_version == 1 (hard reject future).
       Validate manifest.sha256 != "" (sanity).
    5. Create BackupJob{Kind: restore, Status: running, RepoID, BackupRecordID}.
    6. Open fresh engine at temp path/schema:
       - Badger: tempDir := filepath.Join(store.path + ".restore-<ulid>")
       - Postgres: CREATE SCHEMA "<current>_restore_<ulid>" (same conn pool)
       - Memory: new MemoryMetadataStore{} struct
    7. Destination.GetBackup(ctx, recordID) → ReadCloser (streams plaintext
       post-decrypt + streaming SHA-256 verify — Phase 3 D-11).
    8. freshStore.Restore(ctx, reader) — Phase 2 D-06 invariant holds:
       destination is empty.
    9. Reader.Close() — Phase 3 D-11: returns ErrSHA256Mismatch if payload
       hash diverges from manifest (aborts before swap).
   10. Under stores.Service write-lock (new method SwapStore):
          old := registry[storeName]
          registry[storeName] = freshStore
       Also update any per-share BlockStore → new metadata binding if needed
       (shares refer to metadata store by name via Share.MetadataStore —
       no re-binding needed; name unchanged).
   11. Close old engine; delete old backing path:
          - Badger: os.RemoveAll(oldPath)
          - Postgres: DROP SCHEMA old_schema CASCADE (inside orchestrator txn)
          - Memory: GC (just drop reference)
   12. Rename temp → canonical:
          - Badger: os.Rename(tempDir, store.path)
          - Postgres: RENAME SCHEMA "…_restore_<ulid>" TO "<original>"
          - Memory: already swapped (step 10 used freshStore directly)
   13. Bump NFSv4 serverBootVerifier (D-10).
   14. Transition BackupJob → status=succeeded, FinishedAt=now.
       Emit Prometheus counter + OTel span end.
   15. Shares remain disabled (D-04). Operator re-enables explicitly.
  ```

  Failure semantics at each step:
  - Steps 1–4 fail pre-swap: old store untouched; no temp path created.
  - Step 6 fails: old store untouched; temp path cleanup in defer.
  - Step 8 fails (engine Restore error): old store untouched; fresh engine
    + temp path wiped via defer.
  - Step 9 fails (SHA-256 mismatch): old store untouched; fresh engine +
    temp path wiped.
  - Step 10 (swap) is the **commit point** — atomic under the
    `stores.Service` write-lock. No "half-swapped" state.
  - Step 11+ failures (close old / delete old / rename): job is logged as
    `succeeded` (restore is visible to clients), but old backing path /
    dangling temp schema may persist → startup orphan sweep (D-14).

  Rejected: in-place restore (engine.Restore on the live store) — violates
  Phase 2 D-06 "require empty destination". Quiesce-only (stop adapters,
  Restore live) — doesn't cover operator-out-of-band runtime callers
  (Phase 6 API handlers, background refresh loops); side-engine swap
  eliminates the concern.

- **D-06 — Store identity gate: manifest.store_id == target.store_id mandatory.**
  [Auto-selected, Pitfall #4 mitigation]
  Each metadata store engine persists a stable `store_id` (UUID, assigned
  on first open, never rotated). `Manifest.StoreID` is snapshotted at backup
  time. Restore hard-rejects mismatch. Operator workaround for "I want to
  restore backup A's data into a new store B" is Phase 6's explicit
  `--target <new-store-name>` command (deferred `REST2NEW-01`), not a
  `--force-cross-store` flag on the current restore path.

  **Requires schema change in Phase 5:** if metadata stores don't yet
  persist a stable store_id (check per-engine — Phase 1 locked the manifest
  field, engines must populate it on first init), add persistent `store_id`
  to the engine's internal state (Badger: `cfg:store_id` key; Postgres:
  `server_config` row; Memory: pre-populated on construction). Research
  agent verifies whether Phase 1's manifest handled this or if Phase 5
  inherits the gap.

- **D-07 — Overlap guard: restore shares the per-repo mutex with backup.**
  [Auto-selected, extension of Phase 4 D-07]
  `storebackups.Service.RunRestore` acquires the same `overlap.TryLock(repoID)`
  the backup path uses. Concurrent backup + restore in the same repo is
  physically incoherent (the very store we'd be backing up gets swapped
  mid-snapshot); same-mutex makes the contract machine-enforced.
  Backup-in-flight returns 409 to restore caller (same `ErrBackupAlreadyRunning`
  sentinel with repoID in message). Cross-repo concurrent restores are
  allowed — unrelated metadata stores, no shared resource.

- **D-08 — Fresh engine construction path: delegate to `stores.Service.OpenAtPath`.**
  [Auto-selected]
  New method on `stores.Service`:
  `OpenMetadataStoreAtPath(ctx, cfg *MetadataStoreConfig, pathOverride string)`
  returns `metadata.MetadataStore` without registering it. Orchestrator uses
  this to spin up the fresh engine at temp path; registers it via a separate
  `SwapMetadataStore(name, newStore)` call at commit time. Keeps the "open"
  and "register" steps decoupled — registry stays the single source of truth
  for what's live.

- **D-09 — Post-restore NFSv4 boot verifier bump.**
  [Auto-selected, Pitfall #2 belt-and-suspenders]
  `serverBootVerifier` (currently initialized in `internal/adapter/nfs/v4/handlers/write.go:20`
  at process start) is hoisted to an atomically-settable package variable
  with `SetBootVerifier([8]byte)`. Phase 5 `RunRestore` calls
  `writeHandlers.BumpBootVerifier()` on successful swap. Even though shares
  are disabled (D-04), bumping is cheap and covers the edge case where an
  operator re-enables before clients had a chance to observe STATE_NETWORK_NAME_DELETED.
  NFSv4 clients reconnecting after re-enable see a new verifier → reclaim
  grace path → every attempt to reclaim state fails with
  `NFS4ERR_RECLAIM_BAD` → client issues fresh OPENs with new stateids.

- **D-10 — SMB durable handles / leases: no explicit clear step.**
  [Auto-selected, correctness by construction]
  Durable handles and lease state are persisted inside the metadata store
  (`dh:*`, `lock:*` prefixes per `pkg/metadata/store/badger/`). Restoring
  the metadata store replaces these with the snapshot's state. The only
  ephemeral piece — the in-memory adapter session table — is already
  cleared by D-02's disable-drop-connections behavior. Explicit clear is
  redundant.
  If Phase 7 E2E turns up a gap (e.g., some SMB state that lives in
  runtime not metadata), Phase 7 wires the additional clear; Phase 5
  does not speculate.

### Block-Store GC Retention Hold (SAFETY-01)

- **D-11 — At-GC-time manifest union, no persisted hold table.**
  [Auto-selected]
  New `pkg/blockstore/gc.BackupHoldProvider` interface:
  ```go
  type BackupHoldProvider interface {
      HeldPayloadIDs(ctx context.Context) (map[metadata.PayloadID]struct{}, error)
  }
  ```
  Implementation in `pkg/controlplane/runtime/storebackups/backup_hold.go`:
  1. `ListAllBackupRepos(ctx)` → every active repo
  2. For each repo: `ListSucceededRecordsByRepo(ctx, repoID)` → all
     retained records
  3. For each record: `Destination.GetManifestOnly(ctx, record.ID)` →
     manifest.yaml (~KB each; fetched not Get-the-payload)
  4. Union all `manifest.PayloadIDSet` into a single map

  `gc.CollectGarbage` accepts an optional `hold BackupHoldProvider`. Before
  treating a block's PayloadID as orphan, GC checks `held[payloadID]`. Held
  → retain (log "GC: holding orphan for backup"). Not held → eligible for
  orphan path.

  Rejected: persisted `backup_holds` table synced on backup-completion
  (adds write path, failure recovery complexity, race with retention
  deletes). Computed-at-GC-time stays correct through retention deletes
  naturally — deleted manifest → no entries in subsequent GC runs → blocks
  reclaimable.
  Rejected: bloom filter per manifest (compaction isn't needed; PayloadID
  sets are small — metadata stores hold thousands of files, not millions).

- **D-12 — Destination API addition: `GetManifestOnly(ctx, id)`.**
  [Auto-selected]
  Phase 3's `Destination.GetBackup` returns both manifest + payload reader.
  For GC hold, we only need the manifest. Add a lighter-weight method:
  ```go
  GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error)
  ```
  Local FS: read `<repo-root>/<id>/manifest.yaml`.
  S3: `GetObject` on `<prefix>/<id>/manifest.yaml` only — no multipart,
  no payload bandwidth. Both drivers cheap. Avoids the `ReadCloser`
  lifecycle of `GetBackup` when we only need the metadata.

- **D-13 — GC hold applies to orphan-block path only, not to metadata-gated
  deletes.**
  [Auto-selected, scope-narrowed]
  `pkg/blockstore/gc/gc.go` currently deletes a payloadID's blocks when
  no metadata row references it. Phase 5 hooks the hold check at exactly
  this point: "no metadata references AND not in hold set → orphan".
  Metadata-initiated deletes (file unlinked, share removed) are unchanged
  — those are synchronous with the user's intent and don't consult the
  hold. Retention never deletes a backup while blocks are referenced by
  live metadata; what we're protecting is the "file deleted, GC pending,
  backup still holds the payload" window.

- **D-14 — Restore-time orphan sweep.**
  [Auto-selected]
  On `storebackups.Service.Serve(ctx)`, alongside the existing interrupted-job
  recovery (Phase 4 D-19), sweep the metadata-store backing directory / schema
  namespace for orphan restore temp paths:
  - Badger: any `<store.path>.restore-<ulid>` dir older than grace window (1h
    default) → `os.RemoveAll`
  - Postgres: any schema matching `<current>_restore_<ulid>` older than grace
    → `DROP SCHEMA CASCADE`
  - Memory: no-op (process-local, already collected)

  Parallels Phase 3 D-06 destination orphan sweep. Logs every reclaim at
  WARN with age + identifier.

### Record Selection & Job Lifecycle

- **D-15 — Default latest: most recent `BackupRecord` with status=succeeded.**
  [Auto-selected, REST-04]
  `BackupStore.ListSucceededRecordsByRepo(ctx, repoID)` is already used for
  retention (Phase 4); Phase 5 reuses the same call, takes `records[0]`.
  `created_at DESC` order is enforced at the store layer.
  Null case: zero successful records in repo → `ErrNoRestoreCandidate`
  (409 Conflict).

- **D-16 — `--from <backup-id>` validates repo match.**
  [Auto-selected]
  `RunRestore(ctx, repoID, recordID)` with non-nil `recordID`:
  1. `GetBackupRecordByID(ctx, *recordID)` → not found → `ErrRecordNotFound`
  2. `record.RepoID != repoID` → `ErrRecordRepoMismatch` (surface
     "backup is from repo <actual>, not <requested>")
  3. `record.Status != succeeded` → `ErrRecordNotRestorable` (surface
     "backup status: failed/interrupted/pending, cannot restore from it")
  Defensive — Phase 6 CLI may have bugs in the UUID/ULID flow; Phase 5
  catches mistakes at the business-logic layer.

- **D-17 — Mid-restore shutdown cancellation (Phase 4 D-18 extension).**
  [Auto-selected]
  SIGTERM during restore: ctx cancels → `GetBackup` reader returns
  ctx.Err() → `freshStore.Restore` aborts with the ctx error. Fresh store
  + temp path wiped by the existing defer. BackupJob transitions to
  `interrupted` via the same mechanism Phase 4 uses for backups. Next
  server boot: `RecoverInterruptedJobs` already handles it (no Phase-5
  code change).

- **D-18 — No auto-retry on interrupted restore.**
  [Auto-selected, REST-05 interpretation]
  Interrupted restore leaves the old engine intact (swap never happened)
  and the fresh engine / temp path removed. Operator re-issues the same
  `restore --repo X [--from Y]`; a new `BackupJob{kind: restore}` row is
  created; same precondition checks re-run. No checkpoint, no partial
  resume — partial restores are conceptually unsafe (Phase 2 D-07) and
  idempotence is cheaper than checkpoint machinery.

### Observability

- **D-19 — Minimal Prometheus + OTel hooks.**
  [Auto-selected, scope-narrowed]
  Phase 5 ships only the counters/gauges that make a silent-failure of
  backup/restore observable (Pitfall #10):
  - `backup_operations_total{kind="backup|restore", outcome="succeeded|failed|interrupted"}`
  - `backup_last_success_timestamp_seconds{repo_id, kind}`
  - OTel span around `RunBackup` and `RunRestore` — single top-level span
    per operation, no per-step fan-out.
  Deferred to Phase 7 or post-v0.13.0: duration histograms, in-flight
  gauges, retention counters, byte-throughput metrics. One well-placed
  `last_success_timestamp` + operation counter is enough for an operator
  alert rule ("no successful backup in 2×scheduled-period").

- **D-20 — Observability gated by the existing server.metrics.enabled flag.**
  [Auto-selected]
  No new config knob. If metrics are off at the server level, backup
  metrics register into a noop collector. OTel traces follow the same
  gating as existing NFS/SMB spans (`telemetry.enabled`).

### Code Layout & Runtime Integration

- **D-21 — `pkg/backup/restore/` package, parallel to `pkg/backup/executor/`.**
  [Auto-selected, extension of Phase 4 D-24]
  New package holds the restore-specific orchestration primitives:
  ```
  pkg/backup/restore/
    restore.go              — Executor.RunRestore
    fresh_store.go          — OpenFreshEngineAtTemp helpers per engine kind
    swap.go                 — atomic swap coordinator
    errors.go               — Phase-5 sentinels (ErrRestorePreconditionFailed,
                              ErrStoreIdMismatch, ErrStoreKindMismatch,
                              ErrNoRestoreCandidate, ErrRecordRepoMismatch,
                              ErrRecordNotRestorable, ErrRestoreAborted)
  ```
  `storebackups.Service` composes it with `storebackups.Service.RunRestore`
  (user-facing entrypoint) wrapping the `restore.Executor`.

- **D-22 — New methods on `shares.Service`:** `DisableShare`, `EnableShare`,
  `IsShareEnabled`, `ListEnabledSharesForStore`.
  [Auto-selected]
  Mirrors the existing `AddShare / RemoveShare / UpdateShare` pattern at
  `pkg/controlplane/runtime/shares/service.go`. `DisableShare` calls the
  existing `notifyShareChange` chain so adapters pick up the change.
  Persistence: `DisableShare` writes `shares.enabled = false` via the
  composite Store before touching the runtime map (DB-first for crash-consistency).

- **D-23 — New method on `stores.Service`:** `SwapMetadataStore(name,
  newStore)`.
  [Auto-selected]
  Atomic registry swap under write-lock. Returns the displaced store so
  the orchestrator can close + delete it. Mirrors Phase-4 era "rename"
  semantics but at a different layer.
  Companion helper: `OpenMetadataStoreAtPath(ctx, cfg, pathOverride)` for
  fresh-engine construction without registration.

- **D-24 — No new sub-service.**
  [Auto-selected]
  Restore reuses the existing 9-sub-service runtime layout Phase 4 set up.
  `storebackups.Service` covers both kinds (D-25 in Phase 4). No 10th
  sub-service; no "restore" sub-service separate from "backup".
  Matches Phase 4 D-25 explicit guidance.

- **D-25 — Schema migration strategy.**
  [Auto-selected]
  Single migration in this phase:
  ```
  ALTER TABLE shares ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT true;
  ```
  Goes through the existing `AllModels + AutoMigrate` path (Phase 1 D-07
  pattern). Also update `models.Share.Enabled` field with GORM tag
  `gorm:"default:true;not null"`.
  Migration is forward-safe — existing shares default `true` (enabled),
  preserving current behavior.

### Error Taxonomy Additions

- **D-26 — Phase-5 sentinels wrap at two layers.**
  [Auto-selected]
  Runtime layer (`pkg/controlplane/runtime/storebackups/errors.go`
  extended):
  - `ErrRestorePreconditionFailed` → 409 (one or more shares enabled)
  - `ErrNoRestoreCandidate` → 409 (repo has no succeeded records)
  - `ErrStoreIdMismatch` → 400 (manifest.store_id != target store_id)
  - `ErrStoreKindMismatch` → 400 (manifest.store_kind != target store kind)
  - `ErrRecordNotRestorable` → 409 (status != succeeded)
  - `ErrRecordRepoMismatch` → 400 (record belongs to different repo)

  Share layer (`pkg/controlplane/runtime/shares/errors.go` new):
  - `ErrShareAlreadyDisabled` — Disable on already-disabled share (idempotent
    OK, but returns sentinel for caller introspection)
  - `ErrShareNotFound` (existing — reuse)
  - `ErrShareStillInUse` — Disable succeeded but a mount-tracker entry
    remained after timeout (surface loudly; continue anyway per D-03)

### Claude's Discretion

[All below: planner / researcher may refine during Phase 5 planning without
revisiting CONTEXT.md]

- Exact shape of `Destination.GetManifestOnly` signature — whether it returns
  `*manifest.Manifest` directly or the raw bytes (parsed by caller).
  Whatever's cleanest given existing Phase 3 code.
- Whether `shares.Service.DisableShare` should have a `gracePeriod` parameter
  or reuse `lifecycle.ShutdownTimeout` (D-03). Either works; reuse is simpler.
- Postgres restore — temp schema name format (`<original>_restore_<ulid>`)
  and whether to run restore COPY inside a nested transaction vs. per-table
  commits. Phase 2 chose per-engine; Phase 5 reuses that path unmodified.
- Badger restore — whether temp dir lives adjacent to the original
  (`<path>.restore-<ulid>`) or under a sibling parent (`<parent>/.restore/<name>-<ulid>`).
  Adjacent keeps one-dir-per-store; sibling tidies root.
- Whether post-restore NFSv4 boot-verifier bump is a function on the handlers
  package or a runtime-level primitive exposed via the existing
  `adapters.Service` interface. Either path, as long as it's callable from
  `storebackups.Service.RunRestore`.
- Observability: Prometheus metric naming (`backup_operations_total` vs.
  `dittofs_backup_operations_total`) — follow the existing project convention
  at `pkg/controlplane/runtime/` observability plumbing.
- Whether to surface `storebackups.Service.ListRestoreCandidates(ctx, repoID)`
  now (Phase 6 needs it) or defer. Planner picks; trivial either way.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner) MUST read these before planning
or implementing.**

### Phase 1-4 lock-ins (binding contracts)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-CONTEXT.md` — Phase 1 context (models, manifest schema incl. `store_id` / `store_kind` / `payload_id_set`, SAFETY-02 store-layer primitive)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-02-SUMMARY.md` — `BackupStore` sub-interface (Phase 5 calls `ListSucceededRecordsByRepo`, `GetBackupRecordByID`, `UpdateBackupJob`, `RecoverInterruptedJobs`)
- `.planning/phases/01-foundations-models-manifest-capability-interface/01-03-SUMMARY.md` — manifest v1 format + `Backupable` + `PayloadIDSet` typing
- `.planning/phases/02-per-engine-backup-drivers/02-CONTEXT.md` — Phase 2 context; D-06 ("restore destination must be empty") is the foundational invariant Phase 5 relies on for the side-engine approach
- `.planning/phases/03-destination-drivers-encryption/03-CONTEXT.md` — Phase 3 context; D-11 `Destination.GetBackup` streaming SHA-256 verification; D-01 two-file layout means `manifest.yaml` is cheap to fetch standalone (D-12 of this phase adds `GetManifestOnly`)
- `.planning/phases/04-scheduler-retention/04-CONTEXT.md` — Phase 4 context; D-07 overlap guard (Phase 5 extends), D-22 `RegisterRepo/UnregisterRepo` pattern (reused for scheduler integration), D-23 `RunBackup` entrypoint shape (Phase 5 mirrors with `RunRestore`), D-25 single-service-for-both-kinds, D-15 "Phase 5 fills in noop collectors"

### Project-level
- `.planning/REQUIREMENTS.md` §REST — REST-01..05 (Phase 5 requirements)
- `.planning/REQUIREMENTS.md` §SAFETY — SAFETY-01 block-GC hold, SAFETY-02 interrupted-job recovery (already complete at store layer; Phase 5 verifies restore-kind coverage)
- `.planning/REQUIREMENTS.md` §Out of Scope — `REST2NEW-01`, `XENG-01`, `AUTO-01` deferred
- `.planning/research/SUMMARY.md` §"Phase 04: Restore Orchestration + CLI/REST API" — original research scope (Phase 5 takes the orchestration half; CLI/REST moved to Phase 6)
- `.planning/research/SUMMARY.md` §"Phase 05: Retention + Observability + Block-Store GC Integration" — Phase 5 adopts the observability + GC-hold subset (retention shipped in Phase 4)
- `.planning/research/PITFALLS.md` §Pitfall 2 — Restore while shares mounted (drove D-01, D-02, D-09)
- `.planning/research/PITFALLS.md` §Pitfall 3 — Block-store divergence / GC'd blocks (drove D-11, D-12, D-13)
- `.planning/research/PITFALLS.md` §Pitfall 4 — Cross-store contamination (drove D-06)
- `.planning/research/PITFALLS.md` §Pitfall 10 — Silent failures (drove D-19)
- `.planning/PROJECT.md` — single-instance, no clustering (restore is single-process atomic swap; no distributed coordination); Unified Lock Manager ephemeral state post-restore is indistinguishable from post-crash (documented assumption)

### Implementation files Phase 5 touches
- `pkg/controlplane/runtime/storebackups/service.go` — extend with `RunRestore`, restore-path wiring; already contains overlap guard and scheduler plumbing from Phase 4
- `pkg/controlplane/runtime/storebackups/errors.go` — append Phase-5 sentinels (D-26)
- `pkg/controlplane/runtime/shares/service.go` — add `DisableShare` / `EnableShare` / `IsShareEnabled` / `ListEnabledSharesForStore`; extend `Share` struct with `Enabled bool` (D-22)
- `pkg/controlplane/runtime/stores/service.go` — add `SwapMetadataStore(name, newStore)` + `OpenMetadataStoreAtPath(ctx, cfg, pathOverride)` (D-08, D-23)
- `pkg/controlplane/models/share.go` — add `Enabled bool` field (D-01, D-25)
- `pkg/controlplane/store/share.go` — migration handling for new column
- `pkg/backup/restore/` — new package (D-21); restore executor, fresh-engine helpers, swap coordinator, error sentinels
- `pkg/backup/destination/destination.go` — add `GetManifestOnly(ctx, id)` method to `Destination` interface (D-12)
- `pkg/backup/destination/fs/store.go` — implement `GetManifestOnly`
- `pkg/backup/destination/s3/store.go` — implement `GetManifestOnly`
- `pkg/blockstore/gc/gc.go` — add `BackupHoldProvider` interface + hold-check at orphan-detection point (D-11, D-13)
- `pkg/controlplane/runtime/storebackups/backup_hold.go` — new file; `BackupHoldProvider` implementation (D-11)
- `internal/adapter/nfs/v4/handlers/write.go` — hoist `serverBootVerifier` to an atomic-settable package var; export `BumpBootVerifier` (D-09)
- `internal/adapter/nfs/*/dispatch.go` + `internal/adapter/smb/dispatch.go` — adapter-level "refuse if share disabled" check (D-02)
- `pkg/metadata/store/*/backup.go` (memory/badger/postgres) — verify each engine populates a stable `store_id` during first open (D-06)

### Reused infrastructure (read, don't modify)
- `pkg/backup/manifest/manifest.go` — `Manifest` struct, `store_id`, `store_kind`, `payload_id_set`, `sha256` fields
- `pkg/backup/destination/destination.go` — Phase 3 `Destination` interface (`GetBackup` streaming-SHA-verify semantics relied on by D-05 step 9)
- `pkg/backup/backupable.go` — Phase 4 D-27 relocated interface; `Backupable.Restore(r)` contract for empty destination (D-05 step 8)
- `pkg/backup/scheduler/overlap.go` — Phase 4 `OverlapGuard` reused for same-mutex contract (D-07)
- `pkg/controlplane/runtime/lifecycle/service.go` — `ShutdownTimeout` reused for share-disable bound (D-03)
- `pkg/controlplane/runtime/mounts/service.go` (Tracker) — read during D-02 `DisableShare` to find active mounts; `RemoveAllByProtocol` / `RemoveByClient` for eviction
- `pkg/controlplane/store/backup.go` — existing `ListSucceededRecordsByRepo` (Phase 4 retention), `GetBackupRecordByID`, `CreateBackupJob`, `UpdateBackupJob`, `RecoverInterruptedJobs` (Phase 1)
- `pkg/controlplane/runtime/storebackups/target.go` — `StoreResolver` interface (Phase 4 uses it for backup; Phase 5 reuses for restore target resolution)

### External (read at plan/execute time)
- RFC 7530 §3.3.1 — NFSv4 boot verifier semantics (drove D-09)
- MS-SMB2 §3.3.5.9.7 — durable handle reconnect (drove D-10 "handled by metadata replacement")
- BadgerDB `DB.Close` + directory rename semantics — https://pkg.go.dev/github.com/dgraph-io/badger/v4
- PostgreSQL `RENAME SCHEMA` + `DROP SCHEMA CASCADE` — https://www.postgresql.org/docs/current/sql-alterschema.html
- GORM migration for `ALTER TABLE … ADD COLUMN` — https://gorm.io/docs/migration.html
- Go `context.AfterFunc` (Phase 4 uses for `deriveRunCtx`) — Phase 5 inherits; reuse the pattern for restore cancellation

### Boundary docs (Phase 5 does NOT implement these)
- Phase 6 CLI/REST API — operator-facing surface, confirmation prompts, async job polling; Phase 5 only provides `RunRestore(ctx, repoID, recordID *string)` callable path.
- Phase 7 test matrix — chaos (kill mid-restore), cross-version, Localstack E2E matrix; Phase 5 provides unit + integration test coverage for its own surface only.

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets

- **`pkg/controlplane/runtime/storebackups/service.go`** — Phase 4 delivered
  the sub-service skeleton with Serve/Stop, overlap guard, interrupted-job
  recovery invocation, per-repo register/unregister hot-reload, and a unified
  `RunBackup(ctx, repoID)` entrypoint pattern. Phase 5 adds `RunRestore` as
  the sibling entrypoint, reuses the overlap guard, and wires the existing
  `deriveRunCtx` pattern for shutdown cancellation.
- **`pkg/backup/scheduler/overlap.go`** (via Phase 4) — `OverlapGuard.TryLock`
  is the sole concurrency primitive for per-repo exclusivity. Phase 5 calls
  it with the same `repoID` key; no new guard needed.
- **`pkg/controlplane/runtime/stores/service.go`** — the canonical registry
  of live metadata stores. Write-locked map → swap is trivially expressible
  as a new method under the existing lock discipline. `CloseMetadataStores()`
  pattern at line 73 is the template for closing the displaced store
  post-swap.
- **`pkg/controlplane/runtime/shares/service.go`** — 600+ line sub-service
  already has `AddShare / RemoveShare / UpdateShare / OnShareChange /
  notifyShareChange` wiring. D-02 piggybacks the notifyShareChange chain
  to propagate disable transitions to adapters.
- **`pkg/metadata/store/{memory,badger,postgres}/backup.go`** — Phase 2
  drivers implement `Backupable.Restore(r)` with the "destination must be
  empty" invariant. Phase 5 relies on this — the fresh engine at temp path
  IS empty, so the invariant holds by construction.
- **`pkg/backup/destination/destination.go`** — Phase 3 `Destination.GetBackup`
  already does streaming SHA-256 verification and returns `ErrSHA256Mismatch`
  on close. Phase 5 gets integrity verification "for free" from the existing
  driver contract; D-12 adds the cheap `GetManifestOnly` sibling for the GC
  hold path.
- **`pkg/blockstore/gc/gc.go:44`** — `MetadataReconciler` interface is the
  existing extension point. Phase 5 adds `BackupHoldProvider` alongside it
  (not a replacement); the union check at line 142–147 becomes
  "if metadata.GetFileByPayloadID is ok OR held[payloadID]: retain".
- **`internal/adapter/nfs/v4/handlers/write.go:17-23`** — `serverBootVerifier`
  is currently a package-level `[8]byte` initialized in `init()`. D-09 hoists
  it to atomic access via `atomic.Pointer[[8]byte]` or a sync.Mutex-guarded
  setter; refs at `write.go:243` and `commit.go:161` become loads.
- **`pkg/controlplane/store/backup.go`** — the GORM-backed `BackupStore`
  sub-interface already exposes `ListSucceededRecordsByRepo` for retention
  (Phase 4); `GetBackupRecordByID` for by-id lookup; `RecoverInterruptedJobs`
  for both backup and restore kinds (Phase 1 uniform treatment).

### Established Patterns

- **Sub-service composition under `pkg/controlplane/runtime/`** — 9 existing
  sub-services (adapters, shares, mounts, stores, lifecycle, identity,
  clients, blockstoreprobe, storebackups). Phase 5 does NOT add a 10th — it
  extends `storebackups` (D-24) and adds methods to `shares` + `stores`.
- **Explicit runtime API, not polling** — Phase 4 D-22 locked the convention
  (RegisterRepo / UnregisterRepo). Phase 5 follows: `DisableShare` /
  `EnableShare` / `SwapMetadataStore` are direct calls after DB commits,
  never eventual-consistency polling.
- **Typed sentinel errors wrapped with `%w`** — new Phase-5 errors follow
  the convention; `errors.Is` / `errors.As` checks at call sites.
- **GORM migration via AllModels + AutoMigrate** — `shares.enabled` column
  added via AutoMigrate; no manual `RenameColumn`-style manual migration
  needed (additive).
- **DB-first, then runtime** — Phase 4 pattern: DB commit first, then
  runtime notification. D-22 mirrors: `DisableShare` writes DB row before
  triggering adapter disconnects — crash-consistent.
- **`//go:build integration` for real-DB tests** — Phase 5 tests follow
  Phase 4's pattern: unit tests with fake clock + in-memory store,
  integration tests with SQLite fixtures + real Badger.

### Integration Points

- **Phase 6 (CLI/REST) consumes** `storebackups.Service.RunRestore(ctx, repoID,
  recordID *string)` — same pattern as `RunBackup`. Phase 6 owns confirmation
  prompts, `--yes`, `--dry-run`, async job polling; Phase 5 provides only the
  callable surface + the BackupJob rows that Phase 6 polls.
- **Phase 6 (share CLI)** also consumes new `shares.Service.DisableShare` /
  `EnableShare` methods directly (Phase 6 CLI adds `dfsctl share disable` /
  `dfsctl share enable` commands).
- **Phase 7 (testing)** adds chaos tests (kill mid-restore) that validate
  the D-05 step-by-step failure semantics. Phase 5 provides enough
  hook-visibility (logs at WARN + interrupted-job rows) to let Phase 7 assert
  recoverable state after induced crashes.
- **Phase 7 (testing)** adds GC-hold integration tests with a retained
  backup and GC-on-orphaned-metadata — verifies D-11 keeps the block alive.
- **NFS adapter** (`internal/adapter/nfs/*`) — consumes D-09's exported
  `BumpBootVerifier` from Phase 5's restore path; also consults `Share.Enabled`
  on each MOUNT / OPEN call.
- **SMB adapter** (`internal/adapter/smb/*`) — consults `Share.Enabled` on
  each TREE_CONNECT; no D-10 explicit clear needed (state travels with
  metadata store).
- **Block-store GC** (`pkg/blockstore/gc/gc.go`) — consumes D-11's
  `BackupHoldProvider`; GC runs stay orthogonal to backup scheduling.

</code_context>

<specifics>
## Specific Ideas

- **Safety-first is the project's defining v0.13.0 quality.** Every gray-area
  choice in Phase 5 defaulted to the conservative option, matching Phases 2+3+4
  philosophy: side-engine swap over in-place (D-05), hard-reject on
  store-identity mismatch over `--force` escape hatch (D-06), mandatory
  share-disabled precondition over best-effort drain (D-01, D-02), no
  auto-retry on interrupted restore (D-18), shared backup/restore mutex over
  finer-grained concurrency (D-07). The cost is operator friction (explicit
  enable/disable workflow); the benefit is "restore never corrupts".
- **"Reliable and safe" carries the same meaning as Phase 2 (user quote
  session 2026-04-16):** enterprise/edge NAS DR context, no data-loss windows,
  no partial-restore footguns, machine-enforced invariants where possible.
- **Phase 4 D-25 "single service, both kinds" drives the code layout.**
  `storebackups.Service` grows a sibling method (`RunRestore`) rather than
  sprouting a new `runtime/restore/` sub-service. Job kind discrimination
  is already in the schema (Phase 1 `BackupJob.Kind` enum).
- **`serverBootVerifier` bump as belt-and-suspenders, not primary mechanism.**
  Primary invalidation happens via REST-02's share-disable (connections
  gone before restore). The boot-verifier bump protects against an operator
  accidentally re-enabling shares before clients have a chance to observe
  the disconnect (edge case — a client's next reconnect hits a different
  metadata state). Cheap to implement (1-2 lines); measurable safety win.
- **"Metadata backup assumes block store is preserved independently"**
  documented invariant (Pitfall #1). Phase 5 honors it via SAFETY-01 GC hold
  (D-11, D-12, D-13) — metadata backup's `PayloadIDSet` is the contract
  that prevents "restored metadata → GC'd blocks" data loss.
- **Share `Enabled` column is a Phase-5-owned schema change.** It's not
  purely a Phase 6 concern even though Phase 6 adds `dfsctl share disable/
  enable`. Phase 5 requires it for REST-02 enforcement; the migration ships
  with Phase 5 to keep the restore path complete on its own. Phase 6's CLI
  is a thin adapter over Phase 5's runtime primitives.

</specifics>

<deferred>
## Deferred Ideas

- **Restore to a different metadata store (`REST2NEW-01`)** — future
  milestone. Would require a `--target <new-store-name>` flag and relaxing
  D-06's store-identity gate. Phase 5 hard-rejects.
- **Cross-engine restore via JSON IR (`XENG-01`)** — deferred. Manifest
  v1's `store_kind` field plus D-06's hard-reject on mismatch enforce
  engine-matched restore only.
- **Automatic test-restore / backup-verify command (`AUTO-01`)** — future.
  Nothing in Phase 5 precludes a future verify-runs flow reusing
  `Destination.GetBackup` + a no-op `Backupable.Restore` variant.
- **Incremental / PITR restore (`INCR-01`)** — future. Phase 5 always
  replaces the entire store with a single record's snapshot.
- **Checkpoint-based resumable restore** — rejected (D-18). Partial restore
  is conceptually unsafe; idempotence is the simpler contract.
- **Full Prometheus suite** — duration histograms, in-flight gauges,
  retention counters, byte-throughput — deferred beyond Phase 5's minimal
  hooks (D-19). Phase 7 or post-v0.13.0 may expand.
- **Unified Lock Manager state in the restore path** — assumed non-issue:
  locks are ephemeral, restored state has no active locks (equivalent to
  post-crash semantics, handled by NFSv4 reclaim grace / SMB durable-handle
  reconnect). Phase 7 validates this assumption.
- **K8s operator integration** — future operator milestone will patch
  `DittoServer.spec.paused=true` before calling `POST /restore`. Phase 5
  does not expose operator CRD fields.
- **`DisableShare` grace-period parameter** — Phase 5 reuses
  `lifecycle.ShutdownTimeout` (30s default). If operators request finer
  control, future iteration adds a per-call parameter.
- **Restore rollback after successful swap** — "oops, I picked the wrong
  backup". The operator workflow is "restore a different backup id"; the
  first restore creates a backup of the pre-restore state iff the
  operator ran a backup first (documented workflow, not machine-enforced).
- **Persisted block-GC hold table** — considered (Option B for D-11) but
  rejected as over-engineered. At-GC-time manifest union is simpler and
  self-healing.
- **Explicit SMB durable-handle / lease clear on restore** — unnecessary by
  D-10 analysis. Phase 7 E2E may surface a gap; if so, the clear step is
  added at that time, not speculatively in Phase 5.

### Reviewed Todos (not folded)

None — `gsd-tools todo match-phase 5` returned zero matches.

</deferred>

---

*Phase: 05-restore-orchestration-safety-rails*
*Context gathered: 2026-04-16 (`--auto` mode; reviewer should audit decisions
marked "auto-selected" before planning)*

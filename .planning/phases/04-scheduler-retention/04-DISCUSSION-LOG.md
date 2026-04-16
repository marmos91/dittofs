# Phase 4: Scheduler + Retention — Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-04-16
**Phase:** 04-scheduler-retention
**Areas discussed:** Missed-run / catch-up policy, Jitter strategy, Retention combination semantics, Shutdown + failure lifecycle, Retention trigger timing, Scheduler hot-reload on repo config change, Retention error handling, Malformed cron / invalid schedule at startup, Backup ID generation ownership, Backup executor interface shape, Code structure & schema scoping

---

## Missed-run / catch-up policy

| Option | Description | Selected |
|--------|-------------|----------|
| Skip missed entirely | Default robfig/cron/v3 behavior. Matches SCHED-CATCHUP deferred. | ✓ |
| Fire once on startup | Single catch-up if last success is older than schedule interval. | |
| Operator-configurable per repo | BackupRepo.MissedRunPolicy column. | |

**Startup warm-up delay:**

| Option | Description | Selected |
|--------|-------------|----------|
| No startup delay | Scheduler fires on normal next-tick basis. | ✓ |
| Fixed 60s warm-up | Sleep 60s after Serve() to let readiness probes settle. | |

---

## Jitter strategy

| Option | Description | Selected |
|--------|-------------|----------|
| Per-repo stable phase offset | fnv(repo_id) % max_jitter; deterministic. | ✓ |
| Per-run random offset | Fresh rand each tick; drifts daily. | |
| Global concurrency cap | Serial execution, max N concurrent. | |

**Default jitter window:**

| Option | Description | Selected |
|--------|-------------|----------|
| 0–5 minutes | ~15s average spread for 20 repos. | ✓ |
| 0–10 minutes | Conservative for 50+ repos. | |
| Operator-configurable | Add jitter_seconds column. | |

**Global concurrency cap on top of jitter:**

| Option | Description | Selected |
|--------|-------------|----------|
| No global cap | KISS for v0.13.0. | ✓ |
| Soft cap of N=2 | Semaphore limits concurrent backups. | |

---

## Retention combination semantics

| Option | Description | Selected |
|--------|-------------|----------|
| Union — keep if EITHER matches | Restic / kopia / borg convention. | ✓ |
| Intersection — keep only if BOTH match | More aggressive pruning. | |
| Priority: count wins over age | Simpler implementation. | |

**Pinned records and keep_count:**

| Option | Description | Selected |
|--------|-------------|----------|
| Pinned records outside the count | keep_count=7 + 3 pinned = 10 kept. | ✓ |
| Pinned records count toward keep_count | keep_count=7 + 3 pinned = 7 kept. | |

**SCHED-05 safety rail vs age rule:**

| Option | Description | Selected |
|--------|-------------|----------|
| Safety rail always wins | Keep the only successful backup even if age-expired. | ✓ |
| Age rule wins if operator set it | Prune anyway, repo may end empty. | |

---

## Shutdown + failure lifecycle

| Option | Description | Selected |
|--------|-------------|----------|
| Cancel immediately, mark interrupted | Propagate ctx; orphan sweep cleans up. | ✓ |
| Wait up to ShutdownTimeout, then cancel | Grace period for small backups. | |

**Failed / interrupted attempts and BackupRecord rows:**

| Option | Description | Selected |
|--------|-------------|----------|
| Only BackupJob rows track failures | BackupRecord = restorable archives only. | ✓ |
| Failed attempts also create BackupRecord(status=failed) | Adds noise to restore list. | |

**BackupJob row pruning:**

| Option | Description | Selected |
|--------|-------------|----------|
| Prune BackupJob rows older than 30 days | Bounded DB growth. | ✓ |
| Keep BackupJob rows forever | Operator sees full history; unbounded growth. | |
| Defer the decision to a later milestone | Accept unbounded growth for v0.13.0. | |

---

## Retention trigger timing

| Option | Description | Selected |
|--------|-------------|----------|
| Inline after each successful backup | Same goroutine, same mutex. | ✓ |
| Separate global retention cron (@daily) | Independent from backup timing. | |
| On-demand only via Phase 6 CLI | Defers automation. | |

**Retention failure effect on parent job status:**

| Option | Description | Selected |
|--------|-------------|----------|
| Job stays succeeded; retention failures separate | Cleaner alerting surfaces. | ✓ |
| Job marked degraded/partial | New state value, doesn't change operator action. | |

---

## Scheduler hot-reload on repo config change

| Option | Description | Selected |
|--------|-------------|----------|
| Explicit Reschedule / RemoveSchedule API | Called by Phase 6 handlers. | ✓ |
| Periodic poller (settings_watcher pattern) | 10s lag, eventual consistency. | |
| Restart-only (no hot reload) | Documented limitation. | |

---

## Retention error handling

| Option | Description | Selected |
|--------|-------------|----------|
| Continue on error, log each, report summary | Maximizes cleanup throughput. | ✓ |
| Fail fast — abort on first error | Simpler semantics, worse resilience. | |
| Transactional — either all or none | Over-engineered for cloud operations. | |

**Delete order:**

| Option | Description | Selected |
|--------|-------------|----------|
| Destination first, then DB record | Orphan-DB-row is diagnosable via ErrManifestMissing. | ✓ |
| DB record first, then destination | Orphan destination objects not caught by sweep. | |

---

## Malformed cron / invalid schedule at startup

| Option | Description | Selected |
|--------|-------------|----------|
| Per-repo disable-and-continue | Skip bad row with WARN, other repos still scheduled. | ✓ |
| Fail runtime.Serve() hard | One bad row denies-of-service the whole scheduler. | |
| Auto-clear the bad schedule | Silently mutates operator data; violates principle. | |

**Write-time validation (Phase 6):**

| Option | Description | Selected |
|--------|-------------|----------|
| Yes — expose ValidateSchedule(expr) | Fail fast with 400 at API surface. | ✓ |
| No — only startup validation | Typo only surfaces on next restart. | |

---

## Backup ID generation ownership

| Option | Description | Selected |
|--------|-------------|----------|
| Phase 4 executor generates before PutBackup | Single source of truth. | ✓ |
| Phase 3 driver generates internally | Complicates BackupJob tracking. | |

**BackupJob.ID and BackupRecord.ID relationship:**

| Option | Description | Selected |
|--------|-------------|----------|
| Distinct ULIDs | Job and Record are different concepts. | ✓ |
| Same ULID for both | Implies 1:1 even for retries. | |

---

## Backup executor interface shape

| Option | Description | Selected |
|--------|-------------|----------|
| New runtime/storebackups sub-service | Mirrors adapters/shares/mounts pattern. | ✓ |
| Flat module in pkg/backup/ | Breaks runtime convention. | |

**Backup+restore ownership:**

| Option | Description | Selected |
|--------|-------------|----------|
| One storebackups.Service owns both | BackupJob.kind discriminator already exists. | ✓ |
| Separate backups and restores services | Duplicates interrupted-job recovery. | |

---

## Code structure & schema scoping

**User prompt: "Should backups live per store type? So metadata store backups? … Maybe in the future we will add a backup for blockstores as well. Generically speaking we are speaking about store backup."**

| Option | Description | Selected |
|--------|-------------|----------|
| Generic framework + per-resource orchestrators | pkg/backup/scheduler + pkg/backup/executor generic; target-specific wiring | ✓ |
| Explicit per-resource sub-service, primitives under pkg/backup/ | Lighter abstraction, resource code next to runtime wiring | |
| runtime/backups/metadata/ nested structure | Breaks flat sibling convention | |

**Refactor Phase 3 destination code too?**

| Option | Description | Selected |
|--------|-------------|----------|
| Move nothing — pkg/backup/destination stays generic | Phase 3 code is already correctly layered | ✓ |

**Schema migration — target_id + target_kind vs keep metadata_store_id:**

| Option | Description | Selected |
|--------|-------------|----------|
| Rename metadata_store_id → target_id with target_kind discriminator | Future-proof for block-store backup | ✓ |
| Keep schema as-is | Simpler now, separate table for future block-backup | |

**Sub-service name:**

| Option | Description | Selected |
|--------|-------------|----------|
| storebackups — generic umbrella | Matches "store backup" framing | ✓ |
| metadatabackups — scoped to current | Future block lives in sibling sub-service | |

---

## Claude's Discretion

- Clock / time abstraction for testability
- Observability hooks shape (noop collector vs defer to Phase 5)
- Exact internal structure of scheduler (goroutine-per-repo vs central loop)
- Whether to expose `ListInterrupted` helper or let Phase 6 query directly

## Deferred Ideas

- Missed-run catch-up policy (SCHED-CATCHUP)
- Per-repo configurable jitter window
- Global concurrency cap on backups
- Observability wiring (Phase 5)
- Heartbeat / watchdog metric
- K8s operator backup CRD fields
- Block-store backup (entire consumer)
- Cross-engine restore (XENG-01)
- Automatic test-restore ("backup verify", AUTO-01)
- Configurable BackupJob prune window beyond 30 days
- Per-run random jitter as operator-selectable alternative

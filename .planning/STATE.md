---
gsd_state_version: 1.0
milestone: v0.13.0
milestone_name: milestone
status: executing
stopped_at: Completed 05-10-PLAN.md
last_updated: "2026-04-17T09:10:13.058Z"
last_activity: 2026-04-17 -- Phase 6 planning complete
progress:
  total_phases: 7
  completed_phases: 4
  total_plans: 34
  completed_plans: 27
  percent: 79
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-04-15)

**Core value:** Enable enterprise-grade multi-protocol file access with unified locking, Kerberos auth, and immediate cross-protocol visibility
**Current focus:** Phase 05 — restore-orchestration-safety-rails

## Current Position

Phase: 05 (restore-orchestration-safety-rails) — EXECUTING
Plan: 10 of 10
Status: Ready to execute
Last activity: 2026-04-17 -- Phase 6 planning complete

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |
| v3.6 Windows Compatibility | 29.8-32 | 12 | Feb 26-28, 2026 | 2026-02-28 |
| v3.8 SMB3 Protocol Upgrade | 33-40.5 | 26 | Mar 1-4, 2026 | 2026-03-04 |
| v4.2 Benchmarking & Performance | 57-62 | -- | Mar 4, 2026 | 2026-03-04 |
| v4.0 BlockStore Unification | 41-49 | 24 | Mar 9-11, 2026 | 2026-03-11 |
| v4.3 Protocol Gap Fixes | 49.1-49.3 | 1 | Mar 12-13, 2026 | 2026-03-13 |
| v4.7 Offline/Edge Resilience | 63-68 | 10 | Mar 15-20, 2026 | 2026-03-20 |
| v0.10.0 Production Hardening + SMB fixes | 69-73.1 | — | Mar 20-25, 2026 | in flight |

## Accumulated Context

### Decisions

Historical decisions archived in PROJECT.md Key Decisions table.

**v0.13.0 decisions:**

- Reset phase numbering to 1 for v0.13.0 (previous v0.10.0 phase directories archived under `.planning/milestones/v0.10.0-phases/`)
- Fine granularity (from config.json) — 7 phases preserving natural boundaries: foundations, per-engine drivers, destinations, scheduler/retention, restore orchestration, API surface, testing
- Metadata-only scope; block data backup explicitly out of scope
- Destination drivers live in a new `pkg/backup/destination/` package separate from `pkg/blockstore/remote/` (different semantics: immutable archives vs block-addressable chunks) but share AWS client plumbing
- Manifest v1 ships with `payload_id_set` field from day one for forward-compat with block-GC hold (SAFETY-01)
- Restore precondition is share-disabled (REST-02) — shares must be manually disabled before restore; restore returns 409 Conflict otherwise
- Retention is a separate post-upload pass, never races with in-flight backup (SCHED-06)
- `robfig/cron/v3` is the only new direct dependency
- [Phase 05]: Defined narrow shares.ShareStore interface locally (GetShare + UpdateShare only) to avoid runtime→store import cycle
- [Phase 05]: Share.Enabled GORM tag = 'default:true;not null'; post-AutoMigrate backfill covers SQLite ADD-COLUMN dialect
- [Phase 05]: Engine-persistent store_id: Badger uses cfg:store_id key, Postgres uses server_config.store_id column (migration 000008), Memory uses struct field populated on construction; all return ULID via GetStoreID()
- [Phase 05]: target.go DefaultResolver.Resolve returns engine-persistent GetStoreID() instead of volatile cfg.ID — D-06 cross-store contamination gate now meaningful
- [Phase 05-restore-orchestration-safety-rails]: GetManifestOnly returns parsed *manifest.Manifest directly (not raw bytes) — all callers need the parsed form, drivers already parse internally
- [Phase 05-restore-orchestration-safety-rails]: S3 GetBackup delegates manifest prologue to GetManifestOnly — shared error shape, no code duplication
- [Phase 05-restore-orchestration-safety-rails]: Plan 04: Postgres schema-scoped open deferred to Plan 06 — Plan 04 ships the signature + dispatch with a clear deferred-construction error for postgres; Plan 06 wires the real search_path construction
- [Phase 05-restore-orchestration-safety-rails]: Plan 04: ListPostgresRestoreOrphans is REQUIRED (non-optional) — non-Postgres stores produce a clear error rather than silent empty slice, so crash-interrupted restore orphans cannot accumulate undetected
- [Phase 05-restore-orchestration-safety-rails]: Plan 04: Postgres schema orphan CreatedAt derived from embedded ULID timestamp (Option A) rather than pg_stat_file (Option B) — portable, zero extra DB metadata required
- [Phase 05]: Use atomic.Pointer[[8]byte] for serverBootVerifier — lock-free hot-path reads, safe cold-path bump from RunRestore
- [Phase 05]: Plan 06: JobStore interface uses GetBackupRecord (not GetBackupRecordByID) to match real GORMStore method name; Plan 07 can compose without adapters.
- [Phase 05]: Plan 06: RenamePostgresSchema implemented as interface-assertion extension point in CommitSwap; concrete impl deferred to Plan 07 (recommended) or orphan sweep fallback.
- [Phase 05]: Plan 06: Terminal-state UpdateBackupJob uses context.Background() so SAFETY-02 row lands even after parent ctx cancellation.
- [Phase 05]: Plan 05-07: Phase-5 sentinels canonical in pkg/backup/restore (not storebackups) to avoid import cycle
- [Phase 05]: Plan 05-07: RestoreResolver extends StoreResolver (ResolveWithName + ResolveCfg) — backward-compat preserved
- [Phase 05]: Plan 05-07: SetRestoreBumpBootVerifier post-construction setter on runtime.Runtime avoids adapter→runtime import cycle
- [Phase 05]: Plan 08: block-GC hold uses at-GC-time manifest union; no persisted hold table (D-11)
- [Phase 05]: Plan 08: provider errors fail-open (under-hold) rather than abort GC
- [Phase 05]: Plan 09: Shipped MetricsCollector + Tracer interfaces with Noop defaults and OTel concrete; deferred PromMetrics concrete because prometheus/client_golang not in go.mod and Phase 5 forbids new top-level deps.
- [Phase 05]: Plan 09: Propagate share.Enabled from DB model to runtime ShareConfig in init.go (Rule 3 auto-fix) — without this, production upgrades would load all shares as Enabled=false and adapter gates would refuse everything.
- [Phase 05]: Runtime.RunBlockGC production entrypoint closes SAFETY-01 — refuses without BackupHold wiring; dedups distinct remotes by configID via shares.Service.DistinctRemoteStores

### Pending Todos

- Run `/gsd-plan-phase 1` to decompose Phase 1 (Foundations) into executable plans.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-04-16T23:27:10.924Z
Stopped at: Completed 05-10-PLAN.md
Next action: `/gsd-plan-phase 1` — Foundations: models, manifest, capability interface
